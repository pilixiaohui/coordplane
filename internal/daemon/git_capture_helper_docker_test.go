//go:build docker

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/config"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
)

func TestGT03DockerCaptureHelperIsolatesConfigAndPublishesBoundedReadyHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for GT-03: %v", err)
	}
	root := t.TempDir()
	helperBinary := filepath.Join(root, "coordplane-git-helper")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", helperBinary, "./cmd/coordplane-git-helper")
	build.Dir = daemonRepositoryRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, raw)
	}
	image := dockerGitHelperImage(t, ctx, root)
	workspace, head := dockerCaptureRepository(t, root)
	handoffRoot := filepath.Join(root, "handoff")
	helper, err := newDockerCaptureHelper(executor, config.GitConfig{
		CaptureHelperImage: image, CaptureTimeout: time.Minute,
		MaximumBundleBytes: 8 << 20, MaximumObjects: 100, MaximumHandoffBytes: 16 << 20,
	}, handoffRoot, helperBinary)
	if err != nil {
		t.Fatal(err)
	}
	request := gitrepo.CaptureHelperRequest{
		ProjectID: "project-docker-capture", TaskID: "task-docker-capture", RunID: "run-docker-capture",
		Workspace: workspace, ExpectedHead: head, BaseSHA: head,
	}
	fact, err := helper.Capture(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if fact.HeadSHA != head || fact.BundleBytes <= 0 || fact.ObjectCount <= 0 || !strings.HasSuffix(fact.ReadyBundle, "capture.ready/result.bundle") {
		t.Fatalf("Docker capture fact = %#v", fact)
	}
	if _, err := os.Stat(filepath.Join(helper.handoffPath(request), "host-command-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository-local fsmonitor escaped the trusted helper: %v", err)
	}
	if _, err := executor.Inspect(ctx, captureRuntimeRef(request)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("capture helper container survived completion: %v", err)
	}
	if err := helper.Cleanup(ctx, request); err != nil {
		t.Fatal(err)
	}

	limitedRequest := request
	limitedRequest.RunID = "run-object-limit"
	helper.config.MaximumObjects = 1
	if _, err := helper.Capture(ctx, limitedRequest); err == nil || !strings.Contains(err.Error(), "object limit") {
		t.Fatalf("object quota error = %v", err)
	}
	ready := filepath.Join(helper.handoffPath(limitedRequest), "capture.ready")
	if _, err := os.Stat(ready); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quota failure published ready handoff: %v", err)
	}
	helper.config.MaximumObjects = 100
	helper.config.MaximumHandoffBytes = helper.config.MaximumBundleBytes
	if err := os.WriteFile(filepath.Join(handoffRoot, "quota-reservation"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	totalRequest := request
	totalRequest.RunID = "run-total-quota"
	if _, err := helper.Capture(ctx, totalRequest); err == nil || !strings.Contains(err.Error(), "total handoff quota") {
		t.Fatalf("total handoff quota error = %v", err)
	}
}

func TestGT07DockerGCPreviewAndDiscardNeverExecuteWorkspaceGitConfigOnHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for GT-07: %v", err)
	}
	root := t.TempDir()
	helperBinary := filepath.Join(root, "coordplane-git-helper")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", helperBinary, "./cmd/coordplane-git-helper")
	build.Dir = daemonRepositoryRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, raw)
	}
	image := dockerGitHelperImage(t, ctx, root)
	helper, err := newDockerCaptureHelper(executor, config.GitConfig{
		CaptureHelperImage: image, CaptureTimeout: time.Minute,
		MaximumBundleBytes: 8 << 20, MaximumObjects: 100, MaximumHandoffBytes: 16 << 20,
	}, filepath.Join(root, "handoff"), helperBinary)
	if err != nil {
		t.Fatal(err)
	}

	source, _ := dockerCaptureRepository(t, root)
	sourceRef := dockerGit(t, source, "symbolic-ref", "HEAD")
	initializer, err := gitrepo.New(filepath.Join(root, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := initializer.Preflight(ctx, source, sourceRef)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := initializer.Paths("project-docker-gc", "initialize-docker-gc")
	if err != nil {
		t.Fatal(err)
	}
	project := gitrepo.Project{
		ID: "project-docker-gc", OperationID: "initialize-docker-gc",
		SourcePath: preflight.SourcePath, SourceRef: preflight.SourceRef, InitialSHA: preflight.InitialSHA,
		ControlRepoPath: paths.Final, CanonicalRef: preflight.SourceRef,
	}
	if _, err := initializer.Initialize(ctx, project); err != nil {
		t.Fatal(err)
	}
	manager, err := gitrepo.NewWorkspaceManager(initializer, filepath.Join(root, "workspaces"), helper)
	if err != nil {
		t.Fatal(err)
	}
	spec := gitrepo.WorkspaceSpec{ProjectID: project.ID, TaskID: "task-docker-gc", BaseSHA: preflight.InitialSHA}
	workspace, err := manager.Materialize(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "host-fsmonitor-ran")
	command := filepath.Join(workspace.Path, ".git", "host-fsmonitor")
	if err := os.WriteFile(command, []byte(fmt.Sprintf("#!/bin/sh\n: > %q\n", marker)), 0o770); err != nil {
		t.Fatal(err)
	}
	dockerGit(t, workspace.Path, "config", "core.fsmonitor", command)

	preview, err := manager.State(ctx, spec, workspace.HeadSHA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Exists || !preview.Clean || preview.HeadSHA != workspace.HeadSHA || preview.Fingerprint == "" {
		t.Fatalf("GC preview workspace fact = %#v", preview)
	}
	assertHostFSMonitorDidNotRun(t, marker)
	if err := os.WriteFile(filepath.Join(workspace.Path, "dirty.txt"), []byte("dirty\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	if discarded, err := manager.Discard(ctx, spec, workspace.HeadSHA, 1, preview.Fingerprint, func() (bool, error) {
		return true, nil
	}); err == nil || discarded || !strings.Contains(err.Error(), "fingerprint changed") {
		t.Fatalf("stale GC discard discarded=%t err=%v", discarded, err)
	}
	assertHostFSMonitorDidNotRun(t, marker)
	dirty, err := manager.State(ctx, spec, workspace.HeadSHA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Clean || dirty.Fingerprint == preview.Fingerprint {
		t.Fatalf("dirty GC preview fact = %#v, clean = %#v", dirty, preview)
	}
	if discarded, err := manager.Discard(ctx, spec, workspace.HeadSHA, 1, dirty.Fingerprint, func() (bool, error) {
		return true, nil
	}); err != nil || !discarded {
		t.Fatalf("explicit dirty GC discard discarded=%t err=%v", discarded, err)
	}
	assertHostFSMonitorDidNotRun(t, marker)
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded workspace still exists: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(helper.root, project.ID, spec.TaskID))
	if err != nil || len(entries) != 0 {
		t.Fatalf("successful inspect handoff residue = %v, err=%v", entries, err)
	}
}

func assertHostFSMonitorDidNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository-local fsmonitor executed on the host: %v", err)
	}
}

func dockerGitHelperImage(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	for _, candidate := range []string{
		strings.TrimSpace(os.Getenv("COORDPLANE_TEST_GIT_HELPER_IMAGE")),
		"golang:1.23",
		"orchestrator-v4-agent:latest",
	} {
		if candidate == "" {
			continue
		}
		if exec.CommandContext(ctx, "docker", "image", "inspect", candidate).Run() == nil {
			return candidate
		}
	}
	image := "coordplane-git-helper-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	dockerConfig := filepath.Join(root, "docker-config")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerBuild := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image,
		filepath.Join(daemonRepositoryRoot(t), "internal", "daemon", "testdata", "git-capture-helper"))
	dockerBuild.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	if raw, err := dockerBuild.CombinedOutput(); err != nil {
		t.Fatalf("build helper image: %v\n%s", err, raw)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		remove := exec.CommandContext(cleanup, "docker", "image", "rm", "-f", image)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		_ = remove.Run()
	})
	return image
}

func dockerCaptureRepository(t *testing.T, root string) (string, string) {
	t.Helper()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o770); err != nil {
		t.Fatal(err)
	}
	dockerGit(t, workspace, "init", "-q")
	dockerGit(t, workspace, "config", "user.name", "Docker Capture")
	dockerGit(t, workspace, "config", "user.email", "capture@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("captured\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	dockerGit(t, workspace, "add", "result.txt")
	dockerGit(t, workspace, "commit", "-q", "-m", "capture")
	command := filepath.Join(workspace, ".git", "malicious-fsmonitor")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n: > /handoff/host-command-ran\n"), 0o770); err != nil {
		t.Fatal(err)
	}
	dockerGit(t, workspace, "config", "core.fsmonitor", "/workspace/.git/malicious-fsmonitor")
	if err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o770)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o660)
		if info.Mode().Perm()&0o100 != 0 {
			mode = 0o770
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	return workspace, dockerGit(t, workspace, "rev-parse", "HEAD^{commit}")
}

func dockerGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, raw)
	}
	return strings.TrimSpace(string(raw))
}
