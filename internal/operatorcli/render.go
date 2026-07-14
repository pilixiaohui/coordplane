package operatorcli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"coordplane/internal/core"
)

type projectStatusView struct {
	core.Project
	ActualCanonicalSHA   string `json:"actual_canonical_sha,omitempty"`
	ActualCanonicalError string `json:"actual_canonical_error,omitempty"`
}

func findAgent(agents []core.Agent, id string) (core.Agent, bool) {
	for _, agent := range agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return core.Agent{}, false
}

func projectViews(status core.Status) []projectStatusView {
	actual := make(map[string]core.GitState, len(status.ActualRefs))
	for _, state := range status.ActualRefs {
		actual[state.ProjectID] = state
	}
	views := make([]projectStatusView, 0, len(status.Snapshot.Projects))
	for _, project := range status.Snapshot.Projects {
		state := actual[project.ID]
		views = append(views, projectStatusView{
			Project: project, ActualCanonicalSHA: state.ActualSHA, ActualCanonicalError: state.Error,
		})
	}
	return views
}

func findProjectView(status core.Status, id string) (projectStatusView, bool) {
	for _, view := range projectViews(status) {
		if view.ID == id {
			return view, true
		}
	}
	return projectStatusView{}, false
}

func findTaskView(tasks []core.TaskView, id string) (core.TaskView, bool) {
	for _, task := range tasks {
		if task.Task.ID == id {
			return task, true
		}
	}
	return core.TaskView{}, false
}

func render(writer io.Writer, mode string, value any) error {
	if strings.EqualFold(strings.TrimSpace(mode), "json") {
		return json.NewEncoder(writer).Encode(value)
	}
	switch typed := value.(type) {
	case core.Status:
		runs, messages := 0, 0
		for _, task := range typed.Tasks {
			if task.CurrentRun != nil {
				runs++
			}
			messages += task.PendingMessageCount + task.DeliveredMessageCount
		}
		if _, err := fmt.Fprintf(writer, "ready=%t\tprojects_shown=%d\tagents_shown=%d\ttasks_shown=%d\truns_shown=%d\tmessages_for_shown_tasks=%d\tsummary_truncated=%t\n",
			typed.DaemonReady, len(typed.Snapshot.Projects), len(typed.Snapshot.Agents), len(typed.Tasks), runs, messages, typed.SummaryTruncated); err != nil {
			return err
		}
		if typed.Runtime != nil {
			if _, err := fmt.Fprintf(writer, "runtime_workspace_quota_enabled=%t\truntime_tmpfs_limit_bytes=%d\truntime_workspace_quota_reason=%s\n",
				typed.Runtime.WorkspaceQuotaEnabled, typed.Runtime.TmpfsLimitBytes, typed.Runtime.WorkspaceQuotaReason); err != nil {
				return err
			}
		}
		if typed.SummaryTruncated {
			if _, err := fmt.Fprintln(writer, `more=for omitted objects, run "coordplane project list", "coordplane agent list", or "coordplane task list" and follow next_cursor until empty; use each item-specific show command for truncated fields`); err != nil {
				return err
			}
		}
		for _, project := range projectViews(typed) {
			if err := renderProjectStatus(writer, project); err != nil {
				return err
			}
		}
		for _, task := range typed.Tasks {
			if err := renderTaskView(writer, task); err != nil {
				return err
			}
		}
		return nil
	case core.Project:
		return renderProject(writer, typed)
	case core.ProjectDetail:
		return renderProjectStatus(writer, projectStatusView{Project: typed.Project, ActualCanonicalSHA: typed.ActualCanonicalSHA, ActualCanonicalError: typed.ActualCanonicalError})
	case core.ProjectPage:
		for _, project := range typed.Items {
			if err := renderProjectSummary(writer, project); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case projectStatusView:
		return renderProjectStatus(writer, typed)
	case []core.Project:
		for _, project := range typed {
			if err := renderProject(writer, project); err != nil {
				return err
			}
		}
		return nil
	case []projectStatusView:
		for _, project := range typed {
			if err := renderProjectStatus(writer, project); err != nil {
				return err
			}
		}
		return nil
	case core.Agent:
		return renderAgent(writer, typed)
	case []core.Agent:
		for _, agent := range typed {
			if err := renderAgent(writer, agent); err != nil {
				return err
			}
		}
		return nil
	case core.AgentPage:
		for _, agent := range typed.Items {
			if err := renderAgentSummary(writer, agent); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case core.Task:
		return renderTask(writer, typed)
	case core.TaskView:
		return renderTaskView(writer, typed)
	case core.TaskDetail:
		return renderTaskDetail(writer, typed)
	case []core.Task:
		for _, task := range typed {
			if err := renderTask(writer, task); err != nil {
				return err
			}
		}
		return nil
	case core.TaskPage:
		for _, task := range typed.Items {
			if err := renderTaskSummary(writer, task); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case core.Run:
		return renderRun(writer, typed)
	case core.RunPage:
		for _, run := range typed.Items {
			if err := renderRunSummary(writer, run); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case core.ChatResult:
		return renderMessage(writer, typed.Message)
	case core.Message:
		return renderMessage(writer, typed)
	case []core.Message:
		for _, message := range typed {
			if err := renderMessage(writer, message); err != nil {
				return err
			}
		}
		return nil
	case core.MessagePage:
		for _, message := range typed.Items {
			if err := renderMessage(writer, message); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case core.EventPage:
		for _, event := range typed.Items {
			if err := renderEvent(writer, event); err != nil {
				return err
			}
		}
		return renderNextCursor(writer, typed.NextCursor)
	case core.Event:
		return renderEvent(writer, typed)
	case []core.Event:
		for _, event := range typed {
			if err := renderEvent(writer, event); err != nil {
				return err
			}
		}
		return nil
	default:
		return json.NewEncoder(writer).Encode(value)
	}
}

func renderProject(writer io.Writer, project core.Project) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", project.ID, project.Status, project.Name, project.CanonicalSHA)
	return err
}

func renderProjectStatus(writer io.Writer, project projectStatusView) error {
	actual := project.ActualCanonicalSHA
	if actual == "" {
		actual = "unknown"
	}
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", project.ID, project.Status, project.Name, actual, project.ActualCanonicalError)
	return err
}

func renderProjectSummary(writer io.Writer, project core.ProjectSummary) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", project.ID, project.Status, project.Name, project.CanonicalSHA)
	return err
}

func renderAgent(writer io.Writer, agent core.Agent) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", agent.ID, agent.Status, agent.DisplayName, agent.AdapterID)
	return err
}

func renderAgentSummary(writer io.Writer, agent core.AgentSummary) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", agent.ID, agent.Status, agent.DisplayName, agent.AdapterID)
	return err
}

func renderTask(writer io.Writer, task core.Task) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", task.ID, task.Kind, task.Status, task.AssigneeAgentID, task.Title)
	return err
}

func renderTaskSummary(writer io.Writer, task core.TaskSummary) error {
	if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\ttitle_truncated=%t\ttext_truncated=%t\n",
		task.ID, task.Kind, task.Status, task.AssigneeAgentID, task.Title, task.TitleTruncated, task.TextTruncated); err != nil {
		return err
	}
	return renderTaskTruncationHint(writer, task.ID, task.TitleTruncated || task.TextTruncated)
}

func renderTaskView(writer io.Writer, view core.TaskView) error {
	runID := ""
	runTextTruncated := false
	if view.CurrentRun != nil {
		runID = view.CurrentRun.ID
		runTextTruncated = view.CurrentRun.TextTruncated
	}
	if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\trun=%s\tpending=%d\tdelivered=%d\tcanonical=%s\tstale=%t\ttitle_truncated=%t\ttask_text_truncated=%t\trun_text_truncated=%t\n",
		view.Task.ID, view.Task.Status, view.Task.AssigneeAgentID, view.Task.Title, runID,
		view.PendingMessageCount, view.DeliveredMessageCount, view.ActualCanonicalSHA, view.Stale,
		view.Task.TitleTruncated, view.Task.TextTruncated, runTextTruncated); err != nil {
		return err
	}
	if err := renderTaskTruncationHint(writer, view.Task.ID, view.Task.TitleTruncated || view.Task.TextTruncated); err != nil {
		return err
	}
	return renderRunTruncationHint(writer, runID, runTextTruncated)
}

func renderTaskDetail(writer io.Writer, detail core.TaskDetail) error {
	runID := ""
	if detail.CurrentRun != nil {
		runID = detail.CurrentRun.ID
	}
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\trun=%s\tpending=%d\tdelivered=%d\tcanonical=%s\tstale=%t\n",
		detail.Task.ID, detail.Task.Status, detail.Task.AssigneeAgentID, detail.Task.Title, runID,
		detail.PendingMessageCount, detail.DeliveredMessageCount, detail.ActualCanonicalSHA, detail.Stale)
	return err
}

func renderMessage(writer io.Writer, message core.Message) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s:%s\t%s\n", message.ID, message.State, message.SenderKind, message.SenderID, message.Body)
	return err
}

func renderRun(writer io.Writer, run core.Run) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\tgeneration=%d\n", run.ID, run.State, run.TaskID, run.AgentID, run.Generation)
	return err
}

func renderRunSummary(writer io.Writer, run core.RunSummary) error {
	if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\tgeneration=%d\ttext_truncated=%t\n",
		run.ID, run.State, run.TaskID, run.AgentID, run.Generation, run.TextTruncated); err != nil {
		return err
	}
	return renderRunTruncationHint(writer, run.ID, run.TextTruncated)
}

func renderTaskTruncationHint(writer io.Writer, taskID string, truncated bool) error {
	if !truncated {
		return nil
	}
	_, err := fmt.Fprintf(writer, "more=run %q for full Task text\n", "coordplane task show "+taskID+" --output json")
	return err
}

func renderRunTruncationHint(writer io.Writer, runID string, truncated bool) error {
	if !truncated {
		return nil
	}
	_, err := fmt.Fprintf(writer, "more=run %q for full Run text\n", "coordplane run show "+runID+" --output json")
	return err
}

func renderNextCursor(writer io.Writer, cursor string) error {
	if cursor == "" {
		return nil
	}
	_, err := fmt.Fprintf(writer, "next_cursor=%s\n", cursor)
	return err
}

func renderEvent(writer io.Writer, event core.Event) error {
	_, err := fmt.Fprintf(writer, "%d\t%s\t%s:%s\n", event.ID, event.Kind, event.EntityType, event.EntityID)
	return err
}
