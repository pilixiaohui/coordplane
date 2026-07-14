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
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
	"coordplane/internal/transport"
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
	runA := waitForRun(t, fixture, taskA.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	runB := waitForRun(t, fixture, taskB.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	if runA.ContainerID == runB.ContainerID || runA.WorkspacePath == runB.WorkspacePath ||
		runA.HomePath == runB.HomePath || runA.LaunchNonce == runB.LaunchNonce {
		t.Fatalf("Run isolation collapsed: A=%#v B=%#v", runA, runB)
	}
	stateA, err := fixture.executor.Inspect(ctx, runtimeRef(runA))
	if err != nil {
		t.Fatal(err)
	}
	stateB, err := fixture.executor.Inspect(ctx, runtimeRef(runB))
	if err != nil {
		t.Fatal(err)
	}
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
		if _, err := fixture.components.service.CancelTask(ctx, core.TaskActionInput{
			TaskID: task.ID, Reason: "isolation assertion complete", RequestID: "cancel-" + task.ID,
		}); err != nil {
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
	if err := os.WriteFile(fixture.instructions, []byte(strings.Repeat("I", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := fixture.addAgent(t, "Bootstrap Boundary Agent")
	project := fixture.addProject(t, agent.ID)
	task, err := fixture.components.service.CreateTask(fixture.ctx, core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "verify all launch messages", Description: strings.Repeat("D", core.MaximumTaskDescriptionBytes),
		MaxRetries: 0, RequestID: "bootstrap-boundary-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := make(map[string]struct{}, core.MessagePageLimit+5)
	messageBody := strings.Repeat("消息", core.MaximumMessageBodyBytes/len("消息"))
	for index := 0; index < core.MessagePageLimit+5; index++ {
		message, err := fixture.components.service.SendBossMessage(fixture.ctx, core.BossMessageInput{
			ProjectID: project.ID, AgentID: agent.ID, TaskID: task.ID,
			Body: messageBody, Wake: false, RequestID: fmt.Sprintf("bootstrap-message-%02d", index),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantMessages[message.ID] = struct{}{}
	}
	fixture.components.runtime.Start(fixture.ctx)
	marker := filepath.Join(fixture.components.config.Runtime.WorkspaceRoot, project.ID, task.ID, "bootstrap-message-count")
	waitForFile(t, fixture.ctx, marker)
	rawCount, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(rawCount)) != fmt.Sprint(len(wantMessages)) {
		t.Fatalf("container-observed Message count = %q err=%v, want %d", rawCount, err, len(wantMessages))
	}
	active := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(active))
	if err != nil {
		t.Fatal(err)
	}
	for _, argument := range state.CommandArgs {
		if len(argument) > 4096 || strings.Contains(argument, messageBody[:1024]) || strings.Contains(argument, task.Description[:1024]) {
			t.Fatalf("container argv contains oversized bootstrap input: %d bytes", len(argument))
		}
	}
	if !strings.Contains(strings.Join(state.CommandArgs, " "), adapter.ContainerBootstrapPath) {
		t.Fatalf("container argv does not reference mounted bootstrap: %#v", state.CommandArgs)
	}
	bootstrapRaw, err := os.ReadFile(filepath.Join(fixture.components.runtime.controlRoot, active.ID, "bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
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
	snapshot, err := fixture.components.store.Snapshot(fixture.ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
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

func TestRT03ResumeUnavailableCreatesMarkedFreshFallbackRun(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Resume Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "resume fallback", 4)
	fixture.components.runtime.Start(fixture.ctx)
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		persisted, err := fixture.components.store.Task(fixture.ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Status == core.TaskWaiting {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	snapshot, err := fixture.components.store.Snapshot(fixture.ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var runs []core.Run
	for _, run := range snapshot.Runs {
		if run.TaskID == task.ID {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(left, right int) bool { return runs[left].CreatedAt < runs[right].CreatedAt })
	if len(runs) != 3 {
		t.Fatalf("resume fallback Runs = %#v, want exactly 3", runs)
	}
	first, resume, fallback := runs[0], runs[1], runs[2]
	if fallback.CleanupState != core.CleanupRemoved {
		fallback = waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
			return run.ID == fallback.ID && core.IsRunTerminal(run.State) &&
				run.CleanupState == core.CleanupRemoved && task.Status == core.TaskWaiting
		})
	}
	if first.LaunchMode != "start" || first.NativeSessionID == "" || first.ResumedFromRunID != "" {
		t.Fatalf("first start Run = %#v", first)
	}
	if resume.LaunchMode != "resume" || resume.ResumedFromRunID != first.ID ||
		resume.ResumeNativeSessionID != first.NativeSessionID || resume.RuntimeErrorCode != string(core.CodeResumeUnavailable) {
		t.Fatalf("failed resume Run = %#v", resume)
	}
	if fallback.LaunchMode != "start" || fallback.ResumedFromRunID != resume.ID ||
		fallback.ResumeNativeSessionID != "" || fallback.RequestedOutcome != string(core.OutcomeWait) ||
		fallback.CleanupState != core.CleanupRemoved {
		t.Fatalf("fresh fallback Run = %#v", fallback)
	}
	persisted, err := fixture.components.store.Task(fixture.ctx, task.ID)
	if err != nil || persisted.Status != core.TaskWaiting || persisted.CurrentRunID != "" {
		t.Fatalf("fallback Task = %#v err=%v", persisted, err)
	}
	events, err := fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID, RunID: fallback.ID})
	if err != nil {
		t.Fatal(err)
	}
	if countDaemonEvent(events, "run.resume_fallback") != 1 {
		t.Fatalf("fallback Events = %#v", events)
	}
}

func TestRT04CancelStopsContainerButPreservesDirtyWorkspace(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Cancel Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped cancel", 0)
	fixture.components.runtime.Start(fixture.ctx)
	active := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	if _, err := fixture.components.service.CancelTask(fixture.ctx, core.TaskActionInput{
		TaskID: task.ID, Reason: "Boss cancellation", RequestID: "cancel-dirty-task",
	}); err != nil {
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
	active := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	if _, err := fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{
		RunID: active.ID, Reason: "Boss stopped this Run", RequestID: "stop-dirty-run",
	}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.ID == active.ID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskFailed
	})
	persistedTask, err := fixture.components.store.Task(fixture.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.StopOperationID == "" || terminal.TokenRevokedAt == "" ||
		persistedTask.Status == core.TaskCancelled || persistedTask.Status == core.TaskCompleted ||
		!strings.HasPrefix(persistedTask.FailureReason, "RUN_INTERRUPTED") {
		t.Fatalf("run stop changed Task semantics: Run=%#v Task=%#v", terminal, persistedTask)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("run stop reset, cleaned, or deleted dirty workspace: %v", err)
	}
}

func TestRT04DeadlineTimesOutRunWithoutCompletingTaskOrCleaningWorkspace(t *testing.T) {
	fixture := newP3DockerFixtureWithRunTimeout(t, 3*time.Second)
	agent := fixture.addAgent(t, "Timeout Agent")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped timeout", 0)
	fixture.components.runtime.Start(fixture.ctx)
	active := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	if active.DeadlineAt == "" {
		t.Fatalf("active Run has no durable deadline: %#v", active)
	}
	dirty := filepath.Join(active.WorkspacePath, "dirty-runtime.txt")
	waitForFile(t, fixture.ctx, dirty)
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunTimedOut && run.CleanupState == core.CleanupRemoved && task.Status == core.TaskFailed
	})
	persistedTask, err := fixture.components.store.Task(fixture.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.StopRequestedAt == "" || terminal.StopOperationID == "" || terminal.TokenRevokedAt == "" ||
		persistedTask.Status == core.TaskCompleted || !strings.HasPrefix(persistedTask.FailureReason, "RUN_TIMED_OUT") {
		t.Fatalf("timeout did not preserve durable intent/projection: Run=%#v Task=%#v", terminal, persistedTask)
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
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim created reconcile fixture: ok=%v err=%v", ok, err)
	}
	created := prepareCreatedRunForReconcile(t, fixture, claim)
	stopped, err := fixture.components.service.RequestRuntimeStop(fixture.ctx, core.RunStopInput{
		RunID: created.ID, Reason: "operator stop before daemon restart", OperationID: "rt05-stop-operation",
		RequestID: "rt05-stop-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.LaunchPhase != core.LaunchCreated || stopped.StopRequestedAt == "" {
		t.Fatalf("created Run stop intent = %#v", stopped)
	}
	if err := fixture.components.runtime.Reconcile(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	terminal, err := fixture.components.store.Run(fixture.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedTask, err := fixture.components.store.Task(fixture.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != core.RunInterrupted || terminal.LaunchPhase != core.LaunchCreated ||
		terminal.StartedAt != "" || terminal.NativeSessionID != "" || terminal.CleanupState != core.CleanupRemoved ||
		persistedTask.Status == core.TaskCompleted {
		t.Fatalf("reconciled stop started or completed work: Run=%#v Task=%#v", terminal, persistedTask)
	}
	events, err := fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID, RunID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
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
	fixture.addTask(t, project.ID, agent.ID, "hold until stopped restart adoption", 0)
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim active reconcile fixture: ok=%v err=%v", ok, err)
	}
	created := prepareCreatedRunForReconcile(t, fixture, claim)
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, created.ID)
	control, err := fixture.components.runtime.openRunControl(created, controlPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.components.runtime.registerControl(created.ID, control)
	if _, err := fixture.executor.Attach(fixture.ctx, runtimeRef(created)); err != nil {
		t.Fatal(err)
	}
	started, err := fixture.components.service.RecordRunStartIssued(
		fixture.ctx,
		runtimeFactInput(created, runtimeRef(created), "rt05-active-start"),
	)
	if err != nil {
		t.Fatal(err)
	}
	startedRef, err := fixture.executor.Start(fixture.ctx, runtimeRef(started))
	if err != nil {
		t.Fatal(err)
	}
	active, err := fixture.components.service.ObserveProcessAndActivateRun(
		fixture.ctx,
		runtimeFactInput(started, startedRef, "rt05-active-observed"),
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.components.runtime.mu.Lock()
	oldControl := fixture.components.runtime.controls[active.ID]
	fixture.components.runtime.mu.Unlock()
	if oldControl == nil {
		t.Fatal("fault fixture has no original Run control")
	}
	if err := fixture.components.runtime.closeControl(active.ID, oldControl); err != nil {
		t.Fatal(err)
	}

	restarted := newRuntimeController(
		fixture.components.config,
		fixture.components.service,
		fixture.executor,
		adapter.Production(),
		fixture.components.runtime.workspaces,
		fixture.components.runtime.coordlink,
	)
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.Reconcile(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	adopted, err := fixture.components.store.Run(fixture.ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := fixture.components.service.RequestRuntimeStop(fixture.ctx, core.RunStopInput{
		RunID: active.ID, Reason: "restart adoption assertion complete", OperationID: "rt05-active-stop",
		RequestID: "rt05-active-stop-request",
	}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForRun(t, fixture, claim.Task.ID, func(run core.Run, task core.Task) bool {
		return run.ID == active.ID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskFailed
	})
	if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("adopted container survived terminal cleanup: %v", err)
	}
}

func TestRT05ReconcilePreservesOutcomeRecordedBeforeRestart(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Reconcile Outcome Agent")
	project := fixture.addProject(t, agent.ID)
	fixture.addTask(t, project.ID, agent.ID, "outcome then hold across restart", 0)
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim outcome reconcile fixture: ok=%v err=%v", ok, err)
	}
	created := prepareCreatedRunForReconcile(t, fixture, claim)
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, created.ID)
	control, err := fixture.components.runtime.openRunControl(created, controlPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.components.runtime.registerControl(created.ID, control)
	if _, err := fixture.executor.Attach(fixture.ctx, runtimeRef(created)); err != nil {
		t.Fatal(err)
	}
	started, err := fixture.components.service.RecordRunStartIssued(
		fixture.ctx,
		runtimeFactInput(created, runtimeRef(created), "rt05-outcome-start"),
	)
	if err != nil {
		t.Fatal(err)
	}
	startedRef, err := fixture.executor.Start(fixture.ctx, runtimeRef(started))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.components.service.ObserveProcessAndActivateRun(
		fixture.ctx,
		runtimeFactInput(started, startedRef, "rt05-outcome-active"),
	); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var outcome core.Run
	for time.Now().Before(deadline) {
		outcome, err = fixture.components.store.Run(fixture.ctx, started.ID)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.RequestedOutcome == string(core.OutcomeWait) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if outcome.RequestedOutcome != string(core.OutcomeWait) {
		t.Fatalf("provider did not persist outcome before restart: %#v", outcome)
	}
	if err := fixture.components.runtime.closeControl(outcome.ID, control); err != nil {
		t.Fatal(err)
	}

	restarted := newRuntimeController(
		fixture.components.config,
		fixture.components.service,
		fixture.executor,
		adapter.Production(),
		fixture.components.runtime.workspaces,
		fixture.components.runtime.coordlink,
	)
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.Reconcile(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	terminal, err := fixture.components.store.Run(fixture.ctx, outcome.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedTask, err := fixture.components.store.Task(fixture.ctx, claim.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != core.RunExited || terminal.RequestedOutcome != string(core.OutcomeWait) ||
		terminal.StopRequestedAt == "" || terminal.CleanupState != core.CleanupRemoved ||
		persistedTask.Status != core.TaskWaiting || persistedTask.WaitReason != "durable before restart" {
		t.Fatalf("restart lost or downgraded durable outcome: Run=%#v Task=%#v", terminal, persistedTask)
	}
}

func TestRT05ProcessCrashReopensSQLiteAndAdoptsActiveContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "cp3-process-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	repositoryRoot := daemonRepositoryRoot(t)
	coordlinkPath := filepath.Join(root, "coordlink")
	buildCoordlink := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", coordlinkPath, "./cmd/coordlink")
	buildCoordlink.Dir = repositoryRoot
	buildCoordlink.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := buildCoordlink.CombinedOutput(); err != nil {
		t.Fatalf("build coordlink: %v\n%s", err, raw)
	}
	daemonBinary := filepath.Join(root, "coordplane")
	buildDaemon := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", daemonBinary, "./cmd/coordplane")
	buildDaemon.Dir = repositoryRoot
	if raw, err := buildDaemon.CombinedOutput(); err != nil {
		t.Fatalf("build coordplane: %v\n%s", err, raw)
	}
	image := "coordplane-p3-process-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	dockerConfig := filepath.Join(root, "docker-config")
	if err := os.MkdirAll(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	buildImage := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image,
		filepath.Join(repositoryRoot, "internal", "daemon", "testdata", "codex-runtime"))
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
	configPath := writeTestConfig(t, root)
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	rawConfig = []byte(strings.ReplaceAll(string(rawConfig),
		"  docker_network: coordplane\n",
		"  docker_network: none\n",
	))
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "instructions.md")
	if err := os.WriteFile(instructions, []byte("Work only on the assigned Task."), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := seed.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Process Recovery Agent", AdapterID: "codex", Image: image,
		InstructionsFile: instructions, RequestID: "process-recovery-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomeAgent, err := seed.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Shutdown Outcome Agent", AdapterID: "codex", Image: image,
		InstructionsFile: instructions, RequestID: "shutdown-outcome-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedAgent, err := seed.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Shutdown Claim Fence Agent", AdapterID: "codex", Image: image,
		InstructionsFile: instructions, RequestID: "shutdown-claim-fence-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := seed.service.AddProject(ctx, core.AddProjectInput{
		Name: "Process Recovery Project", Source: createSourceRepository(t, root), SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "process-recovery-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := seed.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "hold until stopped process recovery", MaxRetries: 0, RequestID: "process-recovery-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	socket := filepath.Join(root, "data", "operator.sock")
	first := startP3DaemonProcess(t, daemonBinary, configPath, socket, filepath.Join(root, "daemon-first.log"))
	t.Cleanup(func() { stopP3DaemonProcess(t, first) })
	client, err := transport.NewUnixClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	active := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
		return run.State == core.RunActive && run.ContainerID != ""
	})
	runSocket := filepath.Join(root, "data", "run-control", active.ID, "api.sock")
	if info, err := os.Lstat(runSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("active Run socket: info=%v err=%v", info, err)
	}
	killP3DaemonProcess(t, first)
	persistedStore, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := persistedStore.Run(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistedStore.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := executor.Inspect(ctx, runtimeRef(persisted))
	if err != nil || !state.Running || state.Ref.ContainerID != active.ContainerID {
		t.Fatalf("SIGKILL did not preserve the owned container: state=%#v err=%v", state, err)
	}

	second := startP3DaemonProcess(t, daemonBinary, configPath, socket, filepath.Join(root, "daemon-second.log"))
	t.Cleanup(func() { stopP3DaemonProcess(t, second) })
	client, err = transport.NewUnixClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	adopted := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
		return run.ID == active.ID && run.State == core.RunActive && run.ContainerID == active.ContainerID
	})
	newSocketInfo, err := os.Lstat(runSocket)
	if err != nil || newSocketInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("restart did not replace the dead Run socket: info=%v err=%v", newSocketInfo, err)
	}
	token, err := os.ReadFile(filepath.Join(root, "data", "run-control", adopted.ID, "token"))
	if err != nil {
		t.Fatal(err)
	}
	runClient, err := transport.NewUnixClient(runSocket, transport.WithBearerToken(strings.TrimSpace(string(token))))
	if err != nil {
		t.Fatal(err)
	}
	var current core.CurrentTaskResult
	if err := runClient.JSON(ctx, http.MethodGet, "/v1/task/current", nil, &current); err != nil || current.Run.ID != adopted.ID {
		t.Fatalf("rebuilt Run socket is not serving the adopted scope: result=%#v err=%v", current, err)
	}
	var outcomeTask core.Task
	if err := client.JSON(ctx, http.MethodPost, "/v1/tasks", core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: outcomeAgent.ID, Kind: core.TaskWork,
		Title: "hold until stopped shutdown outcome", MaxRetries: 0, RequestID: "shutdown-outcome-task",
	}, &outcomeTask); err != nil {
		t.Fatal(err)
	}
	outcomeRun := waitForOperatorRun(t, client, outcomeTask.ID, func(run core.Run) bool {
		return run.State == core.RunActive && run.ContainerID != ""
	})
	outcomeToken, err := os.ReadFile(filepath.Join(root, "data", "run-control", outcomeRun.ID, "token"))
	if err != nil {
		t.Fatal(err)
	}
	outcomeClient, err := transport.NewUnixClient(
		filepath.Join(root, "data", "run-control", outcomeRun.ID, "api.sock"),
		transport.WithBearerToken(strings.TrimSpace(string(outcomeToken))),
	)
	if err != nil {
		t.Fatal(err)
	}

	signalP3DaemonProcess(t, second)
	waitForP3DaemonNotReady(t, second, client)
	var requested core.OutcomeResult
	if err := outcomeClient.JSON(ctx, http.MethodPost, "/v1/task/outcome", core.OutcomeInput{
		Outcome: core.OutcomeWait, Reason: "durable during shutdown grace", RequestID: "shutdown-grace-outcome",
	}, &requested); err != nil {
		t.Fatal(err)
	}
	if requested.Run.RequestedOutcome != string(core.OutcomeWait) {
		t.Fatalf("shutdown grace outcome was not durable: %#v", requested)
	}
	var queuedTask core.Task
	if err := client.JSON(ctx, http.MethodPost, "/v1/tasks", core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: queuedAgent.ID, Kind: core.TaskWork,
		Title: "must remain queued during shutdown grace", MaxRetries: 0, RequestID: "shutdown-queued-task",
	}, &queuedTask); err != nil {
		t.Fatal(err)
	}
	assertTaskUnclaimedDuringShutdown(t, second, client, queuedTask.ID)
	waitP3DaemonProcess(t, second)
	terminalStore, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := terminalStore.Run(ctx, adopted.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcomeTerminal, err := terminalStore.Run(ctx, outcomeRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcomeTaskPersisted, err := terminalStore.Task(ctx, outcomeTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	queuedTaskPersisted, err := terminalStore.Task(ctx, queuedTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	queuedRuns, err := terminalStore.Runs(ctx, core.RunFilter{TaskID: queuedTask.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := terminalStore.Close(); err != nil {
		t.Fatal(err)
	}
	if terminal.State != core.RunInterrupted || terminal.ContainerID != active.ContainerID ||
		terminal.StopReason != runtimeShutdownReason || terminal.StopRequestedAt == "" ||
		terminal.CleanupState != core.CleanupRemoved {
		t.Fatalf("adopted Run terminal state = %#v", terminal)
	}
	if _, err := executor.Inspect(ctx, runtimeRef(terminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("adopted container survived cleanup: %v", err)
	}
	if outcomeTerminal.State != core.RunExited || outcomeTerminal.RequestedOutcome != string(core.OutcomeWait) ||
		outcomeTerminal.StopRequestedAt == "" || outcomeTerminal.CleanupState != core.CleanupRemoved ||
		outcomeTaskPersisted.Status != core.TaskWaiting || outcomeTaskPersisted.WaitReason != "durable during shutdown grace" {
		t.Fatalf("shutdown downgraded durable outcome: Run=%#v Task=%#v", outcomeTerminal, outcomeTaskPersisted)
	}
	if _, err := executor.Inspect(ctx, runtimeRef(outcomeTerminal)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("outcome container survived shutdown cleanup: %v", err)
	}
	if queuedTaskPersisted.Status != core.TaskQueued || queuedTaskPersisted.CurrentRunID != "" || len(queuedRuns.Items) != 0 {
		t.Fatalf("shutdown admitted a new Run: Task=%#v Runs=%#v", queuedTaskPersisted, queuedRuns)
	}
}

func TestRT02MissingWorkspaceAfterStartIssuedFailsBeforeSecondContainer(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Workspace Fence Agent")
	project := fixture.addProject(t, agent.ID)
	fixture.addTask(t, project.ID, agent.ID, "workspace loss must fail loud", 1)
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim first workspace Run: ok=%v err=%v", ok, err)
	}
	created := prepareCreatedRunForReconcile(t, fixture, claim)
	started, err := fixture.components.service.RecordRunStartIssued(
		fixture.ctx,
		runtimeFactInput(created, runtimeRef(created), "rt02-start-issued"),
	)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := fixture.components.service.RecordRuntimeRunTerminal(fixture.ctx, runtimeTerminalInput(started, core.RunTerminalInput{
		State: core.RunInterrupted, TerminalReason: "fault injected after start intent",
		RequestID: "rt02-first-terminal", OperationID: started.LaunchOperationID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.components.runtime.cleanupRun(
		fixture.ctx,
		terminal.Run,
		runtimeRef(terminal.Run),
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(started.WorkspacePath); err != nil {
		t.Fatal(err)
	}

	var retry core.Claim
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		retry, ok, err = fixture.components.service.ClaimNext(fixture.ctx, project.ID)
		if err != nil {
			t.Fatal(err)
		}
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
	persisted, err := fixture.components.store.Run(fixture.ctx, retry.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}

	repositoryRoot := daemonRepositoryRoot(t)
	root, err := os.MkdirTemp("/tmp", "cp3-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	coordlinkPath := filepath.Join(root, "coordlink")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", coordlinkPath, "./cmd/coordlink")
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build coordlink: %v\n%s", err, raw)
	}
	image := "coordplane-p3-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	imageRoot := filepath.Join(repositoryRoot, "internal", "daemon", "testdata", "codex-runtime")
	dockerConfig := filepath.Join(root, "docker-config")
	if err := os.MkdirAll(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
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

	configPath := writeTestConfig(t, root)
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	rawConfig = []byte(strings.ReplaceAll(string(rawConfig),
		"  docker_network: coordplane\n",
		"  docker_network: none\n",
	))
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "instructions.md")
	if err := os.WriteFile(instructions, []byte("Work only on the assigned Task."), 0o600); err != nil {
		t.Fatal(err)
	}
	source := createSourceRepository(t, root)
	components, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	components.runtime.coordlink = coordlinkPath
	t.Cleanup(func() { _ = components.Close() })
	agent, err := components.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "P3 Docker Agent", AdapterID: "codex", Image: image,
		InstructionsFile: instructions, RequestID: "p3-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := components.service.AddProject(ctx, core.AddProjectInput{
		Name: "p3-project", Source: source, SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "p3-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := components.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "exit zero after saying done completed", MaxRetries: 0, RequestID: "p3-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	components.runtime.Start(ctx)

	var persistedTask core.Task
	var run core.Run
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		persistedTask, err = components.store.Task(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs, readErr := components.store.Runs(ctx, core.RunFilter{TaskID: task.ID, Limit: 10})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(runs.Items) == 1 {
			run, err = components.store.Run(ctx, runs.Items[0].ID)
			if err != nil {
				t.Fatal(err)
			}
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
	logRaw, err := os.ReadFile(run.LogPath)
	if err != nil {
		t.Fatal(err)
	}
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

func daemonRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve daemon test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func gitWorkspaceSpecForTest(task core.Task) gitrepo.WorkspaceSpec {
	return gitrepo.WorkspaceSpec{ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA}
}

type p3DockerFixture struct {
	ctx          context.Context
	root         string
	image        string
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
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
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
	client, err := transport.NewUnixClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var status core.Status
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &status); err == nil && status.DaemonReady {
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
	t.Fatalf("daemon readiness timeout:\n%s", readP3DaemonLog(logPath))
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

func waitForP3DaemonNotReady(t *testing.T, process *p3DaemonProcess, client *transport.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last core.Status
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &last)
		if lastErr == nil && !last.DaemonReady {
			return
		}
		select {
		case err := <-process.done:
			process.command = nil
			_ = process.log.Close()
			t.Fatalf("daemon exited before shutdown grace was observable: %v\n%s", err, readP3DaemonLog(process.logPath))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon did not expose shutdown readiness fence: status=%#v err=%v\n%s", last, lastErr, readP3DaemonLog(process.logPath))
}

func assertTaskUnclaimedDuringShutdown(
	t *testing.T,
	process *p3DaemonProcess,
	client *transport.Client,
	taskID string,
) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	observations := 0
	for time.Now().Before(deadline) {
		var detail core.TaskDetail
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/tasks/"+taskID, nil, &detail); err != nil {
			t.Fatalf("read shutdown-fenced Task: %v\n%s", err, readP3DaemonLog(process.logPath))
		}
		if detail.Task.Status != core.TaskQueued || detail.Task.CurrentRunID != "" || detail.CurrentRun != nil {
			t.Fatalf("shutdown scheduler claimed new Task during grace: %#v", detail)
		}
		observations++
		time.Sleep(20 * time.Millisecond)
	}
	if observations < 2 {
		t.Fatalf("shutdown claim fence observations = %d", observations)
	}
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

func newP3DockerFixture(t *testing.T) *p3DockerFixture {
	return newP3DockerFixtureWithRunTimeout(t, 0)
}

func newP3DockerFixtureWithRunTimeout(t *testing.T, runTimeout time.Duration) *p3DockerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "cp3-")
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := daemonRepositoryRoot(t)
	coordlinkPath := filepath.Join(root, "coordlink")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", coordlinkPath, "./cmd/coordlink")
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build coordlink: %v\n%s", err, raw)
	}
	image := "coordplane-p3-test:" + fmt.Sprintf("%x", time.Now().UnixNano())
	dockerConfig := filepath.Join(root, "docker-config")
	if err := os.MkdirAll(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	imageRoot := filepath.Join(repositoryRoot, "internal", "daemon", "testdata", "codex-runtime")
	buildImage := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, imageRoot)
	buildImage.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	if raw, err := buildImage.CombinedOutput(); err != nil {
		t.Fatalf("build deterministic one-shot image: %v\n%s", err, raw)
	}
	configPath := writeTestConfig(t, root)
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	rawConfig = []byte(strings.ReplaceAll(string(rawConfig),
		"  docker_network: coordplane\n",
		"  docker_network: none\n",
	))
	if runTimeout > 0 {
		rawConfig = []byte(strings.Replace(string(rawConfig),
			"  default_image:", "  run_timeout: "+runTimeout.String()+"\n  default_image:", 1))
	}
	if err := os.WriteFile(configPath, rawConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "instructions.md")
	if err := os.WriteFile(instructions, []byte("Work only on the assigned Task."), 0o600); err != nil {
		t.Fatal(err)
	}
	source := createSourceRepository(t, root)
	assembled, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	assembled.runtime.coordlink = coordlinkPath
	fixture := &p3DockerFixture{
		ctx: ctx, root: root, image: image, instructions: instructions,
		source: source, components: assembled, executor: executor,
	}
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
	launch, err := fixture.components.service.RuntimeLaunchContext(fixture.ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	instructions, instructionsHash, err := readInstructions(launch.Agent.InstructionsFile)
	if err != nil {
		t.Fatal(err)
	}
	workspaceSpec, err := gitWorkspaceSpec(launch.Task)
	if err != nil {
		t.Fatal(err)
	}
	workspacePath, err := fixture.components.runtime.workspaces.Path(launch.Project.ID, launch.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(fixture.components.config.Runtime.AgentHomeRoot, launch.Agent.ID)
	logPath := filepath.Join(fixture.components.config.Runtime.LogRoot, launch.Run.ID, "run.log")
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, launch.Run.ID)
	prepared, err := fixture.components.service.BeginRunLaunch(fixture.ctx, core.RunLaunchInput{
		RunID: launch.Run.ID, Generation: launch.Run.Generation, LaunchNonce: "rt05-created-nonce",
		WorkspacePath: workspacePath, HomePath: homePath, LogPath: logPath,
		InstructionsHash: instructionsHash, LaunchMode: "start", CleanupOperationID: "rt05-cleanup-operation",
		RequestID: "rt05-prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.components.runtime.prepareWorkspace(fixture.ctx, prepared, workspaceSpec); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{homePath, 0o2770}, {filepath.Dir(logPath), 0o700}, {controlPath, 0o750},
	} {
		if err := ensureRuntimeDirectory(directory.path, directory.mode); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap := buildBootstrap(launch, prepared, instructions, workspacePath, workspaceSpec)
	if err := writeRunControlMarker(controlPath, prepared); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(controlPath, "token"), []byte(claim.Token+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(controlPath, "bootstrap"), []byte(bootstrap), 0o440); err != nil {
		t.Fatal(err)
	}
	entry, ok := fixture.components.runtime.adapters.Lookup(prepared.AdapterID)
	if !ok {
		t.Fatalf("adapter %q is not registered", prepared.AdapterID)
	}
	command, err := entry.BuildStartCommand(adapter.LaunchSpec{
		BootstrapPath: adapter.ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	containerSpec, err := fixture.components.runtime.containerSpec(prepared, launch.Task.Kind, command, controlPath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := fixture.executor.Create(fixture.ctx, containerSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = fixture.executor.Stop(cleanup, ref, 0)
		_, _ = fixture.executor.Remove(cleanup, ref)
	})
	created, err := fixture.components.service.RecordContainerCreated(
		fixture.ctx,
		runtimeFactInput(prepared, ref, "rt05-created"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (f *p3DockerFixture) addAgent(t *testing.T, name string) core.Agent {
	t.Helper()
	agent, err := f.components.service.AddAgent(f.ctx, core.AddAgentInput{
		DisplayName: name, AdapterID: "codex", Image: f.image,
		InstructionsFile: f.instructions, RequestID: "agent-" + strings.ReplaceAll(name, " ", "-"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func (f *p3DockerFixture) addProject(t *testing.T, integrationAgentID string) core.Project {
	t.Helper()
	project, err := f.components.service.AddProject(f.ctx, core.AddProjectInput{
		Name: "project-" + integrationAgentID, Source: f.source, SourceRef: "refs/heads/main",
		IntegrationAgentID: integrationAgentID, RequestID: "project-" + integrationAgentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func (f *p3DockerFixture) addTask(t *testing.T, projectID, agentID, title string, retries int) core.Task {
	t.Helper()
	task, err := f.components.service.CreateTask(f.ctx, core.CreateTaskInput{
		ProjectID: projectID, AssigneeAgentID: agentID, Kind: core.TaskWork,
		Title: title, MaxRetries: retries, RequestID: "task-" + strings.ReplaceAll(title, " ", "-"),
	})
	if err != nil {
		t.Fatal(err)
	}
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
		task, err := fixture.components.store.Task(fixture.ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := fixture.components.store.Runs(fixture.ctx, core.RunFilter{TaskID: taskID, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs.Items) > 0 {
			latest, err = fixture.components.store.Run(fixture.ctx, runs.Items[len(runs.Items)-1].ID)
			if err != nil {
				t.Fatal(err)
			}
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
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	peerToken, err := os.ReadFile(peerTokenPath)
	if err != nil {
		t.Fatal(err)
	}
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
		if err := os.WriteFile(canary, []byte("peer-only"), 0o660); err != nil {
			t.Fatal(err)
		}
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
	if err := os.WriteFile(filepath.Join(run.WorkspacePath, label+"-private.txt"), []byte(label+"\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	gitIn(t, run.WorkspacePath, "config", "user.email", label+"@coordplane.local")
	gitIn(t, run.WorkspacePath, "config", "user.name", label)
	gitIn(t, run.WorkspacePath, "add", label+"-private.txt")
	gitIn(t, run.WorkspacePath, "commit", "-m", label+" private commit")
	privateSHA := strings.TrimSpace(gitIn(t, run.WorkspacePath, "rev-parse", "HEAD"))
	gitIn(t, run.WorkspacePath, "update-ref", "refs/heads/main", canonicalSHA)

	dockerExecOutput(t, fixture, run,
		"/bin/sh", "-eu", "-c", `printf '%s\n' "$1" > /workspace/project/.git/refs/heads/main`,
		"rt01-ref", privateSHA,
	)
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
