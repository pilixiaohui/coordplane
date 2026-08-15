//go:build docker

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/transport"
	"coordplane/tests/testsupport"
)

func TestRT01RealDockerKeepsTwoAgentsWorkspacesHomesAndMountsPrivate(t *testing.T) {
	fixture := newP3DockerFixture(t)
	ctx := fixture.ctx
	agentA := fixture.addAgent(t, "Isolation A")
	agentB := fixture.addAgent(t, "Isolation B")
	project := fixture.addProject(t, agentA.ID)
	taskA := fixture.addTask(t, project.ID, agentA.ID, "hold until stopped A", 0)
	taskB := fixture.addTask(t, project.ID, agentB.ID, "hold until stopped B", 0)
	fixture.components.runtime.Start(ctx)
	runA := waitForActiveProviderRun(t, fixture, taskA.ID)
	runB := waitForActiveProviderRun(t, fixture, taskB.ID)
	if runA.ContainerID == runB.ContainerID || runA.WorkspacePath == runB.WorkspacePath ||
		runA.HomePath == runB.HomePath || runA.LaunchNonce == runB.LaunchNonce {
		t.Fatalf("Run isolation collapsed: A=%#v B=%#v", runA, runB)
	}
	stateA := requireRuntimeValue(fixture.executor.Inspect(ctx, runtimeRef(runA)))
	stateB := requireRuntimeValue(fixture.executor.Inspect(ctx, runtimeRef(runB)))
	assertIsolatedContainer(t, fixture, project, runA, runB, stateA)
	assertIsolatedContainer(t, fixture, project, runB, runA, stateB)
	waitForFile(t, fixture.ctx, filepath.Join(runA.WorkspacePath, "dirty-runtime.txt"))
	waitForFile(t, fixture.ctx, filepath.Join(runB.WorkspacePath, "dirty-runtime.txt"))
	assertRunIdentityIsPrivate(t, fixture, runA, runB)
	assertRunIdentityIsPrivate(t, fixture, runB, runA)
	assertContainerCannotAccessPeer(t, fixture, runA, runB)
	assertContainerCannotAccessPeer(t, fixture, runB, runA)
	canonicalBefore := gitOutput(t, "--git-dir="+project.ControlRepoPath, "rev-parse", project.CanonicalRef)
	privateA := mutatePrivateMainFromContainer(t, fixture, runA, canonicalBefore, "agent-a")
	privateB := mutatePrivateMainFromContainer(t, fixture, runB, canonicalBefore, "agent-b")
	if privateA == privateB {
		t.Fatalf("isolated private commits collapsed to %s", privateA)
	}
	assertGitObjectAbsent(t, fixture.ctx, project.ControlRepoPath, true, privateA)
	assertGitObjectAbsent(t, fixture.ctx, project.ControlRepoPath, true, privateB)
	assertGitObjectAbsent(t, fixture.ctx, runA.WorkspacePath, false, privateB)
	assertGitObjectAbsent(t, fixture.ctx, runB.WorkspacePath, false, privateA)
	if canonical := gitOutput(t, "--git-dir="+project.ControlRepoPath, "rev-parse", project.CanonicalRef); canonical != canonicalBefore {
		t.Fatalf("container ref mutation changed canonical: before=%s after=%s", canonicalBefore, canonical)
	}
	for _, task := range []core.Task{taskA, taskB} {
		if _, err := fixture.components.service.CancelTask(ctx, core.TaskActionInput{TaskID: task.ID, Reason: "isolation assertion complete", RequestID: "cancel-" + task.ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []core.Task{taskA, taskB} {
		run := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
			return run.State == core.RunCancelled && run.CleanupState == core.CleanupRemoved && task.Status == core.TaskCancelled
		})
		if _, err := os.Stat(filepath.Join(run.WorkspacePath, "dirty-runtime.txt")); err != nil {
			t.Fatalf("cancel removed/reset dirty workspace for %s: %v", task.ID, err)
		}
	}
	canonicalAfter := gitOutput(t, "--git-dir="+project.ControlRepoPath, "rev-parse", project.CanonicalRef)
	if canonicalAfter != canonicalBefore {
		t.Fatalf("private workspace writes changed canonical: before=%s after=%s", canonicalBefore, canonicalAfter)
	}
}

func TestRT02RealDockerLaunchPreservesAllMessageIDsWithoutOversizedArgv(t *testing.T) {
	fixture := newP3DockerFixture(t)
	requireNoError(t, os.WriteFile(fixture.instructions, []byte(strings.Repeat("I", 1<<20)), 0o600))
	agent := fixture.addAgent(t, "Bootstrap Boundary Agent")
	project := fixture.addProject(t, agent.ID)
	task := requireRuntimeValue(fixture.components.service.CreateTask(fixture.ctx, core.CreateTaskInput{ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork, Title: "verify all launch messages", Description: strings.Repeat("D", core.MaximumTaskDescriptionBytes), MaxRetries: 0, RequestID: "bootstrap-boundary-task"}))
	wantMessages := make(map[string]struct{}, core.MessagePageLimit+5)
	messageBody := strings.Repeat("消息", core.MaximumMessageBodyBytes/len("消息"))
	for index := 0; index < core.MessagePageLimit+5; index++ {
		message := requireRuntimeValue(fixture.components.service.SendBossMessage(fixture.ctx, core.BossMessageInput{ProjectID: project.ID, AgentID: agent.ID, TaskID: task.ID, Body: messageBody, Wake: false, RequestID: fmt.Sprintf("bootstrap-message-%02d", index)}))
		wantMessages[message.ID] = struct{}{}
	}
	fixture.components.runtime.Start(fixture.ctx)
	marker := filepath.Join(fixture.components.config.Runtime.WorkspaceRoot, project.ID, task.ID, "bootstrap-message-count")
	waitForFile(t, fixture.ctx, marker)
	rawCount, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(rawCount)) != fmt.Sprint(len(wantMessages)) {
		t.Fatalf("container-observed Message count = %q err=%v, want %d", rawCount, err, len(wantMessages))
	}
	active := waitForActiveProviderRun(t, fixture, task.ID)
	state := requireRuntimeValue(fixture.executor.Inspect(fixture.ctx, runtimeRef(active)))
	for _, argument := range state.CommandArgs {
		if len(argument) > 4096 || strings.Contains(argument, messageBody[:1024]) || strings.Contains(argument, task.Description[:1024]) {
			t.Fatalf("container argv contains oversized bootstrap input: %d bytes", len(argument))
		}
	}
	if !strings.Contains(strings.Join(state.CommandArgs, " "), adapter.ContainerBootstrapPath) {
		t.Fatalf("container argv does not reference mounted bootstrap: %#v", state.CommandArgs)
	}
	bootstrapRaw := requireRuntimeValue(os.ReadFile(filepath.Join(fixture.components.runtime.controlRoot, active.ID, "bootstrap")))
	bootstrap := string(bootstrapRaw)
	for id := range wantMessages {
		if strings.Count(bootstrap, "["+id+"]") != 1 {
			t.Fatalf("bootstrap Message ID %s count = %d", id, strings.Count(bootstrap, "["+id+"]"))
		}
	}
	if !strings.Contains(bootstrap, "[body omitted: aggregate limit]") {
		t.Fatal("bootstrap did not enforce the aggregate Message body budget")
	}
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.ID == active.ID && core.IsRunTerminal(run.State) &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskWaiting
	})
	snapshot := requireRuntimeValue(fixture.components.store.Snapshot(fixture.ctx, project.ID))
	seen := 0
	for _, message := range snapshot.Messages {
		if _, wanted := wantMessages[message.ID]; !wanted {
			continue
		}
		seen++
		if message.State != core.MessageAcknowledged || message.DeliveredRunID != terminal.ID || message.DeliveryCount != 1 {
			t.Fatalf("Message input did not converge through the real Run: %#v", message)
		}
	}
	if seen != len(wantMessages) {
		t.Fatalf("durable Message count = %d, want %d", seen, len(wantMessages))
	}
}

func TestRT03UnprovenResumeErrorFailsClosedWithoutFallback(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Resume Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "resume error", 1)
	fixture.components.runtime.Start(fixture.ctx)
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		persisted := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, task.ID))
		if persisted.Status == core.TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	snapshot := requireRuntimeValue(fixture.components.store.Snapshot(fixture.ctx, project.ID))
	var runs []core.Run
	for _, run := range snapshot.Runs {
		if run.TaskID == task.ID {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(left, right int) bool { return runs[left].CreatedAt < runs[right].CreatedAt })
	if len(runs) != 2 {
		t.Fatalf("resume error Runs = %#v, want exactly 2", runs)
	}
	first, resume := runs[0], runs[1]
	if resume.CleanupState != core.CleanupRemoved {
		resume = waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
			return run.ID == resume.ID && core.IsRunTerminal(run.State) &&
				run.CleanupState == core.CleanupRemoved && task.Status == core.TaskFailed
		})
	}
	if first.LaunchMode != "start" || first.NativeSessionID == "" || first.ResumedFromRunID != "" {
		t.Fatalf("first start Run = %#v", first)
	}
	if resume.LaunchMode != "resume" || resume.ResumedFromRunID != first.ID ||
		resume.ResumeNativeSessionID != first.NativeSessionID || resume.RuntimeErrorCode != "PROVIDER_ERROR" {
		t.Fatalf("failed resume Run = %#v", resume)
	}
	persisted, err := fixture.components.store.Task(fixture.ctx, task.ID)
	if err != nil || persisted.Status != core.TaskFailed || persisted.CurrentRunID != "" {
		t.Fatalf("failed resume Task = %#v err=%v", persisted, err)
	}
	events := requireRuntimeValue(fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID}))
	if countDaemonEvent(events, "run.resume_fallback") != 0 {
		t.Fatalf("unproven resume error created fallback Events: %#v", events)
	}
}

func TestRT04CancelStopsContainerButPreservesDirtyWorkspace(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Cancel Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped cancel", 0)
	fixture.components.runtime.Start(fixture.ctx)
	active := waitForActiveProviderRun(t, fixture, task.ID)
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	if _, err := fixture.components.service.CancelTask(fixture.ctx, core.TaskActionInput{TaskID: task.ID, Reason: "Boss cancellation", RequestID: "cancel-dirty-task"}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunCancelled && run.CleanupState == core.CleanupRemoved && task.Status == core.TaskCancelled
	})
	if terminal.StopOperationID == "" || terminal.TokenRevokedAt == "" {
		t.Fatalf("cancel intent was not durable before cleanup: %#v", terminal)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("cancel reset, cleaned, or deleted dirty workspace: %v", err)
	}
	if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("cancelled container still exists: %v", err)
	}
}

func TestRT04RunStopInterruptsOnlyTheRunAndPreservesDirtyWorkspace(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Run Stop Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped run only", 0)
	fixture.components.runtime.Start(fixture.ctx)
	active := waitForActiveProviderRun(t, fixture, task.ID)
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	if _, err := fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{RunID: active.ID, Reason: "Boss stopped this Run", RequestID: "stop-dirty-run"}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForInterruptedRemoved(t, fixture, task.ID, active.ID)
	persistedTask := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, task.ID))
	if terminal.StopOperationID == "" || terminal.TokenRevokedAt == "" ||
		persistedTask.Status == core.TaskCancelled || persistedTask.Status == core.TaskCompleted ||
		persistedTask.Status == core.TaskFailed ||
		!strings.HasPrefix(persistedTask.FailureReason, "RUN_INTERRUPTED") {
		t.Fatalf("run stop did not requeue task for resume: Run=%#v Task=%#v", terminal, persistedTask)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("run stop reset, cleaned, or deleted dirty workspace: %v", err)
	}
}

func TestRT04DeadlineTimesOutRunAndRequeuesTaskForResume(t *testing.T) {
	fixture := newP3DockerFixtureWithRunTimeout(t, 3*time.Second)
	agent := fixture.addAgent(t, "Timeout Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped timeout", 0)
	fixture.components.runtime.Start(fixture.ctx)
	active := waitForActiveProviderRun(t, fixture, task.ID)
	if active.DeadlineAt == "" {
		t.Fatalf("active Run has no durable deadline: %#v", active)
	}
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunTimedOut && run.CleanupState == core.CleanupRemoved && task.Status == core.TaskQueued
	})
	persistedTask := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, task.ID))
	if terminal.StopRequestedAt == "" || terminal.StopOperationID == "" || terminal.TokenRevokedAt == "" ||
		persistedTask.Status == core.TaskCompleted || persistedTask.Status == core.TaskFailed ||
		!strings.HasPrefix(persistedTask.FailureReason, "RUN_TIMED_OUT") {
		t.Fatalf("timeout did not requeue task for resume: Run=%#v Task=%#v", terminal, persistedTask)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("timeout reset, cleaned, or deleted dirty workspace: %v", err)
	}
	if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("timed-out container still exists: %v", err)
	}
}

func TestRT05ReconcileCreatedRunWithStopIntentNeverStartsCLI(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Reconcile Stop Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "reconcile stop must not start CLI", 0)
	created := claimPreparedRun(t, fixture, project.ID)
	stopped := requireRuntimeValue(fixture.components.service.RequestRuntimeStop(fixture.ctx, core.RunStopInput{RunID: created.ID, Reason: "operator stop before daemon restart", OperationID: "rt05-stop-operation", RequestID: "rt05-stop-request"}))
	if stopped.LaunchPhase != core.LaunchCreated || stopped.StopRequestedAt == "" {
		t.Fatalf("created Run stop intent = %#v", stopped)
	}
	requireNoError(t, fixture.components.runtime.Reconcile(fixture.ctx))
	terminal := requireRuntimeValue(fixture.components.store.Run(fixture.ctx, created.ID))
	persistedTask := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, task.ID))
	if terminal.State != core.RunInterrupted || terminal.LaunchPhase != core.LaunchCreated ||
		terminal.StartedAt != "" || terminal.NativeSessionID != "" || terminal.CleanupState != core.CleanupRemoved ||
		persistedTask.Status == core.TaskCompleted {
		t.Fatalf("reconciled stop started or completed work: Run=%#v Task=%#v", terminal, persistedTask)
	}
	events := requireRuntimeValue(fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID, RunID: created.ID}))
	if countDaemonEvent(events, "run.start_issued") != 0 || countDaemonEvent(events, "run.active") != 0 {
		t.Fatalf("reconcile crossed Start/active boundary despite stop intent: %#v", events)
	}
	if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("reconciled stopped container still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.components.config.DataDir, "run-control", terminal.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciled Run control directory survived cleanup: %v", err)
	}
}

func TestRT05ReconcileAdoptsRunningContainerAndRebuildsRunSocket(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Reconcile Active Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped restart adoption", 0)
	active := prepareCreatedRunAndActivate(t, fixture, project.ID, "rt05-active-start", "rt05-active-observed")
	restarted, adopted := restartRuntimeController(t, fixture, active.ID)
	if adopted.State != core.RunActive || adopted.ContainerID != active.ContainerID ||
		adopted.Generation != active.Generation || restarted.monitor(active.ID) == nil {
		t.Fatalf("restart did not adopt the durable active Run: before=%#v after=%#v", active, adopted)
	}
	socket := filepath.Join(fixture.components.config.DataDir, "run-control", active.ID, "api.sock")
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("restart did not rebuild the Run API socket: mode=%v err=%v", info, err)
	}
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(adopted))
	if err != nil || !state.Running || state.Ref.ContainerID != active.ContainerID {
		t.Fatalf("restart changed the live container: state=%#v err=%v", state, err)
	}
	if _, err := fixture.components.service.RequestRuntimeStop(fixture.ctx, core.RunStopInput{RunID: active.ID, Reason: "restart adoption assertion complete", OperationID: "rt05-active-stop", RequestID: "rt05-active-stop-request"}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForInterruptedRemoved(t, fixture, task.ID, active.ID)
	if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("adopted container survived terminal cleanup: %v", err)
	}
}

func TestRT05ReconcilePreservesOutcomeRecordedBeforeRestart(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Reconcile Outcome Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "outcome then hold across restart", 0)
	active := prepareCreatedRunAndActivate(t, fixture, project.ID, "rt05-outcome-start", "rt05-outcome-active")

	deadline := time.Now().Add(10 * time.Second)
	outcome := active
	for time.Now().Before(deadline) {
		next := requireRuntimeValue(fixture.components.store.Run(fixture.ctx, active.ID))
		outcome = next
		if outcome.RequestedOutcome == string(core.OutcomeWait) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if outcome.RequestedOutcome != string(core.OutcomeWait) {
		t.Fatalf("provider did not persist outcome before restart: %#v", outcome)
	}
	_, terminal := restartRuntimeController(t, fixture, outcome.ID)
	persistedTask := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, task.ID))
	if terminal.State != core.RunExited || terminal.RequestedOutcome != string(core.OutcomeWait) ||
		terminal.StopRequestedAt == "" || terminal.CleanupState != core.CleanupRemoved ||
		persistedTask.Status != core.TaskWaiting || persistedTask.WaitReason != "durable before restart" {
		t.Fatalf("restart lost or downgraded durable outcome: Run=%#v Task=%#v", terminal, persistedTask)
	}
}

func TestRT02MissingWorkspaceAfterStartIssuedFailsBeforeSecondContainer(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Workspace Fence Agent")
	project := fixture.addProject(t, agent.ID)
	fixture.addTask(t, project.ID, agent.ID, "workspace loss must fail loud", 1)
	created := claimPreparedRun(t, fixture, project.ID)
	started := requireRuntimeValue(fixture.components.service.RecordRunStartIssued(fixture.ctx, runtimeFactInput(created, runtimeRef(created), "rt02-start-issued")))
	terminal := requireRuntimeValue(fixture.components.service.RecordRuntimeRunTerminal(fixture.ctx, runtimeTerminalInput(started, core.RunTerminalInput{State: core.RunInterrupted, TerminalReason: "fault injected after start intent", RequestID: "rt02-first-terminal", OperationID: started.LaunchOperationID})))
	if err := fixture.components.runtime.cleanupRun(
		fixture.ctx,
		terminal.Run,
		runtimeRef(terminal.Run),
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	requireNoError(t, os.RemoveAll(started.WorkspacePath))

	var retry core.Claim
	var ok bool
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		retry, ok, err = fixture.components.service.ClaimNext(fixture.ctx, project.ID)
		requireNoError(t, err)
		if ok {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ok {
		t.Fatal("retry Run was not claimable after runtime backoff")
	}
	if err := fixture.components.runtime.launch(fixture.ctx, retry); err == nil {
		t.Fatal("retry unexpectedly recreated the missing writable workspace")
	}
	persisted := requireRuntimeValue(fixture.components.store.Run(fixture.ctx, retry.Run.ID))
	if persisted.State != core.RunFailed || persisted.LaunchPhase != core.LaunchIntent ||
		persisted.ContainerID != "" || persisted.RuntimeErrorCode != "WORKSPACE_PREPARE_FAILED" ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("workspace-loss retry crossed the create boundary: %#v", persisted)
	}
	if _, err := os.Stat(started.WorkspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace-loss retry recreated the workspace: %v", err)
	}
}

func TestCT04RealDockerExitZeroAndDoneTextCannotCompleteTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	executor := requireRuntimeValue(containerruntime.NewDockerExecutorFromEnvironment())
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}

	repositoryRoot := daemonRepositoryRoot()
	root := requireRuntimeValue(os.MkdirTemp("/tmp", "cp3-"))
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	coordlinkPath := filepath.Join(root, "coordlink")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", coordlinkPath, "./cmd/coordlink")
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build coordlink: %v\n%s", err, raw)
	}
	image := "coordplane-p3-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	imageRoot := filepath.Join(repositoryRoot, "internal", "daemon", "testdata", "claude-runtime")
	dockerConfig := filepath.Join(root, "docker-config")
	requireNoError(t, os.MkdirAll(dockerConfig, 0o700))
	buildImage := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, imageRoot)
	buildImage.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	if raw, err := buildImage.CombinedOutput(); err != nil {
		t.Fatalf("build deterministic one-shot image: %v\n%s", err, raw)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		remove := exec.CommandContext(cleanup, "docker", "image", "rm", "-f", image)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		_ = remove.Run()
	})

	configPath, rawConfig := noneNetworkConfig(t, root)
	requireNoError(t, os.WriteFile(configPath, rawConfig, 0o600))
	instructions := filepath.Join(root, "instructions.md")
	requireNoError(t, os.WriteFile(instructions, []byte("Work only on the assigned Task."), 0o600))
	source := createSourceRepository(t, root)
	components := requireRuntimeValue(buildComponents(ctx, configPath))
	components.runtime.coordlink = coordlinkPath
	t.Cleanup(func() { _ = components.Close() })
	agent := requireRuntimeValue(components.service.AddAgent(ctx, core.AddAgentInput{DisplayName: "P3 Docker Agent", AdapterID: "claude", Image: image, InstructionsFile: instructions, RequestID: "p3-agent"}))
	project := requireRuntimeValue(components.service.AddProject(ctx, core.AddProjectInput{Name: "p3-project", Source: source, SourceRef: "refs/heads/main", IntegrationAgentID: agent.ID, RequestID: "p3-project"}))
	task := requireRuntimeValue(components.service.CreateTask(ctx, core.CreateTaskInput{ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork, Title: "exit zero after saying done completed", MaxRetries: 0, RequestID: "p3-task"}))
	components.runtime.Start(ctx)

	var persistedTask core.Task
	var run core.Run
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		persistedTask := requireRuntimeValue(components.store.Task(ctx, task.ID))
		runs, readErr := components.store.Runs(ctx, core.RunFilter{TaskID: task.ID, Limit: 10})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(runs.Items) == 1 {
			run := requireRuntimeValue(components.store.Run(ctx, runs.Items[0].ID))
			if persistedTask.Status == core.TaskFailed && core.IsRunTerminal(run.State) && run.CleanupState == core.CleanupRemoved {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if persistedTask.Status != core.TaskFailed || !strings.HasPrefix(persistedTask.FailureReason, "NO_TASK_OUTCOME") {
		t.Fatalf("exit-zero Task projection = %#v\nRun = %#v", persistedTask, run)
	}
	if run.State != core.RunExited || run.ExitCode == nil || *run.ExitCode != 0 ||
		run.LaunchPhase != core.LaunchProcessObserved || run.NativeSessionID != "scripted-session-p3" ||
		run.CleanupState != core.CleanupRemoved || run.TokenRevokedAt == "" {
		exit := -1
		if run.ExitCode != nil {
			exit = *run.ExitCode
		}
		t.Fatalf("real Docker Run exit=%d value=%#v", exit, run)
	}
	logRaw := requireRuntimeValue(os.ReadFile(run.LogPath))
	if !strings.Contains(string(logRaw), "done completed") {
		t.Fatalf("Run log omitted provider output: %q", logRaw)
	}
	workspaceFact, err := components.runtime.workspaces.Verify(ctx, gitWorkspaceSpecForTest(task))
	if err != nil || workspaceFact.Path != run.WorkspacePath || workspaceFact.HeadSHA != task.BaseSHA {
		t.Fatalf("private workspace = %#v err=%v", workspaceFact, err)
	}
	if _, err := os.Stat(filepath.Join(components.config.DataDir, "run-control", run.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run control directory survived cleanup: %v", err)
	}
	if _, err := executor.Inspect(ctx, runtimeRef(run)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("container survived cleanup: %v", err)
	}
}

var daemonRepositoryRoot = testsupport.RepositoryRoot

func gitWorkspaceSpecForTest(task core.Task) gitrepo.WorkspaceSpec {
	return gitrepo.WorkspaceSpec{ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA}
}

type p3DockerFixture struct {
	ctx          context.Context
	root         string
	image        string
	adapterID    string
	instructions string
	source       string
	components   *components
	executor     *containerruntime.DockerExecutor
}

type p3DaemonProcess struct {
	command *exec.Cmd
	done    chan error
	log     *os.File
	logPath string
}

func startP3DaemonProcess(t *testing.T, binary, configPath, socket, logPath string) *p3DaemonProcess {
	return startP3DaemonProcessWithEnv(t, binary, configPath, socket, logPath, nil)
}

func startP3DaemonProcessWithEnv(t *testing.T, binary, configPath, socket, logPath string, environment []string) *p3DaemonProcess {
	t.Helper()
	logFile := requireRuntimeValue(os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600))
	command := exec.Command(binary, "serve", "--config", configPath)
	command.Env = append(os.Environ(), environment...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	process := &p3DaemonProcess{command: command, done: make(chan error, 1), log: logFile, logPath: logPath}
	go func() { process.done <- command.Wait() }()
	client := requireRuntimeValue(transport.NewUnixClient(socket))
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus core.Status
	var lastStatusErr error
	for time.Now().Before(deadline) {
		var status core.Status
		lastStatusErr = client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &status)
		lastStatus = status
		if lastStatusErr == nil && status.DaemonReady {
			return process
		}
		select {
		case err := <-process.done:
			process.command = nil
			_ = process.log.Close()
			t.Fatalf("daemon exited before ready: %v\n%s", err, readP3DaemonLog(process.logPath))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	killP3DaemonProcess(t, process)
	t.Fatalf("daemon readiness timeout: status=%#v err=%v\n%s", lastStatus, lastStatusErr, readP3DaemonLog(logPath))
	return nil
}

func killP3DaemonProcess(t *testing.T, process *p3DaemonProcess) {
	t.Helper()
	if process == nil || process.command == nil {
		return
	}
	if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	select {
	case err := <-process.done:
		if err == nil {
			t.Fatal("SIGKILLed daemon exited successfully")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGKILLed daemon did not exit")
	}
	process.command = nil
	_ = process.log.Close()
}

func stopP3DaemonProcess(t *testing.T, process *p3DaemonProcess) {
	t.Helper()
	if process == nil || process.command == nil {
		return
	}
	signalP3DaemonProcess(t, process)
	waitP3DaemonProcess(t, process)
}

func signalP3DaemonProcess(t *testing.T, process *p3DaemonProcess) {
	t.Helper()
	if process == nil || process.command == nil {
		return
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
}

func waitP3DaemonProcess(t *testing.T, process *p3DaemonProcess) {
	t.Helper()
	if process == nil || process.command == nil {
		return
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("daemon shutdown: %v\n%s", err, readP3DaemonLog(process.logPath))
		}
	case <-time.After(35 * time.Second):
		_ = process.command.Process.Kill()
		t.Fatalf("daemon shutdown timeout:\n%s", readP3DaemonLog(process.logPath))
	}
	process.command = nil
	_ = process.log.Close()
}

func readP3DaemonLog(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}

func waitForOperatorRun(t *testing.T, client *transport.Client, taskID string, ready func(core.Run) bool) core.Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var page core.RunPage
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/runs?task_id="+taskID+"&limit=10", nil, &page); err == nil {
			for _, summary := range page.Items {
				var run core.Run
				if err := client.JSON(context.Background(), http.MethodGet, "/v1/runs/"+summary.ID, nil, &run); err == nil && ready(run) {
					return run
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	var page core.RunPage
	err := client.JSON(context.Background(), http.MethodGet, "/v1/runs?task_id="+taskID+"&limit=10", nil, &page)
	t.Fatalf("operator Run did not reach expected state: page=%#v err=%v", page, err)
	return core.Run{}
}

func noneNetworkConfig(t *testing.T, root string) (string, []byte) {
	t.Helper()
	return noneNetworkConfigWithProviderEnv(t, root, nil)
}

func noneNetworkConfigWithProviderEnv(t *testing.T, root string, providerEnv []string) (string, []byte) {
	t.Helper()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "coordplane.yaml")
	raw := testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: filepath.Join(dataDir, "operator.sock"), MaxParallelRuns: 4, CompletedWorkspace: "24h", TerminalTaskRef: "168h", RunLog: "168h", DockerNetwork: "coordplane", DefaultImage: "coordplane-agent:latest", ProviderEnv: providerEnv})
	requireNoError(t, os.WriteFile(configPath, raw, 0o600))
	return configPath, []byte(strings.ReplaceAll(string(raw), "  docker_network: coordplane\n", "  docker_network: none\n"))
}

func newP3DockerFixture(t *testing.T) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithRunTimeout(t, "claude", 0, 90*time.Second)
}

func newP3DockerFixtureWithRunTimeout(t *testing.T, runTimeout time.Duration) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithRunTimeout(t, "claude", runTimeout, 90*time.Second)
}

func newCodexDockerFixture(t *testing.T) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithRunTimeout(t, "codex", 0, 4*time.Minute)
}

func newP3DockerFixtureWithProviderEnv(t *testing.T, providerEnv []string) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithProviderEnvAndRunTimeout(t, "claude", 0, 90*time.Second, providerEnv)
}

func newCodexDockerFixtureWithProviderEnv(t *testing.T, providerEnv []string) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithProviderEnvAndRunTimeout(t, "codex", 0, 4*time.Minute, providerEnv)
}

func newP3DockerFixtureAdapterWithRunTimeout(t *testing.T, adapterName string, runTimeout, ctxTimeout time.Duration) *p3DockerFixture {
	return newP3DockerFixtureAdapterWithProviderEnvAndRunTimeout(t, adapterName, runTimeout, ctxTimeout, nil)
}

func newP3DockerFixtureAdapterWithProviderEnvAndRunTimeout(t *testing.T, adapterName string, runTimeout, ctxTimeout time.Duration, providerEnv []string) *p3DockerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	t.Cleanup(cancel)
	executor := requireRuntimeValue(containerruntime.NewDockerExecutorFromEnvironment())
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}
	root := requireRuntimeValue(os.MkdirTemp("/tmp", "cp3-"))
	repositoryRoot := daemonRepositoryRoot()
	coordlinkPath := filepath.Join(root, "coordlink")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", coordlinkPath, "./cmd/coordlink")
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build coordlink: %v\n%s", err, raw)
	}
	image := "coordplane-p3-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	dockerConfig := filepath.Join(root, "docker-config")
	requireNoError(t, os.MkdirAll(dockerConfig, 0o700))
	imageRoot := filepath.Join(repositoryRoot, "internal", "daemon", "testdata", adapterName+"-runtime")
	buildImage := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, imageRoot)
	buildImage.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	if raw, err := buildImage.CombinedOutput(); err != nil {
		t.Fatalf("build deterministic one-shot image: %v\n%s", err, raw)
	}
	configPath, rawConfig := noneNetworkConfigWithProviderEnv(t, root, providerEnv)
	if runTimeout > 0 {
		rawConfig = []byte(strings.Replace(string(rawConfig), "  default_image:", "  run_timeout: "+runTimeout.String()+"\n  default_image:", 1))
	}
	requireNoError(t, os.WriteFile(configPath, rawConfig, 0o600))
	instructions := filepath.Join(root, "instructions.md")
	requireNoError(t, os.WriteFile(instructions, []byte("Work only on the assigned Task."), 0o600))
	source := createSourceRepository(t, root)
	assembled := requireRuntimeValue(buildComponents(ctx, configPath))
	assembled.runtime.coordlink = coordlinkPath
	fixture := &p3DockerFixture{ctx: ctx, root: root, image: image, adapterID: adapterName, instructions: instructions, source: source, components: assembled, executor: executor}
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		_ = assembled.runtime.Shutdown(shutdown)
		_ = assembled.Close()
		remove := exec.CommandContext(shutdown, "docker", "image", "rm", "-f", image)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		_ = remove.Run()
		_ = os.RemoveAll(root)
	})
	return fixture
}

func prepareCreatedRunForReconcile(t *testing.T, fixture *p3DockerFixture, claim core.Claim) core.Run {
	t.Helper()
	launch := requireRuntimeValue(fixture.components.service.RuntimeLaunchContext(fixture.ctx, claim.Run.ID))
	instructions, _, err := readInstructions(launch.Agent)
	requireNoError(t, err)
	workspaceSpec := requireRuntimeValue(gitWorkspaceSpec(launch.Task))
	workspacePath := requireRuntimeValue(fixture.components.runtime.workspaces.Path(launch.Project.ID, launch.Task.ID))
	homePath := filepath.Join(fixture.components.config.Runtime.AgentHomeRoot, launch.Agent.ID)
	logPath := filepath.Join(fixture.components.config.Runtime.LogRoot, launch.Run.ID, "run.log")
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, launch.Run.ID)
	prepared := requireRuntimeValue(fixture.components.service.BeginRunLaunch(fixture.ctx, core.RunLaunchInput{
		RunID: launch.Run.ID, Generation: launch.Run.Generation, LaunchNonce: "rt05-created-nonce",
		WorkspacePath: workspacePath, HomePath: homePath, LogPath: logPath,
		InstructionsHash: launch.InstructionsHash, LaunchMode: "start", CleanupOperationID: "rt05-cleanup-operation",
		ConfigFingerprint: launch.ConfigFingerprint,
		RequestID:         "rt05-prepare",
	}))
	requireNoError(t, fixture.components.runtime.prepareWorkspace(fixture.ctx, prepared, workspaceSpec))
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{homePath, 0o2770}, {filepath.Dir(logPath), 0o700}, {controlPath, 0o750},
	} {
		requireNoError(t, ensureRuntimeDirectory(directory.path, directory.mode))
	}
	bootstrap := buildBootstrap(launch, prepared, instructions, workspacePath, workspaceSpec)
	requireNoError(t, writeRunControlMarker(controlPath, prepared))
	requireNoError(t, writeRuntimeFile(filepath.Join(controlPath, "token"), []byte(claim.Token+"\n"), 0o440))
	requireNoError(t, writeRuntimeFile(filepath.Join(controlPath, "bootstrap"), []byte(bootstrap), 0o440))
	prepareState := &runtimePrepareState{controller: fixture.components.runtime, controlPath: controlPath, instructions: instructions}
	requireNoError(t, writeRuntimeInstructions(prepareState))
	requireNoError(t, writeRuntimeSecrets(prepareState))
	requireNoError(t, writeRuntimeLaunch(prepareState))
	entry, ok := fixture.components.runtime.adapters.Lookup(prepared.AdapterID)
	if !ok {
		t.Fatalf("adapter %q is not registered", prepared.AdapterID)
	}
	command := requireRuntimeValue(entry.BuildStartCommand(adapter.LaunchSpec{BootstrapPath: adapter.ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}))
	containerSpec := requireRuntimeValue(fixture.components.runtime.containerSpec(prepared, launch.Task.Kind, command, controlPath))
	ref := requireRuntimeValue(fixture.executor.Create(fixture.ctx, containerSpec))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = fixture.executor.Stop(cleanup, ref, 0)
		_, _ = fixture.executor.Remove(cleanup, ref)
	})
	created := requireRuntimeValue(fixture.components.service.RecordContainerCreated(fixture.ctx, runtimeFactInput(prepared, ref, "rt05-created")))
	return created
}

func claimPreparedRun(t *testing.T, fixture *p3DockerFixture, projectID string) core.Run {
	t.Helper()
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, projectID)
	if err != nil || !ok {
		t.Fatalf("claim created reconcile fixture: ok=%v err=%v", ok, err)
	}
	return prepareCreatedRunForReconcile(t, fixture, claim)
}

// prepareCreatedRunAndActivate drives a freshly claimed Run through the manual
// reconcile path (create, attach, start, observe) to the active state, exactly
// as the fault-injection tests did inline. The caller must already have a task
// in queue; the returned Run is live in Docker.
func prepareCreatedRunAndActivate(t *testing.T, fixture *p3DockerFixture, projectID, startPhase, activePhase string) core.Run {
	t.Helper()
	created := claimPreparedRun(t, fixture, projectID)
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, created.ID)
	control := requireRuntimeValue(fixture.components.runtime.openRunControl(created, controlPath))
	fixture.components.runtime.registerControl(created.ID, control)
	if _, err := fixture.executor.Attach(fixture.ctx, runtimeRef(created)); err != nil {
		t.Fatal(err)
	}
	started := requireRuntimeValue(fixture.components.service.RecordRunStartIssued(fixture.ctx, runtimeFactInput(created, runtimeRef(created), startPhase)))
	startedRef := requireRuntimeValue(fixture.executor.Start(fixture.ctx, runtimeRef(started)))
	active := requireRuntimeValue(fixture.components.service.ObserveProcessAndActivateRun(fixture.ctx, runtimeFactInput(started, startedRef, activePhase)))
	return active
}

// restartRuntimeController closes the live Run control and reconciles a fresh
// runtime controller over the same durable state, simulating a daemon restart.
// It returns the fresh controller and the persisted Run after adoption.
func restartRuntimeController(t *testing.T, fixture *p3DockerFixture, runID string) (*runtimeController, core.Run) {
	t.Helper()
	fixture.components.runtime.mu.Lock()
	oldControl := fixture.components.runtime.controls[runID]
	fixture.components.runtime.mu.Unlock()
	if oldControl == nil {
		t.Fatal("fault fixture has no original Run control")
	}
	requireNoError(t, fixture.components.runtime.closeControl(runID, oldControl))
	restarted := newRuntimeController(
		fixture.components.config,
		fixture.components.service,
		fixture.executor,
		adapter.Production(),
		fixture.components.runtime.workspaces,
		fixture.components.runtime.coordlink,
	)
	t.Cleanup(func() { _ = restarted.Close() })
	requireNoError(t, restarted.Reconcile(fixture.ctx))
	adopted := requireRuntimeValue(fixture.components.store.Run(fixture.ctx, runID))
	return restarted, adopted
}

func (f *p3DockerFixture) addAgent(t *testing.T, name string) core.Agent {
	t.Helper()
	agent := requireRuntimeValue(f.components.service.AddAgent(f.ctx, core.AddAgentInput{DisplayName: name, AdapterID: f.adapterID, Image: f.image, InstructionsFile: f.instructions, RequestID: "agent-" + strings.ReplaceAll(name, " ", "-")}))
	return agent
}

func (f *p3DockerFixture) addProject(t *testing.T, integrationAgentID string) core.Project {
	t.Helper()
	project := requireRuntimeValue(f.components.service.AddProject(f.ctx, core.AddProjectInput{Name: "project-" + integrationAgentID, Source: f.source, SourceRef: "refs/heads/main", IntegrationAgentID: integrationAgentID, RequestID: "project-" + integrationAgentID}))
	return project
}

func (f *p3DockerFixture) addTask(t *testing.T, projectID, agentID, title string, retries int) core.Task {
	t.Helper()
	task := requireRuntimeValue(f.components.service.CreateTask(f.ctx, core.CreateTaskInput{ProjectID: projectID, AssigneeAgentID: agentID, Kind: core.TaskWork, Title: title, MaxRetries: retries, RequestID: "task-" + strings.ReplaceAll(title, " ", "-")}))
	return task
}

func waitForRun(
	t *testing.T,
	fixture *p3DockerFixture,
	taskID string,
	predicate func(core.Run, core.Task) bool,
) core.Run {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	var latest core.Run
	for time.Now().Before(deadline) {
		task := requireRuntimeValue(fixture.components.store.Task(fixture.ctx, taskID))
		runs := requireRuntimeValue(fixture.components.store.Runs(fixture.ctx, core.RunFilter{TaskID: taskID, Limit: 20}))
		if len(runs.Items) > 0 {
			latest := requireRuntimeValue(fixture.components.store.Run(fixture.ctx, runs.Items[len(runs.Items)-1].ID))
			if predicate(latest, task) {
				return latest
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Run %s did not converge; latest=%#v", taskID, latest)
	return core.Run{}
}

func waitForFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat runtime fixture %s: %v", path, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for runtime fixture %s: %v", path, ctx.Err())
		case <-timer.C:
			t.Fatalf("provider did not create runtime fixture %s", path)
		case <-ticker.C:
		}
	}
}

func assertIsolatedContainer(
	t *testing.T,
	fixture *p3DockerFixture,
	project core.Project,
	run, peer core.Run,
	state containerruntime.LiveState,
) {
	t.Helper()
	if !state.Running || state.Privileged || state.AutoRemove || state.RestartPolicy != "no" ||
		state.User == "" || strings.HasPrefix(state.User, "0:") || len(state.CapAdd) != 0 ||
		state.PublishedPorts != 0 || !containsDaemonValue(state.CapDrop, "ALL") ||
		!containsDaemonValue(state.SecurityOpt, "no-new-privileges") {
		t.Fatalf("container security state = %#v", state)
	}
	if options := state.Tmpfs["/tmp"]; !strings.Contains(options, "size=67108864") {
		t.Fatalf("Run %s tmpfs quota = %q, want 64 MiB", run.ID, options)
	}
	destinations := make(map[string]string)
	readWrite := make(map[string]bool)
	for _, mount := range state.Mounts {
		destinations[mount.Destination] = mount.Source
		readWrite[mount.Destination] = mount.ReadWrite
		if mount.Source == peer.WorkspacePath || mount.Source == peer.HomePath || mount.Source == peer.LogPath ||
			mount.Source == peer.ContainerID || mount.Source == fixture.components.config.OperatorSocket ||
			mount.Source == filepath.Join(fixture.components.config.DataDir, "coordplane.db") ||
			mount.Source == project.ControlRepoPath || mount.Source == dockerSocketPathForTest() {
			t.Fatalf("Run %s sees forbidden peer/control mount %#v", run.ID, mount)
		}
	}
	if destinations["/workspace/project"] != run.WorkspacePath ||
		destinations["/home/agent"] != run.HomePath ||
		destinations["/run/coordplane"] != filepath.Join(fixture.components.config.DataDir, "run-control", run.ID) ||
		destinations["/usr/local/bin/coordlink"] == "" || len(destinations) != 4 ||
		!readWrite["/workspace/project"] || !readWrite["/home/agent"] || !readWrite["/run/coordplane"] ||
		readWrite["/usr/local/bin/coordlink"] {
		t.Fatalf("Run %s mounts = %#v", run.ID, destinations)
	}
}

func assertRunIdentityIsPrivate(t *testing.T, fixture *p3DockerFixture, run, peer core.Run) {
	t.Helper()
	gitInfo, err := os.Stat(filepath.Join(run.WorkspacePath, ".git"))
	if err != nil || !gitInfo.IsDir() {
		t.Fatalf("Run %s private .git: info=%v err=%v", run.ID, gitInfo, err)
	}
	peerGitInfo, err := os.Stat(filepath.Join(peer.WorkspacePath, ".git"))
	if err != nil || !peerGitInfo.IsDir() || os.SameFile(gitInfo, peerGitInfo) {
		t.Fatalf("Run %s and peer share .git: info=%v err=%v", run.ID, peerGitInfo, err)
	}
	controlPath := filepath.Join(fixture.components.config.DataDir, "run-control", run.ID)
	peerControlPath := filepath.Join(fixture.components.config.DataDir, "run-control", peer.ID)
	if controlPath == peerControlPath {
		t.Fatal("per-Run control directories collapsed")
	}
	for _, socket := range []string{filepath.Join(controlPath, "api.sock"), filepath.Join(peerControlPath, "api.sock")} {
		if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("Run socket %s: info=%v err=%v", socket, info, err)
		}
	}
	tokenPath := filepath.Join(controlPath, "token")
	peerTokenPath := filepath.Join(peerControlPath, "token")
	token := requireRuntimeValue(os.ReadFile(tokenPath))
	peerToken := requireRuntimeValue(os.ReadFile(peerTokenPath))
	if strings.TrimSpace(string(token)) == "" || strings.TrimSpace(string(token)) == strings.TrimSpace(string(peerToken)) {
		t.Fatalf("Run %s token is empty or shared with peer", run.ID)
	}
	inside := dockerExecOutput(t, fixture, run, "cat", "/run/coordplane/token")
	if strings.TrimSpace(inside) != strings.TrimSpace(string(token)) {
		t.Fatalf("Run %s container token mismatch", run.ID)
	}
}

func assertContainerCannotAccessPeer(t *testing.T, fixture *p3DockerFixture, run, peer core.Run) {
	t.Helper()
	peerWorkspaceCanary := filepath.Join(peer.WorkspacePath, ".rt01-peer-workspace")
	peerHomeCanary := filepath.Join(peer.HomePath, ".rt01-peer-home")
	for _, canary := range []string{peerWorkspaceCanary, peerHomeCanary} {
		requireNoError(t, os.WriteFile(canary, []byte("peer-only"), 0o660))
	}
	peerControl := filepath.Join(fixture.components.config.DataDir, "run-control", peer.ID)
	forbidden := []string{
		peerWorkspaceCanary,
		peerHomeCanary,
		filepath.Join(peerControl, "token"),
		filepath.Join(peerControl, "api.sock"),
		fixture.components.config.OperatorSocket,
		filepath.Join(fixture.components.config.DataDir, "coordplane.db"),
		dockerSocketPathForTest(),
	}
	args := []string{"/bin/sh", "-c", `
for path do
  if cat "$path" >/dev/null 2>&1; then
    echo "readable forbidden path: $path" >&2
    exit 41
  fi
  if printf x >>"$path" 2>/dev/null; then
    echo "writable forbidden path: $path" >&2
    exit 42
  fi
done
touch /workspace/project/.rt01-own-workspace /home/agent/.rt01-own-home
`, "rt01-isolation"}
	args = append(args, forbidden...)
	_ = dockerExecOutput(t, fixture, run, args...)
}

func dockerExecOutput(t *testing.T, fixture *p3DockerFixture, run core.Run, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"exec", run.ContainerID}, args...)
	command := exec.CommandContext(fixture.ctx, "docker", commandArgs...)
	command.Env = os.Environ()
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec Run %s: %v\n%s", run.ID, err, raw)
	}
	return string(raw)
}

func mutatePrivateMainFromContainer(
	t *testing.T,
	fixture *p3DockerFixture,
	run core.Run,
	canonicalSHA string,
	label string,
) string {
	t.Helper()
	requireNoError(t, os.WriteFile(filepath.Join(run.WorkspacePath, label+"-private.txt"), []byte(label+"\n"), 0o660))
	gitIn(t, run.WorkspacePath, "config", "user.email", label+"@coordplane.local")
	gitIn(t, run.WorkspacePath, "config", "user.name", label)
	gitIn(t, run.WorkspacePath, "add", label+"-private.txt")
	gitIn(t, run.WorkspacePath, "commit", "-m", label+" private commit")
	privateSHA := strings.TrimSpace(gitIn(t, run.WorkspacePath, "rev-parse", "HEAD"))
	gitIn(t, run.WorkspacePath, "update-ref", "refs/heads/main", canonicalSHA)

	dockerExecOutput(t, fixture, run, "/bin/sh", "-eu", "-c", `printf '%s\n' "$1" > /workspace/project/.git/refs/heads/main`, "rt01-ref", privateSHA)
	if actual := strings.TrimSpace(gitIn(t, run.WorkspacePath, "rev-parse", "refs/heads/main")); actual != privateSHA {
		t.Fatalf("container private main = %s, want %s", actual, privateSHA)
	}
	return privateSHA
}

func assertGitObjectAbsent(t *testing.T, ctx context.Context, repository string, bare bool, sha string) {
	t.Helper()
	args := []string{"-C", repository, "cat-file", "-e", sha + "^{commit}"}
	if bare {
		args = []string{"--git-dir=" + repository, "cat-file", "-e", sha + "^{commit}"}
	}
	command := exec.CommandContext(ctx, "git", args...)
	if raw, err := command.CombinedOutput(); err == nil {
		t.Fatalf("Git repository %s unexpectedly contains private object %s: %s", repository, sha, raw)
	}
}

func dockerSocketPathForTest() string {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

func containsDaemonValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countDaemonEvent(events []core.Event, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
