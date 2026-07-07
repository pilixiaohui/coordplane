package releasehealth

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/capability"
	"coordplane/internal/cpprobe"

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

func TestCPProbeDockerReplayUsesNoToolClaudeSmokePrompts(t *testing.T) {
	for name, args := range map[string][]string{
		"start":  cpProbeClaudeStartArgs(),
		"resume": cpProbeClaudeResumeArgs(),
	} {
		t.Run(name, func(t *testing.T) {
			joined := strings.Join(args, "\n")
			for _, want := range []string{"--print", "CP-PROBE release-health runtime smoke", "Do not call tools", "coordlink"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s args = %q, missing %q", name, joined, want)
				}
			}
			for _, forbidden := range []string{"{{prompt}}", "CoordPlane runtime protocol", "bypassPermissions", "allowedTools"} {
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
