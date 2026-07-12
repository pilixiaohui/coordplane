package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestSnapshotUsesOneFileBackedSQLiteReadTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	reader, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	reachedBarrier := make(chan struct{})
	releaseReader := make(chan struct{})
	type result struct {
		snapshot core.Snapshot
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		snapshot, err := reader.snapshot(ctx, "", func() {
			close(reachedBarrier)
			<-releaseReader
		})
		resultCh <- result{snapshot: snapshot, err: err}
	}()

	select {
	case <-reachedBarrier:
	case <-ctx.Done():
		t.Fatal("snapshot did not reach the post-project barrier")
	}
	writeErr := insertReadFixture(ctx, writer, "after", core.TaskQueued, core.RunStarting, core.MessagePending, true)
	close(releaseReader)
	if writeErr != nil {
		t.Fatal(writeErr)
	}

	var during result
	select {
	case during = <-resultCh:
	case <-ctx.Done():
		t.Fatal("snapshot did not return after the writer committed")
	}
	if during.err != nil {
		t.Fatal(during.err)
	}
	if got := snapshotFamilySizes(during.snapshot); got != [6]int{} {
		t.Fatalf("snapshot mixed a post-barrier commit into the old view: %v", got)
	}
	after, err := reader.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotFamilySizes(after); got != [6]int{1, 1, 1, 1, 1, 2} {
		t.Fatalf("next snapshot did not see the complete committed view: %v", got)
	}
}

func TestTaskRunAndMessageHistoryUseStableOpaqueCursorPages(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, suffix := range []string{"c", "a", "b"} {
		if err := insertReadFixture(ctx, database, suffix, core.TaskCompleted, core.RunExited, core.MessageAcknowledged, false); err != nil {
			t.Fatal(err)
		}
	}
	projects1, err := database.Projects(ctx, core.ProjectFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	projects2, err := database.Projects(ctx, core.ProjectFilter{Limit: 2, Cursor: projects1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, projectIDs(append(projects1.Items, projects2.Items...)), []string{"prj_a", "prj_b", "prj_c"})

	agents1, err := database.Agents(ctx, core.AgentFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	agents2, err := database.Agents(ctx, core.AgentFilter{Limit: 2, Cursor: agents1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, agentIDs(append(agents1.Items, agents2.Items...)), []string{"agt_a", "agt_b", "agt_c"})

	tasks1, err := database.Tasks(ctx, core.TaskFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	tasks2, err := database.Tasks(ctx, core.TaskFilter{Limit: 2, Cursor: tasks1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, taskIDs(tasks1.Items), []string{"tsk_a", "tsk_b"})
	assertIDs(t, taskIDs(tasks2.Items), []string{"tsk_c"})
	if tasks1.NextCursor == "" || strings.Contains(tasks1.NextCursor, "2026-") || tasks2.NextCursor != "" {
		t.Fatalf("task cursors are not opaque/terminal: first=%q second=%q", tasks1.NextCursor, tasks2.NextCursor)
	}

	runs1, err := database.Runs(ctx, core.RunFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	runs2, err := database.Runs(ctx, core.RunFilter{Limit: 2, Cursor: runs1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, runIDs(append(runs1.Items, runs2.Items...)), []string{"run_a", "run_b", "run_c"})

	messages1, err := database.Messages(ctx, core.MessageFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	messages2, err := database.Messages(ctx, core.MessageFilter{Limit: 2, Cursor: messages1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, messageIDs(append(messages1.Items, messages2.Items...)), []string{"msg_a", "msg_b", "msg_c"})

	if _, err := database.Tasks(ctx, core.TaskFilter{Cursor: "created-at-and-id-are-not-public"}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("invalid cursor error = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := database.Messages(ctx, core.MessageFilter{Limit: core.MessagePageLimit + 1}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized message page error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestStatusProjectionIsBoundedAndOmitsHistoricalPayloads(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := insertReadFixture(ctx, database, "zzzz", core.TaskQueued, core.RunStarting, core.MessagePending, true); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE tasks SET title=? WHERE id='tsk_zzzz'`, strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	err = database.Transact(ctx, func(tx core.Transaction) error {
		for index := 0; index < core.StatusSnapshotLimit; index++ {
			id := fmt.Sprintf("tsk_%03d", index)
			if err := tx.InsertTask(core.Task{
				ID: id, ProjectID: "prj_zzzz", Kind: core.TaskWork, CreatedByKind: "boss",
				AssigneeAgentID: "agt_zzzz", Title: id, Status: core.TaskWaiting,
				NextRunAt: readTestTime, Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	projection, err := database.StatusProjection(ctx, "prj_zzzz")
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tasks) != core.StatusSnapshotLimit {
		t.Fatalf("status task count = %d, want hard limit %d", len(projection.Tasks), core.StatusSnapshotLimit)
	}
	if len(projection.Snapshot.Tasks) != 0 || len(projection.Snapshot.Runs) != 0 || len(projection.Snapshot.Messages) != 0 || len(projection.Snapshot.Events) != 0 {
		t.Fatalf("status embedded history payloads: %#v", projection.Snapshot)
	}
	var current *core.TaskView
	for index := range projection.Tasks {
		if projection.Tasks[index].Task.ID == "tsk_zzzz" {
			current = &projection.Tasks[index]
			break
		}
	}
	if current == nil {
		t.Fatal("status omitted the recently updated current task")
	}
	if current.CurrentRun == nil || current.CurrentRun.ID != "run_zzzz" || current.PendingMessageCount != 1 || current.LatestProgress == nil {
		t.Fatalf("current task projection lost live facts: %#v", current)
	}
	if !current.Task.TitleTruncated || len(current.Task.Title) != 256 {
		t.Fatalf("task title summary was not explicitly bounded: %#v", current.Task)
	}
}

func TestStatusProjectionMarksAgentsOmittedAfterRequiredSlotsFillTheBound(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "status-agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := insertReadFixture(ctx, database, "agent_bound", core.TaskCompleted, core.RunExited, core.MessageAcknowledged, false); err != nil {
		t.Fatal(err)
	}
	err = database.Transact(ctx, func(tx core.Transaction) error {
		for index := 0; index < core.StatusSnapshotLimit; index++ {
			id := fmt.Sprintf("agt_required_%02d", index)
			if err := tx.InsertAgent(core.Agent{
				ID: id, DisplayName: id, AdapterID: "test", Image: "test:latest",
				InstructionsFile: "/instructions", Status: core.AgentActive,
				Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
			}); err != nil {
				return err
			}
			if err := tx.InsertTask(core.Task{
				ID: fmt.Sprintf("tsk_required_%02d", index), ProjectID: "prj_agent_bound",
				Kind: core.TaskWork, CreatedByKind: "boss", AssigneeAgentID: id,
				Title: id, Status: core.TaskWaiting, NextRunAt: readTestTime,
				Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
			}); err != nil {
				return err
			}
		}
		return tx.InsertAgent(core.Agent{
			ID: "agt_omitted", DisplayName: "omitted", AdapterID: "test", Image: "test:latest",
			InstructionsFile: "/instructions", Status: core.AgentActive,
			Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	projection, err := database.StatusProjection(ctx, "prj_agent_bound")
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tasks) != core.StatusSnapshotLimit || len(projection.Snapshot.Agents) != core.StatusSnapshotLimit {
		t.Fatalf("status bounds = tasks:%d agents:%d", len(projection.Tasks), len(projection.Snapshot.Agents))
	}
	if !projection.Truncated {
		t.Fatal("status omitted an additional active agent without reporting truncation")
	}
	for _, agent := range projection.Snapshot.Agents {
		if agent.ID == "agt_omitted" {
			t.Fatalf("status displaced a required assignee with the extra agent: %#v", projection.Snapshot.Agents)
		}
	}
}

const readTestTime = "2026-07-12T00:00:00.000000000Z"

func insertReadFixture(ctx context.Context, database *Store, suffix string, taskStatus core.TaskStatus, runState core.RunState, messageState core.MessageState, progress bool) error {
	projectID, agentID, taskID, runID, messageID := "prj_"+suffix, "agt_"+suffix, "tsk_"+suffix, "run_"+suffix, "msg_"+suffix
	currentRunID := ""
	if core.IsRunLive(runState) {
		currentRunID = runID
	}
	return database.Transact(ctx, func(tx core.Transaction) error {
		if err := tx.InsertProject(core.Project{
			ID: projectID, Name: projectID, Source: "/source/" + suffix,
			SourceRef: "refs/heads/main", InitialSHA: strings.Repeat("a", 40),
			ControlRepoPath: "/control/" + suffix, CanonicalRef: "refs/heads/main",
			CanonicalSHA: strings.Repeat("a", 40), Status: core.ProjectActive,
			Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if err := tx.InsertAgent(core.Agent{
			ID: agentID, DisplayName: agentID, AdapterID: "test", Image: "test:latest",
			InstructionsFile: "/instructions", Status: core.AgentActive,
			Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if err := tx.InsertTask(core.Task{
			ID: taskID, ProjectID: projectID, Kind: core.TaskWork, CreatedByKind: "boss",
			AssigneeAgentID: agentID, Title: taskID, Status: taskStatus, CurrentRunID: currentRunID,
			NextRunAt: readTestTime, Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if err := tx.InsertRun(core.Run{
			ID: runID, ProjectID: projectID, TaskID: taskID, AgentID: agentID, Generation: 1,
			AdapterID: "test", Image: "test:latest", State: runState, TokenHash: "token-" + suffix,
			CleanupState: "not_needed", LaunchPhase: "intent", ContainerName: "container-" + suffix,
			LaunchMode: "start", Version: 1, CreatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if err := tx.InsertMessage(core.Message{
			ID: messageID, ProjectID: projectID, TaskID: taskID, SenderKind: "boss",
			RecipientKind: "agent", RecipientID: agentID, Body: "body-" + suffix,
			State: messageState, NextDeliveryAt: readTestTime, IdempotencyKey: "request-" + suffix,
			Version: 1, CreatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(core.Event{
			ProjectID: projectID, EntityType: "task", EntityID: taskID, Kind: "task.created",
			ActorKind: "boss", RequestID: "request-" + suffix, PayloadJSON: "{}", CreatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if progress {
			_, err := tx.AppendEvent(core.Event{
				ProjectID: projectID, EntityType: "task", EntityID: taskID, Kind: "task.progress",
				ActorKind: "agent", ActorID: agentID, RunID: runID,
				RequestID: "progress-" + suffix, PayloadJSON: `{"summary":"ready"}`, CreatedAt: readTestTime,
			})
			return err
		}
		return nil
	})
}

func snapshotFamilySizes(snapshot core.Snapshot) [6]int {
	return [6]int{len(snapshot.Projects), len(snapshot.Agents), len(snapshot.Tasks), len(snapshot.Runs), len(snapshot.Messages), len(snapshot.Events)}
}

func projectIDs(items []core.ProjectSummary) []string {
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	return ids
}

func agentIDs(items []core.AgentSummary) []string {
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	return ids
}

func taskIDs(items []core.TaskSummary) []string {
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	return ids
}

func runIDs(items []core.RunSummary) []string {
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	return ids
}

func messageIDs(items []core.Message) []string {
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	return ids
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
}
