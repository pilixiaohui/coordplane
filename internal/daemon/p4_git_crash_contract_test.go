//go:build contract

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/gitcapture"
	"coordplane/internal/gitrepo"
	"coordplane/internal/store"
	"coordplane/tests/testsupport"
)

func TestGT03SQLiteTaskRunAndRealGitCaptureRecoverAcrossProcessSIGKILL(t *testing.T) {
	if os.Getenv("COORDPLANE_GT03_CORE_GIT_WORKER") == "1" {
		runGT03CoreGitWorker(t)
		return
	}
	points := []struct {
		name, phase string
		terminal    bool
	}{
		{name: "01_pending_capture_run_not_terminal", phase: "nonterminal"},
		{name: "02_run_terminal_handoff_not_ready", phase: "intent_checked", terminal: true},
		{name: "03_handoff_ready_before_import", phase: "handoff_ready", terminal: true},
		{name: "04_objects_imported_before_task_ref", phase: "objects_imported", terminal: true},
		{name: "05_task_ref_before_db_submitted", phase: "task_ref_written", terminal: true},
		{name: "06_db_submitted_before_handoff_cleanup", phase: "submitted_before_cleanup", terminal: true},
	}
	for _, test := range points {
		t.Run(test.name, func(t *testing.T) {
			h := newRealP4Harness(t)
			task, claim, head := prepareGT03Capture(t, h, test.name, test.terminal)
			requireNoError(t, h.database.Close())
			killGT03CoreGitWorker(t, h.root, task.ID, test.phase, filepath.Join(h.root, "gt03-worker-ready"))

			postKill, err := store.Open(context.Background(), filepath.Join(h.root, "coordplane.db"))
			requireNoError(t, err)
			closePostKill := func() { _ = postKill.Close() }
			persisted, err := postKill.Task(context.Background(), task.ID)
			requireNoError(t, err)
			if test.phase == "submitted_before_cleanup" {
				if persisted.Status != core.TaskSubmitted || persisted.PendingAction != "" || persisted.HeadSHA != head {
					t.Fatalf("post-kill submitted task = %#v", persisted)
				}
				handoff := filepath.Join(h.root, "handoff", task.ProjectID, task.ID, claim.Run.ID)
				if _, err := os.Stat(handoff); err != nil {
					t.Fatalf("post-kill finalized handoff missing: %v", err)
				}
				if _, err := os.Stat(filepath.Join(handoff, gitcapture.ReadyName)); err != nil {
					t.Fatalf("post-kill capture.ready missing: %v", err)
				}
				beforeRestart := p4StoreDurableSignature(t, postKill, h.project.ID)
				closePostKill()
				requireNoError(t, os.Chmod(h.root, 0o700))
				configPath := testsupport.WriteFile(t, filepath.Join(h.root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: h.root, OperatorSocket: filepath.Join(h.root, "operator.sock"), WorkspaceRoot: filepath.Join(h.root, "workspaces"), AgentHomeRoot: filepath.Join(h.root, "homes"), LogRoot: filepath.Join(h.root, "logs"), MaxParallelRuns: 4, CompletedWorkspace: "24h", TerminalTaskRef: "168h", RunLog: "168h", DockerNetwork: "coordplane", DefaultImage: "agent:test"}), 0o600)
				first, err := buildComponents(context.Background(), configPath)
				requireNoError(t, err)
				h.database = first.store
				assertGT03RecoveredCapture(t, h, task, claim, head)
				assertP4QuarantineEmpty(t, h.root)
				if after := p4StoreDurableSignature(t, first.store, h.project.ID); after != beforeRestart {
					t.Fatal("first production restart changed finalized capture state")
				}
				requireNoError(t, first.Close())
				second, err := buildComponents(context.Background(), configPath)
				requireNoError(t, err)
				if after := p4StoreDurableSignature(t, second.store, h.project.ID); after != beforeRestart {
					t.Fatal("second production restart changed finalized capture state")
				}
				h.database = second.store
				assertGT03RecoveredCapture(t, h, task, claim, head)
				assertP4QuarantineEmpty(t, h.root)
				requireNoError(t, second.Close())
				return
			} else if persisted.Status != core.TaskFinishing || persisted.PendingAction != "capture" || persisted.HeadSHA != "" {
				t.Fatalf("post-kill pending task = %#v", persisted)
			}
			closePostKill()

			request := gitrepo.CaptureHelperRequest{
				ProjectID: task.ProjectID, TaskID: task.ID, RunID: claim.Run.ID,
				Workspace: claim.Run.WorkspacePath, ExpectedHead: task.PendingExpectedSHA, BaseSHA: task.BaseSHA,
			}
			valid := []gitrepo.CaptureHelperRequest{request}
			requireNoError(t, (&dockerCaptureHelper{root: filepath.Join(h.root, "handoff")}).Recover(valid))
			h.database, h.service = reopenRealP4Service(t, h)
			if !test.terminal {
				h.stopRun(t, claim, test.name)
			}
			h.reconcileGit(t)
			assertGT03RecoveredCapture(t, h, task, claim, head)
		})
	}

	t.Run("task_ref_is_diagnostic_after_capture_fence_changes", func(t *testing.T) {
		h := newRealP4Harness(t)
		task, claim, head := prepareGT03Capture(t, h, "stale-fence", true)
		requireNoError(t, h.database.Close())
		killGT03CoreGitWorker(t, h.root, task.ID, "task_ref_written", filepath.Join(h.root, "gt03-fence-ready"))
		h.database, h.service = reopenRealP4Service(t, h)
		if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
			persisted, err := tx.Task(task.ID)
			if err != nil {
				return err
			}
			expectedVersion := persisted.Version
			persisted.Status, persisted.PendingAction, persisted.CurrentRunID = core.TaskCancelled, "", ""
			persisted.PendingActionID, persisted.PendingActionRunID, persisted.PendingExpectedSHA = "", "", ""
			persisted.PendingActionVersion, persisted.Generation = 0, persisted.Generation+1
			persisted.ClosedAt, persisted.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)
			persisted.Version++
			return tx.UpdateTask(persisted, expectedVersion, core.TaskFinishing)
		}); err != nil {
			t.Fatal(err)
		}
		h.reconcileGit(t)
		cancelled, err := h.database.Task(context.Background(), task.ID)
		if err != nil || cancelled.Status != core.TaskCancelled || cancelled.HeadSHA != "" {
			t.Fatalf("stale capture fence task = %#v err=%v", cancelled, err)
		}
		taskRef, _ := gitrepo.TaskRef(task.ID, claim.Run.ID)
		if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", taskRef+"^{commit}")); actual != head {
			t.Fatalf("diagnostic task ref = %s, want %s", actual, head)
		}
		legacyQuarantine := filepath.Join(h.root, "handoff", "quarantine", "legacy")
		requireNoError(t, os.MkdirAll(legacyQuarantine, 0o700))
		requireNoError(t, os.WriteFile(filepath.Join(legacyQuarantine, "bundle"), []byte("obsolete\n"), 0o600))
		recovery := &dockerCaptureHelper{root: filepath.Join(h.root, "handoff")}
		requireNoError(t, recovery.Recover(nil))
		requireNoError(t, recovery.Recover(nil))
		if _, err := os.Stat(filepath.Join(h.root, "handoff", task.ProjectID, task.ID, claim.Run.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale capture handoff survived production recovery: %v", err)
		}
		assertP4QuarantineEmpty(t, h.root)
	})
}

func assertGT03RecoveredCapture(t *testing.T, h *realP4Harness, task core.Task, claim core.Claim, head string) {
	t.Helper()
	submitted, err := h.database.Task(context.Background(), task.ID)
	taskRef, refErr := gitrepo.TaskRef(task.ID, claim.Run.ID)
	if refErr != nil {
		t.Fatal(refErr)
	}
	if err != nil || submitted.Status != core.TaskSubmitted || submitted.HeadSHA != head || submitted.TaskRef != taskRef {
		t.Fatalf("recovered capture = %#v err=%v", submitted, err)
	}
	refs := strings.Fields(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/"+task.ID))
	if len(refs) != 1 || refs[0] != taskRef {
		t.Fatalf("recovered refs = %#v, want %s", refs, taskRef)
	}
	if canonical := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", h.project.CanonicalRef+"^{commit}")); canonical != h.project.InitialSHA {
		t.Fatalf("capture moved canonical to %s, want %s", canonical, h.project.InitialSHA)
	}
	if _, err := os.Stat(filepath.Join(h.root, "handoff", h.project.ID, task.ID, claim.Run.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered handoff survived: %v", err)
	}
	gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
}

func assertP4QuarantineEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "handoff", "quarantine"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("capture quarantine entries = %#v", entries)
	}
}

func TestGT03ExistingTaskRefReplayIsIdempotentAndRejectsDifferentHead(t *testing.T) {
	h := newRealP4Harness(t)
	task, claim, head := prepareGT03Capture(t, h, "existing-ref", true)
	h.reconcileGit(t)
	var err error
	task, err = h.database.Task(context.Background(), task.ID)
	requireNoError(t, err)
	workspacePath, err := h.workspaces.Path(task.ProjectID, task.ID)
	requireNoError(t, err)
	intent := core.GitCaptureIntent{
		ProjectID: task.ProjectID, TaskID: task.ID, RunID: claim.Run.ID,
		WorkspacePath: workspacePath, ControlRepo: h.project.ControlRepoPath,
		BaseSHA: task.BaseSHA, ExpectedHead: head,
	}
	adapter := projectGitAdapter{initializer: h.initializer, workspaces: h.workspaces}
	before := p4DurableSignature(t, h)
	for _, test := range []struct {
		name, expectedHead string
		wantError          bool
	}{
		{name: "same_head", expectedHead: head},
		{name: "different_expected_head", expectedHead: h.project.InitialSHA, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent.ExpectedHead = test.expectedHead
			fact, err := adapter.Capture(context.Background(), intent)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "expected head") {
					t.Fatalf("mismatched existing ref error = %v", err)
				}
			} else if err != nil || fact.HeadSHA != head || fact.TaskRef != task.TaskRef {
				t.Fatalf("same-head replay = %#v err=%v", fact, err)
			}
			if got := p4DurableSignature(t, h); got != before {
				t.Fatal("existing-ref replay changed SQLite or Event state")
			}
			assertP4Refs(t, h, h.project.InitialSHA, task.ID, head)
		})
	}
}

func TestGT04RealCASProcessKillThenDescendantReplayCompletesSQLite(t *testing.T) {
	h := newRealP4Harness(t)
	task, _, head := prepareGT03Capture(t, h, "gt04-cas-kill", true)
	h.reconcileGit(t)
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: h.integrator.ID, RequestID: "gt04-cas-kill-accept",
	}); err != nil {
		t.Fatal(err)
	}
	requireNoError(t, h.database.Close())
	ready := filepath.Join(h.root, "gt04-cas-ready")
	killGT03CoreGitWorker(t, h.root, task.ID, "after_cas", ready)
	if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", h.project.CanonicalRef+"^{commit}")); actual != head {
		t.Fatalf("post-kill canonical = %s, want CAS target %s", actual, head)
	}
	descendant := h.captureAndAdvanceDirect(t, "gt04-descendant", head, "descendant.txt", "advanced after CAS kill\n")
	h.database, h.service = reopenRealP4Service(t, h)
	pending, err := h.database.Task(context.Background(), task.ID)
	if err != nil || pending.PendingAction != "advance" || pending.Status != core.TaskSubmitted {
		t.Fatalf("post-kill pending advance = %#v err=%v", pending, err)
	}
	h.reconcileGit(t)
	completed, err := h.database.Task(context.Background(), task.ID)
	if err != nil || completed.Status != core.TaskCompleted || completed.FinalCanonicalSHA != descendant || completed.PendingAction != "" {
		t.Fatalf("included CAS recovery = %#v err=%v", completed, err)
	}
	snapshot, err := h.database.Snapshot(context.Background(), h.project.ID)
	requireNoError(t, err)
	for _, candidate := range snapshot.Tasks {
		if candidate.Kind == core.TaskIntegration {
			t.Fatalf("included CAS recovery created integration task: %#v", candidate)
		}
	}
	project, err := h.database.Project(context.Background(), h.project.ID)
	if err != nil || project.CanonicalSHA != descendant {
		t.Fatalf("included CAS project = %#v err=%v", project, err)
	}
	gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
}

func killGT03CoreGitWorker(t *testing.T, root, taskID, mode, ready string) {
	t.Helper()
	worker := exec.Command(os.Args[0], "-test.run=^TestGT03SQLiteTaskRunAndRealGitCaptureRecoverAcrossProcessSIGKILL$", "-test.count=1")
	environment := append(os.Environ(),
		"COORDPLANE_GT03_CORE_GIT_WORKER=1",
		"COORDPLANE_GT03_ROOT="+root,
		"COORDPLANE_GT03_TASK_ID="+taskID,
		"COORDPLANE_GT03_MODE="+mode,
		"COORDPLANE_GT03_READY="+ready,
		"COORDPLANE_CONTRACT_CAPTURE_PHASE="+mode,
		"COORDPLANE_CONTRACT_CAPTURE_PHASE_READY="+ready,
	)
	if mode == "submitted_before_cleanup" {
		environment = append(environment, "COORDPLANE_CONTRACT_CAPTURE_FINALIZED_READY="+ready)
	}
	worker.Env = environment
	requireNoError(t, worker.Start())
	waitP4File(t, ready)
	requireNoError(t, worker.Process.Kill())
	if err := worker.Wait(); err == nil {
		t.Fatal("GT03 worker exited successfully before SIGKILL")
	}
}

func runGT03CoreGitWorker(t *testing.T) {
	root := os.Getenv("COORDPLANE_GT03_ROOT")
	database, err := store.Open(context.Background(), filepath.Join(root, "coordplane.db"))
	requireNoError(t, err)
	defer database.Close()
	initializer, err := gitrepo.New(filepath.Join(root, "repos"))
	requireNoError(t, err)
	helper := localCaptureHelper{root: filepath.Join(root, "handoff")}
	workspaces, err := gitrepo.NewWorkspaceManager(initializer, filepath.Join(root, "workspaces"), helper)
	requireNoError(t, err)
	adapter := projectGitAdapter{initializer: initializer, workspaces: workspaces}
	var controller core.ProjectGit = adapter
	if os.Getenv("COORDPLANE_GT03_MODE") == "after_cas" {
		controller = killAfterAdvanceProjectGit{projectGitAdapter: adapter, ready: os.Getenv("COORDPLANE_GT03_READY")}
	}
	service, err := core.NewService(database, controller, core.ServiceOptions{
		Now: time.Now, NewID: (&realP4IDs{}).New, MaxParallelRuns: 4, AdapterIDs: []string{"claude"},
	})
	requireNoError(t, err)
	switch os.Getenv("COORDPLANE_GT03_MODE") {
	case "after_cas":
		requireNoError(t, service.ReconcileGit(context.Background()))
	case "nonterminal":
		requireNoError(t, service.ReconcileGit(context.Background()))
		publishGT03Ready(t)
	default:
		requireNoError(t, service.ReconcileGit(context.Background()))
	}
}

func publishGT03Ready(t *testing.T) {
	t.Helper()
	requireNoError(t, os.WriteFile(os.Getenv("COORDPLANE_GT03_READY"), []byte("ready"), 0o600))
	select {}
}

type killAfterAdvanceProjectGit struct {
	projectGitAdapter
	ready string
}

func (g killAfterAdvanceProjectGit) Advance(ctx context.Context, intent core.GitAdvanceIntent) (core.GitAdvanceFact, error) {
	fact, err := g.projectGitAdapter.Advance(ctx, intent)
	if err != nil {
		return fact, err
	}
	if err := os.WriteFile(g.ready, []byte("canonical updated"), 0o600); err != nil {
		return core.GitAdvanceFact{}, err
	}
	select {}
}

func reopenRealP4Service(t *testing.T, h *realP4Harness) (*store.Store, *core.Service) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(h.root, "coordplane.db"))
	requireNoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	service, err := core.NewService(database, projectGitAdapter{initializer: h.initializer, workspaces: h.workspaces}, core.ServiceOptions{
		Now: time.Now, NewID: (&realP4IDs{}).New, MaxParallelRuns: 4, AdapterIDs: []string{"claude"},
	})
	requireNoError(t, err)
	service.SetReady(true, "")
	return database, service
}
