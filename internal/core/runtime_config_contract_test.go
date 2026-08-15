package core_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestBeginRunLaunchPersistsFingerprintAndIncludesItInLaunchIntent(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "fingerprint-launch", 0)
	root := t.TempDir()
	instructionsHash, fingerprint := launchFingerprint(t, h, claim.Run.ID)
	input := core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-fingerprint",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: instructionsHash,
		ConfigFingerprint: fingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-fingerprint", RequestID: "prepare-fingerprint",
	}
	prepared, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if prepared.ConfigFingerprint != fingerprint {
		t.Fatalf("persisted fingerprint = %q, want %q", prepared.ConfigFingerprint, fingerprint)
	}
	before := durableSignature(t, h.database, project.ID)
	replayed, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if replayed.ID != prepared.ID || replayed.Version != prepared.Version {
		t.Fatalf("idempotent replay = %#v, want %#v", replayed, prepared)
	}
	h.requireDurableSignature(t, project.ID, before)

	t.Run("same fingerprint different nonce is action in progress", func(t *testing.T) {
		changed := input
		changed.LaunchNonce = "different-nonce"
		changed.RequestID = "different-nonce"
		if _, err := h.service.BeginRunLaunch(context.Background(), changed); !core.IsCode(err, core.CodeActionInProgress) {
			t.Fatalf("different nonce error = %v, want %s", err, core.CodeActionInProgress)
		}
		h.requireDurableSignature(t, project.ID, before)
	})

	t.Run("changed fingerprint is a stale run", func(t *testing.T) {
		changed := input
		changed.ConfigFingerprint = strings.Repeat("b", 64)
		changed.RequestID = "different-fingerprint"
		if _, err := h.service.BeginRunLaunch(context.Background(), changed); !core.IsCode(err, core.CodeStaleRun) {
			t.Fatalf("different fingerprint error = %v, want %s", err, core.CodeStaleRun)
		}
		h.requireDurableSignature(t, project.ID, before)
	})
}

// L4: BeginRunLaunch must bind the persisted InstructionsHash to the Agent's
// immutable instructions, not trust the caller's field. A fresh run launched
// with the correct config fingerprint but a mismatched hash must fail closed
// with no durable writes.
func TestBeginRunLaunchRejectsMismatchedInstructionsHash(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "instructions-hash-mismatch", 0)
	root := t.TempDir()
	_, fingerprint := launchFingerprint(t, h, claim.Run.ID)
	input := core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-instructions-hash",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: strings.Repeat("a", 64),
		ConfigFingerprint: fingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-instructions-hash", RequestID: "prepare-instructions-hash",
	}
	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.BeginRunLaunch(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("mismatched instructions hash error = %v, want %s", err, core.CodeStaleRun)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func TestBeginRunLaunchRejectsInvalidFingerprintWithoutWrites(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "invalid-fingerprint-launch", 0)
	root := t.TempDir()
	before := durableSignature(t, h.database, project.ID)
	for _, fingerprint := range []string{"", "not-hex", strings.Repeat("g", 64), strings.Repeat("a", 63)} {
		input := core.RunLaunchInput{
			RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-invalid-fingerprint",
			WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
			LogPath: filepath.Join(root, "run.log"), InstructionsHash: "sha256-instructions",
			ConfigFingerprint: fingerprint, LaunchMode: "start",
			CleanupOperationID: "cleanup-invalid-fingerprint", RequestID: "invalid-" + fingerprint,
		}
		if _, err := h.service.BeginRunLaunch(context.Background(), input); !core.IsCode(err, core.CodeInvalidArgument) {
			t.Fatalf("fingerprint %q error = %v, want %s", fingerprint, err, core.CodeInvalidArgument)
		}
	}
	h.requireDurableSignature(t, project.ID, before)
}
