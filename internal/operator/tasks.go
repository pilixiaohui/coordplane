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
	"coordplane/internal/events"
	"coordplane/internal/ids"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
)

const CapabilityNameTaskCreate = "operator.task.create"
const CapabilityNameTaskStart = "operator.task.start"

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type RunnerStarter interface {
	StartAssignment(ctx context.Context, agentID, assignmentID string) (cpruntime.AssignmentSession, error)
}

type Config struct {
	Store            *store.Store
	TeamConfig       teamconfig.Config
	TeamConfigLoaded bool
	Runner           RunnerStarter
}

type Service struct {
	store            *store.Store
	db               *sql.DB
	teamConfig       teamconfig.Config
	teamConfigLoaded bool
	runner           RunnerStarter
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Store.DB() == nil {
		return nil, errors.New("operator tasks: store is required")
	}
	return &Service{
		store:            cfg.Store,
		db:               cfg.Store.DB(),
		teamConfig:       cfg.TeamConfig,
		teamConfigLoaded: cfg.TeamConfigLoaded,
		runner:           cfg.Runner,
	}, nil
}

type Subject struct {
	Kind string
	ID   string
}

type CreateTaskInput struct {
	RunLabel               string   `json:"run_label,omitempty"`
	IdempotencyKey         string   `json:"idempotency_key"`
	TeamID                 string   `json:"team_id,omitempty"`
	TeamVersion            int      `json:"team_version,omitempty"`
	Title                  string   `json:"title"`
	Objective              string   `json:"objective"`
	TargetAgentID          string   `json:"target_agent_id,omitempty"`
	TargetRole             string   `json:"target_role,omitempty"`
	CompletionRequirements []string `json:"completion_requirements,omitempty"`
}

type CreateTaskResult struct {
	TaskRunID        string `json:"task_run_id"`
	RootTaskID       string `json:"root_task_id"`
	RootContractID   string `json:"root_contract_id"`
	RootAssignmentID string `json:"root_assignment_id"`
	RootEnvelopeID   string `json:"root_envelope_id"`
	RootMailboxID    string `json:"root_mailbox_id"`
	TeamID           string `json:"team_id"`
	TeamVersion      int    `json:"team_version"`
	Status           string `json:"status"`
	IdempotentReplay bool   `json:"idempotent_replay,omitempty"`
}

type StartTaskInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type StartTaskResult struct {
	TaskRunID        string `json:"task_run_id"`
	RootTaskID       string `json:"root_task_id"`
	RootContractID   string `json:"root_contract_id"`
	RootAssignmentID string `json:"root_assignment_id"`
	TargetAgentID    string `json:"target_agent_id"`
	LeaseID          string `json:"lease_id"`
	AttemptID        string `json:"attempt_id"`
	SessionRouteID   string `json:"session_route_id"`
	RuntimeID        string `json:"runtime_id"`
	Status           string `json:"status"`
	IdempotentReplay bool   `json:"idempotent_replay,omitempty"`
}

type RejectedError struct {
	Code    string
	Message string
}

func (e RejectedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (s *Service) CreateTask(ctx context.Context, subject Subject, in CreateTaskInput) (CreateTaskResult, error) {
	subject = normalizeSubject(subject)
	if subject.Kind != "operator" && subject.Kind != "debug" {
		return CreateTaskResult{}, reject("OPERATOR_SUBJECT_REQUIRED", "operator task creation requires an operator or debug subject")
	}
	normalized, err := s.normalizeInput(in)
	if err != nil {
		return CreateTaskResult{}, err
	}
	requestJSON, err := sanitizedRequestJSON(normalized)
	if err != nil {
		return CreateTaskResult{}, err
	}

	var result CreateTaskResult
	err = s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		existing, ok, err := existingTaskRun(ctx, tx, normalized.IdempotencyKey, requestJSON)
		if err != nil {
			return err
		}
		if ok {
			existing.IdempotentReplay = true
			result = existing
			return nil
		}

		taskRunID, err := ids.New("taskrun")
		if err != nil {
			return err
		}
		contractID, err := ids.New("ctr")
		if err != nil {
			return err
		}
		assignmentID, err := ids.New("asg")
		if err != nil {
			return err
		}
		envelopeID, err := ids.New("env")
		if err != nil {
			return err
		}
		mailboxID, err := ids.New("mbx")
		if err != nil {
			return err
		}

		now := formatTime(time.Now())
		requirementsJSON, err := completionRequirementsJSON(normalized.CompletionRequirements)
		if err != nil {
			return err
		}
		targetKind := "agent"
		targetID := normalized.TargetAgentID
		assigneeAgent := normalized.TargetAgentID
		assigneeRole := ""
		triggerTurn := s.teamConfig.Communication.TriggerTurn("task")

		if _, err := tx.ExecContext(ctx, `
INSERT INTO work_contracts (
  id, tenant_id, title, objective, issuer_agent_id, issuer_contract_id,
  target_kind, target_id, status, completion_requirements_json,
  acceptance_policy_json, created_at, updated_at
) VALUES (?, 'default', ?, ?, 'operator', '', ?, ?, 'open', ?, '{}', ?, ?)`,
			contractID, normalized.Title, normalized.Objective, targetKind, targetID, requirementsJSON, now, now,
		); err != nil {
			return fmt.Errorf("insert operator root contract: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO contract_team_scopes (
  contract_id, tenant_id, team_id, team_version, source, created_at
) VALUES (?, 'default', ?, ?, ?, ?)`,
			contractID, normalized.TeamID, normalized.TeamVersion, CapabilityNameTaskCreate, now,
		); err != nil {
			return fmt.Errorf("bind operator root team scope: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO assignments (
  id, tenant_id, contract_id, assignee_agent_id, assignee_role, state,
  priority, reason, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, 'queued', 0, 'operator_root_task', ?, ?)`,
			assignmentID, contractID, nullable(assigneeAgent), nullable(assigneeRole), now, now,
		); err != nil {
			return fmt.Errorf("insert operator root assignment: %w", err)
		}
		envelopeMetadata, err := json.Marshal(map[string]string{
			"source":      CapabilityNameTaskCreate,
			"task_run_id": taskRunID,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO agent_communication_envelopes (
  id, tenant_id, kind, sender_agent_id, recipient_agent_id, recipient_role,
  thread_id, message_id, contract_id, parent_envelope_id, summary,
  body_inline, body_ref, trigger_turn, metadata_json, created_at
) VALUES (?, 'default', 'task', 'operator', ?, ?, NULL, NULL, ?, NULL, ?, ?, NULL, ?, ?, ?)`,
			envelopeID, nullable(assigneeAgent), nullable(assigneeRole), contractID, normalized.Title,
			normalized.Objective, boolInt(triggerTurn), string(envelopeMetadata), now,
		); err != nil {
			return fmt.Errorf("insert operator task envelope: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO mailbox_items (
  id, tenant_id, recipient_agent_id, recipient_role, envelope_id, reason,
  contract_id, state, trigger_turn, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, 'task_assigned', ?, 'pending', ?, ?, ?)`,
			mailboxID, nullable(assigneeAgent), nullable(assigneeRole), envelopeID, contractID,
			boolInt(triggerTurn), now, now,
		); err != nil {
			return fmt.Errorf("insert operator task mailbox: %w", err)
		}

		out := CreateTaskResult{
			TaskRunID:        taskRunID,
			RootTaskID:       contractID,
			RootContractID:   contractID,
			RootAssignmentID: assignmentID,
			RootEnvelopeID:   envelopeID,
			RootMailboxID:    mailboxID,
			TeamID:           normalized.TeamID,
			TeamVersion:      normalized.TeamVersion,
			Status:           "created",
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO operator_task_runs (
  id, tenant_id, idempotency_key, run_label, operator_subject_kind,
  operator_subject_id, team_id, team_version, root_contract_id,
  root_assignment_id, root_envelope_id, root_mailbox_id, request_json,
  created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskRunID, normalized.IdempotencyKey, normalized.RunLabel, subject.Kind, subject.ID,
			normalized.TeamID, normalized.TeamVersion, contractID, assignmentID, envelopeID,
			mailboxID, string(requestJSON), now,
		); err != nil {
			return fmt.Errorf("insert operator task run: %w", err)
		}
		if err := appendCapabilityAudit(ctx, tx, subject, normalized, out); err != nil {
			return err
		}
		if err := appendOperatorEvents(ctx, tx, subject, normalized, out); err != nil {
			return err
		}
		result = out
		return nil
	})
	return result, err
}

func (s *Service) StartTask(ctx context.Context, subject Subject, taskRunID string, in StartTaskInput) (StartTaskResult, error) {
	subject = normalizeSubject(subject)
	if subject.Kind != "operator" && subject.Kind != "debug" {
		return StartTaskResult{}, reject("OPERATOR_SUBJECT_REQUIRED", "operator task start requires an operator or debug subject")
	}
	if s.runner == nil {
		return StartTaskResult{}, errors.New("operator task start: runner is required")
	}
	taskRunID = strings.TrimSpace(taskRunID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if taskRunID == "" {
		return StartTaskResult{}, reject("TASK_RUN_REQUIRED", "operator task start requires task_run_id")
	}
	root, err := s.rootTaskRun(ctx, taskRunID)
	if err != nil {
		return StartTaskResult{}, err
	}
	if existing, ok, err := s.startedRootSession(ctx, root); err != nil {
		return StartTaskResult{}, err
	} else if ok {
		existing.IdempotentReplay = true
		return existing, nil
	}

	session, err := s.runner.StartAssignment(ctx, root.TargetAgentID, root.RootAssignmentID)
	if err != nil {
		if errors.Is(err, coordination.ErrAssignmentBusy) {
			return StartTaskResult{}, reject("TARGET_AGENT_BUSY", "target agent already has an active assignment")
		}
		if errors.Is(err, coordination.ErrAssignmentNotFound) || errors.Is(err, coordination.ErrAssignmentNotClaimable) {
			return StartTaskResult{}, reject("ROOT_ASSIGNMENT_NOT_STARTABLE", "operator task root assignment is not queued for its target agent")
		}
		return StartTaskResult{}, fmt.Errorf("operator task start: %w", err)
	}
	if session.Route.AssignmentID != root.RootAssignmentID {
		return StartTaskResult{}, reject("TARGET_AGENT_BUSY", "target agent already has an active assignment")
	}
	out := startTaskResult(root, session)
	if err := s.appendStartAudit(ctx, subject, root, out, in.IdempotencyKey); err != nil {
		return StartTaskResult{}, err
	}
	return out, nil
}

func (s *Service) normalizeInput(in CreateTaskInput) (CreateTaskInput, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.RunLabel = strings.TrimSpace(in.RunLabel)
	in.TeamID = strings.TrimSpace(in.TeamID)
	in.Title = strings.TrimSpace(in.Title)
	in.Objective = strings.TrimSpace(in.Objective)
	in.TargetAgentID = strings.TrimSpace(in.TargetAgentID)
	in.TargetRole = strings.TrimSpace(in.TargetRole)
	if in.IdempotencyKey == "" {
		return CreateTaskInput{}, reject("IDEMPOTENCY_KEY_REQUIRED", "operator task creation requires idempotency_key")
	}
	if !s.teamConfigLoaded || s.teamConfig.TeamID == "" || s.teamConfig.Version <= 0 {
		return CreateTaskInput{}, reject("TEAM_CONFIG_REQUIRED", "operator task creation requires a loaded TeamConfig")
	}
	if in.TeamID == "" {
		in.TeamID = s.teamConfig.TeamID
	}
	if in.TeamVersion == 0 {
		in.TeamVersion = s.teamConfig.Version
	}
	if in.TeamID != s.teamConfig.TeamID || in.TeamVersion != s.teamConfig.Version {
		return CreateTaskInput{}, reject("TEAM_SCOPE_REJECTED", "operator task team scope must match the loaded TeamConfig")
	}
	if in.Title == "" {
		return CreateTaskInput{}, reject("TITLE_REQUIRED", "operator task creation requires title")
	}
	if in.Objective == "" {
		return CreateTaskInput{}, reject("OBJECTIVE_REQUIRED", "operator task creation requires objective")
	}
	if in.TargetRole != "" && in.TargetAgentID == "" {
		return CreateTaskInput{}, reject("TARGET_AGENT_REQUIRED", "operator task creation currently requires target_agent_id")
	}
	if in.TargetAgentID == "" {
		return CreateTaskInput{}, reject("TARGET_AGENT_REQUIRED", "operator task creation requires target_agent_id")
	}
	if _, ok := s.teamConfig.Agent(in.TargetAgentID); !ok {
		return CreateTaskInput{}, reject("TARGET_AGENT_REJECTED", "target_agent_id is not declared in the loaded TeamConfig")
	}
	if len(in.CompletionRequirements) == 0 {
		in.CompletionRequirements = []string{"report"}
	}
	requirements := make([]string, 0, len(in.CompletionRequirements))
	for _, requirement := range in.CompletionRequirements {
		requirement = strings.TrimSpace(requirement)
		if requirement != "" {
			requirements = append(requirements, requirement)
		}
	}
	if len(requirements) == 0 {
		requirements = []string{"report"}
	}
	in.CompletionRequirements = requirements
	return in, nil
}

func existingTaskRun(ctx context.Context, tx *sql.Tx, idempotencyKey string, currentRequestJSON []byte) (CreateTaskResult, bool, error) {
	var out CreateTaskResult
	var storedRequestJSON string
	err := tx.QueryRowContext(ctx, `
SELECT id, root_contract_id, root_assignment_id, root_envelope_id, root_mailbox_id,
       team_id, team_version, request_json
FROM operator_task_runs
WHERE idempotency_key = ?`,
		idempotencyKey,
	).Scan(
		&out.TaskRunID,
		&out.RootContractID,
		&out.RootAssignmentID,
		&out.RootEnvelopeID,
		&out.RootMailboxID,
		&out.TeamID,
		&out.TeamVersion,
		&storedRequestJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateTaskResult{}, false, nil
	}
	if err != nil {
		return CreateTaskResult{}, false, fmt.Errorf("lookup operator task idempotency key: %w", err)
	}
	same, err := stableJSONEqual([]byte(storedRequestJSON), currentRequestJSON)
	if err != nil {
		return CreateTaskResult{}, false, err
	}
	if !same {
		return CreateTaskResult{}, false, reject("IDEMPOTENCY_KEY_CONFLICT", "idempotency_key was already used for a different operator task request")
	}
	out.RootTaskID = out.RootContractID
	out.Status = "created"
	return out, true, nil
}

type rootTaskRun struct {
	TaskRunID        string
	RootContractID   string
	RootAssignmentID string
	TargetAgentID    string
}

func (s *Service) rootTaskRun(ctx context.Context, taskRunID string) (rootTaskRun, error) {
	var out rootTaskRun
	err := s.db.QueryRowContext(ctx, `
SELECT otr.id, otr.root_contract_id, otr.root_assignment_id, COALESCE(a.assignee_agent_id, '')
FROM operator_task_runs otr
JOIN assignments a ON a.id = otr.root_assignment_id
WHERE otr.id = ?`,
		taskRunID,
	).Scan(&out.TaskRunID, &out.RootContractID, &out.RootAssignmentID, &out.TargetAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return rootTaskRun{}, reject("TASK_RUN_NOT_FOUND", "operator task run was not found")
	}
	if err != nil {
		return rootTaskRun{}, fmt.Errorf("lookup operator task run: %w", err)
	}
	if out.TargetAgentID == "" {
		return rootTaskRun{}, reject("TARGET_AGENT_REJECTED", "operator task root assignment has no target agent")
	}
	if _, ok := s.teamConfig.Agent(out.TargetAgentID); !ok {
		return rootTaskRun{}, reject("TARGET_AGENT_REJECTED", "operator task target agent is not declared in the loaded TeamConfig")
	}
	return out, nil
}

func (s *Service) startedRootSession(ctx context.Context, root rootTaskRun) (StartTaskResult, bool, error) {
	var routeID string
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(session_route_id, '')
FROM assignments
WHERE id = ?`,
		root.RootAssignmentID,
	).Scan(&routeID)
	if errors.Is(err, sql.ErrNoRows) {
		return StartTaskResult{}, false, reject("TASK_RUN_NOT_FOUND", "operator task root assignment was not found")
	}
	if err != nil {
		return StartTaskResult{}, false, fmt.Errorf("lookup operator task root assignment: %w", err)
	}
	if routeID == "" {
		return StartTaskResult{}, false, nil
	}
	var route cpruntime.SessionRoute
	var routeRaw string
	if err := s.db.QueryRowContext(ctx, `
SELECT route_json
FROM session_routes
WHERE id = ? AND state = 'active'`,
		routeID,
	).Scan(&routeRaw); errors.Is(err, sql.ErrNoRows) {
		return StartTaskResult{}, false, nil
	} else if err != nil {
		return StartTaskResult{}, false, fmt.Errorf("lookup operator task session route: %w", err)
	}
	if err := json.Unmarshal([]byte(routeRaw), &route); err != nil {
		return StartTaskResult{}, false, fmt.Errorf("decode operator task session route: %w", err)
	}
	if route.AssignmentID != root.RootAssignmentID || route.AgentID != root.TargetAgentID {
		return StartTaskResult{}, false, nil
	}
	var attemptStatus, leaseState string
	if err := s.db.QueryRowContext(ctx, `
SELECT a.status, l.state
FROM attempts a
JOIN leases l ON l.id = a.lease_id
WHERE a.id = ? AND l.id = ?`,
		route.AttemptID, route.LeaseID,
	).Scan(&attemptStatus, &leaseState); errors.Is(err, sql.ErrNoRows) {
		return StartTaskResult{}, false, nil
	} else if err != nil {
		return StartTaskResult{}, false, fmt.Errorf("lookup operator task attempt: %w", err)
	}
	if attemptStatus != "running" || leaseState != "active" {
		return StartTaskResult{}, false, nil
	}
	return StartTaskResult{
		TaskRunID:        root.TaskRunID,
		RootTaskID:       root.RootContractID,
		RootContractID:   root.RootContractID,
		RootAssignmentID: root.RootAssignmentID,
		TargetAgentID:    root.TargetAgentID,
		LeaseID:          route.LeaseID,
		AttemptID:        route.AttemptID,
		SessionRouteID:   route.ID,
		RuntimeID:        route.RuntimeID,
		Status:           "started",
	}, true, nil
}

func startTaskResult(root rootTaskRun, session cpruntime.AssignmentSession) StartTaskResult {
	return StartTaskResult{
		TaskRunID:        root.TaskRunID,
		RootTaskID:       root.RootContractID,
		RootContractID:   root.RootContractID,
		RootAssignmentID: root.RootAssignmentID,
		TargetAgentID:    root.TargetAgentID,
		LeaseID:          session.LeaseID,
		AttemptID:        session.AttemptID,
		SessionRouteID:   session.Route.ID,
		RuntimeID:        session.Route.RuntimeID,
		Status:           "started",
	}
}

func appendCapabilityAudit(ctx context.Context, tx *sql.Tx, subject Subject, in CreateTaskInput, out CreateTaskResult) error {
	id, err := ids.New("capcall")
	if err != nil {
		return err
	}
	scopeJSON, err := json.Marshal(map[string]any{
		"team_id":            in.TeamID,
		"team_version":       in.TeamVersion,
		"root_contract_id":   out.RootContractID,
		"root_assignment_id": out.RootAssignmentID,
		"root_envelope_id":   out.RootEnvelopeID,
		"root_mailbox_id":    out.RootMailboxID,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO capability_calls (
  id, tenant_id, trace_id, capability_name, subject_kind, subject_id,
  scope_json, status, idempotency_key, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, 'accepted', ?, ?)`,
		id, out.TaskRunID, CapabilityNameTaskCreate, subject.Kind, subject.ID,
		string(scopeJSON), in.IdempotencyKey, formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("insert operator capability audit: %w", err)
	}
	return nil
}

func (s *Service) appendStartAudit(ctx context.Context, subject Subject, root rootTaskRun, out StartTaskResult, idempotencyKey string) error {
	return s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := ids.New("capcall")
		if err != nil {
			return err
		}
		scopeJSON, err := json.Marshal(map[string]any{
			"root_contract_id":   root.RootContractID,
			"root_assignment_id": root.RootAssignmentID,
			"target_agent_id":    root.TargetAgentID,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO capability_calls (
  id, tenant_id, trace_id, capability_name, subject_kind, subject_id,
  scope_json, status, idempotency_key, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, 'accepted', ?, ?)`,
			id, root.TaskRunID, CapabilityNameTaskStart, subject.Kind, subject.ID,
			string(scopeJSON), nullable(idempotencyKey), formatTime(time.Now()),
		); err != nil {
			return fmt.Errorf("insert operator start capability audit: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"root_contract_id":   out.RootContractID,
			"root_assignment_id": out.RootAssignmentID,
			"lease_id":           out.LeaseID,
			"attempt_id":         out.AttemptID,
			"session_route_id":   out.SessionRouteID,
			"runtime_id":         out.RuntimeID,
			"target_agent_id":    out.TargetAgentID,
		})
		if err != nil {
			return err
		}
		_, err = store.AppendEventTx(ctx, tx, events.Event{
			TenantID:       "default",
			TraceID:        root.TaskRunID,
			Type:           "operator.task.started",
			AggregateType:  "operator_task_run",
			AggregateID:    root.TaskRunID,
			SubjectKind:    subject.Kind,
			SubjectID:      subject.ID,
			CapabilityName: CapabilityNameTaskStart,
			PayloadJSON:    payload,
		})
		return err
	})
}

func appendOperatorEvents(ctx context.Context, tx *sql.Tx, subject Subject, in CreateTaskInput, out CreateTaskResult) error {
	payload, err := json.Marshal(map[string]any{
		"idempotency_key":    in.IdempotencyKey,
		"run_label":          in.RunLabel,
		"root_contract_id":   out.RootContractID,
		"root_assignment_id": out.RootAssignmentID,
		"root_envelope_id":   out.RootEnvelopeID,
		"root_mailbox_id":    out.RootMailboxID,
		"team_id":            in.TeamID,
		"team_version":       in.TeamVersion,
	})
	if err != nil {
		return err
	}
	common := events.Event{
		TenantID:       "default",
		TraceID:        out.TaskRunID,
		SubjectKind:    subject.Kind,
		SubjectID:      subject.ID,
		CapabilityName: CapabilityNameTaskCreate,
	}
	for _, event := range []events.Event{
		{
			Type:          "operator.task.created",
			AggregateType: "operator_task_run",
			AggregateID:   out.TaskRunID,
			PayloadJSON:   payload,
		},
		{
			Type:          "contract.created",
			AggregateType: "work_contract",
			AggregateID:   out.RootContractID,
			PayloadJSON:   payload,
		},
		{
			Type:          "mailbox.created",
			AggregateType: "mailbox_item",
			AggregateID:   out.RootMailboxID,
			PayloadJSON:   payload,
		},
	} {
		event.TenantID = common.TenantID
		event.TraceID = common.TraceID
		event.SubjectKind = common.SubjectKind
		event.SubjectID = common.SubjectID
		event.CapabilityName = common.CapabilityName
		if _, err := store.AppendEventTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

func completionRequirementsJSON(requirements []string) (string, error) {
	raw, err := json.Marshal(map[string][]string{"required_evidence": requirements})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func sanitizedRequestJSON(in CreateTaskInput) ([]byte, error) {
	return json.Marshal(struct {
		RunLabel               string   `json:"run_label,omitempty"`
		IdempotencyKey         string   `json:"idempotency_key"`
		TeamID                 string   `json:"team_id"`
		TeamVersion            int      `json:"team_version"`
		Title                  string   `json:"title"`
		Objective              string   `json:"objective"`
		TargetAgentID          string   `json:"target_agent_id"`
		CompletionRequirements []string `json:"completion_requirements"`
	}{
		RunLabel:               in.RunLabel,
		IdempotencyKey:         in.IdempotencyKey,
		TeamID:                 in.TeamID,
		TeamVersion:            in.TeamVersion,
		Title:                  in.Title,
		Objective:              in.Objective,
		TargetAgentID:          in.TargetAgentID,
		CompletionRequirements: append([]string(nil), in.CompletionRequirements...),
	})
}

func stableJSONEqual(left, right []byte) (bool, error) {
	leftStable, err := stableJSON(left)
	if err != nil {
		return false, err
	}
	rightStable, err := stableJSON(right)
	if err != nil {
		return false, err
	}
	return string(leftStable) == string(rightStable), nil
}

func stableJSON(raw []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode stable JSON: %w", err)
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encode stable JSON: %w", err)
	}
	return out, nil
}

func normalizeSubject(subject Subject) Subject {
	subject.Kind = strings.TrimSpace(subject.Kind)
	subject.ID = strings.TrimSpace(subject.ID)
	if subject.Kind == "" {
		subject.Kind = "operator"
	}
	if subject.ID == "" {
		subject.ID = subject.Kind
	}
	return subject
}

func reject(code, message string) RejectedError {
	return RejectedError{Code: code, Message: message}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
