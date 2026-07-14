//go:build contract

package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/gitcapture"
)

func TestGT03CaptureSixPointProcessSIGKILLRecoveryMatrix(t *testing.T) {
	for _, phase := range []capturePhase{
		capturePhaseIntentChecked,
		capturePhaseHandoffReady,
		capturePhaseBundleVerified,
		capturePhaseObjectsImported,
		capturePhaseTaskRefWritten,
		capturePhaseIntegrityChecked,
	} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			source, initial := newSourceRepository(t)
			controlRoot := filepath.Join(root, "repos")
			initializer, err := New(controlRoot)
			if err != nil {
				t.Fatal(err)
			}
			preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
			if err != nil {
				t.Fatal(err)
			}
			project := testProject(t, initializer, preflight, "project-crash", "operation-crash")
			if _, err := initializer.Initialize(ctx, project); err != nil {
				t.Fatal(err)
			}
			workspaceRoot := filepath.Join(root, "workspaces")
			manager, err := NewWorkspaceManager(initializer, workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-crash", BaseSHA: initial}
			workspace, err := manager.Materialize(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			gitOutput(t, workspace.Path, "config", "user.name", "Crash Capture")
			gitOutput(t, workspace.Path, "config", "user.email", "crash@example.invalid")
			head := commitFile(t, workspace.Path, "crash.txt", string(phase)+"\n", "crash result")
			handoffRoot := filepath.Join(root, "handoff")
			if err := os.MkdirAll(handoffRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(root, "phase-ready")
			environment := captureWorkerEnvironment(controlRoot, workspaceRoot, handoffRoot, head, ready)
			crash := exec.Command(os.Args[0], "-test.run=^TestGT03CaptureCrashWorker$", "-test.count=1")
			crash.Env = append(os.Environ(), append(environment, "COORDPLANE_CAPTURE_CRASH_PHASE="+string(phase))...)
			if err := crash.Start(); err != nil {
				t.Fatal(err)
			}
			waitForCapturePhase(t, crash, ready)
			if err := crash.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := crash.Wait(); err == nil {
				t.Fatal("capture crash worker exited successfully before SIGKILL")
			}

			recover := exec.Command(os.Args[0], "-test.run=^TestGT03CaptureCrashWorker$", "-test.count=1")
			recover.Env = append(os.Environ(), environment...)
			if raw, err := recover.CombinedOutput(); err != nil {
				t.Fatalf("capture recovery: %v\n%s", err, raw)
			}
			taskRef, _ := TaskRef("task-crash", "run-crash")
			if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", taskRef+"^{commit}"); got != head {
				t.Fatalf("recovered task ref = %s, want %s", got, head)
			}
			gitDirOutput(t, project.ControlRepoPath, "fsck", "--full", "--strict")
			importRef := "refs/coordplane/imports/task-crash/run-crash"
			if _, exists, err := initializer.resolveRef(ctx, project.ControlRepoPath, importRef); err != nil || exists {
				t.Fatalf("recovery import ref exists=%t err=%v", exists, err)
			}
			if _, err := os.Stat(filepath.Join(handoffRoot, "project-crash", "task-crash", "run-crash")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful recovery left handoff: %v", err)
			}
		})
	}
}

func TestGT03CaptureCrashWorker(t *testing.T) {
	if os.Getenv("COORDPLANE_CAPTURE_CRASH_WORKER") != "1" {
		return
	}
	controlRoot := os.Getenv("COORDPLANE_CAPTURE_CONTROL_ROOT")
	workspaceRoot := os.Getenv("COORDPLANE_CAPTURE_WORKSPACE_ROOT")
	handoffRoot := os.Getenv("COORDPLANE_CAPTURE_HANDOFF_ROOT")
	head := os.Getenv("COORDPLANE_CAPTURE_HEAD")
	ready := os.Getenv("COORDPLANE_CAPTURE_PHASE_READY")
	initializer, err := New(controlRoot)
	if err != nil {
		t.Fatal(err)
	}
	helper := processCaptureHelper{root: handoffRoot}
	manager, err := NewWorkspaceManager(initializer, workspaceRoot, helper)
	if err != nil {
		t.Fatal(err)
	}
	target := capturePhase(os.Getenv("COORDPLANE_CAPTURE_CRASH_PHASE"))
	contractCapturePhaseHook = func(_ context.Context, phase capturePhase, _ CaptureSpec) error {
		if phase != target {
			return nil
		}
		if err := os.WriteFile(ready, []byte(string(phase)), 0o600); err != nil {
			return err
		}
		select {}
	}
	defer func() { contractCapturePhaseHook = nil }()
	fact, err := manager.Capture(context.Background(), CaptureSpec{
		Workspace: WorkspaceSpec{ProjectID: "project-crash", TaskID: "task-crash", BaseSHA: os.Getenv("COORDPLANE_CAPTURE_BASE")},
		RunID:     "run-crash", ExpectedHead: head,
		ControlRepoPath: filepath.Join(controlRoot, "project-crash.git"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.HeadSHA != head {
		t.Fatalf("capture recovery fact = %#v", fact)
	}
}

type processCaptureHelper struct{ root string }

func (h processCaptureHelper) Capture(ctx context.Context, request CaptureHelperRequest) (CaptureHelperFact, error) {
	handoff := filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID)
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		return CaptureHelperFact{}, err
	}
	fact, err := gitcapture.Capture(ctx, gitcapture.Request{
		Workspace: request.Workspace, Handoff: handoff, ExpectedHead: request.ExpectedHead,
		BaseSHA: request.BaseSHA, SourceSHA: request.SourceSHA,
		MaximumBundleBytes: 64 << 20, MaximumObjects: 250_000,
	})
	return CaptureHelperFact{
		HeadSHA: fact.HeadSHA, ReadyBundle: filepath.Join(handoff, gitcapture.ReadyName, gitcapture.BundleName),
		BundleBytes: fact.BundleBytes, ObjectCount: fact.ObjectCount,
	}, err
}

func (h processCaptureHelper) Inspect(ctx context.Context, request WorkspaceInspectRequest) (WorkspaceInspectFact, error) {
	return inspectWithGitCapture(ctx, h.root, request)
}

func (h processCaptureHelper) Cleanup(_ context.Context, request CaptureHelperRequest) error {
	return os.RemoveAll(filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID))
}

func captureWorkerEnvironment(controlRoot, workspaceRoot, handoffRoot, head, ready string) []string {
	return []string{
		"COORDPLANE_CAPTURE_CRASH_WORKER=1",
		"COORDPLANE_CAPTURE_CONTROL_ROOT=" + controlRoot,
		"COORDPLANE_CAPTURE_WORKSPACE_ROOT=" + workspaceRoot,
		"COORDPLANE_CAPTURE_HANDOFF_ROOT=" + handoffRoot,
		"COORDPLANE_CAPTURE_HEAD=" + head,
		"COORDPLANE_CAPTURE_BASE=" + gitDirOutputForEnvironment(controlRoot),
		"COORDPLANE_CAPTURE_PHASE_READY=" + ready,
	}
}

func gitDirOutputForEnvironment(controlRoot string) string {
	command := exec.Command("git", "--git-dir="+filepath.Join(controlRoot, "project-crash.git"), "rev-parse", "refs/heads/main^{commit}")
	raw, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func waitForCapturePhase(t *testing.T, process *exec.Cmd, ready string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if process.ProcessState != nil && process.ProcessState.Exited() {
			t.Fatal("capture crash worker exited before phase barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = process.Process.Kill()
	_ = process.Wait()
	t.Fatal("capture crash worker did not reach phase barrier")
}
