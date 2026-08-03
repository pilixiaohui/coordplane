package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"coordplane/internal/core"

	_ "modernc.org/sqlite"
)

const busyTimeoutMillis = 5000

var allowedTables = map[string]bool{
	"projects": true, "agents": true, "tasks": true, "runs": true,
	"messages": true, "events": true, "schema_migrations": true,
	"request_dedupes": true, "participants": true, "roles": true,
	"participant_project_role": true, "credentials": true,
}

type Store struct {
	db   *sql.DB
	path string
}

type MigrationResult struct {
	Applied []int `json:"applied"`
}

type SchemaInfo struct {
	Tables      []string `json:"tables"`
	JournalMode string   `json:"journal_mode"`
	ForeignKeys bool     `json:"foreign_keys"`
	BusyTimeout int      `json:"busy_timeout_ms"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.Contains(path, "mode=memory") {
		return nil, core.NewError(core.CodeInvalidArgument, "store requires a file-backed SQLite path", false)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, core.WrapError(core.CodeInvalidArgument, "resolve SQLite path", false, err)
	}
	if err := preflightLegacyAdapterState(ctx, absolute); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := u.Query()
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	query.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, core.WrapError(core.CodeInternal, "open SQLite", false, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: absolute}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, core.WrapError(core.CodeInternal, "open SQLite", false, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// PreflightLegacyAdapterState rejects retired provider state without opening
// the database for writes or running migrations.
func PreflightLegacyAdapterState(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.Contains(path, "mode=memory") {
		return core.NewError(core.CodeInvalidArgument, "store requires a file-backed SQLite path", false)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return core.WrapError(core.CodeInvalidArgument, "resolve SQLite path", false, err)
	}
	return preflightLegacyAdapterState(ctx, absolute)
}

func preflightLegacyAdapterState(ctx context.Context, path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return core.WrapError(core.CodeInternal, "inspect SQLite before startup", false, err)
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return core.WrapError(core.CodeInternal, "open SQLite startup preflight", false, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('agents','runs')`).Scan(&tables); err != nil {
		return core.WrapError(core.CodeInternal, "inspect SQLite startup schema", false, err)
	}
	if tables != 2 {
		return nil
	}
	var legacy int
	if err := db.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM agents WHERE adapter_id='codex') OR EXISTS(SELECT 1 FROM runs WHERE adapter_id='codex') THEN 1 ELSE 0 END`).Scan(&legacy); err != nil {
		return core.WrapError(core.CodeInternal, "inspect retired adapter state", false, err)
	}
	if legacy != 0 {
		return core.NewError(core.CodeLegacySchemaRebuildRequired, "retired Codex durable state requires backup and a fresh data_dir; stop live resources with the old environment", false)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	if s == nil || s.db == nil {
		return MigrationResult{}, core.NewError(core.CodeInternal, "nil store", false)
	}
	tables, err := s.tableNames(ctx)
	if err != nil {
		return MigrationResult{}, core.WrapError(core.CodeInternal, "inspect SQLite schema", false, err)
	}
	if err := validateExistingTables(tables); err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{}
	if len(tables) == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "begin schema migration", false, err)
		}
		defer tx.Rollback()
		for _, statement := range splitStatements(schemaSQL) {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return MigrationResult{}, core.WrapError(core.CodeInternal, "apply schema migration", false, err)
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, 1, initialSchemaName, now); err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "record schema migration", false, err)
		}
		if err := tx.Commit(); err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "commit schema migration", false, err)
		}
		result.Applied = []int{1}
	}
	if len(tables) > 0 && len(tables) < 8 {
		return MigrationResult{}, core.NewError(core.CodeLegacySchemaRebuildRequired, "incomplete CoordPlane v1 schema requires backup and a new data_dir", false)
	}
	version, err := s.migrationVersion(ctx)
	if err != nil {
		return MigrationResult{}, err
	}
	if version == 1 {
		if err := s.validateCanonicalDatabase(ctx, 1); err != nil {
			return MigrationResult{}, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "begin isolation-spec migration", false, err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, isolationSpecMigrationSQL); err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "persist Run isolation spec version", false, err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, 2, isolationSpecMigrationName, now); err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "record isolation-spec migration", false, err)
		}
		if err := tx.Commit(); err != nil {
			return MigrationResult{}, core.WrapError(core.CodeInternal, "commit isolation-spec migration", false, err)
		}
		result.Applied = append(result.Applied, 2)
		version = 2
	}
	if version == 2 {
		if err := s.validateCanonicalDatabase(ctx, 2); err != nil {
			return MigrationResult{}, err
		}
		if err := s.applyParticipantRolesMigration(ctx); err != nil {
			return MigrationResult{}, err
		}
		result.Applied = append(result.Applied, schemaVersion)
	}
	if err := s.validateCanonicalDatabase(ctx, schemaVersion); err != nil {
		return MigrationResult{}, err
	}
	return result, nil
}

// applyParticipantRolesMigration creates the unified participant framework
// tables and seeds the deterministic identities: the default human owner
// participant, the owner role (every capability), the default agent role, and
// the owner's global-scope binding. Seed capability names come from the core
// registry so the migration and the permission layer never drift.
func (s *Store) applyParticipantRolesMigration(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.WrapError(core.CodeInternal, "begin participant roles migration", false, err)
	}
	defer tx.Rollback()
	for _, statement := range splitStatements(participantRolesMigrationSQL) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return core.WrapError(core.CodeInternal, "apply participant roles schema migration", false, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ownerCapabilities, err := json.Marshal(core.CapabilityNames(core.AllCapabilities()))
	if err != nil {
		return core.WrapError(core.CodeInternal, "serialize owner role capabilities", false, err)
	}
	agentCapabilities, err := json.Marshal(core.CapabilityNames(core.AgentDefaultCapabilities()))
	if err != nil {
		return core.WrapError(core.CodeInternal, "serialize agent role capabilities", false, err)
	}
	statements := []string{
		`INSERT INTO participants(id,kind,display_name,status,version,created_at,updated_at)
VALUES(?,?,?,?,1,?,?)`,
		`INSERT INTO roles(id,name,description,capabilities,version,created_at,updated_at)
VALUES(?,?,?,?,1,?,?)`,
		`INSERT INTO participant_project_role(participant_id,project_id,role_id,version,created_at,updated_at)
VALUES(?,?,?,1,?,?)`,
	}
	values := [][]any{
		{core.DefaultHumanParticipantID, string(core.ParticipantKindHuman), "Owner", "active", now, now},
		{core.DefaultOwnerRoleID, "owner", "default owner with every capability", string(ownerCapabilities), now, now},
		{core.DefaultHumanParticipantID, core.GlobalProjectID, core.DefaultOwnerRoleID, now, now},
	}
	for index, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, values[index]...); err != nil {
			return core.WrapError(core.CodeInternal, "seed participant roles", false, err)
		}
	}
	agentRole := `INSERT INTO roles(id,name,description,capabilities,version,created_at,updated_at)
VALUES(?,?,?,?,1,?,?)`
	if _, err := tx.ExecContext(ctx, agentRole, core.DefaultAgentRoleID, "agent", "default CLI agent role", string(agentCapabilities), now, now); err != nil {
		return core.WrapError(core.CodeInternal, "seed default agent role", false, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, schemaVersion, schemaName, now); err != nil {
		return core.WrapError(core.CodeInternal, "record participant roles migration", false, err)
	}
	if err := tx.Commit(); err != nil {
		return core.WrapError(core.CodeInternal, "commit participant roles migration", false, err)
	}
	return nil
}

func (s *Store) migrationVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, legacySchemaError("read SQLite migration version", err)
	}
	if version != 1 && version != 2 && version != schemaVersion {
		return 0, legacySchemaError("legacy database migration history requires backup and a new data_dir", nil)
	}
	return version, nil
}

func (s *Store) SchemaInfo(ctx context.Context) (SchemaInfo, error) {
	tables, err := s.tableNames(ctx)
	if err != nil {
		return SchemaInfo{}, err
	}
	info := SchemaInfo{Tables: tables}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&info.JournalMode); err != nil {
		return SchemaInfo{}, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return SchemaInfo{}, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&info.BusyTimeout); err != nil {
		return SchemaInfo{}, err
	}
	info.ForeignKeys = foreignKeys == 1
	return info, nil
}

func (s *Store) configure(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return core.WrapError(core.CodeInternal, "enable SQLite foreign keys", false, err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, busyTimeoutMillis)); err != nil {
		return core.WrapError(core.CodeInternal, "configure SQLite busy timeout", false, err)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return core.WrapError(core.CodeInternal, "enable SQLite WAL", false, err)
	}
	if !strings.EqualFold(mode, "wal") {
		return core.NewError(core.CodeInternal, "SQLite did not enter WAL mode", false)
	}
	return nil
}

func (s *Store) Transact(ctx context.Context, fn func(core.Transaction) error) error {
	if s == nil || s.db == nil {
		return core.NewError(core.CodeInternal, "nil store", false)
	}
	if fn == nil {
		return core.NewError(core.CodeInvalidArgument, "transaction callback is required", false)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.WrapError(core.CodeInternal, "begin transaction", true, err)
	}
	defer tx.Rollback()
	if err := fn(&unitOfWork{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return core.WrapError(core.CodeInternal, "commit transaction", true, err)
	}
	return nil
}

func (s *Store) tableNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func validateExistingTables(tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	var unexpected []string
	for _, table := range tables {
		if !allowedTables[table] {
			unexpected = append(unexpected, table)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return core.NewError(core.CodeLegacySchemaRebuildRequired, "legacy database tables detected: "+strings.Join(unexpected, ", "), false)
	}
	return nil
}

func splitStatements(script string) []string {
	parts := strings.Split(script, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			statements = append(statements, part)
		}
	}
	return statements
}

func mapNotFound(entity, id string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return core.NewError(core.CodeNotFound, fmt.Sprintf("%s %q was not found", entity, id), false)
	}
	return err
}
