package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/tests/testsupport"
	_ "modernc.org/sqlite"
)

var requireNoError = testsupport.RequireNoError
var gitIn = testsupport.Git
var gitOutput = testsupport.GitCommand

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
	return testsupport.WriteFile(t, configPath, testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: filepath.Join(dataDir, "operator.sock"), MaxParallelRuns: 4, CompletedWorkspace: "24h", TerminalTaskRef: "168h", RunLog: "168h", DockerNetwork: "coordplane", DefaultImage: "coordplane-agent:latest"}), 0o600)
}

func createSourceRepository(t *testing.T, root string) string {
	return testsupport.CreateGitRepository(t, root, "CoordPlane Tests", "tests@coordplane.local")
}
