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

func TestHumanRenderDisclosesPerItemTruncationAndExactRecoveryCommands(t *testing.T) {
	status := core.Status{
		DaemonReady: true,
		Runtime: &core.RuntimeStatus{
			WorkspaceQuotaEnabled: false,
			WorkspaceQuotaReason:  "not enabled: bind mount",
			TmpfsLimitBytes:       64 << 20,
		},
		SummaryTruncated: true,
		Tasks: []core.TaskView{
			{
				Task: core.TaskSummary{
					ID: "task-title", Title: "bounded title", TitleTruncated: true,
				},
				CurrentRun: &core.RunSummary{ID: "run-full"},
			},
			{
				Task: core.TaskSummary{
					ID: "task-text", Title: "full title", TextTruncated: true,
				},
				CurrentRun: &core.RunSummary{ID: "run-text", TextTruncated: true},
			},
			{Task: core.TaskSummary{ID: "task-full", Title: "full text"}},
		},
	}
	var output bytes.Buffer
	if err := render(&output, "human", status); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, field := range []string{
		"runtime_workspace_quota_enabled=false",
		"runtime_tmpfs_limit_bytes=67108864",
		"runtime_workspace_quota_reason=not enabled: bind mount",
		"task-title\t\t\tbounded title\trun=run-full",
		"title_truncated=true\ttask_text_truncated=false\trun_text_truncated=false",
		"task-text\t\t\tfull title\trun=run-text",
		"title_truncated=false\ttask_text_truncated=true\trun_text_truncated=true",
		`coordplane task show task-title --output json`,
		`coordplane task show task-text --output json`,
		`coordplane run show run-text --output json`,
		"task-full\t\t\tfull text\trun=",
	} {
		if !strings.Contains(text, field) {
			t.Errorf("status output lacks %q:\n%s", field, text)
		}
	}
	for _, command := range []string{
		"coordplane task show task-full --output json",
		"coordplane run show run-full --output json",
	} {
		if strings.Contains(text, command) {
			t.Errorf("status output includes unnecessary recovery command %q:\n%s", command, text)
		}
	}
	if !strings.Contains(text, "for omitted objects") || !strings.Contains(text, "item-specific show command") {
		t.Fatalf("generic summary hint confuses object pagination with field recovery:\n%s", text)
	}
}

func TestHumanListSummariesMarkTruncationAndHintOnlyFlaggedItems(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		want     []string
		unwanted []string
	}{
		{
			name: "task list",
			value: core.TaskPage{Items: []core.TaskSummary{
				{ID: "task-title", Title: "bounded", TitleTruncated: true},
				{ID: "task-text", Title: "full", TextTruncated: true},
				{ID: "task-full", Title: "full"},
			}},
			want: []string{
				"title_truncated=true\ttext_truncated=false",
				"title_truncated=false\ttext_truncated=true",
				"coordplane task show task-title --output json",
				"coordplane task show task-text --output json",
			},
			unwanted: []string{"coordplane task show task-full --output json"},
		},
		{
			name: "run list",
			value: core.RunPage{Items: []core.RunSummary{
				{ID: "run-text", TextTruncated: true},
				{ID: "run-full"},
			}},
			want: []string{
				"run-text\t\t\t\tgeneration=0\ttext_truncated=true",
				"run-full\t\t\t\tgeneration=0\ttext_truncated=false",
				"coordplane run show run-text --output json",
			},
			unwanted: []string{"coordplane run show run-full --output json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := render(&output, "human", test.value); err != nil {
				t.Fatal(err)
			}
			for _, value := range test.want {
				if !strings.Contains(output.String(), value) {
					t.Errorf("output lacks %q:\n%s", value, output.String())
				}
			}
			for _, value := range test.unwanted {
				if strings.Contains(output.String(), value) {
					t.Errorf("output includes %q:\n%s", value, output.String())
				}
			}
		})
	}
}

func TestHumanShowRowsDoNotClaimFullObjectsAreTruncated(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
	}{
		{name: "task", value: core.TaskDetail{Task: core.Task{ID: "task-full", Title: "complete"}}},
		{name: "run", value: core.Run{ID: "run-full", TerminalReason: "complete terminal reason"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := render(&output, "human", test.value); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "truncated=") || strings.Contains(output.String(), " show ") {
				t.Fatalf("full show row claims truncation:\n%s", output.String())
			}
		})
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
