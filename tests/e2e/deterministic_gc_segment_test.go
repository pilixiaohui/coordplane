//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

// TestDeterministicSevenWorkspaceGCSegment is the offline (no provider) gate
// for the RMA-02 GC segment Phase B race (COD-64): at 7-workspace scale the
// retention-0 auto-GC deletion window flaps the daemon degraded via orphan
// detection of its own inspect helper containers, and the segment used to hit
// that window with exit 1 (live #11). This test replays the Phase B shape —
// 7 closed tasks/workspaces/refs, retention rewrite 0/0, controlled restart,
// refs-disappearance poll, managed-container settle wait, afterGC preview and
// gc run — and asserts the daemon observes zero degraded windows once the
// settle wait has drained, with CLI stderr + daemon log evidence persisted
// outside t.TempDir on any GC-segment CLI failure.
func TestDeterministicSevenWorkspaceGCSegment(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	if image == "" {
		t.Fatal("E2E_RUNTIME_IMAGE is required")
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e-gc-segment", 8*time.Minute)
	requireNoError(t, err)
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release Docker test resource: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	root := t.TempDir()
	source, initialSHA := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	testsupport.WriteFile(t, instructions, []byte("Execute only the deterministic GC-segment fixture contract.\n"), 0o600)
	// 场景期 retention 非零:自动 GC(2s 周期)在 7 个 Task 逐个 completed 期间不得
	// 删除任何资源;GC 段由下方重写为 0/0 并受控重启(与 RMA-02 GC 段一致)。
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "24h", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image, Tail: "  run_timeout: 3m\n  shutdown_grace: 3s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "GC segment initial daemon startup")

	agent := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--display-name", "GC segment Agent", "--adapter", "claude",
		"--image", image, "--instructions-file", instructions, "--request-id", "gc7-agent", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "GC segment seven workspaces", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agent.ID, "--request-id", "gc7-project", "--output", "json")
	if project.InitialSHA != initialSHA || project.CanonicalSHA != initialSHA {
		t.Fatalf("GC segment Project did not register C0: %#v", project)
	}
	runJSON[core.Agent](t, ctx, coordplane,
		"agent", "pause", agent.ID, "--socket", socket, "--request-id", "gc7-pause", "--output", "json")

	// 逐轮 create→run→accept:每轮 Task 的 base 是上一轮 accept 后的 canonical,
	// head 恒为 canonical 后代,accept 走 direct CAS(无 integration Task),恰好
	// 7 个 completed Task / 7 workspaces / 7 refs。
	var tasks []core.Task
	for index := 1; index <= 7; index++ {
		name := fmt.Sprintf("gc%d", index)
		created := runJSON[core.Task](t, ctx, coordplane,
			"task", "create", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
			"--title", "GC segment source "+name, "--description", "gc_only=1;gc_name="+name,
			"--request-id", "gc7-task-"+name, "--output", "json")
		runJSON[core.Agent](t, ctx, coordplane,
			"agent", "resume", agent.ID, "--socket", socket, "--request-id", "gc7-resume-"+name, "--output", "json")
		submitted := waitForTask(t, ctx, coordplane, socket, created.ID, "GC segment source "+name+" submission", func(task core.Task) bool {
			return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.HeadRunID != "" && task.TaskRef != ""
		})
		if submitted.BaseSHA != projectDetail(t, ctx, coordplane, socket, project.ID).ActualCanonicalSHA {
			t.Fatalf("GC segment source %s base = %s, want canonical %s", name, submitted.BaseSHA, projectDetail(t, ctx, coordplane, socket, project.ID).ActualCanonicalSHA)
		}
		runJSON[core.Task](t, ctx, coordplane,
			"task", "accept", created.ID, "--socket", socket, "--integration-agent", agent.ID,
			"--request-id", "gc7-accept-"+name, "--output", "json")
		completed := waitForTask(t, ctx, coordplane, socket, created.ID, "GC segment source "+name+" direct CAS", func(task core.Task) bool {
			return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
		})
		tasks = append(tasks, completed)
	}
	if len(tasks) != 7 {
		t.Fatalf("GC segment completed Tasks = %d, want 7", len(tasks))
	}
	controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
	finalSHA := gitDir(t, ctx, controlRepo, "rev-parse", project.CanonicalRef)
	workspaceCount := len(listProjectWorkspaceDirs(t, dataDir, project.ID))
	if workspaceCount != 7 {
		t.Fatalf("GC segment workspace dirs = %d, want 7", workspaceCount)
	}
	for _, task := range tasks {
		if got := gitDir(t, ctx, controlRepo, "rev-parse", task.TaskRef); got != task.HeadSHA {
			t.Fatalf("task ref %s = %s, want %s", task.TaskRef, got, task.HeadSHA)
		}
	}
	gitDirSucceeds(t, ctx, controlRepo, "fsck", "--full", "--strict")

	// 阶段 A(场景期 24h 配置,preview 前移):preview 断言 7 workspaces + 7 refs
	// 全部存在;全部 Task closed 不足 24h,自动 GC 无候选,preview 与自动 GC 之间
	// 不存在竞态。
	preview := runGCJSON[core.GCPreview](t, ctx, coordplane, daemon.logPath, "gc", "preview", "--socket", socket, "--output", "json")
	if len(preview.Workspaces) != 7 || len(preview.TaskRefs) != 7 {
		t.Fatalf("GC segment preview cardinality = workspaces:%d refs:%d", len(preview.Workspaces), len(preview.TaskRefs))
	}
	for _, target := range preview.Workspaces {
		if !target.Exists {
			t.Fatalf("GC segment preview workspace %s was deleted during the scenario: %#v", target.TaskID, target)
		}
	}
	for _, target := range preview.TaskRefs {
		if !target.Exists {
			t.Fatalf("GC segment preview task ref %s was deleted during the scenario: %#v", target.TaskID, target)
		}
	}

	// 阶段 B(retention 0/0 + 受控重启):自动 GC 首个 tick 先删 7 workspaces(每项
	// 一个 inspect helper 容器,~2.6s 串行)再删 7 refs;refs 消失轮询通过后 settle
	// 等待 managed 容器清空,排空 degraded flapping 窗口,再执行 afterGC preview /
	// gc run。readyStable 探针断言 settle 之后零 degraded 观察。
	gcRaw, err := os.ReadFile(configPath)
	requireNoError(t, err)
	gcRaw = bytes.ReplaceAll(gcRaw, []byte("completed_workspace: 24h"), []byte("completed_workspace: 0"))
	gcRaw = bytes.ReplaceAll(gcRaw, []byte("terminal_task_ref: 24h"), []byte("terminal_task_ref: 0"))
	gcConfigPath := testsupport.WriteFile(t, filepath.Join(filepath.Dir(configPath), "gc-segment-gc.yaml"), gcRaw, 0o600)
	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop GC segment daemon before retention restart: %v", err)
	}
	daemon = startDaemon(t, coordplane, gcConfigPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "GC segment retention-0 restart")
	eventually(t, ctx, 30*time.Second, "GC segment retention-0 auto-GC deleted all 7 task refs", func() (bool, bool, string) {
		refs := strings.Fields(gitDir(t, ctx, controlRepo, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/"))
		return len(refs) == 0, len(refs) == 0, fmt.Sprintf("remaining task refs: %d", len(refs))
	})
	waitForManagedContainersDrained(t, ctx, daemon.logPath)
	waitForReady(t, ctx, coordplane, socket, "GC segment daemon ready after retention-0 auto-GC")
	readyStable := watchDaemonReadyStable(t, ctx, coordplane, socket)
	afterGC := runGCJSON[core.GCPreview](t, ctx, coordplane, daemon.logPath, "gc", "preview", "--socket", socket, "--output", "json")
	if len(afterGC.Workspaces) != 7 || len(afterGC.TaskRefs) != 7 {
		t.Fatalf("GC segment preview cardinality after retention-0 restart = workspaces:%d refs:%d", len(afterGC.Workspaces), len(afterGC.TaskRefs))
	}
	for _, target := range afterGC.Workspaces {
		if target.Exists {
			t.Fatalf("GC segment preview workspace %s survived retention-0 auto-GC: %#v", target.TaskID, target)
		}
	}
	for _, target := range afterGC.TaskRefs {
		if target.Exists {
			t.Fatalf("GC segment preview task ref %s survived retention-0 auto-GC: %#v", target.TaskID, target)
		}
	}
	if result := runGCJSON[core.GCRunResult](t, ctx, coordplane, daemon.logPath, "gc", "run", "--socket", socket,
		"--confirm", "--request-id", "gc7-gc", "--output", "json"); !result.Completed {
		t.Fatalf("GC segment GC result = %#v", result)
	}
	readyStable()

	waitForWorkspacesRemoved(t, ctx, dataDir, project.ID, taskIDs(tasks)...)
	if refs := strings.Fields(gitDir(t, ctx, controlRepo, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/")); len(refs) != 0 {
		t.Fatalf("GC segment task refs survived gc run: %v", refs)
	}
	waitForNoProjectContainers(t, ctx, project.ID, daemon.logPath)
	if got := gitDir(t, ctx, controlRepo, "rev-parse", project.CanonicalRef); got != finalSHA {
		t.Fatalf("GC segment canonical drifted during GC: got %s want %s", got, finalSHA)
	}
	t.Logf("PASS project=%s canonical=%s workspaces=7 refs=7 settled=true", project.ID, finalSHA)
}

func taskIDs(tasks []core.Task) []string {
	result := make([]string, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, task.ID)
	}
	return result
}

// listProjectWorkspaceDirs returns the immediate workspace directory names for
// a project, used to assert the scenario kept exactly one workspace per task.
func listProjectWorkspaceDirs(t *testing.T, dataDir, projectID string) []string {
	t.Helper()
	root := filepath.Join(dataDir, "workspaces", projectID)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	requireNoError(t, err)
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}
