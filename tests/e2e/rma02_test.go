//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

type rma02Source struct {
	role, marker string
	agent        core.Agent
	task         core.Task
	run          core.Run
	taskCurrent  core.CurrentTaskResult
	fixture      string
	fixtureExit  int
}

func runRMA02(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	requireExecutable(t, "E2E_COORDLINK_BIN")
	reportPath := strings.TrimSpace(os.Getenv("E2E_RMA02_REPORT"))
	if !filepath.IsAbs(reportPath) {
		t.Fatal("E2E_RMA02_REPORT must be an absolute path")
	}
	image, network, providerEnv := liveRuntimeConfig(t)
	releaseLiveDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	started := time.Now().UTC()
	repositoryRoot := testsupport.RepositoryRoot()
	if os.Getenv("E2E_SOURCE_CLEAN") != "1" {
		t.Fatal("RMA-02 requires clean source admission before build")
	}
	revision := git(t, ctx, repositoryRoot, "rev-parse", "HEAD")
	environment := rma02EnvironmentEvidence{
		Go:     strings.TrimSpace(string(runOutput(t, ctx, "", "go", "version"))),
		Git:    strings.TrimSpace(string(runOutput(t, ctx, "", "git", "--version"))),
		Docker: strings.TrimSpace(string(runOutput(t, ctx, "", "docker", "version", "--format", "{{.Server.Version}}"))),
		Claude: realClaudeVersion,
	}

	root := t.TempDir()
	source, initialSHA := createRMA02SourceRepository(t, ctx, root)
	dataDir, socket := filepath.Join(root, "data"), filepath.Join(root, "data", "operator.sock")
	registerLiveHomeCleanup(t, image, dataDir)
	instructions := testsupport.WriteFile(t, filepath.Join(root, "rma02-instructions.md"), []byte(rma02Instructions), 0o600)
	configPath := testsupport.WriteFile(t, filepath.Join(root, "rma02.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{
		DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 4,
		// 场景期 retention 非零(refs/workspaces 存活 24h):验收核验逐 ref rev-parse
		// 断言与 final fixture 之前,自动 GC(2s 周期)不得删除任何资源;
		// GC 段由 runRMA02GCSegment 重写为 0/0 并受控重启(与 deterministic_test.go 一致)。
		CompletedWorkspace: "24h", TerminalTaskRef: "24h", RunLog: "24h",
		DockerNetwork: network, DefaultImage: image, ProviderEnv: providerEnv,
		Tail: "  run_timeout: 18m\n  shutdown_grace: 5s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n",
	}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	trackFailure := registerLiveFailureDiagnostics(t, coordplane, socket, dataDir, providerEnv, func() error { return daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "RMA-02 initial daemon startup")
	sources := addRMA02Sources(t, ctx, coordplane, socket, image, instructions)
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "RMA-02 four-Agent recovery", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", sources[0].agent.ID, "--request-id", "rma02-project", "--output", "json")
	if project.InitialSHA != initialSHA || project.CanonicalSHA != initialSHA {
		t.Fatalf("RMA-02 Project did not register C0: %#v", project)
	}
	for _, source := range sources {
		runJSON[core.Agent](t, ctx, coordplane, "agent", "pause", source.agent.ID, "--socket", socket,
			"--request-id", "rma02-pause-"+source.role, "--output", "json")
	}
	createRMA02Tasks(t, ctx, coordplane, socket, project.ID, initialSHA, sources)
	registerRMA02FailureClassification(t, coordplane, socket, project.ID, sources)
	for index := range sources {
		trackFailure(sources[index].task.ID)
		runJSON[core.Agent](t, ctx, coordplane, "agent", "resume", sources[index].agent.ID,
			"--socket", socket, "--request-id", "rma02-resume-"+sources[index].role, "--output", "json")
	}

	overlapAt, direct := waitForRMA02Barrier(t, ctx, coordplane, socket, project.ID, sources)
	assertRMA02LiveTopology(t, ctx, coordplane, socket, project.ID, sources)
	beforeEvents := allRMA02Events(t, ctx, coordplane, socket, project.ID)
	if countRMA02RunEntityEvents(beforeEvents, sources[0].run.ID, direct.ID, "message.created") != 1 ||
		countRMA02RunEntityEvents(beforeEvents, sources[1].run.ID, direct.ID, "message.acknowledged") != 1 {
		t.Fatal("RMA-02 direct Message did not have one durable create and ack transition before restart")
	}
	before := rma02Fences(sources)
	beforePending := rma02PendingSignature(t, ctx, coordplane, socket, sources)
	beforeProject := projectDetail(t, ctx, coordplane, socket, project.ID)
	if err := daemon.Kill(); err != nil {
		t.Fatalf("SIGKILL RMA-02 daemon: %v", err)
	}
	for _, source := range sources {
		if inspect := inspectContainer(t, ctx, source.run.ContainerID); !inspect.State.Running {
			t.Fatalf("source %s container exited with daemon", source.role)
		}
	}
	restartStartedAt := time.Now().UTC()
	daemon = startDaemon(t, coordplane, configPath, socket)
	waitForReady(t, ctx, coordplane, socket, "RMA-02 adoption readiness fence")
	readyObservedAt := time.Now().UTC()
	after := make([]rma02RunFence, 0, len(sources))
	for index := range sources {
		detail := taskDetail(t, ctx, coordplane, socket, sources[index].task.ID)
		if detail.CurrentRun == nil || detail.CurrentRun.ID != sources[index].run.ID || detail.CurrentRun.State != core.RunActive {
			t.Fatalf("source %s was not adopted in place: %#v", sources[index].role, detail.CurrentRun)
		}
		sources[index].run = *detail.CurrentRun
		if info, err := os.Stat(filepath.Join(dataDir, "run-control", detail.CurrentRun.ID, "api.sock")); err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("source %s listener was not rebuilt: %v", sources[index].role, err)
		}
		after = append(after, rma02Fence(*detail.CurrentRun))
	}
	assertRMA02LiveTopology(t, ctx, coordplane, socket, project.ID, sources)
	afterMessage := rma02Message(t, ctx, coordplane, socket, project.ID)
	afterProject := projectDetail(t, ctx, coordplane, socket, project.ID)
	mutationsBeforeReady := countRMA02ActorEventsBetween(t, allRMA02Events(t, ctx, coordplane, socket, project.ID), restartStartedAt, readyObservedAt)
	var firstContinueAt time.Time
	for _, source := range sources {
		message := sendBossMessage(t, ctx, coordplane, socket, project.ID, source.agent.ID, source.task.ID,
			"RMA02-CONTINUE "+source.role, "rma02-continue-"+source.role)
		created := parseRMA02Time(t, message.CreatedAt)
		if firstContinueAt.IsZero() || created.Before(firstContinueAt) {
			firstContinueAt = created
		}
	}
	restart := rma02RestartEvidence{
		Count: 1, LiveRunsBefore: len(before), Before: before, After: after, ListenerRestored: true,
		MessageStable:        direct.ID == afterMessage.ID && direct.State == afterMessage.State && direct.Version == afterMessage.Version,
		PendingActionsStable: beforePending == rma02PendingSignature(t, ctx, coordplane, socket, sources),
		GitFactsStable:       beforeProject.CanonicalSHA == afterProject.CanonicalSHA && beforeProject.ActualCanonicalSHA == afterProject.ActualCanonicalSHA,
		ReadyObservedAt:      readyObservedAt.Format(time.RFC3339Nano), FirstContinueAt: firstContinueAt.Format(time.RFC3339Nano),
		ReadyBeforeContinue: !firstContinueAt.Before(readyObservedAt), MutationsBeforeReady: mutationsBeforeReady,
	}

	for index := range sources {
		sources[index].task = waitForTaskWithin(t, ctx, coordplane, socket, sources[index].task.ID,
			"RMA-02 source "+sources[index].role+" submission", 8*time.Minute, capturedSubmission)
		requireRMA02FixtureMarker(t, dataDir, sources[index].agent.ID, sources[index].task.ID, "source-"+sources[index].role)
	}
	events := allRMA02Events(t, ctx, coordplane, socket, project.ID)
	sourceEvidence := make([]rma02SourceEvidence, 0, len(sources))
	for index := range sources {
		terminalRun := runJSON[core.Run](t, ctx, coordplane, "run", "show", sources[index].run.ID, "--socket", socket, "--output", "json")
		sources[index].taskCurrent, sources[index].fixture, sources[index].fixtureExit = readRMA02SourceArtifacts(t, dataDir, sources[index].agent.ID, sources[index].task.ID)
		taskCurrentObserved := sources[index].taskCurrent.Task.ID == sources[index].task.ID && sources[index].taskCurrent.Run.ID == terminalRun.ID
		sourceEvidence = append(sourceEvidence, rma02SourceEvidence{
			Role: sources[index].role, TaskID: sources[index].task.ID, AgentID: sources[index].agent.ID, BaseSHA: sources[index].task.BaseSHA,
			RunID: terminalRun.ID, ContainerID: terminalRun.ContainerID, Generation: terminalRun.Generation, LaunchNonce: terminalRun.LaunchNonce,
			LiveFrom: terminalRun.StartedAt, LiveUntil: terminalRun.EndedAt, ProgressMarker: sources[index].marker,
			CoordlinkOperations: 1 + countRMA02Events(events, terminalRun.ID, "task.progress"),
			TaskCurrentObserved: taskCurrentObserved, TaskCurrentTaskID: sources[index].taskCurrent.Task.ID, TaskCurrentRunID: sources[index].taskCurrent.Run.ID,
			FixtureMarker: sources[index].fixture, FixtureEventCount: countRMA02ProgressMarkers(events, terminalRun.ID, rma02FixtureEventMarker(sources[index].task.ID, terminalRun.ID)), FixtureExitCode: sources[index].fixtureExit,
			CommitSHA: sources[index].task.HeadSHA, HeadSHA: sources[index].task.HeadSHA, HeadRunID: sources[index].task.HeadRunID,
			TaskRef: sources[index].task.TaskRef, SubmitEventCount: countRMA02EntityEvents(events, sources[index].task.ID, "task.submit_requested"),
		})
	}

	integrations, integrationCanonicals := acceptRMA02Sources(t, ctx, coordplane, socket, dataDir, project, sources, trackFailure)
	allTasks := make([]core.Task, 0, 7)
	for _, source := range sources {
		allTasks = append(allTasks, taskDetail(t, ctx, coordplane, socket, source.task.ID).Task)
	}
	allTasks = append(allTasks, integrations...)
	controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
	finalProject := projectDetail(t, ctx, coordplane, socket, project.ID)
	finalSHA := gitDir(t, ctx, controlRepo, "rev-parse", project.CanonicalRef)
	if finalProject.CanonicalSHA != finalSHA || finalProject.ActualCanonicalSHA != finalSHA {
		t.Fatalf("RMA-02 canonical projection drift: %#v actual=%s", finalProject, finalSHA)
	}
	for _, source := range sources {
		gitDirSucceeds(t, ctx, controlRepo, "merge-base", "--is-ancestor", source.task.HeadSHA, finalSHA)
	}
	for _, task := range allTasks {
		if got := gitDir(t, ctx, controlRepo, "rev-parse", task.TaskRef); got != task.HeadSHA {
			t.Fatalf("task ref %s = %s, want %s", task.TaskRef, got, task.HeadSHA)
		}
	}
	gitDirSucceeds(t, ctx, controlRepo, "fsck", "--full", "--strict")
	finalCheckout := filepath.Join(root, "final")
	run(t, ctx, "git", "clone", "--quiet", controlRepo, finalCheckout)
	git(t, ctx, finalCheckout, "checkout", "--quiet", finalSHA)
	runIn(t, ctx, finalCheckout, "./fixture-test.sh", "final")
	waitForNoProjectContainers(t, ctx, project.ID, daemon.logPath)

	beforeFinalRestart := rma02PublicState(t, ctx, coordplane, socket, project.ID)
	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop RMA-02 daemon before final query: %v", err)
	}
	daemon = startDaemon(t, coordplane, configPath, socket)
	waitForReady(t, ctx, coordplane, socket, "RMA-02 final query restart")
	afterFinalRestart := rma02PublicState(t, ctx, coordplane, socket, project.ID)
	stable := beforeFinalRestart.signature == afterFinalRestart.signature
	if len(afterFinalRestart.tasks.Items) != 7 || len(afterFinalRestart.runs.Items) != 7 || len(afterFinalRestart.messages.Items) < 5 {
		t.Fatalf("RMA-02 final projection cardinality = tasks:%d runs:%d messages:%d",
			len(afterFinalRestart.tasks.Items), len(afterFinalRestart.runs.Items), len(afterFinalRestart.messages.Items))
	}
	integrationCount := 0
	for _, task := range afterFinalRestart.tasks.Items {
		if task.Kind == core.TaskIntegration {
			integrationCount++
		}
	}
	if integrationCount != 3 {
		t.Fatalf("RMA-02 final integration count = %d, want 3", integrationCount)
	}
	for _, task := range allTasks {
		_ = taskDetail(t, ctx, coordplane, socket, task.ID)
	}
	for _, run := range afterFinalRestart.runs.Items {
		_ = runJSON[core.Run](t, ctx, coordplane, "run", "show", run.ID, "--socket", socket, "--output", "json")
	}
	messageEvents := allRMA02Events(t, ctx, coordplane, socket, project.ID)
	integrationEvidence := buildRMA02IntegrationEvidence(t, ctx, controlRepo, dataDir, integrations, integrationCanonicals, sources, messageEvents)
	for _, source := range sources {
		runJSON[core.Agent](t, ctx, coordplane, "agent", "archive", source.agent.ID,
			"--socket", socket, "--request-id", "rma02-archive-"+source.role, "--output", "json")
	}
	daemon = runRMA02GCSegment(t, ctx, coordplane, configPath, socket, controlRepo, daemon)
	cleanup := collectRMA02Cleanup(t, ctx, coordplane, socket, dataDir, controlRepo, project.ID, allTasks, afterFinalRestart.runs.Items)
	databaseCanonical := rma02SQLiteCanonical(t, filepath.Join(dataDir, "coordplane.db"), project.ID)
	evidence := rma02Evidence{
		SchemaVersion: 1, ScenarioID: "RMA-02", ScenarioExecutions: 1, Result: "PASS_REAL_MULTI_AGENT_LOCAL",
		Revision: revision, SourceClean: true, ImageDigest: image, Environment: environment,
		Commands:  []rma02CommandEvidence{{"source-fixtures", 0}, {"source-submits", 0}, {"daemon-sigkill-restart", 0}, {"accept-cascade", 0}, {"final-fixture", 0}, {"git-fsck", 0}, {"final-restart-query", 0}, {"gc-preview", 0}, {"gc-run", 0}},
		StartedAt: started.Format(time.RFC3339Nano), EndedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ProjectID: project.ID, InitialSHA: initialSHA, Sources: sourceEvidence,
		Overlap: rma02OverlapEvidence{ObservedAt: overlapAt.Format(time.RFC3339Nano), ActiveRunIDs: rma02RunIDs(sources), RunningContainerIDs: rma02ContainerIDs(sources)},
		Message: rma02MessageEvidence{ID: direct.ID, SenderRunID: sources[0].run.ID, DeliveryTaskID: direct.TaskID, RecipientAgentID: sources[1].agent.ID,
			AcknowledgerRunID: sources[1].run.ID, State: string(direct.State),
			CreatedEventCount: countRMA02RunEntityEvents(messageEvents, sources[0].run.ID, direct.ID, "message.created"),
			AckEventCount:     countRMA02RunEntityEvents(messageEvents, sources[1].run.ID, direct.ID, "message.acknowledged"), DurableBeforeRestart: true},
		Restart: restart, DirectCASTaskID: sources[0].task.ID, DirectCASCount: rma02DirectCASCount(allTasks[:4]), Integrations: integrationEvidence,
		Final: rma02FinalEvidence{SQLiteCanonical: databaseCanonical, BossCanonical: finalProject.CanonicalSHA, ActualCanonical: finalSHA,
			SourceAncestors: rma02SourceHeads(sources), TaskRefsVerified: len(allTasks), FixtureExitCode: 0, FSCKExitCode: 0,
			FinalRestartCount: 1, TasksQueried: len(afterFinalRestart.tasks.Items), RunsQueried: len(afterFinalRestart.runs.Items), MessagesQueried: len(afterFinalRestart.messages.Items), StateStableAfterRestart: stable},
		Cleanup: cleanup,
	}
	secrets := make([]string, 0, len(providerEnv))
	for _, name := range providerEnv {
		secrets = append(secrets, os.Getenv(name))
	}
	if err := validateRMA02Evidence(evidence, secrets...); err != nil {
		t.Fatalf("RMA-02 evidence rejected: %v", err)
	}
	writeRMA02Evidence(t, reportPath, evidence)
}

// runRMA02GCSegment 是 RMA-02 GC 段,分两个阶段,均无时序竞态:
//
// 阶段 A(场景期 24h 配置,preview 前移):gc preview 断言 7 workspaces + 7 refs 全部
// 存在——场景期 retention 非零正是验收核验(逐 ref rev-parse)与此刻 preview 的保障;
// 此时所有 Task closed 不足 24h,自动 GC 无候选,preview 与自动 GC 之间不存在竞态。
//
// 阶段 B(GT-07 合同路径,见 tests/contract/p1_binary_test.go
// TestGT07FormalOperatorBinaryChecksOutExactControllerTaskRef 的 config 重写模式):
// 重写 retention 为 0/0 并受控重启 daemon。自动 GC 循环(runRuntimeGC, 2s 周期,
// retention 0)首个 tick 即按当前配置对既有 closed Task 生效:先删 7 workspaces
// (删除时 releaseSourceReference 放行 B/C/D 的源 ref),再删 7 refs。测试用
// for-each-ref 轮询等待 7 refs 消失,可观测地证明「修改 retention 并重启后,自动 GC
// 按当前配置作用于既有 closed Task」;随后 preview 全部 absent、gc run --confirm
// 幂等收敛(dedupe + agent home GC),最终资源收敛由 collectRMA02Cleanup 断言。
//
// 为什么 preview 的「存在性」断言只能放在阶段 A:workspace 状态检查每次启动一个
// capture helper 容器(7 个 workspace 串行约 2.6s),而自动 GC 首个 tick 在重启后
// >=2s 触发——重启后立即 preview 必然与首个 tick 竞态,存在性断言无法确定完成;
// 阶段 B 改为轮询 git refs 观察自动 GC 的确定性结果,不依赖时序运气。
func runRMA02GCSegment(t *testing.T, ctx context.Context, binary, configPath, socket, controlRepo string, daemon *daemonProcess) *daemonProcess {
	t.Helper()
	preview := runGCJSON[core.GCPreview](t, ctx, binary, daemon.logPath, "gc", "preview", "--socket", socket, "--output", "json")
	if len(preview.Workspaces) != 7 || len(preview.TaskRefs) != 7 {
		t.Fatalf("RMA-02 GC preview cardinality = workspaces:%d refs:%d", len(preview.Workspaces), len(preview.TaskRefs))
	}
	for _, target := range preview.Workspaces {
		if !target.Exists {
			t.Fatalf("RMA-02 GC preview workspace %s was deleted during the scenario: %#v", target.TaskID, target)
		}
	}
	// 源 Task B/C/D 的 ref 此时存在但不可 GC:其消费方(integration Task)的
	// source_ref_released_at 仅在消费方 workspace 被 GC 时落库(TaskRefEligible 的
	// blocker 查询),preview 阶段尚未释放;释放发生在阶段 B 的 workspace 删除中。
	for _, target := range preview.TaskRefs {
		if !target.Exists {
			t.Fatalf("RMA-02 GC preview task ref %s was deleted during the scenario: %#v", target.TaskID, target)
		}
	}
	gcRaw, err := os.ReadFile(configPath)
	requireNoError(t, err)
	gcRaw = bytes.ReplaceAll(gcRaw, []byte("completed_workspace: 24h"), []byte("completed_workspace: 0"))
	gcRaw = bytes.ReplaceAll(gcRaw, []byte("terminal_task_ref: 24h"), []byte("terminal_task_ref: 0"))
	gcConfigPath := testsupport.WriteFile(t, filepath.Join(filepath.Dir(configPath), "rma02-gc.yaml"), gcRaw, 0o600)
	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop RMA-02 daemon before GC retention restart: %v", err)
	}
	daemon = startDaemon(t, binary, gcConfigPath, socket)
	waitForReady(t, ctx, binary, socket, "RMA-02 GC retention restart")
	// 自动 GC 首个 tick 于重启后 >=2s 触发,同一 tick 先删 workspaces(含源 ref 释放)
	// 再删 refs;refs 全部消失 ⟹ workspace 阶段无错误完成,7 个资源已被 retention-0
	// 自动 GC 删除。轮询只读 git,与自动 GC 无竞态。
	eventually(t, ctx, 30*time.Second, "RMA-02 retention-0 auto-GC deleted all 7 task refs", func() (bool, bool, string) {
		refs := strings.Fields(gitDir(t, ctx, controlRepo, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/"))
		return len(refs) == 0, len(refs) == 0, fmt.Sprintf("remaining task refs: %d", len(refs))
	})
	// refs 消失只证明自动 GC 删完了 workspace 与 ref,不证明最后一个删除用 inspect
	// helper 容器已移除:workspace 删除每项启动一个 inspect 容器,detectOrphans 对
	// 无 Run row 的 helper 容器 fail-closed,reconciler 在容器移除前持续置 degraded
	// (live #11 即在此窗口 exit 1)。settle 等待全部 managed 容器(含 capture 等非
	// inspect 形态)清空;degraded 状态在容器清空后由下一 reconciler tick(<=2s)清除,
	// waitForReady 确认 flapping 窗口完全排空。workspace 已删 → 状态检查走 absent
	// 快路径(无容器),preview 与自动 GC 无竞态。
	waitForManagedContainersDrained(t, ctx, daemon.logPath)
	waitForReady(t, ctx, binary, socket, "RMA-02 daemon ready after retention-0 auto-GC")
	// readyStable 探针覆盖 afterGC preview → gc run 全程:settle + ready 确认之后
	// 任何 degraded 观察都判失败并带证据,不再靠时序运气通过。
	readyStable := watchDaemonReadyStable(t, ctx, binary, socket)
	afterGC := runGCJSON[core.GCPreview](t, ctx, binary, daemon.logPath, "gc", "preview", "--socket", socket, "--output", "json")
	if len(afterGC.Workspaces) != 7 || len(afterGC.TaskRefs) != 7 {
		t.Fatalf("RMA-02 GC preview cardinality after retention-0 restart = workspaces:%d refs:%d", len(afterGC.Workspaces), len(afterGC.TaskRefs))
	}
	for _, target := range afterGC.Workspaces {
		if target.Exists {
			t.Fatalf("RMA-02 GC preview workspace %s survived retention-0 auto-GC: %#v", target.TaskID, target)
		}
	}
	for _, target := range afterGC.TaskRefs {
		if target.Exists {
			t.Fatalf("RMA-02 GC preview task ref %s survived retention-0 auto-GC: %#v", target.TaskID, target)
		}
	}
	// managed 容器已清空 + daemon ready 确认 ⟹ flapping 窗口完全排空;gc run 期间
	// 所有 workspace 均已 absent(快路径无容器),readyStable 断言全程零 degraded。
	if result := runGCJSON[core.GCRunResult](t, ctx, binary, daemon.logPath, "gc", "run", "--socket", socket,
		"--confirm", "--request-id", "rma02-gc", "--output", "json"); !result.Completed {
		t.Fatalf("RMA-02 GC result = %#v", result)
	}
	readyStable()
	return daemon
}

func addRMA02Sources(t *testing.T, ctx context.Context, binary, socket, image, instructions string) []rma02Source {
	t.Helper()
	sources := make([]rma02Source, 4)
	for index, role := range []string{"A", "B", "C", "D"} {
		agent := runJSON[core.Agent](t, ctx, binary,
			"agent", "add", "--socket", socket, "--id", "agt_rma02_"+strings.ToLower(role), "--display-name", "RMA-02 Agent "+role,
			"--adapter", "claude", "--image", image, "--instructions-file", instructions,
			"--request-id", "rma02-agent-"+strings.ToLower(role), "--output", "json")
		sources[index] = rma02Source{role: role, marker: "RMA02-READY-" + role, agent: agent}
	}
	return sources
}

func createRMA02Tasks(t *testing.T, ctx context.Context, binary, socket, projectID, initialSHA string, sources []rma02Source) {
	t.Helper()
	for _, index := range []int{1, 0, 2, 3} {
		peerAgent, peerTask := "", ""
		if sources[index].role == "A" {
			peerAgent = sources[1].agent.ID
			peerTask = sources[1].task.ID
		}
		description := fmt.Sprintf("rma_role=%s\ntask_marker=%s\nfile=agent-%s.txt\ncontent=agent-%s\\n\nfixture=./fixture-test.sh source %s TASK_ID RUN_ID\npeer_agent_id=%s\npeer_task_id=%s",
			sources[index].role, sources[index].marker, sources[index].role, sources[index].role, sources[index].role, peerAgent, peerTask)
		sources[index].task = runJSON[core.Task](t, ctx, binary,
			"task", "create", "--socket", socket, "--project", projectID, "--agent", sources[index].agent.ID,
			"--title", "RMA-02 source "+sources[index].role, "--description", description,
			"--request-id", "rma02-task-"+sources[index].role, "--output", "json")
		if sources[index].task.BaseSHA != initialSHA {
			t.Fatalf("source %s base = %s, want C0 %s", sources[index].role, sources[index].task.BaseSHA, initialSHA)
		}
	}
}

func waitForRMA02Barrier(t *testing.T, ctx context.Context, binary, socket, projectID string, sources []rma02Source) (time.Time, core.Message) {
	t.Helper()
	type barrier struct {
		runs    []core.Run
		message core.Message
	}
	result := eventually(t, ctx, 7*time.Minute, "RMA-02 four live Runs and acknowledged direct Message", func() (barrier, bool, string) {
		var value barrier
		for _, source := range sources {
			detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", source.task.ID, "--socket", socket, "--output", "json")
			if err != nil || detail.CurrentRun == nil || detail.CurrentRun.State != core.RunActive || detail.LatestProgress == nil || !strings.Contains(detail.LatestProgress.PayloadJSON, source.marker) {
				return value, false, fmt.Sprintf("source %s not at barrier", source.role)
			}
			value.runs = append(value.runs, *detail.CurrentRun)
		}
		message, ok := findRMA02Message(ctx, binary, socket, projectID)
		if !ok || message.State != core.MessageAcknowledged {
			return value, false, "direct Message is not acknowledged"
		}
		value.message = message
		return value, true, ""
	})
	for index := range sources {
		sources[index].run = result.runs[index]
		if inspect := inspectContainer(t, ctx, sources[index].run.ContainerID); !inspect.State.Running {
			t.Fatalf("source %s container is not running at overlap", sources[index].role)
		}
	}
	return time.Now().UTC(), result.message
}

func findRMA02Message(ctx context.Context, binary, socket, projectID string) (core.Message, bool) {
	page, err := commandJSON[core.MessagePage](ctx, binary, "message", "list", "--socket", socket,
		"--project", projectID, "--limit", "20", "--output", "json")
	if err != nil {
		return core.Message{}, false
	}
	for _, message := range page.Items {
		if strings.Contains(message.Body, "RMA02-DIRECT") {
			return message, true
		}
	}
	return core.Message{}, false
}

func rma02Message(t *testing.T, ctx context.Context, binary, socket, projectID string) core.Message {
	t.Helper()
	message, ok := findRMA02Message(ctx, binary, socket, projectID)
	if !ok {
		t.Fatal("RMA-02 direct Message disappeared")
	}
	return message
}

func rma02Fence(run core.Run) rma02RunFence {
	return rma02RunFence{TaskID: run.TaskID, AgentID: run.AgentID, RunID: run.ID, ContainerID: run.ContainerID, Generation: run.Generation, LaunchNonce: run.LaunchNonce}
}

func rma02Fences(sources []rma02Source) []rma02RunFence {
	result := make([]rma02RunFence, 0, len(sources))
	for _, source := range sources {
		result = append(result, rma02Fence(source.run))
	}
	return result
}

func rma02PendingSignature(t *testing.T, ctx context.Context, binary, socket string, sources []rma02Source) string {
	t.Helper()
	var values []string
	for _, source := range sources {
		task := taskDetail(t, ctx, binary, socket, source.task.ID).Task
		values = append(values, task.ID+":"+task.PendingAction+":"+task.PendingActionID)
	}
	return strings.Join(values, "|")
}

func assertRMA02LiveTopology(t *testing.T, ctx context.Context, binary, socket, projectID string, sources []rma02Source) {
	t.Helper()
	wantRuns, wantContainers := rma02RunIDs(sources), rma02ContainerIDs(sources)
	sort.Strings(wantRuns)
	sort.Strings(wantContainers)
	page := runJSON[core.RunPage](t, ctx, binary, "run", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	var live []string
	for _, run := range page.Items {
		if core.IsRunLive(run.State) {
			live = append(live, run.ID)
		}
	}
	sort.Strings(live)
	if strings.Join(live, "\x00") != strings.Join(wantRuns, "\x00") {
		t.Fatalf("RMA-02 live Runs = %v, want %v", live, wantRuns)
	}
	raw := runOutput(t, ctx, "", "docker", "ps", "-q", "--no-trunc", "--filter", "label=coordplane.project_id="+projectID)
	containers := strings.Fields(string(raw))
	sort.Strings(containers)
	if strings.Join(containers, "\x00") != strings.Join(wantContainers, "\x00") {
		t.Fatalf("RMA-02 running containers = %v, want %v", containers, wantContainers)
	}
}

func acceptRMA02Sources(t *testing.T, ctx context.Context, binary, socket, dataDir string, project core.Project, sources []rma02Source, track func(...string)) ([]core.Task, []string) {
	t.Helper()
	var integrations []core.Task
	var observedCanonicals []string
	for index := range sources {
		beforeAccept := projectDetail(t, ctx, binary, socket, project.ID)
		if beforeAccept.CanonicalSHA == "" || beforeAccept.CanonicalSHA != beforeAccept.ActualCanonicalSHA {
			t.Fatalf("RMA-02 canonical drift before accepting source %s: %#v", sources[index].role, beforeAccept)
		}
		runJSON[core.Task](t, ctx, binary, "task", "accept", sources[index].task.ID, "--socket", socket,
			"--integration-agent", sources[0].agent.ID, "--request-id", "rma02-accept-"+sources[index].role, "--output", "json")
		if index == 0 {
			if beforeAccept.ActualCanonicalSHA != project.InitialSHA {
				t.Fatalf("RMA-02 direct CAS observed canonical = %s, want C0 %s", beforeAccept.ActualCanonicalSHA, project.InitialSHA)
			}
			sources[index].task = waitForTaskWithin(t, ctx, binary, socket, sources[index].task.ID, "RMA-02 direct CAS", 2*time.Minute,
				func(task core.Task) bool {
					return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
				})
			if actual := projectDetail(t, ctx, binary, socket, project.ID).ActualCanonicalSHA; actual != sources[index].task.HeadSHA {
				t.Fatalf("RMA-02 direct CAS actual canonical = %s, want %s", actual, sources[index].task.HeadSHA)
			}
			continue
		}
		sources[index].task = waitForTaskWithin(t, ctx, binary, socket, sources[index].task.ID, "RMA-02 stale source link", 2*time.Minute,
			func(task core.Task) bool { return task.Status == core.TaskSubmitted && task.IntegrationTaskID != "" })
		track(sources[index].task.IntegrationTaskID)
		integration := waitForTaskWithin(t, ctx, binary, socket, sources[index].task.IntegrationTaskID, "RMA-02 integration", 8*time.Minute,
			func(task core.Task) bool {
				return task.Status == core.TaskCompleted && task.HeadSHA != "" && task.FinalCanonicalSHA == task.HeadSHA
			})
		if integration.Kind != core.TaskIntegration || integration.SourceTaskID != sources[index].task.ID ||
			integration.SourceRunID != sources[index].task.HeadRunID || integration.SourceTaskRef != sources[index].task.TaskRef ||
			integration.SourceHeadSHA != sources[index].task.HeadSHA || integration.ObservedCanonicalSHA != beforeAccept.ActualCanonicalSHA {
			t.Fatalf("RMA-02 integration source/canonical mapping drift for %s: source=%#v integration=%#v observed=%s",
				sources[index].role, sources[index].task, integration, beforeAccept.ActualCanonicalSHA)
		}
		current := requireRMA02FixtureMarker(t, dataDir, sources[0].agent.ID, integration.ID, "integration")
		if current.Run.ID != integration.HeadRunID {
			t.Fatalf("RMA-02 integration %s fixture Run = %s, want captured Run %s", integration.ID, current.Run.ID, integration.HeadRunID)
		}
		fixtureEvents := countRMA02ProgressMarkers(allRMA02Events(t, ctx, binary, socket, project.ID), integration.HeadRunID, rma02FixtureEventMarker(integration.ID, integration.HeadRunID))
		if fixtureEvents != 1 {
			t.Fatalf("RMA-02 integration %s fixture Event count = %d, want 1", integration.ID, fixtureEvents)
		}
		sources[index].task = waitForTaskWithin(t, ctx, binary, socket, sources[index].task.ID, "RMA-02 source completion", 2*time.Minute,
			func(task core.Task) bool {
				return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == integration.HeadSHA
			})
		integrations = append(integrations, integration)
		observedCanonicals = append(observedCanonicals, beforeAccept.ActualCanonicalSHA)
	}
	return integrations, observedCanonicals
}

func buildRMA02IntegrationEvidence(t *testing.T, ctx context.Context, controlRepo, dataDir string, integrations []core.Task, observedCanonicals []string, sources []rma02Source, events []core.Event) []rma02IntegrationProof {
	t.Helper()
	result := make([]rma02IntegrationProof, 0, len(integrations))
	for index, integration := range integrations {
		source := sources[index+1].task
		current, marker, exitCode := readRMA02SourceArtifacts(t, dataDir, sources[0].agent.ID, integration.ID)
		gitDirSucceeds(t, ctx, controlRepo, "merge-base", "--is-ancestor", integration.SourceHeadSHA, integration.HeadSHA)
		gitDirSucceeds(t, ctx, controlRepo, "merge-base", "--is-ancestor", observedCanonicals[index], integration.HeadSHA)
		result = append(result, rma02IntegrationProof{
			TaskID: integration.ID, RunID: integration.HeadRunID, SourceTaskID: integration.SourceTaskID, SourceRunID: integration.SourceRunID,
			SourceTaskRef: integration.SourceTaskRef, SourceHeadSHA: integration.SourceHeadSHA, ObservedCanonical: observedCanonicals[index],
			HeadSHA: integration.HeadSHA, SourceAncestor: integration.SourceHeadSHA == source.HeadSHA,
			CanonicalAncestor: true, NestedIntegration: source.Kind == core.TaskIntegration,
			FixtureTaskID: current.Task.ID, FixtureRunID: current.Run.ID, FixtureMarker: marker,
			FixtureEventCount: countRMA02ProgressMarkers(events, integration.HeadRunID, rma02FixtureEventMarker(integration.ID, integration.HeadRunID)), FixtureExitCode: exitCode,
			SubmitEventCount: countRMA02EntityEvents(events, integration.ID, "task.submit_requested"),
		})
	}
	return result
}

type rma02State struct {
	signature string
	tasks     core.TaskPage
	runs      core.RunPage
	messages  core.MessagePage
}

func rma02PublicState(t *testing.T, ctx context.Context, binary, socket, projectID string) rma02State {
	t.Helper()
	project := projectDetail(t, ctx, binary, socket, projectID)
	tasks := runJSON[core.TaskPage](t, ctx, binary, "task", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	runs := runJSON[core.RunPage](t, ctx, binary, "run", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	messages := runJSON[core.MessagePage](t, ctx, binary, "message", "list", "--socket", socket, "--project", projectID, "--limit", "20", "--output", "json")
	raw, err := json.Marshal([]any{project, tasks, runs, messages})
	requireNoError(t, err)
	return rma02State{signature: string(raw), tasks: tasks, runs: runs, messages: messages}
}

func allRMA02Events(t *testing.T, ctx context.Context, binary, socket, projectID string) []core.Event {
	t.Helper()
	var result []core.Event
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		args := []string{"events", "tail", "--socket", socket, "--project", projectID, "--limit", "100"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		args = append(args, "--output", "json")
		page := runJSON[core.EventPage](t, ctx, binary, args...)
		result = append(result, page.Items...)
		if page.NextCursor == "" {
			return result
		}
		if page.NextCursor == cursor {
			t.Fatal("RMA-02 Event cursor did not advance")
		}
		cursor = page.NextCursor
	}
	t.Fatal("RMA-02 Event history exceeded 100 pages")
	return nil
}

func countRMA02Events(events []core.Event, runID, kind string) int {
	count := 0
	for _, event := range events {
		if event.RunID == runID && event.Kind == kind {
			count++
		}
	}
	return count
}

func countRMA02EntityEvents(events []core.Event, entityID, kind string) int {
	count := 0
	for _, event := range events {
		if event.EntityID == entityID && event.Kind == kind {
			count++
		}
	}
	return count
}

func countRMA02RunEntityEvents(events []core.Event, runID, entityID, kind string) int {
	count := 0
	for _, event := range events {
		if event.RunID == runID && event.EntityID == entityID && event.Kind == kind {
			count++
		}
	}
	return count
}

func countRMA02ProgressMarkers(events []core.Event, runID, marker string) int {
	count := 0
	for _, event := range events {
		if event.RunID == runID && event.Kind == "task.progress" && strings.Contains(event.PayloadJSON, marker) {
			count++
		}
	}
	return count
}

func countRMA02ActorEventsBetween(t *testing.T, events []core.Event, started, ended time.Time) int {
	t.Helper()
	count := 0
	for _, event := range events {
		if event.ActorKind != "boss" && event.ActorKind != "agent" {
			continue
		}
		created := parseRMA02Time(t, event.CreatedAt)
		if !created.Before(started) && created.Before(ended) {
			count++
		}
	}
	return count
}

func collectRMA02Cleanup(t *testing.T, ctx context.Context, binary, socket, dataDir, controlRepo, projectID string, tasks []core.Task, runs []core.RunSummary) rma02CleanupEvidence {
	t.Helper()
	runPage := runJSON[core.RunPage](t, ctx, binary, "run", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	taskPage := runJSON[core.TaskPage](t, ctx, binary, "task", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	result := rma02CleanupEvidence{GCRan: true}
	for _, run := range runPage.Items {
		if core.IsRunLive(run.State) {
			result.LiveRuns++
		}
		if run.CleanupState == core.CleanupBlocked {
			result.BlockedCleanup++
		}
	}
	for _, task := range taskPage.Items {
		if task.PendingAction != "" {
			result.PendingGitActions++
		}
	}
	raw := runOutput(t, ctx, "", "docker", "ps", "-aq", "--filter", "label=coordplane.project_id="+projectID)
	if ids := strings.Fields(string(raw)); len(ids) != 0 {
		result.OwnedContainers = len(ids)
	}
	workspaceDirs := map[string]bool{projectID: true, ".partial": true, ".partial/" + projectID: true, ".empty-git-template": true}
	handoffDirs := map[string]bool{projectID: true, "quarantine": true}
	for _, task := range tasks {
		handoffDirs[projectID+"/"+task.ID] = true
	}
	logDirs, logFiles := map[string]bool{}, map[string]bool{}
	for _, run := range runs {
		logDirs[run.ID] = true
		logFiles[run.ID+"/run.log"] = true
	}
	result.WorkspaceResidue = requireRMA02ResidueCount(t, filepath.Join(dataDir, "workspaces"), workspaceDirs, nil)
	result.HandoffResidue = requireRMA02ResidueCount(t, filepath.Join(dataDir, "handoff"), handoffDirs, nil)
	result.UnknownControlEntries = requireRMA02ResidueCount(t, filepath.Join(dataDir, "run-control"), nil, nil)
	result.LogResidue = requireRMA02ResidueCount(t, filepath.Join(dataDir, "logs"), logDirs, logFiles)
	result.AgentHomeResidue = requireRMA02ResidueCount(t, filepath.Join(dataDir, "agent-homes"), nil, nil)
	result.TaskRefResidue = len(strings.Fields(gitDir(t, ctx, controlRepo, "for-each-ref", "--format=%(refname)", "refs/coordplane/tasks/")))
	return result
}

func requireRMA02ResidueCount(t *testing.T, root string, allowedDirs, allowedFiles map[string]bool) int {
	t.Helper()
	count, err := countRMA02UnexpectedEntries(root, allowedDirs, allowedFiles)
	requireNoError(t, err)
	return count
}

func countRMA02UnexpectedEntries(root string, allowedDirs, allowedFiles map[string]bool) (int, error) {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() && allowedDirs[relative] {
			return nil
		}
		if !entry.IsDir() && allowedFiles[relative] {
			return nil
		}
		count++
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return count, err
}

func rma02SQLiteCanonical(t *testing.T, databasePath, projectID string) string {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	requireNoError(t, err)
	defer database.Close()
	var sha string
	requireNoError(t, database.QueryRow(`SELECT canonical_sha FROM projects WHERE id=?`, projectID).Scan(&sha))
	return sha
}

func rma02DirectCASCount(tasks []core.Task) int {
	count := 0
	for _, task := range tasks {
		if task.FinalCanonicalSHA == task.HeadSHA {
			count++
		}
	}
	return count
}

func rma02RunIDs(sources []rma02Source) []string {
	result := make([]string, 0, len(sources))
	for _, source := range sources {
		result = append(result, source.run.ID)
	}
	return result
}

func rma02ContainerIDs(sources []rma02Source) []string {
	result := make([]string, 0, len(sources))
	for _, source := range sources {
		result = append(result, source.run.ContainerID)
	}
	return result
}

func rma02SourceHeads(sources []rma02Source) []string {
	result := make([]string, 0, len(sources))
	for _, source := range sources {
		result = append(result, source.task.HeadSHA)
	}
	return result
}

func requireRMA02FixtureMarker(t *testing.T, dataDir, agentID, taskID, marker string) core.CurrentTaskResult {
	t.Helper()
	current, actual, exitCode := readRMA02SourceArtifacts(t, dataDir, agentID, taskID)
	if actual != marker || exitCode != 0 {
		t.Fatalf("Task %s fixture marker or exit code is invalid", taskID)
	}
	return current
}

func readRMA02SourceArtifacts(t *testing.T, dataDir, agentID, taskID string) (core.CurrentTaskResult, string, int) {
	t.Helper()
	current, marker, exitCode, err := readRMA02Artifacts(dataDir, agentID, taskID)
	if err != nil {
		t.Fatalf("Task %s artifacts rejected: %v", taskID, err)
	}
	return current, marker, exitCode
}

func readRMA02Artifacts(dataDir, agentID, taskID string) (core.CurrentTaskResult, string, int, error) {
	var current core.CurrentTaskResult
	if !rma02SafePathComponent(agentID) || !rma02SafePathComponent(taskID) {
		return current, "", 0, errors.New("invalid artifact identity")
	}
	canonicalDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil || filepath.Clean(canonicalDataDir) != filepath.Clean(dataDir) {
		return current, "", 0, errors.New("artifact data root is not canonical")
	}
	dataRoot, err := openRMA02Directory(dataDir)
	if err != nil {
		return current, "", 0, err
	}
	defer dataRoot.Close()

	directory := dataRoot
	var opened []*os.File
	defer func() {
		for index := len(opened) - 1; index >= 0; index-- {
			_ = opened[index].Close()
		}
	}()
	for _, name := range []string{"agent-homes", agentID, ".coordplane-rma02", taskID} {
		directory, err = openRMA02DirectoryAt(directory, name)
		if err != nil {
			return current, "", 0, err
		}
		opened = append(opened, directory)
	}
	entries, readErr := directory.ReadDir(2)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return current, "", 0, errors.New("read artifact Run directory")
	}
	if len(entries) != 1 || !rma02SafePathComponent(entries[0].Name()) {
		return current, "", 0, errors.New("expected exactly one artifact Run directory")
	}
	runID := entries[0].Name()
	runRoot, err := openRMA02DirectoryAt(directory, runID)
	if err != nil {
		return current, "", 0, err
	}
	opened = append(opened, runRoot)

	raw, err := readRMA02ArtifactFile(runRoot, "task-current.json", 64<<10)
	if err != nil {
		return current, "", 0, err
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return current, "", 0, errors.New("decode current Task artifact")
	}
	if current.Task.ID != taskID || current.Run.ID != runID {
		return current, "", 0, errors.New("artifact Task or Run identity mismatch")
	}
	marker, err := readRMA02ArtifactFile(runRoot, "fixture", 4<<10)
	if err != nil {
		return current, "", 0, err
	}
	exitRaw, err := readRMA02ArtifactFile(runRoot, "fixture-exit", 64)
	if err != nil {
		return current, "", 0, err
	}
	exitCode, err := strconv.Atoi(strings.TrimSpace(string(exitRaw)))
	if err != nil {
		return current, "", 0, errors.New("decode fixture exit artifact")
	}
	return current, strings.TrimSpace(string(marker)), exitCode, nil
}

func openRMA02Directory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("open artifact data root")
	}
	return os.NewFile(uintptr(fd), "artifact-data-root"), nil
}

func openRMA02DirectoryAt(parent *os.File, name string) (*os.File, error) {
	if !rma02SafePathComponent(name) {
		return nil, errors.New("invalid artifact path component")
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("open artifact directory")
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readRMA02ArtifactFile(directory *os.File, name string, maximum int64) ([]byte, error) {
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open artifact file %s", name)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Size < 0 || stat.Size > maximum {
		return nil, fmt.Errorf("artifact file %s is not a bounded regular file", name)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, fmt.Errorf("artifact file %s exceeds its read limit", name)
	}
	return raw, nil
}

func rma02SafePathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsRune(value, filepath.Separator)
}

func rma02ArtifactRoot(dataDir, agentID string) string {
	return filepath.Join(dataDir, "agent-homes", agentID, ".coordplane-rma02")
}

func rma02FixtureEventMarker(taskID, runID string) string {
	return "RMA02-FIXTURE-" + taskID + "-" + runID
}

func parseRMA02Time(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	requireNoError(t, err)
	return parsed
}

func registerRMA02FailureClassification(t *testing.T, binary, socket, projectID string, sources []rma02Source) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		facts, err := collectRMA02FailureFacts(ctx, binary, socket, projectID, sources)
		if err != nil {
			t.Log("RMA-02 durable failure fact collection failed; class defaults to product")
			facts = rma02FailureFacts{}
		}
		raw, err := json.Marshal(facts)
		if err != nil {
			t.Errorf("marshal RMA-02 failure facts: %v", err)
			return
		}
		setRMA02FailureClass(t, string(raw))
	})
}

func collectRMA02FailureFacts(ctx context.Context, binary, socket, projectID string, sources []rma02Source) (rma02FailureFacts, error) {
	events, err := rma02EventHistory(ctx, binary, socket, projectID)
	if err != nil {
		return rma02FailureFacts{}, err
	}
	facts := rma02FailureFacts{Events: events}
	for _, source := range sources {
		fact, err := collectRMA02FailureTask(ctx, binary, socket, source.task.ID)
		if err != nil {
			return rma02FailureFacts{}, err
		}
		fact.Role = source.role
		facts.Sources = append(facts.Sources, fact)
	}
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		args := []string{"task", "list", "--socket", socket, "--project", projectID, "--limit", "500"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		args = append(args, "--output", "json")
		page, err := commandJSON[core.TaskPage](ctx, binary, args...)
		if err != nil {
			return rma02FailureFacts{}, err
		}
		for _, task := range page.Items {
			if task.Kind != core.TaskIntegration {
				continue
			}
			fact, err := collectRMA02FailureTask(ctx, binary, socket, task.ID)
			if err != nil {
				return rma02FailureFacts{}, err
			}
			facts.Integrations = append(facts.Integrations, fact)
		}
		if page.NextCursor == "" {
			return facts, nil
		}
		if page.NextCursor == cursor {
			return rma02FailureFacts{}, errors.New("RMA-02 Task cursor did not advance")
		}
		cursor = page.NextCursor
	}
	return rma02FailureFacts{}, errors.New("RMA-02 Task history exceeded 100 pages")
}

func collectRMA02FailureTask(ctx context.Context, binary, socket, taskID string) (rma02FailureTaskFact, error) {
	detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
	if err != nil {
		return rma02FailureTaskFact{}, err
	}
	fact := rma02FailureTaskFact{Task: detail.Task}
	if detail.CurrentRun != nil {
		fact.Run = *detail.CurrentRun
		return fact, nil
	}
	runID := detail.Task.HeadRunID
	if runID == "" {
		cursor := ""
		for pageNumber := 0; pageNumber < 100; pageNumber++ {
			args := []string{"run", "list", "--task", taskID, "--limit", "500"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			args = append(args, "--socket", socket, "--output", "json")
			page, err := commandJSON[core.RunPage](ctx, binary, args...)
			if err != nil {
				return rma02FailureTaskFact{}, err
			}
			for _, run := range page.Items {
				if core.IsRunTerminal(run.State) {
					runID = run.ID
				}
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				return rma02FailureTaskFact{}, errors.New("RMA-02 Run cursor did not advance")
			}
			cursor = page.NextCursor
			if pageNumber == 99 {
				return rma02FailureTaskFact{}, errors.New("RMA-02 Run history exceeded 100 pages")
			}
		}
	}
	if runID != "" {
		fact.Run, err = commandJSON[core.Run](ctx, binary, "run", "show", runID, "--socket", socket, "--output", "json")
	}
	return fact, err
}

func rma02EventHistory(ctx context.Context, binary, socket, projectID string) ([]core.Event, error) {
	var events []core.Event
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		args := []string{"events", "tail", "--socket", socket, "--project", projectID, "--limit", "100"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		args = append(args, "--output", "json")
		page, err := commandJSON[core.EventPage](ctx, binary, args...)
		if err != nil {
			return nil, err
		}
		events = append(events, page.Items...)
		if page.NextCursor == "" {
			return events, nil
		}
		if page.NextCursor == cursor {
			return nil, errors.New("RMA-02 Event cursor did not advance")
		}
		cursor = page.NextCursor
	}
	return nil, errors.New("RMA-02 Event history exceeded 100 pages")
}

func setRMA02FailureClass(t *testing.T, rawFacts string) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("E2E_RMA02_FAILURE_CLASS_FILE"))
	if !filepath.IsAbs(path) {
		t.Fatal("E2E_RMA02_FAILURE_CLASS_FILE must be an absolute path")
	}
	var facts rma02FailureFacts
	if err := json.Unmarshal([]byte(rawFacts), &facts); err != nil {
		t.Fatalf("decode RMA-02 durable failure facts: %v", err)
	}
	class := classifyRMA02Failure(facts)
	requireNoError(t, os.WriteFile(path, []byte(class+"\n"), 0o600))
}

func classifyRMA02Failure(facts rma02FailureFacts) string {
	all := append(append([]rma02FailureTaskFact{}, facts.Sources...), facts.Integrations...)
	for _, fact := range all {
		if fact.Run.RuntimeErrorCode == "PROVIDER_ERROR" {
			return "provider_environment"
		}
	}
	if len(facts.Integrations) != 0 {
		latest := facts.Integrations[len(facts.Integrations)-1]
		if latest.Run.ID == "" || rma02FailureTaskConverged(latest) {
			return "product"
		}
		return "task_spec"
	}
	if rma02ContinueMessagesComplete(facts) {
		for _, fact := range facts.Sources {
			if !rma02FailureTaskConverged(fact) {
				return "task_spec"
			}
		}
		return "product"
	}
	if rma02BarrierFactsComplete(facts) {
		return "product"
	}
	if len(facts.Sources) != 0 {
		return "task_spec"
	}
	return "product"
}

func rma02FailureTaskConverged(fact rma02FailureTaskFact) bool {
	return fact.Run.RequestedOutcome == string(core.OutcomeSubmit) || fact.Task.PendingAction == "capture" || fact.Task.Status == core.TaskSubmitted || fact.Task.Status == core.TaskCompleted
}

func rma02BarrierFactsComplete(facts rma02FailureFacts) bool {
	runs := map[string]string{}
	for _, fact := range facts.Sources {
		if fact.Role == "" || fact.Run.ID == "" || runs[fact.Role] != "" {
			return false
		}
		runs[fact.Role] = fact.Run.ID
	}
	if len(runs) != 4 {
		return false
	}
	ready, acknowledged := map[string]bool{}, false
	for _, event := range facts.Events {
		for role, runID := range runs {
			if event.RunID == runID && event.Kind == "task.progress" && strings.Contains(event.PayloadJSON, "RMA02-READY-"+role) {
				ready[role] = true
			}
		}
		acknowledged = acknowledged || event.RunID == runs["B"] && event.Kind == "message.acknowledged"
	}
	return len(ready) == 4 && acknowledged
}

func rma02ContinueMessagesComplete(facts rma02FailureFacts) bool {
	continued := map[string]bool{}
	for _, event := range facts.Events {
		for _, role := range []string{"A", "B", "C", "D"} {
			if event.Kind == "message.created" && event.ActorKind == "boss" && event.RequestID == "rma02-continue-"+role {
				continued[role] = true
			}
		}
	}
	return len(continued) == 4
}

func createRMA02SourceRepository(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	source := filepath.Join(root, "source")
	requireNoError(t, os.MkdirAll(source, 0o755))
	run(t, ctx, "git", "init", "--quiet", "--initial-branch", "main", source)
	git(t, ctx, source, "config", "user.name", "CoordPlane RMA-02 Fixture")
	git(t, ctx, source, "config", "user.email", "rma02-fixture@coordplane.local")
	testsupport.WriteFile(t, filepath.Join(source, "base.txt"), []byte("C0\n"), 0o644)
	fixture := `#!/bin/sh
set -eu
mode=${1:-}
role=${2:-}
task_id=${3:-}
run_id=${4:-}
coordlink=${RMA02_COORDLINK_BIN:-/usr/local/bin/coordlink}
if [ "$mode" != final ]; then
  [ -n "$task_id" ] && [ -n "$run_id" ]
  artifact_root=$HOME/.coordplane-rma02/$task_id/$run_id
  mkdir -p "$artifact_root"
fi
test "$(cat base.txt)" = C0
case "$mode" in
source)
  case "$role" in A|B|C|D) ;; *) exit 2 ;; esac
  test "$(cat agent-$role.txt)" = "agent-$role"
  for peer in A B C D; do
    [ "$peer" = "$role" ] || [ ! -e "agent-$peer.txt" ]
  done
	marker=source-$role
  ;;
integration)
  found=0
  for peer in A B C D; do
    if [ -e "agent-$peer.txt" ]; then
      test "$(cat agent-$peer.txt)" = "agent-$peer"
      found=$((found + 1))
    fi
  done
  [ "$found" -ge 2 ]
	marker=integration
  ;;
final)
  for peer in A B C D; do test "$(cat agent-$peer.txt)" = "agent-$peer"; done
  ;;
*) exit 2 ;;
esac
if [ "$mode" != final ]; then
	"$coordlink" task current --output json >"$artifact_root/task-current.json.tmp"
	mv "$artifact_root/task-current.json.tmp" "$artifact_root/task-current.json"
	"$coordlink" progress --summary "RMA02-FIXTURE-$task_id-$run_id" --request-id "rma02-fixture-$task_id-$run_id" --output json >/dev/null
	printf '%s\n' "$marker" >"$artifact_root/fixture.tmp"
	mv "$artifact_root/fixture.tmp" "$artifact_root/fixture"
	printf '0\n' >"$artifact_root/fixture-exit.tmp"
	mv "$artifact_root/fixture-exit.tmp" "$artifact_root/fixture-exit"
fi
`
	testsupport.WriteFile(t, filepath.Join(source, "fixture-test.sh"), []byte(fixture), 0o755)
	git(t, ctx, source, "add", "base.txt", "fixture-test.sh")
	git(t, ctx, source, "commit", "--quiet", "-m", "RMA-02 C0")
	return source, git(t, ctx, source, "rev-parse", "HEAD")
}

func writeRMA02Evidence(t *testing.T, path string, evidence rma02Evidence) {
	t.Helper()
	raw, err := json.MarshalIndent(evidence, "", "  ")
	requireNoError(t, err)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	requireNoError(t, err)
	completed := false
	defer func() {
		_ = file.Close()
		if !completed {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write RMA-02 evidence: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync RMA-02 evidence: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close RMA-02 evidence: %v", err)
	}
	completed = true
}

const rma02Instructions = `You are running RMA-02, the real four-Agent CoordPlane reliability gate.
Read the complete bootstrap and run /usr/local/bin/coordlink task current --output json before doing any work. Never infer completion from prose or process exit. The fixture writer records Task/Run-bound evidence below $HOME/.coordplane-rma02.

First, determine the Task Kind: read the literal kind field of the task in the coordlink task current JSON output (task.kind). If it is exactly "integration", execute ONLY the integration Steps below. If it is exactly "work", execute ONLY the work Steps below. NEVER apply the work steps to an integration Task: an integration Task has no marker progress, no RMA02-CONTINUE gate, and no role message duty — do not send RMA02-READY-style progress, do not poll or wait for RMA02-CONTINUE, and do not expect or send a peer message.

For a work Task, read rma_role, task_marker, file, content, fixture, and any peer IDs from the public Task description. Follow the numbered steps in exact order; each step gates the next.

Step 1 (marker progress, first and unique): immediately after coordlink task current, before any other operation, call coordlink progress with the summary set to the EXACT literal of task_marker — RMA02-READY-<role> — and a request ID containing the current Run ID. This marker is the only barrier evidence the harness accepts for your role. After it, do NOT send any other progress until Step 3 has been completed: any later progress overwrites the marker, invalidates your barrier evidence, and fails this round.

Step 2 (role message duty): role A must send RMA02-DIRECT to role B with coordlink message send --to-agent peer_agent_id --task peer_task_id. Role B must poll its inbox and acknowledge that Message exactly once. Roles C and D have no message duty.

Step 3 (CONTINUE gate): poll the inbox until RMA02-CONTINUE <role> arrives for your role, then acknowledge it exactly once. Until it has been received AND acknowledged, you MUST NOT write any file, run ./fixture-test.sh, git commit, run coordlink task submit, or send any other progress. Working or submitting before CONTINUE means your role has no barrier evidence and fails this round.

Step 4 (work sequence, only after Step 3): configure native Git in /workspace/project. Write exactly the requested file/content, run ./fixture-test.sh source ROLE TASK_ID RUN_ID using the current bootstrap IDs, verify $HOME/.coordplane-rma02/TASK_ID/RUN_ID/fixture, commit only agent-ROLE.txt, resolve HEAD with git rev-parse HEAD, and submit that exact SHA with coordlink task submit. Do not create child Tasks.

For an integration Task, use the source head and current canonical recorded in the bootstrap. Follow the numbered steps in exact order; each step gates the next.

Step 1 (merge source): merge the source head into the canonical-based workspace with native git merge --no-ff.

Step 2 (fixture evidence): run ./fixture-test.sh integration _ TASK_ID RUN_ID using the current bootstrap IDs, then verify $HOME/.coordplane-rma02/TASK_ID/RUN_ID/fixture records this Run.

Step 3 (commit): commit the merge result if it is not already committed.

Step 4 (submit exact head): resolve HEAD with git rev-parse HEAD and submit that exact SHA with coordlink task submit. Never create nested integration Tasks or accept a source Task yourself.

Discipline: outside the operations listed above — coordlink task current, progress, message send, inbox polling and ack, task submit, native git, and the fixture script — run NO other coordlink subcommand (in particular chat, agent, project, run, gc) and start NO additional container, session, or process. Keep the environment exactly as provisioned; unexpected side effects invalidate the round.
`
