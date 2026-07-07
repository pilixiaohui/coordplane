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

type CommandCLIProfile struct {
	Name       string
	Backend    string
	Binary     string
	StartArgs  []string
	ResumeArgs []string
	Timeout    time.Duration
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

func (a *CommandCLIAdapter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if req.CLIBackend != "" && req.CLIBackend != a.profile.Backend {
		return StartResult{}, fmt.Errorf("command cli: request backend %q does not match profile %q", req.CLIBackend, a.profile.Backend)
	}
	if err := validateCommandRuntime(req.RuntimeID, req.AttemptID, req.AgentID, req.Workspace, req.HomeDir, req.Env); err != nil {
		return StartResult{}, err
	}
	instance, err := a.runtimeInstance(ctx, req.RuntimeID, req.AttemptID)
	if err != nil {
		return StartResult{}, err
	}
	sessionID, err := newNativeSessionID()
	if err != nil {
		return StartResult{}, err
	}
	prompt := composeStartPrompt(req.BootstrapPrompt)
	command, persistedCommand, _ := renderCommand(a.profile.Binary, a.profile.StartArgs, renderVars{
		SessionID: sessionID,
		Prompt:    prompt,
	})
	sessionRow, err := a.insertSession(ctx, commandSessionInput{
		AttemptID:       req.AttemptID,
		RuntimeID:       req.RuntimeID,
		AgentID:         req.AgentID,
		SessionNativeID: sessionID,
		ContainerID:     instance.ContainerID,
		ContainerName:   instance.ContainerName,
		StartReason:     "start",
		State:           "starting",
		Command:         persistedCommand,
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
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
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
	if result.ExitCode != 0 {
		cause := fmt.Errorf("command cli: %s exited with code %d", a.profile.Backend, result.ExitCode)
		_ = a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, cause)
		return StartResult{}, cause
	}
	if err := a.markSessionExited(ctx, sessionRow.ID, result, transcriptRef, nil); err != nil {
		return StartResult{}, err
	}
	return StartResult{SessionNativeID: sessionID, TranscriptRef: transcriptRef}, nil
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
		_ = a.markSessionFailed(ctx, sessionRow.ID, err)
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
	if result.ExitCode != 0 {
		cause := fmt.Errorf("command cli: %s resume exited with code %d", a.profile.Backend, result.ExitCode)
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
		return ContainerExecResult{}, err
	}
	return result, nil
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

func (a *CommandCLIAdapter) authFailureCause(result ContainerExecResult) (error, bool) {
	if a.profile.Backend != "claude" || result.ExitCode == 0 {
		return nil, false
	}
	if looksLikeClaudeAuthFailure(result.Stdout) || looksLikeClaudeAuthFailure(result.Stderr) {
		return errors.New("CLAUDE_AUTH_REQUIRED: claude authentication required for non-interactive command CLI"), true
	}
	return nil, false
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
		"Use /usr/local/bin/coordlink mailbox list/get to read any mailbox body; this signal intentionally omits mailbox content.\n"
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
			return ContainerExecResult{ProcessRef: "docker-exec:" + spec.ContainerName, ExitCode: exitCode, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, fmt.Errorf("docker exec timed out after %s: %w", spec.Timeout, ctx.Err())
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
