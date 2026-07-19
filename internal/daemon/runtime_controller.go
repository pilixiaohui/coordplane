package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"coordplane/internal/adapter"
	"coordplane/internal/config"
	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
	"coordplane/internal/perfobs"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/transport"
)

const (
	runtimeContainerUID       = 65532
	runtimeLogDrainTimeout    = 10 * time.Second
	runtimeTmpfsLimit         = 64 << 20
	runtimeLogLimit           = 8 << 20
	runtimeRejectedFrameLimit = 1 << 10
	runtimeRejectedErrorLimit = 256
	runtimeRejectedLogReserve = 8 << 10
	runtimeLogTruncatedMarker = "[coordplane: runtime log truncated]\n"
	runtimeLogFailureReason   = "runtime log monitoring failed"
	runtimeLogFailureCode     = "LOG_STREAM_FAILED"
	runtimeSessionFailureCode = "SESSION_PERSIST_FAILED"
)

var (
	errRuntimeLogDrainTimeout = errors.New("runtime log stream did not finish")
	errRuntimeSessionPersist  = errors.New("runtime session event was not persisted")
)

type runtimeController struct {
	config      config.Config
	service     *core.Service
	executor    containerruntime.Executor
	adapters    adapter.Registry
	workspaces  *gitrepo.WorkspaceManager
	coordlink   string
	controlRoot string

	mu                      sync.Mutex
	claimMu                 sync.Mutex
	ctx                     context.Context
	cancel                  context.CancelFunc
	shutdownCtx             context.Context
	started                 bool
	shuttingDown            bool
	degradedReason          string
	schedulerDegradedReason string
	monitors                map[string]*runMonitor
	controls                map[string]*runControl
	runOperations           map[string]*runOperation
	wg                      sync.WaitGroup
}

type runOperation struct{ _ byte }

type runControl struct {
	server  *transport.UnixServer
	done    chan error
	outcome chan struct{}
	path    string

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type runMonitor struct {
	runID   string
	ref     containerruntime.RuntimeRef
	entry   adapter.CLI
	control *runControl
	redact  runtimeRedaction

	waitCancel context.CancelFunc
	wait       chan waitResult
	// waitDelivered is test-only observation of supervisor receipt of Wait.
	waitDelivered chan struct{}
	logs          chan error

	mu          sync.Mutex
	runtimeCode string
	lastError   string
	logsDone    bool
	logErr      error
}

type waitResult struct {
	fact containerruntime.ExitFact
	err  error
}

type runtimePrepareState struct {
	controller    *runtimeController
	ctx           context.Context
	claim         core.Claim
	launch        core.RunLaunchContext
	entry         adapter.CLI
	workspaceSpec gitrepo.WorkspaceSpec
	run           core.Run
	workspacePath string
	homePath      string
	logPath       string
	controlPath   string
	bootstrap     string
	ref           containerruntime.RuntimeRef
	control       *runControl
	quickExit     bool
	complete      bool
	completionErr error
}

type runtimePrepareStep struct {
	name        string
	failureCode string
	run         func(*runtimePrepareState) error
}

var runtimePrepareSteps = []runtimePrepareStep{
	{name: "prepareWorkspace", failureCode: "WORKSPACE_PREPARE_FAILED", run: prepareRuntimeWorkspace},
	{name: "prepareAgentHome", failureCode: "RUNTIME_DIRECTORY_PREPARE_FAILED", run: prepareRuntimeDirectories},
	{name: "writeRunToken", failureCode: "TOKEN_PREPARE_FAILED", run: writeRuntimeToken},
	{name: "writeBootstrap", failureCode: "BOOTSTRAP_PREPARE_FAILED", run: writeRuntimeBootstrap},
	{name: "openRunAPISocket", failureCode: "RUN_SOCKET_PREPARE_FAILED", run: openRuntimeControl},
	{name: "createContainer", failureCode: "CONTAINER_CREATE_FAILED", run: createRuntimeContainer},
	{name: "attachStreams", failureCode: "CONTAINER_ATTACH_FAILED", run: attachRuntimeStreams},
	{name: "startCLI", failureCode: "CONTAINER_START_FAILED", run: startRuntimeCLI},
	{name: "verifyLive", failureCode: "PROCESS_OBSERVE_FAILED", run: verifyRuntimeLive},
}

func newRuntimeController(
	cfg config.Config,
	service *core.Service,
	executor containerruntime.Executor,
	registry adapter.Registry,
	workspaces *gitrepo.WorkspaceManager,
	coordlink string,
) *runtimeController {
	return &runtimeController{
		config: cfg, service: service, executor: executor, adapters: registry,
		workspaces: workspaces, coordlink: coordlink,
		controlRoot: filepath.Join(cfg.DataDir, "run-control"),
		monitors:    make(map[string]*runMonitor), controls: make(map[string]*runControl),
		runOperations: make(map[string]*runOperation),
	}
}

func (c *runtimeController) shutdownGrace() time.Duration {
	if c != nil && c.config.Runtime.ShutdownGrace > 0 {
		return c.config.Runtime.ShutdownGrace
	}
	return config.DefaultShutdownGrace
}

func (c *runtimeController) Start(parent context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.shuttingDown {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	// Daemon shutdown must persist stop intent before cancelling Docker Wait.
	// The serving context is only the signal that invokes Shutdown; explicit
	// controller cancellation owns the runtime worker lifetime.
	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(parent))
	c.started = true
	for _, worker := range runtimeWorkers {
		worker := worker
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			worker.run(c.ctx, c)
		}()
	}
}

func (c *runtimeController) Healthy() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason := c.currentDegradedReasonLocked()
	return reason == "", reason
}

func (c *runtimeController) setDegraded(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degradedReason = c.normalizedDegradedReason(reason)
	c.publishDegradedStateLocked()
}

func (c *runtimeController) clearDegraded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degradedReason = ""
	c.publishDegradedStateLocked()
}

func (c *runtimeController) setSchedulerDegraded(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.schedulerDegradedReason = c.normalizedDegradedReason(reason)
	c.publishDegradedStateLocked()
}

func (c *runtimeController) clearSchedulerDegraded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schedulerDegradedReason == "" {
		return
	}
	c.schedulerDegradedReason = ""
	c.publishDegradedStateLocked()
}

func (c *runtimeController) schedulerHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.shuttingDown && c.degradedReason == ""
}

func (c *runtimeController) normalizedDegradedReason(reason string) string {
	reason = strings.TrimSpace(c.runtimeRedaction(core.Run{}).Text(reason))
	if reason == "" {
		return "Docker runtime unavailable"
	}
	return reason
}

func (c *runtimeController) currentDegradedReasonLocked() string {
	if c.degradedReason != "" {
		return c.degradedReason
	}
	return c.schedulerDegradedReason
}

func (c *runtimeController) publishDegradedStateLocked() {
	if c.shuttingDown {
		c.service.SetReady(false, "daemon shutting down")
		return
	}
	reason := c.currentDegradedReasonLocked()
	c.service.SetReady(reason == "", reason)
}

func (c *runtimeController) launch(ctx context.Context, claim core.Claim) error {
	operation := c.acquireRunOperation(claim.Run.ID)
	if operation == nil {
		return nil
	}
	return c.launchOwned(ctx, claim, operation)
}

func (c *runtimeController) launchOwned(ctx context.Context, claim core.Claim, operation *runOperation) error {
	defer c.releaseRunOperation(claim.Run.ID, operation)

	launch, err := c.service.RuntimeLaunchContext(ctx, claim.Run.ID)
	if err != nil {
		return err
	}
	entry, ok := c.adapters.Lookup(launch.Run.AdapterID)
	if !ok {
		return c.failUnpreparedRun(ctx, launch.Run, "ADAPTER_NOT_REGISTERED", "adapter is not registered")
	}
	instructions, instructionsHash, err := readInstructions(launch.Agent.InstructionsFile)
	if err != nil {
		message := c.runtimeRedaction(launch.Run, launch.Agent.InstructionsFile).Text(err.Error())
		return c.failUnpreparedRun(ctx, launch.Run, "INSTRUCTIONS_UNAVAILABLE", message)
	}

	workspacePath := ""
	workspaceSpec := gitrepo.WorkspaceSpec{}
	if launch.Task.Kind != core.TaskConversation {
		workspaceSpec, err = gitWorkspaceSpec(launch.Task)
		if err != nil {
			return c.failUnpreparedRun(ctx, launch.Run, "WORKSPACE_INPUT_INVALID", err.Error())
		}
		workspacePath, err = c.workspaces.Path(launch.Project.ID, launch.Task.ID)
		if err != nil {
			return c.failUnpreparedRun(ctx, launch.Run, "WORKSPACE_PATH_INVALID", err.Error())
		}
	}
	homePath := filepath.Join(c.config.Runtime.AgentHomeRoot, launch.Agent.ID)
	logPath := filepath.Join(c.config.Runtime.LogRoot, launch.Run.ID, "run.log")
	controlPath := filepath.Join(c.controlRoot, launch.Run.ID)

	mode, resumedFrom, resumeSession := c.selectLaunchMode(ctx, launch, entry, workspacePath)
	deadline := ""
	if c.config.Runtime.RunTimeout > 0 {
		deadline = time.Now().UTC().Add(c.config.Runtime.RunTimeout).Format(time.RFC3339Nano)
	}
	nonce, err := randomRuntimeID("nonce")
	if err != nil {
		return err
	}
	cleanupOperation, err := randomRuntimeID("cleanup")
	if err != nil {
		return err
	}
	prepared, err := c.service.BeginRunLaunch(ctx, core.RunLaunchInput{
		RunID: launch.Run.ID, Generation: launch.Run.Generation, LaunchNonce: nonce,
		WorkspacePath: workspacePath, HomePath: homePath, LogPath: logPath,
		InstructionsHash: instructionsHash, LaunchMode: mode,
		ResumedFromRunID: resumedFrom, ResumeNativeSessionID: resumeSession,
		CleanupOperationID: cleanupOperation, IsolationSpecVersion: runtimeIsolationSpecVersion(), DeadlineAt: deadline,
		RequestID: runtimeRequest(launch.Run, "prepare"),
	})
	if err != nil {
		return err
	}

	state := &runtimePrepareState{
		controller: c, ctx: ctx, claim: claim, launch: launch, entry: entry,
		workspaceSpec: workspaceSpec, run: prepared, workspacePath: workspacePath,
		homePath: homePath, logPath: logPath, controlPath: controlPath,
		bootstrap: buildBootstrap(launch, prepared, instructions, workspacePath, workspaceSpec),
	}
	fail := func(cause error, code string) error {
		if errors.Is(cause, containerruntime.ErrUnavailable) {
			c.setDegraded(cause.Error())
			return cause
		}
		return c.failPreparedRun(context.Background(), state.run, state.ref, state.control, code, cause)
	}
	for _, step := range runtimePrepareSteps {
		if err := step.run(state); err != nil {
			perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
			return fail(fmt.Errorf("%s: %w", step.name, err), step.failureCode)
		}
		if state.complete {
			return state.completionErr
		}
	}
	return errors.New("runtime prepare list finished without observing the CLI")
}

func prepareRuntimeWorkspace(state *runtimePrepareState) error {
	if state.launch.Task.Kind == core.TaskConversation {
		return nil
	}
	if err := state.controller.prepareWorkspace(state.ctx, state.run, state.workspaceSpec); err != nil {
		return err
	}
	if state.launch.Task.Kind == core.TaskIntegration {
		_, err := state.controller.workspaces.RefreshCanonical(
			state.ctx, state.workspaceSpec, state.launch.Project.ControlRepoPath,
			state.launch.Project.CanonicalRef, state.launch.Project.CanonicalSHA,
		)
		return err
	}
	return nil
}

func prepareRuntimeDirectories(state *runtimePrepareState) error {
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{path: state.homePath, mode: 0o2770},
		{path: filepath.Dir(state.logPath), mode: 0o700},
		{path: state.controlPath, mode: 0o750},
	} {
		if err := ensureRuntimeDirectory(directory.path, directory.mode); err != nil {
			return err
		}
	}
	_, err := canonicalExecutable(state.controller.coordlink)
	return err
}

func writeRuntimeToken(state *runtimePrepareState) error {
	if err := writeRunControlMarker(state.controlPath, state.run); err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(state.controlPath, "token"), []byte(state.claim.Token+"\n"), 0o440)
}

func writeRuntimeBootstrap(state *runtimePrepareState) error {
	return writeRuntimeFile(filepath.Join(state.controlPath, "bootstrap"), []byte(state.bootstrap), 0o440)
}

func openRuntimeControl(state *runtimePrepareState) error {
	control, err := state.controller.openRunControl(state.run, state.controlPath)
	if err != nil {
		return err
	}
	state.control = control
	state.controller.registerControl(state.run.ID, control)
	return afterRuntimeContractPhase(state.ctx, runtimePhaseIntentBeforeCreate, state.run)
}

func createRuntimeContainer(state *runtimePrepareState) error {
	launchSpec := adapter.LaunchSpec{
		BootstrapPath: adapter.ContainerBootstrapPath, Conversation: state.launch.Task.Kind == core.TaskConversation,
		ContainerHome: "/home/agent", ContainerWork: containerWorkingDirectory(state.launch.Task.Kind),
	}
	var command adapter.CommandSpec
	var err error
	if state.run.LaunchMode == "resume" {
		command, err = state.entry.BuildResumeCommand(adapter.ResumeSpec{
			LaunchSpec: launchSpec, NativeSessionID: state.run.ResumeNativeSessionID,
		})
	} else {
		command, err = state.entry.BuildStartCommand(launchSpec)
	}
	if err != nil {
		return err
	}
	spec, err := state.controller.containerSpec(state.run, state.launch.Task.Kind, command, state.controlPath)
	if err != nil {
		return err
	}
	perfobs.StartStage("runtime.container.create_start", state.run.ID, perfobs.Fields{
		OperationID: state.run.LaunchOperationID, ProjectID: state.run.ProjectID,
		TaskID: state.run.TaskID, RunID: state.run.ID,
	})
	state.ref, err = state.controller.executor.Create(state.ctx, spec)
	if err != nil {
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	state.run, err = state.controller.service.RecordContainerCreated(
		state.ctx,
		runtimeFactInput(state.run, state.ref, "created"),
	)
	if err != nil {
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	return afterRuntimeContractPhase(state.ctx, runtimePhaseContainerCreated, state.run)
}

func attachRuntimeStreams(state *runtimePrepareState) error {
	_, err := state.controller.executor.Attach(state.ctx, state.ref)
	return err
}

func startRuntimeCLI(state *runtimePrepareState) error {
	started, err := state.controller.service.RecordRunStartIssued(
		state.ctx,
		runtimeFactInput(state.run, state.ref, "start-issued"),
	)
	if err != nil {
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	state.run = started
	startedRef, err := state.controller.executor.Start(state.ctx, state.ref)
	if errors.Is(err, containerruntime.ErrExited) {
		state.quickExit = true
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "exited")
		return nil
	}
	if err != nil {
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	state.ref = startedRef
	return nil
}

func verifyRuntimeLive(state *runtimePrepareState) error {
	monitor := state.controller.newMonitor(state.run, state.ref, state.entry, state.control)
	if err := state.controller.registerMonitor(monitor); err != nil {
		_ = monitor.cancelAndCollectLogs(time.Second)
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	finish := func(result waitResult) {
		state.controller.unregisterMonitor(monitor)
		state.complete = true
		state.completionErr = state.controller.finishObservedRun(state.run, monitor, result)
	}
	if state.quickExit {
		finish(<-monitor.wait)
		return nil
	}
	live, err := state.controller.executor.Inspect(state.ctx, state.ref)
	if err != nil {
		state.controller.unregisterMonitor(monitor)
		_ = monitor.cancelAndCollectLogs(time.Second)
		perfobs.EndStage("runtime.container.create_start", state.run.ID, "error")
		return err
	}
	if !live.Running {
		finish(<-monitor.wait)
		return nil
	}
	perfobs.RuntimeLimit(perfobs.Fields{ProjectID: state.run.ProjectID, TaskID: state.run.TaskID, RunID: state.run.ID}, "agent", live.MemoryBytes, live.NanoCPUs, live.PIDsLimit)
	perfobs.EndStage("runtime.container.create_start", state.run.ID, "success")
	select {
	case result := <-monitor.wait:
		finish(result)
		return nil
	default:
	}
	active, err := state.controller.service.ObserveProcessAndActivateRun(
		state.ctx,
		runtimeFactInput(state.run, state.ref, "active"),
	)
	if err != nil {
		state.controller.unregisterMonitor(monitor)
		_ = monitor.cancelAndCollectLogs(time.Second)
		return err
	}
	if len(state.launch.Messages) > 0 {
		ids := make([]string, 0, len(state.launch.Messages))
		for _, message := range state.launch.Messages {
			ids = append(ids, message.ID)
		}
		_, _ = state.controller.service.RecordMessagesDelivered(state.ctx, core.MessageDeliveryInput{
			RunID: active.ID, MessageIDs: ids, RequestID: runtimeRequest(active, "input"),
			OperationID: active.LaunchOperationID,
		})
	}
	if err := afterRuntimeContractPhase(state.ctx, runtimePhaseProcessObserved, active); err != nil {
		return err
	}
	state.controller.wg.Add(1)
	go func() {
		defer state.controller.wg.Done()
		state.controller.supervise(monitor)
	}()
	state.complete = true
	return nil
}

func (c *runtimeController) prepareWorkspace(ctx context.Context, run core.Run, spec gitrepo.WorkspaceSpec) error {
	path, err := c.workspaces.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		_, err = c.workspaces.Verify(ctx, spec)
		return err
	case !errors.Is(statErr, os.ErrNotExist):
		return statErr
	}
	started, err := c.service.TaskHasStartedRun(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if started {
		return errors.New("workspace is missing after an earlier Run reached start_issued")
	}
	_, err = c.workspaces.Materialize(ctx, spec)
	return err
}

func (c *runtimeController) selectLaunchMode(
	ctx context.Context,
	launch core.RunLaunchContext,
	entry adapter.CLI,
	workspacePath string,
) (mode, resumedFrom, session string) {
	if !entry.Metadata().SupportsResume {
		return "start", "", ""
	}
	previous, err := c.service.LatestTerminalRun(ctx, launch.Task.ID, launch.Agent.ID)
	if err != nil || previous.AdapterID != launch.Run.AdapterID {
		return "start", "", ""
	}
	if previous.RuntimeErrorCode == string(core.CodeResumeUnavailable) {
		return "start", previous.ID, ""
	}
	if previous.NativeSessionID == "" {
		return "start", "", ""
	}
	workspaceID := workspacePath
	if launch.Task.Kind == core.TaskConversation {
		workspaceID = "conversation"
	}
	previousWorkspace := previous.WorkspacePath
	if launch.Task.Kind == core.TaskConversation {
		previousWorkspace = "conversation"
	}
	compatible := entry.ResumeCompatible(
		adapter.SessionContext{AdapterID: previous.AdapterID, AgentID: previous.AgentID, TaskID: previous.TaskID, WorkspaceID: previousWorkspace},
		adapter.SessionContext{AdapterID: launch.Run.AdapterID, AgentID: launch.Run.AgentID, TaskID: launch.Run.TaskID, WorkspaceID: workspaceID},
	)
	if !compatible {
		return "start", "", ""
	}
	return "resume", previous.ID, previous.NativeSessionID
}

func (c *runtimeController) containerSpec(
	run core.Run,
	kind core.TaskKind,
	command adapter.CommandSpec,
	controlPath string,
) (containerruntime.ContainerSpec, error) {
	memoryBytes := int64(512 << 20)
	switch run.IsolationSpecVersion {
	case 0, core.RunIsolationSpecCurrent:
	case core.RunIsolationSpecV1:
		memoryBytes = 1 << 30
	default:
		return containerruntime.ContainerSpec{}, fmt.Errorf("unsupported Run isolation spec version %d", run.IsolationSpecVersion)
	}
	coordlink, err := canonicalExecutable(c.coordlink)
	if err != nil {
		return containerruntime.ContainerSpec{}, err
	}
	env := make(map[string]string, len(command.Env)+2+len(c.config.Runtime.ProviderEnvAllowlist))
	for _, key := range c.config.Runtime.ProviderEnvAllowlist {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	for key, value := range command.Env {
		env[key] = value
	}
	env["COORDPLANE_RUN_SOCKET"] = "/run/coordplane/api.sock"
	env["COORDPLANE_RUN_TOKEN_FILE"] = "/run/coordplane/token"
	mounts := []containerruntime.Mount{
		{Source: run.HomePath, Target: "/home/agent"},
		{Source: coordlink, Target: "/usr/local/bin/coordlink", ReadOnly: true},
		{Source: controlPath, Target: "/run/coordplane"},
	}
	if kind != core.TaskConversation {
		mounts = append([]containerruntime.Mount{{Source: run.WorkspacePath, Target: "/workspace/project"}}, mounts...)
	}
	gid := strconv.Itoa(os.Getgid())
	return containerruntime.ContainerSpec{
		Ref: runtimeRef(run), Image: run.Image,
		Command:          containerruntime.CommandSpec{Executable: command.Executable, Args: command.Args, Env: env},
		SensitiveEnvKeys: append([]string(nil), c.config.Runtime.ProviderEnvAllowlist...),
		WorkingDir:       containerWorkingDirectory(kind), User: strconv.Itoa(runtimeContainerUID) + ":" + gid,
		GroupAdd: []string{gid}, Network: c.config.Runtime.DockerNetwork,
		Mounts: mounts, ReadOnlyRoot: true,
		Limits: containerruntime.ResourceLimits{
			PIDs: 256, MemoryBytes: memoryBytes, NanoCPUs: 1_000_000_000, TmpfsBytes: runtimeTmpfsLimit,
		},
	}, nil
}

func (c *runtimeController) openRunControl(run core.Run, path string) (*runControl, error) {
	scope := core.RunScope{
		ProjectID: run.ProjectID, AgentID: run.AgentID, TaskID: run.TaskID,
		RunID: run.ID, Generation: run.Generation,
	}
	outcome := make(chan struct{}, 1)
	next := transport.NewScopedRunHandler(c.service, scope)
	handler := notifySuccessfulOutcome(next, outcome)
	server, err := transport.NewUnixServerWithMode(path, filepath.Join(path, "api.sock"), 0o660, handler)
	if err != nil {
		return nil, err
	}
	control := &runControl{
		server: server, done: make(chan error, 1), outcome: outcome, path: path,
		closed: make(chan struct{}),
	}
	go func() { control.done <- server.Serve() }()
	return control, nil
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatusWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(value)
}

func notifySuccessfulOutcome(next http.Handler, outcome chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observed := &responseStatusWriter{ResponseWriter: writer}
		next.ServeHTTP(observed, request)
		if observed.status == 0 {
			observed.status = http.StatusOK
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/task/outcome" &&
			observed.status >= http.StatusOK && observed.status < http.StatusMultipleChoices {
			select {
			case outcome <- struct{}{}:
			default:
			}
		}
	})
}
