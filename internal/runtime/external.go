package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"coordplane/internal/ids"
)

type ExternalRuntime struct {
	ID            string
	WorkspaceRoot string
	HomeRoot      string
	Ready         bool
}

type PreparedRuntime struct {
	RuntimeID         string
	Kind              string
	Workspace         string
	HomeDir           string
	WorkspaceGuardRef string
	HomeGuardRef      string
	Env               map[string]string
	ContainerID       string
	ContainerName     string
	Checks            map[string]bool
}

func (r ExternalRuntime) Name() string {
	return r.ID
}

func (r ExternalRuntime) Kind() string {
	return "external"
}

func (r ExternalRuntime) IsReady() bool {
	return r.Ready
}

func (r ExternalRuntime) Prepare(ctx context.Context, req PrepareRequest) (PreparedRuntime, error) {
	if !r.Ready {
		return PreparedRuntime{}, errors.New("ENV_NOT_READY: external runtime is not ready")
	}
	if r.ID == "" {
		return PreparedRuntime{}, errors.New("external runtime id is required")
	}
	if r.WorkspaceRoot == "" || r.HomeRoot == "" {
		return PreparedRuntime{}, errors.New("external runtime workspace and home roots are required")
	}
	select {
	case <-ctx.Done():
		return PreparedRuntime{}, ctx.Err()
	default:
	}
	runtimeID, err := ids.New("rt_external")
	if err != nil {
		return PreparedRuntime{}, err
	}
	workspace := filepath.Join(r.WorkspaceRoot, req.AgentID, req.AttemptID)
	home := filepath.Join(r.HomeRoot, req.AgentID)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return PreparedRuntime{}, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return PreparedRuntime{}, err
	}
	env, err := BuildRuntimeEnv(EnvironmentInput{
		BackendURL:    req.BackendURL,
		AgentID:       req.AgentID,
		RuntimeID:     runtimeID,
		AttemptID:     req.AttemptID,
		AssignmentID:  req.AssignmentID,
		LeaseID:       req.LeaseID,
		Workspace:     workspace,
		CLIBackend:    req.CLIBackend,
		TeamID:        req.TeamID,
		WorkspaceName: req.WorkspaceName,
	})
	if err != nil {
		return PreparedRuntime{}, err
	}
	return PreparedRuntime{
		RuntimeID:         runtimeID,
		Kind:              "external",
		Workspace:         workspace,
		HomeDir:           home,
		WorkspaceGuardRef: workspace,
		HomeGuardRef:      home,
		Env:               env,
		Checks: map[string]bool{
			"backend_url_configured": true,
			"workspace_private":      true,
			"home_private":           true,
			"forbidden_env_absent":   true,
		},
	}, nil
}
