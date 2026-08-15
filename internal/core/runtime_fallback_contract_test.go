package core_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestBeginRunLaunchFallbackSourceRequiresMatchingConfigFingerprint(t *testing.T) {
	t.Run("matching fingerprint is accepted", func(t *testing.T) {
		h := newHarness(t)
		project, claim := createClaimedWorkRun(t, h, "fallback-match", 0)
		_, fingerprint := launchFingerprint(t, h, claim.Run.ID)
		previous := insertFailedResumeFallbackSource(t, h, project, claim, fingerprint)

		prepared, err := h.service.BeginRunLaunch(context.Background(), fallbackLaunchInput(t, h, claim, previous, fingerprint, "prepare-fallback-match"))
		requireNoError(t, err)
		if prepared.ConfigFingerprint != fingerprint || prepared.ResumedFromRunID != previous.ID {
			t.Fatalf("prepared fallback = %#v", prepared)
		}
		events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, RunID: claim.Run.ID})
		requireNoError(t, err)
		if countEvent(events, "run.resume_fallback") != 1 || countEvent(events, "run.launch_prepared") != 1 {
			t.Fatalf("fallback Events = %#v", events)
		}
	})

	t.Run("changed fingerprint is rejected", func(t *testing.T) {
		h := newHarness(t)
		project, claim := createClaimedWorkRun(t, h, "fallback-mismatch", 0)
		_, fingerprint := launchFingerprint(t, h, claim.Run.ID)
		previous := insertFailedResumeFallbackSource(t, h, project, claim, strings.Repeat("b", 64))
		before := durableSignature(t, h.database, project.ID)

		if _, err := h.service.BeginRunLaunch(context.Background(), fallbackLaunchInput(t, h, claim, previous, fingerprint, "prepare-fallback-mismatch")); !core.IsCode(err, core.CodeResumeUnavailable) {
			t.Fatalf("changed fingerprint error = %v, want %s", err, core.CodeResumeUnavailable)
		}
		h.requireDurableSignature(t, project.ID, before)
		run, err := h.database.Run(context.Background(), claim.Run.ID)
		requireNoError(t, err)
		if run.LaunchNonce != "" || run.ConfigFingerprint != "" || run.LaunchPhase != core.LaunchIntent {
			t.Fatalf("changed fingerprint mutated current Run: %#v", run)
		}
		events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, RunID: claim.Run.ID})
		requireNoError(t, err)
		if countEvent(events, "run.resume_fallback") != 0 || countEvent(events, "run.launch_prepared") != 0 {
			t.Fatalf("changed fingerprint wrote fallback Events: %#v", events)
		}
	})
}

func insertFailedResumeFallbackSource(t *testing.T, h *harness, project core.Project, claim core.Claim, fingerprint string) core.Run {
	t.Helper()
	now := h.clock.Now().Format(time.RFC3339Nano)
	previous := core.Run{
		ID: "fallback-" + claim.Run.ID, ProjectID: project.ID, TaskID: claim.Task.ID, AgentID: claim.Run.AgentID,
		Generation: 1, AdapterID: claim.Run.AdapterID, Image: claim.Run.Image,
		ConfigFingerprint: fingerprint, State: core.RunFailed, RuntimeErrorCode: string(core.CodeResumeUnavailable),
		TokenHash: "fallback-token-" + claim.Run.ID, CleanupState: core.CleanupRemoved,
		LaunchPhase: core.LaunchProcessObserved, LaunchMode: "resume", IsolationSpecVersion: core.RunIsolationSpecCurrent,
		Version: 1, CreatedAt: now, StartedAt: now, EndedAt: now,
	}
	requireNoError(t, h.database.Transact(context.Background(), func(tx core.Transaction) error {
		return tx.InsertRun(previous)
	}))
	return previous
}

func fallbackLaunchInput(t *testing.T, h *harness, claim core.Claim, previous core.Run, fingerprint, requestID string) core.RunLaunchInput {
	t.Helper()
	root := t.TempDir()
	instructionsHash, _ := launchFingerprint(t, h, claim.Run.ID)
	return core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-" + requestID,
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
		ConfigFingerprint: fingerprint, LaunchMode: "start", ResumedFromRunID: previous.ID,
		CleanupOperationID: "cleanup-" + requestID, RequestID: requestID,
	}
}
