package core

import (
	"context"
	"strings"
)

func (s *Service) RequestAccept(ctx context.Context, input AcceptInput) (Task, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return Task{}, err
	}
	integrationAgentID := strings.TrimSpace(input.IntegrationAgentID)
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, IntegrationAgentID, AckIDs string }{
		taskID, integrationAgentID, strings.Join(ackIDs, "\x00"),
	})
	if err != nil {
		return Task{}, err
	}
	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		actor, err := s.taskActorForDedupe(tx, input.Token)
		if err != nil {
			return err
		}
		if raw, ok, err := tx.Dedupe(actor.dedupeScope, "task.accept", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			task, err = tx.Task(result.ID)
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
		if actor.kind == "agent" && task.ParentTaskID != actor.current.ID &&
			!(task.CreatedByKind == "agent" && task.CreatedByID == actor.id) {
			return NewError(CodeScopeDenied, "only the parent or creating Agent can accept this task", false)
		}
		if err := ValidateTaskOperation(task.Kind, "accept"); err != nil {
			return err
		}
		if err := taskActionAvailable(task); err != nil {
			return err
		}
		if task.Status != TaskSubmitted {
			return Conflict(CodeInvalidState, "only a submitted task can be accepted", string(task.Status), task.Version)
		}
		if task.HeadSHA == "" || task.HeadRunID == "" || task.TaskRef == "" {
			return NewError(CodeGitInvariantViolation, "submitted task has no complete captured result", false)
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		if integrationAgentID == "" {
			integrationAgentID = project.IntegrationAgentID
		}
		if integrationAgentID == "" {
			return NewError(CodeIntegrationAgentRequired, "an integration Agent is required", false)
		}
		integrator, err := tx.Agent(integrationAgentID)
		if err != nil {
			return err
		}
		if integrator.Status != AgentActive {
			return Conflict(CodeInvalidState, "integration Agent is not active", string(integrator.Status), integrator.Version)
		}
		now := s.nowText()
		if err := s.acknowledgeForActor(tx, ackIDs, task.ProjectID, actor, requestID, now); err != nil {
			return err
		}
		operationID, err := s.requiredID("op")
		if err != nil {
			return err
		}
		expectedVersion := task.Version
		task.AcceptedByKind = actor.kind
		task.AcceptedByID = actor.id
		task.AcceptedAt = now
		task.AcceptedIntegrationAgentID = integrationAgentID
		task.PendingAction = "advance"
		task.PendingActionID = operationID
		task.PendingActionRunID = task.HeadRunID
		task.PendingExpectedSHA = project.CanonicalSHA
		task.PendingTargetSHA = task.HeadSHA
		task.PendingStartedAt = now
		task.Version++
		task.PendingActionVersion = task.Version
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, TaskSubmitted); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"integration_agent_id": integrationAgentID, "expected_sha": project.CanonicalSHA, "target_sha": task.HeadSHA,
		})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.accept_requested", actor.kind, actor.id, actor.runID, requestID, operationID, payload, now)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.canonical_advance_requested", actor.kind, actor.id, actor.runID, requestID, operationID, payload, now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(task.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe(actor.dedupeScope, "task.accept", requestID, raw, now)
	})
	return task, err
}
