//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

// RMA-03 real task: wordfreq, a real Go word-frequency program, developed
// concurrently by two real Claude agents from the same C0 skeleton
// (tokenize.go in P1, count.go in P2). One human participant is owner in P1
// and developer in P2: agentA dispatches a review child task to the human,
// the human converges it with task complete (human_confirm), repair is denied
// in P2 and allowed in P1, a daemon restart preserves everything, and the two
// independently developed modules compose into a working program.
func TestRealClaudeUnifiedParticipantTwoProjectConvergence(t *testing.T) {
	requireRealCLI(t)
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	coordlink := requireExecutable(t, "E2E_COORDLINK_BIN")
	image, network, providerEnv := liveRuntimeConfig(t)
	releaseLiveDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveE2ETimeout)
	defer cancel()
	root := t.TempDir()
	source, initialSHA := createWordfreqSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	registerLiveHomeCleanup(t, image, dataDir)
	instructions := filepath.Join(root, "rma03-instructions.md")
	testsupport.WriteFile(t, instructions, []byte(rma03LiveInstructions), 0o600)
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane-rma03.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "24h", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: network, DefaultImage: image, ProviderEnv: providerEnv, Tail: "  run_timeout: 12m\n  shutdown_grace: 5s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	trackFailure := registerLiveFailureDiagnostics(t, coordplane, socket, dataDir, providerEnv, func() error { return daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "rma03 daemon startup")
	agentA := runJSON[core.Agent](t, ctx, coordplane, "agent", "add", "--socket", socket, "--id", "agt_rma03_a", "--display-name", "RMA03 Agent A", "--adapter", "claude", "--image", image, "--instructions-file", instructions, "--request-id", "rma03-agent-a", "--output", "json")
	agentB := runJSON[core.Agent](t, ctx, coordplane, "agent", "add", "--socket", socket, "--id", "agt_rma03_b", "--display-name", "RMA03 Agent B", "--adapter", "claude", "--image", image, "--instructions-file", instructions, "--request-id", "rma03-agent-b", "--output", "json")

	// Projects first: AddProject mirrors the human's global owner role into
	// the project scopes, granting project.repair in both.
	projectA := runJSON[core.Project](t, ctx, coordplane, "project", "add", "--socket", socket, "--name", "RMA03 P1 wordfreq tokenize", "--repo", source, "--ref", "refs/heads/main", "--integration-agent", agentA.ID, "--request-id", "rma03-project-a", "--output", "json")
	projectB := runJSON[core.Project](t, ctx, coordplane, "project", "add", "--socket", socket, "--name", "RMA03 P2 wordfreq count", "--repo", source, "--ref", "refs/heads/main", "--integration-agent", agentB.ID, "--request-id", "rma03-project-b", "--output", "json")

	setupRoleLayer(t, ctx, coordplane, socket, agentA, projectB, "rma03")

	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane, "agent", "pause", agent.ID, "--socket", socket, "--request-id", "rma03-pause-"+agent.ID, "--output", "json")
	}
	taskA := runJSON[core.Task](t, ctx, coordplane, "task", "create", "--socket", socket, "--project", projectA.ID, "--agent", agentA.ID, "--title", "RMA03 implement tokenize", "--description", "live_module=tokenize;human_review=1", "--request-id", "rma03-task-a", "--output", "json")
	taskB := runJSON[core.Task](t, ctx, coordplane, "task", "create", "--socket", socket, "--project", projectB.ID, "--agent", agentB.ID, "--title", "RMA03 implement count", "--description", "live_module=count", "--request-id", "rma03-task-b", "--output", "json")
	trackFailure(taskA.ID, taskB.ID)
	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane, "agent", "resume", agent.ID, "--socket", socket, "--request-id", "rma03-resume-"+agent.ID, "--output", "json")
	}
	runA, runB := waitForConcurrentRunsWithProgress(t, ctx, coordplane, socket, taskA.ID, taskB.ID, "LIVE-READY", 9*time.Minute)
	assertIsolatedRuns(t, dataDir, runA, runB, inspectContainer(t, ctx, runA.ContainerID), inspectContainer(t, ctx, runB.ContainerID))
	sendBossMessage(t, ctx, coordplane, socket, projectA.ID, agentA.ID, taskA.ID, "LIVE-GO A", "rma03-go-a")
	sendBossMessage(t, ctx, coordplane, socket, projectB.ID, agentB.ID, taskB.ID, "LIVE-GO B", "rma03-go-b")

	taskA = waitForTaskWithin(t, ctx, coordplane, socket, taskA.ID, "rma03 A submission", 11*time.Minute, capturedSubmission)
	taskB = waitForTaskWithin(t, ctx, coordplane, socket, taskB.ID, "rma03 B submission", 11*time.Minute, capturedSubmission)

	// agentA dispatched a review child task to the human participant; the
	// human converges it with task complete (human_confirm evidence).
	var review core.Task
	for _, task := range runJSON[core.TaskPage](t, ctx, coordplane, "task", "list", "--socket", socket, "--project", projectA.ID, "--limit", "500", "--output", "json").Items {
		if task.Title == "RMA03 human review" {
			review = taskDetail(t, ctx, coordplane, socket, task.ID).Task
		}
	}
	if review.ID == "" {
		t.Fatal("RMA03 human review child task not found")
	}
	completed := runJSON[core.Task](t, ctx, coordplane, "task", "complete", review.ID, "--socket", socket, "--summary", "RMA03 human review ok", "--request-id", "rma03-complete", "--output", "json")
	if completed.EvidenceType != string(core.EvidenceHumanConfirm) || completed.HeadSHA != "" || completed.Status != core.TaskCompleted {
		t.Fatalf("rma03 human review complete = %#v", completed)
	}

	// The human accepts both captured results; each canonical converges by
	// direct CAS from the same C0.
	runJSON[core.Task](t, ctx, coordplane, "task", "accept", taskA.ID, "--socket", socket, "--integration-agent", agentA.ID, "--request-id", "rma03-accept-a", "--output", "json")
	taskA = waitForTaskWithin(t, ctx, coordplane, socket, taskA.ID, "rma03 A direct CAS", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	runJSON[core.Task](t, ctx, coordplane, "task", "accept", taskB.ID, "--socket", socket, "--integration-agent", agentB.ID, "--request-id", "rma03-accept-b", "--output", "json")
	taskB = waitForTaskWithin(t, ctx, coordplane, socket, taskB.ID, "rma03 B direct CAS", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	for repo, head := range map[string]string{filepath.Join(dataDir, "repos", projectA.ID+".git"): taskA.HeadSHA, filepath.Join(dataDir, "repos", projectB.ID+".git"): taskB.HeadSHA} {
		if got := gitDir(t, ctx, repo, "rev-parse", "refs/heads/main"); got != head {
			t.Fatalf("rma03 canonical %s = %s, want %s", repo, got, head)
		}
		gitDirSucceeds(t, ctx, repo, "merge-base", "--is-ancestor", initialSHA, head)
		gitDirSucceeds(t, ctx, repo, "fsck", "--full", "--strict")
	}

	// The independently developed modules compose into one working program:
	// P1's canonical carries the real tokenizer, P2's the real counter.
	composeA, composeB := filepath.Join(root, "compose-a"), filepath.Join(root, "compose-b")
	runJSON[core.GitCheckoutFact](t, ctx, coordplane, "task", "checkout", taskA.ID, "--socket", socket, "--dest", composeA, "--output", "json")
	runJSON[core.GitCheckoutFact](t, ctx, coordplane, "task", "checkout", taskB.ID, "--socket", socket, "--dest", composeB, "--output", "json")
	raw, err := os.ReadFile(filepath.Join(composeB, "count.go"))
	requireNoError(t, err)
	testsupport.WriteFile(t, filepath.Join(composeA, "count.go"), raw, 0o644)
	runIn(t, ctx, composeA, "./fixture-test.sh", "full")

	// Permission difference: repair is denied in P2 (developer) and allowed
	// in P1 (owner mirror); the error state is injected through the real
	// SQLite store while the capability gate runs in the daemon.
	dbPath := filepath.Join(dataDir, "coordplane.db")
	requireRepair(t, ctx, coordplane, socket, projectB.ID, false)
	t.Logf("evidence: permission denial project=%s capability=project.repair scope=project outcome=SCOPE_DENIED", projectB.ID)
	flipProjectError(t, ctx, dbPath, projectA.ID)
	requireRepair(t, ctx, coordplane, socket, projectA.ID, true)
	t.Logf("evidence: evidence_type task=%s kind=human_confirm head_sha=empty status=%s", review.ID, completed.Status)
	t.Logf("evidence: evidence_type task=%s kind=captured head_sha=%s", taskA.ID, taskA.HeadSHA)

	// Daemon restart: state, evidence and permission differences persist.
	_ = daemon.Stop()
	daemon = startDaemon(t, coordplane, configPath, socket)
	waitForReady(t, ctx, coordplane, socket, "rma03 daemon restart")
	for project, head := range map[string]string{projectA.ID: taskA.HeadSHA, projectB.ID: taskB.HeadSHA} {
		if after := projectDetail(t, ctx, coordplane, socket, project); after.ActualCanonicalSHA != head || after.Status != core.ProjectActive {
			t.Fatalf("rma03 restarted Project %s = %#v, want active at %s", project, after, head)
		}
	}
	for _, id := range []string{taskA.ID, taskB.ID, review.ID} {
		if task := taskDetail(t, ctx, coordplane, socket, id).Task; task.Status != core.TaskCompleted {
			t.Fatalf("rma03 restarted Task %s status = %s", id, task.Status)
		}
	}
	if task := taskDetail(t, ctx, coordplane, socket, review.ID).Task; task.EvidenceType != string(core.EvidenceHumanConfirm) {
		t.Fatalf("rma03 restarted review evidence = %q", task.EvidenceType)
	}
	requireRepair(t, ctx, coordplane, socket, projectB.ID, false)
	_ = daemon.Stop()
	t.Logf("PASS rma03 projects A=%s B=%s C0=%s A=%s B=%s review=%s binaries=%s/%s", projectA.ID, projectB.ID, initialSHA, taskA.HeadSHA, taskB.HeadSHA, review.ID, coordplane, coordlink)
}

const rma03LiveInstructions = `You are a CoordPlane acceptance-gate agent. Execute the numbered steps below IN ORDER. Do not explore. Do not read documentation. Do not inspect the coordlink binary or its help. Deviating from these steps fails the gate.

1. Run exactly: coordlink progress --summary LIVE-READY
2. Poll your inbox until a message body contains LIVE-GO (run: coordlink inbox list; if not present, wait 2 seconds and repeat).
3. Acknowledge all pending inbox messages: coordlink inbox ack <each message id>
4. cd /workspace/project
5. Read SPEC.md and the task description. Implement ONLY your module file: live_module=tokenize -> tokenize.go; live_module=count -> count.go. Do not modify main.go, SPEC.md or fixture-test.sh.
6. If the task description contains human_review=1, run exactly: coordlink task create --participant participant-owner --title "RMA03 human review" --description "human review of the tokenize module"
7. Run: ./fixture-test.sh <live_module> (the value of live_module from the description). Retry until it passes.
8. Run: git config user.name "RMA03 Agent"; git config user.email rma03@coordplane.local
9. Run: git add <your module file>; git commit -m "rma03 <live_module> implementation"
10. Run: git status --porcelain  (it must print nothing. If it prints files, delete every untracked file with rm -f <file> and repeat until it prints nothing. Never commit untracked artifacts.)
11. Run: head=$(git rev-parse HEAD)  (its output is exactly 40 hex characters)
12. Run exactly: coordlink task submit --summary "rma03 <live_module> result" --expected-head "$head"
13. Wait 5 seconds, then exit.

If a step errors, retry the same step up to 3 times, then continue to the next step. The only files you may write are your module file and git objects.
`

func createWordfreqSourceRepository(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	source := filepath.Join(root, "source")
	requireNoError(t, os.MkdirAll(source, 0o755))
	run(t, ctx, "git", "init", "--quiet", "--initial-branch", "main", source)
	git(t, ctx, source, "config", "user.name", "CoordPlane RMA03 Fixture")
	git(t, ctx, source, "config", "user.email", "rma03-fixture@coordplane.local")
	fixtureRoot := filepath.Join("..", "wordfreq")
	entries, err := os.ReadDir(fixtureRoot)
	requireNoError(t, err)
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(fixtureRoot, entry.Name()))
		requireNoError(t, err)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(entry.Name(), ".sh") {
			mode = 0o755
		}
		testsupport.WriteFile(t, filepath.Join(source, entry.Name()), raw, mode)
	}
	git(t, ctx, source, "add", ".")
	git(t, ctx, source, "commit", "--quiet", "-m", "wordfreq C0 skeleton")
	return source, git(t, ctx, source, "rev-parse", "HEAD")
}
