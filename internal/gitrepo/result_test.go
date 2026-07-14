package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGT02CaptureUsesActualCleanWorkspaceHeadAndCreatesImmutableTaskRef(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-capture", BaseSHA: initial}
	workspace, err := manager.Materialize(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, workspace.Path, "config", "user.name", "Capture Test")
	gitOutput(t, workspace.Path, "config", "user.email", "capture@example.invalid")
	head := commitFile(t, workspace.Path, "result.txt", "captured\n", "captured result")

	fact, err := manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-capture", ExpectedHead: head,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRef := "refs/coordplane/tasks/task-capture/runs/run-capture"
	if fact.HeadSHA != head || fact.TaskRef != wantRef {
		t.Fatalf("capture fact = %#v, want head %s ref %s", fact, head, wantRef)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", wantRef+"^{commit}"); got != head {
		t.Fatalf("task ref = %s, want %s", got, head)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != initial {
		t.Fatalf("capture moved canonical to %s, want %s", got, initial)
	}

	replay, err := manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-capture", ExpectedHead: head,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil || replay != fact {
		t.Fatalf("capture replay = %#v, %v; want %#v", replay, err, fact)
	}
	if _, err := manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-capture", ExpectedHead: initial,
		ControlRepoPath: project.ControlRepoPath,
	}); err == nil || !strings.Contains(err.Error(), "expected head") {
		t.Fatalf("mismatched replay error = %v, want expected-head rejection", err)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", wantRef+"^{commit}"); got != head {
		t.Fatalf("mismatched replay changed task ref to %s, want %s", got, head)
	}
	destination := filepath.Join(t.TempDir(), "review")
	checkout, err := initializer.Checkout(ctx, CheckoutSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		TaskRef: wantRef, ExpectedHead: head, Destination: destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkout.HeadSHA != head || checkout.Destination != destination {
		t.Fatalf("checkout fact = %#v", checkout)
	}
	if got := gitOutput(t, destination, "rev-parse", "HEAD^{commit}"); got != head {
		t.Fatalf("checkout HEAD = %s, want %s", got, head)
	}
	if got := gitOutput(t, destination, "remote"); got != "" {
		t.Fatalf("checkout remotes = %q, want none", got)
	}
}

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

func TestGT04AdvanceUsesExpectedOldCASAndClassifiesNonFastForwardAsStale(t *testing.T) {
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
			Workspace: spec, RunID: runID, ExpectedHead: head,
			ControlRepoPath: project.ControlRepoPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		return fact
	}

	first := capture("task-a", "run-a", "a.txt")
	second := capture("task-b", "run-b", "b.txt")
	advanced, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: first.TaskRef,
		ExpectedOldSHA: initial, TargetSHA: first.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Outcome != AdvanceUpdated || advanced.ActualSHA != first.HeadSHA {
		t.Fatalf("first advance = %#v", advanced)
	}
	recovered, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: first.TaskRef,
		ExpectedOldSHA: initial, TargetSHA: first.HeadSHA,
	})
	if err != nil || recovered.Outcome != AdvanceIncluded || recovered.ActualSHA != first.HeadSHA {
		t.Fatalf("post-CAS replay = %#v err=%v", recovered, err)
	}

	stale, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: second.TaskRef,
		ExpectedOldSHA: initial, TargetSHA: second.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Outcome != AdvanceStale || stale.ActualSHA != first.HeadSHA {
		t.Fatalf("stale advance = %#v", stale)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != first.HeadSHA {
		t.Fatalf("stale advance overwrote canonical with %s, want %s", got, first.HeadSHA)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", second.TaskRef+"^{commit}"); got != second.HeadSHA {
		t.Fatalf("stale result lost task ref: got %s want %s", got, second.HeadSHA)
	}
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

func TestGT07SourceUseAndExpectedOldTaskRefGCShareProjectMaintenanceLock(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-gc", BaseSHA: initial}
	workspace, err := manager.Materialize(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, workspace.Path, "config", "user.name", "GC Test")
	gitOutput(t, workspace.Path, "config", "user.email", "gc@example.invalid")
	head := commitFile(t, workspace.Path, "gc.txt", "reachable\n", "gc result")
	captured, err := manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-gc", ExpectedHead: head,
		ControlRepoPath: project.ControlRepoPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initializer.Advance(ctx, AdvanceSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: captured.TaskRef,
		ExpectedOldSHA: initial, TargetSHA: head,
	}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	useDone := make(chan error, 1)
	var referenced atomic.Bool
	go func() {
		useDone <- initializer.UseTaskRef(ctx, project.ID, project.ControlRepoPath, captured.TaskRef, head, func(string) error {
			referenced.Store(true)
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	authorized := make(chan struct{})
	deleteDone := make(chan struct {
		deleted bool
		err     error
	}, 1)
	go func() {
		deleted, err := initializer.DeleteTaskRef(ctx, DeleteTaskRefSpec{
			ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
			CanonicalRef: project.CanonicalRef, TaskRef: captured.TaskRef, ExpectedHead: head,
		}, func() (bool, error) {
			close(authorized)
			return !referenced.Load(), nil
		})
		deleteDone <- struct {
			deleted bool
			err     error
		}{deleted, err}
	}()
	select {
	case <-authorized:
		t.Fatal("GC predicate ran while source ref use held the maintenance lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-useDone; err != nil {
		t.Fatal(err)
	}
	result := <-deleteDone
	if result.err != nil || result.deleted {
		t.Fatalf("GC with durable source reference deleted=%t err=%v", result.deleted, result.err)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", captured.TaskRef+"^{commit}"); got != head {
		t.Fatalf("blocked GC changed task ref to %s", got)
	}

	referenced.Store(false)
	deleted, err := initializer.DeleteTaskRef(ctx, DeleteTaskRefSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: captured.TaskRef, ExpectedHead: head,
	}, func() (bool, error) { return true, nil })
	if err != nil || !deleted {
		t.Fatalf("eligible GC deleted=%t err=%v", deleted, err)
	}
	if _, exists, err := initializer.resolveRef(ctx, project.ControlRepoPath, captured.TaskRef); err != nil || exists {
		t.Fatalf("task ref after GC exists=%t err=%v", exists, err)
	}
	deleted, err = initializer.DeleteTaskRef(ctx, DeleteTaskRefSpec{
		ProjectID: project.ID, ControlRepoPath: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: captured.TaskRef, ExpectedHead: head,
	}, func() (bool, error) { return true, nil })
	if err != nil || !deleted {
		t.Fatalf("absent ref replay deleted=%t err=%v", deleted, err)
	}
	if err := initializer.Prune(ctx, project.ID, project.ControlRepoPath); err != nil {
		t.Fatal(err)
	}
	gitDirOutput(t, project.ControlRepoPath, "fsck", "--full", "--strict")
}

func TestGT07WorkspaceGCRequiresCleanExactHeadAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-workspace-gc", BaseSHA: initial}
	workspace, err := manager.Materialize(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, workspace.Path, "config", "user.name", "Workspace GC")
	gitOutput(t, workspace.Path, "config", "user.email", "workspace-gc@example.invalid")
	head := commitFile(t, workspace.Path, "result.txt", "kept\n", "captured")
	if _, err := manager.Capture(ctx, CaptureSpec{
		Workspace: spec, RunID: "run-workspace-gc", ExpectedHead: head,
		ControlRepoPath: project.ControlRepoPath,
	}); err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(workspace.Path, "dirty.txt")
	if err := os.WriteFile(dirtyPath, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.Delete(ctx, spec, head, func() (bool, error) { return true, nil })
	if err != nil || deleted {
		t.Fatalf("dirty workspace GC deleted=%t err=%v", deleted, err)
	}
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatalf("dirty workspace was removed: %v", err)
	}
	if err := os.Remove(dirtyPath); err != nil {
		t.Fatal(err)
	}
	deleted, err = manager.Delete(ctx, spec, head, func() (bool, error) { return true, nil })
	if err != nil || !deleted {
		t.Fatalf("clean workspace GC deleted=%t err=%v", deleted, err)
	}
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace after GC: %v", err)
	}
	deleted, err = manager.Delete(ctx, spec, head, func() (bool, error) { return true, nil })
	if err != nil || !deleted {
		t.Fatalf("workspace GC replay deleted=%t err=%v", deleted, err)
	}
}
