//go:build e2e

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/gitcapture"
	"coordplane/tests/testsupport"
)

type rma02FailureTaskFact struct {
	Role string    `json:"role,omitempty"`
	Task core.Task `json:"task"`
	Run  core.Run  `json:"run"`
}

type rma02FailureFacts struct {
	Sources      []rma02FailureTaskFact `json:"sources"`
	Integrations []rma02FailureTaskFact `json:"integrations"`
	Events       []core.Event           `json:"events"`
}

func TestRMA02EvidenceValidatorRejectsIncompleteOrUnsafeProof(t *testing.T) {
	valid := validRMA02Evidence()
	if err := validateRMA02Evidence(valid, "credential-canary"); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
	mutants := []struct {
		name   string
		mutate func(*rma02Evidence)
	}{
		{"missing source", func(e *rma02Evidence) { e.Sources = e.Sources[:3] }},
		{"no overlap", func(e *rma02Evidence) { e.Overlap.ObservedAt = "2026-07-21T10:10:00Z" }},
		{"duplicate active run", func(e *rma02Evidence) { e.Sources[1].RunID = e.Sources[0].RunID }},
		{"missing progress", func(e *rma02Evidence) { e.Sources[1].ProgressMarker = "" }},
		{"missing coordlink operation", func(e *rma02Evidence) { e.Sources[2].CoordlinkOperations = 0 }},
		{"task current not observed", func(e *rma02Evidence) { e.Sources[0].TaskCurrentObserved = false }},
		{"task current belongs to another task", func(e *rma02Evidence) { e.Sources[0].TaskCurrentTaskID = e.Sources[1].TaskID }},
		{"task current belongs to another run", func(e *rma02Evidence) { e.Sources[0].TaskCurrentRunID = e.Sources[1].RunID }},
		{"fixture marker forged without event", func(e *rma02Evidence) { e.Sources[0].FixtureEventCount = 0 }},
		{"fixture execution marker missing", func(e *rma02Evidence) { e.Sources[0].FixtureMarker = "" }},
		{"message not acknowledged", func(e *rma02Evidence) { e.Message.State = "delivered" }},
		{"duplicate ack event", func(e *rma02Evidence) { e.Message.AckEventCount = 2 }},
		{"missing task ref", func(e *rma02Evidence) { e.Sources[0].TaskRef = "" }},
		{"duplicate submit side effect", func(e *rma02Evidence) { e.Sources[0].SubmitEventCount = 2 }},
		{"multiple direct CAS", func(e *rma02Evidence) { e.DirectCASCount = 2 }},
		{"direct CAS assigned to source B", func(e *rma02Evidence) { e.DirectCASTaskID = e.Sources[1].TaskID }},
		{"missing integration", func(e *rma02Evidence) { e.Integrations = e.Integrations[:2] }},
		{"extra integration", func(e *rma02Evidence) { e.Integrations = append(e.Integrations, e.Integrations[0]) }},
		{"source lineage broken", func(e *rma02Evidence) { e.Integrations[0].SourceAncestor = false }},
		{"wrong integration source run", func(e *rma02Evidence) { e.Integrations[0].SourceRunID = e.Sources[2].RunID }},
		{"crossed integration source ref", func(e *rma02Evidence) { e.Integrations[0].SourceTaskRef = e.Sources[2].TaskRef }},
		{"wrong integration source head", func(e *rma02Evidence) { e.Integrations[0].SourceHeadSHA = e.Sources[2].HeadSHA }},
		{"integration task reuses source identity", func(e *rma02Evidence) { e.Integrations[0].TaskID = e.Sources[1].TaskID }},
		{"duplicate integration head", func(e *rma02Evidence) { e.Integrations[1].HeadSHA = e.Integrations[0].HeadSHA }},
		{"stale creation canonical", func(e *rma02Evidence) { e.Integrations[1].ObservedCanonical = e.InitialSHA }},
		{"canonical lineage broken", func(e *rma02Evidence) { e.Integrations[0].CanonicalAncestor = false }},
		{"nested integration", func(e *rma02Evidence) { e.Integrations[0].NestedIntegration = true }},
		{"fixture failed", func(e *rma02Evidence) { e.Final.FixtureExitCode = 1 }},
		{"fsck failed", func(e *rma02Evidence) { e.Final.FSCKExitCode = 1 }},
		{"restart fence drift", func(e *rma02Evidence) { e.Restart.After[0].ContainerID = "ctr-replaced" }},
		{"readiness observation missing", func(e *rma02Evidence) { e.Restart.ReadyObservedAt = "" }},
		{"continue predates readiness", func(e *rma02Evidence) { e.Restart.FirstContinueAt = e.StartedAt }},
		{"mutation before ready", func(e *rma02Evidence) { e.Restart.MutationsBeforeReady = 1 }},
		{"post restart state drift", func(e *rma02Evidence) { e.Final.StateStableAfterRestart = false }},
		{"pending git action", func(e *rma02Evidence) { e.Cleanup.PendingGitActions = 1 }},
		{"blocked cleanup", func(e *rma02Evidence) { e.Cleanup.BlockedCleanup = 1 }},
		{"owned container residue", func(e *rma02Evidence) { e.Cleanup.OwnedContainers = 1 }},
		{"unknown control residue", func(e *rma02Evidence) { e.Cleanup.UnknownControlEntries = 1 }},
		{"workspace residue", func(e *rma02Evidence) { e.Cleanup.WorkspaceResidue = 1 }},
		{"handoff residue", func(e *rma02Evidence) { e.Cleanup.HandoffResidue = 1 }},
		{"log residue", func(e *rma02Evidence) { e.Cleanup.LogResidue = 1 }},
		{"task ref residue", func(e *rma02Evidence) { e.Cleanup.TaskRefResidue = 1 }},
		{"Agent home residue", func(e *rma02Evidence) { e.Cleanup.AgentHomeResidue = 1 }},
		{"scenario omitted", func(e *rma02Evidence) { e.ScenarioExecutions = 0 }},
		{"scenario duplicated", func(e *rma02Evidence) { e.ScenarioExecutions = 2 }},
		{"gate skipped", func(e *rma02Evidence) { e.Result = "SKIP" }},
		{"source dirty", func(e *rma02Evidence) { e.SourceClean = false }},
		{"missing environment", func(e *rma02Evidence) { e.Environment.Docker = "" }},
		{"failed command", func(e *rma02Evidence) { e.Commands[0].ExitCode = 1 }},
		{"secret raw", func(e *rma02Evidence) { e.Revision = "credential-canary" }},
		{"secret escaped", func(e *rma02Evidence) { e.Revision = `credential\u002dcanary` }},
		{"secret hash", func(e *rma02Evidence) {
			digest := sha256.Sum256([]byte("credential-canary"))
			e.Revision = hex.EncodeToString(digest[:])
		}},
	}
	for _, mutant := range mutants {
		t.Run(mutant.name, func(t *testing.T) {
			evidence := cloneRMA02Evidence(t, valid)
			mutant.mutate(&evidence)
			if err := validateRMA02Evidence(evidence, "credential-canary"); err == nil {
				t.Fatal("mutant was accepted")
			}
		})
	}
}

func TestRMA02OwnedResidueScannerRejectsUnknownLeaves(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		directory    bool
		allowedDirs  map[string]bool
		allowedFiles map[string]bool
	}{
		{name: "workspace", path: "project/unknown-task", directory: true, allowedDirs: map[string]bool{"project": true}},
		{name: "handoff", path: "project/task/unknown-run", directory: true, allowedDirs: map[string]bool{"project": true, "project/task": true}},
		{name: "run control", path: "unknown-run", directory: true},
		{name: "log", path: "unknown-run/run.log", allowedDirs: map[string]bool{"known-run": true}, allowedFiles: map[string]bool{"known-run/run.log": true}},
		{name: "Agent home", path: "unknown-agent", directory: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(test.path))
			if test.directory {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("residue"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			count, err := countRMA02UnexpectedEntries(root, test.allowedDirs, test.allowedFiles)
			if err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				t.Fatal("unknown owned residue was accepted")
			}
		})
	}
}

func TestRMA02ProducerArtifactsSurviveProductionCapture(t *testing.T) {
	t.Run("producer routes artifacts outside workspace", func(t *testing.T) {
		root := t.TempDir()
		workspace, _ := createRMA02SourceRepository(t, context.Background(), root)
		fixture, err := os.ReadFile(filepath.Join(workspace, "fixture-test.sh"))
		if err != nil {
			t.Fatal(err)
		}
		producer := rma02Instructions + "\n" + string(fixture)
		for _, required := range []string{
			`$HOME/.coordplane-rma02/task-current.json`,
			`artifact_root=$HOME/.coordplane-rma02`,
			`$artifact_root/fixture`, `$artifact_root/fixture-exit`,
		} {
			if !strings.Contains(producer, required) {
				t.Errorf("RMA-02 producer omits controlled artifact path %q", required)
			}
		}
		for _, forbidden := range []string{">.rma02-task-current.json", ">.rma02-fixture", ">.rma02-fixture-exit"} {
			if strings.Contains(producer, forbidden) {
				t.Errorf("RMA-02 producer leaves capture-visible artifact %q", forbidden)
			}
		}
	})

	for _, mode := range []string{"source", "integration"} {
		t.Run(mode+" artifact sequence", func(t *testing.T) {
			root := t.TempDir()
			workspace, base := createRMA02SourceRepository(t, context.Background(), root)
			testsupport.WriteFile(t, filepath.Join(workspace, "agent-A.txt"), []byte("agent-A\n"), 0o600)
			if mode == "integration" {
				testsupport.WriteFile(t, filepath.Join(workspace, "agent-B.txt"), []byte("agent-B\n"), 0o600)
			}
			git(t, context.Background(), workspace, "add", "agent-A.txt")
			if mode == "integration" {
				git(t, context.Background(), workspace, "add", "agent-B.txt")
			}
			git(t, context.Background(), workspace, "commit", "--quiet", "-m", mode+" result")
			head := git(t, context.Background(), workspace, "rev-parse", "HEAD")

			dataDir, agentID, taskID := filepath.Join(root, "data"), "agent-A", "task-"+mode
			artifacts := filepath.Join(dataDir, "agent-homes", agentID, ".coordplane-rma02")
			if err := os.MkdirAll(artifacts, 0o700); err != nil {
				t.Fatal(err)
			}
			current := core.CurrentTaskResult{Task: core.Task{ID: taskID}, Run: core.Run{ID: "run-" + mode}}
			currentJSON, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			testsupport.WriteFile(t, filepath.Join(artifacts, "task-current.json"), currentJSON, 0o600)
			testsupport.WriteFile(t, filepath.Join(artifacts, "fixture"), []byte(mode+"\n"), 0o600)
			testsupport.WriteFile(t, filepath.Join(artifacts, "fixture-exit"), []byte("0\n"), 0o600)

			fact, err := gitcapture.Capture(context.Background(), gitcapture.Request{
				Workspace: workspace, Handoff: t.TempDir(), ExpectedHead: head, BaseSHA: base,
				MaximumBundleBytes: 8 << 20, MaximumObjects: 100,
			})
			if err != nil || !fact.Clean || fact.HeadSHA != head {
				t.Fatalf("production Capture rejected %s artifact sequence: fact=%#v err=%v", mode, fact, err)
			}
			observed, marker, exitCode := readRMA02SourceArtifacts(t, dataDir, agentID, taskID)
			if observed.Task.ID != taskID || marker != mode || exitCode != 0 {
				t.Fatalf("controlled artifacts after Capture = task:%s marker:%s exit:%d", observed.Task.ID, marker, exitCode)
			}
		})
	}
}

func TestRMA02FailureClassComesFromDurableFacts(t *testing.T) {
	completeBarrier := rma02FailureSources()
	tests := []struct {
		name, want string
		facts      rma02FailureFacts
	}{
		{name: "source provider failure", want: "provider_environment", facts: rma02FailureFacts{
			Sources: []rma02FailureTaskFact{{
				Role: "A", Task: core.Task{ID: "task-A", Kind: core.TaskWork},
				Run: core.Run{ID: "run-A", TaskID: "task-A", State: core.RunFailed, RuntimeErrorCode: "PROVIDER_ERROR"},
			}},
		}},
		{name: "barrier task spec failure", want: "task_spec", facts: rma02FailureFacts{Sources: rma02FailureSources(), Events: rma02BarrierEvents(false)}},
		{name: "recovery product failure", want: "product", facts: rma02FailureFacts{Sources: completeBarrier, Events: rma02BarrierEvents(true)}},
		{name: "integration task spec failure", want: "task_spec", facts: rma02FailureFacts{
			Sources: completeBarrier, Events: rma02BarrierEvents(true),
			Integrations: []rma02FailureTaskFact{{Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration}, Run: core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited}}},
		}},
		{name: "integration capture product failure", want: "product", facts: rma02FailureFacts{
			Sources: completeBarrier, Events: rma02BarrierEvents(true),
			Integrations: []rma02FailureTaskFact{{Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration, Status: core.TaskFailed}, Run: core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited, RequestedOutcome: "submit"}}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "failure-class")
			t.Setenv("E2E_RMA02_FAILURE_CLASS_FILE", path)
			raw, err := json.Marshal(test.facts)
			if err != nil {
				t.Fatal(err)
			}
			setRMA02FailureClass(t, string(raw))
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != test.want {
				t.Fatalf("failure class = %q, want %q", got, test.want)
			}
		})
	}
}

func rma02FailureSources() []rma02FailureTaskFact {
	result := make([]rma02FailureTaskFact, 4)
	for index, role := range []string{"A", "B", "C", "D"} {
		result[index] = rma02FailureTaskFact{Role: role, Task: core.Task{ID: "task-" + role, Kind: core.TaskWork}, Run: core.Run{ID: "run-" + role, TaskID: "task-" + role, State: core.RunActive}}
	}
	return result
}

func rma02BarrierEvents(complete bool) []core.Event {
	var events []core.Event
	for _, role := range []string{"A", "B", "C", "D"} {
		if role == "D" && !complete {
			continue
		}
		events = append(events, core.Event{RunID: "run-" + role, Kind: "task.progress", PayloadJSON: `{"summary":"RMA02-READY-` + role + `"}`})
	}
	if complete {
		events = append(events, core.Event{RunID: "run-B", Kind: "message.acknowledged"})
	}
	return events
}

func TestRealMultiAgentShellMapsStubbedBoundariesToThreeTerminalStates(t *testing.T) {
	tests := []struct {
		name, image, makeMode, testMode, checkerMode string
		staleReport, pass, invalid                   bool
		wantClass                                    string
		wantCount                                    int
	}{
		{name: "admission", invalid: true},
		{name: "build failure", image: rma02StubImage(), makeMode: "fail", wantClass: "product"},
		{name: "test failure without durable class", image: rma02StubImage(), testMode: "fail", wantClass: "product", wantCount: 1},
		{name: "checker failure", image: rma02StubImage(), checkerMode: "fail", wantClass: "product", wantCount: 1},
		{name: "missing fresh report", image: rma02StubImage(), testMode: "no-report", wantClass: "product", wantCount: 1},
		{name: "stale report cannot pass", image: rma02StubImage(), testMode: "no-report", staleReport: true, wantClass: "product", wantCount: 1},
		{name: "success", image: rma02StubImage(), pass: true, wantCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, countPath := rma02StubbedShell(t, test.image, test.makeMode, test.testMode, test.checkerMode, test.staleReport)
			output, err := command.CombinedOutput()
			text := string(output)
			count := 0
			if raw, readErr := os.ReadFile(countPath); readErr == nil {
				count = strings.Count(string(raw), "test\n")
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if strings.Contains(text, realTokenCanary) {
				t.Fatal("RMA-02 shell leaked credential canary")
			}
			if test.invalid {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 || count != 0 || !strings.Contains(text, "INVALID_ENVIRONMENT(") || strings.Contains(text, "FAIL_REAL_MULTI_AGENT") || strings.Contains(text, "PASS_REAL_MULTI_AGENT_LOCAL") {
					t.Fatalf("invalid admission err=%v count=%d output=%s", err, count, text)
				}
				return
			}
			if test.pass {
				if err != nil || count != test.wantCount || !strings.Contains(text, "PASS_REAL_MULTI_AGENT_LOCAL") || strings.Contains(text, "FAIL_REAL_MULTI_AGENT") || strings.Contains(text, "INVALID_ENVIRONMENT(") {
					t.Fatalf("success err=%v count=%d output=%s", err, count, text)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || count != test.wantCount || !strings.Contains(text, "FAIL_REAL_MULTI_AGENT") || !strings.Contains(text, "failure_class="+test.wantClass) || strings.Contains(text, "PASS_REAL_MULTI_AGENT_LOCAL") || strings.Contains(text, "INVALID_ENVIRONMENT(") {
				t.Fatalf("failure err=%v count=%d output=%s", err, count, text)
			}
		})
	}
}

func rma02StubImage() string { return "sha256:" + strings.Repeat("a", 64) }

func rma02StubbedShell(t *testing.T, image, makeMode, testMode, checkerMode string, staleReport bool) (*exec.Cmd, string) {
	t.Helper()
	root, stubs := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(testsupport.RepositoryRoot(), "scripts", "e2e-real-multi-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := testsupport.WriteFile(t, filepath.Join(root, "scripts", "e2e-real-multi-agent.sh"), script, 0o700)
	testsupport.WriteFile(t, filepath.Join(stubs, "docker"), []byte("#!/bin/sh\ncase \"$1\" in version) exit 0;; image) for last do :; done; printf '%s\\n' \"$last\";; run) printf '%s\\n' '"+realClaudeVersion+"';; *) exit 2;; esac\n"), 0o700)
	testsupport.WriteFile(t, filepath.Join(stubs, "git"), []byte("#!/bin/sh\ncase \"$1:$2\" in status:--porcelain) exit 0;; *) exit 2;; esac\n"), 0o700)
	testsupport.WriteFile(t, filepath.Join(stubs, "make"), []byte("#!/bin/sh\n[ \"${STUB_MAKE_MODE:-}\" != fail ]\n"), 0o700)
	goStub := `#!/bin/sh
case "$1" in
test)
  printf 'test\n' >>"$STUB_TEST_COUNT"
  case "${STUB_TEST_MODE:-success}" in
		fail) exit 4;;
    no-report) ;;
    *) printf '{"result":"PASS_REAL_MULTI_AGENT_LOCAL"}\n' >"$E2E_RMA02_REPORT";;
  esac
  printf '{"Action":"pass","Test":"TestRealMultiAgentScenarios"}\n'
  ;;
run) [ "${STUB_CHECKER_MODE:-}" != fail ];;
*) exit 2;;
esac
`
	testsupport.WriteFile(t, filepath.Join(stubs, "go"), []byte(goStub), 0o700)
	report, countPath := filepath.Join(root, "report.json"), filepath.Join(root, "test-count")
	if staleReport {
		testsupport.WriteFile(t, report, []byte("stale-report\n"), 0o600)
	}
	command := exec.Command(scriptPath)
	command.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_RUNTIME_IMAGE="+image,
		"RMA02_OUTPUT="+report, "ANTHROPIC_AUTH_TOKEN="+realTokenCanary, "STUB_MAKE_MODE="+makeMode,
		"STUB_TEST_MODE="+testMode, "STUB_CHECKER_MODE="+checkerMode, "STUB_TEST_COUNT="+countPath,
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
	)
	return command, countPath
}

func TestRealMultiAgentRegistryIsStaticAndDataDriven(t *testing.T) {
	if len(realMultiAgentScenarios) != 1 {
		t.Fatalf("scenario count = %d, want 1", len(realMultiAgentScenarios))
	}
	seen := map[string]bool{}
	for _, scenario := range realMultiAgentScenarios {
		if strings.TrimSpace(scenario.ID) == "" || seen[scenario.ID] || scenario.Run == nil {
			t.Fatalf("invalid scenario registration: %#v", scenario)
		}
		seen[scenario.ID] = true
		if scenario.Requirements != (scenarioRequirements{Sources: 4, DirectCAS: 1, Integrations: 3, Restarts: 1, AckTransitions: 1}) {
			t.Fatalf("%s requirements = %#v", scenario.ID, scenario.Requirements)
		}
	}
}

func TestRealMultiAgentExecutorRunsEachRegistrationOnce(t *testing.T) {
	calls := map[string]int{}
	scenarios := []scenarioSpec{
		{ID: "one", Run: func(*testing.T) { calls["one"]++ }},
		{ID: "two", Run: func(*testing.T) { calls["two"]++ }},
	}
	runScenarioSpecs(t, scenarios)
	if calls["one"] != 1 || calls["two"] != 1 {
		t.Fatalf("scenario calls = %v", calls)
	}
}

func TestRealMultiAgentShellIsThinAndSingleShot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testsupport.RepositoryRoot(), "scripts", "e2e-real-multi-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, forbidden := range []string{"agent add", "task create", "message send", "task accept", "for role", "retry"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("thin shell contains scenario behavior %q", forbidden)
		}
	}
	if strings.Count(script, "go test") != 1 {
		t.Fatalf("go test invocation count = %d, want 1", strings.Count(script, "go test"))
	}
	for _, result := range []string{"PASS_REAL_MULTI_AGENT_LOCAL", "FAIL_REAL_MULTI_AGENT", "INVALID_ENVIRONMENT("} {
		if !strings.Contains(script, result) {
			t.Errorf("shell omits result %q", result)
		}
	}
}

func cloneRMA02Evidence(t *testing.T, source rma02Evidence) rma02Evidence {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone rma02Evidence
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func validRMA02Evidence() rma02Evidence {
	const c0 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	overlap := started.Add(time.Minute)
	sources := make([]rma02SourceEvidence, 4)
	before, after := make([]rma02RunFence, 4), make([]rma02RunFence, 4)
	for index, role := range []string{"A", "B", "C", "D"} {
		head := strings.Repeat(string(rune('b'+index)), 40)
		sources[index] = rma02SourceEvidence{
			Role: role, TaskID: "task-" + role, AgentID: "agent-" + role, BaseSHA: c0,
			RunID: "run-" + role, ContainerID: "container-" + role, Generation: 1, LaunchNonce: "nonce-" + role,
			LiveFrom: started.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano), LiveUntil: started.Add(2 * time.Minute).Format(time.RFC3339Nano),
			ProgressMarker: "RMA02-READY-" + role, CoordlinkOperations: 3,
			TaskCurrentObserved: true, TaskCurrentTaskID: "task-" + role, TaskCurrentRunID: "run-" + role,
			FixtureMarker: "source-" + role, FixtureEventCount: 1, FixtureExitCode: 0,
			CommitSHA: head, HeadSHA: head, HeadRunID: "run-" + role, TaskRef: "refs/coordplane/tasks/task-" + role + "/runs/run-" + role, SubmitEventCount: 1,
		}
		before[index] = rma02RunFence{TaskID: "task-" + role, AgentID: "agent-" + role, RunID: "run-" + role, ContainerID: "container-" + role, Generation: 1, LaunchNonce: "nonce-" + role}
		after[index] = before[index]
	}
	integrations := make([]rma02IntegrationProof, 0, 3)
	observedCanonical := sources[0].HeadSHA
	for index := 1; index < len(sources); index++ {
		source := sources[index]
		integrations = append(integrations, rma02IntegrationProof{
			TaskID: "integration-" + source.Role, SourceTaskID: source.TaskID, SourceRunID: source.RunID,
			SourceTaskRef: source.TaskRef, SourceHeadSHA: source.HeadSHA, ObservedCanonical: observedCanonical,
			HeadSHA: strings.Repeat(string(rune('1'+index)), 40), SourceAncestor: true, CanonicalAncestor: true, SubmitEventCount: 1,
		})
		observedCanonical = integrations[len(integrations)-1].HeadSHA
	}
	finalSHA := integrations[len(integrations)-1].HeadSHA
	return rma02Evidence{
		SchemaVersion: 1, ScenarioID: "RMA-02", ScenarioExecutions: 1, Result: "PASS_REAL_MULTI_AGENT_LOCAL",
		Revision: strings.Repeat("1", 40), SourceClean: true, ImageDigest: "sha256:" + strings.Repeat("2", 64),
		Environment: rma02EnvironmentEvidence{Go: "go1.22", Git: "git version 2", Docker: "27.0", Claude: "2.1.126 (Claude Code)"},
		Commands:    []rma02CommandEvidence{{"source-fixtures", 0}, {"source-submits", 0}, {"daemon-sigkill-restart", 0}, {"accept-cascade", 0}, {"final-fixture", 0}, {"git-fsck", 0}, {"final-restart-query", 0}, {"gc-preview", 0}, {"gc-run", 0}},
		StartedAt:   started.Format(time.RFC3339Nano), EndedAt: started.Add(5 * time.Minute).Format(time.RFC3339Nano),
		ProjectID: "project-rma02", InitialSHA: c0, Sources: sources,
		Overlap:         rma02OverlapEvidence{ObservedAt: overlap.Format(time.RFC3339Nano), ActiveRunIDs: []string{"run-A", "run-B", "run-C", "run-D"}, RunningContainerIDs: []string{"container-A", "container-B", "container-C", "container-D"}},
		Message:         rma02MessageEvidence{ID: "message-A-B", SenderRunID: "run-A", DeliveryTaskID: "task-B", RecipientAgentID: "agent-B", AcknowledgerRunID: "run-B", State: "acknowledged", CreatedEventCount: 1, AckEventCount: 1, DurableBeforeRestart: true},
		Restart:         rma02RestartEvidence{Count: 1, LiveRunsBefore: 4, Before: before, After: after, ListenerRestored: true, MessageStable: true, PendingActionsStable: true, GitFactsStable: true, ReadyObservedAt: started.Add(2 * time.Minute).Format(time.RFC3339Nano), FirstContinueAt: started.Add(3 * time.Minute).Format(time.RFC3339Nano), ReadyBeforeContinue: true},
		DirectCASTaskID: "task-A", DirectCASCount: 1, Integrations: integrations,
		Final:   rma02FinalEvidence{SQLiteCanonical: finalSHA, BossCanonical: finalSHA, ActualCanonical: finalSHA, SourceAncestors: []string{sources[0].HeadSHA, sources[1].HeadSHA, sources[2].HeadSHA, sources[3].HeadSHA}, TaskRefsVerified: 7, FixtureExitCode: 0, FSCKExitCode: 0, FinalRestartCount: 1, TasksQueried: 7, RunsQueried: 7, MessagesQueried: 5, StateStableAfterRestart: true},
		Cleanup: rma02CleanupEvidence{GCRan: true},
	}
}
