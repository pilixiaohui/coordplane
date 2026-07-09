package operator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"coordplane/internal/coordination"
	"coordplane/internal/ids"
	cpruntime "coordplane/internal/runtime"
)

const (
	CapabilityNameTaskWait = "operator.task.wait"

	defaultWaitTimeout      = 30 * time.Second
	defaultWaitPollInterval = 250 * time.Millisecond
)

type WaitTaskInput struct {
	TimeoutSeconds     int `json:"timeout_seconds,omitempty"`
	TimeoutMillis      int `json:"timeout_millis,omitempty"`
	PollIntervalMillis int `json:"poll_interval_millis,omitempty"`
}

type WaitTaskResult struct {
	TaskRunID      string       `json:"task_run_id"`
	Status         string       `json:"status"`
	FailureSummary string       `json:"failure_summary,omitempty"`
	FailureClass   string       `json:"failure_class,omitempty"`
	TerminalReason string       `json:"terminal_reason,omitempty"`
	Evidence       TaskEvidence `json:"evidence"`
}

type TaskEvidence struct {
	SchemaVersion         string                         `json:"schema_version"`
	TaskRunID             string                         `json:"task_run_id"`
	Status                string                         `json:"status"`
	FailureSummary        string                         `json:"failure_summary,omitempty"`
	FailureClass          string                         `json:"failure_class,omitempty"`
	TerminalReason        string                         `json:"terminal_reason,omitempty"`
	Audience              string                         `json:"audience"`
	Operator              TaskEvidenceOperator           `json:"operator"`
	AgentFacing           TaskEvidenceAgentFacing        `json:"agent_facing"`
	Redaction             TaskEvidenceRedaction          `json:"redaction"`
	StartedSessions       []EvidenceStartedSession       `json:"started_sessions"`
	ContractLineage       []EvidenceContract             `json:"contract_lineage"`
	EvidenceRefs          []EvidenceRef                  `json:"evidence_refs,omitempty"`
	ValidationAssessments []EvidenceValidationAssessment `json:"validation_assessments,omitempty"`
	CapabilityCallCounts  map[string]int64               `json:"capability_call_counts"`
	CommunicationCounts   EvidenceCommunicationCounts    `json:"communication_counts"`
	InspectSummary        map[string]int64               `json:"inspect_summary"`
	Terminal              EvidenceTerminal               `json:"terminal"`
}

type TaskEvidenceOperator struct {
	TaskRun EvidenceTaskRun `json:"task_run"`
	Root    EvidenceRoot    `json:"root"`
}

type TaskEvidenceAgentFacing struct {
	RootContractID   string               `json:"root_contract_id"`
	RootAssignmentID string               `json:"root_assignment_id"`
	RootEnvelopeID   string               `json:"root_envelope_id"`
	RootMailboxID    string               `json:"root_mailbox_id"`
	ContractIDs      []string             `json:"contract_ids"`
	SessionRefs      []EvidenceSessionRef `json:"session_refs"`
}

type TaskEvidenceRedaction struct {
	OperatorOnlyFieldsRedacted bool     `json:"operator_only_fields_redacted"`
	TokensIncluded             bool     `json:"tokens_included"`
	HostPathsIncluded          bool     `json:"host_paths_included"`
	RuntimeRootsIncluded       bool     `json:"runtime_roots_included"`
	RedactedFields             []string `json:"redacted_fields"`
}

type EvidenceTaskRun struct {
	ID               string `json:"id"`
	RunLabel         string `json:"run_label,omitempty"`
	TeamID           string `json:"team_id"`
	TeamVersion      int    `json:"team_version"`
	RootContractID   string `json:"root_contract_id"`
	RootAssignmentID string `json:"root_assignment_id"`
	RootEnvelopeID   string `json:"root_envelope_id"`
	RootMailboxID    string `json:"root_mailbox_id"`
	CreatedAt        string `json:"created_at"`
}

type EvidenceRoot struct {
	ContractID      string `json:"contract_id"`
	ContractStatus  string `json:"contract_status"`
	TargetAgentID   string `json:"target_agent_id"`
	AssignmentID    string `json:"assignment_id"`
	AssignmentState string `json:"assignment_state"`
	SessionRouteID  string `json:"session_route_id,omitempty"`
	EnvelopeID      string `json:"envelope_id"`
	MailboxID       string `json:"mailbox_id"`
	MailboxState    string `json:"mailbox_state"`
	MailboxFollowup string `json:"mailbox_followup,omitempty"`
}

type EvidenceContract struct {
	ID               string `json:"id"`
	IssuerContractID string `json:"issuer_contract_id,omitempty"`
	IssuerAgentID    string `json:"issuer_agent_id,omitempty"`
	TargetKind       string `json:"target_kind"`
	TargetID         string `json:"target_id"`
	Status           string `json:"status"`
	Depth            int    `json:"depth"`
}

type EvidenceStartedSession struct {
	ContractID     string `json:"contract_id"`
	AssignmentID   string `json:"assignment_id"`
	AgentID        string `json:"agent_id"`
	LeaseID        string `json:"lease_id"`
	LeaseState     string `json:"lease_state"`
	AttemptID      string `json:"attempt_id"`
	AttemptStatus  string `json:"attempt_status"`
	SessionRouteID string `json:"session_route_id,omitempty"`
	RouteState     string `json:"route_state,omitempty"`
	RuntimeID      string `json:"runtime_id,omitempty"`
	RuntimeState   string `json:"runtime_state,omitempty"`
	CLIBackend     string `json:"cli_backend,omitempty"`
}

type EvidenceSessionRef struct {
	AssignmentID   string `json:"assignment_id"`
	AgentID        string `json:"agent_id"`
	LeaseID        string `json:"lease_id"`
	AttemptID      string `json:"attempt_id"`
	SessionRouteID string `json:"session_route_id,omitempty"`
	RuntimeID      string `json:"runtime_id,omitempty"`
}

type EvidenceRef struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ContractID string `json:"contract_id"`
	ProducedBy string `json:"produced_by"`
	Verdict    string `json:"verdict,omitempty"`
	ContentRef string `json:"content_ref,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type EvidenceValidationAssessment struct {
	ID                 string `json:"id"`
	EvidenceID         string `json:"evidence_id"`
	VerifierAgentID    string `json:"verifier_agent_id"`
	ContractID         string `json:"contract_id"`
	AssessedContractID string `json:"assessed_contract_id"`
	Verdict            string `json:"verdict"`
	CreatedAt          string `json:"created_at"`
}

type EvidenceCommunicationCounts struct {
	Envelopes        int64            `json:"envelopes"`
	MailboxItems     int64            `json:"mailbox_items"`
	DeliveryAttempts int64            `json:"delivery_attempts"`
	EnvelopesByKind  map[string]int64 `json:"envelopes_by_kind"`
	MailboxByState   map[string]int64 `json:"mailbox_by_state"`
}

type EvidenceTerminal struct {
	Status                 string `json:"status"`
	FailureSummary         string `json:"failure_summary,omitempty"`
	FailureClass           string `json:"failure_class,omitempty"`
	TerminalReason         string `json:"terminal_reason,omitempty"`
	RootContractStatus     string `json:"root_contract_status"`
	ReportCount            int64  `json:"report_count"`
	ValidationPassCount    int64  `json:"validation_pass_count"`
	ValidationFailureCount int64  `json:"validation_failure_count"`
	QueuedAssignmentCount  int64  `json:"queued_assignment_count"`
	ActiveAssignmentCount  int64  `json:"active_assignment_count"`
	ActiveLeaseCount       int64  `json:"active_lease_count"`
}

func (s *Service) WaitTask(ctx context.Context, subject Subject, taskRunID string, in WaitTaskInput) (WaitTaskResult, error) {
	subject = normalizeSubject(subject)
	if subject.Kind != "operator" && subject.Kind != "debug" {
		return WaitTaskResult{}, reject("OPERATOR_SUBJECT_REQUIRED", "operator task wait requires an operator or debug subject")
	}
	if s.runner == nil {
		return WaitTaskResult{}, errors.New("operator task wait: runner is required")
	}
	taskRunID = strings.TrimSpace(taskRunID)
	if taskRunID == "" {
		return WaitTaskResult{}, reject("TASK_RUN_REQUIRED", "operator task wait requires task_run_id")
	}
	root, err := s.rootTaskRun(ctx, taskRunID)
	if err != nil {
		return WaitTaskResult{}, err
	}
	if err := s.appendWaitAudit(ctx, subject, root); err != nil {
		return WaitTaskResult{}, err
	}
	timeout := normalizedWaitTimeout(in)
	poll := normalizedWaitPollInterval(in)
	deadline := time.Now().Add(timeout)
	for {
		if err := s.dispatchQueuedLineage(ctx, root.RootContractID); err != nil {
			if _, ok := cpruntime.ErrorFailureClass(err); !ok {
				return WaitTaskResult{}, err
			}
		}
		evidence, err := s.buildTaskEvidence(ctx, root.TaskRunID)
		if err != nil {
			return WaitTaskResult{}, err
		}
		if evidence.Terminal.Status != "running" {
			return waitResultFromEvidence(evidence), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			evidence.Terminal.Status = "timeout"
			evidence.Terminal.FailureSummary = "wait timed out before the root task reached a terminal evidence state"
			evidence.Terminal.TerminalReason = "WAIT_TIMEOUT"
			evidence.Status = evidence.Terminal.Status
			evidence.FailureSummary = evidence.Terminal.FailureSummary
			evidence.TerminalReason = evidence.Terminal.TerminalReason
			return waitResultFromEvidence(evidence), nil
		}
		if remaining < poll {
			poll = remaining
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return WaitTaskResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) Evidence(ctx context.Context, subject Subject, taskRunID string) (TaskEvidence, error) {
	subject = normalizeSubject(subject)
	if subject.Kind != "operator" && subject.Kind != "debug" {
		return TaskEvidence{}, reject("OPERATOR_SUBJECT_REQUIRED", "operator task evidence requires an operator or debug subject")
	}
	taskRunID = strings.TrimSpace(taskRunID)
	if taskRunID == "" {
		return TaskEvidence{}, reject("TASK_RUN_REQUIRED", "operator task evidence requires task_run_id")
	}
	return s.buildTaskEvidence(ctx, taskRunID)
}

func waitResultFromEvidence(evidence TaskEvidence) WaitTaskResult {
	return WaitTaskResult{
		TaskRunID:      evidence.TaskRunID,
		Status:         evidence.Terminal.Status,
		FailureSummary: evidence.Terminal.FailureSummary,
		FailureClass:   evidence.Terminal.FailureClass,
		TerminalReason: evidence.Terminal.TerminalReason,
		Evidence:       evidence,
	}
}

func normalizedWaitTimeout(in WaitTaskInput) time.Duration {
	switch {
	case in.TimeoutMillis > 0:
		return time.Duration(in.TimeoutMillis) * time.Millisecond
	case in.TimeoutSeconds > 0:
		return time.Duration(in.TimeoutSeconds) * time.Second
	default:
		return defaultWaitTimeout
	}
}

func normalizedWaitPollInterval(in WaitTaskInput) time.Duration {
	if in.PollIntervalMillis <= 0 {
		return defaultWaitPollInterval
	}
	return time.Duration(in.PollIntervalMillis) * time.Millisecond
}

func (s *Service) appendWaitAudit(ctx context.Context, subject Subject, root rootTaskRun) error {
	return s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := ids.New("capcall")
		if err != nil {
			return err
		}
		scopeJSON, err := json.Marshal(map[string]any{
			"root_contract_id":   root.RootContractID,
			"root_assignment_id": root.RootAssignmentID,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO capability_calls (
  id, tenant_id, trace_id, capability_name, subject_kind, subject_id,
  scope_json, status, idempotency_key, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, 'accepted', NULL, ?)`,
			id, root.TaskRunID, CapabilityNameTaskWait, subject.Kind, subject.ID,
			string(scopeJSON), formatTime(time.Now()),
		)
		if err != nil {
			return fmt.Errorf("insert operator wait capability audit: %w", err)
		}
		return nil
	})
}

func (s *Service) dispatchQueuedLineage(ctx context.Context, rootContractID string) error {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE lineage(id) AS (
  SELECT ?
  UNION ALL
  SELECT c.id
  FROM work_contracts c
  JOIN lineage l ON c.issuer_contract_id = l.id
)
SELECT a.id, COALESCE(a.assignee_agent_id, ''), COALESCE(a.assignee_role, '')
FROM assignments a
JOIN lineage l ON l.id = a.contract_id
WHERE a.state = 'queued'
ORDER BY a.created_at ASC, a.id ASC`, rootContractID)
	if err != nil {
		return fmt.Errorf("operator task dispatch: list queued assignments: %w", err)
	}
	defer rows.Close()
	type queuedAssignment struct {
		assignmentID string
		agentID      string
		role         string
	}
	var queued []queuedAssignment
	for rows.Next() {
		var item queuedAssignment
		if err := rows.Scan(&item.assignmentID, &item.agentID, &item.role); err != nil {
			return err
		}
		queued = append(queued, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range queued {
		agentID := firstNonEmpty(item.agentID, item.role)
		if agentID == "" {
			continue
		}
		if _, ok := s.teamConfig.Agent(agentID); !ok {
			continue
		}
		if _, err := s.runner.StartAssignment(ctx, agentID, item.assignmentID); err != nil {
			if errors.Is(err, coordination.ErrAssignmentBusy) ||
				errors.Is(err, coordination.ErrAssignmentNotFound) ||
				errors.Is(err, coordination.ErrAssignmentNotClaimable) {
				continue
			}
			if _, ok := cpruntime.ErrorFailureClass(err); ok {
				continue
			}
			return fmt.Errorf("operator task dispatch: start assignment %s for %s: %w", item.assignmentID, agentID, err)
		}
	}
	return nil
}

func (s *Service) buildTaskEvidence(ctx context.Context, taskRunID string) (TaskEvidence, error) {
	run, err := s.evidenceTaskRun(ctx, taskRunID)
	if err != nil {
		return TaskEvidence{}, err
	}
	root, err := s.evidenceRoot(ctx, run)
	if err != nil {
		return TaskEvidence{}, err
	}
	lineage, err := s.evidenceContractLineage(ctx, run.RootContractID)
	if err != nil {
		return TaskEvidence{}, err
	}
	contractIDs := contractLineageIDs(lineage)
	started, err := s.evidenceStartedSessions(ctx, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	refs, err := s.evidenceRefs(ctx, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	validations, err := s.evidenceValidations(ctx, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	capabilityCounts, err := s.evidenceCapabilityCounts(ctx, taskRunID, run.RootContractID, run.RootAssignmentID, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	communicationCounts, err := s.evidenceCommunicationCounts(ctx, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	inspectSummary, err := s.evidenceInspectSummary(ctx)
	if err != nil {
		return TaskEvidence{}, err
	}
	terminal, err := s.evidenceTerminal(ctx, run.RootContractID, contractIDs)
	if err != nil {
		return TaskEvidence{}, err
	}
	evidence := TaskEvidence{
		SchemaVersion:  "operator.task.evidence.v1",
		TaskRunID:      run.ID,
		Status:         terminal.Status,
		FailureSummary: terminal.FailureSummary,
		FailureClass:   terminal.FailureClass,
		TerminalReason: terminal.TerminalReason,
		Audience:       "operator",
		Operator: TaskEvidenceOperator{
			TaskRun: run,
			Root:    root,
		},
		AgentFacing: TaskEvidenceAgentFacing{
			RootContractID:   run.RootContractID,
			RootAssignmentID: run.RootAssignmentID,
			RootEnvelopeID:   run.RootEnvelopeID,
			RootMailboxID:    run.RootMailboxID,
			ContractIDs:      append([]string(nil), contractIDs...),
			SessionRefs:      evidenceSessionRefs(started),
		},
		Redaction: TaskEvidenceRedaction{
			OperatorOnlyFieldsRedacted: true,
			TokensIncluded:             false,
			HostPathsIncluded:          false,
			RuntimeRootsIncluded:       false,
			RedactedFields: []string{
				"operator request payload",
				"runtime credential material",
				"session-local working directories",
				"runtime workspace and home roots",
			},
		},
		StartedSessions:       started,
		ContractLineage:       lineage,
		EvidenceRefs:          refs,
		ValidationAssessments: validations,
		CapabilityCallCounts:  capabilityCounts,
		CommunicationCounts:   communicationCounts,
		InspectSummary:        inspectSummary,
		Terminal:              terminal,
	}
	return evidence, nil
}

func (s *Service) evidenceTaskRun(ctx context.Context, taskRunID string) (EvidenceTaskRun, error) {
	var out EvidenceTaskRun
	err := s.db.QueryRowContext(ctx, `
SELECT id, COALESCE(run_label, ''), team_id, team_version, root_contract_id,
       root_assignment_id, root_envelope_id, root_mailbox_id, created_at
FROM operator_task_runs
WHERE id = ?`, taskRunID).Scan(
		&out.ID,
		&out.RunLabel,
		&out.TeamID,
		&out.TeamVersion,
		&out.RootContractID,
		&out.RootAssignmentID,
		&out.RootEnvelopeID,
		&out.RootMailboxID,
		&out.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceTaskRun{}, reject("TASK_RUN_NOT_FOUND", "operator task run was not found")
	}
	if err != nil {
		return EvidenceTaskRun{}, fmt.Errorf("lookup operator task evidence run: %w", err)
	}
	return out, nil
}

func (s *Service) evidenceRoot(ctx context.Context, run EvidenceTaskRun) (EvidenceRoot, error) {
	var out EvidenceRoot
	err := s.db.QueryRowContext(ctx, `
SELECT c.id, c.status, c.target_id,
       a.id, a.state, COALESCE(a.session_route_id, ''),
       otr.root_envelope_id,
       m.id, m.state, COALESCE(m.followup_ref, '')
FROM operator_task_runs otr
JOIN work_contracts c ON c.id = otr.root_contract_id
JOIN assignments a ON a.id = otr.root_assignment_id
JOIN mailbox_items m ON m.id = otr.root_mailbox_id
WHERE otr.id = ?`, run.ID).Scan(
		&out.ContractID,
		&out.ContractStatus,
		&out.TargetAgentID,
		&out.AssignmentID,
		&out.AssignmentState,
		&out.SessionRouteID,
		&out.EnvelopeID,
		&out.MailboxID,
		&out.MailboxState,
		&out.MailboxFollowup,
	)
	if err != nil {
		return EvidenceRoot{}, fmt.Errorf("lookup operator task evidence root: %w", err)
	}
	return out, nil
}

func (s *Service) evidenceContractLineage(ctx context.Context, rootContractID string) ([]EvidenceContract, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE lineage(id, depth) AS (
  SELECT ?, 0
  UNION ALL
  SELECT c.id, lineage.depth + 1
  FROM work_contracts c
  JOIN lineage ON c.issuer_contract_id = lineage.id
)
SELECT c.id, COALESCE(c.issuer_contract_id, ''), COALESCE(c.issuer_agent_id, ''),
       c.target_kind, c.target_id, c.status, lineage.depth
FROM lineage
JOIN work_contracts c ON c.id = lineage.id
ORDER BY lineage.depth ASC, c.created_at ASC, c.id ASC`, rootContractID)
	if err != nil {
		return nil, fmt.Errorf("lookup contract lineage: %w", err)
	}
	defer rows.Close()
	out := []EvidenceContract{}
	for rows.Next() {
		var contract EvidenceContract
		if err := rows.Scan(
			&contract.ID,
			&contract.IssuerContractID,
			&contract.IssuerAgentID,
			&contract.TargetKind,
			&contract.TargetID,
			&contract.Status,
			&contract.Depth,
		); err != nil {
			return nil, err
		}
		out = append(out, contract)
	}
	return out, rows.Err()
}

func (s *Service) evidenceStartedSessions(ctx context.Context, contractIDs []string) ([]EvidenceStartedSession, error) {
	if len(contractIDs) == 0 {
		return []EvidenceStartedSession{}, nil
	}
	query := `
SELECT a.contract_id, a.id, l.agent_id, l.id, l.state,
       att.id, att.status,
       COALESCE(sr.id, ''), COALESCE(sr.state, ''), COALESCE(sr.runtime_id, ''), COALESCE(sr.cli_backend, ''),
       COALESCE(ri.state, '')
FROM assignments a
JOIN leases l ON l.assignment_id = a.id
JOIN attempts att ON att.lease_id = l.id
LEFT JOIN session_routes sr ON sr.id = l.session_route_id
LEFT JOIN runtime_instances ri ON ri.runtime_id = sr.runtime_id AND ri.attempt_id = att.id
WHERE a.contract_id IN (` + placeholders(len(contractIDs)) + `)
ORDER BY att.started_at ASC, att.id ASC`
	rows, err := s.db.QueryContext(ctx, query, stringArgs(contractIDs)...)
	if err != nil {
		return nil, fmt.Errorf("lookup started sessions: %w", err)
	}
	defer rows.Close()
	out := []EvidenceStartedSession{}
	for rows.Next() {
		var session EvidenceStartedSession
		if err := rows.Scan(
			&session.ContractID,
			&session.AssignmentID,
			&session.AgentID,
			&session.LeaseID,
			&session.LeaseState,
			&session.AttemptID,
			&session.AttemptStatus,
			&session.SessionRouteID,
			&session.RouteState,
			&session.RuntimeID,
			&session.CLIBackend,
			&session.RuntimeState,
		); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *Service) evidenceRefs(ctx context.Context, contractIDs []string) ([]EvidenceRef, error) {
	if len(contractIDs) == 0 {
		return []EvidenceRef{}, nil
	}
	query := `
SELECT id, kind, contract_id, produced_by, COALESCE(verdict, ''),
       COALESCE(content_ref, ''), created_at
FROM evidence
WHERE contract_id IN (` + placeholders(len(contractIDs)) + `)
ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, stringArgs(contractIDs)...)
	if err != nil {
		return nil, fmt.Errorf("lookup evidence refs: %w", err)
	}
	defer rows.Close()
	out := []EvidenceRef{}
	for rows.Next() {
		var ref EvidenceRef
		if err := rows.Scan(&ref.ID, &ref.Kind, &ref.ContractID, &ref.ProducedBy, &ref.Verdict, &ref.ContentRef, &ref.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *Service) evidenceValidations(ctx context.Context, contractIDs []string) ([]EvidenceValidationAssessment, error) {
	if len(contractIDs) == 0 {
		return []EvidenceValidationAssessment{}, nil
	}
	args := append(stringArgs(contractIDs), stringArgs(contractIDs)...)
	query := `
SELECT id, evidence_id, verifier_agent_id, contract_id, assessed_contract_id, verdict, created_at
FROM validation_assessments
WHERE contract_id IN (` + placeholders(len(contractIDs)) + `)
  AND assessed_contract_id IN (` + placeholders(len(contractIDs)) + `)
ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("lookup validation assessments: %w", err)
	}
	defer rows.Close()
	out := []EvidenceValidationAssessment{}
	for rows.Next() {
		var assessment EvidenceValidationAssessment
		if err := rows.Scan(
			&assessment.ID,
			&assessment.EvidenceID,
			&assessment.VerifierAgentID,
			&assessment.ContractID,
			&assessment.AssessedContractID,
			&assessment.Verdict,
			&assessment.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, assessment)
	}
	return out, rows.Err()
}

func (s *Service) evidenceCapabilityCounts(ctx context.Context, taskRunID, rootContractID, rootAssignmentID string, contractIDs []string) (map[string]int64, error) {
	if len(contractIDs) == 0 {
		return map[string]int64{}, nil
	}
	args := []any{rootContractID, rootAssignmentID, taskRunID}
	args = append(args, stringArgs(contractIDs)...)
	rows, err := s.db.QueryContext(ctx, `
SELECT capability_name, COUNT(*)
FROM (
  SELECT capability_name
  FROM capability_calls
  WHERE status = 'accepted'
    AND subject_kind IN ('operator', 'debug')
    AND (
      json_extract(scope_json, '$.root_contract_id') = ?
      OR json_extract(scope_json, '$.root_assignment_id') = ?
      OR (
        trace_id = ?
        AND capability_name IN ('operator.task.create', 'operator.task.start', 'operator.task.wait')
      )
    )
  UNION ALL
  SELECT cc.capability_name
  FROM capability_calls cc
  JOIN leases l ON l.id = json_extract(cc.scope_json, '$.lease_id')
  JOIN assignments a ON a.id = l.assignment_id
  WHERE cc.status = 'accepted'
    AND cc.subject_kind = 'agent'
    AND a.contract_id IN (`+placeholders(len(contractIDs))+`)
)
GROUP BY capability_name
ORDER BY capability_name`, args...)
	if err != nil {
		return nil, fmt.Errorf("lookup capability counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		out[name] = count
	}
	return out, rows.Err()
}

func (s *Service) evidenceCommunicationCounts(ctx context.Context, contractIDs []string) (EvidenceCommunicationCounts, error) {
	out := EvidenceCommunicationCounts{
		EnvelopesByKind: map[string]int64{},
		MailboxByState:  map[string]int64{},
	}
	if len(contractIDs) == 0 {
		return out, nil
	}
	args := stringArgs(contractIDs)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_communication_envelopes WHERE contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.Envelopes); err != nil {
		return out, fmt.Errorf("count envelopes: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailbox_items WHERE contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.MailboxItems); err != nil {
		return out, fmt.Errorf("count mailbox items: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
FROM delivery_attempts d
JOIN mailbox_items m ON m.id = d.mailbox_item_id
WHERE m.contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.DeliveryAttempts); err != nil {
		return out, fmt.Errorf("count delivery attempts: %w", err)
	}
	if err := scanGroupedCounts(ctx, s.db, out.EnvelopesByKind,
		`SELECT kind, COUNT(*) FROM agent_communication_envelopes WHERE contract_id IN (`+placeholders(len(contractIDs))+`) GROUP BY kind ORDER BY kind`,
		args...,
	); err != nil {
		return out, err
	}
	if err := scanGroupedCounts(ctx, s.db, out.MailboxByState,
		`SELECT state, COUNT(*) FROM mailbox_items WHERE contract_id IN (`+placeholders(len(contractIDs))+`) GROUP BY state ORDER BY state`,
		args...,
	); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Service) evidenceInspectSummary(ctx context.Context) (map[string]int64, error) {
	tables := []string{
		"operator_task_runs",
		"work_contracts",
		"assignments",
		"leases",
		"attempts",
		"session_routes",
		"runtime_instances",
		"capability_calls",
		"agent_communication_envelopes",
		"mailbox_items",
		"delivery_attempts",
		"evidence",
		"validation_assessments",
	}
	out := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = count
	}
	return out, nil
}

func (s *Service) evidenceTerminal(ctx context.Context, rootContractID string, contractIDs []string) (EvidenceTerminal, error) {
	var out EvidenceTerminal
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, rootContractID).Scan(&out.RootContractStatus); err != nil {
		return out, fmt.Errorf("lookup root contract status: %w", err)
	}
	if len(contractIDs) == 0 {
		out.Status = "blocked"
		out.FailureSummary = "dead queue: root contract lineage is empty"
		return out, nil
	}
	args := stringArgs(contractIDs)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence WHERE kind = 'report' AND contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.ReportCount); err != nil {
		return out, fmt.Errorf("count report evidence: %w", err)
	}
	validationArgs := append(stringArgs(contractIDs), stringArgs(contractIDs)...)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM validation_assessments WHERE verdict = 'pass' AND contract_id IN (`+placeholders(len(contractIDs))+`) AND assessed_contract_id IN (`+placeholders(len(contractIDs))+`)`,
		validationArgs...,
	).Scan(&out.ValidationPassCount); err != nil {
		return out, fmt.Errorf("count passing validation: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM validation_assessments WHERE verdict IN ('fail', 'blocked') AND contract_id IN (`+placeholders(len(contractIDs))+`) AND assessed_contract_id IN (`+placeholders(len(contractIDs))+`)`,
		validationArgs...,
	).Scan(&out.ValidationFailureCount); err != nil {
		return out, fmt.Errorf("count failing validation: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assignments WHERE state = 'queued' AND contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.QueuedAssignmentCount); err != nil {
		return out, fmt.Errorf("count queued assignments: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assignments WHERE state IN ('claimed', 'waiting') AND contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.ActiveAssignmentCount); err != nil {
		return out, fmt.Errorf("count active assignments: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
WHERE l.state = 'active' AND a.contract_id IN (`+placeholders(len(contractIDs))+`)`,
		args...,
	).Scan(&out.ActiveLeaseCount); err != nil {
		return out, fmt.Errorf("count active leases: %w", err)
	}
	if failure, ok, err := s.evidenceRuntimePolicyFailure(ctx, contractIDs); err != nil {
		return out, err
	} else if ok {
		out.Status = failure.status
		out.FailureSummary = failure.summary
		out.FailureClass = failure.failureClass
		out.TerminalReason = failure.terminalReason
		return out, nil
	}

	unfinishedLineage := out.QueuedAssignmentCount > 0 || out.ActiveAssignmentCount > 0 || out.ActiveLeaseCount > 0
	switch {
	case out.RootContractStatus == "satisfied" && out.ReportCount == 0:
		out.Status = "failed"
		out.FailureSummary = "root task is satisfied without durable report evidence in its contract lineage"
	case out.RootContractStatus == "satisfied" && out.ValidationFailureCount > 0:
		out.Status = "failed"
		out.FailureSummary = "contract lineage contains a failing or blocked validation assessment"
	case out.RootContractStatus == "satisfied" && out.ValidationPassCount == 0:
		out.Status = "failed"
		out.FailureSummary = "root task is satisfied without a passing validation assessment in its contract lineage"
	case out.RootContractStatus == "satisfied" && unfinishedLineage:
		out.Status = "running"
		out.FailureSummary = "root task has report and validation evidence but contract lineage still has unfinished assignments or active leases"
	case out.RootContractStatus == "satisfied":
		out.Status = "passed"
	case out.ValidationFailureCount > 0:
		out.Status = "failed"
		out.FailureSummary = "contract lineage contains a failing or blocked validation assessment"
	case out.QueuedAssignmentCount == 0 && out.ActiveAssignmentCount == 0 && out.ActiveLeaseCount == 0:
		out.Status = "blocked"
		out.FailureSummary = "dead queue: root contract is open and no queued or active lineage assignments remain"
	default:
		out.Status = "running"
	}
	return out, nil
}

type runtimePolicyFailureEvidence struct {
	status         string
	summary        string
	failureClass   string
	terminalReason string
}

func (s *Service) evidenceRuntimePolicyFailure(ctx context.Context, contractIDs []string) (runtimePolicyFailureEvidence, bool, error) {
	if len(contractIDs) == 0 {
		return runtimePolicyFailureEvidence{}, false, nil
	}
	args := stringArgs(contractIDs)
	query := `
SELECT COALESCE(att.transcript_ref, ''), COALESCE(cs.last_error, ''), COALESCE(ri.last_error, '')
FROM assignments a
JOIN leases l ON l.assignment_id = a.id
JOIN attempts att ON att.lease_id = l.id
LEFT JOIN cli_sessions cs ON cs.attempt_id = att.id
LEFT JOIN runtime_instances ri ON ri.attempt_id = att.id
WHERE a.contract_id IN (` + placeholders(len(contractIDs)) + `)
  AND (att.status = 'failed' OR cs.state = 'failed' OR ri.state = 'failed')
ORDER BY att.started_at DESC, cs.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return runtimePolicyFailureEvidence{}, false, fmt.Errorf("lookup runtime policy failure evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptRef, cliError, runtimeError string
		if err := rows.Scan(&attemptRef, &cliError, &runtimeError); err != nil {
			return runtimePolicyFailureEvidence{}, false, err
		}
		if failure, ok := runtimePolicyFailureFromText(attemptRef + "\n" + cliError + "\n" + runtimeError); ok {
			return failure, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return runtimePolicyFailureEvidence{}, false, err
	}
	return runtimePolicyFailureEvidence{}, false, nil
}

func runtimePolicyFailureFromText(text string) (runtimePolicyFailureEvidence, bool) {
	switch {
	case strings.Contains(text, cpruntime.TerminalReasonApprovalPolicyUnavailable):
		return runtimePolicyFailureEvidence{
			status:         "blocked",
			summary:        "runtime approval policy unavailable for non-interactive command execution",
			failureClass:   cpruntime.FailureClassRuntimeApprovalBlocked,
			terminalReason: cpruntime.TerminalReasonApprovalPolicyUnavailable,
		}, true
	case strings.Contains(text, cpruntime.TerminalReasonCommandPolicyDenied):
		return runtimePolicyFailureEvidence{
			status:         "failed",
			summary:        "runtime command policy denied an unauthorized command before execution",
			failureClass:   cpruntime.FailureClassRuntimeCommandDenied,
			terminalReason: cpruntime.TerminalReasonCommandPolicyDenied,
		}, true
	default:
		return runtimePolicyFailureEvidence{}, false
	}
}

func contractLineageIDs(lineage []EvidenceContract) []string {
	out := make([]string, 0, len(lineage))
	for _, contract := range lineage {
		out = append(out, contract.ID)
	}
	return out
}

func evidenceSessionRefs(sessions []EvidenceStartedSession) []EvidenceSessionRef {
	out := make([]EvidenceSessionRef, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, EvidenceSessionRef{
			AssignmentID:   session.AssignmentID,
			AgentID:        session.AgentID,
			LeaseID:        session.LeaseID,
			AttemptID:      session.AttemptID,
			SessionRouteID: session.SessionRouteID,
			RuntimeID:      session.RuntimeID,
		})
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func stringArgs(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func scanGroupedCounts(ctx context.Context, db *sql.DB, target map[string]int64, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			return err
		}
		target[key] = count
	}
	return rows.Err()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
