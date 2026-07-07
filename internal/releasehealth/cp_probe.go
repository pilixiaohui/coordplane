package releasehealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/backend"
	"coordplane/internal/capability"
	"coordplane/internal/codemanagement"
	"coordplane/internal/coordination"
	"coordplane/internal/cpprobe"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/validation"
)

type CPProbe001Config struct {
	DBPath               string
	TeamID               string
	TeamVersion          int
	TeamConfig           string
	DockerTeamID         string
	DockerTeamVersion    int
	DockerTeamConfig     string
	ListenAddr           string
	BackendURL           string
	CoordlinkPath        string
	DockerNetwork        string
	ClaudeBinary         string
	ClaudeEnvKeys        []string
	WorkDir              string
	ArtifactDir          string
	RuntimeWorkspaceRoot string
	RuntimeHomeRoot      string
	EnvironmentBlocker   string
}

type CPProbe001Result struct {
	Scenario       string                   `json:"scenario"`
	Status         cpprobe.ConclusionStatus `json:"status"`
	Mode           string                   `json:"mode"`
	RootContractID string                   `json:"root_contract_id,omitempty"`
	DBPath         string                   `json:"db_path"`
	ArtifactDir    string                   `json:"artifact_dir"`
	Artifacts      map[string]string        `json:"artifacts"`
	DockerReplay   cpprobe.ConclusionStatus `json:"docker_replay_status"`
	CleanupPassed  bool                     `json:"cleanup_passed"`
	Blockers       []string                 `json:"blockers,omitempty"`
	LeakScan       DenylistScanResult       `json:"leak_scan"`
}

type cpProbeState struct {
	app             *backend.Backend
	cfg             CPProbe001Config
	link            *coordlink.Adapter
	trace           cpprobe.ManualTrace
	root            coordination.AddContractResult
	coordinator     coordination.AssignmentNextResult
	developerA      coordination.AssignmentNextResult
	developerB      coordination.AssignmentNextResult
	developerAWork  coordination.AddContractResult
	developerBWork  coordination.AddContractResult
	verifierWork    coordination.AddContractResult
	verifierSession cpruntime.AssignmentSession
	repository      codemanagement.Repository
}

type cpProbeDockerReplayOutcome struct {
	Status         cpprobe.ConclusionStatus
	RootContractID string
	Blocker        string
	CleanupPassed  bool
	DenylistPassed bool
	TraceSteps     []cpprobe.TraceStep
	Inspect        *cpprobe.RedactedInspect
	GitSummary     *cpprobe.GitOperationSummary
}

type cpProbeDockerState struct {
	app              *backend.Backend
	cfg              CPProbe001Config
	driver           *driver
	tinyLedger       cpprobe.TinyLedgerFixture
	root             coordination.AddContractResult
	coordinator      cpruntime.AssignmentSession
	developerA       cpruntime.AssignmentSession
	developerB       cpruntime.AssignmentSession
	verifier         cpruntime.AssignmentSession
	developerAWork   coordination.AddContractResult
	developerBWork   coordination.AddContractResult
	verifierWork     coordination.AddContractResult
	developerAReport coordination.Evidence
	developerBReport coordination.Evidence
	developerAData   dockerBridgeWorkspaceData
	developerBData   dockerBridgeWorkspaceData
	validation       validationData
	finalRef         string
	trace            []cpprobe.TraceStep
}

type dockerBridgeWorkspaceData struct {
	Session           cpruntime.AssignmentSession
	Work              coordination.AddContractResult
	Prepare           codemanagement.WorkspacePrepareResult
	Commands          []commandRunData
	Commit            codemanagement.GitCommitResult
	Submit            codemanagement.SubmitChangeSetResult
	OldPreview        codemanagement.MergePreviewResult
	StaleApply        typedResponse
	ConflictPreview   typedResponse
	Conflicts         codemanagement.ConflictListResult
	Abort             codemanagement.AbortMergeResult
	ResolveCommit     codemanagement.GitCommitResult
	RetryConflict     typedResponse
	Resolve           codemanagement.ResolveMergeResult
	Apply             codemanagement.MergeApplyResult
	Report            coordination.Evidence
	ResumeMailboxID   string
	ResumeQueueState  string
	ValidationCommand commandRunData
}

func RunCPProbe001(ctx context.Context, cfg CPProbe001Config) (CPProbe001Result, error) {
	cfg = cfg.withDefaults()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultWorkflowTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultWorkflowTimeout)
		defer cancel()
	}
	result := CPProbe001Result{
		Scenario:     cpprobe.ScenarioID,
		Status:       cpprobe.ConclusionEnvironmentBlocked,
		Mode:         "manual_closeout",
		DBPath:       cfg.DBPath,
		ArtifactDir:  cfg.ArtifactDir,
		Artifacts:    cpProbeArtifactMap(cfg.ArtifactDir),
		DockerReplay: cpprobe.ConclusionEnvironmentBlocked,
	}
	if strings.TrimSpace(cfg.EnvironmentBlocker) != "" {
		result.Blockers = []string{cfg.EnvironmentBlocker}
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return result, fmt.Errorf("prepare cp-probe release-health workdir: %w", err)
	}
	if err := os.MkdirAll(cfg.ArtifactDir, 0o755); err != nil {
		return result, fmt.Errorf("prepare cp-probe artifact dir: %w", err)
	}
	state, err := newCPProbeState(ctx, cfg)
	if err != nil {
		return result, err
	}
	defer func() {
		_ = state.app.Close()
	}()
	artifacts, rootID, err := state.run(ctx)
	result.RootContractID = rootID
	if err != nil {
		return result, err
	}
	manualApp := state.app
	state.app = nil
	if manualApp != nil {
		_ = manualApp.Close()
	}
	baseArtifacts := artifacts
	outcome := runCPProbeDockerReplay(ctx, cfg, state.repository.ID)
	result.DockerReplay = outcome.Status
	result.CleanupPassed = outcome.CleanupPassed
	if outcome.RootContractID != "" {
		result.RootContractID = outcome.RootContractID
	}
	preScanOutcome := outcome
	if preScanOutcome.Status == cpprobe.ConclusionPassed {
		preScanOutcome.Status = cpprobe.ConclusionFailed
		preScanOutcome.Blocker = "artifact denylist scan has not passed yet"
	}
	artifacts = applyCPProbeDockerReplayOutcome(baseArtifacts, preScanOutcome, cfg.EnvironmentBlocker)
	if err := cpprobe.WriteReportArtifacts(cfg.ArtifactDir, artifacts); err != nil {
		return result, err
	}
	scan, err := ScanFilesForDenylist(cpProbeArtifactPaths(cfg.ArtifactDir), cpProbeDenylist(cfg))
	result.LeakScan = scan
	if err != nil {
		return result, err
	}
	if !scan.Passed {
		result.Status = cpprobe.ConclusionFailed
		result.Mode = "manual_closeout+docker_replay"
		result.Blockers = []string{"artifact denylist scan failed: " + strings.Join(scan.Violations, ", ")}
		return result, fmt.Errorf("cp-probe release-health artifacts leaked forbidden markers: %s", strings.Join(scan.Violations, ", "))
	}
	outcome.DenylistPassed = true
	artifacts = applyCPProbeDockerReplayOutcome(baseArtifacts, outcome, cfg.EnvironmentBlocker)
	result.Status = cpProbeFinalStatus(outcome)
	switch result.Status {
	case cpprobe.ConclusionPassed:
		result.Mode = "manual_closeout+docker_replay"
		result.Blockers = nil
	case cpprobe.ConclusionFailed:
		result.Mode = "manual_closeout+docker_replay"
		result.Blockers = []string{outcome.Blocker}
	default:
		result.Mode = "manual_closeout+docker_replay"
		if strings.TrimSpace(outcome.Blocker) != "" {
			result.Blockers = []string{outcome.Blocker}
		}
	}
	if err := cpprobe.WriteReportArtifacts(cfg.ArtifactDir, artifacts); err != nil {
		return result, err
	}
	scan, err = ScanFilesForDenylist(cpProbeArtifactPaths(cfg.ArtifactDir), cpProbeDenylist(cfg))
	result.LeakScan = scan
	if err != nil {
		return result, err
	}
	if !scan.Passed {
		result.Status = cpprobe.ConclusionFailed
		result.Blockers = []string{"artifact denylist scan failed after final artifact write: " + strings.Join(scan.Violations, ", ")}
		return result, fmt.Errorf("cp-probe release-health artifacts leaked forbidden markers: %s", strings.Join(scan.Violations, ", "))
	}
	return result, cpProbeStatusError(result)
}

func (cfg CPProbe001Config) withDefaults() CPProbe001Config {
	if cfg.WorkDir == "" {
		cfg.WorkDir = DefaultWorkDir
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.WorkDir, "coordplane.db")
	}
	if cfg.TeamID == "" {
		cfg.TeamID = CPProbeDefaultTeamID
	}
	if cfg.TeamVersion == 0 {
		cfg.TeamVersion = CPProbeDefaultTeamVersion
	}
	if cfg.TeamConfig == "" {
		cfg.TeamConfig = CPProbeTeamConfigPath
	}
	if cfg.DockerTeamID == "" {
		cfg.DockerTeamID = CPProbeDockerTeamID
	}
	if cfg.DockerTeamVersion == 0 {
		cfg.DockerTeamVersion = CPProbeDockerTeamVersion
	}
	if cfg.DockerTeamConfig == "" {
		cfg.DockerTeamConfig = CPProbeDockerTeamConfigPath
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListen
	}
	if cfg.BackendURL == "" {
		cfg.BackendURL = DefaultPublicURL
	}
	if cfg.CoordlinkPath == "" {
		cfg.CoordlinkPath = filepath.Join(cfg.WorkDir, "bin", "coordlink")
	}
	if cfg.DockerNetwork == "" {
		cfg.DockerNetwork = DefaultNetwork
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "/usr/local/bin/claude"
	}
	if cfg.ArtifactDir == "" {
		cfg.ArtifactDir = cfg.WorkDir
	}
	if cfg.RuntimeWorkspaceRoot == "" {
		cfg.RuntimeWorkspaceRoot = filepath.Join(cfg.WorkDir, "runtime", "workspaces")
	}
	if cfg.RuntimeHomeRoot == "" {
		cfg.RuntimeHomeRoot = filepath.Join(cfg.WorkDir, "runtime", "home")
	}
	return cfg
}

func newCPProbeState(ctx context.Context, cfg CPProbe001Config) (*cpProbeState, error) {
	app, err := backend.Open(ctx, backend.Config{
		DBPath:               cfg.DBPath,
		ListenAddr:           cfg.ListenAddr,
		TeamConfigPath:       cfg.TeamConfig,
		TeamID:               cfg.TeamID,
		BackendURL:           cfg.BackendURL,
		RuntimeWorkspaceRoot: cfg.RuntimeWorkspaceRoot,
		RuntimeHomeRoot:      cfg.RuntimeHomeRoot,
	})
	if err != nil {
		return nil, err
	}
	return &cpProbeState{
		app:   app,
		cfg:   cfg,
		link:  coordlink.New(app.Dispatcher),
		trace: cpprobe.ManualTrace{Scenario: cpprobe.ScenarioID},
	}, nil
}

func (s *cpProbeState) run(ctx context.Context) (cpprobe.ReportArtifacts, string, error) {
	tinyLedger, err := cpprobe.GenerateTinyLedger(ctx, filepath.Join(s.cfg.WorkDir, "fixtures"))
	if err != nil {
		return cpprobe.ReportArtifacts{}, "", err
	}
	root, err := s.app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "release-health",
		Title:         "CP-PROBE-001 release health",
		Objective:     "Drive the formal CP-PROBE non-Docker manual closeout and write durable artifacts.",
		TargetAgentID: "coordinator",
		CompletionRequirements: []string{
			"report",
		},
	})
	if err != nil {
		return cpprobe.ReportArtifacts{}, "", err
	}
	s.root = root
	s.trace.Steps = append(s.trace.Steps, cpprobe.TraceStep{
		Actor:        "release-health",
		EntryPoint:   "go-service",
		Capability:   "contract.add",
		InputSummary: "create CP-PROBE release-health root contract",
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{"contract_id": root.ContractID, "assignment_id": root.AssignmentID},
	})
	if err := s.driveCoordinatorAndDevelopers(ctx, tinyLedger); err != nil {
		return cpprobe.ReportArtifacts{}, root.ContractID, err
	}
	if err := s.driveVerifierAndRootCloseout(ctx); err != nil {
		return cpprobe.ReportArtifacts{}, root.ContractID, err
	}
	inspect, err := s.app.Inspect(ctx)
	if err != nil {
		return cpprobe.ReportArtifacts{}, root.ContractID, err
	}
	gitSummary, err := s.gitSummary(ctx)
	if err != nil {
		return cpprobe.ReportArtifacts{}, root.ContractID, err
	}
	failureMatrix := cpprobe.FailureMatrix{
		Scenario: cpprobe.ScenarioID,
		Items: []cpprobe.FailureMatrixItem{
			{
				ID:             "formal-entrypoint",
				Status:         "covered",
				Capability:     "coordplane release-health cp-probe-001",
				StateAssertion: "formal command created durable DB state and wrote the five CP-PROBE artifact files outside Go test temp dirs",
			},
			{
				ID:             "manual-service-closeout",
				Status:         "covered",
				Capability:     "contract.add/workspace.prepare/report.submit/validation.assessment/contract.complete",
				StateAssertion: "non-Docker coordinator, developers, verifier, validation assessment, mailbox feedback, and root completion ran through service/capability boundaries",
			},
			{
				ID:             "docker-claude-equivalent-replay",
				Status:         "environment_blocked",
				Capability:     "runtime.docker/claude",
				StateAssertion: s.cfg.EnvironmentBlocker,
				NextStep:       "add the Docker/Claude replay driver for CP-PROBE and promote conclusion only when that replay passes",
			},
		},
	}
	conclusion := cpprobe.ConclusionReport{
		Scenario:         cpprobe.ScenarioID,
		Status:           cpprobe.ConclusionEnvironmentBlocked,
		ManualTraceRef:   cpprobe.ManualTraceArtifact,
		InspectRef:       cpprobe.InspectRedactedArtifact,
		GitSummaryRef:    cpprobe.GitOperationSummaryArtifact,
		FailureMatrixRef: cpprobe.FailureMatrixArtifact,
		Covered: []string{
			"formal release-health CLI/Make/script wiring",
			"durable release-health artifact files in configured workdir",
			"non-Docker manual backend, TeamConfig, contract, workspace, validation, mailbox, and root completion flow",
			"artifact denylist scan hook",
		},
		NotCovered: []string{
			"Docker/Claude equivalent replay",
			s.cfg.EnvironmentBlocker,
		},
		NextSteps: []string{
			"wire CP-PROBE Docker/Claude replay to the same formal entrypoint",
			"change conclusion to passed only after Docker/Claude replay and artifact scan both pass",
		},
	}
	return cpprobe.ReportArtifacts{
		ManualTrace: s.trace,
		Inspect: cpprobe.RedactedInspect{
			Scenario:       cpprobe.ScenarioID,
			Status:         inspect.Status,
			TeamID:         inspect.TeamID,
			RootContractID: root.ContractID,
			Counts:         inspect.Counts,
			Contracts:      contractSummaries(ctx, s.app.DB),
			Workspaces:     gitSummary.Workspaces,
			GitOperations:  gitSummary.Operations,
			Redacted:       true,
			DenylistChecks: []string{
				"database path marker",
				"runtime root marker",
				"authorization bearer marker",
				"secret key prefix marker",
				"Claude and Anthropic token key markers",
			},
		},
		GitSummary:    gitSummary,
		FailureMatrix: failureMatrix,
		Conclusion:    conclusion,
	}, root.ContractID, nil
}

func (s *cpProbeState) driveCoordinatorAndDevelopers(ctx context.Context, tinyLedger cpprobe.TinyLedgerFixture) error {
	coordinator, err := cpProbeCallData[coordination.AssignmentNextResult](ctx, s, "coordinator", "", "assignment.next", "claim CP-PROBE root", nil, nil)
	if err != nil {
		return err
	}
	s.coordinator = coordinator
	if _, err := cpProbeCallData[coordination.Contract](ctx, s, "coordinator", "", "contract.current", "read current root contract", map[string]string{"lease_id": coordinator.Lease.ID}, nil); err != nil {
		return err
	}
	developerAWork, err := cpProbeCallData[coordination.AddContractResult](ctx, s, "coordinator", "", "contract.add", "dispatch developer-a workspace evidence", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer A: CP-PROBE formal workspace evidence",
		"objective":       "Prepare Tiny Ledger workspace and submit report evidence through public capabilities.",
		"target_agent_id": "developer-a",
		"completion_requirements": []string{
			"report",
		},
	}, nil)
	if err != nil {
		return err
	}
	s.developerAWork = developerAWork
	developerBWork, err := cpProbeCallData[coordination.AddContractResult](ctx, s, "coordinator", "", "contract.add", "dispatch developer-b workspace evidence", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer B: CP-PROBE formal workspace evidence",
		"objective":       "Prepare an isolated Tiny Ledger workspace from the same canonical base.",
		"target_agent_id": "developer-b",
		"completion_requirements": []string{
			"report",
		},
	}, nil)
	if err != nil {
		return err
	}
	s.developerBWork = developerBWork
	developerA, err := cpProbeCallData[coordination.AssignmentNextResult](ctx, s, "developer-a", "", "assignment.next", "claim developer-a contract", nil, nil)
	if err != nil {
		return err
	}
	developerB, err := cpProbeCallData[coordination.AssignmentNextResult](ctx, s, "developer-b", "", "assignment.next", "claim developer-b contract", nil, nil)
	if err != nil {
		return err
	}
	s.developerA = developerA
	s.developerB = developerB
	preparedA, err := cpProbeCallData[codemanagement.WorkspacePrepareResult](ctx, s, "developer-a", "", "workspace.prepare", "prepare developer-a workspace", map[string]string{
		"repo_path":        tinyLedger.RepoPath,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(s.cfg.WorkDir, "git-workspaces", "developer-a"),
		"contract_id":      developerA.Contract.ID,
	}, nil)
	if err != nil {
		return err
	}
	s.repository = preparedA.Repository
	preparedB, err := cpProbeCallData[codemanagement.WorkspacePrepareResult](ctx, s, "developer-b", "", "workspace.prepare", "prepare developer-b workspace", map[string]string{
		"repo_path":        tinyLedger.RepoPath,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(s.cfg.WorkDir, "git-workspaces", "developer-b"),
		"contract_id":      developerB.Contract.ID,
	}, nil)
	if err != nil {
		return err
	}
	if preparedA.Workspace.BaseRef != tinyLedger.BaseRef || preparedB.Workspace.BaseRef != tinyLedger.BaseRef {
		return fmt.Errorf("CP-PROBE developer workspace base refs diverged from fixture base")
	}
	if _, err := cpProbeCallData[codemanagement.WorkspaceStatus](ctx, s, "developer-a", "", "workspace.status", "read developer-a workspace status", map[string]string{"workspace_id": preparedA.Workspace.ID}, nil); err != nil {
		return err
	}
	if _, err := cpProbeCallData[codemanagement.GitLogResult](ctx, s, "developer-b", "", "git.log", "read developer-b git log", map[string]any{"workspace_id": preparedB.Workspace.ID, "max_count": 3}, nil); err != nil {
		return err
	}
	reportA, err := cpProbeCallData[coordination.Evidence](ctx, s, "developer-a", "", "report.submit", "submit developer-a formal report", map[string]string{
		"lease_id": developerA.Lease.ID,
		"summary":  "developer-a prepared isolated Tiny Ledger workspace",
		"content":  "workspace_id=" + preparedA.Workspace.ID,
	}, nil)
	if err != nil {
		return err
	}
	if _, err := cpProbeCallData[coordination.CompleteContractResult](ctx, s, "developer-a", "", "contract.complete", "complete developer-a contract", map[string]any{
		"lease_id":     developerA.Lease.ID,
		"evidence_ids": []string{reportA.ID},
		"summary":      "developer-a formal workspace evidence complete",
	}, nil); err != nil {
		return err
	}
	reportB, err := cpProbeCallData[coordination.Evidence](ctx, s, "developer-b", "", "report.submit", "submit developer-b formal report", map[string]string{
		"lease_id": developerB.Lease.ID,
		"summary":  "developer-b prepared isolated Tiny Ledger workspace",
		"content":  "workspace_id=" + preparedB.Workspace.ID,
	}, nil)
	if err != nil {
		return err
	}
	if _, err := cpProbeCallData[coordination.CompleteContractResult](ctx, s, "developer-b", "", "contract.complete", "complete developer-b contract", map[string]any{
		"lease_id":     developerB.Lease.ID,
		"evidence_ids": []string{reportB.ID},
		"summary":      "developer-b formal workspace evidence complete",
	}, nil); err != nil {
		return err
	}
	return nil
}

func (s *cpProbeState) driveVerifierAndRootCloseout(ctx context.Context) error {
	verifierWork, err := cpProbeCallData[coordination.AddContractResult](ctx, s, "coordinator", "", "contract.add", "dispatch verifier assessment", map[string]any{
		"lease_id":        s.coordinator.Lease.ID,
		"title":           "Verifier: CP-PROBE formal assessment",
		"objective":       "Record non-Docker manual CP-PROBE validation assessment.",
		"target_agent_id": "verifier",
		"completion_requirements": []string{
			"validation_assessment",
		},
	}, nil)
	if err != nil {
		return err
	}
	s.verifierWork = verifierWork
	session, err := s.app.Runner.StartNext(ctx, "verifier")
	if err != nil {
		return err
	}
	s.verifierSession = session
	s.trace.Steps = append(s.trace.Steps, cpprobe.TraceStep{
		Actor:        "verifier",
		EntryPoint:   "runtime.runner",
		Capability:   "session.start",
		InputSummary: "start non-Docker verifier runtime session",
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"attempt_id":       session.AttemptID,
			"lease_id":         session.LeaseID,
			"session_route_id": session.Route.ID,
			"runtime_id":       session.Route.RuntimeID,
		},
	})
	report, err := cpProbeCallData[coordination.Evidence](ctx, s, "verifier", session.Route.RuntimeID, "report.submit", "submit verifier review report", map[string]string{
		"lease_id": session.LeaseID,
		"summary":  "CP-PROBE non-Docker manual closeout reviewed",
		"content":  "root_contract_id=" + s.root.ContractID,
	}, map[string]string{"lease_id": session.LeaseID})
	if err != nil {
		return err
	}
	assessment, err := cpProbeCallData[validation.Result](ctx, s, "verifier", session.Route.RuntimeID, "validation.assessment", "record verifier validation assessment", map[string]any{
		"lease_id":             session.LeaseID,
		"assessed_contract_id": s.root.ContractID,
		"verdict":              "pass",
		"reason":               "formal non-Docker manual CP-PROBE closeout produced durable service evidence",
		"summary":              "CP-PROBE non-Docker manual closeout passed; Docker/Claude replay remains blocked",
		"checked_refs": []map[string]string{
			{"kind": "evidence", "id": report.ID},
		},
	}, map[string]string{"lease_id": session.LeaseID})
	if err != nil {
		return err
	}
	if _, err := cpProbeCallData[coordination.CompleteContractResult](ctx, s, "verifier", session.Route.RuntimeID, "contract.complete", "complete verifier contract", map[string]any{
		"lease_id":     session.LeaseID,
		"evidence_ids": []string{assessment.EvidenceID},
		"summary":      "verifier validation assessment complete",
	}, map[string]string{"lease_id": session.LeaseID}); err != nil {
		return err
	}
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "completed",
		Summary:   "CP-PROBE verifier manual closeout completed",
	}); err != nil {
		return fmt.Errorf("finish verifier manual session: %w", err)
	}
	mailboxes, err := cpProbeCallData[[]coordination.MailboxItem](ctx, s, "coordinator", "", "mailbox.list", "coordinator reads child completion mailbox", nil, nil)
	if err != nil {
		return err
	}
	verifierMailbox := mailboxForContract(mailboxes, verifierWork.ContractID)
	if verifierMailbox.ID == "" {
		return fmt.Errorf("verifier child completion mailbox not found")
	}
	if _, err := cpProbeCallData[coordination.MailboxItem](ctx, s, "coordinator", "", "mailbox.get", "coordinator opens verifier mailbox", map[string]string{"mailbox_id": verifierMailbox.ID}, nil); err != nil {
		return err
	}
	if _, err := cpProbeCallData[coordination.MailboxItem](ctx, s, "coordinator", "", "mailbox.resolve", "coordinator resolves verifier mailbox", map[string]string{
		"mailbox_id":   verifierMailbox.ID,
		"followup_ref": "validation_assessment:" + assessment.AssessmentID,
	}, nil); err != nil {
		return err
	}
	rootReport, err := cpProbeCallData[coordination.Evidence](ctx, s, "coordinator", "", "report.submit", "submit root closeout report", map[string]string{
		"lease_id": s.coordinator.Lease.ID,
		"summary":  "CP-PROBE formal root closeout report",
		"content":  "validation_assessment=" + assessment.AssessmentID,
	}, nil)
	if err != nil {
		return err
	}
	if _, err := cpProbeCallData[coordination.CompleteContractResult](ctx, s, "coordinator", "", "contract.complete", "complete CP-PROBE root contract", map[string]any{
		"lease_id":     s.coordinator.Lease.ID,
		"evidence_ids": []string{rootReport.ID},
		"summary":      "CP-PROBE formal non-Docker root complete",
	}, nil); err != nil {
		return err
	}
	return nil
}

func runCPProbeDockerReplay(ctx context.Context, cfg CPProbe001Config, manualRepoID string) cpProbeDockerReplayOutcome {
	outcome := cpProbeDockerReplayOutcome{
		Status:        cpprobe.ConclusionEnvironmentBlocked,
		CleanupPassed: false,
	}
	if strings.TrimSpace(manualRepoID) != "" {
		outcome.TraceSteps = append(outcome.TraceSteps, cpprobe.TraceStep{
			Actor:        "release-health",
			EntryPoint:   "coordplane release-health cp-probe-001",
			Capability:   "docker_replay.preflight",
			InputSummary: "manual closeout repository available for CP-PROBE Docker replay planning",
			Status:       string(capability.StatusAccepted),
			CanonicalIDs: map[string]string{"manual_repo_id": manualRepoID},
		})
	}
	if blocker := strings.TrimSpace(cfg.EnvironmentBlocker); blocker != "" {
		outcome.Blocker = cpProbeRedact(blocker, cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpprobe.TraceStep{
			Actor:        "release-health",
			EntryPoint:   "coordplane release-health cp-probe-001",
			Capability:   "docker_replay.preflight",
			InputSummary: "Docker/Claude replay preflight reported an environment blocker",
			Status:       string(cpprobe.ConclusionEnvironmentBlocked),
			ErrorCode:    "ENVIRONMENT_BLOCKED",
			NextActions:  []string{"provide Docker daemon, coordlink binary, and Claude auth; rerun release-health-cp-probe-001"},
		})
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}

	tinyLedger, err := cpprobe.GenerateTinyLedger(ctx, filepath.Join(cfg.WorkDir, "docker-fixtures"))
	if err != nil {
		outcome.Blocker = cpProbeRedact("prepare Docker Tiny Ledger fixture: "+err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "fixture.prepare", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	if info, err := os.Stat(cfg.CoordlinkPath); err != nil {
		outcome.Blocker = cpProbeRedact("coordlink binary is required for Docker/Claude CP-PROBE replay: "+err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "coordlink.preflight", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	} else if info.IsDir() {
		outcome.Blocker = cpProbeRedact("coordlink path is a directory; Docker/Claude replay requires an executable coordlink file", cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "coordlink.preflight", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	if err := ensureDockerNetwork(ctx, cfg.DockerNetwork); err != nil {
		outcome.Blocker = cpProbeRedact(err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "runtime.docker.network", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}

	app, err := backend.Open(ctx, backend.Config{
		DBPath:           cfg.DBPath,
		ListenAddr:       cfg.ListenAddr,
		TeamConfigPath:   cfg.DockerTeamConfig,
		TeamID:           cfg.DockerTeamID,
		BackendURL:       cfg.BackendURL,
		DockerNetwork:    cfg.DockerNetwork,
		CoordlinkPath:    cfg.CoordlinkPath,
		ClaudeBinary:     cfg.ClaudeBinary,
		ClaudeEnvKeys:    cfg.ClaudeEnvKeys,
		ClaudeStartArgs:  cpProbeClaudeStartArgs(),
		ClaudeResumeArgs: cpProbeClaudeResumeArgs(),
		ClaudeTimeout:    3 * time.Minute,
	})
	if err != nil {
		outcome.Blocker = cpProbeRedact("open Docker replay backend: "+err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "backend.open", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	defer func() {
		_ = app.Close()
	}()
	root, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "release-health",
		Title:         "CP-PROBE-001 Docker Claude replay",
		Objective:     "Replay the CP-PROBE Tiny Ledger protocol through Docker agents and coordlink capabilities only.",
		TargetAgentID: "coordinator",
		CompletionRequirements: []string{
			"report",
		},
	})
	if err != nil {
		outcome.Blocker = cpProbeRedact("create Docker replay root contract: "+err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "contract.add", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	outcome.RootContractID = root.ContractID
	outcome.TraceSteps = append(outcome.TraceSteps, cpprobe.TraceStep{
		Actor:        "release-health",
		EntryPoint:   "go-service",
		Capability:   "contract.add",
		InputSummary: "create CP-PROBE Docker replay root contract",
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{"contract_id": root.ContractID, "assignment_id": root.AssignmentID},
	})

	server, err := startBackendServer(ctx, cfg.ListenAddr, app.Handler)
	if err != nil {
		outcome.Blocker = cpProbeRedact("start Docker replay backend server: "+err.Error(), cfg)
		outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "backend.listen", outcome.Blocker))
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	defer func() {
		_ = server.Shutdown(context.Background())
	}()

	state := &cpProbeDockerState{
		app:        app,
		cfg:        cfg,
		driver:     &driver{executor: cpruntime.DockerExecClient{}},
		tinyLedger: tinyLedger,
		root:       root,
		trace:      outcome.TraceSteps,
	}
	if err := state.run(ctx); err != nil {
		outcome.Status = cpProbeReplayStatus(err)
		outcome.Blocker = cpProbeRedact(err.Error(), cfg)
		outcome.TraceSteps = state.trace
		if len(outcome.TraceSteps) == 0 {
			outcome.TraceSteps = append(outcome.TraceSteps, cpProbeBlockedStep("release-health", "docker_replay.run", outcome.Blocker))
		}
		outcome.Inspect = cpProbeDockerInspect(ctx, app, root.ContractID, nil)
		if summary, summaryErr := cpProbeGitSummary(ctx, app.DB); summaryErr == nil {
			outcome.GitSummary = &summary
		}
		outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
		return outcome
	}
	outcome.TraceSteps = state.trace
	inspect := cpProbeDockerInspect(ctx, app, root.ContractID, nil)
	outcome.Inspect = inspect
	if summary, err := cpProbeGitSummary(ctx, app.DB); err == nil {
		outcome.GitSummary = &summary
	}
	outcome.CleanupPassed = cpProbeCleanupPassed(ctx, cfg, &outcome)
	if !outcome.CleanupPassed {
		outcome.Status = cpprobe.ConclusionEnvironmentBlocked
		if strings.TrimSpace(outcome.Blocker) == "" {
			outcome.Blocker = "managed Docker cleanup did not pass"
		}
		return outcome
	}
	outcome.Status = cpprobe.ConclusionPassed
	return outcome
}

func cpProbeClaudeStartArgs() []string {
	return []string{
		"--session-id", "{{session_id}}",
		"--print",
		"CP-PROBE release-health runtime smoke start. Do not call tools or coordlink. Print CP-PROBE runtime ready and exit.",
	}
}

func cpProbeClaudeResumeArgs() []string {
	return []string{
		"--resume", "{{session_id}}",
		"--print",
		"CP-PROBE release-health runtime smoke resume. Do not call tools or coordlink. Print CP-PROBE runtime resumed and exit.",
	}
}

func (s *cpProbeDockerState) run(ctx context.Context) error {
	coordinator, err := s.startAgent(ctx, "coordinator")
	if err != nil {
		return environmentBlockedError{Message: "coordinator Docker/Claude session could not start", Cause: err}
	}
	s.coordinator = coordinator
	if _, err := cpProbeDockerCallData[coordination.Contract](ctx, s, coordinator, "contract.current", map[string]any{}, "cp-probe-docker-coordinator-current", "read Docker replay root contract"); err != nil {
		return err
	}
	developerAWork, err := cpProbeDockerCallData[coordination.AddContractResult](ctx, s, coordinator, "contract.add", map[string]any{
		"title":                   "Developer A: Docker Tiny Ledger category summary",
		"objective":               "Add category summary to Tiny Ledger from the shared base and merge through controlled Git.",
		"target_agent_id":         "developer-a",
		"completion_requirements": []string{"report", "command_run"},
	}, "cp-probe-docker-add-developer-a", "dispatch Docker developer-a contract")
	if err != nil {
		return err
	}
	s.developerAWork = developerAWork
	developerBWork, err := cpProbeDockerCallData[coordination.AddContractResult](ctx, s, coordinator, "contract.add", map[string]any{
		"title":                   "Developer B: Docker Tiny Ledger transaction_count",
		"objective":               "Add transaction_count from the old base, hit stale/conflict feedback, then repair and merge in the same Docker session/resume chain.",
		"target_agent_id":         "developer-b",
		"completion_requirements": []string{"report", "command_run"},
	}, "cp-probe-docker-add-developer-b", "dispatch Docker developer-b contract")
	if err != nil {
		return err
	}
	s.developerBWork = developerBWork
	if _, err := cpProbeDockerCallData[coordination.Assignment](ctx, s, coordinator, "contract.wait", map[string]any{
		"reason":           "waiting for both Docker Tiny Ledger developer branches",
		"waiting_for_ref":  "contract:" + developerAWork.ContractID + ",contract:" + developerBWork.ContractID,
		"session_route_id": coordinator.Route.ID,
	}, "cp-probe-docker-wait-developers", "coordinator waits for developer A/B replay"); err != nil {
		return err
	}

	developerB, err := s.startDeveloperWorkspace(ctx, "developer-b", developerBWork)
	if err != nil {
		return err
	}
	s.developerB = developerB.Session
	if err := s.writeTinyLedgerFiles(ctx, &developerB, cpprobe.TransactionCountOnlyFiles(), "cp-probe-docker-write-b-txcount", "developer-b writes transaction_count variant inside Docker workspace"); err != nil {
		return err
	}
	bCommit, err := cpProbeDockerCallData[codemanagement.GitCommitResult](ctx, s, developerB.Session, "git.commit", map[string]any{
		"workspace_id":      developerB.Prepare.Workspace.ID,
		"message":           "Add transaction_count summary",
		"paths":             cpprobe.TinyLedgerPaths(),
		"expected_head_ref": developerB.Prepare.Workspace.HeadRef,
	}, "cp-probe-docker-commit-b-txcount", "developer-b commits transaction_count variant")
	if err != nil {
		return err
	}
	developerB.Commit = bCommit
	bSubmit, err := cpProbeDockerCallData[codemanagement.SubmitChangeSetResult](ctx, s, developerB.Session, "changeset.submit", map[string]any{
		"workspace_id":      developerB.Prepare.Workspace.ID,
		"contract_id":       developerBWork.ContractID,
		"summary":           "Docker developer-b transaction_count changeset",
		"evidence_refs":     commandEvidenceIDs(developerB.Commands),
		"expected_head_ref": bCommit.CommitSHA,
	}, "cp-probe-docker-submit-b-txcount", "developer-b submits old-base transaction_count changeset")
	if err != nil {
		return err
	}
	developerB.Submit = bSubmit
	bOldPreview, err := cpProbeDockerCallData[codemanagement.MergePreviewResult](ctx, s, developerB.Session, "git.merge_preview", map[string]any{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": s.tinyLedger.BaseRef,
	}, "cp-probe-docker-preview-b-old-base", "developer-b previews transaction_count against old base")
	if err != nil {
		return err
	}
	if bOldPreview.MergeAttempt.State != "clean" {
		return fmt.Errorf("developer-b old-base merge preview state is %s, want clean", bOldPreview.MergeAttempt.State)
	}
	developerB.OldPreview = bOldPreview

	developerA, err := s.startDeveloperWorkspace(ctx, "developer-a", developerAWork)
	if err != nil {
		return err
	}
	s.developerA = developerA.Session
	if err := s.writeTinyLedgerFiles(ctx, &developerA, cpprobe.CategoryOnlyFiles(), "cp-probe-docker-write-a-category", "developer-a writes category summary variant inside Docker workspace"); err != nil {
		return err
	}
	unauthorized, err := s.driver.callAny(ctx, developerA.Session, "git.status", map[string]any{
		"workspace_id": developerB.Prepare.Workspace.ID,
	}, "cp-probe-docker-deny-cross-workspace")
	if err != nil {
		return err
	}
	s.recordDockerStep("developer-a", "docker.coordlink", "git.status", "developer-a attempts to read developer-b workspace and is rejected", unauthorized)
	if err := requireDockerRejected(unauthorized, "WORKSPACE_NOT_FOUND", "cross-workspace git.status"); err != nil {
		return err
	}
	aCommit, err := cpProbeDockerCallData[codemanagement.GitCommitResult](ctx, s, developerA.Session, "git.commit", map[string]any{
		"workspace_id":      developerA.Prepare.Workspace.ID,
		"message":           "Add category summary",
		"paths":             cpprobe.TinyLedgerPaths(),
		"expected_head_ref": developerA.Prepare.Workspace.HeadRef,
	}, "cp-probe-docker-commit-a-category", "developer-a commits category summary variant")
	if err != nil {
		return err
	}
	developerA.Commit = aCommit
	aSubmit, err := cpProbeDockerCallData[codemanagement.SubmitChangeSetResult](ctx, s, developerA.Session, "changeset.submit", map[string]any{
		"workspace_id":      developerA.Prepare.Workspace.ID,
		"contract_id":       developerAWork.ContractID,
		"summary":           "Docker developer-a category summary changeset",
		"evidence_refs":     commandEvidenceIDs(developerA.Commands),
		"expected_head_ref": aCommit.CommitSHA,
	}, "cp-probe-docker-submit-a-category", "developer-a submits category summary changeset")
	if err != nil {
		return err
	}
	developerA.Submit = aSubmit
	aPreview, err := cpProbeDockerCallData[codemanagement.MergePreviewResult](ctx, s, developerA.Session, "git.merge_preview", map[string]any{
		"changeset_id":        aSubmit.ChangeSet.ID,
		"expected_target_ref": s.tinyLedger.BaseRef,
	}, "cp-probe-docker-preview-a-category", "developer-a previews category summary changeset")
	if err != nil {
		return err
	}
	if aPreview.MergeAttempt.State != "clean" {
		return fmt.Errorf("developer-a merge preview state is %s, want clean", aPreview.MergeAttempt.State)
	}
	aApply, err := cpProbeDockerCallData[codemanagement.MergeApplyResult](ctx, s, developerA.Session, "git.merge_apply", map[string]any{
		"merge_attempt_id":    aPreview.MergeAttempt.ID,
		"expected_target_ref": s.tinyLedger.BaseRef,
	}, "cp-probe-docker-apply-a-category", "developer-a applies category summary changeset")
	if err != nil {
		return err
	}
	if aApply.AppliedRef == "" || aApply.AppliedRef == s.tinyLedger.BaseRef {
		return fmt.Errorf("developer-a apply ref did not advance from base")
	}
	developerA.Apply = aApply
	aReport, err := cpProbeDockerCallData[coordination.Evidence](ctx, s, developerA.Session, "report.submit", map[string]any{
		"summary": "developer-a category summary merged",
		"content": "changeset=" + aSubmit.ChangeSet.ID + "; applied_ref=" + aApply.AppliedRef,
	}, "cp-probe-docker-report-a", "developer-a submits category merge report")
	if err != nil {
		return err
	}
	developerA.Report = aReport
	s.developerAReport = aReport
	if _, err := cpProbeDockerCallData[coordination.CompleteContractResult](ctx, s, developerA.Session, "contract.complete", map[string]any{
		"evidence_ids": append(commandEvidenceIDs(developerA.Commands), aReport.ID),
		"summary":      "developer-a category summary merged through Docker controlled Git",
	}, "cp-probe-docker-complete-a", "developer-a completes category summary contract"); err != nil {
		return err
	}
	if err := s.finishSession(ctx, developerA.Session, "completed", "developer-a category summary merged"); err != nil {
		return err
	}

	staleApply, err := s.driver.callAny(ctx, developerB.Session, "git.merge_apply", map[string]any{
		"merge_attempt_id": bOldPreview.MergeAttempt.ID,
	}, "cp-probe-docker-stale-apply-b")
	if err != nil {
		return err
	}
	s.recordDockerStep("developer-b", "docker.coordlink", "git.merge_apply", "developer-b old-base merge apply is rejected as stale", staleApply)
	if err := requireDockerRejected(staleApply, "STALE_TARGET_REF", "developer-b stale merge apply"); err != nil {
		return err
	}
	developerB.StaleApply = staleApply
	conflictPreview, err := s.driver.callAny(ctx, developerB.Session, "git.merge_preview", map[string]any{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": aApply.AppliedRef,
	}, "cp-probe-docker-conflict-preview-b")
	if err != nil {
		return err
	}
	s.recordDockerStep("developer-b", "docker.coordlink", "git.merge_preview", "developer-b transaction_count preview conflicts after category merge", conflictPreview)
	if err := requireDockerRejected(conflictPreview, "MERGE_CONFLICTS_FOUND", "developer-b conflict preview"); err != nil {
		return err
	}
	developerB.ConflictPreview = conflictPreview
	conflicts, err := cpProbeDockerCallData[codemanagement.ConflictListResult](ctx, s, developerB.Session, "git.conflicts", map[string]any{
		"merge_attempt_id": conflictPreview.CanonicalIDs["merge_attempt_id"],
	}, "cp-probe-docker-conflicts-b", "developer-b inspects merge conflict set")
	if err != nil {
		return err
	}
	if conflicts.ConflictSet.State != "open" || !containsString(conflicts.ConflictSet.Files, "ledger/ledger.go") {
		return fmt.Errorf("developer-b conflict set = %+v, want open ledger/ledger.go conflict", conflicts.ConflictSet)
	}
	developerB.Conflicts = conflicts
	aborted, err := cpProbeDockerCallData[codemanagement.AbortMergeResult](ctx, s, developerB.Session, "git.abort", map[string]any{
		"merge_attempt_id": conflictPreview.CanonicalIDs["merge_attempt_id"],
		"reason":           "exercise Docker conflict abort before same-session repair",
	}, "cp-probe-docker-abort-conflict-b", "developer-b aborts first conflict attempt")
	if err != nil {
		return err
	}
	if aborted.MergeAttempt.State != "aborted" || aborted.ConflictSet == nil || aborted.ConflictSet.State != "abandoned" {
		return fmt.Errorf("developer-b abort result = %+v, want aborted attempt and abandoned conflict set", aborted)
	}
	developerB.Abort = aborted
	if err := s.writeTinyLedgerFiles(ctx, &developerB, cpprobe.ResolvedCategoryAndCountFiles(), "cp-probe-docker-write-b-resolved", "developer-b writes resolved category plus transaction_count variant inside Docker workspace"); err != nil {
		return err
	}
	bResolveCommit, err := cpProbeDockerCallData[codemanagement.GitCommitResult](ctx, s, developerB.Session, "git.commit", map[string]any{
		"workspace_id":      developerB.Prepare.Workspace.ID,
		"message":           "Resolve category and transaction_count",
		"paths":             cpprobe.TinyLedgerPaths(),
		"expected_head_ref": bCommit.CommitSHA,
	}, "cp-probe-docker-commit-b-resolved", "developer-b commits resolved category plus transaction_count variant")
	if err != nil {
		return err
	}
	developerB.ResolveCommit = bResolveCommit
	if _, err := cpProbeDockerCallData[coordination.Assignment](ctx, s, developerB.Session, "contract.wait", map[string]any{
		"reason":           "waiting for coordinator resume signal before conflict resolve/apply",
		"waiting_for_ref":  "merge_attempt:" + conflictPreview.CanonicalIDs["merge_attempt_id"],
		"session_route_id": developerB.Session.Route.ID,
	}, "cp-probe-docker-b-wait-resume", "developer-b waits before same-session conflict repair resume"); err != nil {
		return err
	}
	if err := s.finishSession(ctx, developerB.Session, "waiting", "developer-b waiting for conflict repair resume"); err != nil {
		return err
	}
	resumeMessage, err := cpProbeDockerCallData[coordination.SendMessageResult](ctx, s, coordinator, "message.send", map[string]any{
		"recipient_agent_id": "developer-b",
		"intent":             "status",
		"body":               "resume conflict repair after developer-a category merge",
	}, "cp-probe-docker-signal-b-resume", "coordinator signals developer-b conflict repair resume")
	if err != nil {
		return err
	}
	if resumeMessage.MailboxID == "" {
		return fmt.Errorf("developer-b resume message did not create a mailbox item")
	}
	delivered, err := s.app.Delivery.NotifyMailbox(ctx, resumeMessage.MailboxID)
	if err != nil {
		return fmt.Errorf("notify developer-b resume mailbox: %w", err)
	}
	resumed, err := s.app.Runner.ProcessResumeQueue(ctx, "cp-probe-docker-replay")
	if err != nil {
		return fmt.Errorf("process developer-b resume queue: %w", err)
	}
	developerB.ResumeMailboxID = resumeMessage.MailboxID
	developerB.ResumeQueueState = resumed.State
	s.trace = append(s.trace, cpprobe.TraceStep{
		Actor:        "developer-b",
		EntryPoint:   "runtime.runner",
		Capability:   "session.resume",
		InputSummary: "resume developer-b Docker session from mailbox signal for conflict repair",
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"mailbox_id":       resumeMessage.MailboxID,
			"session_route_id": developerB.Session.Route.ID,
			"delivery_state":   delivered.State,
			"resume_state":     resumed.State,
		},
	})
	if resumed.RouteID != developerB.Session.Route.ID || resumed.MailboxID != resumeMessage.MailboxID {
		return fmt.Errorf("developer-b resume result = %+v, want route %s mailbox %s", resumed, developerB.Session.Route.ID, resumeMessage.MailboxID)
	}
	retryConflict, err := s.driver.callAny(ctx, developerB.Session, "git.merge_preview", map[string]any{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": aApply.AppliedRef,
	}, "cp-probe-docker-retry-conflict-b")
	if err != nil {
		return err
	}
	s.recordDockerStep("developer-b", "docker.coordlink", "git.merge_preview", "developer-b retries conflict preview before resolving with committed repair", retryConflict)
	if err := requireDockerRejected(retryConflict, "MERGE_CONFLICTS_FOUND", "developer-b retry conflict preview"); err != nil {
		return err
	}
	developerB.RetryConflict = retryConflict
	resolved, err := cpProbeDockerCallData[codemanagement.ResolveMergeResult](ctx, s, developerB.Session, "git.resolve", map[string]any{
		"merge_attempt_id":    retryConflict.CanonicalIDs["merge_attempt_id"],
		"resolved_head_ref":   bResolveCommit.CommitSHA,
		"expected_target_ref": aApply.AppliedRef,
	}, "cp-probe-docker-resolve-b", "developer-b resolves retry conflict with same-session repair commit")
	if err != nil {
		return err
	}
	if resolved.MergeAttempt.State != "resolved" || resolved.ConflictSet.State != "resolved" || resolved.ConflictSet.ResolvedBy != "developer-b" {
		return fmt.Errorf("developer-b resolve result = %+v, want resolved by developer-b", resolved)
	}
	developerB.Resolve = resolved
	bApply, err := cpProbeDockerCallData[codemanagement.MergeApplyResult](ctx, s, developerB.Session, "git.merge_apply", map[string]any{
		"merge_attempt_id":    resolved.MergeAttempt.ID,
		"expected_target_ref": aApply.AppliedRef,
	}, "cp-probe-docker-apply-b-resolved", "developer-b applies resolved transaction_count changeset")
	if err != nil {
		return err
	}
	if bApply.AppliedRef == "" || bApply.AppliedRef == aApply.AppliedRef {
		return fmt.Errorf("developer-b apply ref did not advance from developer-a ref")
	}
	developerB.Apply = bApply
	s.finalRef = bApply.AppliedRef
	bReport, err := cpProbeDockerCallData[coordination.Evidence](ctx, s, developerB.Session, "report.submit", map[string]any{
		"summary": "developer-b resolved stale/conflict and merged transaction_count",
		"content": "changeset=" + bSubmit.ChangeSet.ID + "; stale_operation=" + staleApply.CanonicalIDs["operation_id"] + "; final_ref=" + bApply.AppliedRef,
	}, "cp-probe-docker-report-b", "developer-b submits stale/conflict repair report")
	if err != nil {
		return err
	}
	developerB.Report = bReport
	s.developerBReport = bReport
	if _, err := cpProbeDockerCallData[coordination.CompleteContractResult](ctx, s, developerB.Session, "contract.complete", map[string]any{
		"evidence_ids": append(commandEvidenceIDs(developerB.Commands), bReport.ID),
		"summary":      "developer-b repaired stale/conflict feedback and merged transaction_count",
	}, "cp-probe-docker-complete-b", "developer-b completes stale/conflict repair contract"); err != nil {
		return err
	}
	if err := s.finishSession(ctx, developerB.Session, "completed", "developer-b stale/conflict repair completed"); err != nil {
		return err
	}
	s.developerAData = developerA
	s.developerBData = developerB
	if err := s.driveDockerVerifierAndRootCloseout(ctx, coordinator, aSubmit, bSubmit); err != nil {
		return err
	}
	return nil
}

func (s *cpProbeDockerState) startAgent(ctx context.Context, agentID string) (cpruntime.AssignmentSession, error) {
	session, err := s.app.Runner.StartNext(ctx, agentID)
	if err != nil {
		return cpruntime.AssignmentSession{}, err
	}
	if len(session.Env) == 0 || strings.TrimSpace(session.Env["COORDPLANE_TOKEN"]) == "" {
		return cpruntime.AssignmentSession{}, fmt.Errorf("runtime session did not return an in-memory coordlink token")
	}
	s.trace = append(s.trace, cpprobe.TraceStep{
		Actor:        agentID,
		EntryPoint:   "runtime.runner",
		Capability:   "session.start",
		InputSummary: "start Docker/Claude runtime session",
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"attempt_id":       session.AttemptID,
			"lease_id":         session.LeaseID,
			"session_route_id": session.Route.ID,
			"runtime_id":       session.Route.RuntimeID,
		},
	})
	return session, nil
}

func (s *cpProbeDockerState) startDeveloperWorkspace(ctx context.Context, agentID string, work coordination.AddContractResult) (dockerBridgeWorkspaceData, error) {
	var out dockerBridgeWorkspaceData
	session, err := s.startAgent(ctx, agentID)
	if err != nil {
		return out, environmentBlockedError{Message: agentID + " Docker/Claude session could not start", Cause: err}
	}
	out.Session = session
	out.Work = work
	prepared, err := cpProbeDockerCallData[codemanagement.WorkspacePrepareResult](ctx, s, session, "workspace.prepare", map[string]any{
		"repo_path":        s.tinyLedger.RepoPath,
		"canonical_branch": s.tinyLedger.CanonicalBranch,
		"workspace_root":   cpruntime.ContainerWorkspacePath,
		"contract_id":      work.ContractID,
	}, "cp-probe-docker-prepare-"+agentID, agentID+" prepares controlled Docker workspace")
	if err != nil {
		return out, err
	}
	out.Prepare = prepared
	if prepared.Workspace.AgentPath == "" || strings.Contains(prepared.Workspace.AgentPath, prepared.Workspace.Path) {
		return out, fmt.Errorf("%s workspace bridge did not return a container-safe agent path", agentID)
	}
	if prepared.Workspace.BaseRef != s.tinyLedger.BaseRef {
		return out, fmt.Errorf("%s workspace base ref = %s, want %s", agentID, prepared.Workspace.BaseRef, s.tinyLedger.BaseRef)
	}
	if _, err := cpProbeDockerCallData[codemanagement.GitStatusResult](ctx, s, session, "git.status", map[string]any{
		"workspace_id": prepared.Workspace.ID,
	}, "cp-probe-docker-status-"+agentID, agentID+" reads controlled Git status after workspace prepare"); err != nil {
		return out, err
	}
	return out, nil
}

func (s *cpProbeDockerState) writeTinyLedgerFiles(ctx context.Context, data *dockerBridgeWorkspaceData, files map[string]string, keyPrefix, summary string) error {
	if data == nil {
		return fmt.Errorf("docker file writer requires workspace data")
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		if err := s.writeWorkspaceFile(ctx, data, name, files[name], keyPrefix); err != nil {
			return err
		}
	}
	if _, err := cpProbeDockerCallData[codemanagement.GitStatusResult](ctx, s, data.Session, "git.status", map[string]any{
		"workspace_id": data.Prepare.Workspace.ID,
	}, keyPrefix+"-status", summary+"; status observed through controlled Git"); err != nil {
		return err
	}
	if _, err := cpProbeDockerCallData[codemanagement.GitDiffResult](ctx, s, data.Session, "git.diff", map[string]any{
		"workspace_id": data.Prepare.Workspace.ID,
	}, keyPrefix+"-diff", summary+"; diff observed through controlled Git"); err != nil {
		return err
	}
	return nil
}

func (s *cpProbeDockerState) writeWorkspaceFile(ctx context.Context, data *dockerBridgeWorkspaceData, name, content, keyPrefix string) error {
	dir := path.Dir(name)
	script := ": > " + shellQuote(name)
	if dir != "." {
		script = "mkdir -p " + shellQuote(dir) + " && " + script
	}
	truncate, err := s.runWorkspaceShell(ctx, data, script, keyPrefix+"-"+safeKeyPart(name)+"-truncate", "truncate "+name+" inside Docker controlled workspace")
	if err != nil {
		return err
	}
	data.Commands = append(data.Commands, truncate)
	for idx, chunk := range stringChunks(content, 1800) {
		appendRun, err := s.runWorkspaceShell(ctx, data, "printf %s "+shellQuote(chunk)+" >> "+shellQuote(name), fmt.Sprintf("%s-%s-append-%02d", keyPrefix, safeKeyPart(name), idx+1), "append "+name+" chunk inside Docker controlled workspace")
		if err != nil {
			return err
		}
		data.Commands = append(data.Commands, appendRun)
	}
	return nil
}

func (s *cpProbeDockerState) runWorkspaceShell(ctx context.Context, data *dockerBridgeWorkspaceData, script, idempotencyKey, summary string) (commandRunData, error) {
	command, err := cpProbeDockerCallData[commandRunData](ctx, s, data.Session, "command.run", map[string]any{
		"lease_id":         data.Session.LeaseID,
		"cwd":              data.Prepare.Workspace.ID,
		"argv":             []string{"sh", "-lc", script},
		"timeout_seconds":  30,
		"max_output_bytes": 4096,
		"purpose":          "CP-PROBE Docker Tiny Ledger replay",
	}, idempotencyKey, summary)
	if err != nil {
		return commandRunData{}, err
	}
	if command.Status != "succeeded" {
		return commandRunData{}, fmt.Errorf("command.run %s status is %s", idempotencyKey, command.Status)
	}
	return command, nil
}

func (s *cpProbeDockerState) finishSession(ctx context.Context, session cpruntime.AssignmentSession, status, summary string) error {
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    status,
		Summary:   summary,
	}); err != nil {
		return fmt.Errorf("finish %s session: %w", session.Route.AgentID, err)
	}
	s.trace = append(s.trace, cpprobe.TraceStep{
		Actor:        session.Route.AgentID,
		EntryPoint:   "runtime.runner",
		Capability:   "session.finish",
		InputSummary: summary,
		Status:       string(capability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"attempt_id":       session.AttemptID,
			"session_route_id": session.Route.ID,
			"status":           status,
		},
	})
	return nil
}

func (s *cpProbeDockerState) driveDockerVerifierAndRootCloseout(ctx context.Context, coordinator cpruntime.AssignmentSession, aSubmit, bSubmit codemanagement.SubmitChangeSetResult) error {
	verifierWork, err := cpProbeDockerCallData[coordination.AddContractResult](ctx, s, coordinator, "contract.add", map[string]any{
		"title":                   "Verifier: CP-PROBE-001 Docker Tiny Ledger closeout",
		"objective":               "Verify final Tiny Ledger category summary and transaction_count evidence, then submit validation.assessment.",
		"target_agent_id":         "verifier",
		"completion_requirements": []string{"validation_assessment"},
	}, "cp-probe-docker-add-verifier", "coordinator dispatches Docker verifier closeout")
	if err != nil {
		return err
	}
	s.verifierWork = verifierWork
	if _, err := cpProbeDockerCallData[coordination.Assignment](ctx, s, coordinator, "contract.wait", map[string]any{
		"reason":           "waiting for Docker verifier validation assessment",
		"waiting_for_ref":  "contract:" + verifierWork.ContractID,
		"session_route_id": coordinator.Route.ID,
	}, "cp-probe-docker-wait-verifier", "coordinator waits for verifier validation"); err != nil {
		return err
	}
	verifier, err := s.startAgent(ctx, "verifier")
	if err != nil {
		return environmentBlockedError{Message: "verifier Docker/Claude session could not start", Cause: err}
	}
	s.verifier = verifier
	if _, err := cpProbeDockerCallData[coordination.ContractContext](ctx, s, verifier, "contract.context", map[string]any{}, "cp-probe-docker-verifier-context", "verifier reads Docker validation contract context"); err != nil {
		return err
	}
	prepared, err := cpProbeDockerCallData[codemanagement.WorkspacePrepareResult](ctx, s, verifier, "workspace.prepare", map[string]any{
		"repo_path":        s.tinyLedger.RepoPath,
		"canonical_branch": s.tinyLedger.CanonicalBranch,
		"workspace_root":   cpruntime.ContainerWorkspacePath,
		"contract_id":      verifierWork.ContractID,
	}, "cp-probe-docker-verifier-prepare", "verifier prepares final canonical Tiny Ledger workspace")
	if err != nil {
		return err
	}
	if prepared.Workspace.BaseRef != s.finalRef {
		return fmt.Errorf("verifier workspace base ref = %s, want final ref %s", prepared.Workspace.BaseRef, s.finalRef)
	}
	verifyCommand, err := cpProbeDockerCallData[commandRunData](ctx, s, verifier, "command.run", map[string]any{
		"lease_id":         verifier.LeaseID,
		"cwd":              prepared.Workspace.ID,
		"argv":             []string{"node", "-e", tinyLedgerVerifierNodeScript()},
		"timeout_seconds":  30,
		"max_output_bytes": 4096,
		"purpose":          "CP-PROBE Docker verifier Tiny Ledger final state check",
	}, "cp-probe-docker-verifier-command", "verifier checks final Tiny Ledger category and transaction_count state inside Docker workspace")
	if err != nil {
		return err
	}
	if verifyCommand.Status != "succeeded" {
		return fmt.Errorf("verifier command.run status is %s", verifyCommand.Status)
	}
	s.developerBData.ValidationCommand = verifyCommand
	verifierReport, err := cpProbeDockerCallData[coordination.Evidence](ctx, s, verifier, "report.submit", map[string]any{
		"summary": "CP-PROBE Docker Tiny Ledger final state reviewed",
		"content": "final_ref=" + s.finalRef + "; command_run=" + verifyCommand.CommandRunID + "; changesets=" + aSubmit.ChangeSet.ID + "," + bSubmit.ChangeSet.ID,
	}, "cp-probe-docker-verifier-report", "verifier submits Docker final state report")
	if err != nil {
		return err
	}
	assessment, err := cpProbeDockerCallData[validationData](ctx, s, verifier, "validation.assessment", map[string]any{
		"assessed_contract_id": s.root.ContractID,
		"verdict":              "pass",
		"reason":               "Docker CP-PROBE replay completed stale/conflict repair, final Tiny Ledger checks, and durable evidence through coordlink capabilities",
		"summary":              "CP-PROBE-001 Docker Tiny Ledger replay passed",
		"checked_refs": []validation.CheckedRef{
			{Kind: "command_run", ID: verifyCommand.CommandRunID},
			{Kind: "evidence", ID: verifierReport.ID},
		},
	}, "cp-probe-docker-validation-assessment", "verifier submits passed validation.assessment")
	if err != nil {
		return err
	}
	if assessment.Verdict != "pass" || assessment.EvidenceID == "" {
		return fmt.Errorf("verifier validation assessment = %+v, want pass with evidence", assessment)
	}
	s.validation = assessment
	if _, err := cpProbeDockerCallData[coordination.CompleteContractResult](ctx, s, verifier, "contract.complete", map[string]any{
		"evidence_ids": []string{assessment.EvidenceID},
		"summary":      "validation.assessment submitted pass verdict for Docker replay",
	}, "cp-probe-docker-complete-verifier", "verifier completes Docker validation contract"); err != nil {
		return err
	}
	if err := s.finishSession(ctx, verifier, "completed", "verifier submitted Docker validation assessment"); err != nil {
		return err
	}
	mailboxes, err := cpProbeDockerCallData[[]coordination.MailboxItem](ctx, s, coordinator, "mailbox.list", map[string]any{}, "cp-probe-docker-coordinator-mailbox-list", "coordinator reads Docker replay mailbox")
	if err != nil {
		return err
	}
	verifierMailbox := mailboxForContract(mailboxes, verifierWork.ContractID)
	if verifierMailbox.ID == "" {
		return fmt.Errorf("verifier child completion mailbox not found")
	}
	if _, err := cpProbeDockerCallData[coordination.MailboxItem](ctx, s, coordinator, "mailbox.get", map[string]any{
		"mailbox_id": verifierMailbox.ID,
	}, "cp-probe-docker-coordinator-mailbox-get", "coordinator opens verifier validation feedback mailbox"); err != nil {
		return err
	}
	if _, err := cpProbeDockerCallData[coordination.AgentCommunicationEnvelope](ctx, s, coordinator, "communication.read", map[string]any{
		"mailbox_id": verifierMailbox.ID,
	}, "cp-probe-docker-coordinator-communication-read", "coordinator reads verifier result envelope through protected communication.read"); err != nil {
		return err
	}
	if _, err := cpProbeDockerCallData[coordination.MailboxItem](ctx, s, coordinator, "mailbox.resolve", map[string]any{
		"mailbox_id":   verifierMailbox.ID,
		"followup_ref": "validation_assessment:" + assessment.AssessmentID,
	}, "cp-probe-docker-coordinator-mailbox-resolve", "coordinator resolves verifier validation feedback"); err != nil {
		return err
	}
	rootReport, err := cpProbeDockerCallData[coordination.Evidence](ctx, s, coordinator, "report.submit", map[string]any{
		"summary": "CP-PROBE Docker Tiny Ledger root closeout",
		"content": "validation_assessment=" + assessment.AssessmentID + "; final_ref=" + s.finalRef,
	}, "cp-probe-docker-root-report", "coordinator submits Docker root closeout report")
	if err != nil {
		return err
	}
	if _, err := cpProbeDockerCallData[coordination.CompleteContractResult](ctx, s, coordinator, "contract.complete", map[string]any{
		"evidence_ids": []string{rootReport.ID},
		"summary":      "coordinator read verifier validation feedback and completed Docker CP-PROBE root",
	}, "cp-probe-docker-complete-root", "coordinator completes Docker CP-PROBE root contract"); err != nil {
		return err
	}
	return s.finishSession(ctx, coordinator, "completed", "coordinator completed Docker CP-PROBE root contract")
}

func commandEvidenceIDs(commands []commandRunData) []string {
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		if command.EvidenceID != "" {
			out = append(out, command.EvidenceID)
		}
	}
	return out
}

func requireDockerRejected(response typedResponse, code, label string) error {
	if response.Status != capability.StatusRejected || response.ErrorCode != code {
		return fmt.Errorf("%s returned %s %s, want rejected %s", label, response.Status, response.ErrorCode, code)
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringChunks(value string, size int) []string {
	if size <= 0 || len(value) <= size {
		return []string{value}
	}
	var out []string
	for len(value) > size {
		out = append(out, value[:size])
		value = value[size:]
	}
	if value != "" {
		out = append(out, value)
	}
	return out
}

func safeKeyPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "file"
	}
	return out
}

func tinyLedgerVerifierNodeScript() string {
	return `
const fs = require('fs');
function fail(message) {
  console.error(message);
  process.exit(1);
}
const csv = fs.readFileSync('data/transactions.csv', 'utf8').trim().split(/\r?\n/).map(line => line.split(','));
const header = csv[0] || [];
if (header.join(',') !== 'id,type,amount,category') fail('unexpected csv header: ' + header.join(','));
let income = 0, expense = 0, count = 0;
const categories = {};
for (const row of csv.slice(1)) {
  if (row.length !== 4) fail('unexpected row width: ' + row.join(','));
  const amount = Number(row[2]);
  if (!Number.isFinite(amount)) fail('invalid amount: ' + row.join(','));
  count++;
  if (row[1] === 'income') income += amount;
  else if (row[1] === 'expense') expense += amount;
  else fail('invalid type: ' + row[1]);
  categories[row[3]] = (categories[row[3]] || 0) + amount;
}
const ledger = fs.readFileSync('ledger/ledger.go', 'utf8');
if (!ledger.includes('TransactionCount int') || !ledger.includes('Categories       map[string]int')) fail('resolved summary fields missing');
if (income !== 125 || expense !== 40 || income - expense !== 85 || count !== 3) fail('unexpected totals');
if (categories.sales !== 100 || categories.ops !== 40 || categories.services !== 25) fail('unexpected categories');
console.log(JSON.stringify({income, expense, balance: income - expense, transaction_count: count, categories}));
`
}

func cpProbeDockerCallData[T any](ctx context.Context, state *cpProbeDockerState, session cpruntime.AssignmentSession, name string, input map[string]any, idempotencyKey, summary string) (T, error) {
	var out T
	response, err := state.driver.call(ctx, session, name, input, idempotencyKey)
	state.recordDockerStep(session.Route.AgentID, "docker.coordlink", name, summary, response)
	if err != nil {
		return out, err
	}
	if len(response.Data) == 0 {
		return out, fmt.Errorf("coordlink %s accepted response had no data", name)
	}
	if err := json.Unmarshal(response.Data, &out); err != nil {
		return out, fmt.Errorf("decode coordlink %s response data: %w", name, err)
	}
	return out, nil
}

func (s *cpProbeDockerState) recordDockerStep(actor, entrypoint, capabilityName, summary string, response typedResponse) {
	status := string(response.Status)
	if status == "" {
		status = "error"
	}
	step := cpprobe.TraceStep{
		Actor:        actor,
		EntryPoint:   entrypoint,
		Capability:   capabilityName,
		InputSummary: summary,
		Status:       status,
		ErrorCode:    response.ErrorCode,
		CanonicalIDs: cloneStringMap(response.CanonicalIDs),
	}
	s.trace = append(s.trace, step)
}

func cpProbeReplayStatus(err error) cpprobe.ConclusionStatus {
	if err != nil && strings.Contains(err.Error(), "environment blocked") {
		return cpprobe.ConclusionEnvironmentBlocked
	}
	if err != nil && strings.Contains(err.Error(), "DOCKER_RUNTIME_NOT_READY") {
		return cpprobe.ConclusionEnvironmentBlocked
	}
	if err != nil && strings.Contains(err.Error(), "CLAUDE_AUTH_REQUIRED") {
		return cpprobe.ConclusionEnvironmentBlocked
	}
	if err != nil && strings.Contains(err.Error(), "coordlink not available") {
		return cpprobe.ConclusionEnvironmentBlocked
	}
	return cpprobe.ConclusionFailed
}

func cpProbeBlockedStep(actor, capabilityName, blocker string) cpprobe.TraceStep {
	return cpprobe.TraceStep{
		Actor:        actor,
		EntryPoint:   "coordplane release-health cp-probe-001",
		Capability:   capabilityName,
		InputSummary: blocker,
		Status:       string(cpprobe.ConclusionEnvironmentBlocked),
		ErrorCode:    "ENVIRONMENT_BLOCKED",
		NextActions:  []string{"fix the Docker/Claude release-health environment and rerun"},
	}
}

func cpProbeDockerInspect(ctx context.Context, app *backend.Backend, rootContractID string, gitSummary *cpprobe.GitOperationSummary) *cpprobe.RedactedInspect {
	if app == nil {
		return nil
	}
	inspect, err := app.Inspect(ctx)
	if err != nil {
		return nil
	}
	var workspaces []cpprobe.WorkspaceSummary
	var ops []cpprobe.GitOperationBrief
	if gitSummary != nil {
		workspaces = gitSummary.Workspaces
		ops = gitSummary.Operations
	} else if summary, err := cpProbeGitSummary(ctx, app.DB); err == nil {
		workspaces = summary.Workspaces
		ops = summary.Operations
	}
	return &cpprobe.RedactedInspect{
		Scenario:       cpprobe.ScenarioID,
		Status:         inspect.Status,
		TeamID:         inspect.TeamID,
		RootContractID: rootContractID,
		Counts:         inspect.Counts,
		Contracts:      contractSummaries(ctx, app.DB),
		Workspaces:     workspaces,
		GitOperations:  ops,
		Redacted:       true,
		DenylistChecks: []string{
			"database path marker",
			"runtime root marker",
			"docker socket marker",
			"authorization bearer marker",
			"secret key prefix marker",
			"Claude and Anthropic token key markers",
		},
	}
}

func cpProbeGitSummary(ctx context.Context, db *sql.DB) (cpprobe.GitOperationSummary, error) {
	repos, err := repositorySummaries(ctx, db)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	workspaces, err := workspaceSummaries(ctx, db)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	ops, err := gitOperationBriefs(ctx, db)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	return cpprobe.GitOperationSummary{
		Scenario:      cpprobe.ScenarioID,
		Repositories:  repos,
		Workspaces:    workspaces,
		Operations:    ops,
		NoActiveLocks: countRowsWhere(ctx, db, "git_locks", "state = 'active'") == 0,
	}, nil
}

func cpProbeCleanupPassed(ctx context.Context, cfg CPProbe001Config, outcome *cpProbeDockerReplayOutcome) bool {
	if err := cleanupManagedContainers(ctx, cfg.DockerTeamID); err != nil {
		if outcome != nil {
			cleaned := cpProbeRedact(err.Error(), cfg)
			if outcome.Blocker == "" {
				outcome.Blocker = cleaned
			} else if !strings.Contains(outcome.Blocker, cleaned) {
				outcome.Blocker += "; cleanup: " + cleaned
			}
			outcome.TraceSteps = append(outcome.TraceSteps, cpprobe.TraceStep{
				Actor:        "release-health",
				EntryPoint:   "docker",
				Capability:   "cleanup.managed_containers",
				InputSummary: "cleanup managed CP-PROBE Docker containers",
				Status:       string(cpprobe.ConclusionEnvironmentBlocked),
				ErrorCode:    "CLEANUP_NOT_VERIFIED",
			})
		}
		return false
	}
	if outcome != nil {
		outcome.TraceSteps = append(outcome.TraceSteps, cpprobe.TraceStep{
			Actor:        "release-health",
			EntryPoint:   "docker",
			Capability:   "cleanup.managed_containers",
			InputSummary: "cleanup managed CP-PROBE Docker containers",
			Status:       string(capability.StatusAccepted),
		})
	}
	return true
}

func applyCPProbeDockerReplayOutcome(artifacts cpprobe.ReportArtifacts, outcome cpProbeDockerReplayOutcome, fallbackBlocker string) cpprobe.ReportArtifacts {
	blocker := strings.TrimSpace(outcome.Blocker)
	if blocker == "" {
		blocker = strings.TrimSpace(fallbackBlocker)
	}
	if blocker == "" && outcome.Status != cpprobe.ConclusionPassed {
		blocker = "Docker/Claude replay did not satisfy CP-PROBE-001 pass criteria"
	}
	if len(outcome.TraceSteps) > 0 {
		artifacts.ManualTrace.Steps = append(artifacts.ManualTrace.Steps, outcome.TraceSteps...)
	}
	if outcome.Inspect != nil {
		artifacts.Inspect = *outcome.Inspect
	}
	if outcome.GitSummary != nil && len(outcome.GitSummary.Operations) > 0 {
		artifacts.GitSummary = *outcome.GitSummary
	}
	artifacts.FailureMatrix = cpProbeApplyFailureMatrixOutcome(artifacts.FailureMatrix, outcome, blocker)
	artifacts.Conclusion = cpProbeApplyConclusionOutcome(artifacts.Conclusion, outcome, blocker)
	return artifacts
}

func cpProbeApplyFailureMatrixOutcome(matrix cpprobe.FailureMatrix, outcome cpProbeDockerReplayOutcome, blocker string) cpprobe.FailureMatrix {
	finalStatus := cpProbeFinalStatus(outcome)
	item := cpprobe.FailureMatrixItem{
		ID:             "docker-claude-equivalent-replay",
		Status:         string(finalStatus),
		Capability:     "runtime.docker/claude + docker.coordlink + workspace.prepare/command.run/git.*",
		StateAssertion: "Docker replay status: " + string(finalStatus),
	}
	switch finalStatus {
	case cpprobe.ConclusionPassed:
		item.Status = "covered"
		item.StateAssertion = "four Docker/Claude agents completed the CP-PROBE Tiny Ledger replay through backend/coordlink capabilities, verifier/root closeout, denylist, and cleanup"
	case cpprobe.ConclusionFailed:
		item.Status = "failed"
		item.StateAssertion = blocker
		item.NextStep = "complete the Docker Tiny Ledger stale/conflict repair, verifier assessment, and coordinator root closeout replay"
	default:
		item.Status = "environment_blocked"
		item.StateAssertion = blocker
		item.NextStep = "restore Docker/Claude auth, coordlink, and runtime workspace bridge prerequisites and rerun"
	}
	found := false
	for idx := range matrix.Items {
		if matrix.Items[idx].ID == item.ID {
			matrix.Items[idx] = item
			found = true
			break
		}
	}
	if !found {
		matrix.Items = append(matrix.Items, item)
	}
	matrix.Items = upsertFailureMatrixItem(matrix.Items, dockerCoverageItem(outcome, "docker-controlled-workspace-bridge",
		"workspace.prepare + command.run + git.status/git.diff/git.commit",
		dockerTraceHasAccepted(outcome.TraceSteps, "workspace.prepare") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "command.run") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "git.commit"),
		"Docker controlled workspace bridge was not reached",
		"developer containers wrote Tiny Ledger files via command.run and backend Git capabilities observed/committed explicit paths"))
	matrix.Items = upsertFailureMatrixItem(matrix.Items, dockerCoverageItem(outcome, "docker-concurrent-stale-target",
		"git.merge_apply",
		dockerTraceHasRejected(outcome.TraceSteps, "git.merge_apply", "STALE_TARGET_REF"),
		"old-base stale merge apply was not observed",
		"developer-b old-base merge apply was rejected with STALE_TARGET_REF"))
	matrix.Items = upsertFailureMatrixItem(matrix.Items, dockerCoverageItem(outcome, "docker-conflict-resolve",
		"git.merge_preview/git.conflicts/git.abort/git.resolve/git.merge_apply",
		dockerTraceHasRejected(outcome.TraceSteps, "git.merge_preview", "MERGE_CONFLICTS_FOUND") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "git.conflicts") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "git.abort") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "git.resolve") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "git.merge_apply"),
		"same-session stale/conflict repair was not fully observed",
		"developer-b inspected, aborted, resumed, resolved, and applied the conflict repair through Docker coordlink"))
	matrix.Items = upsertFailureMatrixItem(matrix.Items, dockerCoverageItem(outcome, "docker-verifier-root-closeout",
		"validation.assessment + communication.read + contract.complete",
		dockerTraceHasAccepted(outcome.TraceSteps, "validation.assessment") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "communication.read") &&
			dockerTraceHasAccepted(outcome.TraceSteps, "contract.complete"),
		"verifier/root closeout was not fully observed",
		"verifier submitted passed validation.assessment and coordinator read the result envelope before root completion"))
	matrix.Items = upsertFailureMatrixItem(matrix.Items, cpprobe.FailureMatrixItem{
		ID:             "artifact-denylist",
		Status:         coverageStatus(outcome, outcome.DenylistPassed),
		Capability:     "artifact.denylist",
		StateAssertion: coverageAssertion(outcome, outcome.DenylistPassed, "artifact denylist scan has not passed", "five formal artifacts passed denylist leak scan"),
		NextStep:       coverageNextStep(outcome, outcome.DenylistPassed, "remove leaked marker from CP-PROBE artifacts and rerun"),
	})
	matrix.Items = upsertFailureMatrixItem(matrix.Items, cpprobe.FailureMatrixItem{
		ID:             "managed-docker-cleanup",
		Status:         coverageStatus(outcome, outcome.CleanupPassed),
		Capability:     "docker.cleanup",
		StateAssertion: coverageAssertion(outcome, outcome.CleanupPassed, "managed Docker cleanup has not passed", "managed CP-PROBE Docker containers were removed or none remained"),
		NextStep:       coverageNextStep(outcome, outcome.CleanupPassed, "restore Docker cleanup access and rerun"),
	})
	return matrix
}

func dockerCoverageItem(outcome cpProbeDockerReplayOutcome, id, capabilityName string, covered bool, missing, coveredAssertion string) cpprobe.FailureMatrixItem {
	return cpprobe.FailureMatrixItem{
		ID:             id,
		Status:         coverageStatus(outcome, covered),
		Capability:     capabilityName,
		StateAssertion: coverageAssertion(outcome, covered, missing, coveredAssertion),
		NextStep:       coverageNextStep(outcome, covered, "rerun CP-PROBE Docker replay after fixing the blocking behavior"),
	}
}

func coverageStatus(outcome cpProbeDockerReplayOutcome, covered bool) string {
	if covered {
		return "covered"
	}
	switch outcome.Status {
	case cpprobe.ConclusionFailed:
		return "failed"
	case cpprobe.ConclusionPassed:
		return "failed"
	default:
		return "environment_blocked"
	}
}

func coverageAssertion(outcome cpProbeDockerReplayOutcome, covered bool, missing, coveredAssertion string) string {
	if covered {
		return coveredAssertion
	}
	blocker := strings.TrimSpace(outcome.Blocker)
	if blocker == "" {
		return missing
	}
	return missing + ": " + blocker
}

func coverageNextStep(outcome cpProbeDockerReplayOutcome, covered bool, next string) string {
	if covered {
		return ""
	}
	if outcome.Status == cpprobe.ConclusionEnvironmentBlocked {
		return "restore Docker/Claude auth, coordlink, runtime workspace bridge, and cleanup prerequisites and rerun"
	}
	return next
}

func dockerTraceHasAccepted(steps []cpprobe.TraceStep, capabilityName string) bool {
	for _, step := range steps {
		if step.Capability == capabilityName && step.Status == string(capability.StatusAccepted) {
			return true
		}
	}
	return false
}

func dockerTraceHasRejected(steps []cpprobe.TraceStep, capabilityName, errorCode string) bool {
	for _, step := range steps {
		if step.Capability == capabilityName && step.Status == string(capability.StatusRejected) && step.ErrorCode == errorCode {
			return true
		}
	}
	return false
}

func cpProbeApplyConclusionOutcome(report cpprobe.ConclusionReport, outcome cpProbeDockerReplayOutcome, blocker string) cpprobe.ConclusionReport {
	report.Status = cpProbeFinalStatus(outcome)
	switch report.Status {
	case cpprobe.ConclusionPassed:
		report.Covered = appendUnique(report.Covered,
			"Docker/Claude Tiny Ledger replay completed through four container agents",
			"Developer A merged category summary through controlled Git",
			"Developer B observed stale target, conflict feedback, same-session resume, manual resolve, and merge",
			"verifier Docker validation.assessment passed",
			"coordinator read verifier communication envelope and completed root contract",
			"artifact denylist scan and managed Docker cleanup passed",
		)
		report.NotCovered = nil
		report.NextSteps = nil
	case cpprobe.ConclusionFailed:
		report.Covered = appendUnique(report.Covered,
			"Docker/Claude replay preflight reached product behavior validation",
		)
		report.NotCovered = appendUnique(nil,
			"CP-PROBE Docker replay, verifier/root closeout, artifact denylist, and cleanup all passing together",
			blocker,
		)
		report.NextSteps = appendUnique(nil,
			"fix the failing Docker replay behavior and rerun make release-health-cp-probe-001",
			"promote CP-PROBE-001 to passed only after Docker replay, verifier/root closeout, denylist, and cleanup all pass",
		)
	default:
		report.Status = cpprobe.ConclusionEnvironmentBlocked
		report.NotCovered = appendUnique(nil,
			"Docker/Claude equivalent replay",
			blocker,
		)
		report.NextSteps = appendUnique(nil,
			"provide Docker daemon access, a container-reachable backend URL, coordlink binary, and Claude auth",
			"rerun make release-health-cp-probe-001",
		)
	}
	return report
}

func cpProbeFinalStatus(outcome cpProbeDockerReplayOutcome) cpprobe.ConclusionStatus {
	if outcome.Status != cpprobe.ConclusionPassed {
		return outcome.Status
	}
	if !outcome.CleanupPassed {
		return cpprobe.ConclusionEnvironmentBlocked
	}
	if !outcome.DenylistPassed {
		return cpprobe.ConclusionFailed
	}
	return cpprobe.ConclusionPassed
}

func upsertFailureMatrixItem(items []cpprobe.FailureMatrixItem, item cpprobe.FailureMatrixItem) []cpprobe.FailureMatrixItem {
	for idx := range items {
		if items[idx].ID == item.ID {
			items[idx] = item
			return items
		}
	}
	return append(items, item)
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	out := make([]string, 0, len(values)+len(additions))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cpProbeRedact(value string, cfg CPProbe001Config) string {
	out := value
	for _, marker := range cpProbeDenylist(cfg) {
		if strings.TrimSpace(marker) == "" {
			continue
		}
		out = strings.ReplaceAll(out, marker, "[redacted]")
	}
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s *cpProbeState) call(ctx context.Context, actor, runtimeID, capabilityName, inputSummary string, input, scope any) (capability.Response[json.RawMessage], error) {
	raw := json.RawMessage(`{}`)
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return capability.Response[json.RawMessage]{}, err
		}
		raw = encoded
	}
	scopeRaw := json.RawMessage(`{}`)
	if scope != nil {
		encoded, err := json.Marshal(scope)
		if err != nil {
			return capability.Response[json.RawMessage]{}, err
		}
		scopeRaw = encoded
	}
	response := s.link.Call(ctx, capability.Call{
		CapabilityName: capabilityName,
		TraceID:        "cp-probe-001-release-health",
		Subject: capability.Subject{
			Kind:      "agent",
			ID:        actor,
			AgentID:   actor,
			RuntimeID: runtimeID,
		},
		Input: raw,
		Scope: scopeRaw,
	})
	s.trace.Steps = append(s.trace.Steps, cpprobe.TraceStep{
		Actor:        actor,
		EntryPoint:   "coordlink.adapter",
		Capability:   capabilityName,
		InputSummary: inputSummary,
		Status:       string(response.Status),
		ErrorCode:    response.ErrorCode,
		CanonicalIDs: cloneStringMap(response.CanonicalIDs),
		NextActions:  append([]string(nil), response.AllowedNextActions...),
	})
	if response.Status != capability.StatusAccepted {
		return response, fmt.Errorf("cp-probe %s returned %s %s: %s", capabilityName, response.Status, response.ErrorCode, response.Message)
	}
	return response, nil
}

func cpProbeCallData[T any](ctx context.Context, state *cpProbeState, actor, runtimeID, capabilityName, inputSummary string, input, scope any) (T, error) {
	var out T
	response, err := state.call(ctx, actor, runtimeID, capabilityName, inputSummary, input, scope)
	if err != nil {
		return out, err
	}
	if response.Data == nil {
		return out, fmt.Errorf("cp-probe %s accepted response had no data", capabilityName)
	}
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		return out, fmt.Errorf("decode cp-probe %s response data: %w", capabilityName, err)
	}
	return out, nil
}

func (s *cpProbeState) gitSummary(ctx context.Context) (cpprobe.GitOperationSummary, error) {
	repos, err := repositorySummaries(ctx, s.app.DB)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	workspaces, err := workspaceSummaries(ctx, s.app.DB)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	ops, err := gitOperationBriefs(ctx, s.app.DB)
	if err != nil {
		return cpprobe.GitOperationSummary{}, err
	}
	return cpprobe.GitOperationSummary{
		Scenario:      cpprobe.ScenarioID,
		Repositories:  repos,
		Workspaces:    workspaces,
		Operations:    ops,
		NoActiveLocks: countRowsWhere(ctx, s.app.DB, "git_locks", "state = 'active'") == 0,
	}, nil
}

func repositorySummaries(ctx context.Context, db *sql.DB) ([]cpprobe.RepositorySummary, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, canonical_branch, status FROM git_repositories ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cpprobe.RepositorySummary
	for rows.Next() {
		var item cpprobe.RepositorySummary
		if err := rows.Scan(&item.ID, &item.CanonicalBranch, &item.Status); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func contractSummaries(ctx context.Context, db *sql.DB) []cpprobe.ContractSummary {
	rows, err := db.QueryContext(ctx, `
SELECT id, COALESCE(issuer_contract_id, ''), target_id, status
FROM work_contracts
ORDER BY created_at, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []cpprobe.ContractSummary
	for rows.Next() {
		var item cpprobe.ContractSummary
		if err := rows.Scan(&item.ID, &item.IssuerContract, &item.TargetID, &item.Status); err != nil {
			return nil
		}
		out = append(out, item)
	}
	return out
}

func workspaceSummaries(ctx context.Context, db *sql.DB) ([]cpprobe.WorkspaceSummary, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, repo_id, agent_id, COALESCE(contract_id, ''), base_ref, head_ref, state
FROM git_workspaces
ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cpprobe.WorkspaceSummary
	for rows.Next() {
		var item cpprobe.WorkspaceSummary
		if err := rows.Scan(&item.ID, &item.RepoID, &item.AgentID, &item.ContractID, &item.BaseRef, &item.HeadRef, &item.State); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func gitOperationBriefs(ctx context.Context, db *sql.DB) ([]cpprobe.GitOperationBrief, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, operation_type, actor_agent_id, COALESCE(workspace_id, ''), repo_id, before_ref, after_ref, state
FROM git_operations
ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cpprobe.GitOperationBrief
	for rows.Next() {
		var item cpprobe.GitOperationBrief
		if err := rows.Scan(&item.ID, &item.OperationType, &item.ActorAgentID, &item.WorkspaceID, &item.RepoID, &item.BeforeRef, &item.AfterRef, &item.State); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func countRowsWhere(ctx context.Context, db *sql.DB, table, where string) int64 {
	var count int64
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+where).Scan(&count)
	return count
}

func mailboxForContract(items []coordination.MailboxItem, contractID string) coordination.MailboxItem {
	for _, item := range items {
		if strings.Contains(item.FollowupRef, "child_contract:"+contractID) {
			return item
		}
	}
	return coordination.MailboxItem{}
}

func cpProbeArtifactMap(dir string) map[string]string {
	return map[string]string{
		cpprobe.ManualTraceArtifact:         filepath.Join(dir, cpprobe.ManualTraceArtifact),
		cpprobe.InspectRedactedArtifact:     filepath.Join(dir, cpprobe.InspectRedactedArtifact),
		cpprobe.GitOperationSummaryArtifact: filepath.Join(dir, cpprobe.GitOperationSummaryArtifact),
		cpprobe.FailureMatrixArtifact:       filepath.Join(dir, cpprobe.FailureMatrixArtifact),
		cpprobe.ConclusionReportArtifact:    filepath.Join(dir, cpprobe.ConclusionReportArtifact),
	}
}

func cpProbeArtifactPaths(dir string) []string {
	return artifactPaths(dir,
		cpprobe.ManualTraceArtifact,
		cpprobe.InspectRedactedArtifact,
		cpprobe.GitOperationSummaryArtifact,
		cpprobe.FailureMatrixArtifact,
		cpprobe.ConclusionReportArtifact,
	)
}

func cpProbeDenylist(cfg CPProbe001Config) []string {
	markers := append([]string(nil), InspectLeakDenylist...)
	for _, marker := range []string{
		filepath.Clean(cfg.DBPath),
		filepath.Clean(cfg.RuntimeWorkspaceRoot),
		filepath.Clean(cfg.RuntimeHomeRoot),
		filepath.Clean(filepath.Join(cfg.WorkDir, "runtime")),
	} {
		if marker != "." && marker != "" {
			markers = append(markers, marker)
		}
	}
	return markers
}

func cpProbeStatusError(result CPProbe001Result) error {
	if result.Status == cpprobe.ConclusionPassed {
		return nil
	}
	if len(result.Blockers) > 0 {
		return fmt.Errorf("coordplane release-health: %s status is %s: %s", ScenarioCPProbe001, result.Status, strings.Join(result.Blockers, "; "))
	}
	return fmt.Errorf("coordplane release-health: %s status is %s", ScenarioCPProbe001, result.Status)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
