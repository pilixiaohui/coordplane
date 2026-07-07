package coordlinkcli_test

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
	"coordplane/internal/coordlinkcli"
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

	env := mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL": server.URL,
		"COORDPLANE_AGENT_ID":    "builder",
		"COORDPLANE_RUNTIME_ID":  "runtime_test",
		"COORDPLANE_TOKEN":       "agent-token-test",
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
	if current.code != 1 {
		t.Fatalf("contract.current exit = %d stderr=%s stdout=%s", current.code, current.stderr, current.stdout)
	}
	currentEnvelope := decodeEnvelope(t, current.stdout)
	if currentEnvelope["status"] != "error" || currentEnvelope["error_code"] != "CONTRACT_CURRENT_FAILED" {
		t.Fatalf("contract.current envelope = %#v, want backend typed error", currentEnvelope)
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

	for _, agentID := range []string{"coordinator", "developer", "verifier"} {
		env := mapEnv(map[string]string{
			"COORDPLANE_BACKEND_URL": server.URL,
			"COORDPLANE_AGENT_ID":    agentID,
			"COORDPLANE_RUNTIME_ID":  "runtime_" + agentID,
			"COORDPLANE_TOKEN":       "agent-token-test",
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
