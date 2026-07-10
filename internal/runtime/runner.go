package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"coordplane/internal/bootstrap"
	"coordplane/internal/coordination"
	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/objects"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Runner struct {
	db            *sql.DB
	coordination  *coordination.Service
	cfg           teamconfig.Config
	skills        *skills.Registry
	runtime       RuntimeBackend
	runtimes      map[string]RuntimeBackend
	adapter       CLIAdapter
	objects       *objects.Store
	backendURL    string
	workspaceName string
}

type RunnerConfig struct {
	Store           *store.Store
	Coordination    *coordination.Service
	TeamConfig      teamconfig.Config
	Skills          *skills.Registry
	Runtime         RuntimeBackend
	RuntimeBackends map[string]RuntimeBackend
	Adapter         CLIAdapter
	BackendURL      string
	WorkspaceName   string
}

type startPreflightAdapter interface {
	PreflightStart(context.Context, StartRequest) error
}

func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Store == nil {
		return nil, errors.New("runtime runner: store is nil")
	}
	if cfg.Coordination == nil {
		return nil, errors.New("runtime runner: coordination service is nil")
	}
	if cfg.Adapter == nil {
		return nil, errors.New("runtime runner: CLI adapter is nil")
	}
	if cfg.Runtime == nil && len(cfg.RuntimeBackends) == 0 {
		return nil, errors.New("runtime runner: runtime backend is nil")
	}
	if cfg.BackendURL == "" {
		cfg.BackendURL = "http://coordplane.local"
	}
	if cfg.WorkspaceName == "" {
		cfg.WorkspaceName = "default"
	}
	runner := &Runner{
		db:            cfg.Store.DB(),
		coordination:  cfg.Coordination,
		cfg:           cfg.TeamConfig,
		skills:        cfg.Skills,
		runtime:       cfg.Runtime,
		runtimes:      cloneRuntimeBackends(cfg.RuntimeBackends),
		adapter:       cfg.Adapter,
		objects:       objects.NewStore(cfg.Store),
		backendURL:    cfg.BackendURL,
		workspaceName: cfg.WorkspaceName,
	}
	if err := runner.ReconcileRuntimeCleanup(context.Background()); err != nil {
		return nil, fmt.Errorf("runtime runner: reconcile managed runtime cleanup: %w", err)
	}
	return runner, nil
}

func cloneRuntimeBackends(in map[string]RuntimeBackend) map[string]RuntimeBackend {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]RuntimeBackend, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (r *Runner) runtimeForAgent(agent teamconfig.AgentConfig) (RuntimeBackend, error) {
	if agent.RuntimeProfile != "" && r.runtimes != nil {
		if backend, ok := r.runtimes[agent.RuntimeProfile]; ok {
			if backend == nil {
				return nil, fmt.Errorf("runtime runner: runtime profile %q is nil", agent.RuntimeProfile)
			}
			return backend, nil
		}
	}
	if r.runtime != nil {
		return r.runtime, nil
	}
	return nil, fmt.Errorf("runtime runner: runtime profile %q is not registered", agent.RuntimeProfile)
}

func (r *Runner) StartNext(ctx context.Context, agentID string) (AssignmentSession, error) {
	if agentID == "" {
		return AssignmentSession{}, errors.New("runtime runner: agent id is required")
	}
	agent, ok := r.cfg.Agent(agentID)
	if !ok {
		return AssignmentSession{}, fmt.Errorf("runtime runner: agent %q is not declared in TeamConfig", agentID)
	}
	runtimeBackend, err := r.runtimeForAgent(agent)
	if err != nil {
		return AssignmentSession{}, err
	}
	next, err := r.coordination.AssignmentNext(ctx, coordination.AssignmentNextInput{
		AgentID:  agentID,
		LeaseFor: time.Hour,
	})
	if err != nil {
		return AssignmentSession{}, err
	}
	if next.Idle {
		return AssignmentSession{}, fmt.Errorf("runtime runner: no assignment for agent %q", agentID)
	}
	return r.startClaimed(ctx, agent, runtimeBackend, next)
}

func (r *Runner) StartAssignment(ctx context.Context, agentID, assignmentID string) (AssignmentSession, error) {
	if agentID == "" || assignmentID == "" {
		return AssignmentSession{}, errors.New("runtime runner: agent id and assignment id are required")
	}
	agent, ok := r.cfg.Agent(agentID)
	if !ok {
		return AssignmentSession{}, fmt.Errorf("runtime runner: agent %q is not declared in TeamConfig", agentID)
	}
	runtimeBackend, err := r.runtimeForAgent(agent)
	if err != nil {
		return AssignmentSession{}, err
	}
	next, err := r.coordination.AssignmentClaim(ctx, coordination.AssignmentClaimInput{
		AgentID:      agentID,
		AssignmentID: assignmentID,
		LeaseFor:     time.Hour,
	})
	if err != nil {
		return AssignmentSession{}, err
	}
	return r.startClaimed(ctx, agent, runtimeBackend, next)
}

func (r *Runner) startClaimed(ctx context.Context, agent teamconfig.AgentConfig, runtimeBackend RuntimeBackend, next coordination.AssignmentNextResult) (AssignmentSession, error) {
	agentID := agent.ID
	if existing, ok, err := r.activeSessionForLease(ctx, next.Lease.ID); err != nil {
		return AssignmentSession{}, err
	} else if ok {
		return existing, nil
	}
	attemptID, err := r.createAttempt(ctx, next.Lease.ID, agent.CLIBackend, runtimeBackend.Kind(), "new_assignment")
	if err != nil {
		return AssignmentSession{}, err
	}
	prepareLease, err := r.AcquirePrepareLease(ctx, PrepareLeaseInput{
		LeaseID:   next.Lease.ID,
		AttemptID: attemptID,
		AgentID:   agentID,
		Owner:     "runner:" + runtimeBackend.Name(),
		TTL:       5 * time.Minute,
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := r.failAttempt(cleanupCtx, attemptID, err.Error()); cleanupErr != nil {
			return AssignmentSession{}, errors.Join(err, fmt.Errorf("runtime runner: cleanup failed: %w", cleanupErr))
		}
		return AssignmentSession{}, err
	}
	failPreparedAttempt := func(cause error) (AssignmentSession, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cleanupErrors []error
		if err := r.failAttempt(cleanupCtx, attemptID, cause.Error()); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("fail attempt: %w", err))
		}
		if err := r.ReleasePrepareLease(cleanupCtx, prepareLease.ID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("release prepare lease: %w", err))
		}
		if err := r.finalizeManagedRuntime(cleanupCtx, attemptID, cause.Error()); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		if len(cleanupErrors) > 0 {
			return AssignmentSession{}, errors.Join(cause, errors.Join(cleanupErrors...))
		}
		return AssignmentSession{}, cause
	}

	prepareRequest := PrepareRequest{
		AgentID:        agentID,
		AttemptID:      attemptID,
		AssignmentID:   next.Assignment.ID,
		LeaseID:        next.Lease.ID,
		ContractID:     next.Contract.ID,
		TeamID:         r.cfg.TeamID,
		RuntimeProfile: agent.RuntimeProfile,
		CLIBackend:     agent.CLIBackend,
		BackendURL:     r.backendURL,
		WorkspaceName:  r.workspaceName,
	}
	prepared, err := runtimeBackend.Prepare(ctx, prepareRequest)
	if err != nil {
		return failPreparedAttempt(err)
	}
	workspaceGuardRef := firstNonEmpty(prepared.WorkspaceGuardRef, prepared.Workspace)
	homeGuardRef := firstNonEmpty(prepared.HomeGuardRef, prepared.HomeDir)
	if err := r.protectActiveResource(ctx, activeGuardInput{
		ResourceKind: "workspace",
		ResourceRef:  workspaceGuardRef,
		AttemptID:    attemptID,
		LeaseID:      next.Lease.ID,
	}); err != nil {
		return failPreparedAttempt(err)
	}
	if err := r.protectActiveResource(ctx, activeGuardInput{
		ResourceKind: "home",
		ResourceRef:  homeGuardRef,
		AttemptID:    attemptID,
		LeaseID:      next.Lease.ID,
	}); err != nil {
		return failPreparedAttempt(err)
	}
	if err := r.setAttemptStatus(ctx, attemptID, "ready_to_launch"); err != nil {
		return failPreparedAttempt(err)
	}
	prompt, err := r.bootstrapPrompt(ctx, agent, next)
	if err != nil {
		return failPreparedAttempt(err)
	}
	env := prepared.Env
	if env == nil {
		env, err = r.env(agentID, prepared.RuntimeID, attemptID, next.Assignment.ID, next.Lease.ID, prepared.Workspace, agent.CLIBackend)
		if err != nil {
			return failPreparedAttempt(err)
		}
	}
	if err := ValidateRuntimeEnv(env); err != nil {
		return failPreparedAttempt(err)
	}
	sessionNativeID, err := newNativeSessionID()
	if err != nil {
		return failPreparedAttempt(err)
	}
	startReq := StartRequest{
		AgentID:         agentID,
		AttemptID:       attemptID,
		AssignmentID:    next.Assignment.ID,
		LeaseID:         next.Lease.ID,
		ContractID:      next.Contract.ID,
		SessionNativeID: sessionNativeID,
		RuntimeID:       prepared.RuntimeID,
		CLIBackend:      agent.CLIBackend,
		Workspace:       prepared.Workspace,
		HomeDir:         prepared.HomeDir,
		Env:             env,
		BootstrapPrompt: prompt,
	}
	if preflight, ok := r.adapter.(startPreflightAdapter); ok {
		if err := preflight.PreflightStart(ctx, startReq); err != nil {
			return failPreparedAttempt(err)
		}
	}
	if err := r.recordPreparedRuntime(ctx, runtimeBackend, agent, prepareRequest, prepared, env); err != nil {
		return failPreparedAttempt(err)
	}
	route, err := r.PinSession(ctx, PinInput{
		AttemptID:       attemptID,
		LeaseID:         next.Lease.ID,
		AssignmentID:    next.Assignment.ID,
		AgentID:         agentID,
		RuntimeID:       prepared.RuntimeID,
		CLIBackend:      agent.CLIBackend,
		SessionNativeID: sessionNativeID,
		Workdir:         prepared.Workspace,
		HomeDir:         prepared.HomeDir,
	})
	if err != nil {
		return failPreparedAttempt(err)
	}
	if err := r.attachActiveGuards(ctx, attemptID, route.ID); err != nil {
		return failPreparedAttempt(err)
	}
	if err := r.setAttemptStatus(ctx, attemptID, "running"); err != nil {
		return failPreparedAttempt(err)
	}
	start, err := r.adapter.Start(ctx, startReq)
	if err != nil {
		return failPreparedAttempt(err)
	}
	if start.SessionNativeID != "" && start.SessionNativeID != sessionNativeID {
		return failPreparedAttempt(fmt.Errorf("runtime runner: CLI adapter returned session %q, want planned session %q", start.SessionNativeID, sessionNativeID))
	}
	if start.TranscriptRef != "" {
		if err := r.setTranscriptRef(ctx, attemptID, start.TranscriptRef); err != nil {
			return failPreparedAttempt(err)
		}
	}
	if capabilities, ok := AdapterCapabilitiesForBackend(r.adapter, agent.CLIBackend); ok && capabilities.ReturnsOnProcessExit {
		finalizeErr := r.finalizeOneShotExit(next.Lease.ID, attemptID)
		cleanupErr := r.finalizeManagedRuntime(ctx, attemptID, "one-shot process exited")
		if finalizeErr != nil || cleanupErr != nil {
			return AssignmentSession{}, errors.Join(finalizeErr, cleanupErr)
		}
	}
	if err := r.convergeReleasedLeaseBookkeeping(ctx, next.Lease.ID, attemptID); err != nil {
		return AssignmentSession{}, err
	}
	if err := r.ReleasePrepareLease(ctx, prepareLease.ID); err != nil {
		return AssignmentSession{}, err
	}
	return AssignmentSession{
		AttemptID:     attemptID,
		Route:         route,
		LeaseID:       next.Lease.ID,
		Env:           cloneStringMap(env),
		ContainerName: prepared.ContainerName,
	}, nil
}

func (r *Runner) finalizeOneShotExit(leaseID, attemptID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var leaseState, assignmentState string
	if err := r.db.QueryRowContext(cleanupCtx, `
SELECT l.state, a.state
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
WHERE l.id = ? AND EXISTS (SELECT 1 FROM attempts att WHERE att.id = ? AND att.lease_id = l.id)`,
		leaseID, attemptID,
	).Scan(&leaseState, &assignmentState); err != nil {
		return fmt.Errorf("runtime runner: finalize one-shot exit: %w", err)
	}
	switch {
	case leaseState == "released":
		return r.convergeReleasedLeaseBookkeeping(cleanupCtx, leaseID, attemptID)
	case assignmentState == "waiting":
		_, err := r.FinishSession(cleanupCtx, TerminalReport{
			AttemptID: attemptID,
			Status:    "waiting",
			Summary:   "one-shot provider exited after contract.wait",
		})
		return err
	default:
		cause := NewAgentExitedWithoutTerminalAction("one-shot provider exited successfully without contract.complete or contract.wait")
		failed, err := r.failAttemptIfActive(cleanupCtx, attemptID, cause.Error())
		if err != nil {
			return fmt.Errorf("%w; cleanup failed: %v", cause, err)
		}
		if !failed {
			return r.convergeOneShotAfterSkippedFailure(cleanupCtx, leaseID, attemptID)
		}
		return cause
	}
}

func (r *Runner) convergeOneShotAfterSkippedFailure(ctx context.Context, leaseID, attemptID string) error {
	var leaseState, assignmentState string
	if err := r.db.QueryRowContext(ctx, `
SELECT l.state, a.state
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
WHERE l.id = ? AND EXISTS (SELECT 1 FROM attempts att WHERE att.id = ? AND att.lease_id = l.id)`,
		leaseID, attemptID,
	).Scan(&leaseState, &assignmentState); err != nil {
		return fmt.Errorf("runtime runner: recheck one-shot final state: %w", err)
	}
	switch {
	case leaseState == "released":
		return r.convergeReleasedLeaseBookkeeping(ctx, leaseID, attemptID)
	case assignmentState == "waiting":
		_, err := r.FinishSession(ctx, TerminalReport{
			AttemptID: attemptID,
			Status:    "waiting",
			Summary:   "one-shot provider exited after concurrent contract.wait",
		})
		return err
	default:
		return errors.New("runtime runner: one-shot failure was skipped without a released or waiting canonical state")
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (r *Runner) PinSession(ctx context.Context, in PinInput) (SessionRoute, error) {
	if in.AttemptID == "" || in.LeaseID == "" || in.AgentID == "" || in.SessionNativeID == "" {
		return SessionRoute{}, errors.New("session.pin: attempt_id, lease_id, agent_id, and session_native_id are required")
	}
	var route SessionRoute
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		existing, ok, err := existingPinnedRoute(ctx, tx, in.AttemptID)
		if err != nil {
			return err
		}
		if ok {
			route = existing
			return nil
		}
		assignmentID, err := verifyAttemptLease(ctx, tx, in.AttemptID, in.LeaseID, in.AgentID)
		if err != nil {
			return err
		}
		if in.AssignmentID == "" {
			in.AssignmentID = assignmentID
		}
		routeID, err := ids.New("route")
		if err != nil {
			return err
		}
		route = SessionRoute{
			ID:              routeID,
			AgentID:         in.AgentID,
			RuntimeID:       in.RuntimeID,
			CLIBackend:      in.CLIBackend,
			SessionNativeID: in.SessionNativeID,
			Workdir:         in.Workdir,
			HomeDir:         in.HomeDir,
			AttemptID:       in.AttemptID,
			LeaseID:         in.LeaseID,
			AssignmentID:    in.AssignmentID,
			State:           "active",
		}
		raw, err := json.Marshal(route)
		if err != nil {
			return err
		}
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
INSERT INTO session_routes (
  id, tenant_id, agent_id, runtime_id, cli_backend, session_native_id,
  route_json, state, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, 'active', ?, ?)`,
			route.ID, route.AgentID, route.RuntimeID, route.CLIBackend,
			route.SessionNativeID, string(raw), now, now,
		); err != nil {
			return fmt.Errorf("insert session route: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE attempts SET session_native_id = ? WHERE id = ?`,
			in.SessionNativeID, in.AttemptID,
		); err != nil {
			return fmt.Errorf("pin attempt session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE leases SET runtime_id = ?, session_route_id = ?, updated_at = ? WHERE id = ? AND agent_id = ?`,
			in.RuntimeID, route.ID, now, in.LeaseID, in.AgentID,
		); err != nil {
			return fmt.Errorf("pin lease route: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE assignments SET session_route_id = ?, updated_at = ? WHERE id = ?`,
			route.ID, now, in.AssignmentID,
		); err != nil {
			return fmt.Errorf("pin assignment route: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "session.pinned", "session_route", route.ID, map[string]string{
			"attempt_id": in.AttemptID,
			"lease_id":   in.LeaseID,
		}); err != nil {
			return err
		}
		if capabilities, ok := AdapterCapabilitiesForBackend(r.adapter, in.CLIBackend); ok {
			if _, err := appendAdapterCapabilityEvent(ctx, tx, route.ID, in.CLIBackend, capabilities); err != nil {
				return err
			}
		}
		return nil
	})
	return route, err
}

func (r *Runner) SameTurnSteerCapability(ctx context.Context, routeID string) (string, bool, bool, error) {
	route, err := r.Route(ctx, routeID)
	if err != nil {
		return "", false, false, err
	}
	capabilities, ok := AdapterCapabilitiesForBackend(r.adapter, route.CLIBackend)
	if !ok {
		return route.CLIBackend, false, false, nil
	}
	return route.CLIBackend, capabilities.SupportsSameTurnSteer, true, nil
}

func (r *Runner) SteerMailbox(ctx context.Context, routeID, mailboxID string) error {
	route, err := r.Route(ctx, routeID)
	if err != nil {
		return err
	}
	item, err := r.mailbox(ctx, mailboxID)
	if err != nil {
		return err
	}
	if item.RecipientAgentID != route.AgentID {
		return fmt.Errorf("session.steer: mailbox %s belongs to %s, not route agent %s", mailboxID, item.RecipientAgentID, route.AgentID)
	}
	payload := SteerPayload{
		RouteID:         route.ID,
		AgentID:         route.AgentID,
		SessionNativeID: route.SessionNativeID,
		MailboxID:       item.ID,
		Reason:          item.Reason,
	}
	if err := r.adapter.Steer(ctx, SteerRequest{Route: route, Payload: payload}); err != nil {
		return err
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendEvent(ctx, tx, "session.steer_sent", "mailbox_item", mailboxID, map[string]string{
			"route_id": routeID,
		})
		return err
	})
}

func (r *Runner) FinishSession(ctx context.Context, report TerminalReport) (Attempt, error) {
	if report.AttemptID == "" {
		return Attempt{}, errors.New("session.finish: attempt_id is required")
	}
	if !terminalStatus(report.Status) {
		return Attempt{}, fmt.Errorf("session.finish: status %q is not terminal", report.Status)
	}
	var attempt Attempt
	var alreadyFinished bool
	adapterReport := report
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		current, err := attemptByIDTx(ctx, tx, report.AttemptID)
		if err != nil {
			return err
		}
		if current.EndedAt != nil && terminalStatus(current.Status) {
			attempt = current
			alreadyFinished = true
			return nil
		}
		now := formatTime(time.Now())
		transcriptRef := report.TranscriptRef
		if report.TranscriptContent != "" {
			transcript, err := r.objects.PutTranscriptTx(ctx, tx, objects.PutTranscriptInput{
				AttemptID:   current.ID,
				Content:     []byte(report.TranscriptContent),
				ContentType: "text/plain; charset=utf-8",
			})
			if err != nil {
				return err
			}
			transcriptRef = transcript.ObjectRef
			adapterReport.TranscriptRef = transcript.ObjectRef
			adapterReport.TranscriptContent = ""
		}
		if transcriptRef == "" {
			transcriptRef = current.TranscriptRef
			adapterReport.TranscriptRef = transcriptRef
			adapterReport.TranscriptContent = ""
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE attempts SET status = ?, transcript_ref = ?, ended_at = ? WHERE id = ?`,
			report.Status, transcriptRef, now, report.AttemptID,
		); err != nil {
			return fmt.Errorf("finish attempt: %w", err)
		}
		switch report.Status {
		case "completed":
			if _, err := tx.ExecContext(ctx, `
UPDATE leases SET state = 'released', updated_at = ? WHERE id = ? AND state = 'active'`,
				now, current.LeaseID,
			); err != nil {
				return fmt.Errorf("release completed lease: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE assignments SET state = 'returned', updated_at = ?
WHERE id = (SELECT assignment_id FROM leases WHERE id = ?) AND state = 'claimed'`,
				now, current.LeaseID,
			); err != nil {
				return fmt.Errorf("return completed assignment: %w", err)
			}
			if err := markRouteTerminalForLease(ctx, tx, current.LeaseID, report.Status, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE runtime_tokens SET state = 'revoked', updated_at = ?
WHERE attempt_id = ? AND state = 'active'`,
				now, current.ID,
			); err != nil {
				return fmt.Errorf("revoke completed runtime tokens: %w", err)
			}
			if err := releaseActiveGuardsTx(ctx, tx, current.ID, now); err != nil {
				return err
			}
		case "waiting":
			routeID, _ := routeIDForLease(ctx, tx, current.LeaseID)
			if routeID != "" {
				if _, err := tx.ExecContext(ctx, `
UPDATE session_routes SET state = 'waiting', updated_at = ? WHERE id = ?`,
					now, routeID,
				); err != nil {
					return fmt.Errorf("mark route waiting: %w", err)
				}
			}
		case "failed", "interrupted", "expired":
			if err := markRouteTerminalForLease(ctx, tx, current.LeaseID, report.Status, now); err != nil {
				return err
			}
			if err := releaseActiveGuardsTx(ctx, tx, current.ID, now); err != nil {
				return err
			}
		}
		if err := releasePrepareLeasesByAttemptTx(ctx, tx, current.ID, now); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, "session.finished", "attempt", report.AttemptID, map[string]string{
			"status": report.Status,
		})
		if err != nil {
			return err
		}
		attempt, err = attemptByIDTx(ctx, tx, report.AttemptID)
		return err
	})
	if err != nil {
		return Attempt{}, err
	}
	var finalizationErrors []error
	if !alreadyFinished {
		if err := r.adapter.Finish(ctx, adapterReport); err != nil {
			finalizationErrors = append(finalizationErrors, err)
		}
	}
	if report.Status != "waiting" {
		if err := r.finalizeManagedRuntime(ctx, report.AttemptID, "session "+report.Status); err != nil {
			finalizationErrors = append(finalizationErrors, err)
		}
	}
	return attempt, errors.Join(finalizationErrors...)
}

func (r *Runner) finalizeManagedRuntime(ctx context.Context, attemptID, reason string) error {
	var profile string
	err := r.db.QueryRowContext(context.WithoutCancel(ctx), `
SELECT runtime_profile
FROM runtime_instances
WHERE attempt_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`, attemptID).Scan(&profile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup managed runtime finalizer: %w", err)
	}
	var backend RuntimeBackend
	if r.runtimes != nil {
		backend = r.runtimes[profile]
	}
	if backend == nil && r.runtime != nil && (profile == "" || r.runtime.Name() == profile) {
		backend = r.runtime
	}
	finalizer, ok := backend.(RuntimeFinalizer)
	if !ok {
		return nil
	}
	if err := finalizer.FinalizeRuntime(ctx, attemptID, reason); err != nil {
		return fmt.Errorf("finalize managed runtime: %w", err)
	}
	return nil
}

func (r *Runner) ReconcileRuntimeCleanup(ctx context.Context) error {
	seen := make(map[RuntimeBackend]bool)
	var reconcileErrors []error
	for _, backend := range r.runtimes {
		if backend == nil || seen[backend] {
			continue
		}
		seen[backend] = true
		if reconciler, ok := backend.(RuntimeFinalizer); ok {
			if err := reconciler.ReconcileRuntimeCleanup(ctx); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("runtime %s: %w", backend.Name(), err))
			}
		}
	}
	if r.runtime != nil && !seen[r.runtime] {
		if reconciler, ok := r.runtime.(RuntimeFinalizer); ok {
			if err := reconciler.ReconcileRuntimeCleanup(ctx); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("runtime %s: %w", r.runtime.Name(), err))
			}
		}
	}
	return errors.Join(reconcileErrors...)
}

func (r *Runner) Attempt(ctx context.Context, attemptID string) (Attempt, error) {
	var attempt Attempt
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		attempt, err = attemptByIDTx(ctx, tx, attemptID)
		return err
	})
	return attempt, err
}

func (r *Runner) Route(ctx context.Context, routeID string) (SessionRoute, error) {
	var route SessionRoute
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		route, err = routeByIDTx(ctx, tx, routeID)
		return err
	})
	return route, err
}

func (r *Runner) createAttempt(ctx context.Context, leaseID, cliBackend, runtimeKind, reason string) (string, error) {
	attemptID, err := ids.New("att")
	if err != nil {
		return "", err
	}
	now := formatTime(time.Now())
	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO attempts (
  id, tenant_id, lease_id, cli_backend, runtime_kind, start_reason,
  status, started_at
) VALUES (?, 'default', ?, ?, ?, ?, 'preparing', ?)`,
			attemptID, leaseID, cliBackend, runtimeKind, reason, now,
		); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
		_, err := appendEvent(ctx, tx, "session.attempt_created", "attempt", attemptID, map[string]string{
			"lease_id": leaseID,
		})
		return err
	})
	return attemptID, err
}

func (r *Runner) convergeReleasedLeaseBookkeeping(ctx context.Context, leaseID, attemptID string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		return convergeReleasedLeaseBookkeepingTx(ctx, tx, leaseID, attemptID, formatTime(time.Now()))
	})
}

func convergeReleasedLeaseBookkeepingTx(ctx context.Context, tx *sql.Tx, leaseID, attemptID, now string) error {
	var leaseState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, leaseID).Scan(&leaseState); err != nil {
		return fmt.Errorf("lookup released lease bookkeeping scope: %w", err)
	}
	if leaseState != "released" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE attempts
SET status = 'completed', ended_at = COALESCE(ended_at, ?)
WHERE id = ? AND lease_id = ? AND status IN ('preparing', 'ready_to_launch', 'running')`,
		now, attemptID, leaseID,
	); err != nil {
		return fmt.Errorf("complete released lease attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE session_routes
SET state = 'completed', updated_at = ?
WHERE id = (SELECT session_route_id FROM leases WHERE id = ?)
  AND state = 'active'`,
		now, leaseID,
	); err != nil {
		return fmt.Errorf("complete released lease route: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runtime_tokens
SET state = 'revoked', updated_at = ?
WHERE attempt_id = ? AND lease_id = ? AND state = 'active'`,
		now, attemptID, leaseID,
	); err != nil {
		return fmt.Errorf("revoke released lease runtime tokens: %w", err)
	}
	if err := releaseActiveGuardsTx(ctx, tx, attemptID, now); err != nil {
		return err
	}
	return nil
}

func (r *Runner) bootstrapPrompt(ctx context.Context, agent teamconfig.AgentConfig, next coordination.AssignmentNextResult) (string, error) {
	var summaries []skills.Summary
	var err error
	if r.skills != nil {
		summaries, err = r.skills.ListForAgent(ctx, r.cfg, agent.ID)
		if err != nil {
			return "", err
		}
	}
	assignment := fmt.Sprintf(
		"assignment_id=%s\ncontract_id=%s\ntitle=%s\nobjective=%s\nlease_id=%s",
		next.Assignment.ID,
		next.Contract.ID,
		next.Contract.Title,
		next.Contract.Objective,
		next.Lease.ID,
	)
	return bootstrap.Compose(bootstrap.Context{
		Agent:             agent,
		SkillSummaries:    summaries,
		AssignmentSummary: assignment,
	}), nil
}

func (r *Runner) env(agentID, runtimeID, attemptID, assignmentID, leaseID, workspace, cliBackend string) (map[string]string, error) {
	return BuildRuntimeEnv(EnvironmentInput{
		BackendURL:    r.backendURL,
		AgentID:       agentID,
		RuntimeID:     runtimeID,
		AttemptID:     attemptID,
		AssignmentID:  assignmentID,
		LeaseID:       leaseID,
		Workspace:     workspace,
		CLIBackend:    cliBackend,
		TeamID:        r.cfg.TeamID,
		WorkspaceName: r.workspaceName,
	})
}

func (r *Runner) recordPreparedRuntime(ctx context.Context, backend RuntimeBackend, agent teamconfig.AgentConfig, req PrepareRequest, prepared PreparedRuntime, env map[string]string) error {
	if prepared.RuntimeID == "" {
		return errors.New("runtime runner: prepared runtime id is required")
	}
	if env == nil {
		return errors.New("runtime runner: prepared runtime env is required")
	}
	var existing int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_instances WHERE runtime_id = ?`, prepared.RuntimeID).Scan(&existing); err != nil {
		return fmt.Errorf("lookup runtime instance: %w", err)
	}
	if existing > 0 {
		return nil
	}
	instanceID, err := ids.New("rti")
	if err != nil {
		return err
	}
	checks := prepared.Checks
	if len(checks) == 0 {
		checks = map[string]bool{"runtime_ready": true}
	}
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	envKeysJSON, err := json.Marshal(RuntimeEnvKeys(env))
	if err != nil {
		return err
	}
	runtimeKind := firstNonEmpty(prepared.Kind, backend.Kind())
	runtimeProfile := firstNonEmpty(agent.RuntimeProfile, backend.Name(), runtimeKind)
	now := formatTime(time.Now())
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_instances (
  id, tenant_id, runtime_id, runtime_profile, runtime_kind, agent_id,
  attempt_id, lease_id, container_id, container_name, state,
  workspace_path, home_path, host_workspace_ref, host_home_ref,
  checks_json, env_keys_json, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?, ?, ?, ?, ?, ?, ?)`,
			instanceID, prepared.RuntimeID, runtimeProfile, runtimeKind,
			req.AgentID, req.AttemptID, req.LeaseID, prepared.ContainerID,
			prepared.ContainerName, prepared.Workspace, prepared.HomeDir,
			firstNonEmpty(prepared.WorkspaceGuardRef, prepared.Workspace),
			firstNonEmpty(prepared.HomeGuardRef, prepared.HomeDir),
			string(checksJSON), string(envKeysJSON), now, now,
		); err != nil {
			return fmt.Errorf("insert runtime instance: %w", err)
		}
		if err := recordRuntimeTokenTx(ctx, tx, runtimeTokenInput{
			AgentID:   req.AgentID,
			RuntimeID: prepared.RuntimeID,
			AttemptID: req.AttemptID,
			LeaseID:   req.LeaseID,
			Token:     env["COORDPLANE_TOKEN"],
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "runtime.prepare_started", "runtime_instance", prepared.RuntimeID, map[string]any{
			"attempt_id": req.AttemptID,
			"agent_id":   req.AgentID,
			"kind":       runtimeKind,
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "runtime.env_injected", "runtime_instance", prepared.RuntimeID, map[string]any{
			"env_keys": RuntimeEnvKeys(env),
		}); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, "runtime.ready", "runtime_instance", prepared.RuntimeID, map[string]any{
			"attempt_id": req.AttemptID,
			"agent_id":   req.AgentID,
		})
		return err
	})
}

func (r *Runner) setAttemptStatus(ctx context.Context, attemptID, status string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE attempts SET status = ? WHERE id = ?`, status, attemptID)
		return err
	})
}

func (r *Runner) setTranscriptRef(ctx context.Context, attemptID, transcriptRef string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE attempts SET transcript_ref = ? WHERE id = ?`, transcriptRef, attemptID)
		return err
	})
}

func (r *Runner) failAttempt(ctx context.Context, attemptID, reason string) error {
	_, err := r.failAttemptIfActive(ctx, attemptID, reason)
	return err
}

func (r *Runner) failAttemptIfActive(ctx context.Context, attemptID, reason string) (bool, error) {
	failed := false
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		current, err := attemptByIDTx(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		now := formatTime(time.Now())
		var leaseState, assignmentState string
		if err := tx.QueryRowContext(ctx, `
SELECT l.state, a.state
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
WHERE l.id = ?`, current.LeaseID).Scan(&leaseState, &assignmentState); err != nil {
			return err
		}
		if current.EndedAt != nil && terminalStatus(current.Status) {
			return nil
		}
		if leaseState != "active" || assignmentState != "claimed" {
			if leaseState == "released" {
				return convergeReleasedLeaseBookkeepingTx(ctx, tx, current.LeaseID, attemptID, now)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
		UPDATE attempts SET status = 'failed', transcript_ref = COALESCE(NULLIF(transcript_ref, ''), ?), ended_at = ? WHERE id = ?`,
			reason, now, attemptID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE leases SET state = 'released', updated_at = ? WHERE id = ? AND state = 'active'`,
			now, current.LeaseID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE assignments SET state = 'queued', session_route_id = NULL, updated_at = ?
WHERE id = (SELECT assignment_id FROM leases WHERE id = ?) AND state = 'claimed'`,
			now, current.LeaseID,
		); err != nil {
			return err
		}
		if err := markRouteTerminalForLease(ctx, tx, current.LeaseID, "failed", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE leases SET runtime_id = NULL, session_route_id = NULL, updated_at = ? WHERE id = ?`,
			now, current.LeaseID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE runtime_tokens SET state = 'revoked', updated_at = ? WHERE attempt_id = ? AND state = 'active'`,
			now, attemptID,
		); err != nil {
			return err
		}
		if err := releaseActiveGuardsTx(ctx, tx, attemptID, now); err != nil {
			return err
		}
		if err := releasePrepareLeasesByAttemptTx(ctx, tx, attemptID, now); err != nil {
			return err
		}
		if _, err = appendEvent(ctx, tx, "session.failed", "attempt", attemptID, map[string]string{"reason": reason}); err != nil {
			return err
		}
		failed = true
		return nil
	})
	return failed, err
}

func (r *Runner) mailbox(ctx context.Context, mailboxID string) (coordination.MailboxItem, error) {
	var item coordination.MailboxItem
	err := r.db.QueryRowContext(ctx, `
SELECT id, COALESCE(recipient_agent_id, ''), reason, COALESCE(thread_id, ''),
  COALESCE(message_id, ''), COALESCE(contract_id, ''), COALESCE(session_route_id, ''),
  state, COALESCE(followup_ref, '')
FROM mailbox_items
WHERE id = ?`, mailboxID).Scan(
		&item.ID, &item.RecipientAgentID, &item.Reason, &item.ThreadID,
		&item.MessageID, &item.ContractID, &item.SessionRouteID, &item.State, &item.FollowupRef,
	)
	return item, err
}

func existingPinnedRoute(ctx context.Context, tx *sql.Tx, attemptID string) (SessionRoute, bool, error) {
	row := tx.QueryRowContext(ctx, `
SELECT sr.id, sr.agent_id, sr.runtime_id, sr.cli_backend, sr.session_native_id, sr.route_json, sr.state
FROM session_routes sr
JOIN leases l ON l.session_route_id = sr.id
JOIN attempts a ON a.lease_id = l.id
WHERE a.id = ?
LIMIT 1`, attemptID)
	route, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRoute{}, false, nil
	}
	if err != nil {
		return SessionRoute{}, false, err
	}
	return route, true, nil
}

func verifyAttemptLease(ctx context.Context, tx *sql.Tx, attemptID, leaseID, agentID string) (string, error) {
	var assignmentID string
	err := tx.QueryRowContext(ctx, `
SELECT l.assignment_id
FROM attempts a
JOIN leases l ON l.id = a.lease_id
WHERE a.id = ? AND a.lease_id = ? AND l.id = ? AND l.agent_id = ?`,
		attemptID, leaseID, leaseID, agentID,
	).Scan(&assignmentID)
	if err != nil {
		return "", fmt.Errorf("verify attempt lease: %w", err)
	}
	return assignmentID, nil
}

func routeByIDTx(ctx context.Context, tx *sql.Tx, routeID string) (SessionRoute, error) {
	row := tx.QueryRowContext(ctx, `
SELECT id, agent_id, runtime_id, cli_backend, session_native_id, route_json, state
FROM session_routes
WHERE id = ?`, routeID)
	return scanRoute(row)
}

func scanRoute(row interface{ Scan(...any) error }) (SessionRoute, error) {
	var route SessionRoute
	var raw string
	if err := row.Scan(
		&route.ID,
		&route.AgentID,
		&route.RuntimeID,
		&route.CLIBackend,
		&route.SessionNativeID,
		&raw,
		&route.State,
	); err != nil {
		return SessionRoute{}, err
	}
	if raw != "" {
		var persisted SessionRoute
		if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
			return SessionRoute{}, err
		}
		persisted.ID = route.ID
		persisted.AgentID = route.AgentID
		persisted.RuntimeID = route.RuntimeID
		persisted.CLIBackend = route.CLIBackend
		persisted.SessionNativeID = route.SessionNativeID
		persisted.State = route.State
		return persisted, nil
	}
	return route, nil
}

func attemptByIDTx(ctx context.Context, tx *sql.Tx, attemptID string) (Attempt, error) {
	row := tx.QueryRowContext(ctx, `
SELECT id, lease_id, cli_backend, runtime_kind, COALESCE(session_native_id, ''),
  start_reason, status, COALESCE(transcript_ref, ''), started_at, COALESCE(ended_at, '')
FROM attempts
WHERE id = ?`, attemptID)
	var attempt Attempt
	var startedAt, endedAt string
	if err := row.Scan(
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
	); err != nil {
		return Attempt{}, err
	}
	parsedStart, err := parseTime(startedAt)
	if err != nil {
		return Attempt{}, err
	}
	attempt.StartedAt = parsedStart
	if endedAt != "" {
		parsedEnd, err := parseTime(endedAt)
		if err != nil {
			return Attempt{}, err
		}
		attempt.EndedAt = &parsedEnd
	}
	return attempt, nil
}

func routeIDForLease(ctx context.Context, tx *sql.Tx, leaseID string) (string, error) {
	var routeID string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(session_route_id, '') FROM leases WHERE id = ?`, leaseID).Scan(&routeID)
	return routeID, err
}

func terminalStatus(status string) bool {
	switch status {
	case "completed", "waiting", "failed", "interrupted", "expired":
		return true
	default:
		return false
	}
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType, aggregateType, aggregateID string, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return store.AppendEventTx(ctx, tx, events.Event{
		TenantID:      "default",
		SubjectKind:   "system",
		SubjectID:     "runtime",
		Type:          eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		PayloadJSON:   raw,
	})
}

func appendAdapterCapabilityEvent(ctx context.Context, tx *sql.Tx, routeID, cliBackend string, capabilities CLIAdapterCapabilities) (string, error) {
	return appendEvent(ctx, tx, "cli.adapter_capabilities", "session_route", routeID, map[string]any{
		"cli_backend":               cliBackend,
		"supports_same_turn_steer":  capabilities.SupportsSameTurnSteer,
		"capability_schema_version": 1,
		"capability_source":         "adapter",
	})
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
