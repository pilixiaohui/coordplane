package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type runtimeTokenInput struct {
	AgentID   string
	RuntimeID string
	AttemptID string
	LeaseID   string
	Token     string
}

func recordRuntimeTokenTx(ctx context.Context, tx *sql.Tx, in runtimeTokenInput) error {
	if tx == nil {
		return errors.New("runtime token: transaction is required")
	}
	if in.AgentID == "" || in.RuntimeID == "" || in.AttemptID == "" || in.LeaseID == "" || strings.TrimSpace(in.Token) == "" {
		return errors.New("runtime token: agent, runtime, attempt, lease, and token are required")
	}
	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
UPDATE runtime_tokens
SET state = 'revoked', updated_at = ?
WHERE attempt_id = ? AND state = 'active'`,
		now, in.AttemptID,
	); err != nil {
		return fmt.Errorf("revoke prior runtime tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_tokens (
  token_hash, tenant_id, agent_id, runtime_id, attempt_id, lease_id,
  state, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, 'active', ?, ?)
ON CONFLICT(token_hash) DO UPDATE SET
  agent_id = excluded.agent_id,
  runtime_id = excluded.runtime_id,
  attempt_id = excluded.attempt_id,
  lease_id = excluded.lease_id,
  state = 'active',
  updated_at = excluded.updated_at`,
		RuntimeTokenHash(in.Token), in.AgentID, in.RuntimeID, in.AttemptID, in.LeaseID, now, now,
	); err != nil {
		return fmt.Errorf("insert runtime token: %w", err)
	}
	return nil
}

func (r *Runner) recordRuntimeToken(ctx context.Context, in runtimeTokenInput) error {
	if r == nil || r.db == nil {
		return errors.New("runtime token: runner database is required")
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		return recordRuntimeTokenTx(ctx, tx, in)
	})
}
