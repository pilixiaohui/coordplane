package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/gitcapture"
	"coordplane/tests/testsupport"
)

var requireNoError = testsupport.RequireNoError

func TestGT01MaterializeCreatesExactPrivateClone(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-a", BaseSHA: initial}
	hookRoot := t.TempDir()
	sentinel := filepath.Join(hookRoot, "host-hook-ran")
	requireNoError(t, os.WriteFile(filepath.Join(hookRoot, "post-checkout"), []byte("#!/bin/sh\n: > "+sentinel+"\n"), 0o700))
	home := t.TempDir()
	requireNoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\thooksPath = "+hookRoot+"\n[credential]\n\thelper = host-only\n"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hookRoot)

	fact, err := manager.Materialize(ctx, spec)
	requireNoError(t, err)
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace preparation executed an ambient host hook: %v", err)
	}
	wantPath := filepath.Join(manager.root, project.ID, spec.TaskID)
	if fact.Path != wantPath || fact.HeadSHA != initial || fact.TaskBranch != "refs/heads/coordplane/task/task-a" {
		t.Fatalf("workspace fact = %+v, want path %s at %s", fact, wantPath, initial)
	}
	if got := gitOutput(t, fact.Path, "rev-parse", "HEAD^{commit}"); got != initial {
		t.Fatalf("workspace HEAD = %s, want base %s", got, initial)
	}
	if got := gitOutput(t, fact.Path, "symbolic-ref", "HEAD"); got != fact.TaskBranch {
		t.Fatalf("workspace branch = %s, want %s", got, fact.TaskBranch)
	}
	assertPrivateWorkspace(t, initializer, manager, spec, fact.Path)
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != initial {
		t.Fatalf("control canonical = %s after materialization, want %s", got, initial)
	}

	markerPath, err := manager.markerPath(spec.ProjectID, spec.TaskID)
	requireNoError(t, err)
	if strings.HasPrefix(markerPath, fact.Path+string(os.PathSeparator)) {
		t.Fatalf("ownership marker %s is inside mounted workspace %s", markerPath, fact.Path)
	}
	markerRaw, err := os.ReadFile(markerPath)
	requireNoError(t, err)
	if strings.Contains(string(markerRaw), project.ControlRepoPath) || strings.Contains(string(markerRaw), initializer.root) {
		t.Fatalf("workspace marker leaked control path: %s", markerRaw)
	}
	partialEntries, err := os.ReadDir(filepath.Join(manager.root, ".partial", project.ID))
	requireNoError(t, err)
	if len(partialEntries) != 0 {
		t.Fatalf("published workspace left partial entries: %v", partialEntries)
	}
}

func TestGT01TaskWorkspacesAreIndependentAndVerifyDoesNotReset(t *testing.T) {
	ctx := context.Background()
	_, manager, project, _, initial := newWorkspaceFixture(t)
	specA := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-a", BaseSHA: initial}
	specB := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-b", BaseSHA: initial}
	factA, err := manager.Materialize(ctx, specA)
	requireNoError(t, err)
	factB, err := manager.Materialize(ctx, specB)
	requireNoError(t, err)
	if factA.Path == factB.Path || factA.TaskBranch == factB.TaskBranch {
		t.Fatalf("task workspaces are not distinct: A=%+v B=%+v", factA, factB)
	}

	gitOutput(t, factA.Path, "config", "user.email", "task-a@coordplane.local")
	gitOutput(t, factA.Path, "config", "user.name", "Task A")
	aHead := commitFile(t, factA.Path, "a.txt", "task a\n", "task a commit")
	gitOutput(t, factA.Path, "update-ref", "refs/heads/main", aHead)
	if gitObjectExists(t, factB.Path, aHead) {
		t.Fatalf("task B unexpectedly sees task A commit %s", aHead)
	}
	if gitObjectExists(t, project.ControlRepoPath, aHead) {
		t.Fatalf("control repository unexpectedly sees task A commit %s", aHead)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != initial {
		t.Fatalf("task ref mutation changed canonical to %s", got)
	}

	dirtyPath := filepath.Join(factA.Path, "untracked.txt")
	requireNoError(t, os.WriteFile(dirtyPath, []byte("preserve me\n"), 0o600))
	before := workspaceSnapshot(t, factA.Path)
	verified, err := manager.Verify(ctx, specA)
	requireNoError(t, err)
	if verified.HeadSHA != aHead {
		t.Fatalf("verified HEAD = %s, want %s", verified.HeadSHA, aHead)
	}
	after := workspaceSnapshot(t, factA.Path)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Verify mutated reusable workspace\n before: %+v\n  after: %+v", before, after)
	}
	if _, err := manager.Materialize(ctx, specA); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Materialize error = %v, want existing workspace rejection", err)
	}
	if afterAgain := workspaceSnapshot(t, factA.Path); !reflect.DeepEqual(afterAgain, before) {
		t.Fatalf("rejected Materialize mutated workspace: %+v", afterAgain)
	}
}

func TestGT01VerifyRejectsIsolationTamperingWithoutRepairOrPathLeak(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-tamper", BaseSHA: initial}
	fact, err := manager.Materialize(ctx, spec)
	requireNoError(t, err)

	t.Run("control origin", func(t *testing.T) {
		gitOutput(t, fact.Path, "remote", "add", "origin", project.ControlRepoPath)
		before := workspaceSnapshot(t, fact.Path)
		_, err := manager.Verify(ctx, spec)
		assertWorkspaceRejection(t, err, "origin", initializer.root, manager.root, project.ControlRepoPath)
		if after := workspaceSnapshot(t, fact.Path); !reflect.DeepEqual(after, before) {
			t.Fatalf("Verify repaired tampered origin: %+v", after)
		}
		gitOutput(t, fact.Path, "remote", "remove", "origin")
	})

	t.Run("alternates", func(t *testing.T) {
		alternates := filepath.Join(fact.Path, ".git", "objects", "info", "alternates")
		requireNoError(t, os.WriteFile(alternates, []byte(filepath.Join(project.ControlRepoPath, "objects")+"\n"), 0o600))
		_, err := manager.Verify(ctx, spec)
		assertWorkspaceRejection(t, err, "alternates", initializer.root, manager.root, project.ControlRepoPath)
		requireNoError(t, os.Remove(alternates))
	})

	t.Run("workspace access", func(t *testing.T) {
		requireNoError(t, os.Chmod(fact.Path, 0o700))
		_, err := manager.Verify(ctx, spec)
		assertWorkspaceRejection(t, err, "group-rw", initializer.root, manager.root, project.ControlRepoPath)
		info, statErr := os.Stat(fact.Path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("Verify rewrote workspace mode to %v", info.Mode())
		}
		requireNoError(t, os.Chmod(fact.Path, 0o770|os.ModeSetgid))
	})

	t.Run("ownership marker", func(t *testing.T) {
		markerPath, err := manager.markerPath(spec.ProjectID, spec.TaskID)
		requireNoError(t, err)
		raw, err := os.ReadFile(markerPath)
		requireNoError(t, err)
		tampered := strings.Replace(string(raw), spec.TaskID, "other-task", 1)
		requireNoError(t, os.WriteFile(markerPath, []byte(tampered), 0o600))
		_, err = manager.Verify(ctx, spec)
		assertWorkspaceRejection(t, err, "marker", initializer.root, manager.root, project.ControlRepoPath)
	})
}

func TestGT01MaterializeImportsExactSourceThroughMovableConvenienceRef(t *testing.T) {
	ctx := context.Background()
	initializer, manager, project, sourceRepo, initial := newWorkspaceFixture(t)
	sourceHead := commitFile(t, sourceRepo, "source.txt", "source result\n", "source result")
	source := WorkspaceSource{
		TaskID:  "source-task",
		RunID:   "source-run",
		TaskRef: "refs/coordplane/tasks/source-task/runs/source-run",
		HeadSHA: sourceHead,
	}
	gitCommand(t,
		"-c", "protocol.file.allow=always",
		"--git-dir="+project.ControlRepoPath,
		"fetch", "--no-tags", "--no-write-fetch-head", sourceRepo,
		sourceHead+":"+source.TaskRef,
	)
	wrongSource := source
	wrongSource.HeadSHA = initial
	wrongSpec := WorkspaceSpec{
		ProjectID: project.ID, TaskID: "wrong-review-task", BaseSHA: initial, Source: &wrongSource,
	}
	if _, err := manager.Materialize(ctx, wrongSpec); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Materialize mismatched source error = %v", err)
	}
	wrongPath, err := manager.Path(wrongSpec.ProjectID, wrongSpec.TaskID)
	requireNoError(t, err)
	if _, err := os.Lstat(wrongPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched source published a workspace: %v", err)
	}
	spec := WorkspaceSpec{
		ProjectID: project.ID,
		TaskID:    "review-task",
		BaseSHA:   initial,
		Source:    &source,
	}

	fact, err := manager.Materialize(ctx, spec)
	requireNoError(t, err)
	if fact.SourceRef != "refs/heads/coordplane/source/source-task" {
		t.Fatalf("source convenience ref = %s", fact.SourceRef)
	}
	if got := gitOutput(t, fact.Path, "rev-parse", fact.SourceRef+"^{commit}"); got != sourceHead {
		t.Fatalf("source convenience ref = %s, want %s", got, sourceHead)
	}
	if got := gitOutput(t, fact.Path, "remote"); got != "" {
		t.Fatalf("source import persisted remote %q", got)
	}
	assertPrivateWorkspace(t, initializer, manager, spec, fact.Path)
	gitDirOutput(t, project.ControlRepoPath, "update-ref", project.CanonicalRef, sourceHead, initial)
	canonicalRef, err := manager.RefreshCanonical(ctx, spec, project.ControlRepoPath, project.CanonicalRef, sourceHead)
	requireNoError(t, err)
	if got := gitOutput(t, fact.Path, "rev-parse", canonicalRef+"^{commit}"); got != sourceHead {
		t.Fatalf("canonical convenience ref = %s, want %s", got, sourceHead)
	}
	if got := gitOutput(t, fact.Path, "remote"); got != "" {
		t.Fatalf("canonical refresh persisted remote %q", got)
	}

	gitOutput(t, fact.Path, "update-ref", fact.SourceRef, initial, sourceHead)
	if _, err := manager.Verify(ctx, spec); err != nil {
		t.Fatalf("Verify rejected movable convenience ref: %v", err)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", source.TaskRef+"^{commit}"); got != sourceHead {
		t.Fatalf("moving convenience ref changed control source to %s", got)
	}
}

func TestGT01VerifyNeverCreatesMissingWorkspace(t *testing.T) {
	ctx := context.Background()
	_, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "missing-task", BaseSHA: initial}
	want, err := manager.Path(spec.ProjectID, spec.TaskID)
	requireNoError(t, err)
	if _, err := manager.Verify(ctx, spec); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Verify missing workspace error = %v", err)
	}
	if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Verify created missing workspace: %v", err)
	}
}

func TestProjectMaintenanceLockIsPerProjectAndHonorsCancellation(t *testing.T) {
	var locks projectLocks
	unlock, err := locks.lock(context.Background(), "project-a")
	requireNoError(t, err)
	defer unlock()

	unlockOther, err := locks.lock(context.Background(), "project-b")
	requireNoError(t, err)
	unlockOther()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.lock(ctx, "project-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("contended project lock error = %v, want context cancellation", err)
	}
}

func newWorkspaceFixture(t *testing.T) (*Initializer, *WorkspaceManager, Project, string, string) {
	t.Helper()
	ctx, sourceRepo, initial, initializer, preflight := preflightFixture(t)
	project := testProject(t, initializer, preflight, "project-workspace", "operation-workspace")
	_, err := initializer.Initialize(ctx, project)
	requireNoError(t, err)
	manager, err := NewWorkspaceManager(initializer, filepath.Join(t.TempDir(), "workspaces"), testCaptureHelper{root: t.TempDir()})
	requireNoError(t, err)
	return initializer, manager, project, sourceRepo, initial
}

type testCaptureHelper struct{ root string }

func (h testCaptureHelper) Capture(ctx context.Context, request CaptureHelperRequest) (CaptureHelperFact, error) {
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

func (h testCaptureHelper) Inspect(ctx context.Context, request WorkspaceInspectRequest) (WorkspaceInspectFact, error) {
	return inspectWithGitCapture(ctx, h.root, request)
}

func (h testCaptureHelper) Cleanup(_ context.Context, request CaptureHelperRequest) error {
	return os.RemoveAll(filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID))
}

func inspectWithGitCapture(ctx context.Context, root string, request WorkspaceInspectRequest) (WorkspaceInspectFact, error) {
	parent := filepath.Join(root, request.ProjectID, request.TaskID)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return WorkspaceInspectFact{}, err
	}
	handoff, err := os.MkdirTemp(parent, "inspect-")
	if err != nil {
		return WorkspaceInspectFact{}, err
	}
	defer os.RemoveAll(handoff)
	fact, err := gitcapture.Inspect(ctx, gitcapture.InspectRequest{
		Workspace: request.Workspace, Handoff: handoff, MaximumObjects: 250_000,
	})
	return WorkspaceInspectFact{
		HeadSHA: fact.HeadSHA, StatusDigest: fact.StatusDigest, ObjectCount: fact.ObjectCount,
		Clean: fact.Clean, Unfinished: fact.Unfinished,
	}, err
}

func assertPrivateWorkspace(t *testing.T, initializer *Initializer, manager *WorkspaceManager, spec WorkspaceSpec, path string) {
	t.Helper()
	gitDir := filepath.Join(path, ".git")
	requireNoError(t, validateDirectDirectory(gitDir, "workspace Git directory"))
	common := gitOutput(t, path, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	resolvedCommon, err := filepath.EvalSymlinks(common)
	requireNoError(t, err)
	if resolvedCommon != gitDir {
		t.Fatalf("common Git directory = %s, want private %s", resolvedCommon, gitDir)
	}
	for _, forbidden := range []string{
		filepath.Join(gitDir, "commondir"),
		filepath.Join(gitDir, "objects", "info", "alternates"),
	} {
		if _, err := os.Lstat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forbidden shared Git metadata exists at %s: %v", forbidden, err)
		}
	}
	if got := gitOutput(t, path, "remote"); got != "" {
		t.Fatalf("workspace remotes = %q, want none", got)
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	requireNoError(t, err)
	if bytes.Contains(config, []byte("credential")) || bytes.Contains(config, []byte("hooksPath")) {
		t.Fatalf("workspace config inherited host Git behavior: %s", config)
	}
	for _, hostPath := range []string{initializer.root, filepath.Join(initializer.root, spec.ProjectID+".git")} {
		if strings.Contains(string(config), hostPath) {
			t.Fatalf("workspace config leaked host path %s", hostPath)
		}
	}
	if err := filepath.WalkDir(gitDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, hostRoot := range []string{initializer.root, manager.root} {
			if bytes.Contains(raw, []byte(hostRoot)) {
				t.Fatalf("workspace Git metadata %s leaked host root %s", path, hostRoot)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan workspace Git metadata: %v", err)
	}
	if err := validateWorkspaceAccess(path); err != nil {
		t.Fatalf("published workspace access: %v", err)
	}
	rootInfo, err := os.Stat(path)
	requireNoError(t, err)
	if rootInfo.Mode().Perm() != 0o770 || rootInfo.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("workspace root mode = %v, want group-rw setgid without world access", rootInfo.Mode())
	}
	if got := gitOutput(t, path, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("new workspace is dirty: %q", got)
	}
	markerPath, err := manager.markerPath(spec.ProjectID, spec.TaskID)
	requireNoError(t, err)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("workspace ownership marker: %v", err)
	}
}

func assertWorkspaceRejection(t *testing.T, err error, want string, forbiddenPaths ...string) {
	t.Helper()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
		t.Fatalf("workspace error = %v, want containing %q", err, want)
	}
	for _, forbidden := range forbiddenPaths {
		if forbidden != "" && strings.Contains(err.Error(), forbidden) {
			t.Fatalf("workspace error leaked host path %s: %v", forbidden, err)
		}
	}
}

type workspaceState struct {
	Head   string
	Branch string
	Status string
	Config []byte
}

func workspaceSnapshot(t *testing.T, path string) workspaceState {
	t.Helper()
	config, err := os.ReadFile(filepath.Join(path, ".git", "config"))
	requireNoError(t, err)
	return workspaceState{
		Head:   gitOutput(t, path, "rev-parse", "HEAD^{commit}"),
		Branch: gitOutput(t, path, "symbolic-ref", "HEAD"),
		Status: gitOutput(t, path, "status", "--porcelain=v1", "--untracked-files=all"),
		Config: config,
	}
}

func gitObjectExists(t *testing.T, repoPath, sha string) bool {
	t.Helper()
	args := []string{"-C", repoPath, "cat-file", "-e", sha + "^{commit}"}
	if strings.HasSuffix(repoPath, ".git") {
		args = []string{"--git-dir=" + repoPath, "cat-file", "-e", sha + "^{commit}"}
	}
	cmd := exec.Command("git", args...)
	cmd.Env = gitEnvironment()
	return cmd.Run() == nil
}

func TestGT06ExpandHeadResolvesShortPrefixInWorkspace(t *testing.T) {
	ctx := context.Background()
	_, manager, project, _, initial := newWorkspaceFixture(t)
	spec := WorkspaceSpec{ProjectID: project.ID, TaskID: "task-expand", BaseSHA: initial}
	fact, err := manager.Materialize(ctx, spec)
	requireNoError(t, err)

	gitOutput(t, fact.Path, "config", "user.email", "expand@coordplane.local")
	gitOutput(t, fact.Path, "config", "user.name", "Expand")
	head := commitFile(t, fact.Path, "result.txt", "done\n", "task result")
	if len(head) != 40 {
		t.Fatalf("commitFile returned %q", head)
	}

	full, err := manager.ExpandHead(ctx, spec, fact.Path, head[:8])
	requireNoError(t, err)
	if full != head {
		t.Fatalf("ExpandHead(%.8s) = %q, want %q", head, full, head)
	}

	for _, bad := range []struct {
		ref  string
		want string
	}{
		{ref: "", want: "expected head is empty"},
		{ref: "zzzzzz", want: "does not resolve to a commit"},
	} {
		if _, err := manager.ExpandHead(ctx, spec, fact.Path, bad.ref); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("ExpandHead(%q) error = %v, want containing %q", bad.ref, err, bad.want)
		}
	}
}
