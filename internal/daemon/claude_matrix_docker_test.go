//go:build docker

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func TestClaudeScriptedDockerResumeSameConfig(t *testing.T) {
	fixture := newP3DockerFixture(t)
	fixture.components.runtime.Start(fixture.ctx)

	agent := addProviderInstructionsAgent(t, fixture, "Claude Resume Agent", "RESUME-CANARY-CLAUDE")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped claude resume", 0)
	first := waitForActiveProviderRun(t, fixture, task.ID)
	assertClaudeStartShape(t, inspectProviderRun(t, fixture, first))
	if first.NativeSessionID == "" {
		t.Fatalf("first Claude Run has no native session: %#v", first)
	}

	interruptProviderRun(t, fixture, task.ID, first.ID, "claude-resume-stop")
	second := waitForActiveProviderRun(t, fixture, task.ID)
	if second.LaunchMode != "resume" || second.ResumedFromRunID != first.ID ||
		second.ResumeNativeSessionID != first.NativeSessionID {
		t.Fatalf("same-config Claude resume = %#v, want resume of %s", second, first.ID)
	}
	assertClaudeResumeShape(t, inspectProviderRun(t, fixture, second), first.NativeSessionID)
	cancelProviderTask(t, fixture, task.ID)
}

func TestClaudeScriptedDockerAdoptRestartKeepsRedaction(t *testing.T) {
	secret := "provider-secret-claude-adopt-001"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)
	fixture := newP3DockerFixtureWithProviderEnv(t, []string{"ANTHROPIC_AUTH_TOKEN"})
	canary := "REDACT-CANARY-CLAUDE-ADOPT"
	agent := addProviderInstructionsAgent(t, fixture, "Claude Adopt Agent", canary)
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped adopt restart", 0)

	active := prepareCreatedRunAndActivate(t, fixture, project.ID, "adopt-start-issued", "adopt-active")
	waitForFile(t, fixture.ctx, filepath.Join(active.WorkspacePath, "secret-observed"))
	restarted, adopted := restartRuntimeController(t, fixture, active.ID)
	if adopted.State != core.RunActive || adopted.ContainerID != active.ContainerID ||
		adopted.Generation != active.Generation || restarted.monitor(active.ID) == nil {
		t.Fatalf("restart did not adopt the durable active Run: before=%#v after=%#v", active, adopted)
	}

	// I6: after restart-and-adopt the run-scoped secrets and bootstrap files are
	// reused, so log redaction stays effective for the previously injected secret
	// and the bootstrap canary.
	if _, err := fixture.components.service.RequestRuntimeStop(fixture.ctx, core.RunStopInput{
		RunID: active.ID, Reason: "adopt redaction assertion complete", OperationID: "adopt-stop",
		RequestID: "adopt-stop-request",
	}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForInterruptedRemoved(t, fixture, task.ID, active.ID)
	logRaw, err := os.ReadFile(terminal.LogPath)
	requireNoError(t, err)
	for _, forbidden := range []string{canary, secret} {
		if strings.Contains(string(logRaw), forbidden) {
			t.Fatalf("adopted run.log leaked %q: %s", forbidden, logRaw)
		}
	}
	if !strings.Contains(string(logRaw), redactedSecret) {
		t.Fatalf("adopted run.log did not redact echoed provider secret: %s", logRaw)
	}
}

func claudeBaseArgs() []string {
	return []string{"claude", "-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--", runtimeBootstrapPrompt()}
}

func assertClaudeStartShape(t *testing.T, state containerruntime.LiveState) {
	t.Helper()
	assertEntrypoint(t, state)
	assertCommandArgs(t, state, "Claude start", claudeBaseArgs())
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent"})
}

func assertClaudeResumeShape(t *testing.T, state containerruntime.LiveState, sessionID string) {
	t.Helper()
	assertEntrypoint(t, state)
	want := []string{"claude", "-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--resume", sessionID, "--", runtimeBootstrapPrompt()}
	assertCommandArgs(t, state, "Claude resume", want)
	assertEnvDigests(t, state, map[string]string{"HOME": "/home/agent"})
}

func assertClaudeProviderShape(t *testing.T, state containerruntime.LiveState, model, subagent, baseURL, effort string) {
	t.Helper()
	assertEntrypoint(t, state)
	assertCommandArgs(t, state, "Claude provider", claudeBaseArgs())
	assertEnvDigests(t, state, map[string]string{
		"HOME": "/home/agent", "ANTHROPIC_MODEL": model, "CLAUDE_CODE_SUBAGENT_MODEL": subagent,
		"ANTHROPIC_BASE_URL": baseURL, "CLAUDE_CODE_EFFORT_LEVEL": effort,
	})
}
