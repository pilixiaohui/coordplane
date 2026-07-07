package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"coordplane/internal/ids"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

var (
	ErrNoRunnableItem = errors.New("queue: no runnable item")
	ErrLeaseNotOwned  = errors.New("queue: lease is not held by owner")
)

type Queue struct {
	db *sql.DB
}

func New(db *sql.DB) *Queue {
	return &Queue{db: db}
}

type NewItem struct {
	ID             string
	TenantID       string
	QueueName      string
	Kind           string
	PayloadRef     string
	IdempotencyKey string
	Priority       int
	NextRunAt      time.Time
}

type Item struct {
	ID             string
	TenantID       string
	QueueName      string
	Kind           string
	PayloadRef     string
	State          string
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	AttemptCount   int
	NextRunAt      time.Time
	LastError      string
	IdempotencyKey string
	Priority       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (q *Queue) Enqueue(ctx context.Context, in NewItem, now time.Time) (Item, bool, error) {
	if q == nil || q.db == nil {
		return Item{}, false, errors.New("queue: nil database")
	}
	if in.QueueName == "" {
		return Item{}, false, errors.New("queue: queue name is required")
	}
	if in.Kind == "" {
		return Item{}, false, errors.New("queue: kind is required")
	}
	if in.PayloadRef == "" {
		return Item{}, false, errors.New("queue: payload ref is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if in.NextRunAt.IsZero() {
		in.NextRunAt = now
	}
	if in.TenantID == "" {
		in.TenantID = "default"
	}
	if in.ID == "" {
		id, err := ids.New("que")
		if err != nil {
			return Item{}, false, err
		}
		in.ID = id
	}

	if in.IdempotencyKey != "" {
		existing, err := q.findByIdempotency(ctx, in.QueueName, in.IdempotencyKey)
		switch {
		case err == nil:
			return existing, false, nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return Item{}, false, err
		}
	}

	_, err := q.db.ExecContext(ctx, `
INSERT INTO queue_items (
  id, tenant_id, queue_name, kind, payload_ref, state, attempt_count,
  next_run_at, idempotency_key, priority, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'queued', 0, ?, ?, ?, ?, ?)`,
		in.ID, in.TenantID, in.QueueName, in.Kind, in.PayloadRef,
		formatTime(in.NextRunAt), in.IdempotencyKey, in.Priority, formatTime(now), formatTime(now),
	)
	if err != nil {
		if in.IdempotencyKey != "" {
			existing, lookupErr := q.findByIdempotency(ctx, in.QueueName, in.IdempotencyKey)
			if lookupErr == nil {
				return existing, false, nil
			}
		}
		return Item{}, false, fmt.Errorf("enqueue %s: %w", in.QueueName, err)
	}

	item, err := q.Get(ctx, in.ID)
	if err != nil {
		return Item{}, false, err
	}
	return item, true, nil
}

func (q *Queue) Claim(ctx context.Context, queueName, owner string, leaseFor time.Duration, now time.Time) (*Item, error) {
	if q == nil || q.db == nil {
		return nil, errors.New("queue: nil database")
	}
	if queueName == "" {
		return nil, errors.New("queue: queue name is required")
	}
	if owner == "" {
		return nil, errors.New("queue: owner is required")
	}
	if leaseFor <= 0 {
		return nil, errors.New("queue: lease duration must be positive")
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM queue_items
WHERE queue_name = ?
  AND (
    (state = 'queued' AND next_run_at <= ?)
    OR (state = 'leased' AND lease_expires_at <= ?)
  )
ORDER BY priority DESC, next_run_at ASC, created_at ASC
LIMIT 10`, queueName, formatTime(now), formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("query claim candidates: %w", err)
	}

	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claim candidate: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close claim candidates: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claim candidates: %w", err)
	}

	for _, id := range candidates {
		leaseExpiresAt := now.Add(leaseFor)
		result, err := tx.ExecContext(ctx, `
UPDATE queue_items
SET state = 'leased',
    lease_owner = ?,
    lease_expires_at = ?,
    attempt_count = attempt_count + 1,
    updated_at = ?
WHERE id = ?
  AND queue_name = ?
  AND (
    (state = 'queued' AND next_run_at <= ?)
    OR (state = 'leased' AND lease_expires_at <= ?)
  )`,
			owner, formatTime(leaseExpiresAt), formatTime(now), id, queueName, formatTime(now), formatTime(now))
		if err != nil {
			return nil, fmt.Errorf("claim item %s: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read claim rows affected: %w", err)
		}
		if affected == 0 {
			continue
		}
		item, err := getTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit claim: %w", err)
		}
		return &item, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit empty claim: %w", err)
	}
	return nil, nil
}

func (q *Queue) Complete(ctx context.Context, id, owner string) error {
	if q == nil || q.db == nil {
		return errors.New("queue: nil database")
	}
	result, err := q.db.ExecContext(ctx, `
UPDATE queue_items
SET state = 'done',
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = ?
WHERE id = ? AND state = 'leased' AND lease_owner = ?`, formatTime(time.Now()), id, owner)
	if err != nil {
		return fmt.Errorf("complete queue item %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read complete rows affected: %w", err)
	}
	if affected == 0 {
		return ErrLeaseNotOwned
	}
	return nil
}

type BackoffFunc func(attemptCount int) time.Duration

func FixedBackoff(delay time.Duration) BackoffFunc {
	return func(int) time.Duration {
		return delay
	}
}

func (q *Queue) Fail(ctx context.Context, id, owner string, retryLimit int, backoff BackoffFunc, now time.Time, cause error) (Item, error) {
	if q == nil || q.db == nil {
		return Item{}, errors.New("queue: nil database")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if backoff == nil {
		backoff = FixedBackoff(0)
	}

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, fmt.Errorf("begin fail tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	item, err := getTx(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	if item.State != "leased" || item.LeaseOwner != owner {
		return Item{}, ErrLeaseNotOwned
	}

	state := "queued"
	leaseOwner := sql.NullString{}
	leaseExpiresAt := sql.NullString{}
	nextRunAt := now.Add(backoff(item.AttemptCount))
	if retryLimit <= 0 || item.AttemptCount >= retryLimit {
		state = "dead"
		nextRunAt = now
	}
	lastError := ""
	if cause != nil {
		lastError = cause.Error()
	}

	_, err = tx.ExecContext(ctx, `
UPDATE queue_items
SET state = ?,
    lease_owner = ?,
    lease_expires_at = ?,
    next_run_at = ?,
    last_error = ?,
    updated_at = ?
WHERE id = ? AND state = 'leased' AND lease_owner = ?`,
		state, leaseOwner, leaseExpiresAt, formatTime(nextRunAt), lastError, formatTime(now), id, owner)
	if err != nil {
		return Item{}, fmt.Errorf("fail queue item %s: %w", id, err)
	}

	updated, err := getTx(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("commit fail: %w", err)
	}
	return updated, nil
}

func (q *Queue) Get(ctx context.Context, id string) (Item, error) {
	if q == nil || q.db == nil {
		return Item{}, errors.New("queue: nil database")
	}
	return scanItem(q.db.QueryRowContext(ctx, selectItemSQL+` WHERE id = ?`, id))
}

func (q *Queue) findByIdempotency(ctx context.Context, queueName, idempotencyKey string) (Item, error) {
	return scanItem(q.db.QueryRowContext(ctx, selectItemSQL+` WHERE queue_name = ? AND idempotency_key = ?`, queueName, idempotencyKey))
}

func getTx(ctx context.Context, tx *sql.Tx, id string) (Item, error) {
	return scanItem(tx.QueryRowContext(ctx, selectItemSQL+` WHERE id = ?`, id))
}

const selectItemSQL = `SELECT id, tenant_id, queue_name, kind, payload_ref, state,
  lease_owner, lease_expires_at, attempt_count, next_run_at, last_error,
  idempotency_key, priority, created_at, updated_at
FROM queue_items`

type itemScanner interface {
	Scan(...any) error
}

func scanItem(row itemScanner) (Item, error) {
	var item Item
	var leaseOwner sql.NullString
	var leaseExpiresAt sql.NullString
	var lastError sql.NullString
	var idempotencyKey sql.NullString
	var nextRunAt string
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&item.ID,
		&item.TenantID,
		&item.QueueName,
		&item.Kind,
		&item.PayloadRef,
		&item.State,
		&leaseOwner,
		&leaseExpiresAt,
		&item.AttemptCount,
		&nextRunAt,
		&lastError,
		&idempotencyKey,
		&item.Priority,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Item{}, err
	}
	item.LeaseOwner = leaseOwner.String
	item.LastError = lastError.String
	item.IdempotencyKey = idempotencyKey.String
	var err error
	item.NextRunAt, err = parseTime(nextRunAt)
	if err != nil {
		return Item{}, fmt.Errorf("parse next_run_at: %w", err)
	}
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Item{}, fmt.Errorf("parse created_at: %w", err)
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Item{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if leaseExpiresAt.Valid && leaseExpiresAt.String != "" {
		parsed, err := parseTime(leaseExpiresAt.String)
		if err != nil {
			return Item{}, fmt.Errorf("parse lease_expires_at: %w", err)
		}
		item.LeaseExpiresAt = &parsed
	}
	return item, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
