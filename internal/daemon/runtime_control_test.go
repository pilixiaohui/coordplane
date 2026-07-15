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
	requireNoError(t, err)
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
			requireNoError(t, os.Chmod(path, 0o770))
		},
		"directory symlink": func(t *testing.T, root, path string, _ core.Run) {
			requireNoError(t, os.RemoveAll(path))
			target := filepath.Join(root, "other-control")
			requireNoError(t, os.Mkdir(target, runControlDirectoryMode))
			requireNoError(t, os.Symlink(target, path))
		},
		"token mode": func(t *testing.T, _, path string, _ core.Run) {
			requireNoError(t, os.Chmod(filepath.Join(path, "token"), 0o640))
		},
		"token symlink": func(t *testing.T, _, path string, _ core.Run) {
			requireNoError(t, os.Remove(filepath.Join(path, "token")))
			requireNoError(t, os.Symlink(runControlMarkerName, filepath.Join(path, "token")))
		},
		"token hardlink": func(t *testing.T, _, path string, _ core.Run) {
			requireNoError(t, os.Link(filepath.Join(path, "token"), filepath.Join(path, "token-copy")))
		},
		"token framing": func(t *testing.T, _, path string, _ core.Run) {
			requireNoError(t, writeRuntimeFile(filepath.Join(path, "token"), []byte("token-control\nsecond\n"), runControlFileMode))
		},
		"marker mismatch": func(t *testing.T, _, path string, run core.Run) {
			run.Generation++
			requireNoError(t, writeRunControlMarker(path, run))
		},
		"marker trailing content": func(t *testing.T, _, path string, run core.Run) {
			requireNoError(t, writeRunControlMarker(path, run))
			markerPath := filepath.Join(path, runControlMarkerName)
			raw, err := os.ReadFile(markerPath)
			requireNoError(t, err)
			requireNoError(t, writeRuntimeFile(markerPath, append(raw, []byte("{}")...), runControlFileMode))
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

func TestRunControlRemovalRevalidatesDurableIdentity(t *testing.T) {
	root, path, run := newRunControlFixture(t)
	other := run
	other.Generation++
	requireNoError(t, writeRunControlMarker(path, other))
	if err := removeRunControl(root, run); !errors.Is(err, containerruntime.ErrOwnership) {
		t.Fatalf("remove drifted control error = %v, want ownership failure", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("drifted run control was deleted: %v", err)
	}
}

func newRunControlFixture(t *testing.T) (string, string, core.Run) {
	t.Helper()
	root := t.TempDir()
	run := core.Run{
		ID: "run-control", ProjectID: "project-control", TaskID: "task-control", AgentID: "agent-control",
		Generation: 3, LaunchNonce: "nonce-control",
	}
	path := filepath.Join(root, run.ID)
	requireNoError(t, os.Mkdir(path, runControlDirectoryMode))
	requireNoError(t, writeRunControlMarker(path, run))
	requireNoError(t, writeRuntimeFile(filepath.Join(path, "token"), []byte("token-control\n"), runControlFileMode))
	return root, path, run
}
