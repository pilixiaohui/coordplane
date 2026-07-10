package coordination

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/objects"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

var (
	ErrAssignmentBusy         = errors.New("assignment claim: target agent already has active assignment")
	ErrAssignmentNotFound     = errors.New("assignment claim: assignment not found")
	ErrAssignmentNotClaimable = errors.New("assignment claim: assignment is not queued for target agent")
)

type Service struct {
	db          *sql.DB
	objects     *objects.Store
	teamID      string
	teamVersion int
}

func NewService(s *store.Store) *Service {
	return &Service{db: s.DB(), objects: objects.NewStore(s)}
}

func NewServiceWithTeam(s *store.Store, teamID string, teamVersion int) *Service {
	service := NewService(s)
	service.teamID = teamID
	service.teamVersion = teamVersion
	return service
}

func (s *Service) ObjectStore() *objects.Store {
	return s.objects
}

type Contract struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Objective      string `json:"objective"`
	IssuerAgentID  string `json:"issuer_agent_id,omitempty"`
	IssuerContract string `json:"issuer_contract_id,omitempty"`
	TargetKind     string `json:"target_kind"`
	TargetID       string `json:"target_id"`
	Status         string `json:"status"`
	TeamID         string `json:"team_id,omitempty"`
	TeamVersion    int    `json:"team_version,omitempty"`
}

type Assignment struct {
	ID             string `json:"id"`
	ContractID     string `json:"contract_id"`
	AssigneeAgent  string `json:"assignee_agent_id,omitempty"`
	AssigneeRole   string `json:"assignee_role,omitempty"`
	State          string `json:"state"`
	SessionRouteID string `json:"session_route_id,omitempty"`
}

type Lease struct {
	ID             string    `json:"id"`
	AssignmentID   string    `json:"assignment_id"`
	AgentID        string    `json:"agent_id"`
	SessionRouteID string    `json:"session_route_id,omitempty"`
	State          string    `json:"state"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type Evidence struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ContractID string `json:"contract_id"`
	ProducedBy string `json:"produced_by"`
	ContentRef string `json:"content_ref,omitempty"`
	Summary    string `json:"summary"`
}

type Message struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Intent   string `json:"intent"`
	Body     string `json:"body"`
}

type AgentCommunicationEnvelope struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	SenderAgentID    string    `json:"sender_agent_id"`
	RecipientAgentID string    `json:"recipient_agent_id,omitempty"`
	RecipientRole    string    `json:"recipient_role,omitempty"`
	ThreadID         string    `json:"thread_id,omitempty"`
	MessageID        string    `json:"message_id,omitempty"`
	ContractID       string    `json:"contract_id,omitempty"`
	ParentEnvelopeID string    `json:"parent_envelope_id,omitempty"`
	Summary          string    `json:"summary"`
	BodyInline       string    `json:"body_inline,omitempty"`
	BodyRef          string    `json:"body_ref,omitempty"`
	TriggerTurn      bool      `json:"trigger_turn"`
	CreatedAt        time.Time `json:"created_at"`
}

type MailboxItem struct {
	ID               string `json:"id"`
	RecipientAgentID string `json:"recipient_agent_id"`
	EnvelopeID       string `json:"envelope_id,omitempty"`
	EnvelopeKind     string `json:"envelope_kind,omitempty"`
	Reason           string `json:"reason"`
	ThreadID         string `json:"thread_id,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	ContractID       string `json:"contract_id,omitempty"`
	SessionRouteID   string `json:"session_route_id,omitempty"`
	State            string `json:"state"`
	FollowupRef      string `json:"followup_ref,omitempty"`
	TriggerTurn      bool   `json:"trigger_turn"`
}

type AddContractInput struct {
	IssuerLeaseID          string   `json:"lease_id,omitempty"`
	IssuerAgentID          string   `json:"issuer_agent_id,omitempty"`
	Title                  string   `json:"title"`
	Objective              string   `json:"objective"`
	TargetAgentID          string   `json:"target_agent_id,omitempty"`
	TargetRole             string   `json:"target_role,omitempty"`
	CompletionRequirements []string `json:"completion_requirements,omitempty"`
}

type AddContractResult struct {
	ContractID   string `json:"contract_id"`
	AssignmentID string `json:"assignment_id"`
	EnvelopeID   string `json:"envelope_id"`
	MailboxID    string `json:"mailbox_id,omitempty"`
}

type AssignmentNextInput struct {
	AgentID  string        `json:"agent_id,omitempty"`
	LeaseFor time.Duration `json:"-"`
	Now      time.Time     `json:"-"`
}

type AssignmentClaimInput struct {
	AgentID      string        `json:"agent_id,omitempty"`
	AssignmentID string        `json:"assignment_id"`
	LeaseFor     time.Duration `json:"-"`
	Now          time.Time     `json:"-"`
}

type AssignmentNextResult struct {
	Assignment Assignment `json:"assignment,omitempty"`
	Contract   Contract   `json:"contract,omitempty"`
	Lease      Lease      `json:"lease,omitempty"`
	Idle       bool       `json:"idle"`
}

type WaitContractInput struct {
	LeaseID        string `json:"lease_id"`
	AgentID        string `json:"agent_id,omitempty"`
	Reason         string `json:"reason"`
	WaitingForRef  string `json:"waiting_for_ref,omitempty"`
	SessionRouteID string `json:"session_route_id,omitempty"`
}

type CompleteContractInput struct {
	LeaseID     string   `json:"lease_id"`
	AgentID     string   `json:"agent_id,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Summary     string   `json:"summary,omitempty"`
}

type CompleteContractResult struct {
	ContractID  string   `json:"contract_id"`
	Status      string   `json:"status"`
	EnvelopeID  string   `json:"envelope_id,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type SendMessageInput struct {
	LeaseID          string `json:"lease_id"`
	AgentID          string `json:"agent_id,omitempty"`
	ThreadID         string `json:"thread_id,omitempty"`
	RecipientAgentID string `json:"recipient_agent_id,omitempty"`
	Intent           string `json:"intent"`
	Body             string `json:"body"`
}

type SendMessageResult struct {
	ThreadID   string `json:"thread_id"`
	MessageID  string `json:"message_id"`
	EnvelopeID string `json:"envelope_id"`
	MailboxID  string `json:"mailbox_id,omitempty"`
}

type ResolveMailboxInput struct {
	AgentID     string `json:"agent_id,omitempty"`
	MailboxID   string `json:"mailbox_id"`
	FollowupRef string `json:"followup_ref"`
}

type SubmitReportInput struct {
	LeaseID string `json:"lease_id"`
	AgentID string `json:"agent_id,omitempty"`
	Summary string `json:"summary"`
	Content string `json:"content,omitempty"`
}

type CommunicationReadInput struct {
	AgentID    string `json:"agent_id,omitempty"`
	EnvelopeID string `json:"envelope_id,omitempty"`
	MailboxID  string `json:"mailbox_id,omitempty"`
}

type envelopeInsert struct {
	Kind             string
	SenderAgentID    string
	RecipientAgentID string
	RecipientRole    string
	ThreadID         string
	MessageID        string
	ContractID       string
	ParentEnvelopeID string
	Summary          string
	BodyInline       string
	BodyRef          string
	TriggerTurn      bool
	MetadataJSON     string
}

func (s *Service) AddContract(ctx context.Context, in AddContractInput) (AddContractResult, error) {
	if in.Title == "" {
		return AddContractResult{}, errors.New("contract.add: title is required")
	}
	if in.Objective == "" {
		return AddContractResult{}, errors.New("contract.add: objective is required")
	}
	if in.TargetAgentID == "" && in.TargetRole == "" {
		return AddContractResult{}, errors.New("contract.add: target agent or role is required")
	}
	requirements := in.CompletionRequirements
	if len(requirements) == 0 {
		requirements = []string{"report"}
	}
	reqJSON, err := json.Marshal(map[string][]string{"required_evidence": requirements})
	if err != nil {
		return AddContractResult{}, err
	}

	var result AddContractResult
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		issuerAgentID := in.IssuerAgentID
		issuerContractID := ""
		teamID := s.teamID
		teamVersion := s.teamVersion
		if in.IssuerLeaseID != "" {
			scope, err := activeLeaseScope(ctx, tx, in.IssuerLeaseID, in.IssuerAgentID)
			if err != nil {
				return err
			}
			issuerAgentID = scope.Lease.AgentID
			issuerContractID = scope.Contract.ID
			teamID = scope.Contract.TeamID
			teamVersion = scope.Contract.TeamVersion
		}
		contractID, err := ids.New("ctr")
		if err != nil {
			return err
		}
		assignmentID, err := ids.New("asg")
		if err != nil {
			return err
		}
		policy := s.communicationPolicyForTx(ctx, tx, teamID, teamVersion)
		if in.IssuerLeaseID != "" && !policy.AllowFollowupTask {
			return addContractRejectedErr{response: capability.Rejected[AddContractResult](
				"FOLLOWUP_TASK_DISABLED",
				"TeamConfig communication policy does not allow follow-up task contracts",
				capability.WithRepairHint("send a message or ask the coordinator to enable allow_followup_task"),
				capability.WithAllowedNextActions("message.send", "contract.current"),
				capability.WithRetryable(false),
			)}
		}
		targetKind := "agent"
		targetID := in.TargetAgentID
		assigneeAgent := in.TargetAgentID
		assigneeRole := ""
		if targetID == "" {
			targetKind = "role"
			targetID = in.TargetRole
			assigneeRole = in.TargetRole
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO work_contracts (
  id, tenant_id, title, objective, issuer_agent_id, issuer_contract_id,
  target_kind, target_id, status, completion_requirements_json,
  acceptance_policy_json, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, 'open', ?, '{}', ?, ?)`,
			contractID, in.Title, in.Objective, issuerAgentID, issuerContractID,
			targetKind, targetID, string(reqJSON), now, now,
		); err != nil {
			return fmt.Errorf("insert contract: %w", err)
		}
		if teamID != "" && teamVersion > 0 {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO contract_team_scopes (
  contract_id, tenant_id, team_id, team_version, source, created_at
) VALUES (?, 'default', ?, ?, 'contract.add', ?)`,
				contractID, teamID, teamVersion, now,
			); err != nil {
				return fmt.Errorf("bind contract team scope: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO assignments (
  id, tenant_id, contract_id, assignee_agent_id, assignee_role, state,
  priority, reason, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, 'queued', 0, 'new_contract', ?, ?)`,
			assignmentID, contractID, assigneeAgent, assigneeRole, now, now,
		); err != nil {
			return fmt.Errorf("insert assignment: %w", err)
		}
		triggerTurn := policy.TriggerTurn("task")
		envelopeID, err := insertEnvelopeTx(ctx, tx, envelopeInsert{
			Kind:             "task",
			SenderAgentID:    firstNonEmpty(issuerAgentID, "operator"),
			RecipientAgentID: assigneeAgent,
			RecipientRole:    assigneeRole,
			ContractID:       contractID,
			Summary:          in.Title,
			BodyInline:       in.Objective,
			TriggerTurn:      triggerTurn,
		})
		if err != nil {
			return fmt.Errorf("insert task envelope: %w", err)
		}
		mailboxID, err := ids.New("mbx")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO mailbox_items (
  id, tenant_id, recipient_agent_id, recipient_role, envelope_id, reason,
  contract_id, state, trigger_turn, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, 'task_assigned', ?, 'pending', ?, ?, ?)`,
			mailboxID, nullable(assigneeAgent), nullable(assigneeRole), envelopeID,
			contractID, boolInt(triggerTurn), now, now,
		); err != nil {
			return fmt.Errorf("insert task mailbox: %w", err)
		}
		if _, err := appendFact(ctx, tx, "contract.created", "work_contract", contractID, map[string]string{"assignment_id": assignmentID, "envelope_id": envelopeID}); err != nil {
			return err
		}
		if _, err := appendFact(ctx, tx, "mailbox.created", "mailbox_item", mailboxID, map[string]string{"reason": "task_assigned", "envelope_id": envelopeID}); err != nil {
			return err
		}
		result = AddContractResult{ContractID: contractID, AssignmentID: assignmentID, EnvelopeID: envelopeID, MailboxID: mailboxID}
		return nil
	})
	return result, err
}

func (s *Service) AssignmentNext(ctx context.Context, in AssignmentNextInput) (AssignmentNextResult, error) {
	if in.AgentID == "" {
		return AssignmentNextResult{}, errors.New("assignment.next: agent id is required")
	}
	if in.LeaseFor <= 0 {
		in.LeaseFor = time.Hour
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var out AssignmentNextResult
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if existing, ok, err := activeLeaseForAgent(ctx, tx, in.AgentID, now); err != nil {
			return err
		} else if ok {
			out = existing
			return nil
		}
		assignment, contract, ok, err := nextQueuedAssignment(ctx, tx, in.AgentID)
		if err != nil || !ok {
			out.Idle = true
			return err
		}
		var claimErr error
		out, claimErr = claimAssignmentTx(ctx, tx, in.AgentID, assignment, contract, in.LeaseFor, now)
		return claimErr
	})
	return out, err
}

func (s *Service) AssignmentClaim(ctx context.Context, in AssignmentClaimInput) (AssignmentNextResult, error) {
	if in.AgentID == "" {
		return AssignmentNextResult{}, errors.New("assignment.claim: agent id is required")
	}
	if in.AssignmentID == "" {
		return AssignmentNextResult{}, errors.New("assignment.claim: assignment id is required")
	}
	if in.LeaseFor <= 0 {
		in.LeaseFor = time.Hour
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var out AssignmentNextResult
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if existing, ok, err := activeLeaseForAgent(ctx, tx, in.AgentID, now); err != nil {
			return err
		} else if ok {
			if existing.Assignment.ID != in.AssignmentID {
				return ErrAssignmentBusy
			}
			out = existing
			return nil
		}
		assignment, contract, err := assignmentByID(ctx, tx, in.AssignmentID)
		if err != nil {
			return err
		}
		var claimErr error
		out, claimErr = claimAssignmentTx(ctx, tx, in.AgentID, assignment, contract, in.LeaseFor, now)
		return claimErr
	})
	return out, err
}

func (s *Service) AssignmentWatch(ctx context.Context, agentID string) ([]Assignment, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, contract_id, COALESCE(assignee_agent_id, ''), COALESCE(assignee_role, ''), state, COALESCE(session_route_id, '')
FROM assignments
WHERE state = 'queued' AND (assignee_agent_id = ? OR assignee_role = ?)
ORDER BY priority DESC, created_at ASC`, agentID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Assignment{}
	for rows.Next() {
		var a Assignment
		if err := rows.Scan(&a.ID, &a.ContractID, &a.AssigneeAgent, &a.AssigneeRole, &a.State, &a.SessionRouteID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) CurrentContract(ctx context.Context, leaseID, agentID string) (Contract, error) {
	return s.scopedContract(ctx, leaseID, agentID)
}

func (s *Service) ContractContext(ctx context.Context, leaseID, agentID string) (ContractContext, error) {
	contract, err := s.scopedContract(ctx, leaseID, agentID)
	if err != nil {
		return ContractContext{}, err
	}
	evidence, err := s.listEvidence(ctx, contract.ID)
	if err != nil {
		return ContractContext{}, err
	}
	return ContractContext{Contract: contract, Evidence: evidence}, nil
}

type ContractContext struct {
	Contract Contract   `json:"contract"`
	Evidence []Evidence `json:"evidence"`
}

func (s *Service) WaitContract(ctx context.Context, in WaitContractInput) (Assignment, error) {
	if in.Reason == "" {
		return Assignment{}, errors.New("contract.wait: reason is required")
	}
	var assignment Assignment
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		scope, err := activeLeaseScope(ctx, tx, in.LeaseID, in.AgentID)
		if err != nil {
			return err
		}
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
UPDATE assignments SET state = 'waiting', session_route_id = ?, updated_at = ? WHERE id = ?`,
			in.SessionRouteID, now, scope.Assignment.ID,
		); err != nil {
			return fmt.Errorf("mark assignment waiting: %w", err)
		}
		assignment = scope.Assignment
		assignment.State = "waiting"
		assignment.SessionRouteID = in.SessionRouteID
		_, err = appendFact(ctx, tx, "contract.waiting", "work_contract", scope.Contract.ID, map[string]string{
			"assignment_id":   scope.Assignment.ID,
			"waiting_for_ref": in.WaitingForRef,
		})
		return err
	})
	return assignment, err
}

func (s *Service) CompleteContract(ctx context.Context, in CompleteContractInput) capability.Response[CompleteContractResult] {
	var result CompleteContractResult
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		scope, err := activeLeaseScope(ctx, tx, in.LeaseID, in.AgentID)
		if err != nil {
			return err
		}
		evidenceIDs, requirements, err := bindRequiredEvidence(ctx, tx, scope.Contract.ID, in.LeaseID, in.EvidenceIDs)
		if err != nil {
			return err
		}
		if missing := missingRequiredEvidence(requirements); len(missing) > 0 {
			return rejectedErr{response: missingEvidenceResponse[CompleteContractResult](scope.Contract.ID, in.LeaseID, requirements)}
		}
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE work_contracts SET status = 'satisfied', updated_at = ? WHERE id = ?`, now, scope.Contract.ID); err != nil {
			return fmt.Errorf("satisfy contract: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'released', updated_at = ? WHERE id = ?`, now, in.LeaseID); err != nil {
			return fmt.Errorf("release lease: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assignments SET state = 'returned', updated_at = ? WHERE id = ?`, now, scope.Assignment.ID); err != nil {
			return fmt.Errorf("return assignment: %w", err)
		}
		if err := convergeReleasedLeaseBookkeeping(ctx, tx, in.LeaseID, now); err != nil {
			return err
		}
		resultSummary := strings.TrimSpace(in.Summary)
		if resultSummary == "" {
			resultSummary = "contract satisfied"
		}
		policy := s.communicationPolicyForTx(ctx, tx, scope.Contract.TeamID, scope.Contract.TeamVersion)
		triggerTurn := policy.TriggerTurn("result")
		parentEnvelopeID, err := taskEnvelopeForContract(ctx, tx, scope.Contract.ID)
		if err != nil {
			return err
		}
		envelopeID, err := insertEnvelopeTx(ctx, tx, envelopeInsert{
			Kind:             "result",
			SenderAgentID:    scope.Lease.AgentID,
			RecipientAgentID: scope.Contract.IssuerAgentID,
			ContractID:       scope.Contract.ID,
			ParentEnvelopeID: parentEnvelopeID,
			Summary:          resultSummary,
			BodyInline:       resultBody(scope.Contract.ID, evidenceIDs, resultSummary),
			TriggerTurn:      triggerTurn,
		})
		if err != nil {
			return fmt.Errorf("insert result envelope: %w", err)
		}
		if scope.Contract.IssuerAgentID != "" && scope.Contract.IssuerContract != "" {
			if err := createChildCompletedMailbox(ctx, tx, scope.Contract, evidenceIDs, envelopeID, triggerTurn); err != nil {
				return err
			}
		}
		if _, err := appendFact(ctx, tx, "contract.satisfied", "work_contract", scope.Contract.ID, map[string]string{"lease_id": in.LeaseID, "envelope_id": envelopeID}); err != nil {
			return err
		}
		result = CompleteContractResult{ContractID: scope.Contract.ID, Status: "satisfied", EnvelopeID: envelopeID, EvidenceIDs: evidenceIDs}
		return nil
	})
	if err != nil {
		var rejected rejectedErr
		if errors.As(err, &rejected) {
			return rejected.response
		}
		return capability.Error[CompleteContractResult]("CONTRACT_COMPLETE_FAILED", err.Error(), false)
	}
	return capability.Accepted(result)
}

func convergeReleasedLeaseBookkeeping(ctx context.Context, tx *sql.Tx, leaseID, now string) error {
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
WHERE lease_id = ? AND status IN ('preparing', 'ready_to_launch', 'running')`,
		now, leaseID,
	); err != nil {
		return fmt.Errorf("complete released lease attempts: %w", err)
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
WHERE lease_id = ? AND state = 'active'`,
		now, leaseID,
	); err != nil {
		return fmt.Errorf("revoke released lease runtime tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE active_guards
SET state = 'released', updated_at = ?
WHERE lease_id = ? AND state = 'active'`,
		now, leaseID,
	); err != nil {
		return fmt.Errorf("release released lease active guards: %w", err)
	}
	return nil
}

func (s *Service) SubmitReport(ctx context.Context, in SubmitReportInput) (Evidence, error) {
	if in.Summary == "" {
		return Evidence{}, errors.New("report.submit: summary is required")
	}
	var evidence Evidence
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		scope, err := activeLeaseScope(ctx, tx, in.LeaseID, in.AgentID)
		if err != nil {
			return err
		}
		evidenceID, err := ids.New("evd")
		if err != nil {
			return err
		}
		contentRef := ""
		if in.Content != "" {
			object, err := s.objects.PutTx(ctx, tx, objects.PutInput{
				OwnerAgent:  in.AgentID,
				Content:     []byte(in.Content),
				ContentType: "text/plain; charset=utf-8",
			})
			if err != nil {
				return err
			}
			contentRef = object.Ref
		}
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
INSERT INTO evidence (
  id, tenant_id, kind, contract_id, produced_by, content_ref,
  inline_content, summary, created_at
) VALUES (?, 'default', 'report', ?, ?, ?, NULL, ?, ?)`,
			evidenceID, scope.Contract.ID, in.AgentID, nullable(contentRef), in.Summary, now,
		); err != nil {
			return fmt.Errorf("insert report evidence: %w", err)
		}
		evidence = Evidence{ID: evidenceID, Kind: "report", ContractID: scope.Contract.ID, ProducedBy: in.AgentID, ContentRef: contentRef, Summary: in.Summary}
		_, err = appendFact(ctx, tx, "evidence.report_submitted", "evidence", evidenceID, map[string]string{"contract_id": scope.Contract.ID})
		return err
	})
	return evidence, err
}

func (s *Service) SendMessage(ctx context.Context, in SendMessageInput) capability.Response[SendMessageResult] {
	var result SendMessageResult
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		scope, err := activeLeaseScope(ctx, tx, in.LeaseID, in.AgentID)
		if err != nil {
			return err
		}
		policy := s.communicationPolicyForTx(ctx, tx, scope.Contract.TeamID, scope.Contract.TeamVersion)
		if !policy.AllowDirectMessage {
			return messageRejectedErr{response: capability.Rejected[SendMessageResult](
				"DIRECT_MESSAGE_DISABLED",
				"TeamConfig communication policy does not allow direct messages",
				capability.WithRepairHint("use an allowed contract workflow or ask the coordinator to enable allow_direct_message"),
				capability.WithAllowedNextActions("contract.current", "contract.add"),
				capability.WithRetryable(false),
			)}
		}
		if policy.TaskRequiresContract && in.Intent == "task_request" {
			return messageRejectedErr{response: capability.Rejected[SendMessageResult](
				"TASK_REQUIRES_CONTRACT",
				"TeamConfig communication policy requires explicit task requests to use contract.add",
				capability.WithRepairHint("call contract.add with title, objective, target, and completion requirements"),
				capability.WithAllowedNextActions("contract.add", "message.send"),
				capability.WithRetryable(false),
			)}
		}
		threadID := in.ThreadID
		now := formatTime(time.Now())
		if threadID == "" {
			var err error
			threadID, err = ids.New("thr")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO threads (id, tenant_id, scope, subject, created_by, created_at)
VALUES (?, 'default', 'contract', ?, ?, ?)`,
				threadID, scope.Contract.ID, in.AgentID, now,
			); err != nil {
				return fmt.Errorf("insert thread: %w", err)
			}
		}
		messageID, err := ids.New("msg")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO messages (
  id, tenant_id, thread_id, sender_agent_id, body, references_json, intent, created_at
) VALUES (?, 'default', ?, ?, ?, '[]', ?, ?)`,
			messageID, threadID, in.AgentID, in.Body, in.Intent, now,
		); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		triggerTurn := policy.TriggerTurn("message")
		envelopeID, err := insertEnvelopeTx(ctx, tx, envelopeInsert{
			Kind:             "message",
			SenderAgentID:    in.AgentID,
			RecipientAgentID: in.RecipientAgentID,
			ThreadID:         threadID,
			MessageID:        messageID,
			ContractID:       scope.Contract.ID,
			Summary:          messageSummary(in.Intent, in.Body),
			BodyInline:       in.Body,
			TriggerTurn:      triggerTurn,
		})
		if err != nil {
			return fmt.Errorf("insert message envelope: %w", err)
		}
		mailboxID := ""
		if in.RecipientAgentID != "" {
			mailboxID, err = ids.New("mbx")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO mailbox_items (
  id, tenant_id, recipient_agent_id, envelope_id, reason, thread_id, message_id,
  contract_id, state, trigger_turn, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
				mailboxID, in.RecipientAgentID, envelopeID, mailboxReason(in.Intent),
				threadID, messageID, scope.Contract.ID, boolInt(triggerTurn), now, now,
			); err != nil {
				return fmt.Errorf("insert mailbox: %w", err)
			}
		}
		result = SendMessageResult{ThreadID: threadID, MessageID: messageID, EnvelopeID: envelopeID, MailboxID: mailboxID}
		_, err = appendFact(ctx, tx, "message.sent", "message", messageID, map[string]string{"contract_id": scope.Contract.ID, "envelope_id": envelopeID})
		return err
	})
	if err != nil {
		var rejected messageRejectedErr
		if errors.As(err, &rejected) {
			return rejected.response
		}
		return capability.Error[SendMessageResult]("MESSAGE_SEND_FAILED", err.Error(), false)
	}
	return capability.Accepted(result)
}

func (s *Service) MailboxList(ctx context.Context, agentID string) ([]MailboxItem, error) {
	return s.mailboxes(ctx, agentID, "")
}

func (s *Service) MailboxGet(ctx context.Context, agentID, mailboxID string) (MailboxItem, error) {
	items, err := s.mailboxes(ctx, agentID, mailboxID)
	if err != nil {
		return MailboxItem{}, err
	}
	if len(items) == 0 {
		return MailboxItem{}, sql.ErrNoRows
	}
	return items[0], nil
}

func (s *Service) MailboxResolve(ctx context.Context, in ResolveMailboxInput) capability.Response[MailboxItem] {
	if in.FollowupRef == "" {
		return capability.Rejected[MailboxItem](
			"MISSING_MAILBOX_FOLLOWUP",
			"mailbox.resolve requires a durable follow-up action reference",
			capability.WithCanonicalID("mailbox_id", in.MailboxID),
			capability.WithMissing("follow_up_action", "message.send"),
			capability.WithAllowedNextActions("message.send", "contract.add", "report.submit", "contract.complete"),
			capability.WithRetryable(true),
		)
	}
	var item MailboxItem
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		item, err = mailboxByID(ctx, tx, in.AgentID, in.MailboxID)
		if err != nil {
			return err
		}
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
UPDATE mailbox_items SET state = 'resolved', followup_ref = ?, updated_at = ? WHERE id = ? AND recipient_agent_id = ?`,
			in.FollowupRef, now, in.MailboxID, in.AgentID,
		); err != nil {
			return fmt.Errorf("resolve mailbox: %w", err)
		}
		item.State = "resolved"
		item.FollowupRef = in.FollowupRef
		_, err = appendFact(ctx, tx, "mailbox.resolved", "mailbox_item", in.MailboxID, map[string]string{"followup_ref": in.FollowupRef})
		return err
	})
	if err != nil {
		return capability.Error[MailboxItem]("MAILBOX_RESOLVE_FAILED", err.Error(), false)
	}
	return capability.Accepted(item)
}

func (s *Service) scopedContract(ctx context.Context, leaseID, agentID string) (Contract, error) {
	var contract Contract
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		scope, err := activeLeaseScope(ctx, tx, leaseID, agentID)
		if err != nil {
			return err
		}
		contract = scope.Contract
		return nil
	})
	return contract, err
}

func (s *Service) listEvidence(ctx context.Context, contractID string) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, kind, contract_id, produced_by, COALESCE(content_ref, ''), summary
FROM evidence
WHERE contract_id = ?
ORDER BY created_at ASC`, contractID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Evidence{}
	for rows.Next() {
		var ev Evidence
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.ContractID, &ev.ProducedBy, &ev.ContentRef, &ev.Summary); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Service) mailboxes(ctx context.Context, agentID, mailboxID string) ([]MailboxItem, error) {
	query := `
SELECT m.id, COALESCE(m.recipient_agent_id, ''), COALESCE(m.envelope_id, ''),
  COALESCE(e.kind, ''), m.reason, COALESCE(m.thread_id, ''),
  COALESCE(m.message_id, ''), COALESCE(m.contract_id, ''), COALESCE(m.session_route_id, ''),
  m.state, COALESCE(m.followup_ref, ''), COALESCE(m.trigger_turn, 1)
FROM mailbox_items m
LEFT JOIN agent_communication_envelopes e ON e.id = m.envelope_id
WHERE m.recipient_agent_id = ?`
	args := []any{agentID}
	if mailboxID != "" {
		query += " AND m.id = ?"
		args = append(args, mailboxID)
	} else {
		query += " AND m.state IN ('pending', 'claimed')"
	}
	query += " ORDER BY m.created_at ASC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailboxItem{}
	for rows.Next() {
		item, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type leaseScope struct {
	Lease      Lease
	Assignment Assignment
	Contract   Contract
}

func activeLeaseScope(ctx context.Context, tx *sql.Tx, leaseID, agentID string) (leaseScope, error) {
	var scope leaseScope
	var expiresRaw string
	err := tx.QueryRowContext(ctx, `
SELECT l.id, l.assignment_id, l.agent_id, COALESCE(l.session_route_id, ''), l.state, l.expires_at,
  a.id, a.contract_id, COALESCE(a.assignee_agent_id, ''), COALESCE(a.assignee_role, ''), a.state, COALESCE(a.session_route_id, ''),
  c.id, c.title, c.objective, COALESCE(c.issuer_agent_id, ''), COALESCE(c.issuer_contract_id, ''),
  c.target_kind, c.target_id, c.status, COALESCE(ts.team_id, ''), COALESCE(ts.team_version, 0)
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
JOIN work_contracts c ON c.id = a.contract_id
LEFT JOIN contract_team_scopes ts ON ts.contract_id = c.id
WHERE l.id = ? AND l.agent_id = ? AND l.state = 'active'`,
		leaseID, agentID,
	).Scan(
		&scope.Lease.ID, &scope.Lease.AssignmentID, &scope.Lease.AgentID, &scope.Lease.SessionRouteID, &scope.Lease.State, &expiresRaw,
		&scope.Assignment.ID, &scope.Assignment.ContractID, &scope.Assignment.AssigneeAgent, &scope.Assignment.AssigneeRole, &scope.Assignment.State, &scope.Assignment.SessionRouteID,
		&scope.Contract.ID, &scope.Contract.Title, &scope.Contract.Objective, &scope.Contract.IssuerAgentID, &scope.Contract.IssuerContract,
		&scope.Contract.TargetKind, &scope.Contract.TargetID, &scope.Contract.Status, &scope.Contract.TeamID, &scope.Contract.TeamVersion,
	)
	if err != nil {
		return leaseScope{}, fmt.Errorf("active lease scope: %w", err)
	}
	expiresAt, err := parseTime(expiresRaw)
	if err != nil {
		return leaseScope{}, err
	}
	if time.Now().After(expiresAt) {
		return leaseScope{}, errors.New("active lease scope: lease expired")
	}
	scope.Lease.ExpiresAt = expiresAt
	return scope, nil
}

func activeLeaseForAgent(ctx context.Context, tx *sql.Tx, agentID string, now time.Time) (AssignmentNextResult, bool, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT l.id, l.assignment_id, l.agent_id, COALESCE(l.session_route_id, ''), l.state, l.expires_at,
  a.id, a.contract_id, COALESCE(a.assignee_agent_id, ''), COALESCE(a.assignee_role, ''), a.state, COALESCE(a.session_route_id, ''),
  c.id, c.title, c.objective, COALESCE(c.issuer_agent_id, ''), COALESCE(c.issuer_contract_id, ''),
  c.target_kind, c.target_id, c.status, COALESCE(ts.team_id, ''), COALESCE(ts.team_version, 0)
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
JOIN work_contracts c ON c.id = a.contract_id
LEFT JOIN contract_team_scopes ts ON ts.contract_id = c.id
WHERE l.agent_id = ? AND l.state = 'active' AND l.expires_at > ?
ORDER BY l.created_at ASC
LIMIT 1`, agentID, formatTime(now))
	if err != nil {
		return AssignmentNextResult{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return AssignmentNextResult{}, false, rows.Err()
	}
	var out AssignmentNextResult
	var expiresRaw string
	if err := rows.Scan(
		&out.Lease.ID, &out.Lease.AssignmentID, &out.Lease.AgentID, &out.Lease.SessionRouteID, &out.Lease.State, &expiresRaw,
		&out.Assignment.ID, &out.Assignment.ContractID, &out.Assignment.AssigneeAgent, &out.Assignment.AssigneeRole, &out.Assignment.State, &out.Assignment.SessionRouteID,
		&out.Contract.ID, &out.Contract.Title, &out.Contract.Objective, &out.Contract.IssuerAgentID, &out.Contract.IssuerContract,
		&out.Contract.TargetKind, &out.Contract.TargetID, &out.Contract.Status, &out.Contract.TeamID, &out.Contract.TeamVersion,
	); err != nil {
		return AssignmentNextResult{}, false, err
	}
	expiresAt, err := parseTime(expiresRaw)
	if err != nil {
		return AssignmentNextResult{}, false, err
	}
	out.Lease.ExpiresAt = expiresAt
	return out, true, rows.Err()
}

func nextQueuedAssignment(ctx context.Context, tx *sql.Tx, agentID string) (Assignment, Contract, bool, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT a.id, a.contract_id, COALESCE(a.assignee_agent_id, ''), COALESCE(a.assignee_role, ''), a.state, COALESCE(a.session_route_id, ''),
  c.id, c.title, c.objective, COALESCE(c.issuer_agent_id, ''), COALESCE(c.issuer_contract_id, ''),
  c.target_kind, c.target_id, c.status, COALESCE(ts.team_id, ''), COALESCE(ts.team_version, 0)
FROM assignments a
JOIN work_contracts c ON c.id = a.contract_id
LEFT JOIN contract_team_scopes ts ON ts.contract_id = c.id
WHERE a.state = 'queued' AND (a.assignee_agent_id = ? OR a.assignee_role = ?)
ORDER BY a.priority DESC, a.created_at ASC
LIMIT 1`, agentID, agentID)
	if err != nil {
		return Assignment{}, Contract{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Assignment{}, Contract{}, false, rows.Err()
	}
	var assignment Assignment
	var contract Contract
	if err := rows.Scan(
		&assignment.ID, &assignment.ContractID, &assignment.AssigneeAgent, &assignment.AssigneeRole, &assignment.State, &assignment.SessionRouteID,
		&contract.ID, &contract.Title, &contract.Objective, &contract.IssuerAgentID, &contract.IssuerContract,
		&contract.TargetKind, &contract.TargetID, &contract.Status, &contract.TeamID, &contract.TeamVersion,
	); err != nil {
		return Assignment{}, Contract{}, false, err
	}
	return assignment, contract, true, rows.Err()
}

func assignmentByID(ctx context.Context, tx *sql.Tx, assignmentID string) (Assignment, Contract, error) {
	row := tx.QueryRowContext(ctx, `
SELECT a.id, a.contract_id, COALESCE(a.assignee_agent_id, ''), COALESCE(a.assignee_role, ''), a.state, COALESCE(a.session_route_id, ''),
  c.id, c.title, c.objective, COALESCE(c.issuer_agent_id, ''), COALESCE(c.issuer_contract_id, ''),
  c.target_kind, c.target_id, c.status, COALESCE(ts.team_id, ''), COALESCE(ts.team_version, 0)
FROM assignments a
JOIN work_contracts c ON c.id = a.contract_id
LEFT JOIN contract_team_scopes ts ON ts.contract_id = c.id
WHERE a.id = ?`, assignmentID)
	var assignment Assignment
	var contract Contract
	if err := row.Scan(
		&assignment.ID, &assignment.ContractID, &assignment.AssigneeAgent, &assignment.AssigneeRole, &assignment.State, &assignment.SessionRouteID,
		&contract.ID, &contract.Title, &contract.Objective, &contract.IssuerAgentID, &contract.IssuerContract,
		&contract.TargetKind, &contract.TargetID, &contract.Status, &contract.TeamID, &contract.TeamVersion,
	); errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, Contract{}, ErrAssignmentNotFound
	} else if err != nil {
		return Assignment{}, Contract{}, err
	}
	return assignment, contract, nil
}

func claimAssignmentTx(ctx context.Context, tx *sql.Tx, agentID string, assignment Assignment, contract Contract, leaseFor time.Duration, now time.Time) (AssignmentNextResult, error) {
	if assignment.State != "queued" || (assignment.AssigneeAgent != agentID && assignment.AssigneeRole != agentID) {
		return AssignmentNextResult{}, ErrAssignmentNotClaimable
	}
	nowRaw := formatTime(now)
	result, err := tx.ExecContext(ctx, `
UPDATE assignments
SET state = 'claimed', updated_at = ?
WHERE id = ?
  AND state = 'queued'
  AND (assignee_agent_id = ? OR assignee_role = ?)`,
		nowRaw, assignment.ID, agentID, agentID,
	)
	if err != nil {
		return AssignmentNextResult{}, fmt.Errorf("claim assignment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AssignmentNextResult{}, fmt.Errorf("claim assignment rows affected: %w", err)
	}
	if affected != 1 {
		return AssignmentNextResult{}, ErrAssignmentNotClaimable
	}
	leaseID, err := ids.New("lease")
	if err != nil {
		return AssignmentNextResult{}, err
	}
	expires := now.Add(leaseFor)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO leases (
  id, tenant_id, assignment_id, agent_id, session_route_id, state,
  expires_at, created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, 'active', ?, ?, ?)`,
		leaseID, assignment.ID, agentID, assignment.SessionRouteID,
		formatTime(expires), nowRaw, nowRaw,
	); err != nil {
		return AssignmentNextResult{}, fmt.Errorf("insert lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE mailbox_items
SET state = 'resolved', followup_ref = ?, updated_at = ?
WHERE contract_id = ?
  AND reason = 'task_assigned'
  AND state = 'pending'
  AND (recipient_agent_id = ? OR recipient_role = ?)`,
		"lease:"+leaseID, nowRaw, assignment.ContractID, agentID, agentID,
	); err != nil {
		return AssignmentNextResult{}, fmt.Errorf("resolve claimed task mailbox: %w", err)
	}
	lease := Lease{ID: leaseID, AssignmentID: assignment.ID, AgentID: agentID, SessionRouteID: assignment.SessionRouteID, State: "active", ExpiresAt: expires}
	assignment.State = "claimed"
	out := AssignmentNextResult{Assignment: assignment, Contract: contract, Lease: lease}
	_, err = appendFact(ctx, tx, "assignment.claimed", "assignment", assignment.ID, map[string]string{"lease_id": leaseID})
	return out, err
}

type evidenceRequirementStatus struct {
	Kind       string
	PresentIDs []string
}

func bindRequiredEvidence(ctx context.Context, tx *sql.Tx, contractID, leaseID string, evidenceIDs []string) ([]string, []evidenceRequirementStatus, error) {
	required, err := requiredEvidence(ctx, tx, contractID)
	if err != nil {
		return nil, nil, err
	}
	bound := make([]string, 0, len(evidenceIDs)+len(required))
	seenEvidence := make(map[string]bool)
	presentByKind := make(map[string][]string)
	requiredKinds := make(map[string]bool, len(required))
	for _, kind := range required {
		requiredKinds[kind] = true
	}
	for _, id := range evidenceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "EVIDENCE_NOT_FOUND", "contract.complete evidence_ids contains an empty evidence id", id)}
		}
		if seenEvidence[id] {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "AMBIGUOUS_REQUIRED_EVIDENCE", "contract.complete evidence_ids contains a duplicate evidence id", id)}
		}
		var kind, evidenceContractID string
		err := tx.QueryRowContext(ctx, `SELECT kind, contract_id FROM evidence WHERE id = ?`, id).Scan(&kind, &evidenceContractID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "EVIDENCE_NOT_FOUND", "contract.complete evidence id does not exist", id)}
		}
		if err != nil {
			return nil, nil, err
		}
		if evidenceContractID != contractID {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "EVIDENCE_CONTRACT_MISMATCH", "contract.complete evidence belongs to another contract", id)}
		}
		if !requiredKinds[kind] {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "EVIDENCE_KIND_MISMATCH", "contract.complete evidence kind is not required by this contract", id)}
		}
		if len(presentByKind[kind]) > 0 {
			return nil, nil, rejectedErr{response: invalidEvidenceBindingResponse(contractID, leaseID, "AMBIGUOUS_REQUIRED_EVIDENCE", "contract.complete requires exactly one evidence id for each required kind", id)}
		}
		bound = append(bound, id)
		seenEvidence[id] = true
		presentByKind[kind] = append(presentByKind[kind], id)
	}
	requirements := make([]evidenceRequirementStatus, 0, len(required))
	for _, kind := range required {
		status := evidenceRequirementStatus{
			Kind:       kind,
			PresentIDs: append([]string(nil), presentByKind[kind]...),
		}
		if len(status.PresentIDs) == 0 {
			available, err := latestEvidenceIDsForKind(ctx, tx, contractID, kind)
			if err != nil {
				return nil, nil, err
			}
			if len(available) > 1 {
				return nil, nil, rejectedErr{response: ambiguousEvidenceResponse(contractID, leaseID, kind, len(available))}
			}
			if len(available) == 1 {
				chosen := available[0]
				status.PresentIDs = []string{chosen}
				if !seenEvidence[chosen] {
					bound = append(bound, chosen)
					seenEvidence[chosen] = true
				}
			}
		}
		requirements = append(requirements, status)
	}
	return bound, requirements, nil
}

func invalidEvidenceBindingResponse(contractID, leaseID, code, message, evidenceID string) capability.Response[CompleteContractResult] {
	opts := []capability.RejectedOption{
		capability.WithCanonicalID("contract_id", contractID),
		capability.WithCanonicalID("lease_id", leaseID),
		capability.WithRepairHint("retry contract.complete with exactly one evidence id from this contract for each required evidence kind"),
		capability.WithAllowedNextActions("contract.context", "contract.complete"),
		capability.WithRetryable(true),
	}
	if evidenceID != "" {
		opts = append(opts, capability.WithCanonicalID("evidence_id", evidenceID))
	}
	return capability.Rejected[CompleteContractResult](code, message, opts...)
}

func ambiguousEvidenceResponse(contractID, leaseID, kind string, candidateCount int) capability.Response[CompleteContractResult] {
	return capability.Rejected[CompleteContractResult](
		"AMBIGUOUS_REQUIRED_EVIDENCE",
		fmt.Sprintf("contract.complete found %d %s evidence candidates; bind one explicitly", candidateCount, kind),
		capability.WithCanonicalID("contract_id", contractID),
		capability.WithCanonicalID("lease_id", leaseID),
		capability.WithRepairHint("retry contract.complete with evidence_ids containing exactly one evidence id for each required kind"),
		capability.WithAllowedNextActions("contract.context", "contract.complete"),
		capability.WithRetryable(true),
	)
}

func latestEvidenceIDsForKind(ctx context.Context, tx *sql.Tx, contractID, kind string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM evidence
WHERE contract_id = ? AND kind = ?
ORDER BY created_at DESC, id DESC`,
		contractID, kind,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func missingRequiredEvidence(requirements []evidenceRequirementStatus) []evidenceRequirementStatus {
	var missing []evidenceRequirementStatus
	for _, req := range requirements {
		if len(req.PresentIDs) == 0 {
			missing = append(missing, req)
		}
	}
	return missing
}

func requiredEvidence(ctx context.Context, tx *sql.Tx, contractID string) ([]string, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT completion_requirements_json FROM work_contracts WHERE id = ?`, contractID).Scan(&raw); err != nil {
		return nil, err
	}
	var decoded struct {
		RequiredEvidence []string `json:"required_evidence"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, err
	}
	if len(decoded.RequiredEvidence) == 0 {
		return []string{"report"}, nil
	}
	seen := make(map[string]bool, len(decoded.RequiredEvidence))
	result := make([]string, 0, len(decoded.RequiredEvidence))
	for _, kind := range decoded.RequiredEvidence {
		kind = strings.TrimSpace(kind)
		if kind != "" && !seen[kind] {
			seen[kind] = true
			result = append(result, kind)
		}
	}
	if len(result) == 0 {
		return []string{"report"}, nil
	}
	return result, nil
}

func createChildCompletedMailbox(ctx context.Context, tx *sql.Tx, contract Contract, evidenceIDs []string, envelopeID string, triggerTurn bool) error {
	mailboxID, err := ids.New("mbx")
	if err != nil {
		return err
	}
	sessionRouteID := ""
	_ = tx.QueryRowContext(ctx, `
SELECT COALESCE(session_route_id, '') FROM assignments WHERE contract_id = ? ORDER BY updated_at DESC LIMIT 1`,
		contract.IssuerContract,
	).Scan(&sessionRouteID)
	now := formatTime(time.Now())
	_, err = tx.ExecContext(ctx, `
INSERT INTO mailbox_items (
  id, tenant_id, recipient_agent_id, envelope_id, reason, contract_id,
  session_route_id, state, trigger_turn, followup_ref, created_at, updated_at
) VALUES (?, 'default', ?, ?, 'child_completed', ?, ?, 'pending', ?, ?, ?, ?)`,
		mailboxID, contract.IssuerAgentID, envelopeID, contract.IssuerContract, sessionRouteID,
		boolInt(triggerTurn), "child_contract:"+contract.ID+";evidence:"+strings.Join(evidenceIDs, ","), now, now,
	)
	if err != nil {
		return fmt.Errorf("insert child_completed mailbox: %w", err)
	}
	_, err = appendFact(ctx, tx, "mailbox.created", "mailbox_item", mailboxID, map[string]string{"reason": "child_completed", "envelope_id": envelopeID})
	return err
}

func mailboxByID(ctx context.Context, tx *sql.Tx, agentID, mailboxID string) (MailboxItem, error) {
	row := tx.QueryRowContext(ctx, `
SELECT m.id, COALESCE(m.recipient_agent_id, ''), COALESCE(m.envelope_id, ''),
  COALESCE(e.kind, ''), m.reason, COALESCE(m.thread_id, ''),
  COALESCE(m.message_id, ''), COALESCE(m.contract_id, ''), COALESCE(m.session_route_id, ''),
  m.state, COALESCE(m.followup_ref, ''), COALESCE(m.trigger_turn, 1)
FROM mailbox_items m
LEFT JOIN agent_communication_envelopes e ON e.id = m.envelope_id
WHERE m.id = ? AND m.recipient_agent_id = ?`, mailboxID, agentID)
	return scanMailbox(row)
}

func scanMailbox(row interface{ Scan(...any) error }) (MailboxItem, error) {
	var item MailboxItem
	err := row.Scan(
		&item.ID, &item.RecipientAgentID, &item.EnvelopeID, &item.EnvelopeKind,
		&item.Reason, &item.ThreadID, &item.MessageID, &item.ContractID,
		&item.SessionRouteID, &item.State, &item.FollowupRef, &item.TriggerTurn,
	)
	return item, err
}

func (s *Service) ReadCommunication(ctx context.Context, in CommunicationReadInput) capability.Response[AgentCommunicationEnvelope] {
	if in.AgentID == "" {
		return communicationDenied("agent identity is required")
	}
	if in.EnvelopeID == "" && in.MailboxID == "" {
		return capability.Rejected[AgentCommunicationEnvelope](
			"COMMUNICATION_REFERENCE_REQUIRED",
			"communication.read requires envelope_id or mailbox_id",
			capability.WithRepairHint("retry with an envelope_id from mailbox.list/get or a mailbox_id"),
			capability.WithAllowedNextActions("mailbox.list", "mailbox.get", "communication.read"),
			capability.WithRetryable(false),
		)
	}
	envelope, authorized, err := s.communicationEnvelopeForAgent(ctx, in)
	if err != nil {
		return capability.Error[AgentCommunicationEnvelope]("COMMUNICATION_READ_FAILED", err.Error(), false)
	}
	if !authorized {
		return communicationDenied("communication envelope is not available to this agent")
	}
	return capability.Accepted(envelope)
}

func (s *Service) communicationEnvelopeForAgent(ctx context.Context, in CommunicationReadInput) (AgentCommunicationEnvelope, bool, error) {
	query := `
SELECT e.id, e.kind, e.sender_agent_id, COALESCE(e.recipient_agent_id, ''),
  COALESCE(e.recipient_role, ''), COALESCE(e.thread_id, ''),
  COALESCE(e.message_id, ''), COALESCE(e.contract_id, ''),
  COALESCE(e.parent_envelope_id, ''), e.summary, COALESCE(e.body_inline, ''),
  COALESCE(e.body_ref, ''), e.trigger_turn, e.created_at,
  COALESCE(c.issuer_agent_id, ''), COALESCE(c.target_kind, ''),
  COALESCE(c.target_id, ''), COALESCE(m.recipient_agent_id, '')
FROM agent_communication_envelopes e
LEFT JOIN work_contracts c ON c.id = e.contract_id
LEFT JOIN mailbox_items m ON m.envelope_id = e.id AND m.id = ?
WHERE e.id = ?`
	envelopeID := in.EnvelopeID
	if envelopeID == "" {
		var err error
		envelopeID, err = envelopeIDForMailbox(ctx, s.db, in.MailboxID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return AgentCommunicationEnvelope{}, false, nil
			}
			return AgentCommunicationEnvelope{}, false, err
		}
	}
	row := s.db.QueryRowContext(ctx, query, in.MailboxID, envelopeID)
	var envelope AgentCommunicationEnvelope
	var triggerTurn int
	var createdRaw string
	var issuerAgentID, targetKind, targetID, mailboxRecipient string
	if err := row.Scan(
		&envelope.ID,
		&envelope.Kind,
		&envelope.SenderAgentID,
		&envelope.RecipientAgentID,
		&envelope.RecipientRole,
		&envelope.ThreadID,
		&envelope.MessageID,
		&envelope.ContractID,
		&envelope.ParentEnvelopeID,
		&envelope.Summary,
		&envelope.BodyInline,
		&envelope.BodyRef,
		&triggerTurn,
		&createdRaw,
		&issuerAgentID,
		&targetKind,
		&targetID,
		&mailboxRecipient,
	); errors.Is(err, sql.ErrNoRows) {
		return AgentCommunicationEnvelope{}, false, nil
	} else if err != nil {
		return AgentCommunicationEnvelope{}, false, err
	}
	createdAt, err := parseTime(createdRaw)
	if err != nil {
		return AgentCommunicationEnvelope{}, false, err
	}
	envelope.TriggerTurn = triggerTurn != 0
	envelope.CreatedAt = createdAt
	authorized := in.AgentID == envelope.SenderAgentID ||
		in.AgentID == envelope.RecipientAgentID ||
		in.AgentID == envelope.RecipientRole ||
		in.AgentID == issuerAgentID ||
		(targetKind == "agent" && in.AgentID == targetID) ||
		in.AgentID == mailboxRecipient
	return envelope, authorized, nil
}

func envelopeIDForMailbox(ctx context.Context, db *sql.DB, mailboxID string) (string, error) {
	var envelopeID string
	err := db.QueryRowContext(ctx, `
SELECT COALESCE(envelope_id, '')
FROM mailbox_items
WHERE id = ?`, mailboxID).Scan(&envelopeID)
	if err != nil {
		return "", err
	}
	if envelopeID == "" {
		return "", sql.ErrNoRows
	}
	return envelopeID, nil
}

func communicationDenied(message string) capability.Response[AgentCommunicationEnvelope] {
	return capability.Rejected[AgentCommunicationEnvelope](
		"COMMUNICATION_ACCESS_DENIED",
		message,
		capability.WithRepairHint("read only mailbox items and envelopes addressed to your agent or contract scope"),
		capability.WithAllowedNextActions("mailbox.list", "mailbox.get"),
		capability.WithRetryable(false),
	)
}

func missingEvidenceResponse[T any](contractID, leaseID string, requirements []evidenceRequirementStatus) capability.Response[T] {
	missing := missingRequiredEvidence(requirements)
	opts := []capability.RejectedOption{
		capability.WithCanonicalID("contract_id", contractID),
		capability.WithCanonicalID("lease_id", leaseID),
		capability.WithRepairHint(missingEvidenceRepairHint(requirements)),
		capability.WithAllowedNextActions(missingEvidenceNextActions(missing)...),
		capability.WithRetryable(true),
	}
	for _, req := range requirements {
		if len(req.PresentIDs) > 0 {
			opts = append(opts, capability.WithCanonicalID("available_"+canonicalEvidenceKey(req.Kind)+"_evidence_id", req.PresentIDs[0]))
			continue
		}
		opts = append(opts, capability.WithMissing(req.Kind, evidenceAction(req.Kind)))
	}
	return capability.Rejected[T](
		"MISSING_REQUIRED_EVIDENCE",
		"contract.complete requires all required evidence refs before this contract can be satisfied",
		opts...,
	)
}

func missingEvidenceRepairHint(requirements []evidenceRequirementStatus) string {
	var available []string
	for _, req := range requirements {
		if len(req.PresentIDs) > 0 {
			available = append(available, req.Kind+"="+req.PresentIDs[0])
		}
	}
	hint := "submit the missing evidence, then retry contract.complete with evidence_ids containing one evidence ref for each required kind"
	if len(available) > 0 {
		hint += "; available required evidence refs: " + strings.Join(available, ", ")
	}
	return hint
}

func missingEvidenceNextActions(missing []evidenceRequirementStatus) []string {
	seen := make(map[string]bool)
	actions := make([]string, 0, len(missing)+3)
	add := func(action string) {
		if action == "" || seen[action] {
			return
		}
		seen[action] = true
		actions = append(actions, action)
	}
	for _, req := range missing {
		add(evidenceAction(req.Kind))
	}
	add("contract.context")
	add("message.send")
	add("contract.complete")
	return actions
}

func evidenceAction(kind string) string {
	switch kind {
	case "report":
		return "report.submit"
	case "validation_assessment":
		return "validation.assessment"
	case "command_run":
		return "command.run"
	case "changeset":
		return "changeset.submit"
	default:
		return kind + ".submit"
	}
}

func canonicalEvidenceKey(kind string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_", " ", "_")
	return replacer.Replace(kind)
}

type rejectedErr struct {
	response capability.Response[CompleteContractResult]
}

func (e rejectedErr) Error() string {
	return e.response.ErrorCode
}

type addContractRejectedErr struct {
	response capability.Response[AddContractResult]
}

func (e addContractRejectedErr) Error() string {
	return e.response.ErrorCode
}

type messageRejectedErr struct {
	response capability.Response[SendMessageResult]
}

func (e messageRejectedErr) Error() string {
	return e.response.ErrorCode
}

func mailboxReason(intent string) string {
	switch intent {
	case "question":
		return "question"
	case "repair":
		return "repair_required"
	default:
		return "feedback"
	}
}

func insertEnvelopeTx(ctx context.Context, tx *sql.Tx, in envelopeInsert) (string, error) {
	envelopeID, err := ids.New("env")
	if err != nil {
		return "", err
	}
	metadataJSON := strings.TrimSpace(in.MetadataJSON)
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	now := formatTime(time.Now())
	_, err = tx.ExecContext(ctx, `
INSERT INTO agent_communication_envelopes (
  id, tenant_id, kind, sender_agent_id, recipient_agent_id, recipient_role,
  thread_id, message_id, contract_id, parent_envelope_id, summary,
  body_inline, body_ref, trigger_turn, metadata_json, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		envelopeID, in.Kind, in.SenderAgentID, nullable(in.RecipientAgentID),
		nullable(in.RecipientRole), nullable(in.ThreadID), nullable(in.MessageID),
		nullable(in.ContractID), nullable(in.ParentEnvelopeID), in.Summary,
		nullable(in.BodyInline), nullable(in.BodyRef), boolInt(in.TriggerTurn),
		metadataJSON, now,
	)
	if err != nil {
		return "", err
	}
	return envelopeID, nil
}

func (s *Service) communicationPolicyForTx(ctx context.Context, tx *sql.Tx, teamID string, teamVersion int) teamconfig.CommunicationConfig {
	if teamID == "" || teamVersion <= 0 {
		return teamconfig.DefaultCommunicationConfig()
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `
SELECT config_json
FROM team_config_versions
WHERE team_id = ? AND version = ?`, teamID, teamVersion).Scan(&raw); err != nil {
		return teamconfig.DefaultCommunicationConfig()
	}
	var cfg teamconfig.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return teamconfig.DefaultCommunicationConfig()
	}
	return cfg.Communication.Normalized()
}

func taskEnvelopeForContract(ctx context.Context, tx *sql.Tx, contractID string) (string, error) {
	var envelopeID string
	err := tx.QueryRowContext(ctx, `
SELECT id
FROM agent_communication_envelopes
WHERE kind = 'task' AND contract_id = ?
ORDER BY created_at ASC, id ASC
LIMIT 1`, contractID).Scan(&envelopeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query task envelope: %w", err)
	}
	return envelopeID, nil
}

func messageSummary(intent, body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return firstNonEmpty(intent, "message")
	}
	if len(body) <= 120 {
		return body
	}
	return body[:120]
}

func resultBody(contractID string, evidenceIDs []string, summary string) string {
	return "contract:" + contractID + ";summary:" + summary + ";evidence:" + strings.Join(evidenceIDs, ",")
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func appendFact(ctx context.Context, tx *sql.Tx, eventType, aggregateType, aggregateID string, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return store.AppendEventTx(ctx, tx, events.Event{
		TenantID:      "default",
		SubjectKind:   "system",
		SubjectID:     "coordination",
		Type:          eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		PayloadJSON:   raw,
	})
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

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
