//go:build docker

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func TestCodexScriptedDockerStartArgvEnvAndLogRedaction(t *testing.T) {
	secret := "provider-secret-codex-matrix-001"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)
	fixture := newCodexDockerFixtureWithProviderEnv(t, []string{"ANTHROPIC_AUTH_TOKEN"})
	canary := "REDACT-CANARY-CODEX-MATRIX"
	agent := addProviderInstructionsAgent(t, fixture, "Codex Start Agent", canary)
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "codex matrix hold", 0)
	fixture.components.runtime.Start(fixture.ctx)

	active := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(active))
	requireNoError(t, err)
	assertCodexStartShape(t, state, "/workspace/project")

	raw, err := exec.CommandContext(fixture.ctx, "docker", "inspect", active.ContainerID).CombinedOutput()
	requireNoError(t, err)
	for _, forbidden := range []string{canary, secret} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("Docker inspect leaked %q", forbidden)
		}
	}

	// I1: the provider secret must reach the provider process as a runtime env
	// value (sourced from the secrets file), observable by writing it into the
	// host-mounted workspace from inside the container.
	observed, err := os.ReadFile(filepath.Join(active.WorkspacePath, "secret-observed"))
	requireNoError(t, err)
	if string(observed) != secret {
		t.Fatalf("provider secret observed in workspace = %q, want %q", observed, secret)
	}

	if _, err := fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{
		RunID: active.ID, Reason: "codex matrix stop", RequestID: "codex-stop-" + active.ID,
	}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.ID == active.ID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskQueued
	})
	logRaw, err := os.ReadFile(terminal.LogPath)
	requireNoError(t, err)
	for _, forbidden := range []string{canary, secret} {
		if strings.Contains(string(logRaw), forbidden) {
			t.Fatalf("run.log leaked %q: %s", forbidden, logRaw)
		}
	}
	if !strings.Contains(string(logRaw), redactedSecret) {
		t.Fatalf("run.log did not redact echoed provider secret: %s", logRaw)
	}
}

func TestCodexScriptedDockerResumeFreshFallbackMatrix(t *testing.T) {
	fixture := newCodexDockerFixture(t)
	fixture.components.runtime.Start(fixture.ctx)

	t.Run("same config resumes", func(t *testing.T) {
		agent := addProviderInstructionsAgent(t, fixture, "Codex Resume Agent", "RESUME-CANARY")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex resume matrix", 0)
		first := waitForActiveProviderRun(t, fixture, task.ID)
		assertCodexStartShape(t, inspectProviderRun(t, fixture, first), "/workspace/project")
		if first.NativeSessionID == "" {
			t.Fatalf("first Codex Run has no native session: %#v", first)
		}

		interruptProviderRun(t, fixture, task.ID, first.ID, "codex-resume-stop")
		second := waitForActiveProviderRun(t, fixture, task.ID)
		if second.LaunchMode != "resume" || second.ResumedFromRunID != first.ID ||
			second.ResumeNativeSessionID != first.NativeSessionID {
			t.Fatalf("same-config Codex resume = %#v, want resume of %s", second, first.ID)
		}
		assertCodexResumeShape(t, inspectProviderRun(t, fixture, second), first.NativeSessionID)
		cancelProviderTask(t, fixture, task.ID)
	})

	t.Run("config change starts fresh", func(t *testing.T) {
		agent := addProviderInstructionsAgent(t, fixture, "Codex Fresh Agent", "FRESH-CANARY")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex fresh matrix", 0)
		first := waitForActiveProviderRun(t, fixture, task.ID)

		current, err := fixture.components.service.Agent(fixture.ctx, agent.ID)
		requireNoError(t, err)
		if _, err := fixture.components.service.UpdateAgent(fixture.ctx, core.UpdateAgentInput{
			ID: current.ID, Version: current.Version,
			AgentConfigInput: core.AgentConfigInput{
				DisplayName: current.DisplayName, AdapterID: current.AdapterID, Image: current.Image,
				InstructionsText: current.InstructionsText, Effort: "low",
			},
			RequestID: "codex-fresh-update-" + current.ID,
		}); err != nil {
			t.Fatal(err)
		}
		interruptProviderRun(t, fixture, task.ID, first.ID, "codex-fresh-stop")

		second := waitForActiveProviderRun(t, fixture, task.ID)
		if second.LaunchMode != "start" || second.ResumedFromRunID != "" || second.ResumeNativeSessionID != "" {
			t.Fatalf("config-change Codex launch = %#v, want fresh start", second)
		}
		assertCodexStartShapeWithEffort(t, inspectProviderRun(t, fixture, second), "/workspace/project", "low")
		cancelProviderTask(t, fixture, task.ID)
	})

	t.Run("session not found falls back to fresh start", func(t *testing.T) {
		agent := addProviderInstructionsAgent(t, fixture, "Codex Fallback Agent", "resume error")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex fallback matrix", 1)
		first := waitForActiveProviderRun(t, fixture, task.ID)
		interruptProviderRun(t, fixture, task.ID, first.ID, "codex-fallback-stop")

		fallback := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
			return core.IsRunTerminal(run.State) && run.CleanupState == core.CleanupRemoved &&
				run.LaunchMode == "start" &&
				run.ResumedFromRunID != "" && run.ResumeNativeSessionID == "" &&
				run.NativeSessionID != "" && run.ID != first.ID
		})
		events, err := fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID, RunID: fallback.ID})
		requireNoError(t, err)
		if countDaemonEvent(events, "run.resume_fallback") != 1 {
			t.Fatalf("Codex fallback Events = %#v", events)
		}
		runs, err := fixture.components.store.Runs(fixture.ctx, core.RunFilter{TaskID: task.ID, Limit: 20})
		requireNoError(t, err)
		var failedResume core.Run
		for _, summary := range runs.Items {
			run, readErr := fixture.components.store.Run(fixture.ctx, summary.ID)
			requireNoError(t, readErr)
			if run.ID == fallback.ResumedFromRunID {
				failedResume = run
			}
		}
		if failedResume.ID == "" || failedResume.LaunchMode != "resume" ||
			failedResume.ResumedFromRunID != first.ID ||
			failedResume.RuntimeErrorCode != string(core.CodeResumeUnavailable) {
			t.Fatalf("Codex failed resume Run = %#v", failedResume)
		}
		cancelProviderTask(t, fixture, task.ID)
	})
}

func TestCodexScriptedDockerProviderConfigFingerprint(t *testing.T) {
	model := "codex-mini"
	subagent := "codex-haiku"
	baseURL := "https://example.invalid/v1"
	effort := "max"
	canary := "REDACT-CANARY-CODEX-PROVIDER"
	fixture := newCodexDockerFixture(t)
	agent, err := fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
		DisplayName: "Codex Provider Agent", AdapterID: "codex", Image: fixture.image,
		InstructionsText: canary, Model: model, SubagentModel: subagent, BaseURL: baseURL, Effort: effort,
		RequestID: "codex-provider-agent",
	})
	requireNoError(t, err)
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "codex provider matrix", 0)
	fixture.components.runtime.Start(fixture.ctx)

	active := waitForActiveProviderRun(t, fixture, task.ID)
	state := inspectProviderRun(t, fixture, active)
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("Codex entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
	want := []string{"codex", "exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", "-C", "/workspace/project",
		"-m", model, "-c", "default_subagent_model=" + subagent, "-c", "model_reasoning_effort=" + effort,
		"-c", "model_providers.codex.name=codex", "-c", "model_providers.codex.base_url=" + baseURL,
		"--", runtimeBootstrapPrompt()}
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("Codex provider argv = %#v, want %#v", state.CommandArgs, want)
	}
	assertCodexEnvironment(t, state)

	// I4: the persisted ConfigFingerprint must equal the fingerprint derived
	// from the same launch inputs.
	wantFingerprint, err := core.RuntimeConfigFingerprint(core.RuntimeConfigFingerprintInput{
		AdapterID: "codex", Image: fixture.image, Model: model, SubagentModel: subagent,
		BaseURL: baseURL, Effort: effort, InstructionsHash: hex.EncodeToString(sha256Sum(canary)),
	})
	requireNoError(t, err)
	if active.ConfigFingerprint != wantFingerprint {
		t.Fatalf("Codex ConfigFingerprint = %q, want %q", active.ConfigFingerprint, wantFingerprint)
	}
	cancelProviderTask(t, fixture, task.ID)
}

func addProviderInstructionsAgent(t *testing.T, fixture *p3DockerFixture, name, instructionsText string) core.Agent {
	t.Helper()
	agent, err := fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
		DisplayName: name, AdapterID: fixture.adapterID, Image: fixture.image,
		InstructionsText: instructionsText, RequestID: "provider-agent-" + strings.ReplaceAll(name, " ", "-"),
	})
	requireNoError(t, err)
	return agent
}

func waitForActiveProviderRun(t *testing.T, fixture *p3DockerFixture, taskID string) core.Run {
	t.Helper()
	return waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
}

func interruptProviderRun(t *testing.T, fixture *p3DockerFixture, taskID, runID, requestID string) {
	t.Helper()
	if _, err := fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{
		RunID: runID, Reason: "codex matrix interrupt", RequestID: requestID,
	}); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return run.ID == runID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskQueued
	})
}

func cancelProviderTask(t *testing.T, fixture *p3DockerFixture, taskID string) {
	t.Helper()
	if _, err := fixture.components.service.CancelTask(fixture.ctx, core.TaskActionInput{
		TaskID: taskID, Reason: "codex matrix complete", RequestID: "codex-cancel-" + taskID,
	}); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return task.Status == core.TaskCancelled
	})
}

func inspectProviderRun(t *testing.T, fixture *p3DockerFixture, run core.Run) containerruntime.LiveState {
	t.Helper()
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(run))
	requireNoError(t, err)
	return state
}

func runtimeBootstrapPrompt() string {
	return "Read and follow the complete CoordPlane run bootstrap at " + adapter.ContainerBootstrapPath + " before acting."
}

func assertCodexStartShape(t *testing.T, state containerruntime.LiveState, workdir string) {
	t.Helper()
	assertCodexStartArgv(t, state, workdir, nil)
	assertCodexEnvironment(t, state)
}

func assertCodexStartShapeWithEffort(t *testing.T, state containerruntime.LiveState, workdir, effort string) {
	t.Helper()
	assertCodexStartArgv(t, state, workdir, []string{"-c", "model_reasoning_effort=" + effort})
	assertCodexEnvironment(t, state)
}

func assertCodexStartArgv(t *testing.T, state containerruntime.LiveState, workdir string, providerArgs []string) {
	t.Helper()
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("Codex entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
	want := []string{"codex", "exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", "-C", workdir}
	want = append(want, providerArgs...)
	want = append(want, "--", runtimeBootstrapPrompt())
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("Codex start argv = %#v, want %#v", state.CommandArgs, want)
	}
}

func assertCodexResumeShape(t *testing.T, state containerruntime.LiveState, sessionID string) {
	t.Helper()
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("Codex entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
	want := []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", sessionID, "--", runtimeBootstrapPrompt()}
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("Codex resume argv = %#v, want %#v", state.CommandArgs, want)
	}
	assertCodexEnvironment(t, state)
}

func assertCodexEnvironment(t *testing.T, state containerruntime.LiveState) {
	t.Helper()
	digests := make(map[string]string, len(state.Environment))
	for _, fact := range state.Environment {
		digests[fact.Name] = fact.ValueDigest
	}
	for name, value := range map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"} {
		want := hex.EncodeToString(sha256Sum(value))
		if digests[name] != want {
			t.Fatalf("Codex env %s digest = %q, want %q (facts=%#v)", name, digests[name], want, state.Environment)
		}
	}
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
