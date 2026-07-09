package releaseacceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/store"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Service struct {
	db           *sql.DB
	store        *store.Store
	dbPath       string
	capabilities []string
}

type Config struct {
	Store        *store.Store
	DBPath       string
	Capabilities []string
}

type EvaluateInput struct {
	RootContractID string `json:"root_contract_id"`
	TeamID         string `json:"team_id,omitempty"`
	TeamVersion    int    `json:"team_version,omitempty"`
	RunLabel       string `json:"run_label,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
}

type Acceptance struct {
	ID               string            `json:"id"`
	RootContractID   string            `json:"root_contract_id"`
	TeamID           string            `json:"team_id"`
	TeamVersion      int               `json:"team_version"`
	Status           string            `json:"status"`
	RunLabel         string            `json:"run_label,omitempty"`
	PredicateResults []PredicateResult `json:"predicate_results"`
	EvidenceRefs     []string          `json:"evidence_refs,omitempty"`
	InspectSummary   InspectSummary    `json:"inspect_summary"`
	EventCursor      map[string]any    `json:"event_cursor,omitempty"`
	FailureSummary   string            `json:"failure_summary,omitempty"`
	CreatedBy        string            `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
}

type PredicateResult struct {
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Required      bool     `json:"required"`
	Message       string   `json:"message"`
	CanonicalRefs []string `json:"canonical_refs,omitempty"`
}

type InspectSummary struct {
	RootContractID string         `json:"root_contract_id,omitempty"`
	TeamID         string         `json:"team_id,omitempty"`
	TeamVersion    int            `json:"team_version,omitempty"`
	Status         string         `json:"status,omitempty"`
	PredicateCount int            `json:"predicate_count"`
	PassedCount    int            `json:"passed_count"`
	FailedCount    int            `json:"failed_count"`
	BlockedCount   int            `json:"blocked_count"`
	Failed         []string       `json:"failed,omitempty"`
	CanonicalRefs  map[string]int `json:"canonical_ref_counts,omitempty"`
}

type Summary struct {
	Latest *Acceptance    `json:"latest,omitempty"`
	Counts map[string]int `json:"counts"`
}

type evidenceContext struct {
	RootContractID  string
	TeamID          string
	TeamVersion     int
	ContractIDs     []string
	ContractSet     map[string]bool
	RootExists      bool
	RootStatus      string
	RootTeamID      string
	RootTeamVersion int
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("release_acceptance: store is required")
	}
	return &Service{
		db:           cfg.Store.DB(),
		store:        cfg.Store,
		dbPath:       cfg.DBPath,
		capabilities: append([]string(nil), cfg.Capabilities...),
	}, nil
}

func (s *Service) Evaluate(ctx context.Context, in EvaluateInput) (Acceptance, error) {
	if strings.TrimSpace(in.RootContractID) == "" {
		return Acceptance{}, errors.New("release_acceptance: root_contract_id is required")
	}
	in.RootContractID = strings.TrimSpace(in.RootContractID)
	in.TeamID = strings.TrimSpace(in.TeamID)
	in.RunLabel = strings.TrimSpace(in.RunLabel)
	if in.CreatedBy == "" {
		in.CreatedBy = "operator"
	}
	if in.TeamID == "" || in.TeamVersion == 0 {
		teamID, version, err := s.activeTeam(ctx)
		if err != nil {
			return Acceptance{}, err
		}
		if in.TeamID == "" {
			in.TeamID = teamID
		}
		if in.TeamVersion == 0 {
			in.TeamVersion = version
		}
	}
	if existing, ok, err := s.acceptanceByRun(ctx, in); err != nil {
		return Acceptance{}, err
	} else if ok {
		return existing, nil
	}

	evCtx, err := s.evidenceContext(ctx, in)
	if err != nil {
		return Acceptance{}, err
	}
	predicates := []PredicateResult{
		s.predicateRootContract(ctx, evCtx),
		s.predicateTeamScopeBinding(ctx, evCtx),
		s.predicateBackendDurable(ctx),
		s.predicateTeamConfig(ctx, evCtx),
		s.predicateDockerRuntime(ctx, evCtx),
		s.predicateRealCLI(ctx, evCtx),
		s.predicateCommandRun(ctx, evCtx),
		s.predicateValidation(ctx, evCtx),
		s.predicateRootComplete(ctx, evCtx),
		s.predicateMailboxResumeSteer(ctx, evCtx),
		s.predicateGitChangeset(ctx, evCtx),
	}
	status := aggregateStatus(predicates)
	evidenceRefs := collectRefs(predicates)
	inspectSummary := summarize(in.RootContractID, in.TeamID, in.TeamVersion, status, predicates, evidenceRefs)
	failureSummary := failureSummary(predicates)
	return s.record(ctx, in, status, predicates, evidenceRefs, inspectSummary, failureSummary)
}

func (s *Service) predicateRootContract(ctx context.Context, ev evidenceContext) PredicateResult {
	if !ev.RootExists {
		return failed("root_contract_present", "root contract does not exist", "contract:"+ev.RootContractID)
	}
	return passed("root_contract_present", "root contract exists", "contract:"+ev.RootContractID)
}

func (s *Service) predicateTeamScopeBinding(ctx context.Context, ev evidenceContext) PredicateResult {
	if !ev.RootExists {
		return failed("team_scope_binding", "root contract does not exist, so TeamConfig scope cannot be proven", "contract:"+ev.RootContractID)
	}
	if ev.RootTeamID != ev.TeamID || ev.RootTeamVersion != ev.TeamVersion {
		return failed("team_scope_binding", "root contract TeamConfig binding does not match requested team/version", "contract:"+ev.RootContractID, "team:"+ev.TeamID, fmt.Sprintf("version:%d", ev.TeamVersion))
	}
	if len(ev.ContractIDs) == 0 {
		return failed("team_scope_binding", "contract lineage is empty", "contract:"+ev.RootContractID)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT contract_id, team_id, team_version
FROM contract_team_scopes
WHERE contract_id IN (`+placeholders(len(ev.ContractIDs))+`)`, anySlice(ev.ContractIDs)...)
	if err != nil {
		return failed("team_scope_binding", "TeamConfig scope binding lookup failed", err.Error())
	}
	defer rows.Close()
	bound := map[string]struct {
		teamID      string
		teamVersion int
	}{}
	for rows.Next() {
		var contractID, teamID string
		var teamVersion int
		if err := rows.Scan(&contractID, &teamID, &teamVersion); err != nil {
			return failed("team_scope_binding", "TeamConfig scope binding scan failed", err.Error())
		}
		bound[contractID] = struct {
			teamID      string
			teamVersion int
		}{teamID: teamID, teamVersion: teamVersion}
	}
	if err := rows.Err(); err != nil {
		return failed("team_scope_binding", "TeamConfig scope binding iteration failed", err.Error())
	}
	var bad []string
	for _, contractID := range ev.ContractIDs {
		scope, ok := bound[contractID]
		if !ok {
			bad = append(bad, "missing_contract_team_scope:"+contractID)
			continue
		}
		if scope.teamID != ev.TeamID || scope.teamVersion != ev.TeamVersion {
			bad = append(bad, fmt.Sprintf("mismatched_contract_team_scope:%s:%s:%d", contractID, scope.teamID, scope.teamVersion))
		}
	}
	if len(bad) > 0 {
		return failed("team_scope_binding", "contract lineage cannot be proven to belong to requested TeamConfig scope", bad...)
	}
	refs := []string{"team:" + ev.TeamID, fmt.Sprintf("team_version:%d", ev.TeamVersion)}
	for _, contractID := range ev.ContractIDs {
		refs = append(refs, "contract:"+contractID)
	}
	return passed("team_scope_binding", "contract lineage is bound to requested TeamConfig scope", refs...)
}

func (s *Service) predicateBackendDurable(ctx context.Context) PredicateResult {
	var missing []string
	if s.dbPath == "" || s.dbPath == ":memory:" {
		missing = append(missing, "file_backed_sqlite")
	}
	applied, err := s.migrations(ctx)
	if err != nil {
		return failed("backend_durable_ready", "migration lookup failed", err.Error())
	}
	requiredMigrations := []string{
		"001_core_store_queue_events",
		"002_team_config_skill_registry",
		"003_session_lifecycle_guards",
		"004_object_store",
		"005_controlled_git_v1",
		"006_controlled_git_v2",
		"007_runtime_evidence",
		"008_cli_sessions",
		"009_command_runs",
		"010_runtime_tokens",
		"011_validation_assessments",
		"012_release_acceptances",
		"013_contract_team_scopes",
		"014_agent_communication_envelopes",
		"015_controlled_git_operation_evidence",
		"016_controlled_git_operation_subject_kind",
	}
	for _, migration := range requiredMigrations {
		if !applied[migration] {
			missing = append(missing, migration)
		}
	}
	capabilities := map[string]bool{}
	for _, name := range s.capabilities {
		capabilities[name] = true
	}
	for _, name := range []string{"command.run", "validation.assessment", "contract.complete", "mailbox.resolve", "changeset.submit"} {
		if !capabilities[name] {
			missing = append(missing, "capability:"+name)
		}
	}
	if len(missing) > 0 {
		return failed("backend_durable_ready", "backend durability or registry evidence is missing", missing...)
	}
	return passed("backend_durable_ready", "backend migrations and registries are durable", "db:file")
}

func (s *Service) predicateTeamConfig(ctx context.Context, ev evidenceContext) PredicateResult {
	var configJSON string
	err := s.db.QueryRowContext(ctx, `
SELECT config_json
FROM team_config_versions
WHERE team_id = ? AND version = ? AND active = 1`, ev.TeamID, ev.TeamVersion).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return failed("teamconfig_validation_policy", "active TeamConfig version was not found", "team:"+ev.TeamID)
	}
	if err != nil {
		return failed("teamconfig_validation_policy", "TeamConfig lookup failed", err.Error())
	}
	var cfg struct {
		Termination map[string]any `json:"termination"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return failed("teamconfig_validation_policy", "TeamConfig JSON could not be decoded", err.Error())
	}
	acceptedBy := stringFromMap(cfg.Termination, "accepted_by_capability")
	finalCapability := stringFromMap(cfg.Termination, "final_acceptance_capability")
	if acceptedBy != "validation.assessment" && finalCapability != "validation.assessment" {
		return failed("teamconfig_validation_policy", "TeamConfig termination policy does not point to validation.assessment", "team:"+ev.TeamID)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT agent_id, capabilities_json
FROM team_config_agents
WHERE team_id = ? AND version = ?`, ev.TeamID, ev.TeamVersion)
	if err != nil {
		return failed("teamconfig_validation_policy", "TeamConfig agent lookup failed", err.Error())
	}
	defer rows.Close()
	var validationAgents []string
	for rows.Next() {
		var agentID, raw string
		if err := rows.Scan(&agentID, &raw); err != nil {
			return failed("teamconfig_validation_policy", "TeamConfig agent scan failed", err.Error())
		}
		var caps []string
		if err := json.Unmarshal([]byte(raw), &caps); err != nil {
			return failed("teamconfig_validation_policy", "TeamConfig capability list could not be decoded", agentID)
		}
		if contains(caps, "validation.assessment") {
			validationAgents = append(validationAgents, agentID)
		}
	}
	if err := rows.Err(); err != nil {
		return failed("teamconfig_validation_policy", "TeamConfig agent iteration failed", err.Error())
	}
	if len(validationAgents) == 0 {
		return failed("teamconfig_validation_policy", "no TeamConfig agent is authorized for validation.assessment", "team:"+ev.TeamID)
	}
	sort.Strings(validationAgents)
	return passed("teamconfig_validation_policy", "TeamConfig delegates final acceptance to validation.assessment", prefixed("agent", validationAgents)...)
}

func (s *Service) predicateDockerRuntime(ctx context.Context, ev evidenceContext) PredicateResult {
	if len(ev.ContractIDs) == 0 {
		return failed("docker_runtime_evidence", "contract lineage is empty", "contract:"+ev.RootContractID)
	}
	rows, err := s.queryLineage(ctx, ev, `
SELECT ri.id, ri.runtime_kind, ri.state, ri.checks_json, a.cli_backend
FROM runtime_instances ri
JOIN attempts a ON a.id = ri.attempt_id
JOIN leases l ON l.id = a.lease_id
JOIN assignments asn ON asn.id = l.assignment_id
WHERE asn.contract_id IN (`+placeholders(len(ev.ContractIDs))+`)`, ev.ContractIDs...)
	if err != nil {
		return failed("docker_runtime_evidence", "runtime lookup failed", err.Error())
	}
	defer rows.Close()
	var refs []string
	var bad []string
	for rows.Next() {
		var id, kind, state, checksJSON, cliBackend string
		if err := rows.Scan(&id, &kind, &state, &checksJSON, &cliBackend); err != nil {
			return failed("docker_runtime_evidence", "runtime scan failed", err.Error())
		}
		refs = append(refs, "runtime:"+id)
		if kind != "docker" || (state != "ready" && state != "stopped") {
			bad = append(bad, fmt.Sprintf("%s:%s:%s", id, kind, state))
		}
		checks := map[string]bool{}
		if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
			bad = append(bad, "invalid_runtime_checks:"+id)
			continue
		}
		for _, name := range []string{"workspace_writable", "home_writable", "git_workspace_writable", "cli_user_consistent"} {
			if !checks[name] {
				bad = append(bad, "missing_runtime_check:"+id+":"+name)
			}
		}
		if cliBackend == "claude" {
			for _, name := range []string{"claude_present", "claude_auth_configured", "claude_auth_probe_passed", "claude_auth_probe_redacted", "home_private", "home_persistent"} {
				if !checks[name] {
					bad = append(bad, "missing_claude_auth_check:"+id+":"+name)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return failed("docker_runtime_evidence", "runtime iteration failed", err.Error())
	}
	if len(refs) == 0 {
		return failed("docker_runtime_evidence", "no runtime instances exist for the release contract lineage")
	}
	if len(bad) > 0 {
		return failed("docker_runtime_evidence", "runtime evidence contains non-Docker or non-terminal-ready instances", bad...)
	}
	return passed("docker_runtime_evidence", "all runtime evidence is Docker-backed", refs...)
}

func (s *Service) predicateRealCLI(ctx context.Context, ev evidenceContext) PredicateResult {
	rows, err := s.queryLineage(ctx, ev, `
SELECT cs.id, cs.cli_backend, cs.start_reason, cs.state, COALESCE(cs.transcript_ref, '')
FROM cli_sessions cs
JOIN attempts a ON a.id = cs.attempt_id
JOIN leases l ON l.id = a.lease_id
JOIN assignments asn ON asn.id = l.assignment_id
WHERE asn.contract_id IN (`+placeholders(len(ev.ContractIDs))+`)`, ev.ContractIDs...)
	if err != nil {
		return failed("real_cli_session_lifecycle", "CLI session lookup failed", err.Error())
	}
	defer rows.Close()
	var refs []string
	var bad []string
	var sawStart, sawResume, sawTerminal bool
	for rows.Next() {
		var id, backend, reason, state, transcriptRef string
		if err := rows.Scan(&id, &backend, &reason, &state, &transcriptRef); err != nil {
			return failed("real_cli_session_lifecycle", "CLI session scan failed", err.Error())
		}
		refs = append(refs, "cli_session:"+id)
		if backend == "fake" || backend == "" {
			bad = append(bad, "fake_or_missing_cli:"+id)
		}
		if transcriptRef == "" {
			bad = append(bad, "missing_transcript:"+id)
		}
		if reason == "start" {
			sawStart = true
		}
		if reason == "resume" {
			sawResume = true
		}
		if state == "finished" || state == "exited" {
			sawTerminal = true
		}
	}
	if err := rows.Err(); err != nil {
		return failed("real_cli_session_lifecycle", "CLI session iteration failed", err.Error())
	}
	switch {
	case len(refs) == 0:
		return failed("real_cli_session_lifecycle", "no CLI session evidence exists")
	case len(bad) > 0:
		return failed("real_cli_session_lifecycle", "CLI session evidence is fake or incomplete", bad...)
	case !sawStart || !sawResume || !sawTerminal:
		return failed("real_cli_session_lifecycle", "CLI lifecycle lacks start, resume, or terminal evidence", refs...)
	default:
		return passed("real_cli_session_lifecycle", "real CLI start/resume/finish evidence exists", refs...)
	}
}

func (s *Service) predicateCommandRun(ctx context.Context, ev evidenceContext) PredicateResult {
	rows, err := s.queryLineage(ctx, ev, `
SELECT cr.id, cr.agent_id, cr.lease_id, cr.contract_id, cr.status, cr.stdout_ref, cr.stderr_ref, cr.evidence_id, ri.runtime_kind
FROM command_runs cr
LEFT JOIN runtime_instances ri ON ri.runtime_id = cr.runtime_id AND ri.attempt_id = cr.attempt_id
WHERE cr.contract_id IN (`+placeholders(len(ev.ContractIDs))+`)`, ev.ContractIDs...)
	if err != nil {
		return failed("command_run_public_evidence", "command_run lookup failed", err.Error())
	}
	type commandRunRow struct {
		id          string
		agentID     string
		leaseID     string
		contractID  string
		status      string
		stdoutRef   string
		stderrRef   string
		evidenceID  string
		runtimeKind string
	}
	var runs []commandRunRow
	for rows.Next() {
		var run commandRunRow
		if err := rows.Scan(&run.id, &run.agentID, &run.leaseID, &run.contractID, &run.status, &run.stdoutRef, &run.stderrRef, &run.evidenceID, &run.runtimeKind); err != nil {
			_ = rows.Close()
			return failed("command_run_public_evidence", "command_run scan failed", err.Error())
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return failed("command_run_public_evidence", "command_run iteration failed", err.Error())
	}
	if err := rows.Err(); err != nil {
		return failed("command_run_public_evidence", "command_run iteration failed", err.Error())
	}
	var refs []string
	var bad []string
	for _, run := range runs {
		refs = append(refs, "command_run:"+run.id)
		if run.status != "succeeded" || run.stdoutRef == "" || run.stderrRef == "" || run.evidenceID == "" || run.runtimeKind != "docker" {
			bad = append(bad, "incomplete_command_run:"+run.id)
			continue
		}
		if !s.acceptedCapabilityCallForLease(ctx, "command.run", run.agentID, run.leaseID) {
			bad = append(bad, "missing_public_call:"+run.id)
		}
		if !s.evidenceExistsInLineage(ctx, ev, run.evidenceID, "command_run", "") {
			bad = append(bad, "missing_command_evidence:"+run.id)
		}
	}
	if len(refs) == 0 {
		return failed("command_run_public_evidence", "no command_run evidence exists for the release lineage")
	}
	if len(bad) > 0 {
		return failed("command_run_public_evidence", "command_run evidence is incomplete or internal-only", bad...)
	}
	return passed("command_run_public_evidence", "public command.run evidence is durable", refs...)
}

func (s *Service) predicateValidation(ctx context.Context, ev evidenceContext) PredicateResult {
	rows, err := s.queryLineage(ctx, ev, `
SELECT id, verifier_agent_id, lease_id, contract_id, verdict, checked_refs_json, evidence_id
FROM validation_assessments
WHERE contract_id IN (`+placeholders(len(ev.ContractIDs))+`)
   OR assessed_contract_id IN (`+placeholders(len(ev.ContractIDs))+`)`, append(ev.ContractIDs, ev.ContractIDs...)...)
	if err != nil {
		return failed("validation_assessment_pass", "validation lookup failed", err.Error())
	}
	type validationRow struct {
		id              string
		agentID         string
		leaseID         string
		contractID      string
		verdict         string
		checkedRefsJSON string
		evidenceID      string
	}
	var assessments []validationRow
	for rows.Next() {
		var assessment validationRow
		if err := rows.Scan(&assessment.id, &assessment.agentID, &assessment.leaseID, &assessment.contractID, &assessment.verdict, &assessment.checkedRefsJSON, &assessment.evidenceID); err != nil {
			_ = rows.Close()
			return failed("validation_assessment_pass", "validation scan failed", err.Error())
		}
		assessments = append(assessments, assessment)
	}
	if err := rows.Close(); err != nil {
		return failed("validation_assessment_pass", "validation iteration failed", err.Error())
	}
	if err := rows.Err(); err != nil {
		return failed("validation_assessment_pass", "validation iteration failed", err.Error())
	}
	var refs []string
	var bad []string
	var sawPass bool
	for _, assessment := range assessments {
		refs = append(refs, "validation_assessment:"+assessment.id)
		if assessment.verdict != "pass" {
			bad = append(bad, "non_pass_verdict:"+assessment.id+":"+assessment.verdict)
			continue
		}
		if !s.acceptedCapabilityCallForLease(ctx, "validation.assessment", assessment.agentID, assessment.leaseID) {
			bad = append(bad, "missing_public_call:"+assessment.id)
			continue
		}
		if !s.evidenceExistsInLineage(ctx, ev, assessment.evidenceID, "validation_assessment", "pass") {
			bad = append(bad, "missing_validation_evidence:"+assessment.id)
			continue
		}
		for _, missing := range s.missingScopedCheckedRefs(ctx, ev, assessment.checkedRefsJSON, "command_run", "changeset", "evidence") {
			bad = append(bad, missing+":"+assessment.id)
		}
		sawPass = true
	}
	if len(refs) == 0 {
		return failed("validation_assessment_pass", "no canonical validation_assessment exists")
	}
	if len(bad) > 0 || !sawPass {
		return failed("validation_assessment_pass", "validation evidence did not produce a canonical pass", append(bad, refs...)...)
	}
	return passed("validation_assessment_pass", "canonical validation assessment passed", refs...)
}

func (s *Service) predicateRootComplete(ctx context.Context, ev evidenceContext) PredicateResult {
	if ev.RootStatus != "satisfied" {
		return failed("root_contract_completed", "root contract is not satisfied", "contract:"+ev.RootContractID)
	}
	completionLeaseID := s.rootCompletionLease(ctx, ev.RootContractID)
	if completionLeaseID == "" {
		return failed("root_contract_completed", "root satisfaction event is missing", "contract:"+ev.RootContractID)
	}
	agentID := s.leaseAgent(ctx, completionLeaseID)
	if !s.acceptedCapabilityCallForLease(ctx, "contract.complete", agentID, completionLeaseID) {
		return failed("root_contract_completed", "accepted contract.complete capability call is missing", "contract:"+ev.RootContractID)
	}
	if !s.validationEvidenceInLineage(ctx, ev) {
		return failed("root_contract_completed", "root completion cannot be tied to validation evidence in lineage", "contract:"+ev.RootContractID)
	}
	return passed("root_contract_completed", "root contract was explicitly completed", "contract:"+ev.RootContractID)
}

func (s *Service) predicateMailboxResumeSteer(ctx context.Context, ev evidenceContext) PredicateResult {
	rows, err := s.queryLineage(ctx, ev, `
SELECT id, COALESCE(session_route_id, '')
FROM mailbox_items
WHERE contract_id IN (`+placeholders(len(ev.ContractIDs))+`)
  AND reason <> 'task_assigned'`, ev.ContractIDs...)
	if err != nil {
		return failed("mailbox_resume_steer", "mailbox lookup failed", err.Error())
	}
	type mailboxRow struct {
		id      string
		routeID string
	}
	var mailboxes []mailboxRow
	var mailboxRefs []string
	for rows.Next() {
		var id, routeID string
		if err := rows.Scan(&id, &routeID); err != nil {
			return failed("mailbox_resume_steer", "mailbox scan failed", err.Error())
		}
		mailboxes = append(mailboxes, mailboxRow{id: id, routeID: routeID})
		mailboxRefs = append(mailboxRefs, "mailbox:"+id)
	}
	if err := rows.Err(); err != nil {
		return failed("mailbox_resume_steer", "mailbox iteration failed", err.Error())
	}
	if err := rows.Close(); err != nil {
		return failed("mailbox_resume_steer", "mailbox iteration failed", err.Error())
	}
	if len(mailboxRefs) == 0 {
		return failed("mailbox_resume_steer", "mailbox delivery/resume/steer evidence is missing", "mailbox")
	}
	var bad []string
	var refs []string
	for _, mailbox := range mailboxes {
		sameTurnRefs, sameTurnOK, sameTurnMissing := s.sameTurnMailboxPath(ctx, mailbox.id)
		if sameTurnOK {
			refs = append(refs, sameTurnRefs...)
			continue
		}
		fallbackRefs, fallbackOK, fallbackMissing := s.fallbackResumeMailboxPath(ctx, mailbox.id, mailbox.routeID)
		if fallbackOK {
			refs = append(refs, fallbackRefs...)
			continue
		}
		bad = append(bad, prefixed("same_turn_missing", sameTurnMissing)...)
		bad = append(bad, prefixed("fallback_missing", fallbackMissing)...)
		bad = append(bad, "mailbox_delivery_path:"+mailbox.id)
	}
	if len(bad) > 0 {
		return failed("mailbox_resume_steer", "mailbox delivery/resume/steer evidence is missing or incomplete", bad...)
	}
	refs = append(refs, mailboxRefs...)
	return passed("mailbox_resume_steer", "mailbox delivery has complete same-turn or fallback resume evidence", refs...)
}

type resumeQueuedEvidence struct {
	RouteID               string
	QueueItemID           string
	Reason                string
	CLIBackend            string
	SupportsSameTurnSteer bool
	CapabilityKnown       bool
}

func (s *Service) sameTurnMailboxPath(ctx context.Context, mailboxID string) ([]string, bool, []string) {
	routeIDs, err := s.acceptedDeliveryRoutes(ctx, mailboxID)
	if err != nil {
		return nil, false, []string{"delivery_attempt_lookup"}
	}
	if len(routeIDs) == 0 {
		return nil, false, []string{"delivery_attempt.accepted"}
	}
	if !s.mailboxSteerSent(ctx, mailboxID) {
		return nil, false, []string{"session.steer_sent"}
	}
	var missing []string
	for _, routeID := range routeIDs {
		capability, known, _ := s.routeAdapterCapability(ctx, routeID)
		switch {
		case !known:
			missing = append(missing, "adapter_capability_unknown:"+routeID)
		case !capability.SupportsSameTurnSteer:
			missing = append(missing, "adapter_same_turn_unsupported:"+routeID)
		default:
			return []string{"mailbox:" + mailboxID, "session_route:" + routeID, "delivery_attempt:same_turn"}, true, nil
		}
	}
	return nil, false, missing
}

func (s *Service) fallbackResumeMailboxPath(ctx context.Context, mailboxID, mailboxRouteID string) ([]string, bool, []string) {
	events, err := s.resumeQueuedEvents(ctx, mailboxID)
	if err != nil {
		return nil, false, []string{"delivery.resume_queued_lookup"}
	}
	if len(events) == 0 {
		return nil, false, []string{"delivery.resume_queued"}
	}
	if !s.fallbackDeliveryAttemptExists(ctx, mailboxID) {
		return nil, false, []string{"delivery_attempt.fallback_or_failed"}
	}
	var allMissing []string
	for _, event := range events {
		routeID := firstNonEmpty(event.RouteID, mailboxRouteID)
		missing := []string{}
		if routeID == "" {
			missing = append(missing, "session_route")
		}
		if !event.CapabilityKnown {
			missing = append(missing, "adapter_capability_unknown")
		}
		if event.QueueItemID == "" {
			missing = append(missing, "queue_item_id")
		} else if !s.resumeQueueDone(ctx, event.QueueItemID, mailboxID) {
			missing = append(missing, "runtime.resume_done")
		}
		if routeID != "" {
			routeCapability, known, backend := s.routeAdapterCapability(ctx, routeID)
			switch {
			case !known:
				missing = append(missing, "adapter_capability_unknown:"+routeID)
			case event.CapabilityKnown && routeCapability.SupportsSameTurnSteer != event.SupportsSameTurnSteer:
				missing = append(missing, "adapter_capability_mismatch:"+routeID)
			}
			if event.CLIBackend != "" && backend != "" && event.CLIBackend != backend {
				missing = append(missing, "adapter_backend_mismatch:"+routeID)
			}
			if !s.sessionResumedForMailbox(ctx, routeID, mailboxID) {
				missing = append(missing, "session.resumed")
			}
			if !s.resumeCLISessionForRoute(ctx, routeID) {
				missing = append(missing, "cli_session.resume")
			}
		}
		if len(missing) == 0 {
			return []string{"mailbox:" + mailboxID, "session_route:" + routeID, "queue_item:" + event.QueueItemID, "delivery_attempt:fallback_resume"}, true, nil
		}
		allMissing = append(allMissing, missing...)
	}
	return nil, false, allMissing
}

func (s *Service) acceptedDeliveryRoutes(ctx context.Context, mailboxID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(route_id, '')
FROM delivery_attempts
WHERE mailbox_item_id = ? AND state = 'accepted'
ORDER BY created_at, id`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var routeID string
		if err := rows.Scan(&routeID); err != nil {
			return nil, err
		}
		if routeID != "" {
			out = append(out, routeID)
		}
	}
	return compactStrings(out), rows.Err()
}

func (s *Service) fallbackDeliveryAttemptExists(ctx context.Context, mailboxID string) bool {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM delivery_attempts
WHERE mailbox_item_id = ? AND state IN ('failed', 'fallback')`, mailboxID).Scan(&count)
	return err == nil && count > 0
}

func (s *Service) mailboxSteerSent(ctx context.Context, mailboxID string) bool {
	return s.eventExistsForAny(ctx, "session.steer_sent", "mailbox_item", []string{mailboxID})
}

func (s *Service) resumeQueuedEvents(ctx context.Context, mailboxID string) ([]resumeQueuedEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'delivery.resume_queued'
  AND aggregate_type = 'mailbox_item'
  AND aggregate_id = ?
ORDER BY occurred_at, id`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resumeQueuedEvidence
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			out = append(out, resumeQueuedEvidence{})
			continue
		}
		capability, known := boolFromMap(payload, "supports_same_turn_steer")
		out = append(out, resumeQueuedEvidence{
			RouteID:               stringFromMap(payload, "route_id"),
			QueueItemID:           stringFromMap(payload, "queue_item_id"),
			Reason:                stringFromMap(payload, "reason"),
			CLIBackend:            stringFromMap(payload, "cli_backend"),
			SupportsSameTurnSteer: capability,
			CapabilityKnown:       known,
		})
	}
	return out, rows.Err()
}

func (s *Service) routeAdapterCapability(ctx context.Context, routeID string) (struct{ SupportsSameTurnSteer bool }, bool, string) {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'cli.adapter_capabilities'
  AND aggregate_type = 'session_route'
  AND aggregate_id = ?
ORDER BY occurred_at DESC, id DESC`, routeID)
	if err != nil {
		return struct{ SupportsSameTurnSteer bool }{}, false, ""
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return struct{ SupportsSameTurnSteer bool }{}, false, ""
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		backend := stringFromMap(payload, "cli_backend")
		supports, ok := boolFromMap(payload, "supports_same_turn_steer")
		if ok {
			return struct{ SupportsSameTurnSteer bool }{SupportsSameTurnSteer: supports}, true, backend
		}
		if backend != "" {
			return struct{ SupportsSameTurnSteer bool }{}, false, backend
		}
	}
	return struct{ SupportsSameTurnSteer bool }{}, false, ""
}

func (s *Service) resumeQueueDone(ctx context.Context, queueItemID, mailboxID string) bool {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM queue_items
WHERE id = ?
  AND queue_name = 'runtime.resume'
  AND kind = 'mailbox.resume'
  AND state = 'done'
  AND payload_ref = ?`, queueItemID, "mailbox:"+mailboxID).Scan(&count)
	return err == nil && count > 0
}

func (s *Service) sessionResumedForMailbox(ctx context.Context, routeID, mailboxID string) bool {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'session.resumed'
  AND aggregate_type = 'session_route'
  AND aggregate_id = ?
ORDER BY occurred_at DESC, id DESC`, routeID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		if jsonArrayContainsString(payload["mailbox_ids"], mailboxID) {
			return true
		}
	}
	return false
}

func (s *Service) resumeCLISessionForRoute(ctx context.Context, routeID string) bool {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM cli_sessions cs
JOIN session_routes sr ON sr.id = ?
WHERE cs.runtime_id = sr.runtime_id
  AND cs.agent_id = sr.agent_id
  AND cs.cli_backend = sr.cli_backend
  AND cs.session_native_id = sr.session_native_id
  AND cs.start_reason = 'resume'
  AND cs.resume_of <> ''
  AND cs.transcript_ref <> ''
  AND cs.state IN ('resumed', 'exited', 'finished')`, routeID).Scan(&count)
	return err == nil && count > 0
}

func (s *Service) predicateGitChangeset(ctx context.Context, ev evidenceContext) PredicateResult {
	rows, err := s.queryLineage(ctx, ev, `
SELECT id, workspace_id, repo_id, evidence_refs_json
FROM changesets
WHERE contract_id IN (`+placeholders(len(ev.ContractIDs))+`)
  AND state = 'submitted'`, ev.ContractIDs...)
	if err != nil {
		return failed("git_changeset_evidence_chain", "changeset lookup failed", err.Error())
	}
	type changesetRow struct {
		id               string
		workspaceID      string
		repoID           string
		evidenceRefsJSON string
	}
	var changesets []changesetRow
	for rows.Next() {
		var changeset changesetRow
		if err := rows.Scan(&changeset.id, &changeset.workspaceID, &changeset.repoID, &changeset.evidenceRefsJSON); err != nil {
			_ = rows.Close()
			return failed("git_changeset_evidence_chain", "changeset scan failed", err.Error())
		}
		changesets = append(changesets, changeset)
	}
	if err := rows.Close(); err != nil {
		return failed("git_changeset_evidence_chain", "changeset iteration failed", err.Error())
	}
	if err := rows.Err(); err != nil {
		return failed("git_changeset_evidence_chain", "changeset iteration failed", err.Error())
	}
	var refs []string
	var hasEvidenceRefs bool
	var workspaceIDs []string
	var repoIDs []string
	for _, changeset := range changesets {
		refs = append(refs, "changeset:"+changeset.id)
		workspaceIDs = append(workspaceIDs, changeset.workspaceID)
		repoIDs = append(repoIDs, changeset.repoID)
		if s.evidenceRefsExistInLineage(ctx, ev, jsonStringArray(changeset.evidenceRefsJSON)) {
			hasEvidenceRefs = true
		}
	}
	var missing []string
	if len(refs) == 0 {
		missing = append(missing, "changeset")
	}
	if !hasEvidenceRefs {
		missing = append(missing, "changeset_evidence_refs")
	}
	if count, err := s.countGitOperationsForChangesets(ctx, workspaceIDs, repoIDs); err != nil || count == 0 {
		missing = append(missing, "git_operation")
	}
	if !s.validationHasScopedCheckedRefs(ctx, ev, "changeset", "evidence") {
		missing = append(missing, "validation_checked_refs")
	}
	if len(missing) > 0 {
		return failed("git_changeset_evidence_chain", "Git changeset evidence chain is incomplete", append(missing, refs...)...)
	}
	return passed("git_changeset_evidence_chain", "Git changeset evidence chain is present", refs...)
}

func (s *Service) evidenceContext(ctx context.Context, in EvaluateInput) (evidenceContext, error) {
	out := evidenceContext{
		RootContractID: in.RootContractID,
		TeamID:         in.TeamID,
		TeamVersion:    in.TeamVersion,
		ContractSet:    map[string]bool{},
	}
	var status, teamID string
	var teamVersion int
	err := s.db.QueryRowContext(ctx, `
SELECT c.status, COALESCE(ts.team_id, ''), COALESCE(ts.team_version, 0)
FROM work_contracts c
LEFT JOIN contract_team_scopes ts ON ts.contract_id = c.id
WHERE c.id = ?`, in.RootContractID).Scan(&status, &teamID, &teamVersion)
	if err == nil {
		out.RootExists = true
		out.RootStatus = status
		out.RootTeamID = teamID
		out.RootTeamVersion = teamVersion
		out.ContractIDs = append(out.ContractIDs, in.RootContractID)
		out.ContractSet[in.RootContractID] = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if out.RootExists {
		frontier := []string{in.RootContractID}
		for len(frontier) > 0 {
			next := []string{}
			rows, err := s.db.QueryContext(ctx, `SELECT id FROM work_contracts WHERE issuer_contract_id IN (`+placeholders(len(frontier))+`)`, anySlice(frontier)...)
			if err != nil {
				return out, err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return out, err
				}
				if !out.ContractSet[id] {
					out.ContractSet[id] = true
					out.ContractIDs = append(out.ContractIDs, id)
					next = append(next, id)
				}
			}
			if err := rows.Close(); err != nil {
				return out, err
			}
			frontier = next
		}
	}
	sort.Strings(out.ContractIDs)
	return out, nil
}

func (s *Service) activeTeam(ctx context.Context) (string, int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT team_id, version FROM team_config_versions WHERE active = 1 ORDER BY created_at DESC, version DESC LIMIT 1`)
	var teamID string
	var version int
	if err := row.Scan(&teamID, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, errors.New("release_acceptance: no active TeamConfig is loaded")
		}
		return "", 0, err
	}
	return teamID, version, nil
}

func (s *Service) record(ctx context.Context, in EvaluateInput, status string, predicates []PredicateResult, evidenceRefs []string, inspectSummary InspectSummary, failureSummary string) (Acceptance, error) {
	id, err := ids.New("rel")
	if err != nil {
		return Acceptance{}, err
	}
	predicateJSON, err := json.Marshal(predicates)
	if err != nil {
		return Acceptance{}, err
	}
	evidenceJSON, err := json.Marshal(evidenceRefs)
	if err != nil {
		return Acceptance{}, err
	}
	inspectJSON, err := json.Marshal(inspectSummary)
	if err != nil {
		return Acceptance{}, err
	}
	eventCursor := map[string]any{"evaluated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	eventCursorJSON, err := json.Marshal(eventCursor)
	if err != nil {
		return Acceptance{}, err
	}
	now := formatTime(time.Now())
	err = s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO release_acceptances (
  id, tenant_id, root_contract_id, team_id, team_version, status, run_label,
  predicate_results_json, evidence_refs_json, inspect_summary_json,
  event_cursor_json, failure_summary, created_by, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, in.RootContractID, in.TeamID, in.TeamVersion, status, in.RunLabel,
			string(predicateJSON), string(evidenceJSON), string(inspectJSON),
			string(eventCursorJSON), failureSummary, in.CreatedBy, now,
		); err != nil {
			return fmt.Errorf("insert release acceptance: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "release_acceptance.evaluation_requested", id, in, map[string]any{
			"status":          status,
			"predicate_count": len(predicates),
		}); err != nil {
			return err
		}
		for _, predicate := range predicates {
			eventType := "release_acceptance.predicate_failed"
			if predicate.Status == "passed" {
				eventType = "release_acceptance.predicate_passed"
			}
			if _, err := appendEvent(ctx, tx, eventType, id, in, map[string]any{
				"predicate": predicate.Name,
				"status":    predicate.Status,
			}); err != nil {
				return err
			}
		}
		finalEvent := "release_acceptance.failed"
		if status == "passed" {
			finalEvent = "release_acceptance.passed"
		} else if status == "blocked" {
			finalEvent = "release_acceptance.blocked"
		}
		if _, err := appendEvent(ctx, tx, finalEvent, id, in, map[string]any{
			"root_contract_id": in.RootContractID,
			"team_id":          in.TeamID,
			"team_version":     in.TeamVersion,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Acceptance{}, err
	}
	return s.acceptanceByID(ctx, id)
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType, acceptanceID string, in EvaluateInput, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return store.AppendEventTx(ctx, tx, events.Event{
		TenantID:       "default",
		SubjectKind:    "operator",
		SubjectID:      in.CreatedBy,
		CapabilityName: "release_acceptance.evaluate",
		Type:           eventType,
		AggregateType:  "release_acceptance",
		AggregateID:    acceptanceID,
		PayloadJSON:    raw,
	})
}

func (s *Service) acceptanceByRun(ctx context.Context, in EvaluateInput) (Acceptance, bool, error) {
	row := s.db.QueryRowContext(ctx, acceptanceSelectSQL()+`
WHERE root_contract_id = ? AND team_id = ? AND team_version = ? AND run_label = ?
LIMIT 1`, in.RootContractID, in.TeamID, in.TeamVersion, in.RunLabel)
	out, err := scanAcceptance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Acceptance{}, false, nil
	}
	if err != nil {
		return Acceptance{}, false, err
	}
	return out, true, nil
}

func (s *Service) acceptanceByID(ctx context.Context, id string) (Acceptance, error) {
	return scanAcceptance(s.db.QueryRowContext(ctx, acceptanceSelectSQL()+`WHERE id = ?`, id))
}

func List(ctx context.Context, db *sql.DB) ([]Acceptance, error) {
	rows, err := db.QueryContext(ctx, acceptanceSelectSQL()+`ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list release acceptances: %w", err)
	}
	defer rows.Close()
	var out []Acceptance
	for rows.Next() {
		acceptance, err := scanAcceptance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acceptance)
	}
	return out, rows.Err()
}

func LatestSummary(ctx context.Context, db *sql.DB) (Summary, error) {
	rows, err := db.QueryContext(ctx, acceptanceSelectSQL()+`ORDER BY created_at DESC, id DESC LIMIT 1`)
	if err != nil {
		return Summary{}, fmt.Errorf("latest release acceptance: %w", err)
	}
	summary := Summary{Counts: map[string]int{"passed": 0, "failed": 0, "blocked": 0}}
	if rows.Next() {
		acceptance, err := scanAcceptance(rows)
		if err != nil {
			return Summary{}, err
		}
		summary.Latest = &acceptance
	}
	if err := rows.Close(); err != nil {
		return Summary{}, err
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}
	countRows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM release_acceptances GROUP BY status`)
	if err != nil {
		return Summary{}, err
	}
	defer countRows.Close()
	for countRows.Next() {
		var status string
		var count int
		if err := countRows.Scan(&status, &count); err != nil {
			return Summary{}, err
		}
		summary.Counts[status] = count
	}
	return summary, countRows.Err()
}

func acceptanceSelectSQL() string {
	return `
SELECT id, root_contract_id, team_id, team_version, status, run_label,
  predicate_results_json, evidence_refs_json, inspect_summary_json,
  event_cursor_json, failure_summary, created_by, created_at
FROM release_acceptances
`
}

type rowScanner interface {
	Scan(...any) error
}

func scanAcceptance(row rowScanner) (Acceptance, error) {
	var out Acceptance
	var predicateJSON, evidenceJSON, inspectJSON, eventCursorJSON, createdAt string
	if err := row.Scan(
		&out.ID,
		&out.RootContractID,
		&out.TeamID,
		&out.TeamVersion,
		&out.Status,
		&out.RunLabel,
		&predicateJSON,
		&evidenceJSON,
		&inspectJSON,
		&eventCursorJSON,
		&out.FailureSummary,
		&out.CreatedBy,
		&createdAt,
	); err != nil {
		return Acceptance{}, err
	}
	if err := json.Unmarshal([]byte(predicateJSON), &out.PredicateResults); err != nil {
		return Acceptance{}, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &out.EvidenceRefs); err != nil {
		return Acceptance{}, err
	}
	if err := json.Unmarshal([]byte(inspectJSON), &out.InspectSummary); err != nil {
		return Acceptance{}, err
	}
	if eventCursorJSON != "" {
		_ = json.Unmarshal([]byte(eventCursorJSON), &out.EventCursor)
	}
	parsed, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return Acceptance{}, err
	}
	out.CreatedAt = parsed
	return out, nil
}

func (s *Service) migrations(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		out[version] = true
	}
	return out, rows.Err()
}

func (s *Service) queryLineage(ctx context.Context, ev evidenceContext, query string, contractIDs ...string) (*sql.Rows, error) {
	if len(contractIDs) == 0 {
		return nil, errors.New("empty contract lineage")
	}
	return s.db.QueryContext(ctx, query, anySlice(contractIDs)...)
}

func (s *Service) acceptedCapabilityCallForLease(ctx context.Context, capabilityName, agentID, leaseID string) bool {
	if leaseID == "" {
		return false
	}
	query := `
SELECT scope_json
FROM capability_calls
WHERE capability_name = ? AND status = 'accepted'`
	args := []any{capabilityName}
	if agentID != "" {
		query += ` AND subject_id = ?`
		args = append(args, agentID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false
		}
		if scopeLeaseID(raw) == leaseID {
			return true
		}
	}
	return false
}

func scopeLeaseID(raw string) string {
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return ""
	}
	value, _ := values["lease_id"].(string)
	return strings.TrimSpace(value)
}

func (s *Service) rootCompletionLease(ctx context.Context, rootContractID string) string {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'contract.satisfied'
  AND aggregate_type = 'work_contract'
  AND aggregate_id = ?
ORDER BY occurred_at DESC`, rootContractID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return ""
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		leaseID, _ := payload["lease_id"].(string)
		if strings.TrimSpace(leaseID) != "" {
			return strings.TrimSpace(leaseID)
		}
	}
	return ""
}

func (s *Service) leaseAgent(ctx context.Context, leaseID string) string {
	var agentID string
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM leases WHERE id = ?`, leaseID).Scan(&agentID)
	if err != nil {
		return ""
	}
	return agentID
}

func (s *Service) evidenceExistsInLineage(ctx context.Context, ev evidenceContext, evidenceID, kind, verdict string) bool {
	evidenceID = normalizeRefID("evidence", evidenceID)
	if evidenceID == "" {
		return false
	}
	query := `SELECT contract_id FROM evidence WHERE id = ?`
	args := []any{evidenceID}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	if verdict != "" {
		query += ` AND verdict = ?`
		args = append(args, verdict)
	}
	var contractID string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&contractID)
	return err == nil && ev.ContractSet[contractID]
}

func (s *Service) evidenceRefsExistInLineage(ctx context.Context, ev evidenceContext, refs []string) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if !s.evidenceExistsInLineage(ctx, ev, ref, "", "") {
			return false
		}
	}
	return true
}

func (s *Service) eventExistsForAny(ctx context.Context, eventType, aggregateType string, aggregateIDs []string) bool {
	aggregateIDs = compactStrings(aggregateIDs)
	if len(aggregateIDs) == 0 {
		return false
	}
	query := `
SELECT COUNT(*)
FROM events
WHERE event_type = ?
  AND aggregate_type = ?
  AND aggregate_id IN (` + placeholders(len(aggregateIDs)) + `)`
	args := []any{eventType, aggregateType}
	args = append(args, anySlice(aggregateIDs)...)
	count, err := s.countRows(ctx, query, args...)
	return err == nil && count > 0
}

func (s *Service) countDeliveryAttemptsForMailboxes(ctx context.Context, mailboxIDs []string) (int, error) {
	mailboxIDs = compactStrings(mailboxIDs)
	if len(mailboxIDs) == 0 {
		return 0, nil
	}
	query := `
SELECT COUNT(*)
FROM delivery_attempts
WHERE state IN ('accepted', 'failed', 'fallback')
  AND mailbox_item_id IN (` + placeholders(len(mailboxIDs)) + `)`
	return s.countRows(ctx, query, anySlice(mailboxIDs)...)
}

func (s *Service) countResumeQueueForMailboxes(ctx context.Context, mailboxIDs []string) (int, error) {
	mailboxIDs = compactStrings(mailboxIDs)
	if len(mailboxIDs) == 0 {
		return 0, nil
	}
	refs := make([]string, len(mailboxIDs))
	for i, id := range mailboxIDs {
		refs[i] = "mailbox:" + id
	}
	query := `
SELECT COUNT(*)
FROM queue_items
WHERE queue_name = 'runtime.resume'
  AND kind = 'mailbox.resume'
  AND state = 'done'
  AND payload_ref IN (` + placeholders(len(refs)) + `)`
	return s.countRows(ctx, query, anySlice(refs)...)
}

func (s *Service) countGitOperationsForChangesets(ctx context.Context, workspaceIDs, repoIDs []string) (int, error) {
	workspaceIDs = compactStrings(workspaceIDs)
	repoIDs = compactStrings(repoIDs)
	if len(workspaceIDs) == 0 || len(repoIDs) == 0 {
		return 0, nil
	}
	query := `
SELECT COUNT(*)
FROM git_operations
WHERE state = 'succeeded'
  AND workspace_id IN (` + placeholders(len(workspaceIDs)) + `)
  AND repo_id IN (` + placeholders(len(repoIDs)) + `)`
	args := anySlice(workspaceIDs)
	args = append(args, anySlice(repoIDs)...)
	return s.countRows(ctx, query, args...)
}

func (s *Service) validationEvidenceInLineage(ctx context.Context, ev evidenceContext) bool {
	if len(ev.ContractIDs) == 0 {
		return false
	}
	query := `
SELECT id
FROM evidence
WHERE kind = 'validation_assessment'
  AND verdict = 'pass'
  AND contract_id IN (` + placeholders(len(ev.ContractIDs)) + `)
LIMIT 1`
	var id string
	err := s.db.QueryRowContext(ctx, query, anySlice(ev.ContractIDs)...).Scan(&id)
	return err == nil
}

func (s *Service) validationHasScopedCheckedRefs(ctx context.Context, ev evidenceContext, required ...string) bool {
	if len(ev.ContractIDs) == 0 {
		return false
	}
	query := `
SELECT checked_refs_json
FROM validation_assessments
WHERE verdict = 'pass'
  AND (contract_id IN (` + placeholders(len(ev.ContractIDs)) + `)
    OR assessed_contract_id IN (` + placeholders(len(ev.ContractIDs)) + `))`
	rows, err := s.db.QueryContext(ctx, query, anySlice(append(ev.ContractIDs, ev.ContractIDs...))...)
	if err != nil {
		return false
	}
	var snapshots []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return false
		}
		snapshots = append(snapshots, raw)
	}
	if err := rows.Close(); err != nil {
		return false
	}
	if err := rows.Err(); err != nil {
		return false
	}
	for _, raw := range snapshots {
		if len(s.missingScopedCheckedRefs(ctx, ev, raw, required...)) == 0 {
			return true
		}
	}
	return false
}

func (s *Service) missingScopedCheckedRefs(ctx context.Context, ev evidenceContext, raw string, required ...string) []string {
	refs := checkedRefsByKind(raw)
	var missing []string
	for _, kind := range required {
		ids := refs[kind]
		if len(ids) == 0 {
			missing = append(missing, "missing_checked_ref_"+kind)
			continue
		}
		foundScoped := false
		for _, id := range ids {
			if s.checkedRefExistsInLineage(ctx, ev, kind, id) {
				foundScoped = true
				break
			}
		}
		if !foundScoped {
			missing = append(missing, "missing_checked_ref_"+kind)
		}
	}
	return missing
}

func (s *Service) checkedRefExistsInLineage(ctx context.Context, ev evidenceContext, kind, id string) bool {
	id = normalizeRefID(kind, id)
	if id == "" {
		return false
	}
	switch kind {
	case "command_run":
		var contractID string
		err := s.db.QueryRowContext(ctx, `SELECT contract_id FROM command_runs WHERE id = ?`, id).Scan(&contractID)
		return err == nil && ev.ContractSet[contractID]
	case "changeset":
		var contractID string
		err := s.db.QueryRowContext(ctx, `SELECT COALESCE(contract_id, '') FROM changesets WHERE id = ?`, id).Scan(&contractID)
		return err == nil && ev.ContractSet[contractID]
	case "evidence":
		return s.evidenceExistsInLineage(ctx, ev, id, "", "")
	default:
		return false
	}
}

func (s *Service) countRows(ctx context.Context, query string, args ...any) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func aggregateStatus(predicates []PredicateResult) string {
	var blocked bool
	for _, predicate := range predicates {
		if !predicate.Required {
			continue
		}
		switch predicate.Status {
		case "failed":
			return "failed"
		case "blocked":
			blocked = true
		}
	}
	if blocked {
		return "blocked"
	}
	return "passed"
}

func summarize(rootContractID, teamID string, teamVersion int, status string, predicates []PredicateResult, refs []string) InspectSummary {
	summary := InspectSummary{
		RootContractID: rootContractID,
		TeamID:         teamID,
		TeamVersion:    teamVersion,
		Status:         status,
		PredicateCount: len(predicates),
		CanonicalRefs:  map[string]int{},
	}
	for _, predicate := range predicates {
		switch predicate.Status {
		case "passed":
			summary.PassedCount++
		case "blocked":
			summary.BlockedCount++
			summary.Failed = append(summary.Failed, predicate.Name)
		default:
			summary.FailedCount++
			summary.Failed = append(summary.Failed, predicate.Name)
		}
	}
	for _, ref := range refs {
		prefix := ref
		if idx := strings.Index(ref, ":"); idx > 0 {
			prefix = ref[:idx]
		}
		summary.CanonicalRefs[prefix]++
	}
	sort.Strings(summary.Failed)
	return summary
}

func failureSummary(predicates []PredicateResult) string {
	var failed []string
	for _, predicate := range predicates {
		if predicate.Required && predicate.Status != "passed" {
			failed = append(failed, predicate.Name)
		}
	}
	sort.Strings(failed)
	return strings.Join(failed, ",")
}

func collectRefs(predicates []PredicateResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, predicate := range predicates {
		for _, ref := range predicate.CanonicalRefs {
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

func passed(name, message string, refs ...string) PredicateResult {
	return PredicateResult{Name: name, Status: "passed", Required: true, Message: message, CanonicalRefs: cleanRefs(refs)}
}

func failed(name, message string, refs ...string) PredicateResult {
	return PredicateResult{Name: name, Status: "failed", Required: true, Message: message, CanonicalRefs: cleanRefs(refs)}
}

func cleanRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func checkedRefKinds(raw string) map[string]bool {
	byKind := checkedRefsByKind(raw)
	out := map[string]bool{}
	for kind, ids := range byKind {
		out[kind] = len(ids) > 0
	}
	return out
}

func checkedRefsByKind(raw string) map[string][]string {
	var refs []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &refs)
	out := map[string][]string{}
	for _, ref := range refs {
		kind := strings.TrimSpace(ref.Kind)
		id := strings.TrimSpace(ref.ID)
		if kind == "" || id == "" {
			continue
		}
		out[kind] = append(out[kind], id)
	}
	return out
}

func normalizeRefID(kind, ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{kind + ":", strings.ReplaceAll(kind, "_", "-") + ":"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(ref, prefix))
		}
	}
	return ref
}

func jsonStringArray(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	values := make([]string, n)
	for i := range values {
		values[i] = "?"
	}
	return strings.Join(values, ",")
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func prefixed(prefix string, values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = prefix + ":" + value
	}
	return out
}

func stringFromMap(values map[string]any, key string) string {
	raw, ok := values[key]
	if !ok {
		return ""
	}
	text, _ := raw.(string)
	return text
}

func boolFromMap(values map[string]any, key string) (bool, bool) {
	raw, ok := values[key]
	if !ok {
		return false, false
	}
	value, ok := raw.(bool)
	return value, ok
}

func jsonArrayContainsString(raw any, needle string) bool {
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		text, ok := value.(string)
		if ok && text == needle {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
