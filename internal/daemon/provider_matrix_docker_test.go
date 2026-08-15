//go:build docker

package daemon

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

// providerStartCase drives the I1/I2 Docker matrix over every provider
// adapter: the provider secret must reach the container env without leaking
// into docker inspect or run.log, and the launched argv/env must match the
// adapter contract exactly.
type providerStartCase struct {
	adapter    string
	name       string
	fixture    func(t *testing.T, providerEnv []string) *p3DockerFixture
	canary     string
	taskDesc   string
	stopReason string
	startShape func(t *testing.T, state containerruntime.LiveState)
}

func providerStartCases() []providerStartCase {
	return []providerStartCase{
		{adapter: "claude", name: "Claude Start Agent", fixture: newP3DockerFixtureWithProviderEnv,
			canary: "REDACT-CANARY-CLAUDE-MATRIX", taskDesc: "hold until stopped claude matrix", stopReason: "claude matrix stop",
			startShape: assertClaudeStartShape},
		{adapter: "codex", name: "Codex Start Agent", fixture: newCodexDockerFixtureWithProviderEnv,
			canary: "REDACT-CANARY-CODEX-MATRIX", taskDesc: "codex matrix hold", stopReason: "codex matrix stop",
			startShape: func(t *testing.T, state containerruntime.LiveState) {
				assertCodexStartShape(t, state, "/workspace/project")
			}},
	}
}

func TestScriptedDockerProviderStartArgvEnvAndLogRedaction(t *testing.T) {
	for _, tc := range providerStartCases() {
		t.Run(tc.adapter, func(t *testing.T) {
			secret := "provider-secret-" + tc.adapter + "-matrix-001"
			t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)
			fixture := tc.fixture(t, []string{"ANTHROPIC_AUTH_TOKEN"})
			agent := addProviderInstructionsAgent(t, fixture, tc.name, tc.canary)
			project := fixture.addProject(t, agent.ID)
			task := fixture.addTask(t, project.ID, agent.ID, tc.taskDesc, 0)
			fixture.components.runtime.Start(fixture.ctx)

			active := waitForActiveProviderRun(t, fixture, task.ID)
			tc.startShape(t, inspectProviderRun(t, fixture, active))

			raw, err := exec.CommandContext(fixture.ctx, "docker", "inspect", active.ContainerID).CombinedOutput()
			requireNoError(t, err)
			for _, forbidden := range []string{tc.canary, secret} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("Docker inspect leaked %q", forbidden)
				}
			}

			// I1: the provider secret must reach the provider process as a runtime
			// env value (sourced from the secrets file), observable by writing it
			// into the host-mounted workspace from inside the container.
			observed, err := os.ReadFile(filepath.Join(active.WorkspacePath, "secret-observed"))
			requireNoError(t, err)
			if string(observed) != secret {
				t.Fatalf("provider secret observed in workspace = %q, want %q", observed, secret)
			}

			stopAndAssertLogRedaction(t, fixture, task.ID, active, tc.adapter, tc.stopReason, tc.canary, secret)
		})
	}
}

func stopAndAssertLogRedaction(t *testing.T, fixture *p3DockerFixture, taskID string, active core.Run, adapter, stopReason, canary, secret string) {
	t.Helper()
	if _, err := fixture.components.service.RequestRunStop(fixture.ctx, core.RunStopInput{
		RunID: active.ID, Reason: stopReason, RequestID: adapter + "-stop-" + active.ID,
	}); err != nil {
		t.Fatal(err)
	}
	terminal := waitForInterruptedRemoved(t, fixture, taskID, active.ID)
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

// providerFingerprintCase drives the I3/I4 Docker matrix: the argv/env
// projected from ProviderConfig must be token-exact, and the persisted
// ConfigFingerprint must match the fingerprint of the same launch inputs.
type providerFingerprintCase struct {
	adapter, name, canary, model, subagent, baseURL, effort string
	fixture                                                 func(t *testing.T) *p3DockerFixture
	shape                                                   func(t *testing.T, state containerruntime.LiveState, model, subagent, baseURL, effort string)
}

func providerFingerprintCases() []providerFingerprintCase {
	return []providerFingerprintCase{
		{adapter: "claude", name: "Claude Provider Agent", canary: "REDACT-CANARY-CLAUDE-PROVIDER",
			model: "claude-sonnet-4-7", subagent: "claude-haiku-4-5", baseURL: "https://api.example.com", effort: "high",
			fixture: newP3DockerFixture, shape: assertClaudeProviderShape},
		{adapter: "codex", name: "Codex Provider Agent", canary: "REDACT-CANARY-CODEX-PROVIDER",
			model: "codex-mini", subagent: "codex-haiku", baseURL: "https://example.invalid/v1", effort: "max",
			fixture: newCodexDockerFixture, shape: assertCodexProviderShape},
	}
}

func TestScriptedDockerProviderConfigFingerprint(t *testing.T) {
	for _, tc := range providerFingerprintCases() {
		t.Run(tc.adapter, func(t *testing.T) {
			fixture := tc.fixture(t)
			agent, err := fixture.components.service.AddAgent(fixture.ctx, core.AddAgentInput{
				DisplayName: tc.name, AdapterID: tc.adapter, Image: fixture.image,
				InstructionsText: tc.canary, Model: tc.model, SubagentModel: tc.subagent, BaseURL: tc.baseURL, Effort: tc.effort,
				RequestID: tc.adapter + "-provider-agent",
			})
			requireNoError(t, err)
			project := fixture.addProject(t, agent.ID)
			task := fixture.addTask(t, project.ID, agent.ID, tc.adapter+" provider matrix", 0)
			fixture.components.runtime.Start(fixture.ctx)

			active := waitForActiveProviderRun(t, fixture, task.ID)
			tc.shape(t, inspectProviderRun(t, fixture, active), tc.model, tc.subagent, tc.baseURL, tc.effort)

			// I4: the persisted ConfigFingerprint must equal the fingerprint
			// derived from the same launch inputs.
			wantFingerprint, err := core.RuntimeConfigFingerprint(core.RuntimeConfigFingerprintInput{
				AdapterID: tc.adapter, Image: fixture.image, Model: tc.model, SubagentModel: tc.subagent,
				BaseURL: tc.baseURL, Effort: tc.effort, InstructionsHash: hex.EncodeToString(sha256Sum(tc.canary)),
			})
			requireNoError(t, err)
			if active.ConfigFingerprint != wantFingerprint {
				t.Fatalf("%s ConfigFingerprint = %q, want %q", tc.adapter, active.ConfigFingerprint, wantFingerprint)
			}
			cancelProviderTask(t, fixture, task.ID)
		})
	}
}

func assertEnvDigests(t *testing.T, state containerruntime.LiveState, want map[string]string) {
	t.Helper()
	digests := make(map[string]string, len(state.Environment))
	for _, fact := range state.Environment {
		digests[fact.Name] = fact.ValueDigest
	}
	for name, value := range want {
		digest := hex.EncodeToString(sha256Sum(value))
		if digests[name] != digest {
			t.Fatalf("env %s digest = %q, want %q (facts=%#v)", name, digests[name], digest, state.Environment)
		}
	}
}
