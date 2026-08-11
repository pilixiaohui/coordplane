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

// TestBudgetDeadlineTimesOutRun proves the live project honors a self-declared
// task budget end to end: task create --budget N lands in the input, the
// launched Run gets a durable deadline, the deadline actually kills the run,
// and the task is requeued as a resume point rather than failed.
//
// The runtime image scripted agent is left waiting in its inbox loop (P5-GO is
// never sent), so the run holds no requested outcome when the deadline fires
// and the terminal projection follows the clean resume path.
func TestBudgetDeadlineTimesOutRun(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	coordlink := requireExecutable(t, "E2E_COORDLINK_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	if image == "" {
		t.Fatal("E2E_RUNTIME_IMAGE is required")
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e", e2eTimeout)
	requireNoError(t, err)
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release Docker test resource: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	root := t.TempDir()
	source, _ := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	testsupport.WriteFile(t, instructions, []byte("Execute only the deterministic P5 bootstrap contract.\n"), 0o600)
	// run_timeout 0 and stall_timeout 0 so the task budget is the only wall-clock cap.
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{
		DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "0", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image,
		Tail: "  run_timeout: 0\n  stall_timeout: 0\n  shutdown_grace: 3s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n",
	}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "initial daemon startup")

	agent := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--display-name", "Budget Agent", "--adapter", "claude",
		"--image", image, "--instructions-file", instructions, "--request-id", "budget-agent", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "Budget deadline", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agent.ID, "--request-id", "budget-project", "--output", "json")

	// The scripted work role parses p5_role from the description and then waits
	// for P5-GO in its inbox. We never send P5-GO, so the agent just polls the
	// inbox until the budget deadline fires.
	const budgetSeconds int64 = 12
	task := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--title", "budget deadline run", "--description", "p5_role=A;no_direct=1",
		"--budget", "12", "--request-id", "budget-task", "--output", "json")
	if task.BudgetSeconds != budgetSeconds {
		t.Fatalf("task create --budget 12 => BudgetSeconds=%d, want %d", task.BudgetSeconds, budgetSeconds)
	}

	// Wait for the run to be active with a durable deadline pinned to the budget.
	detail := eventually(t, ctx, 45*time.Second, "active Run with budget deadline", func() (core.TaskDetail, bool, string) {
		current, err := commandJSON[core.TaskDetail](ctx, coordplane, "task", "show", task.ID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.TaskDetail{}, false, err.Error()
		}
		if current.CurrentRun == nil || current.CurrentRun.State != core.RunActive || current.CurrentRun.DeadlineAt == "" {
			return current, false, "no active run with deadline yet"
		}
		return current, true, ""
	})
	deadline, err := time.Parse(time.RFC3339Nano, detail.CurrentRun.DeadlineAt)
	requireNoError(t, err)
	now := time.Now().UTC()
	if deadline.Before(now) || deadline.After(now.Add(30*time.Second)) {
		t.Fatalf("deadline %s is outside expected now+12s window (now=%s)", deadline.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	runID := detail.CurrentRun.ID

	// The deadline must actually fire: run goes timed out, container removed,
	// and the task requeued as a resume point with the runtime failure prefix.
	terminal := eventually(t, ctx, 90*time.Second, "Run timed out and task requeued", func() (core.Run, bool, string) {
		run, err := commandJSON[core.Run](ctx, coordplane, "run", "show", runID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		if run.State != core.RunTimedOut || run.CleanupState != core.CleanupRemoved {
			return run, false, "state=" + string(run.State) + " cleanup=" + string(run.CleanupState)
		}
		current, err := commandJSON[core.TaskDetail](ctx, coordplane, "task", "show", task.ID, "--socket", socket, "--output", "json")
		if err != nil {
			return run, false, err.Error()
		}
		ok := current.CurrentRun == nil && current.Task.Status == core.TaskQueued &&
			strings.HasPrefix(current.Task.FailureReason, "RUN_TIMED_OUT")
		return run, ok, "task=" + string(current.Task.Status) + " failure=" + current.Task.FailureReason
	})
	if terminal.StopReason != "deadline exceeded" {
		t.Fatalf("Run StopReason = %q, want %q", terminal.StopReason, "deadline exceeded")
	}
	t.Logf("PASS task=%s run=%s deadline=%s -> timed_out, task requeued RUN_TIMED_OUT", task.ID, runID, terminal.DeadlineAt)
	t.Logf("formal binaries: coordplane=%s coordlink=%s", coordplane, coordlink)
}

// TestBudgetZeroDisablesDeadline is the positive control: without --budget the
// task budget is 0, the launched Run has no durable deadline, and the run is
// still live well after a budgeted deadline would have fired.
func TestBudgetZeroDisablesDeadline(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	coordlink := requireExecutable(t, "E2E_COORDLINK_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	if image == "" {
		t.Fatal("E2E_RUNTIME_IMAGE is required")
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e", e2eTimeout)
	requireNoError(t, err)
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release Docker test resource: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	root := t.TempDir()
	source, _ := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	testsupport.WriteFile(t, instructions, []byte("Execute only the deterministic P5 bootstrap contract.\n"), 0o600)
	// run_timeout 0: without a task budget there is no wall-clock cap at all.
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{
		DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "0", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image,
		Tail: "  run_timeout: 0\n  stall_timeout: 0\n  shutdown_grace: 3s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n",
	}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "initial daemon startup")

	agent := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--display-name", "NoBudget Agent", "--adapter", "claude",
		"--image", image, "--instructions-file", instructions, "--request-id", "nobudget-agent", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "No budget control", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agent.ID, "--request-id", "nobudget-project", "--output", "json")

	task := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--title", "no budget control", "--description", "p5_role=A;no_direct=1",
		"--request-id", "nobudget-task", "--output", "json")
	if task.BudgetSeconds != 0 {
		t.Fatalf("task create without --budget => BudgetSeconds=%d, want 0", task.BudgetSeconds)
	}

	// Wait for an active run, assert it has no deadline, and keep waiting long
	// enough that a 12s budget (matching the sibling test) would already have
	// fired and killed the run.
	active := eventually(t, ctx, 45*time.Second, "active Run without deadline", func() (core.Run, bool, string) {
		current, err := commandJSON[core.TaskDetail](ctx, coordplane, "task", "show", task.ID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		if current.CurrentRun == nil || current.CurrentRun.State != core.RunActive {
			return core.Run{}, false, "no active run yet"
		}
		return *current.CurrentRun, true, ""
	})
	if active.DeadlineAt != "" {
		t.Fatalf("no-budget active Run has DeadlineAt=%q, want none", active.DeadlineAt)
	}
	runID := active.ID
	survived := eventually(t, ctx, 20*time.Second, "run still live after budget-equivalent window", func() (core.Run, bool, string) {
		run, err := commandJSON[core.Run](ctx, coordplane, "run", "show", runID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		return run, run.State == core.RunActive, "state=" + string(run.State)
	})
	if survived.State != core.RunActive {
		t.Fatalf("no-budget run was killed without a deadline: %#v", survived)
	}
	t.Logf("PASS task=%s run=%s still active with no deadline (budget 0 = no cap)", task.ID, runID)
	t.Logf("formal binaries: coordplane=%s coordlink=%s", coordplane, coordlink)
}
