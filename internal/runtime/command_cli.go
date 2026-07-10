package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"coordplane/internal/ids"
	"coordplane/internal/objects"
	"coordplane/internal/store"
)

const defaultCommandCLITimeout = 2 * time.Minute

const (
	providerPolicyModeEnv       = "COORDPLANE_PROVIDER_POLICY_MODE"
	providerPolicyModeStrict    = "strict_coordlink_call"
	providerPolicyAllowlistEnv  = "COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES"
	providerPolicyAuditTraceEnv = "COORDPLANE_PROVIDER_AUDIT_TRACE_ID"
	providerPolicyAuditAgentEnv = "COORDPLANE_PROVIDER_AUDIT_AGENT_ID"
	providerPolicyAuditLeaseEnv = "COORDPLANE_PROVIDER_AUDIT_LEASE_ID"
)

type CommandCLIProfile struct {
	Name                   string
	Backend                string
	Binary                 string
	StartArgs              []string
	ResumeArgs             []string
	Timeout                time.Duration
	RuntimeCommandPolicies map[string]RuntimeCommandPolicy
}

type CommandCLIAdapterConfig struct {
	Store    *store.Store
	Profile  CommandCLIProfile
	Executor ContainerExecutor
}

type CommandCLIAdapter struct {
	db       *sql.DB
	objects  *objects.Store
	profile  CommandCLIProfile
	executor ContainerExecutor
}

type preparedStartCommand struct {
	Instance         RuntimeInstance
	SessionNativeID  string
	Prompt           string
	Command          []string
	PersistedCommand []string
}

type ContainerExecutor interface {
	Exec(context.Context, ContainerExecSpec) (ContainerExecResult, error)
}

type ContainerExecSpec struct {
	ContainerName string
	Workdir       string
	HomeDir       string
	Env           map[string]string
	Command       []string
	Stdin         string
	Timeout       time.Duration
}

type ContainerExecResult struct {
	ProcessRef string
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
}

func NewClaudeCommandCLIAdapter(cfg CommandCLIAdapterConfig) (*CommandCLIAdapter, error) {
	profile := cfg.Profile
	if profile.Name == "" {
		profile.Name = "claude"
	}
	if profile.Backend == "" {
		profile.Backend = "claude"
	}
	if len(profile.StartArgs) == 0 {
		profile.StartArgs = []string{"--session-id", "{{session_id}}", "--print"}
	}
	if len(profile.ResumeArgs) == 0 {
		profile.ResumeArgs = []string{"--resume", "{{session_id}}", "--print"}
	}
	if profile.Timeout <= 0 {
		profile.Timeout = defaultCommandCLITimeout
	}
	if cfg.Executor == nil {
		cfg.Executor = DockerExecClient{}
	}
	adapter := &CommandCLIAdapter{
		profile:  profile,
		executor: cfg.Executor,
	}
	if cfg.Store != nil {
		adapter.db = cfg.Store.DB()
		adapter.objects = objects.NewStore(cfg.Store)
	}
	if err := adapter.validateProfile(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (a *CommandCLIAdapter) Capabilities() CLIAdapterCapabilities {
	return CLIAdapterCapabilities{SupportsSameTurnSteer: false}
}

func (a *CommandCLIAdapter) PreflightStart(ctx context.Context, req StartRequest) error {
	_, err := a.prepareStartCommand(ctx, req)
	return err
}

func (a *CommandCLIAdapter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	prepared, err := a.prepareStartCommand(ctx, req)
	if err != nil {
		return StartResult{}, err
	}
	instance := prepared.Instance
	sessionID := prepared.SessionNativeID
	prompt := prepared.Prompt
	command := prepared.Command
	sessionRow, err := a.insertSession(ctx, commandSessionInput{
		AttemptID:       req.AttemptID,
		RuntimeID:       req.RuntimeID,
		AgentID:         req.AgentID,
		SessionNativeID: sessionID,
		ContainerID:     instance.ContainerID,
		ContainerName:   instance.ContainerName,
		StartReason:     "start",
		State:           "starting",
		Command:         prepared.PersistedCommand,
		EnvKeys:         commandEnvKeys(req.Env),
	})
	if err != nil {
		return StartResult{}, err
	}
	if strings.TrimSpace(req.BootstrapPrompt) == "" {
		cause := errors.New("command cli: bootstrap prompt is required")
		_ = a.markSessionFailed(ctx, sessionRow.ID, cause)
		return StartResult{}, cause
	}
	if err := a.requireAuthProbe(instance); err != nil {
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
		return StartResult{}, err
	}
	result, err := a.exec(ctx, sessionRow.ID, instance, command, req.Env, prompt)
	if err != nil {
		_ = a.markExecError(ctx, sessionRow.ID, req.AttemptID, result, err)
		return StartResult{}, err
	}
	if cause, ok := a.authFailureCause(result); ok {
		transcriptRef := a.failureTranscriptRef(ctx, sessionRow.ID, cause)
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return StartResult{}, cause
	}
	transcriptRef, err := a.persistTranscript(ctx, req.AttemptID, result)
	if err != nil {
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
		return StartResult{}, err
	}
	if cause, ok := a.approvalFailureCause(instance, result); ok {
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return StartResult{}, cause
	}
	if result.ExitCode != 0 {
		cause := fmt.Errorf("command cli: %s exited with code %d", a.profile.Backend, result.ExitCode)
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return StartResult{}, cause
	}
	if cause := a.requireProviderPolicyProgress(ctx, instance, command, req.Env); cause != nil {
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return StartResult{}, cause
	}
	if err := a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, nil); err != nil {
		return StartResult{}, err
	}
	return StartResult{SessionNativeID: sessionID, TranscriptRef: transcriptRef}, nil
}

func (a *CommandCLIAdapter) prepareStartCommand(ctx context.Context, req StartRequest) (preparedStartCommand, error) {
	if req.CLIBackend != "" && req.CLIBackend != a.profile.Backend {
		return preparedStartCommand{}, fmt.Errorf("command cli: request backend %q does not match profile %q", req.CLIBackend, a.profile.Backend)
	}
	if err := validateCommandRuntime(req.RuntimeID, req.AttemptID, req.AgentID, req.Workspace, req.HomeDir, req.Env); err != nil {
		return preparedStartCommand{}, err
	}
	instance, err := a.runtimeInstance(ctx, req.RuntimeID, req.AttemptID)
	if err != nil {
		return preparedStartCommand{}, err
	}
	sessionID := req.SessionNativeID
	if sessionID == "" {
		sessionID, err = newNativeSessionID()
		if err != nil {
			return preparedStartCommand{}, err
		}
	}
	prompt := composeStartPrompt(req.BootstrapPrompt)
	command, persistedCommand, _ := renderCommand(a.profile.Binary, a.profile.StartArgs, renderVars{
		SessionID: sessionID,
		Prompt:    prompt,
	})
	command, persistedCommand, err = a.withProviderCommandPolicy(instance, command, persistedCommand)
	if err != nil {
		return preparedStartCommand{}, err
	}
	if err := a.enforceCommandPolicy(instance, command); err != nil {
		return preparedStartCommand{}, err
	}
	return preparedStartCommand{
		Instance:         instance,
		SessionNativeID:  sessionID,
		Prompt:           prompt,
		Command:          command,
		PersistedCommand: persistedCommand,
	}, nil
}

func (a *CommandCLIAdapter) Resume(ctx context.Context, req ResumeRequest) error {
	if req.Route.CLIBackend != a.profile.Backend {
		return fmt.Errorf("command cli: route backend %q does not match profile %q", req.Route.CLIBackend, a.profile.Backend)
	}
	if err := validateCommandRuntime(req.Route.RuntimeID, req.Route.AttemptID, req.Route.AgentID, req.Route.Workdir, req.Route.HomeDir, req.Env); err != nil {
		return err
	}
	instance, err := a.runtimeInstance(ctx, req.Route.RuntimeID, req.Route.AttemptID)
	if err != nil {
		return err
	}
	prompt := composeResumePrompt(req)
	command, persistedCommand, _ := renderCommand(a.profile.Binary, a.profile.ResumeArgs, renderVars{
		SessionID: req.Route.SessionNativeID,
		Prompt:    prompt,
	})
	command, persistedCommand, err = a.withProviderCommandPolicy(instance, command, persistedCommand)
	if err != nil {
		return err
	}
	if err := a.enforceCommandPolicy(instance, command); err != nil {
		return err
	}
	resumeOf, _ := a.firstSessionID(ctx, req.Route.AttemptID, req.Route.SessionNativeID)
	sessionRow, err := a.insertSession(ctx, commandSessionInput{
		AttemptID:       req.Route.AttemptID,
		RuntimeID:       req.Route.RuntimeID,
		AgentID:         req.Route.AgentID,
		SessionNativeID: req.Route.SessionNativeID,
		ContainerID:     instance.ContainerID,
		ContainerName:   instance.ContainerName,
		StartReason:     "resume",
		ResumeOf:        resumeOf,
		State:           "resumed",
		Command:         persistedCommand,
		EnvKeys:         commandEnvKeys(req.Env),
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Reason) == "" && len(req.MailboxIDs) == 0 {
		cause := errors.New("command cli: resume prompt is required")
		_ = a.markSessionFailed(ctx, sessionRow.ID, cause)
		return cause
	}
	if err := a.requireAuthProbe(instance); err != nil {
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
		return err
	}
	result, err := a.exec(ctx, sessionRow.ID, instance, command, req.Env, prompt)
	if err != nil {
		_ = a.markExecError(ctx, sessionRow.ID, req.Route.AttemptID, result, err)
		return err
	}
	if cause, ok := a.authFailureCause(result); ok {
		transcriptRef := a.failureTranscriptRef(ctx, sessionRow.ID, cause)
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return cause
	}
	transcriptRef, err := a.persistTranscript(ctx, req.Route.AttemptID, result)
	if err != nil {
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
		return err
	}
	if cause, ok := a.approvalFailureCause(instance, result); ok {
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return cause
	}
	if result.ExitCode != 0 {
		cause := fmt.Errorf("command cli: %s resume exited with code %d", a.profile.Backend, result.ExitCode)
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return cause
	}
	if cause := a.requireProviderPolicyProgress(ctx, instance, command, req.Env); cause != nil {
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return cause
	}
	return a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, nil)
}

func (a *CommandCLIAdapter) Steer(ctx context.Context, req SteerRequest) error {
	return errors.New("command cli: same-turn steer is not supported; use fallback resume")
}

func (a *CommandCLIAdapter) Finish(ctx context.Context, report TerminalReport) error {
	if report.AttemptID == "" {
		return errors.New("command cli: finish attempt id is required")
	}
	now := formatTime(time.Now())
	return withTx(ctx, a.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE cli_sessions
SET state = 'finished', transcript_ref = COALESCE(NULLIF(?, ''), transcript_ref),
  ended_at = COALESCE(ended_at, ?), updated_at = ?
WHERE attempt_id = ? AND state <> 'finished'`,
			report.TranscriptRef, now, now, report.AttemptID,
		)
		if err != nil {
			return fmt.Errorf("finish cli session: %w", err)
		}
		_, err = appendEvent(ctx, tx, "cli.finished", "attempt", report.AttemptID, map[string]any{
			"status":         report.Status,
			"transcript_ref": report.TranscriptRef,
		})
		return err
	})
}

func (a *CommandCLIAdapter) validateProfile() error {
	if a == nil {
		return errors.New("command cli: nil adapter")
	}
	if a.db == nil || a.objects == nil {
		return errors.New("command cli: store is required")
	}
	if strings.TrimSpace(a.profile.Name) == "" || strings.TrimSpace(a.profile.Backend) == "" || strings.TrimSpace(a.profile.Binary) == "" {
		return errors.New("command cli: profile name, backend, and binary are required")
	}
	if !strings.HasPrefix(a.profile.Binary, "/") {
		return errors.New("command cli: profile binary must be an absolute container path")
	}
	if len(a.profile.StartArgs) == 0 || len(a.profile.ResumeArgs) == 0 {
		return errors.New("command cli: start and resume args are required")
	}
	return nil
}

func validateCommandRuntime(runtimeID, attemptID, agentID, workspace, home string, env map[string]string) error {
	if runtimeID == "" || attemptID == "" || agentID == "" {
		return errors.New("command cli: runtime, attempt, and agent identity are required")
	}
	if workspace != ContainerWorkspacePath || home != ContainerHomePath {
		return fmt.Errorf("command cli: real CLI must run in docker workspace %s and home %s", ContainerWorkspacePath, ContainerHomePath)
	}
	if env != nil {
		if err := ValidateRuntimeEnv(env); err != nil {
			return err
		}
	}
	return nil
}

func (a *CommandCLIAdapter) runtimeInstance(ctx context.Context, runtimeID, attemptID string) (RuntimeInstance, error) {
	var instance RuntimeInstance
	var checksJSON, envKeysJSON, createdAt, updatedAt string
	err := a.db.QueryRowContext(ctx, `
SELECT id, runtime_id, runtime_kind, runtime_profile, agent_id, attempt_id, lease_id,
  container_id, container_name, image, network, state, workspace_path, home_path,
  checks_json, env_keys_json, last_error, created_at, updated_at
FROM runtime_instances
WHERE runtime_id = ? AND attempt_id = ?`,
		runtimeID, attemptID,
	).Scan(
		&instance.ID,
		&instance.RuntimeID,
		&instance.RuntimeKind,
		&instance.RuntimeProfile,
		&instance.AgentID,
		&instance.AttemptID,
		&instance.LeaseID,
		&instance.ContainerID,
		&instance.ContainerName,
		&instance.Image,
		&instance.Network,
		&instance.State,
		&instance.WorkspacePath,
		&instance.HomePath,
		&checksJSON,
		&envKeysJSON,
		&instance.LastError,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return RuntimeInstance{}, fmt.Errorf("command cli: load docker runtime instance: %w", err)
	}
	if instance.RuntimeKind != "docker" || instance.State != "ready" || instance.ContainerName == "" {
		return RuntimeInstance{}, fmt.Errorf("command cli: runtime %s is %s/%s without ready docker container", runtimeID, instance.RuntimeKind, instance.State)
	}
	if instance.WorkspacePath != ContainerWorkspacePath || instance.HomePath != ContainerHomePath {
		return RuntimeInstance{}, fmt.Errorf("command cli: runtime paths are %s/%s, want container paths", instance.WorkspacePath, instance.HomePath)
	}
	if checksJSON != "" {
		if err := json.Unmarshal([]byte(checksJSON), &instance.Checks); err != nil {
			return RuntimeInstance{}, err
		}
	}
	if envKeysJSON != "" {
		if err := json.Unmarshal([]byte(envKeysJSON), &instance.EnvKeys); err != nil {
			return RuntimeInstance{}, err
		}
	}
	var errParse error
	instance.CreatedAt, errParse = parseTime(createdAt)
	if errParse != nil {
		return RuntimeInstance{}, errParse
	}
	instance.UpdatedAt, errParse = parseTime(updatedAt)
	if errParse != nil {
		return RuntimeInstance{}, errParse
	}
	return instance, nil
}

type commandSessionInput struct {
	AttemptID       string
	RuntimeID       string
	AgentID         string
	SessionNativeID string
	ContainerID     string
	ContainerName   string
	StartReason     string
	ResumeOf        string
	State           string
	Command         []string
	EnvKeys         []string
}

func (a *CommandCLIAdapter) insertSession(ctx context.Context, in commandSessionInput) (CLISession, error) {
	sessionID, err := ids.New("cli")
	if err != nil {
		return CLISession{}, err
	}
	commandJSON, err := json.Marshal(in.Command)
	if err != nil {
		return CLISession{}, err
	}
	envKeysJSON, err := json.Marshal(in.EnvKeys)
	if err != nil {
		return CLISession{}, err
	}
	now := formatTime(time.Now())
	err = withTx(ctx, a.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cli_sessions (
  id, tenant_id, attempt_id, runtime_id, agent_id, cli_backend, profile_name,
  session_native_id, container_id, container_name, state, start_reason, resume_of,
  command_json, env_keys_json, started_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, in.AttemptID, in.RuntimeID, in.AgentID, a.profile.Backend,
			a.profile.Name, in.SessionNativeID, in.ContainerID, in.ContainerName,
			in.State, in.StartReason, in.ResumeOf, string(commandJSON), string(envKeysJSON),
			now, now,
		); err != nil {
			return fmt.Errorf("insert cli session: %w", err)
		}
		eventType := "cli.start_requested"
		if in.StartReason == "resume" {
			eventType = "cli.resumed"
		}
		_, err := appendEvent(ctx, tx, eventType, "cli_session", sessionID, map[string]any{
			"attempt_id":        in.AttemptID,
			"runtime_id":        in.RuntimeID,
			"agent_id":          in.AgentID,
			"cli_backend":       a.profile.Backend,
			"session_native_id": in.SessionNativeID,
			"container_name":    in.ContainerName,
			"env_keys":          in.EnvKeys,
		})
		return err
	})
	if err != nil {
		return CLISession{}, err
	}
	return CLISession{
		ID:              sessionID,
		AttemptID:       in.AttemptID,
		RuntimeID:       in.RuntimeID,
		AgentID:         in.AgentID,
		CLIBackend:      a.profile.Backend,
		ProfileName:     a.profile.Name,
		SessionNativeID: in.SessionNativeID,
		ContainerID:     in.ContainerID,
		ContainerName:   in.ContainerName,
		State:           in.State,
		StartReason:     in.StartReason,
		ResumeOf:        in.ResumeOf,
		Command:         append([]string(nil), in.Command...),
		EnvKeys:         append([]string(nil), in.EnvKeys...),
		StartedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (a *CommandCLIAdapter) exec(ctx context.Context, sessionID string, instance RuntimeInstance, command []string, env map[string]string, stdin string) (ContainerExecResult, error) {
	if len(command) == 0 {
		return ContainerExecResult{}, errors.New("command cli: empty command")
	}
	execEnv := make(map[string]string, len(env)+1)
	for key, value := range env {
		execEnv[key] = value
	}
	execEnv["HOME"] = ContainerHomePath
	if err := a.addProviderPolicyEnv(instance, command, execEnv); err != nil {
		return ContainerExecResult{}, err
	}
	if err := a.markProcessStarted(ctx, sessionID, command, commandEnvKeys(execEnv)); err != nil {
		return ContainerExecResult{}, err
	}
	result, err := a.executor.Exec(ctx, ContainerExecSpec{
		ContainerName: instance.ContainerName,
		Workdir:       ContainerWorkspacePath,
		HomeDir:       ContainerHomePath,
		Env:           execEnv,
		Command:       command,
		Stdin:         stdin,
		Timeout:       a.profile.Timeout,
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func (a *CommandCLIAdapter) addProviderPolicyEnv(instance RuntimeInstance, command []string, env map[string]string) error {
	if a.profile.Backend != "claude" || isCoordlinkCommand(command) {
		return nil
	}
	policy, ok := a.runtimeCommandPolicy(instance.RuntimeProfile)
	if !ok {
		return nil
	}
	if _, err := claudeProviderPermissionArgs(policy); err != nil {
		return err
	}
	allowed := allowedProviderCapabilities(policy)
	if len(allowed) == 0 {
		return NewRuntimeApprovalPolicyUnavailable("claude command runtime provider policy has no safe coordlink allowlist")
	}
	env[providerPolicyModeEnv] = providerPolicyModeStrict
	env[providerPolicyAllowlistEnv] = strings.Join(allowed, ",")
	env[providerPolicyAuditTraceEnv] = env["COORDPLANE_TRACE_ID"]
	env[providerPolicyAuditAgentEnv] = instance.AgentID
	env[providerPolicyAuditLeaseEnv] = instance.LeaseID
	return nil
}

func (a *CommandCLIAdapter) requireAuthProbe(instance RuntimeInstance) error {
	if a.profile.Backend != "claude" {
		return nil
	}
	for _, name := range []string{"claude_present", "claude_auth_configured", "claude_auth_probe_passed"} {
		if !instance.Checks[name] {
			return fmt.Errorf("CLAUDE_AUTH_REQUIRED: command cli requires runtime check %s", name)
		}
	}
	return nil
}

func (a *CommandCLIAdapter) enforceCommandPolicy(instance RuntimeInstance, command []string) error {
	policy, ok := a.runtimeCommandPolicy(instance.RuntimeProfile)
	if !ok {
		return nil
	}
	if a.profile.Backend == "claude" && !isCoordlinkCommand(command) {
		providerArgs, err := claudeProviderPermissionArgs(policy)
		if err != nil {
			return err
		}
		if !commandContainsClaudeProviderPolicy(command, providerArgs) {
			return NewRuntimeApprovalPolicyUnavailable("claude command runtime command_policy was not converted to an auditable provider approval configuration")
		}
		return nil
	}
	return EvaluateCommandPolicy(command, policy)
}

func (a *CommandCLIAdapter) withProviderCommandPolicy(instance RuntimeInstance, command, persisted []string) ([]string, []string, error) {
	if a.profile.Backend != "claude" || isCoordlinkCommand(command) {
		return command, persisted, nil
	}
	policy, ok := a.runtimeCommandPolicy(instance.RuntimeProfile)
	if !ok {
		return command, persisted, nil
	}
	args, err := claudeProviderPermissionArgs(policy)
	if err != nil {
		return nil, nil, err
	}
	command = append(append([]string(nil), command...), args...)
	persisted = append(append([]string(nil), persisted...), args...)
	return command, persisted, nil
}

func claudeProviderPermissionArgs(policy RuntimeCommandPolicy) ([]string, error) {
	if !policy.NonInteractiveApproval || len(policy.AllowCoordlinkCapabilities) == 0 {
		return nil, NewRuntimeApprovalPolicyUnavailable("claude command runtime requires runtime profile command_policy with non_interactive_approval and coordlink allowlist")
	}
	allowed := allowedProviderCapabilities(policy)
	rules := make([]string, 0, len(allowed))
	for _, capabilityName := range allowed {
		if !safeProviderCapabilityName(capabilityName) {
			return nil, NewRuntimeApprovalPolicyUnavailable("claude command runtime cannot safely express command_policy capability in provider permissions")
		}
		rules = append(rules, "Bash("+ContainerCoordlinkPath+" call "+capabilityName+" *)")
	}
	rules = append(rules, providerBootstrapReadRules()...)
	sort.Strings(rules)
	denyRules := []string{
		"Bash(sh *)",
		"Bash(bash *)",
		"Bash(dash *)",
		"Bash(zsh *)",
		"Bash(fish *)",
		"Bash(env *)",
		"Bash(printenv *)",
		"Bash(cat *)",
		"Bash(head *)",
		"Bash(tail *)",
		"Bash(grep *)",
		"Bash(find *)",
		"Bash(ls *)",
		"Bash(pwd)",
		"Bash(stat *)",
		"Bash(sqlite3 *)",
		"Bash(psql *)",
		"Bash(mysql *)",
		"Bash(docker *)",
		"Bash(podman *)",
		"Bash(curl *)",
		"Bash(wget *)",
		"Bash(nc *)",
		"Bash(netcat *)",
		"Bash(ssh *)",
		"Bash(python *)",
		"Bash(python3 *)",
		"Bash(node *)",
	}
	sort.Strings(denyRules)
	return []string{
		"--safe-mode",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--tools", "Bash",
		"--allowedTools", strings.Join(rules, ","),
		"--disallowedTools", strings.Join(denyRules, ","),
	}, nil
}

func providerBootstrapReadRules() []string {
	return []string{
		"Bash(" + ContainerCoordlinkPath + " capability list)",
		"Bash(" + ContainerCoordlinkPath + " skill list)",
		"Bash(" + ContainerCoordlinkPath + " skill read contract-delegation)",
		"Bash(" + ContainerCoordlinkPath + " skill read controlled-git)",
		"Bash(" + ContainerCoordlinkPath + " skill read coordplane-service)",
	}
}

func allowedProviderCapabilities(policy RuntimeCommandPolicy) []string {
	seen := make(map[string]bool, len(policy.AllowCoordlinkCapabilities))
	out := make([]string, 0, len(policy.AllowCoordlinkCapabilities))
	for _, capabilityName := range policy.AllowCoordlinkCapabilities {
		capabilityName = strings.TrimSpace(capabilityName)
		if capabilityName == "" || seen[capabilityName] {
			continue
		}
		seen[capabilityName] = true
		out = append(out, capabilityName)
	}
	sort.Strings(out)
	return out
}

func safeProviderCapabilityName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return strings.Contains(name, ".")
}

func commandContainsClaudeProviderPolicy(command, providerArgs []string) bool {
	if len(providerArgs) == 0 {
		return false
	}
	for _, arg := range providerArgs {
		if !containsCommandArg(command, arg) {
			return false
		}
	}
	return !containsCommandArg(command, "bypassPermissions") &&
		!containsCommandArg(command, "--dangerously-skip-permissions") &&
		!containsCommandArg(command, "--allow-dangerously-skip-permissions")
}

func containsCommandArg(command []string, want string) bool {
	for _, arg := range command {
		if arg == want {
			return true
		}
	}
	return false
}

func (a *CommandCLIAdapter) requireProviderPolicyProgress(ctx context.Context, instance RuntimeInstance, command []string, env map[string]string) error {
	if a.profile.Backend != "claude" || isCoordlinkCommand(command) {
		return nil
	}
	policy, ok := a.runtimeCommandPolicy(instance.RuntimeProfile)
	if !ok {
		return nil
	}
	if _, err := claudeProviderPermissionArgs(policy); err != nil {
		return err
	}
	traceID := strings.TrimSpace(env["COORDPLANE_TRACE_ID"])
	if traceID == "" {
		return NewRuntimeApprovalPolicyUnavailable("claude command runtime provider policy could not audit accepted coordlink capability calls without trace id")
	}
	if strings.TrimSpace(instance.LeaseID) == "" {
		return NewRuntimeApprovalPolicyUnavailable("claude command runtime provider policy could not audit accepted coordlink capability calls without lease id")
	}
	allowed := make(map[string]bool, len(policy.AllowCoordlinkCapabilities))
	for _, capabilityName := range allowedProviderCapabilities(policy) {
		allowed[capabilityName] = true
	}
	rows, err := a.db.QueryContext(ctx, `
SELECT capability_name, scope_json
FROM capability_calls
WHERE trace_id = ? AND subject_kind = 'agent' AND subject_id = ? AND status = 'accepted'`,
		traceID, instance.AgentID,
	)
	if err != nil {
		return fmt.Errorf("command cli: audit accepted provider policy capability calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var capabilityName, scopeJSON string
		if err := rows.Scan(&capabilityName, &scopeJSON); err != nil {
			return err
		}
		if allowed[capabilityName] && scopeMatchesLease(scopeJSON, instance.LeaseID) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return NewRuntimeApprovalPolicyUnavailable("claude command runtime provider exited without an accepted allowlisted coordlink capability call")
}

func scopeMatchesLease(scopeJSON, leaseID string) bool {
	if strings.TrimSpace(leaseID) == "" {
		return false
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(scopeJSON), &values); err != nil {
		return false
	}
	value, _ := values["lease_id"].(string)
	return value == leaseID
}

func (a *CommandCLIAdapter) runtimeCommandPolicy(runtimeProfile string) (RuntimeCommandPolicy, bool) {
	if a == nil || len(a.profile.RuntimeCommandPolicies) == 0 {
		return RuntimeCommandPolicy{}, false
	}
	policy, ok := a.profile.RuntimeCommandPolicies[runtimeProfile]
	if !ok {
		return RuntimeCommandPolicy{}, true
	}
	return policy, true
}

func (a *CommandCLIAdapter) approvalFailureCause(instance RuntimeInstance, result ContainerExecResult) (error, bool) {
	if _, ok := a.runtimeCommandPolicy(instance.RuntimeProfile); !ok {
		return nil, false
	}
	if looksLikeInteractiveApprovalPrompt(result.Stdout) || looksLikeInteractiveApprovalPrompt(result.Stderr) {
		return NewRuntimeApprovalPolicyUnavailable("command CLI provider requested interactive approval for a runtime command"), true
	}
	return nil, false
}

func (a *CommandCLIAdapter) authFailureCause(result ContainerExecResult) (error, bool) {
	if a.profile.Backend != "claude" || result.ExitCode == 0 {
		return nil, false
	}
	if looksLikeClaudeAuthFailure(result.Stdout) || looksLikeClaudeAuthFailure(result.Stderr) {
		return errors.New("CLAUDE_AUTH_REQUIRED: claude authentication required for non-interactive command CLI"), true
	}
	return nil, false
}

func looksLikeInteractiveApprovalPrompt(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"do you want to proceed",
		"requires approval",
		"needs approval",
		"request approval",
		"approval required",
		"permission required",
		"allow this command",
		"approve this command",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (a *CommandCLIAdapter) markProcessStarted(ctx context.Context, sessionID string, command []string, envKeys []string) error {
	return withTx(ctx, a.db, func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
UPDATE cli_sessions SET state = 'running', updated_at = ? WHERE id = ?`,
			now, sessionID,
		); err != nil {
			return fmt.Errorf("mark cli process running: %w", err)
		}
		_, err := appendEvent(ctx, tx, "cli.process_started", "cli_session", sessionID, map[string]any{
			"command":  redactCommand(command),
			"env_keys": envKeys,
		})
		return err
	})
}

func (a *CommandCLIAdapter) markSessionExited(ctx context.Context, sessionID string, result ContainerExecResult, transcriptRef string, cause error) error {
	state := "exited"
	lastError := ""
	if cause != nil {
		state = "failed"
		lastError = cause.Error()
	}
	now := formatTime(time.Now())
	return withTx(ctx, a.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE cli_sessions
SET state = ?, process_ref = ?, exit_code = ?, last_error = ?, transcript_ref = ?,
  ended_at = ?, updated_at = ?
WHERE id = ?`,
			state, result.ProcessRef, result.ExitCode, lastError, transcriptRef, now, now, sessionID,
		); err != nil {
			return fmt.Errorf("mark cli session exited: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "cli.session_id_captured", "cli_session", sessionID, map[string]any{
			"transcript_ref": transcriptRef,
		}); err != nil {
			return err
		}
		eventType := "cli.exited"
		if cause != nil {
			eventType = "cli.failed"
		}
		_, err := appendEvent(ctx, tx, eventType, "cli_session", sessionID, map[string]any{
			"exit_code":      result.ExitCode,
			"process_ref":    result.ProcessRef,
			"transcript_ref": transcriptRef,
			"error":          lastError,
		})
		return err
	})
}

func (a *CommandCLIAdapter) markExecError(ctx context.Context, sessionID, attemptID string, result ContainerExecResult, cause error) error {
	if result.ProcessRef == "" && len(result.Stdout) == 0 && len(result.Stderr) == 0 {
		return a.markSessionFailed(ctx, sessionID, cause)
	}
	transcriptRef, err := a.persistTranscript(ctx, attemptID, result)
	if err != nil {
		transcriptRef = a.failureTranscriptRef(ctx, sessionID, cause)
	}
	return a.markSessionExited(ctx, sessionID, result, transcriptRef, cause)
}

func (a *CommandCLIAdapter) markSessionFailed(ctx context.Context, sessionID string, cause error) error {
	transcriptRef := a.failureTranscriptRef(ctx, sessionID, cause)
	now := formatTime(time.Now())
	return withTx(ctx, a.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE cli_sessions
SET state = 'failed', last_error = ?, transcript_ref = COALESCE(NULLIF(?, ''), transcript_ref),
  ended_at = ?, updated_at = ?
WHERE id = ?`,
			cause.Error(), transcriptRef, now, now, sessionID,
		); err != nil {
			return fmt.Errorf("mark cli session failed: %w", err)
		}
		_, err := appendEvent(ctx, tx, "cli.failed", "cli_session", sessionID, map[string]any{
			"error":          cause.Error(),
			"transcript_ref": transcriptRef,
		})
		return err
	})
}

func (a *CommandCLIAdapter) failureTranscriptRef(ctx context.Context, sessionID string, cause error) string {
	if a == nil || a.db == nil || a.objects == nil {
		return ""
	}
	var attemptID, existing string
	if err := a.db.QueryRowContext(ctx, `
SELECT attempt_id, COALESCE(transcript_ref, '')
FROM cli_sessions
WHERE id = ?`, sessionID).Scan(&attemptID, &existing); err != nil {
		return ""
	}
	if existing != "" {
		return existing
	}
	out, err := a.objects.PutTranscript(ctx, objects.PutTranscriptInput{
		AttemptID: attemptID,
		Content: []byte(
			"coordplane cli session failed before transcript capture\n" +
				"error: " + cause.Error() + "\n",
		),
		ContentType: "text/plain; charset=utf-8",
	})
	if err != nil {
		return ""
	}
	return out.ObjectRef
}

func (a *CommandCLIAdapter) persistTranscript(ctx context.Context, attemptID string, result ContainerExecResult) (string, error) {
	var transcript bytes.Buffer
	if len(result.Stdout) > 0 {
		transcript.WriteString("stdout:\n")
		transcript.Write(result.Stdout)
		if result.Stdout[len(result.Stdout)-1] != '\n' {
			transcript.WriteByte('\n')
		}
	}
	if len(result.Stderr) > 0 {
		transcript.WriteString("stderr:\n")
		transcript.Write(result.Stderr)
		if result.Stderr[len(result.Stderr)-1] != '\n' {
			transcript.WriteByte('\n')
		}
	}
	if transcript.Len() == 0 {
		transcript.WriteString("coordplane cli transcript: no output\n")
	}
	out, err := a.objects.PutTranscript(ctx, objects.PutTranscriptInput{
		AttemptID:   attemptID,
		Content:     transcript.Bytes(),
		ContentType: "text/plain; charset=utf-8",
	})
	if err != nil {
		return "", err
	}
	return out.ObjectRef, nil
}

func (a *CommandCLIAdapter) firstSessionID(ctx context.Context, attemptID, nativeID string) (string, error) {
	var id string
	err := a.db.QueryRowContext(ctx, `
SELECT id
FROM cli_sessions
WHERE attempt_id = ? AND session_native_id = ? AND start_reason = 'start'
ORDER BY started_at ASC, id ASC
LIMIT 1`,
		attemptID, nativeID,
	).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

type renderVars struct {
	SessionID string
	Prompt    string
}

func renderCommand(binary string, args []string, vars renderVars) ([]string, []string, bool) {
	command := []string{binary}
	persisted := []string{binary}
	promptPlaceholder := false
	for _, arg := range args {
		rendered := strings.ReplaceAll(arg, "{{session_id}}", vars.SessionID)
		persistedArg := rendered
		if strings.Contains(rendered, "{{prompt}}") {
			promptPlaceholder = true
			rendered = strings.TrimSpace(strings.ReplaceAll(rendered, "{{prompt}}", ""))
			persistedArg = rendered
			if rendered == "" {
				continue
			}
		}
		command = append(command, rendered)
		persisted = append(persisted, persistedArg)
	}
	return command, persisted, promptPlaceholder
}

func redactCommand(command []string) []string {
	out := append([]string(nil), command...)
	for i, arg := range out {
		if strings.Contains(arg, "\n") || len(arg) > 160 {
			out[i] = "<redacted-arg>"
		}
	}
	return out
}

func composeStartPrompt(bootstrap string) string {
	return strings.TrimSpace(bootstrap) + "\n\nCoordPlane runtime protocol:\n" +
		"- You are inside the Docker runtime at /workspace/project with HOME=/home/agent.\n" +
		"- Use /usr/local/bin/coordlink for all CoordPlane backend reads and writes.\n" +
		"- Capabilities must be invoked only as /usr/local/bin/coordlink call <capability>.\n" +
		"- Do not use shortcut forms such as /usr/local/bin/coordlink mailbox list, contract get, assignment get, or raw backend URLs.\n" +
		"- To inspect current work, call contract.current; to read mailbox, call mailbox.list and mailbox.get; to read context, call contract.context.\n" +
		"- Do not read host files, backend database paths, or runtime roots.\n"
}

func composeResumePrompt(req ResumeRequest) string {
	mailboxes := strings.Join(req.MailboxIDs, ",")
	if mailboxes == "" {
		mailboxes = "none"
	}
	return "Resume the existing CoordPlane session.\n" +
		"Reason: " + req.Reason + "\n" +
		"Pending mailbox ids: " + mailboxes + "\n" +
		"Resume handshake only: run exactly `/usr/local/bin/coordlink call contract.current`, then stop and summarize RESUME_HANDSHAKE_DONE.\n" +
		"Do not call mailbox.get, communication.read, mailbox.resolve, message.send, report.submit, contract.add, contract.complete, or raw backend URLs during resume.\n" +
		"This signal intentionally omits mailbox content; the canonical driver will process pending mailbox work after the resume handshake.\n"
}

func commandEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func newNativeSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate native session id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

type DockerExecClient struct {
	Binary string
}

func (c DockerExecClient) Exec(ctx context.Context, spec ContainerExecSpec) (ContainerExecResult, error) {
	if spec.ContainerName == "" || len(spec.Command) == 0 || spec.Workdir == "" || spec.HomeDir == "" {
		return ContainerExecResult{}, errors.New("docker exec: container, command, workdir, and home are required")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	binary := c.Binary
	if binary == "" {
		binary = "docker"
	}
	args := []string{"exec"}
	if spec.Stdin != "" {
		args = append(args, "-i")
	}
	args = append(args, "--workdir", spec.Workdir)
	envFile, err := newDockerEnvFile(spec.Env)
	if err != nil {
		return ContainerExecResult{}, err
	}
	defer envFile.Cleanup()
	args = envFile.AppendArgs(args)
	args = append(args, spec.ContainerName)
	args = append(args, spec.Command...)
	cmd := exec.CommandContext(ctx, binary, args...)
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if ctx.Err() != nil {
			return ContainerExecResult{ProcessRef: "docker-exec:" + spec.ContainerName, ExitCode: exitCode, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, NewRuntimeExecTimeout(fmt.Sprintf("docker exec timed out after %s", spec.Timeout), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return ContainerExecResult{ProcessRef: "docker-exec:" + spec.ContainerName, ExitCode: exitCode, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
		}
	}
	return ContainerExecResult{
		ProcessRef: "docker-exec:" + spec.ContainerName,
		ExitCode:   exitCode,
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
	}, nil
}

func ListCLISessions(ctx context.Context, db *sql.DB) ([]CLISession, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, attempt_id, runtime_id, agent_id, cli_backend, profile_name,
  session_native_id, container_id, container_name, process_ref, state,
  start_reason, resume_of, exit_code, last_error, transcript_ref,
  command_json, env_keys_json, started_at, COALESCE(ended_at, ''), updated_at
FROM cli_sessions
ORDER BY started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list cli sessions: %w", err)
	}
	defer rows.Close()
	var out []CLISession
	for rows.Next() {
		var session CLISession
		var exitCode sql.NullInt64
		var commandJSON, envKeysJSON, startedAt, endedAt, updatedAt string
		if err := rows.Scan(
			&session.ID,
			&session.AttemptID,
			&session.RuntimeID,
			&session.AgentID,
			&session.CLIBackend,
			&session.ProfileName,
			&session.SessionNativeID,
			&session.ContainerID,
			&session.ContainerName,
			&session.ProcessRef,
			&session.State,
			&session.StartReason,
			&session.ResumeOf,
			&exitCode,
			&session.LastError,
			&session.TranscriptRef,
			&commandJSON,
			&envKeysJSON,
			&startedAt,
			&endedAt,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		if exitCode.Valid {
			value := int(exitCode.Int64)
			session.ExitCode = &value
		}
		if commandJSON != "" {
			if err := json.Unmarshal([]byte(commandJSON), &session.Command); err != nil {
				return nil, err
			}
		}
		if envKeysJSON != "" {
			if err := json.Unmarshal([]byte(envKeysJSON), &session.EnvKeys); err != nil {
				return nil, err
			}
		}
		var err error
		session.StartedAt, err = parseTime(startedAt)
		if err != nil {
			return nil, err
		}
		if endedAt != "" {
			parsed, err := parseTime(endedAt)
			if err != nil {
				return nil, err
			}
			session.EndedAt = &parsed
		}
		session.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}
