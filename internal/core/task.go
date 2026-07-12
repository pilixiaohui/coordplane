package core

import (
	"context"
	"strings"
)

func (s *Service) CreateTask(ctx context.Context, input CreateTaskInput) (Task, error) {
	projectID, err := requireText("project_id", input.ProjectID)
	if err != nil {
		return Task{}, err
	}
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
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	if input.Kind == "" {
		input.Kind = TaskWork
	}
	if input.Kind != TaskWork {
		return Task{}, NewError(CodeInvalidArgument, "Boss task create currently accepts kind=work", false)
	}
	if input.MaxRetries < 0 {
		return Task{}, NewError(CodeInvalidArgument, "max_retries cannot be negative", false)
	}
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ProjectID, AgentID, Kind, Title, Description, AckIDs string
		Priority, MaxRetries                                 int
	}{projectID, agentID, string(input.Kind), title, description, strings.Join(ackIDs, "\x00"), input.Priority, input.MaxRetries})
	if err != nil {
		return Task{}, err
	}
	project, err := s.repository.Project(ctx, projectID)
	if err != nil {
		return Task{}, err
	}
	if project.Status != ProjectActive {
		return Task{}, Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
	}
	baseSHA, err := s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve canonical ref", false, err)
	}

	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", "task.create", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			task, err = tx.Task(result.ID)
			return err
		}
		currentProject, err := tx.Project(projectID)
		if err != nil {
			return err
		}
		if currentProject.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(currentProject.Status), currentProject.Version)
		}
		agent, err := tx.Agent(agentID)
		if err != nil {
			return err
		}
		if agent.Status == AgentArchived {
			return Conflict(CodeInvalidState, "archived agent cannot receive a task", string(agent.Status), agent.Version)
		}
		now := s.nowText()
		if err := s.acknowledgeForActor(tx, ackIDs, projectID, taskMutationActor{kind: "boss"}, requestID, now); err != nil {
			return err
		}
		taskID, err := s.requiredID("tsk")
		if err != nil {
			return err
		}
		task = Task{
			ID: taskID, ProjectID: projectID, Kind: TaskWork, CreatedByKind: "boss",
			AssigneeAgentID: agentID, Title: title, Description: description,
			Priority: input.Priority, Status: TaskQueued, Generation: 0, NextRunAt: now,
			MaxRetries: input.MaxRetries, BaseSHA: baseSHA, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertTask(task); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"kind": task.Kind, "base_sha": task.BaseSHA})
		if _, err := tx.AppendEvent(event(projectID, "task", task.ID, "task.created", "boss", "", "", requestID, "", payload, now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(task.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe("boss", "task.create", requestID, raw, now)
	})
	return task, err
}

func (s *Service) CloseConversation(ctx context.Context, taskID, requestID string) (Task, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Task{}, err
	}
	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		task, err = tx.Task(strings.TrimSpace(taskID))
		if err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, "close"); err != nil {
			return Conflict(CodeInvalidState, "task kind cannot be closed", string(task.Status), task.Version)
		}
		if err := taskActionAvailable(task); err != nil {
			return err
		}
		if err := ValidateTaskTransition(task.Kind, task.Status, TaskCompleted); err != nil {
			return Conflict(CodeInvalidState, "conversation is not waiting", string(task.Status), task.Version)
		}
		now := s.nowText()
		messages, err := tx.MessagesForTask(task.ID)
		if err != nil {
			return err
		}
		for _, message := range messages {
			if message.RecipientKind != "agent" || (message.State != MessagePending && message.State != MessageDelivered) {
				continue
			}
			expectedVersion, expectedState := message.Version, message.State
			message.State = MessageCancelled
			message.LastDeliveryError = "conversation closed by Boss"
			message.Version++
			if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(task.ProjectID, "message", message.ID, "message.cancelled", "boss", "", "", requestID, "", eventPayload(map[string]any{"reason": "conversation_closed"}), now)); err != nil {
				return err
			}
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = TaskCompleted
		task.CompletedAt = now
		task.ClosedAt = now
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.completed", "boss", "", "", requestID, "", "{}", now))
		return err
	})
	return task, err
}
