package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"coordplane/internal/core"
)

type historyCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type eventCursor struct {
	BeforeID int64 `json:"before_id"`
}

const pageDataJSONBudget = 1 << 20

func (s *Store) Project(ctx context.Context, id string) (core.Project, error) {
	project, err := scanProject(s.db.QueryRowContext(ctx, projectSelect+` WHERE id=?`, id))
	return project, mapNotFound("project", id, err)
}

func (s *Store) ProjectsByStatus(ctx context.Context, statuses ...core.ProjectStatus) ([]core.Project, error) {
	query := projectSelect
	args := make([]any, 0, len(statuses))
	if len(statuses) > 0 {
		query += ` WHERE status IN (` + quotePlaceholders(len(statuses)) + `)`
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collectProjects(rows)
}

func (s *Store) Agent(ctx context.Context, id string) (core.Agent, error) {
	agent, err := scanAgent(s.db.QueryRowContext(ctx, agentSelect+` WHERE id=?`, id))
	return agent, mapNotFound("agent", id, err)
}

func (s *Store) Task(ctx context.Context, id string) (core.Task, error) {
	task, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE id=?`, id))
	return task, mapNotFound("task", id, err)
}

func (s *Store) Run(ctx context.Context, id string) (core.Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE id=?`, id))
	return run, mapNotFound("run", id, err)
}

// Snapshot reads all six durable object families from one SQLite snapshot.
func (s *Store) Snapshot(ctx context.Context, projectID string) (core.Snapshot, error) {
	return s.snapshot(ctx, strings.TrimSpace(projectID), nil)
}

// snapshot accepts a package-private barrier so the transaction boundary can
// be proven against a concurrent file-backed SQLite writer.
func (s *Store) snapshot(ctx context.Context, projectID string, afterProjects func()) (core.Snapshot, error) {
	var snapshot core.Snapshot
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	if snapshot, err = readSnapshot(ctx, tx, projectID, afterProjects); err != nil {
		return core.Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Snapshot{}, err
	}
	return snapshot, nil
}

func readSnapshot(ctx context.Context, tx *sql.Tx, projectID string, afterProjects func()) (core.Snapshot, error) {
	var snapshot core.Snapshot
	projectQuery := projectSelect
	var projectArgs []any
	if projectID != "" {
		projectQuery += ` WHERE id=?`
		projectArgs = append(projectArgs, projectID)
	}
	rows, err := tx.QueryContext(ctx, projectQuery+` ORDER BY created_at,id`, projectArgs...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Projects, err = collectProjects(rows); err != nil {
		return snapshot, err
	}
	if afterProjects != nil {
		afterProjects()
	}
	rows, err = tx.QueryContext(ctx, agentSelect+` ORDER BY created_at,id`)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Agents, err = collectAgents(rows); err != nil {
		return snapshot, err
	}
	taskQuery, runQuery, messageQuery, eventQuery := taskSelect, runSelect, messageSelect, eventSelect
	var args []any
	if projectID != "" {
		taskQuery += ` WHERE project_id=?`
		runQuery += ` WHERE project_id=?`
		messageQuery += ` WHERE project_id=?`
		eventQuery += ` WHERE project_id=?`
		args = []any{projectID}
	}
	rows, err = tx.QueryContext(ctx, taskQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Tasks, err = collectTasks(rows); err != nil {
		return snapshot, err
	}
	rows, err = tx.QueryContext(ctx, runQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Runs, err = collectRuns(rows); err != nil {
		return snapshot, err
	}
	rows, err = tx.QueryContext(ctx, messageQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Messages, err = collectMessages(rows); err != nil {
		return snapshot, err
	}
	rows, err = tx.QueryContext(ctx, eventQuery+` ORDER BY id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Events, err = collectEvents(rows); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// StatusProjection returns a fixed-size current view. Historical runs and
// messages remain available only through their cursor-paginated readers.
func (s *Store) StatusProjection(ctx context.Context, projectID string) (core.StatusProjection, error) {
	var projection core.StatusProjection
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return projection, err
	}
	defer tx.Rollback()
	projectID = strings.TrimSpace(projectID)

	projectQuery := projectSelect
	var projectArgs []any
	if projectID != "" {
		projectQuery += ` WHERE id=?`
		projectArgs = append(projectArgs, projectID)
	}
	projectQuery += ` ORDER BY created_at,id`
	if projectID == "" {
		projectQuery += ` LIMIT ?`
		projectArgs = append(projectArgs, core.StatusSnapshotLimit+1)
	}
	rows, err := tx.QueryContext(ctx, projectQuery, projectArgs...)
	if err != nil {
		return projection, err
	}
	projects, err := collectProjects(rows)
	if err != nil {
		return projection, err
	}
	if len(projects) > core.StatusSnapshotLimit {
		projects = projects[:core.StatusSnapshotLimit]
		projection.Truncated = true
	}
	projection.Snapshot.Projects = projects
	projectIDs := make([]any, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project.ID)
	}

	taskQuery := taskSelect + ` WHERE status NOT IN ('completed','cancelled')`
	var args []any
	if len(projectIDs) == 0 {
		taskQuery += ` AND 1=0`
	} else {
		taskQuery += ` AND project_id IN (` + quotePlaceholders(len(projectIDs)) + `)`
		args = append(args, projectIDs...)
	}
	rows, err = tx.QueryContext(ctx, taskQuery+` ORDER BY updated_at DESC,id DESC LIMIT ?`, append(args, core.StatusSnapshotLimit+1)...)
	if err != nil {
		return projection, err
	}
	tasks, err := collectTasks(rows)
	if err != nil {
		return projection, err
	}
	if len(tasks) > core.StatusSnapshotLimit {
		tasks = tasks[:core.StatusSnapshotLimit]
		projection.Truncated = true
	}

	currentRunIDs := make([]any, 0, len(tasks))
	requiredAgentIDs := make([]string, 0, len(tasks))
	seenAgents := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task.CurrentRunID != "" {
			currentRunIDs = append(currentRunIDs, task.CurrentRunID)
		}
		if !seenAgents[task.AssigneeAgentID] {
			seenAgents[task.AssigneeAgentID] = true
			requiredAgentIDs = append(requiredAgentIDs, task.AssigneeAgentID)
		}
	}
	var runs []core.Run
	if len(currentRunIDs) > 0 {
		rows, err = tx.QueryContext(ctx, runSelect+` WHERE id IN (`+quotePlaceholders(len(currentRunIDs))+`) ORDER BY created_at,id`, currentRunIDs...)
		if err != nil {
			return projection, err
		}
		runs, err = collectRuns(rows)
		if err != nil {
			return projection, err
		}
	}

	var agents []core.Agent
	if len(requiredAgentIDs) > 0 {
		requiredArgs := make([]any, len(requiredAgentIDs))
		for index := range requiredAgentIDs {
			requiredArgs[index] = requiredAgentIDs[index]
		}
		rows, err = tx.QueryContext(ctx, agentSelect+` WHERE id IN (`+quotePlaceholders(len(requiredArgs))+`) ORDER BY updated_at DESC,id DESC`, requiredArgs...)
		if err != nil {
			return projection, err
		}
		agents, err = collectAgents(rows)
		if err != nil {
			return projection, err
		}
	}
	remaining := core.StatusSnapshotLimit - len(agents)
	agentQuery := agentSelect + ` WHERE status<>'archived'`
	agentArgs := make([]any, 0, len(requiredAgentIDs)+1)
	if len(requiredAgentIDs) > 0 {
		agentQuery += ` AND id NOT IN (` + quotePlaceholders(len(requiredAgentIDs)) + `)`
		for _, id := range requiredAgentIDs {
			agentArgs = append(agentArgs, id)
		}
	}
	agentQuery += ` ORDER BY updated_at DESC,id DESC LIMIT ?`
	agentArgs = append(agentArgs, remaining+1)
	rows, err = tx.QueryContext(ctx, agentQuery, agentArgs...)
	if err != nil {
		return projection, err
	}
	extra, err := collectAgents(rows)
	if err != nil {
		return projection, err
	}
	if len(extra) > remaining {
		extra = extra[:remaining]
		projection.Truncated = true
	}
	agents = append(agents, extra...)
	projection.Snapshot.Agents = agents

	projection.Tasks, err = statusTaskViews(ctx, tx, tasks, runs)
	if err != nil {
		return core.StatusProjection{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.StatusProjection{}, err
	}
	return projection, nil
}

// TaskProjection joins one exact Task with its current Run, message counts,
// latest progress, and Project inside one SQLite read transaction.
func (s *Store) TaskProjection(ctx context.Context, id string) (core.TaskProjection, error) {
	var projection core.TaskProjection
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return projection, err
	}
	defer tx.Rollback()
	id = strings.TrimSpace(id)
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE id=?`, id))
	if err != nil {
		return projection, mapNotFound("task", id, err)
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=?`, task.ProjectID))
	if err != nil {
		return projection, mapNotFound("project", task.ProjectID, err)
	}
	var runs []core.Run
	if task.CurrentRunID != "" {
		run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE id=?`, task.CurrentRunID))
		if err != nil {
			return projection, mapNotFound("run", task.CurrentRunID, err)
		}
		runs = append(runs, run)
	}
	views, err := statusTaskViews(ctx, tx, []core.Task{task}, runs)
	if err != nil {
		return projection, err
	}
	view := views[0]
	detail := core.TaskDetail{
		Task: task, LatestProgress: view.LatestProgress,
		PendingMessageCount: view.PendingMessageCount, DeliveredMessageCount: view.DeliveredMessageCount,
		Derived: true,
	}
	if len(runs) == 1 {
		runCopy := runs[0]
		detail.CurrentRun = &runCopy
	}
	projection = core.TaskProjection{Project: project, Task: detail}
	if err := tx.Commit(); err != nil {
		return core.TaskProjection{}, err
	}
	return projection, nil
}

func statusTaskViews(ctx context.Context, tx *sql.Tx, tasks []core.Task, runs []core.Run) ([]core.TaskView, error) {
	views := make([]core.TaskView, len(tasks))
	if len(tasks) == 0 {
		return views, nil
	}
	runsByID := make(map[string]core.RunSummary, len(runs))
	for _, run := range runs {
		runsByID[run.ID] = runSummary(run)
	}
	counts := make(map[string][2]int, len(tasks))
	args := make([]any, 0, len(tasks))
	for _, task := range tasks {
		args = append(args, task.ID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT task_id,state,COUNT(*) FROM messages WHERE task_id IN (`+quotePlaceholders(len(tasks))+`) AND state IN ('pending','delivered') GROUP BY task_id,state`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var taskID string
		var state core.MessageState
		var count int
		if err := rows.Scan(&taskID, &state, &count); err != nil {
			rows.Close()
			return nil, err
		}
		value := counts[taskID]
		if state == core.MessagePending {
			value[0] = count
		} else {
			value[1] = count
		}
		counts[taskID] = value
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for index, task := range tasks {
		count := counts[task.ID]
		views[index] = core.TaskView{Task: taskSummary(task), PendingMessageCount: count[0], DeliveredMessageCount: count[1], Derived: true}
		if run, ok := runsByID[task.CurrentRunID]; ok {
			runCopy := run
			views[index].CurrentRun = &runCopy
		}
		progress, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE entity_type='task' AND entity_id=? AND kind='task.progress' ORDER BY id DESC LIMIT 1`, task.ID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			progressCopy := progress
			views[index].LatestProgress = &progressCopy
		}
	}
	return views, nil
}

func taskSummary(task core.Task) core.TaskSummary {
	title, truncated := boundedUTF8(task.Title, 256)
	waitReason, waitTruncated := boundedUTF8(task.WaitReason, 512)
	resultSummary, resultTruncated := boundedUTF8(task.ResultSummary, 512)
	failureReason, failureTruncated := boundedUTF8(task.FailureReason, 512)
	return core.TaskSummary{
		ID: task.ID, ProjectID: task.ProjectID, Kind: task.Kind, ParentTaskID: task.ParentTaskID,
		AssigneeAgentID: task.AssigneeAgentID, Title: title, TitleTruncated: truncated,
		Priority: task.Priority, Status: task.Status, CurrentRunID: task.CurrentRunID,
		Generation: task.Generation, NextRunAt: task.NextRunAt, RetryCount: task.RetryCount, MaxRetries: task.MaxRetries,
		WaitReason: waitReason, ResultSummary: resultSummary, FailureReason: failureReason,
		TextTruncated: waitTruncated || resultTruncated || failureTruncated,
		BaseSHA:       task.BaseSHA, HeadSHA: task.HeadSHA, TaskRef: task.TaskRef,
		AcceptedByKind: task.AcceptedByKind, AcceptedByID: task.AcceptedByID,
		AcceptedIntegrationAgentID: task.AcceptedIntegrationAgentID,
		FinalCanonicalSHA:          task.FinalCanonicalSHA, IntegrationTaskID: task.IntegrationTaskID,
		SourceTaskID: task.SourceTaskID, SourceRunID: task.SourceRunID,
		SourceTaskRef: task.SourceTaskRef, SourceHeadSHA: task.SourceHeadSHA,
		PendingAction: task.PendingAction, PendingActionID: task.PendingActionID,
		Version: task.Version, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		SubmittedAt: task.SubmittedAt, CompletedAt: task.CompletedAt, ClosedAt: task.ClosedAt,
	}
}

func runSummary(run core.Run) core.RunSummary {
	terminalReason, terminalTruncated := boundedUTF8(run.TerminalReason, 512)
	lastError, errorTruncated := boundedUTF8(run.LastError, 512)
	runtimeErrorCode, codeTruncated := boundedUTF8(run.RuntimeErrorCode, 256)
	return core.RunSummary{
		ID: run.ID, ProjectID: run.ProjectID, TaskID: run.TaskID, AgentID: run.AgentID,
		Generation: run.Generation, State: run.State, ContainerIDPresent: run.ContainerID != "",
		NativeSessionPresent: run.NativeSessionID != "", HeartbeatAt: run.HeartbeatAt,
		DeadlineAt: run.DeadlineAt, LastObservedAt: run.LastObservedAt,
		LaunchPhase: run.LaunchPhase, CleanupState: run.CleanupState,
		TerminalReason: terminalReason, LastError: lastError, RuntimeErrorCode: runtimeErrorCode,
		TextTruncated: terminalTruncated || errorTruncated || codeTruncated, Version: run.Version,
		CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
	}
}

func boundedUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func (s *Store) Projects(ctx context.Context, filter core.ProjectFilter) (core.ProjectPage, error) {
	limit, err := core.NormalizeCompactPageLimit(filter.Limit)
	if err != nil {
		return core.ProjectPage{}, err
	}
	cursor, err := decodeCursor(strings.TrimSpace(filter.Cursor))
	if err != nil {
		return core.ProjectPage{}, err
	}
	query, args := addCursor(projectSummarySelect+` WHERE 1=1`, nil, cursor)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.ProjectPage{}, err
	}
	items, err := collectProjectSummaries(rows)
	if err != nil {
		return core.ProjectPage{}, err
	}
	bounded, next, err := budgetPage(items, limit, func(item core.ProjectSummary) (string, string) { return item.CreatedAt, item.ID })
	if err != nil {
		return core.ProjectPage{}, err
	}
	return core.ProjectPage{Items: bounded, NextCursor: next}, nil
}

func (s *Store) Agents(ctx context.Context, filter core.AgentFilter) (core.AgentPage, error) {
	limit, err := core.NormalizeCompactPageLimit(filter.Limit)
	if err != nil {
		return core.AgentPage{}, err
	}
	cursor, err := decodeCursor(strings.TrimSpace(filter.Cursor))
	if err != nil {
		return core.AgentPage{}, err
	}
	query, args := addCursor(agentSummarySelect+` WHERE 1=1`, nil, cursor)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.AgentPage{}, err
	}
	items, err := collectAgentSummaries(rows)
	if err != nil {
		return core.AgentPage{}, err
	}
	bounded, next, err := budgetPage(items, limit, func(item core.AgentSummary) (string, string) { return item.CreatedAt, item.ID })
	if err != nil {
		return core.AgentPage{}, err
	}
	return core.AgentPage{Items: bounded, NextCursor: next}, nil
}

func (s *Store) Tasks(ctx context.Context, filter core.TaskFilter) (core.TaskPage, error) {
	limit, cursor, err := pageInputs(filter.Limit, filter.Cursor)
	if err != nil {
		return core.TaskPage{}, err
	}
	query := taskSummarySelect + ` WHERE 1=1`
	var args []any
	if projectID := strings.TrimSpace(filter.ProjectID); projectID != "" {
		query += ` AND project_id=?`
		args = append(args, projectID)
	}
	query, args = addCursor(query, args, cursor)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.TaskPage{}, err
	}
	items, err := collectTaskSummaries(rows)
	if err != nil {
		return core.TaskPage{}, err
	}
	bounded, next, err := budgetPage(items, limit, func(item core.TaskSummary) (string, string) { return item.CreatedAt, item.ID })
	if err != nil {
		return core.TaskPage{}, err
	}
	return core.TaskPage{Items: bounded, NextCursor: next}, nil
}

func (s *Store) Runs(ctx context.Context, filter core.RunFilter) (core.RunPage, error) {
	limit, cursor, err := pageInputs(filter.Limit, filter.Cursor)
	if err != nil {
		return core.RunPage{}, err
	}
	query := runSummarySelect + ` WHERE 1=1`
	var args []any
	for _, value := range []struct{ column, value string }{
		{"project_id", filter.ProjectID}, {"task_id", filter.TaskID}, {"agent_id", filter.AgentID},
	} {
		if trimmed := strings.TrimSpace(value.value); trimmed != "" {
			query += ` AND ` + value.column + `=?`
			args = append(args, trimmed)
		}
	}
	query, args = addCursor(query, args, cursor)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.RunPage{}, err
	}
	items, err := collectRunSummaries(rows)
	if err != nil {
		return core.RunPage{}, err
	}
	bounded, next, err := budgetPage(items, limit, func(item core.RunSummary) (string, string) { return item.CreatedAt, item.ID })
	if err != nil {
		return core.RunPage{}, err
	}
	return core.RunPage{Items: bounded, NextCursor: next}, nil
}

func (s *Store) Messages(ctx context.Context, filter core.MessageFilter) (core.MessagePage, error) {
	limit, err := core.NormalizeMessagePageLimit(filter.Limit)
	if err != nil {
		return core.MessagePage{}, err
	}
	cursor, err := decodeCursor(strings.TrimSpace(filter.Cursor))
	if err != nil {
		return core.MessagePage{}, err
	}
	query := messageSelect + ` WHERE 1=1`
	var args []any
	for _, value := range []struct{ column, value string }{
		{"project_id", filter.ProjectID}, {"task_id", filter.TaskID},
		{"recipient_kind", filter.RecipientKind}, {"recipient_id", filter.RecipientID},
	} {
		if trimmed := strings.TrimSpace(value.value); trimmed != "" {
			query += ` AND ` + value.column + `=?`
			args = append(args, trimmed)
		}
	}
	query, args = addCursor(query, args, cursor)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.MessagePage{}, err
	}
	items, err := collectMessages(rows)
	if err != nil {
		return core.MessagePage{}, err
	}
	bounded, next, err := budgetPage(items, limit, func(item core.Message) (string, string) { return item.CreatedAt, item.ID })
	if err != nil {
		return core.MessagePage{}, err
	}
	return core.MessagePage{Items: bounded, NextCursor: next}, nil
}

func budgetPage[T any](items []T, limit int, key func(T) (string, string)) ([]T, string, error) {
	candidateCount := len(items)
	if candidateCount > limit {
		candidateCount = limit
	}
	bounded := make([]T, 0, candidateCount)
	used := 64
	for _, item := range items[:candidateCount] {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, "", core.WrapError(core.CodeInternal, "encode history page item", false, err)
		}
		if len(bounded) > 0 && used+len(raw)+1 > pageDataJSONBudget {
			break
		}
		if len(bounded) == 0 && used+len(raw) > pageDataJSONBudget {
			return nil, "", core.NewError(core.CodeInternal, "history item exceeds response budget", false)
		}
		bounded = append(bounded, item)
		used += len(raw) + 1
	}
	hasMore := len(items) > len(bounded)
	if !hasMore {
		return bounded, "", nil
	}
	createdAt, id := key(bounded[len(bounded)-1])
	return bounded, encodeCursor(createdAt, id), nil
}

func pageInputs(limit int, rawCursor string) (int, historyCursor, error) {
	normalized, err := core.NormalizePageLimit(limit)
	if err != nil {
		return 0, historyCursor{}, err
	}
	cursor, err := decodeCursor(strings.TrimSpace(rawCursor))
	if err != nil {
		return 0, historyCursor{}, err
	}
	return normalized, cursor, nil
}

func addCursor(query string, args []any, cursor historyCursor) (string, []any) {
	if cursor.ID == "" {
		return query, args
	}
	query += ` AND (created_at>? OR (created_at=? AND id>?))`
	return query, append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
}

func encodeCursor(createdAt, id string) string {
	raw, _ := json.Marshal(historyCursor{CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(raw string) (historyCursor, error) {
	if raw == "" {
		return historyCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, invalidCursor()
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor historyCursor
	if err := decoder.Decode(&cursor); err != nil {
		return historyCursor{}, invalidCursor()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return historyCursor{}, invalidCursor()
	}
	if strings.TrimSpace(cursor.ID) == "" || strings.TrimSpace(cursor.CreatedAt) == "" {
		return historyCursor{}, invalidCursor()
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
		return historyCursor{}, invalidCursor()
	}
	return cursor, nil
}

func invalidCursor() error {
	return core.NewError(core.CodeInvalidArgument, "cursor is invalid", false)
}

func (s *Store) EventsPage(ctx context.Context, filter core.EventFilter) (core.EventPage, error) {
	limit, err := core.NormalizeEventPageLimit(filter.Limit)
	if err != nil {
		return core.EventPage{}, err
	}
	beforeID, err := decodeEventCursor(strings.TrimSpace(filter.Cursor))
	if err != nil {
		return core.EventPage{}, err
	}
	query, args := eventQuery(filter)
	if beforeID > 0 {
		query += ` AND id<?`
		args = append(args, beforeID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.EventPage{}, err
	}
	items, err := collectEvents(rows)
	if err != nil {
		return core.EventPage{}, err
	}
	bounded, next, err := budgetEventPage(items, limit)
	if err != nil {
		return core.EventPage{}, err
	}
	return core.EventPage{Items: bounded, NextCursor: next}, nil
}

func budgetEventPage(items []core.Event, limit int) ([]core.Event, string, error) {
	candidateCount := len(items)
	if candidateCount > limit {
		candidateCount = limit
	}
	bounded := make([]core.Event, 0, candidateCount)
	used := 64
	for _, item := range items[:candidateCount] {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, "", core.WrapError(core.CodeInternal, "encode event page item", false, err)
		}
		if len(bounded) > 0 && used+len(raw)+1 > pageDataJSONBudget {
			break
		}
		if len(bounded) == 0 && used+len(raw) > pageDataJSONBudget {
			return nil, "", core.NewError(core.CodeInternal, "event exceeds response budget", false)
		}
		bounded = append(bounded, item)
		used += len(raw) + 1
	}
	hasMore := len(items) > len(bounded)
	if hasMore {
		next := encodeEventCursor(bounded[len(bounded)-1].ID)
		reverseEvents(bounded)
		return bounded, next, nil
	}
	reverseEvents(bounded)
	return bounded, "", nil
}

func reverseEvents(events []core.Event) {
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
}

func encodeEventCursor(beforeID int64) string {
	raw, _ := json.Marshal(eventCursor{BeforeID: beforeID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeEventCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, invalidCursor()
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor eventCursor
	if err := decoder.Decode(&cursor); err != nil {
		return 0, invalidCursor()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || cursor.BeforeID <= 0 {
		return 0, invalidCursor()
	}
	return cursor.BeforeID, nil
}

func (s *Store) Events(ctx context.Context, filter core.EventFilter) ([]core.Event, error) {
	if strings.TrimSpace(filter.Cursor) != "" {
		page, err := s.EventsPage(ctx, filter)
		return page.Items, err
	}
	query, args := eventQuery(filter)
	if filter.Limit > 0 {
		query += ` ORDER BY id DESC LIMIT ?`
		args = append(args, filter.Limit)
	} else {
		query += ` ORDER BY id`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	events, err := collectEvents(rows)
	if err != nil {
		return nil, err
	}
	if filter.Limit > 0 {
		reverseEvents(events)
	}
	return events, nil
}

func eventQuery(filter core.EventFilter) (string, []any) {
	query := eventSelect + ` WHERE 1=1`
	var args []any
	if filter.ProjectID = strings.TrimSpace(filter.ProjectID); filter.ProjectID != "" {
		query += ` AND project_id=?`
		args = append(args, filter.ProjectID)
	}
	if filter.EntityType = strings.TrimSpace(filter.EntityType); filter.EntityType != "" {
		query += ` AND entity_type=?`
		args = append(args, filter.EntityType)
	}
	if filter.EntityID = strings.TrimSpace(filter.EntityID); filter.EntityID != "" {
		query += ` AND entity_id=?`
		args = append(args, filter.EntityID)
	}
	if filter.RunID = strings.TrimSpace(filter.RunID); filter.RunID != "" {
		query += ` AND run_id=?`
		args = append(args, filter.RunID)
	}
	if filter.Kind = strings.TrimSpace(filter.Kind); filter.Kind != "" {
		query += ` AND kind=?`
		args = append(args, filter.Kind)
	}
	return query, args
}

var _ core.Repository = (*Store)(nil)
