package store

import (
	"context"

	"coordplane/internal/core"
)

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

func (s *Store) Snapshot(ctx context.Context, projectID string) (core.Snapshot, error) {
	var snapshot core.Snapshot
	projectQuery := projectSelect
	var projectArgs []any
	if projectID != "" {
		projectQuery += ` WHERE id=?`
		projectArgs = append(projectArgs, projectID)
	}
	projectQuery += ` ORDER BY created_at,id`
	rows, err := s.db.QueryContext(ctx, projectQuery, projectArgs...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Projects, err = collectProjects(rows); err != nil {
		return snapshot, err
	}
	rows, err = s.db.QueryContext(ctx, agentSelect+` ORDER BY created_at,id`)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Agents, err = collectAgents(rows); err != nil {
		return snapshot, err
	}
	taskQuery, runQuery, messageQuery := taskSelect, runSelect, messageSelect
	var args []any
	if projectID != "" {
		taskQuery += ` WHERE project_id=?`
		runQuery += ` WHERE project_id=?`
		messageQuery += ` WHERE project_id=?`
		args = []any{projectID}
	}
	rows, err = s.db.QueryContext(ctx, taskQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Tasks, err = collectTasks(rows); err != nil {
		return snapshot, err
	}
	rows, err = s.db.QueryContext(ctx, runQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Runs, err = collectRuns(rows); err != nil {
		return snapshot, err
	}
	rows, err = s.db.QueryContext(ctx, messageQuery+` ORDER BY created_at,id`, args...)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Messages, err = collectMessages(rows); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *Store) Messages(ctx context.Context, filter core.MessageFilter) ([]core.Message, error) {
	query := messageSelect + ` WHERE 1=1`
	var args []any
	if filter.ProjectID != "" {
		query += ` AND project_id=?`
		args = append(args, filter.ProjectID)
	}
	if filter.TaskID != "" {
		query += ` AND task_id=?`
		args = append(args, filter.TaskID)
	}
	if filter.RecipientKind != "" {
		query += ` AND recipient_kind=?`
		args = append(args, filter.RecipientKind)
	}
	if filter.RecipientID != "" {
		query += ` AND recipient_id=?`
		args = append(args, filter.RecipientID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at,id`, args...)
	if err != nil {
		return nil, err
	}
	return collectMessages(rows)
}

func (s *Store) Events(ctx context.Context, filter core.EventFilter) ([]core.Event, error) {
	query := eventSelect + ` WHERE 1=1`
	var args []any
	if filter.ProjectID != "" {
		query += ` AND project_id=?`
		args = append(args, filter.ProjectID)
	}
	if filter.EntityType != "" {
		query += ` AND entity_type=?`
		args = append(args, filter.EntityType)
	}
	if filter.EntityID != "" {
		query += ` AND entity_id=?`
		args = append(args, filter.EntityID)
	}
	if filter.RunID != "" {
		query += ` AND run_id=?`
		args = append(args, filter.RunID)
	}
	if filter.Kind != "" {
		query += ` AND kind=?`
		args = append(args, filter.Kind)
	}
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
		for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
			events[left], events[right] = events[right], events[left]
		}
	}
	return events, nil
}

var _ core.Repository = (*Store)(nil)
