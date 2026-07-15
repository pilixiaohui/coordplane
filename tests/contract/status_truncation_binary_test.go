package contract_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func TestStatusHumanBinaryReportsTruncatedTasksAndAgents(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "")
	source := createRepository(t, root)
	daemon := startDaemon(t, configPath, socket)
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })

	agents := make([]core.Agent, 0, core.StatusSnapshotLimit+1)
	for index := 0; index <= core.StatusSnapshotLimit; index++ {
		raw := runBinaryJSON(t, testBinaries.coordplane,
			"agent", "add", "--socket", socket,
			"--display-name", fmt.Sprintf("Status Agent %02d", index),
			"--adapter", "codex", "--image", "agent:latest",
			"--instructions-file", filepath.Join(root, "agent.md"),
			"--request-id", fmt.Sprintf("status-agent-%02d", index), "--output", "json")
		var agent core.Agent
		decodeJSON(t, raw, &agent)
		runBinaryJSON(t, testBinaries.coordplane,
			"agent", "pause", agent.ID, "--socket", socket,
			"--request-id", fmt.Sprintf("status-agent-pause-%02d", index), "--output", "json")
		agents = append(agents, agent)
	}

	projectRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "add", "--socket", socket, "--name", "status-project",
		"--repo", source, "--ref", "refs/heads/main",
		"--request-id", "status-project", "--output", "json")
	var project core.Project
	decodeJSON(t, projectRaw, &project)

	for index, agent := range agents {
		runBinaryJSON(t, testBinaries.coordplane,
			"task", "create", "--socket", socket, "--project", project.ID,
			"--agent", agent.ID, "--title", fmt.Sprintf("Status task %02d", index),
			"--request-id", fmt.Sprintf("status-task-%02d", index), "--output", "json")
	}

	jsonOutput := runBinaryJSON(t, testBinaries.coordplane,
		"status", "--socket", socket, "--project", project.ID, "--output", "json")
	var status core.Status
	decodeJSON(t, jsonOutput, &status)
	if !status.SummaryTruncated || len(status.Snapshot.Agents) != core.StatusSnapshotLimit || len(status.Tasks) != core.StatusSnapshotLimit {
		t.Fatalf("status fixture is not a truncated %d-item projection: %#v", core.StatusSnapshotLimit, status)
	}

	command := exec.Command(testBinaries.coordplane, "status", "--socket", socket, "--project", project.ID)
	humanOutput, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("human status: %v\n%s", err, humanOutput)
	}
	var humanStatus core.Status
	decodeJSON(t, humanOutput, &humanStatus)
	if !humanStatus.SummaryTruncated || len(humanStatus.Snapshot.Agents) != core.StatusSnapshotLimit || len(humanStatus.Tasks) != core.StatusSnapshotLimit {
		t.Fatalf("human status does not preserve bounded projection metadata: %#v", humanStatus)
	}
}

func TestStatusAndRunListBinariesDiscloseFieldTruncationAndRecoverExactDetails(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "cp-field-trunc-")
	requireNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "")
	source := createRepository(t, root)
	daemon := startDaemon(t, configPath, socket)
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })

	agentRaw := runBinaryJSON(t, testBinaries.coordplane,
		"agent", "add", "--socket", socket,
		"--display-name", "Field truncation agent",
		"--adapter", "codex", "--image", "agent:latest",
		"--instructions-file", filepath.Join(root, "agent.md"),
		"--request-id", "field-truncation-agent", "--output", "json")
	var agent core.Agent
	decodeJSON(t, agentRaw, &agent)
	runBinaryJSON(t, testBinaries.coordplane,
		"agent", "pause", agent.ID, "--socket", socket,
		"--request-id", "field-truncation-agent-pause", "--output", "json")

	projectRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "add", "--socket", socket, "--name", "field-truncation-project",
		"--repo", source, "--ref", "refs/heads/main",
		"--request-id", "field-truncation-project", "--output", "json")
	var project core.Project
	decodeJSON(t, projectRaw, &project)

	longTitle := "标题: " + strings.Repeat("中文任务", 40)
	longReason := "终止: " + strings.Repeat("运行失败原因", 50)
	if len(longTitle) <= 256 || len(longTitle) > 512 || len(longReason) <= 512 || len(longReason) > core.MaximumTerminalTextBytes {
		t.Fatalf("fixture is outside legal field bounds: title=%d reason=%d", len(longTitle), len(longReason))
	}
	if !utf8.ValidString(longTitle) || !utf8.ValidString(longReason) {
		t.Fatal("fixture must contain valid UTF-8")
	}
	taskRaw := runBinaryJSON(t, testBinaries.coordplane,
		"task", "create", "--socket", socket, "--project", project.ID,
		"--agent", agent.ID, "--title", longTitle, "--max-retries", "0",
		"--request-id", "field-truncation-task", "--output", "json")
	var task core.Task
	decodeJSON(t, taskRaw, &task)
	if task.Title != longTitle {
		t.Fatalf("production task create changed legal title: got %d bytes, want %d", len(task.Title), len(longTitle))
	}

	stopDaemon(t, daemon, socket)
	database, err := store.Open(context.Background(), filepath.Join(dataDir, "coordplane.db"))
	requireNoError(t, err)
	service, err := core.NewService(database, &contractGit{sha: project.InitialSHA, root: filepath.Join(dataDir, "repos")}, core.ServiceOptions{MaxParallelRuns: 1, AdapterIDs: []string{"codex"}})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := service.SetAgentStatus(context.Background(), agent.ID, core.AgentActive, "field-truncation-agent-resume"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	claim, ok, err := service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		_ = database.Close()
		t.Fatalf("claim field-truncation task: claim=%#v ok=%t err=%v", claim, ok, err)
	}
	active := activateContractRuntimeRun(t, context.Background(), service, claim, "field-truncation")
	interrupted := interruptContractRuntimeRun(t, context.Background(), service, active, longReason, "field-truncation-interrupt")
	if interrupted.TerminalReason != longReason {
		_ = database.Close()
		t.Fatalf("P1 interrupt changed legal terminal reason: got %d bytes, want %d", len(interrupted.TerminalReason), len(longReason))
	}
	requireNoError(t, database.Close())
	daemon = startDaemon(t, configPath, socket)

	statusRaw := runBinaryJSON(t, testBinaries.coordplane,
		"status", "--socket", socket, "--project", project.ID, "--output", "json")
	var status core.Status
	decodeJSON(t, statusRaw, &status)
	if len(status.Snapshot.Projects) != 1 || len(status.Snapshot.Agents) != 1 || len(status.Tasks) != 1 {
		t.Fatalf("fixture unexpectedly hit the %d-object projection bound: %#v", core.StatusSnapshotLimit, status)
	}
	if !status.SummaryTruncated {
		t.Fatalf("status did not propagate per-item field truncation: %#v", status)
	}
	taskSummary := status.Tasks[0].Task
	if taskSummary.ID != task.ID || !taskSummary.TitleTruncated || !taskSummary.TextTruncated {
		t.Fatalf("status task does not disclose field truncation: %#v", taskSummary)
	}
	wantFailure := "RUN_INTERRUPTED: " + longReason
	wantTitleSummary := byteSafeSummary(t, longTitle, 256)
	wantFailureSummary := byteSafeSummary(t, wantFailure, 512)
	wantTerminalSummary := byteSafeSummary(t, longReason, 512)
	assertUTF8ByteBound(t, "status task title", taskSummary.Title, 256)
	assertUTF8ByteBound(t, "status task failure reason", taskSummary.FailureReason, 512)
	if taskSummary.Title != wantTitleSummary || taskSummary.FailureReason != wantFailureSummary {
		t.Fatalf("status task summaries are not deterministically clipped: %#v", taskSummary)
	}

	statusCommand := exec.Command(testBinaries.coordplane,
		"status", "--socket", socket, "--project", project.ID)
	statusHuman, err := statusCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("human status: %v\n%s", err, statusHuman)
	}
	for _, field := range []string{
		`"summary_truncated": true`,
		`"title_truncated": true`,
		`"text_truncated": true`,
	} {
		if !strings.Contains(string(statusHuman), field) {
			t.Errorf("human status does not disclose %q:\n%s", field, statusHuman)
		}
	}

	tasksRaw := runBinaryJSON(t, testBinaries.coordplane,
		"task", "list", "--socket", socket, "--project", project.ID, "--output", "json")
	var tasks core.TaskPage
	decodeJSON(t, tasksRaw, &tasks)
	if len(tasks.Items) != 1 || tasks.Items[0].ID != task.ID ||
		!tasks.Items[0].TitleTruncated || !tasks.Items[0].TextTruncated {
		t.Fatalf("task list does not disclose field truncation: %#v", tasks)
	}
	listedTask := tasks.Items[0]
	assertUTF8ByteBound(t, "task list title", listedTask.Title, 256)
	assertUTF8ByteBound(t, "task list failure reason", listedTask.FailureReason, 512)
	if listedTask.Title != taskSummary.Title || listedTask.FailureReason != taskSummary.FailureReason ||
		listedTask.TitleTruncated != taskSummary.TitleTruncated || listedTask.TextTruncated != taskSummary.TextTruncated {
		t.Fatalf("status and task list summaries disagree: status=%#v list=%#v", taskSummary, listedTask)
	}
	taskListCommand := exec.Command(testBinaries.coordplane,
		"task", "list", "--socket", socket, "--project", project.ID)
	taskListHuman, err := taskListCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("human task list: %v\n%s", err, taskListHuman)
	}
	for _, field := range []string{
		`"title_truncated": true`,
		`"text_truncated": true`,
	} {
		if !strings.Contains(string(taskListHuman), field) {
			t.Errorf("human task list does not disclose %q:\n%s", field, taskListHuman)
		}
	}

	runsRaw := runBinaryJSON(t, testBinaries.coordplane,
		"run", "list", "--socket", socket, "--project", project.ID, "--output", "json")
	var runs core.RunPage
	decodeJSON(t, runsRaw, &runs)
	if len(runs.Items) != 1 || runs.Items[0].ID != claim.Run.ID || !runs.Items[0].TextTruncated {
		t.Fatalf("run list does not disclose terminal text truncation: %#v", runs)
	}
	assertUTF8ByteBound(t, "run list terminal reason", runs.Items[0].TerminalReason, 512)
	if runs.Items[0].TerminalReason != wantTerminalSummary {
		t.Fatalf("run terminal summary is not deterministically clipped: %#v", runs.Items[0])
	}
	runCommand := exec.Command(testBinaries.coordplane,
		"run", "list", "--socket", socket, "--project", project.ID)
	runHuman, err := runCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("human run list: %v\n%s", err, runHuman)
	}
	for _, field := range []string{
		`"text_truncated": true`,
	} {
		if !strings.Contains(string(runHuman), field) {
			t.Errorf("human run list does not disclose %q:\n%s", field, runHuman)
		}
	}

	taskDetailRaw := runBinaryJSON(t, testBinaries.coordplane,
		"task", "show", task.ID, "--socket", socket, "--output", "json")
	var taskDetail core.TaskDetail
	decodeJSON(t, taskDetailRaw, &taskDetail)
	if taskDetail.Task.Status != core.TaskFailed || taskDetail.Task.Title != longTitle || taskDetail.Task.FailureReason != wantFailure {
		t.Fatalf("task show did not recover full fields: title=%d/%d failure=%d/%d",
			len(taskDetail.Task.Title), len(longTitle), len(taskDetail.Task.FailureReason), len(wantFailure))
	}
	runDetailRaw := runBinaryJSON(t, testBinaries.coordplane,
		"run", "show", claim.Run.ID, "--socket", socket, "--output", "json")
	var runDetail core.Run
	decodeJSON(t, runDetailRaw, &runDetail)
	if runDetail.State != core.RunInterrupted || runDetail.TerminalReason != longReason {
		t.Fatalf("run show did not recover full terminal reason: got %d bytes, want %d", len(runDetail.TerminalReason), len(longReason))
	}
}

func byteSafeSummary(t *testing.T, value string, limit int) string {
	t.Helper()
	var summary strings.Builder
	for _, current := range value {
		if summary.Len()+utf8.RuneLen(current) > limit {
			break
		}
		if _, err := summary.WriteRune(current); err != nil {
			t.Fatal(err)
		}
	}
	return summary.String()
}

func assertUTF8ByteBound(t *testing.T, name, value string, limit int) {
	t.Helper()
	if !utf8.ValidString(value) {
		t.Fatalf("%s is not valid UTF-8: %q", name, value)
	}
	if len(value) > limit {
		t.Fatalf("%s is %d bytes, exceeds %d-byte summary budget", name, len(value), limit)
	}
}
