package daemon

import (
	"context"
	"errors"
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
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
)

func TestRunOperationOwnershipSerializesRuntimeSideEffects(t *testing.T) {
	controller := &runtimeController{
		monitors:      make(map[string]*runMonitor),
		runOperations: make(map[string]*runOperation),
	}
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

func TestReconcileSkipsRunOwnedBetweenClaimAndMonitorRegistration(t *testing.T) {
	service := newRuntimeTestService(t)
	root := t.TempDir()
	instructions := filepath.Join(root, "instructions.md")
	if err := os.WriteFile(instructions, []byte("Run only the assigned conversation."), 0o600); err != nil {
		t.Fatal(err)
	}
	coordlink := filepath.Join(root, "coordlink")
	if err := os.WriteFile(coordlink, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	agent, err := service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Launch Race Agent", AdapterID: "codex", Image: "agent:test",
		InstructionsFile: instructions, RequestID: "add-launch-race-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.AddProject(context.Background(), core.AddProjectInput{
		Name: "Launch Race Project", Source: "/source", SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "add-launch-race-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "exercise launch ownership",
		Wake: true, RequestID: "wake-launch-race",
	}); err != nil {
		t.Fatal(err)
	}
	executor := newBlockingLaunchExecutor()
	cfg := config.Config{DataDir: root, Runtime: config.RuntimeConfig{
		DockerNetwork: "none", WorkspaceRoot: filepath.Join(root, "workspaces"),
		AgentHomeRoot: filepath.Join(root, "homes"), LogRoot: filepath.Join(root, "logs"),
	}}
	controller := newRuntimeController(cfg, service, executor, adapter.Production(), nil, coordlink)
	controller.ctx, controller.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { _ = controller.Close() })
	claim, operation, ok, err := controller.claimNext(context.Background())
	if err != nil || !ok {
		t.Fatalf("claim runtime test Run: ok=%t err=%v", ok, err)
	}
	launchDone := make(chan error, 1)
	go func() { launchDone <- controller.launchOwned(context.Background(), claim, operation) }()
	select {
	case <-executor.createEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("launch did not reach the blocked Create boundary")
	}

	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executor.inspectCalls.Load() != 0 {
		t.Fatalf("reconcile inspected a Run owned by launch: calls=%d", executor.inspectCalls.Load())
	}
	persisted, err := service.Run(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.RunStarting || persisted.LaunchNonce == "" || persisted.ContainerID != "" {
		t.Fatalf("reconcile mutated launch-owned Run: %#v", persisted)
	}
	close(executor.allowCreate)
	select {
	case err := <-launchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch did not finish after Create was released")
	}
	active := waitForRuntimeTestRun(t, service, claim.Run.ID, func(run core.Run) bool {
		return run.State == core.RunActive
	})
	if executor.createCalls.Load() != 1 || executor.startCalls.Load() != 1 ||
		executor.waitCalls.Load() != 1 || executor.logCalls.Load() != 1 {
		t.Fatalf("launch/reconcile duplicated runtime side effects: create=%d start=%d wait=%d logs=%d",
			executor.createCalls.Load(), executor.startCalls.Load(), executor.waitCalls.Load(), executor.logCalls.Load())
	}
	if controller.monitor(active.ID) == nil {
		t.Fatal("active Run has no registered monitor")
	}
	if _, err := service.RequestRuntimeStop(context.Background(), core.RunStopInput{
		RunID: active.ID, Reason: "race assertion complete", OperationID: "stop-launch-race",
		RequestID: "stop-launch-race-request",
	}); err != nil {
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
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	snapshot, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim stale-snapshot Run: ok=%t err=%v", ok, err)
	}
	terminal, err := service.RecordRuntimeRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: snapshot.Run.ID, Generation: snapshot.Run.Generation,
		LaunchOperationID: snapshot.Run.LaunchOperationID,
		State:             core.RunFailed, TerminalReason: "launch owner already converged",
		RuntimeErrorCode: "LAUNCH_OWNER_FAILED", RequestID: "terminal-before-reconcile-owner",
		OperationID: snapshot.Run.LaunchOperationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtimeTestExecutor{}
	controller := &runtimeController{service: service, executor: executor}

	if err := controller.reconcileOwnedRun(context.Background(), snapshot.Run); err != nil {
		t.Fatalf("reconcile stale snapshot after ownership handoff: %v", err)
	}
	if executor.inspectCalls.Load() != 0 {
		t.Fatalf("stale terminal Run reached Docker inspect: calls=%d", executor.inspectCalls.Load())
	}
	persisted, err := service.Run(context.Background(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != terminal.Run.State || persisted.TerminalReason != terminal.Run.TerminalReason ||
		persisted.RuntimeErrorCode != terminal.Run.RuntimeErrorCode {
		t.Fatalf("stale reconciliation changed canonical terminal fact: %#v", persisted)
	}
}

func TestShutdownPersistsIntentBeforeCancellingMonitorAndConverges(t *testing.T) {
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim shutdown Run: ok=%t err=%v", ok, err)
	}
	active, _ := activateRuntimeTestRun(t, service, claim)
	root := t.TempDir()
	controller := &runtimeController{
		service: service, executor: &runtimeTestExecutor{}, adapters: adapter.Production(), controlRoot: root,
		monitors: make(map[string]*runMonitor), controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}
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
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case durable := <-intentBeforeCancel:
		if !durable {
			t.Fatal("monitor Wait was cancelled before daemon_shutdown intent became durable")
		}
	default:
		t.Fatal("shutdown did not cancel the registered monitor")
	}
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.RunInterrupted || persisted.StopReason != runtimeShutdownReason ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("shutdown Run did not converge: %#v", persisted)
	}
	if _, _, ok, err := controller.claimNext(context.Background()); err != nil || ok {
		t.Fatalf("shutdown controller admitted another Run: ok=%t err=%v", ok, err)
	}
}

func TestShutdownConvergesNonceLessRun(t *testing.T) {
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim unprepared shutdown Run: ok=%t err=%v", ok, err)
	}
	executor := &runtimeTestExecutor{}
	controller := &runtimeController{
		service: service, executor: executor, adapters: adapter.Production(), controlRoot: t.TempDir(),
		monitors: make(map[string]*runMonitor), controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := service.Run(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim runtime task: ok=%t err=%v", ok, err)
	}
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := &runtimeTestExecutor{}
	controller := &runtimeController{service: service, executor: executor}

	controller.stopForDurableIntent(&runMonitor{runID: active.ID, ref: ref})
	if executor.stopCalls.Load() != 0 {
		t.Fatalf("false outcome signal stopped healthy container: calls=%d", executor.stopCalls.Load())
	}
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	controller := &runtimeController{
		service: service, monitors: map[string]*runMonitor{monitor.runID: monitor},
	}
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
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim session-failure Run: ok=%t err=%v", ok, err)
	}
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := newMonitorFailureExecutor()
	monitor := &runMonitor{
		runID: active.ID, ref: ref,
		wait: make(chan waitResult, 1), logs: make(chan error, 1),
	}
	controller := &runtimeController{
		service: service, executor: executor, controlRoot: t.TempDir(),
		monitors: map[string]*runMonitor{active.ID: monitor}, controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}
	monitor.logs <- errors.Join(errRuntimeSessionPersist, errors.New("injected persistence failure"))
	go func() {
		<-executor.stopped
		monitor.wait <- waitResult{fact: containerruntime.ExitFact{Ref: ref, ExitCode: 143}}
	}()

	controller.supervise(monitor)
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.StopRequestedAt == "" || persisted.StopReason != runtimeLogFailureReason ||
		persisted.State != core.RunInterrupted || persisted.RuntimeErrorCode != runtimeSessionFailureCode ||
		persisted.CleanupState != core.CleanupRemoved {
		t.Fatalf("session persistence failure did not durably stop and converge Run: %#v", persisted)
	}
	if executor.stopCalls.Load() == 0 {
		t.Fatal("session persistence failure left the container running")
	}
}

func TestShutdownReplaysTailLogsBeforeTerminalConvergence(t *testing.T) {
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim shutdown replay Run: ok=%t err=%v", ok, err)
	}
	active, ref := activateRuntimeTestRun(t, service, claim)
	executor := &shutdownReplayExecutor{
		runtimeTestExecutor: runtimeTestExecutor{},
		payload:             "{\"type\":\"thread.started\",\"thread_id\":\"shutdown-tail-session\"}\n",
		ref:                 ref,
		firstLogStarted:     make(chan struct{}),
		stopped:             make(chan struct{}),
	}
	controller := &runtimeController{
		config:  config.Config{MaxParallelRuns: 1, Runtime: config.RuntimeConfig{ShutdownGrace: 275 * time.Millisecond}},
		service: service, executor: executor,
		adapters: adapter.Production(), controlRoot: t.TempDir(),
		monitors: make(map[string]*runMonitor), controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}
	controller.ctx, controller.cancel = context.WithCancel(context.Background())
	entry, ok := controller.adapters.Lookup(active.AdapterID)
	if !ok {
		t.Fatalf("lookup shutdown replay adapter %q", active.AdapterID)
	}
	monitor := controller.newMonitor(active, ref, entry, nil)
	controller.monitors[active.ID] = monitor
	controller.wg.Add(1)
	go func() {
		defer controller.wg.Done()
		controller.supervise(monitor)
	}()
	select {
	case <-executor.firstLogStarted:
	case <-time.After(time.Second):
		t.Fatal("initial Docker log follow did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim unavailable-runtime task: ok=%t err=%v", ok, err)
	}
	active, _ := activateRuntimeTestRun(t, service, claim)
	controller := &runtimeController{
		service: service, executor: &unavailableRuntimeExecutor{},
		monitors: make(map[string]*runMonitor), controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	healthy, reason := controller.Healthy()
	if healthy || reason == "" {
		t.Fatalf("Docker outage did not degrade runtime: healthy=%t reason=%q", healthy, reason)
	}
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != active.State || persisted.ContainerID != active.ContainerID ||
		persisted.LaunchPhase != active.LaunchPhase || persisted.CleanupState != active.CleanupState {
		t.Fatalf("Docker outage rewrote live Run: before=%#v after=%#v", active, persisted)
	}
}

func TestUnavailableContainerRemovalKeepsTerminalCleanupBlocked(t *testing.T) {
	service := newRuntimeTestService(t)
	addRuntimeTestTask(t, service)
	claim, ok, err := service.ClaimNext(context.Background(), "")
	if err != nil || !ok {
		t.Fatalf("claim cleanup task: ok=%t err=%v", ok, err)
	}
	active, ref := activateRuntimeTestRun(t, service, claim)
	exitCode := 1
	terminal, err := service.RecordRuntimeRunTerminal(context.Background(), runtimeTerminalInput(active, core.RunTerminalInput{
		State: core.RunExited, ExitCode: &exitCode, TerminalReason: "provider exited",
		RequestID: "terminal-before-cleanup-outage", OperationID: active.LaunchOperationID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	executor := &cleanupBlockingExecutor{}
	controller := &runtimeController{service: service, executor: executor, controlRoot: t.TempDir()}
	if err := controller.cleanupRun(context.Background(), terminal.Run, ref, nil, nil); !errors.Is(err, containerruntime.ErrUnavailable) {
		t.Fatalf("cleanup outage error = %v", err)
	}
	persisted, err := service.Run(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != terminal.Run.State || persisted.CleanupState != core.CleanupBlocked ||
		persisted.LastError == "" || executor.removeCalls.Load() != 1 {
		t.Fatalf("cleanup outage guessed removal or changed terminal fact: %#v remove_calls=%d", persisted, executor.removeCalls.Load())
	}
}

type runtimeTestExecutor struct {
	containerruntime.Executor
	inspectCalls atomic.Int32
	stopCalls    atomic.Int32
}

type monitorFailureExecutor struct {
	runtimeTestExecutor
	stopped chan struct{}
	once    sync.Once
}

func newMonitorFailureExecutor() *monitorFailureExecutor {
	return &monitorFailureExecutor{stopped: make(chan struct{})}
}

func (e *monitorFailureExecutor) Stop(
	context.Context,
	containerruntime.RuntimeRef,
	time.Duration,
) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	e.once.Do(func() { close(e.stopped) })
	return containerruntime.StopResult{}, nil
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
	containerruntime.Executor
	createEntered chan struct{}
	allowCreate   chan struct{}
	stopped       chan struct{}
	stopOnce      sync.Once
	ref           containerruntime.RuntimeRef
	createCalls   atomic.Int32
	startCalls    atomic.Int32
	waitCalls     atomic.Int32
	logCalls      atomic.Int32
	inspectCalls  atomic.Int32
}

func newBlockingLaunchExecutor() *blockingLaunchExecutor {
	return &blockingLaunchExecutor{
		createEntered: make(chan struct{}), allowCreate: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func (*blockingLaunchExecutor) Ping(context.Context) error { return nil }

func (e *blockingLaunchExecutor) Create(ctx context.Context, spec containerruntime.ContainerSpec) (containerruntime.RuntimeRef, error) {
	e.createCalls.Add(1)
	e.ref = spec.Ref
	e.ref.ContainerID = "container-launch-race"
	close(e.createEntered)
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

func (*blockingLaunchExecutor) Inject(context.Context, containerruntime.RuntimeRef, []byte) (containerruntime.InjectResult, error) {
	return containerruntime.InjectResult{}, containerruntime.ErrUnsupported
}

func (e *blockingLaunchExecutor) Inspect(context.Context, containerruntime.RuntimeRef) (containerruntime.LiveState, error) {
	e.inspectCalls.Add(1)
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
		return containerruntime.ExitFact{Ref: e.ref, ExitCode: 143}, nil
	case <-ctx.Done():
		return containerruntime.ExitFact{}, ctx.Err()
	}
}

func (e *blockingLaunchExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	e.logCalls.Add(1)
	return io.NopCloser(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"launch-race-session\"}\n")), nil
}

func (e *blockingLaunchExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	e.stopOnce.Do(func() { close(e.stopped) })
	return containerruntime.StopResult{}, nil
}

func (*blockingLaunchExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	return containerruntime.RemoveResult{}, nil
}

func (*blockingLaunchExecutor) Managed(context.Context) ([]containerruntime.LiveState, error) {
	return nil, nil
}

func (e *runtimeTestExecutor) Ping(context.Context) error { return nil }

func (e *runtimeTestExecutor) Inspect(context.Context, containerruntime.RuntimeRef) (containerruntime.LiveState, error) {
	e.inspectCalls.Add(1)
	return containerruntime.LiveState{}, containerruntime.ErrNotFound
}

func (e *runtimeTestExecutor) Stop(context.Context, containerruntime.RuntimeRef, time.Duration) (containerruntime.StopResult, error) {
	e.stopCalls.Add(1)
	return containerruntime.StopResult{}, nil
}

func (*runtimeTestExecutor) Wait(_ context.Context, ref containerruntime.RuntimeRef) (containerruntime.ExitFact, error) {
	return containerruntime.ExitFact{Ref: ref, ExitCode: 143}, nil
}

func (*runtimeTestExecutor) Remove(context.Context, containerruntime.RuntimeRef) (containerruntime.RemoveResult, error) {
	return containerruntime.RemoveResult{}, nil
}

func (e *runtimeTestExecutor) Managed(context.Context) ([]containerruntime.LiveState, error) {
	return nil, nil
}

func (e *runtimeTestExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	return nil, containerruntime.ErrUnsupported
}

type runtimeTestGit struct {
	sha string
}

func (g *runtimeTestGit) Preflight(context.Context, string, string) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{
		Source: "/source", SourceRef: "refs/heads/main", InitialSHA: g.sha,
		CanonicalRef: "refs/heads/main", CanonicalSHA: g.sha,
	}, nil
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
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service, err := core.NewService(database, &runtimeTestGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, core.ServiceOptions{MaxParallelRuns: 4, AdapterIDs: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	service.SetReady(true, "")
	return service
}

func addRuntimeTestTask(t *testing.T, service *core.Service) core.Task {
	t.Helper()
	ctx := context.Background()
	agent, err := service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Runtime Agent", AdapterID: "codex", Image: "agent:test",
		InstructionsFile: "/instructions/runtime.md", RequestID: "add-runtime-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.AddProject(ctx, core.AddProjectInput{
		Name: "Runtime Project", Source: "/source", SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "add-runtime-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "Runtime task", RequestID: "add-runtime-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func activateRuntimeTestRun(t *testing.T, service *core.Service, claim core.Claim) (core.Run, containerruntime.RuntimeRef) {
	t.Helper()
	root := t.TempDir()
	prepared, err := service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "active-nonce",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: "instructions-hash",
		LaunchMode: "start", CleanupOperationID: "active-cleanup", RequestID: "prepare-active-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := runtimeRef(prepared)
	ref.ContainerID = "container-active"
	created, err := service.RecordContainerCreated(context.Background(), runtimeFactInput(prepared, ref, "created-test"))
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.RecordRunStartIssued(context.Background(), runtimeFactInput(created, ref, "started-test"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ObserveProcessAndActivateRun(context.Background(), runtimeFactInput(started, ref, "active-test"))
	if err != nil {
		t.Fatal(err)
	}
	return active, ref
}

func waitForRuntimeTestRun(t *testing.T, service *core.Service, runID string, ready func(core.Run) bool) core.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.Run(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if ready(run) {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := service.Run(context.Background(), runID)
	t.Fatalf("Run did not reach expected state: run=%#v err=%v", run, err)
	return core.Run{}
}
