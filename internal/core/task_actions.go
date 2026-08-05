package core

import (
	"context"
	"strings"
)

type taskMutationActor struct {
	kind        string
	id          string
	runID       string
	dedupeScope string
	run         Run
	current     Task
}

type taskMutation func(Transaction, *Task, taskMutationActor, string, string) error

func (s *Service) WakeTask(ctx context.Context, input TaskActionInput) (Task, error) {
	return s.applyTaskMutation(ctx, input, "task.wake", func(tx Transaction, task *Task, actor taskMutationActor, requestID, now string) error {
		if err := taskActionAvailable(*task); err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, "wake"); err != nil {
			return err
		}
		if task.Status != TaskWaiting {
			return Conflict(CodeInvalidState, "only a waiting task can be woken", string(task.Status), task.Version)
		}
		messages, err := tx.MessagesForTask(task.ID)
		if err != nil {
			return err
		}
		for _, message := range messages {
			if message.RecipientKind != "agent" || message.State != MessagePending ||
				message.MaxDeliveries == 0 || message.DeliveryCount < message.MaxDeliveries {
				continue
			}
			expectedMessageVersion := message.Version
			message.DeliveryCount = 0
			message.NextDeliveryAt = now
			message.LastDeliveryError = ""
			message.Version++
			if err := tx.UpdateMessage(message, expectedMessageVersion, MessagePending); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"reason": "task_wake"})
			if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.retry_enabled", actor.kind, actor.id, actor.runID, requestID, "", payload, now)); err != nil {
				return err
			}
		}
		expectedVersion := task.Version
		task.Status = TaskQueued
		task.NextRunAt = now
		task.WaitReason = ""
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, TaskWaiting); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.requeued", actor.kind, actor.id, actor.runID, requestID, "", eventPayload(map[string]any{"reason": "explicit_wake"}), now))
		return err
	})
}

func (s *Service) RetryTask(ctx context.Context, input TaskActionInput) (Task, error) {
	return s.applyTaskMutation(ctx, input, "task.retry", func(tx Transaction, task *Task, actor taskMutationActor, requestID, now string) error {
		if err := taskActionAvailable(*task); err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, "retry"); err != nil {
			return err
		}
		if task.Status != TaskFailed {
			return Conflict(CodeInvalidState, "only a failed task can be retried", string(task.Status), task.Version)
		}
		expectedVersion := task.Version
		task.Status = TaskQueued
		task.CurrentRunID = ""
		task.Generation++
		task.NextRunAt = now
		task.FailureReason = ""
		clearTaskAction(task)
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, TaskFailed); err != nil {
			return err
		}
		_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.requeued", actor.kind, actor.id, actor.runID, requestID, "", eventPayload(map[string]any{"reason": "explicit_retry"}), now))
		return err
	})
}

func (s *Service) CancelTask(ctx context.Context, input TaskActionInput) (Task, error) {
	return s.applyTaskMutation(ctx, input, "task.cancel", func(tx Transaction, task *Task, actor taskMutationActor, requestID, now string) error {
		if err := taskActionAvailable(*task); err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, "cancel"); err != nil {
			return err
		}
		if err := ValidateTaskTransition(task.Kind, task.Status, TaskCancelled); err != nil {
			return Conflict(CodeInvalidState, "task cannot be cancelled", string(task.Status), task.Version)
		}
		if task.Kind == TaskIntegration && task.SourceTaskID != "" {
			source, err := tx.Task(task.SourceTaskID)
			if err != nil {
				return err
			}
			if source.PendingAction != "" {
				return Conflict(CodeActionInProgress, "source task action is in progress", string(source.Status), source.Version)
			}
			if err := integrationSourceFence(source, *task); err != nil {
				return err
			}
			expectedSourceVersion := source.Version
			source.IntegrationTaskID = ""
			source.AcceptedByKind = ""
			source.AcceptedByID = ""
			source.AcceptedAt = ""
			source.AcceptedIntegrationAgentID = ""
			source.Version++
			source.UpdatedAt = now
			if err := tx.UpdateTask(source, expectedSourceVersion, TaskSubmitted); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"integration_task_id": task.ID})
			if _, err := tx.AppendEvent(event(source.ProjectID, "task", source.ID, "git.integration_released", actor.kind, actor.id, actor.runID, requestID, "", payload, now)); err != nil {
				return err
			}
		}
		preserveCurrentRun := false
		if task.CurrentRunID != "" {
			run, err := tx.Run(task.CurrentRunID)
			if err != nil {
				return err
			}
			if IsRunLive(run.State) {
				preserveCurrentRun = true
				operationID := run.StopOperationID
				changed := false
				if run.StopRequestedAt == "" {
					operationID, err = s.requiredID("op")
					if err != nil {
						return err
					}
					run.StopRequestedAt = now
					run.StopReason = boundedDurableText(input.Reason, MaximumTerminalTextBytes)
					run.StopOperationID = operationID
					changed = true
				}
				if operationID == "" {
					operationID, err = s.requiredID("op")
					if err != nil {
						return err
					}
					run.StopOperationID = operationID
					changed = true
				}
				if run.TokenRevokedAt == "" {
					run.TokenRevokedAt = now
					changed = true
				}
				if changed {
					expectedVersion, expectedState := run.Version, run.State
					run.Version++
					if err := tx.UpdateRun(run, expectedVersion, expectedState); err != nil {
						return err
					}
				}
				payload := eventPayload(map[string]any{
					"reason": run.StopReason, "cancel_reason": boundedDurableText(input.Reason, MaximumTerminalTextBytes),
					"task_cancelled": true,
				})
				if _, err := tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.stop_requested", actor.kind, actor.id, run.ID, requestID, operationID, payload, now)); err != nil {
					return err
				}
			}
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = TaskCancelled
		if !preserveCurrentRun {
			task.CurrentRunID = ""
		}
		task.Generation++
		task.ClosedAt = now
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"reason": boundedDurableText(input.Reason, MaximumOutcomeTextBytes)})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.cancelled", actor.kind, actor.id, actor.runID, requestID, "", payload, now)); err != nil {
			return err
		}
		if err := s.disposeUnresolvedMessages(tx, *task, actor.kind, actor.id, actor.runID, requestID, now); err != nil {
			return err
		}
		return s.notifyParentOfChild(tx, *task, actor.kind, actor.id, actor.runID, requestID, now)
	})
}

func (s *Service) ReworkTask(ctx context.Context, input TaskActionInput) (Task, error) {
	return s.applyTaskMutation(ctx, input, "task.rework", func(tx Transaction, task *Task, actor taskMutationActor, requestID, now string) error {
		if err := ValidateTaskOperation(task.Kind, "rework"); err != nil {
			return err
		}
		if err := taskActionAvailable(*task); err != nil {
			return err
		}
		if actor.kind == "agent" && task.ParentTaskID != actor.current.ID &&
			!(task.CreatedByKind == "agent" && task.CreatedByID == actor.id) {
			return NewError(CodeScopeDenied, "only the parent or creating Agent can rework this task", false)
		}
		if task.Status != TaskSubmitted {
			return Conflict(CodeInvalidState, "only a submitted task can be reworked", string(task.Status), task.Version)
		}
		expectedVersion := task.Version
		task.Status = TaskQueued
		task.NextRunAt = now
		task.AcceptedByKind = ""
		task.AcceptedByID = ""
		task.AcceptedAt = ""
		task.AcceptedIntegrationAgentID = ""
		clearTaskAction(task)
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, TaskSubmitted); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"reason": boundedDurableText(input.Reason, MaximumOutcomeTextBytes)})
		_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.requeued", actor.kind, actor.id, actor.runID, requestID, "", payload, now))
		return err
	})
}

func (s *Service) applyTaskMutation(ctx context.Context, input TaskActionInput, operation string, mutate taskMutation) (Task, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return Task{}, err
	}
	input.Reason, err = optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return Task{}, err
	}
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, Reason, AckIDs string }{taskID, input.Reason, strings.Join(ackIDs, "\x00")})
	if err != nil {
		return Task{}, err
	}
	dedupe := requestDedupe{"", operation, requestID, inputHash}
	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		actor, err := s.taskActorForDedupe(tx, input.Token)
		if err != nil {
			return err
		}
		dedupe.scope = actor.dedupeScope
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			task, err = tx.Task(replay.ID)
			return err
		}
		task, err = tx.Task(taskID)
		if err != nil {
			return err
		}
		actor, err = s.authorizeTaskActor(tx, input.Token, task, actor)
		if err != nil {
			return err
		}
		now := s.nowText()
		if err := s.acknowledgeForActor(tx, ackIDs, task.ProjectID, actor, requestID, now); err != nil {
			return err
		}
		if err := mutate(tx, &task, actor, requestID, now); err != nil {
			return err
		}
		return dedupe.record(tx, task.ID, "", now)
	})
	return task, err
}

func (s *Service) taskActorForDedupe(tx Transaction, token string) (taskMutationActor, error) {
	if strings.TrimSpace(token) == "" {
		return taskMutationActor{kind: "boss", dedupeScope: "boss"}, nil
	}
	run, err := scopedRun(tx, token)
	if err != nil {
		return taskMutationActor{}, err
	}
	return taskMutationActor{kind: "agent", id: run.AgentID, runID: run.ID, dedupeScope: "run:" + run.ID, run: run}, nil
}

func (s *Service) authorizeTaskActor(tx Transaction, token string, target Task, actor taskMutationActor) (taskMutationActor, error) {
	if actor.kind == "boss" {
		return actor, nil
	}
	run, current, err := s.authenticateRun(tx, token)
	if err != nil {
		return taskMutationActor{}, err
	}
	if !runCanReadTask(run, current, target) {
		return taskMutationActor{}, NewError(CodeScopeDenied, "task is outside the current run scope", false)
	}
	actor.run, actor.current, actor.id, actor.runID = run, current, run.AgentID, run.ID
	return actor, nil
}

func (s *Service) acknowledgeForActor(tx Transaction, ids []string, projectID string, actor taskMutationActor, requestID, now string) error {
	if actor.kind == "agent" {
		_, err := s.acknowledgeAgentMessages(tx, ids, projectID, actor.id, actor.runID, requestID, now)
		return err
	}
	for _, id := range ids {
		message, err := tx.Message(id)
		if err != nil {
			return err
		}
		if message.ProjectID != projectID || message.RecipientKind != "boss" {
			return NewError(CodeScopeDenied, "message is outside the Boss action scope", false)
		}
		if message.State == MessageAcknowledged {
			continue
		}
		if message.State != MessagePending && message.State != MessageDelivered {
			return Conflict(CodeInvalidState, "message cannot be acknowledged", string(message.State), message.Version)
		}
		expectedVersion, expectedState := message.Version, message.State
		message.State = MessageAcknowledged
		message.AcknowledgedAt = now
		message.Version++
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(projectID, "message", message.ID, "message.acknowledged", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
	}
	return nil
}
