package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestProviderAuditRequirementBackfillFromLegacyDatabases(t *testing.T) {
	for _, tc := range []struct {
		name        string
		through     string
		wantApplied []string
	}{
		{name: "021 to 023", through: "021_provider_tool_outcomes", wantApplied: []string{"022_provider_audit_requirement", "023_provider_audit_requirement_backfill"}},
		{name: "022 to 023", through: "022_provider_audit_requirement", wantApplied: []string{"023_provider_audit_requirement_backfill"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
			if err != nil {
				t.Fatalf("open legacy SQLite: %v", err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			migrateTestDBThrough(t, ctx, db, tc.through)
			seedLegacyProviderAuditMatrix(t, ctx, db, tc.through == "022_provider_audit_requirement")

			before := migrationDataCounts(t, ctx, db)
			result, err := New(db).Migrate(ctx)
			if err != nil {
				t.Fatalf("migrate legacy provider audit DB: %v", err)
			}
			if !reflect.DeepEqual(result.Applied, tc.wantApplied) {
				t.Fatalf("applied migrations = %v, want %v", result.Applied, tc.wantApplied)
			}

			want := map[string][4]any{
				"cli_legacy_complete":   {"required", "legacy_audit_terminal", "complete", ""},
				"cli_legacy_failed":     {"required", "legacy_audit_terminal", "failed", "PROVIDER_AUDIT_PARSE_FAILED"},
				"cli_policy_incomplete": {"required", "contract_policy_match", "failed", "PROVIDER_AUDIT_LEGACY_INCOMPLETE"},
				"cli_policy_pending":    {"required", "contract_policy_match", "not_requested", ""},
				"cli_plain_claude":      {"not_required", "contract_policy_absent", "not_requested", ""},
				"cli_non_claude":        {"not_required", "legacy_non_claude", "not_requested", ""},
				"cli_unresolved":        {"unresolved", "scope_missing", "failed", "PROVIDER_AUDIT_REQUIREMENT_UNRESOLVED"},
			}
			if tc.through == "022_provider_audit_requirement" {
				want["cli_explicit_required"] = [4]any{"required", "explicit_required", "failed", "PROVIDER_AUDIT_LEGACY_INCOMPLETE"}
			}
			for id, expected := range want {
				var requirement, reason, auditState, auditCode string
				var compatibleRequired int
				if err := db.QueryRowContext(ctx, `
SELECT provider_audit_requirement_state, provider_audit_requirement_reason,
       provider_audit_state, provider_audit_error_code, provider_audit_required
FROM cli_sessions WHERE id = ?`, id).Scan(&requirement, &reason, &auditState, &auditCode, &compatibleRequired); err != nil {
					t.Fatalf("query migrated session %s: %v", id, err)
				}
				got := [4]any{requirement, reason, auditState, auditCode}
				if got != expected {
					t.Fatalf("session %s = %#v, want %#v", id, got, expected)
				}
				wantBool := 1
				if requirement == "not_required" {
					wantBool = 0
				}
				if compatibleRequired != wantBool {
					t.Fatalf("session %s compatibility bool = %d, want %d for %s", id, compatibleRequired, wantBool, requirement)
				}
			}
			if after := migrationDataCounts(t, ctx, db); !reflect.DeepEqual(after, before) {
				t.Fatalf("migration changed protected row counts: before=%v after=%v", before, after)
			}
			var integrity string
			if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity_check = %q/%v", integrity, err)
			}
			var foreignKeyFailures int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyFailures); err != nil || foreignKeyFailures != 0 {
				t.Fatalf("foreign_key_check failures = %d/%v", foreignKeyFailures, err)
			}
			second, err := New(db).Migrate(ctx)
			if err != nil || len(second.Applied) != 0 {
				t.Fatalf("second migrate = %+v/%v, want no-op", second, err)
			}
		})
	}
}

func migrateTestDBThrough(t *testing.T, ctx context.Context, db *sql.DB, through string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
)`); err != nil {
		t.Fatalf("create migration table: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin legacy migration: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	found := false
	for _, migration := range migrations {
		if err := execStatements(ctx, tx, migration.SQL); err != nil {
			t.Fatalf("apply legacy migration %s: %v", migration.Version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, migration.Version, migration.Name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatalf("record legacy migration %s: %v", migration.Version, err)
		}
		if migration.Version == through {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("legacy migration %s is unavailable", through)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit legacy migrations: %v", err)
	}
}

func seedLegacyProviderAuditMatrix(t *testing.T, ctx context.Context, db *sql.DB, hasRequirementBool bool) {
	t.Helper()
	const (
		snapshotAt = "2026-01-01T00:00:00.000000000Z"
		scopeAt    = "2026-01-01T00:01:00.000000000Z"
		rowAt      = "2026-01-01T00:02:00.000000000Z"
		configJSON = `{"team_id":"audit-team","version":1,"runtime_profiles":{"docker-policy":{"kind":"docker","command_policy":{"non_interactive_approval":true,"allow_coordlink_capabilities":["contract.current"]}},"external-plain":{"kind":"external"}},"agents":[{"id":"policy-agent","runtime_profile":"docker-policy","cli_backend":"claude","capabilities":["contract.current"]},{"id":"plain-agent","runtime_profile":"external-plain","cli_backend":"claude"},{"id":"fake-agent","runtime_profile":"external-plain","cli_backend":"fake"}]}`
	)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO team_config_versions (team_id, version, active, raw_yaml, config_json, created_at) VALUES ('audit-team', 1, 1, 'legacy', ?, ?)`, []any{configJSON, snapshotAt}},
		{`INSERT INTO events (id, tenant_id, event_type, aggregate_type, aggregate_id, payload_json, occurred_at) VALUES ('evt_team_snapshot', 'default', 'team_config.loaded', 'team_config', 'audit-team:1', ?, ?)`, []any{configJSON, snapshotAt}},
	} {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed TeamConfig snapshot: %v", err)
		}
	}
	type legacyCase struct {
		id, backend, agent, profile, runtimeKind string
		auditState, auditCode                    string
		active, scoped, explicitRequired         bool
	}
	cases := []legacyCase{
		{id: "legacy_complete", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "complete", scoped: true},
		{id: "legacy_failed", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "failed", auditCode: "PROVIDER_AUDIT_PARSE_FAILED", scoped: true},
		{id: "policy_incomplete", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "not_requested", scoped: true},
		{id: "policy_pending", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "not_requested", active: true, scoped: true},
		{id: "plain_claude", backend: "claude", agent: "plain-agent", profile: "external-plain", runtimeKind: "external", auditState: "not_requested", scoped: true},
		{id: "non_claude", backend: "fake", agent: "fake-agent", profile: "external-plain", runtimeKind: "external", auditState: "not_requested", scoped: true},
		{id: "unresolved", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "not_requested"},
	}
	if hasRequirementBool {
		cases = append(cases, legacyCase{id: "explicit_required", backend: "claude", agent: "policy-agent", profile: "docker-policy", runtimeKind: "docker", auditState: "not_requested", scoped: true, explicitRequired: true})
	}
	for _, item := range cases {
		contractID := "ctr_" + item.id
		assignmentID := "asg_" + item.id
		leaseID := "lease_" + item.id
		attemptID := "att_" + item.id
		runtimeID := "rt_" + item.id
		assignmentState, leaseState, attemptState, runtimeState, sessionState := "returned", "released", "completed", "stopped", "finished"
		endedAt := rowAt
		if item.active {
			assignmentState, leaseState, attemptState, runtimeState, sessionState = "claimed", "active", "running", "ready", "running"
			endedAt = ""
		}
		statements := []struct {
			query string
			args  []any
		}{
			{`INSERT INTO work_contracts (id, title, objective, target_kind, target_id, status, created_at, updated_at) VALUES (?, 'legacy audit', 'legacy audit', 'agent', ?, 'satisfied', ?, ?)`, []any{contractID, item.agent, rowAt, rowAt}},
			{`INSERT INTO assignments (id, contract_id, assignee_agent_id, state, reason, created_at, updated_at) VALUES (?, ?, ?, ?, 'legacy audit', ?, ?)`, []any{assignmentID, contractID, item.agent, assignmentState, rowAt, rowAt}},
			{`INSERT INTO leases (id, assignment_id, agent_id, runtime_id, state, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, '2027-01-01T00:00:00.000000000Z', ?, ?)`, []any{leaseID, assignmentID, item.agent, runtimeID, leaseState, rowAt, rowAt}},
			{`INSERT INTO attempts (id, lease_id, cli_backend, runtime_kind, start_reason, status, started_at, ended_at) VALUES (?, ?, ?, ?, 'legacy audit', ?, ?, NULLIF(?, ''))`, []any{attemptID, leaseID, item.backend, item.runtimeKind, attemptState, rowAt, endedAt}},
			{`INSERT INTO runtime_instances (id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id, lease_id, state, workspace_path, home_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '/workspace', '/home/agent', ?, ?)`, []any{"ri_" + item.id, runtimeID, item.profile, item.runtimeKind, item.agent, attemptID, leaseID, runtimeState, rowAt, rowAt}},
		}
		if item.scoped {
			statements = append(statements, struct {
				query string
				args  []any
			}{`INSERT INTO contract_team_scopes (contract_id, team_id, team_version, source, created_at) VALUES (?, 'audit-team', 1, 'legacy', ?)`, []any{contractID, scopeAt}})
		}
		for _, statement := range statements {
			if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
				t.Fatalf("seed legacy case %s: %v", item.id, err)
			}
		}
		if hasRequirementBool {
			if _, err := db.ExecContext(ctx, `
INSERT INTO cli_sessions (
  id, attempt_id, runtime_id, agent_id, cli_backend, profile_name, session_native_id,
  state, start_reason, provider_audit_state, provider_audit_error_code,
  provider_audit_required, started_at, ended_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'legacy audit', ?, ?, ?, ?, NULLIF(?, ''), ?)`,
				"cli_"+item.id, attemptID, runtimeID, item.agent, item.backend, item.backend,
				"native_"+item.id, sessionState, item.auditState, item.auditCode, item.explicitRequired,
				rowAt, endedAt, rowAt); err != nil {
				t.Fatalf("seed legacy 022 session %s: %v", item.id, err)
			}
		} else if _, err := db.ExecContext(ctx, `
INSERT INTO cli_sessions (
  id, attempt_id, runtime_id, agent_id, cli_backend, profile_name, session_native_id,
  state, start_reason, provider_audit_state, provider_audit_error_code,
  started_at, ended_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'legacy audit', ?, ?, ?, NULLIF(?, ''), ?)`,
			"cli_"+item.id, attemptID, runtimeID, item.agent, item.backend, item.backend,
			"native_"+item.id, sessionState, item.auditState, item.auditCode, rowAt, endedAt, rowAt); err != nil {
			t.Fatalf("seed legacy 021 session %s: %v", item.id, err)
		}
	}
}

func migrationDataCounts(t *testing.T, ctx context.Context, db *sql.DB) map[string]int {
	t.Helper()
	out := make(map[string]int)
	for _, table := range []string{"cli_sessions", "provider_tool_outcomes", "transcripts", "events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = count
	}
	return out
}
