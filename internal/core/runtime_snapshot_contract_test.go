package core_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

// R4-R6: an Agent configuration change between claim and launch must make the
// claim-time ConfigFingerprint stale, so BeginRunLaunch fails closed with
// STALE_RUN and never touches the durable Run row or Event stream.
func TestRunLaunchSnapshotRejectsAgentConfigChangeAfterClaim(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*core.AgentConfigInput)
	}{
		{"model", func(input *core.AgentConfigInput) { input.Model = "changed-model" }},
		{"subagent model", func(input *core.AgentConfigInput) { input.SubagentModel = "changed-subagent" }},
		{"base URL", func(input *core.AgentConfigInput) { input.BaseURL = "https://changed.example/v1" }},
		{"instructions", func(input *core.AgentConfigInput) { input.InstructionsText = "Changed durable instructions for the assigned task." }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			project, claim := createClaimedWorkRun(t, h, "snapshot-"+test.name, 0)
			root := t.TempDir()
			instructionsHash, fingerprint := launchFingerprint(t, h, claim.Run.ID)
			agent, err := h.database.Agent(context.Background(), claim.Run.AgentID)
			requireNoError(t, err)
			config := core.AgentConfigInput{
				DisplayName: agent.DisplayName, AdapterID: agent.AdapterID, Image: agent.Image,
				InstructionsText: agent.InstructionsText, Model: agent.Model,
				SubagentModel: agent.SubagentModel, BaseURL: agent.BaseURL, Effort: agent.Effort,
			}
			test.mutate(&config)
			if _, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
				ID: agent.ID, Version: agent.Version, AgentConfigInput: config,
				RequestID: "snapshot-update-" + test.name,
			}); err != nil {
				t.Fatal(err)
			}
			// The re-derived snapshot must have moved, otherwise this test would
			// pass vacuously and the config change never reached the fingerprint.
			launchAfter, err := h.service.RuntimeLaunchContext(context.Background(), claim.Run.ID)
			requireNoError(t, err)
			if launchAfter.ConfigFingerprint == fingerprint {
				t.Fatalf("%s change did not move the config fingerprint", test.name)
			}
			before := durableSignature(t, h.database, project.ID)
			input := core.RunLaunchInput{
				RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-snapshot",
				WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
				LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
				ConfigFingerprint: fingerprint, LaunchMode: "start",
				CleanupOperationID: "cleanup-snapshot", RequestID: "prepare-snapshot-" + test.name,
			}
			if _, err := h.service.BeginRunLaunch(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
				t.Fatalf("BeginRunLaunch after %s change error = %v, want %s", test.name, err, core.CodeStaleRun)
			}
			h.requireDurableSignature(t, project.ID, before)
			run, err := h.database.Run(context.Background(), claim.Run.ID)
			requireNoError(t, err)
			if run.LaunchNonce != "" || run.InstructionsHash != "" || run.ConfigFingerprint != "" {
				t.Fatalf("failed BeginRunLaunch mutated Run: %#v", run)
			}
		})
	}
}

// R6 effort: the one-shot adapter of the shared harness declares no allowed
// efforts, so an effort change needs a fixture whose adapter registry does.
func TestRunLaunchSnapshotRejectsEffortChangeAfterClaim(t *testing.T) {
	h := newEffortHarness(t)
	agent, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "effort-agent", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "Work only on the assigned Task.", Effort: "high",
		RequestID: "effort-snapshot-agent",
	})
	requireNoError(t, err)
	project := h.addProject(t, "effort-snapshot-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "effort snapshot", MaxRetries: 0, RequestID: "effort-snapshot-task",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	root := t.TempDir()
	instructionsHash, fingerprint := launchFingerprint(t, h, claim.Run.ID)
	if _, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
		ID: agent.ID, Version: agent.Version,
		AgentConfigInput: core.AgentConfigInput{
			DisplayName: agent.DisplayName, AdapterID: agent.AdapterID, Image: agent.Image,
			InstructionsText: agent.InstructionsText, Effort: "low",
		},
		RequestID: "effort-snapshot-update",
	}); err != nil {
		t.Fatal(err)
	}
	before := durableSignature(t, h.database, project.ID)
	input := core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-effort-snapshot",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
		ConfigFingerprint: fingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-effort-snapshot", RequestID: "prepare-effort-snapshot",
	}
	if _, err := h.service.BeginRunLaunch(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("BeginRunLaunch after effort change error = %v, want %s", err, core.CodeStaleRun)
	}
	h.requireDurableSignature(t, project.ID, before)
}

// R7: an unchanged Agent between claim and launch preserves the claim-time
// snapshot, so BeginRunLaunch succeeds, persists the same fingerprint, and
// re-derives an identical fingerprint on a later launch-context read.
func TestRunLaunchSnapshotAllowsUnchangedConfigAndPersistsFingerprint(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "snapshot-clean", 0)
	root := t.TempDir()
	instructionsHash, fingerprint := launchFingerprint(t, h, claim.Run.ID)
	input := core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-snapshot-clean",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
		ConfigFingerprint: fingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-snapshot-clean", RequestID: "prepare-snapshot-clean",
	}
	prepared, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if prepared.InstructionsHash != instructionsHash || prepared.ConfigFingerprint != fingerprint {
		t.Fatalf("persisted snapshot mismatch: hash=%q/%q fingerprint=%q/%q",
			prepared.InstructionsHash, instructionsHash, prepared.ConfigFingerprint, fingerprint)
	}
	launch, err := h.service.RuntimeLaunchContext(context.Background(), claim.Run.ID)
	requireNoError(t, err)
	if launch.InstructionsHash != instructionsHash || launch.ConfigFingerprint != fingerprint {
		t.Fatalf("launch context drifted after prepare: hash=%q/%q fingerprint=%q/%q",
			launch.InstructionsHash, instructionsHash, launch.ConfigFingerprint, fingerprint)
	}
	before := durableSignature(t, h.database, project.ID)
	replayed, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if replayed.ID != prepared.ID || replayed.Version != prepared.Version {
		t.Fatalf("idempotent replay = %#v, want %#v", replayed, prepared)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func newEffortHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coordplane.db")
	database, err := store.Open(context.Background(), path)
	requireNoError(t, err)
	clock := &testClock{value: time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)}
	ids := &testIDs{}
	git := &fakeGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	service, err := core.NewService(database, git, core.ServiceOptions{
		Now: clock.Now, NewID: ids.New, MaxParallelRuns: 4,
		AdapterIDs: []string{"claude"},
		Adapters: []core.AdapterDescriptor{
			{ID: "claude", Name: "claude", AllowedEfforts: []string{"low", "high"}},
		},
	})
	requireNoError(t, err)
	service.SetReady(true, "")
	h := &harness{t: t, path: path, database: database, service: service, git: git, clock: clock, ids: ids}
	t.Cleanup(func() { _ = h.database.Close() })
	return h
}
