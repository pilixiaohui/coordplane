package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/transport"

	_ "modernc.org/sqlite"
)

func TestDaemonReadyIsServedOnlyOnOperatorUnixSocket(t *testing.T) {
	root := t.TempDir()
	configPath := writeTestConfig(t, root)
	daemon, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx) }()
	socketPath := filepath.Join(root, "data", "operator.sock")
	client, err := transport.NewUnixClient(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var status core.Status
	for {
		err = client.JSON(context.Background(), "GET", "/v1/status", nil, &status)
		if err == nil && status.DaemonReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready: status=%s err=%v", mustJSON(status), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Runtime == nil || status.Runtime.WorkspaceQuotaEnabled ||
		!strings.Contains(status.Runtime.WorkspaceQuotaReason, "host bind mount") ||
		status.Runtime.TmpfsLimitBytes != runtimeTmpfsLimit {
		t.Fatalf("runtime quota status = %#v", status.Runtime)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("operator socket mode = %o", info.Mode().Perm())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operator socket survived shutdown: %v", err)
	}
}

func TestGT00CompositionRegistersAndReconcilesRealProject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	configPath := writeTestConfig(t, root)
	source := createSourceRepository(t, root)
	components, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := components.service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Integrator", AdapterID: "codex", Image: "agent:latest",
		InstructionsFile: filepath.Join(root, "instructions.md"), RequestID: "add-integrator",
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := components.service.AddProject(ctx, core.AddProjectInput{
		Name: "real-project", Source: source, SourceRef: "refs/heads/main",
		IntegrationAgentID: agent.ID, RequestID: "add-real-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != core.ProjectActive || project.InitialSHA == "" {
		t.Fatalf("project = %#v", project)
	}
	actual := gitOutput(t, "--git-dir", project.ControlRepoPath, "rev-parse", "refs/heads/main^{commit}")
	if actual != project.InitialSHA {
		t.Fatalf("control canonical = %s, initial = %s", actual, project.InitialSHA)
	}
	gitOutput(t, "--git-dir", project.ControlRepoPath, "fsck", "--connectivity-only")
	if err := os.WriteFile(filepath.Join(source, "advanced.txt"), []byte("advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, "add", "advanced.txt")
	gitIn(t, source, "commit", "-m", "advance canonical")
	advanced := strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD"))
	gitOutput(t, "--git-dir="+project.ControlRepoPath,
		"-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", source, advanced)
	gitOutput(t, "--git-dir="+project.ControlRepoPath,
		"update-ref", project.CanonicalRef, advanced, project.InitialSHA)
	if err := components.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.store.Project(ctx, project.ID)
	if err != nil || persisted.Status != core.ProjectActive || persisted.InitialSHA != project.InitialSHA || persisted.CanonicalSHA != advanced {
		t.Fatalf("persisted project = %#v err=%v", persisted, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	missingRepo := filepath.Join(root, "missing-control.git")
	if err := os.Rename(project.ControlRepoPath, missingRepo); err != nil {
		t.Fatal(err)
	}
	degraded, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer degraded.Close()
	persisted, err = degraded.store.Project(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != core.ProjectError || persisted.LastError == "" {
		t.Fatalf("missing active repo did not fail closed: %#v", persisted)
	}
	if persisted.CanonicalSHA != advanced {
		t.Fatalf("missing active repo rewrote cached canonical: got %q want %q", persisted.CanonicalSHA, advanced)
	}
	if _, err := degraded.service.RepairProject(ctx, project.ID, "repair-missing-active-repo"); !core.IsCode(err, core.CodeGitInvariantViolation) {
		t.Fatalf("repair missing formerly-active repo error = %v, want %s", err, core.CodeGitInvariantViolation)
	}
	if _, err := os.Stat(project.ControlRepoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair recreated missing formerly-active repo: %v", err)
	}
	persisted, err = degraded.store.Project(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != core.ProjectError || persisted.CanonicalSHA != advanced {
		t.Fatalf("failed repair changed formerly-active project: %#v", persisted)
	}
	if err := degraded.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(missingRepo, project.ControlRepoPath); err != nil {
		t.Fatal(err)
	}
	restored, err := buildComponents(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	repaired, err := restored.service.RepairProject(ctx, project.ID, "repair-restored-active-repo")
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != core.ProjectActive || repaired.CanonicalSHA != advanced || repaired.InitialSHA != project.InitialSHA {
		t.Fatalf("verified restored project = %#v, want actual canonical %s", repaired, advanced)
	}
	if got := gitOutput(t, "--git-dir="+project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != advanced {
		t.Fatalf("restored canonical = %s, want %s", got, advanced)
	}
}

func TestGT00CompositionQuarantinesRepositoryWithoutProjectRow(t *testing.T) {
	root := t.TempDir()
	configPath := writeTestConfig(t, root)
	reposRoot := filepath.Join(root, "data", "repos")
	orphan := filepath.Join(reposRoot, "orphan.git")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "sentinel"), []byte("unowned\n"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 0 {
		t.Fatalf("unowned repository was adopted: %#v", snapshot.Projects)
	}
}

func TestCompositionRejectsSecondDaemonAndLegacyDatabase(t *testing.T) {
	t.Run("single data dir", func(t *testing.T) {
		root := t.TempDir()
		configPath := writeTestConfig(t, root)
		first, err := buildComponents(context.Background(), configPath)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		second, err := buildComponents(context.Background(), configPath)
		if second != nil {
			_ = second.Close()
			t.Fatal("second daemon acquired the same data_dir")
		}
		if !errors.Is(err, ErrDataDirLocked) {
			t.Fatalf("second daemon error = %v", err)
		}
	})

	t.Run("legacy schema releases lock", func(t *testing.T) {
		root := t.TempDir()
		configPath := writeTestConfig(t, root)
		dataDir := filepath.Join(root, "data")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dataDir, "coordplane.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE queue_items(id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		components, err := buildComponents(context.Background(), configPath)
		if components != nil {
			_ = components.Close()
			t.Fatal("legacy database became ready")
		}
		if !core.IsCode(err, core.CodeLegacySchemaRebuildRequired) {
			t.Fatalf("legacy error = %v", err)
		}
		lock, err := AcquireDataDirLock(dataDir)
		if err != nil {
			t.Fatalf("startup failure leaked data-dir lock: %v", err)
		}
		_ = lock.Close()
	})
}

func TestCompositionRejectsUnsafeDataDirectoriesBeforeStoreOpen(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		want    string
	}{
		{
			name: "configured directory lacks owner execute",
			prepare: func(t *testing.T, root string) {
				path := filepath.Join(root, "data", "workspaces")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "owner must have rwx permissions",
		},
		{
			name: "configured directory is group writable",
			prepare: func(t *testing.T, root string) {
				path := filepath.Join(root, "data", "agent-homes")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o770); err != nil {
					t.Fatal(err)
				}
			},
			want: "must not be group/other writable",
		},
		{
			name: "generated directory is a symlink",
			prepare: func(t *testing.T, root string) {
				dataDir := filepath.Join(root, "data")
				outside := filepath.Join(root, "outside-run-control")
				if err := os.MkdirAll(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dataDir, "run-control")); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a symlink",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := writeTestConfig(t, root)
			test.prepare(t, root)
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
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func createSourceRepository(t *testing.T, root string) string {
	t.Helper()
	repository := filepath.Join(root, "source")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repository, "init", "-b", "main")
	gitIn(t, repository, "config", "user.email", "tests@coordplane.local")
	gitIn(t, repository, "config", "user.name", "CoordPlane Tests")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	command := exec.Command("git", args...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
	return string(bytesTrimSpace(raw))
}

func bytesTrimSpace(raw []byte) []byte {
	start, end := 0, len(raw)
	for start < end && (raw[start] == ' ' || raw[start] == '\n' || raw[start] == '\r' || raw[start] == '\t') {
		start++
	}
	for end > start && (raw[end-1] == ' ' || raw[end-1] == '\n' || raw[end-1] == '\r' || raw[end-1] == '\t') {
		end--
	}
	return raw[start:end]
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
