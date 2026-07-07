package validation

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
	"coordplane/internal/store"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Service struct {
	db    *sql.DB
	store *store.Store
}

type Config struct {
	Store *store.Store
}

type Input struct {
	LeaseID            string       `json:"lease_id"`
	AssessedContractID string       `json:"assessed_contract_id,omitempty"`
	Verdict            string       `json:"verdict"`
	Reason             string       `json:"reason"`
	Summary            string       `json:"summary"`
	CheckedRefs        []CheckedRef `json:"checked_refs"`
	Blockers           []string     `json:"blockers,omitempty"`
}

type CheckedRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type Result struct {
	AssessmentID       string `json:"assessment_id"`
	EvidenceID         string `json:"evidence_id"`
	VerifierAgentID    string `json:"verifier_agent_id"`
	LeaseID            string `json:"lease_id"`
	AssignmentID       string `json:"assignment_id"`
	ContractID         string `json:"contract_id"`
	AttemptID          string `json:"attempt_id"`
	RuntimeID          string `json:"runtime_id"`
	SessionRouteID     string `json:"session_route_id"`
	AssessedContractID string `json:"assessed_contract_id"`
	Verdict            string `json:"verdict"`
	CheckedRefCount    int    `json:"checked_ref_count"`
	Summary            string `json:"summary"`
}

type Assessment struct {
	ID                 string        `json:"id"`
	VerifierAgentID    string        `json:"verifier_agent_id"`
	LeaseID            string        `json:"lease_id"`
	AssignmentID       string        `json:"assignment_id"`
	ContractID         string        `json:"contract_id"`
	AttemptID          string        `json:"attempt_id"`
	SessionRouteID     string        `json:"session_route_id"`
	RuntimeID          string        `json:"runtime_id"`
	AssessedContractID string        `json:"assessed_contract_id"`
	Verdict            string        `json:"verdict"`
	Reason             string        `json:"reason"`
	Summary            string        `json:"summary"`
	CheckedRefs        []CheckedRef  `json:"checked_refs,omitempty"`
	RefSnapshot        []RefSnapshot `json:"ref_snapshot,omitempty"`
	EvidenceID         string        `json:"evidence_id"`
	IdempotencyKey     string        `json:"idempotency_key,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
}

type RefSnapshot struct {
	Kind             string         `json:"kind"`
	ID               string         `json:"id,omitempty"`
	Ref              string         `json:"ref,omitempty"`
	ContractID       string         `json:"contract_id,omitempty"`
	AgentID          string         `json:"agent_id,omitempty"`
	OwnerAgentID     string         `json:"owner_agent_id,omitempty"`
	Status           string         `json:"status,omitempty"`
	State            string         `json:"state,omitempty"`
	ExitCode         *int           `json:"exit_code,omitempty"`
	EvidenceKind     string         `json:"evidence_kind,omitempty"`
	Verdict          string         `json:"verdict,omitempty"`
	ContentRef       string         `json:"content_ref,omitempty"`
	ObjectRef        string         `json:"object_ref,omitempty"`
	Checksum         string         `json:"checksum,omitempty"`
	SizeBytes        int64          `json:"size_bytes,omitempty"`
	ContentType      string         `json:"content_type,omitempty"`
	StdoutRef        string         `json:"stdout_ref,omitempty"`
	StderrRef        string         `json:"stderr_ref,omitempty"`
	StdoutBytes      int            `json:"stdout_bytes,omitempty"`
	StderrBytes      int            `json:"stderr_bytes,omitempty"`
	StdoutTruncated  bool           `json:"stdout_truncated,omitempty"`
	StderrTruncated  bool           `json:"stderr_truncated,omitempty"`
	WorkspaceID      string         `json:"workspace_id,omitempty"`
	RepoID           string         `json:"repo_id,omitempty"`
	BaseRef          string         `json:"base_ref,omitempty"`
	HeadRef          string         `json:"head_ref,omitempty"`
	CommitCount      int            `json:"commit_count,omitempty"`
	EvidenceRefCount int            `json:"evidence_ref_count,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        time.Time      `json:"created_at,omitempty"`
	UpdatedAt        time.Time      `json:"updated_at,omitempty"`
}

type scope struct {
	AgentID          string
	LeaseID          string
	AssignmentID     string
	ContractID       string
	ContractIssuerID string
	AttemptID        string
	RuntimeID        string
	SessionRouteID   string
}

type contractSummary struct {
	ID             string
	IssuerContract string
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("validation.assessment: store is required")
	}
	return &Service{db: cfg.Store.DB(), store: cfg.Store}, nil
}

func RegisterCapabilities(registry *capability.Registry, service *Service) error {
	if registry == nil {
		return errors.New("validation capabilities: registry is nil")
	}
	if service == nil {
		return errors.New("validation capabilities: service is nil")
	}
	return registry.Register(capability.Definition{
		Name:           "validation.assessment",
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		RejectedSchema: json.RawMessage(`{"type":"object"}`),
		SideEffect:     capability.SideEffectWrite,
		RequiredScope:  "agent_lease_session",
		Idempotency:    true,
		SkillRefs:      []string{"coordplane-service"},
	}, service.handleAssessment)
}

func (s *Service) handleAssessment(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input Input
	if err := decodeInputWithScope(call, &input); err != nil {
		return capability.Rejected[json.RawMessage](
			"INVALID_CAPABILITY_INPUT",
			"validation.assessment input is invalid: "+err.Error(),
			capability.WithRepairHint("retry with lease_id, verdict, reason, summary, and checked_refs"),
			capability.WithAllowedNextActions("validation.assessment"),
			capability.WithRetryable(false),
		)
	}
	result, response := s.Assess(ctx, call.Subject, input, call.IdempotencyKey)
	if response.Status != "" {
		return response
	}
	accepted, err := capability.AcceptedJSON(result)
	if err != nil {
		return capability.Error[json.RawMessage]("VALIDATION_RESPONSE_ENCODE_FAILED", err.Error(), false)
	}
	return accepted
}

func (s *Service) Assess(ctx context.Context, subject capability.Subject, input Input, idempotencyKey string) (Result, capability.Response[json.RawMessage]) {
	agentID := agentIDFromSubject(subject)
	validated, response := validateInput(input)
	if response.Status != "" {
		return Result{}, response
	}
	resolved, response := s.resolveScope(ctx, agentID, subject.RuntimeID, validated.LeaseID)
	if response.Status != "" {
		return Result{}, response
	}
	if validated.AssessedContractID == "" {
		validated.AssessedContractID = resolved.ContractID
	}
	if response := s.canAssessContract(ctx, resolved, validated.AssessedContractID); response.Status != "" {
		return Result{}, response
	}
	if idempotencyKey != "" {
		existing, ok, err := s.assessmentByIdempotency(ctx, resolved, idempotencyKey)
		if err != nil {
			return Result{}, capability.Error[json.RawMessage]("VALIDATION_IDEMPOTENCY_LOOKUP_FAILED", err.Error(), true)
		}
		if ok {
			return resultFromAssessment(existing), capability.Response[json.RawMessage]{}
		}
	}
	snapshots, response := s.resolveCheckedRefs(ctx, resolved, validated.AssessedContractID, validated.CheckedRefs)
	if response.Status != "" {
		return Result{}, response
	}
	assessment, err := s.record(ctx, resolved, validated, snapshots, idempotencyKey)
	if err != nil {
		return Result{}, capability.Error[json.RawMessage]("VALIDATION_ASSESSMENT_RECORD_FAILED", err.Error(), true)
	}
	return resultFromAssessment(assessment), capability.Response[json.RawMessage]{}
}

func validateInput(in Input) (Input, capability.Response[json.RawMessage]) {
	in.LeaseID = strings.TrimSpace(in.LeaseID)
	in.AssessedContractID = strings.TrimSpace(in.AssessedContractID)
	in.Verdict = strings.ToLower(strings.TrimSpace(in.Verdict))
	in.Reason = strings.TrimSpace(in.Reason)
	in.Summary = strings.TrimSpace(in.Summary)
	if in.LeaseID == "" {
		return Input{}, reject("VALIDATION_LEASE_REQUIRED", "validation.assessment requires lease_id", "retry from the active verifier lease")
	}
	switch in.Verdict {
	case "pass", "fail", "blocked":
	default:
		return Input{}, reject("INVALID_VALIDATION_VERDICT", "verdict must be pass, fail, or blocked", "retry with verdict pass, fail, or blocked")
	}
	if in.Reason == "" {
		return Input{}, reject("VALIDATION_REASON_REQUIRED", "reason is required", "include the reason for the canonical verdict")
	}
	if in.Summary == "" {
		return Input{}, reject("VALIDATION_SUMMARY_REQUIRED", "summary is required", "include an inspect-safe summary")
	}
	if len(in.CheckedRefs) == 0 {
		return Input{}, reject("VALIDATION_CHECKED_REFS_REQUIRED", "checked_refs is required", "reference at least one durable command_run, changeset, evidence, object, or artifact")
	}
	if in.Verdict == "blocked" && len(in.Blockers) == 0 {
		return Input{}, reject("VALIDATION_BLOCKERS_REQUIRED", "blocked verdict requires blockers", "include blocker descriptions for a blocked assessment")
	}
	for i := range in.CheckedRefs {
		in.CheckedRefs[i].Kind = strings.TrimSpace(in.CheckedRefs[i].Kind)
		in.CheckedRefs[i].ID = strings.TrimSpace(in.CheckedRefs[i].ID)
		in.CheckedRefs[i].Ref = strings.TrimSpace(in.CheckedRefs[i].Ref)
		if in.CheckedRefs[i].Kind == "" {
			return Input{}, reject("VALIDATION_REF_INVALID", "checked_refs kind is required", "include kind for every checked ref")
		}
		if in.CheckedRefs[i].ID == "" && in.CheckedRefs[i].Ref == "" {
			return Input{}, reject("VALIDATION_REF_INVALID", "checked_refs id or ref is required", "include id or ref for every checked ref")
		}
	}
	return in, capability.Response[json.RawMessage]{}
}

func (s *Service) resolveScope(ctx context.Context, agentID, subjectRuntimeID, leaseID string) (scope, capability.Response[json.RawMessage]) {
	if agentID == "" {
		return scope{}, reject("VALIDATION_AGENT_REQUIRED", "validation.assessment requires an agent subject", "retry from an authenticated verifier coordlink subject")
	}
	var out scope
	var leaseState, attemptStatus, routeState, runtimeState string
	err := s.db.QueryRowContext(ctx, `
SELECT l.agent_id, l.id, l.assignment_id, asn.contract_id,
  COALESCE(c.issuer_contract_id, ''),
  a.id, a.status,
  sr.id, sr.runtime_id, sr.state,
  ri.state
FROM leases l
JOIN assignments asn ON asn.id = l.assignment_id
JOIN work_contracts c ON c.id = asn.contract_id
JOIN attempts a ON a.lease_id = l.id
JOIN session_routes sr ON sr.id = l.session_route_id
JOIN runtime_instances ri ON ri.runtime_id = sr.runtime_id AND ri.attempt_id = a.id
WHERE l.id = ? AND l.agent_id = ?
ORDER BY a.started_at DESC, a.id DESC
LIMIT 1`, leaseID, agentID).Scan(
		&out.AgentID,
		&out.LeaseID,
		&out.AssignmentID,
		&out.ContractID,
		&out.ContractIssuerID,
		&out.AttemptID,
		&attemptStatus,
		&out.SessionRouteID,
		&out.RuntimeID,
		&routeState,
		&runtimeState,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return scope{}, reject("VALIDATION_SCOPE_REJECTED", "active lease/session/runtime scope was not found for this agent", "claim a verifier assignment and submit from its active runtime session")
	}
	if err != nil {
		return scope{}, capability.Error[json.RawMessage]("VALIDATION_SCOPE_LOOKUP_FAILED", err.Error(), true)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, leaseID).Scan(&leaseState); err != nil {
		return scope{}, capability.Error[json.RawMessage]("VALIDATION_LEASE_LOOKUP_FAILED", err.Error(), true)
	}
	switch {
	case leaseState != "active":
		return scope{}, reject("VALIDATION_LEASE_REJECTED", "lease is not active", "submit validation only while holding the active verifier lease")
	case attemptStatus != "running":
		return scope{}, reject("VALIDATION_ATTEMPT_REJECTED", "attempt is not running", "start or resume the verifier session before validation.assessment")
	case routeState != "active":
		return scope{}, reject("VALIDATION_SESSION_REJECTED", "session route is not active", "submit validation only from an active session")
	case runtimeState != "ready":
		return scope{}, reject("VALIDATION_RUNTIME_REJECTED", "runtime is not ready", "submit validation from a ready runtime session")
	case subjectRuntimeID != "" && subjectRuntimeID != out.RuntimeID:
		return scope{}, reject("VALIDATION_RUNTIME_REJECTED", "subject runtime_id does not match the active session route", "retry from the coordlink runtime identity injected into this session")
	}
	return out, capability.Response[json.RawMessage]{}
}

func (s *Service) canAssessContract(ctx context.Context, resolved scope, assessedContractID string) capability.Response[json.RawMessage] {
	assessed, err := s.contractSummary(ctx, assessedContractID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject("VALIDATION_CONTRACT_NOT_FOUND", "assessed_contract_id does not exist", "retry with a contract visible from the verifier context")
	}
	if err != nil {
		return capability.Error[json.RawMessage]("VALIDATION_CONTRACT_LOOKUP_FAILED", err.Error(), true)
	}
	if assessed.ID == resolved.ContractID ||
		(assessed.IssuerContract != "" && assessed.IssuerContract == resolved.ContractID) ||
		(resolved.ContractIssuerID != "" && resolved.ContractIssuerID == assessed.ID) ||
		(resolved.ContractIssuerID != "" && assessed.IssuerContract == resolved.ContractIssuerID) {
		return capability.Response[json.RawMessage]{}
	}
	return reject("VALIDATION_CONTRACT_SCOPE_REJECTED", "assessed_contract_id is outside the verifier contract scope", "reference the verifier contract, its parent, child, or sibling contract")
}

func (s *Service) contractSummary(ctx context.Context, contractID string) (contractSummary, error) {
	var out contractSummary
	err := s.db.QueryRowContext(ctx, `
SELECT id, COALESCE(issuer_contract_id, '')
FROM work_contracts
WHERE id = ?`, contractID).Scan(&out.ID, &out.IssuerContract)
	return out, err
}

func (s *Service) assessmentByIdempotency(ctx context.Context, resolved scope, key string) (Assessment, bool, error) {
	assessment, err := scanAssessment(s.db.QueryRowContext(ctx, assessmentSelectSQL()+`
WHERE verifier_agent_id = ? AND attempt_id = ? AND idempotency_key = ?
LIMIT 1`, resolved.AgentID, resolved.AttemptID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Assessment{}, false, nil
	}
	if err != nil {
		return Assessment{}, false, err
	}
	return assessment, true, nil
}

func (s *Service) resolveCheckedRefs(ctx context.Context, resolved scope, assessedContractID string, refs []CheckedRef) ([]RefSnapshot, capability.Response[json.RawMessage]) {
	allowedContracts := map[string]bool{
		resolved.ContractID: true,
		assessedContractID:  true,
	}
	snapshots := make([]RefSnapshot, 0, len(refs))
	for _, ref := range refs {
		snapshot, response := s.resolveCheckedRef(ctx, resolved, allowedContracts, ref)
		if response.Status != "" {
			return nil, response
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, capability.Response[json.RawMessage]{}
}

func (s *Service) resolveCheckedRef(ctx context.Context, resolved scope, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	switch ref.Kind {
	case "command_run":
		return s.resolveCommandRun(ctx, allowedContracts, ref)
	case "changeset":
		return s.resolveChangeSet(ctx, allowedContracts, ref)
	case "evidence":
		return s.resolveEvidence(ctx, allowedContracts, ref)
	case "object":
		return s.resolveObject(ctx, allowedContracts, ref)
	case "artifact":
		return s.resolveArtifact(ctx, resolved, allowedContracts, ref)
	default:
		return RefSnapshot{}, reject("VALIDATION_REF_KIND_REJECTED", "checked ref kind is not supported", "use command_run, changeset, evidence, object, or artifact")
	}
}

func (s *Service) resolveCommandRun(ctx context.Context, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	id := firstNonEmpty(ref.ID, ref.Ref)
	row := s.db.QueryRowContext(ctx, `
SELECT id, agent_id, contract_id, status, exit_code, stdout_ref, stderr_ref,
  stdout_bytes, stderr_bytes, stdout_truncated, stderr_truncated, evidence_id, created_at, updated_at
FROM command_runs
WHERE id = ?`, id)
	var snapshot RefSnapshot
	var exitCode sql.NullInt64
	var stdoutTruncated, stderrTruncated int
	var createdAt, updatedAt string
	if err := row.Scan(
		&snapshot.ID,
		&snapshot.AgentID,
		&snapshot.ContractID,
		&snapshot.Status,
		&exitCode,
		&snapshot.StdoutRef,
		&snapshot.StderrRef,
		&snapshot.StdoutBytes,
		&snapshot.StderrBytes,
		&stdoutTruncated,
		&stderrTruncated,
		&snapshot.ContentRef,
		&createdAt,
		&updatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return refNotFound("command_run", id)
	} else if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	if !allowedContracts[snapshot.ContractID] {
		return refScopeRejected("command_run", id)
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		snapshot.ExitCode = &value
	}
	snapshot.Kind = "command_run"
	snapshot.Ref = id
	snapshot.StdoutTruncated = stdoutTruncated != 0
	snapshot.StderrTruncated = stderrTruncated != 0
	snapshot.CreatedAt = parseTimeOrZero(createdAt)
	snapshot.UpdatedAt = parseTimeOrZero(updatedAt)
	return snapshot, capability.Response[json.RawMessage]{}
}

func (s *Service) resolveChangeSet(ctx context.Context, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	id := firstNonEmpty(ref.ID, ref.Ref)
	row := s.db.QueryRowContext(ctx, `
SELECT id, workspace_id, repo_id, COALESCE(contract_id, ''), base_ref, head_ref,
  commit_ids_json, evidence_refs_json, state, created_at, updated_at
FROM changesets
WHERE id = ?`, id)
	var snapshot RefSnapshot
	var commitIDsJSON, evidenceRefsJSON, createdAt, updatedAt string
	if err := row.Scan(
		&snapshot.ID,
		&snapshot.WorkspaceID,
		&snapshot.RepoID,
		&snapshot.ContractID,
		&snapshot.BaseRef,
		&snapshot.HeadRef,
		&commitIDsJSON,
		&evidenceRefsJSON,
		&snapshot.State,
		&createdAt,
		&updatedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return refNotFound("changeset", id)
	} else if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	if snapshot.ContractID == "" || !allowedContracts[snapshot.ContractID] {
		return refScopeRejected("changeset", id)
	}
	var commitIDs, evidenceRefs []string
	_ = json.Unmarshal([]byte(commitIDsJSON), &commitIDs)
	_ = json.Unmarshal([]byte(evidenceRefsJSON), &evidenceRefs)
	snapshot.Kind = "changeset"
	snapshot.Ref = id
	snapshot.CommitCount = len(commitIDs)
	snapshot.EvidenceRefCount = len(evidenceRefs)
	snapshot.CreatedAt = parseTimeOrZero(createdAt)
	snapshot.UpdatedAt = parseTimeOrZero(updatedAt)
	return snapshot, capability.Response[json.RawMessage]{}
}

func (s *Service) resolveEvidence(ctx context.Context, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	id := firstNonEmpty(ref.ID, ref.Ref)
	row := s.db.QueryRowContext(ctx, `
SELECT id, kind, contract_id, produced_by, COALESCE(content_ref, ''),
  COALESCE(verdict, ''), created_at
FROM evidence
WHERE id = ?`, id)
	var snapshot RefSnapshot
	var createdAt string
	if err := row.Scan(
		&snapshot.ID,
		&snapshot.EvidenceKind,
		&snapshot.ContractID,
		&snapshot.AgentID,
		&snapshot.ContentRef,
		&snapshot.Verdict,
		&createdAt,
	); errors.Is(err, sql.ErrNoRows) {
		return refNotFound("evidence", id)
	} else if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	if !allowedContracts[snapshot.ContractID] {
		return refScopeRejected("evidence", id)
	}
	snapshot.Kind = "evidence"
	snapshot.Ref = id
	snapshot.CreatedAt = parseTimeOrZero(createdAt)
	return snapshot, capability.Response[json.RawMessage]{}
}

func (s *Service) resolveObject(ctx context.Context, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	objectRef := firstNonEmpty(ref.Ref, ref.ID)
	row := s.db.QueryRowContext(ctx, `
SELECT object_ref, COALESCE(owner_agent_id, ''), checksum, size_bytes, content_type, created_at
FROM object_blobs
WHERE object_ref = ?`, objectRef)
	var snapshot RefSnapshot
	var createdAt string
	if err := row.Scan(
		&snapshot.ObjectRef,
		&snapshot.OwnerAgentID,
		&snapshot.Checksum,
		&snapshot.SizeBytes,
		&snapshot.ContentType,
		&createdAt,
	); errors.Is(err, sql.ErrNoRows) {
		return refNotFound("object", objectRef)
	} else if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	contractID, ok, err := s.objectLinkedContract(ctx, objectRef, allowedContracts)
	if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	if !ok {
		return refScopeRejected("object", objectRef)
	}
	snapshot.Kind = "object"
	snapshot.Ref = objectRef
	snapshot.ContractID = contractID
	snapshot.CreatedAt = parseTimeOrZero(createdAt)
	return snapshot, capability.Response[json.RawMessage]{}
}

func (s *Service) objectLinkedContract(ctx context.Context, objectRef string, allowedContracts map[string]bool) (string, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT contract_id FROM evidence WHERE content_ref = ?
UNION
SELECT contract_id FROM command_runs WHERE stdout_ref = ? OR stderr_ref = ?`,
		objectRef, objectRef, objectRef,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var contractID string
		if err := rows.Scan(&contractID); err != nil {
			return "", false, err
		}
		if allowedContracts[contractID] {
			return contractID, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func (s *Service) resolveArtifact(ctx context.Context, resolved scope, allowedContracts map[string]bool, ref CheckedRef) (RefSnapshot, capability.Response[json.RawMessage]) {
	id := firstNonEmpty(ref.ID, ref.Ref)
	row := s.db.QueryRowContext(ctx, `
SELECT id, owner_agent_id, object_ref, checksum, size_bytes, content_type, metadata_json, created_at
FROM artifacts
WHERE id = ?`, id)
	var snapshot RefSnapshot
	var metadataJSON, createdAt string
	if err := row.Scan(
		&snapshot.ID,
		&snapshot.OwnerAgentID,
		&snapshot.ObjectRef,
		&snapshot.Checksum,
		&snapshot.SizeBytes,
		&snapshot.ContentType,
		&metadataJSON,
		&createdAt,
	); errors.Is(err, sql.ErrNoRows) {
		return refNotFound("artifact", id)
	} else if err != nil {
		return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
	}
	var metadata map[string]any
	if metadataJSON != "" {
		if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
			return RefSnapshot{}, capability.Error[json.RawMessage]("VALIDATION_REF_LOOKUP_FAILED", err.Error(), true)
		}
	}
	contractID := stringFromMap(metadata, "contract_id")
	if contractID != "" {
		if !allowedContracts[contractID] {
			return refScopeRejected("artifact", id)
		}
	} else if snapshot.OwnerAgentID != resolved.AgentID {
		return refScopeRejected("artifact", id)
	} else {
		contractID = resolved.ContractID
	}
	snapshot.Kind = "artifact"
	snapshot.Ref = id
	snapshot.ContractID = contractID
	snapshot.Metadata = metadata
	snapshot.CreatedAt = parseTimeOrZero(createdAt)
	return snapshot, capability.Response[json.RawMessage]{}
}

func (s *Service) record(ctx context.Context, resolved scope, input Input, snapshots []RefSnapshot, idempotencyKey string) (Assessment, error) {
	assessmentID, err := ids.New("val")
	if err != nil {
		return Assessment{}, err
	}
	evidenceID, err := ids.New("ev")
	if err != nil {
		return Assessment{}, err
	}
	checkedRefsJSON, err := json.Marshal(input.CheckedRefs)
	if err != nil {
		return Assessment{}, err
	}
	snapshotJSON, err := json.Marshal(snapshots)
	if err != nil {
		return Assessment{}, err
	}
	now := formatTime(time.Now())
	var recorded Assessment
	err = s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO validation_assessments (
  id, tenant_id, verifier_agent_id, lease_id, assignment_id, contract_id,
  attempt_id, session_route_id, runtime_id, assessed_contract_id, verdict,
  reason, summary, checked_refs_json, ref_snapshot_json, evidence_id,
  idempotency_key, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			assessmentID, resolved.AgentID, resolved.LeaseID, resolved.AssignmentID,
			resolved.ContractID, resolved.AttemptID, resolved.SessionRouteID,
			resolved.RuntimeID, input.AssessedContractID, input.Verdict,
			input.Reason, input.Summary, string(checkedRefsJSON), string(snapshotJSON),
			evidenceID, nullable(idempotencyKey), now,
		); err != nil {
			return fmt.Errorf("insert validation assessment: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO evidence (
  id, tenant_id, kind, contract_id, produced_by, content_ref,
  inline_content, summary, verdict, created_at
) VALUES (?, 'default', 'validation_assessment', ?, ?, ?, '', ?, ?, ?)`,
			evidenceID, resolved.ContractID, resolved.AgentID, "validation_assessment:"+assessmentID,
			input.Summary, input.Verdict, now,
		); err != nil {
			return fmt.Errorf("insert validation evidence: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "validation.assessment_requested", assessmentID, resolved, map[string]any{
			"assessed_contract_id": input.AssessedContractID,
			"checked_ref_count":    len(input.CheckedRefs),
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "validation.assessment_submitted", assessmentID, resolved, map[string]any{
			"assessed_contract_id": input.AssessedContractID,
			"verdict":              input.Verdict,
			"evidence_id":          evidenceID,
		}); err != nil {
			return err
		}
		verdictEvent := "validation.assessment_" + input.Verdict
		if input.Verdict == "fail" {
			verdictEvent = "validation.assessment_failed"
		}
		if input.Verdict == "pass" {
			verdictEvent = "validation.assessment_passed"
		}
		if _, err := appendEvent(ctx, tx, verdictEvent, assessmentID, resolved, map[string]any{
			"assessed_contract_id": input.AssessedContractID,
			"verdict":              input.Verdict,
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "evidence.validation_assessment_recorded", assessmentID, resolved, map[string]any{
			"evidence_id": evidenceID,
			"content_ref": "validation_assessment:" + assessmentID,
		}); err != nil {
			return err
		}
		assessment, err := assessmentByIDTx(ctx, tx, assessmentID)
		if err != nil {
			return fmt.Errorf("read validation assessment: %w", err)
		}
		recorded = assessment
		return nil
	})
	return recorded, err
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType, assessmentID string, resolved scope, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return store.AppendEventTx(ctx, tx, events.Event{
		TenantID:       "default",
		SubjectKind:    "agent",
		SubjectID:      resolved.AgentID,
		AgentID:        resolved.AgentID,
		RuntimeID:      resolved.RuntimeID,
		CapabilityName: "validation.assessment",
		Type:           eventType,
		AggregateType:  "validation_assessment",
		AggregateID:    assessmentID,
		PayloadJSON:    raw,
	})
}

func ListAssessments(ctx context.Context, db *sql.DB) ([]Assessment, error) {
	rows, err := db.QueryContext(ctx, assessmentSelectSQL()+`ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list validation assessments: %w", err)
	}
	defer rows.Close()
	var out []Assessment
	for rows.Next() {
		assessment, err := scanAssessment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, assessment)
	}
	return out, rows.Err()
}

func assessmentByIDTx(ctx context.Context, tx *sql.Tx, assessmentID string) (Assessment, error) {
	return scanAssessment(tx.QueryRowContext(ctx, assessmentSelectSQL()+`WHERE id = ?`, assessmentID))
}

func assessmentSelectSQL() string {
	return `
SELECT id, verifier_agent_id, lease_id, assignment_id, contract_id, attempt_id,
  session_route_id, runtime_id, assessed_contract_id, verdict, reason, summary,
  checked_refs_json, ref_snapshot_json, evidence_id, COALESCE(idempotency_key, ''),
  created_at
FROM validation_assessments
`
}

type rowScanner interface {
	Scan(...any) error
}

func scanAssessment(row rowScanner) (Assessment, error) {
	var out Assessment
	var checkedRefsJSON, snapshotJSON, createdAt string
	if err := row.Scan(
		&out.ID,
		&out.VerifierAgentID,
		&out.LeaseID,
		&out.AssignmentID,
		&out.ContractID,
		&out.AttemptID,
		&out.SessionRouteID,
		&out.RuntimeID,
		&out.AssessedContractID,
		&out.Verdict,
		&out.Reason,
		&out.Summary,
		&checkedRefsJSON,
		&snapshotJSON,
		&out.EvidenceID,
		&out.IdempotencyKey,
		&createdAt,
	); err != nil {
		return Assessment{}, err
	}
	if checkedRefsJSON != "" {
		if err := json.Unmarshal([]byte(checkedRefsJSON), &out.CheckedRefs); err != nil {
			return Assessment{}, err
		}
	}
	if snapshotJSON != "" {
		if err := json.Unmarshal([]byte(snapshotJSON), &out.RefSnapshot); err != nil {
			return Assessment{}, err
		}
	}
	parsed, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return Assessment{}, err
	}
	out.CreatedAt = parsed
	return out, nil
}

func resultFromAssessment(assessment Assessment) Result {
	return Result{
		AssessmentID:       assessment.ID,
		EvidenceID:         assessment.EvidenceID,
		VerifierAgentID:    assessment.VerifierAgentID,
		LeaseID:            assessment.LeaseID,
		AssignmentID:       assessment.AssignmentID,
		ContractID:         assessment.ContractID,
		AttemptID:          assessment.AttemptID,
		RuntimeID:          assessment.RuntimeID,
		SessionRouteID:     assessment.SessionRouteID,
		AssessedContractID: assessment.AssessedContractID,
		Verdict:            assessment.Verdict,
		CheckedRefCount:    len(assessment.CheckedRefs),
		Summary:            assessment.Summary,
	}
}

func decodeInputWithScope(call capability.Call, target any) error {
	var merged map[string]any
	if len(call.Scope) > 0 {
		if err := json.Unmarshal(call.Scope, &merged); err != nil {
			return fmt.Errorf("decode scope: %w", err)
		}
	}
	if merged == nil {
		merged = make(map[string]any)
	}
	if len(call.Input) > 0 {
		var input map[string]any
		if err := json.Unmarshal(call.Input, &input); err != nil {
			return err
		}
		for key, value := range input {
			merged[key] = value
		}
	}
	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(mergedBytes, target)
}

func reject(code, message, repair string) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		code,
		message,
		capability.WithRepairHint(repair),
		capability.WithAllowedNextActions("validation.assessment", "contract.context"),
		capability.WithRetryable(false),
	)
}

func refNotFound(kind, ref string) (RefSnapshot, capability.Response[json.RawMessage]) {
	return RefSnapshot{}, capability.Rejected[json.RawMessage](
		"VALIDATION_REF_NOT_FOUND",
		fmt.Sprintf("%s ref does not exist", kind),
		capability.WithCanonicalID("ref", ref),
		capability.WithRepairHint("retry with durable refs visible from the verifier context"),
		capability.WithAllowedNextActions("contract.context", "validation.assessment"),
		capability.WithRetryable(false),
	)
}

func refScopeRejected(kind, ref string) (RefSnapshot, capability.Response[json.RawMessage]) {
	return RefSnapshot{}, capability.Rejected[json.RawMessage](
		"VALIDATION_REF_SCOPE_REJECTED",
		fmt.Sprintf("%s ref is outside the verifier assessment scope", kind),
		capability.WithCanonicalID("ref", ref),
		capability.WithRepairHint("reference only refs attached to the verifier or assessed contract"),
		capability.WithAllowedNextActions("contract.context", "validation.assessment"),
		capability.WithRetryable(false),
	)
}

func agentIDFromSubject(subject capability.Subject) string {
	if subject.AgentID != "" {
		return subject.AgentID
	}
	if subject.Kind == "agent" {
		return subject.ID
	}
	return subject.ID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringFromMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
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

func parseTimeOrZero(value string) time.Time {
	parsed, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
