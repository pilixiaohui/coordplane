package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"coordplane/internal/core"
)

func TestSnapshotUsesOneFileBackedSQLiteReadTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	reader, err := Open(ctx, path)
	requireNoError(t, err)
	defer reader.Close()
	writer, err := Open(ctx, path)
	requireNoError(t, err)
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
	requireNoError(t, err)
	if got := snapshotFamilySizes(after); got != [6]int{1, 1, 1, 1, 1, 2} {
		t.Fatalf("next snapshot did not see the complete committed view: %v", got)
	}
}

func TestTaskRunAndMessageHistoryUseStableOpaqueCursorPages(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "pages.db"))
	requireNoError(t, err)
	defer database.Close()
	for _, suffix := range []string{"c", "a", "b"} {
		requireNoError(t, insertReadFixture(ctx, database, suffix, core.TaskCompleted, core.RunExited, core.MessageAcknowledged, false))
	}
	projects1, err := database.Projects(ctx, core.ProjectFilter{Limit: 2})
	requireNoError(t, err)
	projects2, err := database.Projects(ctx, core.ProjectFilter{Limit: 2, Cursor: projects1.NextCursor})
	requireNoError(t, err)
	assertIDs(t, projectIDs(append(projects1.Items, projects2.Items...)), []string{"prj_a", "prj_b", "prj_c"})

	agents1, err := database.Agents(ctx, core.AgentFilter{Limit: 2})
	requireNoError(t, err)
	agents2, err := database.Agents(ctx, core.AgentFilter{Limit: 2, Cursor: agents1.NextCursor})
	requireNoError(t, err)
	assertIDs(t, agentIDs(append(agents1.Items, agents2.Items...)), []string{"agt_a", "agt_b", "agt_c"})

	tasks1, err := database.Tasks(ctx, core.TaskFilter{Limit: 2})
	requireNoError(t, err)
	tasks2, err := database.Tasks(ctx, core.TaskFilter{Limit: 2, Cursor: tasks1.NextCursor})
	requireNoError(t, err)
	assertIDs(t, taskIDs(tasks1.Items), []string{"tsk_a", "tsk_b"})
	assertIDs(t, taskIDs(tasks2.Items), []string{"tsk_c"})
	if tasks1.NextCursor == "" || strings.Contains(tasks1.NextCursor, "2026-") || tasks2.NextCursor != "" {
		t.Fatalf("task cursors are not opaque/terminal: first=%q second=%q", tasks1.NextCursor, tasks2.NextCursor)
	}

	runs1, err := database.Runs(ctx, core.RunFilter{Limit: 2})
	requireNoError(t, err)
	runs2, err := database.Runs(ctx, core.RunFilter{Limit: 2, Cursor: runs1.NextCursor})
	requireNoError(t, err)
	assertIDs(t, runIDs(append(runs1.Items, runs2.Items...)), []string{"run_a", "run_b", "run_c"})

	messages1, err := database.Messages(ctx, core.MessageFilter{Limit: 2})
	requireNoError(t, err)
	messages2, err := database.Messages(ctx, core.MessageFilter{Limit: 2, Cursor: messages1.NextCursor})
	requireNoError(t, err)
	assertIDs(t, messageIDs(append(messages1.Items, messages2.Items...)), []string{"msg_a", "msg_b", "msg_c"})

	if _, err := database.Tasks(ctx, core.TaskFilter{Cursor: "created-at-and-id-are-not-public"}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("invalid cursor error = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := database.Messages(ctx, core.MessageFilter{Limit: core.MessagePageLimit + 1}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized message page error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestTaskHasStartedRunSearchesBeyondTheFirstHundredHistoryRows(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "run-history.db"))
	requireNoError(t, err)
	defer database.Close()

	const (
		projectID = "prj_run_history"
		agentID   = "agt_run_history"
		taskID    = "tsk_run_history"
		otherTask = "tsk_without_started_run"
	)
	if err := database.Transact(ctx, func(tx core.Transaction) error {
		if err := tx.InsertProject(core.Project{
			ID: projectID, Name: "Run history", Source: "/source", SourceRef: "refs/heads/main",
			InitialSHA: strings.Repeat("a", 40), ControlRepoPath: "/control/run-history.git",
			CanonicalRef: "refs/heads/main", CanonicalSHA: strings.Repeat("a", 40),
			Status: core.ProjectActive, Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		}); err != nil {
			return err
		}
		if err := tx.InsertAgent(core.Agent{
			ID: agentID, DisplayName: "Run history", AdapterID: "test", Image: "test:latest",
			InstructionsFile: "/instructions", Status: core.AgentActive,
			Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
		}); err != nil {
			return err
		}
		for _, id := range []string{taskID, otherTask} {
			if err := tx.InsertTask(core.Task{
				ID: id, ProjectID: projectID, Kind: core.TaskWork, CreatedByKind: "boss",
				AssigneeAgentID: agentID, Title: id, Status: core.TaskFailed, NextRunAt: readTestTime,
				Version: 1, CreatedAt: readTestTime, UpdatedAt: readTestTime,
			}); err != nil {
				return err
			}
		}
		for index := 0; index <= core.MaximumCompactPageLimit+1; index++ {
			phase := core.LaunchIntent
			containerID := ""
			if index == core.MaximumCompactPageLimit+1 {
				phase = core.LaunchStartIssued
				containerID = "container-after-first-page"
			}
			createdAt := fmt.Sprintf("2026-07-12T00:00:00.%09dZ", index)
			if err := tx.InsertRun(core.Run{
				ID: fmt.Sprintf("run_history_%03d", index), ProjectID: projectID,
				TaskID: taskID, AgentID: agentID, Generation: int64(index + 1),
				AdapterID: "test", Image: "test:latest", State: core.RunFailed,
				ContainerID: containerID, TokenHash: fmt.Sprintf("token-history-%03d", index),
				CleanupState: core.CleanupNotNeeded, LaunchNonce: fmt.Sprintf("nonce-%03d", index),
				LaunchOperationID: fmt.Sprintf("operation-%03d", index), LaunchPhase: phase,
				ContainerName: fmt.Sprintf("container-history-%03d", index), LaunchMode: "start",
				Version: 1, CreatedAt: createdAt, EndedAt: createdAt,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	firstPage, err := database.Runs(ctx, core.RunFilter{TaskID: taskID, Limit: core.MaximumCompactPageLimit})
	requireNoError(t, err)
	if len(firstPage.Items) != core.MaximumCompactPageLimit || firstPage.NextCursor == "" {
		t.Fatalf("first Run page = %d rows, next=%q", len(firstPage.Items), firstPage.NextCursor)
	}
	for _, run := range firstPage.Items {
		if run.LaunchPhase == core.LaunchStartIssued || run.LaunchPhase == core.LaunchProcessObserved {
			t.Fatalf("started Run unexpectedly appeared in first page: %#v", run)
		}
	}
	started, err := database.TaskHasStartedRun(ctx, taskID)
	if err != nil || !started {
		t.Fatalf("TaskHasStartedRun(%s) = %t, err=%v", taskID, started, err)
	}
	started, err = database.TaskHasStartedRun(ctx, otherTask)
	if err != nil || started {
		t.Fatalf("TaskHasStartedRun(%s) = %t, err=%v", otherTask, started, err)
	}
}

func TestEventHistoryUsesOpaqueIDCursorWithoutGapsOrDuplicates(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "event-pages.db"))
	requireNoError(t, err)
	defer database.Close()
	payload, err := json.Marshal(map[string]string{"text": strings.Repeat("\x01", 4000)})
	requireNoError(t, err)
	const eventCount = 47
	if err := database.Transact(ctx, func(tx core.Transaction) error {
		for index := 1; index <= eventCount; index++ {
			if _, err := tx.AppendEvent(core.Event{
				ProjectID: "prj_events", EntityType: "daemon", EntityID: "daemon",
				Kind: fmt.Sprintf("test.event.%02d", index), ActorKind: "daemon",
				PayloadJSON: string(payload), CreatedAt: readTestTime,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cursor := ""
	seen := make(map[int64]bool, eventCount)
	var previousOldest int64
	for pageNumber := 0; pageNumber < eventCount; pageNumber++ {
		page, err := database.EventsPage(ctx, core.EventFilter{
			ProjectID: "prj_events", Cursor: cursor, Limit: core.MaximumEventPageLimit,
		})
		requireNoError(t, err)
		if len(page.Items) == 0 {
			t.Fatalf("event page %d made no cursor progress", pageNumber)
		}
		raw, err := json.Marshal(page)
		requireNoError(t, err)
		if len(raw) >= pageDataJSONBudget {
			t.Fatalf("event page %d exceeded JSON budget: %d", pageNumber, len(raw))
		}
		for index, event := range page.Items {
			if index > 0 && page.Items[index-1].ID >= event.ID {
				t.Fatalf("event page %d is not ascending: %#v", pageNumber, page.Items)
			}
			if seen[event.ID] {
				t.Fatalf("event ID %d appeared more than once", event.ID)
			}
			seen[event.ID] = true
		}
		oldest := page.Items[0].ID
		newest := page.Items[len(page.Items)-1].ID
		if previousOldest > 0 && newest >= previousOldest {
			t.Fatalf("page %d did not move to older IDs: newest=%d previous_oldest=%d", pageNumber, newest, previousOldest)
		}
		previousOldest = oldest
		if page.NextCursor == "" {
			cursor = ""
			break
		}
		if page.NextCursor == fmt.Sprint(oldest) {
			t.Fatalf("event cursor exposed the keyset ID: %q", page.NextCursor)
		}
		cursor = page.NextCursor
	}
	if cursor != "" || len(seen) != eventCount {
		t.Fatalf("event traversal cursor=%q count=%d, want %d", cursor, len(seen), eventCount)
	}
	for id := int64(1); id <= eventCount; id++ {
		if !seen[id] {
			t.Fatalf("event traversal omitted ID %d", id)
		}
	}
	if _, err := database.EventsPage(ctx, core.EventFilter{Cursor: "not-an-event-cursor"}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("invalid event cursor error = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := database.EventsPage(ctx, core.EventFilter{Limit: core.MaximumEventPageLimit + 1}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized event page error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestStatusProjectionIsBoundedAndOmitsHistoricalPayloads(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "status.db"))
	requireNoError(t, err)
	defer database.Close()
	requireNoError(t, insertReadFixture(ctx, database, "zzzz", core.TaskQueued, core.RunStarting, core.MessagePending, true))
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
	requireNoError(t, err)

	projection, err := database.StatusProjection(ctx, "prj_zzzz")
	requireNoError(t, err)
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

func TestTaskAndRunSummariesUseConsistentUTF8ByteBudgets(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "utf8-summaries.db"))
	requireNoError(t, err)
	defer database.Close()
	requireNoError(t, insertReadFixture(ctx, database, "utf8", core.TaskRunning, core.RunActive, core.MessagePending, false))

	title := strings.Repeat("题", 100)
	waitReason := strings.Repeat("等", 200)
	resultSummary := strings.Repeat("果", 200)
	failure := strings.Repeat("败", 200)
	terminal := strings.Repeat("终", 200)
	lastError := strings.Repeat("错", 200)
	runtimeErrorCode := strings.Repeat("码", 100)
	if _, err := database.db.ExecContext(ctx, `UPDATE tasks SET title=?,wait_reason=?,result_summary=?,failure_reason=? WHERE id='tsk_utf8'`, title, waitReason, resultSummary, failure); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE runs SET terminal_reason=?,last_error=?,runtime_error_code=? WHERE id='run_utf8'`, terminal, lastError, runtimeErrorCode); err != nil {
		t.Fatal(err)
	}

	status, err := database.StatusProjection(ctx, "prj_utf8")
	requireNoError(t, err)
	if status.Truncated || len(status.Tasks) != 1 || status.Tasks[0].CurrentRun == nil {
		t.Fatalf("status object bound/current run = truncated:%t tasks:%d view:%#v", status.Truncated, len(status.Tasks), status.Tasks)
	}
	tasks, err := database.Tasks(ctx, core.TaskFilter{ProjectID: "prj_utf8"})
	requireNoError(t, err)
	runs, err := database.Runs(ctx, core.RunFilter{ProjectID: "prj_utf8"})
	requireNoError(t, err)
	if len(tasks.Items) != 1 || len(runs.Items) != 1 {
		t.Fatalf("list object counts = tasks:%d runs:%d", len(tasks.Items), len(runs.Items))
	}

	statusTask, listTask := status.Tasks[0].Task, tasks.Items[0]
	statusRun, listRun := *status.Tasks[0].CurrentRun, runs.Items[0]
	if statusTask != listTask {
		t.Fatalf("status/list task summaries disagree: status=%#v list=%#v", statusTask, listTask)
	}
	if statusRun != listRun {
		t.Fatalf("status/list run summaries disagree: status=%#v list=%#v", statusRun, listRun)
	}
	assertUTF8Budget(t, "task title", listTask.Title, 256, 255)
	assertUTF8Budget(t, "task wait reason", listTask.WaitReason, 512, 510)
	assertUTF8Budget(t, "task result summary", listTask.ResultSummary, 512, 510)
	assertUTF8Budget(t, "task failure", listTask.FailureReason, 512, 510)
	assertUTF8Budget(t, "run terminal", listRun.TerminalReason, 512, 510)
	assertUTF8Budget(t, "run last error", listRun.LastError, 512, 510)
	assertUTF8Budget(t, "run runtime error code", listRun.RuntimeErrorCode, 256, 255)
	if !listTask.TitleTruncated || !listTask.TextTruncated || !listRun.TextTruncated {
		t.Fatalf("truncation flags = title:%t task_text:%t run_text:%t", listTask.TitleTruncated, listTask.TextTruncated, listRun.TextTruncated)
	}
	fullTask, err := database.Task(ctx, "tsk_utf8")
	requireNoError(t, err)
	fullRun, err := database.Run(ctx, "run_utf8")
	requireNoError(t, err)
	if fullTask.Title != title || fullTask.WaitReason != waitReason || fullTask.ResultSummary != resultSummary || fullTask.FailureReason != failure {
		t.Fatalf("full task read did not preserve UTF-8 text: %#v", fullTask)
	}
	if fullRun.TerminalReason != terminal || fullRun.LastError != lastError || fullRun.RuntimeErrorCode != runtimeErrorCode {
		t.Fatalf("full run read did not preserve UTF-8 text: %#v", fullRun)
	}
}

func assertUTF8Budget(t *testing.T, field, value string, limit, wantBytes int) {
	t.Helper()
	if !utf8.ValidString(value) || len(value) != wantBytes || len(value) > limit {
		t.Fatalf("%s summary = %d bytes valid_utf8:%t, want %d valid bytes within %d-byte budget", field, len(value), utf8.ValidString(value), wantBytes, limit)
	}
}

func TestStatusProjectionMarksAgentsOmittedAfterRequiredSlotsFillTheBound(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "status-agents.db"))
	requireNoError(t, err)
	defer database.Close()
	requireNoError(t, insertReadFixture(ctx, database, "agent_bound", core.TaskCompleted, core.RunExited, core.MessageAcknowledged, false))
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
	requireNoError(t, err)

	projection, err := database.StatusProjection(ctx, "prj_agent_bound")
	requireNoError(t, err)
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
