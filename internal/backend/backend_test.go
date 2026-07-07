package backend_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"coordplane/internal/backend"
	"coordplane/internal/capability"
	"coordplane/internal/coordination"

	_ "modernc.org/sqlite"
)

func TestServeBackendInitializesRealSQLiteFileAndInspectReadiness(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "coordplane.db")
	teamConfigPath := writeTeamConfig(t, dir)

	app, err := backend.Open(ctx, backend.Config{
		DBPath:         dbPath,
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: teamConfigPath,
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer app.Close()

	if info, err := os.Stat(dbPath); err != nil || info.Size() == 0 {
		t.Fatalf("db file stat = %v/%v, want durable sqlite file", info, err)
	}

	health := getJSON(t, app.Handler, "/healthz")
	if health["status"] != "ok" || health["service"] != "coordplane" {
		t.Fatalf("health = %#v, want coordplane ok", health)
	}
	ready := getJSON(t, app.Handler, "/readyz")
	assertServeInspect(t, ready, dbPath)
	inspect := getJSON(t, app.Handler, "/inspect")
	assertServeInspect(t, inspect, dbPath)

	dbMigrations := countRows(t, ctx, app.DB, "schema_migrations")
	if got := int64Field(t, inspect, "counts", "schema_migrations"); got != dbMigrations || got == 0 {
		t.Fatalf("inspect migration count = %d, db count = %d", got, dbMigrations)
	}
	if events := countRows(t, ctx, app.DB, "events"); events == 0 || int64Field(t, inspect, "counts", "events") != events {
		t.Fatalf("events count inspect/db = %v/%d, want TeamConfig load event", inspect["counts"], events)
	}
	if skills := countRows(t, ctx, app.DB, "skill_packages"); skills < 3 || int64Field(t, inspect, "counts", "skill_packages") != skills {
		t.Fatalf("skill package count inspect/db = %v/%d, want builtins", inspect["counts"], skills)
	}

	if err := app.Close(); err != nil {
		t.Fatalf("close first backend: %v", err)
	}
	reopened, err := backend.Open(ctx, backend.Config{
		DBPath:     dbPath,
		ListenAddr: "127.0.0.1:0",
		TeamID:     "accept-test",
	})
	if err != nil {
		t.Fatalf("reopen backend with existing db: %v", err)
	}
	defer reopened.Close()
	if !reopened.TeamConfigLoaded || reopened.TeamConfig.TeamID != "accept-test" {
		t.Fatalf("reopened TeamConfig = loaded:%v cfg:%+v, want persisted accept-test", reopened.TeamConfigLoaded, reopened.TeamConfig)
	}
}

func TestServeCapabilityDiscoveryUsesLoadedTeamConfigPolicy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: writeTeamConfig(t, dir),
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer app.Close()

	builder := getJSON(t, app.Handler, "/capabilities?agent_id=builder")
	if builder["status"] != "accepted" || builder["ok"] != true {
		t.Fatalf("builder discovery = %#v, want accepted", builder)
	}
	names := capabilityNames(t, builder)
	if !reflect.DeepEqual(names, []string{"contract.current", "object.inspect"}) {
		t.Fatalf("builder capabilities = %v, want scoped contract.current/object.inspect", names)
	}

	intruder := getJSONWithStatus(t, app.Handler, "/capabilities?agent_id=intruder", http.StatusBadRequest)
	if intruder["status"] != "rejected" || intruder["error_code"] != "UNAUTHORIZED_CAPABILITY_DISCOVERY" {
		t.Fatalf("intruder discovery = %#v, want unauthorized discovery", intruder)
	}
	if _, ok := intruder["data"]; ok {
		t.Fatalf("intruder received discovery data: %#v", intruder)
	}
}

func TestThreeAgentFixtureLoadsTeamConfigRuntimeCLIAndPrompts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: threeAgentFixturePath(t),
	})
	if err != nil {
		t.Fatalf("open backend with three-agent fixture: %v", err)
	}
	defer app.Close()

	if !app.TeamConfigLoaded || app.TeamConfig.TeamID != "cp-accept-001-three-agent" || app.TeamConfig.Version != 1 {
		t.Fatalf("loaded TeamConfig = loaded:%v cfg:%+v, want cp-accept-001-three-agent v1", app.TeamConfigLoaded, app.TeamConfig)
	}
	if got := app.TeamConfig.RuntimeProfiles["external-local"]; got.Kind != "external" || got.WorkspaceMode != "host_path" {
		t.Fatalf("external-local runtime profile = %+v, want external host_path", got)
	}

	var prompts []string
	for _, agentID := range []string{"coordinator", "developer", "verifier"} {
		agent, ok := app.TeamConfig.Agent(agentID)
		if !ok {
			t.Fatalf("fixture agent %q missing", agentID)
		}
		if agent.RuntimeProfile != "external-local" || agent.CLIBackend != "fake" {
			t.Fatalf("%s runtime/cli = %s/%s, want fixture-provided external-local/fake", agentID, agent.RuntimeProfile, agent.CLIBackend)
		}
		if strings.TrimSpace(agent.RolePrompt) == "" {
			t.Fatalf("%s role prompt is empty; want prompt loaded from fixture", agentID)
		}
		prompts = append(prompts, agent.RolePrompt)
		assertNamesEqual(t, agent.Capabilities, threeAgentExpectedCapabilities[agentID])
		assertNamesEqual(t, agent.Skills, threeAgentExpectedSkills[agentID])

		var rolePrompt, runtimeProfile, cliBackend, skillsJSON, capabilitiesJSON string
		if err := app.DB.QueryRowContext(ctx, `
SELECT role_prompt, runtime_profile, cli_backend, skills_json, capabilities_json
FROM team_config_agents
WHERE team_id = ? AND version = ? AND agent_id = ?`,
			"cp-accept-001-three-agent", 1, agentID,
		).Scan(&rolePrompt, &runtimeProfile, &cliBackend, &skillsJSON, &capabilitiesJSON); err != nil {
			t.Fatalf("load canonical TeamConfig row for %s: %v", agentID, err)
		}
		if rolePrompt != agent.RolePrompt || runtimeProfile != "external-local" || cliBackend != "fake" {
			t.Fatalf("%s canonical row prompt/runtime/cli = %q/%s/%s, want fixture values", agentID, rolePrompt, runtimeProfile, cliBackend)
		}
		assertJSONNamesEqual(t, skillsJSON, threeAgentExpectedSkills[agentID])
		assertJSONNamesEqual(t, capabilitiesJSON, threeAgentExpectedCapabilities[agentID])
	}
	assertPromptsNotInBackendImplementation(t, prompts)
}

func TestThreeAgentFixtureScopesHTTPCapabilitiesAndSkills(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: threeAgentFixturePath(t),
	})
	if err != nil {
		t.Fatalf("open backend with three-agent fixture: %v", err)
	}
	defer app.Close()

	for _, agentID := range []string{"coordinator", "developer", "verifier"} {
		capabilityEnvelope := getJSON(t, app.Handler, "/capabilities?agent_id="+agentID)
		if capabilityEnvelope["status"] != "accepted" || capabilityEnvelope["ok"] != true {
			t.Fatalf("%s capability discovery = %#v, want accepted", agentID, capabilityEnvelope)
		}
		assertNamesEqual(t, capabilityNames(t, capabilityEnvelope), threeAgentExpectedCapabilities[agentID])

		skillEnvelope := getJSON(t, app.Handler, "/skills?agent_id="+agentID)
		if skillEnvelope["status"] != "accepted" || skillEnvelope["ok"] != true {
			t.Fatalf("%s skill list = %#v, want accepted", agentID, skillEnvelope)
		}
		assertNamesEqual(t, skillNames(t, skillEnvelope), threeAgentExpectedSkills[agentID])
	}

	developerSkill := getJSON(t, app.Handler, "/skills/controlled-git?agent_id=developer")
	if developerSkill["status"] != "accepted" || !strings.Contains(stringField(t, objectField(t, developerSkill, "data"), "content"), "git.commit") {
		t.Fatalf("developer controlled-git read = %#v, want accepted skill content", developerSkill)
	}
	verifierSkill := getJSONWithStatus(t, app.Handler, "/skills/controlled-git?agent_id=verifier", http.StatusBadRequest)
	if verifierSkill["status"] != "rejected" || verifierSkill["error_code"] != "SKILL_READ_REJECTED" {
		t.Fatalf("verifier controlled-git read = %#v, want typed rejected", verifierSkill)
	}
	if _, ok := verifierSkill["data"]; ok {
		t.Fatalf("verifier unauthorized skill read leaked data: %#v", verifierSkill)
	}

	rejectedCall := postJSONWithStatus(t, app.Handler, "/call", `{
		"capability": "git.commit",
		"subject": {"kind": "agent", "id": "verifier", "agent_id": "verifier"},
		"input": {"message": "not allowed", "paths": ["feature.txt"]}
	}`, http.StatusBadRequest)
	if rejectedCall["status"] != "rejected" || rejectedCall["error_code"] != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("verifier git.commit call = %#v, want unauthorized typed rejected", rejectedCall)
	}
	if _, ok := rejectedCall["data"]; ok {
		t.Fatalf("verifier unauthorized capability call leaked data: %#v", rejectedCall)
	}
	if calls := countRows(t, ctx, app.DB, "capability_calls"); calls != 4 {
		t.Fatalf("capability_calls = %d, want three capability.list audits plus rejected git.commit", calls)
	}
}

func TestDockerTeamConfigFixtureRegistersDockerRuntimeProfile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	coordlinkPath := filepath.Join(dir, "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: dockerThreeAgentFixturePath(t),
		CoordlinkPath:  coordlinkPath,
	})
	if err != nil {
		t.Fatalf("open backend with docker fixture: %v", err)
	}
	defer app.Close()

	if !app.TeamConfigLoaded || app.TeamConfig.TeamID != "cp-accept-001-three-agent-docker" {
		t.Fatalf("docker TeamConfig = loaded:%v cfg:%+v", app.TeamConfigLoaded, app.TeamConfig)
	}
	if got := app.TeamConfig.RuntimeProfiles["docker-default"]; got.Kind != "docker" || got.Image != "alpine:3.20" || got.WorkspaceMode != "isolated" {
		t.Fatalf("docker runtime profile = %+v, want docker alpine isolated", got)
	}
	inspect := getJSON(t, app.Handler, "/inspect")
	registry := arrayField(t, inspect, "runtime_registry")
	if len(registry) != 1 {
		t.Fatalf("runtime registry = %#v, want one docker profile", registry)
	}
	entry, ok := registry[0].(map[string]any)
	if !ok {
		t.Fatalf("runtime registry entry = %#v, want object", registry[0])
	}
	if entry["kind"] != "docker" || entry["profile"] != "docker-default" || entry["ready"] != true ||
		entry["workspace_root"] != "/workspace/project" || entry["home_root"] != "/home/agent" {
		t.Fatalf("docker runtime entry = %#v, want ready container-visible paths", entry)
	}
	if got := int64Field(t, inspect, "counts", "runtime_instances"); got != 0 {
		t.Fatalf("runtime_instances count = %d, want 0 before runner start", got)
	}
	if got := int64Field(t, inspect, "counts", "cli_sessions"); got != 0 {
		t.Fatalf("cli_sessions count = %d, want 0 before runner start", got)
	}
	cliRegistry := arrayField(t, inspect, "cli_adapter_registry")
	if len(cliRegistry) != 2 {
		t.Fatalf("cli adapter registry = %#v, want fake plus claude profile readiness", cliRegistry)
	}
}

func TestDockerClaudeFixtureRegistersCommandCLIProfile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	coordlinkPath := filepath.Join(dir, "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: dockerClaudeThreeAgentFixturePath(t),
		CoordlinkPath:  coordlinkPath,
		ClaudeBinary:   "/usr/local/bin/claude",
	})
	if err != nil {
		t.Fatalf("open backend with docker claude fixture: %v", err)
	}
	defer app.Close()

	if !app.TeamConfigLoaded || app.TeamConfig.TeamID != "cp-accept-001-three-agent-docker-claude" {
		t.Fatalf("docker claude TeamConfig = loaded:%v cfg:%+v", app.TeamConfigLoaded, app.TeamConfig)
	}
	for _, agentID := range []string{"coordinator", "developer", "verifier"} {
		agent, ok := app.TeamConfig.Agent(agentID)
		if !ok {
			t.Fatalf("fixture agent %s missing", agentID)
		}
		if agent.RuntimeProfile != "docker-default" || agent.CLIBackend != "claude" {
			t.Fatalf("%s runtime/cli = %s/%s, want docker-default/claude from fixture", agentID, agent.RuntimeProfile, agent.CLIBackend)
		}
	}
	inspect := getJSON(t, app.Handler, "/inspect")
	cliRegistry := arrayField(t, inspect, "cli_adapter_registry")
	var sawClaude bool
	for _, raw := range cliRegistry {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("cli registry entry = %#v, want object", raw)
		}
		if entry["name"] == "claude" {
			sawClaude = true
			if entry["kind"] != "command" || entry["ready"] != true {
				t.Fatalf("claude entry = %#v, want ready command profile", entry)
			}
		}
	}
	if !sawClaude {
		t.Fatalf("cli registry = %#v, missing claude profile", cliRegistry)
	}
	if sessions := arrayField(t, inspect, "cli_sessions"); len(sessions) != 0 {
		t.Fatalf("inspect cli_sessions = %#v, want empty before runtime start", sessions)
	}
}

func TestCommunicationReadPublicBoundaryRequiresRuntimeTokenSubjectBinding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: threeAgentFixturePath(t),
	})
	if err != nil {
		t.Fatalf("open backend with three-agent fixture: %v", err)
	}
	defer app.Close()

	if _, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "coordinate",
		Objective:     "coordinate work",
		TargetAgentID: "coordinator",
	}); err != nil {
		t.Fatalf("add coordinator contract: %v", err)
	}
	coordinator, err := app.Runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator runtime: %v", err)
	}
	developerTask, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "developer only",
		Objective:     "developer-only envelope body",
		TargetAgentID: "developer",
	})
	if err != nil {
		t.Fatalf("add developer contract: %v", err)
	}
	developer, err := app.Runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start developer runtime: %v", err)
	}
	if _, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "verify",
		Objective:     "verify work",
		TargetAgentID: "verifier",
	}); err != nil {
		t.Fatalf("add verifier contract: %v", err)
	}
	verifier, err := app.Runner.StartNext(ctx, "verifier")
	if err != nil {
		t.Fatalf("start verifier runtime: %v", err)
	}
	sent := app.Coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          developer.LeaseID,
		AgentID:          "developer",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             "coordinator-only body summary should remain behind communication.read",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.EnvelopeID == "" || sent.Data.MailboxID == "" {
		t.Fatalf("developer message.send = %+v, want coordinator mailbox", sent)
	}

	coordinatorToken := coordinator.Env["COORDPLANE_TOKEN"]
	developerToken := developer.Env["COORDPLANE_TOKEN"]
	verifierToken := verifier.Env["COORDPLANE_TOKEN"]
	if coordinatorToken == "" || developerToken == "" || verifierToken == "" {
		t.Fatalf("runtime sessions missing tokens: coordinator=%q developer=%q verifier=%q", coordinatorToken, developerToken, verifierToken)
	}
	coordinatorSubject := capability.Subject{Kind: "agent", ID: "coordinator", AgentID: "coordinator", RuntimeID: coordinator.Route.RuntimeID}
	verifierSubject := capability.Subject{Kind: "agent", ID: "verifier", AgentID: "verifier", RuntimeID: verifier.Route.RuntimeID}
	forgedCoordinatorSubject := capability.Subject{Kind: "agent", ID: "coordinator", AgentID: "coordinator", RuntimeID: developer.Route.RuntimeID}

	cases := []struct {
		name        string
		token       string
		subject     capability.Subject
		input       map[string]any
		code        string
		forbiddenID string
	}{
		{
			name:        "missing token",
			subject:     coordinatorSubject,
			input:       map[string]any{"envelope_id": sent.Data.EnvelopeID},
			code:        "AUTH_TOKEN_REQUIRED",
			forbiddenID: sent.Data.EnvelopeID,
		},
		{
			name:        "wrong token",
			token:       "tok_wrong",
			subject:     coordinatorSubject,
			input:       map[string]any{"envelope_id": sent.Data.EnvelopeID},
			code:        "AUTH_TOKEN_REJECTED",
			forbiddenID: sent.Data.EnvelopeID,
		},
		{
			name:        "developer token cannot spoof coordinator subject",
			token:       developerToken,
			subject:     forgedCoordinatorSubject,
			input:       map[string]any{"envelope_id": sent.Data.EnvelopeID},
			code:        "AUTH_SUBJECT_MISMATCH",
			forbiddenID: sent.Data.EnvelopeID,
		},
		{
			name:        "coordinator token cannot read developer-only envelope",
			token:       coordinatorToken,
			subject:     coordinatorSubject,
			input:       map[string]any{"envelope_id": developerTask.EnvelopeID},
			code:        "COMMUNICATION_ACCESS_DENIED",
			forbiddenID: developerTask.EnvelopeID,
		},
		{
			name:        "unrelated token with known coordinator mailbox is denied",
			token:       verifierToken,
			subject:     verifierSubject,
			input:       map[string]any{"mailbox_id": sent.Data.MailboxID},
			code:        "COMMUNICATION_ACCESS_DENIED",
			forbiddenID: sent.Data.EnvelopeID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := postCapabilityCallRaw(t, app.Handler, tc.token, capability.Call{
				CapabilityName: "communication.read",
				Subject:        tc.subject,
				Input:          mustRawJSON(t, tc.input),
			}, http.StatusBadRequest)
			var response capability.Response[json.RawMessage]
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatalf("decode communication.read response: %v\nbody=%s", err, string(body))
			}
			if response.Status != capability.StatusRejected || response.ErrorCode != tc.code {
				t.Fatalf("response = %+v, want rejected %s; body=%s", response, tc.code, string(body))
			}
			assertRejectedCommunicationReadDoesNotLeak(t, body, tc.forbiddenID)
		})
	}
}

func writeTeamConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "team.yaml")
	raw := []byte(`team_id: accept-test
version: 1
runtime_profiles:
  external-local:
    kind: external
agents:
  - id: builder
    runtime_profile: external-local
    cli_backend: fake
    skills:
      - coordplane-service
    capabilities:
      - contract.current
      - object.inspect
`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write TeamConfig: %v", err)
	}
	return path
}

var threeAgentExpectedCapabilities = map[string][]string{
	"coordinator": {
		"assignment.next",
		"assignment.watch",
		"communication.read",
		"contract.add",
		"contract.complete",
		"contract.context",
		"contract.current",
		"contract.wait",
		"mailbox.get",
		"mailbox.list",
		"mailbox.resolve",
		"message.send",
		"object.inspect",
		"object.read",
		"report.submit",
	},
	"developer": {
		"assignment.next",
		"assignment.watch",
		"changeset.abandon",
		"changeset.submit",
		"communication.read",
		"contract.complete",
		"contract.context",
		"contract.current",
		"git.abort",
		"git.commit",
		"git.conflicts",
		"git.diff",
		"git.log",
		"git.merge_apply",
		"git.merge_preview",
		"git.recover",
		"git.resolve",
		"git.rollback",
		"git.status",
		"object.inspect",
		"object.read",
		"report.submit",
		"workspace.prepare",
		"workspace.status",
		"workspace.sync",
	},
	"verifier": {
		"assignment.next",
		"assignment.watch",
		"communication.read",
		"contract.complete",
		"contract.context",
		"contract.current",
		"mailbox.get",
		"mailbox.list",
		"mailbox.resolve",
		"object.inspect",
		"object.read",
		"report.submit",
		"validation.assessment",
	},
}

var threeAgentExpectedSkills = map[string][]string{
	"coordinator": {"contract-delegation", "coordplane-service"},
	"developer":   {"controlled-git", "coordplane-service"},
	"verifier":    {"coordplane-service"},
}

func threeAgentFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path: runtime caller unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "team_config", "fixtures", "cp_accept_001_three_agent.yaml")
}

func dockerThreeAgentFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path: runtime caller unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "team_config", "fixtures", "cp_accept_001_three_agent_docker.yaml")
}

func dockerClaudeThreeAgentFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path: runtime caller unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "team_config", "fixtures", "cp_accept_001_three_agent_docker_claude.yaml")
}

func getJSON(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	return getJSONWithStatus(t, handler, path, http.StatusOK)
}

func getJSONWithStatus(t *testing.T, handler http.Handler, path string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, status, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s JSON: %v; body=%s", path, err, rec.Body.String())
	}
	return out
}

func postJSONWithStatus(t *testing.T, handler http.Handler, path, body string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("POST %s status = %d, want %d; body=%s", path, rec.Code, status, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s JSON: %v; body=%s", path, err, rec.Body.String())
	}
	return out
}

func postCapabilityCallRaw(t *testing.T, handler http.Handler, token string, call capability.Call, status int) []byte {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal capability call: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("POST /call status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return raw
}

func assertRejectedCommunicationReadDoesNotLeak(t *testing.T, raw []byte, forbiddenID string) {
	t.Helper()
	for _, forbidden := range []string{forbiddenID, "envelope_id", "summary", `"body"`, "body_inline", "body_ref", "coordinator-only body", "developer-only envelope body"} {
		if forbidden != "" && bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("rejected communication.read leaked %q: %s", forbidden, string(raw))
		}
	}
}

func assertServeInspect(t *testing.T, envelope map[string]any, dbPath string) {
	t.Helper()
	if envelope["status"] != "ok" {
		t.Fatalf("inspect status = %#v, want ok", envelope["status"])
	}
	if envelope["db_path"] != dbPath || envelope["sqlite_file_backed"] != true {
		t.Fatalf("inspect db identity = %#v, want %s file-backed", envelope, dbPath)
	}
	for _, key := range []string{
		"capability_registry_initialized",
		"skill_registry_initialized",
		"queue_initialized",
		"runner_initialized",
		"team_config_loaded",
	} {
		if envelope[key] != true {
			t.Fatalf("inspect %s = %#v, want true in %#v", key, envelope[key], envelope)
		}
	}
	if envelope["acceptance_gate_state"] != "step_passed_only" {
		t.Fatalf("acceptance gate state = %#v, want step_passed_only", envelope["acceptance_gate_state"])
	}
	if got := stringSet(t, envelope["capabilities"]); !got["contract.current"] || !got["git.merge_preview"] || !got["object.inspect"] || !got["validation.assessment"] {
		t.Fatalf("capabilities = %#v, want coordination/object/git/validation capabilities", envelope["capabilities"])
	}
	if got := stringSet(t, envelope["builtin_skills"]); !got["coordplane-service"] || !got["controlled-git"] {
		t.Fatalf("builtin skills = %#v, want coordplane-service and controlled-git", envelope["builtin_skills"])
	}
	if len(arrayField(t, envelope, "migrations")) == 0 {
		t.Fatalf("migrations missing from inspect: %#v", envelope)
	}
	if len(arrayField(t, envelope, "runtime_registry")) != 1 || len(arrayField(t, envelope, "cli_adapter_registry")) < 1 {
		t.Fatalf("runtime/adapter registries = %#v/%#v, want initialized placeholders", envelope["runtime_registry"], envelope["cli_adapter_registry"])
	}
}

func capabilityNames(t *testing.T, envelope map[string]any) []string {
	t.Helper()
	var names []string
	for _, raw := range arrayField(t, envelope, "data") {
		definition, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("capability definition = %#v, want object", raw)
		}
		name, ok := definition["name"].(string)
		if !ok {
			t.Fatalf("capability name = %#v, want string", definition["name"])
		}
		names = append(names, name)
	}
	return names
}

func skillNames(t *testing.T, envelope map[string]any) []string {
	t.Helper()
	var names []string
	for _, raw := range arrayField(t, envelope, "data") {
		summary, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("skill summary = %#v, want object", raw)
		}
		name, ok := summary["name"].(string)
		if !ok {
			t.Fatalf("skill name = %#v, want string", summary["name"])
		}
		names = append(names, name)
	}
	return names
}

func assertNamesEqual(t *testing.T, got, want []string) {
	t.Helper()
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("names = %v, want %v", gotCopy, wantCopy)
	}
}

func assertJSONNamesEqual(t *testing.T, raw string, want []string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode JSON names %q: %v", raw, err)
	}
	assertNamesEqual(t, got, want)
}

func assertPromptsNotInBackendImplementation(t *testing.T, prompts []string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve backend package path: runtime caller unavailable")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatalf("read backend package dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(filepath.Dir(file), entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read backend implementation %s: %v", path, err)
		}
		source := string(raw)
		for _, prompt := range prompts {
			if strings.Contains(source, strings.TrimSpace(prompt)) {
				t.Fatalf("role prompt text from fixture is hardcoded in backend implementation %s", entry.Name())
			}
		}
	}
}

func stringSet(t *testing.T, raw any) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("raw string array = %#v, want array", raw)
	}
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("array value = %#v, want string", value)
		}
		set[text] = true
	}
	return set
}

func arrayField(t *testing.T, envelope map[string]any, key string) []any {
	t.Helper()
	values, ok := envelope[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", key, envelope[key])
	}
	return values
}

func objectField(t *testing.T, envelope map[string]any, key string) map[string]any {
	t.Helper()
	object, ok := envelope[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, envelope[key])
	}
	return object
}

func stringField(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want string", key, object[key])
	}
	return value
}

func int64Field(t *testing.T, envelope map[string]any, objectKey, key string) int64 {
	t.Helper()
	object, ok := envelope[objectKey].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", objectKey, envelope[objectKey])
	}
	value, ok := object[key].(float64)
	if !ok {
		t.Fatalf("%s.%s = %#v, want number", objectKey, key, object[key])
	}
	return int64(value)
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}
