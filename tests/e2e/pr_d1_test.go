//go:build e2e

package e2e_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
	_ "modernc.org/sqlite"
)

func TestPRD1HumanCollaboratesAcrossTwoProjectsWithHotRoleConfig(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	coordlink := requireExecutable(t, "E2E_COORDLINK_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	if image == "" {
		t.Fatal("E2E_RUNTIME_IMAGE is required")
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e", e2eTimeout)
	requireNoError(t, err)
	defer func() { requireNoError(t, release()) }()
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	root := t.TempDir()
	source, initialSHA := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	testsupport.WriteFile(t, instructions, []byte("Execute only the deterministic P5 bootstrap contract.\n"), 0o600)
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "0", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image, Tail: "  run_timeout: 2m\n  shutdown_grace: 3s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "initial daemon startup")

	coowner := runJSON[core.Agent](t, ctx, coordplane, "agent", "add", "--socket", socket, "--display-name", "PRD1 coowner", "--adapter", "claude", "--image", image, "--instructions-file", instructions, "--request-id", "prd1-coowner", "--output", "json")
	agentA := runJSON[core.Agent](t, ctx, coordplane, "agent", "add", "--socket", socket, "--display-name", "PRD1 Agent A", "--adapter", "claude", "--image", image, "--instructions-file", instructions, "--request-id", "prd1-agent-a", "--output", "json")
	agentB := runJSON[core.Agent](t, ctx, coordplane, "agent", "add", "--socket", socket, "--display-name", "PRD1 Agent B", "--adapter", "claude", "--image", image, "--instructions-file", instructions, "--request-id", "prd1-agent-b", "--output", "json")

	projectA := runJSON[core.Project](t, ctx, coordplane, "project", "add", "--socket", socket, "--name", "PRD1 P1", "--repo", source, "--ref", "refs/heads/main", "--integration-agent", agentA.ID, "--request-id", "prd1-project-a", "--output", "json")
	projectB := runJSON[core.Project](t, ctx, coordplane, "project", "add", "--socket", socket, "--name", "PRD1 P2", "--repo", source, "--ref", "refs/heads/main", "--integration-agent", agentB.ID, "--request-id", "prd1-project-b", "--output", "json")

	coownerRole, _, developer := setupRoleLayer(t, ctx, coordplane, socket, coowner, projectB, "prd1")

	taskA := runJSON[core.Task](t, ctx, coordplane, "task", "create", "--socket", socket, "--project", projectA.ID, "--agent", agentA.ID, "--title", "PRD1 work A", "--description", "p5_role=A;human_review=1;no_direct=1", "--request-id", "prd1-task-a", "--output", "json")
	taskB := runJSON[core.Task](t, ctx, coordplane, "task", "create", "--socket", socket, "--project", projectB.ID, "--agent", agentB.ID, "--title", "PRD1 work B", "--description", "p5_role=B;no_direct=1", "--request-id", "prd1-task-b", "--output", "json")
	sendBossMessage(t, ctx, coordplane, socket, projectA.ID, agentA.ID, taskA.ID, "P5-GO A", "prd1-go-a")
	sendBossMessage(t, ctx, coordplane, socket, projectB.ID, agentB.ID, taskB.ID, "P5-GO B", "prd1-go-b")

	taskA = waitForTask(t, ctx, coordplane, socket, taskA.ID, "A captured submission", func(task core.Task) bool {
		return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.TaskRef != ""
	})
	taskB = waitForTask(t, ctx, coordplane, socket, taskB.ID, "B captured submission", func(task core.Task) bool {
		return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.TaskRef != ""
	})

	var review core.Task
	for _, task := range runJSON[core.TaskPage](t, ctx, coordplane, "task", "list", "--socket", socket, "--project", projectA.ID, "--limit", "500", "--output", "json").Items {
		if task.Title == "P5 human review" {
			review = taskDetail(t, ctx, coordplane, socket, task.ID).Task
		}
	}
	if review.ID == "" {
		t.Fatal("review child task not found")
	}
	completed := runJSON[core.Task](t, ctx, coordplane, "task", "complete", review.ID, "--socket", socket, "--summary", "PRD1 human review ok", "--request-id", "prd1-complete", "--output", "json")
	if completed.EvidenceType != string(core.EvidenceHumanConfirm) || completed.HeadSHA != "" || completed.Status != core.TaskCompleted {
		t.Fatalf("human review complete = %#v", completed)
	}
	runJSON[core.Task](t, ctx, coordplane, "task", "accept", taskA.ID, "--socket", socket, "--integration-agent", agentA.ID, "--request-id", "prd1-accept-a", "--output", "json")
	taskA = waitForTask(t, ctx, coordplane, socket, taskA.ID, "A direct CAS", func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	runJSON[core.Task](t, ctx, coordplane, "task", "accept", taskB.ID, "--socket", socket, "--integration-agent", agentB.ID, "--request-id", "prd1-accept-b", "--output", "json")
	taskB = waitForTask(t, ctx, coordplane, socket, taskB.ID, "B direct CAS", func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	for repo, head := range map[string]string{filepath.Join(dataDir, "repos", projectA.ID+".git"): taskA.HeadSHA, filepath.Join(dataDir, "repos", projectB.ID+".git"): taskB.HeadSHA} {
		if got := gitDir(t, ctx, repo, "rev-parse", "refs/heads/main"); got != head {
			t.Fatalf("canonical %s = %s, want %s", repo, got, head)
		}
		gitDirSucceeds(t, ctx, repo, "merge-base", "--is-ancestor", initialSHA, head)
		gitDirSucceeds(t, ctx, repo, "fsck", "--full", "--strict")
	}

	// Permission difference (PR-D1): repair is denied in P2 (developer) and
	// allowed in P1 (owner mirror). The error state is injected through the
	// real SQLite store; the capability gate itself runs in the daemon.
	dbPath := filepath.Join(dataDir, "coordplane.db")
	requireRepair(t, ctx, coordplane, socket, projectB.ID, false)
	flipProjectError(t, ctx, dbPath, projectA.ID)
	requireRepair(t, ctx, coordplane, socket, projectA.ID, true)
	runJSON[core.Role](t, ctx, coordplane, append([]string{"role", "update", developer.ID, "--socket", socket, "--name", "prd1-developer", "--capability", string(core.CapabilityProjectRepair), "--request-id", "prd1-role-grant-repair", "--output", "json"}, capabilityArgs(core.AllCapabilities())...)...)
	flipProjectError(t, ctx, dbPath, projectB.ID)
	requireRepair(t, ctx, coordplane, socket, projectB.ID, true)
	runJSON[core.Role](t, ctx, coordplane, append([]string{"role", "update", developer.ID, "--socket", socket, "--name", "prd1-developer", "--request-id", "prd1-role-revoke-repair", "--output", "json"}, capabilityArgs(core.AllCapabilities(), core.CapabilityProjectRepair)...)...)
	requireRepair(t, ctx, coordplane, socket, projectB.ID, false)
	runJSON[struct{ Unbound string }](t, ctx, coordplane, "participant", "unbind", core.DefaultHumanParticipantID, "--socket", socket, "--project", projectA.ID, "--role", core.DefaultOwnerRoleID, "--request-id", "prd1-unbind-a", "--output", "json")
	requireRepair(t, ctx, coordplane, socket, projectA.ID, false)
	runJSON[core.ParticipantRoleBinding](t, ctx, coordplane, "participant", "bind", core.DefaultHumanParticipantID, "--socket", socket, "--project", projectA.ID, "--role", coownerRole.ID, "--request-id", "prd1-rebind-a", "--output", "json")
	flipProjectError(t, ctx, dbPath, projectA.ID)
	requireRepair(t, ctx, coordplane, socket, projectA.ID, true)

	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop daemon before recovery: %v\n%s", err, readLog(daemon.logPath))
	}
	daemon = startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "daemon restart")
	for project, head := range map[string]string{projectA.ID: taskA.HeadSHA, projectB.ID: taskB.HeadSHA} {
		if after := projectDetail(t, ctx, coordplane, socket, project); after.ActualCanonicalSHA != head || after.Status != core.ProjectActive {
			t.Fatalf("restarted Project %s = %#v, want active at %s", project, after, head)
		}
	}
	for _, id := range []string{taskA.ID, taskB.ID, review.ID} {
		if task := taskDetail(t, ctx, coordplane, socket, id).Task; task.Status != core.TaskCompleted {
			t.Fatalf("restarted Task %s status = %s", id, task.Status)
		}
	}
	if task := taskDetail(t, ctx, coordplane, socket, review.ID).Task; task.EvidenceType != string(core.EvidenceHumanConfirm) {
		t.Fatalf("restarted review evidence = %q", task.EvidenceType)
	}
	requireRepair(t, ctx, coordplane, socket, projectB.ID, false)
	_ = daemon.Stop()
	t.Logf("PASS projects A=%s B=%s C0=%s A=%s B=%s review=%s binaries=%s/%s", projectA.ID, projectB.ID, initialSHA, taskA.HeadSHA, taskB.HeadSHA, review.ID, coordplane, coordlink)
}

func capabilityArgs(capabilities []core.Capability, without ...core.Capability) []string {
	var args []string
	for _, capability := range capabilities {
		if len(without) == 1 && capability == without[0] {
			continue
		}
		args = append(args, "--capability", string(capability))
	}
	return args
}

func requireRepair(t *testing.T, ctx context.Context, binary, socket, projectID string, allowed bool) {
	t.Helper()
	raw, err := commandOutput(ctx, "", binary, "project", "repair", projectID, "--socket", socket, "--output", "json")
	if allowed && err != nil {
		t.Fatalf("repair in %s denied: %s", projectID, raw)
	}
	if !allowed && err == nil {
		t.Fatalf("repair in %s succeeded, want SCOPE_DENIED: %s", projectID, raw)
	}
}

func flipProjectError(t *testing.T, ctx context.Context, dbPath, projectID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	requireNoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, `UPDATE projects SET status='error', last_error='e2e injected error state' WHERE id=?`, projectID)
	requireNoError(t, err)
}

// setupRoleLayer grants a coowner participant.manage globally, swaps the
// human's global role to one without project.repair, and binds a developer
// role (no project.repair) in projectB.
func setupRoleLayer(t *testing.T, ctx context.Context, binary, socket string, coowner core.Agent, projectB core.Project, prefix string) (coownerRole, globalOperator, developer core.Role) {
	t.Helper()
	coownerRole = runJSON[core.Role](t, ctx, binary, append([]string{"role", "create", "--socket", socket, "--name", prefix + "-coowner", "--request-id", prefix + "-role-coowner", "--output", "json"}, capabilityArgs(core.AllCapabilities())...)...)
	globalOperator = runJSON[core.Role](t, ctx, binary, append([]string{"role", "create", "--socket", socket, "--name", prefix + "-global-operator", "--request-id", prefix + "-role-global", "--output", "json"}, capabilityArgs(core.AllCapabilities(), core.CapabilityProjectRepair)...)...)
	developer = runJSON[core.Role](t, ctx, binary, append([]string{"role", "create", "--socket", socket, "--name", prefix + "-developer", "--request-id", prefix + "-role-developer", "--output", "json"}, capabilityArgs(core.AllCapabilities(), core.CapabilityProjectRepair)...)...)
	runJSON[core.ParticipantRoleBinding](t, ctx, binary, "participant", "bind", coowner.ID, "--socket", socket, "--project", "global", "--role", coownerRole.ID, "--request-id", prefix+"-bind-coowner", "--output", "json")
	runJSON[core.ParticipantRoleBinding](t, ctx, binary, "participant", "bind", core.DefaultHumanParticipantID, "--socket", socket, "--project", "global", "--role", globalOperator.ID, "--request-id", prefix+"-bind-global", "--output", "json")
	runJSON[struct{ Unbound string }](t, ctx, binary, "participant", "unbind", core.DefaultHumanParticipantID, "--socket", socket, "--project", "global", "--role", core.DefaultOwnerRoleID, "--request-id", prefix+"-unbind-global", "--output", "json")
	runJSON[struct{ Unbound string }](t, ctx, binary, "participant", "unbind", core.DefaultHumanParticipantID, "--socket", socket, "--project", projectB.ID, "--role", core.DefaultOwnerRoleID, "--request-id", prefix+"-unbind-b", "--output", "json")
	runJSON[core.ParticipantRoleBinding](t, ctx, binary, "participant", "bind", core.DefaultHumanParticipantID, "--socket", socket, "--project", projectB.ID, "--role", developer.ID, "--request-id", prefix+"-bind-b", "--output", "json")
	return coownerRole, globalOperator, developer
}
