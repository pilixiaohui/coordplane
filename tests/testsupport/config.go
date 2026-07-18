package testsupport

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"
)

type RuntimeConfigFixture struct {
	DataDir, OperatorSocket, WorkspaceRoot, AgentHomeRoot, LogRoot, CompletedWorkspace, TerminalTaskRef, RunLog, DockerNetwork, DefaultImage, Tail string
	MaxParallelRuns                                                                                                                                int
	ProviderEnv                                                                                                                                    []string
}

func RuntimeConfigYAML(f RuntimeConfigFixture) []byte {
	f.WorkspaceRoot = cmp.Or(f.WorkspaceRoot, filepath.Join(f.DataDir, "workspaces"))
	f.AgentHomeRoot = cmp.Or(f.AgentHomeRoot, filepath.Join(f.DataDir, "agent-homes"))
	f.LogRoot = cmp.Or(f.LogRoot, filepath.Join(f.DataDir, "logs"))
	allowlist := " []"
	if len(f.ProviderEnv) > 0 {
		allowlist = "\n    - " + strings.Join(f.ProviderEnv, "\n    - ")
	}
	return []byte(fmt.Sprintf(`data_dir: %s
operator_socket: %s
max_parallel_runs: %d
retention:
  completed_workspace: %s
  terminal_task_ref: %s
  run_log: %s
runtime:
  docker_network: %s
  workspace_root: %s
  agent_home_root: %s
  log_root: %s
  default_image: %s
  provider_env_allowlist:%s
%s`, f.DataDir, f.OperatorSocket, f.MaxParallelRuns, f.CompletedWorkspace, f.TerminalTaskRef, f.RunLog,
		f.DockerNetwork, f.WorkspaceRoot, f.AgentHomeRoot, f.LogRoot, f.DefaultImage, allowlist, f.Tail))
}
