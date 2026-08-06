package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestRuntimeTerminalInterruptRequeuesTaskForResumeForStartingAndActiveRuns(t *testing.T) {
	tests := []struct {
		name     string
		activate bool
	}{
		{name: "starting"},
		{name: "active", activate: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			project, claim := createClaimedWorkRun(t, h, "interrupt-"+test.name, 0)
			var run core.Run
			if test.activate {
				run = prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), test.name)
			} else {
				run = prepareRuntimeRun(t, h, claim, t.TempDir(), test.name)
			}
			requestID := test.name + "-interrupt"

			if _, err := recordInterruptedRun(h, run, "runtime lost", requestID); err != nil {
				t.Fatal(err)
			}

			// An interruption is a resume point, not a failure: the task returns
			// to the queue so the next launch resumes the session.
			assertRetryProjection(t, h, project.ID, claim.Task.ID, claim.Run.ID, core.RunInterrupted, core.TaskQueued, 0)
			assertRetryEvents(t, h, project.ID, requestID, "run.interrupted", "task.requeued")
		})
	}
}

func TestRuntimeTerminalInterruptAtRetryLimitStillRequeuesForResume(t *testing.T) {
	h := newHarness(t)
	project, first := createClaimedWorkRun(t, h, "resume-always", 1)
	first.Run = prepareRuntimeRun(t, h, first, t.TempDir(), "first")
	firstTerminal, err := recordInterruptedRun(h, first.Run, "start lost", "first-interrupt")
	requireNoError(t, err)
	assertRetryProjection(t, h, project.ID, first.Task.ID, first.Run.ID, core.RunInterrupted, core.TaskQueued, 0)
	assertRetryEvents(t, h, project.ID, "first-interrupt", "run.interrupted", "task.requeued")
	recordCleanupRemoved(t, h, firstTerminal.Run, "first-cleanup")

	h.clock.Advance(time.Second)
	second, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("second claim: ok=%v err=%v", ok, err)
	}
	second.Run = prepareAndActivateRuntimeRun(t, h, second, t.TempDir(), "second")
	if _, err := recordInterruptedRun(h, second.Run, "process lost", "second-interrupt"); err != nil {
		t.Fatal(err)
	}
	// At retryCount == maxRetries the interrupt is still a resume point: the
	// retry budget is not consumed by interruption at all, the task returns to
	// the queue so the next launch resumes the session.
	assertRetryProjection(t, h, project.ID, first.Task.ID, second.Run.ID, core.RunInterrupted, core.TaskQueued, 0)
	assertRetryEvents(t, h, project.ID, "second-interrupt", "run.interrupted", "task.requeued")

	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	requireNoError(t, err)
	if countEvent(events, "task.requeued") != 2 || countEvent(events, "task.failed") != 0 ||
		countEvent(events, "run.interrupted") != 2 {
		t.Fatalf("interrupt resume events = %#v", events)
	}
}

func TestRuntimeTerminalUsesRetryBudgetAndBackoffExactlyOnce(t *testing.T) {
	h := newHarness(t)
	project, first := createClaimedWorkRun(t, h, "one-retry", 1)
	first.Run = prepareRuntimeRun(t, h, first, t.TempDir(), "first")
	firstTerminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: first.Run.ID, State: core.RunExited, ExitCode: intPointer(1),
		TerminalReason: "no task outcome", RequestID: "first-exit",
	})
	requireNoError(t, err)
	assertRetryProjection(t, h, project.ID, first.Task.ID, first.Run.ID, core.RunExited, core.TaskQueued, 1)
	assertRetryEvents(t, h, project.ID, "first-exit", "run.exited", "task.requeued")
	recordCleanupRemoved(t, h, firstTerminal.Run, "first-cleanup")

	h.clock.Advance(time.Second)
	second, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("second claim: ok=%v err=%v", ok, err)
	}
	second.Run = prepareAndActivateRuntimeRun(t, h, second, t.TempDir(), "second")
	if _, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: second.Run.ID, State: core.RunExited, ExitCode: intPointer(1),
		TerminalReason: "no task outcome again", RequestID: "second-exit",
	}); err != nil {
		t.Fatal(err)
	}
	// An exit without an outcome exhausts the retry budget exactly once.
	assertRetryProjection(t, h, project.ID, first.Task.ID, second.Run.ID, core.RunExited, core.TaskFailed, 1)
	assertRetryEvents(t, h, project.ID, "second-exit", "run.exited", "task.failed")

	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	requireNoError(t, err)
	if countEvent(events, "task.requeued") != 1 || countEvent(events, "task.failed") != 1 ||
		countEvent(events, "run.exited") != 2 {
		t.Fatalf("retry terminal events = %#v", events)
	}
}

func TestRuntimeTerminalBoundsReasonAndReplaysWithoutDurableSideEffects(t *testing.T) {
	h := newHarness(t)
	project, claim := createActiveWorkRun(t, h, "bounded-interrupt", 0)
	largeReason := strings.Repeat("\x01", core.MaximumEventPayloadBytes)
	if _, err := recordInterruptedRun(h, claim.Run, largeReason, "bounded-interrupt"); err != nil {
		t.Fatalf("large interrupt reason did not converge: %v", err)
	}

	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	requireNoError(t, err)
	run := runWithID(t, snapshot, claim.Run.ID)
	if run.State != core.RunInterrupted || len(run.TerminalReason) > core.MaximumTerminalTextBytes ||
		!strings.HasSuffix(run.TerminalReason, "...[truncated]") || run.LastError != "" || run.ExitCode != nil {
		t.Fatalf("bounded interrupt run = %#v", run)
	}
	beforeReplay := durableSignature(t, h.database, project.ID)
	if _, err := recordInterruptedRun(h, claim.Run, largeReason, "bounded-interrupt-replay"); err != nil {
		t.Fatalf("idempotent interrupt replay: %v", err)
	}
	h.requireDurableSignature(t, project.ID, beforeReplay)
	if _, err := recordInterruptedRun(h, claim.Run, "different reason", "changed-interrupt"); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("changed terminal fact error = %v, want %s", err, core.CodeInvalidState)
	}
	h.requireDurableSignature(t, project.ID, beforeReplay)
}

func createClaimedWorkRun(t *testing.T, h *harness, name string, maxRetries int) (core.Project, core.Claim) {
	t.Helper()
	agent := h.addAgent(t, name+"-agent")
	project := h.addProject(t, name+"-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: name, MaxRetries: maxRetries, RequestID: name + "-task",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claim.Task.ID != task.ID {
		t.Fatalf("claimed %s, want %s", claim.Task.ID, task.ID)
	}
	return project, claim
}

func createActiveWorkRun(t *testing.T, h *harness, name string, maxRetries int) (core.Project, core.Claim) {
	t.Helper()
	project, claim := createClaimedWorkRun(t, h, name, maxRetries)
	claim.Run = prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), name)
	return project, claim
}

func recordInterruptedRun(h *harness, run core.Run, reason, requestID string) (core.RunTerminalResult, error) {
	input := runtimeTerminalInput(run, core.RunInterrupted, requestID)
	input.TerminalReason = reason
	return h.service.RecordRuntimeRunTerminal(context.Background(), input)
}

func recordCleanupRemoved(t *testing.T, h *harness, run core.Run, requestID string) {
	t.Helper()
	fact := runtimeFact(run, run.ContainerID)
	fact.RequestID = requestID
	if _, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: fact, CleanupOperationID: run.CleanupOperationID, State: core.CleanupRemoved,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRetryProjection(t *testing.T, h *harness, projectID, taskID, runID string, runState core.RunState, status core.TaskStatus, retryCount int) {
	t.Helper()
	snapshot, err := h.database.Snapshot(context.Background(), projectID)
	requireNoError(t, err)
	task := taskWithID(t, snapshot, taskID)
	run := runWithID(t, snapshot, runID)
	if task.Status != status || task.RetryCount != retryCount || task.CurrentRunID != "" {
		t.Fatalf("retry task = %#v, want status=%s count=%d and no current run", task, status, retryCount)
	}
	if run.State != runState || run.TokenRevokedAt == "" || run.EndedAt == "" {
		t.Fatalf("terminal run = %#v", run)
	}
	if status == core.TaskQueued && task.NextRunAt <= run.EndedAt {
		t.Fatalf("retry has no backoff: next_run_at=%s ended_at=%s", task.NextRunAt, run.EndedAt)
	}
}

func assertRetryEvents(t *testing.T, h *harness, projectID, requestID, runKind, taskKind string) {
	t.Helper()
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: projectID})
	requireNoError(t, err)
	kinds := map[string]int{}
	for _, event := range events {
		if event.RequestID == requestID {
			kinds[event.Kind]++
		}
	}
	if kinds[runKind] != 1 || kinds[taskKind] != 1 || len(kinds) != 2 {
		t.Fatalf("retry request %s Events = %#v", requestID, kinds)
	}
}

func taskWithID(t *testing.T, snapshot core.Snapshot, id string) core.Task {
	t.Helper()
	for _, task := range snapshot.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %s not found in %#v", id, snapshot.Tasks)
	return core.Task{}
}

func runWithID(t *testing.T, snapshot core.Snapshot, id string) core.Run {
	t.Helper()
	for _, run := range snapshot.Runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("run %s not found in %#v", id, snapshot.Runs)
	return core.Run{}
}
