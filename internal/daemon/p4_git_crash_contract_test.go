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
	"coordplane/internal/gitrepo"
	"coordplane/internal/store"
)

func TestGT03SQLiteTaskRunAndRealGitCaptureRecoverAcrossProcessSIGKILL(t *testing.T) {
	if os.Getenv("COORDPLANE_GT03_CORE_GIT_WORKER") == "1" {
		runGT03CoreGitWorker(t)
		return
	}
	for _, test := range []struct {
		name     string
		terminal bool
		mode     string
	}{
		{name: "pending_capture_run_not_terminal", mode: "nonterminal"},
		{name: "task_ref_written_before_db_submitted", terminal: true, mode: "after_ref"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRealP4Harness(t)
			task, claim, head := prepareGT03Capture(t, h, test.name, test.terminal)
			if err := h.database.Close(); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(h.root, "gt03-worker-ready")
			killGT03CoreGitWorker(t, h.root, task.ID, test.mode, ready)
			h.database, h.service = reopenRealP4Service(t, h)
			persisted, err := h.database.Task(context.Background(), task.ID)
			if err != nil || persisted.Status != core.TaskFinishing || persisted.PendingAction != "capture" || persisted.HeadSHA != "" {
				t.Fatalf("post-kill task = %#v err=%v", persisted, err)
			}
			taskRef, err := gitrepo.TaskRef(task.ID, claim.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.terminal {
				if actual := strings.TrimSpace(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "rev-parse", taskRef+"^{commit}")); actual != head {
					t.Fatalf("post-kill task ref = %s, want %s", actual, head)
				}
			} else {
				probe := exec.Command("git", "--git-dir="+h.project.ControlRepoPath, "show-ref", "--verify", "--quiet", taskRef)
				if err := probe.Run(); err == nil {
					t.Fatalf("nonterminal Run published task ref %s", taskRef)
				}
				h.stopRun(t, claim, test.name)
			}
			if err := h.service.ReconcileGit(context.Background()); err != nil {
				t.Fatal(err)
			}
			submitted, err := h.database.Task(context.Background(), task.ID)
			if err != nil || submitted.Status != core.TaskSubmitted || submitted.HeadSHA != head || submitted.TaskRef != taskRef {
				t.Fatalf("recovered capture = %#v err=%v", submitted, err)
			}
			refs := strings.Fields(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/"+task.ID))
			if len(refs) != 1 || refs[0] != taskRef {
				t.Fatalf("recovered refs = %#v, want %s", refs, taskRef)
			}
			gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "fsck", "--full", "--strict")
		})
	}

	t.Run("db_submitted_before_handoff_cleanup", func(t *testing.T) {
		h := newRealP4Harness(t)
		task, claim, _ := prepareGT03Capture(t, h, "submitted-handoff", true)
		if err := h.service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
		handoff := filepath.Join(h.root, "handoff", h.project.ID, task.ID, claim.Run.ID)
		if err := os.MkdirAll(filepath.Join(handoff, "capture.ready"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(handoff, "capture.ready", "facts.json"), []byte("diagnostic evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.database.Close(); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(h.root, "gt03-submitted-ready")
		killGT03CoreGitWorker(t, h.root, task.ID, "submitted_handoff", ready)
		helper := &dockerCaptureHelper{root: filepath.Join(h.root, "handoff")}
		if err := helper.Recover(nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(handoff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("submitted capture handoff survived recovery: %v", err)
		}
		h.database, h.service = reopenRealP4Service(t, h)
		if err := h.service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
		submitted, err := h.database.Task(context.Background(), task.ID)
		if err != nil || submitted.Status != core.TaskSubmitted || submitted.PendingAction != "" {
			t.Fatalf("submitted handoff replay task = %#v err=%v", submitted, err)
		}
		taskRef, _ := gitrepo.TaskRef(task.ID, claim.Run.ID)
		refs := strings.Fields(gitOutput(t, "--git-dir="+h.project.ControlRepoPath, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/"+task.ID))
		if len(refs) != 1 || refs[0] != taskRef {
			t.Fatalf("submitted handoff replay refs = %#v", refs)
		}
	})
}

func TestGT04RealCASProcessKillThenDescendantReplayCompletesSQLite(t *testing.T) {
	h := newRealP4Harness(t)
	task, _, head := prepareGT03Capture(t, h, "gt04-cas-kill", true)
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, IntegrationAgentID: h.integrator.ID, RequestID: "gt04-cas-kill-accept",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.database.Close(); err != nil {
		t.Fatal(err)
	}
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
	if err := h.service.ReconcileGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := h.database.Task(context.Background(), task.ID)
	if err != nil || completed.Status != core.TaskCompleted || completed.FinalCanonicalSHA != descendant || completed.PendingAction != "" {
		t.Fatalf("included CAS recovery = %#v err=%v", completed, err)
	}
	snapshot, err := h.database.Snapshot(context.Background(), h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
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

func prepareGT03Capture(t *testing.T, h *realP4Harness, suffix string, terminal bool) (core.Task, core.Claim, string) {
	t.Helper()
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: h.project.ID, AssigneeAgentID: h.worker.ID, Kind: core.TaskWork,
		Title: "GT03 " + suffix, MaxRetries: 1, RequestID: "gt03-create-" + suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := h.claim(t, task.ID)
	workspace, err := h.workspaces.Materialize(context.Background(), taskWorkspaceSpec(claim.Task))
	if err != nil {
		t.Fatal(err)
	}
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

func killGT03CoreGitWorker(t *testing.T, root, taskID, mode, ready string) {
	t.Helper()
	worker := exec.Command(os.Args[0], "-test.run=^TestGT03SQLiteTaskRunAndRealGitCaptureRecoverAcrossProcessSIGKILL$", "-test.count=1")
	worker.Env = append(os.Environ(),
		"COORDPLANE_GT03_CORE_GIT_WORKER=1",
		"COORDPLANE_GT03_ROOT="+root,
		"COORDPLANE_GT03_TASK_ID="+taskID,
		"COORDPLANE_GT03_MODE="+mode,
		"COORDPLANE_GT03_READY="+ready,
	)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	waitForGT03File(t, ready)
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := worker.Wait(); err == nil {
		t.Fatal("GT03 worker exited successfully before SIGKILL")
	}
}

func waitForGT03File(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat GT03 ready file: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GT03 worker did not publish %s", path)
}

func runGT03CoreGitWorker(t *testing.T) {
	root := os.Getenv("COORDPLANE_GT03_ROOT")
	database, err := store.Open(context.Background(), filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	initializer, err := gitrepo.New(filepath.Join(root, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := gitrepo.NewWorkspaceManager(initializer, filepath.Join(root, "workspaces"), localCaptureHelper{root: filepath.Join(root, "handoff")})
	if err != nil {
		t.Fatal(err)
	}
	adapter := projectGitAdapter{initializer: initializer, workspaces: workspaces}
	var controller core.ProjectGit = adapter
	if os.Getenv("COORDPLANE_GT03_MODE") == "after_ref" {
		controller = killAfterCaptureProjectGit{projectGitAdapter: adapter, ready: os.Getenv("COORDPLANE_GT03_READY")}
	} else if os.Getenv("COORDPLANE_GT03_MODE") == "after_cas" {
		controller = killAfterAdvanceProjectGit{projectGitAdapter: adapter, ready: os.Getenv("COORDPLANE_GT03_READY")}
	}
	service, err := core.NewService(database, controller, core.ServiceOptions{
		Now: time.Now, NewID: (&realP4IDs{}).New, MaxParallelRuns: 4, AdapterIDs: []string{"codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("COORDPLANE_GT03_MODE") {
	case "after_ref", "after_cas":
		if err := service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
	case "nonterminal":
		if err := service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
		publishGT03Ready(t)
	case "submitted_handoff":
		task, err := database.Task(context.Background(), os.Getenv("COORDPLANE_GT03_TASK_ID"))
		if err != nil || task.Status != core.TaskSubmitted {
			t.Fatalf("submitted worker task = %#v err=%v", task, err)
		}
		publishGT03Ready(t)
	default:
		t.Fatalf("unknown GT03 worker mode %q", os.Getenv("COORDPLANE_GT03_MODE"))
	}
}

func publishGT03Ready(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("COORDPLANE_GT03_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

type killAfterCaptureProjectGit struct {
	projectGitAdapter
	ready string
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

func (g killAfterCaptureProjectGit) Capture(ctx context.Context, intent core.GitCaptureIntent) (core.GitCaptureFact, error) {
	fact, err := g.projectGitAdapter.Capture(ctx, intent)
	if err != nil {
		return fact, err
	}
	if err := os.WriteFile(g.ready, []byte("task ref written"), 0o600); err != nil {
		return core.GitCaptureFact{}, err
	}
	select {}
}

func reopenRealP4Service(t *testing.T, h *realP4Harness) (*store.Store, *core.Service) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(h.root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service, err := core.NewService(database, projectGitAdapter{initializer: h.initializer, workspaces: h.workspaces}, core.ServiceOptions{
		Now: time.Now, NewID: (&realP4IDs{}).New, MaxParallelRuns: 4, AdapterIDs: []string{"codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.SetReady(true, "")
	return database, service
}
