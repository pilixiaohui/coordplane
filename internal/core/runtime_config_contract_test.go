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
	firstFingerprint := strings.Repeat("a", 64)
	secondFingerprint := strings.Repeat("b", 64)
	input := core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-fingerprint",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: "sha256-instructions",
		ConfigFingerprint: firstFingerprint, LaunchMode: "start",
		CleanupOperationID: "cleanup-fingerprint", RequestID: "prepare-fingerprint",
	}
	prepared, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if prepared.ConfigFingerprint != firstFingerprint {
		t.Fatalf("persisted fingerprint = %q, want %q", prepared.ConfigFingerprint, firstFingerprint)
	}
	before := durableSignature(t, h.database, project.ID)
	replayed, err := h.service.BeginRunLaunch(context.Background(), input)
	requireNoError(t, err)
	if replayed.ID != prepared.ID || replayed.Version != prepared.Version {
		t.Fatalf("idempotent replay = %#v, want %#v", replayed, prepared)
	}
	h.requireDurableSignature(t, project.ID, before)

	changed := input
	changed.ConfigFingerprint = secondFingerprint
	changed.RequestID = "different-fingerprint"
	if _, err := h.service.BeginRunLaunch(context.Background(), changed); !core.IsCode(err, core.CodeActionInProgress) {
		t.Fatalf("different fingerprint error = %v, want %s", err, core.CodeActionInProgress)
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
