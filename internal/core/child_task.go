package core

import (
	"context"
	"strings"
)

func (s *Service) CreateChildTask(ctx context.Context, input CreateChildTaskInput) (Task, error) {
	agentID, err := requireText("assignee_agent_id", input.AssigneeAgentID)
	if err != nil {
		return Task{}, err
	}
	title, err := requireText("title", input.Title)
	if err != nil {
		return Task{}, err
	}
	description, err := optionalTextWithin("description", input.Description, MaximumTaskDescriptionBytes)
	if err != nil {
		return Task{}, err
	}
	if input.MaxRetries < 0 {
		return Task{}, NewError(CodeInvalidArgument, "max_retries cannot be negative", false)
	}
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct {
		AgentID, Title, Description, AckIDs string
		Priority, MaxRetries                int
	}{agentID, title, description, strings.Join(ackIDs, "\x00"), input.Priority, input.MaxRetries})
	if err != nil {
		return Task{}, err
	}

	var project Project
	var child Task
	replayed := false
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		if raw, ok, err := tx.Dedupe("run:"+scope.ID, "task.create_child", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			child, err = tx.Task(result.ID)
			replayed = err == nil
			return err
		}
		_, parent, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		project, err = tx.Project(parent.ProjectID)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	if replayed {
		return child, nil
	}
	baseSHA, err := s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve canonical ref", false, err)
	}

	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		actorScope := "run:" + scope.ID
		if raw, ok, err := tx.Dedupe(actorScope, "task.create_child", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			child, err = tx.Task(result.ID)
			return err
		}
		run, parent, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		currentProject, err := tx.Project(parent.ProjectID)
		if err != nil {
			return err
		}
		if currentProject.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(currentProject.Status), currentProject.Version)
		}
		if currentProject.Version != project.Version || currentProject.CanonicalRef != project.CanonicalRef {
			return Conflict(CodeVersionConflict, "project changed while creating child task", string(currentProject.Status), currentProject.Version)
		}
		assignee, err := tx.Agent(agentID)
		if err != nil {
			return err
		}
		if assignee.Status == AgentArchived {
			return Conflict(CodeInvalidState, "archived agent cannot receive a task", string(assignee.Status), assignee.Version)
		}
		now := s.nowText()
		if _, err := s.acknowledgeAgentMessages(tx, ackIDs, parent.ProjectID, run.AgentID, run.ID, requestID, now); err != nil {
			return err
		}
		childID, err := s.requiredID("tsk")
		if err != nil {
			return err
		}
		child = Task{
			ID: childID, ProjectID: parent.ProjectID, Kind: TaskWork,
			ParentTaskID: parent.ID, CreatedByKind: "agent", CreatedByID: run.AgentID,
			AssigneeAgentID: agentID, Title: title, Description: description,
			Priority: input.Priority, Status: TaskQueued, NextRunAt: now,
			MaxRetries: input.MaxRetries, BaseSHA: baseSHA, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertTask(child); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"kind": child.Kind, "parent_task_id": parent.ID, "base_sha": baseSHA})
		if _, err := tx.AppendEvent(event(parent.ProjectID, "task", child.ID, "task.created", "agent", run.AgentID, run.ID, requestID, "", payload, now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(child.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe(actorScope, "task.create_child", requestID, raw, now)
	})
	return child, err
}
