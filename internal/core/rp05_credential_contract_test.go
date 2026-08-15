package core_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
	_ "modernc.org/sqlite"
)

// RP-05: human credentials are stored hashed only; a revoked credential
// rejects the next request; rotation invalidates the old secret immediately.
func requireDenied(t *testing.T, err error) {
	t.Helper()
	if !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("credential gate error = %v, want SCOPE_DENIED", err)
	}
}

func TestRP05CredentialLifecycleHashesAndRevokes(t *testing.T) {
	h := newHarness(t)
	if err := h.service.AuthenticateOperator(context.Background(), ""); err != nil {
		t.Fatalf("fresh install must keep the legacy trust boundary: %v", err)
	}

	added, err := h.service.AddCredential(context.Background(), core.AddCredentialInput{
		ParticipantID: core.DefaultHumanParticipantID, Kind: core.CredentialKindOperatorToken,
		Secret: "first-secret-0123456789", RequestID: "rp05-add",
	})
	requireNoError(t, err)
	if added.Status != core.CredentialActive {
		t.Fatalf("added status = %q", added.Status)
	}
	// The plaintext secret must never be persisted.
	var secretHash string
	db, err := sql.Open("sqlite", "file:"+h.path+"?_pragma=busy_timeout(5000)")
	requireNoError(t, err)
	if err := db.QueryRowContext(context.Background(), `SELECT secret_hash FROM credentials WHERE id=?`, added.ID).Scan(&secretHash); err != nil {
		t.Fatal(err)
	}
	if secretHash == "" || strings.Contains(secretHash, "first-secret") {
		t.Fatalf("secret persisted in plaintext or empty: %q", secretHash)
	}

	// Wrong or missing secrets are rejected; the correct one passes.
	requireDenied(t, h.service.AuthenticateOperator(context.Background(), ""))
	requireDenied(t, h.service.AuthenticateOperator(context.Background(), "wrong-secret-0123456789"))
	requireNoError(t, h.service.AuthenticateOperator(context.Background(), "first-secret-0123456789"))

	// A second active credential cannot be added while one is active.
	if _, err := h.service.AddCredential(context.Background(), core.AddCredentialInput{
		ParticipantID: core.DefaultHumanParticipantID, Kind: core.CredentialKindOperatorToken,
		Secret: "second-secret-0123456789", RequestID: "rp05-add-dup",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("duplicate add error = %v", err)
	}

	// Rotation: the new secret authenticates, the old one is rejected.
	rotated, err := h.service.RotateCredential(context.Background(), core.AddCredentialInput{
		ParticipantID: core.DefaultHumanParticipantID, Kind: core.CredentialKindOperatorToken,
		Secret: "second-secret-0123456789", RequestID: "rp05-rotate",
	})
	requireNoError(t, err)
	if rotated.ID == added.ID {
		t.Fatal("rotation reused the old credential id")
	}
	requireDenied(t, h.service.AuthenticateOperator(context.Background(), "first-secret-0123456789"))
	requireNoError(t, h.service.AuthenticateOperator(context.Background(), "second-secret-0123456789"))

	// Revocation: every subsequent request is rejected, even with the correct
	// secret, and the state survives a restart of the store.
	if _, err := h.service.RevokeCredential(context.Background(), core.DefaultHumanParticipantID, "rp05-revoke"); err != nil {
		t.Fatal(err)
	}
	requireDenied(t, h.service.AuthenticateOperator(context.Background(), "second-secret-0123456789"))

	// The revocation must persist across a Daemon restart: re-open the same
	// SQLite file and a fresh Service, then the revoked secret is still rejected.
	requireNoError(t, h.database.Close())
	reopened, err := store.Open(context.Background(), h.path)
	requireNoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := core.NewService(reopened, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
	})
	requireNoError(t, err)
	restarted.SetReady(true, "")
	requireDenied(t, restarted.AuthenticateOperator(context.Background(), "second-secret-0123456789"))
}
