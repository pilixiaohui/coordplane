//go:build e2e

package e2e_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
	_ "modernc.org/sqlite"
)

const e2eTimeout = 3 * time.Minute

func TestDeterministicTwoAgentConvergence(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	coordlink := requireExecutable(t, "E2E_COORDLINK_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	if image == "" {
		t.Fatal("E2E_RUNTIME_IMAGE is required")
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e", e2eTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release Docker test resource: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	root := t.TempDir()
	source, initialSHA := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	writeFile(t, instructions, []byte("Execute only the deterministic P5 bootstrap contract.\n"), 0o600)
	configPath := writeConfig(t, root, dataDir, socket, image)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "initial daemon startup")

	agentA := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--display-name", "P5 Agent A", "--adapter", "codex",
		"--image", image, "--instructions-file", instructions, "--request-id", "p5-agent-a", "--output", "json")
	agentB := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--display-name", "P5 Agent B", "--adapter", "codex",
		"--image", image, "--instructions-file", instructions, "--request-id", "p5-agent-b", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "P5 deterministic", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agentA.ID, "--request-id", "p5-project", "--output", "json")
	if project.InitialSHA != initialSHA || project.CanonicalSHA != initialSHA || project.Status != core.ProjectActive {
		t.Fatalf("registered Project = %#v, want active at C0 %s", project, initialSHA)
	}

	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane,
			"agent", "pause", agent.ID, "--socket", socket, "--request-id", "p5-pause-"+agent.ID, "--output", "json")
	}
	taskA := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agentA.ID,
		"--title", "P5 work A", "--description", "p5_role=A;peer_agent_id="+agentB.ID,
		"--request-id", "p5-task-a", "--output", "json")
	taskB := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agentB.ID,
		"--title", "P5 work B", "--description", "p5_role=B;peer_agent_id="+agentA.ID,
		"--request-id", "p5-task-b", "--output", "json")
	if taskA.BaseSHA != initialSHA || taskB.BaseSHA != initialSHA {
		t.Fatalf("work Tasks do not share C0: A=%s B=%s want=%s", taskA.BaseSHA, taskB.BaseSHA, initialSHA)
	}
	bootA := sendBossMessage(t, ctx, coordplane, socket, project.ID, agentA.ID, taskA.ID, "P5-BOOT A", "p5-boot-a")
	bootB := sendBossMessage(t, ctx, coordplane, socket, project.ID, agentB.ID, taskB.ID, "P5-BOOT B", "p5-boot-b")

	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane,
			"agent", "resume", agent.ID, "--socket", socket, "--request-id", "p5-resume-"+agent.ID, "--output", "json")
	}

	runA, runB := waitForConcurrentRuns(t, ctx, coordplane, socket, taskA.ID, taskB.ID)
	inspectA := inspectContainer(t, ctx, runA.ContainerID)
	inspectB := inspectContainer(t, ctx, runB.ContainerID)
	assertIsolatedRuns(t, dataDir, runA, runB, inspectA, inspectB)

	sendBossMessage(t, ctx, coordplane, socket, project.ID, agentA.ID, taskA.ID, "P5-GO A", "p5-go-a")
	sendBossMessage(t, ctx, coordplane, socket, project.ID, agentB.ID, taskB.ID, "P5-GO B", "p5-go-b")

	taskA = waitForTask(t, ctx, coordplane, socket, taskA.ID, "A captured submission", func(task core.Task) bool {
		return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.HeadRunID != "" && task.TaskRef != ""
	})
	taskB = waitForTask(t, ctx, coordplane, socket, taskB.ID, "B captured submission", func(task core.Task) bool {
		return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.HeadRunID != "" && task.TaskRef != ""
	})
	if taskA.HeadSHA == taskB.HeadSHA || taskA.HeadRunID != runA.ID || taskB.HeadRunID != runB.ID {
		t.Fatalf("captured work identities = A(%s,%s) B(%s,%s)", taskA.HeadSHA, taskA.HeadRunID, taskB.HeadSHA, taskB.HeadRunID)
	}

	checkout := filepath.Join(root, "review-a")
	fact := runJSON[core.GitCheckoutFact](t, ctx, coordplane,
		"task", "checkout", taskA.ID, "--socket", socket, "--dest", checkout, "--output", "json")
	if fact.HeadSHA != taskA.HeadSHA || git(t, ctx, checkout, "rev-parse", "HEAD") != taskA.HeadSHA {
		t.Fatalf("Boss checkout = %#v, want exact A head %s", fact, taskA.HeadSHA)
	}
	assertFile(t, filepath.Join(checkout, "agent-A.txt"), "agent-A\n")
	if _, err := os.Stat(filepath.Join(checkout, "agent-B.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("A review checkout unexpectedly contains B result: %v", err)
	}

	runJSON[core.Task](t, ctx, coordplane,
		"task", "accept", taskA.ID, "--socket", socket, "--integration-agent", agentA.ID,
		"--request-id", "p5-accept-a", "--output", "json")
	taskA = waitForTask(t, ctx, coordplane, socket, taskA.ID, "A direct CAS", func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	projectAfterA := projectDetail(t, ctx, coordplane, socket, project.ID)
	if projectAfterA.ActualCanonicalSHA != taskA.HeadSHA {
		t.Fatalf("canonical after A = %s, want %s", projectAfterA.ActualCanonicalSHA, taskA.HeadSHA)
	}

	runJSON[core.Task](t, ctx, coordplane,
		"task", "accept", taskB.ID, "--socket", socket, "--integration-agent", agentA.ID,
		"--request-id", "p5-accept-b", "--output", "json")
	taskB = waitForTask(t, ctx, coordplane, socket, taskB.ID, "B stale integration link", func(task core.Task) bool {
		return task.IntegrationTaskID != "" && task.Status == core.TaskSubmitted
	})
	integrationID := taskB.IntegrationTaskID
	integration := waitForTask(t, ctx, coordplane, socket, integrationID, "integration CAS", func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.HeadSHA != "" && task.TaskRef != "" && task.FinalCanonicalSHA == task.HeadSHA
	})
	taskB = waitForTask(t, ctx, coordplane, socket, taskB.ID, "B completed through integration", func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == integration.HeadSHA
	})
	if integration.Kind != core.TaskIntegration || integration.SourceTaskID != taskB.ID ||
		integration.SourceRunID != taskB.HeadRunID || integration.SourceTaskRef != taskB.TaskRef ||
		integration.SourceHeadSHA != taskB.HeadSHA || integration.HeadRunID == "" {
		t.Fatalf("integration identity does not pin B capture: integration=%#v B=%#v", integration, taskB)
	}

	controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
	finalSHA := gitDir(t, ctx, controlRepo, "rev-parse", project.CanonicalRef)
	if finalSHA != integration.HeadSHA || taskA.FinalCanonicalSHA != taskA.HeadSHA || taskB.FinalCanonicalSHA != finalSHA {
		t.Fatalf("final SHA mismatch: actual=%s integration=%s A=%s B=%s", finalSHA, integration.HeadSHA, taskA.FinalCanonicalSHA, taskB.FinalCanonicalSHA)
	}
	for label, ancestor := range map[string]string{"C0": initialSHA, "A": taskA.HeadSHA, "B": taskB.HeadSHA} {
		gitDirSucceeds(t, ctx, controlRepo, "merge-base", "--is-ancestor", ancestor, finalSHA)
		t.Logf("canonical lineage includes %s %s", label, ancestor)
	}
	if got := gitDir(t, ctx, controlRepo, "show", "-s", "--format=%s", finalSHA); got != "P5 integrate stale source" {
		t.Fatalf("final commit subject = %q, want scripted integration commit", got)
	}
	for _, task := range []core.Task{taskA, taskB, integration} {
		if got := gitDir(t, ctx, controlRepo, "rev-parse", task.TaskRef); got != task.HeadSHA {
			t.Fatalf("task ref %s = %s, want %s", task.TaskRef, got, task.HeadSHA)
		}
	}
	gitDirSucceeds(t, ctx, controlRepo, "fsck", "--full", "--strict")
	finalCheckout := filepath.Join(root, "final")
	run(t, ctx, "git", "clone", "--quiet", controlRepo, finalCheckout)
	git(t, ctx, finalCheckout, "checkout", "--quiet", finalSHA)
	runIn(t, ctx, finalCheckout, "./fixture-test.sh")

	assertOneIntegrationTask(t, ctx, coordplane, socket, project.ID, integrationID)
	waitForMessageEvidence(t, ctx, coordplane, socket, project.ID, agentB.ID, taskA.ID, bootA.ID, bootB.ID)
	assertPublicProjection(t, ctx, coordplane, socket, project.ID, finalSHA, taskA.ID, taskB.ID, integrationID)
	waitForNoProjectContainers(t, ctx, project.ID)

	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop daemon before recovery: %v\n%s", err, readLog(daemon.logPath))
	}
	daemon = startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "daemon restart")
	projectAfterRestart := projectDetail(t, ctx, coordplane, socket, project.ID)
	if projectAfterRestart.ActualCanonicalSHA != finalSHA || projectAfterRestart.Status != core.ProjectActive {
		t.Fatalf("restarted Project = %#v, want active at %s", projectAfterRestart, finalSHA)
	}
	for _, expected := range []struct {
		id     string
		status core.TaskStatus
	}{{taskA.ID, core.TaskCompleted}, {taskB.ID, core.TaskCompleted}, {integrationID, core.TaskCompleted}} {
		if task := taskDetail(t, ctx, coordplane, socket, expected.id).Task; task.Status != expected.status {
			t.Fatalf("restarted Task %s status = %s, want %s", expected.id, task.Status, expected.status)
		}
	}

	preview := runJSON[core.GCPreview](t, ctx, coordplane,
		"gc", "preview", "--socket", socket, "--output", "json")
	if len(preview.Workspaces) == 0 || len(preview.TaskRefs) == 0 {
		t.Fatalf("GC preview omitted maintained resources: %#v", preview)
	}
	gcResult := runJSON[core.GCRunResult](t, ctx, coordplane,
		"gc", "run", "--socket", socket, "--confirm", "--request-id", "p5-gc", "--output", "json")
	if !gcResult.Completed {
		t.Fatalf("GC run = %#v", gcResult)
	}
	waitForWorkspacesRemoved(t, ctx, dataDir, project.ID, taskA.ID, taskB.ID, integrationID)
	for _, task := range []core.Task{taskA, taskB, integration} {
		if got := gitDir(t, ctx, controlRepo, "rev-parse", task.TaskRef); got != task.HeadSHA {
			t.Fatalf("GC removed retained task ref %s: got %s want %s", task.TaskRef, got, task.HeadSHA)
		}
	}
	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop recovered daemon: %v\n%s", err, readLog(daemon.logPath))
	}
	assertSQLiteTruth(t, filepath.Join(dataDir, "coordplane.db"), taskA.ID, taskB.ID, integrationID, finalSHA)
	t.Logf("PASS project=%s C0=%s A=%s B=%s integration=%s final=%s", project.ID, initialSHA, taskA.HeadSHA, taskB.HeadSHA, integrationID, finalSHA)
	t.Logf("formal binaries: coordplane=%s coordlink=%s", coordplane, coordlink)
}

type daemonProcess struct {
	command  *exec.Cmd
	done     chan error
	logPath  string
	stopOnce sync.Once
	stopErr  error
}

func startDaemon(t *testing.T, binary, configPath, socket string) *daemonProcess {
	t.Helper()
	logPath := filepath.Join(filepath.Dir(configPath), fmt.Sprintf("daemon-%d.log", time.Now().UnixNano()))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "serve", "--config", configPath)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start daemon: %v", err)
	}
	process := &daemonProcess{command: command, done: make(chan error, 1), logPath: logPath}
	go func() {
		process.done <- command.Wait()
		_ = logFile.Close()
	}()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return process
		}
		select {
		case err := <-process.done:
			t.Fatalf("daemon exited during startup: %v\n%s", err, readLog(logPath))
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon socket did not appear\n%s", readLog(logPath))
	return nil
}

func (p *daemonProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		if p.command.Process != nil {
			_ = p.command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case p.stopErr = <-p.done:
		case <-time.After(20 * time.Second):
			if p.command.Process != nil {
				_ = p.command.Process.Kill()
			}
			p.stopErr = fmt.Errorf("daemon did not stop within 20s: %w", <-p.done)
		}
	})
	return p.stopErr
}

func waitForReady(t *testing.T, ctx context.Context, binary, socket, reason string) core.Status {
	t.Helper()
	return eventually(t, ctx, 30*time.Second, reason, func() (core.Status, bool, string) {
		status, err := commandJSON[core.Status](ctx, binary, "status", "--socket", socket, "--output", "json")
		if err != nil {
			return core.Status{}, false, err.Error()
		}
		return status, status.DaemonReady, status.Reason
	})
}

func waitForConcurrentRuns(t *testing.T, ctx context.Context, binary, socket, taskA, taskB string) (core.Run, core.Run) {
	t.Helper()
	return waitForConcurrentRunsWithProgress(t, ctx, binary, socket, taskA, taskB, "P5-READY", 45*time.Second)
}

func waitForConcurrentRunsWithProgress(
	t *testing.T,
	ctx context.Context,
	binary, socket, taskA, taskB, progress string,
	timeout time.Duration,
) (core.Run, core.Run) {
	t.Helper()
	type pair struct{ A, B core.Run }
	result := eventually(t, ctx, timeout, "two active Docker Runs with "+progress, func() (pair, bool, string) {
		a, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskA, "--socket", socket, "--output", "json")
		if err != nil {
			return pair{}, false, err.Error()
		}
		b, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskB, "--socket", socket, "--output", "json")
		if err != nil {
			return pair{}, false, err.Error()
		}
		ready := func(detail core.TaskDetail) bool {
			return detail.Task.Status == core.TaskRunning && detail.CurrentRun != nil &&
				detail.CurrentRun.State == core.RunActive && detail.CurrentRun.ContainerID != "" &&
				detail.LatestProgress != nil && strings.Contains(detail.LatestProgress.PayloadJSON, progress)
		}
		if !ready(a) || !ready(b) {
			return pair{}, false, fmt.Sprintf("A=%s/%v B=%s/%v", a.Task.Status, a.CurrentRun != nil, b.Task.Status, b.CurrentRun != nil)
		}
		return pair{*a.CurrentRun, *b.CurrentRun}, true, ""
	})
	return result.A, result.B
}

func waitForTask(t *testing.T, ctx context.Context, binary, socket, taskID, reason string, predicate func(core.Task) bool) core.Task {
	t.Helper()
	return eventually(t, ctx, 60*time.Second, reason, func() (core.Task, bool, string) {
		detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Task{}, false, err.Error()
		}
		return detail.Task, predicate(detail.Task), fmt.Sprintf("status=%s pending=%s integration=%s failure=%s", detail.Task.Status, detail.Task.PendingAction, detail.Task.IntegrationTaskID, detail.Task.FailureReason)
	})
}

func sendBossMessage(t *testing.T, ctx context.Context, binary, socket, projectID, agentID, taskID, body, requestID string) core.Message {
	t.Helper()
	return runJSON[core.Message](t, ctx, binary,
		"message", "send", "--socket", socket, "--project", projectID, "--agent", agentID,
		"--task", taskID, "--body", body, "--request-id", requestID, "--output", "json")
}

func projectDetail(t *testing.T, ctx context.Context, binary, socket, projectID string) core.ProjectDetail {
	t.Helper()
	return runJSON[core.ProjectDetail](t, ctx, binary,
		"project", "show", projectID, "--socket", socket, "--output", "json")
}

func taskDetail(t *testing.T, ctx context.Context, binary, socket, taskID string) core.TaskDetail {
	t.Helper()
	return runJSON[core.TaskDetail](t, ctx, binary,
		"task", "show", taskID, "--socket", socket, "--output", "json")
}

func assertOneIntegrationTask(t *testing.T, ctx context.Context, binary, socket, projectID, integrationID string) {
	t.Helper()
	page := runJSON[core.TaskPage](t, ctx, binary,
		"task", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	var integrations []string
	for _, task := range page.Items {
		if task.Kind == core.TaskIntegration {
			integrations = append(integrations, task.ID)
		}
	}
	if len(integrations) != 1 || integrations[0] != integrationID {
		t.Fatalf("integration Tasks = %v, want only %s", integrations, integrationID)
	}
}

func waitForMessageEvidence(t *testing.T, ctx context.Context, binary, socket, projectID, agentBID, taskAID, bootAID, bootBID string) {
	t.Helper()
	evidence := eventually(t, ctx, 45*time.Second, "direct Message ack and unacked launch redelivery", func() (core.MessagePage, bool, string) {
		page, err := commandJSON[core.MessagePage](ctx, binary,
			"message", "list", "--socket", socket, "--project", projectID, "--limit", "20", "--output", "json")
		if err != nil {
			return core.MessagePage{}, false, err.Error()
		}
		var direct, bootA, bootB *core.Message
		for index := range page.Items {
			message := &page.Items[index]
			switch {
			case strings.Contains(message.Body, "P5-DIRECT"):
				direct = message
			case message.ID == bootAID:
				bootA = message
			case message.ID == bootBID:
				bootB = message
			}
		}
		ok := direct != nil && direct.State == core.MessageAcknowledged && direct.RecipientID == agentBID &&
			direct.RelatedTaskID == taskAID && bootA != nil && bootA.State == core.MessageAcknowledged &&
			bootB != nil && bootB.State == core.MessageAcknowledged && bootB.DeliveryCount >= 1
		return page, ok, fmt.Sprintf("direct=%#v bootA=%#v bootB=%#v", direct, bootA, bootB)
	})
	var direct core.Message
	for _, message := range evidence.Items {
		if strings.Contains(message.Body, "P5-DIRECT") {
			direct = message
		}
	}
	conversation := taskDetail(t, ctx, binary, socket, direct.TaskID).Task
	if conversation.Kind != core.TaskConversation || conversation.AssigneeAgentID != agentBID || conversation.Status != core.TaskWaiting {
		t.Fatalf("direct Message delivery Task = %#v", conversation)
	}
	events := runJSON[core.EventPage](t, ctx, binary,
		"events", "tail", "--socket", socket, "--project", projectID, "--limit", "100", "--output", "json")
	redelivered := false
	for _, event := range events.Items {
		if event.EntityID == bootBID && event.Kind == "message.redelivered" {
			redelivered = true
		}
	}
	if !redelivered {
		t.Fatalf("events omit redelivery of unacknowledged launch Message %s", bootBID)
	}
}

func assertPublicProjection(t *testing.T, ctx context.Context, binary, socket, projectID, finalSHA string, taskIDs ...string) {
	t.Helper()
	status := runJSON[core.Status](t, ctx, binary,
		"status", "--socket", socket, "--project", projectID, "--output", "json")
	if !status.DaemonReady || len(status.ActualRefs) != 1 || status.ActualRefs[0].ActualSHA != finalSHA {
		t.Fatalf("Boss status does not expose final Git truth: %#v", status)
	}
	wanted := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		wanted[id] = true
	}
	tasks := runJSON[core.TaskPage](t, ctx, binary,
		"task", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	for _, task := range tasks.Items {
		delete(wanted, task.ID)
	}
	if len(wanted) != 0 {
		t.Fatalf("Boss status omitted Tasks: %v", wanted)
	}
	runs := runJSON[core.RunPage](t, ctx, binary,
		"run", "list", "--socket", socket, "--project", projectID, "--limit", "500", "--output", "json")
	if len(runs.Items) < 4 {
		t.Fatalf("Boss Run history has %d entries, want work A/B, conversation, integration", len(runs.Items))
	}
	for _, run := range runs.Items {
		if core.IsRunLive(run.State) {
			t.Fatalf("Run remained live after convergence: %#v", run)
		}
	}
}

type dockerInspection struct {
	ID    string `json:"Id"`
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func inspectContainer(t *testing.T, ctx context.Context, id string) dockerInspection {
	t.Helper()
	raw := runOutput(t, ctx, "", "docker", "inspect", id)
	var values []dockerInspection
	if err := json.Unmarshal(raw, &values); err != nil || len(values) != 1 {
		t.Fatalf("decode docker inspect for %s: %v (documents=%d)", id, err, len(values))
	}
	return values[0]
}

func assertIsolatedRuns(t *testing.T, dataDir string, runA, runB core.Run, a, b dockerInspection) {
	t.Helper()
	if !a.State.Running || !b.State.Running || a.ID == b.ID || runA.ID == runB.ID || runA.ContainerName == runB.ContainerName {
		t.Fatalf("Runs were not concurrently live and distinct: A=%#v/%#v B=%#v/%#v", runA, a.State, runB, b.State)
	}
	mounts := func(inspect dockerInspection) map[string]struct {
		source string
		rw     bool
	} {
		result := make(map[string]struct {
			source string
			rw     bool
		})
		for _, mount := range inspect.Mounts {
			result[mount.Destination] = struct {
				source string
				rw     bool
			}{mount.Source, mount.RW}
		}
		return result
	}
	am, bm := mounts(a), mounts(b)
	for _, target := range []string{"/workspace/project", "/home/agent", "/run/coordplane"} {
		left, leftOK := am[target]
		right, rightOK := bm[target]
		if !leftOK || !rightOK || !left.rw || !right.rw || left.source == right.source {
			t.Fatalf("isolated mount %s: A=%#v B=%#v", target, left, right)
		}
	}
	for _, inspect := range []dockerInspection{a, b} {
		for _, mount := range inspect.Mounts {
			if mount.Source == dataDir || mount.Source == filepath.Join(dataDir, "coordplane.db") ||
				strings.HasPrefix(mount.Source, filepath.Join(dataDir, "repos")+string(filepath.Separator)) {
				t.Fatalf("Run mounted control truth: %#v", mount)
			}
		}
	}
	if a.Config.Labels["coordplane.run_id"] != runA.ID || b.Config.Labels["coordplane.run_id"] != runB.ID {
		t.Fatalf("Docker ownership labels do not match Runs: A=%v B=%v", a.Config.Labels, b.Config.Labels)
	}
}

func waitForNoProjectContainers(t *testing.T, ctx context.Context, projectID string) {
	t.Helper()
	eventually(t, ctx, 30*time.Second, "all project containers removed", func() (string, bool, string) {
		raw, err := commandOutput(ctx, "", "docker", "ps", "-aq", "--filter", "label=coordplane.project_id="+projectID)
		if err != nil {
			return "", false, err.Error()
		}
		value := strings.TrimSpace(string(raw))
		return value, value == "", value
	})
}

func waitForWorkspacesRemoved(t *testing.T, ctx context.Context, dataDir, projectID string, taskIDs ...string) {
	t.Helper()
	eventually(t, ctx, 20*time.Second, "terminal workspaces removed", func() (bool, bool, string) {
		var existing []string
		for _, taskID := range taskIDs {
			path := filepath.Join(dataDir, "workspaces", projectID, taskID)
			if _, err := os.Stat(path); err == nil {
				existing = append(existing, taskID)
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, false, err.Error()
			}
		}
		return len(existing) == 0, len(existing) == 0, strings.Join(existing, ",")
	})
}

func assertSQLiteTruth(t *testing.T, path, taskA, taskB, integrationID, finalSHA string) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	wantTables := []string{"agents", "events", "messages", "projects", "request_dedupes", "runs", "schema_migrations", "tasks"}
	if strings.Join(tables, ",") != strings.Join(wantTables, ",") {
		t.Fatalf("SQLite tables = %v, want six objects plus infrastructure %v", tables, wantTables)
	}
	for _, taskID := range []string{taskA, taskB, integrationID} {
		var status, final string
		if err := database.QueryRow(`SELECT status, final_canonical_sha FROM tasks WHERE id=?`, taskID).Scan(&status, &final); err != nil {
			t.Fatal(err)
		}
		if status != string(core.TaskCompleted) || final == "" {
			t.Fatalf("SQLite Task %s = status %s final %s", taskID, status, final)
		}
		if taskID != taskA && final != finalSHA {
			t.Fatalf("SQLite Task %s final = %s, want %s", taskID, final, finalSHA)
		}
	}
	var liveRuns, messages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM runs WHERE state IN ('starting','active')`).Scan(&liveRuns); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if liveRuns != 0 || messages < 5 {
		t.Fatalf("SQLite terminal truth: live Runs=%d Messages=%d", liveRuns, messages)
	}
}

func createSourceRepository(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, ctx, "git", "init", "--quiet", "--initial-branch", "main", source)
	git(t, ctx, source, "config", "user.name", "CoordPlane P5 Fixture")
	git(t, ctx, source, "config", "user.email", "p5-fixture@coordplane.local")
	writeFile(t, filepath.Join(source, "base.txt"), []byte("C0\n"), 0o644)
	writeFile(t, filepath.Join(source, "fixture-test.sh"), []byte("#!/bin/sh\nset -eu\ntest \"$(cat base.txt)\" = C0\nfor role in A B; do\n  file=agent-$role.txt\n  if [ -e \"$file\" ]; then test \"$(cat \"$file\")\" = agent-$role; fi\ndone\n"), 0o755)
	git(t, ctx, source, "add", "base.txt", "fixture-test.sh")
	git(t, ctx, source, "commit", "--quiet", "-m", "P5 C0")
	return source, git(t, ctx, source, "rev-parse", "HEAD")
}

func writeConfig(t *testing.T, root, dataDir, socket, image string) string {
	t.Helper()
	path := filepath.Join(root, "coordplane.yaml")
	content := fmt.Sprintf(`data_dir: %s
operator_socket: %s
max_parallel_runs: 2
retention:
  completed_workspace: 0
  terminal_task_ref: 24h
  run_log: 24h
runtime:
  docker_network: none
  workspace_root: %s
  agent_home_root: %s
  log_root: %s
  default_image: %s
  provider_env_allowlist: []
  run_timeout: 2m
  shutdown_grace: 3s
git:
  capture_helper_image: %s
  capture_timeout: 30s
  maximum_bundle_bytes: 67108864
  maximum_objects: 250000
  maximum_handoff_bytes: 268435456
`, dataDir, socket, filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "agent-homes"), filepath.Join(dataDir, "logs"), image, image)
	writeFile(t, path, []byte(content), 0o600)
	return path
}

func runJSON[T any](t *testing.T, ctx context.Context, binary string, args ...string) T {
	t.Helper()
	value, err := commandJSON[T](ctx, binary, args...)
	if err != nil {
		t.Fatalf("%s %s: %v", binary, strings.Join(args, " "), err)
	}
	return value
}

func commandJSON[T any](ctx context.Context, binary string, args ...string) (T, error) {
	var value T
	raw, err := commandOutput(ctx, "", binary, args...)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fmt.Errorf("decode JSON: %w; output=%s", err, raw)
	}
	return value, nil
}

func eventually[T any](t *testing.T, ctx context.Context, timeout time.Duration, reason string, probe func() (T, bool, string)) T {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last T
	lastDetail := "not probed"
	for time.Now().Before(deadline) && ctx.Err() == nil {
		value, ok, detail := probe()
		last, lastDetail = value, detail
		if ok {
			return value
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s: %s (context=%v)", reason, lastDetail, ctx.Err())
	return last
}

func requireExecutable(t *testing.T, name string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(name))
	if path == "" || !filepath.IsAbs(path) {
		t.Fatalf("%s must be an absolute executable path", name)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("%s=%s is not executable: %v", name, path, err)
	}
	return path
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != want {
		t.Fatalf("file %s = %q err=%v, want %q", path, raw, err, want)
	}
}

func git(t *testing.T, ctx context.Context, directory string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(runOutput(t, ctx, directory, "git", args...)))
}

func gitDir(t *testing.T, ctx context.Context, directory string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(runOutput(t, ctx, "", "git", append([]string{"--git-dir=" + directory}, args...)...)))
}

func gitDirSucceeds(t *testing.T, ctx context.Context, directory string, args ...string) {
	t.Helper()
	runOutput(t, ctx, "", "git", append([]string{"--git-dir=" + directory}, args...)...)
}

func run(t *testing.T, ctx context.Context, command string, args ...string) {
	t.Helper()
	runOutput(t, ctx, "", command, args...)
}

func runIn(t *testing.T, ctx context.Context, directory, command string, args ...string) {
	t.Helper()
	runOutput(t, ctx, directory, command, args...)
}

func runOutput(t *testing.T, ctx context.Context, directory, command string, args ...string) []byte {
	t.Helper()
	raw, err := commandOutput(ctx, directory, command, args...)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func commandOutput(ctx context.Context, directory, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = directory
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return raw, fmt.Errorf("%s %s: %w; output=%s", command, strings.Join(args, " "), err, raw)
	}
	return raw, nil
}

func readLog(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}
