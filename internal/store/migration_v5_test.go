package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestCT05V4ToV5TaskBudgetSecondsMigration upgrades a genuine v4 database
// (tasks table rebuilt by the human task lifecycle migration, so it has no
// budget_seconds column) and asserts the additive ALTER lands: existing tasks
// default to 0, the CHECK rejects negative budgets, and a second Migrate is a
// no-op.
func TestCT05V4ToV5TaskBudgetSecondsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	requireNoError(t, err)
	for _, statement := range splitStatements(schemaSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	execTestSQL(t, ctx, db, isolationSpecMigrationSQL)
	for _, statement := range splitStatements(participantRolesMigrationSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	// The human task lifecycle rebuild drops the v1 tasks table; it must run
	// before any row references a task so the implicit DELETE on DROP stays
	// free of foreign key violations.
	for _, statement := range splitStatements(humanTaskLifecycleMigrationSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	now := "2026-08-06T00:00:00Z"
	for _, row := range []string{
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'coordplane_v1_six_objects',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(2,'coordplane_v2_run_isolation_spec',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(3,'coordplane_v3_participant_roles',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(4,'coordplane_v4_human_task_lifecycle',?)`,
		`INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt-1','Agent One','one-shot','img','/i','active',1,?,?)`,
		`INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,integration_agent_id,status,version,created_at,updated_at) VALUES('prj-1','project one','/source','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/control','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','','active',1,?,?)`,
		`INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk-1','prj-1','work','boss','agt-1','Implement','Do it','queued',?,1,?,?)`,
	} {
		execTestSQL(t, ctx, db, row, now, now, now, now, now)
	}
	requireNoError(t, db.Close())

	store, err := Open(ctx, path)
	requireNoError(t, err)
	defer store.Close()
	version, err := store.migrationVersion(ctx)
	requireNoError(t, err)
	if version != schemaVersion {
		t.Fatalf("schema version after upgrade = %d, want %d", version, schemaVersion)
	}
	var budget int64
	if err := store.db.QueryRowContext(ctx, `SELECT budget_seconds FROM tasks WHERE id='tsk-1'`).Scan(&budget); err != nil {
		t.Fatal(err)
	}
	if budget != 0 {
		t.Fatalf("existing task budget_seconds = %d, want default 0", budget)
	}
	insert := `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,budget_seconds,next_run_at,version,created_at,updated_at)
		VALUES('tsk-budget','prj-1','work','boss','agt-1','Budgeted','Self-declared cap', 'queued',300,?,1,?,?)`
	execTestSQL(t, ctx, store.db, insert, now, now, now)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,budget_seconds,next_run_at,version,created_at,updated_at)
		VALUES('tsk-negative','prj-1','work','boss','agt-1','Negative','Must be rejected', 'queued',-1,?,1,?,?)`, now, now, now); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("negative budget_seconds insert error = %v, want CHECK constraint", err)
	}
	result, err := store.Migrate(ctx)
	requireNoError(t, err)
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
}
