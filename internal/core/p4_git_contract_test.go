package core_test

import (
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
)

func TestGT02ToGT04CaptureAndDirectAdvanceConvergeFromDurableIntents(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "git-worker")
	integrator := h.addAgent(t, "git-integrator")
	project := h.addProject(t, "git-direct", integrator.ID)
	task := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "direct")

	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	captured, err := h.database.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Status != core.TaskSubmitted || captured.HeadSHA == "" || captured.TaskRef == "" || captured.PendingAction != "" {
		t.Fatalf("captured task = %#v", captured)
	}
	accepted, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: integrator.ID, RequestID: "accept-direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.PendingAction != "advance" || accepted.AcceptedIntegrationAgentID != integrator.ID {
		t.Fatalf("accepted task = %#v", accepted)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := h.database.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != core.TaskCompleted || completed.FinalCanonicalSHA != captured.HeadSHA || completed.PendingAction != "" {
		t.Fatalf("completed task = %#v", completed)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 {
		t.Fatalf("reconcile replay created tasks: %#v", snapshot.Tasks)
	}
}

func TestCT05SourceTaskCreationAndCheckoutCopyExactCapturedIdentity(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "source-worker")
	integrator := h.addAgent(t, "source-integrator")
	project := h.addProject(t, "source-project", integrator.ID)
	source := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "source")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	source, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: integrator.ID, Kind: core.TaskWork,
		Title: "review exact source", SourceTaskID: source.ID, RequestID: "create-source-review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if derived.SourceTaskID != source.ID || derived.SourceRunID != source.HeadRunID ||
		derived.SourceTaskRef != source.TaskRef || derived.SourceHeadSHA != source.HeadSHA {
		t.Fatalf("derived source identity = %#v; source = %#v", derived, source)
	}
	destination := filepath.Join(t.TempDir(), "review")
	checkout, err := h.service.CheckoutTask(context.Background(), core.TaskCheckoutInput{
		TaskID: source.ID, Destination: destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkout.HeadSHA != source.HeadSHA || checkout.Destination != destination {
		t.Fatalf("checkout = %#v", checkout)
	}
}

func TestGT05StaleCreatesOneIntegrationTaskAndFinalCASCompletesBoth(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "stale-worker")
	integrator := h.addAgent(t, "stale-integrator")
	project := h.addProject(t, "git-stale", integrator.ID)
	source := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "stale")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: source.ID, IntegrationAgentID: integrator.ID, RequestID: "accept-stale",
	}); err != nil {
		t.Fatal(err)
	}
	h.git.mu.Lock()
	h.git.advanceOutcome = core.GitAdvanceStale
	h.git.advanceActual = "cccccccccccccccccccccccccccccccccccccccc"
	h.git.sha = h.git.advanceActual
	h.git.mu.Unlock()
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 2 {
		t.Fatalf("stale task count = %d, want source + one integration: %#v", len(snapshot.Tasks), snapshot.Tasks)
	}
	var integration core.Task
	for _, candidate := range snapshot.Tasks {
		if candidate.Kind == core.TaskIntegration {
			integration = candidate
		}
	}
	if integration.ID == "" || integration.SourceTaskID != source.ID || integration.AssigneeAgentID != integrator.ID || integration.SourceTaskRef == "" {
		t.Fatalf("integration task = %#v", integration)
	}
	persistedSource, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedSource.IntegrationTaskID != integration.ID || persistedSource.Status != core.TaskSubmitted {
		t.Fatalf("linked source = %#v", persistedSource)
	}

	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != integration.ID {
		t.Fatalf("integration claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "activate-integration"); err != nil {
		t.Fatal(err)
	}
	integrationHead := "dddddddddddddddddddddddddddddddddddddddd"
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "integrated", ExpectedHead: integrationHead,
		RequestID: "submit-integration",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, claim.Run.ID, "terminal-integration")
	terminalRun, err := h.database.Run(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recordCleanupRemoved(t, h, terminalRun, "cleanup-integration")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.git.mu.Lock()
	h.git.advanceOutcome = core.GitAdvanceStale
	h.git.advanceActual = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	h.git.sha = h.git.advanceActual
	h.git.mu.Unlock()
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	requeued, err := h.database.Task(context.Background(), integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != core.TaskQueued || requeued.PendingAction != "" {
		t.Fatalf("second-stale integration = %#v", requeued)
	}
	afterSecondStale, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSecondStale.Tasks) != 2 {
		t.Fatalf("second stale created nested integration: %#v", afterSecondStale.Tasks)
	}
	linkedSource, err := h.database.Task(context.Background(), source.ID)
	if err != nil || linkedSource.IntegrationTaskID != integration.ID {
		t.Fatalf("source link after second stale = %#v err=%v", linkedSource, err)
	}

	secondClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || secondClaim.Task.ID != integration.ID {
		t.Fatalf("requeued integration claim = %#v ok=%t err=%v", secondClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), secondClaim.Run.ID, "activate-integration-retry"); err != nil {
		t.Fatal(err)
	}
	integrationHead = "ffffffffffffffffffffffffffffffffffffffff"
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: secondClaim.Token, Outcome: core.OutcomeSubmit, Summary: "integrated retry", ExpectedHead: integrationHead,
		RequestID: "submit-integration-retry",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, secondClaim.Run.ID, "terminal-integration-retry")
	h.git.mu.Lock()
	h.git.advanceOutcome = core.GitAdvanceUpdated
	h.git.advanceActual = ""
	h.git.mu.Unlock()
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}

	completedSource, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedIntegration, err := h.database.Task(context.Background(), integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedSource.Status != core.TaskCompleted || completedIntegration.Status != core.TaskCompleted {
		t.Fatalf("completion source=%#v integration=%#v", completedSource, completedIntegration)
	}
	if completedSource.FinalCanonicalSHA != integrationHead || completedIntegration.FinalCanonicalSHA != integrationHead {
		t.Fatalf("final canonical source=%s integration=%s", completedSource.FinalCanonicalSHA, completedIntegration.FinalCanonicalSHA)
	}
}

func createAndSubmitCodeTask(t *testing.T, h *harness, project core.Project, worker core.Agent, head, suffix string) core.Task {
	t.Helper()
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "code " + suffix, MaxRetries: 1, RequestID: "create-" + suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
