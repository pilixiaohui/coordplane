package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"coordplane/internal/core"
)

type schemaObject struct {
	Type      string
	Name      string
	TableName string
	SQL       string
}

func (s *Store) validateCanonicalDatabase(ctx context.Context, version int) error {
	expected, err := canonicalSchemaObjects(ctx, version)
	if err != nil {
		return core.WrapError(core.CodeInternal, "build canonical SQLite schema", false, err)
	}
	actual, err := readSchemaObjects(ctx, s.db)
	if err != nil {
		return legacySchemaError("inspect existing SQLite schema", err)
	}
	if err := compareSchemaObjects(expected, actual); err != nil {
		return err
	}
	if err := s.validateMigrationHistory(ctx, version); err != nil {
		return err
	}
	if err := s.validateIntegrity(ctx); err != nil {
		return err
	}
	return s.validateForeignKeys(ctx)
}

func canonicalSchemaObjects(ctx context.Context, version int) (map[string]schemaObject, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range splitStatements(schemaSQL) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	if version >= 2 {
		if _, err := db.ExecContext(ctx, isolationSpecMigrationSQL); err != nil {
			return nil, err
		}
	}
	return readSchemaObjects(ctx, db)
}

func readSchemaObjects(ctx context.Context, db *sql.DB) (map[string]schemaObject, error) {
	rows, err := db.QueryContext(ctx, `
SELECT type,name,tbl_name,sql
FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%'
  AND type IN ('table','index','view','trigger')
ORDER BY type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make(map[string]schemaObject)
	for rows.Next() {
		var object schemaObject
		var definition sql.NullString
		if err := rows.Scan(&object.Type, &object.Name, &object.TableName, &definition); err != nil {
			return nil, err
		}
		if !definition.Valid {
			return nil, fmt.Errorf("schema object %s %q has no definition", object.Type, object.Name)
		}
		object.SQL = strings.TrimSpace(definition.String)
		objects[schemaObjectKey(object)] = object
	}
	return objects, rows.Err()
}

func compareSchemaObjects(expected, actual map[string]schemaObject) error {
	if len(actual) != len(expected) {
		return legacySchemaError(fmt.Sprintf("SQLite schema object count is %d, want %d", len(actual), len(expected)), nil)
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := expected[key]
		got, ok := actual[key]
		if !ok {
			return legacySchemaError(fmt.Sprintf("SQLite schema is missing %s %q", want.Type, want.Name), nil)
		}
		if got.TableName != want.TableName || got.SQL != want.SQL {
			return legacySchemaError(fmt.Sprintf("SQLite %s %q does not match the canonical schema", want.Type, want.Name), nil)
		}
	}
	return nil
}

func schemaObjectKey(object schemaObject) string {
	return object.Type + "\x00" + object.Name
}

func (s *Store) validateMigrationHistory(ctx context.Context, version int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT version,name,applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return legacySchemaError("read SQLite migration history", err)
	}
	defer rows.Close()
	type migrationRecord struct {
		version   int
		name      string
		appliedAt string
	}
	var history []migrationRecord
	for rows.Next() {
		var record migrationRecord
		if err := rows.Scan(&record.version, &record.name, &record.appliedAt); err != nil {
			return legacySchemaError("read SQLite migration history", err)
		}
		history = append(history, record)
	}
	if err := rows.Err(); err != nil {
		return legacySchemaError("read SQLite migration history", err)
	}
	want := []struct {
		version int
		name    string
	}{{1, initialSchemaName}}
	if version >= 2 {
		want = append(want, struct {
			version int
			name    string
		}{schemaVersion, schemaName})
	}
	if len(history) != len(want) {
		return legacySchemaError("legacy database migration history requires backup and a new data_dir", nil)
	}
	for index, record := range history {
		if record.version != want[index].version || record.name != want[index].name {
			return legacySchemaError("legacy database migration history requires backup and a new data_dir", nil)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.appliedAt); err != nil {
			return legacySchemaError("SQLite migration timestamp is invalid", err)
		}
	}
	return nil
}

func (s *Store) validateIntegrity(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return legacySchemaError("run SQLite integrity check", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return legacySchemaError("read SQLite integrity check", err)
		}
		count++
		if result != "ok" {
			return legacySchemaError("SQLite integrity check failed", nil)
		}
	}
	if err := rows.Err(); err != nil {
		return legacySchemaError("read SQLite integrity check", err)
	}
	if count != 1 {
		return legacySchemaError("SQLite integrity check returned an unexpected result", nil)
	}
	return nil
}

func (s *Store) validateForeignKeys(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return legacySchemaError("run SQLite foreign key check", err)
	}
	defer rows.Close()
	if rows.Next() {
		return legacySchemaError("SQLite foreign key check failed", nil)
	}
	if err := rows.Err(); err != nil {
		return legacySchemaError("read SQLite foreign key check", err)
	}
	return nil
}

func legacySchemaError(message string, cause error) error {
	if cause != nil {
		return core.WrapError(core.CodeLegacySchemaRebuildRequired, message, false, cause)
	}
	return core.NewError(core.CodeLegacySchemaRebuildRequired, message, false)
}
