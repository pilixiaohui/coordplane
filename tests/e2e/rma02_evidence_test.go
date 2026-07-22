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
	"syscall"
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
		{"integration fixture wrong task", func(e *rma02Evidence) { e.Integrations[0].FixtureTaskID = e.Integrations[1].TaskID }},
		{"integration fixture wrong run", func(e *rma02Evidence) { e.Integrations[0].FixtureRunID = e.Integrations[1].RunID }},
		{"integration fixture marker missing", func(e *rma02Evidence) { e.Integrations[0].FixtureMarker = "" }},
		{"integration fixture event missing", func(e *rma02Evidence) { e.Integrations[0].FixtureEventCount = 0 }},
		{"integration fixture event duplicated", func(e *rma02Evidence) { e.Integrations[0].FixtureEventCount = 2 }},
		{"integration fixture failed", func(e *rma02Evidence) { e.Integrations[0].FixtureExitCode = 1 }},
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

func TestRMA02FormalArtifactWriterSurvivesProductionCapture(t *testing.T) {
	t.Run("producer routes artifacts outside workspace", func(t *testing.T) {
		root := t.TempDir()
		workspace, _ := createRMA02SourceRepository(t, context.Background(), root)
		fixture, err := os.ReadFile(filepath.Join(workspace, "fixture-test.sh"))
		if err != nil {
			t.Fatal(err)
		}
		producer := rma02Instructions + "\n" + string(fixture)
		for _, required := range []string{
			`artifact_root=$HOME/.coordplane-rma02/$task_id/$run_id`,
			`RMA02_COORDLINK_BIN`,
			`task current --output json`,
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
			dataDir, agentID, taskID, runID := filepath.Join(root, "data"), "agent-A", "task-"+mode, "run-"+mode
			home := filepath.Join(dataDir, "agent-homes", agentID)
			progress := executeRMA02FixtureWriter(t, workspace, home, mode, "A", taskID, runID)
			git(t, context.Background(), workspace, "add", "agent-A.txt")
			if mode == "integration" {
				git(t, context.Background(), workspace, "add", "agent-B.txt")
			}
			git(t, context.Background(), workspace, "commit", "--quiet", "-m", mode+" result")
			head := git(t, context.Background(), workspace, "rev-parse", "HEAD")

			fact, err := gitcapture.Capture(context.Background(), gitcapture.Request{
				Workspace: workspace, Handoff: t.TempDir(), ExpectedHead: head, BaseSHA: base,
				MaximumBundleBytes: 8 << 20, MaximumObjects: 100,
			})
			if err != nil || !fact.Clean || fact.HeadSHA != head {
				t.Fatalf("production Capture rejected %s artifact sequence: fact=%#v err=%v", mode, fact, err)
			}
			observed, marker, exitCode := readRMA02SourceArtifacts(t, dataDir, agentID, taskID)
			wantMarker := mode
			if mode == "source" {
				wantMarker = "source-A"
			}
			if observed.Task.ID != taskID || observed.Run.ID != runID || marker != wantMarker || exitCode != 0 {
				t.Fatalf("controlled artifacts after Capture = task:%s run:%s marker:%s exit:%d", observed.Task.ID, observed.Run.ID, marker, exitCode)
			}
			if strings.Count(progress, "\n") != 1 || !strings.Contains(progress, taskID) || !strings.Contains(progress, runID) {
				t.Fatalf("formal fixture progress = %q, want one Task/Run-bound Event", progress)
			}
		})
	}
}

func TestRMA02RejectsStaleArtifactForSameIntegrationAgent(t *testing.T) {
	if os.Getenv("RMA02_STALE_ARTIFACT_CHILD") == "1" {
		requireRMA02FixtureMarker(t, os.Getenv("RMA02_STALE_DATA_DIR"), "agent-A", "integration-C", "integration")
		return
	}
	root := t.TempDir()
	workspace, _ := createRMA02SourceRepository(t, context.Background(), root)
	testsupport.WriteFile(t, filepath.Join(workspace, "agent-A.txt"), []byte("agent-A\n"), 0o600)
	testsupport.WriteFile(t, filepath.Join(workspace, "agent-B.txt"), []byte("agent-B\n"), 0o600)
	dataDir := filepath.Join(root, "data")
	executeRMA02FixtureWriter(t, workspace, filepath.Join(dataDir, "agent-homes", "agent-A"), "integration", "A", "integration-B", "run-integration-B")

	command := exec.Command(os.Args[0], "-test.run=^TestRMA02RejectsStaleArtifactForSameIntegrationAgent$")
	command.Env = append(os.Environ(), "RMA02_STALE_ARTIFACT_CHILD=1", "RMA02_STALE_DATA_DIR="+dataDir)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("integration C accepted integration B artifact: %s", output)
	}
}

func TestRMA02ArtifactReaderRejectsUntrustedFilesWithoutLeaking(t *testing.T) {
	if mode := os.Getenv("RMA02_UNTRUSTED_ARTIFACT_CHILD"); mode != "" {
		dataDir := os.Getenv("RMA02_UNTRUSTED_ARTIFACT_DATA")
		if mode == "replacement" {
			runRoot := filepath.Join(rma02ArtifactRoot(dataDir, "agent-A"), "task-A", "run-A")
			for index := 0; index < 200; index++ {
				replacement := filepath.Join(runRoot, "fixture.replacement")
				if index%2 == 0 {
					if err := os.Symlink("/proc/self/environ", replacement); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(replacement, []byte("source-A\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, filepath.Join(runRoot, "fixture")); err != nil {
					t.Fatal(err)
				}
				requireRMA02FixtureMarker(t, dataDir, "agent-A", "task-A", "source-A")
			}
			return
		}
		requireRMA02FixtureMarker(t, dataDir, "agent-A", "task-A", "source-A")
		return
	}

	const canary = "rma02-artifact-secret-canary"
	for _, mode := range []string{"symlink", "fifo", "oversized", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			dataDir := writeRMA02ArtifactFixture(t)
			runRoot := filepath.Join(rma02ArtifactRoot(dataDir, "agent-A"), "task-A", "run-A")
			switch mode {
			case "symlink":
				requireNoError(t, os.Remove(filepath.Join(runRoot, "fixture")))
				requireNoError(t, os.Symlink("/proc/self/environ", filepath.Join(runRoot, "fixture")))
			case "fifo":
				requireNoError(t, os.Remove(filepath.Join(runRoot, "fixture-exit")))
				requireNoError(t, syscall.Mkfifo(filepath.Join(runRoot, "fixture-exit"), 0o600))
			case "oversized":
				current, err := json.Marshal(core.CurrentTaskResult{Task: core.Task{ID: "task-A"}, Run: core.Run{ID: "run-A"}})
				requireNoError(t, err)
				requireNoError(t, os.WriteFile(filepath.Join(runRoot, "task-current.json"), append(current, []byte(strings.Repeat(" ", 1<<20))...), 0o600))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRMA02ArtifactReaderRejectsUntrustedFilesWithoutLeaking$")
			command.Env = append(os.Environ(),
				"RMA02_UNTRUSTED_ARTIFACT_CHILD="+mode,
				"RMA02_UNTRUSTED_ARTIFACT_DATA="+dataDir,
				"RMA02_ARTIFACT_SECRET="+canary,
			)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("artifact reader blocked on %s", mode)
			}
			if err == nil {
				t.Fatalf("artifact reader accepted %s input", mode)
			}
			if strings.Contains(string(output), canary) {
				t.Fatalf("artifact reader leaked canary for %s", mode)
			}
		})
	}
}

func TestRMA02ArtifactReaderRejectsSymlinkedDirectoriesAndOversizedArtifacts(t *testing.T) {
	for _, component := range []string{"agent-homes", "agent-A", ".coordplane-rma02", "task-A", "run-A"} {
		t.Run("symlink directory "+component, func(t *testing.T) {
			dataDir := writeRMA02ArtifactFixture(t)
			path := dataDir
			for _, part := range []string{"agent-homes", "agent-A", ".coordplane-rma02", "task-A", "run-A"} {
				path = filepath.Join(path, part)
				if part == component {
					break
				}
			}
			moved := filepath.Join(t.TempDir(), "rma02-artifact-secret-canary")
			requireNoError(t, os.Rename(path, moved))
			requireNoError(t, os.Symlink(moved, path))
			if _, _, _, err := readRMA02Artifacts(dataDir, "agent-A", "task-A"); err == nil || strings.Contains(err.Error(), "rma02-artifact-secret-canary") {
				t.Fatalf("artifact reader accepted or leaked symlinked %s: %v", component, err)
			}
		})
	}

	limits := []struct {
		name    string
		maximum int64
	}{
		{name: "task-current.json", maximum: 64 << 10},
		{name: "fixture", maximum: 4 << 10},
		{name: "fixture-exit", maximum: 64},
	}
	for _, test := range limits {
		t.Run("oversized "+test.name, func(t *testing.T) {
			dataDir := writeRMA02ArtifactFixture(t)
			runRoot := filepath.Join(rma02ArtifactRoot(dataDir, "agent-A"), "task-A", "run-A")
			requireNoError(t, os.WriteFile(filepath.Join(runRoot, test.name), []byte(strings.Repeat("x", int(test.maximum+1))), 0o600))
			directory, err := openRMA02Directory(runRoot)
			requireNoError(t, err)
			defer directory.Close()
			if _, err := readRMA02ArtifactFile(directory, test.name, test.maximum); err == nil {
				t.Fatalf("artifact reader accepted %s above %d bytes", test.name, test.maximum)
			}
		})
	}
}

func writeRMA02ArtifactFixture(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	runRoot := filepath.Join(rma02ArtifactRoot(dataDir, "agent-A"), "task-A", "run-A")
	requireNoError(t, os.MkdirAll(runRoot, 0o700))
	current, err := json.Marshal(core.CurrentTaskResult{Task: core.Task{ID: "task-A"}, Run: core.Run{ID: "run-A"}})
	requireNoError(t, err)
	testsupport.WriteFile(t, filepath.Join(runRoot, "task-current.json"), current, 0o600)
	testsupport.WriteFile(t, filepath.Join(runRoot, "fixture"), []byte("source-A\n"), 0o600)
	testsupport.WriteFile(t, filepath.Join(runRoot, "fixture-exit"), []byte("0\n"), 0o600)
	return dataDir
}

func executeRMA02FixtureWriter(t *testing.T, workspace, home, mode, role, taskID, runID string) string {
	t.Helper()
	root := t.TempDir()
	currentPath := filepath.Join(root, "current.json")
	current, err := json.Marshal(core.CurrentTaskResult{Task: core.Task{ID: taskID}, Run: core.Run{ID: runID}})
	if err != nil {
		t.Fatal(err)
	}
	testsupport.WriteFile(t, currentPath, current, 0o600)
	progressPath := filepath.Join(root, "progress.log")
	stub := testsupport.WriteFile(t, filepath.Join(root, "coordlink"), []byte(`#!/bin/sh
set -eu
case "$1:$2" in
task:current) cat "$RMA02_CURRENT_JSON" ;;
progress:*) printf 'progress %s\n' "$*" >>"$RMA02_PROGRESS_LOG"; printf '{}\n' ;;
*) exit 2 ;;
esac
`), 0o700)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(workspace, "fixture-test.sh"), mode, role, taskID, runID)
	command.Dir = workspace
	command.Env = append(os.Environ(), "HOME="+home, "RMA02_COORDLINK_BIN="+stub, "RMA02_CURRENT_JSON="+currentPath, "RMA02_PROGRESS_LOG="+progressPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("formal %s fixture writer: %v output=%s", mode, err, output)
	}
	raw, err := os.ReadFile(progressPath)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
		{name: "source continue task spec failure", want: "task_spec", facts: rma02FailureFacts{Sources: completeBarrier, Events: append(rma02BarrierEvents(true), rma02ContinueEvents()...)}},
		{name: "one submitted source does not mask unconverged peer", want: "task_spec", facts: rma02FailureFacts{
			Sources: []rma02FailureTaskFact{
				{Role: "A", Task: core.Task{ID: "task-A", Kind: core.TaskWork, Status: core.TaskSubmitted}, Run: core.Run{ID: "run-A", TaskID: "task-A", State: core.RunExited, RequestedOutcome: "submit"}},
				{Role: "B", Task: core.Task{ID: "task-B", Kind: core.TaskWork, Status: core.TaskFailed}, Run: core.Run{ID: "run-B", TaskID: "task-B", State: core.RunExited}},
				completeBarrier[2], completeBarrier[3],
			},
			Events: append(rma02BarrierEvents(true), rma02ContinueEvents()...),
		}},
		{name: "unrelated ack does not complete barrier", want: "task_spec", facts: rma02FailureFacts{Sources: completeBarrier, Events: append(rma02BarrierEvents(false), core.Event{RunID: "run-unrelated", Kind: "message.acknowledged"})}},
		{name: "integration task spec failure", want: "task_spec", facts: rma02FailureFacts{
			Sources: completeBarrier, Events: rma02BarrierEvents(true),
			Integrations: []rma02FailureTaskFact{{Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration}, Run: core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited}}},
		}},
		{name: "latest integration task spec beats prior capture", want: "task_spec", facts: rma02FailureFacts{
			Sources: completeBarrier, Events: rma02BarrierEvents(true),
			Integrations: []rma02FailureTaskFact{
				{Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration, Status: core.TaskCompleted}, Run: core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited, RequestedOutcome: "submit"}},
				{Task: core.Task{ID: "integration-C", Kind: core.TaskIntegration, Status: core.TaskFailed}, Run: core.Run{ID: "run-integration-C", TaskID: "integration-C", State: core.RunExited}},
			},
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

func TestRMA02FailureClassRequiresDurableConvergence(t *testing.T) {
	for _, kind := range []string{"source", "integration"} {
		for _, outcome := range []string{"", "wait", "fail", "submit"} {
			name := outcome
			if name == "" {
				name = "empty"
			}
			t.Run(kind+" "+name, func(t *testing.T) {
				facts := rma02FailureFacts{Sources: rma02FailureSources(), Events: append(rma02BarrierEvents(true), rma02ContinueEvents()...)}
				if kind == "source" {
					for index := range facts.Sources {
						facts.Sources[index].Run.RequestedOutcome = outcome
					}
				} else {
					facts.Integrations = []rma02FailureTaskFact{{
						Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration},
						Run:  core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited, RequestedOutcome: outcome},
					}}
				}
				want := "task_spec"
				if outcome == "submit" {
					want = "product"
				}
				if got := classifyRMA02Failure(facts); got != want {
					t.Fatalf("%s outcome %q classified as %q, want %q", kind, outcome, got, want)
				}
			})
		}
	}

	for _, state := range []struct {
		name          string
		status        core.TaskStatus
		pending, want string
	}{
		{name: "capture", pending: "capture", want: "product"},
		{name: "submitted", status: core.TaskSubmitted, want: "product"},
		{name: "completed", status: core.TaskCompleted, want: "product"},
		{name: "unrelated advance", pending: "advance", want: "task_spec"},
	} {
		for _, kind := range []string{"source", "integration"} {
			t.Run(kind+" "+state.name, func(t *testing.T) {
				facts := rma02FailureFacts{Sources: rma02FailureSources(), Events: append(rma02BarrierEvents(true), rma02ContinueEvents()...)}
				if kind == "source" {
					for index := range facts.Sources {
						facts.Sources[index].Task.Status = state.status
						facts.Sources[index].Task.PendingAction = state.pending
					}
				} else {
					facts.Integrations = []rma02FailureTaskFact{{
						Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration, Status: state.status, PendingAction: state.pending},
						Run:  core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited},
					}}
				}
				if got := classifyRMA02Failure(facts); got != state.want {
					t.Fatalf("%s %s state classified as %q, want %q", kind, state.name, got, state.want)
				}
			})
		}
	}
}

func TestRMA02FailureCollectorPaginatesAndWritesClassification(t *testing.T) {
	root := t.TempDir()
	writeRMA02CollectorFixture(t, root, "events-1", core.EventPage{Items: []core.Event{{RunID: "run-A", Kind: "task.progress"}}, NextCursor: "events-next"})
	writeRMA02CollectorFixture(t, root, "events-2", core.EventPage{Items: []core.Event{{RunID: "run-B-new", Kind: "message.acknowledged"}}})
	writeRMA02CollectorFixture(t, root, "tasks-1", core.TaskPage{Items: []core.TaskSummary{{ID: "task-A", Kind: core.TaskWork}}, NextCursor: "tasks-next"})
	writeRMA02CollectorFixture(t, root, "tasks-2", core.TaskPage{Items: []core.TaskSummary{{ID: "integration-B", Kind: core.TaskIntegration}}})

	sources := make([]rma02Source, 4)
	for index, role := range []string{"A", "B", "C", "D"} {
		taskID, runID := "task-"+role, "run-"+role
		sources[index] = rma02Source{role: role, task: core.Task{ID: taskID}}
		detail := core.TaskDetail{Task: core.Task{ID: taskID, Kind: core.TaskWork}}
		if role != "B" {
			detail.CurrentRun = &core.Run{ID: runID, TaskID: taskID, State: core.RunActive}
		}
		writeRMA02CollectorFixture(t, root, "task-"+taskID, detail)
	}
	writeRMA02CollectorFixture(t, root, "runs-task-B-1", core.RunPage{Items: []core.RunSummary{{ID: "run-B-old", TaskID: "task-B", State: core.RunExited}}, NextCursor: "runs-next"})
	writeRMA02CollectorFixture(t, root, "runs-task-B-2", core.RunPage{Items: []core.RunSummary{{ID: "run-B-new", TaskID: "task-B", State: core.RunExited}}})
	writeRMA02CollectorFixture(t, root, "run-run-B-old", core.Run{ID: "run-B-old", TaskID: "task-B", State: core.RunExited})
	writeRMA02CollectorFixture(t, root, "run-run-B-new", core.Run{ID: "run-B-new", TaskID: "task-B", State: core.RunExited})
	writeRMA02CollectorFixture(t, root, "task-integration-B", core.TaskDetail{Task: core.Task{ID: "integration-B", Kind: core.TaskIntegration, HeadRunID: "run-integration-B"}})
	writeRMA02CollectorFixture(t, root, "run-run-integration-B", core.Run{ID: "run-integration-B", TaskID: "integration-B", State: core.RunExited})

	stub := testsupport.WriteFile(t, filepath.Join(root, "coordplane"), []byte(`#!/bin/sh
set -eu
cursor=
task=
previous=
for argument in "$@"; do
  [ "$previous" != --cursor ] || cursor=$argument
  [ "$previous" != --task ] || task=$argument
  previous=$argument
done
case "$1:$2" in
events:tail) [ "$cursor" = events-next ] && page=events-2 || page=events-1 ;;
task:list) [ "$cursor" = tasks-next ] && page=tasks-2 || page=tasks-1 ;;
task:show) page=task-$3 ;;
run:list) [ "$cursor" = runs-next ] && page=runs-$task-2 || page=runs-$task-1 ;;
run:show) page=run-$3 ;;
*) exit 2 ;;
esac
cat "$RMA02_COLLECTOR_ROOT/$page.json"
`), 0o700)
	t.Setenv("RMA02_COLLECTOR_ROOT", root)
	facts, err := collectRMA02FailureFacts(context.Background(), stub, "/stub/socket", "project-rma02", sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Events) != 2 {
		t.Errorf("collector Event count = %d, want paginated 2", len(facts.Events))
	}
	if facts.Sources[1].Run.ID != "run-B-new" {
		t.Errorf("collector selected Run %q, want latest terminal run-B-new", facts.Sources[1].Run.ID)
	}
	if len(facts.Integrations) != 1 || facts.Integrations[0].Task.ID != "integration-B" {
		t.Errorf("collector integrations = %#v, want second-page integration-B", facts.Integrations)
	}
	path := filepath.Join(root, "failure-class")
	t.Setenv("E2E_RMA02_FAILURE_CLASS_FILE", path)
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	setRMA02FailureClass(t, string(raw))
	if class, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(class)) != "task_spec" {
		t.Fatalf("collector classification file = %q err=%v, want task_spec", class, err)
	}
}

func TestRMA02FailureCollectorAttributesMixedSourcesToUnconvergedTask(t *testing.T) {
	root := t.TempDir()
	writeRMA02CollectorFixture(t, root, "events", core.EventPage{Items: append(rma02BarrierEvents(true), rma02ContinueEvents()...)})
	writeRMA02CollectorFixture(t, root, "tasks", core.TaskPage{})
	sources := make([]rma02Source, 4)
	for index, role := range []string{"A", "B", "C", "D"} {
		taskID, runID := "task-"+role, "run-"+role
		sources[index] = rma02Source{role: role, task: core.Task{ID: taskID}}
		task := core.Task{ID: taskID, Kind: core.TaskWork, Status: core.TaskSubmitted}
		run := core.Run{ID: runID, TaskID: taskID, State: core.RunExited, RequestedOutcome: "submit"}
		if role == "B" {
			task.Status = core.TaskFailed
			run.RequestedOutcome = ""
		}
		writeRMA02CollectorFixture(t, root, "task-"+taskID, core.TaskDetail{Task: task, CurrentRun: &run})
	}
	stub := testsupport.WriteFile(t, filepath.Join(root, "coordplane"), []byte(`#!/bin/sh
set -eu
case "$1:$2" in
events:tail) page=events ;;
task:list) page=tasks ;;
task:show) page=task-$3 ;;
*) exit 2 ;;
esac
cat "$RMA02_COLLECTOR_ROOT/$page.json"
`), 0o700)
	t.Setenv("RMA02_COLLECTOR_ROOT", root)
	facts, err := collectRMA02FailureFacts(context.Background(), stub, "/stub/socket", "project-rma02", sources)
	requireNoError(t, err)
	raw, err := json.Marshal(facts)
	requireNoError(t, err)
	classPath := filepath.Join(root, "failure-class")
	t.Setenv("E2E_RMA02_FAILURE_CLASS_FILE", classPath)
	setRMA02FailureClass(t, string(raw))
	if class, readErr := os.ReadFile(classPath); readErr != nil || strings.TrimSpace(string(class)) != "task_spec" {
		t.Fatalf("mixed source classification = %q err=%v, want task_spec", class, readErr)
	}
}

func TestRMA02FailureCollectorFallsBackToProduct(t *testing.T) {
	if os.Getenv("RMA02_FAILURE_FALLBACK_CHILD") == "1" {
		registerRMA02FailureClassification(t, filepath.Join(t.TempDir(), "missing-coordplane"), "/missing/socket", "project-rma02", []rma02Source{{role: "A", task: core.Task{ID: "task-A"}}})
		t.Fatal("force RMA-02 failure cleanup")
	}
	path := filepath.Join(t.TempDir(), "failure-class")
	command := exec.Command(os.Args[0], "-test.run=^TestRMA02FailureCollectorFallsBackToProduct$")
	command.Env = append(os.Environ(), "RMA02_FAILURE_FALLBACK_CHILD=1", "E2E_RMA02_FAILURE_CLASS_FILE="+path)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failure fallback child unexpectedly passed: %s", output)
	}
	if class, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(class)) != "product" {
		t.Fatalf("collector failure class = %q err=%v, want product", class, err)
	}
}

func TestRMA02FailureOutputRedactsCollectorCLIAndProviderErrors(t *testing.T) {
	if mode := os.Getenv("RMA02_FAILURE_OUTPUT_CHILD"); mode != "" {
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "rma02-failure-secret-canary")
		switch mode {
		case "collector-unreachable":
			registerRMA02FailureClassification(t, "/rma02-host-path-canary/missing-coordplane", "/rma02-host-path-canary/operator.sock", "project-rma02", []rma02Source{{role: "A", task: core.Task{ID: "task-A"}}})
			t.Fatal("force collector cleanup")
		case "cli-non-json":
			root := t.TempDir()
			stub := testsupport.WriteFile(t, filepath.Join(root, "coordplane"), []byte("#!/bin/sh\nprintf '%s\\n' 'rma02-failure-secret-canary /rma02-host-path-canary'\n"), 0o700)
			registerRMA02FailureClassification(t, stub, "/rma02-host-path-canary/operator.sock", "project-rma02", []rma02Source{{role: "A", task: core.Task{ID: "task-A"}}})
			t.Fatal("force invalid JSON cleanup")
		case "provider-error":
			stub := testsupport.WriteFile(t, filepath.Join(t.TempDir(), "coordplane-rma02-host-path-canary"), []byte("#!/bin/sh\nprintf '%s\\n' 'rma02-failure-secret-canary /rma02-host-path-canary'\nexit 9\n"), 0o700)
			runJSON[core.Task](t, context.Background(), stub, "task", "show", "task-A", "--socket", "/rma02-host-path-canary/operator.sock", "--output", "json")
		}
		return
	}

	for _, mode := range []string{"collector-unreachable", "cli-non-json", "provider-error"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestRMA02FailureOutputRedactsCollectorCLIAndProviderErrors$")
			command.Env = append(os.Environ(), "RMA02_FAILURE_OUTPUT_CHILD="+mode)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("failure-output child %s unexpectedly passed", mode)
			}
			text := string(output)
			for _, forbidden := range []string{"rma02-failure-secret-canary", "/rma02-host-path-canary"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("failure-output child %s leaked %q", mode, forbidden)
				}
			}
		})
	}
}

func writeRMA02CollectorFixture(t *testing.T, root, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.WriteFile(t, filepath.Join(root, name+".json"), raw, 0o600)
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

func rma02ContinueEvents() []core.Event {
	events := make([]core.Event, 0, 4)
	for _, role := range []string{"A", "B", "C", "D"} {
		events = append(events, core.Event{Kind: "message.created", ActorKind: "boss", RequestID: "rma02-continue-" + role})
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
		{name: "build failure output is redacted", image: rma02StubImage(), makeMode: "leak-fail", wantClass: "product"},
		{name: "test failure without durable class", image: rma02StubImage(), testMode: "fail", wantClass: "product", wantCount: 1},
		{name: "checker failure", image: rma02StubImage(), checkerMode: "fail", wantClass: "product", wantCount: 1},
		{name: "missing fresh report", image: rma02StubImage(), testMode: "no-report", wantClass: "product", wantCount: 1},
		{name: "existing report is invalid admission", image: rma02StubImage(), staleReport: true, invalid: true},
		{name: "test failure output is redacted", image: rma02StubImage(), testMode: "leak-fail", wantClass: "product", wantCount: 1},
		{name: "success", image: rma02StubImage(), pass: true, wantCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, countPath, report := rma02StubbedShell(t, test.image, test.makeMode, test.testMode, test.checkerMode, test.staleReport)
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
			if strings.Contains(text, "rma02-host-path-canary") {
				t.Fatal("RMA-02 shell leaked host path canary")
			}
			if test.invalid {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 || count != 0 || !strings.Contains(text, "INVALID_ENVIRONMENT(") || strings.Contains(text, "FAIL_REAL_MULTI_AGENT") || strings.Contains(text, "PASS_REAL_MULTI_AGENT_LOCAL") {
					t.Fatalf("invalid admission err=%v count=%d output=%s", err, count, text)
				}
				if test.staleReport {
					raw, readErr := os.ReadFile(report)
					if readErr != nil || string(raw) != "stale-report\n" {
						t.Fatalf("existing report changed: raw=%q err=%v", raw, readErr)
					}
				}
				return
			}
			if test.pass {
				if err != nil || count != test.wantCount || !strings.Contains(text, "PASS_REAL_MULTI_AGENT_LOCAL") || strings.Contains(text, "FAIL_REAL_MULTI_AGENT") || strings.Contains(text, "INVALID_ENVIRONMENT(") {
					t.Fatalf("success err=%v count=%d output=%s", err, count, text)
				}
				raw, readErr := os.ReadFile(report)
				if readErr != nil || string(raw) != rma02StubReport {
					t.Fatalf("published report = %q err=%v, want exact staged evidence", raw, readErr)
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

const rma02StubReport = "{\"result\":\"PASS_REAL_MULTI_AGENT_LOCAL\"}\n"

func rma02StubbedShell(t *testing.T, image, makeMode, testMode, checkerMode string, staleReport bool) (*exec.Cmd, string, string) {
	t.Helper()
	root, stubs, temporaryRoot := t.TempDir(), t.TempDir(), t.TempDir()
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
	testsupport.WriteFile(t, filepath.Join(stubs, "make"), []byte(`#!/bin/sh
case "${STUB_MAKE_MODE:-}" in
fail) exit 1 ;;
leak-fail) printf '%s\n' 'real-gate-auth-token-canary'; printf '%s\n' 'real-gate-auth-token-canary' >&2; exit 1 ;;
esac
`), 0o700)
	goStub := `#!/bin/sh
case "$1" in
test)
  printf 'test\n' >>"$STUB_TEST_COUNT"
	case "${STUB_TEST_MODE:-success}" in
			fail) exit 4;;
			leak-fail) printf '%s\n' 'rma02-host-path-canary real-gate-auth-token-canary'; exit 4;;
			concurrent) printf '%s\n' 'attacker-owned' >"$RMA02_REQUESTED_OUTPUT"; printf '%s' '` + rma02StubReport + `' >"$E2E_RMA02_REPORT";;
		    no-report) ;;
	    *) printf '%s' '` + rma02StubReport + `' >"$E2E_RMA02_REPORT";;
  esac
  printf '{"Action":"pass","Test":"TestRealMultiAgentScenarios"}\n'
  ;;
run) [ "${STUB_CHECKER_MODE:-}" != fail ];;
*) exit 2;;
esac
`
	testsupport.WriteFile(t, filepath.Join(stubs, "go"), []byte(goStub), 0o700)
	report, countPath := filepath.Join(temporaryRoot, "report.json"), filepath.Join(temporaryRoot, "test-count")
	if staleReport {
		testsupport.WriteFile(t, report, []byte("stale-report\n"), 0o600)
	}
	command := exec.Command(scriptPath)
	command.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_RUNTIME_IMAGE="+image,
		"RMA02_OUTPUT="+report, "ANTHROPIC_AUTH_TOKEN="+realTokenCanary, "STUB_MAKE_MODE="+makeMode,
		"STUB_TEST_MODE="+testMode, "STUB_CHECKER_MODE="+checkerMode, "STUB_TEST_COUNT="+countPath,
		"RMA02_REQUESTED_OUTPUT="+report, "TMPDIR="+temporaryRoot,
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
	)
	return command, countPath, report
}

func TestRealMultiAgentShellPublishesReportWithoutClobbering(t *testing.T) {
	t.Run("default output publishes exact report", func(t *testing.T) {
		command, _, report := rma02StubbedShell(t, rma02StubImage(), "", "", "", false)
		command.Env = replaceRMA02CommandEnv(command.Env, "RMA02_OUTPUT", "")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("default report success: %v output=%s", err, output)
		}
		var published string
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "RMA02_EVIDENCE=") {
				published = strings.TrimPrefix(line, "RMA02_EVIDENCE=")
			}
		}
		if published == "" || filepath.Dir(filepath.Dir(published)) != filepath.Dir(report) {
			t.Fatalf("default evidence path = %q, want controlled TMPDIR", published)
		}
		raw, readErr := os.ReadFile(published)
		if readErr != nil || string(raw) != rma02StubReport {
			t.Fatalf("default published report = %q err=%v, want exact staged evidence", raw, readErr)
		}
	})

	t.Run("symlink target", func(t *testing.T) {
		command, countPath, report := rma02StubbedShell(t, rma02StubImage(), "", "", "", false)
		target := filepath.Join(t.TempDir(), "target")
		testsupport.WriteFile(t, target, []byte("target-owned\n"), 0o600)
		requireNoError(t, os.Symlink(target, report))
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 || strings.Contains(string(output), "PASS_REAL_MULTI_AGENT_LOCAL") {
			t.Fatalf("symlink output admission err=%v output=%s", err, output)
		}
		if raw, readErr := os.ReadFile(target); readErr != nil || string(raw) != "target-owned\n" {
			t.Fatalf("symlink target changed: raw=%q err=%v", raw, readErr)
		}
		if _, readErr := os.Stat(countPath); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("symlink output reached test boundary: %v", readErr)
		}
	})

	t.Run("concurrent target replacement", func(t *testing.T) {
		command, _, report := rma02StubbedShell(t, rma02StubImage(), "", "concurrent", "", false)
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || strings.Contains(string(output), "PASS_REAL_MULTI_AGENT_LOCAL") {
			t.Fatalf("concurrent publication err=%v output=%s", err, output)
		}
		if raw, readErr := os.ReadFile(report); readErr != nil || string(raw) != "attacker-owned\n" {
			t.Fatalf("concurrent target was overwritten: raw=%q err=%v", raw, readErr)
		}
	})

	t.Run("invalid output leaves no temporary resources", func(t *testing.T) {
		command, countPath, report := rma02StubbedShell(t, rma02StubImage(), "", "", "", false)
		temporaryRoot := filepath.Dir(report)
		command.Env = replaceRMA02CommandEnv(command.Env, "RMA02_OUTPUT", "relative-report.json")
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 || strings.Contains(string(output), "PASS_REAL_MULTI_AGENT_LOCAL") {
			t.Fatalf("relative output admission err=%v output=%s", err, output)
		}
		entries, readErr := os.ReadDir(temporaryRoot)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("invalid output left temporary resources: %v", entries)
		}
		if _, readErr := os.Stat(countPath); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("invalid output reached test boundary: %v", readErr)
		}
	})

	t.Run("output outside controlled directory", func(t *testing.T) {
		command, _, report := rma02StubbedShell(t, rma02StubImage(), "", "", "", false)
		outside := filepath.Join(t.TempDir(), "report.json")
		command.Env = replaceRMA02CommandEnv(command.Env, "RMA02_OUTPUT", outside)
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 || strings.Contains(string(output), "PASS_REAL_MULTI_AGENT_LOCAL") {
			t.Fatalf("outside output admission err=%v output=%s", err, output)
		}
		if _, readErr := os.Stat(outside); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("outside output was created: %v", readErr)
		}
		_ = report
	})
}

func replaceRMA02CommandEnv(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
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
			TaskID: "integration-" + source.Role, RunID: "run-integration-" + source.Role, SourceTaskID: source.TaskID, SourceRunID: source.RunID,
			SourceTaskRef: source.TaskRef, SourceHeadSHA: source.HeadSHA, ObservedCanonical: observedCanonical,
			HeadSHA: strings.Repeat(string(rune('1'+index)), 40), SourceAncestor: true, CanonicalAncestor: true, SubmitEventCount: 1,
		})
		integrations[len(integrations)-1].FixtureTaskID = integrations[len(integrations)-1].TaskID
		integrations[len(integrations)-1].FixtureRunID = integrations[len(integrations)-1].RunID
		integrations[len(integrations)-1].FixtureMarker = "integration"
		integrations[len(integrations)-1].FixtureEventCount = 1
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
