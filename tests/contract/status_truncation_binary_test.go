package contract_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
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
			"--adapter", "one-shot", "--image", "agent:latest",
			"--instructions-file", filepath.Join(root, "agent.md"),
			"--request-id", fmt.Sprintf("status-agent-%02d", index), "--output", "json")
		var agent core.Agent
		decodeJSON(t, raw, &agent)
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
	header, _, _ := strings.Cut(string(humanOutput), "\n")
	for _, field := range []string{
		"projects_shown=1",
		fmt.Sprintf("agents_shown=%d", core.StatusSnapshotLimit),
		fmt.Sprintf("tasks_shown=%d", core.StatusSnapshotLimit),
		"summary_truncated=true",
	} {
		if !strings.Contains(header, field) {
			t.Errorf("status header %q does not disclose %q", header, field)
		}
	}
	for _, hint := range []string{"coordplane agent list", "coordplane task list", "next_cursor"} {
		if !strings.Contains(string(humanOutput), hint) {
			t.Errorf("truncated status output lacks continuation hint %q:\n%s", hint, humanOutput)
		}
	}
}
