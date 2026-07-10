package backend

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coordplane/internal/coordination"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestBackendCloseDrainsActiveRuntimeCleanupBeforeClosingDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "coordplane.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open lifecycle SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("migrate lifecycle SQLite: %v", err)
	}
	manager := newDrainBarrierDockerManager()
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:          db,
		ProfileName: "docker-default",
		TeamID:      "backend-close-drain",
		Ready:       true,
		Docker:      manager,
	})
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordination.NewService(st),
		TeamConfig:      backendCloseDrainTeamConfig(),
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-default": dockerRuntime},
		Adapter:         cpruntime.NewFakeCLIAdapter(),
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("new lifecycle runner: %v", err)
	}
	seedBackendCloseDrainCleanup(t, ctx, db)

	app := &Backend{DB: db, Runner: runner}
	app.startRuntimeCleanupReconciler(5 * time.Millisecond)
	t.Cleanup(func() {
		manager.Release()
		_ = app.Close()
	})

	select {
	case <-manager.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime cleanup did not enter Docker manager barrier")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- app.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Backend.Close returned before active cleanup drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	var cleanupState string
	if err := db.QueryRowContext(ctx, `SELECT cleanup_state FROM runtime_instances WHERE id = 'ri_backend_close_drain'`).Scan(&cleanupState); err != nil {
		t.Fatalf("database became unavailable while Close waited for cleanup: %v", err)
	}
	if cleanupState != "in_progress" {
		t.Fatalf("cleanup state at drain barrier = %s, want in_progress", cleanupState)
	}

	manager.Release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Backend.Close after cleanup drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Backend.Close did not return after cleanup barrier release")
	}
	select {
	case <-app.runtimeCleanupDone:
	default:
		t.Fatal("runtime cleanup goroutine did not stop before Backend.Close returned")
	}
	if err := db.PingContext(ctx); err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
		t.Fatalf("database ping after Backend.Close = %v, want closed database", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("repeated Backend.Close: %v", err)
	}
	if got := manager.inspectCalls.Load(); got != 1 {
		t.Fatalf("Docker inspect calls after repeated Close = %d, want 1", got)
	}
	if got := manager.removeCalls.Load(); got != 0 {
		t.Fatalf("Docker remove calls for absent container = %d, want 0", got)
	}

	reopened, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen lifecycle SQLite: %v", err)
	}
	defer reopened.Close()
	var state string
	if err := reopened.QueryRowContext(ctx, `SELECT cleanup_state FROM runtime_instances WHERE id = 'ri_backend_close_drain'`).Scan(&state); err != nil {
		t.Fatalf("read cleanup state after Backend.Close: %v", err)
	}
	if state != "removed" {
		t.Fatalf("cleanup state after Backend.Close = %s, want removed", state)
	}
	var removedEvents int
	if err := reopened.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event_type = 'runtime.cleanup_removed' AND aggregate_id = 'rt_backend_close_drain'`).Scan(&removedEvents); err != nil {
		t.Fatalf("count removed events after Backend.Close: %v", err)
	}
	if removedEvents != 1 {
		t.Fatalf("runtime.cleanup_removed events = %d, want 1", removedEvents)
	}
}

type drainBarrierDockerManager struct {
	entered      chan struct{}
	release      chan struct{}
	enterOnce    sync.Once
	releaseOnce  sync.Once
	inspectCalls atomic.Int32
	removeCalls  atomic.Int32
}

func newDrainBarrierDockerManager() *drainBarrierDockerManager {
	return &drainBarrierDockerManager{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (m *drainBarrierDockerManager) PrepareContainer(context.Context, cpruntime.DockerContainerSpec) (cpruntime.DockerContainerResult, error) {
	return cpruntime.DockerContainerResult{}, errors.New("prepare is not used by cleanup lifecycle test")
}

func (m *drainBarrierDockerManager) InspectContainer(ctx context.Context, _ string) (map[string]string, error) {
	m.inspectCalls.Add(1)
	m.enterOnce.Do(func() { close(m.entered) })
	select {
	case <-m.release:
		return nil, os.ErrNotExist
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *drainBarrierDockerManager) RemoveContainer(context.Context, string) error {
	m.removeCalls.Add(1)
	return nil
}

func (m *drainBarrierDockerManager) Release() {
	m.releaseOnce.Do(func() { close(m.release) })
}

func backendCloseDrainTeamConfig() teamconfig.Config {
	return teamconfig.Config{
		TeamID:  "backend-close-drain",
		Version: 1,
		RuntimeProfiles: map[string]teamconfig.RuntimeProfile{
			"docker-default": {Kind: "docker", Image: "alpine:3.20", WorkspaceMode: "isolated"},
		},
	}
}

func seedBackendCloseDrainCleanup(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	for _, statement := range []string{
		`INSERT INTO work_contracts (id, title, objective, target_kind, target_id, status, created_at, updated_at) VALUES ('ctr_backend_close_drain', 'close drain', 'drain cleanup before database close', 'agent', 'developer', 'active', '` + now + `', '` + now + `')`,
		`INSERT INTO assignments (id, contract_id, assignee_agent_id, state, reason, created_at, updated_at) VALUES ('asg_backend_close_drain', 'ctr_backend_close_drain', 'developer', 'returned', 'close drain', '` + now + `', '` + now + `')`,
		`INSERT INTO leases (id, assignment_id, agent_id, state, expires_at, created_at, updated_at) VALUES ('lease_backend_close_drain', 'asg_backend_close_drain', 'developer', 'released', '` + now + `', '` + now + `', '` + now + `')`,
		`INSERT INTO attempts (id, lease_id, cli_backend, runtime_kind, start_reason, status, started_at, ended_at) VALUES ('att_backend_close_drain', 'lease_backend_close_drain', 'fake', 'docker', 'close drain', 'failed', '` + now + `', '` + now + `')`,
		`INSERT INTO runtime_instances (id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id, lease_id, container_id, container_name, image, network, state, workspace_path, home_path, cleanup_state, cleanup_reason, created_at, updated_at) VALUES ('ri_backend_close_drain', 'rt_backend_close_drain', 'docker-default', 'docker', 'developer', 'att_backend_close_drain', 'lease_backend_close_drain', 'container-backend-close-drain', 'coordplane-backend-close-drain', 'alpine:3.20', 'bridge', 'stopped', '/workspace', '/home/agent', 'pending', 'backend close drain', '` + now + `', '` + now + `')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed backend close drain cleanup: %v", err)
		}
	}
}
