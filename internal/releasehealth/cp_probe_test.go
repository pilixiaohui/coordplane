package releasehealth

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/cpprobe"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestCPProbeConclusionRequiresDockerCleanupAndDenylistForPassed(t *testing.T) {
	base := cpProbeBaseConclusion()
	outcome := cpProbeDockerReplayOutcome{
		Status:        cpprobe.ConclusionPassed,
		CleanupPassed: true,
		TraceSteps: []cpprobe.TraceStep{
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "workspace.prepare", Status: string(capability.StatusAccepted)},
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "command.run", Status: string(capability.StatusAccepted)},
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "git.commit", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_apply", Status: string(capability.StatusRejected), ErrorCode: "STALE_TARGET_REF"},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_preview", Status: string(capability.StatusRejected), ErrorCode: "MERGE_CONFLICTS_FOUND"},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.conflicts", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.abort", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.resolve", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_apply", Status: string(capability.StatusAccepted)},
			{Actor: "verifier", EntryPoint: "docker.coordlink", Capability: "validation.assessment", Status: string(capability.StatusAccepted)},
			{Actor: "coordinator", EntryPoint: "docker.coordlink", Capability: "communication.read", Status: string(capability.StatusAccepted)},
			{Actor: "coordinator", EntryPoint: "docker.coordlink", Capability: "contract.complete", Status: string(capability.StatusAccepted)},
		},
	}

	pendingScan := cpProbeApplyConclusionOutcome(base, outcome, "")
	if pendingScan.Status != cpprobe.ConclusionFailed {
		t.Fatalf("conclusion before denylist scan = %s, want failed", pendingScan.Status)
	}

	outcome.DenylistPassed = true
	passed := cpProbeApplyConclusionOutcome(base, outcome, "")
	if passed.Status != cpprobe.ConclusionPassed || len(passed.NotCovered) != 0 || len(passed.NextSteps) != 0 {
		t.Fatalf("conclusion after denylist+cleanup = %+v, want clean passed conclusion", passed)
	}

	outcome.CleanupPassed = false
	cleanupBlocked := cpProbeApplyConclusionOutcome(base, outcome, "cleanup failed")
	if cleanupBlocked.Status != cpprobe.ConclusionEnvironmentBlocked {
		t.Fatalf("conclusion with cleanup failure = %s, want environment_blocked", cleanupBlocked.Status)
	}
}

func TestCPProbeDockerReplayUsesAuditableCoordlinkSmokePrompts(t *testing.T) {
	for name, tc := range map[string]struct {
		args       []string
		capability string
	}{
		"start":  {args: cpProbeClaudeStartArgs(), capability: "contract.current"},
		"resume": {args: cpProbeClaudeResumeArgs(), capability: "contract.current"},
	} {
		t.Run(name, func(t *testing.T) {
			joined := strings.Join(tc.args, "\n")
			for _, want := range []string{"--print", "CP-PROBE release-health runtime smoke", "/usr/local/bin/coordlink call " + tc.capability, "Do not call any other tool"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s args = %q, missing %q", name, joined, want)
				}
			}
			for _, forbidden := range []string{"{{prompt}}", "CoordPlane runtime protocol", "bypassPermissions", "Bash(*)"} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("%s args expose prompt or grant tool permissions during smoke start: %q", name, joined)
				}
			}
		})
	}
}

func TestCPProbeFailureMatrixRecordsDockerReplayCloseoutAndArtifactGates(t *testing.T) {
	outcome := cpProbeDockerReplayOutcome{
		Status:         cpprobe.ConclusionPassed,
		CleanupPassed:  true,
		DenylistPassed: true,
		TraceSteps: []cpprobe.TraceStep{
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "workspace.prepare", Status: string(capability.StatusAccepted)},
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "command.run", Status: string(capability.StatusAccepted)},
			{Actor: "developer-a", EntryPoint: "docker.coordlink", Capability: "git.commit", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_apply", Status: string(capability.StatusRejected), ErrorCode: "STALE_TARGET_REF"},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_preview", Status: string(capability.StatusRejected), ErrorCode: "MERGE_CONFLICTS_FOUND"},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.conflicts", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.abort", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.resolve", Status: string(capability.StatusAccepted)},
			{Actor: "developer-b", EntryPoint: "docker.coordlink", Capability: "git.merge_apply", Status: string(capability.StatusAccepted)},
			{Actor: "verifier", EntryPoint: "docker.coordlink", Capability: "validation.assessment", Status: string(capability.StatusAccepted)},
			{Actor: "coordinator", EntryPoint: "docker.coordlink", Capability: "communication.read", Status: string(capability.StatusAccepted)},
			{Actor: "coordinator", EntryPoint: "docker.coordlink", Capability: "contract.complete", Status: string(capability.StatusAccepted)},
		},
	}
	matrix := cpProbeApplyFailureMatrixOutcome(cpprobe.FailureMatrix{
		Scenario: cpprobe.ScenarioID,
		Items: []cpprobe.FailureMatrixItem{{
			ID:             "docker-claude-equivalent-replay",
			Status:         "environment_blocked",
			Capability:     "runtime.docker/claude",
			StateAssertion: "old state",
		}},
	}, outcome, "")
	if err := matrix.Validate(); err != nil {
		t.Fatalf("failure matrix invalid: %v", err)
	}
	for _, id := range []string{
		"docker-claude-equivalent-replay",
		"docker-controlled-workspace-bridge",
		"docker-concurrent-stale-target",
		"docker-conflict-resolve",
		"docker-verifier-root-closeout",
		"artifact-denylist",
		"managed-docker-cleanup",
	} {
		if got := failureMatrixStatus(matrix, id); got != "covered" {
			t.Fatalf("matrix item %s = %s, want covered", id, got)
		}
	}
}

func TestCPProbeManualCloseoutReleasesRuntimeGuards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	result, err := RunCPProbe001(ctx, CPProbe001Config{
		DBPath:               filepath.Join(dir, "coordplane.db"),
		WorkDir:              filepath.Join(dir, "work"),
		ArtifactDir:          filepath.Join(dir, "artifacts"),
		RuntimeWorkspaceRoot: filepath.Join(dir, "runtime", "workspaces"),
		RuntimeHomeRoot:      filepath.Join(dir, "runtime", "home"),
		EnvironmentBlocker:   "test skips Docker replay",
	})
	if err == nil || result.Status != cpprobe.ConclusionEnvironmentBlocked {
		t.Fatalf("RunCPProbe001 error/status = %v/%s, want environment_blocked from Docker replay blocker", err, result.Status)
	}
	db, openErr := sql.Open("sqlite", filepath.Join(dir, "coordplane.db"))
	if openErr != nil {
		t.Fatalf("open db: %v", openErr)
	}
	defer func() { _ = db.Close() }()
	if got := countRowsWhere(ctx, db, "active_guards", "state = 'active'"); got != 0 {
		t.Fatalf("active guards after CP-PROBE manual closeout = %d, want 0", got)
	}
	if got := countRowsWhere(ctx, db, "attempts", "status = 'running'"); got != 0 {
		t.Fatalf("running attempts after CP-PROBE manual closeout = %d, want 0", got)
	}
}

func TestCPProbeGitSummaryRejectsAgentOperationWithoutRuntimeFromDB(t *testing.T) {
	for _, executionLocation := range []string{"backend_control_plane", "runtime_container"} {
		t.Run(executionLocation, func(t *testing.T) {
			ctx := context.Background()
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			if _, err := store.New(db).Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			insertGitOperationWithoutRuntime(t, ctx, db, executionLocation)

			ops, err := gitOperationBriefs(ctx, db)
			if err != nil {
				t.Fatalf("read operation briefs: %v", err)
			}
			if len(ops) != 1 {
				t.Fatalf("operation briefs = %d, want 1", len(ops))
			}
			if ops[0].SubjectKind != "agent_runtime" || ops[0].RuntimeID != "" {
				t.Fatalf("operation brief subject/runtime = %q/%q, want agent_runtime with empty runtime", ops[0].SubjectKind, ops[0].RuntimeID)
			}

			_, err = cpProbeGitSummary(ctx, db)
			if err == nil || !strings.Contains(err.Error(), "requires runtime_id") {
				t.Fatalf("cpProbeGitSummary error = %v, want runtime_id validation failure", err)
			}

			summary := cpprobe.GitOperationSummary{
				Scenario:      cpprobe.ScenarioID,
				Repositories:  []cpprobe.RepositorySummary{{ID: "repo_1", CanonicalBranch: "main", Status: "active"}},
				Operations:    ops,
				NoActiveLocks: true,
			}
			artifactDir := filepath.Join(t.TempDir(), "artifacts")
			err = cpprobe.WriteReportArtifacts(artifactDir, cpProbeValidArtifactsWithGitSummary(summary))
			if err == nil || !strings.Contains(err.Error(), "requires runtime_id") {
				t.Fatalf("WriteReportArtifacts error = %v, want runtime_id validation failure", err)
			}
			if _, statErr := os.Stat(filepath.Join(artifactDir, cpprobe.GitOperationSummaryArtifact)); !os.IsNotExist(statErr) {
				t.Fatalf("git summary artifact stat error = %v, want not created", statErr)
			}
		})
	}
}

func cpProbeBaseConclusion() cpprobe.ConclusionReport {
	return cpprobe.ConclusionReport{
		Scenario:         cpprobe.ScenarioID,
		Status:           cpprobe.ConclusionEnvironmentBlocked,
		ManualTraceRef:   cpprobe.ManualTraceArtifact,
		InspectRef:       cpprobe.InspectRedactedArtifact,
		GitSummaryRef:    cpprobe.GitOperationSummaryArtifact,
		FailureMatrixRef: cpprobe.FailureMatrixArtifact,
		Covered:          []string{"manual closeout"},
	}
}

func failureMatrixStatus(matrix cpprobe.FailureMatrix, id string) string {
	for _, item := range matrix.Items {
		if item.ID == id {
			return item.Status
		}
	}
	return ""
}

func insertGitOperationWithoutRuntime(t *testing.T, ctx context.Context, db *sql.DB, executionLocation string) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_repositories (
  id, tenant_id, source_path, canonical_branch, status, created_at, updated_at
) VALUES ('repo_1', 'default', '/redacted/source', 'main', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_operations (
  id, tenant_id, operation_type, subject_kind, actor_agent_id, workspace_id,
  repo_id, runtime_id, execution_location, before_ref, after_ref, stdout,
  stderr, exit_code, state, feedback_json, created_at, completed_at
) VALUES (
  'gitop_missing_runtime', 'default', 'workspace.prepare', 'agent_runtime',
  'developer-a', NULL, 'repo_1', '', ?, '', 'head', '', '', 0,
  'succeeded', '{"message":"test"}', ?, ?
)`, executionLocation, now, now); err != nil {
		t.Fatalf("insert operation: %v", err)
	}
}

func cpProbeValidArtifactsWithGitSummary(summary cpprobe.GitOperationSummary) cpprobe.ReportArtifacts {
	return cpprobe.ReportArtifacts{
		ManualTrace: cpprobe.ManualTrace{
			Scenario: cpprobe.ScenarioID,
			Steps: []cpprobe.TraceStep{{
				Actor:      "release-health",
				EntryPoint: "test",
				Capability: "workspace.prepare",
				Status:     string(capability.StatusAccepted),
			}},
		},
		Inspect: cpprobe.RedactedInspect{
			Scenario:       cpprobe.ScenarioID,
			Status:         "ok",
			TeamID:         "cp-probe-test",
			RootContractID: "ctr_root",
			Counts:         map[string]int64{"git_operations": 1},
			Redacted:       true,
		},
		GitSummary: summary,
		FailureMatrix: cpprobe.FailureMatrix{
			Scenario: cpprobe.ScenarioID,
			Items: []cpprobe.FailureMatrixItem{{
				ID:             "missing-runtime-negative",
				Status:         "covered",
				Capability:     "workspace.prepare",
				StateAssertion: "agent operation without runtime must fail artifact validation",
			}},
		},
		Conclusion: cpProbeBaseConclusion(),
	}
}
