package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"

	_ "modernc.org/sqlite"
)

var requireNoError = testsupport.RequireNoError

func openTestStore(t *testing.T, ctx context.Context, name string) *Store {
	t.Helper()
	database, err := Open(ctx, filepath.Join(t.TempDir(), name))
	requireNoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func requireOpenError(t *testing.T, path, description string) error {
	t.Helper()
	opened, err := Open(context.Background(), path)
	if opened != nil {
		_ = opened.Close()
		t.Fatalf("%s unexpectedly opened", description)
	}
	if err == nil {
		t.Fatalf("%s returned no error", description)
	}
	return err
}

func execTestSQL(t *testing.T, ctx context.Context, db *sql.DB, statement string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(ctx, statement, args...)
	requireNoError(t, err)
}

func TestCT01FileMigrationIsExactAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, "coordplane.db")
	info, err := store.SchemaInfo(ctx)
	requireNoError(t, err)
	if info.JournalMode != "wal" || !info.ForeignKeys || info.BusyTimeout != busyTimeoutMillis {
		t.Fatalf("SQLite pragmas = %#v", info)
	}
	result, err := store.Migrate(ctx)
	requireNoError(t, err)
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
	store.db.SetConnMaxLifetime(time.Nanosecond)
	for range 3 {
		time.Sleep(time.Millisecond)
		info, err = store.SchemaInfo(ctx)
		requireNoError(t, err)
		if info.JournalMode != "wal" || !info.ForeignKeys || info.BusyTimeout != busyTimeoutMillis {
			t.Fatalf("replacement connection pragmas = %#v", info)
		}
	}
	if store.db.Stats().MaxLifetimeClosed == 0 {
		t.Fatal("test did not replace a SQLite connection")
	}
}

func TestCT01LegacyDatabaseFailsClosedWithoutSchemaRewrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite", path)
	requireNoError(t, err)
	execTestSQL(t, ctx, db, `CREATE TABLE work_contracts(id TEXT PRIMARY KEY, status TEXT); INSERT INTO work_contracts VALUES('old','active')`)
	requireNoError(t, db.Close())
	err = requireOpenError(t, path, "legacy database")
	if !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
		t.Fatalf("Open() error = %v, want %s", err, core.CodeLegacySchemaRebuildRequired)
	}
	after, err := os.ReadDir(dir)
	requireNoError(t, err)
	if len(after) != 1 || after[0].Name() != filepath.Base(path) {
		t.Fatalf("legacy database gained side files: %v", after)
	}
	db, err = sql.Open("sqlite", path)
	requireNoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var status string
	requireNoError(t, db.QueryRow(`SELECT status FROM work_contracts WHERE id='old'`).Scan(&status))
	if status != "active" {
		t.Fatalf("legacy row status = %q, want active", status)
	}
	var projects int
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&projects))
	if projects != 0 {
		t.Fatal("new projects table was written beside legacy schema")
	}
}

func TestCT01PartialOrCorruptDatabaseNeverBecomesReady(t *testing.T) {
	t.Run("partial allowed table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.db")
		db, err := sql.Open("sqlite", path)
		requireNoError(t, err)
		execTestSQL(t, context.Background(), db, `CREATE TABLE agents(id TEXT PRIMARY KEY)`)
		_ = db.Close()
		requireOpenError(t, path, "partial database")
	})
	t.Run("corrupt bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.db")
		requireNoError(t, os.WriteFile(path, []byte("not a sqlite database"), 0o600))
		requireOpenError(t, path, "corrupt database")
	})
}

func TestCT01ExistingSchemaDriftFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB)
	}{
		{
			name: "critical index dropped",
			mutate: func(t *testing.T, db *sql.DB) {
				execTestSQL(t, context.Background(), db, `DROP INDEX runs_one_live_per_agent`)
			},
		},
		{
			name: "allowed table structurally changed",
			mutate: func(t *testing.T, db *sql.DB) {
				execTestSQL(t, context.Background(), db, `ALTER TABLE events RENAME COLUMN payload_json TO payload_text`)
			},
		},
		{
			name: "hidden migration history",
			mutate: func(t *testing.T, db *sql.DB) {
				execTestSQL(t, context.Background(), db, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(0,'legacy','2026-07-12T00:00:00Z')`)
			},
		},
		{
			name: "foreign key violation",
			mutate: func(t *testing.T, db *sql.DB) {
				const now = "2026-07-12T00:00:00Z"
				execTestSQL(t, context.Background(), db, `INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt_orphan','Orphan','one-shot','image','/instructions','active',1,?,?)`, now, now)
				execTestSQL(t, context.Background(), db, `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk_orphan','prj_missing','work','boss','agt_orphan','Orphan','Orphan','queued',?,1,?,?)`, now, now, now)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "drift.db")
			opened, err := Open(context.Background(), path)
			requireNoError(t, err)
			requireNoError(t, opened.Close())

			db, err := sql.Open("sqlite", path)
			requireNoError(t, err)
			test.mutate(t, db)
			requireNoError(t, db.Close())

			err = requireOpenError(t, path, "structurally drifted database")
			if !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
				t.Fatalf("Open() error = %v, want %s", err, core.CodeLegacySchemaRebuildRequired)
			}
		})
	}
}

func TestMutationAndEventRollbackTogether(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, "atomic.db")
	agent := core.Agent{
		ID: "agt_atomic", DisplayName: "Atomic", AdapterID: "one-shot", Image: "image",
		InstructionsFile: "/instructions", Status: core.AgentActive, Version: 1,
		CreatedAt: "2026-07-12T00:00:00Z", UpdatedAt: "2026-07-12T00:00:00Z",
	}
	err := store.Transact(ctx, func(tx core.Transaction) error {
		if err := tx.InsertAgent(agent); err != nil {
			return err
		}
		_, err := tx.AppendEvent(core.Event{
			EntityType: "legacy-object", EntityID: agent.ID, Kind: "agent.created",
			ActorKind: "boss", PayloadJSON: "{}", CreatedAt: agent.CreatedAt,
		})
		return err
	})
	if err == nil {
		t.Fatal("invalid event unexpectedly committed")
	}
	snapshot, err := store.Snapshot(ctx, "")
	requireNoError(t, err)
	if len(snapshot.Agents) != 0 {
		t.Fatalf("business row survived event failure: %#v", snapshot.Agents)
	}
	events, err := store.Events(ctx, core.EventFilter{})
	requireNoError(t, err)
	if len(events) != 0 {
		t.Fatalf("events survived rollback: %#v", events)
	}
}

func TestEventsLimitReturnsTheRecentTailInStableOrder(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx, "events.db")
	err := store.Transact(ctx, func(tx core.Transaction) error {
		for _, kind := range []string{"first", "second", "third"} {
			if _, err := tx.AppendEvent(core.Event{
				EntityType: "daemon", EntityID: "daemon", Kind: kind,
				ActorKind: "daemon", PayloadJSON: "{}", CreatedAt: "2026-07-12T00:00:00Z",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	requireNoError(t, err)
	events, err := store.Events(ctx, core.EventFilter{Limit: 2})
	requireNoError(t, err)
	if len(events) != 2 || events[0].Kind != "second" || events[1].Kind != "third" {
		t.Fatalf("event tail = %#v", events)
	}
}
