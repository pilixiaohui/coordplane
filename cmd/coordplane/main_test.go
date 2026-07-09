package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskCreateCommandPostsToOperatorTasksEndpoint(t *testing.T) {
	payloadPath := writeTaskPayload(t)
	var gotPath, gotAuth string
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode request payload: %v", err)
		}
		writeAccepted(t, w, map[string]any{
			"task_run_id":        "taskrun_create",
			"root_contract_id":   "ctr_create",
			"root_assignment_id": "asg_create",
			"status":             "created",
		})
	}))
	defer server.Close()

	t.Setenv("TEST_OPERATOR_TOKEN", "operator-secret")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"task", "create",
		"--backend-url", server.URL,
		"--operator-token-env", "TEST_OPERATOR_TOKEN",
		"--payload", payloadPath,
	}, &stdout, &stderr, server.Client())
	if err != nil {
		t.Fatalf("task create error = %v; stderr=%s", err, stderr.String())
	}
	if gotPath != "/operator/tasks" || gotAuth != "Bearer operator-secret" {
		t.Fatalf("request path/auth = %s/%s", gotPath, gotAuth)
	}
	if gotPayload["idempotency_key"] != "operator-cli-test" || gotPayload["target_agent_id"] != "coordinator" {
		t.Fatalf("payload = %#v, want operator task JSON", gotPayload)
	}
	if !strings.Contains(stdout.String(), `"task_run_id":"taskrun_create"`) {
		t.Fatalf("stdout = %s, want create response", stdout.String())
	}
}

func TestTaskRunCommandCreatesThenStartsOperatorTask(t *testing.T) {
	payloadPath := writeTaskPayload(t)
	var paths []string
	var startAuth string
	var startPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/operator/tasks":
			if r.Header.Get("Authorization") != "Bearer operator-secret" {
				t.Fatalf("create auth = %q", r.Header.Get("Authorization"))
			}
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_run",
				"root_contract_id":   "ctr_run",
				"root_assignment_id": "asg_run",
				"status":             "created",
			})
		case "/operator/tasks/taskrun_run/start":
			startAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&startPayload); err != nil {
				t.Fatalf("decode start payload: %v", err)
			}
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_run",
				"root_contract_id":   "ctr_run",
				"root_assignment_id": "asg_run",
				"target_agent_id":    "coordinator",
				"lease_id":           "lease_run",
				"attempt_id":         "att_run",
				"session_route_id":   "route_run",
				"runtime_id":         "rt_run",
				"status":             "started",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	t.Setenv("TEST_OPERATOR_TOKEN", "operator-secret")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"task", "run",
		"--backend-url", server.URL,
		"--operator-token-env", "TEST_OPERATOR_TOKEN",
		"--payload", payloadPath,
		"--start-idempotency-key", "start-cli-test",
	}, &stdout, &stderr, server.Client())
	if err != nil {
		t.Fatalf("task run error = %v; stderr=%s", err, stderr.String())
	}
	if strings.Join(paths, ",") != "/operator/tasks,/operator/tasks/taskrun_run/start" {
		t.Fatalf("paths = %v, want create then start", paths)
	}
	if startAuth != "Bearer operator-secret" || startPayload["idempotency_key"] != "start-cli-test" {
		t.Fatalf("start auth/payload = %s/%#v", startAuth, startPayload)
	}
	if !strings.Contains(stdout.String(), `"session_route_id":"route_run"`) {
		t.Fatalf("stdout = %s, want start response", stdout.String())
	}
}

func TestTaskRunCommandWaitsAndWritesEvidenceThroughOperatorAPI(t *testing.T) {
	payloadPath := writeTaskPayload(t)
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	var paths []string
	var waitPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer operator-secret" {
			t.Fatalf("%s auth = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/operator/tasks":
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_wait",
				"root_contract_id":   "ctr_wait",
				"root_assignment_id": "asg_wait",
				"status":             "created",
			})
		case "/operator/tasks/taskrun_wait/start":
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_wait",
				"root_contract_id":   "ctr_wait",
				"root_assignment_id": "asg_wait",
				"target_agent_id":    "coordinator",
				"lease_id":           "lease_wait",
				"attempt_id":         "att_wait",
				"session_route_id":   "route_wait",
				"runtime_id":         "rt_wait",
				"status":             "started",
			})
		case "/operator/tasks/taskrun_wait/wait":
			if err := json.NewDecoder(r.Body).Decode(&waitPayload); err != nil {
				t.Fatalf("decode wait payload: %v", err)
			}
			writeAccepted(t, w, map[string]any{
				"task_run_id": "taskrun_wait",
				"status":      "passed",
				"evidence": map[string]any{
					"schema_version": "operator.task.evidence.v1",
					"task_run_id":    "taskrun_wait",
					"status":         "passed",
					"terminal": map[string]any{
						"status": "passed",
					},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	t.Setenv("TEST_OPERATOR_TOKEN", "operator-secret")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"task", "run",
		"--backend-url", server.URL,
		"--operator-token-env", "TEST_OPERATOR_TOKEN",
		"--payload", payloadPath,
		"--wait",
		"--wait-timeout-ms", "25",
		"--poll-interval-ms", "1",
		"--evidence-out", evidencePath,
	}, &stdout, &stderr, server.Client())
	if err != nil {
		t.Fatalf("task run --wait error = %v; stderr=%s", err, stderr.String())
	}
	if strings.Join(paths, ",") != "/operator/tasks,/operator/tasks/taskrun_wait/start,/operator/tasks/taskrun_wait/wait" {
		t.Fatalf("paths = %v, want create start wait", paths)
	}
	if waitPayload["timeout_millis"] != float64(25) || waitPayload["poll_interval_millis"] != float64(1) {
		t.Fatalf("wait payload = %#v, want CLI timeout/poll", waitPayload)
	}
	if !strings.Contains(stdout.String(), `"status":"passed"`) {
		t.Fatalf("stdout = %s, want wait response", stdout.String())
	}
	rawEvidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("read evidence out: %v", err)
	}
	if !strings.Contains(string(rawEvidence), `"task_run_id":"taskrun_wait"`) || !strings.Contains(string(rawEvidence), `"schema_version":"operator.task.evidence.v1"`) {
		t.Fatalf("evidence file = %s, want public wait evidence", string(rawEvidence))
	}
}

func TestTaskRunCommandWritesNotPassedEvidenceWithUnfinishedCounts(t *testing.T) {
	payloadPath := writeTaskPayload(t)
	evidencePath := filepath.Join(t.TempDir(), "unfinished-evidence.json")
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer operator-secret" {
			t.Fatalf("%s auth = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/operator/tasks":
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_unfinished",
				"root_contract_id":   "ctr_unfinished",
				"root_assignment_id": "asg_unfinished",
				"status":             "created",
			})
		case "/operator/tasks/taskrun_unfinished/start":
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_unfinished",
				"root_contract_id":   "ctr_unfinished",
				"root_assignment_id": "asg_unfinished",
				"target_agent_id":    "coordinator",
				"lease_id":           "lease_unfinished",
				"attempt_id":         "att_unfinished",
				"session_route_id":   "route_unfinished",
				"runtime_id":         "rt_unfinished",
				"status":             "started",
			})
		case "/operator/tasks/taskrun_unfinished/wait":
			writeAccepted(t, w, map[string]any{
				"task_run_id":     "taskrun_unfinished",
				"status":          "timeout",
				"failure_summary": "wait timed out before unfinished lineage quiesced",
				"evidence": map[string]any{
					"schema_version": "operator.task.evidence.v1",
					"task_run_id":    "taskrun_unfinished",
					"status":         "timeout",
					"terminal": map[string]any{
						"status":                  "timeout",
						"root_contract_status":    "satisfied",
						"report_count":            1,
						"validation_pass_count":   1,
						"queued_assignment_count": 0,
						"active_assignment_count": 1,
						"active_lease_count":      1,
					},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	t.Setenv("TEST_OPERATOR_TOKEN", "operator-secret")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"task", "run",
		"--backend-url", server.URL,
		"--operator-token-env", "TEST_OPERATOR_TOKEN",
		"--payload", payloadPath,
		"--wait",
		"--wait-timeout-ms", "25",
		"--poll-interval-ms", "1",
		"--evidence-out", evidencePath,
	}, &stdout, &stderr, server.Client())
	if err != nil {
		t.Fatalf("task run unfinished wait error = %v; stderr=%s", err, stderr.String())
	}
	if strings.Join(paths, ",") != "/operator/tasks,/operator/tasks/taskrun_unfinished/start,/operator/tasks/taskrun_unfinished/wait" {
		t.Fatalf("paths = %v, want create start wait", paths)
	}
	if strings.Contains(stdout.String(), `"status":"passed"`) {
		t.Fatalf("stdout = %s, must not contain passed status", stdout.String())
	}
	rawEvidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("read unfinished evidence out: %v", err)
	}
	evidenceText := string(rawEvidence)
	if strings.Contains(evidenceText, `"status":"passed"`) ||
		!strings.Contains(evidenceText, `"root_contract_status":"satisfied"`) ||
		!strings.Contains(evidenceText, `"active_assignment_count":1`) ||
		!strings.Contains(evidenceText, `"active_lease_count":1`) {
		t.Fatalf("unfinished evidence file = %s, want not-passed evidence with unfinished counts", evidenceText)
	}
}

func TestTaskRunCommandWritesEvidenceWhenStartFails(t *testing.T) {
	payloadPath := writeTaskPayload(t)
	evidencePath := filepath.Join(t.TempDir(), "start-failure-evidence.json")
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer operator-secret" {
			t.Fatalf("%s auth = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/operator/tasks":
			writeAccepted(t, w, map[string]any{
				"task_run_id":        "taskrun_start_failed",
				"root_contract_id":   "ctr_start_failed",
				"root_assignment_id": "asg_start_failed",
				"status":             "created",
			})
		case "/operator/tasks/taskrun_start_failed/start":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          false,
				"status":      "error",
				"error_code":  "OPERATOR_TASK_START_FAILED",
				"message":     "RUNTIME_APPROVAL_POLICY_UNAVAILABLE: command runtime provider is not configured for non-interactive approval",
				"retryable":   false,
				"repair_hint": "fix runtime command_policy",
			})
		case "/operator/tasks/taskrun_start_failed/evidence":
			writeAccepted(t, w, map[string]any{
				"schema_version":  "operator.task.evidence.v1",
				"task_run_id":     "taskrun_start_failed",
				"status":          "blocked",
				"failure_class":   "runtime_approval_blocked",
				"terminal_reason": "RUNTIME_APPROVAL_POLICY_UNAVAILABLE",
				"terminal": map[string]any{
					"status":          "blocked",
					"failure_class":   "runtime_approval_blocked",
					"terminal_reason": "RUNTIME_APPROVAL_POLICY_UNAVAILABLE",
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	t.Setenv("TEST_OPERATOR_TOKEN", "operator-secret")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"task", "run",
		"--backend-url", server.URL,
		"--operator-token-env", "TEST_OPERATOR_TOKEN",
		"--payload", payloadPath,
		"--wait",
		"--evidence-out", evidencePath,
	}, &stdout, &stderr, server.Client())
	if err == nil || !strings.Contains(err.Error(), "OPERATOR_TASK_START_FAILED") {
		t.Fatalf("task run start failure error = %v; stderr=%s", err, stderr.String())
	}
	if strings.Join(paths, ",") != "/operator/tasks,/operator/tasks/taskrun_start_failed/start,/operator/tasks/taskrun_start_failed/evidence" {
		t.Fatalf("paths = %v, want create start evidence", paths)
	}
	rawEvidence, readErr := os.ReadFile(evidencePath)
	if readErr != nil {
		t.Fatalf("read evidence out after start failure: %v", readErr)
	}
	if !strings.Contains(string(rawEvidence), `"failure_class":"runtime_approval_blocked"`) ||
		!strings.Contains(string(rawEvidence), `"terminal_reason":"RUNTIME_APPROVAL_POLICY_UNAVAILABLE"`) {
		t.Fatalf("start failure evidence file = %s, want runtime approval blocker", string(rawEvidence))
	}
}

func writeTaskPayload(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.json")
	payload := []byte(`{
  "run_label": "cli operator task",
  "idempotency_key": "operator-cli-test",
  "team_id": "cp-accept-001-three-agent",
  "team_version": 1,
  "title": "CLI operator seeded task",
  "objective": "Create and start through the operator HTTP API.",
  "target_agent_id": "coordinator",
  "completion_requirements": ["report"]
}`)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return path
}

func writeAccepted(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"status": "accepted",
		"data":   data,
	}); err != nil {
		t.Fatalf("write response: %v", err)
	}
}
