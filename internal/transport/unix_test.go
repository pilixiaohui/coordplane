package transport_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

func TestUnixServerAndJSONClientUseTheSocketAndRemoveItOnClose(t *testing.T) {
	dataDir := t.TempDir()
	socketPath := filepath.Join(dataDir, "operator.sock")
	operations := &operatorFake{}
	server, err := transport.NewUnixServer(dataDir, socketPath, transport.NewOperatorHandler(operations))
	if err != nil {
		t.Fatalf("NewUnixServer: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
	})

	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("stat Unix socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("Unix socket mode = %v, want socket 0600", info.Mode())
	}

	client, err := transport.NewUnixClient(socketPath)
	if err != nil {
		t.Fatalf("NewUnixClient: %v", err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status core.Status
	if err := client.JSON(ctx, http.MethodGet, "/v1/status?project_id=prj-socket", nil, &status); err != nil {
		t.Fatalf("JSON over Unix socket: %v", err)
	}
	if len(operations.calls) != 1 || operations.calls[0].name != "status" || operations.calls[0].value != "prj-socket" {
		t.Fatalf("operator calls = %+v", operations.calls)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("close Unix server: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Unix server did not return after Close")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after Close: %v", err)
	}
}

func TestUnixClientForwardsBearerAndDecodesCoreError(t *testing.T) {
	dataDir := t.TempDir()
	socketPath := filepath.Join(dataDir, "api.sock")
	operations := &runFake{err: core.Conflict(core.CodeStaleRun, "stale", "exited", 4)}
	server, err := transport.NewUnixServer(dataDir, socketPath, transport.NewRunHandler(operations))
	if err != nil {
		t.Fatalf("NewUnixServer: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})
	client, err := transport.NewUnixClient(socketPath, transport.WithBearerToken("old-run-token"))
	if err != nil {
		t.Fatalf("NewUnixClient: %v", err)
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = client.JSON(ctx, http.MethodPost, "/v1/progress", map[string]string{"summary": "late"}, nil)
	var coreErr *core.Error
	if !errors.As(err, &coreErr) || coreErr.Code != core.CodeStaleRun || coreErr.State != "exited" || coreErr.Version != 4 {
		t.Fatalf("JSON error = %v, want STALE_RUN conflict", err)
	}
	if len(operations.calls) != 1 || operations.calls[0].token != "old-run-token" {
		t.Fatalf("run calls = %+v", operations.calls)
	}
}

func TestListenUnixReplacesOnlyAStaleSocketInsideOwnedDataDir(t *testing.T) {
	dataDir := t.TempDir()
	socketPath := filepath.Join(dataDir, "daemon.sock")
	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	unixStale, ok := stale.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", stale)
	}
	unixStale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stale socket path missing: %v", err)
	}

	listener, err := transport.ListenUnix(dataDir, socketPath)
	if err != nil {
		t.Fatalf("ListenUnix over stale socket: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close replacement listener: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement socket remains after Close: %v", err)
	}
}

func TestListenUnixRefusesLiveSocketRegularFileAndOutsidePath(t *testing.T) {
	dataDir := t.TempDir()
	livePath := filepath.Join(dataDir, "live.sock")
	live, err := transport.ListenUnix(dataDir, livePath)
	if err != nil {
		t.Fatalf("first ListenUnix: %v", err)
	}
	defer live.Close()
	if _, err := transport.ListenUnix(dataDir, livePath); err == nil {
		t.Fatal("ListenUnix replaced a live socket")
	}
	if info, err := os.Lstat(livePath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("live socket was removed: info=%v err=%v", info, err)
	}

	regularPath := filepath.Join(dataDir, "regular.sock")
	if err := os.WriteFile(regularPath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write regular sentinel: %v", err)
	}
	if _, err := transport.ListenUnix(dataDir, regularPath); err == nil {
		t.Fatal("ListenUnix replaced a regular file")
	}
	if raw, err := os.ReadFile(regularPath); err != nil || string(raw) != "keep" {
		t.Fatalf("regular sentinel changed: %q/%v", raw, err)
	}

	outsidePath := filepath.Join(t.TempDir(), "outside.sock")
	if _, err := transport.ListenUnix(dataDir, outsidePath); err == nil {
		t.Fatal("ListenUnix accepted a socket outside data_dir")
	}
}
