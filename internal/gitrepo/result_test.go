package gitrepo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGT02DirtyOrMismatchedWorkspaceCreatesNoControlRef(t *testing.T) {
	ctx := context.Background()
	_, manager, project, _, initial := newWorkspaceFixture(t)

	tests := []struct {
		name   string
		mutate func(t *testing.T, path string) string
		want   string
	}{
		{
			name: "dirty untracked file",
			mutate: func(t *testing.T, path string) string {
				if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("dirty\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return initial
			},
			want: "clean",
		},
		{
			name: "incorrect expected head",
			mutate: func(t *testing.T, path string) string {
				gitOutput(t, path, "config", "user.name", "Capture Test")
				gitOutput(t, path, "config", "user.email", "capture@example.invalid")
				_ = commitFile(t, path, "result.txt", "new\n", "new head")
				return initial
			},
			want: "expected head",
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			taskID := "task-reject-" + string(rune('a'+index))
			runID := "run-reject-" + string(rune('a'+index))
			spec := WorkspaceSpec{ProjectID: project.ID, TaskID: taskID, BaseSHA: initial}
			workspace, err := manager.Materialize(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			expected := test.mutate(t, workspace.Path)
			_, err = manager.Capture(ctx, CaptureSpec{
				Workspace: spec, RunID: runID, ExpectedHead: expected,
				ControlRepoPath: project.ControlRepoPath,
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("Capture() error = %v, want containing %q", err, test.want)
			}
			ref := "refs/coordplane/tasks/" + taskID + "/runs/" + runID
			if _, exists, resolveErr := manager.initializer.resolveRef(ctx, project.ControlRepoPath, ref); resolveErr != nil || exists {
				t.Fatalf("rejected capture ref exists=%t err=%v", exists, resolveErr)
			}
		})
	}
}

func TestGT03CorruptReadyBundleFailsLoudAndLeavesControllerFSCKClean(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	manager.capture = corruptCaptureHelper{next: manager.capture}
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-corrupt", BaseSHA: initial}
	workspace, err := manager.Materialize(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, workspace.Path, "config", "user.name", "Corrupt Capture")
	gitOutput(t, workspace.Path, "config", "user.email", "corrupt@example.invalid")
	head := commitFile(t, workspace.Path, "corrupt.txt", "corrupt me\n", "corrupt capture")
	_, err = manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-corrupt", ExpectedHead: head, ControlRepoPath: project.ControlRepoPath,
	})
	if err == nil {
		t.Fatalf("corrupt capture error = %v", err)
	}
	taskRef, _ := TaskRef(spec.TaskID, "run-corrupt")
	if _, exists, resolveErr := initializer.resolveRef(ctx, project.ControlRepoPath, taskRef); resolveErr != nil || exists {
		t.Fatalf("corrupt capture task ref exists=%t err=%v", exists, resolveErr)
	}
	gitDirOutput(t, project.ControlRepoPath, "fsck", "--full", "--strict")
}

func TestGT03PublicCaptureErrorsRedactDistinctHandoffRootAndRawGitArgs(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"verify", "fetch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			initializer, manager, project, _, initial := newWorkspaceFixture(t)
			handoffRoot := t.TempDir()
			manager.capture = testCaptureHelper{root: handoffRoot}
			spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-redact-" + mode, BaseSHA: initial}
			workspace, err := manager.Materialize(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			gitOutput(t, workspace.Path, "config", "user.name", "Redaction Test")
			gitOutput(t, workspace.Path, "config", "user.email", "redaction@example.invalid")
			head := commitFile(t, workspace.Path, "redact.txt", mode+"\n", "redaction "+mode)
			runID := "run-redact-" + mode
			wrapper := filepath.Join(t.TempDir(), "git")
			script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" %s "*) echo "$*" >&2; exit 23 ;;
esac
exec %q "$@"
`, map[string]string{"verify": "bundle verify", "fetch": "fetch"}[mode], realGit)
			if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			initializer.gitPath = wrapper
			_, err = manager.Capture(ctx, CaptureSpec{
				Workspace: spec, RunID: runID, ExpectedHead: head, ControlRepoPath: project.ControlRepoPath,
			})
			if err == nil {
				t.Fatal("Capture() succeeded through failing Git wrapper")
			}
			want := map[string]string{
				"verify": "capture bundle verification failed",
				"fetch":  "import capture handoff",
			}[mode]
			if !strings.Contains(strings.ToLower(err.Error()), want) {
				t.Fatalf("public error = %v, want safe context %q", err, want)
			}
			readyBundle := filepath.Join(handoffRoot, project.ID, spec.TaskID, runID, "capture.ready", "result.bundle")
			for _, forbidden := range []string{
				handoffRoot, manager.root, initializer.root, project.ControlRepoPath, readyBundle,
				"--git-dir", "bundle verify", "--no-write-fetch-head", "HEAD:refs/coordplane/imports",
			} {
				if forbidden != "" && strings.Contains(err.Error(), forbidden) {
					t.Fatalf("public error leaked %q: %v", forbidden, err)
				}
			}
			taskRef, err := TaskRef(spec.TaskID, runID)
			if err != nil {
				t.Fatal(err)
			}
			initializer.gitPath = realGit
			if _, exists, err := initializer.resolveRef(ctx, project.ControlRepoPath, taskRef); err != nil || exists {
				t.Fatalf("rejected capture task ref exists=%t err=%v", exists, err)
			}
			gitDirOutput(t, project.ControlRepoPath, "fsck", "--full", "--strict")
		})
	}
}

type corruptCaptureHelper struct{ next CaptureHelper }

func (h corruptCaptureHelper) Capture(ctx context.Context, request CaptureHelperRequest) (CaptureHelperFact, error) {
	fact, err := h.next.Capture(ctx, request)
	if err != nil {
		return CaptureHelperFact{}, err
	}
	raw, err := os.ReadFile(fact.ReadyBundle)
	if err != nil {
		return CaptureHelperFact{}, err
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(fact.ReadyBundle, raw, 0o600); err != nil {
		return CaptureHelperFact{}, err
	}
	return fact, nil
}

func (h corruptCaptureHelper) Inspect(ctx context.Context, request WorkspaceInspectRequest) (WorkspaceInspectFact, error) {
	return h.next.Inspect(ctx, request)
}

func (h corruptCaptureHelper) Cleanup(ctx context.Context, request CaptureHelperRequest) error {
	return h.next.Cleanup(ctx, request)
}

func TestGT04ConcurrentExpectedOldCASBarrierUpdatesCanonicalExactlyOnce(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	capture := func(taskID, runID, filename string) CaptureFact {
		t.Helper()
		spec := WorkspaceSpec{ProjectID: project.ID, TaskID: taskID, BaseSHA: initial}
		workspace, err := manager.Materialize(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		gitOutput(t, workspace.Path, "config", "user.name", taskID)
		gitOutput(t, workspace.Path, "config", "user.email", taskID+"@example.invalid")
		head := commitFile(t, workspace.Path, filename, taskID+"\n", taskID)
		fact, err := manager.Capture(ctx, CaptureSpec{
			Workspace: spec, RunID: runID, ExpectedHead: head, ControlRepoPath: project.ControlRepoPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		return fact
	}
	left := capture("task-cas-left", "run-cas-left", "left.txt")
	right := capture("task-cas-right", "run-cas-right", "right.txt")
	type result struct {
		fact AdvanceFact
		err  error
	}
	var winner CaptureFact
	for iteration := 0; iteration < 8; iteration++ {
		start := make(chan struct{})
		results := make(chan result, 2)
		for _, captured := range []CaptureFact{left, right} {
			captured := captured
			go func() {
				<-start
				fact, err := initializer.Advance(ctx, AdvanceSpec{
					ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
					CanonicalRef: project.CanonicalRef, TaskRef: captured.TaskRef,
					ExpectedOldSHA: initial, TargetSHA: captured.HeadSHA,
				})
				results <- result{fact: fact, err: err}
			}()
		}
		close(start)
		updated, stale := 0, 0
		for range 2 {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			switch result.fact.Outcome {
			case AdvanceUpdated:
				updated++
			case AdvanceStale:
				stale++
			default:
				t.Fatalf("iteration %d concurrent advance = %#v", iteration, result.fact)
			}
		}
		if updated != 1 || stale != 1 {
			t.Fatalf("iteration %d outcomes updated=%d stale=%d", iteration, updated, stale)
		}
		canonical := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}")
		switch canonical {
		case left.HeadSHA:
			winner = left
		case right.HeadSHA:
			winner = right
		default:
			t.Fatalf("iteration %d canonical = %s, want one captured head", iteration, canonical)
		}
		if iteration < 7 {
			gitDirOutput(t, project.ControlRepoPath, "update-ref", project.CanonicalRef, initial, canonical)
		}
	}

	descendantSpec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-cas-descendant", BaseSHA: winner.HeadSHA}
	descendantWorkspace, err := manager.Materialize(ctx, descendantSpec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, descendantWorkspace.Path, "config", "user.name", "CAS Descendant")
	gitOutput(t, descendantWorkspace.Path, "config", "user.email", "cas-descendant@example.invalid")
	descendantHead := commitFile(t, descendantWorkspace.Path, "descendant.txt", "descendant\n", "canonical descendant")
	descendant, err := manager.Capture(ctx, CaptureSpec{
		Workspace: descendantSpec, RunID: "run-cas-descendant", ExpectedHead: descendantHead,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath, CanonicalRef: project.CanonicalRef,
		TaskRef: descendant.TaskRef, ExpectedOldSHA: winner.HeadSHA, TargetSHA: descendant.HeadSHA,
	}); err != nil || advanced.Outcome != AdvanceUpdated {
		t.Fatalf("advance winner descendant = %#v err=%v", advanced, err)
	}
	if replay, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath, CanonicalRef: project.CanonicalRef,
		TaskRef: winner.TaskRef, ExpectedOldSHA: initial, TargetSHA: winner.HeadSHA,
	}); err != nil || replay.Outcome != AdvanceIncluded || replay.ActualSHA != descendant.HeadSHA {
		t.Fatalf("post-CAS descendant replay = %#v err=%v", replay, err)
	}
	for _, captured := range []CaptureFact{left, right} {
		if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", captured.TaskRef+"^{commit}"); got != captured.HeadSHA {
			t.Fatalf("concurrent loser ref %s = %s, want %s", captured.TaskRef, got, captured.HeadSHA)
		}
	}
	gitDirOutput(t, project.ControlRepoPath, "fsck", "--full", "--strict")
}

func TestGT05IntegrationCaptureRequiresRealCanonicalAndSourceLineage(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)

	sourceSpec := WorkspaceSpec{ProjectID: project.ID, TaskID: "source-lineage", BaseSHA: initial}
	sourceWorkspace, err := manager.Materialize(ctx, sourceSpec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, sourceWorkspace.Path, "config", "user.name", "Source")
	gitOutput(t, sourceWorkspace.Path, "config", "user.email", "source@example.invalid")
	sourceHead := commitFile(t, sourceWorkspace.Path, "source.txt", "source\n", "source")
	sourceCapture, err := manager.Capture(ctx, CaptureSpec{
		Workspace: sourceSpec, RunID: "source-run", ExpectedHead: sourceHead,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := WorkspaceSource{
		TaskID: sourceSpec.TaskID, RunID: "source-run",
		TaskRef: sourceCapture.TaskRef, HeadSHA: sourceHead,
	}

	canonicalSpec := WorkspaceSpec{ProjectID: project.ID, TaskID: "canonical-lineage", BaseSHA: initial}
	canonicalWorkspace, err := manager.Materialize(ctx, canonicalSpec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, canonicalWorkspace.Path, "config", "user.name", "Canonical")
	gitOutput(t, canonicalWorkspace.Path, "config", "user.email", "canonical@example.invalid")
	canonicalHead := commitFile(t, canonicalWorkspace.Path, "canonical.txt", "canonical\n", "canonical")
	canonicalCapture, err := manager.Capture(ctx, CaptureSpec{
		Workspace: canonicalSpec, RunID: "canonical-run", ExpectedHead: canonicalHead,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: canonicalCapture.TaskRef,
		ExpectedOldSHA: initial, TargetSHA: canonicalHead,
	}); err != nil {
		t.Fatal(err)
	}

	invalidSpec := WorkspaceSpec{
		ProjectID: project.ID, TaskID: "integration-invalid", BaseSHA: canonicalHead, Source: &source,
	}
	invalidWorkspace, err := manager.Materialize(ctx, invalidSpec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, invalidWorkspace.Path, "config", "user.name", "Invalid Integration")
	gitOutput(t, invalidWorkspace.Path, "config", "user.email", "invalid@example.invalid")
	invalidHead := commitFile(t, invalidWorkspace.Path, "invalid.txt", "no source\n", "invalid integration")
	if _, err := manager.Capture(ctx, CaptureSpec{
		Workspace: invalidSpec, RunID: "invalid-run", ExpectedHead: invalidHead,
		ControlRepoPath: project.ControlRepoPath,
	}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "source") {
		t.Fatalf("source-free integration capture error = %v", err)
	}
	invalidRef, _ := TaskRef(invalidSpec.TaskID, "invalid-run")
	if _, exists, err := initializer.resolveRef(ctx, project.ControlRepoPath, invalidRef); err != nil || exists {
		t.Fatalf("invalid integration ref exists=%t err=%v", exists, err)
	}

	validSpec := WorkspaceSpec{
		ProjectID: project.ID, TaskID: "integration-valid", BaseSHA: canonicalHead, Source: &source,
	}
	validWorkspace, err := manager.Materialize(ctx, validSpec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, validWorkspace.Path, "config", "user.name", "Valid Integration")
	gitOutput(t, validWorkspace.Path, "config", "user.email", "valid@example.invalid")
	gitOutput(t, validWorkspace.Path, "merge", "--no-ff", source.ConvenienceRef(), "-m", "integrate source")
	integrationHead := gitOutput(t, validWorkspace.Path, "rev-parse", "HEAD^{commit}")
	integrationCapture, err := manager.Capture(ctx, CaptureSpec{
		Workspace: validSpec, RunID: "integration-run", ExpectedHead: integrationHead,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: integrationCapture.TaskRef,
		ExpectedOldSHA: canonicalHead, TargetSHA: integrationHead,
	})
	if err != nil || advanced.ActualSHA != integrationHead {
		t.Fatalf("integration advance = %#v err=%v", advanced, err)
	}
	for _, ancestor := range []string{canonicalHead, sourceHead} {
		if ok, err := initializer.isAncestor(ctx, project.ControlRepoPath, true, ancestor, integrationHead); err != nil || !ok {
			t.Fatalf("integration lineage %s -> %s: ok=%t err=%v", ancestor, integrationHead, ok, err)
		}
	}
}
