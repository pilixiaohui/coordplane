//go:build docker

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
	"coordplane/internal/transport"
)

func TestRT05DaemonSIGKILLCrashPointIntentMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for RT-05: %v", err)
	}
	artifacts := buildRT05ProcessArtifacts(t, ctx)
	phases := []runtimeContractPhase{
		runtimePhaseIntentBeforeCreate,
		runtimePhaseContainerCreated,
		runtimePhaseProcessObserved,
		runtimePhaseProcessExited,
		runtimePhaseTerminalPersisted,
	}
	for _, phase := range phases {
		intents := []string{"stop", "cancel", "timeout"}
		if phase == runtimePhaseProcessObserved || phase == runtimePhaseProcessExited || phase == runtimePhaseTerminalPersisted {
			intents = append([]string{"outcome"}, intents...)
		}
		for _, intent := range intents {
			phase, intent := phase, intent
			t.Run("reachable_recovery/"+string(phase)+"/"+intent, func(t *testing.T) {
				runRT05ProcessCase(t, ctx, executor, artifacts, phase, intent)
			})
		}
	}
	for _, phase := range phases[:2] {
		phase := phase
		t.Run("pre_active_outcome_NA/"+string(phase), func(t *testing.T) {
			runRT05ProcessCase(t, ctx, executor, artifacts, phase, "outcome")
		})
	}
}

func TestRT05EarlyOwnerFallbackIsSoleContainerCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for RT-05: %v", err)
	}
	artifacts := buildRT05ProcessArtifacts(t, ctx)
	root, err := os.MkdirTemp("/tmp", "cp-rt05-owner-fallback-")
	requireNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	databasePath := filepath.Join(root, "data", "coordplane.db")
	disableFallback := os.Getenv("COORDPLANE_CONTRACT_DISABLE_RT05_OWNER_FALLBACK") == "1"
	var taskID string

	t.Run("container_created_sigkill", func(t *testing.T) {
		configPath := writeTestConfig(t, root)
		rawConfig, err := os.ReadFile(configPath)
		requireNoError(t, err)
		rawConfig = []byte(strings.ReplaceAll(string(rawConfig), "  docker_network: coordplane\n", "  docker_network: none\n"))
		requireNoError(t, os.WriteFile(configPath, rawConfig, 0o600))
		instructions := filepath.Join(root, "instructions.md")
		requireNoError(t, os.WriteFile(instructions, []byte("Execute only the owner-fallback crash task."), 0o600))
		seed, err := buildComponents(ctx, configPath)
		requireNoError(t, err)
		agent, err := seed.service.AddAgent(ctx, core.AddAgentInput{
			DisplayName: "RT-05 owner fallback", AdapterID: "claude", Image: artifacts.image,
			InstructionsFile: instructions, RequestID: "rt05-owner-fallback-agent",
		})
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
		project, err := seed.service.AddProject(ctx, core.AddProjectInput{
			Name: "RT-05 owner fallback", Source: createSourceRepository(t, root),
			SourceRef: "refs/heads/main", IntegrationAgentID: agent.ID,
			RequestID: "rt05-owner-fallback-project",
		})
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
		task, err := seed.service.CreateTask(ctx, core.CreateTaskInput{
			ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
			Title: "crash before immutable cleanup registration", MaxRetries: 0,
			RequestID: "rt05-owner-fallback-task",
		})
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
		taskID = task.ID
		requireNoError(t, seed.Close())
		if !disableFallback {
			registerRT05OwnerFallback(t, executor, databasePath, task.ID)
		}

		socket := filepath.Join(root, "data", "operator.sock")
		readyPath := filepath.Join(root, "runtime-phase-ready")
		first := startP3DaemonProcessWithEnv(t, artifacts.daemon, configPath, socket, filepath.Join(root, "daemon-first.log"), []string{
			"COORDPLANE_CONTRACT_RUNTIME_PHASE=" + string(runtimePhaseContainerCreated),
			"COORDPLANE_CONTRACT_RUNTIME_PHASE_READY=" + readyPath,
		})
		t.Cleanup(func() { killP3DaemonProcess(t, first) })
		client, err := transport.NewUnixClient(socket)
		requireNoError(t, err)
		waitForRT05Phase(t, first, readyPath, runtimePhaseContainerCreated)
		created := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
			return run.State == core.RunStarting && run.ContainerID != ""
		})
		killP3DaemonProcess(t, first)
		database, err := store.Open(ctx, databasePath)
		requireNoError(t, err)
		persisted, readErr := database.Run(ctx, created.ID)
		closeErr := database.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read durable container_created Run: read=%v close=%v", readErr, closeErr)
		}
		if persisted.ContainerID == "" || persisted.ContainerID != created.ContainerID {
			t.Fatalf("durable container identity = %q, want %q", persisted.ContainerID, created.ContainerID)
		}
	})

	if disableFallback {
		t.Cleanup(func() { cleanupRT05OwnedRuns(t, executor, databasePath, taskID) })
	}
	assertRT05NoOwnedContainers(t, executor, taskID)
}

type rt05ProcessArtifacts struct {
	root       string
	daemon     string
	coordlink  string
	image      string
	dockerConf string
}

func buildRT05ProcessArtifacts(t *testing.T, ctx context.Context) rt05ProcessArtifacts {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "cp-rt05-matrix-")
	requireNoError(t, err)
	repositoryRoot := daemonRepositoryRoot()
	artifacts := rt05ProcessArtifacts{
		root: root, daemon: filepath.Join(root, "coordplane-contract"),
		coordlink:  filepath.Join(root, "coordlink"),
		image:      "coordplane-rt05-matrix:" + fmt.Sprintf("%x", time.Now().UnixNano()),
		dockerConf: filepath.Join(root, "docker-config"),
	}
	requireNoError(t, os.MkdirAll(artifacts.dockerConf, 0o700))
	for _, target := range []struct {
		output string
		path   string
		tags   string
		env    []string
	}{
		{output: artifacts.daemon, path: "./cmd/coordplane", tags: "contract"},
		{output: artifacts.coordlink, path: "./cmd/coordlink", env: []string{"CGO_ENABLED=0"}},
	} {
		args := []string{"build", "-buildvcs=false"}
		if target.tags != "" {
			args = append(args, "-tags="+target.tags)
		}
		args = append(args, "-o", target.output, target.path)
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = repositoryRoot
		command.Env = append(os.Environ(), target.env...)
		if raw, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build RT-05 binary %s: %v\n%s", target.path, err, raw)
		}
	}
	build := exec.CommandContext(ctx, "docker", "build", "-q", "-t", artifacts.image,
		filepath.Join(repositoryRoot, "internal", "daemon", "testdata", "claude-runtime"))
	build.Env = append(os.Environ(), "DOCKER_CONFIG="+artifacts.dockerConf)
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build RT-05 image: %v\n%s", err, raw)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		remove := exec.CommandContext(cleanup, "docker", "image", "rm", "-f", artifacts.image)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+artifacts.dockerConf)
		_ = remove.Run()
		_ = os.RemoveAll(root)
	})
	return artifacts
}

func runRT05ProcessCase(
	t *testing.T,
	ctx context.Context,
	executor *containerruntime.DockerExecutor,
	artifacts rt05ProcessArtifacts,
	phase runtimeContractPhase,
	intent string,
) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "cp-rt05-case-")
	requireNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configPath := writeTestConfig(t, root)
	rawConfig, err := os.ReadFile(configPath)
	requireNoError(t, err)
	rawConfig = []byte(strings.ReplaceAll(string(rawConfig), "  docker_network: coordplane\n", "  docker_network: none\n"))
	if intent == "timeout" {
		rawConfig = []byte(strings.Replace(string(rawConfig), "  default_image:", "  run_timeout: 2s\n  default_image:", 1))
	}
	requireNoError(t, os.WriteFile(configPath, rawConfig, 0o600))
	instructions := filepath.Join(root, "instructions.md")
	requireNoError(t, os.WriteFile(instructions, []byte("Execute only the assigned crash-matrix task."), 0o600))
	seed, err := buildComponents(ctx, configPath)
	requireNoError(t, err)
	agent, err := seed.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "RT-05 " + string(phase) + " " + intent,
		AdapterID:   "claude", Image: artifacts.image, InstructionsFile: instructions,
		RequestID: "rt05-agent-" + string(phase) + "-" + intent,
	})
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	project, err := seed.service.AddProject(ctx, core.AddProjectInput{
		Name:   "RT-05 " + string(phase) + " " + intent,
		Source: createSourceRepository(t, root), SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "rt05-project-" + string(phase) + "-" + intent,
	})
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	title := "hold until stopped crash matrix"
	if phase == runtimePhaseProcessExited {
		title = "exit after active for crash matrix"
	}
	task, err := seed.service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: title, MaxRetries: 0, RequestID: "rt05-task-" + string(phase) + "-" + intent,
	})
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	requireNoError(t, seed.Close())
	registerRT05OwnerFallback(t, executor, filepath.Join(root, "data", "coordplane.db"), task.ID)

	socket := filepath.Join(root, "data", "operator.sock")
	readyPath := filepath.Join(root, "runtime-phase-ready")
	first := startP3DaemonProcessWithEnv(t, artifacts.daemon, configPath, socket, filepath.Join(root, "daemon-first.log"), []string{
		"COORDPLANE_CONTRACT_RUNTIME_PHASE=" + string(phase),
		"COORDPLANE_CONTRACT_RUNTIME_PHASE_READY=" + readyPath,
	})
	t.Cleanup(func() { killP3DaemonProcess(t, first) })
	client, err := transport.NewUnixClient(socket)
	requireNoError(t, err)

	var crashRun core.Run
	intentDurable := false
	if phase == runtimePhaseTerminalPersisted {
		crashRun = waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
			return run.State == core.RunActive
		})
		registerRT05ImmutableCleanup(t, executor, crashRun)
		if intent == "outcome" {
			waitForRT05DurableProgress(t, ctx, root, crashRun)
		}
		intentDurable = applyRT05Intent(t, ctx, client, root, task, crashRun, intent, true)
		waitForRT05Phase(t, first, readyPath, phase)
		crashRun = waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
			return core.IsRunTerminal(run.State) && run.CleanupState == core.CleanupPending
		})
	} else {
		waitForRT05Phase(t, first, readyPath, phase)
		crashRun = waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
			switch phase {
			case runtimePhaseIntentBeforeCreate:
				return run.State == core.RunStarting && run.LaunchNonce != "" && run.ContainerID == ""
			case runtimePhaseContainerCreated:
				return run.State == core.RunStarting && run.ContainerID != ""
			case runtimePhaseProcessObserved, runtimePhaseProcessExited:
				return run.State == core.RunActive
			default:
				return false
			}
		})
		registerRT05ImmutableCleanup(t, executor, crashRun)
		outcomeAllowed := phase == runtimePhaseProcessObserved || phase == runtimePhaseProcessExited
		if intent == "outcome" && outcomeAllowed {
			waitForRT05DurableProgress(t, ctx, root, crashRun)
		}
		intentDurable = applyRT05Intent(t, ctx, client, root, task, crashRun, intent, outcomeAllowed)
	}
	killP3DaemonProcess(t, first)
	assertRT05CrashFact(t, ctx, executor, root, crashRun, phase)

	second := startP3DaemonProcess(t, artifacts.daemon, configPath, socket, filepath.Join(root, "daemon-second.log"))
	t.Cleanup(func() { stopP3DaemonProcess(t, second) })
	client, err = transport.NewUnixClient(socket)
	requireNoError(t, err)
	if intent == "outcome" && !intentDurable {
		if phase == runtimePhaseContainerCreated {
			active := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool { return run.State == core.RunActive })
			if !applyRT05Intent(t, ctx, client, root, task, active, "stop", true) {
				t.Fatal("failed to stop the recovered N/A admission case")
			}
		}
		final := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
			return core.IsRunTerminal(run.State) && run.CleanupState == core.CleanupRemoved
		})
		stopP3DaemonProcess(t, second)
		assertRT05Converged(t, ctx, executor, root, project.ID, task.ID, final.ID, "outcome_NA", 1)
		return
	}
	final := waitForOperatorRun(t, client, task.ID, func(run core.Run) bool {
		if !core.IsRunTerminal(run.State) || run.CleanupState != core.CleanupRemoved {
			return false
		}
		return intent != "outcome" || run.RequestedOutcome == string(core.OutcomeWait)
	})
	stopP3DaemonProcess(t, second)
	assertRT05Converged(t, ctx, executor, root, project.ID, task.ID, final.ID, intent, 1)
}

func registerRT05OwnerFallback(t *testing.T, executor *containerruntime.DockerExecutor, databasePath, taskID string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupRT05OwnedRuns(t, executor, databasePath, taskID)
		assertRT05NoOwnedContainers(t, executor, taskID)
	})
}

func cleanupRT05OwnedRuns(t *testing.T, executor *containerruntime.DockerExecutor, databasePath, taskID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Errorf("RT-05 owner fallback open database: %v", err)
		return
	}
	defer database.Close()
	cursor := ""
	for {
		page, err := database.Runs(ctx, core.RunFilter{TaskID: taskID, Cursor: cursor, Limit: core.MaximumCompactPageLimit})
		if err != nil {
			t.Errorf("RT-05 owner fallback list Runs: %v", err)
			return
		}
		for _, summary := range page.Items {
			run, err := database.Run(ctx, summary.ID)
			if err != nil {
				t.Errorf("RT-05 owner fallback load Run %s: %v", summary.ID, err)
				continue
			}
			if run.ContainerID == "" {
				continue
			}
			ref := runtimeRef(run)
			if _, err := executor.Stop(ctx, ref, 0); err != nil {
				t.Errorf("RT-05 owner fallback stop Run %s: %v", run.ID, err)
				continue
			}
			if _, err := executor.Remove(ctx, ref); err != nil {
				t.Errorf("RT-05 owner fallback remove Run %s: %v", run.ID, err)
			}
		}
		if page.NextCursor == "" {
			return
		}
		cursor = page.NextCursor
	}
}

func assertRT05NoOwnedContainers(t *testing.T, executor *containerruntime.DockerExecutor, taskID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	managed, err := executor.Managed(ctx)
	if err != nil {
		t.Fatalf("RT-05 list managed containers: %v", err)
	}
	for _, state := range managed {
		if state.Ref.TaskID == taskID {
			t.Errorf("RT-05 owned container residue after cleanup: task=%s run=%s container=%s", taskID, state.Ref.RunID, state.Ref.ContainerID)
		}
	}
}

func registerRT05ImmutableCleanup(t *testing.T, executor *containerruntime.DockerExecutor, run core.Run) {
	t.Helper()
	if run.ContainerID == "" {
		return
	}
	ref := runtimeRef(run)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := executor.Stop(ctx, ref, 0); err != nil {
			t.Errorf("RT-05 cleanup immutable Run %s stop: %v", ref.RunID, err)
			return
		}
		if _, err := executor.Remove(ctx, ref); err != nil {
			t.Errorf("RT-05 cleanup immutable Run %s remove: %v", ref.RunID, err)
		}
	})
}

func waitForRT05DurableProgress(t *testing.T, ctx context.Context, root string, run core.Run) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		database, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
		requireNoError(t, err)
		events, readErr := database.Events(ctx, core.EventFilter{
			ProjectID: run.ProjectID, RunID: run.ID, EntityType: "task", EntityID: run.TaskID,
		})
		closeErr := database.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read RT-05 progress barrier: events=%v close=%v", readErr, closeErr)
		}
		count := countDaemonEvent(events, "task.progress")
		if count == 1 {
			return
		}
		if count > 1 {
			t.Fatalf("RT-05 Run %s progress barrier count = %d", run.ID, count)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("RT-05 Run %s did not durably record progress before outcome injection", run.ID)
}

func applyRT05Intent(
	t *testing.T,
	ctx context.Context,
	client *transport.Client,
	root string,
	task core.Task,
	run core.Run,
	intent string,
	outcomeAllowed bool,
) bool {
	t.Helper()
	switch intent {
	case "outcome":
		requestID := "rt05-outcome-" + run.ID
		before := ""
		if !outcomeAllowed {
			before = rt05DurableSignature(t, ctx, root, task.ProjectID, run.ID, requestID)
		}
		token, err := os.ReadFile(filepath.Join(root, "data", "run-control", run.ID, "token"))
		requireNoError(t, err)
		runClient, err := transport.NewUnixClient(
			filepath.Join(root, "data", "run-control", run.ID, "api.sock"),
			transport.WithBearerToken(strings.TrimSpace(string(token))),
		)
		requireNoError(t, err)
		var result core.OutcomeResult
		err = runClient.JSON(ctx, http.MethodPost, "/v1/task/outcome", core.OutcomeInput{
			Outcome: core.OutcomeWait, Reason: "RT-05 durable outcome", RequestID: requestID,
		}, &result)
		if !outcomeAllowed {
			typed := core.AsError(err)
			if !core.IsCode(err, core.CodeRunStarting) || !typed.Retryable || typed.State != string(core.RunStarting) || typed.Version != run.Version {
				t.Fatalf("pre-active outcome error = %#v, want retryable %s at Run version %d", typed, core.CodeRunStarting, run.Version)
			}
			if after := rt05DurableSignature(t, ctx, root, task.ProjectID, run.ID, requestID); after != before {
				t.Fatal("pre-active outcome changed Task/Run/Message/Event/dedupe durable state")
			}
			return false
		}
		if err != nil || result.Run.RequestedOutcome != string(core.OutcomeWait) {
			t.Fatalf("persist outcome: result=%#v err=%v", result, err)
		}
		assertRT05OutcomeDurable(t, ctx, root, task.ID, run.ID, requestID)
		return true
	case "stop":
		var updated core.Run
		if err := client.JSON(ctx, http.MethodPost, "/v1/runs/"+run.ID+"/stop", core.RunStopInput{
			Reason: "RT-05 durable stop", RequestID: "rt05-stop-" + run.ID,
		}, &updated); err != nil || updated.StopRequestedAt == "" {
			t.Fatalf("persist stop: Run=%#v err=%v", updated, err)
		}
		return true
	case "cancel":
		var cancelled core.Task
		if err := client.JSON(ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/cancel", core.TaskActionInput{
			Reason: "RT-05 durable cancel", RequestID: "rt05-cancel-" + run.ID,
		}, &cancelled); err != nil || cancelled.Status != core.TaskCancelled {
			t.Fatalf("persist cancel: Task=%#v err=%v", cancelled, err)
		}
		return true
	case "timeout":
		deadline, err := time.Parse(time.RFC3339Nano, run.DeadlineAt)
		if err != nil {
			t.Fatalf("timeout Run deadline = %q: %v", run.DeadlineAt, err)
		}
		if wait := time.Until(deadline); wait > 0 {
			timer := time.NewTimer(wait + 50*time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-timer.C:
			}
		}
		return true
	default:
		t.Fatalf("unknown RT-05 intent %q", intent)
		return false
	}
}

func waitForRT05Phase(t *testing.T, process *p3DaemonProcess, path string, phase runtimeContractPhase) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) == string(phase) {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-process.done:
			process.command = nil
			_ = process.log.Close()
			t.Fatalf("daemon exited before RT-05 phase %s: %v\n%s", phase, err, readP3DaemonLog(process.logPath))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("RT-05 phase %s timeout:\n%s", phase, readP3DaemonLog(process.logPath))
}

func assertRT05CrashFact(
	t *testing.T,
	ctx context.Context,
	executor *containerruntime.DockerExecutor,
	root string,
	run core.Run,
	phase runtimeContractPhase,
) {
	t.Helper()
	database, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	requireNoError(t, err)
	persisted, err := database.Run(ctx, run.ID)
	if closeErr := database.Close(); err != nil || closeErr != nil {
		t.Fatalf("read crash Run: err=%v close=%v", err, closeErr)
	}
	state, inspectErr := executor.Inspect(ctx, runtimeRef(persisted))
	switch phase {
	case runtimePhaseIntentBeforeCreate:
		if persisted.ContainerID != "" || !errors.Is(inspectErr, containerruntime.ErrNotFound) {
			t.Fatalf("intent crash fact: Run=%#v Docker=%#v err=%v", persisted, state, inspectErr)
		}
	case runtimePhaseContainerCreated:
		if inspectErr != nil || persisted.ContainerID == "" || state.Status != containerruntime.StatusCreated || state.Running {
			t.Fatalf("created crash fact: Run=%#v Docker=%#v err=%v", persisted, state, inspectErr)
		}
	case runtimePhaseProcessObserved:
		if inspectErr != nil || persisted.State != core.RunActive || !state.Running {
			t.Fatalf("active crash fact: Run=%#v Docker=%#v err=%v", persisted, state, inspectErr)
		}
	case runtimePhaseProcessExited, runtimePhaseTerminalPersisted:
		if inspectErr != nil || state.Status != containerruntime.StatusExited || state.Running {
			t.Fatalf("exit crash fact: Run=%#v Docker=%#v err=%v", persisted, state, inspectErr)
		}
	}
}

func assertRT05Converged(
	t *testing.T,
	ctx context.Context,
	executor *containerruntime.DockerExecutor,
	root, projectID, taskID, runID, intent string,
	wantRuns int,
) {
	t.Helper()
	database, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	requireNoError(t, err)
	defer database.Close()
	run, err := database.Run(ctx, runID)
	requireNoError(t, err)
	task, err := database.Task(ctx, taskID)
	requireNoError(t, err)
	runs, err := database.Runs(ctx, core.RunFilter{TaskID: taskID, Limit: 10})
	requireNoError(t, err)
	if len(runs.Items) != wantRuns || run.CleanupState != core.CleanupRemoved {
		t.Fatalf("RT-05 duplicated Run or missed cleanup: Runs=%#v Run=%#v", runs.Items, run)
	}
	for _, summary := range runs.Items {
		candidate, err := database.Run(ctx, summary.ID)
		requireNoError(t, err)
		if candidate.CleanupState != core.CleanupRemoved {
			t.Fatalf("RT-05 Run %s cleanup = %s", candidate.ID, candidate.CleanupState)
		}
		events, err := database.Events(ctx, core.EventFilter{ProjectID: projectID, RunID: candidate.ID})
		requireNoError(t, err)
		for _, kind := range []string{"run.created", "run.container_created", "run.start_issued", "run.active", "run.terminal"} {
			if count := countDaemonEvent(events, kind); count > 1 || (kind == "run.created" && count != 1) {
				t.Fatalf("RT-05 Run %s Event %s count = %d: %#v", candidate.ID, kind, count, events)
			}
		}
	}
	switch intent {
	case "outcome":
		if run.State != core.RunExited || run.RequestedOutcome != string(core.OutcomeWait) || task.Status != core.TaskWaiting {
			t.Fatalf("outcome recovery = Run %#v Task %#v", run, task)
		}
	case "outcome_NA":
		if run.RequestedOutcome != "" || run.RequestedAt != "" || task.Status != core.TaskFailed {
			t.Fatalf("pre-active outcome N/A admitted durable outcome: Run %#v Task %#v", run, task)
		}
	case "stop":
		if run.State != core.RunInterrupted || run.StopRequestedAt == "" || task.Status != core.TaskFailed {
			t.Fatalf("stop recovery = Run %#v Task %#v", run, task)
		}
	case "cancel":
		if run.State != core.RunCancelled || run.StopRequestedAt == "" || task.Status != core.TaskCancelled {
			t.Fatalf("cancel recovery = Run %#v Task %#v", run, task)
		}
	case "timeout":
		if run.State != core.RunTimedOut || run.StopRequestedAt == "" || task.Status != core.TaskFailed {
			t.Fatalf("timeout recovery = Run %#v Task %#v", run, task)
		}
	}
	if _, err := executor.Inspect(ctx, runtimeRef(run)); !errors.Is(err, containerruntime.ErrNotFound) {
		t.Fatalf("RT-05 container survived cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "run-control", run.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RT-05 run-control survived cleanup: %v", err)
	}
}

func rt05DurableSignature(
	t *testing.T,
	ctx context.Context,
	root, projectID, runID, requestID string,
) string {
	t.Helper()
	database, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	requireNoError(t, err)
	defer database.Close()
	snapshot, err := database.Snapshot(ctx, projectID)
	requireNoError(t, err)
	dedupeExists := false
	if err := database.Transact(ctx, func(tx core.Transaction) error {
		_, dedupeExists, err = tx.Dedupe("run:"+runID, "task.outcome", requestID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Snapshot     core.Snapshot `json:"snapshot"`
		DedupeExists bool          `json:"dedupe_exists"`
	}{Snapshot: snapshot, DedupeExists: dedupeExists})
	requireNoError(t, err)
	return string(raw)
}

func assertRT05OutcomeDurable(t *testing.T, ctx context.Context, root, taskID, runID, requestID string) {
	t.Helper()
	database, err := store.Open(ctx, filepath.Join(root, "data", "coordplane.db"))
	requireNoError(t, err)
	defer database.Close()
	run, err := database.Run(ctx, runID)
	requireNoError(t, err)
	task, err := database.Task(ctx, taskID)
	requireNoError(t, err)
	dedupeExists := false
	if err := database.Transact(ctx, func(tx core.Transaction) error {
		_, dedupeExists, err = tx.Dedupe("run:"+runID, "task.outcome", requestID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	events, err := database.Events(ctx, core.EventFilter{ProjectID: run.ProjectID, RunID: run.ID})
	requireNoError(t, err)
	if run.RequestedOutcome != string(core.OutcomeWait) || run.RequestedAt == "" || run.TokenRevokedAt == "" ||
		task.Status != core.TaskFinishing || !dedupeExists || countDaemonEvent(events, "run.outcome_requested") != 1 {
		t.Fatalf("outcome was not durable before SIGKILL: Run=%#v Task=%#v dedupe=%t Events=%#v", run, task, dedupeExists, events)
	}
}

// TestRT05HelperContainersDoNotTripOrphanIsolation locks the COD-64 Part 2
// orphan-isolation boundary at the RT-05 boundary (real SQLite + real Docker):
// the daemon's own short-lived helper containers (coordplane-git-inspect-* /
// coordplane-git-capture-* fingerprint: AgentID git-helper, generation 1,
// LaunchNonce equal to the 24-hex name digest, RunID an operation digest with
// no Run row by design) must NOT trip fail-closed orphan detection — they
// exist in every capture/inspect window and would otherwise flap the daemon
// degraded (live #11). The fail-closed property itself is unchanged: a run
// container (coordplane-run-*) without a Run row must still degrade the daemon
// with the orphan quarantine reason.
func TestRT05HelperContainersDoNotTripOrphanIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for RT-05: %v", err)
	}
	artifacts := buildRT05ProcessArtifacts(t, ctx)
	root, err := os.MkdirTemp("/tmp", "cp-rt05-helper-orphan-")
	requireNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configPath := writeTestConfig(t, root)
	rawConfig, err := os.ReadFile(configPath)
	requireNoError(t, err)
	rawConfig = []byte(strings.ReplaceAll(string(rawConfig), "  docker_network: coordplane\n", "  docker_network: none\n"))
	requireNoError(t, os.WriteFile(configPath, rawConfig, 0o600))

	socket := filepath.Join(root, "data", "operator.sock")
	daemon := startP3DaemonProcessWithEnv(t, artifacts.daemon, configPath, socket, filepath.Join(root, "daemon.log"), nil)
	t.Cleanup(func() { killP3DaemonProcess(t, daemon) })
	client, err := transport.NewUnixClient(socket)
	requireNoError(t, err)
	waitForRT05DaemonReady(t, client)

	// 24-hex digest — the production shape captureRuntimeRef / inspectRuntimeRef
	// emit (hex.EncodeToString(digest[:12]) = 24 hex chars). The pre-rework
	// fixture used a 12-hex digest that production never generates, so the
	// exclusion matched the fixture while missing every real helper container.
	digest := "a3b4c5d6e7f80123456789ab"
	helperLabels := map[string]string{
		"coordplane.managed":          "true",
		"coordplane.runtime_contract": "v1",
		"coordplane.project_id":       "prj_rt05_helper",
		"coordplane.task_id":          "tsk_rt05_helper",
		"coordplane.agent_id":         "git-helper",
		"coordplane.run_id":           "0123456789abcdef0123456789abcdef",
		"coordplane.generation":       "1",
		"coordplane.launch_nonce":     digest,
	}
	createRT05ManagedContainer(t, ctx, "coordplane-git-inspect-"+digest, helperLabels, artifacts.image)
	assertRT05ManagedVisible(t, executor, "coordplane-git-inspect-"+digest)
	// The helper-shaped container must not degrade the daemon across several
	// reconciler ticks (2s period); any degraded observation here is the
	// flapping live #11 hit.
	assertRT05DaemonStaysReady(t, client, 5*time.Second)

	runID := "run_abcdef0123456789abcdef0123456789"
	runLabels := map[string]string{
		"coordplane.managed":          "true",
		"coordplane.runtime_contract": "v1",
		"coordplane.project_id":       "prj_rt05_helper",
		"coordplane.task_id":          "tsk_rt05_helper",
		"coordplane.agent_id":         "agt_rt05_helper",
		"coordplane.run_id":           runID,
		"coordplane.generation":       "1",
		"coordplane.launch_nonce":     "9abcdef01234",
	}
	createRT05ManagedContainer(t, ctx, "coordplane-run-"+runID, runLabels, artifacts.image)
	assertRT05ManagedVisible(t, executor, "coordplane-run-"+runID)
	// Fail-closed property unchanged: a run-shaped container with no Run row
	// must still be quarantined/manual and degrade the daemon.
	assertRT05DaemonDegradedOrphan(t, client, 10*time.Second)
}

func waitForRT05DaemonReady(t *testing.T, client *transport.Client) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last core.Status
	for time.Now().Before(deadline) {
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &last); err == nil && last.DaemonReady {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("RT-05 daemon did not become ready: %#v", last)
}

func createRT05ManagedContainer(t *testing.T, ctx context.Context, name string, labels map[string]string, image string) {
	t.Helper()
	args := []string{"create", "--name", name}
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	args = append(args, image)
	command := exec.CommandContext(ctx, "docker", args...)
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("docker create %s: %v\n%s", name, err, raw)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

func assertRT05ManagedVisible(t *testing.T, executor *containerruntime.DockerExecutor, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	states, err := executor.Managed(ctx)
	requireNoError(t, err)
	for _, state := range states {
		if state.Ref.ContainerName == name {
			return
		}
	}
	t.Fatalf("fixture container %s is not visible to daemon Managed()", name)
}

func assertRT05DaemonStaysReady(t *testing.T, client *transport.Client, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	var last core.Status
	for time.Now().Before(deadline) {
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &last); err != nil {
			t.Fatalf("RT-05 status query: %v", err)
		}
		if !last.DaemonReady {
			t.Fatalf("RT-05 daemon degraded while only a helper-shaped managed container was present: %s", last.Reason)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertRT05DaemonDegradedOrphan(t *testing.T, client *transport.Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last core.Status
	for time.Now().Before(deadline) {
		if err := client.JSON(context.Background(), http.MethodGet, "/v1/status", nil, &last); err == nil &&
			!last.DaemonReady && strings.Contains(last.Reason, "orphan") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("RT-05 daemon did not fail closed on a run container without a Run row: last status=%#v", last)
}
