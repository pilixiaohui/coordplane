//go:build docker

package daemon

import (
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

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

		current := requireRuntimeValue(fixture.components.service.Agent(fixture.ctx, agent.ID))
		requireRuntimeValue(fixture.components.service.UpdateAgent(fixture.ctx, core.UpdateAgentInput{
			ID: current.ID, Version: current.Version,
			AgentConfigInput: core.AgentConfigInput{
				DisplayName: current.DisplayName, AdapterID: current.AdapterID, Image: current.Image,
				InstructionsText: current.InstructionsText, Effort: "low",
			},
			RequestID: "codex-fresh-update-" + current.ID,
		}))
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
		events := requireRuntimeValue(fixture.components.store.Events(fixture.ctx, core.EventFilter{ProjectID: project.ID, RunID: fallback.ID}))
		if countDaemonEvent(events, "run.resume_fallback") != 1 {
			t.Fatalf("Codex fallback Events = %#v", events)
		}
		runs := requireRuntimeValue(fixture.components.store.Runs(fixture.ctx, core.RunFilter{TaskID: task.ID, Limit: 20}))
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

func addProviderInstructionsAgent(t *testing.T, fixture *p3DockerFixture, name, instructionsText string) core.Agent {
	t.Helper()
	agent := requireRuntimeValue(fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
		DisplayName: name, AdapterID: fixture.adapterID, Image: fixture.image,
		InstructionsText: instructionsText, RequestID: "provider-agent-" + strings.ReplaceAll(name, " ", "-"),
	}))
	return agent
}

func waitForActiveProviderRun(t *testing.T, fixture *p3DockerFixture, taskID string) core.Run {
	t.Helper()
	return waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && run.NativeSessionID != "" && task.Status == core.TaskRunning
	})
}

func interruptProviderRun(t *testing.T, fixture *p3DockerFixture, taskID, runID, requestID string) {
	t.Helper()
	requireRuntimeValue(fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{
		RunID: runID, Reason: "provider matrix interrupt", RequestID: requestID,
	}))
	waitForInterruptedRemoved(t, fixture, taskID, runID)
}

func waitForInterruptedRemoved(t *testing.T, fixture *p3DockerFixture, taskID, runID string) core.Run {
	t.Helper()
	return waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return run.ID == runID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskQueued
	})
}

func cancelProviderTask(t *testing.T, fixture *p3DockerFixture, taskID string) {
	t.Helper()
	requireRuntimeValue(fixture.components.service.CancelTask(fixture.ctx, core.TaskActionInput{
		TaskID: taskID, Reason: "provider matrix complete", RequestID: "provider-cancel-" + taskID,
	}))
	waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return task.Status == core.TaskCancelled
	})
}

func inspectProviderRun(t *testing.T, fixture *p3DockerFixture, run core.Run) containerruntime.LiveState {
	t.Helper()
	state := requireRuntimeValue(fixture.executor.Inspect(fixture.ctx, runtimeRef(run)))
	return state
}

func runtimeBootstrapPrompt() string {
	return "Read and follow the complete CoordPlane run bootstrap at " + adapter.ContainerBootstrapPath + " before acting."
}

func assertEntrypoint(t *testing.T, state containerruntime.LiveState) {
	t.Helper()
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
}

func assertCommandArgs(t *testing.T, state containerruntime.LiveState, what string, want []string) {
	t.Helper()
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("%s argv = %#v, want %#v", what, state.CommandArgs, want)
	}
}

func assertCodexStartShape(t *testing.T, state containerruntime.LiveState, workdir string) {
	t.Helper()
	assertCodexStartArgv(t, state, workdir, nil)
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"})
}

func assertCodexStartShapeWithEffort(t *testing.T, state containerruntime.LiveState, workdir, effort string) {
	t.Helper()
	assertCodexStartArgv(t, state, workdir, []string{"-c", "model_reasoning_effort=" + effort})
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"})
}

func assertCodexStartArgv(t *testing.T, state containerruntime.LiveState, workdir string, providerArgs []string) {
	t.Helper()
	assertEntrypoint(t, state)
	want := []string{"codex", "exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", "-C", workdir}
	want = append(want, providerArgs...)
	want = append(want, "--", runtimeBootstrapPrompt())
	assertCommandArgs(t, state, "Codex start", want)
}

func assertCodexResumeShape(t *testing.T, state containerruntime.LiveState, sessionID string) {
	t.Helper()
	assertEntrypoint(t, state)
	want := []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", sessionID, "--", runtimeBootstrapPrompt()}
	assertCommandArgs(t, state, "Codex resume", want)
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"})
}

func assertCodexProviderShape(t *testing.T, state containerruntime.LiveState, model, subagent, baseURL, effort string) {
	t.Helper()
	assertCodexStartArgv(t, state, "/workspace/project", []string{
		"-m", model, "-c", "default_subagent_model=" + subagent, "-c", "model_reasoning_effort=" + effort,
		"-c", "model_providers.codex.name=codex", "-c", "model_providers.codex.base_url=" + baseURL,
	})
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"})
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
