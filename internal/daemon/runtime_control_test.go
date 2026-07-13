package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/transport"
)

func TestRunControlValidationRequiresOwnedIdentityAndTokenHash(t *testing.T) {
	root, _, run := newRunControlFixture(t)
	called := false
	err := validateRunControl(context.Background(), root, run, func(_ context.Context, token string, scope core.RunScope) error {
		called = true
		if token != "token-control" || scope != (core.RunScope{
			ProjectID: run.ProjectID, AgentID: run.AgentID, TaskID: run.TaskID,
			RunID: run.ID, Generation: run.Generation,
		}) {
			t.Fatalf("authorization input token=%q scope=%#v", token, scope)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("valid run control rejected: called=%t err=%v", called, err)
	}
}

func TestCloseControlIsIdempotentAcrossConcurrentConvergence(t *testing.T) {
	root := t.TempDir()
	server, err := transport.NewUnixServer(root, filepath.Join(root, "api.sock"), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	control := &runControl{
		server: server, done: make(chan error, 1), outcome: make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	go func() { control.done <- server.Serve() }()
	controller := &runtimeController{controls: map[string]*runControl{"run-close": control}}

	errorsByCaller := make([]error, 2)
	var wait sync.WaitGroup
	for index := range errorsByCaller {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errorsByCaller[index] = controller.closeControl("run-close", control)
		}(index)
	}
	wait.Wait()
	for index, err := range errorsByCaller {
		if err != nil {
			t.Fatalf("close caller %d: %v", index, err)
		}
	}
	if len(controller.controls) != 0 {
		t.Fatalf("closed control remained registered: %#v", controller.controls)
	}
}

func TestRunControlValidationRejectsFilesystemAndScopeDrift(t *testing.T) {
	tests := map[string]func(*testing.T, string, string, core.Run){
		"directory mode": func(t *testing.T, _, path string, _ core.Run) {
			if err := os.Chmod(path, 0o770); err != nil {
				t.Fatal(err)
			}
		},
		"directory symlink": func(t *testing.T, root, path string, _ core.Run) {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "other-control")
			if err := os.Mkdir(target, runControlDirectoryMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"token mode": func(t *testing.T, _, path string, _ core.Run) {
			if err := os.Chmod(filepath.Join(path, "token"), 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"token symlink": func(t *testing.T, _, path string, _ core.Run) {
			if err := os.Remove(filepath.Join(path, "token")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(runControlMarkerName, filepath.Join(path, "token")); err != nil {
				t.Fatal(err)
			}
		},
		"token hardlink": func(t *testing.T, _, path string, _ core.Run) {
			if err := os.Link(filepath.Join(path, "token"), filepath.Join(path, "token-copy")); err != nil {
				t.Fatal(err)
			}
		},
		"token framing": func(t *testing.T, _, path string, _ core.Run) {
			if err := writeRuntimeFile(filepath.Join(path, "token"), []byte("token-control\nsecond\n"), runControlFileMode); err != nil {
				t.Fatal(err)
			}
		},
		"marker mismatch": func(t *testing.T, _, path string, run core.Run) {
			run.Generation++
			if err := writeRunControlMarker(path, run); err != nil {
				t.Fatal(err)
			}
		},
		"marker trailing content": func(t *testing.T, _, path string, run core.Run) {
			if err := writeRunControlMarker(path, run); err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(path, runControlMarkerName)
			raw, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeRuntimeFile(markerPath, append(raw, []byte("{}")...), runControlFileMode); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root, path, run := newRunControlFixture(t)
			mutate(t, root, path, run)
			called := false
			err := validateRunControl(context.Background(), root, run, func(context.Context, string, core.RunScope) error {
				called = true
				return nil
			})
			if !errors.Is(err, containerruntime.ErrOwnership) {
				t.Fatalf("control drift error = %v", err)
			}
			if called {
				t.Fatal("invalid control files reached token authorization")
			}
		})
	}

	t.Run("token hash or scope", func(t *testing.T) {
		root, _, run := newRunControlFixture(t)
		sentinel := errors.New("token hash mismatch")
		err := validateRunControl(context.Background(), root, run, func(context.Context, string, core.RunScope) error {
			return sentinel
		})
		if !errors.Is(err, containerruntime.ErrOwnership) {
			t.Fatalf("token hash mismatch error = %v", err)
		}
	})
}

func newRunControlFixture(t *testing.T) (string, string, core.Run) {
	t.Helper()
	root := t.TempDir()
	run := core.Run{
		ID: "run-control", ProjectID: "project-control", TaskID: "task-control", AgentID: "agent-control",
		Generation: 3, LaunchNonce: "nonce-control",
	}
	path := filepath.Join(root, run.ID)
	if err := os.Mkdir(path, runControlDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := writeRunControlMarker(path, run); err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeFile(filepath.Join(path, "token"), []byte("token-control\n"), runControlFileMode); err != nil {
		t.Fatal(err)
	}
	return root, path, run
}
