package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"coordplane/internal/ids"
)

const (
	runtimeCleanupTimeout      = 30 * time.Second
	runtimeCleanupLease        = 45 * time.Second
	runtimeCleanupPersistLimit = 5 * time.Second
)

type DockerContainerManager interface {
	InspectContainer(context.Context, string) (map[string]string, error)
	RemoveContainer(context.Context, string) error
}

type RuntimeFinalizer interface {
	FinalizeRuntime(context.Context, string, string) error
	ReconcileRuntimeCleanup(context.Context) error
}

type runtimeCleanupRecord struct {
	ID                  string
	RuntimeID           string
	AttemptID           string
	LeaseID             string
	ContainerID         string
	ContainerName       string
	RuntimeState        string
	CleanupState        string
	CleanupLeaseExpires string
	AttemptStatus       string
	LeaseState          string
	RouteState          string
}

func (r *DockerRuntime) FinalizeRuntime(parent context.Context, attemptID, reason string) error {
	if r == nil || r.db == nil || attemptID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), runtimeCleanupTimeout)
	defer cancel()
	record, owner, claimed, err := r.claimRuntimeCleanup(ctx, attemptID, reason)
	if err != nil || !claimed {
		return err
	}
	cleanupErr := r.removeOwnedContainer(ctx, record)
	if cleanupErr == nil {
		cleanupErr = r.confirmRuntimeRemoved(ctx, record, owner)
	}
	if cleanupErr == nil {
		return nil
	}
	persistCtx, persistCancel := context.WithTimeout(context.Background(), runtimeCleanupPersistLimit)
	defer persistCancel()
	if persistErr := r.markRuntimeCleanupFailed(persistCtx, record, owner, cleanupErr); persistErr != nil {
		return errors.Join(cleanupErr, fmt.Errorf("persist runtime cleanup failure: %w", persistErr))
	}
	return cleanupErr
}

func (r *DockerRuntime) ReconcileRuntimeCleanup(parent context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), runtimeCleanupTimeout)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `
SELECT ri.attempt_id, COALESCE(NULLIF(ri.cleanup_reason, ''), 'runtime cleanup reconciliation')
FROM runtime_instances ri
JOIN attempts a ON a.id = ri.attempt_id
LEFT JOIN leases l ON l.id = ri.lease_id
LEFT JOIN session_routes sr ON sr.id = l.session_route_id
WHERE ri.runtime_kind = 'docker'
  AND ri.cleanup_state <> 'removed'
  AND a.status <> 'waiting'
  AND COALESCE(sr.state, '') <> 'waiting'
  AND (ri.state = 'failed' OR a.status IN ('completed', 'failed', 'interrupted', 'expired'))
ORDER BY ri.created_at, ri.id`)
	if err != nil {
		return fmt.Errorf("list runtime cleanup reconciliation candidates: %w", err)
	}
	var candidates [][2]string
	for rows.Next() {
		var attemptID, reason string
		if err := rows.Scan(&attemptID, &reason); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, [2]string{attemptID, reason})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var cleanupErrors []error
	for _, candidate := range candidates {
		if err := r.FinalizeRuntime(ctx, candidate[0], candidate[1]); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("reconcile attempt %s: %w", candidate[0], err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (r *DockerRuntime) claimRuntimeCleanup(ctx context.Context, attemptID, reason string) (runtimeCleanupRecord, string, bool, error) {
	var record runtimeCleanupRecord
	owner, err := ids.New("cleanup")
	if err != nil {
		return record, "", false, err
	}
	now := time.Now()
	nowText := formatTime(now)
	leaseExpires := formatTime(now.Add(runtimeCleanupLease))
	claimed := false
	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		if err := scanRuntimeCleanupRecord(tx.QueryRowContext(ctx, `
SELECT ri.id, ri.runtime_id, ri.attempt_id, ri.lease_id,
  ri.container_id, ri.container_name, ri.state, ri.cleanup_state,
  ri.cleanup_lease_expires_at, a.status, l.state, COALESCE(sr.state, '')
FROM runtime_instances ri
JOIN attempts a ON a.id = ri.attempt_id
JOIN leases l ON l.id = ri.lease_id
LEFT JOIN session_routes sr ON sr.id = l.session_route_id
WHERE ri.attempt_id = ? AND ri.runtime_kind = 'docker'`, attemptID), &record); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lookup managed runtime cleanup: %w", err)
		}
		if record.CleanupState == "removed" || record.AttemptStatus == "waiting" || record.RouteState == "waiting" {
			return nil
		}
		if record.RuntimeState != "failed" && !terminalStatus(record.AttemptStatus) && record.LeaseState == "active" {
			return nil
		}
		if reason == "" {
			reason = "managed runtime terminal cleanup"
		}
		requested, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET cleanup_state = 'pending', cleanup_reason = ?, cleanup_error = '', updated_at = ?
WHERE id = ? AND cleanup_state IN ('not_requested', 'failed')`, reason, nowText, record.ID)
		if err != nil {
			return fmt.Errorf("request managed runtime cleanup: %w", err)
		}
		if count, _ := requested.RowsAffected(); count > 0 {
			if _, err := appendEvent(ctx, tx, "runtime.cleanup_requested", "runtime_instance", record.RuntimeID, map[string]any{
				"attempt_id": record.AttemptID,
				"reason":     reason,
			}); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET cleanup_state = 'in_progress', cleanup_owner = ?, cleanup_lease_expires_at = ?,
  cleanup_attempts = cleanup_attempts + 1, updated_at = ?
WHERE id = ?
  AND (
    cleanup_state = 'pending'
    OR cleanup_state = 'failed'
    OR (cleanup_state = 'in_progress' AND cleanup_lease_expires_at < ?)
  )`, owner, leaseExpires, nowText, record.ID, nowText)
		if err != nil {
			return fmt.Errorf("claim managed runtime cleanup: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		claimed = count == 1
		return nil
	})
	return record, owner, claimed, err
}

func (r *DockerRuntime) removeOwnedContainer(ctx context.Context, record runtimeCleanupRecord) error {
	manager, ok := r.docker.(DockerContainerManager)
	if !ok {
		return errors.New("managed runtime cleanup: Docker client does not support inspect/remove")
	}
	containerRef := record.ContainerName
	if containerRef == "" {
		containerRef = record.ContainerID
	}
	if containerRef == "" {
		return nil
	}
	labels, err := manager.InspectContainer(ctx, containerRef)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed container before cleanup: %w", err)
	}
	wantLabels := map[string]string{
		"coordplane.managed":    "true",
		"coordplane.runtime_id": record.RuntimeID,
		"coordplane.attempt_id": record.AttemptID,
		"coordplane.lease_id":   record.LeaseID,
	}
	for key, want := range wantLabels {
		if labels[key] != want {
			return fmt.Errorf("managed runtime cleanup ownership mismatch for %s", key)
		}
	}
	if err := manager.RemoveContainer(ctx, containerRef); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove managed container: %w", err)
	}
	if _, err := manager.InspectContainer(ctx, containerRef); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("managed runtime cleanup: container still exists after remove")
		}
		return fmt.Errorf("inspect managed container after cleanup: %w", err)
	}
	return nil
}

func (r *DockerRuntime) confirmRuntimeRemoved(ctx context.Context, record runtimeCleanupRecord, owner string) error {
	now := formatTime(time.Now())
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET state = CASE WHEN state = 'failed' THEN 'failed' ELSE 'stopped' END,
  cleanup_state = 'removed', cleanup_error = '', cleanup_lease_expires_at = '',
  removed_at = ?, updated_at = ?
WHERE id = ? AND cleanup_state = 'in_progress' AND cleanup_owner = ?`, now, now, record.ID, owner)
		if err != nil {
			return fmt.Errorf("confirm managed runtime removed: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("confirm managed runtime removed: stale cleanup owner")
		}
		_, err = appendEvent(ctx, tx, "runtime.cleanup_removed", "runtime_instance", record.RuntimeID, map[string]any{
			"attempt_id": record.AttemptID,
		})
		return err
	})
}

func (r *DockerRuntime) markRuntimeCleanupFailed(ctx context.Context, record runtimeCleanupRecord, owner string, cause error) error {
	now := formatTime(time.Now())
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET state = CASE WHEN state = 'failed' THEN 'failed' ELSE 'stopped' END,
  cleanup_state = 'failed', cleanup_error = ?, cleanup_lease_expires_at = '', updated_at = ?
WHERE id = ? AND cleanup_state = 'in_progress' AND cleanup_owner = ?`, cause.Error(), now, record.ID, owner)
		if err != nil {
			return fmt.Errorf("mark managed runtime cleanup failed: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("mark managed runtime cleanup failed: stale cleanup owner")
		}
		_, err = appendEvent(ctx, tx, "runtime.cleanup_failed", "runtime_instance", record.RuntimeID, map[string]any{
			"attempt_id": record.AttemptID,
			"error":      cause.Error(),
		})
		return err
	})
}

func scanRuntimeCleanupRecord(row interface{ Scan(...any) error }, out *runtimeCleanupRecord) error {
	return row.Scan(
		&out.ID,
		&out.RuntimeID,
		&out.AttemptID,
		&out.LeaseID,
		&out.ContainerID,
		&out.ContainerName,
		&out.RuntimeState,
		&out.CleanupState,
		&out.CleanupLeaseExpires,
		&out.AttemptStatus,
		&out.LeaseState,
		&out.RouteState,
	)
}
