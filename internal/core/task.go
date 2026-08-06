package core

import (
	"context"
	"strings"
)

func (s *Service) CreateTask(ctx context.Context, input CreateTaskInput) (Task, error) {
	if input.Kind == "" {
		input.Kind = TaskWork
	}
	if input.Kind != TaskWork {
		return Task{}, NewError(CodeInvalidArgument, "Boss task create currently accepts kind=work", false)
	}
	projectID, err := requireText("project_id", input.ProjectID)
	if err != nil {
		return Task{}, err
	}
	request, err := s.normalizeWorkTask(input.AssigneeAgentID, input.AssigneeParticipantID, input.Title, input.Description,
		input.SourceTaskID, input.Priority, input.MaxRetries, input.BudgetSeconds, input.AckMessageIDs, input.RequestID)
	if err != nil {
		return Task{}, err
	}
	request.projectID, request.retryOfTaskID = projectID, strings.TrimSpace(input.RetryOfTaskID)
	inputHash, err := inputFingerprint(struct {
		ProjectID, AgentID, Kind, Title, Description, SourceTaskID, RetryOfTaskID, AckIDs string
		Priority, MaxRetries                                                              int
		BudgetSeconds                                                                     int64
	}{projectID, request.agentID, string(input.Kind), request.title, request.description,
		request.sourceTaskID, request.retryOfTaskID, strings.Join(request.ackIDs, "\x00"), request.priority, request.maxRetries,
		request.budgetSeconds})
	if err != nil {
		return Task{}, err
	}
	request.dedupe = requestDedupe{"boss", "task.create", request.requestID, inputHash}
	return s.createWorkTask(ctx, request)
}

func validateSourceTask(projectID string, source Task) error {
	if source.ProjectID != projectID {
		return NewError(CodeScopeDenied, "source task is outside the target project", false)
	}
	if source.Status != TaskSubmitted && source.Status != TaskCompleted {
		return Conflict(CodeInvalidState, "source task must be submitted or completed", string(source.Status), source.Version)
	}
	if source.HeadSHA == "" || source.HeadRunID == "" || source.TaskRef == "" {
		return NewError(CodeGitInvariantViolation, "source task has no complete captured result", false)
	}
	return nil
}

func validateRetryTarget(projectID string, target Task) error {
	if target.ProjectID != projectID {
		return NewError(CodeScopeDenied, "retry target is outside the target project", false)
	}
	if target.Status != TaskCompleted && target.Status != TaskCancelled {
		return Conflict(CodeInvalidState, "retry target must be completed or cancelled", string(target.Status), target.Version)
	}
	return nil
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
