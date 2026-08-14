//go:build docker

package daemon

import (
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

func TestClaudeScriptedDockerStartArgvEnvAndLogRedaction(t *testing.T) {
	secret := "provider-secret-claude-matrix-001"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)
	fixture := newP3DockerFixtureWithProviderEnv(t, []string{"ANTHROPIC_AUTH_TOKEN"})
	canary := "REDACT-CANARY-CLAUDE-MATRIX"
	agent := addProviderInstructionsAgent(t, fixture, "Claude Start Agent", canary)
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "hold until stopped claude matrix", 0)
	fixture.components.runtime.Start(fixture.ctx)

	active := waitForActiveProviderRun(t, fixture, task.ID)
	state, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(active))
	requireNoError(t, err)
	assertClaudeStartShape(t, state)

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
		RunID: active.ID, Reason: "claude matrix stop", RequestID: "claude-stop-" + active.ID,
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

func TestClaudeScriptedDockerProviderConfigFingerprint(t *testing.T) {
	model := "claude-sonnet-4-7"
	subagent := "claude-haiku-4-5"
	baseURL := "https://api.example.com"
	effort := "high"
	canary := "REDACT-CANARY-CLAUDE-PROVIDER"
	fixture := newP3DockerFixture(t)
	agent, err := fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
		DisplayName: "Claude Provider Agent", AdapterID: "claude", Image: fixture.image,
		InstructionsText: canary, Model: model, SubagentModel: subagent, BaseURL: baseURL, Effort: effort,
		RequestID: "claude-provider-agent",
	})
	requireNoError(t, err)
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "claude provider matrix", 0)
	fixture.components.runtime.Start(fixture.ctx)

	active := waitForActiveProviderRun(t, fixture, task.ID)
	state := inspectProviderRun(t, fixture, active)
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("Claude entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
	want := []string{"claude", "-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--", runtimeBootstrapPrompt()}
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("Claude provider argv = %#v, want %#v", state.CommandArgs, want)
	}
	assertClaudeProviderEnvironment(t, state, model, subagent, baseURL, effort)

	// I4: the persisted ConfigFingerprint must equal the fingerprint derived
	// from the same launch inputs.
	wantFingerprint, err := core.RuntimeConfigFingerprint(core.RuntimeConfigFingerprintInput{
		AdapterID: "claude", Image: fixture.image, Model: model, SubagentModel: subagent,
		BaseURL: baseURL, Effort: effort, InstructionsHash: hex.EncodeToString(sha256Sum(canary)),
	})
	requireNoError(t, err)
	if active.ConfigFingerprint != wantFingerprint {
		t.Fatalf("Claude ConfigFingerprint = %q, want %q", active.ConfigFingerprint, wantFingerprint)
	}
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

	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim adopt fixture: ok=%v err=%v", ok, err)
	}
	created := prepareCreatedRunForReconcile(t, fixture, claim)
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, created.ID)
	control, err := fixture.components.runtime.openRunControl(created, controlPath)
	requireNoError(t, err)
	fixture.components.runtime.registerControl(created.ID, control)
	if _, err := fixture.executor.Attach(fixture.ctx, runtimeRef(created)); err != nil {
		t.Fatal(err)
	}
	started, err := fixture.components.service.RecordRunStartIssued(
		fixture.ctx,
		runtimeFactInput(created, runtimeRef(created), "adopt-start-issued"),
	)
	requireNoError(t, err)
	startedRef, err := fixture.executor.Start(fixture.ctx, runtimeRef(started))
	requireNoError(t, err)
	active, err := fixture.components.service.ObserveProcessAndActivateRun(
		fixture.ctx,
		runtimeFactInput(started, startedRef, "adopt-active"),
	)
	requireNoError(t, err)
	waitForFile(t, fixture.ctx, filepath.Join(active.WorkspacePath, "secret-observed"))

	fixture.components.runtime.mu.Lock()
	oldControl := fixture.components.runtime.controls[active.ID]
	fixture.components.runtime.mu.Unlock()
	if oldControl == nil {
		t.Fatal("fault fixture has no original Run control")
	}
	requireNoError(t, fixture.components.runtime.closeControl(active.ID, oldControl))

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
	adopted, err := fixture.components.store.Run(fixture.ctx, active.ID)
	requireNoError(t, err)
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
	terminal := waitForRun(t, fixture, task.ID, func(run core.Run, task core.Task) bool {
		return run.ID == active.ID && run.State == core.RunInterrupted &&
			run.CleanupState == core.CleanupRemoved && task.Status == core.TaskQueued
	})
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

func assertClaudeStartShape(t *testing.T, state containerruntime.LiveState) {
	t.Helper()
	if len(state.Entrypoint) != 1 || state.Entrypoint[0] != runtimeLaunchExecutable {
		t.Fatalf("Claude entrypoint = %#v, want [%s]", state.Entrypoint, runtimeLaunchExecutable)
	}
	want := []string{"claude", "-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--", runtimeBootstrapPrompt()}
	if !reflect.DeepEqual(state.CommandArgs, want) {
		t.Fatalf("Claude start argv = %#v, want %#v", state.CommandArgs, want)
	}
	assertClaudeEnvironment(t, state)
}

func assertClaudeEnvironment(t *testing.T, state containerruntime.LiveState) {
	t.Helper()
	digests := make(map[string]string, len(state.Environment))
	for _, fact := range state.Environment {
		digests[fact.Name] = fact.ValueDigest
	}
	for name, value := range map[string]string{"HOME": "/home/agent"} {
		want := hex.EncodeToString(sha256Sum(value))
		if digests[name] != want {
			t.Fatalf("Claude env %s digest = %q, want %q (facts=%#v)", name, digests[name], want, state.Environment)
		}
	}
}

func assertClaudeProviderEnvironment(t *testing.T, state containerruntime.LiveState, model, subagent, baseURL, effort string) {
	t.Helper()
	digests := make(map[string]string, len(state.Environment))
	for _, fact := range state.Environment {
		digests[fact.Name] = fact.ValueDigest
	}
	for name, value := range map[string]string{
		"HOME": "/home/agent", "ANTHROPIC_MODEL": model, "CLAUDE_CODE_SUBAGENT_MODEL": subagent,
		"ANTHROPIC_BASE_URL": baseURL, "CLAUDE_CODE_EFFORT_LEVEL": effort,
	} {
		want := hex.EncodeToString(sha256Sum(value))
		if digests[name] != want {
			t.Fatalf("Claude env %s digest = %q, want %q (facts=%#v)", name, digests[name], want, state.Environment)
		}
	}
}
