package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/config"
	"coordplane/internal/core"
	"coordplane/internal/gitcapture"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
)

func TestRunOperationOwnershipSerializesRuntimeSideEffects(t *testing.T) {
	controller := &runtimeController{monitors: make(map[string]*runMonitor), runOperations: make(map[string]*runOperation)}
	const contenders = 32
	start := make(chan struct{})
	winners := make(chan *runOperation, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if operation := controller.acquireRunOperation("run-serial"); operation != nil {
				winners <- operation
			}
		}()
	}
	close(start)
	wait.Wait()
	close(winners)
	var operation *runOperation
	for winner := range winners {
		if operation != nil {
			t.Fatal("more than one concurrent operation acquired the same Run")
		}
		operation = winner
	}
	if operation == nil {
		t.Fatal("no concurrent operation acquired the Run")
	}

	controller.releaseRunOperation("run-serial", &runOperation{})
	if contender := controller.acquireRunOperation("run-serial"); contender != nil {
		t.Fatal("stale release cleared the current operation owner")
	}
	monitor := &runMonitor{runID: "run-serial"}
	if err := controller.registerMonitor(monitor); err != nil {
		t.Fatalf("owner could not hand the Run to its monitor: %v", err)
	}
	controller.releaseRunOperation("run-serial", operation)
	if contender := controller.acquireRunOperation("run-serial"); contender != nil {
		t.Fatal("operation acquired a Run with an active monitor")
	}
	controller.unregisterMonitor(&runMonitor{runID: "run-serial"})
	if controller.monitor("run-serial") != monitor {
		t.Fatal("stale monitor removal cleared the current monitor")
	}
	controller.unregisterMonitor(monitor)
	if next := controller.acquireRunOperation("run-serial"); next == nil {
		t.Fatal("Run remained owned after operation and monitor release")
	} else {
		controller.releaseRunOperation("run-serial", next)
	}
}

func TestInspectHelperStableIdentityRecoversInspectRemoveFailure(t *testing.T) {
	inspectErr, removeErr := errors.New("inspect unavailable"), errors.New("remove unavailable")
	request := gitrepo.WorkspaceInspectRequest{ProjectID: "project", TaskID: "task", Workspace: "/workspace/task"}
	ref := inspectRuntimeRef(request, inspectOperationID(request))
	executor := newBlockingLaunchExecutor()
	executor.inspectErr, executor.removeErr = inspectErr, removeErr
	close(executor.allowCreate)
	close(executor.stopped)
	helper := dockerCaptureHelper{executor: executor, config: config.GitConfig{CaptureTimeout: time.Second}, root: t.TempDir()}
	if err := helper.runContainer(context.Background(), containerruntime.ContainerSpec{Ref: ref}, false); !errors.Is(err, inspectErr) || !errors.Is(err, removeErr) || !executor.present {
		t.Fatalf("Inspect+Remove failure = %v, present=%t", err, executor.present)
	}
	executor.removeErr = nil
	ready := filepath.Join(helper.root, "project", "task", "inspect-"+inspectOperationID(request), "inspect.ready")
	requireNoError(t, os.MkdirAll(ready, 0o700))
	handoff, err := helper.prepareInspectHandoff(context.Background(), request, ref, inspectOperationID(request))
	if err != nil || executor.present {
		t.Fatalf("prepare exited Inspect residue: %v present=%t", err, executor.present)
	}
	if _, err := os.Stat(filepath.Join(handoff, "inspect.ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale inspect.ready survived reset: %v", err)
	}
}

func TestInspectHelperDiscardRetryReinspectsMutatedWorkspace(t *testing.T) {
	ctx, fixture := context.Background(), newRealP4Harness(t)
	root, head := fixture.root, fixture.project.InitialSHA
	executable := filepath.Join(root, "coordplane-git-helper")
	requireNoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))
	executor := &staleInspectExecutor{head: head}
	helper := requireRuntimeValue(newDockerCaptureHelper(executor, config.GitConfig{CaptureHelperImage: "helper", CaptureTimeout: 20 * time.Millisecond, MaximumObjects: 100, MaximumBundleBytes: 1 << 20, MaximumHandoffBytes: 2 << 20}, filepath.Join(root, "handoff"), executable))
	manager := requireRuntimeValue(gitrepo.NewWorkspaceManager(fixture.initializer, filepath.Join(root, "retry-workspaces"), helper))
	spec := gitrepo.WorkspaceSpec{ProjectID: fixture.project.ID, TaskID: "task-inspect-retry", BaseSHA: head}
	workspace := requireRuntimeValue(manager.Materialize(ctx, spec))
	preview := requireRuntimeValue(manager.State(ctx, spec, workspace.HeadSHA, 1))
	if discarded, err := manager.Discard(ctx, spec, workspace.HeadSHA, 1, preview.Fingerprint, func() (bool, error) { return true, nil }); discarded || err == nil {
		t.Fatalf("first discard discarded=%t err=%v", discarded, err)
	}
	requireNoError(t, os.WriteFile(filepath.Join(workspace.Path, "mutation"), []byte("new\n"), 0o600))
	discarded, err := manager.Discard(ctx, spec, workspace.HeadSHA, 1, preview.Fingerprint, func() (bool, error) { return true, nil })
	if discarded || err == nil || !strings.Contains(err.Error(), "fingerprint changed") || executor.createCalls.Load() != 3 || executor.stopCalls.Load() != 1 {
		t.Fatalf("retry discarded=%t err=%v creates=%d stops=%d", discarded, err, executor.createCalls.Load(), executor.stopCalls.Load())
	}
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatalf("retry removed mutated workspace: %v", err)
	}
}

func TestRuntimeWorkersWaitForExplicitShutdownAfterParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	controller := &runtimeController{}
	controller.Start(parent)
	cancelParent()
	select {
	case <-controller.contextDone():
		t.Fatal("serving context cancelled runtime workers before shutdown persisted stop intent")
	default:
	}
	controller.cancelRuntimeWorkers()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := controller.waitForRuntimeWorkers(waitCtx); err != nil {
		t.Fatalf("join explicitly cancelled runtime workers: %v", err)
	}
}

func TestRunLogRetentionTreatsPreLaunchEmptyPathAsAbsentAndLeavesGCReady(t *testing.T) {
	ctx := context.Background()
	service, claim := claimRuntimeTestRun(t)
	controller := &runtimeController{service: service, config: config.Config{Runtime: config.RuntimeConfig{LogRoot: filepath.Join(t.TempDir(), "logs")}}}
	if err := controller.failUnpreparedRun(ctx, claim.Run, "INSTRUCTIONS_UNAVAILABLE", "missing instructions"); err == nil {
		t.Fatal("pre-launch failure did not report its terminal error")
	}
	terminal, err := service.Run(ctx, claim.Run.ID)
	if err != nil || !core.IsRunTerminal(terminal.State) || terminal.LogPath != "" {
		t.Fatalf("pre-launch terminal Run = %#v err=%v", terminal, err)
	}
	if err := controller.cleanupRunLogs(ctx, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("retention=0 rejected absent Run log: %v", err)
	}
	if err := service.ReconcileWorkspaceGC(ctx, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("workspace GC after absent Run log: %v", err)
	}
	if err := service.ReconcileGitGC(ctx, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("Git GC after absent Run log: %v", err)
	}
	status, err := service.Status(ctx, "")
	if err != nil || !status.DaemonReady {
		t.Fatalf("readiness after retention/GC = %#v err=%v", status, err)
	}
}

func TestReconcileSkipsRunOwnedBetweenClaimAndMonitorRegistration(t *testing.T) {
	service := newRuntimeTestService(t)
	root := t.TempDir()
	coordlink := filepath.Join(root, "coordlink")
	requireNoError(t, os.WriteFile(coordlink, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	agent, project := addRuntimeTestProject(service, runtimeTestInstructionsText)
	if _, err := service.Chat(context.Background(), core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "exercise launch ownership", Wake: true, RequestID: "wake-launch-race"}); err != nil {
		t.Fatal(err)
	}
	executor := newBlockingLaunchExecutor()
	cfg := config.Config{DataDir: root, Runtime: config.RuntimeConfig{DockerNetwork: "none", WorkspaceRoot: filepath.Join(root, "workspaces"), AgentHomeRoot: filepath.Join(root, "homes"), LogRoot: filepath.Join(root, "logs")}}
	controller := newRuntimeController(cfg, service, executor, adapter.Production(), nil, coordlink)
	controller.ctx, controller.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { _ = controller.Close() })
	claim, operation, ok, err := controller.claimNext(context.Background())
	if err != nil || !ok {
		t.Fatalf("claim runtime test Run: ok=%t err=%v", ok, err)
	}
	launchDone := make(chan error, 1)
	go func() { launchDone <- controller.launchOwned(context.Background(), claim, operation) }()
	waitRuntimeSignal(t, executor.createEntered, 5*time.Second, "launch did not reach the blocked Create boundary")

	requireNoError(t, controller.Reconcile(context.Background()))
	if executor.inspectCalls.Load() != 0 {
		t.Fatalf("reconcile inspected a Run owned by launch: calls=%d", executor.inspectCalls.Load())
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), claim.Run.ID))
	if persisted.State != core.RunStarting || persisted.LaunchNonce == "" || persisted.ContainerID != "" {
		t.Fatalf("reconcile mutated launch-owned Run: %#v", persisted)
	}
	close(executor.allowCreate)
	select {
	case err := <-launchDone:
		requireNoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("launch did not finish after Create was released")
	}
	active := waitForRuntimeTestRun(t, service, claim.Run.ID, func(run core.Run) bool {
		return run.State == core.RunActive
	})
	if executor.createCalls.Load() != 1 || executor.startCalls.Load() != 1 ||
		executor.waitCalls.Load() != 1 || executor.logCalls.Load() != 1 {
		t.Fatalf("launch/reconcile duplicated runtime side effects: create=%d start=%d wait=%d logs=%d", executor.createCalls.Load(), executor.startCalls.Load(), executor.waitCalls.Load(), executor.logCalls.Load())
	}
	if controller.monitor(active.ID) == nil {
		t.Fatal("active Run has no registered monitor")
	}
	if _, err := service.RequestRuntimeStop(context.Background(), core.RunStopInput{RunID: active.ID, Reason: "race assertion complete", OperationID: "stop-launch-race", RequestID: "stop-launch-race-request"}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForRuntimeTestRun(t, service, active.ID, func(run core.Run) bool {
		return core.IsRunTerminal(run.State) && run.CleanupState == core.CleanupRemoved
	})
	if terminal.State != core.RunInterrupted {
		t.Fatalf("stopped launch-race Run = %#v", terminal)
	}
}

func TestReconcileRefreshesCanonicalRunAfterOwnershipHandoff(t *testing.T) {
	service, snapshot := claimRuntimeTestRun(t)
	terminal := requireRuntimeValue(service.RecordRuntimeRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: snapshot.Run.ID, Generation: snapshot.Run.Generation,
		LaunchOperationID: snapshot.Run.LaunchOperationID,
		State:             core.RunFailed, TerminalReason: "launch owner already converged",
		RuntimeErrorCode: "LAUNCH_OWNER_FAILED", RequestID: "terminal-before-reconcile-owner",
		OperationID: snapshot.Run.LaunchOperationID,
	}))
	executor := &runtimeTestExecutor{}
	controller := &runtimeController{service: service, executor: executor}

	if err := controller.reconcileOwnedRun(context.Background(), snapshot.Run); err != nil {
		t.Fatalf("reconcile stale snapshot after ownership handoff: %v", err)
	}
	if executor.inspectCalls.Load() != 0 {
		t.Fatalf("stale terminal Run reached Docker inspect: calls=%d", executor.inspectCalls.Load())
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), snapshot.Run.ID))
	if persisted.State != terminal.Run.State || persisted.TerminalReason != terminal.Run.TerminalReason ||
		persisted.RuntimeErrorCode != terminal.Run.RuntimeErrorCode {
		t.Fatalf("stale reconciliation changed canonical terminal fact: %#v", persisted)
	}
}

func TestReconcileRejectsSensitiveEnvironmentMismatchBeforeMonitor(t *testing.T) {
	const providerName = "ANTHROPIC_AUTH_TOKEN"
	t.Setenv(providerName, "current-auth-canary")
	service, claim := claimRuntimeTestRun(t)
	active, _ := activateRuntimeTestRun(t, service, claim)
	requireRuntimeValue(service.SendBossMessage(context.Background(), core.BossMessageInput{ProjectID: active.ProjectID, AgentID: active.AgentID, TaskID: active.TaskID, Body: "remain pending across rejected adoption", RequestID: "adoption-message-rogue-env"}))
	executor := &runtimeTestExecutor{}
	controller := newRuntimeTestController(t, service, executor)
	controller.config.Runtime = config.RuntimeConfig{DockerNetwork: "none", ProviderEnvAllowlist: []string{providerName}}
	controller.coordlink = requireRuntimeValue(os.Executable())
	controlPath := filepath.Join(controller.controlRoot, active.ID)
	requireNoError(t, os.Mkdir(controlPath, runControlDirectoryMode))
	requireNoError(t, writeRunControlMarker(controlPath, active))
	requireNoError(t, writeRuntimeFile(filepath.Join(controlPath, "bootstrap"), []byte("adoption bootstrap"), runControlFileMode))
	command := requireRuntimeValue((adapter.Claude{}).BuildStartCommand(adapter.LaunchSpec{BootstrapPath: adapter.ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}))
	spec := requireRuntimeValue(controller.containerSpec(active, claim.Task.Kind, command, controlPath))
	state := containerruntime.LiveState{Ref: spec.Ref, Image: spec.Image, Entrypoint: []string{spec.Command.Executable}, CommandArgs: spec.Command.Args, Status: containerruntime.StatusRunning, Running: true}
	for name, value := range spec.Command.Env {
		state.Environment = append(state.Environment, containerruntime.EnvironmentFact{Name: name, ValueDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(value)))})
	}
	// The spec no longer carries provider secrets in the container environment,
	// so the only sensitive-key mismatch that remains is a rogue container that
	// smuggled an allowlisted secret into its environment. Adoption must refuse
	// to start it.
	state.Environment = append(state.Environment, containerruntime.EnvironmentFact{Name: providerName, ValueDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("leaked-auth-canary")))})
	executor.state = &state
	before := requireRuntimeValue(json.Marshal(requireRuntimeValue(service.Snapshot(context.Background(), active.ProjectID))))
	requireNoError(t, controller.Reconcile(context.Background()))
	after := requireRuntimeValue(json.Marshal(requireRuntimeValue(service.Snapshot(context.Background(), active.ProjectID))))
	if string(before) != string(after) {
		t.Fatal("rejected adoption advanced durable Task, Run, session, outcome, Message, or Event state")
	}
	time.Sleep(10 * time.Millisecond)
	if executor.logCalls.Load() != 0 || executor.waitCalls.Load() != 0 || controller.monitor(active.ID) != nil {
		t.Fatalf("rejected adoption side effects: Logs=%d Wait=%d monitor=%v", executor.logCalls.Load(), executor.waitCalls.Load(), controller.monitor(active.ID) != nil)
	}
	if _, err := os.Stat(active.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected adoption created a runtime log: %v", err)
	}
	healthy, reason := controller.Healthy()
	wantReason := fmt.Sprintf("reconcile Run %s: %s: container isolation environment mismatch", active.ID, containerruntime.ErrOwnership)
	if healthy || reason != wantReason {
		t.Fatal("rejected adoption did not degrade with only the generic ownership error")
	}
}

func TestShutdownPersistsIntentBeforeCancellingMonitorAndConverges(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, _ := activateRuntimeTestRun(t, service, claim)
	root := t.TempDir()
	controller := newRuntimeTestController(t, service, &runtimeTestExecutor{})
	controller.controlRoot = root
	intentBeforeCancel := make(chan bool, 1)
	var monitor *runMonitor
	monitor = &runMonitor{runID: active.ID, waitCancel: func() {
		persisted, readErr := service.Run(context.Background(), active.ID)
		intentBeforeCancel <- readErr == nil && persisted.StopRequestedAt != "" &&
			persisted.StopReason == runtimeShutdownReason && persisted.StopOperationID != ""
		controller.unregisterMonitor(monitor)
	}}
	controller.monitors[active.ID] = monitor

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	requireNoError(t, controller.Shutdown(ctx))
	select {
	case durable := <-intentBeforeCancel:
		if !durable {
			t.Fatal("monitor Wait was cancelled before daemon_shutdown intent became durable")
		}
	default:
		t.Fatal("shutdown did not cancel the registered monitor")
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.State != core.RunInterrupted || persisted.StopReason != runtimeShutdownReason ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("shutdown Run did not converge: %#v", persisted)
	}
	if _, _, ok, err := controller.claimNext(context.Background()); err != nil || ok {
		t.Fatalf("shutdown controller admitted another Run: ok=%t err=%v", ok, err)
	}
}

func TestShutdownConvergesNonceLessRun(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	executor := &runtimeTestExecutor{}
	controller := newRuntimeTestController(t, service, executor)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	requireNoError(t, controller.Shutdown(ctx))
	persisted := requireRuntimeValue(service.Run(context.Background(), claim.Run.ID))
	if persisted.State != core.RunFailed || persisted.StopReason != runtimeShutdownReason ||
		persisted.StopOperationID == "" || persisted.RuntimeErrorCode != "DAEMON_SHUTDOWN" {
		t.Fatalf("nonce-less shutdown Run did not converge through fenced terminal state: %#v", persisted)
	}
	if executor.stopCalls.Load() != 0 {
		t.Fatalf("nonce-less shutdown tried to stop a container: calls=%d", executor.stopCalls.Load())
	}
}

func TestOutcomeNotificationRequiresSuccessfulResponse(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		status     int
		wantSignal bool
	}{
		{name: "accepted", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusOK, wantSignal: true},
		{name: "accepted without body", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusNoContent, wantSignal: true},
		{name: "malformed", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusBadRequest},
		{name: "unauthorized", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusForbidden},
		{name: "core rejection", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusConflict},
		{name: "internal failure", method: http.MethodPost, path: "/v1/task/outcome", status: http.StatusInternalServerError},
		{name: "wrong method", method: http.MethodGet, path: "/v1/task/outcome", status: http.StatusOK},
		{name: "wrong path", method: http.MethodPost, path: "/v1/progress", status: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := make(chan struct{}, 1)
			next := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
			})
			handler := notifySuccessfulOutcome(next, outcome)
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			select {
			case <-outcome:
				if !test.wantSignal {
					t.Fatal("failed or unrelated request emitted an outcome signal")
				}
			default:
				if test.wantSignal {
					t.Fatal("successful outcome request omitted its signal")
				}
			}
		})
	}
}

func TestFalseOutcomeSignalCannotStopHealthyRun(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := &runtimeTestExecutor{}
	controller := &runtimeController{service: service, executor: executor}

	controller.stopForDurableIntent(&runMonitor{runID: active.ID, ref: ref})
	if executor.stopCalls.Load() != 0 {
		t.Fatalf("false outcome signal stopped healthy container: calls=%d", executor.stopCalls.Load())
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.RequestedOutcome != "" || persisted.StopRequestedAt != "" {
		t.Fatalf("false outcome signal created durable stop intent: %#v", persisted)
	}
}

func TestSupervisorAbandonmentCancelsAndCollectsMonitor(t *testing.T) {
	service := newRuntimeTestService(t)
	cancelled := make(chan struct{})
	logs := make(chan error, 1)
	var once sync.Once
	monitor := &runMonitor{
		runID: "missing-run", control: &runControl{outcome: make(chan struct{}, 1)},
		wait: make(chan waitResult), logs: logs,
		waitCancel: func() {
			once.Do(func() {
				close(cancelled)
				logs <- nil
			})
		},
	}
	controller := &runtimeController{service: service, monitors: map[string]*runMonitor{monitor.runID: monitor}}
	controller.supervise(monitor)
	select {
	case <-cancelled:
	default:
		t.Fatal("supervisor returned without cancelling its Wait/Logs context")
	}
	if !monitor.logsDone {
		t.Fatal("supervisor returned without collecting the log goroutine")
	}
	if controller.monitor(monitor.runID) != nil {
		t.Fatal("abandoned monitor remained registered")
	}
}

func TestSupervisorStopsRunAfterSessionPersistenceFailure(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := &monitorFailureExecutor{stopped: make(chan struct{})}
	monitor := &runMonitor{runID: active.ID, ref: ref, wait: make(chan waitResult, 1), logs: make(chan error, 1)}
	controller := newRuntimeTestController(t, service, executor)
	controller.monitors[active.ID] = monitor
	monitor.logs <- errors.Join(errRuntimeSessionPersist, errors.New("injected persistence failure"))
	go func() {
		<-executor.stopped
		monitor.wait <- waitResult{fact: containerruntime.ExitFact{Ref: ref, ExitCode: 143}}
	}()

	controller.supervise(monitor)
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.StopRequestedAt == "" || persisted.StopReason != runtimeLogFailureReason ||
		persisted.State != core.RunInterrupted || persisted.RuntimeErrorCode != runtimeSessionFailureCode ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("session persistence failure did not durably stop and converge Run: %#v", persisted)
	}
	if executor.stopCalls.Load() == 0 {
		t.Fatal("session persistence failure left the container running")
	}
}

func TestSupervisorFailsClosedOnJSONLookingClaudeFrames(t *testing.T) {
	for _, test := range []struct {
		name, frame, session, evidence string
		waitFirst, exitFirst           bool
	}{
		{name: "malformed wait first", waitFirst: true, evidence: "metadata"},
		{name: "init without session", frame: `{"type":"system","subtype":"init"}`},
		{name: "success without is_error", frame: `{"type":"result","subtype":"success"}`},
		{name: "assistant missing message", frame: `{"type":"assistant"}`, exitFirst: true},
		{name: "assistant null message", frame: `{"type":"assistant","message":null}`, exitFirst: true},
		{name: "assistant empty message", frame: `{"type":"assistant","message":{}}`},
		{name: "rejected assistant reasoning is metadata only", session: "diagnostic-session", evidence: "assistant"},
		{name: "rejected invalid tool input is metadata only", session: "reserve-session", evidence: "reserve", exitFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, claim := claimRuntimeTestRun(t)
			message := requireRuntimeValue(service.SendBossMessage(context.Background(), core.BossMessageInput{ProjectID: claim.Task.ProjectID, AgentID: claim.Task.AssigneeAgentID, TaskID: claim.Task.ID, Body: "must remain pending", RequestID: "protocol-message-" + test.name}))
			active, ref := activateRuntimeTestRun(t, service, claim)
			frame, ordinary, accepted := test.frame, "", []byte(nil)
			secret, lowerDigest, upperDigest := "", "", ""
			executor := &monitorFailureExecutor{stopped: make(chan struct{})}
			executor.state = &containerruntime.LiveState{Ref: ref, Running: true, Status: containerruntime.StatusRunning}
			controller := newRuntimeTestController(t, service, executor)
			canonical := requireRuntimeValue(service.Snapshot(context.Background(), claim.Task.ProjectID)).Projects[0].CanonicalSHA
			var beforeFailure core.Run
			var beforeTask core.TaskDetail
			var beforeCleanup core.Snapshot
			if test.evidence != "" {
				const providerName = "ANTHROPIC_AUTH_TOKEN"
				secret = "provider-\"secret\"\\canary\n<&>"
				lowerDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(secret)))
				upperDigest = strings.ToUpper(lowerDigest)
				t.Setenv(providerName, secret)
				controller.config.Runtime.ProviderEnvAllowlist = []string{providerName}
				ordinary = "stderr: " + string(requireRuntimeValue(json.Marshal(secret))) + " " + lowerDigest + " " + upperDigest
				accepted = requireRuntimeValue(json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "private reasoning", "signature": "private signature"}, map[string]any{"type": "text", "text": "visible text"}, map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{secret: map[string]any{"secret": secret, "digests": []string{lowerDigest, upperDigest}}}}}}}))
				if test.evidence == "metadata" {
					rejected := requireRuntimeValue(json.Marshal(map[string]any{"type": "system", "subtype": "init", "secret": secret}))
					frame = string(rejected[:len(rejected)-1])
				} else {
					content := []any{map[string]any{"type": "thinking", "thinking": "private-reasoning-canary", "signature": "signature-canary"}}
					if test.evidence == "assistant" {
						content = append(content, map[string]any{"type": "unknown", "secret": secret})
					} else {
						content = append(content, map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": []any{secret}})
					}
					rejected := requireRuntimeValue(json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"type": "message", "role": "assistant", "content": content}}))
					init := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q}`, test.session)
					frame = strings.Join([]string{init, string(accepted), "[" + string(accepted), ordinary, string(rejected)}, "\n")
					if test.evidence == "reserve" {
						filler := `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("o", 512<<10) + `"}]}}` + "\n"
						frame = strings.Repeat(filler, runtimeLogLimit/len(filler)+1) + frame
					}
					executor.beforeStop = func() []byte {
						beforeFailure = requireRuntimeValue(service.Run(context.Background(), active.ID))
						beforeTask = requireRuntimeValue(service.Task(context.Background(), claim.Task.ID))
						beforeCleanup = requireRuntimeValue(service.Snapshot(context.Background(), claim.Task.ProjectID))
						return requireRuntimeValue(os.ReadFile(active.LogPath))
					}
				}
			}
			populateRuntimeTestControl(t, controller, active)
			executor.payload = frame
			if test.exitFirst {
				executor.releaseWait = make(chan struct{}, 1)
				executor.releaseWait <- struct{}{}
			}
			if test.waitFirst {
				executor.releaseWait, executor.releaseLogs = make(chan struct{}), make(chan struct{})
				monitor := requireRuntimeValue(controller.newMonitor(active, ref, adapter.Claude{}, nil))
				monitor.wait, monitor.waitDelivered = make(chan waitResult), make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					controller.supervise(monitor)
				}()
				close(executor.releaseWait)
				waitRuntimeSignal(t, monitor.waitDelivered, time.Second, "Wait-first supervisor did not receive Wait fact")
				close(executor.releaseLogs)
				waitRuntimeSignal(t, done, time.Second, "Wait-first supervisor did not converge")
			} else {
				monitor := requireRuntimeValue(controller.newMonitor(active, ref, adapter.Claude{}, nil))
				controller.supervise(monitor)
			}
			if test.evidence != "" {
				raw := executor.capturedLog
				if raw == nil {
					raw = requireRuntimeValue(os.ReadFile(active.LogPath))
				}
				text := strings.TrimSuffix(string(raw), "\n")
				line := text[strings.LastIndex(text, "\n")+1:]
				const prefix = `[coordplane: adapter frame rejected] `
				requireRuntimeCondition(t, strings.HasPrefix(line, prefix+`{"error":`) && strings.Contains(line, `,"frame":`), "rejected-frame diagnostic shape/order = ", string(raw))
				var evidence map[string]any
				requireNoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &evidence))
				savedFrame, _ := evidence["frame"].(string)
				wantError := "adapter: unsupported Claude assistant content block"
				if test.evidence == "metadata" {
					wantError = "adapter: invalid Claude JSON event"
				} else if test.evidence == "reserve" {
					wantError = "adapter: invalid Claude tool_use block"
				}
				requireRuntimeCondition(t, evidence["error"] == wantError, "rejected-frame diagnostic = ", evidence)
				encodedSecret := requireRuntimeValue(json.Marshal(secret))
				escapedSecret := string(encodedSecret[1 : len(encodedSecret)-1])
				doubleEncoded := requireRuntimeValue(json.Marshal(escapedSecret))
				for _, forbidden := range []string{secret, escapedSecret, string(doubleEncoded[1 : len(doubleEncoded)-1]), lowerDigest, upperDigest, "private-reasoning-canary", "signature-canary"} {
					requireRuntimeCondition(t, !strings.Contains(string(raw), forbidden) && !strings.Contains(savedFrame, forbidden), "rejected-frame diagnostic leaked sensitive value ", forbidden)
				}
				switch test.evidence {
				case "metadata":
					requireRuntimeCondition(t, savedFrame == fmt.Sprintf(`{"bytes":%d,"sanitized":false}`, len(frame)), "unsafe malformed-frame fallback = ", evidence)
				case "assistant":
					streamLines := strings.Split(text, "\n")
					var decoded []any
					requireNoError(t, json.Unmarshal([]byte("["+strings.Join(streamLines[1:4], ",")+"]"), &decoded))
					decodedStream := requireRuntimeValue(json.Marshal(decoded))
					wantStream := fmt.Sprintf(`[{"message":{"content":[{"text":"visible text","type":"text"},{"id":"tool-1","input":{"[REDACTED_SECRET]":{"digests":["[REDACTED_SECRET]","[REDACTED_SECRET]"],"secret":"[REDACTED_SECRET]"}},"name":"Read","type":"tool_use"}],"role":"assistant","type":"message"},"type":"assistant"},{"bytes":%d,"sanitized":false},{"bytes":%d,"sanitized":false}]`, len(accepted)+1, len(ordinary))
					requireRuntimeCondition(t, string(decodedStream) == wantStream && savedFrame == fmt.Sprintf(`{"bytes":%d,"sanitized":false}`, len(strings.Split(frame, "\n")[4])), "structured runtime diagnostics = ", string(decodedStream), "; rejected=", evidence)
				case "reserve":
					requireRuntimeCondition(t, len(raw) <= runtimeLogLimit && strings.Count(string(raw), runtimeLogTruncatedMarker) == 1 && savedFrame == fmt.Sprintf(`{"bytes":%d,"sanitized":false}`, len(strings.Split(frame, "\n")[len(strings.Split(frame, "\n"))-1])), "reserved rejected-frame boundary: bytes=", len(raw), " evidence=", evidence)
				}
				if test.evidence != "metadata" && !test.exitFirst {
					requireRuntimeCondition(t, !core.IsRunTerminal(beforeFailure.State) && beforeFailure.CleanupState != core.CleanupRemoved && beforeFailure.NativeSessionID == test.session && beforeFailure.RequestedOutcome == "" && beforeTask.LatestProgress == nil && beforeTask.Task.Status == core.TaskRunning && beforeTask.Task.HeadSHA == "" && beforeTask.Task.IntegrationTaskID == "" && beforeTask.Task.FinalCanonicalSHA == "" && beforeCleanup.Projects[0].CanonicalSHA == canonical, "protocol diagnostic advanced state before failure/cleanup: Run=", beforeFailure, " Task=", beforeTask, " Project=", beforeCleanup.Projects[0])
				}
			}
			requireRuntimeCondition(t, executor.stopCalls.Load() > 0 && executor.removeCalls.Load() > 0, "protocol failure cleanup calls stop=", executor.stopCalls.Load(), " remove=", executor.removeCalls.Load())
			persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
			task := requireRuntimeValue(service.Task(context.Background(), claim.Task.ID)).Task
			requireRuntimeCondition(t, persisted.State == core.RunInterrupted && persisted.RuntimeErrorCode == runtimeLogFailureCode && persisted.NativeSessionID == test.session && persisted.RequestedOutcome == "" && persisted.CleanupState == core.CleanupRemoved && persisted.LastError != "" && task.Status == core.TaskQueued && task.CurrentRunID == "", "protocol frame did not fail closed: Run=", persisted, " Task=", task)
			messages := requireRuntimeValue(service.ListMessages(context.Background(), core.MessageFilter{TaskID: claim.Task.ID}))
			requireRuntimeCondition(t, len(messages.Items) == 1 && messages.Items[0].ID == message.ID && messages.Items[0].State != core.MessageAcknowledged, "protocol frame acknowledged a Message: ", messages.Items)
		})
	}
}

func requireRuntimeCondition(t *testing.T, condition bool, details ...any) {
	t.Helper()
	if !condition {
		t.Fatal(details...)
	}
}

func TestShutdownReplaysTailLogsBeforeTerminalConvergence(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := &shutdownReplayExecutor{
		runtimeTestExecutor: runtimeTestExecutor{},
		payload:             "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"shutdown-tail-session\"}\n",
		ref:                 ref,
		firstLogStarted:     make(chan struct{}),
		stopped:             make(chan struct{}),
	}
	controller := newRuntimeTestController(t, service, executor)
	controller.config = config.Config{MaxParallelRuns: 1, Runtime: config.RuntimeConfig{ShutdownGrace: 275 * time.Millisecond}}
	controller.ctx, controller.cancel = context.WithCancel(context.Background())
	populateRuntimeTestControl(t, controller, active)
	entry, ok := controller.adapters.Lookup(active.AdapterID)
	if !ok {
		t.Fatalf("lookup shutdown replay adapter %q", active.AdapterID)
	}
	monitor := requireRuntimeValue(controller.newMonitor(active, ref, entry, nil))
	controller.monitors[active.ID] = monitor
	controller.wg.Add(1)
	go func() {
		defer controller.wg.Done()
		controller.supervise(monitor)
	}()
	waitRuntimeSignal(t, executor.firstLogStarted, time.Second, "initial Docker log follow did not start")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	requireNoError(t, controller.Shutdown(ctx))
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.NativeSessionID != "shutdown-tail-session" || persisted.State != core.RunInterrupted ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("shutdown did not replay tail session before terminal convergence: %#v", persisted)
	}
	if executor.logCalls.Load() != 2 {
		t.Fatalf("shutdown Docker log calls = %d, want initial follow plus offset-zero replay", executor.logCalls.Load())
	}
	if grace := time.Duration(executor.stopGrace.Load()); grace != 275*time.Millisecond {
		t.Fatalf("shutdown Docker stop grace = %s, want configured 275ms", grace)
	}
}

func TestDockerUnavailableDegradesWithoutRewritingLiveRun(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, _ := activateRuntimeTestRun(t, service, claim)
	controller := newRuntimeTestController(t, service, &unavailableRuntimeExecutor{})
	requireNoError(t, controller.Reconcile(context.Background()))
	healthy, reason := controller.Healthy()
	if healthy || reason == "" {
		t.Fatalf("Docker outage did not degrade runtime: healthy=%t reason=%q", healthy, reason)
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.State != active.State || persisted.ContainerID != active.ContainerID ||
		persisted.LaunchPhase != active.LaunchPhase || persisted.CleanupState != active.CleanupState {
		t.Fatalf("Docker outage rewrote live Run: before=%#v after=%#v", active, persisted)
	}
}

func TestRuntimeNaturalShutdownGracePreservesConfiguredLargeGrace(t *testing.T) {
	controller := &runtimeController{config: config.Config{Runtime: config.RuntimeConfig{ShutdownGrace: 60 * time.Second}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second+runtimeShutdownOverhead)
	defer cancel()

	grace := controller.runtimeNaturalShutdownGrace(ctx)
	if grace < 59*time.Second || grace > 60*time.Second {
		t.Fatalf("natural shutdown grace = %s, want configured 60s with only fixed cleanup overhead reserved", grace)
	}
}

func TestSuccessfulReconcileDoesNotClearSchedulerInvariantDegradedState(t *testing.T) {
	service := newRuntimeTestService(t)
	controller := newRuntimeTestController(t, service, &healthyReconcileExecutor{})
	controller.setSchedulerDegraded(`adapter "removed-adapter" is not registered`)

	requireNoError(t, controller.Reconcile(context.Background()))
	healthy, reason := controller.Healthy()
	if healthy || !strings.Contains(reason, "removed-adapter") {
		t.Fatalf("reconcile cleared scheduler invariant degradation: healthy=%t reason=%q", healthy, reason)
	}
	status := requireRuntimeValue(service.Status(context.Background(), ""))
	if status.DaemonReady || !strings.Contains(status.Reason, "removed-adapter") {
		t.Fatalf("status cleared scheduler invariant degradation: %#v", status)
	}
}

func TestUnavailableContainerRemovalKeepsTerminalCleanupBlocked(t *testing.T) {
	service, claim := claimRuntimeTestRun(t)
	active, ref := activateRuntimeTestRun(t, service, claim)
	exitCode := 1
	terminal := requireRuntimeValue(service.RecordRuntimeRunTerminal(context.Background(), runtimeTerminalInput(active, core.RunTerminalInput{State: core.RunExited, ExitCode: &exitCode, TerminalReason: "provider exited", RequestID: "terminal-before-cleanup-outage", OperationID: active.LaunchOperationID})))
	executor := &cleanupBlockingExecutor{}
	controller := &runtimeController{service: service, executor: executor, controlRoot: t.TempDir()}
	if err := controller.cleanupRun(context.Background(), terminal.Run, ref, nil, nil); !errors.Is(err, containerruntime.ErrUnavailable) {
		t.Fatalf("cleanup outage error = %v", err)
	}
	persisted := requireRuntimeValue(service.Run(context.Background(), active.ID))
	if persisted.State != terminal.Run.State || persisted.CleanupState != core.CleanupBlocked ||
		persisted.LastError == "" || executor.removeCalls.Load() != 1 {
		t.Fatalf("cleanup outage guessed removal or changed terminal fact: %#v remove_calls=%d", persisted, executor.removeCalls.Load())
	}
}

type runtimeTestExecutor struct {
	containerruntime.Executor
	inspectCalls atomic.Int32
	stopCalls    atomic.Int32
	waitCalls    atomic.Int32
	logCalls     atomic.Int32
	state        *containerruntime.LiveState
}

type monitorFailureExecutor struct {
	runtimeTestExecutor
	payload                  string
	stopped                  chan struct{}
	once                     sync.Once
	releaseWait, releaseLogs chan struct{}
	beforeStop               func() []byte
	capturedLog              []byte
	removeCalls              atomic.Int32
}

func (e *monitorFailureExecutor) Stop(
	context.Context,
	containerruntime.RuntimeRef,
	time.Duration,
) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	e.once.Do(func() {
		if e.beforeStop != nil {
			e.capturedLog = e.beforeStop()
		}
		close(e.stopped)
	})
	return containerruntime.StopResult{}, nil
}

func (e *monitorFailureExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	if e.releaseLogs != nil {
		<-e.releaseLogs
	}
	return io.NopCloser(strings.NewReader(e.payload)), nil
}

func (e *monitorFailureExecutor) Wait(ctx context.Context, ref containerruntime.RuntimeRef) (containerruntime.ExitFact, error) {
	if e.releaseWait != nil {
		<-e.releaseWait
		return containerruntime.ExitFact{Ref: ref, ExitCode: 143}, nil
	}
	select {
	case <-e.stopped:
		return containerruntime.ExitFact{Ref: ref, ExitCode: 143}, nil
	case <-ctx.Done():
		return containerruntime.ExitFact{}, ctx.Err()
	}
}

func (e *monitorFailureExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	e.removeCalls.Add(1)
	return containerruntime.RemoveResult{}, nil
}

type shutdownReplayExecutor struct {
	runtimeTestExecutor
	payload         string
	ref             containerruntime.RuntimeRef
	firstLogStarted chan struct{}
	stopped         chan struct{}
	stopOnce        sync.Once
	logCalls        atomic.Int32
	stopGrace       atomic.Int64
}

func (e *shutdownReplayExecutor) Logs(
	ctx context.Context,
	_ containerruntime.RuntimeRef,
	_ bool,
) (io.ReadCloser, error) {
	if e.logCalls.Add(1) == 1 {
		close(e.firstLogStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return io.NopCloser(strings.NewReader(e.payload)), nil
}

func (e *shutdownReplayExecutor) Inspect(
	context.Context,
	containerruntime.RuntimeRef,
) (containerruntime.LiveState, error) {
	select {
	case <-e.stopped:
		exit := 143
		return containerruntime.LiveState{Ref: e.ref, Status: containerruntime.StatusExited, ExitCode: &exit}, nil
	default:
		return containerruntime.LiveState{Ref: e.ref, Status: containerruntime.StatusRunning, Running: true, PID: 42}, nil
	}
}

func (e *shutdownReplayExecutor) Wait(
	ctx context.Context,
	_ containerruntime.RuntimeRef,
) (containerruntime.ExitFact, error) {
	select {
	case <-e.stopped:
		return containerruntime.ExitFact{Ref: e.ref, ExitCode: 143}, nil
	case <-ctx.Done():
		return containerruntime.ExitFact{}, ctx.Err()
	}
}

func (e *shutdownReplayExecutor) Stop(
	_ context.Context,
	_ containerruntime.RuntimeRef,
	grace time.Duration,
) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	e.stopGrace.CompareAndSwap(0, int64(grace))
	e.stopOnce.Do(func() { close(e.stopped) })
	return containerruntime.StopResult{}, nil
}

type unavailableRuntimeExecutor struct{ containerruntime.Executor }

func (*unavailableRuntimeExecutor) Ping(context.Context) error {
	return containerruntime.ErrUnavailable
}

type healthyReconcileExecutor struct{ runtimeTestExecutor }

type cleanupBlockingExecutor struct {
	containerruntime.Executor
	removeCalls atomic.Int32
}

func (*cleanupBlockingExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	return containerruntime.StopResult{}, nil
}

func (e *cleanupBlockingExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	e.removeCalls.Add(1)
	return containerruntime.RemoveResult{}, containerruntime.ErrUnavailable
}

type blockingLaunchExecutor struct {
	runtimeTestExecutor
	createEntered chan struct{}
	allowCreate   chan struct{}
	stopped       chan struct{}
	stopOnce      sync.Once
	createOnce    sync.Once
	ref           containerruntime.RuntimeRef
	createCalls   atomic.Int32
	startCalls    atomic.Int32
	waitCalls     atomic.Int32
	logCalls      atomic.Int32
	inspectCalls  atomic.Int32
	inspectErr    error
	removeErr     error
	present       bool
}

type staleInspectExecutor struct {
	runtimeTestExecutor
	ref                      containerruntime.RuntimeRef
	workspace, handoff, head string
	generation               int
	present, stopped         bool
	timedOut, removeFailed   bool
	captured                 gitcapture.Fact
	createCalls              atomic.Int32
}

func (e *staleInspectExecutor) Create(_ context.Context, spec containerruntime.ContainerSpec) (containerruntime.RuntimeRef, error) {
	e.generation++
	e.createCalls.Add(1)
	e.ref, e.ref.ContainerID = spec.Ref, "inspect-container"
	e.present, e.stopped = true, false
	for _, mount := range spec.Mounts {
		if mount.Target == "/workspace" {
			e.workspace = mount.Source
		}
		if mount.Target == "/handoff" {
			e.handoff = mount.Source
		}
	}
	return e.ref, nil
}

func (e *staleInspectExecutor) Start(context.Context, containerruntime.RuntimeRef) (containerruntime.RuntimeRef, error) {
	return e.ref, nil
}

func (e *staleInspectExecutor) Inspect(context.Context, containerruntime.RuntimeRef) (containerruntime.LiveState, error) {
	if !e.present {
		return containerruntime.LiveState{}, containerruntime.ErrNotFound
	}
	return containerruntime.LiveState{Ref: e.ref, Status: containerruntime.StatusRunning, Running: true, MemoryBytes: 1, NanoCPUs: 1, PIDsLimit: 1}, nil
}

func (e *staleInspectExecutor) Wait(ctx context.Context, _ containerruntime.RuntimeRef) (containerruntime.ExitFact, error) {
	if e.generation == 2 && !e.timedOut && !e.stopped {
		e.captured, e.timedOut = e.inspectFact(), true
		<-ctx.Done()
		return containerruntime.ExitFact{}, ctx.Err()
	}
	if e.stopped {
		return containerruntime.ExitFact{Ref: e.ref, ExitCode: 143}, nil
	}
	fact, handoff := e.inspectFact(), e.handoff
	if e.generation == 2 {
		fact = e.captured
	}
	ready := filepath.Join(handoff, gitcapture.InspectReadyName)
	if err := os.MkdirAll(ready, 0o700); err != nil {
		return containerruntime.ExitFact{}, err
	}
	raw, _ := json.Marshal(fact)
	if err := os.WriteFile(filepath.Join(ready, gitcapture.FactsName), raw, 0o600); err != nil {
		return containerruntime.ExitFact{}, err
	}
	return containerruntime.ExitFact{Ref: e.ref}, nil
}

func (e *staleInspectExecutor) inspectFact() gitcapture.Fact {
	status := []byte(nil)
	clean := true
	if _, err := os.Stat(filepath.Join(e.workspace, "mutation")); err == nil {
		status, clean = []byte("mutation"), false
	}
	digest := sha256.Sum256(status)
	return gitcapture.Fact{HeadSHA: e.head, StatusDigest: fmt.Sprintf("%x", digest), ObjectCount: 1, Clean: clean}
}

func (e *staleInspectExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	e.stopped = true
	return containerruntime.StopResult{}, nil
}

func (e *staleInspectExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	if e.generation == 2 && e.timedOut && !e.removeFailed {
		e.removeFailed = true
		return containerruntime.RemoveResult{}, errors.New("remove failed")
	}
	e.present = false
	return containerruntime.RemoveResult{}, nil
}

func (*staleInspectExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func newBlockingLaunchExecutor() *blockingLaunchExecutor {
	return &blockingLaunchExecutor{createEntered: make(chan struct{}), allowCreate: make(chan struct{}), stopped: make(chan struct{})}
}

func (e *blockingLaunchExecutor) Create(ctx context.Context, spec containerruntime.ContainerSpec) (containerruntime.RuntimeRef, error) {
	e.createCalls.Add(1)
	e.ref = spec.Ref
	e.ref.ContainerID = "container-launch-race"
	e.present = true
	e.createOnce.Do(func() { close(e.createEntered) })
	select {
	case <-e.allowCreate:
		return e.ref, nil
	case <-ctx.Done():
		return containerruntime.RuntimeRef{}, ctx.Err()
	}
}

func (e *blockingLaunchExecutor) Attach(context.Context, containerruntime.RuntimeRef) (containerruntime.RuntimeRef, error) {
	return e.ref, nil
}

func (e *blockingLaunchExecutor) Start(context.Context, containerruntime.RuntimeRef) (containerruntime.RuntimeRef, error) {
	e.startCalls.Add(1)
	return e.ref, nil
}

func (e *blockingLaunchExecutor) Inspect(context.Context, containerruntime.RuntimeRef) (containerruntime.LiveState, error) {
	e.inspectCalls.Add(1)
	if e.inspectErr != nil {
		err := e.inspectErr
		e.inspectErr = nil
		return containerruntime.LiveState{}, err
	}
	if !e.present {
		return containerruntime.LiveState{}, containerruntime.ErrNotFound
	}
	select {
	case <-e.stopped:
		exit := 143
		return containerruntime.LiveState{Ref: e.ref, Status: containerruntime.StatusExited, ExitCode: &exit}, nil
	default:
		return containerruntime.LiveState{Ref: e.ref, Status: containerruntime.StatusRunning, Running: true, PID: 42}, nil
	}
}

func (e *blockingLaunchExecutor) Wait(ctx context.Context, _ containerruntime.RuntimeRef) (containerruntime.ExitFact, error) {
	e.waitCalls.Add(1)
	select {
	case <-e.stopped:
		exit := 143
		return containerruntime.ExitFact{Ref: e.ref, ExitCode: exit}, nil
	case <-ctx.Done():
		return containerruntime.ExitFact{}, ctx.Err()
	}
}

func (e *blockingLaunchExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	e.logCalls.Add(1)
	return io.NopCloser(strings.NewReader("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"launch-race-session\"}\n")), nil
}

func (e *blockingLaunchExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	e.stopOnce.Do(func() { close(e.stopped) })
	return containerruntime.StopResult{}, nil
}

func (e *blockingLaunchExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	if e.removeErr != nil {
		return containerruntime.RemoveResult{}, e.removeErr
	}
	e.present = false
	return containerruntime.RemoveResult{}, nil
}

func (e *runtimeTestExecutor) Ping(context.Context) error { return nil }

func (e *runtimeTestExecutor) Inspect(context.Context, containerruntime.RuntimeRef) (containerruntime.LiveState, error) {
	e.inspectCalls.Add(1)
	if e.state != nil {
		return *e.state, nil
	}
	return containerruntime.LiveState{}, containerruntime.ErrNotFound
}

func (e *runtimeTestExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	return containerruntime.StopResult{}, nil
}

func (e *runtimeTestExecutor) Wait(_ context.Context, ref containerruntime.RuntimeRef) (containerruntime.ExitFact, error) {
	e.waitCalls.Add(1)
	return containerruntime.ExitFact{Ref: ref, ExitCode: 143}, nil
}

func (*runtimeTestExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	return containerruntime.RemoveResult{}, nil
}

func (e *runtimeTestExecutor) Managed(context.Context) ([]containerruntime.LiveState, error) {
	return nil, nil
}

func (e *runtimeTestExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	e.logCalls.Add(1)
	return nil, containerruntime.ErrUnsupported
}

type runtimeTestGit struct{ sha string }

func (g *runtimeTestGit) Preflight(context.Context, string, string) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{Source: "/source", SourceRef: "refs/heads/main", InitialSHA: g.sha, CanonicalRef: "refs/heads/main", CanonicalSHA: g.sha}, nil
}

func (g *runtimeTestGit) ControlPath(projectID string) string {
	return "/control/" + projectID + ".git"
}

func (g *runtimeTestGit) Initialize(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}

func (g *runtimeTestGit) Verify(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}

func (*runtimeTestGit) Exists(string) bool { return true }

func (g *runtimeTestGit) Resolve(context.Context, string, string) (string, error) { return g.sha, nil }

func newRuntimeTestService(t *testing.T) *core.Service {
	t.Helper()
	database := requireRuntimeValue(store.Open(context.Background(), filepath.Join(t.TempDir(), "coordplane.db")))
	t.Cleanup(func() { _ = database.Close() })
	service := requireRuntimeValue(core.NewService(database, &runtimeTestGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, core.ServiceOptions{MaxParallelRuns: 4, AdapterIDs: []string{"claude"}}))
	service.SetReady(true, "")
	return service
}

func claimRuntimeTestRun(t *testing.T) (*core.Service, core.Claim) {
	t.Helper()
	service := newRuntimeTestService(t)
	agent, project := addRuntimeTestProject(service, runtimeTestInstructionsText)
	task := requireRuntimeValue(service.CreateTask(context.Background(), core.CreateTaskInput{ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork, Title: "Runtime task", RequestID: "add-runtime-task"}))
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim runtime test Run: claim=%#v ok=%t err=%v", claim, ok, err)
	}
	return service, claim
}

func newRuntimeTestController(t *testing.T, service *core.Service, executor containerruntime.Executor) *runtimeController {
	return &runtimeController{service: service, executor: executor, adapters: adapter.Production(), controlRoot: t.TempDir(), monitors: make(map[string]*runMonitor), controls: make(map[string]*runControl), runOperations: make(map[string]*runOperation)}
}

// populateRuntimeTestControl writes the control files the launch path would
// have produced (identity, token, launch, secrets, instructions) so a test that
// skips the real container launch still satisfies the redaction lineage
// precondition and the cleanup-time ownership validation. The secrets file
// mirrors the controller's provider allowlist env, and the instructions file
// must byte-match the Agent text behind the run's InstructionsHash exactly or
// redaction fails closed.
func populateRuntimeTestControl(t *testing.T, controller *runtimeController, run core.Run) {
	t.Helper()
	controlPath := filepath.Join(controller.controlRoot, run.ID)
	requireNoError(t, os.MkdirAll(controlPath, runControlDirectoryMode))
	requireNoError(t, os.Chmod(controlPath, runControlDirectoryMode))
	requireNoError(t, writeRunControlMarker(controlPath, run))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, "token"), []byte("test-run-token\n"), runControlFileMode))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte(runtimeLaunchScript), 0o550))
	secrets := map[string]string{}
	for _, name := range controller.config.Runtime.ProviderEnvAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			secrets[name] = value
		}
	}
	raw := requireRuntimeValue(serializeRunSecretsFile(secrets))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), raw, runControlFileMode))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeInstructionsFile), []byte(runtimeTestInstructionsText), runControlFileMode))
}

const runtimeTestInstructionsText = "Run only the assigned conversation."

func addRuntimeTestProject(service *core.Service, instructionsText string) (core.Agent, core.Project) {
	ctx := context.Background()
	agent := requireRuntimeValue(service.AddAgent(ctx, core.AddAgentInput{DisplayName: "Runtime Agent", AdapterID: "claude", Image: "agent:test", InstructionsText: instructionsText, RequestID: "add-runtime-agent"}))
	project := requireRuntimeValue(service.AddProject(ctx, core.AddProjectInput{Name: "Runtime Project", Source: "/source", SourceRef: "refs/heads/main", IntegrationAgentID: agent.ID, RequestID: "add-runtime-project"}))
	return agent, project
}

func activateRuntimeTestRun(t *testing.T, service *core.Service, claim core.Claim) (core.Run, containerruntime.RuntimeRef) {
	t.Helper()
	root := t.TempDir()
	launch := requireRuntimeValue(service.RuntimeLaunchContext(context.Background(), claim.Run.ID))
	prepared := requireRuntimeValue(service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "active-nonce",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: launch.InstructionsHash,
		ConfigFingerprint: launch.ConfigFingerprint,
		LaunchMode:        "start", CleanupOperationID: "active-cleanup", RequestID: "prepare-active-run",
	}))
	ref := runtimeRef(prepared)
	ref.ContainerID = "container-active"
	created := requireRuntimeValue(service.RecordContainerCreated(context.Background(), runtimeFactInput(prepared, ref, "created-test")))
	started := requireRuntimeValue(service.RecordRunStartIssued(context.Background(), runtimeFactInput(created, ref, "started-test")))
	active := requireRuntimeValue(service.ObserveProcessAndActivateRun(context.Background(), runtimeFactInput(started, ref, "active-test")))
	return active, ref
}

func requireRuntimeValue[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func waitForRuntimeTestRun(t *testing.T, service *core.Service, runID string, ready func(core.Run) bool) core.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run := requireRuntimeValue(service.Run(context.Background(), runID))
		if ready(run) {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := service.Run(context.Background(), runID)
	t.Fatalf("Run did not reach expected state: run=%#v err=%v", run, err)
	return core.Run{}
}

func waitRuntimeSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatal(message)
	}
}
