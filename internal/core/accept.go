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
	var snapshot Task
	var snapshotProject Project
	var replay Task
	var replayed bool
	selectedIntegrator := integrationAgentID
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
			replay, err = tx.Task(result.ID)
			replayed = err == nil
			return err
		}
		snapshot, err = tx.Task(taskID)
		if err != nil {
			return err
		}
		actor, err = s.authorizeTaskActor(tx, input.Token, snapshot, actor)
		if err != nil {
			return err
		}
		if actor.kind == "agent" && snapshot.ParentTaskID != actor.current.ID &&
			!(snapshot.CreatedByKind == "agent" && snapshot.CreatedByID == actor.id) {
			return NewError(CodeScopeDenied, "only the parent or creating Agent can accept this task", false)
		}
		if err := ValidateTaskOperation(snapshot.Kind, "accept"); err != nil {
			return err
		}
		if err := taskActionAvailable(snapshot); err != nil {
			return err
		}
		if snapshot.Status != TaskSubmitted {
			return Conflict(CodeInvalidState, "only a submitted task can be accepted", string(snapshot.Status), snapshot.Version)
		}
		if snapshot.HeadSHA == "" || snapshot.HeadRunID == "" || snapshot.TaskRef == "" {
			return NewError(CodeGitInvariantViolation, "submitted task has no complete captured result", false)
		}
		snapshotProject, err = tx.Project(snapshot.ProjectID)
		if err != nil {
			return err
		}
		if snapshotProject.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(snapshotProject.Status), snapshotProject.Version)
		}
		if selectedIntegrator == "" {
			selectedIntegrator = snapshotProject.IntegrationAgentID
		}
		if selectedIntegrator == "" {
			return NewError(CodeIntegrationAgentRequired, "an integration Agent is required", false)
		}
		integrator, err := tx.Agent(selectedIntegrator)
		if err != nil {
			return err
		}
		if integrator.Status != AgentActive {
			return Conflict(CodeInvalidState, "integration Agent is not active", string(integrator.Status), integrator.Version)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	if replayed {
		return replay, nil
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return Task{}, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	actualCanonical, err := s.projectGit.Resolve(ctx, snapshotProject.ControlRepoPath, snapshotProject.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve actual canonical ref", false, err)
	}
	actualTaskHead, err := executor.ResolveTaskRef(ctx, GitTaskRefIntent{
		ProjectID: snapshot.ProjectID, ControlRepo: snapshotProject.ControlRepoPath,
		TaskRef: snapshot.TaskRef, ExpectedSHA: snapshot.HeadSHA,
	})
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve captured task ref", false, err)
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
		if task.Version != snapshot.Version || task.TaskRef != snapshot.TaskRef || task.HeadSHA != snapshot.HeadSHA {
			return Conflict(CodeVersionConflict, "task changed while resolving Git truth", string(task.Status), task.Version)
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		if project.Version != snapshotProject.Version || project.ControlRepoPath != snapshotProject.ControlRepoPath || project.CanonicalRef != snapshotProject.CanonicalRef {
			return Conflict(CodeVersionConflict, "project changed while resolving Git truth", string(project.Status), project.Version)
		}
		integrationAgentID = selectedIntegrator
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
		task.PendingExpectedSHA = actualCanonical
		task.PendingTargetSHA = actualTaskHead
		task.PendingStartedAt = now
		task.Version++
		task.PendingActionVersion = task.Version
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, TaskSubmitted); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"integration_agent_id": integrationAgentID, "expected_sha": actualCanonical, "target_sha": actualTaskHead,
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
