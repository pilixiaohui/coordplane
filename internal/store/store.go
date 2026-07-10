package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"coordplane/internal/events"
	"coordplane/internal/ids"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) DB() *sql.DB {
	return s.db
}

type MigrationResult struct {
	Applied []string
}

type migration struct {
	Version string
	Name    string
	SQL     string
}

var migrations = []migration{
	{
		Version: "001_core_store_queue_events",
		Name:    "core store, event log, and DB queue",
		SQL:     coreSchemaSQL,
	},
	{
		Version: "002_team_config_skill_registry",
		Name:    "TeamConfig and skill registry",
		SQL:     teamConfigSkillSchemaSQL,
	},
	{
		Version: "003_session_lifecycle_guards",
		Name:    "session lifecycle prepare leases and active guards",
		SQL:     sessionLifecycleSchemaSQL,
	},
	{
		Version: "004_object_store",
		Name:    "durable immutable object blobs",
		SQL:     objectStoreSchemaSQL,
	},
	{
		Version: "005_controlled_git_v1",
		Name:    "controlled Git repositories, workspaces, operations, locks, and changesets",
		SQL:     controlledGitSchemaSQL,
	},
	{
		Version: "006_controlled_git_v2",
		Name:    "controlled Git merge attempts, conflict sets, and rollback points",
		SQL:     controlledGitV2SchemaSQL,
	},
	{
		Version: "007_runtime_evidence",
		Name:    "runtime instances and inspect evidence",
		SQL:     runtimeEvidenceSchemaSQL,
	},
	{
		Version: "008_cli_sessions",
		Name:    "durable CLI sessions and process evidence",
		SQL:     cliSessionSchemaSQL,
	},
	{
		Version: "009_command_runs",
		Name:    "agent-facing command runs and durable command evidence",
		SQL:     commandRunSchemaSQL,
	},
	{
		Version: "010_runtime_tokens",
		Name:    "runtime-issued bearer token hashes",
		SQL:     runtimeTokenSchemaSQL,
	},
	{
		Version: "011_validation_assessments",
		Name:    "canonical validation assessments and evidence",
		SQL:     validationAssessmentSchemaSQL,
	},
	{
		Version: "012_release_acceptances",
		Name:    "release acceptance predicate ledger",
		SQL:     releaseAcceptanceSchemaSQL,
	},
	{
		Version: "013_contract_team_scopes",
		Name:    "durable contract to TeamConfig scope binding",
		SQL:     contractTeamScopeSchemaSQL,
	},
	{
		Version: "014_agent_communication_envelopes",
		Name:    "canonical agent communication envelopes and mailbox projection",
		SQL:     agentCommunicationEnvelopeSchemaSQL,
	},
	{
		Version: "015_controlled_git_operation_evidence",
		Name:    "controlled Git repository aliases and operation execution evidence",
		SQL:     controlledGitOperationEvidenceSchemaSQL,
	},
	{
		Version: "016_controlled_git_operation_subject_kind",
		Name:    "controlled Git operation subject origin evidence",
		SQL:     controlledGitOperationSubjectKindSchemaSQL,
	},
	{
		Version: "017_operator_task_runs",
		Name:    "operator root task run idempotency ledger",
		SQL:     operatorTaskRunsSchemaSQL,
	},
	{
		Version: "018_capability_audit_outcomes",
		Name:    "capability audit outcomes and runtime scope",
		SQL:     capabilityAuditOutcomeSchemaSQL,
	},
	{
		Version: "019_managed_runtime_cleanup",
		Name:    "managed runtime cleanup ownership and recovery ledger",
		SQL:     managedRuntimeCleanupSchemaSQL,
	},
	{
		Version: "020_contract_completion_evidence",
		Name:    "canonical contract completion evidence bindings",
		SQL:     contractCompletionEvidenceSchemaSQL,
	},
	{
		Version: "021_provider_tool_outcomes",
		Name:    "redacted provider tool outcome projection",
		SQL:     providerToolOutcomeSchemaSQL,
	},
}

func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	if s == nil || s.db == nil {
		return MigrationResult{}, errors.New("store: nil database")
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
)`); err != nil {
		return MigrationResult{}, fmt.Errorf("create schema_migrations: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var result MigrationResult
	for _, m := range migrations {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, m.Version).Scan(&existing)
		switch {
		case err == nil:
			continue
		case errors.Is(err, sql.ErrNoRows):
		default:
			return MigrationResult{}, fmt.Errorf("read migration %s: %w", m.Version, err)
		}

		if err := execStatements(ctx, tx, m.SQL); err != nil {
			return MigrationResult{}, fmt.Errorf("apply migration %s: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, formatTime(time.Now()),
		); err != nil {
			return MigrationResult{}, fmt.Errorf("record migration %s: %w", m.Version, err)
		}
		result.Applied = append(result.Applied, m.Version)
	}

	if err := tx.Commit(); err != nil {
		return MigrationResult{}, fmt.Errorf("commit migrations: %w", err)
	}
	return result, nil
}

func (s *Store) Tx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	if s == nil || s.db == nil {
		return errors.New("store: nil database")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (s *Store) AppendEvent(ctx context.Context, event events.Event) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("store: nil database")
	}
	return appendEvent(ctx, s.db, event)
}

func AppendEventTx(ctx context.Context, tx *sql.Tx, event events.Event) (string, error) {
	if tx == nil {
		return "", errors.New("store: nil transaction")
	}
	return appendEvent(ctx, tx, event)
}

type eventExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func appendEvent(ctx context.Context, execer eventExecer, event events.Event) (string, error) {
	if event.Type == "" {
		return "", errors.New("store: event type is required")
	}
	if event.AggregateType == "" {
		return "", errors.New("store: event aggregate type is required")
	}
	if event.AggregateID == "" {
		return "", errors.New("store: event aggregate id is required")
	}
	if event.ID == "" {
		id, err := ids.New("evt")
		if err != nil {
			return "", err
		}
		event.ID = id
	}
	if event.TenantID == "" {
		event.TenantID = "default"
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now()
	}
	payload := string(event.PayloadJSON)
	if payload == "" {
		payload = "{}"
	}

	_, err := execer.ExecContext(ctx, `
INSERT INTO events (
  id, tenant_id, trace_id, subject_kind, subject_id, agent_id, runtime_id,
  capability_name, event_type, aggregate_type, aggregate_id, payload_json, occurred_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.TenantID, event.TraceID, event.SubjectKind, event.SubjectID,
		event.AgentID, event.RuntimeID, event.CapabilityName, event.Type,
		event.AggregateType, event.AggregateID, payload, formatTime(event.OccurredAt),
	)
	if err != nil {
		return "", fmt.Errorf("append event %s: %w", event.Type, err)
	}
	return event.ID, nil
}

type EventFilter struct {
	TraceID       string
	AggregateID   string
	AggregateType string
	Limit         int
}

func (s *Store) ListEvents(ctx context.Context, filter EventFilter) ([]events.Event, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: nil database")
	}
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if filter.TraceID != "" {
		clauses = append(clauses, "trace_id = ?")
		args = append(args, filter.TraceID)
	}
	if filter.AggregateID != "" {
		clauses = append(clauses, "aggregate_id = ?")
		args = append(args, filter.AggregateID)
	}
	if filter.AggregateType != "" {
		clauses = append(clauses, "aggregate_type = ?")
		args = append(args, filter.AggregateType)
	}

	query := `SELECT id, tenant_id, trace_id, subject_kind, subject_id, agent_id, runtime_id,
  capability_name, event_type, aggregate_type, aggregate_id, payload_json, occurred_at
FROM events`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY occurred_at ASC, id ASC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []events.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

type eventScanner interface {
	Scan(...any) error
}

func scanEvent(row eventScanner) (events.Event, error) {
	var event events.Event
	var payload string
	var occurredAt string
	if err := row.Scan(
		&event.ID,
		&event.TenantID,
		&event.TraceID,
		&event.SubjectKind,
		&event.SubjectID,
		&event.AgentID,
		&event.RuntimeID,
		&event.CapabilityName,
		&event.Type,
		&event.AggregateType,
		&event.AggregateID,
		&payload,
		&occurredAt,
	); err != nil {
		return events.Event{}, fmt.Errorf("scan event: %w", err)
	}
	parsed, err := parseTime(occurredAt)
	if err != nil {
		return events.Event{}, fmt.Errorf("parse event time: %w", err)
	}
	event.PayloadJSON = []byte(payload)
	event.OccurredAt = parsed
	return event, nil
}

func execStatements(ctx context.Context, tx *sql.Tx, script string) error {
	for _, statement := range strings.Split(script, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
