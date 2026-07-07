package runtime_test

import (
	"context"
	"database/sql"
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
