package runtime

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestProviderAuditFailsClosedWhenTranscriptIdentityIsMissing(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.New(db).Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := formatTime(time.Now())
	for _, statement := range []string{
		`INSERT INTO work_contracts (id, title, objective, target_kind, target_id, status, created_at, updated_at) VALUES ('ctr_provider_identity', 'provider identity', 'fail closed', 'agent', 'developer', 'active', '` + now + `', '` + now + `')`,
		`INSERT INTO assignments (id, contract_id, assignee_agent_id, state, reason, created_at, updated_at) VALUES ('asg_provider_identity', 'ctr_provider_identity', 'developer', 'claimed', 'provider identity', '` + now + `', '` + now + `')`,
		`INSERT INTO leases (id, assignment_id, agent_id, state, expires_at, created_at, updated_at) VALUES ('lease_provider_identity', 'asg_provider_identity', 'developer', 'active', '` + formatTime(time.Now().Add(time.Hour)) + `', '` + now + `', '` + now + `')`,
		`INSERT INTO attempts (id, lease_id, cli_backend, runtime_kind, start_reason, status, started_at) VALUES ('att_provider_identity', 'lease_provider_identity', 'claude', 'docker', 'start', 'running', '` + now + `')`,
		`INSERT INTO cli_sessions (id, attempt_id, runtime_id, agent_id, cli_backend, profile_name, session_native_id, state, start_reason, started_at, updated_at) VALUES ('cli_provider_identity', 'att_provider_identity', 'rt_provider_identity', 'developer', 'claude', 'claude', 'native_provider_identity', 'running', 'start', '` + now + `', '` + now + `')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed provider audit identity fixture: %v", err)
		}
	}

	adapter := &CommandCLIAdapter{
		db:      db,
		profile: CommandCLIProfile{Backend: "claude", RuntimeCommandPolicies: map[string]RuntimeCommandPolicy{"docker-default": {NonInteractiveApproval: true}}},
	}
	_, err = adapter.projectProviderToolOutcomes(ctx, "cli_provider_identity", RuntimeInstance{
		RuntimeID:      "rt_provider_identity",
		RuntimeProfile: "docker-default",
		AttemptID:      "att_provider_identity",
		LeaseID:        "lease_provider_identity",
	}, ContainerExecResult{Stdout: []byte(`{"type":"result"}` + "\n")}, "")
	if err == nil || !strings.Contains(err.Error(), "transcript identity is unavailable") {
		t.Fatalf("missing transcript identity error = %v", err)
	}
	var state, code string
	if err := db.QueryRowContext(ctx, `SELECT provider_audit_state, provider_audit_error_code FROM cli_sessions WHERE id = 'cli_provider_identity'`).Scan(&state, &code); err != nil {
		t.Fatalf("query provider audit state: %v", err)
	}
	if state != "failed" || code != providerAuditParseFailedCode {
		t.Fatalf("provider audit state = %s/%s, want failed/%s", state, code, providerAuditParseFailedCode)
	}
	var outcomes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_tool_outcomes`).Scan(&outcomes); err != nil {
		t.Fatalf("count provider outcomes: %v", err)
	}
	if outcomes != 0 {
		t.Fatalf("provider outcomes with missing transcript identity = %d, want 0", outcomes)
	}
}
