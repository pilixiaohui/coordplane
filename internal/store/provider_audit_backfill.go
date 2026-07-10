package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	providerAuditLegacyIncompleteCode = "PROVIDER_AUDIT_LEGACY_INCOMPLETE"
	providerAuditUnresolvedCode       = "PROVIDER_AUDIT_REQUIREMENT_UNRESOLVED"
)

var errInvalidProviderPolicySnapshot = errors.New("invalid provider policy snapshot")

type legacyProviderAuditSession struct {
	ID                   string
	CLIBackend           string
	AgentID              string
	RuntimeID            string
	AuditState           string
	AuditErrorCode       string
	CompatibleRequired   bool
	SessionState         string
	AttemptStatus        string
	AttemptCLIBackend    string
	AttemptRuntimeKind   string
	LeaseState           string
	LeaseAgentID         string
	LeaseRuntimeID       string
	AssignmentAgentID    string
	RuntimeProfile       string
	RuntimeKind          string
	RuntimeAgentID       string
	RuntimeRecordID      string
	TeamID               string
	TeamVersion          int
	ScopeCreatedAt       string
	ProviderOutcomeCount int
}

type historicalTeamConfig struct {
	TeamID          string                              `json:"team_id"`
	Version         int                                 `json:"version"`
	Agents          []historicalTeamConfigAgent         `json:"agents"`
	RuntimeProfiles map[string]historicalRuntimeProfile `json:"runtime_profiles"`
}

type historicalTeamConfigAgent struct {
	ID             string   `json:"id"`
	RuntimeProfile string   `json:"runtime_profile"`
	CLIBackend     string   `json:"cli_backend"`
	Capabilities   []string `json:"capabilities"`
}

type historicalRuntimeProfile struct {
	Kind          string                         `json:"kind"`
	CommandPolicy historicalRuntimeCommandPolicy `json:"command_policy"`
}

type historicalRuntimeCommandPolicy struct {
	NonInteractiveApproval     bool     `json:"non_interactive_approval"`
	AllowCoordlinkCapabilities []string `json:"allow_coordlink_capabilities"`
}

type providerAuditRequirementClassification struct {
	State          string
	Reason         string
	AuditState     string
	AuditErrorCode string
	CompatibleBool bool
}

func backfillProviderAuditRequirements(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT cs.id, cs.cli_backend, cs.agent_id, cs.runtime_id,
       cs.provider_audit_state, cs.provider_audit_error_code,
       cs.provider_audit_required, cs.state,
       COALESCE(att.status, ''), COALESCE(att.cli_backend, ''), COALESCE(att.runtime_kind, ''),
       COALESCE(l.state, ''), COALESCE(l.agent_id, ''), COALESCE(l.runtime_id, ''),
       COALESCE(a.assignee_agent_id, ''),
       COALESCE(ri.runtime_profile, ''), COALESCE(ri.runtime_kind, ''),
       COALESCE(ri.agent_id, ''), COALESCE(ri.runtime_id, ''),
       COALESCE(cts.team_id, ''), COALESCE(cts.team_version, 0), COALESCE(cts.created_at, ''),
       (SELECT COUNT(*) FROM provider_tool_outcomes pto WHERE pto.cli_session_id = cs.id)
FROM cli_sessions cs
LEFT JOIN attempts att ON att.id = cs.attempt_id
LEFT JOIN leases l ON l.id = att.lease_id
LEFT JOIN assignments a ON a.id = l.assignment_id
LEFT JOIN contract_team_scopes cts ON cts.contract_id = a.contract_id
LEFT JOIN runtime_instances ri ON ri.attempt_id = cs.attempt_id AND ri.runtime_id = cs.runtime_id
ORDER BY cs.started_at, cs.id`)
	if err != nil {
		return fmt.Errorf("list legacy provider audit sessions: %w", err)
	}
	var sessions []legacyProviderAuditSession
	for rows.Next() {
		var session legacyProviderAuditSession
		if err := rows.Scan(
			&session.ID, &session.CLIBackend, &session.AgentID, &session.RuntimeID,
			&session.AuditState, &session.AuditErrorCode, &session.CompatibleRequired,
			&session.SessionState, &session.AttemptStatus, &session.AttemptCLIBackend,
			&session.AttemptRuntimeKind, &session.LeaseState, &session.LeaseAgentID,
			&session.LeaseRuntimeID, &session.AssignmentAgentID, &session.RuntimeProfile,
			&session.RuntimeKind, &session.RuntimeAgentID, &session.RuntimeRecordID,
			&session.TeamID, &session.TeamVersion, &session.ScopeCreatedAt,
			&session.ProviderOutcomeCount,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy provider audit session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, session := range sessions {
		classification, err := classifyLegacyProviderAudit(ctx, tx, session)
		if err != nil {
			return fmt.Errorf("classify provider audit session %s: %w", session.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE cli_sessions
SET provider_audit_requirement_state = ?, provider_audit_requirement_reason = ?,
    provider_audit_required = ?, provider_audit_state = ?, provider_audit_error_code = ?
WHERE id = ?`, classification.State, classification.Reason, classification.CompatibleBool,
			classification.AuditState, classification.AuditErrorCode, session.ID); err != nil {
			return fmt.Errorf("backfill provider audit session %s: %w", session.ID, err)
		}
	}
	return nil
}

func classifyLegacyProviderAudit(ctx context.Context, tx *sql.Tx, session legacyProviderAuditSession) (providerAuditRequirementClassification, error) {
	if session.CLIBackend != "claude" {
		if session.AuditState == "not_requested" && session.ProviderOutcomeCount == 0 {
			return providerAuditClassification(session, "not_required", "legacy_non_claude"), nil
		}
		return unresolvedProviderAuditClassification(session, "runtime_mismatch"), nil
	}
	if session.CompatibleRequired {
		return requiredProviderAuditClassification(session, "explicit_required"), nil
	}
	if session.AuditState == "complete" || session.AuditState == "failed" || session.ProviderOutcomeCount > 0 {
		return requiredProviderAuditClassification(session, "legacy_audit_terminal"), nil
	}
	if !legacyProviderAuditLineageConsistent(session) {
		return unresolvedProviderAuditClassification(session, "runtime_mismatch"), nil
	}
	if session.TeamID == "" || session.TeamVersion <= 0 || session.ScopeCreatedAt == "" {
		return unresolvedProviderAuditClassification(session, "scope_missing"), nil
	}
	snapshot, err := loadHistoricalTeamConfigSnapshot(ctx, tx, session.TeamID, session.TeamVersion, session.ScopeCreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return unresolvedProviderAuditClassification(session, "policy_snapshot_missing"), nil
	}
	if errors.Is(err, errInvalidProviderPolicySnapshot) {
		return unresolvedProviderAuditClassification(session, "policy_snapshot_invalid"), nil
	}
	if err != nil {
		return providerAuditRequirementClassification{}, err
	}
	policyRequired, conclusive := historicalProviderPolicy(snapshot, session)
	if !conclusive {
		return unresolvedProviderAuditClassification(session, "runtime_mismatch"), nil
	}
	if policyRequired {
		return requiredProviderAuditClassification(session, "contract_policy_match"), nil
	}
	return providerAuditClassification(session, "not_required", "contract_policy_absent"), nil
}

func legacyProviderAuditLineageConsistent(session legacyProviderAuditSession) bool {
	return session.AttemptStatus != "" &&
		session.AttemptCLIBackend == session.CLIBackend &&
		session.LeaseState != "" && session.LeaseAgentID == session.AgentID &&
		session.LeaseRuntimeID == session.RuntimeID &&
		session.AssignmentAgentID == session.AgentID &&
		session.RuntimeRecordID == session.RuntimeID &&
		session.RuntimeAgentID == session.AgentID &&
		session.RuntimeKind == session.AttemptRuntimeKind &&
		session.RuntimeProfile != ""
}

func loadHistoricalTeamConfigSnapshot(ctx context.Context, tx *sql.Tx, teamID string, version int, scopeCreatedAt string) (historicalTeamConfig, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'team_config.loaded'
  AND aggregate_type = 'team_config'
  AND aggregate_id = ?
  AND occurred_at <= ?
ORDER BY occurred_at DESC, id DESC
LIMIT 1`, fmt.Sprintf("%s:%d", teamID, version), scopeCreatedAt).Scan(&raw)
	if err != nil {
		return historicalTeamConfig{}, err
	}
	var snapshot historicalTeamConfig
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return historicalTeamConfig{}, fmt.Errorf("%w: %v", errInvalidProviderPolicySnapshot, err)
	}
	if snapshot.TeamID != teamID || snapshot.Version != version {
		return historicalTeamConfig{}, fmt.Errorf("%w: TeamConfig snapshot identity mismatch", errInvalidProviderPolicySnapshot)
	}
	return snapshot, nil
}

func historicalProviderPolicy(snapshot historicalTeamConfig, session legacyProviderAuditSession) (bool, bool) {
	var agent historicalTeamConfigAgent
	found := false
	for _, candidate := range snapshot.Agents {
		if candidate.ID == session.AgentID {
			agent = candidate
			found = true
			break
		}
	}
	if !found || agent.CLIBackend != session.CLIBackend || agent.RuntimeProfile != session.RuntimeProfile {
		return false, false
	}
	profile, ok := snapshot.RuntimeProfiles[agent.RuntimeProfile]
	if !ok || profile.Kind != session.RuntimeKind {
		return false, false
	}
	policy := profile.CommandPolicy
	if !policy.NonInteractiveApproval || len(policy.AllowCoordlinkCapabilities) == 0 {
		return false, true
	}
	allowed := make(map[string]bool, len(policy.AllowCoordlinkCapabilities))
	for _, capability := range policy.AllowCoordlinkCapabilities {
		if capability == "" {
			return false, false
		}
		allowed[capability] = true
	}
	for _, capability := range agent.Capabilities {
		if capability == "" || !allowed[capability] {
			return false, false
		}
	}
	return true, true
}

func providerAuditClassification(session legacyProviderAuditSession, state, reason string) providerAuditRequirementClassification {
	return providerAuditRequirementClassification{
		State: state, Reason: reason, AuditState: session.AuditState,
		AuditErrorCode: session.AuditErrorCode, CompatibleBool: state != "not_required",
	}
}

func requiredProviderAuditClassification(session legacyProviderAuditSession, reason string) providerAuditRequirementClassification {
	classification := providerAuditClassification(session, "required", reason)
	if session.AuditState == "not_requested" && !legacyProviderAuditStillActive(session) {
		classification.AuditState = "failed"
		classification.AuditErrorCode = providerAuditLegacyIncompleteCode
	}
	return classification
}

func unresolvedProviderAuditClassification(session legacyProviderAuditSession, reason string) providerAuditRequirementClassification {
	classification := providerAuditClassification(session, "unresolved", reason)
	classification.AuditState = "failed"
	classification.AuditErrorCode = providerAuditUnresolvedCode
	return classification
}

func legacyProviderAuditStillActive(session legacyProviderAuditSession) bool {
	switch session.SessionState {
	case "starting", "running", "resumed":
	default:
		return false
	}
	switch session.AttemptStatus {
	case "preparing", "running", "waiting":
	default:
		return false
	}
	return session.LeaseState == "active"
}
