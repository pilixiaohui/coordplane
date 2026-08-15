package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
)

// TestCT07V6ToV7AgentRuntimeConfigMigration upgrades a genuine v6 database
// with an existing CLI-agent mirror and terminal legacy Runs. It asserts the
// migration is additive and derived-only: the five agent/participant columns
// land with defaults, the CLI-agent participant is mirrored from agents,
// human rows stay empty, terminal Runs with a known instructions hash receive
// the canonical fingerprint, and starting or hash-less Runs stay
// non-resumable with an empty fingerprint.
func TestCT07V6ToV7AgentRuntimeConfigMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6.db")
	const now = "2026-08-13T00:00:00Z"
	const instructionsHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	db := buildV6TestDatabase(t, path, now)
	execTestSQL(t, ctx, db, `INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt-1','Agent One','claude','agent:latest','/instructions','active',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO participants(id,kind,display_name,status,credential_id,adapter_id,image,instructions_file,version,created_at,updated_at) VALUES('agt-1','cli_agent','Agent One','active','','claude','agent:latest','/instructions',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO participants(id,kind,display_name,status,credential_id,version,created_at,updated_at) VALUES('human-1','human','Reviewer','active','credential-1',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,status,version,created_at,updated_at) VALUES('prj-1','Project One','/source','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/control','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','active',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk-1','prj-1','work','boss','agt-1','Implement','Do it','queued',?,1,?,?)`, now, now, now)
	for _, run := range []struct{ id, state, hash string }{
		{id: "run-terminal", state: "exited", hash: instructionsHash},
		{id: "run-starting", state: "starting", hash: instructionsHash},
		{id: "run-empty-hash", state: "exited"},
	} {
		execTestSQL(t, ctx, db, `INSERT INTO runs(id,project_id,task_id,agent_id,generation,adapter_id,image,instructions_hash,state,token_hash,cleanup_state,container_name,launch_mode,version,created_at) VALUES(?,?,?,?,1,'claude','agent:latest',?,?,'token-'||?,?,?,'start',1,?)`, run.id, "prj-1", "tsk-1", "agt-1", run.hash, run.state, run.id, "not_needed", "coordplane-"+run.id, now)
	}
	var eventCount int
	requireNoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&eventCount))
	requireNoError(t, db.Close())

	store, err := Open(ctx, path)
	requireNoError(t, err)
	defer store.Close()
	version, err := store.migrationVersion(ctx)
	requireNoError(t, err)
	if version != schemaVersion {
		t.Fatalf("schema version after upgrade = %d, want %d", version, schemaVersion)
	}
	assertColumnCounts(t, store.db, map[string]int{"agents": 5, "participants": 5, "runs": 1})
	for _, id := range []string{"agt-1", "human-1"} {
		var model, subagent, baseURL, effort, instructions string
		requireNoError(t, store.db.QueryRowContext(ctx, `SELECT model,subagent_model,base_url,effort,instructions_text FROM participants WHERE id=?`, id).Scan(&model, &subagent, &baseURL, &effort, &instructions))
		if model != "" || subagent != "" || baseURL != "" || effort != "" || instructions != "" {
			t.Fatalf("participant %s new fields = %q/%q/%q/%q/%q, want all empty", id, model, subagent, baseURL, effort, instructions)
		}
	}
	expected, err := core.RuntimeConfigFingerprint(core.RuntimeConfigFingerprintInput{
		AdapterID: "claude", Image: "agent:latest", InstructionsHash: instructionsHash,
	})
	requireNoError(t, err)
	for _, test := range []struct{ id, want string }{
		{id: "run-terminal", want: expected},
		{id: "run-starting"},
		{id: "run-empty-hash"},
	} {
		var fingerprint string
		requireNoError(t, store.db.QueryRowContext(ctx, `SELECT config_fingerprint FROM runs WHERE id=?`, test.id).Scan(&fingerprint))
		if fingerprint != test.want {
			t.Fatalf("run %s config_fingerprint = %q, want %q", test.id, fingerprint, test.want)
		}
	}
	var afterEventCount int
	requireNoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&afterEventCount))
	if afterEventCount != eventCount {
		t.Fatalf("v7 migration created events: before=%d after=%d", eventCount, afterEventCount)
	}
	result, err := store.Migrate(ctx)
	requireNoError(t, err)
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
}

// TestCT07LegalV6SchemaWithCodexStateFailsClosed pins the E3 contract: a
// genuine v6 schema containing a pre-v7 Codex row is unprovable legacy state
// and must be rejected before any migration or ready transition.
func TestCT07LegalV6SchemaWithCodexStateFailsClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6-codex.db")
	const now = "2026-08-13T00:00:00Z"
	db := buildV6TestDatabase(t, path, now)
	execTestSQL(t, ctx, db, `INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt-legacy-codex','Legacy Codex','codex','agent:latest','/instructions','active',1,?,?)`, now, now)
	requireNoError(t, db.Close())

	if opened, err := Open(ctx, path); !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("legal v6 schema with Codex state error = %v, want %s", err, core.CodeLegacySchemaRebuildRequired)
	}
}

// TestCT07V6ToV7MigrationRollsBackAtomically forces the derived Run backfill
// to abort after the DDL has run and proves the whole transaction is undone:
// no v7 column or migration record is observable after reopening the file.
func TestCT07V6ToV7MigrationRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6-rollback.db")
	const now = "2026-08-13T00:00:00Z"
	db := buildV6TestDatabase(t, path, now)
	execTestSQL(t, ctx, db, `INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES('agt-1','Agent One','claude','agent:latest','/instructions','active',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO participants(id,kind,display_name,status,credential_id,adapter_id,image,instructions_file,version,created_at,updated_at) VALUES('agt-1','cli_agent','Agent One','active','','claude','agent:latest','/instructions',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,status,version,created_at,updated_at) VALUES('prj-1','Project One','/source','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/control','refs/heads/main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','active',1,?,?)`, now, now)
	execTestSQL(t, ctx, db, `INSERT INTO tasks(id,project_id,kind,created_by_kind,assignee_agent_id,title,description,status,next_run_at,version,created_at,updated_at) VALUES('tsk-1','prj-1','work','boss','agt-1','Implement','Do it','queued',?,1,?,?)`, now, now, now)
	// A terminal run with an incomplete legacy adapter/image identity makes
	// the canonical fingerprint derivation fail after the v7 DDL has run. The
	// invalid row is legitimate v6 data (the old schema has no non-empty CHECK
	// on those columns), so schema validation passes and the migration must
	// roll the entire transaction back.
	execTestSQL(t, ctx, db, `INSERT INTO runs(id,project_id,task_id,agent_id,generation,adapter_id,image,instructions_hash,state,token_hash,cleanup_state,container_name,launch_mode,version,created_at) VALUES('run-1','prj-1','tsk-1','agt-1',1,'','','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','exited','token','not_needed','coordplane-run-1','start',1,?)`, now)
	requireNoError(t, db.Close())

	if opened, openErr := Open(ctx, path); openErr == nil {
		_ = opened.Close()
		t.Fatal("v7 migration unexpectedly committed despite forced backfill failure")
	}

	db, err := sql.Open("sqlite", path)
	requireNoError(t, err)
	defer db.Close()
	var version int
	requireNoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version))
	if version != 6 {
		t.Fatalf("schema version after rollback = %d, want 6", version)
	}
	for table, column := range map[string]string{
		"agents":       "model",
		"participants": "instructions_text",
		"runs":         "config_fingerprint",
	} {
		var count int
		requireNoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count))
		if count != 0 {
			t.Fatalf("%s.%s survived the rolled-back migration", table, column)
		}
	}
}

func buildV6TestDatabase(t *testing.T, path, now string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	requireNoError(t, err)
	ctx := context.Background()
	for _, statement := range splitStatements(schemaSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	execTestSQL(t, ctx, db, isolationSpecMigrationSQL)
	for _, statement := range splitStatements(participantRolesMigrationSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	for _, statement := range splitStatements(humanTaskLifecycleMigrationSQL) {
		execTestSQL(t, ctx, db, statement)
	}
	execTestSQL(t, ctx, db, budgetSecondsMigrationSQL)
	for _, migration := range []struct {
		version int
		name    string
	}{
		{1, initialSchemaName},
		{2, isolationSpecMigrationName},
		{3, participantRolesMigrationName},
		{4, schemaName},
		{5, budgetSecondsMigrationName},
		{6, projectDeleteCapabilityMigrationName},
	} {
		execTestSQL(t, ctx, db, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, migration.version, migration.name, now)
	}
	ownerCapabilities, err := json.Marshal(core.CapabilityNames(core.AllCapabilities()))
	requireNoError(t, err)
	agentCapabilities, err := json.Marshal(core.CapabilityNames(core.AgentDefaultCapabilities()))
	requireNoError(t, err)
	execTestSQL(t, ctx, db, `INSERT INTO participants(id,kind,display_name,status,version,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, core.DefaultHumanParticipantID, string(core.ParticipantKindHuman), "Owner", "active", now, now)
	execTestSQL(t, ctx, db, `INSERT INTO roles(id,name,description,capabilities,version,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, core.DefaultOwnerRoleID, "owner", "default owner with every capability", string(ownerCapabilities), now, now)
	execTestSQL(t, ctx, db, `INSERT INTO roles(id,name,description,capabilities,version,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, core.DefaultAgentRoleID, "agent", "default CLI agent role", string(agentCapabilities), now, now)
	execTestSQL(t, ctx, db, `INSERT INTO participant_project_role(participant_id,project_id,role_id,version,created_at,updated_at) VALUES(?,?,?,1,?,?)`, core.DefaultHumanParticipantID, core.GlobalProjectID, core.DefaultOwnerRoleID, now, now)
	return db
}

func assertColumnCounts(t *testing.T, db *sql.DB, want map[string]int) {
	t.Helper()
	ctx := context.Background()
	for table, wantCount := range want {
		var count int
		requireNoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name IN ('model','subagent_model','base_url','effort','instructions_text','config_fingerprint')`, table).Scan(&count))
		if count != wantCount {
			t.Fatalf("%s new configuration columns = %d, want %d", table, count, wantCount)
		}
	}
}
