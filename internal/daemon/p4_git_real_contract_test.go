package daemon

import (
	"context"
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
)

func TestGT06RealConflictSecondStaleRequeuesSameIntegrationTaskAndCompletes(t *testing.T) {
	h := newRealP4Harness(t)
	ctx := context.Background()
	sourceTask, err := h.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: h.project.ID, AssigneeAgentID: h.worker.ID, Kind: core.TaskWork,
		Title: "source with same-line change", MaxRetries: 2, RequestID: "gt06-source-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceClaim := h.claim(t, sourceTask.ID)
	sourceSpec := taskWorkspaceSpec(sourceClaim.Task)
	sourceWorkspace, err := h.workspaces.Materialize(ctx, sourceSpec)
	if err != nil {
		t.Fatal(err)
	}
	h.activate(t, sourceClaim, sourceWorkspace.Path, "gt06-source")
	configureRealGitWorkspace(t, sourceWorkspace.Path, "Source Worker")
	writeRealGitFile(t, sourceWorkspace.Path, "README.md", "source line\n")
	sourceHead := commitRealGitWorkspace(t, sourceWorkspace.Path, "source same-line change")
	h.submitAndStop(t, sourceClaim, sourceHead, "gt06-source")
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}
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
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.database.Snapshot(ctx, h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	integration := onlyIntegrationTask(t, snapshot.Tasks)
	if source := taskByID(t, snapshot.Tasks, sourceTask.ID); source.IntegrationTaskID != integration.ID || source.Status != core.TaskSubmitted {
		t.Fatalf("source after first stale = %#v", source)
	}

	firstClaim := h.claim(t, integration.ID)
	integrationSpec := taskWorkspaceSpec(firstClaim.Task)
	integrationWorkspace, err := h.workspaces.Materialize(ctx, integrationSpec)
	if err != nil {
		t.Fatal(err)
	}
	h.activate(t, firstClaim, integrationWorkspace.Path, "gt06-integration-first")
	configureRealGitWorkspace(t, integrationWorkspace.Path, "Integration Worker")
	merge := exec.Command("git", "-C", integrationWorkspace.Path, "merge", "--no-ff", integrationSpec.Source.ConvenienceRef(), "-m", "merge source")
	merge.Env = append(os.Environ(), "LC_ALL=C")
	if raw, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("same-line integration merge unexpectedly succeeded: %s", raw)
	}
	if unmerged := strings.TrimSpace(gitIn(t, integrationWorkspace.Path, "diff", "--name-only", "--diff-filter=U")); unmerged != "README.md" {
		t.Fatalf("unmerged paths = %q", unmerged)
	}
	if _, err := h.service.Progress(ctx, core.ProgressInput{
		Token: firstClaim.Token, Summary: "real same-line conflict in README.md", RequestID: "gt06-conflict-progress",
	}); err != nil {
		t.Fatal(err)
	}
	writeRealGitFile(t, integrationWorkspace.Path, "README.md", "canonical line\nsource line\n")
	gitIn(t, integrationWorkspace.Path, "add", "README.md")
	gitIn(t, integrationWorkspace.Path, "commit", "-m", "resolve real same-line conflict")
	firstIntegrationHead := strings.TrimSpace(gitIn(t, integrationWorkspace.Path, "rev-parse", "HEAD^{commit}"))
	h.submitAndStop(t, firstClaim, firstIntegrationHead, "gt06-integration-first")
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}

	secondCanonical := h.captureAndAdvanceDirect(t, "canonical-second", firstCanonical, "winner.txt", "second winner\n")
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}
	requeued, err := h.database.Task(ctx, integration.ID)
	if err != nil || requeued.Status != core.TaskQueued || requeued.Generation != firstClaim.Run.Generation || requeued.ObservedCanonicalSHA != firstCanonical {
		t.Fatalf("second-stale integration = %#v err=%v", requeued, err)
	}
	sourceAfterStale, err := h.database.Task(ctx, sourceTask.ID)
	if err != nil || sourceAfterStale.Status != core.TaskSubmitted || sourceAfterStale.IntegrationTaskID != integration.ID {
		t.Fatalf("source after second stale = %#v err=%v", sourceAfterStale, err)
	}
	messages, err := h.database.Messages(ctx, core.MessageFilter{TaskID: integration.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
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
	canonicalRef, err := h.workspaces.RefreshCanonical(ctx, integrationSpec, h.project.ControlRepoPath, h.project.CanonicalRef, secondCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitIn(t, integrationWorkspace.Path, "rev-parse", canonicalRef+"^{commit}")); got != secondCanonical {
		t.Fatalf("imported second canonical = %s, want %s", got, secondCanonical)
	}
	h.activate(t, secondClaim, integrationWorkspace.Path, "gt06-integration-second")
	gitIn(t, integrationWorkspace.Path, "merge", "--no-ff", canonicalRef, "-m", "integrate second canonical")
	finalHead := strings.TrimSpace(gitIn(t, integrationWorkspace.Path, "rev-parse", "HEAD^{commit}"))
	h.submitAndStop(t, secondClaim, finalHead, "gt06-integration-second")
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReconcileGit(ctx); err != nil {
		t.Fatal(err)
	}
	finalSource, err := h.database.Task(ctx, sourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalIntegration, err := h.database.Task(ctx, integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalSource.Status != core.TaskCompleted || finalIntegration.Status != core.TaskCompleted ||
		finalSource.FinalCanonicalSHA != finalHead || finalIntegration.FinalCanonicalSHA != finalHead {
		t.Fatalf("final source=%#v integration=%#v", finalSource, finalIntegration)
	}
	if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", h.project.CanonicalRef+"^{commit}")); actual != finalHead {
		t.Fatalf("final canonical = %s, want %s", actual, finalHead)
	}
	finalSnapshot, err := h.database.Snapshot(ctx, h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := onlyIntegrationTask(t, finalSnapshot.Tasks).ID; got != integration.ID {
		t.Fatalf("nested integration task created: got %s want %s", got, integration.ID)
	}
	gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
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
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := initializer.Preflight(context.Background(), source, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := initializer.Paths("project-real-p4", "initialize-real-p4")
	if err != nil {
		t.Fatal(err)
	}
	gitProject := gitrepo.Project{
		ID: "project-real-p4", OperationID: "initialize-real-p4",
		SourcePath: preflight.SourcePath, SourceRef: preflight.SourceRef, InitialSHA: preflight.InitialSHA,
		ControlRepoPath: paths.Final, CanonicalRef: preflight.SourceRef,
	}
	if _, err := initializer.Initialize(context.Background(), gitProject); err != nil {
		t.Fatal(err)
	}
	helper := localCaptureHelper{root: filepath.Join(root, "handoff")}
	if err := os.MkdirAll(helper.root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaces, err := gitrepo.NewWorkspaceManager(initializer, filepath.Join(root, "workspaces"), helper)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	clock := &realP4Clock{value: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)}
	project := core.Project{
		ID: gitProject.ID, Name: "real P4", Source: preflight.SourcePath, SourceRef: preflight.SourceRef,
		InitialSHA: preflight.InitialSHA, ControlRepoPath: paths.Final, CanonicalRef: preflight.SourceRef,
		CanonicalSHA: preflight.InitialSHA, IntegrationAgentID: "agent-real-integrator",
		Status: core.ProjectActive, Version: 1,
	}
	worker := core.Agent{ID: "agent-real-worker", DisplayName: "Worker", AdapterID: "codex", Image: "agent:test", Status: core.AgentActive, Version: 1}
	integrator := core.Agent{ID: "agent-real-integrator", DisplayName: "Integrator", AdapterID: "codex", Image: "agent:test", Status: core.AgentActive, Version: 1}
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
		Now: clock.Now, NewID: ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
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

func (h *realP4Harness) activate(t *testing.T, claim core.Claim, workspace, prefix string) core.Run {
	t.Helper()
	run, err := h.service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: prefix + "-nonce",
		WorkspacePath: workspace, HomePath: filepath.Join(h.root, "homes", claim.Run.ID),
		LogPath: filepath.Join(h.root, "logs", claim.Run.ID+".log"), InstructionsHash: prefix + "-instructions",
		LaunchMode: "start", CleanupOperationID: prefix + "-cleanup", RequestID: prefix + "-prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	fact := core.RunRuntimeFactInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: prefix + "-container", RequestID: prefix + "-created",
	}
	run, err = h.service.RecordContainerCreated(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	fact.ContainerID, fact.RequestID = run.ContainerID, prefix+"-started"
	run, err = h.service.RecordRunStartIssued(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	fact.ContainerID, fact.RequestID = run.ContainerID, prefix+"-active"
	run, err = h.service.ObserveProcessAndActivateRun(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: core.RunExited, ExitCode: &exitCode, TerminalReason: "process_exited", RequestID: prefix + "-terminal",
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	configureRealGitWorkspace(t, workspace.Path, suffix)
	writeRealGitFile(t, workspace.Path, filename, content)
	head := commitRealGitWorkspace(t, workspace.Path, suffix)
	captured, err := h.workspaces.Capture(ctx, gitrepo.CaptureSpec{
		Workspace: spec, RunID: runID, ExpectedHead: head, ControlRepoPath: h.project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
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

func taskByID(t *testing.T, tasks []core.Task, id string) core.Task {
	t.Helper()
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %s missing: %#v", id, tasks)
	return core.Task{}
}

func configureRealGitWorkspace(t *testing.T, workspace, name string) {
	t.Helper()
	gitIn(t, workspace, "config", "user.name", name)
	gitIn(t, workspace, "config", "user.email", strings.ReplaceAll(strings.ToLower(name), " ", "-")+"@example.invalid")
}

func writeRealGitFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o660); err != nil {
		t.Fatal(err)
	}
}

func commitRealGitWorkspace(t *testing.T, workspace, message string) string {
	t.Helper()
	gitIn(t, workspace, "add", "--all")
	gitIn(t, workspace, "commit", "-m", message)
	return strings.TrimSpace(gitIn(t, workspace, "rev-parse", "HEAD^{commit}"))
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
