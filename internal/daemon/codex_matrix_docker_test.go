//go:build docker

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func TestCodexScriptedDockerStartArgvEnvAndLogRedaction(t *testing.T) {
	fixture := newCodexDockerFixture(t)
	canary := "REDACT-CANARY-CODEX-MATRIX"
	agent := addCodexInstructionsAgent(t, fixture, "Codex Start Agent", canary)
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
	if strings.Contains(string(raw), canary) {
		t.Fatal("Docker inspect leaked instructions_text")
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
	if strings.Contains(string(logRaw), canary) {
		t.Fatalf("run.log leaked instructions_text: %s", logRaw)
	}
	if !strings.Contains(string(logRaw), redactedSecret) {
		t.Fatalf("run.log did not redact echoed instructions_text: %s", logRaw)
	}
}

func TestCodexScriptedDockerResumeFreshFallbackMatrix(t *testing.T) {
	fixture := newCodexDockerFixture(t)
	fixture.components.runtime.Start(fixture.ctx)

	t.Run("same config resumes", func(t *testing.T) {
		agent := addCodexInstructionsAgent(t, fixture, "Codex Resume Agent", "RESUME-CANARY")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex resume matrix", 0)
		first := waitForActiveCodexRun(t, fixture, task.ID)
		assertCodexStartShape(t, inspectCodexRun(t, fixture, first), "/workspace/project")
		if first.NativeSessionID == "" {
			t.Fatalf("first Codex Run has no native session: %#v", first)
		}

		interruptCodexRun(t, fixture, task.ID, first.ID, "codex-resume-stop")
		second := waitForActiveCodexRun(t, fixture, task.ID)
		if second.LaunchMode != "resume" || second.ResumedFromRunID != first.ID ||
			second.ResumeNativeSessionID != first.NativeSessionID {
			t.Fatalf("same-config Codex resume = %#v, want resume of %s", second, first.ID)
		}
		assertCodexResumeShape(t, inspectCodexRun(t, fixture, second), first.NativeSessionID)
		cancelCodexTask(t, fixture, task.ID)
	})

	t.Run("config change starts fresh", func(t *testing.T) {
		agent := addCodexInstructionsAgent(t, fixture, "Codex Fresh Agent", "FRESH-CANARY")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex fresh matrix", 0)
		first := waitForActiveCodexRun(t, fixture, task.ID)

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
		interruptCodexRun(t, fixture, task.ID, first.ID, "codex-fresh-stop")

		second := waitForActiveCodexRun(t, fixture, task.ID)
		if second.LaunchMode != "start" || second.ResumedFromRunID != "" || second.ResumeNativeSessionID != "" {
			t.Fatalf("config-change Codex launch = %#v, want fresh start", second)
		}
		assertCodexStartShape(t, inspectCodexRun(t, fixture, second), "/workspace/project")
		state := inspectCodexRun(t, fixture, second)
		if !strings.Contains(strings.Join(state.CommandArgs, " "), "model_reasoning_effort=low") {
			t.Fatalf("fresh Codex container lacks updated effort argv: %#v", state.CommandArgs)
		}
		cancelCodexTask(t, fixture, task.ID)
	})

	t.Run("session not found falls back to fresh start", func(t *testing.T) {
		agent := addCodexInstructionsAgent(t, fixture, "Codex Fallback Agent", "resume error")
		project := fixture.addProject(t, agent.ID)
		task := fixture.addTask(t, project.ID, agent.ID, "codex fallback matrix", 1)
		first := waitForActiveCodexRun(t, fixture, task.ID)
		interruptCodexRun(t, fixture, task.ID, first.ID, "codex-fallback-stop")

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
		cancelCodexTask(t, fixture, task.ID)
	})
}

func addCodexInstructionsAgent(t *testing.T, fixture *p3DockerFixture, name, instructionsText string) core.Agent {
	t.Helper()
	agent, err := fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
		DisplayName: name, AdapterID: "codex", Image: fixture.image,
		InstructionsText: instructionsText, RequestID: "codex-agent-" + strings.ReplaceAll(name, " ", "-"),
	})
	requireNoError(t, err)
	return agent
}

func waitForActiveCodexRun(t *testing.T, fixture *p3DockerFixture, taskID string) core.Run {
	t.Helper()
	return waitForRun(t, fixture, taskID, func(run core.Run, task core.Task) bool {
		return run.State == core.RunActive && task.Status == core.TaskRunning
	})
}

func interruptCodexRun(t *testing.T, fixture *p3DockerFixture, taskID, runID, requestID string) {
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

func cancelCodexTask(t *testing.T, fixture *p3DockerFixture, taskID string) {
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

func inspectCodexRun(t *testing.T, fixture *p3DockerFixture, run core.Run) containerruntime.LiveState {
	t.Helper()
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(run))
	requireNoError(t, err)
	return state
}

func assertCodexStartShape(t *testing.T, state containerruntime.LiveState, workdir string) {
	t.Helper()
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != "codex" {
		t.Fatalf("Codex entrypoint = %#v", state.Entrypoint)
	}
	joined := strings.Join(state.CommandArgs, " ")
	for _, part := range []string{"exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", "-C", workdir,
		"--", adapter.ContainerBootstrapPath} {
		if !strings.Contains(joined, part) {
			t.Fatalf("Codex start argv missing %q: %#v", part, state.CommandArgs)
		}
	}
	assertCodexEnvironment(t, state)
}

func assertCodexResumeShape(t *testing.T, state containerruntime.LiveState, sessionID string) {
	t.Helper()
	joined := strings.Join(state.CommandArgs, " ")
	for _, part := range []string{"exec", "resume", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--ignore-user-config", sessionID,
		"--", adapter.ContainerBootstrapPath} {
		if !strings.Contains(joined, part) {
			t.Fatalf("Codex resume argv missing %q: %#v", part, state.CommandArgs)
		}
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
