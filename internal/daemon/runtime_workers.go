package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

const (
	runtimeShutdownLimit  = 30 * time.Second
	runtimeShutdownPoll   = 50 * time.Millisecond
	runtimeShutdownReason = "daemon_shutdown"
)

type runtimeWorker struct {
	name string
	run  func(context.Context, *runtimeController)
}

var runtimeWorkers = []runtimeWorker{
	{name: "scheduler", run: runScheduler},
	{name: "notifier", run: runNotifier},
	{name: "supervisor", run: runSupervisor},
	{name: "reconciler", run: runReconciler},
	{name: "gc", run: runRuntimeGC},
}

func runScheduler(ctx context.Context, controller *runtimeController) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			healthy, _ := controller.Healthy()
			if !healthy {
				continue
			}
			claim, operation, ok, err := controller.claimNext(ctx)
			if err != nil || !ok {
				continue
			}
			_ = controller.launchOwned(ctx, claim, operation)
		}
	}
}

func runNotifier(ctx context.Context, controller *runtimeController) {
	// The production one-shot adapter has no Inject path. Durable Message wake
	// state is consumed by scheduler input; this worker remains the static owner
	// of any future live-adapter notification optimization.
	<-ctx.Done()
}

func runSupervisor(ctx context.Context, controller *runtimeController) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			controller.mu.Lock()
			monitors := make([]*runMonitor, 0, len(controller.monitors))
			for _, monitor := range controller.monitors {
				monitors = append(monitors, monitor)
			}
			controller.mu.Unlock()
			for _, monitor := range monitors {
				run, err := controller.service.Run(ctx, monitor.runID)
				if err == nil && monitor.control != nil && (run.RequestedOutcome != "" || run.StopRequestedAt != "") {
					select {
					case monitor.control.outcome <- struct{}{}:
					default:
					}
				}
			}
		}
	}
}

func runReconciler(ctx context.Context, controller *runtimeController) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := controller.Reconcile(ctx); err != nil {
				controller.setDegraded(err.Error())
			}
		}
	}
}

func runRuntimeGC(ctx context.Context, controller *runtimeController) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			controller.cleanupTerminalRuns(ctx)
		}
	}
}

func (c *runtimeController) Reconcile(ctx context.Context) error {
	if err := c.executor.Ping(ctx); err != nil {
		if errors.Is(err, containerruntime.ErrUnavailable) {
			c.setDegraded(err.Error())
			return nil
		}
		return err
	}
	c.claimMu.Lock()
	liveRuns, err := c.service.LiveRuns(ctx)
	c.claimMu.Unlock()
	if err != nil {
		return err
	}
	for _, run := range liveRuns {
		operation := c.acquireRunOperation(run.ID)
		if operation == nil {
			continue
		}
		operationErr := func() error {
			defer c.releaseRunOperation(run.ID, operation)
			return c.reconcileOwnedRun(ctx, run)
		}()
		if operationErr != nil {
			wrapped := fmt.Errorf("reconcile Run %s: %w", run.ID, operationErr)
			c.setDegraded(wrapped.Error())
			if errors.Is(operationErr, containerruntime.ErrUnavailable) || errors.Is(operationErr, containerruntime.ErrOwnership) {
				return nil
			}
			return wrapped
		}
	}
	c.cleanupTerminalRuns(ctx)
	if err := c.detectOrphans(ctx); err != nil {
		c.setDegraded(err.Error())
		return nil
	}
	c.clearDegraded()
	return nil
}

func (c *runtimeController) reconcileOwnedRun(ctx context.Context, snapshot core.Run) error {
	run, err := c.service.Run(ctx, snapshot.ID)
	if err != nil {
		return err
	}
	if core.IsRunTerminal(run.State) {
		return nil
	}
	if run.LaunchNonce == "" {
		if _, err := c.service.RecordRuntimeRunTerminal(ctx, core.RunTerminalInput{
			RunID: run.ID, Generation: run.Generation, LaunchOperationID: run.LaunchOperationID,
			State: core.RunFailed, TerminalReason: "starting Run has no launch nonce",
			RuntimeErrorCode: "LAUNCH_INTENT_INCOMPLETE", RequestID: runtimeRequest(run, "reconcile-unprepared"),
			OperationID: run.LaunchOperationID,
		}); err != nil {
			return fmt.Errorf("reconcile unprepared Run %s: %w", run.ID, err)
		}
		return nil
	}
	return c.reconcileRun(ctx, run)
}

func (c *runtimeController) claimNext(ctx context.Context) (core.Claim, *runOperation, bool, error) {
	c.claimMu.Lock()
	defer c.claimMu.Unlock()
	c.mu.Lock()
	shuttingDown := c.shuttingDown
	c.mu.Unlock()
	if shuttingDown {
		return core.Claim{}, nil, false, nil
	}
	claim, ok, err := c.service.ClaimNextForAdapters(ctx, "", c.adapters.Names())
	if err != nil || !ok {
		return claim, nil, ok, err
	}
	operation := c.acquireRunOperation(claim.Run.ID)
	if operation == nil {
		return core.Claim{}, nil, false, errors.New("claimed Run already has a runtime owner")
	}
	return claim, operation, true, nil
}

func (c *runtimeController) reconcileRun(ctx context.Context, run core.Run) error {
	ref := runtimeRef(run)
	state, err := c.executor.Inspect(ctx, ref)
	if errors.Is(err, containerruntime.ErrNotFound) {
		updated, task, hasIntent, intentErr := c.reconcileDurableIntent(ctx, run)
		if intentErr != nil {
			return intentErr
		}
		if hasIntent {
			run = updated
			terminalState, reason := reconcileIntentTerminal(run, task)
			return c.finishMissingReconciledRun(ctx, run, ref, terminalState, reason)
		}
		terminalState := core.RunFailed
		reason := "container was not created"
		if run.LaunchPhase == core.LaunchStartIssued || run.LaunchPhase == core.LaunchProcessObserved || run.State == core.RunActive {
			terminalState = core.RunInterrupted
			reason = "owned container is missing after start was issued"
		}
		return c.finishMissingReconciledRun(ctx, run, ref, terminalState, reason)
	}
	if err != nil {
		return err
	}
	ref = state.Ref
	if run.ContainerID == "" {
		created, recordErr := c.service.RecordContainerCreated(ctx, runtimeFactInput(run, ref, "reconcile-created"))
		if recordErr != nil {
			return recordErr
		}
		run = created
	}
	if err := c.validateAdoptedContainer(ctx, run, state); err != nil {
		return err
	}
	entry, ok := c.adapters.Lookup(run.AdapterID)
	if !ok {
		return fmt.Errorf("adapter %q is not registered", run.AdapterID)
	}
	updated, _, hasIntent, err := c.reconcileDurableIntent(ctx, run)
	if err != nil {
		return err
	}
	if hasIntent {
		return c.stopReconciledRun(ctx, updated, ref, state, entry)
	}
	control, err := c.rebuildControl(ctx, run)
	if err != nil {
		return err
	}
	switch state.Status {
	case containerruntime.StatusExited:
		monitor := c.newMonitor(run, ref, entry, control)
		result := <-monitor.wait
		return c.finishObservedRun(run, monitor, result)
	case containerruntime.StatusCreated:
		if run.LaunchPhase == core.LaunchCreated {
			started, recordErr := c.service.RecordRunStartIssued(ctx, runtimeFactInput(run, ref, "reconcile-start"))
			if recordErr != nil {
				return recordErr
			}
			run = started
		}
		if run.LaunchPhase != core.LaunchStartIssued {
			return errors.New("created container has an incompatible durable launch phase")
		}
		if _, err := c.executor.Attach(ctx, ref); err != nil {
			return err
		}
		if _, err := c.executor.Start(ctx, ref); err != nil {
			return err
		}
		return c.adoptRunning(ctx, run, ref, entry, control)
	case containerruntime.StatusRunning:
		return c.adoptRunning(ctx, run, ref, entry, control)
	default:
		return errors.New("Docker returned an unsupported container state")
	}
}

func (c *runtimeController) reconcileDurableIntent(ctx context.Context, run core.Run) (core.Run, core.Task, bool, error) {
	projection, err := c.service.Task(ctx, run.TaskID)
	if err != nil {
		return core.Run{}, core.Task{}, false, err
	}
	task := projection.Task
	hasIntent := run.StopRequestedAt != "" || run.RequestedOutcome != "" ||
		task.Status == core.TaskCancelled || deadlineReached(run.DeadlineAt)
	if !hasIntent || run.StopRequestedAt != "" {
		return run, task, hasIntent, nil
	}

	reason := "task outcome recorded"
	switch {
	case task.Status == core.TaskCancelled:
		reason = "Task cancelled"
	case deadlineReached(run.DeadlineAt):
		reason = "deadline exceeded"
	}
	operation, err := randomRuntimeID("reconcile-stop")
	if err != nil {
		return core.Run{}, core.Task{}, false, err
	}
	updated, err := c.service.RequestRuntimeStop(ctx, core.RunStopInput{
		RunID: run.ID, Reason: reason, OperationID: operation,
		RequestID: runtimeRequest(run, "reconcile-stop"),
	})
	if err != nil {
		return core.Run{}, core.Task{}, false, err
	}
	return updated, task, true, nil
}

func reconcileIntentTerminal(run core.Run, task core.Task) (core.RunState, string) {
	switch {
	case task.Status == core.TaskCancelled:
		return core.RunCancelled, "Task cancelled"
	case run.StopReason == "deadline exceeded":
		return core.RunTimedOut, "Run deadline exceeded"
	default:
		reason := run.StopReason
		if reason == "" {
			reason = "durable stop intent observed during reconciliation"
		}
		return core.RunInterrupted, reason
	}
}

func (c *runtimeController) finishMissingReconciledRun(
	ctx context.Context,
	run core.Run,
	ref containerruntime.RuntimeRef,
	state core.RunState,
	reason string,
) error {
	operation := run.LaunchOperationID
	if run.StopOperationID != "" {
		operation = run.StopOperationID
	}
	terminal, err := c.service.RecordRuntimeRunTerminal(ctx, core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: state, TerminalReason: reason,
		RuntimeErrorCode: "CONTAINER_NOT_FOUND", RequestID: runtimeRequest(run, "reconcile-not-found"),
		OperationID: operation,
	})
	if err != nil {
		return err
	}
	return c.cleanupRun(ctx, terminal.Run, ref, nil, nil)
}

func (c *runtimeController) stopReconciledRun(
	ctx context.Context,
	run core.Run,
	ref containerruntime.RuntimeRef,
	state containerruntime.LiveState,
	entry adapter.CLI,
) error {
	if state.Status == containerruntime.StatusCreated {
		stopCtx, cancel := context.WithTimeout(context.Background(), runtimeStopGrace+10*time.Second)
		_, stopErr := c.executor.Stop(stopCtx, ref, runtimeStopGrace)
		cancel()
		if stopErr != nil {
			return stopErr
		}
		projection, err := c.service.Task(ctx, run.TaskID)
		if err != nil {
			return err
		}
		terminalState, reason := reconcileIntentTerminal(run, projection.Task)
		operation := run.StopOperationID
		if operation == "" {
			operation = run.LaunchOperationID
		}
		terminal, err := c.service.RecordRuntimeRunTerminal(ctx, core.RunTerminalInput{
			RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
			LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
			State: terminalState, TerminalReason: reason,
			RequestID: runtimeRequest(run, "reconcile-terminal"), OperationID: operation,
		})
		if err != nil {
			return err
		}
		return c.cleanupRun(ctx, terminal.Run, ref, nil, nil)
	}

	monitor := c.newMonitor(run, ref, entry, nil)
	if err := c.registerMonitor(monitor); err != nil {
		_ = monitor.cancelAndCollectLogs(2 * time.Second)
		return err
	}
	defer c.unregisterMonitor(monitor)
	if state.Status == containerruntime.StatusRunning {
		stopCtx, cancel := context.WithTimeout(context.Background(), runtimeStopGrace+10*time.Second)
		_, stopErr := c.executor.Stop(stopCtx, ref, runtimeStopGrace)
		cancel()
		if stopErr != nil {
			_ = monitor.cancelAndCollectLogs(2 * time.Second)
			return stopErr
		}
	}
	select {
	case result := <-monitor.wait:
		return c.finishObservedRun(run, monitor, result)
	case <-time.After(runtimeStopGrace + 10*time.Second):
		_ = monitor.cancelAndCollectLogs(2 * time.Second)
		return errors.New("timed out waiting for reconciled container to stop")
	}
}

func (c *runtimeController) adoptRunning(
	ctx context.Context,
	run core.Run,
	ref containerruntime.RuntimeRef,
	entry adapter.CLI,
	control *runControl,
) error {
	monitor := c.newMonitor(run, ref, entry, control)
	if err := c.registerMonitor(monitor); err != nil {
		_ = monitor.cancelAndCollectLogs(2 * time.Second)
		return err
	}
	if run.State == core.RunStarting {
		if run.LaunchPhase != core.LaunchStartIssued {
			c.unregisterMonitor(monitor)
			_ = monitor.cancelAndCollectLogs(2 * time.Second)
			return errors.New("running container lacks start_issued durable phase")
		}
		if _, err := c.service.ObserveProcessAndActivateRun(ctx, runtimeFactInput(run, ref, "reconcile-active")); err != nil {
			c.unregisterMonitor(monitor)
			_ = monitor.cancelAndCollectLogs(2 * time.Second)
			return err
		}
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.supervise(monitor)
	}()
	return nil
}

func (c *runtimeController) rebuildControl(ctx context.Context, run core.Run) (*runControl, error) {
	c.mu.Lock()
	if existing := c.controls[run.ID]; existing != nil {
		c.mu.Unlock()
		return existing, nil
	}
	c.mu.Unlock()
	path := filepath.Join(c.controlRoot, run.ID)
	if err := validateRunControl(ctx, c.controlRoot, run, c.service.AuthorizeRunScope); err != nil {
		return nil, err
	}
	control, err := c.openRunControl(run, path)
	if err != nil {
		return nil, err
	}
	c.registerControl(run.ID, control)
	return control, nil
}

func (c *runtimeController) cleanupTerminalRuns(ctx context.Context) {
	runs, err := c.service.RunsNeedingCleanup(ctx)
	if err != nil {
		return
	}
	for _, run := range runs {
		operation := c.acquireRunOperation(run.ID)
		if operation == nil {
			continue
		}
		func() {
			defer c.releaseRunOperation(run.ID, operation)
			_ = c.cleanupRun(ctx, run, runtimeRef(run), nil, nil)
		}()
	}
}

func (c *runtimeController) detectOrphans(ctx context.Context) error {
	states, err := c.executor.Managed(ctx)
	if err != nil {
		return err
	}
	liveAgents := make(map[string]string)
	for _, state := range states {
		run, runErr := c.service.Run(ctx, state.Ref.RunID)
		if core.IsCode(runErr, core.CodeNotFound) {
			return fmt.Errorf("managed orphan container %s requires manual quarantine", state.Ref.ContainerName)
		}
		if runErr != nil {
			return runErr
		}
		if err := containerruntime.ValidateOwnership(runtimeRef(run), state.Ref); err != nil {
			return err
		}
		if state.Running {
			if previous := liveAgents[run.AgentID]; previous != "" && previous != run.ID {
				return fmt.Errorf("Agent %s has multiple live owned containers", run.AgentID)
			}
			liveAgents[run.AgentID] = run.ID
		}
	}
	return nil
}

func (c *runtimeController) Shutdown(ctx context.Context) error {
	c.stopRuntimeAdmission()
	ctx, cancel := boundedRuntimeShutdownContext(ctx)
	defer cancel()
	c.mu.Lock()
	c.shutdownCtx = ctx
	c.mu.Unlock()

	graceErr := c.waitForNaturalShutdown(ctx)
	intentErr := c.persistShutdownIntents(ctx)
	c.cancelRuntimeWorkers()
	workerErr := c.waitForRuntimeWorkers(ctx)
	convergeErr := c.convergeShutdownRuns(ctx)

	c.mu.Lock()
	controls := make(map[string]*runControl, len(c.controls))
	for id, control := range c.controls {
		controls[id] = control
	}
	c.mu.Unlock()
	var controlErr error
	for id, control := range controls {
		controlErr = errors.Join(controlErr, c.closeControlContext(ctx, id, control))
	}
	return errors.Join(graceErr, intentErr, workerErr, convergeErr, controlErr)
}

func (c *runtimeController) stopRuntimeAdmission() {
	// claimMu closes the gap between the scheduler's admission check and the
	// transaction that creates a Run.
	c.claimMu.Lock()
	c.mu.Lock()
	c.shuttingDown = true
	c.mu.Unlock()
	c.claimMu.Unlock()
}

func boundedRuntimeShutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if _, ok := parent.Deadline(); ok {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, runtimeShutdownLimit)
}

func runtimeNaturalShutdownGrace(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return runtimeStopGrace
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	grace := remaining / 4
	if grace > runtimeStopGrace {
		grace = runtimeStopGrace
	}
	return grace
}

func (c *runtimeController) waitForNaturalShutdown(ctx context.Context) error {
	grace := runtimeNaturalShutdownGrace(ctx)
	if grace <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(runtimeShutdownPoll)
	defer ticker.Stop()
	for {
		liveRuns, err := c.service.LiveRuns(ctx)
		if err != nil {
			return err
		}
		if len(liveRuns) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

func (c *runtimeController) persistShutdownIntents(ctx context.Context) error {
	liveRuns, err := c.service.LiveRuns(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, snapshot := range liveRuns {
		run, readErr := c.service.Run(ctx, snapshot.ID)
		if readErr != nil {
			result = errors.Join(result, fmt.Errorf("read shutdown Run %s: %w", snapshot.ID, readErr))
			continue
		}
		if core.IsRunTerminal(run.State) || run.StopRequestedAt != "" {
			continue
		}
		if run.RequestedOutcome == "" {
			updated, _, hasIntent, intentErr := c.reconcileDurableIntent(ctx, run)
			if intentErr != nil {
				result = errors.Join(result, fmt.Errorf("persist existing stop intent for Run %s: %w", run.ID, intentErr))
				continue
			}
			if hasIntent || updated.StopRequestedAt != "" {
				continue
			}
		}
		operation, operationErr := randomRuntimeID("shutdown")
		if operationErr != nil {
			result = errors.Join(result, operationErr)
			continue
		}
		_, stopErr := c.service.RequestRuntimeStop(ctx, core.RunStopInput{
			RunID: run.ID, Reason: runtimeShutdownReason, OperationID: operation,
			RequestID: runtimeRequest(run, "shutdown"),
		})
		if stopErr != nil {
			latest, latestErr := c.service.Run(ctx, run.ID)
			if latestErr == nil && (core.IsRunTerminal(latest.State) || latest.StopRequestedAt != "") {
				continue
			}
			result = errors.Join(result, fmt.Errorf("persist shutdown stop for Run %s: %w", run.ID, stopErr))
		}
	}
	return result
}

func (c *runtimeController) cancelRuntimeWorkers() {
	c.mu.Lock()
	if c.cancel == nil {
		c.ctx, c.cancel = context.WithCancel(context.Background())
	}
	cancel := c.cancel
	monitors := make([]*runMonitor, 0, len(c.monitors))
	for _, monitor := range c.monitors {
		monitors = append(monitors, monitor)
	}
	c.mu.Unlock()
	cancel()
	// Reconciliation may create monitors before Start installs c.ctx. Cancel
	// their own Wait contexts as well so shutdown can always join them.
	for _, monitor := range monitors {
		if monitor.waitCancel != nil {
			monitor.waitCancel()
		}
	}
}

func (c *runtimeController) convergeShutdownRuns(ctx context.Context) error {
	liveRuns, err := c.service.LiveRuns(ctx)
	if err != nil {
		return err
	}
	if len(liveRuns) == 0 {
		return nil
	}
	limit := c.config.MaxParallelRuns
	if limit < 1 {
		limit = 1
	}
	if limit > len(liveRuns) {
		limit = len(liveRuns)
	}
	jobs := make(chan core.Run)
	results := make(chan error, len(liveRuns))
	for range limit {
		go func() {
			for snapshot := range jobs {
				results <- c.convergeShutdownRun(ctx, snapshot)
			}
		}()
	}
	for _, snapshot := range liveRuns {
		jobs <- snapshot
	}
	close(jobs)
	var result error
	for range liveRuns {
		result = errors.Join(result, <-results)
	}
	return result
}

func (c *runtimeController) convergeShutdownRun(ctx context.Context, snapshot core.Run) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	operation := c.acquireRunOperation(snapshot.ID)
	if operation == nil {
		return fmt.Errorf("shutdown Run %s still has a runtime owner", snapshot.ID)
	}
	defer c.releaseRunOperation(snapshot.ID, operation)
	run, err := c.service.Run(ctx, snapshot.ID)
	if err != nil {
		return err
	}
	if core.IsRunTerminal(run.State) {
		return nil
	}
	return c.shutdownRun(ctx, run)
}

func (c *runtimeController) waitForRuntimeWorkers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *runtimeController) shutdownRun(ctx context.Context, run core.Run) error {
	if run.StopRequestedAt == "" {
		operation, err := randomRuntimeID("shutdown")
		if err != nil {
			return err
		}
		updated, err := c.service.RequestRuntimeStop(ctx, core.RunStopInput{
			RunID: run.ID, Reason: runtimeShutdownReason, OperationID: operation,
			RequestID: runtimeRequest(run, "shutdown"),
		})
		if err != nil {
			return err
		}
		run = updated
	}
	if run.LaunchNonce == "" {
		operation := run.StopOperationID
		if operation == "" {
			operation = run.LaunchOperationID
		}
		terminal, err := c.service.RecordRuntimeRunTerminal(ctx, core.RunTerminalInput{
			RunID: run.ID, Generation: run.Generation, LaunchOperationID: run.LaunchOperationID,
			State: core.RunFailed, TerminalReason: "daemon shutdown before runtime launch was prepared",
			RuntimeErrorCode: "DAEMON_SHUTDOWN", RequestID: runtimeRequest(run, "shutdown-terminal"),
			OperationID: operation,
		})
		if err != nil {
			return err
		}
		return c.cleanupRun(ctx, terminal.Run, runtimeRef(terminal.Run), nil, nil)
	}
	ref := runtimeRef(run)
	grace := runtimeStopGrace
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < grace {
			grace = max(time.Duration(0), remaining)
		}
	}
	_, stopErr := c.executor.Stop(ctx, ref, grace)
	if stopErr != nil && !errors.Is(stopErr, containerruntime.ErrNotFound) {
		return stopErr
	}
	exit, waitErr := c.executor.Wait(ctx, ref)
	entry, ok := c.adapters.Lookup(run.AdapterID)
	if !ok {
		return fmt.Errorf("shutdown Run %s adapter %q is not registered", run.ID, run.AdapterID)
	}
	c.mu.Lock()
	control := c.controls[run.ID]
	c.mu.Unlock()
	monitor := &runMonitor{
		runID: run.ID, ref: ref, entry: entry, control: control,
		redact: c.runtimeRedaction(run), logs: make(chan error, 1),
	}
	monitor.logs <- c.streamLogs(ctx, run, ref, entry, monitor)
	return c.finishObservedRunContext(ctx, run, monitor, waitResult{fact: exit, err: waitErr})
}

func (c *runtimeController) Close() error {
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Shutdown(ctx)
}
