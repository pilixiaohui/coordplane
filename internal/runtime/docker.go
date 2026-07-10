package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"coordplane/internal/ids"
	"coordplane/internal/secrets"
)

const defaultDockerNetwork = "coordplane"
const defaultClaudeAuthProbeTimeout = 30 * time.Second

type DockerRuntimeConfig struct {
	DB             *sql.DB
	ProfileName    string
	TeamID         string
	Image          string
	Network        string
	RuntimeRoot    string
	CoordlinkPath  string
	DBPath         string
	ClaudeBinary   string
	Ready          bool
	Docker         DockerClient
	SecretProvider secrets.Provider
	AuthProbe      ClaudeAuthProbe
}

type DockerRuntime struct {
	db            *sql.DB
	profileName   string
	teamID        string
	image         string
	network       string
	runtimeRoot   string
	coordlinkPath string
	dbPath        string
	claudeBinary  string
	ready         bool
	docker        DockerClient
	secrets       secrets.Provider
	authProbe     ClaudeAuthProbe
}

type DockerClient interface {
	PrepareContainer(context.Context, DockerContainerSpec) (DockerContainerResult, error)
}

type DockerContainerSpec struct {
	RuntimeID     string
	ContainerName string
	Image         string
	Network       string
	User          string
	Labels        map[string]string
	Env           map[string]string
	Mounts        []DockerMount
	BackendURL    string
}

type DockerMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type DockerContainerResult struct {
	ContainerID string
	Checks      map[string]bool
}

type ClaudeAuthProbe interface {
	ProbeClaudeAuth(context.Context, ClaudeAuthProbeSpec) (ClaudeAuthProbeResult, error)
}

type ClaudeAuthProbeSpec struct {
	ContainerName string
	Workdir       string
	HomeDir       string
	Binary        string
	Env           map[string]string
	AuthSource    string
	IdentityRef   string
	Timeout       time.Duration
}

type ClaudeAuthProbeResult struct {
	Checks    map[string]bool
	ErrorCode string
}

func NewDockerRuntime(cfg DockerRuntimeConfig) *DockerRuntime {
	if cfg.Network == "" {
		cfg.Network = defaultDockerNetwork
	}
	if cfg.Docker == nil {
		cfg.Docker = DockerCLIClient{}
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "/usr/local/bin/claude"
	}
	if cfg.AuthProbe == nil {
		cfg.AuthProbe = DockerClaudeAuthProbe{}
	}
	return &DockerRuntime{
		db:            cfg.DB,
		profileName:   cfg.ProfileName,
		teamID:        cfg.TeamID,
		image:         cfg.Image,
		network:       cfg.Network,
		runtimeRoot:   cfg.RuntimeRoot,
		coordlinkPath: cfg.CoordlinkPath,
		dbPath:        cfg.DBPath,
		claudeBinary:  cfg.ClaudeBinary,
		ready:         cfg.Ready,
		docker:        cfg.Docker,
		secrets:       cfg.SecretProvider,
		authProbe:     cfg.AuthProbe,
	}
}

func (r *DockerRuntime) Name() string {
	if r == nil {
		return ""
	}
	return r.profileName
}

func (r *DockerRuntime) Kind() string {
	return "docker"
}

func (r *DockerRuntime) IsReady() bool {
	return r != nil && r.ready
}

func (r *DockerRuntime) Prepare(ctx context.Context, req PrepareRequest) (prepared PreparedRuntime, retErr error) {
	if err := r.validate(req); err != nil {
		return PreparedRuntime{}, err
	}
	runtimeID, err := ids.New("rt_docker")
	if err != nil {
		return PreparedRuntime{}, err
	}
	containerName := DockerSafeName(fmt.Sprintf("coordplane-%s-%s-%s", r.teamID, req.AgentID, shortID(req.AttemptID)))
	env, err := BuildRuntimeEnv(EnvironmentInput{
		BackendURL:    req.BackendURL,
		AgentID:       req.AgentID,
		RuntimeID:     runtimeID,
		AttemptID:     req.AttemptID,
		AssignmentID:  req.AssignmentID,
		LeaseID:       req.LeaseID,
		Workspace:     ContainerWorkspacePath,
		CLIBackend:    req.CLIBackend,
		TeamID:        req.TeamID,
		WorkspaceName: req.WorkspaceName,
	})
	if err != nil {
		return PreparedRuntime{}, err
	}
	authMaterial, err := r.authMaterial(ctx, req, runtimeID)
	if err != nil {
		return PreparedRuntime{}, err
	}
	for key, value := range authMaterial.Env {
		env[key] = value
	}
	if err := ValidateRuntimeEnv(env); err != nil {
		return PreparedRuntime{}, err
	}
	workspaceHost := filepath.Join(r.runtimeRoot, "workspaces", safePathPart(req.AgentID), safePathPart(req.AttemptID), "project")
	homeHost := filepath.Join(r.runtimeRoot, "home", safePathPart(req.AgentID))
	if err := r.validateHostPaths(workspaceHost, homeHost); err != nil {
		return PreparedRuntime{}, err
	}
	if err := os.MkdirAll(workspaceHost, 0o755); err != nil {
		return PreparedRuntime{}, fmt.Errorf("docker runtime: create workspace: %w", err)
	}
	if err := os.MkdirAll(homeHost, 0o755); err != nil {
		return PreparedRuntime{}, fmt.Errorf("docker runtime: create home: %w", err)
	}
	executionUser := currentExecutionUser()
	mounts := []DockerMount{
		{Source: workspaceHost, Target: ContainerWorkspacePath},
		{Source: homeHost, Target: ContainerHomePath},
		{Source: r.coordlinkPath, Target: ContainerCoordlinkPath, ReadOnly: true},
	}
	if err := validateDockerMounts(mounts, []string{r.runtimeRoot, r.coordlinkPath}, r.dbPath); err != nil {
		return PreparedRuntime{}, err
	}
	checks := map[string]bool{
		"coordlink_present":        true,
		"workspace_private":        true,
		"home_persistent":          true,
		"forbidden_env_absent":     true,
		"forbidden_mount_absent":   true,
		"backend_url_configured":   req.BackendURL != "",
		"other_agent_paths_absent": true,
		"home_private":             true,
	}
	if len(authMaterial.Env) > 0 {
		checks["claude_auth_configured"] = true
		checks["claude_auth_source_secret_provider_env"] = true
	}
	instanceID, err := r.insertPreparing(ctx, runtimeID, req, containerName, workspaceHost, homeHost, checks, env)
	if err != nil {
		return PreparedRuntime{}, err
	}
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
		defer cancel()
		var cleanupErrors []error
		if err := r.markFailed(cleanupCtx, instanceID, runtimeID, retErr, checks); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("mark Docker prepare failed: %w", err))
		}
		if err := r.FinalizeRuntime(cleanupCtx, req.AttemptID, "docker prepare failed"); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		if len(cleanupErrors) > 0 {
			retErr = errors.Join(retErr, errors.Join(cleanupErrors...))
		}
	}()
	if len(authMaterial.Env) > 0 {
		if err := r.recordAuthMaterialInjected(ctx, runtimeID, req, authMaterial); err != nil {
			return PreparedRuntime{}, err
		}
	}

	spec := DockerContainerSpec{
		RuntimeID:     runtimeID,
		ContainerName: containerName,
		Image:         r.image,
		Network:       r.network,
		User:          executionUser,
		Labels: map[string]string{
			"coordplane.managed":    "true",
			"coordplane.team_id":    r.teamID,
			"coordplane.agent_id":   req.AgentID,
			"coordplane.attempt_id": req.AttemptID,
			"coordplane.lease_id":   req.LeaseID,
			"coordplane.runtime_id": runtimeID,
		},
		Env:        env,
		Mounts:     mounts,
		BackendURL: req.BackendURL,
	}
	result, err := r.docker.PrepareContainer(ctx, spec)
	if result.ContainerID != "" {
		if persistErr := r.recordContainerIdentity(ctx, instanceID, result.ContainerID); persistErr != nil {
			return PreparedRuntime{}, persistErr
		}
	}
	if err != nil {
		return PreparedRuntime{}, err
	}
	for key, value := range result.Checks {
		checks[key] = value
	}
	if err := validateRuntimeChecks(checks, ""); err != nil {
		return PreparedRuntime{}, err
	}
	if req.CLIBackend == "claude" {
		probe, err := r.probeClaudeAuth(ctx, runtimeID, spec, authMaterial)
		for key, value := range probe.Checks {
			checks[key] = value
		}
		if err != nil {
			_ = r.recordAuthProbeFailed(ctx, runtimeID, req, probe, err)
			return PreparedRuntime{}, err
		}
		if err := r.recordAuthProbePassed(ctx, runtimeID, req, probe, authMaterial); err != nil {
			return PreparedRuntime{}, err
		}
		if err := validateRuntimeChecks(checks, req.CLIBackend); err != nil {
			return PreparedRuntime{}, err
		}
	}
	if err := validateRuntimeChecks(checks, req.CLIBackend); err != nil {
		return PreparedRuntime{}, err
	}
	if err := r.markReady(ctx, instanceID, runtimeID, result.ContainerID, checks); err != nil {
		return PreparedRuntime{}, err
	}
	prepared = PreparedRuntime{
		RuntimeID:         runtimeID,
		Kind:              "docker",
		Workspace:         ContainerWorkspacePath,
		HomeDir:           ContainerHomePath,
		WorkspaceGuardRef: "runtime:" + runtimeID + ":workspace",
		HomeGuardRef:      "runtime:" + runtimeID + ":home",
		Env:               env,
		ContainerID:       result.ContainerID,
		ContainerName:     containerName,
		Checks:            cloneBoolMap(checks),
	}
	return prepared, nil
}

func (r *DockerRuntime) authMaterial(ctx context.Context, req PrepareRequest, runtimeID string) (secrets.Material, error) {
	if req.CLIBackend != "claude" || r.secrets == nil {
		return secrets.Material{}, nil
	}
	material, err := r.secrets.RuntimeAuth(ctx, secrets.Request{
		TeamID:     req.TeamID,
		AgentID:    req.AgentID,
		RuntimeID:  runtimeID,
		AttemptID:  req.AttemptID,
		CLIBackend: req.CLIBackend,
	})
	if err != nil {
		return secrets.Material{}, err
	}
	if err := secrets.ValidateMaterial(material); err != nil {
		return secrets.Material{}, err
	}
	return material, nil
}

func (r *DockerRuntime) validate(req PrepareRequest) error {
	if r == nil {
		return errors.New("docker runtime: nil backend")
	}
	if !r.ready {
		return errors.New("DOCKER_RUNTIME_NOT_READY: docker runtime is not ready")
	}
	if r.db == nil {
		return errors.New("docker runtime: database is required")
	}
	if r.profileName == "" || r.teamID == "" || r.image == "" || r.runtimeRoot == "" || r.coordlinkPath == "" {
		return errors.New("docker runtime: profile, team, image, runtime root, and coordlink path are required")
	}
	if req.AgentID == "" || req.AttemptID == "" || req.AssignmentID == "" || req.LeaseID == "" ||
		req.TeamID == "" || req.CLIBackend == "" || req.BackendURL == "" || req.WorkspaceName == "" {
		return errors.New("docker runtime: prepare request identity is incomplete")
	}
	info, err := os.Stat(r.coordlinkPath)
	if err != nil {
		return fmt.Errorf("docker runtime: coordlink not available: %w", err)
	}
	if info.IsDir() {
		return errors.New("docker runtime: coordlink path is a directory")
	}
	return nil
}

func (r *DockerRuntime) validateHostPaths(workspaceHost, homeHost string) error {
	for label, path := range map[string]string{
		"runtime_root": r.runtimeRoot,
		"workspace":    workspaceHost,
		"home":         homeHost,
	} {
		if filepath.Clean(path) == "." || filepath.IsAbs(path) == false {
			return fmt.Errorf("docker runtime: %s path must be absolute", label)
		}
	}
	dbDir := ""
	if r.dbPath != "" {
		dbDir = filepath.Dir(r.dbPath)
	}
	if dbDir != "" {
		for label, path := range map[string]string{"workspace": workspaceHost, "home": homeHost} {
			if sameOrChild(path, dbDir) || sameOrChild(dbDir, path) {
				return fmt.Errorf("docker runtime: %s path overlaps backend database directory", label)
			}
		}
	}
	if sameOrChild(workspaceHost, homeHost) || sameOrChild(homeHost, workspaceHost) {
		return errors.New("docker runtime: workspace and home paths overlap")
	}
	return nil
}

func (r *DockerRuntime) insertPreparing(ctx context.Context, runtimeID string, req PrepareRequest, containerName, workspaceHost, homeHost string, checks map[string]bool, env map[string]string) (string, error) {
	instanceID, err := ids.New("rti")
	if err != nil {
		return "", err
	}
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return "", err
	}
	envKeysJSON, err := json.Marshal(RuntimeEnvKeys(env))
	if err != nil {
		return "", err
	}
	now := formatTime(time.Now())
	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_instances (
  id, tenant_id, runtime_id, runtime_profile, runtime_kind, agent_id,
  attempt_id, lease_id, container_name, image, network, state,
  workspace_path, home_path, host_workspace_ref, host_home_ref,
  coordlink_path, checks_json, env_keys_json, created_at, updated_at
) VALUES (?, 'default', ?, ?, 'docker', ?, ?, ?, ?, ?, ?, 'preparing', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			instanceID, runtimeID, r.profileName, req.AgentID, req.AttemptID, req.LeaseID,
			containerName, r.image, r.network, ContainerWorkspacePath, ContainerHomePath,
			workspaceHost, homeHost, r.coordlinkPath, string(checksJSON), string(envKeysJSON),
			now, now,
		); err != nil {
			return fmt.Errorf("insert runtime instance: %w", err)
		}
		if err := recordRuntimeTokenTx(ctx, tx, runtimeTokenInput{
			AgentID:   req.AgentID,
			RuntimeID: runtimeID,
			AttemptID: req.AttemptID,
			LeaseID:   req.LeaseID,
			Token:     env["COORDPLANE_TOKEN"],
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "runtime.prepare_started", "runtime_instance", runtimeID, map[string]any{
			"attempt_id": req.AttemptID,
			"agent_id":   req.AgentID,
			"kind":       "docker",
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "runtime.env_injected", "runtime_instance", runtimeID, map[string]any{
			"env_keys": RuntimeEnvKeys(env),
		}); err != nil {
			return err
		}
		return nil
	})
	return instanceID, err
}

func (r *DockerRuntime) markReady(ctx context.Context, instanceID, runtimeID, containerID string, checks map[string]bool) error {
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	now := formatTime(time.Now())
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET container_id = ?, state = 'ready', checks_json = ?, updated_at = ?
WHERE id = ?`,
			containerID, string(checksJSON), now, instanceID,
		); err != nil {
			return fmt.Errorf("mark runtime ready: %w", err)
		}
		if _, err := appendEvent(ctx, tx, "runtime.container_created", "runtime_instance", runtimeID, map[string]any{
			"container_id": containerID,
		}); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, "runtime.coordlink_verified", "runtime_instance", runtimeID, map[string]any{
			"coordlink_path": ContainerCoordlinkPath,
		}); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, "runtime.ready", "runtime_instance", runtimeID, map[string]any{
			"container_id": containerID,
			"checks":       checks,
		})
		return err
	})
}

func (r *DockerRuntime) recordContainerIdentity(ctx context.Context, instanceID, containerID string) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE runtime_instances
SET container_id = ?, updated_at = ?
WHERE id = ?`, containerID, formatTime(time.Now()), instanceID)
	if err != nil {
		return fmt.Errorf("record runtime container identity: %w", err)
	}
	return nil
}

func (r *DockerRuntime) markFailed(ctx context.Context, instanceID, runtimeID string, cause error, checks ...map[string]bool) error {
	now := formatTime(time.Now())
	checksJSON := ""
	if len(checks) > 0 && checks[0] != nil {
		raw, err := json.Marshal(checks[0])
		if err != nil {
			return err
		}
		checksJSON = string(raw)
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if checksJSON != "" {
			if _, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET state = 'failed', last_error = ?, checks_json = ?, updated_at = ?
WHERE id = ?`,
				cause.Error(), checksJSON, now, instanceID,
			); err != nil {
				return fmt.Errorf("mark runtime failed: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `
UPDATE runtime_instances
SET state = 'failed', last_error = ?, updated_at = ?
WHERE id = ?`,
			cause.Error(), now, instanceID,
		); err != nil {
			return fmt.Errorf("mark runtime failed: %w", err)
		}
		_, err := appendEvent(ctx, tx, "runtime.prepare_failed", "runtime_instance", runtimeID, map[string]any{
			"error": cause.Error(),
		})
		return err
	})
}

func (r *DockerRuntime) recordAuthMaterialInjected(ctx context.Context, runtimeID string, req PrepareRequest, material secrets.Material) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendEvent(ctx, tx, "runtime.auth_material_injected", "runtime_instance", runtimeID, map[string]any{
			"agent_id":     req.AgentID,
			"attempt_id":   req.AttemptID,
			"cli_backend":  req.CLIBackend,
			"source":       material.Source,
			"env_keys":     secrets.EnvKeys(material.Env),
			"identity_ref": material.IdentityRef,
		})
		return err
	})
}

func (r *DockerRuntime) probeClaudeAuth(ctx context.Context, runtimeID string, container DockerContainerSpec, material secrets.Material) (ClaudeAuthProbeResult, error) {
	if r.authProbe == nil {
		return ClaudeAuthProbeResult{}, errors.New("CLAUDE_AUTH_REQUIRED: claude auth probe is not configured")
	}
	if err := r.recordAuthProbeStarted(ctx, runtimeID, container, material); err != nil {
		return ClaudeAuthProbeResult{}, err
	}
	probeEnv := cloneStringMap(container.Env)
	probeEnv["HOME"] = ContainerHomePath
	source := material.Source
	if source == "" {
		source = "preseeded_home"
	}
	result, err := r.authProbe.ProbeClaudeAuth(ctx, ClaudeAuthProbeSpec{
		ContainerName: container.ContainerName,
		Workdir:       ContainerWorkspacePath,
		HomeDir:       ContainerHomePath,
		Binary:        r.claudeBinary,
		Env:           probeEnv,
		AuthSource:    source,
		IdentityRef:   material.IdentityRef,
		Timeout:       defaultClaudeAuthProbeTimeout,
	})
	if result.Checks == nil {
		result.Checks = make(map[string]bool)
	}
	if err != nil {
		if result.ErrorCode == "" {
			result.ErrorCode = "CLAUDE_AUTH_REQUIRED"
		}
		return result, err
	}
	result.Checks["claude_present"] = true
	result.Checks["claude_auth_configured"] = true
	result.Checks["claude_auth_probe_passed"] = true
	result.Checks["home_private"] = true
	result.Checks["home_persistent"] = true
	if source == "secret_provider_env" {
		result.Checks["claude_auth_source_secret_provider_env"] = true
	} else {
		result.Checks["claude_auth_source_preseeded_home"] = true
	}
	return result, nil
}

func (r *DockerRuntime) recordAuthProbeStarted(ctx context.Context, runtimeID string, container DockerContainerSpec, material secrets.Material) error {
	source := material.Source
	if source == "" {
		source = "preseeded_home"
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendEvent(ctx, tx, "runtime.auth_probe_started", "runtime_instance", runtimeID, map[string]any{
			"cli_backend":  "claude",
			"source":       source,
			"env_keys":     secrets.EnvKeys(material.Env),
			"identity_ref": material.IdentityRef,
			"home":         ContainerHomePath,
			"container":    container.ContainerName,
		})
		return err
	})
}

func (r *DockerRuntime) recordAuthProbePassed(ctx context.Context, runtimeID string, req PrepareRequest, result ClaudeAuthProbeResult, material secrets.Material) error {
	source := material.Source
	if source == "" {
		source = "preseeded_home"
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendEvent(ctx, tx, "runtime.auth_probe_passed", "runtime_instance", runtimeID, map[string]any{
			"agent_id":     req.AgentID,
			"attempt_id":   req.AttemptID,
			"cli_backend":  req.CLIBackend,
			"source":       source,
			"env_keys":     secrets.EnvKeys(material.Env),
			"identity_ref": material.IdentityRef,
			"checks": map[string]bool{
				"claude_present":             result.Checks["claude_present"],
				"claude_auth_probe_passed":   result.Checks["claude_auth_probe_passed"],
				"claude_auth_configured":     result.Checks["claude_auth_configured"],
				"home_private":               result.Checks["home_private"],
				"home_persistent":            result.Checks["home_persistent"],
				"secret_provider_env_source": result.Checks["claude_auth_source_secret_provider_env"],
				"preseeded_home_source":      result.Checks["claude_auth_source_preseeded_home"],
			},
		})
		return err
	})
}

func (r *DockerRuntime) recordAuthProbeFailed(ctx context.Context, runtimeID string, req PrepareRequest, result ClaudeAuthProbeResult, cause error) error {
	errorCode := result.ErrorCode
	if errorCode == "" {
		errorCode = "CLAUDE_AUTH_REQUIRED"
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendEvent(ctx, tx, "runtime.auth_probe_failed", "runtime_instance", runtimeID, map[string]any{
			"agent_id":    req.AgentID,
			"attempt_id":  req.AttemptID,
			"cli_backend": req.CLIBackend,
			"error_code":  errorCode,
			"error":       redactedAuthError(cause),
		})
		return err
	})
}

type DockerCLIClient struct {
	Binary string
}

type DockerClaudeAuthProbe struct {
	Executor ContainerExecutor
}

func (p DockerClaudeAuthProbe) ProbeClaudeAuth(ctx context.Context, spec ClaudeAuthProbeSpec) (ClaudeAuthProbeResult, error) {
	if spec.ContainerName == "" || spec.Workdir == "" || spec.HomeDir == "" || spec.Binary == "" {
		return ClaudeAuthProbeResult{}, errors.New("CLAUDE_AUTH_REQUIRED: claude auth probe identity is incomplete")
	}
	executor := p.Executor
	if executor == nil {
		executor = DockerExecClient{}
	}
	checks := map[string]bool{
		"claude_present":             false,
		"claude_auth_configured":     false,
		"claude_auth_probe_passed":   false,
		"home_private":               true,
		"home_persistent":            true,
		"claude_auth_probe_redacted": true,
	}
	presence, err := executor.Exec(ctx, ContainerExecSpec{
		ContainerName: spec.ContainerName,
		Workdir:       spec.Workdir,
		HomeDir:       spec.HomeDir,
		Env:           spec.Env,
		Command:       []string{"sh", "-lc", "test -x " + shellQuote(spec.Binary)},
		Timeout:       spec.Timeout,
	})
	if err != nil || presence.ExitCode != 0 {
		return ClaudeAuthProbeResult{Checks: checks, ErrorCode: "CLAUDE_NOT_FOUND"}, errors.New("CLAUDE_NOT_FOUND: claude binary is not executable in the Docker runtime")
	}
	checks["claude_present"] = true
	probe, err := executor.Exec(ctx, ContainerExecSpec{
		ContainerName: spec.ContainerName,
		Workdir:       spec.Workdir,
		HomeDir:       spec.HomeDir,
		Env:           spec.Env,
		Command:       []string{spec.Binary, "--print"},
		Stdin:         "CoordPlane Claude auth probe. Reply with ok.\n",
		Timeout:       spec.Timeout,
	})
	if err != nil {
		return ClaudeAuthProbeResult{Checks: checks, ErrorCode: "CLAUDE_AUTH_REQUIRED"}, errors.New("CLAUDE_AUTH_REQUIRED: claude auth probe failed")
	}
	if probe.ExitCode != 0 {
		errorCode := "CLAUDE_AUTH_PROBE_FAILED"
		if looksLikeClaudeAuthFailure(probe.Stdout) || looksLikeClaudeAuthFailure(probe.Stderr) {
			errorCode = "CLAUDE_AUTH_REQUIRED"
		}
		return ClaudeAuthProbeResult{Checks: checks, ErrorCode: errorCode}, errors.New(errorCode + ": claude non-interactive auth probe failed")
	}
	checks["claude_auth_configured"] = true
	checks["claude_auth_probe_passed"] = true
	if spec.AuthSource == "secret_provider_env" {
		checks["claude_auth_source_secret_provider_env"] = true
	} else {
		checks["claude_auth_source_preseeded_home"] = true
	}
	return ClaudeAuthProbeResult{Checks: checks}, nil
}

func (c DockerCLIClient) PrepareContainer(ctx context.Context, spec DockerContainerSpec) (DockerContainerResult, error) {
	binary := c.Binary
	if binary == "" {
		binary = "docker"
	}
	if spec.ContainerName == "" || spec.Image == "" {
		return DockerContainerResult{}, errors.New("docker client: container name and image are required")
	}
	envFile, err := newDockerEnvFile(spec.Env)
	if err != nil {
		return DockerContainerResult{}, err
	}
	defer envFile.Cleanup()
	args := []string{"run", "-d", "--name", spec.ContainerName}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	for key, value := range sortedStringMap(spec.Labels) {
		args = append(args, "--label", key+"="+value)
	}
	args = envFile.AppendArgs(args)
	for _, mount := range spec.Mounts {
		value := "type=bind,src=" + mount.Source + ",dst=" + mount.Target
		if mount.ReadOnly {
			value += ",readonly"
		}
		args = append(args, "--mount", value)
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	args = append(args, spec.Image, "sleep", "3600")
	raw, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return DockerContainerResult{}, fmt.Errorf("docker run failed: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	containerID := strings.TrimSpace(string(raw))
	checks := make(map[string]bool)
	for name, args := range map[string][]string{
		"coordlink_present":      {"exec", spec.ContainerName, "test", "-x", ContainerCoordlinkPath},
		"workspace_private":      {"exec", spec.ContainerName, "test", "-d", ContainerWorkspacePath},
		"home_persistent":        {"exec", spec.ContainerName, "test", "-d", ContainerHomePath},
		"workspace_writable":     {"exec", spec.ContainerName, "sh", "-lc", "test -w /workspace/project && touch /workspace/project/.coordplane-write-test && rm -f /workspace/project/.coordplane-write-test"},
		"home_writable":          {"exec", spec.ContainerName, "sh", "-lc", "test -w /home/agent && touch /home/agent/.coordplane-write-test && rm -f /home/agent/.coordplane-write-test"},
		"git_workspace_writable": {"exec", spec.ContainerName, "sh", "-lc", "mkdir -p /workspace/project/.coordplane-git-write-test && touch /workspace/project/.coordplane-git-write-test/file && rm -rf /workspace/project/.coordplane-git-write-test"},
	} {
		if raw, err := exec.CommandContext(ctx, binary, args...).CombinedOutput(); err != nil {
			return DockerContainerResult{ContainerID: containerID, Checks: checks}, fmt.Errorf("docker check %s failed: %w: %s", name, err, strings.TrimSpace(string(raw)))
		}
		checks[name] = true
	}
	if spec.User != "" {
		raw, err := exec.CommandContext(ctx, binary, "exec", spec.ContainerName, "sh", "-lc", "printf '%s:%s' \"$(id -u)\" \"$(id -g)\"").CombinedOutput()
		if err != nil {
			return DockerContainerResult{ContainerID: containerID, Checks: checks}, fmt.Errorf("docker check cli_user_consistent failed: %w: %s", err, strings.TrimSpace(string(raw)))
		}
		checks["cli_user_consistent"] = strings.TrimSpace(string(raw)) == spec.User
		if !checks["cli_user_consistent"] {
			return DockerContainerResult{ContainerID: containerID, Checks: checks}, fmt.Errorf("docker check cli_user_consistent failed: got %q want %q", strings.TrimSpace(string(raw)), spec.User)
		}
	}
	envArgs := []string{"exec"}
	envArgs = envFile.AppendArgs(envArgs)
	envArgs = append(envArgs, spec.ContainerName, ContainerCoordlinkPath, "capability", "list")
	if raw, err := exec.CommandContext(ctx, binary, envArgs...).CombinedOutput(); err != nil {
		return DockerContainerResult{ContainerID: containerID, Checks: checks}, fmt.Errorf("docker coordlink backend check failed: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	checks["backend_reachable"] = true
	return DockerContainerResult{ContainerID: containerID, Checks: checks}, nil
}

func (c DockerCLIClient) InspectContainer(ctx context.Context, containerName string) (map[string]string, error) {
	binary := c.Binary
	if binary == "" {
		binary = "docker"
	}
	if strings.TrimSpace(containerName) == "" {
		return nil, errors.New("docker inspect: container name is required")
	}
	raw, err := exec.CommandContext(ctx, binary, "inspect", "--format", "{{json .Config.Labels}}", containerName).CombinedOutput()
	if err != nil {
		if dockerContainerNotFound(raw) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("docker inspect failed: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	var labels map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &labels); err != nil {
		return nil, fmt.Errorf("decode docker container labels: %w", err)
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	return labels, nil
}

func (c DockerCLIClient) RemoveContainer(ctx context.Context, containerName string) error {
	binary := c.Binary
	if binary == "" {
		binary = "docker"
	}
	if strings.TrimSpace(containerName) == "" {
		return errors.New("docker remove: container name is required")
	}
	raw, err := exec.CommandContext(ctx, binary, "rm", "-f", containerName).CombinedOutput()
	if err != nil {
		if dockerContainerNotFound(raw) {
			return os.ErrNotExist
		}
		return fmt.Errorf("docker remove failed: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	return nil
}

func dockerContainerNotFound(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	return strings.Contains(lower, "no such container") || strings.Contains(lower, "no such object")
}

type dockerEnvFile struct {
	dir  string
	path string
}

func newDockerEnvFile(env map[string]string) (dockerEnvFile, error) {
	if len(env) == 0 {
		return dockerEnvFile{}, nil
	}
	dir, err := os.MkdirTemp("", "coordplane-docker-env-*")
	if err != nil {
		return dockerEnvFile{}, fmt.Errorf("docker env-file: create temp dir: %w", err)
	}
	out := dockerEnvFile{dir: dir, path: filepath.Join(dir, "env")}
	cleanup := func() {
		out.Cleanup()
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return dockerEnvFile{}, fmt.Errorf("docker env-file: secure temp dir: %w", err)
	}
	var b strings.Builder
	for _, key := range RuntimeEnvKeys(env) {
		value := env[key]
		if strings.ContainsAny(key, "=\r\n\x00") {
			cleanup()
			return dockerEnvFile{}, fmt.Errorf("docker env-file: invalid env key %q", key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			cleanup()
			return dockerEnvFile{}, fmt.Errorf("docker env-file: env value for %s contains unsupported newline or NUL", key)
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(value)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(out.path, []byte(b.String()), 0o600); err != nil {
		cleanup()
		return dockerEnvFile{}, fmt.Errorf("docker env-file: write: %w", err)
	}
	if err := os.Chmod(out.path, 0o600); err != nil {
		cleanup()
		return dockerEnvFile{}, fmt.Errorf("docker env-file: secure file: %w", err)
	}
	return out, nil
}

func (f dockerEnvFile) AppendArgs(args []string) []string {
	if f.path == "" {
		return args
	}
	return append(args, "--env-file", f.path)
}

func (f dockerEnvFile) Cleanup() {
	if f.path != "" {
		_ = os.Remove(f.path)
	}
	if f.dir != "" {
		_ = os.Remove(f.dir)
	}
}

func currentExecutionUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

func validateRuntimeChecks(checks map[string]bool, cliBackend string) error {
	for _, name := range []string{"workspace_writable", "home_writable", "git_workspace_writable", "cli_user_consistent", "home_private", "home_persistent"} {
		if !checks[name] {
			return fmt.Errorf("docker runtime: required check %s did not pass", name)
		}
	}
	if cliBackend == "claude" {
		for _, name := range []string{"claude_present", "claude_auth_configured", "claude_auth_probe_passed", "claude_auth_probe_redacted"} {
			if !checks[name] {
				return fmt.Errorf("CLAUDE_AUTH_REQUIRED: docker runtime required check %s did not pass", name)
			}
		}
	}
	return nil
}

func ListRuntimeInstances(ctx context.Context, db *sql.DB) ([]RuntimeInstance, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, runtime_id, runtime_kind, runtime_profile, agent_id, attempt_id, lease_id,
  container_id, container_name, image, network, state, workspace_path, home_path,
  checks_json, env_keys_json, last_error, cleanup_state, cleanup_reason, cleanup_error,
  cleanup_owner, cleanup_attempts, removed_at, created_at, updated_at
FROM runtime_instances
ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list runtime instances: %w", err)
	}
	defer rows.Close()
	var out []RuntimeInstance
	for rows.Next() {
		var instance RuntimeInstance
		var checksJSON, envKeysJSON, removedAt, createdAt, updatedAt string
		if err := rows.Scan(
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
			&instance.CleanupState,
			&instance.CleanupReason,
			&instance.CleanupError,
			&instance.CleanupOwner,
			&instance.CleanupAttempts,
			&removedAt,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		if checksJSON != "" {
			if err := json.Unmarshal([]byte(checksJSON), &instance.Checks); err != nil {
				return nil, err
			}
		}
		if envKeysJSON != "" {
			if err := json.Unmarshal([]byte(envKeysJSON), &instance.EnvKeys); err != nil {
				return nil, err
			}
		}
		var err error
		instance.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		instance.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		if removedAt != "" {
			value, err := parseTime(removedAt)
			if err != nil {
				return nil, err
			}
			instance.RemovedAt = &value
		}
		out = append(out, instance)
	}
	return out, rows.Err()
}

func DockerSafeName(value string) string {
	lower := strings.ToLower(value)
	re := regexp.MustCompile(`[^a-z0-9_.-]+`)
	clean := strings.Trim(re.ReplaceAllString(lower, "-"), "-_.")
	if clean == "" {
		return "coordplane-runtime"
	}
	if len(clean) > 120 {
		return clean[:120]
	}
	return clean
}

func validateDockerMounts(mounts []DockerMount, allowedSources []string, dbPath string) error {
	for _, mount := range mounts {
		if mount.Source == "" || mount.Target == "" {
			return errors.New("docker runtime: mount source and target are required")
		}
		if !dockerTargetAllowed(mount.Target) {
			return fmt.Errorf("docker runtime: forbidden mount target %q", mount.Target)
		}
		cleanSource := filepath.Clean(mount.Source)
		if cleanSource == "/var/run/docker.sock" || cleanSource == "/run/docker.sock" {
			return errors.New("docker runtime: docker socket mount is forbidden")
		}
		if hostCredentialSourceForbidden(cleanSource) {
			return fmt.Errorf("docker runtime: host credential source %q is forbidden", mount.Source)
		}
		if dbPath != "" && sameOrChild(dbPath, cleanSource) {
			return errors.New("docker runtime: backend database path mount is forbidden")
		}
		allowed := false
		for _, allowedSource := range allowedSources {
			if sameOrChild(cleanSource, allowedSource) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("docker runtime: mount source %q is outside allowlist", mount.Source)
		}
	}
	return nil
}

func dockerTargetAllowed(target string) bool {
	switch target {
	case ContainerWorkspacePath, ContainerHomePath, ContainerCoordlinkPath:
		return true
	default:
		return false
	}
}

func hostCredentialSourceForbidden(source string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if filepath.Clean(source) == filepath.Clean(home) {
		return true
	}
	for _, forbidden := range []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".config"),
		filepath.Join(home, ".ssh"),
	} {
		if sameOrChild(source, forbidden) {
			return true
		}
	}
	return false
}

func sameOrChild(path, parent string) bool {
	cleanPath := filepath.Clean(path)
	cleanParent := filepath.Clean(parent)
	if cleanPath == cleanParent {
		return true
	}
	rel, err := filepath.Rel(cleanParent, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safePathPart(value string) string {
	clean := DockerSafeName(value)
	if clean == "" {
		return "unknown"
	}
	return clean
}

func shortID(value string) string {
	value = DockerSafeName(value)
	if len(value) <= 12 {
		return value
	}
	return value[len(value)-12:]
}

func sortedStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out[key] = values[key]
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func looksLikeClaudeAuthFailure(raw []byte) bool {
	normalized := strings.ToLower(string(raw))
	return strings.Contains(normalized, "not logged in") ||
		strings.Contains(normalized, "please run /login") ||
		strings.Contains(normalized, "authentication") ||
		strings.Contains(normalized, "auth required")
}

func redactedAuthError(cause error) string {
	if cause == nil {
		return ""
	}
	message := cause.Error()
	if strings.Contains(message, ":") {
		return strings.TrimSpace(strings.SplitN(message, ":", 2)[0])
	}
	return message
}
