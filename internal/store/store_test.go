package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"coordplane/internal/core"

	_ "modernc.org/sqlite"
)

func TestCT01FileMigrationIsExactAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "coordplane.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.SchemaInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantTables := []string{"agents", "events", "messages", "projects", "request_dedupes", "runs", "schema_migrations", "tasks"}
	if !reflect.DeepEqual(info.Tables, wantTables) {
		t.Fatalf("tables = %v, want %v", info.Tables, wantTables)
	}
	if info.JournalMode != "wal" || !info.ForeignKeys || info.BusyTimeout != busyTimeoutMillis {
		t.Fatalf("SQLite pragmas = %#v", info)
	}
	result, err := store.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	info, err = reopened.SchemaInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(info.Tables, wantTables) {
		t.Fatalf("reopened tables = %v", info.Tables)
	}
}

func TestCT01V1RunMigrationPreservesLegacyIsolationSpec(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "coordplane-v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range splitStatements(schemaSQL) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	const now = "2026-07-12T00:00:00Z"
	statements := []string{
		`INSERT INTO schema_migrations VALUES(1,'coordplane_v1_six_objects','2026-07-12T00:00:00Z')`,
		`INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,status,version,created_at,updated_at) VALUES('prj_v1','v1','/source','main','abc','/control','refs/heads/main','abc','active',1,'` + now + `','` + now + `')`,
		`INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt_v1','v1','codex','image','/instructions','active',1,'` + now + `','` + now + `')`,
		`INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,current_run_id,generation,next_run_at,version,created_at,updated_at) VALUES('tsk_v1','prj_v1','work','boss','agt_v1','v1','','running','run_v1',1,'` + now + `',1,'` + now + `','` + now + `')`,
		`INSERT INTO runs(id,project_id,task_id,agent_id,generation,adapter_id,image,state,token_hash,cleanup_state,container_name,launch_mode,version,created_at) VALUES('run_v1','prj_v1','tsk_v1','agt_v1',1,'codex','image','active','token-v1','not_needed','coordplane-run-v1','start',1,'` + now + `')`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.Run(ctx, "run_v1")
	if err != nil {
		t.Fatal(err)
	}
	if run.IsolationSpecVersion != core.RunIsolationSpecV1 {
		t.Fatalf("migrated isolation spec = %d, want legacy v1", run.IsolationSpecVersion)
	}
}

func TestCT01SQLitePragmasSurviveConnectionReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "connection-churn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.db.SetConnMaxLifetime(time.Nanosecond)

	for range 3 {
		time.Sleep(time.Millisecond)
		info, err := store.SchemaInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.JournalMode != "wal" || !info.ForeignKeys || info.BusyTimeout != busyTimeoutMillis {
			t.Fatalf("replacement connection SQLite pragmas = %#v", info)
		}
	}
	if store.db.Stats().MaxLifetimeClosed == 0 {
		t.Fatal("test did not replace a SQLite connection")
	}
}

func TestCT01LegacyDatabaseFailsClosedWithoutSchemaRewrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE work_contracts(id TEXT PRIMARY KEY, status TEXT); INSERT INTO work_contracts VALUES('old','active')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, path)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("legacy database unexpectedly opened")
	}
	if !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
		t.Fatalf("error = %v", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM work_contracts WHERE id='old'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("legacy row changed: status=%q err=%v", status, err)
	}
	var newTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&newTables); err != nil {
		t.Fatal(err)
	}
	if newTables != 0 {
		t.Fatal("new schema was written beside legacy schema")
	}
}

func TestCT01PartialOrCorruptDatabaseNeverBecomesReady(t *testing.T) {
	t.Run("partial allowed table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE agents(id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		store, err := Open(context.Background(), path)
		if store != nil {
			_ = store.Close()
			t.Fatal("partial database unexpectedly opened")
		}
		if err == nil {
			t.Fatal("partial database returned no error")
		}
	})
	t.Run("corrupt bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.db")
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := Open(context.Background(), path)
		if store != nil {
			_ = store.Close()
			t.Fatal("corrupt database unexpectedly opened")
		}
		if err == nil {
			t.Fatal("corrupt database returned no error")
		}
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
				if _, err := db.Exec(`DROP INDEX runs_one_live_per_agent`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "allowed table structurally changed",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`ALTER TABLE events RENAME COLUMN payload_json TO payload_text`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hidden migration history",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(0,'legacy','2026-07-12T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "foreign key violation",
			mutate: func(t *testing.T, db *sql.DB) {
				const now = "2026-07-12T00:00:00Z"
				if _, err := db.Exec(`INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt_orphan','Orphan','one-shot','image','/instructions','active',1,?,?)`, now, now); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk_orphan','prj_missing','work','boss','agt_orphan','Orphan','Orphan','queued',?,1,?,?)`, now, now, now); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "drift.db")
			opened, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Open(context.Background(), path)
			if reopened != nil {
				_ = reopened.Close()
				t.Fatal("structurally drifted database unexpectedly opened")
			}
			if !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
				t.Fatalf("Open() error = %v, want %s", err, core.CodeLegacySchemaRebuildRequired)
			}
		})
	}
}

func TestMutationAndEventRollbackTogether(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := core.Agent{
		ID: "agt_atomic", DisplayName: "Atomic", AdapterID: "one-shot", Image: "image",
		InstructionsFile: "/instructions", Status: core.AgentActive, Version: 1,
		CreatedAt: "2026-07-12T00:00:00Z", UpdatedAt: "2026-07-12T00:00:00Z",
	}
	err = store.Transact(ctx, func(tx core.Transaction) error {
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
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Agents) != 0 {
		t.Fatalf("business row survived event failure: %#v", snapshot.Agents)
	}
	events, err := store.Events(ctx, core.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events survived rollback: %#v", events)
	}
}

func TestEventsLimitReturnsTheRecentTailInStableOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Transact(ctx, func(tx core.Transaction) error {
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
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, core.EventFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "second" || events[1].Kind != "third" {
		t.Fatalf("event tail = %#v", events)
	}
}
