package core_test

import (
	"context"
	"path/filepath"
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
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: integrator.ID, RequestID: "retention-accept",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err := h.database.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	closedAt, err := time.Parse(time.RFC3339Nano, task.ClosedAt)
	if err != nil {
		t.Fatal(err)
	}
	service, err := core.NewService(h.database, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
		CompletedWorkspaceRetention: retention, TerminalTaskRefRetention: retention,
	})
	if err != nil {
		t.Fatal(err)
	}
	setNextClock := func(next time.Time) {
		h.clock.mu.Lock()
		h.clock.value = next.Add(-time.Microsecond)
		h.clock.mu.Unlock()
	}

	setNextClock(closedAt.Add(retention).Add(-time.Microsecond))
	if result, err := service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
		TaskID: task.ID, ExpectedFingerprint: "workspace-fingerprint", RequestID: "retention-workspace-early",
	}); err == nil || result.Discarded {
		t.Fatalf("workspace age < retention discarded: result=%#v err=%v", result, err)
	}
	h.git.mu.Lock()
	workspaceCalls := h.git.discardWorkspaceCalls
	h.git.mu.Unlock()
	if workspaceCalls != 0 {
		t.Fatalf("early workspace discard calls = %d", workspaceCalls)
	}
	setNextClock(closedAt.Add(retention))
	if result, err := service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
		TaskID: task.ID, ExpectedFingerprint: "workspace-fingerprint", RequestID: "retention-workspace-boundary",
	}); err != nil || !result.Discarded {
		t.Fatalf("workspace age == retention result=%#v err=%v", result, err)
	}

	setNextClock(closedAt.Add(retention).Add(-time.Microsecond))
	if result, err := service.GCDiscardTaskRef(context.Background(), core.GCDiscardTaskRefInput{
		TaskID: task.ID, RunID: task.HeadRunID, ExpectedSHA: task.HeadSHA, RequestID: "retention-ref-early",
	}); err == nil || result.Discarded {
		t.Fatalf("task ref age < retention discarded: result=%#v err=%v", result, err)
	}
	setNextClock(closedAt.Add(retention))
	if result, err := service.GCDiscardTaskRef(context.Background(), core.GCDiscardTaskRefInput{
		TaskID: task.ID, RunID: task.HeadRunID, ExpectedSHA: task.HeadSHA, RequestID: "retention-ref-boundary",
	}); err != nil || !result.Discarded {
		t.Fatalf("task ref age == retention result=%#v err=%v", result, err)
	}
}

func TestGT07PeriodicWorkspaceGCReleasesSourceRefAfterAbsentReplay(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "periodic-release-worker")
	integrator := h.addAgent(t, "periodic-release-integrator")
	project := h.addProject(t, "periodic-release-project", integrator.ID)
	source := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "periodic-release-source")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: source.ID, IntegrationAgentID: integrator.ID, RequestID: "periodic-release-accept",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	source, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: integrator.ID, Kind: core.TaskWork,
		Title: "periodic source consumer", SourceTaskID: source.ID, RequestID: "periodic-release-consumer",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: consumer.ID, Reason: "closed", RequestID: "periodic-release-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := h.git.DeleteWorkspace(context.Background(), core.GitDeleteWorkspaceIntent{
		ProjectID: consumer.ProjectID, TaskID: consumer.ID, BaseSHA: consumer.BaseSHA, ExpectedHead: consumer.BaseSHA,
	}, func() (bool, error) {
		return h.database.WorkspaceEligible(context.Background(), consumer.ID, "9999-12-31T23:59:59.999999999Z")
	})
	if err != nil || !deleted {
		t.Fatalf("pre-crash workspace delete deleted=%t err=%v", deleted, err)
	}
	before, err := h.database.Task(context.Background(), consumer.ID)
	if err != nil || before.SourceRefReleasedAt != "" {
		t.Fatalf("pre-replay consumer = %#v err=%v", before, err)
	}
	if err := h.service.ReconcileWorkspaceGC(context.Background(), "9999-12-31T23:59:59.999999999Z"); err != nil {
		t.Fatal(err)
	}
	released, err := h.database.Task(context.Background(), consumer.ID)
	if err != nil || released.SourceRefReleasedAt == "" {
		t.Fatalf("periodic replay consumer = %#v err=%v", released, err)
	}
	if err := h.service.ReconcileWorkspaceGC(context.Background(), "9999-12-31T23:59:59.999999999Z"); err != nil {
		t.Fatal(err)
	}
	replayed, err := h.database.Task(context.Background(), consumer.ID)
	if err != nil || replayed.Version != released.Version || replayed.SourceRefReleasedAt != released.SourceRefReleasedAt {
		t.Fatalf("periodic release replay = %#v err=%v, want %#v", replayed, err, released)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{EntityType: "task", EntityID: consumer.ID})
	if err != nil || countEvent(events, "gc.source_ref_released") != 1 {
		t.Fatalf("source release events=%d err=%v", countEvent(events, "gc.source_ref_released"), err)
	}
}

func TestGT07RetryOfRequiresClosedSameProjectAndUsesCurrentCanonical(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "retry-lineage-worker")
	integrator := h.addAgent(t, "retry-lineage-integrator")
	project := h.addProject(t, "retry-lineage-project", integrator.ID)
	completed := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "retry-lineage-completed")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: completed.ID, IntegrationAgentID: integrator.ID, RequestID: "retry-lineage-accept",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, _ = h.database.Task(context.Background(), completed.ID)
	cancelled, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Title: "cancelled retry target", RequestID: "retry-lineage-cancelled-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err = h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: cancelled.ID, Reason: "retry in a new task", RequestID: "retry-lineage-cancelled-close",
	})
	if err != nil {
		t.Fatal(err)
	}
	currentCanonical := "cccccccccccccccccccccccccccccccccccccccc"
	h.git.mu.Lock()
	h.git.sha = currentCanonical
	h.git.mu.Unlock()

	for _, target := range []core.Task{completed, cancelled} {
		t.Run(string(target.Status), func(t *testing.T) {
			requestID := "retry-lineage-create-" + string(target.Status)
			input := core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: worker.ID, Title: "retry " + target.ID,
				RetryOfTaskID: target.ID, RequestID: requestID,
			}
			created, err := h.service.CreateTask(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if created.RetryOfTaskID != target.ID || created.BaseSHA != currentCanonical || created.SourceTaskID != "" {
				t.Fatalf("retry task = %#v", created)
			}
			replay, err := h.service.CreateTask(context.Background(), input)
			if err != nil || replay.ID != created.ID {
				t.Fatalf("retry dedupe replay = %#v err=%v, want %s", replay, err, created.ID)
			}
		})
	}

	open, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Title: "open retry target", RequestID: "retry-lineage-open",
	})
	if err != nil {
		t.Fatal(err)
	}
	other := h.addProject(t, "retry-lineage-other-project", "")
	cross, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: other.ID, AssigneeAgentID: worker.ID, Title: "cross retry target", RequestID: "retry-lineage-cross-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	cross, err = h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: cross.ID, Reason: "closed", RequestID: "retry-lineage-cross-close",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		name, target string
		code         core.ErrorCode
	}{
		{name: "open", target: open.ID, code: core.CodeInvalidState},
		{name: "cross_project", target: cross.ID, code: core.CodeScopeDenied},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			before := h.durableSignature(t, "")
			_, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: worker.ID, Title: "invalid retry",
				RetryOfTaskID: invalid.target, RequestID: "retry-lineage-invalid-" + invalid.name,
			})
			if !core.IsCode(err, invalid.code) {
				t.Fatalf("invalid retry error = %v, want %s", err, invalid.code)
			}
			if after := h.durableSignature(t, ""); after != before {
				t.Fatal("rejected retry changed durable state")
			}
		})
	}
}

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

func TestGT07TerminalDurableSourceReferenceBlocksTaskRefGCUntilRelease(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "durable-source-worker")
	integrator := h.addAgent(t, "durable-source-integrator")
	project := h.addProject(t, "durable-source-project", integrator.ID)
	source := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "durable-source")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	source, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: integrator.ID, Kind: core.TaskWork,
		Title: "terminal source consumer", SourceTaskID: source.ID, RequestID: "create-terminal-consumer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: consumer.ID, Reason: "review finished", RequestID: "cancel-terminal-consumer",
	}); err != nil {
		t.Fatal(err)
	}
	eligible, err := h.database.TaskRefEligible(context.Background(), source.ID, source.TaskRef, "9999-12-31T23:59:59Z")
	if err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("terminal consumer's unreleased durable source ref allowed source task-ref GC")
	}
}

func TestGT07FormalGCDiscardIsCASFencedIdempotentAndReleasesSourceRef(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "gc-worker")
	integrator := h.addAgent(t, "gc-integrator")
	project := h.addProject(t, "gc-project", integrator.ID)
	source := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "gc-source")
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: source.ID, IntegrationAgentID: integrator.ID, RequestID: "gc-source-accept",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	source, err := h.database.Task(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: integrator.ID, Kind: core.TaskWork,
		Title: "discarded terminal consumer", SourceTaskID: source.ID, RequestID: "gc-consumer-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: consumer.ID, Reason: "review complete", RequestID: "gc-consumer-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := h.service.GCPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var target core.GCWorkspaceTarget
	for _, candidate := range preview.Workspaces {
		if candidate.TaskID == consumer.ID {
			target = candidate
		}
	}
	if target.Fingerprint == "" {
		t.Fatalf("GC preview omitted consumer workspace identity: %#v", preview)
	}
	input := core.GCDiscardWorkspaceInput{
		TaskID: consumer.ID, ExpectedFingerprint: target.Fingerprint, RequestID: "gc-discard-consumer",
	}
	first, err := h.service.GCDiscardWorkspace(context.Background(), input)
	if err != nil || !first.Discarded {
		t.Fatalf("discard workspace = %#v err=%v", first, err)
	}
	replay, err := h.service.GCDiscardWorkspace(context.Background(), input)
	if err != nil || replay != first {
		t.Fatalf("discard replay = %#v err=%v, want %#v", replay, err, first)
	}
	input.ExpectedFingerprint = "changed"
	if _, err := h.service.GCDiscardWorkspace(context.Background(), input); !core.IsCode(err, core.CodeVersionConflict) {
		t.Fatalf("request-id input mismatch error = %v", err)
	}
	released, err := h.database.Task(context.Background(), consumer.ID)
	if err != nil || released.SourceRefReleasedAt == "" {
		t.Fatalf("released consumer = %#v err=%v", released, err)
	}
	eligible, err := h.database.TaskRefEligible(context.Background(), source.ID, source.TaskRef, "9999-12-31T23:59:59Z")
	if err != nil || !eligible {
		t.Fatalf("source ref after explicit release eligible=%t err=%v", eligible, err)
	}

	refInput := core.GCDiscardTaskRefInput{
		TaskID: source.ID, RunID: source.HeadRunID, ExpectedSHA: source.HeadSHA, RequestID: "gc-discard-source-ref",
	}
	refResult, err := h.service.GCDiscardTaskRef(context.Background(), refInput)
	if err != nil || !refResult.Discarded {
		t.Fatalf("discard task ref = %#v err=%v", refResult, err)
	}
	refReplay, err := h.service.GCDiscardTaskRef(context.Background(), refInput)
	if err != nil || refReplay != refResult {
		t.Fatalf("discard task ref replay = %#v err=%v", refReplay, err)
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
	if err != nil {
		t.Fatal(err)
	}
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
