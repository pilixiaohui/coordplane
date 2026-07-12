package operatorcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestStatusHumanRenderDisclosesBoundedProjection(t *testing.T) {
	status := truncatedStatusFixture()
	var output bytes.Buffer
	if err := render(&output, "human", status); err != nil {
		t.Fatal(err)
	}

	firstLine, _, _ := strings.Cut(output.String(), "\n")
	for _, field := range []string{
		"projects_shown=1",
		"agents_shown=8",
		"tasks_shown=8",
		"runs_shown=0",
		"messages_for_shown_tasks=0",
		"summary_truncated=true",
	} {
		if !strings.Contains(firstLine, field) {
			t.Errorf("status header %q does not disclose %q", firstLine, field)
		}
	}
	if strings.Contains(firstLine, "\tprojects=") || strings.Contains(firstLine, "\tagents=") || strings.Contains(firstLine, "\ttasks=") {
		t.Errorf("status header presents bounded counts as totals: %q", firstLine)
	}
	for _, hint := range []string{"coordplane project list", "coordplane agent list", "coordplane task list", "next_cursor"} {
		if !strings.Contains(output.String(), hint) {
			t.Errorf("truncated status output lacks continuation hint %q:\n%s", hint, output.String())
		}
	}
}

func TestStatusJSONRenderRemainsCanonical(t *testing.T) {
	status := truncatedStatusFixture()
	want, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	var output bytes.Buffer
	if err := render(&output, "json", status); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("JSON render changed\n got: %s\nwant: %s", output.Bytes(), want)
	}
}

func truncatedStatusFixture() core.Status {
	status := core.Status{
		DaemonReady:      true,
		SummaryTruncated: true,
		Snapshot: core.Snapshot{
			Projects: []core.Project{{ID: "project-1", Name: "Project", Status: core.ProjectActive}},
		},
	}
	for index := 0; index < core.StatusSnapshotLimit; index++ {
		agentID := fmt.Sprintf("agent-%02d", index)
		status.Snapshot.Agents = append(status.Snapshot.Agents, core.Agent{
			ID: agentID, DisplayName: agentID, Status: core.AgentActive,
		})
		status.Tasks = append(status.Tasks, core.TaskView{Task: core.TaskSummary{
			ID: fmt.Sprintf("task-%02d", index), ProjectID: "project-1",
			AssigneeAgentID: agentID, Status: core.TaskQueued,
		}})
	}
	return status
}
