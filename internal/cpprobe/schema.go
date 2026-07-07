package cpprobe

import (
	"errors"
	"fmt"
)

const (
	ManualTraceArtifact         = "cp_probe_001_manual_trace.md"
	InspectRedactedArtifact     = "cp_probe_001_inspect_redacted.json"
	GitOperationSummaryArtifact = "cp_probe_001_git_operation_summary.json"
	FailureMatrixArtifact       = "cp_probe_001_failure_matrix.md"
	ConclusionReportArtifact    = "cp_probe_001_conclusion.md"
)

type ManualTrace struct {
	Scenario string      `json:"scenario"`
	Steps    []TraceStep `json:"steps"`
}

type TraceStep struct {
	Actor        string            `json:"actor"`
	EntryPoint   string            `json:"entrypoint"`
	Capability   string            `json:"capability"`
	InputSummary string            `json:"input_summary"`
	Status       string            `json:"status"`
	ErrorCode    string            `json:"error_code,omitempty"`
	CanonicalIDs map[string]string `json:"canonical_ids,omitempty"`
	NextActions  []string          `json:"next_actions,omitempty"`
}

type RedactedInspect struct {
	Scenario       string              `json:"scenario"`
	Status         string              `json:"status"`
	TeamID         string              `json:"team_id"`
	RootContractID string              `json:"root_contract_id"`
	Counts         map[string]int64    `json:"counts"`
	Contracts      []ContractSummary   `json:"contracts,omitempty"`
	Workspaces     []WorkspaceSummary  `json:"workspaces,omitempty"`
	GitOperations  []GitOperationBrief `json:"git_operations,omitempty"`
	Redacted       bool                `json:"redacted"`
	DenylistChecks []string            `json:"denylist_checks,omitempty"`
}

type GitOperationSummary struct {
	Scenario       string               `json:"scenario"`
	Repositories   []RepositorySummary  `json:"repositories,omitempty"`
	Workspaces     []WorkspaceSummary   `json:"workspaces,omitempty"`
	Operations     []GitOperationBrief  `json:"operations"`
	RollbackPoints []RollbackPointBrief `json:"rollback_points,omitempty"`
	NoActiveLocks  bool                 `json:"no_active_locks"`
}

type RepositorySummary struct {
	ID              string `json:"id"`
	CanonicalBranch string `json:"canonical_branch"`
	Status          string `json:"status"`
}

type ContractSummary struct {
	ID             string `json:"id"`
	IssuerContract string `json:"issuer_contract_id,omitempty"`
	TargetID       string `json:"target_id"`
	Status         string `json:"status"`
}

type WorkspaceSummary struct {
	ID         string `json:"id"`
	RepoID     string `json:"repo_id"`
	AgentID    string `json:"agent_id"`
	ContractID string `json:"contract_id,omitempty"`
	BaseRef    string `json:"base_ref"`
	HeadRef    string `json:"head_ref"`
	State      string `json:"state"`
}

type GitOperationBrief struct {
	ID            string `json:"id"`
	OperationType string `json:"operation_type"`
	ActorAgentID  string `json:"actor_agent_id"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	RepoID        string `json:"repo_id"`
	BeforeRef     string `json:"before_ref"`
	AfterRef      string `json:"after_ref"`
	State         string `json:"state"`
	ErrorCode     string `json:"error_code,omitempty"`
}

type RollbackPointBrief struct {
	ID          string `json:"id"`
	OperationID string `json:"operation_id"`
	RepoID      string `json:"repo_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	TargetRef   string `json:"target_ref"`
	BeforeRef   string `json:"before_ref"`
	AfterRef    string `json:"after_ref"`
	State       string `json:"state"`
}

type FailureMatrix struct {
	Scenario string              `json:"scenario"`
	Items    []FailureMatrixItem `json:"items"`
}

type FailureMatrixItem struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	Capability         string   `json:"capability"`
	ExpectedErrorCodes []string `json:"expected_error_codes,omitempty"`
	ActualErrorCode    string   `json:"actual_error_code,omitempty"`
	StateAssertion     string   `json:"state_assertion"`
	NextStep           string   `json:"next_step,omitempty"`
}

type ConclusionStatus string

const (
	ConclusionPassed             ConclusionStatus = "passed"
	ConclusionFailed             ConclusionStatus = "failed"
	ConclusionEnvironmentBlocked ConclusionStatus = "environment_blocked"
)

type ConclusionReport struct {
	Scenario         string           `json:"scenario"`
	Status           ConclusionStatus `json:"status"`
	ManualTraceRef   string           `json:"manual_trace_ref"`
	InspectRef       string           `json:"inspect_ref"`
	GitSummaryRef    string           `json:"git_summary_ref"`
	FailureMatrixRef string           `json:"failure_matrix_ref"`
	Covered          []string         `json:"covered"`
	NotCovered       []string         `json:"not_covered,omitempty"`
	NextSteps        []string         `json:"next_steps,omitempty"`
}

func (t ManualTrace) Validate() error {
	if t.Scenario == "" {
		return errors.New("manual trace scenario is required")
	}
	if len(t.Steps) == 0 {
		return errors.New("manual trace requires at least one step")
	}
	for i, step := range t.Steps {
		if step.Actor == "" || step.EntryPoint == "" || step.Capability == "" || step.Status == "" {
			return fmt.Errorf("manual trace step %d requires actor, entrypoint, capability, and status", i)
		}
	}
	return nil
}

func (i RedactedInspect) Validate() error {
	if i.Scenario == "" || i.Status == "" || i.TeamID == "" || i.RootContractID == "" {
		return errors.New("redacted inspect requires scenario, status, team_id, and root_contract_id")
	}
	if !i.Redacted {
		return errors.New("redacted inspect must set redacted=true")
	}
	if i.Counts == nil {
		return errors.New("redacted inspect requires counts")
	}
	return nil
}

func (s GitOperationSummary) Validate() error {
	if s.Scenario == "" {
		return errors.New("git operation summary scenario is required")
	}
	if len(s.Operations) == 0 {
		return errors.New("git operation summary requires at least one operation")
	}
	for _, op := range s.Operations {
		if op.ID == "" || op.OperationType == "" || op.ActorAgentID == "" || op.RepoID == "" || op.State == "" {
			return fmt.Errorf("git operation %q requires id, operation_type, actor_agent_id, repo_id, and state", op.ID)
		}
	}
	for _, point := range s.RollbackPoints {
		if point.ID == "" || point.OperationID == "" || point.RepoID == "" || point.TargetRef == "" || point.BeforeRef == "" || point.AfterRef == "" || point.State == "" {
			return fmt.Errorf("rollback point %q requires id, operation_id, repo_id, target_ref, before_ref, after_ref, and state", point.ID)
		}
	}
	return nil
}

func (m FailureMatrix) Validate() error {
	if m.Scenario == "" {
		return errors.New("failure matrix scenario is required")
	}
	if len(m.Items) == 0 {
		return errors.New("failure matrix requires at least one item")
	}
	for _, item := range m.Items {
		if item.ID == "" || item.Status == "" || item.Capability == "" || item.StateAssertion == "" {
			return fmt.Errorf("failure matrix item %q requires status, capability, and state assertion", item.ID)
		}
		switch item.Status {
		case "covered", "pending", "failed", "environment_blocked":
		default:
			return fmt.Errorf("failure matrix item %q has invalid status %q", item.ID, item.Status)
		}
	}
	return nil
}

func (r ConclusionReport) Validate() error {
	if r.Scenario == "" {
		return errors.New("conclusion report scenario is required")
	}
	switch r.Status {
	case ConclusionPassed, ConclusionFailed, ConclusionEnvironmentBlocked:
	default:
		return fmt.Errorf("conclusion report status %q is invalid", r.Status)
	}
	if r.ManualTraceRef == "" || r.InspectRef == "" || r.GitSummaryRef == "" || r.FailureMatrixRef == "" {
		return errors.New("conclusion report requires all artifact refs")
	}
	if r.Status == ConclusionPassed && len(r.NotCovered) > 0 {
		return errors.New("passed conclusion cannot list uncovered requirements")
	}
	return nil
}
