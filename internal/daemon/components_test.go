package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestGT00CompositionQuarantinesRepositoryWithoutProjectRow(t *testing.T) {
	root := t.TempDir()
	configPath := writeTestConfig(t, root)
	reposRoot := filepath.Join(root, "data", "repos")
	orphan := filepath.Join(reposRoot, "orphan.git")
	requireNoError(t, os.MkdirAll(orphan, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(orphan, "sentinel"), []byte("unowned\n"), 0o600))

	components, err := buildComponents(context.Background(), configPath)
	if components != nil {
		_ = components.Close()
		t.Fatal("daemon became ready in the same startup that quarantined an unowned repository")
	}
	if err == nil || !strings.Contains(err.Error(), "orphan.git") {
		t.Fatalf("quarantine startup error = %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unowned repository remains active: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(reposRoot, ".quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine entries=%d err=%v", len(entries), err)
	}
	components, err = buildComponents(context.Background(), configPath)
	if err != nil {
		t.Fatalf("restart after quarantine: %v", err)
	}
	defer components.Close()
	snapshot, err := components.store.Snapshot(context.Background(), "")
	requireNoError(t, err)
	if len(snapshot.Projects) != 0 {
		t.Fatalf("unowned repository was adopted: %#v", snapshot.Projects)
	}
}

func TestCompositionRejectsUnsafeDataDirectoriesBeforeStoreOpen(t *testing.T) {
	tests := []struct {
		name, path, want string
		mode             os.FileMode
		symlink          bool
	}{
		{"configured directory lacks owner execute", "workspaces", "owner must have rwx permissions", 0o600, false},
		{"configured directory is group writable", "agent-homes", "must not be group/other writable", 0o770, false},
		{"generated directory is a symlink", "run-control", "not a symlink", 0, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := writeTestConfig(t, root)
			path := filepath.Join(root, "data", test.path)
			requireNoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			if test.symlink {
				outside := filepath.Join(root, "outside-"+test.path)
				requireNoError(t, os.MkdirAll(outside, 0o700))
				requireNoError(t, os.Symlink(outside, path))
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			} else if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			components, err := buildComponents(context.Background(), configPath)
			if components != nil {
				_ = components.Close()
				t.Fatal("unsafe data directory reached a usable composition")
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("startup error = %v, want containing %q", err, test.want)
			}
			dataDir := filepath.Join(root, "data")
			if _, statErr := os.Stat(filepath.Join(dataDir, "coordplane.db")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unsafe startup created database: %v", statErr)
			}
			lock, lockErr := AcquireDataDirLock(dataDir)
			if lockErr != nil {
				t.Fatalf("unsafe startup leaked data-dir lock: %v", lockErr)
			}
			_ = lock.Close()
		})
	}
}

func writeTestConfig(t *testing.T, root string) string {
	t.Helper()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "coordplane.yaml")
	raw := fmt.Sprintf(`data_dir: %s
operator_socket: %s
max_parallel_runs: 4
retention:
  completed_workspace: 24h
  terminal_task_ref: 168h
  run_log: 168h
runtime:
  docker_network: coordplane
  workspace_root: %s
  agent_home_root: %s
  log_root: %s
  default_image: coordplane-agent:latest
  provider_env_allowlist: []
`, dataDir, filepath.Join(dataDir, "operator.sock"), filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "agent-homes"), filepath.Join(dataDir, "logs"))
	requireNoError(t, os.WriteFile(configPath, []byte(raw), 0o600))
	return configPath
}

func createSourceRepository(t *testing.T, root string) string {
	t.Helper()
	repository := filepath.Join(root, "source")
	requireNoError(t, os.MkdirAll(repository, 0o700))
	gitIn(t, repository, "init", "-b", "main")
	gitIn(t, repository, "config", "user.email", "tests@coordplane.local")
	gitIn(t, repository, "config", "user.name", "CoordPlane Tests")
	requireNoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600))
	gitIn(t, repository, "add", "README.md")
	gitIn(t, repository, "commit", "-m", "initial")
	return repository
}

func gitIn(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
	return string(raw)
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitIn(t, ".", args...))
}
