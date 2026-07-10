package runtime_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/claudeenv"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/secrets"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestDockerRuntimeGateRequiresExplicitEnvironment(t *testing.T) {
	if os.Getenv("COORDPLANE_DOCKER_GATE") != "1" {
		t.Skip("set COORDPLANE_DOCKER_GATE=1 with COORDPLANE_COORDLINK_PATH and COORDPLANE_BACKEND_URL to run the real Docker runtime gate")
	}
	coordlinkPath := os.Getenv("COORDPLANE_COORDLINK_PATH")
	backendURL := os.Getenv("COORDPLANE_BACKEND_URL")
	if coordlinkPath == "" || backendURL == "" {
		t.Skip("real Docker gate requires COORDPLANE_COORDLINK_PATH and COORDPLANE_BACKEND_URL")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	network := os.Getenv("COORDPLANE_DOCKER_NETWORK")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, cleanup := newIntegrationDB(t)
	defer cleanup()
	defer cleanupRuntimeContainers(t, db)
	runtimeRoot := filepath.Join(t.TempDir(), "docker-runtime")
	backend := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-gate",
		TeamID:        "docker-gate",
		Image:         image,
		Network:       network,
		RuntimeRoot:   runtimeRoot,
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
	})
	prepared, err := backend.Prepare(ctx, cpruntime.PrepareRequest{
		AgentID:        "developer",
		AttemptID:      "att_docker_gate",
		AssignmentID:   "asn_docker_gate",
		LeaseID:        "lease_docker_gate",
		ContractID:     "ctr_docker_gate",
		TeamID:         "docker-gate",
		RuntimeProfile: "docker-gate",
		CLIBackend:     "fake",
		BackendURL:     backendURL,
		WorkspaceName:  "docker-gate-workspace",
	})
	if err != nil {
		t.Fatalf("prepare docker runtime: %v", err)
	}
	if prepared.ContainerName != "" {
		t.Cleanup(func() {
			_ = exec.Command("docker", "rm", "-f", prepared.ContainerName).Run()
		})
	}
	if prepared.Workspace != cpruntime.ContainerWorkspacePath ||
		prepared.HomeDir != cpruntime.ContainerHomePath ||
		prepared.Env["COORDPLANE_TOKEN"] == "" ||
		prepared.ContainerID == "" {
		t.Fatalf("prepared docker runtime = %+v", prepared)
	}
	for _, name := range []string{"workspace_writable", "home_writable", "git_workspace_writable", "cli_user_consistent"} {
		if !prepared.Checks[name] {
			t.Fatalf("prepared docker checks = %#v, want %s", prepared.Checks, name)
		}
	}
}

func TestDockerManagedCleanupFileSQLiteGate(t *testing.T) {
	if os.Getenv("COORDPLANE_DOCKER_CLEANUP_GATE") != "1" {
		t.Skip("set COORDPLANE_DOCKER_CLEANUP_GATE=1 to run the real Docker/file-SQLite cleanup matrix")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	network := os.Getenv("COORDPLANE_DOCKER_NETWORK")
	if network == "" {
		network = "bridge"
	}
	for _, tc := range []struct {
		name                 string
		coordlinkExit        int
		wantState            string
		removedBeforeConfirm bool
	}{
		{name: "terminal success removes owned container", wantState: "stopped"},
		{name: "post-create check failure removes owned container", coordlinkExit: 7, wantState: "failed"},
		{name: "removed before database confirmation converges from NotFound", wantState: "stopped", removedBeforeConfirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "coordplane.db")
			db := newFileIntegrationDB(t, dbPath)
			defer db.Close()
			attemptID := "att_cleanup_gate"
			leaseID := "lease_cleanup_gate"
			seedDockerCleanupScope(t, ctx, db, attemptID, leaseID)
			coordlinkPath := filepath.Join(dir, "coordlink")
			coordlink := "#!/bin/sh\nexit 0\n"
			if tc.coordlinkExit != 0 {
				coordlink = "#!/bin/sh\nexit 7\n"
			}
			if err := os.WriteFile(coordlinkPath, []byte(coordlink), 0o755); err != nil {
				t.Fatalf("write cleanup gate coordlink: %v", err)
			}
			managed := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
				DB:            db,
				ProfileName:   "docker-cleanup-gate",
				TeamID:        "docker-cleanup-gate",
				Image:         image,
				Network:       network,
				RuntimeRoot:   dir + "-runtime",
				CoordlinkPath: coordlinkPath,
				DBPath:        dbPath,
				Ready:         true,
			})
			prepared, prepareErr := managed.Prepare(ctx, cpruntime.PrepareRequest{
				AgentID:        "developer",
				AttemptID:      attemptID,
				AssignmentID:   "asg_cleanup_gate",
				LeaseID:        leaseID,
				ContractID:     "ctr_cleanup_gate",
				TeamID:         "docker-cleanup-gate",
				RuntimeProfile: "docker-cleanup-gate",
				CLIBackend:     "fake",
				BackendURL:     "http://coordplane.invalid",
				WorkspaceName:  "docker-cleanup-gate",
			})
			var containerName string
			if err := db.QueryRowContext(ctx, `SELECT container_name FROM runtime_instances WHERE attempt_id = ?`, attemptID).Scan(&containerName); err != nil {
				t.Fatalf("query cleanup gate container name: %v (prepare error: %v)", err, prepareErr)
			}
			t.Cleanup(func() {
				_ = exec.Command("docker", "rm", "-f", containerName).Run()
			})
			if tc.coordlinkExit == 0 {
				if prepareErr != nil || prepared.ContainerID == "" {
					t.Fatalf("prepare managed container = %+v, err=%v", prepared, prepareErr)
				}
				now := time.Now().UTC().Format(time.RFC3339Nano)
				if _, err := db.ExecContext(ctx, `UPDATE attempts SET status = 'completed', ended_at = ? WHERE id = ?`, now, attemptID); err != nil {
					t.Fatalf("complete cleanup gate attempt: %v", err)
				}
				if _, err := db.ExecContext(ctx, `UPDATE leases SET state = 'released', updated_at = ? WHERE id = ?`, now, leaseID); err != nil {
					t.Fatalf("release cleanup gate lease: %v", err)
				}
				if tc.removedBeforeConfirm {
					expired := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
					if _, err := db.ExecContext(ctx, `
UPDATE runtime_instances
SET cleanup_state = 'in_progress', cleanup_reason = 'removed before confirmation',
  cleanup_owner = 'cleanup-crashed-gate', cleanup_lease_expires_at = ?,
  cleanup_attempts = 1, updated_at = ?
WHERE attempt_id = ?`, expired, expired, attemptID); err != nil {
						t.Fatalf("seed removed-before-confirm cleanup claim: %v", err)
					}
					if raw, err := exec.CommandContext(ctx, "docker", "rm", "-f", containerName).CombinedOutput(); err != nil {
						t.Fatalf("externally remove managed container before DB confirmation: %v: %s", err, raw)
					}
					if err := managed.ReconcileRuntimeCleanup(context.Background()); err != nil {
						t.Fatalf("reconcile removed-before-confirm managed container: %v", err)
					}
				} else if err := managed.FinalizeRuntime(context.Background(), attemptID, "real Docker cleanup gate complete"); err != nil {
					t.Fatalf("finalize managed container: %v", err)
				}
			} else if prepareErr == nil {
				t.Fatal("post-create coordlink check unexpectedly succeeded")
			}
			var state, cleanupState string
			if err := db.QueryRowContext(ctx, `SELECT state, cleanup_state FROM runtime_instances WHERE attempt_id = ?`, attemptID).Scan(&state, &cleanupState); err != nil {
				t.Fatalf("query cleanup gate runtime state: %v", err)
			}
			if state != tc.wantState || cleanupState != "removed" {
				t.Fatalf("cleanup gate runtime state = %s/%s, want %s/removed", state, cleanupState, tc.wantState)
			}
			if _, err := (cpruntime.DockerCLIClient{}).InspectContainer(ctx, containerName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("docker inspect after cleanup = %v, want NotFound", err)
			}
			var removedEvents int
			if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM events
WHERE event_type = 'runtime.cleanup_removed'
  AND aggregate_id = (SELECT runtime_id FROM runtime_instances WHERE attempt_id = ?)`, attemptID).Scan(&removedEvents); err != nil {
				t.Fatalf("count cleanup removed events: %v", err)
			}
			if removedEvents != 1 {
				t.Fatalf("runtime.cleanup_removed events = %d, want 1", removedEvents)
			}
			raw, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=coordplane.attempt_id="+attemptID).CombinedOutput()
			if err != nil {
				t.Fatalf("list matching managed containers: %v: %s", err, raw)
			}
			if strings.TrimSpace(string(raw)) != "" {
				t.Fatalf("managed containers remain for attempt %s: %s", attemptID, raw)
			}
			assertSQLiteIntegrity(t, ctx, db)
		})
	}
}

func TestDockerExecClientStdinGatePassesInputToContainerProcess(t *testing.T) {
	if os.Getenv("COORDPLANE_DOCKER_STDIN_GATE") != "1" {
		t.Skip("set COORDPLANE_DOCKER_STDIN_GATE=1 to verify docker exec -i passes stdin into a real container process")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	name := cpruntime.DockerSafeName("coordplane-stdin-gate-" + time.Now().UTC().Format("150405.000000000"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	raw, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, image, "sleep", "60").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run stdin gate: %v: %s", err, raw)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	result, err := cpruntime.DockerExecClient{}.Exec(ctx, cpruntime.ContainerExecSpec{
		ContainerName: name,
		Workdir:       "/",
		HomeDir:       "/",
		Command:       []string{"sh", "-lc", "cat"},
		Stdin:         "coordplane-stdin-gate\n",
		Timeout:       10 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("docker exec stdin gate failed: result=%+v err=%v stderr=%s", result, err, result.Stderr)
	}
	if string(result.Stdout) != "coordplane-stdin-gate\n" {
		t.Fatalf("docker exec stdout = %q, want stdin echoed by container process", result.Stdout)
	}
}

func TestDockerClaudeAuthProbeGateRequiresExplicitEnvironment(t *testing.T) {
	if os.Getenv("COORDPLANE_DOCKER_CLAUDE_AUTH_GATE") != "1" {
		t.Skip("set COORDPLANE_DOCKER_CLAUDE_AUTH_GATE=1 to verify real Docker/Claude non-interactive auth probe behavior")
	}
	coordlinkPath := os.Getenv("COORDPLANE_COORDLINK_PATH")
	backendURL := os.Getenv("COORDPLANE_BACKEND_URL")
	if coordlinkPath == "" || backendURL == "" {
		t.Skip("real Docker Claude auth gate requires COORDPLANE_COORDLINK_PATH and COORDPLANE_BACKEND_URL")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_REAL_CLI_IMAGE")
	if image == "" {
		image = os.Getenv("COORDPLANE_DOCKER_IMAGE")
	}
	if image == "" {
		t.Fatal("COORDPLANE_REAL_CLI_IMAGE or COORDPLANE_DOCKER_IMAGE is required for the Claude auth gate")
	}
	claudeBin := os.Getenv("COORDPLANE_CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin = "/usr/local/bin/claude"
	}
	authKeys := splitCSV(os.Getenv("COORDPLANE_CLAUDE_ENV"))
	if len(authKeys) == 0 {
		authKeys = claudeenv.RuntimeKeys
	}
	expectPass := os.Getenv("COORDPLANE_CLAUDE_AUTH_EXPECT_PASS") == "1"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, cleanup := newIntegrationDB(t)
	defer cleanup()
	runtimeRoot := filepath.Join(t.TempDir(), "docker-runtime")
	var provider secrets.Provider
	if len(authKeys) > 0 {
		provider = secrets.NewEnvProvider(authKeys)
	}
	backend := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:             db,
		ProfileName:    "docker-claude-auth-gate",
		TeamID:         "docker-claude-auth-gate",
		Image:          image,
		Network:        os.Getenv("COORDPLANE_DOCKER_NETWORK"),
		RuntimeRoot:    runtimeRoot,
		CoordlinkPath:  coordlinkPath,
		DBPath:         filepath.Join(t.TempDir(), "coordplane.db"),
		ClaudeBinary:   claudeBin,
		Ready:          true,
		SecretProvider: provider,
	})
	prepared, err := backend.Prepare(ctx, cpruntime.PrepareRequest{
		AgentID:        "developer",
		AttemptID:      "att_docker_claude_auth_gate",
		AssignmentID:   "asn_docker_claude_auth_gate",
		LeaseID:        "lease_docker_claude_auth_gate",
		ContractID:     "ctr_docker_claude_auth_gate",
		TeamID:         "docker-claude-auth-gate",
		RuntimeProfile: "docker-claude-auth-gate",
		CLIBackend:     "claude",
		BackendURL:     backendURL,
		WorkspaceName:  "docker-claude-auth-gate-workspace",
	})
	if prepared.ContainerName != "" {
		t.Cleanup(func() {
			_ = exec.Command("docker", "rm", "-f", prepared.ContainerName).Run()
		})
	}
	if expectPass {
		if err != nil {
			t.Fatalf("prepare with configured Claude auth failed: %v", err)
		}
		if !prepared.Checks["claude_auth_probe_passed"] {
			t.Fatalf("prepared checks = %#v, want claude auth probe passed", prepared.Checks)
		}
		return
	}
	if err == nil {
		t.Fatalf("prepare without expected Claude auth succeeded with checks %#v; set COORDPLANE_CLAUDE_AUTH_EXPECT_PASS=1 for a passing auth gate", prepared.Checks)
	}
	if !strings.Contains(err.Error(), "CLAUDE_AUTH") && !strings.Contains(err.Error(), "CLAUDE_NOT_FOUND") {
		t.Fatalf("prepare error = %v, want typed Claude auth/probe failure", err)
	}
}

func newIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	st := store.New(db)
	if _, err := st.Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("migrate: %v", err)
	}
	return db, func() {
		_ = db.Close()
	}
}

func newFileIntegrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open file SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		t.Fatalf("enable SQLite foreign keys: %v", err)
	}
	if _, err := store.New(db).Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("migrate file SQLite: %v", err)
	}
	return db
}

func seedDockerCleanupScope(t *testing.T, ctx context.Context, db *sql.DB, attemptID, leaseID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO work_contracts (id, title, objective, target_kind, target_id, status, completion_requirements_json, acceptance_policy_json, created_at, updated_at) VALUES ('ctr_cleanup_gate', 'cleanup gate', 'verify real Docker cleanup', 'agent', 'developer', 'open', '["report"]', '{}', ?, ?)`, []any{now, now}},
		{`INSERT INTO assignments (id, contract_id, assignee_agent_id, state, reason, created_at, updated_at) VALUES ('asg_cleanup_gate', 'ctr_cleanup_gate', 'developer', 'claimed', 'cleanup_gate', ?, ?)`, []any{now, now}},
		{`INSERT INTO leases (id, assignment_id, agent_id, state, expires_at, created_at, updated_at) VALUES (?, 'asg_cleanup_gate', 'developer', 'active', ?, ?, ?)`, []any{leaseID, expires, now, now}},
		{`INSERT INTO attempts (id, lease_id, cli_backend, runtime_kind, start_reason, status, started_at) VALUES (?, ?, 'fake', 'docker', 'cleanup_gate', 'preparing', ?)`, []any{attemptID, leaseID, now}},
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Docker cleanup scope: %v", err)
		}
	}
}

func assertSQLiteIntegrity(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("SQLite integrity_check = %q, err=%v", integrity, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("SQLite foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("SQLite foreign_key_check reported a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("SQLite foreign_key_check rows: %v", err)
	}
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
