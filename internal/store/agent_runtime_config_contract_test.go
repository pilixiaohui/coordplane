package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestAgentParticipantAndRunConfigFieldsRoundTripAndSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-config.db")
	database, err := Open(ctx, path)
	requireNoError(t, err)
	const now = "2026-08-14T00:00:00.000000000Z"
	agent := core.Agent{
		ID: "agt-config", DisplayName: "Config", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "persisted prompt", Model: "model-a", SubagentModel: "sub-a",
		BaseURL: "https://example.invalid/v1", Effort: "medium", Status: core.AgentActive,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	participant := core.Participant{
		ID: agent.ID, Kind: core.ParticipantKindCLIAgent, DisplayName: agent.DisplayName,
		Status: string(agent.Status), AdapterID: agent.AdapterID, Image: agent.Image,
		InstructionsText: agent.InstructionsText, Model: agent.Model, SubagentModel: agent.SubagentModel,
		BaseURL: agent.BaseURL, Effort: agent.Effort, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	project := core.Project{
		ID: "prj-config", Name: "Config", Source: "/source", SourceRef: "refs/heads/main",
		InitialSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ControlRepoPath: "/control.git",
		CanonicalRef: "refs/heads/main", CanonicalSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: core.ProjectActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	task := core.Task{
		ID: "tsk-config", ProjectID: project.ID, Kind: core.TaskWork, CreatedByKind: "boss",
		AssigneeAgentID: agent.ID, Title: "Config", Description: "Config", Status: core.TaskQueued,
		Generation: 1, NextRunAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	fingerprint := strings.Repeat("a", 64)
	requireNoError(t, database.Transact(ctx, func(tx core.Transaction) error {
		if err := tx.InsertAgent(agent); err != nil {
			return err
		}
		if err := tx.InsertProject(project); err != nil {
			return err
		}
		if err := tx.InsertTask(task); err != nil {
			return err
		}
		return tx.InsertParticipant(participant)
	}))
	requireNoError(t, database.Close())

	reopened, err := Open(ctx, path)
	requireNoError(t, err)
	defer reopened.Close()
	gotAgent, err := reopened.Agent(ctx, agent.ID)
	requireNoError(t, err)
	if gotAgent.InstructionsText != agent.InstructionsText || gotAgent.Model != agent.Model ||
		gotAgent.SubagentModel != agent.SubagentModel || gotAgent.BaseURL != agent.BaseURL ||
		gotAgent.Effort != agent.Effort {
		t.Fatalf("restored Agent = %#v", gotAgent)
	}
	gotParticipant, err := reopened.Participant(ctx, agent.ID)
	requireNoError(t, err)
	if gotParticipant.InstructionsText != participant.InstructionsText ||
		gotParticipant.Model != participant.Model ||
		gotParticipant.SubagentModel != participant.SubagentModel ||
		gotParticipant.BaseURL != participant.BaseURL ||
		gotParticipant.Effort != participant.Effort {
		t.Fatalf("restored Participant = %#v", gotParticipant)
	}

	// Exercise the Run fingerprint column directly so a storage-layer
	// regression cannot hide behind the core launch path.
	run := core.Run{
		ID: "run-config", ProjectID: project.ID, TaskID: task.ID, AgentID: agent.ID, Generation: 1,
		AdapterID: agent.AdapterID, Image: agent.Image, InstructionsHash: "instructions-hash",
		ConfigFingerprint: fingerprint, State: core.RunStarting, TokenHash: "token-config",
		CleanupState: "not_needed", LaunchPhase: "intent", ContainerName: "coordplane-run-config",
		LaunchMode: "start", Version: 1, CreatedAt: now,
	}
	requireNoError(t, reopened.Transact(ctx, func(tx core.Transaction) error { return tx.InsertRun(run) }))
	gotRun, err := reopened.Run(ctx, run.ID)
	requireNoError(t, err)
	if gotRun.ConfigFingerprint != fingerprint {
		t.Fatalf("restored Run fingerprint = %q, want %q", gotRun.ConfigFingerprint, fingerprint)
	}
}

func TestParticipantUpdateCASRejectsStaleVersionWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, ctx, "participant-cas.db")
	const now = "2026-08-14T00:00:00.000000000Z"
	participant := core.Participant{
		ID: "agt-participant", Kind: core.ParticipantKindCLIAgent, DisplayName: "Participant",
		Status: "active", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "prompt", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	requireNoError(t, database.Transact(ctx, func(tx core.Transaction) error {
		return tx.InsertParticipant(participant)
	}))
	before, err := database.Participant(ctx, participant.ID)
	requireNoError(t, err)
	err = database.Transact(ctx, func(tx core.Transaction) error {
		stale := participant
		stale.Version++
		return tx.UpdateParticipant(stale, participant.Version-1)
	})
	if !core.IsCode(err, core.CodeVersionConflict) {
		t.Fatalf("stale participant CAS error = %v, want %s", err, core.CodeVersionConflict)
	}
	after, err := database.Participant(ctx, participant.ID)
	requireNoError(t, err)
	if after.Version != before.Version || after.DisplayName != before.DisplayName {
		t.Fatalf("stale participant update changed row: before=%#v after=%#v", before, after)
	}
}
