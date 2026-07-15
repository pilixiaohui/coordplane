package core_test

import (
	"context"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestGT07DiscardRespectsRetentionBeforeAndAtBoundary(t *testing.T) {
	const retention = 24 * time.Hour
	h := newHarness(t)
	worker := h.addAgent(t, "retention-worker")
	integrator := h.addAgent(t, "retention-integrator")
	project := h.addProject(t, "retention-project", integrator.ID)
	task := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "retention")
	requireNoError(t, h.service.ReconcileGit(context.Background()))
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: integrator.ID, RequestID: "retention-accept",
	}); err != nil {
		t.Fatal(err)
	}
	requireNoError(t, h.service.ReconcileGit(context.Background()))
	task, err := h.database.Task(context.Background(), task.ID)
	requireNoError(t, err)
	closedAt, err := time.Parse(time.RFC3339Nano, task.ClosedAt)
	requireNoError(t, err)
	service, err := core.NewService(h.database, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
		CompletedWorkspaceRetention: retention, TerminalTaskRefRetention: retention,
	})
	requireNoError(t, err)
	setNextClock := func(next time.Time) {
		h.clock.mu.Lock()
		h.clock.value = next.Add(-time.Microsecond)
		h.clock.mu.Unlock()
	}

	setNextClock(closedAt.Add(retention).Add(-time.Microsecond))
	preview, err := service.GCPreview(context.Background())
	requireNoError(t, err)
	var workspaceTarget core.GCWorkspaceTarget
	for _, candidate := range preview.Workspaces {
		if candidate.TaskID == task.ID {
			workspaceTarget = candidate
		}
	}
	if workspaceTarget.Fingerprint == "" || workspaceTarget.Eligible {
		t.Fatalf("early workspace preview = %#v", workspaceTarget)
	}
	setNextClock(closedAt.Add(retention).Add(-time.Microsecond))
	earlySignature := h.durableSignature(t, project.ID)
	if result, err := service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
		TaskID: task.ID, ExpectedFingerprint: workspaceTarget.Fingerprint, RequestID: "retention-workspace-early",
	}); err == nil || result.Discarded {
		t.Fatalf("workspace age < retention discarded: result=%#v err=%v", result, err)
	}
	if after := h.durableSignature(t, project.ID); after != earlySignature {
		t.Fatal("early workspace discard changed durable state")
	}
	h.git.mu.Lock()
	workspaceCalls := h.git.discardWorkspaceCalls
	h.git.mu.Unlock()
	if workspaceCalls != 0 {
		t.Fatalf("early workspace discard calls = %d", workspaceCalls)
	}
	setNextClock(closedAt.Add(retention))
	if result, err := service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
		TaskID: task.ID, ExpectedFingerprint: workspaceTarget.Fingerprint, RequestID: "retention-workspace-boundary",
	}); err != nil || !result.Discarded {
		t.Fatalf("workspace age == retention result=%#v err=%v", result, err)
	}

	setNextClock(closedAt.Add(retention).Add(-3 * time.Microsecond))
	preview, err = service.GCPreview(context.Background())
	requireNoError(t, err)
	var refTarget core.GCTaskRefTarget
	for _, candidate := range preview.TaskRefs {
		if candidate.TaskID == task.ID {
			refTarget = candidate
		}
	}
	if refTarget.ActualSHA != task.HeadSHA || refTarget.Eligible {
		t.Fatalf("early task-ref preview = %#v", refTarget)
	}
	setNextClock(closedAt.Add(retention).Add(-time.Microsecond))
	earlySignature = h.durableSignature(t, project.ID)
	if result, err := service.GCDiscardTaskRef(context.Background(), core.GCDiscardTaskRefInput{
		TaskID: task.ID, RunID: task.HeadRunID, ExpectedSHA: task.HeadSHA, RequestID: "retention-ref-early",
	}); err == nil || result.Discarded {
		t.Fatalf("task ref age < retention discarded: result=%#v err=%v", result, err)
	}
	if after := h.durableSignature(t, project.ID); after != earlySignature {
		t.Fatal("early task-ref discard changed durable state")
	}
	setNextClock(closedAt.Add(retention))
	if result, err := service.GCDiscardTaskRef(context.Background(), core.GCDiscardTaskRefInput{
		TaskID: task.ID, RunID: task.HeadRunID, ExpectedSHA: task.HeadSHA, RequestID: "retention-ref-boundary",
	}); err != nil || !result.Discarded {
		t.Fatalf("task ref age == retention result=%#v err=%v", result, err)
	}
}

func TestGT07ConcurrentGCRequestIDConflictHasOneDestructiveSuccess(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "gc-request-worker")
	integrator := h.addAgent(t, "gc-request-integrator")
	project := h.addProject(t, "gc-request-project", integrator.ID)
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "cancelled discard target", RequestID: "gc-request-task",
	})
	requireNoError(t, err)
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, Reason: "discard", RequestID: "gc-request-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsByFingerprint := make(chan struct {
		fingerprint string
		err         error
	}, 2)
	for _, fingerprint := range []string{"workspace-fingerprint", "different-fingerprint"} {
		fingerprint := fingerprint
		go func() {
			<-start
			_, err := h.service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
				TaskID: task.ID, ExpectedFingerprint: fingerprint, RequestID: "shared-gc-request",
			})
			errorsByFingerprint <- struct {
				fingerprint string
				err         error
			}{fingerprint, err}
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		result := <-errorsByFingerprint
		if result.err == nil {
			successes++
			continue
		}
		if !core.IsCode(result.err, core.CodeVersionConflict) {
			t.Fatalf("fingerprint %s error = %v", result.fingerprint, result.err)
		}
	}
	h.git.mu.Lock()
	destructiveCalls := h.git.discardWorkspaceCalls
	h.git.mu.Unlock()
	if successes != 1 || destructiveCalls != 1 {
		t.Fatalf("GC request race successes=%d destructive_calls=%d", successes, destructiveCalls)
	}
}

func createAndSubmitCodeTask(t *testing.T, h *harness, project core.Project, worker core.Agent, head, suffix string) core.Task {
	t.Helper()
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "code " + suffix, MaxRetries: 1, RequestID: "create-" + suffix,
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "activate-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "result " + suffix,
		ExpectedHead: head, RequestID: "submit-" + suffix,
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, claim.Run.ID, "terminal-"+suffix)
	return task
}

func terminalActiveRun(t *testing.T, h *harness, runID, requestID string) {
	t.Helper()
	run, err := h.database.Run(context.Background(), runID)
	requireNoError(t, err)
	zero := 0
	if _, err := h.service.RecordRuntimeRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: core.RunExited, ExitCode: &zero, TerminalReason: "submitted",
		RequestID: requestID, OperationID: run.LaunchOperationID,
	}); err != nil {
		t.Fatal(err)
	}
}
