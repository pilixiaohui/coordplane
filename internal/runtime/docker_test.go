package runtime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/claudeenv"
	"coordplane/internal/coordination"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/secrets"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestRuntimeEnvAllowlistRejectsForbiddenKeys(t *testing.T) {
	env, err := cpruntime.BuildRuntimeEnv(cpruntime.EnvironmentInput{
		BackendURL:    "http://coordplane.test",
		AgentID:       "developer",
		RuntimeID:     "rt_docker_test",
		AttemptID:     "att_test",
		AssignmentID:  "asn_test",
		LeaseID:       "lease_test",
		Workspace:     cpruntime.ContainerWorkspacePath,
		CLIBackend:    "fake",
		TeamID:        "team-test",
		WorkspaceName: "workspace-test",
	})
	if err != nil {
		t.Fatalf("build env: %v", err)
	}
	if err := cpruntime.ValidateRuntimeEnv(env); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
	for _, key := range []string{"PATH", "COORDPLANE_DB_PATH", "COORDPLANE_RUNTIME_ROOT"} {
		t.Run(key, func(t *testing.T) {
			unsafe := cloneStringMap(env)
			unsafe[key] = "forbidden"
			if err := cpruntime.ValidateRuntimeEnv(unsafe); err == nil {
				t.Fatalf("ValidateRuntimeEnv accepted forbidden key %s", key)
			}
		})
	}
}

func TestRuntimeEnvAllowlistAcceptsOnlyConfiguredClaudeCLIEnv(t *testing.T) {
	env, err := cpruntime.BuildRuntimeEnv(cpruntime.EnvironmentInput{
		BackendURL:    "http://coordplane.test",
		AgentID:       "developer",
		RuntimeID:     "rt_docker_test",
		AttemptID:     "att_test",
		AssignmentID:  "asn_test",
		LeaseID:       "lease_test",
		Workspace:     cpruntime.ContainerWorkspacePath,
		CLIBackend:    "claude",
		TeamID:        "team-test",
		WorkspaceName: "workspace-test",
	})
	if err != nil {
		t.Fatalf("build env: %v", err)
	}
	for _, key := range claudeenv.RuntimeKeys {
		withClaudeEnv := cloneStringMap(env)
		withClaudeEnv[key] = "configured"
		if err := cpruntime.ValidateRuntimeEnv(withClaudeEnv); err != nil {
			t.Fatalf("ValidateRuntimeEnv rejected configured Claude CLI env %s: %v", key, err)
		}
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_API_KEY", "CLAUDE_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		withLegacyEnv := cloneStringMap(env)
		withLegacyEnv[key] = "legacy"
		if err := cpruntime.ValidateRuntimeEnv(withLegacyEnv); err == nil {
			t.Fatalf("ValidateRuntimeEnv accepted legacy Claude env key %s", key)
		}
	}
}

func TestDockerSafeNameIsDeterministicAndDockerCompatible(t *testing.T) {
	got := cpruntime.DockerSafeName("CoordPlane/Team A:Developer#01")
	if got != "coordplane-team-a-developer-01" {
		t.Fatalf("safe name = %q, want docker-compatible normalized name", got)
	}
}

func TestRunnerStartsDockerRuntimeWithDurableEvidenceAndScopedEnv(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarness(t, nil)
	add := addContract(t, ctx, h.coordination, "developer")

	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start docker runtime: %v", err)
	}
	specs := h.docker.Specs()
	if len(specs) != 1 {
		t.Fatalf("docker specs = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Env["COORDPLANE_AGENT_ID"] != "developer" ||
		spec.Env["COORDPLANE_LEASE_ID"] != session.LeaseID ||
		spec.Env["COORDPLANE_ATTEMPT_ID"] != session.AttemptID ||
		spec.Env["COORDPLANE_WORKSPACE"] != cpruntime.ContainerWorkspacePath ||
		spec.Env["COORDPLANE_RUNTIME_ID"] == "" ||
		spec.Env["COORDPLANE_TOKEN"] == "" {
		t.Fatalf("docker env = %#v, want scoped runtime identity", spec.Env)
	}
	if strings.Contains(mustJSON(t, spec.Env), "DB_PATH") || strings.Contains(mustJSON(t, spec.Env), "RUNTIME_ROOT") {
		t.Fatalf("docker env leaked forbidden host setting: %#v", spec.Env)
	}
	assertMount(t, spec.Mounts, cpruntime.ContainerWorkspacePath, false)
	assertMount(t, spec.Mounts, cpruntime.ContainerHomePath, false)
	assertMount(t, spec.Mounts, cpruntime.ContainerCoordlinkPath, true)
	if spec.User == "" {
		t.Fatalf("docker spec user is empty, want numeric execution user for bind-mounted workspace ownership")
	}

	starts := h.fake.Starts()
	if len(starts) != 1 {
		t.Fatalf("fake starts = %d, want 1", len(starts))
	}
	start := starts[0]
	if start.RuntimeID != spec.Env["COORDPLANE_RUNTIME_ID"] ||
		start.Workspace != cpruntime.ContainerWorkspacePath ||
		start.HomeDir != cpruntime.ContainerHomePath ||
		start.Env["COORDPLANE_TOKEN"] != spec.Env["COORDPLANE_TOKEN"] {
		t.Fatalf("adapter start = %+v, want docker-prepared route/env", start)
	}
	attempt := attemptRow(t, ctx, h.db, session.AttemptID)
	if attempt.RuntimeKind != "docker" || attempt.Status != "running" {
		t.Fatalf("attempt = %+v, want running docker attempt", attempt)
	}
	route := routeRow(t, ctx, h.db, session.Route.ID)
	if route.RuntimeID != spec.Env["COORDPLANE_RUNTIME_ID"] ||
		route.Workdir != cpruntime.ContainerWorkspacePath ||
		route.HomeDir != cpruntime.ContainerHomePath {
		t.Fatalf("route = %+v, want container-visible docker paths", route)
	}
	if got := assignmentRoute(t, ctx, h.db, add.AssignmentID); got != session.Route.ID {
		t.Fatalf("assignment route = %s, want %s", got, session.Route.ID)
	}

	instance := runtimeInstanceForAttempt(t, ctx, h.db, session.AttemptID)
	if instance.RuntimeKind != "docker" || instance.RuntimeProfile != "docker-default" ||
		instance.AgentID != "developer" || instance.State != "ready" ||
		instance.ContainerID != "container-"+spec.ContainerName ||
		instance.WorkspacePath != cpruntime.ContainerWorkspacePath ||
		instance.HomePath != cpruntime.ContainerHomePath {
		t.Fatalf("runtime instance = %+v, want ready docker evidence", instance)
	}
	if !instance.Checks["coordlink_present"] ||
		!instance.Checks["backend_reachable"] ||
		!instance.Checks["forbidden_env_absent"] ||
		!instance.Checks["forbidden_mount_absent"] ||
		!instance.Checks["workspace_writable"] ||
		!instance.Checks["home_writable"] ||
		!instance.Checks["git_workspace_writable"] ||
		!instance.Checks["cli_user_consistent"] {
		t.Fatalf("runtime checks = %#v, want pass evidence", instance.Checks)
	}
	if strings.Contains(mustJSON(t, instance), spec.Env["COORDPLANE_TOKEN"]) {
		t.Fatalf("runtime inspect instance leaked token value: %+v", instance)
	}
	events := countRowsWhere(t, ctx, h.db, "events", "aggregate_type = 'runtime_instance'")
	if events < 4 {
		t.Fatalf("runtime instance events = %d, want prepare/env/container/ready evidence", events)
	}
}

func TestManagedDockerRuntimeCleanupConvergesWithDetachedContextAndIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newDockerRuntimeHarness(t, nil)
	addContract(t, ctx, h.coordination, "developer")
	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start docker runtime: %v", err)
	}
	h.docker.onInspect = func(context.Context) { cancel() }

	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "completed",
		Summary:   "managed runtime completed",
	}); err != nil {
		t.Fatalf("finish managed runtime after parent cancellation: %v", err)
	}
	assertRuntimeCleanup(t, context.Background(), h.db, session.AttemptID, "stopped", "removed")
	if h.docker.HasContainer(session.ContainerName) {
		t.Fatalf("managed container %s still exists after completed cleanup", session.ContainerName)
	}
	if got := h.docker.RemovedCount(); got != 1 {
		t.Fatalf("docker remove calls = %d, want 1", got)
	}

	if _, err := h.runner.FinishSession(context.Background(), cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "completed",
	}); err != nil {
		t.Fatalf("repeat finish managed runtime: %v", err)
	}
	if got := h.docker.RemovedCount(); got != 1 {
		t.Fatalf("docker remove calls after repeated finish = %d, want idempotent 1", got)
	}
	if got := countRowsWhere(t, context.Background(), h.db, "events", "event_type = 'runtime.cleanup_removed'"); got != 1 {
		t.Fatalf("runtime.cleanup_removed events = %d, want 1", got)
	}
}

func TestManagedDockerRuntimeWaitingSurvivesResumeUntilCompletion(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarness(t, nil)
	addContract(t, ctx, h.coordination, "developer")
	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start docker runtime: %v", err)
	}
	if _, err := h.coordination.WaitContract(ctx, coordination.WaitContractInput{
		LeaseID:        session.LeaseID,
		AgentID:        "developer",
		Reason:         "waiting for resumable work",
		WaitingForRef:  "contract:child",
		SessionRouteID: session.Route.ID,
	}); err != nil {
		t.Fatalf("wait contract: %v", err)
	}
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "waiting",
	}); err != nil {
		t.Fatalf("finish waiting runtime: %v", err)
	}
	assertRuntimeCleanup(t, ctx, h.db, session.AttemptID, "ready", "not_requested")
	if !h.docker.HasContainer(session.ContainerName) {
		t.Fatalf("waiting managed container %s was removed before resume", session.ContainerName)
	}
	if _, err := h.runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{
		RouteID: session.Route.ID,
		Reason:  "resume managed runtime",
	}); err != nil {
		t.Fatalf("resume waiting managed runtime: %v", err)
	}
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "completed",
	}); err != nil {
		t.Fatalf("complete resumed managed runtime: %v", err)
	}
	assertRuntimeCleanup(t, ctx, h.db, session.AttemptID, "stopped", "removed")
	if h.docker.HasContainer(session.ContainerName) {
		t.Fatalf("resumed managed container %s still exists after completion", session.ContainerName)
	}
}

func TestManagedDockerRuntimeCleanupFencesForeignOwnershipLabels(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarness(t, nil)
	addContract(t, ctx, h.coordination, "developer")
	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start docker runtime: %v", err)
	}
	h.docker.SetContainerLabel(session.ContainerName, "coordplane.runtime_id", "rt_foreign_owner")

	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID: session.AttemptID,
		Status:    "completed",
	}); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("finish with foreign container labels error = %v, want ownership fence", err)
	}
	assertRuntimeCleanup(t, ctx, h.db, session.AttemptID, "stopped", "failed")
	if !h.docker.HasContainer(session.ContainerName) {
		t.Fatalf("foreign-owned container %s was removed", session.ContainerName)
	}
	if got := h.docker.RemovedCount(); got != 0 {
		t.Fatalf("docker remove calls for foreign-owned container = %d, want 0", got)
	}
}

func TestManagedDockerRuntimeReconcilesPendingCleanupAfterCrash(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarness(t, nil)
	addContract(t, ctx, h.coordination, "developer")
	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start docker runtime: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.ExecContext(ctx, `UPDATE attempts SET status = 'failed', ended_at = ? WHERE id = ?`, now, session.AttemptID); err != nil {
		t.Fatalf("mark crash attempt failed: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE leases SET state = 'released', updated_at = ? WHERE id = ?`, now, session.LeaseID); err != nil {
		t.Fatalf("release crash lease: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `
UPDATE runtime_instances
SET cleanup_state = 'pending', cleanup_reason = 'crash recovery', updated_at = ?
WHERE attempt_id = ?`, now, session.AttemptID); err != nil {
		t.Fatalf("persist pending cleanup intent: %v", err)
	}
	reconciler, ok := any(h.dockerRuntime).(interface {
		ReconcileRuntimeCleanup(context.Context) error
	})
	if !ok {
		t.Fatal("DockerRuntime does not implement crash cleanup reconciler")
	}
	if err := reconciler.ReconcileRuntimeCleanup(ctx); err != nil {
		t.Fatalf("reconcile pending runtime cleanup: %v", err)
	}
	assertRuntimeCleanup(t, ctx, h.db, session.AttemptID, "stopped", "removed")
	if h.docker.HasContainer(session.ContainerName) {
		t.Fatalf("crash-reconciled container %s still exists", session.ContainerName)
	}
	if err := reconciler.ReconcileRuntimeCleanup(ctx); err != nil {
		t.Fatalf("repeat cleanup reconciliation: %v", err)
	}
	if got := h.docker.RemovedCount(); got != 1 {
		t.Fatalf("docker remove calls after repeated reconciliation = %d, want 1", got)
	}
}

func TestDockerRuntimePrepareFailureFailsClosedWithoutAdapterStart(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarness(t, errors.New("docker daemon unavailable"))
	add := addContract(t, ctx, h.coordination, "developer")

	if _, err := h.runner.StartNext(ctx, "developer"); err == nil {
		t.Fatal("StartNext succeeded with docker prepare failure")
	}
	if starts := h.fake.Starts(); len(starts) != 0 {
		t.Fatalf("fake adapter starts = %+v, want none after prepare failure", starts)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'running'"); got != 0 {
		t.Fatalf("running attempts = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'failed'"); got != 1 {
		t.Fatalf("failed attempts = %d, want 1", got)
	}
	if got := countActiveLeases(t, ctx, h.db, add.AssignmentID); got != 0 {
		t.Fatalf("active leases after docker failure = %d, want 0", got)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment state = %s, want queued after fail-closed prepare", got)
	}
	var state, lastError string
	if err := h.db.QueryRowContext(ctx, `
SELECT state, last_error
FROM runtime_instances
WHERE runtime_kind = 'docker'`).Scan(&state, &lastError); err != nil {
		t.Fatalf("query runtime instance: %v", err)
	}
	if state != "failed" || !strings.Contains(lastError, "docker daemon unavailable") {
		t.Fatalf("runtime instance state/error = %s/%s, want failed prepare evidence", state, lastError)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type = 'runtime.prepare_failed'"); got != 1 {
		t.Fatalf("runtime.prepare_failed events = %d, want 1", got)
	}
}

func TestDockerRuntimeMissingWritableCheckFailsClosedWithoutAdapterStart(t *testing.T) {
	ctx := context.Background()
	h := newDockerRuntimeHarnessWithClient(t, &recordingDockerClient{
		checks: map[string]bool{
			"backend_reachable":      true,
			"workspace_writable":     true,
			"home_writable":          true,
			"git_workspace_writable": true,
			"cli_user_consistent":    false,
		},
	})
	addContract(t, ctx, h.coordination, "developer")

	if _, err := h.runner.StartNext(ctx, "developer"); err == nil || !strings.Contains(err.Error(), "cli_user_consistent") {
		t.Fatalf("StartNext error = %v, want fail-closed missing cli_user_consistent check", err)
	}
	if starts := h.fake.Starts(); len(starts) != 0 {
		t.Fatalf("fake adapter starts = %+v, want none after incomplete docker checks", starts)
	}
	instance := onlyRuntimeInstance(t, ctx, h.db)
	if instance.State != "failed" || !strings.Contains(instance.LastError, "cli_user_consistent") {
		t.Fatalf("runtime instance = %+v, want failed missing check evidence", instance)
	}
	assertRuntimeCleanup(t, ctx, h.db, instance.AttemptID, "failed", "removed")
	if h.docker.HasContainer(instance.ContainerName) {
		t.Fatalf("post-create check failure left managed container %s", instance.ContainerName)
	}
}

func TestDockerRuntimeInjectsClaudeAuthEnvAndStoresOnlyRedactedEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	docker := &recordingDockerClient{}
	probe := &recordingClaudeAuthProbe{}
	backend := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-claude",
		TeamID:        "runtime-docker-auth-test",
		Image:         "alpine:3.20",
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		ClaudeBinary:  "/usr/local/bin/claude",
		Ready:         true,
		Docker:        docker,
		AuthProbe:     probe,
		SecretProvider: &secrets.EnvProvider{
			Keys: claudeenv.RuntimeKeys,
			Lookup: func(key string) (string, bool) {
				switch key {
				case "ANTHROPIC_AUTH_TOKEN":
					return "runtime-auth-token-secret", true
				case "ANTHROPIC_BASE_URL":
					return "https://anthropic-runtime.test", true
				case "ANTHROPIC_MODEL":
					return "claude-runtime-model", true
				case "ANTHROPIC_DEFAULT_OPUS_MODEL":
					return "claude-opus-runtime", true
				case "ANTHROPIC_DEFAULT_SONNET_MODEL":
					return "claude-sonnet-runtime", true
				case "ANTHROPIC_DEFAULT_HAIKU_MODEL":
					return "claude-haiku-runtime", true
				case "CLAUDE_CODE_SUBAGENT_MODEL":
					return "claude-subagent-runtime", true
				case "CLAUDE_CODE_EFFORT_LEVEL":
					return "high", true
				}
				return "", false
			},
		},
	})
	prepared, err := backend.Prepare(ctx, cpruntime.PrepareRequest{
		AgentID:        "developer",
		AttemptID:      "att_auth",
		AssignmentID:   "asg_auth",
		LeaseID:        "lease_auth",
		ContractID:     "ctr_auth",
		TeamID:         "runtime-docker-auth-test",
		RuntimeProfile: "docker-claude",
		CLIBackend:     "claude",
		BackendURL:     "http://coordplane.test",
		WorkspaceName:  "test-workspace",
	})
	if err != nil {
		t.Fatalf("prepare claude docker runtime: %v", err)
	}
	if !prepared.Checks["claude_auth_probe_passed"] || !prepared.Checks["claude_auth_source_secret_provider_env"] {
		t.Fatalf("prepared checks = %#v, want passed secret-provider auth probe", prepared.Checks)
	}
	if len(docker.specs) != 1 || len(probe.specs) != 1 {
		t.Fatalf("docker specs = %d probe specs = %d, want one each", len(docker.specs), len(probe.specs))
	}
	for _, key := range claudeenv.RuntimeKeys {
		if docker.specs[0].Env[key] == "" {
			t.Fatalf("docker spec env missing configured Claude CLI env %s: %#v", key, docker.specs[0].Env)
		}
		if probe.specs[0].Env[key] != docker.specs[0].Env[key] {
			t.Fatalf("probe env[%s] = %q, want docker runtime value %q", key, probe.specs[0].Env[key], docker.specs[0].Env[key])
		}
	}
	assertMount(t, docker.specs[0].Mounts, cpruntime.ContainerHomePath, false)
	for _, mount := range docker.specs[0].Mounts {
		if strings.Contains(mount.Source, ".claude") || strings.Contains(mount.Source, "/.config") {
			t.Fatalf("docker mount leaked host credential path: %+v", mount)
		}
	}
	instance := onlyRuntimeInstance(t, ctx, db)
	rawInstance := mustJSON(t, instance)
	if strings.Contains(rawInstance, "runtime-auth-token-secret") {
		t.Fatalf("runtime inspect leaked auth secret: %s", rawInstance)
	}
	for _, key := range claudeenv.RuntimeKeys {
		if !containsString(instance.EnvKeys, key) {
			t.Fatalf("runtime env keys = %v, want key name %s only", instance.EnvKeys, key)
		}
	}
	rawEvents := eventPayloadsJSON(t, ctx, db)
	if strings.Contains(rawEvents, "runtime-auth-token-secret") {
		t.Fatalf("runtime events leaked auth secret: %s", rawEvents)
	}
	for _, eventType := range []string{"runtime.auth_material_injected", "runtime.auth_probe_started", "runtime.auth_probe_passed"} {
		if got := countRowsWhere(t, ctx, db, "events", "event_type = '"+eventType+"'"); got != 1 {
			t.Fatalf("%s events = %d, want 1", eventType, got)
		}
	}
}

func TestDockerRuntimeMissingClaudeAuthProbeFailsClosed(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	probe := &recordingClaudeAuthProbe{
		result: cpruntime.ClaudeAuthProbeResult{
			Checks: map[string]bool{
				"claude_present":             true,
				"claude_auth_probe_redacted": true,
			},
			ErrorCode: "CLAUDE_AUTH_REQUIRED",
		},
		err: errors.New("CLAUDE_AUTH_REQUIRED: claude non-interactive auth is unavailable"),
	}
	backend := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-claude",
		TeamID:        "runtime-docker-auth-test",
		Image:         "alpine:3.20",
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		ClaudeBinary:  "/usr/local/bin/claude",
		Ready:         true,
		Docker:        &recordingDockerClient{},
		AuthProbe:     probe,
	})
	_, err = backend.Prepare(ctx, cpruntime.PrepareRequest{
		AgentID:        "developer",
		AttemptID:      "att_auth_missing",
		AssignmentID:   "asg_auth_missing",
		LeaseID:        "lease_auth_missing",
		ContractID:     "ctr_auth_missing",
		TeamID:         "runtime-docker-auth-test",
		RuntimeProfile: "docker-claude",
		CLIBackend:     "claude",
		BackendURL:     "http://coordplane.test",
		WorkspaceName:  "test-workspace",
	})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("prepare error = %v, want typed auth failure", err)
	}
	instance := onlyRuntimeInstance(t, ctx, db)
	if instance.State != "failed" || !strings.Contains(instance.LastError, "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("runtime instance = %+v, want failed auth evidence", instance)
	}
	if instance.Checks["claude_auth_probe_passed"] {
		t.Fatalf("runtime checks = %#v, want auth probe not passed", instance.Checks)
	}
	if got := countRowsWhere(t, ctx, db, "events", "event_type = 'runtime.auth_probe_failed'"); got != 1 {
		t.Fatalf("runtime.auth_probe_failed events = %d, want 1", got)
	}
}

func TestDockerCLIClientPrepareContainerUsesPrivateEnvFileWithoutArgvSecrets(t *testing.T) {
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls.txt")
	envPathsPath := filepath.Join(dir, "env-paths.txt")
	envModesPath := filepath.Join(dir, "env-modes.txt")
	envCopiesPath := filepath.Join(dir, "env-copies.txt")
	dockerPath := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
printf 'CALL\n' >> %s
printf '%%s\n' "$@" >> %s
prev=''
for arg in "$@"; do
  if [ "$prev" = "--env-file" ]; then
    printf '%%s\n' "$arg" >> %s
    stat -c '%%a' "$arg" >> %s
    cat "$arg" >> %s
    printf '\n---\n' >> %s
  fi
  prev="$arg"
done
case "$1" in
  rm)
    exit 0
    ;;
  run)
    printf 'container-secret-boundary\n'
    exit 0
    ;;
  exec)
    exit 0
    ;;
esac
exit 0
`, callsPath, callsPath, envPathsPath, envModesPath, envCopiesPath, envCopiesPath)
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker binary: %v", err)
	}
	result, err := (cpruntime.DockerCLIClient{Binary: dockerPath}).PrepareContainer(context.Background(), cpruntime.DockerContainerSpec{
		ContainerName: "coordplane-secret-boundary",
		Image:         "alpine:3.20",
		Env: map[string]string{
			"COORDPLANE_AGENT_ID":    "developer",
			"COORDPLANE_TOKEN":       "COORDPLANE_TOKEN_RUN_SENTINEL",
			"ANTHROPIC_AUTH_TOKEN":   "ANTHROPIC_AUTH_TOKEN_RUN_SENTINEL",
			"COORDPLANE_BACKEND_URL": "http://coordplane.test",
		},
		Mounts: []cpruntime.DockerMount{
			{Source: filepath.Join(dir, "workspace"), Target: cpruntime.ContainerWorkspacePath},
			{Source: filepath.Join(dir, "home"), Target: cpruntime.ContainerHomePath},
		},
	})
	if err != nil {
		t.Fatalf("prepare container: %v", err)
	}
	if result.ContainerID != "container-secret-boundary" || !result.Checks["backend_reachable"] {
		t.Fatalf("prepare result = %+v, want backend-reachable container evidence", result)
	}
	callsRaw, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	if !strings.Contains(string(callsRaw), "--env-file") {
		t.Fatalf("docker calls = %s, want --env-file", callsRaw)
	}
	for _, forbidden := range []string{"COORDPLANE_TOKEN_RUN_SENTINEL", "ANTHROPIC_AUTH_TOKEN_RUN_SENTINEL"} {
		if strings.Contains(string(callsRaw), forbidden) {
			t.Fatalf("docker host argv leaked %q: %s", forbidden, callsRaw)
		}
	}
	envCopies, err := os.ReadFile(envCopiesPath)
	if err != nil {
		t.Fatalf("read copied env-files: %v", err)
	}
	for _, want := range []string{"COORDPLANE_TOKEN=COORDPLANE_TOKEN_RUN_SENTINEL", "ANTHROPIC_AUTH_TOKEN=ANTHROPIC_AUTH_TOKEN_RUN_SENTINEL"} {
		if !strings.Contains(string(envCopies), want) {
			t.Fatalf("env-file copies = %q, missing %q", envCopies, want)
		}
	}
	modesRaw, err := os.ReadFile(envModesPath)
	if err != nil {
		t.Fatalf("read env-file modes: %v", err)
	}
	for _, mode := range strings.Fields(string(modesRaw)) {
		if mode != "600" {
			t.Fatalf("env-file modes = %q, want every file mode 600", modesRaw)
		}
	}
	pathsRaw, err := os.ReadFile(envPathsPath)
	if err != nil {
		t.Fatalf("read env-file paths: %v", err)
	}
	for _, path := range strings.Fields(string(pathsRaw)) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("env-file path %q still exists or stat failed unexpectedly: %v", path, err)
		}
	}
}

func TestDockerCLIClientPrepareContainerCleansEnvFileWhenRunFails(t *testing.T) {
	dir := t.TempDir()
	envPathsPath := filepath.Join(dir, "env-paths.txt")
	dockerPath := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
prev=''
for arg in "$@"; do
  if [ "$prev" = "--env-file" ]; then
    printf '%%s\n' "$arg" >> %s
  fi
  prev="$arg"
done
case "$1" in
  rm)
    exit 0
    ;;
  run)
    printf 'run failed\n' >&2
    exit 42
    ;;
esac
exit 0
`, envPathsPath)
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker binary: %v", err)
	}
	_, err := (cpruntime.DockerCLIClient{Binary: dockerPath}).PrepareContainer(context.Background(), cpruntime.DockerContainerSpec{
		ContainerName: "coordplane-secret-boundary",
		Image:         "alpine:3.20",
		Env:           map[string]string{"COORDPLANE_TOKEN": "COORDPLANE_TOKEN_RUN_FAILURE_SENTINEL"},
	})
	if err == nil || !strings.Contains(err.Error(), "docker run failed") {
		t.Fatalf("prepare container error = %v, want docker run failed", err)
	}
	pathsRaw, err := os.ReadFile(envPathsPath)
	if err != nil {
		t.Fatalf("read env-file paths: %v", err)
	}
	for _, path := range strings.Fields(string(pathsRaw)) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("env-file path %q still exists or stat failed unexpectedly: %v", path, err)
		}
	}
}

type dockerRuntimeHarness struct {
	db            *sql.DB
	store         *store.Store
	coordination  *coordination.Service
	runner        *cpruntime.Runner
	fake          *cpruntime.FakeCLIAdapter
	docker        *recordingDockerClient
	dockerRuntime *cpruntime.DockerRuntime
}

func newDockerRuntimeHarness(t *testing.T, dockerErr error) dockerRuntimeHarness {
	t.Helper()
	return newDockerRuntimeHarnessWithClient(t, &recordingDockerClient{err: dockerErr})
}

func newDockerRuntimeHarnessWithClient(t *testing.T, docker *recordingDockerClient) dockerRuntimeHarness {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register builtin skills: %v", err)
	}
	coordSvc := coordination.NewService(st)
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-default",
		TeamID:        "runtime-docker-test",
		Image:         "alpine:3.20",
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
		Docker:        docker,
	})
	fake := cpruntime.NewFakeCLIAdapter()
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      dockerRuntimeTeamConfig(),
		Skills:          skillRegistry,
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-default": dockerRuntime},
		Adapter:         fake,
		BackendURL:      "http://coordplane.test",
		WorkspaceName:   "test-workspace",
	})
	if err != nil {
		t.Fatalf("new docker runner: %v", err)
	}
	return dockerRuntimeHarness{
		db:            db,
		store:         st,
		coordination:  coordSvc,
		runner:        runner,
		fake:          fake,
		docker:        docker,
		dockerRuntime: dockerRuntime,
	}
}

func dockerRuntimeTeamConfig() teamconfig.Config {
	return teamconfig.Config{
		TeamID:  "runtime-docker-test",
		Version: 1,
		RuntimeProfiles: map[string]teamconfig.RuntimeProfile{
			"docker-default": {Kind: "docker", Image: "alpine:3.20", WorkspaceMode: "isolated"},
		},
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "developer",
				RolePrompt:     "developer role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   step5Capabilities(),
			},
		},
	}
}

type recordingDockerClient struct {
	err        error
	checks     map[string]bool
	specs      []cpruntime.DockerContainerSpec
	containers map[string]map[string]string
	removed    []string
	removeErr  error
	onInspect  func(context.Context)
}

func (c *recordingDockerClient) PrepareContainer(ctx context.Context, spec cpruntime.DockerContainerSpec) (cpruntime.DockerContainerResult, error) {
	select {
	case <-ctx.Done():
		return cpruntime.DockerContainerResult{}, ctx.Err()
	default:
	}
	c.specs = append(c.specs, cloneDockerSpec(spec))
	if c.err != nil {
		return cpruntime.DockerContainerResult{}, c.err
	}
	checks := map[string]bool{
		"backend_reachable":      true,
		"workspace_writable":     true,
		"home_writable":          true,
		"git_workspace_writable": true,
		"cli_user_consistent":    true,
	}
	for key, value := range c.checks {
		checks[key] = value
	}
	containerID := "container-" + spec.ContainerName
	if c.containers == nil {
		c.containers = make(map[string]map[string]string)
	}
	c.containers[spec.ContainerName] = cloneStringMap(spec.Labels)
	return cpruntime.DockerContainerResult{
		ContainerID: containerID,
		Checks:      checks,
	}, nil
}

func (c *recordingDockerClient) InspectContainer(ctx context.Context, containerName string) (map[string]string, error) {
	if c.onInspect != nil {
		onInspect := c.onInspect
		c.onInspect = nil
		onInspect(ctx)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	labels, ok := c.containers[containerName]
	if !ok {
		return nil, os.ErrNotExist
	}
	return cloneStringMap(labels), nil
}

func (c *recordingDockerClient) RemoveContainer(ctx context.Context, containerName string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if c.removeErr != nil {
		return c.removeErr
	}
	if _, ok := c.containers[containerName]; !ok {
		return os.ErrNotExist
	}
	delete(c.containers, containerName)
	c.removed = append(c.removed, containerName)
	return nil
}

func (c *recordingDockerClient) HasContainer(containerName string) bool {
	_, ok := c.containers[containerName]
	return ok
}

func (c *recordingDockerClient) SetContainerLabel(containerName, key, value string) {
	if c.containers[containerName] == nil {
		c.containers[containerName] = make(map[string]string)
	}
	c.containers[containerName][key] = value
}

func (c *recordingDockerClient) RemovedCount() int {
	return len(c.removed)
}

func (c *recordingDockerClient) Specs() []cpruntime.DockerContainerSpec {
	out := make([]cpruntime.DockerContainerSpec, len(c.specs))
	for i, spec := range c.specs {
		out[i] = cloneDockerSpec(spec)
	}
	return out
}

type recordingClaudeAuthProbe struct {
	result cpruntime.ClaudeAuthProbeResult
	err    error
	specs  []cpruntime.ClaudeAuthProbeSpec
}

func (p *recordingClaudeAuthProbe) ProbeClaudeAuth(ctx context.Context, spec cpruntime.ClaudeAuthProbeSpec) (cpruntime.ClaudeAuthProbeResult, error) {
	select {
	case <-ctx.Done():
		return cpruntime.ClaudeAuthProbeResult{}, ctx.Err()
	default:
	}
	p.specs = append(p.specs, cloneClaudeAuthProbeSpec(spec))
	if p.result.Checks == nil && p.err == nil {
		return cpruntime.ClaudeAuthProbeResult{Checks: map[string]bool{
			"claude_present":                         true,
			"claude_auth_configured":                 true,
			"claude_auth_probe_passed":               true,
			"claude_auth_probe_redacted":             true,
			"home_private":                           true,
			"home_persistent":                        true,
			"claude_auth_source_secret_provider_env": spec.AuthSource == "secret_provider_env",
			"claude_auth_source_preseeded_home":      spec.AuthSource != "secret_provider_env",
		}}, nil
	}
	return p.result, p.err
}

func cloneClaudeAuthProbeSpec(spec cpruntime.ClaudeAuthProbeSpec) cpruntime.ClaudeAuthProbeSpec {
	cloned := spec
	cloned.Env = cloneStringMap(spec.Env)
	return cloned
}

func cloneDockerSpec(spec cpruntime.DockerContainerSpec) cpruntime.DockerContainerSpec {
	cloned := spec
	cloned.Labels = cloneStringMap(spec.Labels)
	cloned.Env = cloneStringMap(spec.Env)
	cloned.Mounts = append([]cpruntime.DockerMount(nil), spec.Mounts...)
	return cloned
}

func assertRuntimeCleanup(t *testing.T, ctx context.Context, db *sql.DB, attemptID, wantState, wantCleanupState string) {
	t.Helper()
	var state, cleanupState string
	if err := db.QueryRowContext(ctx, `
SELECT state, cleanup_state
FROM runtime_instances
WHERE attempt_id = ?`, attemptID).Scan(&state, &cleanupState); err != nil {
		t.Fatalf("read runtime cleanup for attempt %s: %v", attemptID, err)
	}
	if state != wantState || cleanupState != wantCleanupState {
		t.Fatalf("runtime state/cleanup for attempt %s = %s/%s, want %s/%s", attemptID, state, cleanupState, wantState, wantCleanupState)
	}
}

func onlyRuntimeInstance(t *testing.T, ctx context.Context, db *sql.DB) cpruntime.RuntimeInstance {
	t.Helper()
	instances, err := cpruntime.ListRuntimeInstances(ctx, db)
	if err != nil {
		t.Fatalf("list runtime instances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("runtime instances = %+v, want exactly one", instances)
	}
	return instances[0]
}

func assertMount(t *testing.T, mounts []cpruntime.DockerMount, target string, readOnly bool) {
	t.Helper()
	for _, mount := range mounts {
		if mount.Target == target {
			if mount.Source == "" || mount.ReadOnly != readOnly {
				t.Fatalf("mount %s = %+v, want source and readOnly=%v", target, mount, readOnly)
			}
			return
		}
	}
	t.Fatalf("mount target %s missing from %+v", target, mounts)
}

func runtimeInstanceForAttempt(t *testing.T, ctx context.Context, db *sql.DB, attemptID string) cpruntime.RuntimeInstance {
	t.Helper()
	instances, err := cpruntime.ListRuntimeInstances(ctx, db)
	if err != nil {
		t.Fatalf("list runtime instances: %v", err)
	}
	for _, instance := range instances {
		if instance.AttemptID == attemptID {
			return instance
		}
	}
	t.Fatalf("runtime instance for attempt %s not found in %+v", attemptID, instances)
	return cpruntime.RuntimeInstance{}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func eventPayloadsJSON(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT payload_json FROM events ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan event payload: %v", err)
		}
		b.WriteString(payload)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event payloads: %v", err)
	}
	return b.String()
}
