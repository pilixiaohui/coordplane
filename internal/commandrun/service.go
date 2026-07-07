package commandrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/objects"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
)

const (
	defaultTimeoutSeconds = 30
	maxTimeoutSeconds     = 120
	defaultOutputBytes    = 1 << 20
	maxOutputBytes        = 4 << 20
	timeLayout            = "2006-01-02T15:04:05.000000000Z07:00"
)

type Service struct {
	db       *sql.DB
	store    *store.Store
	objects  *objects.Store
	executor cpruntime.ContainerExecutor
}

type Config struct {
	Store    *store.Store
	Executor cpruntime.ContainerExecutor
}

type Input struct {
	LeaseID        string            `json:"lease_id"`
	Argv           []string          `json:"argv"`
	CWD            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
	Purpose        string            `json:"purpose,omitempty"`
}

type Result struct {
	CommandRunID    string `json:"command_run_id"`
	ContractID      string `json:"contract_id"`
	AttemptID       string `json:"attempt_id"`
	RuntimeID       string `json:"runtime_id"`
	SessionRouteID  string `json:"session_route_id"`
	EvidenceID      string `json:"evidence_id,omitempty"`
	Status          string `json:"status"`
	ExitCode        int    `json:"exit_code"`
	StdoutRef       string `json:"stdout_ref"`
	StderrRef       string `json:"stderr_ref"`
	StdoutBytes     int    `json:"stdout_bytes"`
	StderrBytes     int    `json:"stderr_bytes"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	Summary         string `json:"summary"`
}

type Run struct {
	ID              string     `json:"id"`
	AgentID         string     `json:"agent_id"`
	LeaseID         string     `json:"lease_id"`
	AssignmentID    string     `json:"assignment_id"`
	ContractID      string     `json:"contract_id"`
	AttemptID       string     `json:"attempt_id"`
	SessionRouteID  string     `json:"session_route_id"`
	RuntimeID       string     `json:"runtime_id"`
	ContainerID     string     `json:"container_id,omitempty"`
	ContainerName   string     `json:"container_name,omitempty"`
	CWD             string     `json:"cwd"`
	Argv            []string   `json:"argv,omitempty"`
	EnvKeys         []string   `json:"env_keys,omitempty"`
	Status          string     `json:"status"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	StdoutRef       string     `json:"stdout_ref,omitempty"`
	StderrRef       string     `json:"stderr_ref,omitempty"`
	StdoutBytes     int        `json:"stdout_bytes"`
	StderrBytes     int        `json:"stderr_bytes"`
	StdoutTruncated bool       `json:"stdout_truncated"`
	StderrTruncated bool       `json:"stderr_truncated"`
	TimeoutSeconds  int        `json:"timeout_seconds"`
	DurationMS      int64      `json:"duration_ms"`
	EvidenceID      string     `json:"evidence_id,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type scope struct {
	AgentID        string
	LeaseID        string
	AssignmentID   string
	ContractID     string
	AttemptID      string
	RuntimeID      string
	SessionRouteID string
	ContainerID    string
	ContainerName  string
	WorkspacePath  string
	HomePath       string
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("command.run: store is required")
	}
	if cfg.Executor == nil {
		cfg.Executor = cpruntime.DockerExecClient{}
	}
	return &Service{
		db:       cfg.Store.DB(),
		store:    cfg.Store,
		objects:  objects.NewStore(cfg.Store),
		executor: cfg.Executor,
	}, nil
}

func RegisterCapabilities(registry *capability.Registry, service *Service) error {
	if registry == nil {
		return errors.New("command.run capabilities: registry is nil")
	}
	if service == nil {
		return errors.New("command.run capabilities: service is nil")
	}
	return registry.Register(capability.Definition{
		Name:           "command.run",
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		RejectedSchema: json.RawMessage(`{"type":"object"}`),
		SideEffect:     capability.SideEffectExternalExec,
		RequiredScope:  "agent_lease_session",
		Idempotency:    true,
		SkillRefs:      []string{"coordplane-service"},
	}, service.handleRun)
}

func (s *Service) handleRun(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input Input
	if err := decodeInputWithScope(call, &input); err != nil {
		return capability.Rejected[json.RawMessage](
			"INVALID_CAPABILITY_INPUT",
			"command.run input is invalid: "+err.Error(),
			capability.WithRepairHint("retry with lease_id, argv, optional cwd/env/timeout_seconds/max_output_bytes"),
			capability.WithAllowedNextActions("command.run"),
			capability.WithRetryable(false),
		)
	}
	result, response := s.Run(ctx, call.Subject, input, call.IdempotencyKey)
	if response.Status != "" {
		return response
	}
	accepted, err := capability.AcceptedJSON(result)
	if err != nil {
		return capability.Error[json.RawMessage]("COMMAND_RUN_RESPONSE_ENCODE_FAILED", err.Error(), false)
	}
	return accepted
}

func (s *Service) Run(ctx context.Context, subject capability.Subject, input Input, idempotencyKey string) (Result, capability.Response[json.RawMessage]) {
	agentID := agentIDFromSubject(subject)
	validated, reject := validateInput(input)
	if reject.Status != "" {
		return Result{}, reject
	}
	resolved, reject := s.resolveScope(ctx, agentID, subject.RuntimeID, input.LeaseID)
	if reject.Status != "" {
		return Result{}, reject
	}
	if idempotencyKey != "" {
		existing, ok, err := s.commandRunByIdempotency(ctx, resolved, idempotencyKey)
		if err != nil {
			return Result{}, capability.Error[json.RawMessage]("COMMAND_RUN_IDEMPOTENCY_LOOKUP_FAILED", err.Error(), true)
		}
		if ok {
			return resultFromRun(existing), capability.Response[json.RawMessage]{}
		}
	}

	runID, err := s.insertRunning(ctx, resolved, validated, idempotencyKey)
	if err != nil {
		return Result{}, capability.Error[json.RawMessage]("COMMAND_RUN_RECORD_FAILED", err.Error(), true)
	}
	started := time.Now()
	execCtx := ctx
	cancel := func() {}
	if validated.TimeoutSeconds > 0 {
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(validated.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	result, execErr := s.executor.Exec(execCtx, cpruntime.ContainerExecSpec{
		ContainerName: resolved.ContainerName,
		Workdir:       validated.Workdir,
		HomeDir:       cpruntime.ContainerHomePath,
		Env:           cloneStringMap(validated.Env),
		Command:       append([]string(nil), validated.Argv...),
		Timeout:       time.Duration(validated.TimeoutSeconds) * time.Second,
	})
	status := "succeeded"
	lastError := ""
	exitCode := result.ExitCode
	if execErr != nil {
		status = "failed"
		lastError = execErr.Error()
		exitCode = -1
		if errors.Is(execErr, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			status = "timed_out"
		}
	} else if result.ExitCode != 0 {
		status = "failed"
	}
	stdout, stdoutTruncated := capBytes(result.Stdout, validated.MaxOutputBytes)
	stderr, stderrTruncated := capBytes(result.Stderr, validated.MaxOutputBytes)
	completed, err := s.completeRun(ctx, runID, resolved, completion{
		Status:          status,
		ExitCode:        exitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated,
		DurationMS:      time.Since(started).Milliseconds(),
		LastError:       lastError,
	})
	if err != nil {
		return Result{}, capability.Error[json.RawMessage]("COMMAND_RUN_COMPLETE_FAILED", err.Error(), true)
	}
	return resultFromRun(completed), capability.Response[json.RawMessage]{}
}

type validatedInput struct {
	Argv           []string
	Workdir        string
	CWD            string
	Env            map[string]string
	EnvKeys        []string
	TimeoutSeconds int
	MaxOutputBytes int
}

func validateInput(in Input) (validatedInput, capability.Response[json.RawMessage]) {
	if strings.TrimSpace(in.LeaseID) == "" {
		return validatedInput{}, reject("COMMAND_LEASE_REQUIRED", "command.run requires lease_id", "retry with the active lease_id for the current assignment")
	}
	argv, err := validateArgv(in.Argv)
	if err != nil {
		return validatedInput{}, reject("COMMAND_ARGV_REJECTED", err.Error(), "use an argv array with an allowed executable and bounded arguments")
	}
	workdir, cwd, err := normalizeCWD(in.CWD)
	if err != nil {
		return validatedInput{}, reject("COMMAND_CWD_REJECTED", err.Error(), "use cwd relative to /workspace/project")
	}
	env, envKeys, err := validateEnv(in.Env)
	if err != nil {
		return validatedInput{}, reject("COMMAND_ENV_REJECTED", err.Error(), "remove forbidden env keys and retry")
	}
	timeout := in.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}
	if timeout > maxTimeoutSeconds {
		return validatedInput{}, reject("COMMAND_TIMEOUT_REJECTED", "timeout_seconds exceeds maximum", "use a shorter timeout")
	}
	outputBytes := in.MaxOutputBytes
	if outputBytes <= 0 {
		outputBytes = defaultOutputBytes
	}
	if outputBytes > maxOutputBytes {
		return validatedInput{}, reject("COMMAND_OUTPUT_LIMIT_REJECTED", "max_output_bytes exceeds maximum", "use a smaller output cap")
	}
	return validatedInput{
		Argv:           argv,
		Workdir:        workdir,
		CWD:            cwd,
		Env:            env,
		EnvKeys:        envKeys,
		TimeoutSeconds: timeout,
		MaxOutputBytes: outputBytes,
	}, capability.Response[json.RawMessage]{}
}

func validateArgv(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("argv is required")
	}
	if len(argv) > 64 {
		return nil, errors.New("argv has too many arguments")
	}
	out := make([]string, len(argv))
	total := 0
	for i, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("argv[%d] is empty", i)
		}
		if strings.Contains(arg, "\x00") {
			return nil, fmt.Errorf("argv[%d] contains NUL", i)
		}
		if len(arg) > 4096 {
			return nil, fmt.Errorf("argv[%d] exceeds length limit", i)
		}
		total += len(arg)
		out[i] = arg
	}
	if total > 16384 {
		return nil, errors.New("argv exceeds total length limit")
	}
	if len(out) == 1 && strings.ContainsAny(out[0], " \t\n;&|") {
		return nil, errors.New("single string shell commands are not accepted; pass an explicit argv array")
	}
	deny := map[string]bool{
		"docker": true, "podman": true, "mount": true, "umount": true,
		"sudo": true, "su": true, "ssh": true, "scp": true, "nc": true,
	}
	if deny[path.Base(out[0])] {
		return nil, fmt.Errorf("executable %q is denied", path.Base(out[0]))
	}
	return out, nil
}

func normalizeCWD(cwd string) (string, string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		cwd = "."
	}
	if path.IsAbs(cwd) {
		return "", "", fmt.Errorf("cwd %q must be relative", cwd)
	}
	clean := path.Clean(cwd)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("cwd %q escapes workspace", cwd)
	}
	if clean == "." {
		return cpruntime.ContainerWorkspacePath, ".", nil
	}
	return path.Join(cpruntime.ContainerWorkspacePath, clean), clean, nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnv(env map[string]string) (map[string]string, []string, error) {
	if len(env) == 0 {
		return nil, nil, nil
	}
	out := make(map[string]string, len(env))
	keys := make([]string, 0, len(env))
	for key, value := range env {
		if !envKeyPattern.MatchString(key) {
			return nil, nil, fmt.Errorf("env key %q is invalid", key)
		}
		upper := strings.ToUpper(key)
		for _, denied := range []string{"COORDPLANE_", "TOKEN", "SECRET", "PASSWORD", "API_KEY", "APIKEY", "DB_PATH", "DATABASE_PATH", "RUNTIME_ROOT"} {
			if strings.Contains(upper, denied) {
				return nil, nil, fmt.Errorf("env key %q is forbidden", key)
			}
		}
		switch upper {
		case "PATH", "HOME", "SHELL":
			return nil, nil, fmt.Errorf("env key %q is forbidden", key)
		}
		if strings.Contains(value, "\x00") {
			return nil, nil, fmt.Errorf("env key %q contains NUL", key)
		}
		out[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return out, keys, nil
}

func (s *Service) resolveScope(ctx context.Context, agentID, subjectRuntimeID, leaseID string) (scope, capability.Response[json.RawMessage]) {
	if agentID == "" {
		return scope{}, reject("COMMAND_AGENT_REQUIRED", "command.run requires an agent subject", "retry from an authenticated coordlink agent subject")
	}
	var out scope
	var leaseState, attemptStatus, runtimeKind, routeState, runtimeState string
	err := s.db.QueryRowContext(ctx, `
SELECT l.agent_id, l.id, l.assignment_id, asn.contract_id,
  a.id, a.status, a.runtime_kind,
  sr.id, sr.runtime_id, sr.state,
  ri.container_id, ri.container_name, ri.state, ri.workspace_path, ri.home_path
FROM leases l
JOIN assignments asn ON asn.id = l.assignment_id
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
		&out.AttemptID,
		&attemptStatus,
		&runtimeKind,
		&out.SessionRouteID,
		&out.RuntimeID,
		&routeState,
		&out.ContainerID,
		&out.ContainerName,
		&runtimeState,
		&out.WorkspacePath,
		&out.HomePath,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return scope{}, reject("COMMAND_SCOPE_REJECTED", "active lease/session/runtime scope was not found for this agent", "claim an assignment and start a Docker-backed session before command.run")
	}
	if err != nil {
		return scope{}, capability.Error[json.RawMessage]("COMMAND_SCOPE_LOOKUP_FAILED", err.Error(), true)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, leaseID).Scan(&leaseState); err != nil {
		return scope{}, capability.Error[json.RawMessage]("COMMAND_LEASE_LOOKUP_FAILED", err.Error(), true)
	}
	switch {
	case leaseState != "active":
		return scope{}, reject("COMMAND_LEASE_REJECTED", "lease is not active", "run command.run only while holding the active lease")
	case attemptStatus != "running":
		return scope{}, reject("COMMAND_ATTEMPT_REJECTED", "attempt is not running", "start or resume the session before command.run")
	case routeState != "active":
		return scope{}, reject("COMMAND_SESSION_REJECTED", "session route is not active", "run command.run only from an active session")
	case runtimeKind != "docker" || runtimeState != "ready" || out.ContainerName == "":
		return scope{}, reject("COMMAND_RUNTIME_REJECTED", "runtime is not a ready Docker container", "use command.run only from a Docker runtime session")
	case out.WorkspacePath != cpruntime.ContainerWorkspacePath || out.HomePath != cpruntime.ContainerHomePath:
		return scope{}, reject("COMMAND_RUNTIME_REJECTED", "runtime paths do not match the container workspace contract", "prepare a Docker runtime with /workspace/project and /home/agent")
	case subjectRuntimeID != "" && subjectRuntimeID != out.RuntimeID:
		return scope{}, reject("COMMAND_RUNTIME_REJECTED", "subject runtime_id does not match the active session route", "retry from the coordlink runtime identity injected into this session")
	}
	return out, capability.Response[json.RawMessage]{}
}

func (s *Service) commandRunByIdempotency(ctx context.Context, resolved scope, key string) (Run, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, agent_id, lease_id, assignment_id, contract_id, attempt_id, session_route_id,
  runtime_id, container_id, container_name, cwd, argv_json, env_keys_json, status,
  exit_code, stdout_ref, stderr_ref, stdout_bytes, stderr_bytes, stdout_truncated,
  stderr_truncated, timeout_seconds, duration_ms, evidence_id, last_error,
  COALESCE(idempotency_key, ''), created_at, started_at, COALESCE(ended_at, ''), updated_at
FROM command_runs
WHERE agent_id = ? AND attempt_id = ? AND idempotency_key = ?
LIMIT 1`, resolved.AgentID, resolved.AttemptID, key)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return run, true, nil
}

func (s *Service) insertRunning(ctx context.Context, resolved scope, in validatedInput, idempotencyKey string) (string, error) {
	runID, err := ids.New("cmdrun")
	if err != nil {
		return "", err
	}
	argvJSON, err := json.Marshal(in.Argv)
	if err != nil {
		return "", err
	}
	envKeysJSON, err := json.Marshal(in.EnvKeys)
	if err != nil {
		return "", err
	}
	now := formatTime(time.Now())
	err = s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO command_runs (
  id, tenant_id, agent_id, lease_id, assignment_id, contract_id, attempt_id,
  session_route_id, runtime_id, container_id, container_name, cwd, argv_json,
  env_keys_json, status, timeout_seconds, idempotency_key, created_at,
  started_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?)`,
			runID, resolved.AgentID, resolved.LeaseID, resolved.AssignmentID,
			resolved.ContractID, resolved.AttemptID, resolved.SessionRouteID,
			resolved.RuntimeID, resolved.ContainerID, resolved.ContainerName,
			in.CWD, string(argvJSON), string(envKeysJSON), in.TimeoutSeconds,
			nullable(idempotencyKey), now, now, now,
		); err != nil {
			return fmt.Errorf("insert command run: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "command.run_requested", runID, resolved, map[string]any{
			"cwd":      in.CWD,
			"argv":     in.Argv,
			"env_keys": in.EnvKeys,
		}); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, "command.exec_started", runID, resolved, map[string]any{
			"container_name": resolved.ContainerName,
			"workdir":        in.Workdir,
		})
		return err
	})
	return runID, err
}

type completion struct {
	Status          string
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	DurationMS      int64
	LastError       string
}

func (s *Service) completeRun(ctx context.Context, runID string, resolved scope, done completion) (Run, error) {
	var completed Run
	err := s.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stdout, err := s.objects.PutTx(ctx, tx, objects.PutInput{
			OwnerAgent:  resolved.AgentID,
			Content:     done.Stdout,
			ContentType: "text/plain; charset=utf-8",
		})
		if err != nil {
			return fmt.Errorf("put stdout object: %w", err)
		}
		stderr, err := s.objects.PutTx(ctx, tx, objects.PutInput{
			OwnerAgent:  resolved.AgentID,
			Content:     done.Stderr,
			ContentType: "text/plain; charset=utf-8",
		})
		if err != nil {
			return fmt.Errorf("put stderr object: %w", err)
		}
		evidenceID, err := ids.New("ev")
		if err != nil {
			return err
		}
		summary := commandSummary(done.Status, done.ExitCode, stdout.Ref, stderr.Ref, len(done.Stdout), len(done.Stderr))
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
INSERT INTO evidence (
  id, tenant_id, kind, contract_id, produced_by, content_ref,
  inline_content, summary, verdict, created_at
) VALUES (?, 'default', 'command_run', ?, ?, ?, '', ?, ?, ?)`,
			evidenceID, resolved.ContractID, resolved.AgentID, "command_run:"+runID,
			summary, done.Status, now,
		); err != nil {
			return fmt.Errorf("insert command evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE command_runs
SET status = ?, exit_code = ?, stdout_ref = ?, stderr_ref = ?,
  stdout_bytes = ?, stderr_bytes = ?, stdout_truncated = ?, stderr_truncated = ?,
  duration_ms = ?, evidence_id = ?, last_error = ?, ended_at = ?, updated_at = ?
WHERE id = ?`,
			done.Status, done.ExitCode, stdout.Ref, stderr.Ref, len(done.Stdout), len(done.Stderr),
			boolInt(done.StdoutTruncated), boolInt(done.StderrTruncated), done.DurationMS,
			evidenceID, done.LastError, now, now, runID,
		); err != nil {
			return fmt.Errorf("update command run: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "command.output_captured", runID, resolved, map[string]any{
			"stdout_ref":       stdout.Ref,
			"stderr_ref":       stderr.Ref,
			"stdout_bytes":     len(done.Stdout),
			"stderr_bytes":     len(done.Stderr),
			"stdout_truncated": done.StdoutTruncated,
			"stderr_truncated": done.StderrTruncated,
		}); err != nil {
			return err
		}
		eventType := "command.succeeded"
		if done.Status == "failed" {
			eventType = "command.failed"
		}
		if done.Status == "timed_out" {
			eventType = "command.timed_out"
		}
		if _, err := appendEvent(ctx, tx, eventType, runID, resolved, map[string]any{
			"exit_code":   done.ExitCode,
			"last_error":  done.LastError,
			"evidence_id": evidenceID,
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "evidence.command_run_recorded", runID, resolved, map[string]any{
			"evidence_id": evidenceID,
			"content_ref": "command_run:" + runID,
		}); err != nil {
			return err
		}
		run, err := runByIDTx(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("read completed command run: %w", err)
		}
		completed = run
		return nil
	})
	return completed, err
}

func appendEvent(ctx context.Context, tx *sql.Tx, eventType, runID string, resolved scope, payload any) (string, error) {
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
		CapabilityName: "command.run",
		Type:           eventType,
		AggregateType:  "command_run",
		AggregateID:    runID,
		PayloadJSON:    raw,
	})
}

func ListRuns(ctx context.Context, db *sql.DB) ([]Run, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, agent_id, lease_id, assignment_id, contract_id, attempt_id, session_route_id,
  runtime_id, container_id, container_name, cwd, argv_json, env_keys_json, status,
  exit_code, stdout_ref, stderr_ref, stdout_bytes, stderr_bytes, stdout_truncated,
  stderr_truncated, timeout_seconds, duration_ms, evidence_id, last_error,
  COALESCE(idempotency_key, ''), created_at, started_at, COALESCE(ended_at, ''), updated_at
FROM command_runs
ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list command runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func runByIDTx(ctx context.Context, tx *sql.Tx, runID string) (Run, error) {
	return scanRun(tx.QueryRowContext(ctx, `
SELECT id, agent_id, lease_id, assignment_id, contract_id, attempt_id, session_route_id,
  runtime_id, container_id, container_name, cwd, argv_json, env_keys_json, status,
  exit_code, stdout_ref, stderr_ref, stdout_bytes, stderr_bytes, stdout_truncated,
  stderr_truncated, timeout_seconds, duration_ms, evidence_id, last_error,
  COALESCE(idempotency_key, ''), created_at, started_at, COALESCE(ended_at, ''), updated_at
FROM command_runs
WHERE id = ?`, runID))
}

type rowScanner interface {
	Scan(...any) error
}

func scanRun(row rowScanner) (Run, error) {
	var run Run
	var argvJSON, envKeysJSON, createdAt, startedAt, endedAt, updatedAt string
	var exitCode sql.NullInt64
	var stdoutTruncated, stderrTruncated int
	if err := row.Scan(
		&run.ID,
		&run.AgentID,
		&run.LeaseID,
		&run.AssignmentID,
		&run.ContractID,
		&run.AttemptID,
		&run.SessionRouteID,
		&run.RuntimeID,
		&run.ContainerID,
		&run.ContainerName,
		&run.CWD,
		&argvJSON,
		&envKeysJSON,
		&run.Status,
		&exitCode,
		&run.StdoutRef,
		&run.StderrRef,
		&run.StdoutBytes,
		&run.StderrBytes,
		&stdoutTruncated,
		&stderrTruncated,
		&run.TimeoutSeconds,
		&run.DurationMS,
		&run.EvidenceID,
		&run.LastError,
		&run.IdempotencyKey,
		&createdAt,
		&startedAt,
		&endedAt,
		&updatedAt,
	); err != nil {
		return Run{}, err
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	if argvJSON != "" {
		if err := json.Unmarshal([]byte(argvJSON), &run.Argv); err != nil {
			return Run{}, err
		}
	}
	if envKeysJSON != "" {
		if err := json.Unmarshal([]byte(envKeysJSON), &run.EnvKeys); err != nil {
			return Run{}, err
		}
	}
	run.StdoutTruncated = stdoutTruncated != 0
	run.StderrTruncated = stderrTruncated != 0
	var err error
	run.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Run{}, err
	}
	run.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return Run{}, err
	}
	if endedAt != "" {
		parsed, err := parseTime(endedAt)
		if err != nil {
			return Run{}, err
		}
		run.EndedAt = &parsed
	}
	run.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

func resultFromRun(run Run) Result {
	exitCode := 0
	if run.ExitCode != nil {
		exitCode = *run.ExitCode
	}
	return Result{
		CommandRunID:    run.ID,
		ContractID:      run.ContractID,
		AttemptID:       run.AttemptID,
		RuntimeID:       run.RuntimeID,
		SessionRouteID:  run.SessionRouteID,
		EvidenceID:      run.EvidenceID,
		Status:          run.Status,
		ExitCode:        exitCode,
		StdoutRef:       run.StdoutRef,
		StderrRef:       run.StderrRef,
		StdoutBytes:     run.StdoutBytes,
		StderrBytes:     run.StderrBytes,
		StdoutTruncated: run.StdoutTruncated,
		StderrTruncated: run.StderrTruncated,
		Summary:         commandSummary(run.Status, exitCode, run.StdoutRef, run.StderrRef, run.StdoutBytes, run.StderrBytes),
	}
}

func capBytes(in []byte, limit int) ([]byte, bool) {
	if limit <= 0 || len(in) <= limit {
		return append([]byte(nil), in...), false
	}
	return append([]byte(nil), in[:limit]...), true
}

func commandSummary(status string, exitCode int, stdoutRef, stderrRef string, stdoutBytes, stderrBytes int) string {
	return fmt.Sprintf("command.run %s exit_code=%d stdout_bytes=%d stderr_bytes=%d stdout_ref=%s stderr_ref=%s",
		status, exitCode, stdoutBytes, stderrBytes, stdoutRef, stderrRef)
}

func reject(code, message, repair string) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		code,
		message,
		capability.WithRepairHint(repair),
		capability.WithAllowedNextActions("command.run"),
		capability.WithRetryable(false),
	)
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
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
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

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
