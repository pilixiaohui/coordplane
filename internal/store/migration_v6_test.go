package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"coordplane/internal/core"
)

// TestCT06V5ToV6ProjectDeleteCapabilityMigration upgrades a genuine v5 database
// whose seeded owner role predates the project.delete capability and asserts
// the data-only migration rewrites role-owner back to the canonical capability
// set from the core registry: project.delete is granted, and a second Migrate
// is a no-op.
func TestCT06V5ToV6ProjectDeleteCapabilityMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	requireNoError(t, err)
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
	now := "2026-08-13T00:00:00Z"
	for _, row := range []string{
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'coordplane_v1_six_objects',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(2,'coordplane_v2_run_isolation_spec',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(3,'coordplane_v3_participant_roles',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(4,'coordplane_v4_human_task_lifecycle',?)`,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(5,'coordplane_v5_task_budget_seconds',?)`,
		`INSERT INTO participants(id,kind,display_name,status,version,created_at,updated_at) VALUES('participant-owner','human','Owner','active',1,?,?)`,
	} {
		execTestSQL(t, ctx, db, row, now, now)
	}
	// Seed the owner role the way migration v3 did, but from a registry that
	// predates project.delete, so the live database is missing the capability.
	staleCapabilities := core.CapabilityNames(core.AllCapabilities())
	staleCapabilities = removeString(staleCapabilities, string(core.CapabilityProjectDelete))
	staleJSON, err := json.Marshal(staleCapabilities)
	requireNoError(t, err)
	execTestSQL(t, ctx, db,
		`INSERT INTO roles(id,name,description,capabilities,version,created_at,updated_at)
		VALUES('role-owner','owner','default owner with every capability',?,1,?,?)`,
		string(staleJSON), now, now)
	requireNoError(t, db.Close())

	store, err := Open(ctx, path)
	requireNoError(t, err)
	defer store.Close()
	version, err := store.migrationVersion(ctx)
	requireNoError(t, err)
	if version != schemaVersion {
		t.Fatalf("schema version after upgrade = %d, want %d", version, schemaVersion)
	}
	var capabilitiesJSON string
	if err := store.db.QueryRowContext(ctx, `SELECT capabilities FROM roles WHERE id='role-owner'`).Scan(&capabilitiesJSON); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal([]byte(capabilitiesJSON), &names); err != nil {
		t.Fatalf("role-owner capabilities are not valid JSON: %v", err)
	}
	capabilities, err := core.ParseCapabilities(names)
	requireNoError(t, err)
	if !core.HasCapability(capabilities, core.CapabilityProjectDelete) {
		t.Fatalf("owner role is missing project.delete after migration: %s", capabilitiesJSON)
	}
	if len(capabilities) != len(core.AllCapabilities()) {
		t.Fatalf("owner role has %d capabilities, want %d: %s", len(capabilities), len(core.AllCapabilities()), capabilitiesJSON)
	}
	result, err := store.Migrate(ctx)
	requireNoError(t, err)
	if len(result.Applied) != 0 {
		t.Fatalf("second migration applied %v", result.Applied)
	}
}

func removeString(values []string, want string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value != want {
			kept = append(kept, value)
		}
	}
	sort.Strings(kept)
	return kept
}
