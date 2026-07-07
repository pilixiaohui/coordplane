package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/backend"

	_ "modernc.org/sqlite"
)

func TestReleaseHealthCommandWritesFailedLedgerAndReturnsNonzero(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coordplane.db")
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         dbPath,
		TeamConfigPath: filepath.Join("..", "..", "team_config", "fixtures", "cp_accept_001_three_agent_docker_claude.yaml"),
	})
	if err != nil {
		t.Fatalf("seed backend db: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close seed backend: %v", err)
	}

	stdout := captureStdout(t, func() {
		err = run([]string{
			"release-health",
			"cp-accept-001",
			"--db", dbPath,
			"--root-contract", "ctr_missing",
			"--team-id", "cp-accept-001-three-agent-docker-claude",
			"--team-version", "1",
			"--run-label", "command-fail-closed",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "status is failed") {
		t.Fatalf("release-health error = %v, want failed status", err)
	}
	if !strings.Contains(stdout, `"status": "failed"`) ||
		!strings.Contains(stdout, `"team_id": "cp-accept-001-three-agent-docker-claude"`) {
		t.Fatalf("release-health stdout = %s, want structured failed acceptance", stdout)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_acceptances WHERE run_label = 'command-fail-closed'`).Scan(&count); err != nil {
		t.Fatalf("count release_acceptances: %v", err)
	}
	if count != 1 {
		t.Fatalf("release_acceptances = %d, want failed ledger row", count)
	}
}

func TestReleaseHealthCommandUsesRootContractEnvAsReviewOverride(t *testing.T) {
	t.Setenv("COORDPLANE_ROOT_CONTRACT_ID", "ctr_missing_env")
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coordplane.db")
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         dbPath,
		TeamConfigPath: filepath.Join("..", "..", "team_config", "fixtures", "cp_accept_001_three_agent_docker_claude.yaml"),
	})
	if err != nil {
		t.Fatalf("seed backend db: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close seed backend: %v", err)
	}

	stdout := captureStdout(t, func() {
		err = run([]string{
			"release-health",
			"cp-accept-001",
			"--db", dbPath,
			"--team-id", "cp-accept-001-three-agent-docker-claude",
			"--team-version", "1",
			"--run-label", "env-review-override",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "status is failed") {
		t.Fatalf("release-health env override error = %v, want failed review", err)
	}
	if !strings.Contains(stdout, `"root_contract_id": "ctr_missing_env"`) {
		t.Fatalf("release-health stdout = %s, want env root review", stdout)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var roots int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_contracts WHERE target_id = 'coordinator'`).Scan(&roots); err != nil {
		t.Fatalf("count root contracts: %v", err)
	}
	if roots != 0 {
		t.Fatalf("root contracts = %d, want env override to avoid driving workflow", roots)
	}
}

func TestReleaseHealthCommandWithoutRootDrivesWorkflowAndFailsClosed(t *testing.T) {
	t.Setenv("COORDPLANE_ROOT_CONTRACT_ID", "")
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coordplane.db")

	var err error
	stdout := captureStdout(t, func() {
		err = run([]string{
			"release-health",
			"cp-accept-001",
			"--db", dbPath,
			"--teamconfig", filepath.Join("..", "..", "team_config", "fixtures", "cp_accept_001_three_agent_docker_claude.yaml"),
			"--listen", "127.0.0.1:0",
			"--backend-url", "http://127.0.0.1:1",
			"--docker-network", "",
			"--workdir", dir,
			"--run-label", "command-drive-fail-closed",
		})
	})
	if err == nil {
		t.Fatalf("release-health without root unexpectedly passed")
	}
	if strings.Contains(err.Error(), "--root-contract") || strings.Contains(err.Error(), "COORDPLANE_ROOT_CONTRACT_ID") {
		t.Fatalf("release-health error = %v, still requires preexisting root", err)
	}
	if !strings.Contains(err.Error(), "coordinator Docker/Claude session could not start") {
		t.Fatalf("release-health error = %v, want workflow-mode environment failure", err)
	}
	if !strings.Contains(stdout, `"status": "failed"`) ||
		!strings.Contains(stdout, `"root_contract_id": "ctr_`) {
		t.Fatalf("release-health stdout = %s, want failed acceptance for created root", stdout)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var roots int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_contracts WHERE target_id = 'coordinator'`).Scan(&roots); err != nil {
		t.Fatalf("count created root contracts: %v", err)
	}
	if roots != 1 {
		t.Fatalf("root contracts = %d, want workflow-created root", roots)
	}
	var acceptances int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_acceptances WHERE run_label = 'command-drive-fail-closed'`).Scan(&acceptances); err != nil {
		t.Fatalf("count release_acceptances: %v", err)
	}
	if acceptances != 1 {
		t.Fatalf("release_acceptances = %d, want failed ledger row", acceptances)
	}
}

func TestReleaseHealthCPProbeCommandWritesFormalArtifactsAndReturnsEnvironmentBlocked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coordplane.db")

	var err error
	stdout := captureStdout(t, func() {
		err = run([]string{
			"release-health",
			"cp-probe-001",
			"--db", dbPath,
			"--teamconfig", filepath.Join("..", "..", "team_config", "fixtures", "cp_probe_001_manual_service.yaml"),
			"--listen", "127.0.0.1:0",
			"--backend-url", "http://127.0.0.1:1",
			"--workdir", dir,
			"--artifact-dir", dir,
			"--environment-blocker", "Docker/Claude replay not available in command test",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "status is environment_blocked") {
		t.Fatalf("cp-probe release-health error = %v, want environment_blocked", err)
	}
	if !strings.Contains(stdout, `"scenario": "CP-PROBE-001"`) ||
		!strings.Contains(stdout, `"status": "environment_blocked"`) ||
		!strings.Contains(stdout, `"docker_replay_status": "environment_blocked"`) ||
		!strings.Contains(stdout, `"root_contract_id": "ctr_`) {
		t.Fatalf("cp-probe stdout = %s, want structured environment-blocked result", stdout)
	}
	for _, name := range []string{
		"cp_probe_001_manual_trace.md",
		"cp_probe_001_inspect_redacted.json",
		"cp_probe_001_git_operation_summary.json",
		"cp_probe_001_failure_matrix.md",
		"cp_probe_001_conclusion.md",
	} {
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if len(raw) == 0 {
			t.Fatalf("%s is empty", name)
		}
		for _, forbidden := range []string{"coordplane.db", "Bearer ", "sk-", "ANTHROPIC_AUTH_TOKEN", filepath.Join(dir, "runtime")} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%s leaked forbidden marker %q:\n%s", name, forbidden, raw)
			}
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var assessments int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_assessments WHERE verdict = 'pass'`).Scan(&assessments); err != nil {
		t.Fatalf("count validation_assessments: %v", err)
	}
	if assessments != 1 {
		t.Fatalf("validation_assessments = %d, want one non-Docker manual assessment", assessments)
	}
	var roots int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_contracts WHERE target_id = 'coordinator' AND status = 'satisfied'`).Scan(&roots); err != nil {
		t.Fatalf("count satisfied root contracts: %v", err)
	}
	if roots != 1 {
		t.Fatalf("satisfied root contracts = %d, want 1", roots)
	}
	failureMatrix, err := os.ReadFile(filepath.Join(dir, "cp_probe_001_failure_matrix.md"))
	if err != nil {
		t.Fatalf("read failure matrix: %v", err)
	}
	if !strings.Contains(string(failureMatrix), "docker-controlled-workspace-bridge") ||
		!strings.Contains(string(failureMatrix), "environment_blocked") {
		t.Fatalf("failure matrix = %s, want Docker replay bridge status and environment_blocked", failureMatrix)
	}
}

func TestReleaseHealthCommandRejectsUnknownScenario(t *testing.T) {
	err := run([]string{"release-health", "unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown release-health scenario") {
		t.Fatalf("unknown scenario error = %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = writer
	fn()
	_ = writer.Close()
	os.Stdout = original
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(raw)
}
