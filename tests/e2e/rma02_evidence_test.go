//go:build e2e

package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/tests/testsupport"
)

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
		{"message not acknowledged", func(e *rma02Evidence) { e.Message.State = "delivered" }},
		{"duplicate ack event", func(e *rma02Evidence) { e.Message.AckEventCount = 2 }},
		{"missing task ref", func(e *rma02Evidence) { e.Sources[0].TaskRef = "" }},
		{"duplicate submit side effect", func(e *rma02Evidence) { e.Sources[0].SubmitEventCount = 2 }},
		{"multiple direct CAS", func(e *rma02Evidence) { e.DirectCASCount = 2 }},
		{"missing integration", func(e *rma02Evidence) { e.Integrations = e.Integrations[:2] }},
		{"extra integration", func(e *rma02Evidence) { e.Integrations = append(e.Integrations, e.Integrations[0]) }},
		{"source lineage broken", func(e *rma02Evidence) { e.Integrations[0].SourceAncestor = false }},
		{"canonical lineage broken", func(e *rma02Evidence) { e.Integrations[0].CanonicalAncestor = false }},
		{"nested integration", func(e *rma02Evidence) { e.Integrations[0].NestedIntegration = true }},
		{"fixture failed", func(e *rma02Evidence) { e.Final.FixtureExitCode = 1 }},
		{"fsck failed", func(e *rma02Evidence) { e.Final.FSCKExitCode = 1 }},
		{"restart fence drift", func(e *rma02Evidence) { e.Restart.After[0].ContainerID = "ctr-replaced" }},
		{"mutation before ready", func(e *rma02Evidence) { e.Restart.MutationsBeforeReady = 1 }},
		{"post restart state drift", func(e *rma02Evidence) { e.Final.StateStableAfterRestart = false }},
		{"pending git action", func(e *rma02Evidence) { e.Cleanup.PendingGitActions = 1 }},
		{"blocked cleanup", func(e *rma02Evidence) { e.Cleanup.BlockedCleanup = 1 }},
		{"owned container residue", func(e *rma02Evidence) { e.Cleanup.OwnedContainers = 1 }},
		{"unknown control residue", func(e *rma02Evidence) { e.Cleanup.UnknownControlEntries = 1 }},
		{"workspace residue", func(e *rma02Evidence) { e.Cleanup.WorkspaceResidue = 1 }},
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
			ProgressMarker: "RMA02-READY-" + role, CoordlinkOperations: 3, FixtureExitCode: 0,
			CommitSHA: head, HeadSHA: head, HeadRunID: "run-" + role, TaskRef: "refs/coordplane/tasks/task-" + role + "/runs/run-" + role, SubmitEventCount: 1,
		}
		before[index] = rma02RunFence{TaskID: "task-" + role, AgentID: "agent-" + role, RunID: "run-" + role, ContainerID: "container-" + role, Generation: 1, LaunchNonce: "nonce-" + role}
		after[index] = before[index]
	}
	integrations := make([]rma02IntegrationProof, 0, 3)
	for index := 1; index < len(sources); index++ {
		source := sources[index]
		integrations = append(integrations, rma02IntegrationProof{
			TaskID: "integration-" + source.Role, SourceTaskID: source.TaskID, SourceRunID: source.RunID,
			SourceTaskRef: source.TaskRef, SourceHeadSHA: source.HeadSHA, ObservedCanonical: strings.Repeat("f", 40),
			HeadSHA: strings.Repeat(string(rune('1'+index)), 40), SourceAncestor: true, CanonicalAncestor: true, SubmitEventCount: 1,
		})
	}
	finalSHA := strings.Repeat("f", 40)
	return rma02Evidence{
		SchemaVersion: 1, ScenarioID: "RMA-02", ScenarioExecutions: 1, Result: "PASS_REAL_MULTI_AGENT_LOCAL",
		Revision: strings.Repeat("1", 40), SourceClean: true, ImageDigest: "sha256:" + strings.Repeat("2", 64),
		Environment: rma02EnvironmentEvidence{Go: "go1.22", Git: "git version 2", Docker: "27.0", Claude: "2.1.126 (Claude Code)"},
		Commands:    []rma02CommandEvidence{{"source-fixtures", 0}, {"source-submits", 0}, {"daemon-sigkill-restart", 0}, {"accept-cascade", 0}, {"final-fixture", 0}, {"git-fsck", 0}, {"final-restart-query", 0}, {"gc-preview", 0}, {"gc-run", 0}},
		StartedAt:   started.Format(time.RFC3339Nano), EndedAt: started.Add(5 * time.Minute).Format(time.RFC3339Nano),
		ProjectID: "project-rma02", InitialSHA: c0, Sources: sources,
		Overlap:         rma02OverlapEvidence{ObservedAt: overlap.Format(time.RFC3339Nano), ActiveRunIDs: []string{"run-A", "run-B", "run-C", "run-D"}, RunningContainerIDs: []string{"container-A", "container-B", "container-C", "container-D"}},
		Message:         rma02MessageEvidence{ID: "message-A-B", SenderRunID: "run-A", DeliveryTaskID: "task-B", RecipientAgentID: "agent-B", AcknowledgerRunID: "run-B", State: "acknowledged", CreatedEventCount: 1, AckEventCount: 1, DurableBeforeRestart: true},
		Restart:         rma02RestartEvidence{Count: 1, LiveRunsBefore: 4, Before: before, After: after, ListenerRestored: true, MessageStable: true, PendingActionsStable: true, GitFactsStable: true, ReadyBeforeContinue: true},
		DirectCASTaskID: "task-A", DirectCASCount: 1, Integrations: integrations,
		Final:   rma02FinalEvidence{SQLiteCanonical: finalSHA, BossCanonical: finalSHA, ActualCanonical: finalSHA, SourceAncestors: []string{sources[0].HeadSHA, sources[1].HeadSHA, sources[2].HeadSHA, sources[3].HeadSHA}, TaskRefsVerified: 7, FixtureExitCode: 0, FSCKExitCode: 0, FinalRestartCount: 1, TasksQueried: 7, RunsQueried: 7, MessagesQueried: 5, StateStableAfterRestart: true},
		Cleanup: rma02CleanupEvidence{GCRan: true},
	}
}
