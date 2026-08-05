package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestCT04V3ToV4HumanTaskLifecycleMigration upgrades a genuine v3 database:
// task/message participant references backfill, agents gain participant rows,
// foreign keys stay clean, and a human task (assignee_agent_id=”) inserts
// once the agent FK is gone. A second Migrate is a no-op.
func TestCT04V3ToV4HumanTaskLifecycleMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	requireNoError(t, err)
	for _, statement := range splitStatements(schemaSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	execTestSQL(t, ctx, db, isolationSpecMigrationSQL)
	for _, statement := range splitStatements(participantRolesMigrationSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	now := "2026-08-03T00:00:00Z"
	for _, row := range []string{
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'coordplane_v1_six_objects',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(2,'coordplane_v2_run_isolation_spec',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(3,'coordplane_v3_participant_roles',?)`,
		`INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt-1','Agent One','one-shot','img','/i','active',1,?,?)`,
		`INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,integration_agent_id,status,version,created_at,updated_at) VALUES('prj-1','project one','/source','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/control','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','','active',1,?,?)`,
		`INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk-1','prj-1','work','boss','agt-1','Implement','Do it','queued',?,1,?,?)`,
		`INSERT INTO messages(id,project_id,task_id,sender_kind,sender_id,recipient_kind,recipient_id,body,wake,state,next_delivery_at,idempotency_key,version,created_at) VALUES('msg-boss','prj-1','tsk-1','agent','agt-1','boss','','progress',0,'pending',?,'k1',1,?)`,
		`INSERT INTO messages(id,project_id,task_id,sender_kind,sender_id,recipient_kind,recipient_id,body,wake,state,next_delivery_at,idempotency_key,version,created_at) VALUES('msg-agent','prj-1','tsk-1','boss','','agent','agt-1','work',1,'pending',?,'k2',1,?)`,
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
	var assigneeParticipant, evidence string
	if err := store.db.QueryRowContext(ctx, `SELECT assignee_participant_id, evidence_type FROM tasks WHERE id='tsk-1'`).Scan(&assigneeParticipant, &evidence); err != nil {
		t.Fatal(err)
	}
	if assigneeParticipant != "agt-1" || evidence != "" {
		t.Fatalf("task backfill = %q/%q", assigneeParticipant, evidence)
	}
	var bossRecipient, agentRecipient, kind string
	if err := store.db.QueryRowContext(ctx, `SELECT recipient_participant_id FROM messages WHERE id='msg-boss'`).Scan(&bossRecipient); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT recipient_participant_id FROM messages WHERE id='msg-agent'`).Scan(&agentRecipient); err != nil {
		t.Fatal(err)
	}
	if bossRecipient != "participant-owner" || agentRecipient != "agt-1" {
		t.Fatalf("message backfill = %q/%q", bossRecipient, agentRecipient)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT kind FROM participants WHERE id='agt-1'`).Scan(&kind); err != nil || kind != "cli_agent" {
		t.Fatalf("agent participant row = kind %q err %v", kind, err)
	}
	var taskSQL string
	if err := store.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&taskSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(taskSQL, "REFERENCES agents") {
		t.Fatalf("tasks table still references agents: %s", taskSQL)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_participant_id,title,description,status,next_run_at,version,created_at,updated_at)
		VALUES('tsk-human','prj-1','work','boss','participant-owner','Human','Review','queued',?,1,?,?)`, now, now, now); err != nil {
		t.Fatalf("human-assigned task insert failed: %v", err)
	}
	result, err := store.Migrate(ctx)
	requireNoError(t, err)
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
}
