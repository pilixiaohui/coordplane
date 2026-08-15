package core_test

import (
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
)

// R4-R6: an Agent configuration change between claim and launch must make the
// claim-time ConfigFingerprint stale, so BeginRunLaunch fails closed with
// STALE_RUN and never touches the durable Run row or Event stream.
func TestRunLaunchSnapshotRejectsConfigChangeAfterClaim(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.AgentConfigInput)
	}{
		{"model", func(input *core.AgentConfigInput) { input.Model = "changed-model" }},
		{"subagent model", func(input *core.AgentConfigInput) { input.SubagentModel = "changed-subagent" }},
		{"base URL", func(input *core.AgentConfigInput) { input.BaseURL = "https://changed.example/v1" }},
		{"instructions", func(input *core.AgentConfigInput) {
			input.InstructionsText = "Changed durable instructions for the assigned task."
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			project, claim := createClaimedWorkRun(t, h, "snapshot-"+test.name, 0)
			agent, err := h.database.Agent(context.Background(), claim.Run.AgentID)
			requireNoError(t, err)
			input := snapshotLaunchInput(t, h, claim, test.name)
			config := agentConfigInput(agent)
			test.mutate(&config)
			if _, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{ID: agent.ID, Version: agent.Version, AgentConfigInput: config, RequestID: "snapshot-update-" + test.name}); err != nil {
				t.Fatal(err)
			}
			assertStaleSnapshot(t, h, project, input, test.name)
		})
	}
}

// R6 effort: the one-shot adapter of the shared harness declares no allowed
// efforts, so an effort change needs a fixture whose adapter registry does.
func TestRunLaunchSnapshotRejectsEffortChangeAfterClaim(t *testing.T) {
	h := newEffortHarness(t)
	agent, err := h.service.AddAgent(context.Background(), core.AddAgentInput{DisplayName: "effort-agent", AdapterID: "claude", Image: "agent:latest", InstructionsText: "Work only on the assigned Task.", Effort: "high", RequestID: "effort-snapshot-agent"})
	requireNoError(t, err)
	project := h.addProject(t, "effort-snapshot-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork, Title: "effort snapshot", MaxRetries: 0, RequestID: "effort-snapshot-task"})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	input := snapshotLaunchInput(t, h, claim, "effort")
	if _, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
		ID: agent.ID, Version: agent.Version,
		AgentConfigInput: core.AgentConfigInput{DisplayName: agent.DisplayName, AdapterID: agent.AdapterID, Image: agent.Image, InstructionsText: agent.InstructionsText, Effort: "low"},
		RequestID:        "effort-snapshot-update",
	}); err != nil {
		t.Fatal(err)
	}
	assertStaleSnapshot(t, h, project, input, "effort")
}

// assertStaleSnapshot drives the R4-R6 assertion: the fingerprint must have
// moved since the claim-time input, BeginRunLaunch must fail closed, and the
// durable Run row must stay untouched.
func assertStaleSnapshot(t *testing.T, h *harness, project core.Project, input core.RunLaunchInput, name string) {
	t.Helper()
	launchAfter, err := h.service.RuntimeLaunchContext(context.Background(), input.RunID)
	requireNoError(t, err)
	if launchAfter.ConfigFingerprint == input.ConfigFingerprint {
		t.Fatalf("%s change did not move the config fingerprint", name)
	}
	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.BeginRunLaunch(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("BeginRunLaunch after %s change error = %v, want %s", name, err, core.CodeStaleRun)
	}
	h.requireDurableSignature(t, project.ID, before)
	run, err := h.database.Run(context.Background(), input.RunID)
	requireNoError(t, err)
	if run.LaunchNonce != "" || run.InstructionsHash != "" || run.ConfigFingerprint != "" {
		t.Fatalf("failed BeginRunLaunch mutated Run: %#v", run)
	}
}

// R7: an unchanged Agent between claim and launch preserves the claim-time
// snapshot, so BeginRunLaunch succeeds, persists the same fingerprint, and
// re-derives an identical fingerprint on a later launch-context read.
func TestRunLaunchSnapshotAllowsUnchangedConfigAndPersistsFingerprint(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "snapshot-clean", 0)
	input := snapshotLaunchInput(t, h, claim, "snapshot-clean")
	prepared, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if prepared.InstructionsHash != input.InstructionsHash || prepared.ConfigFingerprint != input.ConfigFingerprint {
		t.Fatalf("persisted snapshot mismatch: hash=%q/%q fingerprint=%q/%q", prepared.InstructionsHash, input.InstructionsHash, prepared.ConfigFingerprint, input.ConfigFingerprint)
	}
	launch, err := h.service.RuntimeLaunchContext(context.Background(), claim.Run.ID)
	requireNoError(t, err)
	if launch.InstructionsHash != input.InstructionsHash || launch.ConfigFingerprint != input.ConfigFingerprint {
		t.Fatalf("launch context drifted after prepare: hash=%q/%q fingerprint=%q/%q", launch.InstructionsHash, input.InstructionsHash, launch.ConfigFingerprint, input.ConfigFingerprint)
	}
	before := durableSignature(t, h.database, project.ID)
	replayed, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if replayed.ID != prepared.ID || replayed.Version != prepared.Version {
		t.Fatalf("idempotent replay = %#v, want %#v", replayed, prepared)
	}
	h.requireDurableSignature(t, project.ID, before)
}

// snapshotLaunchInput builds a BeginRunLaunch input from the claim-time
// instructions hash and config fingerprint.
func snapshotLaunchInput(t *testing.T, h *harness, claim core.Claim, name string) core.RunLaunchInput {
	t.Helper()
	instructionsHash, fingerprint := launchFingerprint(t, h, claim.Run.ID)
	root := filepath.Dir(h.path)
	return core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-" + name,
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
		ConfigFingerprint: fingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-" + name, RequestID: "prepare-" + name,
	}
}

func agentConfigInput(agent core.Agent) core.AgentConfigInput {
	return core.AgentConfigInput{DisplayName: agent.DisplayName, AdapterID: agent.AdapterID, Image: agent.Image, InstructionsText: agent.InstructionsText, Model: agent.Model, SubagentModel: agent.SubagentModel, BaseURL: agent.BaseURL, Effort: agent.Effort}
}

func newEffortHarness(t *testing.T) *harness {
	return newAdapterHarness(t, []string{"claude"}, core.AdapterDescriptor{ID: "claude", Name: "claude", AllowedEfforts: []string{"low", "high"}})
}
