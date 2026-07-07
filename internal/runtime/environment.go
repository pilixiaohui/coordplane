package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"coordplane/internal/claudeenv"
	"coordplane/internal/ids"
)

var allowedRuntimeEnv = buildAllowedRuntimeEnv()

func buildAllowedRuntimeEnv() map[string]bool {
	env := map[string]bool{
		"COORDPLANE_BACKEND_URL":  true,
		"COORDPLANE_AGENT_ID":     true,
		"COORDPLANE_RUNTIME_ID":   true,
		"COORDPLANE_TOKEN":        true,
		"COORDPLANE_WORKSPACE":    true,
		"COORDPLANE_TRACE_ID":     true,
		"COORDPLANE_ASSIGNMENT":   true,
		"COORDPLANE_LEASE_ID":     true,
		"COORDPLANE_ATTEMPT_ID":   true,
		"COORDPLANE_CLI_BACKEND":  true,
		"COORDPLANE_TEAM":         true,
		"COORDPLANE_WORKSPACE_ID": true,
	}
	for _, key := range claudeenv.RuntimeKeys {
		env[key] = true
	}
	return env
}

func BuildRuntimeEnv(in EnvironmentInput) (map[string]string, error) {
	if in.BackendURL == "" || in.AgentID == "" || in.RuntimeID == "" ||
		in.AttemptID == "" || in.AssignmentID == "" || in.LeaseID == "" ||
		in.Workspace == "" || in.CLIBackend == "" || in.TeamID == "" ||
		in.WorkspaceName == "" {
		return nil, errors.New("runtime env: backend, identity, workspace, cli backend, team, and workspace name are required")
	}
	traceID, err := ids.New("trace")
	if err != nil {
		return nil, err
	}
	token, err := ids.New("tok")
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		"COORDPLANE_BACKEND_URL":  in.BackendURL,
		"COORDPLANE_AGENT_ID":     in.AgentID,
		"COORDPLANE_RUNTIME_ID":   in.RuntimeID,
		"COORDPLANE_TOKEN":        token,
		"COORDPLANE_WORKSPACE":    in.Workspace,
		"COORDPLANE_TRACE_ID":     traceID,
		"COORDPLANE_ASSIGNMENT":   in.AssignmentID,
		"COORDPLANE_LEASE_ID":     in.LeaseID,
		"COORDPLANE_ATTEMPT_ID":   in.AttemptID,
		"COORDPLANE_CLI_BACKEND":  in.CLIBackend,
		"COORDPLANE_TEAM":         in.TeamID,
		"COORDPLANE_WORKSPACE_ID": in.WorkspaceName,
	}
	if err := ValidateRuntimeEnv(env); err != nil {
		return nil, err
	}
	return env, nil
}

func ValidateRuntimeEnv(env map[string]string) error {
	for key, value := range env {
		if !allowedRuntimeEnv[key] {
			return fmt.Errorf("runtime env: forbidden key %q", key)
		}
		upperKey := strings.ToUpper(key)
		for _, denied := range []string{"DB_PATH", "RUNTIME_ROOT", "DATABASE_PATH"} {
			if strings.Contains(upperKey, denied) {
				return fmt.Errorf("runtime env: forbidden key %q", key)
			}
		}
		if key != "COORDPLANE_BACKEND_URL" && key != "COORDPLANE_WORKSPACE" && strings.TrimSpace(value) == "" {
			return fmt.Errorf("runtime env: key %q is empty", key)
		}
	}
	return nil
}

func RuntimeEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func RuntimeTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
