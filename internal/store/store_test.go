package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"coordplane/internal/events"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestMigrateCreatesCanonicalTablesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if got, want := first.Applied, []string{
		"001_core_store_queue_events",
		"002_team_config_skill_registry",
		"003_session_lifecycle_guards",
		"004_object_store",
		"005_controlled_git_v1",
		"006_controlled_git_v2",
		"007_runtime_evidence",
		"008_cli_sessions",
		"009_command_runs",
		"010_runtime_tokens",
		"011_validation_assessments",
		"012_release_acceptances",
		"013_contract_team_scopes",
		"014_agent_communication_envelopes",
	}; !equalStrings(got, want) {
		t.Fatalf("applied migrations = %v, want %v", got, want)
	}

	second, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Fatalf("second migrate applied %v, want none", second.Applied)
	}

	for _, table := range []string{
		"agents",
		"work_contracts",
		"assignments",
		"leases",
		"attempts",
		"session_routes",
		"threads",
		"messages",
		"agent_communication_envelopes",
		"mailbox_items",
		"evidence",
		"delivery_attempts",
		"capability_calls",
		"artifacts",
		"transcripts",
		"queue_items",
		"events",
		"team_config_versions",
		"team_config_agents",
		"skill_packages",
		"prepare_leases",
		"active_guards",
		"object_blobs",
		"git_repositories",
		"git_workspaces",
		"git_operations",
		"git_locks",
		"changesets",
		"git_merge_attempts",
		"git_conflict_sets",
		"git_rollback_points",
		"runtime_instances",
		"cli_sessions",
		"command_runs",
		"runtime_tokens",
		"validation_assessments",
		"release_acceptances",
		"contract_team_scopes",
	} {
		var name string
		err := s.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing after migration: %v", table, err)
		}
	}
	for _, column := range []string{"envelope_id", "trigger_turn"} {
		if !columnExists(t, ctx, s.DB(), "mailbox_items", column) {
			t.Fatalf("mailbox_items.%s missing after migration", column)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func columnExists(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return false
}

func TestAppendEventPersistsTraceableFact(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	occurredAt := time.Date(2026, 7, 4, 3, 0, 0, 0, time.UTC)
	id, err := s.AppendEvent(ctx, events.Event{
		ID:             "evt_test",
		TenantID:       "default",
		TraceID:        "trace_123",
		SubjectKind:    "agent",
		SubjectID:      "agent_builder",
		AgentID:        "agent_builder",
		RuntimeID:      "runtime_external",
		CapabilityName: "contract.add",
		Type:           "contract.created",
		AggregateType:  "work_contract",
		AggregateID:    "ctr_123",
		PayloadJSON:    []byte(`{"title":"build protocol kernel"}`),
		OccurredAt:     occurredAt,
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if id != "evt_test" {
		t.Fatalf("event id = %q, want evt_test", id)
	}

	got, err := s.ListEvents(ctx, store.EventFilter{TraceID: "trace_123"})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events length = %d, want 1", len(got))
	}
	event := got[0]
	if event.Type != "contract.created" || event.AggregateID != "ctr_123" {
		t.Fatalf("event = %+v, want persisted contract.created fact", event)
	}
	if string(event.PayloadJSON) != `{"title":"build protocol kernel"}` {
		t.Fatalf("payload = %s", event.PayloadJSON)
	}
	if !event.OccurredAt.Equal(occurredAt) {
		t.Fatalf("occurred_at = %s, want %s", event.OccurredAt, occurredAt)
	}
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	return store.New(db)
}
