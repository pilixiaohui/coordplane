package releasehealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"coordplane/internal/backend"
	"coordplane/internal/capability"
	"coordplane/internal/codemanagement"
	"coordplane/internal/coordination"
	"coordplane/internal/releaseacceptance"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/validation"
)

const (
	defaultWorkflowTimeout = 45 * time.Minute
	defaultCoordlinkTTL    = 2 * time.Minute
)

type CPAccept001Config struct {
	DBPath        string
	RootContract  string
	TeamID        string
	TeamVersion   int
	TeamConfig    string
	ListenAddr    string
	BackendURL    string
	CoordlinkPath string
	DockerNetwork string
	ClaudeBinary  string
	ClaudeEnvKeys []string
	WorkDir       string
	RunLabel      string
	CreatedBy     string
}

type CPAccept001Result struct {
	Mode           string
	RootContractID string
	Acceptance     releaseacceptance.Acceptance
	Inspect        *backend.Inspect
}

type workflowState struct {
	app           *backend.Backend
	cfg           CPAccept001Config
	driver        *driver
	root          coordination.AddContractResult
	coordinator   cpruntime.AssignmentSession
	developer     cpruntime.AssignmentSession
	verifier      cpruntime.AssignmentSession
	developerWork coordination.AddContractResult
	verifierWork  coordination.AddContractResult
	commandRun    commandRunData
	developerRpt  coordination.Evidence
	changeSet     codemanagement.SubmitChangeSetResult
	validation    validationData
	rootReport    coordination.Evidence
}

type driver struct {
	executor cpruntime.ContainerExecutor
}

type typedResponse struct {
	OK           bool              `json:"ok"`
	Status       capability.Status `json:"status"`
	Data         json.RawMessage   `json:"data,omitempty"`
	ErrorCode    string            `json:"error_code,omitempty"`
	Message      string            `json:"message,omitempty"`
	RepairHint   string            `json:"repair_hint,omitempty"`
	CanonicalIDs map[string]string `json:"canonical_ids,omitempty"`
}

type commandRunData struct {
	CommandRunID string `json:"command_run_id"`
	EvidenceID   string `json:"evidence_id"`
	Status       string `json:"status"`
	StdoutRef    string `json:"stdout_ref"`
	StderrRef    string `json:"stderr_ref"`
}

type validationData struct {
	AssessmentID string `json:"assessment_id"`
	EvidenceID   string `json:"evidence_id"`
	Verdict      string `json:"verdict"`
}

type environmentBlockedError struct {
	Message string
	Cause   error
}

func (e environmentBlockedError) Error() string {
	if e.Cause == nil {
		return "release-health environment blocked: " + e.Message
	}
	return "release-health environment blocked: " + e.Message + ": " + e.Cause.Error()
}

func (e environmentBlockedError) Unwrap() error {
	return e.Cause
}

func RunCPAccept001(ctx context.Context, cfg CPAccept001Config) (CPAccept001Result, error) {
	cfg = cfg.withDefaults()
	if cfg.RootContract != "" {
		return evaluateExisting(ctx, cfg)
	}
	if cfg.DBPath == "" {
		return CPAccept001Result{}, errors.New("coordplane release-health: database path is required")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultWorkflowTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultWorkflowTimeout)
		defer cancel()
	}
	return driveWorkflow(ctx, cfg)
}

func (cfg CPAccept001Config) withDefaults() CPAccept001Config {
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(DefaultWorkDir, "coordplane.db")
	}
	if cfg.TeamID == "" {
		cfg.TeamID = DefaultTeamID
	}
	if cfg.TeamVersion == 0 {
		cfg.TeamVersion = DefaultTeamVersion
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListen
	}
	if cfg.BackendURL == "" {
		cfg.BackendURL = DefaultPublicURL
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "/usr/local/bin/claude"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = DefaultWorkDir
	}
	if cfg.RunLabel == "" {
		cfg.RunLabel = "cp-accept-001-release-health"
	}
	if cfg.CreatedBy == "" {
		cfg.CreatedBy = "release-health"
	}
	return cfg
}

func evaluateExisting(ctx context.Context, cfg CPAccept001Config) (CPAccept001Result, error) {
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         cfg.DBPath,
		TeamConfigPath: cfg.TeamConfig,
		TeamID:         cfg.TeamID,
	})
	if err != nil {
		return CPAccept001Result{}, err
	}
	defer func() {
		_ = app.Close()
	}()
	acceptance, err := app.EvaluateReleaseAcceptance(ctx, releaseacceptance.EvaluateInput{
		RootContractID: cfg.RootContract,
		TeamID:         cfg.TeamID,
		TeamVersion:    cfg.TeamVersion,
		RunLabel:       cfg.RunLabel,
		CreatedBy:      cfg.CreatedBy,
	})
	if err != nil {
		return CPAccept001Result{}, err
	}
	inspect, _ := app.Inspect(ctx)
	result := CPAccept001Result{
		Mode:           "verify_existing",
		RootContractID: cfg.RootContract,
		Acceptance:     acceptance,
		Inspect:        &inspect,
	}
	return result, statusError(acceptance)
}

func driveWorkflow(ctx context.Context, cfg CPAccept001Config) (CPAccept001Result, error) {
	if cfg.TeamConfig == "" {
		cfg.TeamConfig = TeamConfigPath
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return CPAccept001Result{}, fmt.Errorf("prepare release-health workdir: %w", err)
	}
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         cfg.DBPath,
		ListenAddr:     cfg.ListenAddr,
		TeamConfigPath: cfg.TeamConfig,
		TeamID:         cfg.TeamID,
		BackendURL:     cfg.BackendURL,
		DockerNetwork:  cfg.DockerNetwork,
		CoordlinkPath:  cfg.CoordlinkPath,
		ClaudeBinary:   cfg.ClaudeBinary,
		ClaudeEnvKeys:  cfg.ClaudeEnvKeys,
	})
	if err != nil {
		return CPAccept001Result{}, err
	}
	defer func() {
		_ = app.Close()
	}()
	state := &workflowState{
		app:    app,
		cfg:    cfg,
		driver: &driver{executor: cpruntime.DockerExecClient{}},
	}
	result := CPAccept001Result{Mode: "drive_workflow"}
	root, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "release-health",
		Title:         "CP-ACCEPT-001 release health",
		Objective:     "Drive the formal coordinator/developer/verifier Docker Claude workflow and evaluate durable evidence.",
		TargetAgentID: "coordinator",
		CompletionRequirements: []string{
			"report",
		},
	})
	if err != nil {
		return result, fmt.Errorf("create release-health root contract: %w", err)
	}
	state.root = root
	result.RootContractID = root.ContractID

	if err := ensureDockerNetwork(ctx, cfg.DockerNetwork); err != nil {
		acceptance, evalErr := evaluateRoot(ctx, app, cfg, root.ContractID)
		result.Acceptance = acceptance
		if inspect, inspectErr := app.Inspect(ctx); inspectErr == nil {
			result.Inspect = &inspect
		}
		if evalErr != nil {
			return result, fmt.Errorf("%w; release acceptance evaluation failed: %v", err, evalErr)
		}
		return result, err
	}

	server, err := startBackendServer(ctx, cfg.ListenAddr, app.Handler)
	if err != nil {
		acceptance, evalErr := evaluateRoot(ctx, app, cfg, root.ContractID)
		result.Acceptance = acceptance
		if inspect, inspectErr := app.Inspect(ctx); inspectErr == nil {
			result.Inspect = &inspect
		}
		if evalErr != nil {
			return result, fmt.Errorf("start backend server: %w; release acceptance evaluation failed: %v", err, evalErr)
		}
		return result, fmt.Errorf("start backend server: %w", err)
	}
	defer func() {
		_ = server.Shutdown(context.Background())
	}()

	runErr := state.run(ctx)
	acceptance, evalErr := evaluateRoot(ctx, app, cfg, root.ContractID)
	result.Acceptance = acceptance
	inspect, inspectErr := app.Inspect(ctx)
	if inspectErr == nil {
		result.Inspect = &inspect
	}
	cleanupErr := cleanupManagedContainers(ctx, cfg.TeamID)
	switch {
	case runErr != nil:
		if evalErr != nil {
			return result, fmt.Errorf("%w; release acceptance evaluation failed: %v", runErr, evalErr)
		}
		return result, runErr
	case evalErr != nil:
		return result, evalErr
	case cleanupErr != nil:
		return result, cleanupErr
	default:
		return result, statusError(acceptance)
	}
}

func (s *workflowState) run(ctx context.Context) error {
	coordinator, err := s.startAgent(ctx, "coordinator")
	if err != nil {
		return environmentBlockedError{Message: "coordinator Docker/Claude session could not start", Cause: err}
	}
	s.coordinator = coordinator
	if _, err := s.driver.call(ctx, coordinator, "contract.current", map[string]any{}, "rh-coordinator-current"); err != nil {
		return err
	}
	developerWork, err := callData[coordination.AddContractResult](ctx, s.driver, coordinator, "contract.add", map[string]any{
		"title":                   "CP-ACCEPT-001 developer evidence",
		"objective":               "Produce command.run, report, and controlled Git changeset evidence for release health.",
		"target_agent_id":         "developer",
		"completion_requirements": []string{"report", "command_run"},
	}, "rh-add-developer")
	if err != nil {
		return err
	}
	s.developerWork = developerWork
	verifierWork, err := callData[coordination.AddContractResult](ctx, s.driver, coordinator, "contract.add", map[string]any{
		"title":                   "CP-ACCEPT-001 verifier assessment",
		"objective":               "Assess the developer evidence and record canonical validation.assessment.",
		"target_agent_id":         "verifier",
		"completion_requirements": []string{"validation_assessment"},
	}, "rh-add-verifier")
	if err != nil {
		return err
	}
	s.verifierWork = verifierWork
	if _, err := callData[coordination.Assignment](ctx, s.driver, coordinator, "contract.wait", map[string]any{
		"reason":           "waiting for developer evidence before release acceptance",
		"waiting_for_ref":  "contract:" + developerWork.ContractID,
		"session_route_id": coordinator.Route.ID,
	}, "rh-root-wait-developer"); err != nil {
		return err
	}
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: coordinator.AttemptID,
		Status:    "waiting",
		Summary:   "waiting for release-health child evidence",
	}); err != nil {
		return fmt.Errorf("mark coordinator waiting: %w", err)
	}

	developer, err := s.startAgent(ctx, "developer")
	if err != nil {
		return environmentBlockedError{Message: "developer Docker/Claude session could not start", Cause: err}
	}
	s.developer = developer
	commandRun, err := callData[commandRunData](ctx, s.driver, developer, "command.run", map[string]any{
		"argv":             []string{"sh", "-lc", "printf 'cp-accept-001 command.run ok\\n' && test -w /workspace/project && test -w /home/agent"},
		"timeout_seconds":  30,
		"max_output_bytes": 4096,
		"purpose":          "release-health evidence",
	}, "rh-command-run")
	if err != nil {
		return err
	}
	s.commandRun = commandRun
	report, err := callData[coordination.Evidence](ctx, s.driver, developer, "report.submit", map[string]any{
		"summary": "developer produced command.run and controlled Git release-health evidence",
		"content": "Developer release-health report: command_run=" + commandRun.CommandRunID,
	}, "rh-developer-report")
	if err != nil {
		return err
	}
	s.developerRpt = report
	changeset, err := s.prepareChangeset(ctx)
	if err != nil {
		return err
	}
	s.changeSet = changeset
	if _, err := s.driver.call(ctx, developer, "contract.complete", map[string]any{
		"evidence_ids": []string{report.ID, commandRun.EvidenceID},
		"summary":      "developer release-health evidence complete",
	}, "rh-developer-complete"); err != nil {
		return err
	}
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: developer.AttemptID,
		Status:    "completed",
		Summary:   "developer workflow completed",
	}); err != nil {
		return fmt.Errorf("finish developer session: %w", err)
	}
	mailboxID, err := firstPendingMailbox(ctx, s.app.DB, "coordinator", s.root.ContractID)
	if err != nil {
		return fmt.Errorf("find coordinator child-completed mailbox: %w", err)
	}
	if _, err := s.app.Delivery.NotifyMailbox(ctx, mailboxID); err != nil {
		return fmt.Errorf("notify coordinator mailbox: %w", err)
	}
	resumed, err := s.app.Runner.ProcessResumeQueue(ctx, "release-health")
	if err != nil {
		return fmt.Errorf("process coordinator resume queue: %w", err)
	}
	if strings.TrimSpace(resumed.Env["COORDPLANE_TOKEN"]) != "" {
		coordinator.Env = resumed.Env
		s.coordinator.Env = resumed.Env
	}
	if _, err := s.driver.call(ctx, coordinator, "mailbox.resolve", map[string]any{
		"mailbox_id":   mailboxID,
		"followup_ref": "contract:" + s.verifierWork.ContractID,
	}, "rh-coordinator-mailbox-resolve"); err != nil {
		return err
	}

	verifier, err := s.startAgent(ctx, "verifier")
	if err != nil {
		return environmentBlockedError{Message: "verifier Docker/Claude session could not start", Cause: err}
	}
	s.verifier = verifier
	assessment, err := callData[validationData](ctx, s.driver, verifier, "validation.assessment", map[string]any{
		"assessed_contract_id": s.developerWork.ContractID,
		"verdict":              "pass",
		"reason":               "release-health durable command, changeset, and report evidence were present",
		"summary":              "CP-ACCEPT-001 release-health validation passed",
		"checked_refs": []validation.CheckedRef{
			{Kind: "command_run", ID: commandRun.CommandRunID},
			{Kind: "changeset", ID: changeset.ChangeSet.ID},
			{Kind: "evidence", ID: report.ID},
		},
	}, "rh-validation-assessment")
	if err != nil {
		return err
	}
	s.validation = assessment
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: verifier.AttemptID,
		Status:    "completed",
		Summary:   "verifier assessment recorded",
	}); err != nil {
		return fmt.Errorf("finish verifier session: %w", err)
	}

	rootReport, err := callData[coordination.Evidence](ctx, s.driver, coordinator, "report.submit", map[string]any{
		"summary": "root workflow has command_run, changeset, mailbox resume, and validation evidence",
		"content": "Root release-health report: validation_assessment=" + assessment.AssessmentID,
	}, "rh-root-report")
	if err != nil {
		return err
	}
	s.rootReport = rootReport
	if _, err := s.driver.call(ctx, coordinator, "contract.complete", map[string]any{
		"evidence_ids": []string{rootReport.ID},
		"summary":      "CP-ACCEPT-001 release-health root complete",
	}, "rh-root-complete"); err != nil {
		return err
	}
	if _, err := s.app.Runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: coordinator.AttemptID,
		Status:    "completed",
		Summary:   "release-health root completed",
	}); err != nil {
		return fmt.Errorf("finish coordinator session: %w", err)
	}
	return nil
}

func (s *workflowState) startAgent(ctx context.Context, agentID string) (cpruntime.AssignmentSession, error) {
	session, err := s.app.Runner.StartNext(ctx, agentID)
	if err != nil {
		return cpruntime.AssignmentSession{}, err
	}
	if len(session.Env) == 0 || strings.TrimSpace(session.Env["COORDPLANE_TOKEN"]) == "" {
		return cpruntime.AssignmentSession{}, errors.New("runtime session did not return an in-memory coordlink token")
	}
	return session, nil
}

func (s *workflowState) prepareChangeset(ctx context.Context) (codemanagement.SubmitChangeSetResult, error) {
	repoPath, err := ensureGitRepository(ctx, filepath.Join(s.cfg.WorkDir, "repository"))
	if err != nil {
		return codemanagement.SubmitChangeSetResult{}, err
	}
	repo, err := s.app.CodeManagement.RegisterRepository(ctx, codemanagement.RegisterRepositoryInput{
		RepoPath:        repoPath,
		Alias:           "cp-accept-001",
		CanonicalBranch: "main",
	})
	if err != nil {
		return codemanagement.SubmitChangeSetResult{}, fmt.Errorf("register release-health repository: %w", err)
	}
	workspaceRoot := s.prepareChangesetWorkspaceRoot()
	prepared, err := callData[codemanagement.WorkspacePrepareResult](ctx, s.driver, s.developer, "workspace.prepare", map[string]any{
		"repo_id":          repo.ID,
		"canonical_branch": "main",
		"workspace_root":   workspaceRoot,
		"contract_id":      s.developerWork.ContractID,
	}, "rh-workspace-prepare")
	if err != nil {
		return codemanagement.SubmitChangeSetResult{}, err
	}
	filePath := filepath.Join(prepared.Workspace.Path, "release-health.txt")
	content := []byte("CP-ACCEPT-001 release-health controlled Git evidence\n")
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		return codemanagement.SubmitChangeSetResult{}, fmt.Errorf("write release-health workspace file: %w", err)
	}
	commit, err := callData[codemanagement.GitCommitResult](ctx, s.driver, s.developer, "git.commit", map[string]any{
		"workspace_id":      prepared.Workspace.ID,
		"message":           "Add CP-ACCEPT-001 release-health evidence",
		"paths":             []string{"release-health.txt"},
		"expected_head_ref": prepared.Workspace.HeadRef,
	}, "rh-git-commit")
	if err != nil {
		return codemanagement.SubmitChangeSetResult{}, err
	}
	changeset, err := callData[codemanagement.SubmitChangeSetResult](ctx, s.driver, s.developer, "changeset.submit", map[string]any{
		"workspace_id":      commit.Workspace.ID,
		"contract_id":       s.developerWork.ContractID,
		"summary":           "CP-ACCEPT-001 release-health changeset",
		"evidence_refs":     []string{s.commandRun.EvidenceID, s.developerRpt.ID},
		"expected_head_ref": commit.CommitSHA,
	}, "rh-changeset-submit")
	if err != nil {
		return codemanagement.SubmitChangeSetResult{}, err
	}
	return changeset, nil
}

func (s *workflowState) prepareChangesetWorkspaceRoot() string {
	if strings.TrimSpace(s.developer.ContainerName) != "" {
		if root := strings.TrimSpace(s.developer.Route.Workdir); root != "" {
			return root
		}
		return cpruntime.ContainerWorkspacePath
	}
	return filepath.Join(s.cfg.WorkDir, "git-workspaces")
}

func callData[T any](ctx context.Context, d *driver, session cpruntime.AssignmentSession, name string, input map[string]any, idempotencyKey string) (T, error) {
	var out T
	response, err := d.call(ctx, session, name, input, idempotencyKey)
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

func (d *driver) call(ctx context.Context, session cpruntime.AssignmentSession, name string, input map[string]any, idempotencyKey string) (typedResponse, error) {
	response, err := d.callAny(ctx, session, name, input, idempotencyKey)
	if err != nil {
		return typedResponse{}, err
	}
	if response.Status != capability.StatusAccepted {
		return response, fmt.Errorf("coordlink %s returned %s %s: %s", name, response.Status, response.ErrorCode, response.Message)
	}
	return response, nil
}

func (d *driver) callAny(ctx context.Context, session cpruntime.AssignmentSession, name string, input map[string]any, idempotencyKey string) (typedResponse, error) {
	if d.executor == nil {
		d.executor = cpruntime.DockerExecClient{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return typedResponse{}, err
	}
	command := []string{cpruntime.ContainerCoordlinkPath, "call", name, "--input", string(raw)}
	if session.LeaseID != "" {
		command = append(command, "--lease-id", session.LeaseID)
	}
	if idempotencyKey != "" {
		command = append(command, "--idempotency-key", idempotencyKey)
	}
	result, err := d.executor.Exec(ctx, cpruntime.ContainerExecSpec{
		ContainerName: session.ContainerName,
		Workdir:       cpruntime.ContainerWorkspacePath,
		HomeDir:       cpruntime.ContainerHomePath,
		Env:           session.Env,
		Command:       command,
		Timeout:       defaultCoordlinkTTL,
	})
	if err != nil {
		return typedResponse{}, fmt.Errorf("exec coordlink %s: %w: %s", name, err, strings.TrimSpace(string(result.Stderr)))
	}
	var response typedResponse
	if err := json.Unmarshal(result.Stdout, &response); err != nil {
		return typedResponse{}, fmt.Errorf("decode coordlink %s response: exit=%d stdout=%s stderr=%s: %w", name, result.ExitCode, strings.TrimSpace(string(result.Stdout)), strings.TrimSpace(string(result.Stderr)), err)
	}
	if result.ExitCode != 0 && response.Status != capability.StatusRejected {
		return response, fmt.Errorf("coordlink %s exited %d: %s", name, result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	return response, nil
}

func evaluateRoot(ctx context.Context, app *backend.Backend, cfg CPAccept001Config, rootContractID string) (releaseacceptance.Acceptance, error) {
	return app.EvaluateReleaseAcceptance(ctx, releaseacceptance.EvaluateInput{
		RootContractID: rootContractID,
		TeamID:         cfg.TeamID,
		TeamVersion:    cfg.TeamVersion,
		RunLabel:       cfg.RunLabel,
		CreatedBy:      cfg.CreatedBy,
	})
}

func statusError(acceptance releaseacceptance.Acceptance) error {
	if acceptance.Status != "passed" {
		return fmt.Errorf("coordplane release-health: %s status is %s", ScenarioCPAccept001, acceptance.Status)
	}
	for _, predicate := range acceptance.PredicateResults {
		if predicate.Required && predicate.Status != "passed" {
			return fmt.Errorf("coordplane release-health: required predicate %s is %s", predicate.Name, predicate.Status)
		}
	}
	return nil
}

func startBackendServer(ctx context.Context, listenAddr string, handler http.Handler) (*http.Server, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	select {
	case err := <-errCh:
		_ = listener.Close()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return server, nil
		}
		return nil, err
	case <-ctx.Done():
		_ = listener.Close()
		return nil, ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return server, nil
	}
}

func ensureDockerNetwork(ctx context.Context, network string) error {
	if strings.TrimSpace(network) == "" {
		return nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return environmentBlockedError{Message: "docker CLI is required for release-health workflow mode", Cause: err}
	}
	inspect := exec.CommandContext(ctx, "docker", "network", "inspect", network)
	if raw, err := inspect.CombinedOutput(); err == nil {
		_ = raw
		return nil
	}
	create := exec.CommandContext(ctx, "docker", "network", "create", network)
	raw, err := create.CombinedOutput()
	if err != nil {
		return environmentBlockedError{Message: "Docker network could not be created; check Docker daemon/registry environment", Cause: fmt.Errorf("%w: %s", err, strings.TrimSpace(string(raw)))}
	}
	return nil
}

func cleanupManagedContainers(ctx context.Context, teamID string) error {
	if strings.TrimSpace(teamID) == "" {
		return nil
	}
	ps := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=coordplane.managed=true",
		"--filter", "label=coordplane.team_id="+teamID)
	raw, err := ps.Output()
	if err != nil {
		return environmentBlockedError{Message: "managed Docker container cleanup query failed", Cause: err}
	}
	ids := strings.Fields(string(raw))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	rm := exec.CommandContext(ctx, "docker", args...)
	if raw, err := rm.CombinedOutput(); err != nil {
		return environmentBlockedError{Message: "managed Docker container cleanup failed", Cause: fmt.Errorf("%w: %s", err, strings.TrimSpace(string(raw)))}
	}
	return nil
}

func ensureGitRepository(ctx context.Context, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		return abs, nil
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	if err := runGit(ctx, abs, "init"); err != nil {
		return "", err
	}
	if err := runGit(ctx, abs, "checkout", "-B", "main"); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(abs, "README.md"), []byte("# CP-ACCEPT-001 release-health repo\n"), 0o644); err != nil {
		return "", err
	}
	if err := runGit(ctx, abs, "add", "README.md"); err != nil {
		return "", err
	}
	if err := runGit(ctx, abs, "-c", "user.name=CoordPlane", "-c", "user.email=coordplane@example.invalid", "commit", "-m", "Initial release-health repository"); err != nil {
		return "", err
	}
	return abs, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(raw)))
	}
	return nil
}

func firstPendingMailbox(ctx context.Context, db *sql.DB, agentID, contractID string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `
SELECT id
FROM mailbox_items
WHERE recipient_agent_id = ?
  AND contract_id = ?
  AND state = 'pending'
ORDER BY created_at ASC
LIMIT 1`, agentID, contractID).Scan(&id)
	return id, err
}
