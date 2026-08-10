package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

type runtimeCleanupState struct {
	controller *runtimeController
	ctx        context.Context
	run        core.Run
	ref        containerruntime.RuntimeRef
	control    *runControl
}

type runtimeCleanupStep struct {
	name string
	run  func(*runtimeCleanupState) error
}

var runtimeCleanupSteps = []runtimeCleanupStep{
	{name: "stopContainer", run: stopCleanupContainer},
	{name: "removeContainer", run: removeCleanupContainer},
	{name: "closeRunAPISocket", run: closeCleanupControl},
	{name: "removeRunControl", run: removeCleanupControl},
}

func (c *runtimeController) supervise(monitor *runMonitor) {
	defer c.unregisterMonitor(monitor)
	defer func() { _ = monitor.cancelAndCollectLogs(2 * time.Second) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var logFailure error
	for {
		select {
		case result := <-monitor.wait:
			if errors.Is(result.err, context.Canceled) {
				select {
				case <-c.contextDone():
					return
				default:
				}
			}
			if errors.Is(result.err, containerruntime.ErrUnavailable) {
				c.setDegraded(result.err.Error())
				return
			}
			_ = c.finishObservedRun(core.Run{}, monitor, result)
			return
		case err := <-monitor.logs:
			logFailure = monitor.rememberLogResult(err)
			if logFailure == nil {
				continue
			}
			if errors.Is(logFailure, context.Canceled) {
				select {
				case <-c.contextDone():
					return
				default:
				}
			}
			c.stopForLogFailure(monitor, logFailure)
		case <-monitor.outcome():
			c.stopForDurableIntent(monitor)
		case <-ticker.C:
			run, err := c.service.Run(context.Background(), monitor.runID)
			if err != nil || core.IsRunTerminal(run.State) {
				return
			}
			if run.RequestedOutcome != "" {
				c.stopForDurableIntent(monitor)
				continue
			}
			if logFailure != nil && run.StopRequestedAt == "" {
				c.stopForLogFailure(monitor, logFailure)
				continue
			}
			if run.StopRequestedAt != "" || run.TokenRevokedAt != "" {
				c.stopOwnedContainer(monitor.ref)
				continue
			}
			if deadlineReached(run.DeadlineAt) || c.runStalled(run) {
				c.stopForDurableIntent(monitor)
				continue
			}
			state, inspectErr := c.executor.Inspect(context.Background(), monitor.ref)
			switch {
			case inspectErr == nil && state.Running:
				_, _ = c.service.RecordRunHeartbeat(context.Background(), core.RunHeartbeatInput{
					RunRuntimeFactInput: runtimeFactInput(run, monitor.ref, "heartbeat"),
				})
			case errors.Is(inspectErr, containerruntime.ErrUnavailable):
				c.setDegraded(inspectErr.Error())
				return
			case errors.Is(inspectErr, containerruntime.ErrNotFound):
				_ = c.finishObservedRun(run, monitor, waitResult{err: containerruntime.ErrNotFound})
				return
			case inspectErr != nil:
				_ = c.finishObservedRun(run, monitor, waitResult{err: inspectErr})
				return
			}
		case <-c.contextDone():
			c.stopForDurableIntent(monitor)
			return
		}
	}
}

func (m *runMonitor) outcome() <-chan struct{} {
	if m.control == nil {
		return nil
	}
	return m.control.outcome
}

func (c *runtimeController) stopForLogFailure(monitor *runMonitor, failure error) {
	monitor.setLogFailure(failure)

	run, err := c.service.Run(context.Background(), monitor.runID)
	if err != nil || core.IsRunTerminal(run.State) {
		if err != nil {
			c.setDegraded(monitor.redact.Text(err.Error()))
		}
		return
	}
	if run.StopRequestedAt == "" {
		updated, _, hasIntent, intentErr := c.reconcileDurableIntent(context.Background(), run)
		if intentErr != nil {
			c.setDegraded(monitor.redact.Text(intentErr.Error()))
			return
		}
		run = updated
		if !hasIntent {
			operation, operationErr := randomRuntimeID("log-failure")
			if operationErr != nil {
				c.setDegraded(operationErr.Error())
				return
			}
			if _, err = c.service.RequestRuntimeStop(context.Background(), core.RunStopInput{
				RunID: run.ID, Reason: runtimeLogFailureReason, OperationID: operation,
				RequestID: runtimeRequest(run, "log-failure"),
			}); err != nil {
				c.setDegraded(monitor.redact.Text(err.Error()))
				return
			}
		}
	}
	c.stopOwnedContainer(monitor.ref)
}

func (c *runtimeController) stopForDurableIntent(monitor *runMonitor) {
	run, err := c.service.Run(context.Background(), monitor.runID)
	if err != nil || core.IsRunTerminal(run.State) {
		return
	}
	_, _, hasIntent, err := c.reconcileDurableIntent(context.Background(), run)
	if err != nil || !hasIntent {
		return
	}
	c.stopOwnedContainer(monitor.ref)
}

func (c *runtimeController) stopOwnedContainer(ref containerruntime.RuntimeRef) {
	stopCtx, cancel := c.runtimeStopContext()
	defer cancel()
	if _, err := c.executor.Stop(stopCtx, ref, c.shutdownGrace()); errors.Is(err, containerruntime.ErrUnavailable) {
		c.setDegraded(err.Error())
	}
}

func (c *runtimeController) finishObservedRun(previous core.Run, monitor *runMonitor, result waitResult) error {
	ctx, cancel := c.runtimeConvergenceContext()
	defer cancel()
	return c.finishObservedRunContext(ctx, previous, monitor, result)
}

func (c *runtimeController) runtimeStopContext() (context.Context, context.CancelFunc) {
	c.mu.Lock()
	shutdownCtx := c.shutdownCtx
	c.mu.Unlock()
	if shutdownCtx != nil {
		return context.WithCancel(shutdownCtx)
	}
	return context.WithTimeout(context.Background(), c.shutdownGrace()+10*time.Second)
}

func (c *runtimeController) runtimeConvergenceContext() (context.Context, context.CancelFunc) {
	c.mu.Lock()
	shutdownCtx := c.shutdownCtx
	c.mu.Unlock()
	if shutdownCtx != nil {
		return context.WithCancel(shutdownCtx)
	}
	return context.WithCancel(context.Background())
}

func (c *runtimeController) finishObservedRunContext(
	ctx context.Context,
	previous core.Run,
	monitor *runMonitor,
	result waitResult,
) error {
	run, err := c.service.Run(ctx, monitor.runID)
	if err != nil {
		if previous.ID == "" {
			return err
		}
		run = previous
	}
	if core.IsRunTerminal(run.State) {
		return c.cleanupRun(ctx, run, monitor.ref, monitor.control, monitor)
	}
	if err := afterRuntimeContractPhase(ctx, runtimePhaseProcessExited, run); err != nil {
		return err
	}
	state := core.RunExited
	reason := "CLI process exited"
	lastError := ""
	logErr := monitor.collectLogs(runtimeLogDrainTimeout)
	if errors.Is(logErr, errRuntimeLogDrainTimeout) {
		return logErr
	}
	if logErr != nil {
		monitor.setLogFailure(logErr)
	}
	runtimeCode, providerError := monitor.errorFact()
	if providerError != "" {
		lastError = providerError
	} else if logErr != nil {
		lastError = monitor.redact.Text(logErr.Error())
	}
	var exitCode *int
	if result.err == nil {
		exit := result.fact.ExitCode
		exitCode = &exit
	} else {
		state = core.RunInterrupted
		reason = "runtime lost the container without a trusted exit fact"
		lastError = monitor.redact.Text(result.err.Error())
	}
	if logErr != nil && run.RequestedOutcome == "" {
		state = core.RunInterrupted
		reason = runtimeLogFailureReason
	}
	task, taskErr := c.service.Task(ctx, run.TaskID)
	if taskErr == nil && task.Task.Status == core.TaskCancelled {
		state = core.RunCancelled
		reason = "Task cancelled"
	} else if run.StopReason == "deadline exceeded" || run.StopReason == "stalled" {
		state = core.RunTimedOut
		reason = "Run deadline exceeded"
		if run.StopReason == "stalled" {
			reason = "Run stalled"
		}
	} else if run.RequestedOutcome == "" && run.StopRequestedAt != "" {
		state = core.RunInterrupted
		reason = run.StopReason
	}
	operation := run.LaunchOperationID
	if run.StopOperationID != "" {
		operation = run.StopOperationID
	}
	terminal, terminalErr := c.service.RecordRuntimeRunTerminal(ctx, runtimeTerminalInput(run, core.RunTerminalInput{
		State: state, ExitCode: exitCode, TerminalReason: reason,
		LastError: lastError, RuntimeErrorCode: runtimeCode,
		RequestID: runtimeRequest(run, "terminal"), OperationID: operation,
	}))
	if terminalErr != nil {
		return terminalErr
	}
	if err := afterRuntimeContractPhase(ctx, runtimePhaseTerminalPersisted, terminal.Run); err != nil {
		return err
	}
	return c.cleanupRun(ctx, terminal.Run, monitor.ref, monitor.control, monitor)
}

func (c *runtimeController) failUnpreparedRun(ctx context.Context, run core.Run, code, message string) error {
	message = c.runtimeRedaction(run).Text(message)
	result, err := c.service.RecordRuntimeRunTerminal(ctx, runtimeTerminalInput(run, core.RunTerminalInput{
		State: core.RunFailed, TerminalReason: message,
		RuntimeErrorCode: code, RequestID: runtimeRequest(run, "prepare-failed"),
		OperationID: run.LaunchOperationID,
	}))
	if err != nil {
		return err
	}
	return fmt.Errorf("runtime: Run %s failed before launch: %s", result.Run.ID, message)
}

func (c *runtimeController) failPreparedRun(
	ctx context.Context,
	run core.Run,
	ref containerruntime.RuntimeRef,
	control *runControl,
	code string,
	cause error,
) error {
	message := strings.TrimSpace(c.runtimeRedaction(run).Text(cause.Error()))
	terminal, err := c.service.RecordRuntimeRunTerminal(ctx, runtimeTerminalInput(run, core.RunTerminalInput{
		State: core.RunFailed, TerminalReason: "Run launch failed",
		LastError: message, RuntimeErrorCode: code,
		RequestID: runtimeRequest(run, "launch-failed"), OperationID: run.LaunchOperationID,
	}))
	if err == nil {
		if ref.RunID == "" {
			ref = runtimeRef(terminal.Run)
		}
		_ = c.cleanupRun(context.Background(), terminal.Run, ref, control, nil)
	}
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (c *runtimeController) cleanupRun(
	ctx context.Context,
	run core.Run,
	ref containerruntime.RuntimeRef,
	control *runControl,
	monitor *runMonitor,
) error {
	if run.CleanupState == core.CleanupRemoved || run.CleanupState == core.CleanupNotNeeded {
		return nil
	}
	if monitor != nil {
		_ = monitor.cancelAndCollectLogs(2 * time.Second)
	}
	if run.CleanupState == core.CleanupBlocked {
		pending, err := c.service.RecordRunCleanup(ctx, core.RunCleanupInput{
			RunRuntimeFactInput: runtimeFactInput(run, ref, "cleanup-retry"),
			CleanupOperationID:  run.CleanupOperationID, State: core.CleanupPending,
		})
		if err != nil {
			return err
		}
		run = pending
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, c.shutdownGrace()+20*time.Second)
	defer cancel()
	state := &runtimeCleanupState{controller: c, ctx: cleanupCtx, run: run, ref: ref, control: control}
	for _, step := range runtimeCleanupSteps {
		if err := step.run(state); err != nil {
			return c.blockCleanup(run, ref, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	_, err := c.service.RecordRunCleanup(ctx, core.RunCleanupInput{
		RunRuntimeFactInput: runtimeFactInput(run, ref, "cleanup-removed"),
		CleanupOperationID:  run.CleanupOperationID, State: core.CleanupRemoved,
	})
	return err
}

func stopCleanupContainer(state *runtimeCleanupState) error {
	if state.ref.RunID == "" || state.ref.LaunchNonce == "" {
		return nil
	}
	_, err := state.controller.executor.Stop(state.ctx, state.ref, 0)
	if errors.Is(err, containerruntime.ErrNotFound) {
		return nil
	}
	return err
}

func removeCleanupContainer(state *runtimeCleanupState) error {
	if state.ref.RunID == "" || state.ref.LaunchNonce == "" {
		return nil
	}
	_, err := state.controller.executor.Remove(state.ctx, state.ref)
	return err
}

func closeCleanupControl(state *runtimeCleanupState) error {
	if state.control == nil {
		state.controller.mu.Lock()
		state.control = state.controller.controls[state.run.ID]
		state.controller.mu.Unlock()
	}
	return state.controller.closeControlContext(state.ctx, state.run.ID, state.control)
}

func removeCleanupControl(state *runtimeCleanupState) error {
	return removeRunControl(state.controller.controlRoot, state.run)
}

func (c *runtimeController) blockCleanup(run core.Run, ref containerruntime.RuntimeRef, cause error) error {
	if errors.Is(cause, containerruntime.ErrUnavailable) {
		c.setDegraded(cause.Error())
	}
	_, recordErr := c.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: runtimeFactInput(run, ref, "cleanup-blocked"),
		CleanupOperationID:  run.CleanupOperationID, State: core.CleanupBlocked,
		LastError: c.runtimeRedaction(run).Text(cause.Error()),
	})
	return errors.Join(cause, recordErr)
}

func (c *runtimeController) closeControl(runID string, control *runControl) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.closeControlContext(ctx, runID, control)
}

func (c *runtimeController) closeControlContext(ctx context.Context, runID string, control *runControl) error {
	if control == nil {
		return nil
	}
	control.closeOnce.Do(func() {
		closeErr := control.server.Close()
		var serveErr error
		select {
		case serveErr = <-control.done:
		case <-ctx.Done():
			serveErr = fmt.Errorf("run control server did not stop: %w", ctx.Err())
		}
		control.closeErr = errors.Join(closeErr, serveErr)
		close(control.closed)
	})
	<-control.closed
	c.mu.Lock()
	if c.controls[runID] == control {
		delete(c.controls, runID)
	}
	c.mu.Unlock()
	return control.closeErr
}

func removeRunControl(root string, run core.Run) error {
	root = filepath.Clean(root)
	path := filepath.Join(root, run.ID)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("run control cleanup path escaped its root")
	}
	_, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateRunControlIdentity(root, run); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func deadlineReached(value string) bool {
	if value == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !time.Now().UTC().Before(deadline)
}

// runDeadlineAt returns the absolute deadline for a launch: the task's
// self-declared budget when set, otherwise the global run_timeout backstop.
// A zero budget and zero run_timeout mean no deadline (no wall-clock cap).
func runDeadlineAt(budgetSeconds int64, runTimeout time.Duration, now time.Time) string {
	budget := time.Duration(0)
	if budgetSeconds > 0 {
		budget = time.Duration(budgetSeconds) * time.Second
	}
	if runTimeout > 0 && (budget == 0 || runTimeout < budget) {
		budget = runTimeout
	}
	if budget <= 0 {
		return ""
	}
	return now.UTC().Add(budget).Format(time.RFC3339Nano)
}

// runStalled reports whether the run has stopped making observable progress:
// no heartbeat or process observation for longer than runtime.stall_timeout.
// A zero stall_timeout disables stall detection.
func (c *runtimeController) runStalled(run core.Run) bool {
	if c.config.Runtime.StallTimeout <= 0 {
		return false
	}
	last := run.HeartbeatAt
	if last == "" {
		last = run.LastObservedAt
	}
	if last == "" {
		last = run.StartedAt
	}
	if last == "" {
		return false
	}
	observed, err := time.Parse(time.RFC3339Nano, last)
	if err != nil {
		return false
	}
	return time.Since(observed) > c.config.Runtime.StallTimeout
}

func (c *runtimeController) contextDone() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		return make(chan struct{})
	}
	return c.ctx.Done()
}
