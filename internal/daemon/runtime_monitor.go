package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	"coordplane/internal/perfobs"
	containerruntime "coordplane/internal/runtime"
)

func (c *runtimeController) newMonitor(run core.Run, ref containerruntime.RuntimeRef, entry adapter.CLI, control *runControl) *runMonitor {
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	waitCtx, cancel := context.WithCancel(parent)
	monitor := &runMonitor{
		runID: run.ID, ref: ref, entry: entry, control: control,
		redact: c.runtimeRedaction(run), waitCancel: cancel,
		wait: make(chan waitResult, 1), logs: make(chan error, 1),
	}
	go func() {
		fact, err := c.executor.Wait(waitCtx, ref)
		monitor.wait <- waitResult{fact: fact, err: err}
		if monitor.waitDelivered != nil {
			close(monitor.waitDelivered)
		}
	}()
	go func() { monitor.logs <- c.streamLogs(waitCtx, run, ref, entry, monitor) }()
	return monitor
}

func (c *runtimeController) streamLogs(ctx context.Context, run core.Run, ref containerruntime.RuntimeRef, entry adapter.CLI, monitor *runMonitor) error {
	stream, err := c.executor.Logs(ctx, ref, true)
	if err != nil {
		return err
	}
	defer stream.Close()
	file, outputErr := os.OpenFile(run.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if file != nil {
		defer file.Close()
	}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	written := int64(0)
	contentLimit := int64(runtimeLogLimit - len(runtimeLogTruncatedMarker))
	truncated := false
	for scanner.Scan() {
		line := scanner.Bytes()
		perfobs.ClientLine(line, perfobs.Fields{
			ProjectID: run.ProjectID, TaskID: run.TaskID, RunID: run.ID, OperationID: run.LaunchOperationID,
		})
		event, parseErr := entry.ParseEvent(line)
		if parseErr != nil && bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
			return fmt.Errorf("adapter protocol frame rejected: %w", parseErr)
		}
		if parseErr == nil {
			switch event.Kind {
			case adapter.EventSessionStarted:
				if _, err := c.service.RecordRunSession(context.Background(), core.RunSessionInput{
					RunRuntimeFactInput: runtimeFactInput(run, ref, "session"), NativeSessionID: event.NativeSessionID,
				}); err != nil {
					failure := fmt.Errorf("%w: %v", errRuntimeSessionPersist, err)
					monitor.setRuntimeError(runtimeSessionFailureCode, monitor.redact.Text(failure.Error()))
					return failure
				}
			case adapter.EventResumeUnavailable:
				monitor.setRuntimeError(string(core.CodeResumeUnavailable), monitor.redact.Text(event.Message))
			case adapter.EventProviderError:
				monitor.setRuntimeError("PROVIDER_ERROR", monitor.redact.Text(event.Message))
			}
		}

		redactedLine := []byte(monitor.redact.Text(string(line)))
		if outputErr == nil && !truncated {
			remaining := contentLimit - written
			completeLine := remaining > 0 && int64(len(redactedLine)+1) <= remaining
			if remaining > 0 {
				output := redactedLine
				if !completeLine {
					output = output[:maxInt(0, int(remaining)-1)]
				}
				count, writeErr := file.Write(append(append([]byte(nil), output...), '\n'))
				written += int64(count)
				if writeErr != nil {
					outputErr = writeErr
				}
			}
			if outputErr == nil && !completeLine {
				if _, err := file.WriteString(runtimeLogTruncatedMarker); err != nil {
					outputErr = err
				} else {
					truncated = true
				}
			}
		}
	}
	return errors.Join(outputErr, scanner.Err())
}

func (m *runMonitor) setRuntimeError(code, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtimeCode == "" || code == string(core.CodeResumeUnavailable) {
		m.runtimeCode, m.lastError = code, message
	}
}

func (m *runMonitor) setLogFailure(failure error) {
	code := runtimeLogFailureCode
	if errors.Is(failure, errRuntimeSessionPersist) {
		code = runtimeSessionFailureCode
	}
	m.mu.Lock()
	m.runtimeCode, m.lastError = code, m.redact.Text(failure.Error())
	m.mu.Unlock()
}

func (m *runMonitor) errorFact() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimeCode, m.lastError
}

func (m *runMonitor) collectLogs(timeout time.Duration) error {
	m.mu.Lock()
	if m.logsDone {
		err := m.logErr
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	select {
	case err := <-m.logs:
		return m.rememberLogResult(err)
	case <-time.After(timeout):
		return errRuntimeLogDrainTimeout
	}
}

func (m *runMonitor) rememberLogResult(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.logsDone {
		m.logsDone, m.logErr = true, err
	}
	return m.logErr
}

func (m *runMonitor) cancelAndCollectLogs(timeout time.Duration) error {
	if m.waitCancel != nil {
		m.waitCancel()
	}
	return m.collectLogs(timeout)
}

func (c *runtimeController) registerControl(runID string, control *runControl) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls[runID] = control
}

func (c *runtimeController) registerMonitor(monitor *runMonitor) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runOperations[monitor.runID] == nil {
		return errors.New("runtime monitor registration requires Run operation ownership")
	}
	if c.monitors[monitor.runID] != nil {
		return errors.New("runtime monitor is already registered")
	}
	c.monitors[monitor.runID] = monitor
	return nil
}

func (c *runtimeController) unregisterMonitor(monitor *runMonitor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.monitors[monitor.runID] == monitor {
		delete(c.monitors, monitor.runID)
	}
}

func (c *runtimeController) monitor(runID string) *runMonitor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.monitors[runID]
}
