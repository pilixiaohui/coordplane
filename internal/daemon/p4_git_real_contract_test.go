package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/gitcapture"
	"coordplane/internal/gitrepo"
	"coordplane/internal/store"
	"coordplane/internal/transport"
	"coordplane/tests/testsupport"
)

var durableSignature = testsupport.DurableSignature

func TestGT04CT08FormalCommandsDeterministicallyFenceAcceptAndRevocation(t *testing.T) {
	binary := buildP4Binary(t, "coordplane")
	competitors := []string{"rework", "cancel"}
	for _, competitor := range competitors {
		for _, order := range []string{"revocation_first", "accept_first"} {
			t.Run(competitor+"/"+order, func(t *testing.T) {
				h := newRealP4Harness(t)
				task, _, head := prepareGT03Capture(t, h, "ct08-"+competitor+"-"+order, true)
				h.reconcileGit(t)
				gate := newBarrierProjectGit(projectGitAdapter{initializer: h.initializer, workspaces: h.workspaces}, order)
				h.service = newRealP4Service(t, h.database, gate)
				socket, stop := startP4OperatorServer(t, h.root, h.service)
				defer stop()
				acceptArgs := []string{"task", "accept", task.ID, "--socket", socket, "--request-id", "ct08-accept", "--output", "json"}
				competeArgs := []string{"task", competitor, task.ID, "--socket", socket, "--reason", competitor, "--request-id", "ct08-" + competitor, "--output", "json"}

				if order == "revocation_first" {
					acceptResult := make(chan p4CLIResult, 1)
					go func() { acceptResult <- runP4OperatorCLI(binary, acceptArgs...) }()
					waitRuntimeSignal(t, gate.resolveEntered, 10*time.Second, "Git executor barrier timeout")
					if result := runP4OperatorCLI(binary, competeArgs...); result.code != 0 {
						t.Fatalf("winning %s command: %s", competitor, result.stderr)
					}
					winnerSignature := durableSignature(t, h.database, h.project.ID)
					close(gate.resolveRelease)
					if result := <-acceptResult; result.code == 0 || !strings.Contains(result.stderr, string(core.CodeInvalidState)) {
						t.Fatalf("stale accept result = %#v", result)
					}
					if got := durableSignature(t, h.database, h.project.ID); got != winnerSignature {
						t.Fatal("stale accept changed DB or Event state after revocation won")
					}
					if gate.advanceCallCount() != 0 {
						t.Fatal("stale accept reached the mutating Git advance boundary")
					}
					assertP4Refs(t, h, h.project.InitialSHA, task.ID, head)
					return
				}

				if result := runP4OperatorCLI(binary, acceptArgs...); result.code != 0 {
					t.Fatalf("winning accept command: %s", result.stderr)
				}
				accepted, err := h.database.Task(context.Background(), task.ID)
				if err != nil || accepted.PendingAction != "advance" || accepted.PendingActionID == "" ||
					accepted.PendingActionVersion != accepted.Version || accepted.AcceptedIntegrationAgentID != h.integrator.ID {
					t.Fatalf("accepted task = %#v err=%v", accepted, err)
				}
				reconcile := make(chan error, 1)
				go func() { reconcile <- h.service.ReconcileGit(context.Background()) }()
				waitRuntimeSignal(t, gate.advanceEntered, 10*time.Second, "Git executor barrier timeout")
				beforeLoser := durableSignature(t, h.database, h.project.ID)
				if result := runP4OperatorCLI(binary, competeArgs...); result.code == 0 || !strings.Contains(result.stderr, string(core.CodeActionInProgress)) {
					t.Fatalf("losing %s result = %#v", competitor, result)
				}
				if got := durableSignature(t, h.database, h.project.ID); got != beforeLoser {
					t.Fatalf("losing %s changed DB or Event state", competitor)
				}
				assertP4Refs(t, h, h.project.InitialSHA, task.ID, head)
				close(gate.advanceRelease)
				requireNoError(t, <-reconcile)
				completed, err := h.database.Task(context.Background(), task.ID)
				if err != nil || completed.Status != core.TaskCompleted || completed.FinalCanonicalSHA != head {
					t.Fatalf("completed accepted task = %#v err=%v", completed, err)
				}
				assertP4Refs(t, h, head, task.ID, head)
			})
		}
	}
}

func TestGT06RealConflictSecondStaleRequeuesSameIntegrationTaskAndCompletes(t *testing.T) {
	h := newRealP4Harness(t)
	coordlink := buildP4Binary(t, "coordlink")
	ctx := context.Background()
	sourceTask, err := h.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: h.project.ID, AssigneeAgentID: h.worker.ID, Kind: core.TaskWork,
		Title: "source with same-line change", MaxRetries: 2, RequestID: "gt06-source-create",
	})
	requireNoError(t, err)
	sourceClaim := h.claim(t, sourceTask.ID)
	sourceSpec := taskWorkspaceSpec(sourceClaim.Task)
	sourceWorkspace, err := h.workspaces.Materialize(ctx, sourceSpec)
	requireNoError(t, err)
	h.activate(t, sourceClaim, sourceWorkspace.Path, "gt06-source")
	configureRealGitWorkspace(t, sourceWorkspace.Path, "Source Worker")
	writeRealGitFile(t, sourceWorkspace.Path, "README.md", "source line\n")
	sourceHead := commitRealGitWorkspace(t, sourceWorkspace.Path, "source same-line change")
	h.submitAndStop(t, sourceClaim, sourceHead, "gt06-source")
	h.reconcileGit(t)
	sourceTask, err = h.database.Task(ctx, sourceTask.ID)
	if err != nil || sourceTask.Status != core.TaskSubmitted {
		t.Fatalf("captured source = %#v err=%v", sourceTask, err)
	}
	if _, err := h.service.RequestAccept(ctx, core.AcceptInput{
		TaskID: sourceTask.ID, IntegrationAgentID: h.integrator.ID, RequestID: "gt06-source-accept",
	}); err != nil {
		t.Fatal(err)
	}

	firstCanonical := h.captureAndAdvanceDirect(t, "canonical-first", h.project.InitialSHA, "README.md", "canonical line\n")
	h.reconcileGit(t)
	snapshot, err := h.database.Snapshot(ctx, h.project.ID)
	requireNoError(t, err)
	integration := onlyIntegrationTask(t, snapshot.Tasks)
	source, err := h.database.Task(ctx, sourceTask.ID)
	if err != nil || source.IntegrationTaskID != integration.ID || source.Status != core.TaskSubmitted {
		t.Fatalf("source after first stale = %#v err=%v", source, err)
	}

	firstClaim := h.claim(t, integration.ID)
	integrationSpec := taskWorkspaceSpec(firstClaim.Task)
	firstRun := prepareP4ScriptedRun(t, h, firstClaim, "gt06-integration-first")
	runP4IntegrationCLI(t, coordlink, firstRun, integrationSpec.Source.ConvenienceRef(), true)
	firstIntegrationHead := strings.TrimSpace(gitIn(t, firstRun.workspace, "rev-parse", "HEAD^{commit}"))
	firstRun.close(t)
	h.stopRun(t, firstClaim, "gt06-integration-first")
	progress, err := h.database.Events(ctx, core.EventFilter{ProjectID: h.project.ID, EntityType: "task", EntityID: integration.ID})
	requireNoError(t, err)
	foundConflict := false
	for _, event := range progress {
		foundConflict = foundConflict || event.Kind == "task.progress" && strings.Contains(event.PayloadJSON, "same-line conflict")
	}
	if !foundConflict {
		t.Fatalf("scripted integration CLI did not publish conflict progress: %#v", progress)
	}
	h.reconcileGit(t)

	secondCanonical := h.captureAndAdvanceDirect(t, "canonical-second", firstCanonical, "winner.txt", "second winner\n")
	h.reconcileGit(t)
	requeued, err := h.database.Task(ctx, integration.ID)
	if err != nil || requeued.Status != core.TaskQueued || requeued.Generation != firstClaim.Run.Generation ||
		requeued.ObservedCanonicalSHA != firstCanonical || requeued.HeadSHA != firstIntegrationHead {
		t.Fatalf("second-stale integration = %#v err=%v", requeued, err)
	}
	sourceAfterStale, err := h.database.Task(ctx, sourceTask.ID)
	if err != nil || sourceAfterStale.Status != core.TaskSubmitted || sourceAfterStale.IntegrationTaskID != integration.ID {
		t.Fatalf("source after second stale = %#v err=%v", sourceAfterStale, err)
	}
	messages, err := h.database.Messages(ctx, core.MessageFilter{TaskID: integration.ID, Limit: 20})
	requireNoError(t, err)
	foundSecondStale := false
	for _, message := range messages.Items {
		if strings.Contains(message.Body, secondCanonical) {
			foundSecondStale = true
		}
	}
	if !foundSecondStale {
		t.Fatalf("second-stale canonical %s not visible in messages: %#v", secondCanonical, messages.Items)
	}

	secondClaim := h.claim(t, integration.ID)
	if secondClaim.Run.Generation != firstClaim.Run.Generation+1 || secondClaim.Task.ID != integration.ID {
		t.Fatalf("second integration claim = %#v", secondClaim)
	}
	secondRun := prepareP4ScriptedRun(t, h, secondClaim, "gt06-integration-second")
	if got := strings.TrimSpace(gitIn(t, secondRun.workspace, "rev-parse", "refs/heads/coordplane/canonical^{commit}")); got != secondCanonical {
		t.Fatalf("imported second canonical = %s, want %s", got, secondCanonical)
	}
	runP4IntegrationCLI(t, coordlink, secondRun, "refs/heads/coordplane/canonical", false)
	finalHead := strings.TrimSpace(gitIn(t, secondRun.workspace, "rev-parse", "HEAD^{commit}"))
	secondRun.close(t)
	h.stopRun(t, secondClaim, "gt06-integration-second")
	h.reconcileGit(t)
	h.reconcileGit(t)
	finalSource, err := h.database.Task(ctx, sourceTask.ID)
	requireNoError(t, err)
	finalIntegration, err := h.database.Task(ctx, integration.ID)
	requireNoError(t, err)
	if finalSource.Status != core.TaskCompleted || finalIntegration.Status != core.TaskCompleted ||
		finalSource.FinalCanonicalSHA != finalHead || finalIntegration.FinalCanonicalSHA != finalHead {
		t.Fatalf("final source=%#v integration=%#v", finalSource, finalIntegration)
	}
	if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", h.project.CanonicalRef+"^{commit}")); actual != finalHead {
		t.Fatalf("final canonical = %s, want %s", actual, finalHead)
	}
	finalSnapshot, err := h.database.Snapshot(ctx, h.project.ID)
	requireNoError(t, err)
	if got := onlyIntegrationTask(t, finalSnapshot.Tasks).ID; got != integration.ID {
		t.Fatalf("nested integration task created: got %s want %s", got, integration.ID)
	}
	gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
}

func TestGT07WorkspaceDeleteCrashReopenReleasesDurableSourceOnce(t *testing.T) {
	h := newRealP4Harness(t)
	ctx := context.Background()
	source := completeP4Task(t, h, "gt07-replay-source")
	consumer, err := h.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: h.project.ID, AssigneeAgentID: h.integrator.ID,
		Title: "replay source consumer", SourceTaskID: source.ID, RequestID: "gt07-replay-create",
	})
	requireNoError(t, err)
	spec := taskWorkspaceSpec(consumer)
	workspace, err := h.workspaces.Materialize(ctx, spec)
	requireNoError(t, err)
	consumer, err = h.service.CancelTask(ctx, core.TaskActionInput{
		TaskID: consumer.ID, Reason: "finished", RequestID: "gt07-replay-cancel",
	})
	requireNoError(t, err)
	dirty := filepath.Join(workspace.Path, "dirty.txt")
	requireNoError(t, os.WriteFile(dirty, []byte("preserve\n"), 0o600))
	requireNoError(t, h.service.ReconcileWorkspaceGC(ctx, consumer.ClosedAt))
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatalf("dirty workspace was deleted: %v", err)
	}
	requireNoError(t, os.Remove(dirty))
	deleted, err := h.workspaces.Delete(ctx, spec, consumer.BaseSHA, func() (bool, error) { return true, nil })
	if err != nil || !deleted {
		t.Fatalf("pre-crash workspace delete = %t err=%v", deleted, err)
	}
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace survived pre-crash delete: %v", err)
	}
	if persisted, err := h.database.Task(ctx, consumer.ID); err != nil || persisted.SourceRefReleasedAt != "" {
		t.Fatalf("source ref released before restart = %#v err=%v", persisted, err)
	}
	requireNoError(t, h.database.Close())
	h.database, err = store.Open(ctx, filepath.Join(h.root, "coordplane.db"))
	requireNoError(t, err)
	t.Cleanup(func() { _ = h.database.Close() })
	h.service = newRealP4Service(t, h.database, projectGitAdapter{initializer: h.initializer, workspaces: h.workspaces})
	requireNoError(t, h.service.ReconcileWorkspaceGC(ctx, consumer.ClosedAt))
	released, err := h.database.Task(ctx, consumer.ID)
	if err != nil || released.SourceRefReleasedAt == "" {
		t.Fatalf("reopened source release = %#v err=%v", released, err)
	}
	stable := durableSignature(t, h.database, h.project.ID)
	requireNoError(t, h.service.ReconcileWorkspaceGC(ctx, consumer.ClosedAt))
	if replay := durableSignature(t, h.database, h.project.ID); replay != stable {
		t.Fatal("absent workspace replay changed durable state")
	}
	events, err := h.database.Events(ctx, core.EventFilter{ProjectID: h.project.ID, EntityID: consumer.ID})
	releaseEvents := 0
	for _, event := range events {
		if event.Kind == "gc.source_ref_released" {
			releaseEvents++
		}
	}
	if err != nil || releaseEvents != 1 {
		t.Fatalf("source release events = %#v err=%v", events, err)
	}
	assertP4TaskRef(t, h, source.TaskRef, source.HeadSHA)
}

func TestGT07SourceRefRetentionUsesLaterIntegrationAndSourceClosedAt(t *testing.T) {
	h := newRealP4Harness(t)
	ctx := context.Background()
	source, _, _ := prepareGT03Capture(t, h, "gt07-later-close-source", true)
	h.reconcileGit(t)
	source, err := h.database.Task(ctx, source.ID)
	requireNoError(t, err)
	if _, err := h.service.RequestAccept(ctx, core.AcceptInput{
		TaskID: source.ID, IntegrationAgentID: h.integrator.ID, RequestID: "gt07-later-close-accept",
	}); err != nil {
		t.Fatal(err)
	}
	h.captureAndAdvanceDirect(t, "gt07-later-close-winner", h.project.InitialSHA, "winner.txt", "winner\n")
	h.reconcileGit(t)
	snapshot, err := h.database.Snapshot(ctx, h.project.ID)
	requireNoError(t, err)
	integration := onlyIntegrationTask(t, snapshot.Tasks)
	if _, err := h.workspaces.Materialize(ctx, taskWorkspaceSpec(integration)); err != nil {
		t.Fatal(err)
	}
	integration, err = h.service.CancelTask(ctx, core.TaskActionInput{
		TaskID: integration.ID, Reason: "cancel integration", RequestID: "gt07-later-close-integration",
	})
	requireNoError(t, err)
	requireNoError(t, h.service.ReconcileWorkspaceGC(ctx, integration.ClosedAt))
	released, err := h.database.Task(ctx, integration.ID)
	if err != nil || released.SourceRefReleasedAt == "" {
		t.Fatalf("integration source release = %#v err=%v", released, err)
	}
	source, err = h.service.CancelTask(ctx, core.TaskActionInput{
		TaskID: source.ID, Reason: "cancel source later", RequestID: "gt07-later-close-source",
	})
	if err != nil || source.ClosedAt <= integration.ClosedAt {
		t.Fatalf("later source close = %#v integration=%#v err=%v", source, integration, err)
	}
	if eligible, err := h.database.TaskRefEligible(ctx, source.ID, source.TaskRef, integration.ClosedAt); err != nil || eligible {
		t.Fatalf("integration cutoff eligible=%t err=%v", eligible, err)
	}
	if eligible, err := h.database.TaskRefEligible(ctx, source.ID, source.TaskRef, source.ClosedAt); err != nil || !eligible {
		t.Fatalf("later source cutoff eligible=%t err=%v", eligible, err)
	}
	assertP4TaskRef(t, h, source.TaskRef, source.HeadSHA)
}

type realP4Harness struct {
	root        string
	database    *store.Store
	service     *core.Service
	initializer *gitrepo.Initializer
	workspaces  *gitrepo.WorkspaceManager
	project     core.Project
	worker      core.Agent
	integrator  core.Agent
}

func newRealP4Harness(t *testing.T) *realP4Harness {
	t.Helper()
	root := t.TempDir()
	source := createSourceRepository(t, root)
	writeRealGitFile(t, source, "README.md", "base line\n")
	gitIn(t, source, "add", "README.md")
	gitIn(t, source, "commit", "-m", "same-line base")
	initializer, err := gitrepo.New(filepath.Join(root, "repos"))
	requireNoError(t, err)
	preflight, err := initializer.Preflight(context.Background(), source, "refs/heads/main")
	requireNoError(t, err)
	paths, err := initializer.Paths("project-real-p4", "initialize-real-p4")
	requireNoError(t, err)
	gitProject := gitrepo.Project{
		ID: "project-real-p4", OperationID: "initialize-real-p4",
		SourcePath: preflight.SourcePath, SourceRef: preflight.SourceRef, InitialSHA: preflight.InitialSHA,
		ControlRepoPath: paths.Final, CanonicalRef: preflight.SourceRef,
	}
	if _, err := initializer.Initialize(context.Background(), gitProject); err != nil {
		t.Fatal(err)
	}
	helper := localCaptureHelper{root: filepath.Join(root, "handoff")}
	requireNoError(t, os.MkdirAll(helper.root, 0o700))
	workspaces, err := gitrepo.NewWorkspaceManager(initializer, filepath.Join(root, "workspaces"), helper)
	requireNoError(t, err)
	database, err := store.Open(context.Background(), filepath.Join(root, "coordplane.db"))
	requireNoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	clock := &realP4Clock{value: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)}
	project := core.Project{
		ID: gitProject.ID, Name: "real P4", Source: preflight.SourcePath, SourceRef: preflight.SourceRef,
		InitialSHA: preflight.InitialSHA, ControlRepoPath: paths.Final, CanonicalRef: preflight.SourceRef,
		CanonicalSHA: preflight.InitialSHA, IntegrationAgentID: "agent-real-integrator",
		Status: core.ProjectActive, Version: 1,
	}
	worker := core.Agent{ID: "agent-real-worker", DisplayName: "Worker", AdapterID: "claude", Image: "agent:test", InstructionsText: "Work only on the assigned Task.", Status: core.AgentActive, Version: 1}
	integrator := core.Agent{ID: "agent-real-integrator", DisplayName: "Integrator", AdapterID: "claude", Image: "agent:test", InstructionsText: "Work only on the assigned Task.", Status: core.AgentActive, Version: 1}
	now := clock.Now().UTC().Format(time.RFC3339Nano)
	project.CreatedAt, project.UpdatedAt = now, now
	worker.CreatedAt, worker.UpdatedAt = now, now
	integrator.CreatedAt, integrator.UpdatedAt = now, now
	if err := database.Transact(context.Background(), func(tx core.Transaction) error {
		if err := tx.InsertProject(project); err != nil {
			return err
		}
		if err := tx.InsertAgent(worker); err != nil {
			return err
		}
		return tx.InsertAgent(integrator)
	}); err != nil {
		t.Fatal(err)
	}
	ids := &realP4IDs{}
	service, err := core.NewService(database, projectGitAdapter{initializer: initializer, workspaces: workspaces}, core.ServiceOptions{
		Now: clock.Now, NewID: ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"claude"},
	})
	requireNoError(t, err)
	service.SetReady(true, "")
	return &realP4Harness{
		root: root, database: database, service: service, initializer: initializer, workspaces: workspaces,
		project: project, worker: worker, integrator: integrator,
	}
}

func (h *realP4Harness) claim(t *testing.T, taskID string) core.Claim {
	t.Helper()
	claim, ok, err := h.service.ClaimNext(context.Background(), h.project.ID)
	if err != nil || !ok || claim.Task.ID != taskID {
		t.Fatalf("claim task %s = %#v ok=%t err=%v", taskID, claim, ok, err)
	}
	return claim
}

func (h *realP4Harness) reconcileGit(t *testing.T) {
	t.Helper()
	requireNoError(t, h.service.ReconcileGit(context.Background()))
}

func (h *realP4Harness) activate(t *testing.T, claim core.Claim, workspace, prefix string) core.Run {
	t.Helper()
	launch, err := h.service.RuntimeLaunchContext(context.Background(), claim.Run.ID)
	requireNoError(t, err)
	run, err := h.service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: prefix + "-nonce",
		WorkspacePath: workspace, HomePath: filepath.Join(h.root, "homes", claim.Run.ID),
		LogPath: filepath.Join(h.root, "logs", claim.Run.ID+".log"), InstructionsHash: launch.InstructionsHash,
		ConfigFingerprint: launch.ConfigFingerprint,
		LaunchMode:        "start", CleanupOperationID: prefix + "-cleanup", RequestID: prefix + "-prepare",
	})
	requireNoError(t, err)
	fact := core.RunRuntimeFactInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: prefix + "-container", RequestID: prefix + "-created",
	}
	run, err = h.service.RecordContainerCreated(context.Background(), fact)
	requireNoError(t, err)
	fact.ContainerID, fact.RequestID = run.ContainerID, prefix+"-started"
	run, err = h.service.RecordRunStartIssued(context.Background(), fact)
	requireNoError(t, err)
	fact.ContainerID, fact.RequestID = run.ContainerID, prefix+"-active"
	run, err = h.service.ObserveProcessAndActivateRun(context.Background(), fact)
	requireNoError(t, err)
	return run
}

func (h *realP4Harness) submitAndStop(t *testing.T, claim core.Claim, head, prefix string) {
	t.Helper()
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: prefix, ExpectedHead: head, RequestID: prefix + "-submit",
	}); err != nil {
		t.Fatal(err)
	}
	h.stopRun(t, claim, prefix)
}

func (h *realP4Harness) stopRun(t *testing.T, claim core.Claim, prefix string) {
	t.Helper()
	run, err := h.database.Run(context.Background(), claim.Run.ID)
	requireNoError(t, err)
	exitCode := 0
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: core.RunExited, ExitCode: &exitCode, TerminalReason: "process_exited", RequestID: prefix + "-terminal",
	})
	requireNoError(t, err)
	if _, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: core.RunRuntimeFactInput{
			RunID: terminal.Run.ID, Generation: terminal.Run.Generation, LaunchNonce: terminal.Run.LaunchNonce,
			LaunchOperationID: terminal.Run.LaunchOperationID, ContainerID: terminal.Run.ContainerID,
			RequestID: prefix + "-cleanup",
		},
		CleanupOperationID: terminal.Run.CleanupOperationID, State: core.CleanupRemoved,
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *realP4Harness) captureAndAdvanceDirect(t *testing.T, suffix, baseSHA, filename, content string) string {
	t.Helper()
	ctx := context.Background()
	taskID, runID := "direct-"+suffix, "run-"+suffix
	spec := gitrepo.WorkspaceSpec{ProjectID: h.project.ID, TaskID: taskID, BaseSHA: baseSHA}
	workspace, err := h.workspaces.Materialize(ctx, spec)
	requireNoError(t, err)
	configureRealGitWorkspace(t, workspace.Path, suffix)
	writeRealGitFile(t, workspace.Path, filename, content)
	head := commitRealGitWorkspace(t, workspace.Path, suffix)
	captured, err := h.workspaces.Capture(ctx, gitrepo.CaptureSpec{
		Workspace: spec, RunID: runID, ExpectedHead: head, ControlRepoPath: h.project.ControlRepoPath,
	})
	requireNoError(t, err)
	advanced, err := h.initializer.Advance(ctx, gitrepo.AdvanceSpec{
		ProjectID: h.project.ID, ControlRepoPath: h.project.ControlRepoPath,
		CanonicalRef: h.project.CanonicalRef, TaskRef: captured.TaskRef,
		ExpectedOldSHA: baseSHA, TargetSHA: head,
	})
	if err != nil || advanced.Outcome != gitrepo.AdvanceUpdated {
		t.Fatalf("direct advance %s = %#v err=%v", suffix, advanced, err)
	}
	return head
}

func taskWorkspaceSpec(task core.Task) gitrepo.WorkspaceSpec {
	spec := gitrepo.WorkspaceSpec{ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA}
	if task.SourceTaskID != "" {
		spec.Source = &gitrepo.WorkspaceSource{
			TaskID: task.SourceTaskID, RunID: task.SourceRunID,
			TaskRef: task.SourceTaskRef, HeadSHA: task.SourceHeadSHA,
		}
	}
	return spec
}

func onlyIntegrationTask(t *testing.T, tasks []core.Task) core.Task {
	t.Helper()
	var found core.Task
	for _, task := range tasks {
		if task.Kind != core.TaskIntegration {
			continue
		}
		if found.ID != "" {
			t.Fatalf("multiple integration tasks: %#v", tasks)
		}
		found = task
	}
	if found.ID == "" {
		t.Fatalf("integration task missing: %#v", tasks)
	}
	return found
}

func configureRealGitWorkspace(t *testing.T, workspace, name string) {
	t.Helper()
	gitIn(t, workspace, "config", "user.name", name)
	gitIn(t, workspace, "config", "user.email", strings.ReplaceAll(strings.ToLower(name), " ", "-")+"@example.invalid")
}

func writeRealGitFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	requireNoError(t, os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o660))
}

func commitRealGitWorkspace(t *testing.T, workspace, message string) string {
	t.Helper()
	gitIn(t, workspace, "add", "--all")
	gitIn(t, workspace, "commit", "-m", message)
	return strings.TrimSpace(gitIn(t, workspace, "rev-parse", "HEAD^{commit}"))
}

type barrierProjectGit struct {
	projectGitAdapter
	blockResolve                   bool
	blockAdvance                   bool
	resolveEntered, resolveRelease chan struct{}
	advanceEntered, advanceRelease chan struct{}
	resolveOnce, advanceOnce       sync.Once
	mu                             sync.Mutex
	advanceCalls                   int
}

func newBarrierProjectGit(adapter projectGitAdapter, order string) *barrierProjectGit {
	return &barrierProjectGit{
		projectGitAdapter: adapter,
		blockResolve:      order == "revocation_first",
		blockAdvance:      order == "accept_first",
		resolveEntered:    make(chan struct{}), resolveRelease: make(chan struct{}),
		advanceEntered: make(chan struct{}), advanceRelease: make(chan struct{}),
	}
}

func (g *barrierProjectGit) ResolveTaskRef(ctx context.Context, intent core.GitTaskRefIntent) (string, error) {
	actual, err := g.projectGitAdapter.ResolveTaskRef(ctx, intent)
	if err == nil && g.blockResolve {
		g.resolveOnce.Do(func() { close(g.resolveEntered) })
		select {
		case <-g.resolveRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return actual, err
}

func (g *barrierProjectGit) Advance(ctx context.Context, intent core.GitAdvanceIntent) (core.GitAdvanceFact, error) {
	g.mu.Lock()
	g.advanceCalls++
	g.mu.Unlock()
	if g.blockAdvance {
		g.advanceOnce.Do(func() { close(g.advanceEntered) })
		select {
		case <-g.advanceRelease:
		case <-ctx.Done():
			return core.GitAdvanceFact{}, ctx.Err()
		}
	}
	return g.projectGitAdapter.Advance(ctx, intent)
}

func (g *barrierProjectGit) advanceCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.advanceCalls
}

type p4CLIResult struct {
	code   int
	stderr string
}

func runP4OperatorCLI(binary string, args ...string) p4CLIResult {
	raw, err := exec.Command(binary, args...).CombinedOutput()
	if err != nil {
		return p4CLIResult{code: 1, stderr: string(raw)}
	}
	return p4CLIResult{}
}

func buildP4Binary(t *testing.T, name string) string {
	t.Helper()
	working, err := os.Getwd()
	requireNoError(t, err)
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/"+name)
	command.Dir = filepath.Clean(filepath.Join(working, "..", ".."))
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build formal %s binary: %v\n%s", name, err, raw)
	}
	return binary
}

type p4ScriptedRun struct {
	controller *runtimeController
	run        core.Run
	workspace  string
	socket     string
	tokenFile  string
	control    *runControl
}

func prepareP4ScriptedRun(t *testing.T, h *realP4Harness, claim core.Claim, prefix string) p4ScriptedRun {
	t.Helper()
	ctx := context.Background()
	launch, err := h.service.RuntimeLaunchContext(ctx, claim.Run.ID)
	requireNoError(t, err)
	spec, err := gitWorkspaceSpec(launch.Task)
	requireNoError(t, err)
	workspace, err := h.workspaces.Path(launch.Project.ID, launch.Task.ID)
	requireNoError(t, err)
	run, err := h.service.BeginRunLaunch(ctx, core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: prefix + "-nonce",
		WorkspacePath: workspace, HomePath: filepath.Join(h.root, "homes", claim.Run.ID),
		LogPath: filepath.Join(h.root, "logs", claim.Run.ID+".log"), InstructionsHash: launch.InstructionsHash,
		ConfigFingerprint: launch.ConfigFingerprint,
		LaunchMode:        "start", CleanupOperationID: prefix + "-cleanup", RequestID: prefix + "-prepare",
	})
	requireNoError(t, err)
	controller := &runtimeController{service: h.service, workspaces: h.workspaces, controls: make(map[string]*runControl)}
	state := &runtimePrepareState{controller: controller, ctx: ctx, launch: launch, workspaceSpec: spec, run: run}
	requireNoError(t, prepareRuntimeWorkspace(state))
	fact := core.RunRuntimeFactInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: prefix + "-container", RequestID: prefix + "-created",
	}
	run, err = h.service.RecordContainerCreated(ctx, fact)
	requireNoError(t, err)
	fact.ContainerID, fact.RequestID = run.ContainerID, prefix+"-started"
	run, err = h.service.RecordRunStartIssued(ctx, fact)
	requireNoError(t, err)
	fact.RequestID = prefix + "-active"
	run, err = h.service.ObserveProcessAndActivateRun(ctx, fact)
	requireNoError(t, err)
	controlPath := filepath.Join(h.root, "run-control", run.ID)
	requireNoError(t, os.MkdirAll(controlPath, runControlDirectoryMode))
	tokenFile := filepath.Join(controlPath, "token")
	requireNoError(t, os.WriteFile(tokenFile, []byte(claim.Token+"\n"), runControlFileMode))
	control, err := controller.openRunControl(run, controlPath)
	requireNoError(t, err)
	controller.controls[run.ID] = control
	return p4ScriptedRun{
		controller: controller, run: run, workspace: workspace,
		socket: filepath.Join(controlPath, "api.sock"), tokenFile: tokenFile, control: control,
	}
}

func (r p4ScriptedRun) close(t *testing.T) {
	t.Helper()
	requireNoError(t, r.controller.closeControl(r.run.ID, r.control))
}

func runP4IntegrationCLI(t *testing.T, coordlink string, run p4ScriptedRun, mergeRef string, conflict bool) {
	t.Helper()
	mode := "refresh"
	if conflict {
		mode = "conflict"
	}
	script := `
git config user.name "Scripted Integration CLI"
git config user.email scripted-integration@example.invalid
if [ "$3" = conflict ]; then
  if git merge --no-ff "$2" -m "merge source"; then
    exit 20
  fi
  [ "$(git diff --name-only --diff-filter=U)" = README.md ]
  "$1" progress --summary "real same-line conflict in README.md" --request-id gt06-conflict-progress --output json >/dev/null
  printf 'canonical line\nsource line\n' > README.md
  git add README.md
  git commit -m "resolve real same-line conflict"
else
  git merge --no-ff "$2" -m "integrate refreshed canonical"
fi
head=$(git rev-parse HEAD^{commit})
"$1" task submit --summary "$3 resolved" --expected-head "$head" --request-id "gt06-$3-submit" --output json >/dev/null
`
	command := exec.Command("sh", "-eu", "-c", script, "scripted-integration", coordlink, mergeRef, mode)
	command.Dir = run.workspace
	command.Env = append(os.Environ(),
		"COORDPLANE_RUN_SOCKET="+run.socket,
		"COORDPLANE_RUN_TOKEN_FILE="+run.tokenFile,
		"LC_ALL=C",
	)
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("scripted integration CLI: %v\n%s", err, raw)
	}
}

func startP4OperatorServer(t *testing.T, root string, service *core.Service) (string, func()) {
	t.Helper()
	socket := filepath.Join(root, "operator.sock")
	server, err := transport.NewUnixServer(root, socket, transport.NewOperatorHandler(service))
	requireNoError(t, err)
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	var once sync.Once
	return socket, func() {
		once.Do(func() {
			_ = server.Close()
			<-done
		})
	}
}

func waitP4File(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("path did not appear: %s", path)
		}
	}
}

func newRealP4Service(t *testing.T, database *store.Store, controller core.ProjectGit) *core.Service {
	t.Helper()
	service, err := core.NewService(database, controller, core.ServiceOptions{
		Now: time.Now, NewID: (&realP4IDs{}).New, MaxParallelRuns: 4, AdapterIDs: []string{"claude"},
	})
	requireNoError(t, err)
	service.SetReady(true, "")
	return service
}

func assertP4Refs(t *testing.T, h *realP4Harness, canonical, taskID, taskHead string) {
	t.Helper()
	if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", h.project.CanonicalRef+"^{commit}")); actual != canonical {
		t.Fatalf("canonical = %s, want %s", actual, canonical)
	}
	refs := strings.Fields(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "for-each-ref", "--format=%(objectname)", "refs/coordplane/tasks/"+taskID))
	if len(refs) != 1 || refs[0] != taskHead {
		t.Fatalf("task refs = %#v, want one %s", refs, taskHead)
	}
}

func assertP4TaskRef(t *testing.T, h *realP4Harness, ref, head string) {
	t.Helper()
	if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", ref+"^{commit}")); actual != head {
		t.Fatalf("task ref %s = %s, want %s", ref, actual, head)
	}
}

func completeP4Task(t *testing.T, h *realP4Harness, suffix string) core.Task {
	t.Helper()
	task, _, _ := prepareGT03Capture(t, h, suffix, true)
	h.reconcileGit(t)
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: h.integrator.ID, RequestID: suffix + "-accept",
	}); err != nil {
		t.Fatal(err)
	}
	h.reconcileGit(t)
	completed, err := h.database.Task(context.Background(), task.ID)
	if err != nil || completed.Status != core.TaskCompleted {
		t.Fatalf("completed task = %#v err=%v", completed, err)
	}
	return completed
}

func prepareGT03Capture(t *testing.T, h *realP4Harness, suffix string, terminal bool) (core.Task, core.Claim, string) {
	t.Helper()
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: h.project.ID, AssigneeAgentID: h.worker.ID, Kind: core.TaskWork,
		Title: "GT03 " + suffix, MaxRetries: 1, RequestID: "gt03-create-" + suffix,
	})
	requireNoError(t, err)
	claim := h.claim(t, task.ID)
	workspace, err := h.workspaces.Materialize(context.Background(), taskWorkspaceSpec(claim.Task))
	requireNoError(t, err)
	h.activate(t, claim, workspace.Path, "gt03-"+suffix)
	configureRealGitWorkspace(t, workspace.Path, "GT03 "+suffix)
	writeRealGitFile(t, workspace.Path, "gt03-"+suffix+".txt", suffix+"\n")
	head := commitRealGitWorkspace(t, workspace.Path, "GT03 "+suffix)
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: suffix,
		ExpectedHead: head, RequestID: "gt03-submit-" + suffix,
	}); err != nil {
		t.Fatal(err)
	}
	if terminal {
		h.stopRun(t, claim, "gt03-"+suffix)
	}
	task, err = h.database.Task(context.Background(), task.ID)
	if err != nil || task.Status != core.TaskFinishing || task.PendingAction != "capture" {
		t.Fatalf("prepared GT03 task = %#v err=%v", task, err)
	}
	return task, claim, head
}

type localCaptureHelper struct{ root string }

func (h localCaptureHelper) Capture(ctx context.Context, request gitrepo.CaptureHelperRequest) (gitrepo.CaptureHelperFact, error) {
	handoff := filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID)
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	fact, err := gitcapture.Capture(ctx, gitcapture.Request{
		Workspace: request.Workspace, Handoff: handoff, ExpectedHead: request.ExpectedHead,
		BaseSHA: request.BaseSHA, SourceSHA: request.SourceSHA,
		MaximumBundleBytes: 64 << 20, MaximumObjects: 250_000,
	})
	return gitrepo.CaptureHelperFact{
		HeadSHA: fact.HeadSHA, ReadyBundle: filepath.Join(handoff, gitcapture.ReadyName, gitcapture.BundleName),
		BundleBytes: fact.BundleBytes, ObjectCount: fact.ObjectCount,
	}, err
}

func (h localCaptureHelper) Inspect(ctx context.Context, request gitrepo.WorkspaceInspectRequest) (gitrepo.WorkspaceInspectFact, error) {
	parent := filepath.Join(h.root, request.ProjectID, request.TaskID)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	handoff, err := os.MkdirTemp(parent, "inspect-")
	if err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	defer os.RemoveAll(handoff)
	fact, err := gitcapture.Inspect(ctx, gitcapture.InspectRequest{
		Workspace: request.Workspace, Handoff: handoff, MaximumObjects: 250_000,
	})
	return gitrepo.WorkspaceInspectFact{
		HeadSHA: fact.HeadSHA, StatusDigest: fact.StatusDigest, ObjectCount: fact.ObjectCount,
		Clean: fact.Clean, Unfinished: fact.Unfinished,
	}, err
}

func (h localCaptureHelper) Cleanup(_ context.Context, request gitrepo.CaptureHelperRequest) error {
	return os.RemoveAll(filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID))
}

type realP4Clock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *realP4Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(time.Microsecond)
	return c.value
}

type realP4IDs struct {
	mu     sync.Mutex
	counts map[string]int
}

func (i *realP4IDs) New(prefix string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.counts == nil {
		i.counts = make(map[string]int)
	}
	i.counts[prefix]++
	return fmt.Sprintf("%s-real-%03d", prefix, i.counts[prefix]), nil
}

var _ gitrepo.CaptureHelper = localCaptureHelper{}
