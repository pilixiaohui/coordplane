package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"coordplane/internal/ids"
	"coordplane/internal/queue"
)

var ErrActiveResource = errors.New("runtime lifecycle: resource is active")

type resumeAdapter interface {
	Resume(ctx context.Context, req ResumeRequest) error
}

type activeGuardInput struct {
	ResourceKind   string
	ResourceRef    string
	AttemptID      string
	LeaseID        string
	SessionRouteID string
}

func (r *Runner) AcquirePrepareLease(ctx context.Context, in PrepareLeaseInput) (PrepareLease, error) {
	if in.LeaseID == "" || in.AgentID == "" || in.Owner == "" {
		return PrepareLease{}, errors.New("prepare lease: lease_id, agent_id, and owner are required")
	}
	if in.TTL <= 0 {
		in.TTL = 5 * time.Minute
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	expiresAt := now.Add(in.TTL)

	var out PrepareLease
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var leaseState string
		if err := tx.QueryRowContext(ctx, `
SELECT state
FROM leases
WHERE id = ? AND agent_id = ?`,
			in.LeaseID, in.AgentID,
		).Scan(&leaseState); err != nil {
			return fmt.Errorf("prepare lease: verify lease: %w", err)
		}
		if leaseState != "active" {
			return fmt.Errorf("prepare lease: lease %s is %s, not active", in.LeaseID, leaseState)
		}

		existing, ok, err := activePrepareLeaseForLease(ctx, tx, in.LeaseID)
		if err != nil {
			return err
		}
		if ok {
			if existing.Owner != in.Owner && now.Before(existing.ExpiresAt) {
				return fmt.Errorf("prepare lease: %w: lease %s is held by %s", ErrActiveResource, in.LeaseID, existing.Owner)
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE prepare_leases
SET attempt_id = ?, agent_id = ?, owner = ?, expires_at = ?, updated_at = ?
WHERE id = ?`,
				nullable(in.AttemptID), in.AgentID, in.Owner, formatTime(expiresAt), formatTime(now), existing.ID,
			); err != nil {
				return fmt.Errorf("refresh prepare lease: %w", err)
			}
			out = existing
			out.AttemptID = in.AttemptID
			out.AgentID = in.AgentID
			out.Owner = in.Owner
			out.ExpiresAt = expiresAt
			out.UpdatedAt = now.UTC()
			return nil
		}

		leaseID, err := ids.New("prep")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO prepare_leases (
  id, tenant_id, lease_id, attempt_id, agent_id, owner, state,
  expires_at, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, 'active', ?, ?, ?)`,
			leaseID, in.LeaseID, nullable(in.AttemptID), in.AgentID, in.Owner,
			formatTime(expiresAt), formatTime(now), formatTime(now),
		); err != nil {
			return fmt.Errorf("insert prepare lease: %w", err)
		}
		out = PrepareLease{
			ID:        leaseID,
			LeaseID:   in.LeaseID,
			AttemptID: in.AttemptID,
			AgentID:   in.AgentID,
			Owner:     in.Owner,
			State:     "active",
			ExpiresAt: expiresAt.UTC(),
			CreatedAt: now.UTC(),
			UpdatedAt: now.UTC(),
		}
		return nil
	})
	return out, err
}

func (r *Runner) ReleasePrepareLease(ctx context.Context, prepareLeaseID string) error {
	if prepareLeaseID == "" {
		return errors.New("prepare lease: id is required")
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE prepare_leases
SET state = 'released', updated_at = ?
WHERE id = ? AND state = 'active'`,
			formatTime(time.Now()), prepareLeaseID,
		)
		return err
	})
}

func (r *Runner) ResumeRoute(ctx context.Context, in ResumeRouteInput) (ResumeResult, error) {
	if in.RouteID == "" {
		return ResumeResult{}, errors.New("session.resume: route_id is required")
	}
	resumer, ok := r.adapter.(resumeAdapter)
	if !ok {
		return ResumeResult{}, errors.New("session.resume: CLI adapter does not support resume")
	}

	var route SessionRoute
	var attempt Attempt
	var alreadyResumed bool
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		route, err = routeByIDTx(ctx, tx, in.RouteID)
		if err != nil {
			return err
		}
		if !resumableRouteState(route.State) {
			return fmt.Errorf("session.resume: route %s is %s, not resumable", route.ID, route.State)
		}
		attempt, err = attemptByIDTx(ctx, tx, route.AttemptID)
		if err != nil {
			return err
		}
		if !resumableAttemptStatus(attempt.Status) {
			return fmt.Errorf("session.resume: attempt %s is %s, not resumable", attempt.ID, attempt.Status)
		}
		alreadyResumed, err = sessionResumeEventExists(ctx, tx, route.ID)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ResumeResult{}, err
	}
	if alreadyResumed && route.State == "active" && attempt.Status == "running" {
		return ResumeResult{AttemptID: attempt.ID, RouteID: route.ID, State: "already_resumed", MailboxIDs: append([]string(nil), in.MailboxIDs...)}, nil
	}
	env, err := r.env(route.AgentID, route.RuntimeID, attempt.ID, route.AssignmentID, attempt.LeaseID, route.Workdir, route.CLIBackend)
	if err != nil {
		return ResumeResult{}, err
	}
	if err := r.recordRuntimeToken(ctx, runtimeTokenInput{
		AgentID:   route.AgentID,
		RuntimeID: route.RuntimeID,
		AttemptID: attempt.ID,
		LeaseID:   attempt.LeaseID,
		Token:     env["COORDPLANE_TOKEN"],
	}); err != nil {
		return ResumeResult{}, err
	}

	if err := resumer.Resume(ctx, ResumeRequest{
		Route:      route,
		Reason:     in.Reason,
		MailboxIDs: append([]string(nil), in.MailboxIDs...),
		Env:        env,
	}); err != nil {
		return ResumeResult{}, err
	}

	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		if _, err := ensureActiveGuardTx(ctx, tx, activeGuardInput{
			ResourceKind:   "session_resume",
			ResourceRef:    route.ID,
			AttemptID:      attempt.ID,
			LeaseID:        attempt.LeaseID,
			SessionRouteID: route.ID,
		}, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE attempts
SET status = 'running', ended_at = NULL
WHERE id = ?`,
			attempt.ID,
		); err != nil {
			return fmt.Errorf("mark resumed attempt running: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE session_routes SET state = 'active', updated_at = ? WHERE id = ?`,
			now, route.ID,
		); err != nil {
			return fmt.Errorf("mark resumed route active: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "session.resumed", "session_route", route.ID, map[string]any{
			"attempt_id":  attempt.ID,
			"mailbox_ids": in.MailboxIDs,
			"reason":      in.Reason,
		}); err != nil {
			return err
		}
		if capabilities, ok := AdapterCapabilitiesForBackend(r.adapter, route.CLIBackend); ok {
			if _, err := appendAdapterCapabilityEvent(ctx, tx, route.ID, route.CLIBackend, capabilities); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{AttemptID: attempt.ID, RouteID: route.ID, State: "resumed", MailboxIDs: append([]string(nil), in.MailboxIDs...)}, nil
}

func (r *Runner) ProcessResumeQueue(ctx context.Context, owner string) (ResumeQueueResult, error) {
	if owner == "" {
		return ResumeQueueResult{}, errors.New("runtime.resume: owner is required")
	}
	q := queue.New(r.db)
	item, err := q.Claim(ctx, "runtime.resume", owner, time.Minute, time.Now())
	if err != nil {
		return ResumeQueueResult{}, err
	}
	if item == nil {
		return ResumeQueueResult{Idle: true, State: "idle"}, nil
	}

	mailboxID, ok := strings.CutPrefix(item.PayloadRef, "mailbox:")
	if !ok || mailboxID == "" {
		_, _ = q.Fail(ctx, item.ID, owner, 1, queue.FixedBackoff(time.Minute), time.Now(), errors.New("runtime.resume: malformed payload_ref"))
		return ResumeQueueResult{}, fmt.Errorf("runtime.resume: malformed payload_ref %q", item.PayloadRef)
	}
	mailbox, err := r.mailbox(ctx, mailboxID)
	if err != nil {
		_, _ = q.Fail(ctx, item.ID, owner, 3, queue.FixedBackoff(time.Minute), time.Now(), err)
		return ResumeQueueResult{}, err
	}
	if mailbox.State != "pending" {
		if err := q.Complete(ctx, item.ID, owner); err != nil {
			return ResumeQueueResult{}, err
		}
		return ResumeQueueResult{QueueItemID: item.ID, MailboxID: mailboxID, State: "skipped"}, nil
	}
	routeID := mailbox.SessionRouteID
	if routeID == "" {
		routeID, err = r.latestResumableRouteForAgent(ctx, mailbox.RecipientAgentID)
		if err != nil {
			_, _ = q.Fail(ctx, item.ID, owner, 3, queue.FixedBackoff(time.Minute), time.Now(), err)
			return ResumeQueueResult{}, err
		}
	}
	resumed, err := r.ResumeRoute(ctx, ResumeRouteInput{
		RouteID:    routeID,
		Reason:     "mailbox.resume",
		MailboxIDs: []string{mailboxID},
	})
	if err != nil {
		_, _ = q.Fail(ctx, item.ID, owner, 3, queue.FixedBackoff(time.Minute), time.Now(), err)
		return ResumeQueueResult{}, err
	}
	if err := q.Complete(ctx, item.ID, owner); err != nil {
		return ResumeQueueResult{}, err
	}
	return ResumeQueueResult{
		QueueItemID: item.ID,
		MailboxID:   mailboxID,
		RouteID:     resumed.RouteID,
		State:       resumed.State,
	}, nil
}

func (r *Runner) GuardCleanup(ctx context.Context, target CleanupTarget) error {
	if target.ResourceKind == "" || target.ResourceRef == "" {
		return errors.New("cleanup guard: resource_kind and resource_ref are required")
	}
	var blocked bool
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		switch target.ResourceKind {
		case "lease":
			var state string
			err := tx.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, target.ResourceRef).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			blocked = state == "active"
		case "attempt":
			var status string
			err := tx.QueryRowContext(ctx, `SELECT status FROM attempts WHERE id = ?`, target.ResourceRef).Scan(&status)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			blocked = activeAttemptStatus(status)
		case "session_route", "session":
			var state string
			err := tx.QueryRowContext(ctx, `SELECT state FROM session_routes WHERE id = ?`, target.ResourceRef).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			blocked = resumableRouteState(state)
		default:
			var id string
			err := tx.QueryRowContext(ctx, `
SELECT id
FROM active_guards
WHERE resource_kind = ? AND resource_ref = ? AND state = 'active'
LIMIT 1`,
				target.ResourceKind, target.ResourceRef,
			).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			blocked = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("%w: %s %s", ErrActiveResource, target.ResourceKind, target.ResourceRef)
	}
	return nil
}

func (r *Runner) latestResumableRouteForAgent(ctx context.Context, agentID string) (string, error) {
	var routeID string
	err := r.db.QueryRowContext(ctx, `
SELECT id
FROM session_routes
WHERE agent_id = ? AND state IN ('active', 'waiting')
ORDER BY updated_at DESC, created_at DESC
LIMIT 1`, agentID).Scan(&routeID)
	if err != nil {
		return "", fmt.Errorf("runtime.resume: find route for %s: %w", agentID, err)
	}
	return routeID, nil
}

func (r *Runner) activeSessionForLease(ctx context.Context, leaseID string) (AssignmentSession, bool, error) {
	var session AssignmentSession
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		attempt, routeID, ok, err := activeAttemptForLeaseTx(ctx, tx, leaseID)
		if err != nil || !ok {
			return err
		}
		if routeID == "" {
			return fmt.Errorf("runtime runner: lease %s already has active attempt %s without a pinned route", leaseID, attempt.ID)
		}
		route, err := routeByIDTx(ctx, tx, routeID)
		if err != nil {
			return err
		}
		session = AssignmentSession{AttemptID: attempt.ID, Route: route, LeaseID: leaseID}
		return nil
	})
	if err != nil {
		return AssignmentSession{}, false, err
	}
	if session.AttemptID == "" {
		return AssignmentSession{}, false, nil
	}
	return session, true, nil
}

func (r *Runner) protectActiveResource(ctx context.Context, in activeGuardInput) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := ensureActiveGuardTx(ctx, tx, in, formatTime(time.Now()))
		return err
	})
}

func (r *Runner) attachActiveGuards(ctx context.Context, attemptID, routeID string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE active_guards
SET session_route_id = ?, updated_at = ?
WHERE attempt_id = ? AND state = 'active'`,
			routeID, formatTime(time.Now()), attemptID,
		)
		return err
	})
}

func (r *Runner) releaseActiveGuardsForAttempt(ctx context.Context, attemptID string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		return releaseActiveGuardsTx(ctx, tx, attemptID, formatTime(time.Now()))
	})
}

func activePrepareLeaseForLease(ctx context.Context, tx *sql.Tx, leaseID string) (PrepareLease, bool, error) {
	row := tx.QueryRowContext(ctx, `
SELECT id, lease_id, COALESCE(attempt_id, ''), agent_id, owner, state,
  expires_at, created_at, updated_at
FROM prepare_leases
WHERE lease_id = ? AND state = 'active'
LIMIT 1`, leaseID)
	lease, err := scanPrepareLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PrepareLease{}, false, nil
	}
	if err != nil {
		return PrepareLease{}, false, err
	}
	return lease, true, nil
}

func scanPrepareLease(row interface{ Scan(...any) error }) (PrepareLease, error) {
	var out PrepareLease
	var expiresAt, createdAt, updatedAt string
	if err := row.Scan(
		&out.ID,
		&out.LeaseID,
		&out.AttemptID,
		&out.AgentID,
		&out.Owner,
		&out.State,
		&expiresAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return PrepareLease{}, err
	}
	var err error
	out.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return PrepareLease{}, err
	}
	out.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return PrepareLease{}, err
	}
	out.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return PrepareLease{}, err
	}
	return out, nil
}

func ensureActiveGuardTx(ctx context.Context, tx *sql.Tx, in activeGuardInput, now string) (bool, error) {
	if in.ResourceKind == "" || in.ResourceRef == "" || in.AttemptID == "" || in.LeaseID == "" {
		return false, errors.New("active guard: resource_kind, resource_ref, attempt_id, and lease_id are required")
	}
	var id, attemptID string
	err := tx.QueryRowContext(ctx, `
SELECT id, attempt_id
FROM active_guards
WHERE resource_kind = ? AND resource_ref = ? AND state = 'active'
LIMIT 1`,
		in.ResourceKind, in.ResourceRef,
	).Scan(&id, &attemptID)
	if err == nil {
		if attemptID != in.AttemptID {
			return false, fmt.Errorf("%w: %s %s guarded by attempt %s", ErrActiveResource, in.ResourceKind, in.ResourceRef, attemptID)
		}
		_, err := tx.ExecContext(ctx, `
UPDATE active_guards
SET lease_id = ?, session_route_id = ?, updated_at = ?
WHERE id = ?`,
			in.LeaseID, nullable(in.SessionRouteID), now, id,
		)
		return false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	guardID, err := ids.New("guard")
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO active_guards (
  id, tenant_id, resource_kind, resource_ref, attempt_id, lease_id,
  session_route_id, state, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, 'active', ?, ?)`,
		guardID, in.ResourceKind, in.ResourceRef, in.AttemptID, in.LeaseID,
		nullable(in.SessionRouteID), now, now,
	)
	return true, err
}

func releaseActiveGuardsTx(ctx context.Context, tx *sql.Tx, attemptID, now string) error {
	_, err := tx.ExecContext(ctx, `
UPDATE active_guards
SET state = 'released', updated_at = ?
WHERE attempt_id = ? AND state = 'active'`,
		now, attemptID,
	)
	return err
}

func releasePrepareLeasesByAttemptTx(ctx context.Context, tx *sql.Tx, attemptID, now string) error {
	_, err := tx.ExecContext(ctx, `
UPDATE prepare_leases
SET state = 'released', updated_at = ?
WHERE attempt_id = ? AND state = 'active'`,
		now, attemptID,
	)
	return err
}

func sessionResumeEventExists(ctx context.Context, tx *sql.Tx, routeID string) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
SELECT id
FROM events
WHERE event_type = 'session.resumed'
  AND aggregate_type = 'session_route'
  AND aggregate_id = ?
LIMIT 1`, routeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func markRouteTerminalForLease(ctx context.Context, tx *sql.Tx, leaseID, state, now string) error {
	routeID, err := routeIDForLease(ctx, tx, leaseID)
	if err != nil {
		return err
	}
	if routeID == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE session_routes SET state = ?, updated_at = ? WHERE id = ?`,
		state, now, routeID,
	)
	return err
}

func activeAttemptForLeaseTx(ctx context.Context, tx *sql.Tx, leaseID string) (Attempt, string, bool, error) {
	row := tx.QueryRowContext(ctx, `
SELECT a.id, a.lease_id, a.cli_backend, a.runtime_kind, COALESCE(a.session_native_id, ''),
  a.start_reason, a.status, COALESCE(a.transcript_ref, ''), a.started_at,
  COALESCE(a.ended_at, ''), COALESCE(l.session_route_id, '')
FROM attempts a
JOIN leases l ON l.id = a.lease_id
WHERE a.lease_id = ? AND a.status IN ('preparing', 'ready_to_launch', 'running', 'waiting')
ORDER BY a.started_at DESC
LIMIT 1`, leaseID)
	var attempt Attempt
	var startedAt, endedAt, routeID string
	err := row.Scan(
		&attempt.ID,
		&attempt.LeaseID,
		&attempt.CLIBackend,
		&attempt.RuntimeKind,
		&attempt.SessionNativeID,
		&attempt.StartReason,
		&attempt.Status,
		&attempt.TranscriptRef,
		&startedAt,
		&endedAt,
		&routeID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, "", false, nil
	}
	if err != nil {
		return Attempt{}, "", false, err
	}
	parsedStart, err := parseTime(startedAt)
	if err != nil {
		return Attempt{}, "", false, err
	}
	attempt.StartedAt = parsedStart
	if endedAt != "" {
		parsedEnd, err := parseTime(endedAt)
		if err != nil {
			return Attempt{}, "", false, err
		}
		attempt.EndedAt = &parsedEnd
	}
	return attempt, routeID, true, nil
}

func activeAttemptStatus(status string) bool {
	switch status {
	case "preparing", "ready_to_launch", "running", "waiting":
		return true
	default:
		return false
	}
}

func resumableAttemptStatus(status string) bool {
	switch status {
	case "preparing", "ready_to_launch", "running", "waiting", "interrupted", "expired":
		return true
	default:
		return false
	}
}

func resumableRouteState(state string) bool {
	switch state {
	case "active", "waiting":
		return true
	default:
		return false
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
