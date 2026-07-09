package coordlinkcli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"coordplane/internal/backend"
	"coordplane/internal/coordlinkcli"
	cpruntime "coordplane/internal/runtime"
)

func TestCoordlinkCLIUsesBackendTypedResponsesAndDurableCallAudit(t *testing.T) {
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
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	auth := insertRuntimeAuthSession(t, ctx, app.DB, "builder")

	env := mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL": server.URL,
		"COORDPLANE_AGENT_ID":    "builder",
		"COORDPLANE_RUNTIME_ID":  auth.RuntimeID,
		"COORDPLANE_TOKEN":       auth.Token,
	})

	list := runCLI(t, env, "capability", "list")
	if list.code != 0 {
		t.Fatalf("capability list exit = %d stderr=%s stdout=%s", list.code, list.stderr, list.stdout)
	}
	listEnvelope := decodeEnvelope(t, list.stdout)
	if listEnvelope["status"] != "accepted" {
		t.Fatalf("capability list = %#v, want accepted", listEnvelope)
	}
	if names := capabilityNames(t, listEnvelope); strings.Join(names, ",") != "contract.current" {
		t.Fatalf("capability names = %v, want only contract.current", names)
	}

	current := runCLI(t, env, "call", "contract.current")
	if current.code != 0 {
		t.Fatalf("contract.current exit = %d stderr=%s stdout=%s", current.code, current.stderr, current.stdout)
	}
	currentEnvelope := decodeEnvelope(t, current.stdout)
	if currentEnvelope["status"] != "accepted" {
		t.Fatalf("contract.current envelope = %#v, want accepted", currentEnvelope)
	}
	currentData, ok := currentEnvelope["data"].(map[string]any)
	if !ok || currentData["id"] != "ctr_builder_coordlink_cli" {
		t.Fatalf("contract.current data = %#v, want builder auth contract", currentEnvelope["data"])
	}

	skillList := runCLI(t, env, "skill", "list")
	if skillList.code != 0 {
		t.Fatalf("skill list exit = %d stderr=%s stdout=%s", skillList.code, skillList.stderr, skillList.stdout)
	}
	skillListEnvelope := decodeEnvelope(t, skillList.stdout)
	if skillListEnvelope["status"] != "accepted" || !strings.Contains(skillList.stdout, "coordplane-service") {
		t.Fatalf("skill list envelope = %#v stdout=%s, want coordplane-service", skillListEnvelope, skillList.stdout)
	}

	skillRead := runCLI(t, env, "skill", "read", "coordplane-service")
	if skillRead.code != 0 {
		t.Fatalf("skill read exit = %d stderr=%s stdout=%s", skillRead.code, skillRead.stderr, skillRead.stdout)
	}
	skillReadEnvelope := decodeEnvelope(t, skillRead.stdout)
	if skillReadEnvelope["status"] != "accepted" || !strings.Contains(skillRead.stdout, "contract.current") {
		t.Fatalf("skill read envelope = %#v stdout=%s, want content from backend", skillReadEnvelope, skillRead.stdout)
	}

	unauthorized := runCLI(t, env, "call", "object.read", "--input", `{"object_ref":"obj_sha256_missing"}`)
	if unauthorized.code != 2 {
		t.Fatalf("unauthorized exit = %d stderr=%s stdout=%s", unauthorized.code, unauthorized.stderr, unauthorized.stdout)
	}
	unauthorizedEnvelope := decodeEnvelope(t, unauthorized.stdout)
	if unauthorizedEnvelope["status"] != "rejected" || unauthorizedEnvelope["error_code"] != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("unauthorized envelope = %#v, want typed rejected preserved", unauthorizedEnvelope)
	}

	if calls := countRows(t, ctx, app.DB, "capability_calls"); calls != 3 {
		t.Fatalf("capability_calls = %d, want capability.list + two calls", calls)
	}
	inspect := getInspect(t, server.URL)
	counts, ok := inspect["counts"].(map[string]any)
	if !ok {
		t.Fatalf("inspect counts = %#v, want object", inspect["counts"])
	}
	if got := int64(counts["capability_calls"].(float64)); got != 3 {
		t.Fatalf("inspect capability_calls = %d, want 3", got)
	}
}

func TestCoordlinkCLIProviderPolicyStrictModeRejectsSuffixOverridesBeforeSideEffects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: threeAgentFixturePath(t),
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	auth := insertRuntimeAuthSession(t, ctx, app.DB, "coordinator")
	attackerHeaders := make(chan string, 1)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHeaders <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"accepted","data":{}}`))
	}))
	defer attacker.Close()
	inputPath := filepath.Join(dir, "provider-input.json")
	if err := os.WriteFile(inputPath, []byte(`{"title":"stolen","objective":"from input-file","target_agent_id":"builder"}`), 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}
	env := mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL":                   server.URL,
		"COORDPLANE_AGENT_ID":                      "coordinator",
		"COORDPLANE_RUNTIME_ID":                    auth.RuntimeID,
		"COORDPLANE_WORKSPACE_ID":                  "workspace_provider_policy",
		"COORDPLANE_TOKEN":                         auth.Token,
		"COORDPLANE_LEASE_ID":                      auth.LeaseID,
		"COORDPLANE_TRACE_ID":                      "trace_provider_policy_strict",
		"COORDPLANE_PROVIDER_POLICY_MODE":          "strict_coordlink_call",
		"COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES": "contract.current,contract.add",
		"COORDPLANE_PROVIDER_AUDIT_TRACE_ID":       "trace_provider_policy_strict",
		"COORDPLANE_PROVIDER_AUDIT_AGENT_ID":       "coordinator",
		"COORDPLANE_PROVIDER_AUDIT_LEASE_ID":       auth.LeaseID,
	})

	current := runCLI(t, env, "call", "contract.current")
	if current.code != 0 {
		t.Fatalf("strict contract.current exit=%d stdout=%s stderr=%s", current.code, current.stdout, current.stderr)
	}
	childInput := `{"title":"strict child","objective":"child from strict provider policy","target_agent_id":"developer"}`
	add := runCLI(t, env, "call", "contract.add", "--input", childInput)
	if add.code != 0 {
		t.Fatalf("strict contract.add exit=%d stdout=%s stderr=%s", add.code, add.stdout, add.stderr)
	}
	idempotentChildInput := `{"title":"strict idempotent child","objective":"child from strict provider policy idempotency","target_agent_id":"developer"}`
	idempotentAdd := runCLI(t, env, "call", "contract.add", "--input", idempotentChildInput, "--idempotency-key", "strict-ok")
	if idempotentAdd.code != 0 {
		t.Fatalf("strict contract.add with idempotency exit=%d stdout=%s stderr=%s", idempotentAdd.code, idempotentAdd.stdout, idempotentAdd.stderr)
	}
	if got := countRowsWhere(t, ctx, app.DB, "capability_calls", "trace_id = 'trace_provider_policy_strict' AND capability_name = 'contract.add' AND subject_kind = 'agent' AND subject_id = 'coordinator' AND status = 'accepted' AND idempotency_key = 'strict-ok'"); got != 1 {
		t.Fatalf("strict idempotent accepted contract.add calls = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, app.DB,
		"work_contracts c JOIN assignments a ON a.contract_id = c.id JOIN mailbox_items m ON m.contract_id = c.id",
		"c.title = 'strict idempotent child' AND c.target_id = 'developer' AND c.status = 'open' AND a.state = 'queued' AND m.state = 'pending'",
	); got != 1 {
		t.Fatalf("strict idempotent child durable state count = %d, want child contract/assignment/mailbox", got)
	}
	beforeCalls := countRows(t, ctx, app.DB, "capability_calls")
	beforeContracts := countRows(t, ctx, app.DB, "work_contracts")
	beforeAssignments := countRows(t, ctx, app.DB, "assignments")
	beforeMailbox := countRows(t, ctx, app.DB, "mailbox_items")

	cases := []struct {
		name string
		args []string
	}{
		{name: "attacker backend", args: []string{"call", "contract.current", "--backend", attacker.URL}},
		{name: "input file", args: []string{"call", "contract.add", "--input-file", inputPath}},
		{name: "token override", args: []string{"call", "contract.current", "--token", "LEAKED_RUNTIME_TOKEN"}},
		{name: "agent override", args: []string{"call", "contract.current", "--agent", "attacker-agent"}},
		{name: "runtime override", args: []string{"call", "contract.current", "--runtime", "rt_attacker"}},
		{name: "workspace override", args: []string{"call", "contract.current", "--workspace", "ws_attacker"}},
		{name: "trace override", args: []string{"call", "contract.current", "--trace-id", "attacker"}},
		{name: "lease override", args: []string{"call", "contract.current", "--lease-id", "other"}},
		{name: "scope override", args: []string{"call", "contract.current", "--scope", `{"lease_id":"other"}`}},
		{name: "url in input", args: []string{"call", "contract.add", "--input", `{"title":"bad","objective":"https://attacker.invalid","target_agent_id":"builder"}`}},
		{name: "host path in input", args: []string{"call", "contract.add", "--input", `{"title":"bad","objective":"/home/agent/secret","target_agent_id":"builder"}`}},
		{name: "shell metachar", args: []string{"call", "contract.current", "--idempotency-key", "ok; cat /home/agent/secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLI(t, env, tc.args...)
			if result.code != 2 {
				t.Fatalf("%s exit=%d stdout=%s stderr=%s, want provider policy denial", tc.name, result.code, result.stdout, result.stderr)
			}
			for _, forbidden := range []string{auth.Token, "LEAKED_RUNTIME_TOKEN", inputPath, attacker.URL, "Authorization", "Bearer"} {
				if strings.Contains(result.stderr, forbidden) || strings.Contains(result.stdout, forbidden) {
					t.Fatalf("%s leaked forbidden marker %q stdout=%s stderr=%s", tc.name, forbidden, result.stdout, result.stderr)
				}
			}
		})
	}
	select {
	case header := <-attackerHeaders:
		t.Fatalf("attacker backend received Authorization header %q", header)
	default:
	}
	if got := countRows(t, ctx, app.DB, "capability_calls"); got != beforeCalls {
		t.Fatalf("capability_calls = %d, want unchanged %d", got, beforeCalls)
	}
	if got := countRows(t, ctx, app.DB, "work_contracts"); got != beforeContracts {
		t.Fatalf("work_contracts = %d, want unchanged %d", got, beforeContracts)
	}
	if got := countRows(t, ctx, app.DB, "assignments"); got != beforeAssignments {
		t.Fatalf("assignments = %d, want unchanged %d", got, beforeAssignments)
	}
	if got := countRows(t, ctx, app.DB, "mailbox_items"); got != beforeMailbox {
		t.Fatalf("mailbox_items = %d, want unchanged %d", got, beforeMailbox)
	}
	if got := countRowsWhere(t, ctx, app.DB, "work_contracts", "title = 'stolen' OR title = 'bad'"); got != 0 {
		t.Fatalf("denied suffix created %d forbidden child contracts", got)
	}
}

func TestCoordlinkCLIThreeAgentFixtureScopesCapabilitiesAndSkills(t *testing.T) {
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
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	sessions := map[string]authSession{
		"coordinator": insertRuntimeAuthSession(t, ctx, app.DB, "coordinator"),
		"developer":   insertRuntimeAuthSession(t, ctx, app.DB, "developer"),
		"verifier":    insertRuntimeAuthSession(t, ctx, app.DB, "verifier"),
	}

	for _, agentID := range []string{"coordinator", "developer", "verifier"} {
		env := mapEnv(map[string]string{
			"COORDPLANE_BACKEND_URL": server.URL,
			"COORDPLANE_AGENT_ID":    agentID,
			"COORDPLANE_RUNTIME_ID":  sessions[agentID].RuntimeID,
			"COORDPLANE_TOKEN":       sessions[agentID].Token,
		})
		list := runCLI(t, env, "capability", "list")
		if list.code != 0 {
			t.Fatalf("%s capability list exit = %d stderr=%s stdout=%s", agentID, list.code, list.stderr, list.stdout)
		}
		assertNamesEqual(t, capabilityNames(t, decodeEnvelope(t, list.stdout)), threeAgentExpectedCapabilities[agentID])

		skills := runCLI(t, env, "skill", "list")
		if skills.code != 0 {
			t.Fatalf("%s skill list exit = %d stderr=%s stdout=%s", agentID, skills.code, skills.stderr, skills.stdout)
		}
		assertNamesEqual(t, skillNames(t, decodeEnvelope(t, skills.stdout)), threeAgentExpectedSkills[agentID])
	}

	developerEnv := mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL": server.URL,
		"COORDPLANE_AGENT_ID":    "developer",
		"COORDPLANE_RUNTIME_ID":  sessions["developer"].RuntimeID,
		"COORDPLANE_TOKEN":       sessions["developer"].Token,
	})
	controlledGit := runCLI(t, developerEnv, "skill", "read", "controlled-git")
	if controlledGit.code != 0 {
		t.Fatalf("developer skill read exit = %d stderr=%s stdout=%s", controlledGit.code, controlledGit.stderr, controlledGit.stdout)
	}
	controlledGitEnvelope := decodeEnvelope(t, controlledGit.stdout)
	if controlledGitEnvelope["status"] != "accepted" || !strings.Contains(controlledGit.stdout, "git.commit") {
		t.Fatalf("developer controlled-git read = %#v stdout=%s, want accepted content", controlledGitEnvelope, controlledGit.stdout)
	}

	verifierEnv := mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL": server.URL,
		"COORDPLANE_AGENT_ID":    "verifier",
		"COORDPLANE_RUNTIME_ID":  sessions["verifier"].RuntimeID,
		"COORDPLANE_TOKEN":       sessions["verifier"].Token,
	})
	deniedSkill := runCLI(t, verifierEnv, "skill", "read", "controlled-git")
	if deniedSkill.code != 2 {
		t.Fatalf("verifier skill read exit = %d stderr=%s stdout=%s", deniedSkill.code, deniedSkill.stderr, deniedSkill.stdout)
	}
	deniedSkillEnvelope := decodeEnvelope(t, deniedSkill.stdout)
	if deniedSkillEnvelope["status"] != "rejected" || deniedSkillEnvelope["error_code"] != "SKILL_READ_REJECTED" {
		t.Fatalf("verifier controlled-git read = %#v, want typed rejected", deniedSkillEnvelope)
	}
	if _, ok := deniedSkillEnvelope["data"]; ok {
		t.Fatalf("verifier unauthorized skill read leaked data: %#v", deniedSkillEnvelope)
	}

	deniedCapability := runCLI(t, verifierEnv, "call", "git.commit", "--input", `{"message":"not allowed","paths":["feature.txt"]}`)
	if deniedCapability.code != 2 {
		t.Fatalf("verifier git.commit exit = %d stderr=%s stdout=%s", deniedCapability.code, deniedCapability.stderr, deniedCapability.stdout)
	}
	deniedCapabilityEnvelope := decodeEnvelope(t, deniedCapability.stdout)
	if deniedCapabilityEnvelope["status"] != "rejected" || deniedCapabilityEnvelope["error_code"] != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("verifier git.commit = %#v, want unauthorized typed rejected", deniedCapabilityEnvelope)
	}
	if _, ok := deniedCapabilityEnvelope["data"]; ok {
		t.Fatalf("verifier unauthorized capability call leaked data: %#v", deniedCapabilityEnvelope)
	}
	if calls := countRows(t, ctx, app.DB, "capability_calls"); calls != 4 {
		t.Fatalf("capability_calls = %d, want three capability.list audits plus rejected git.commit", calls)
	}
}

type cliResult struct {
	code   int
	stdout string
	stderr string
}

type authSession struct {
	Token     string
	RuntimeID string
	LeaseID   string
}

func insertRuntimeAuthSession(t *testing.T, ctx context.Context, db *sql.DB, agentID string) authSession {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	token := "tok_" + agentID + "_coordlink_cli"
	runtimeID := "rt_" + agentID + "_coordlink_cli"
	attemptID := "att_" + agentID + "_coordlink_cli"
	leaseID := "lease_" + agentID + "_coordlink_cli"
	assignmentID := "asg_" + agentID + "_coordlink_cli"
	routeID := "route_" + agentID + "_coordlink_cli"
	contractID := "ctr_" + agentID + "_coordlink_cli"
	execSQL(t, ctx, db, `
INSERT INTO work_contracts (
  id, title, objective, issuer_agent_id, issuer_contract_id, target_kind,
  target_id, status, completion_requirements_json, acceptance_policy_json,
  created_at, updated_at
) VALUES (?, 'auth', 'auth discovery', 'operator', '', 'agent', ?, 'active', '{}', '{}', ?, ?)`,
		contractID, agentID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO assignments (
  id, contract_id, assignee_agent_id, assignee_role, state, priority, reason,
  session_route_id, created_at, updated_at
) VALUES (?, ?, ?, '', 'claimed', 0, 'auth discovery', ?, ?, ?)`,
		assignmentID, contractID, agentID, routeID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO session_routes (
  id, agent_id, runtime_id, cli_backend, session_native_id, route_json, state,
  created_at, updated_at
) VALUES (?, ?, ?, 'fake', ?, '{}', 'active', ?, ?)`,
		routeID, agentID, runtimeID, "native_"+agentID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO leases (
  id, assignment_id, agent_id, runtime_id, session_route_id, state, expires_at,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		leaseID, assignmentID, agentID, runtimeID, routeID, time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), now, now)
	execSQL(t, ctx, db, `
INSERT INTO attempts (
  id, lease_id, cli_backend, runtime_kind, session_native_id, start_reason,
  status, transcript_ref, started_at
) VALUES (?, ?, 'fake', 'external', ?, 'initial', 'running', '', ?)`,
		attemptID, leaseID, "native_"+agentID, now)
	execSQL(t, ctx, db, `
INSERT INTO runtime_instances (
  id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id,
  lease_id, state, workspace_path, home_path, checks_json, env_keys_json,
  created_at, updated_at
) VALUES (?, ?, 'external-local', 'external', ?, ?, ?, 'ready', '/workspace', '/home/agent', '{}', '[]', ?, ?)`,
		"rti_"+agentID+"_coordlink_cli", runtimeID, agentID, attemptID, leaseID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO runtime_tokens (
  token_hash, agent_id, runtime_id, attempt_id, lease_id, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`,
		cpruntime.RuntimeTokenHash(token), agentID, runtimeID, attemptID, leaseID, now, now)
	return authSession{Token: token, RuntimeID: runtimeID, LeaseID: leaseID}
}

func execSQL(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec SQL: %v\n%s", err, query)
	}
}

func runCLI(t *testing.T, env func(string) string, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := coordlinkcli.Run(context.Background(), args, env, strings.NewReader(""), &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func writeTeamConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "team.yaml")
	raw := []byte(`team_id: coordlink-test
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
`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write TeamConfig: %v", err)
	}
	return path
}

func threeAgentFixturePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path: runtime caller unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "team_config", "fixtures", "cp_accept_001_three_agent.yaml")
}

func decodeEnvelope(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode envelope: %v; raw=%s", err, raw)
	}
	return out
}

func capabilityNames(t *testing.T, envelope map[string]any) []string {
	t.Helper()
	rawData, ok := envelope["data"].([]any)
	if !ok {
		t.Fatalf("data = %#v, want array", envelope["data"])
	}
	var names []string
	for _, raw := range rawData {
		definition, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("definition = %#v, want object", raw)
		}
		name, ok := definition["name"].(string)
		if !ok {
			t.Fatalf("name = %#v, want string", definition["name"])
		}
		names = append(names, name)
	}
	return names
}

func skillNames(t *testing.T, envelope map[string]any) []string {
	t.Helper()
	rawData, ok := envelope["data"].([]any)
	if !ok {
		t.Fatalf("data = %#v, want array", envelope["data"])
	}
	var names []string
	for _, raw := range rawData {
		summary, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("summary = %#v, want object", raw)
		}
		name, ok := summary["name"].(string)
		if !ok {
			t.Fatalf("name = %#v, want string", summary["name"])
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

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", table, where)).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func getInspect(t *testing.T, backendURL string) map[string]any {
	t.Helper()
	httpResp, httpErr := http.Get(backendURL + "/inspect")
	if httpErr != nil {
		t.Fatalf("GET inspect: %v", httpErr)
	}
	defer httpResp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	return out
}
