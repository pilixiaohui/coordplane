package testsupport

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func WriteFile(t testing.TB, path string, content []byte, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func RequireNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func GitCommand(t testing.TB, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func Git(t testing.TB, directory string, args ...string) string {
	t.Helper()
	return GitCommand(t, append([]string{"-C", directory}, args...)...)
}

func GitDir(t testing.TB, directory string, args ...string) string {
	t.Helper()
	return GitCommand(t, append([]string{"--git-dir=" + directory}, args...)...)
}

func RepositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func CreateGitRepository(t testing.TB, root, name, email string) string {
	t.Helper()
	path := filepath.Join(root, "source")
	RequireNoError(t, os.MkdirAll(path, 0o700))
	Git(t, path, "init", "-b", "main")
	Git(t, path, "config", "user.email", email)
	Git(t, path, "config", "user.name", name)
	WriteFile(t, filepath.Join(path, "README.md"), []byte("initial\n"), 0o600)
	Git(t, path, "add", "README.md")
	Git(t, path, "commit", "-m", "initial")
	return path
}
