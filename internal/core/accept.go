package core

import (
	"context"
	"strings"
)

type acceptFacts struct {
	task       Task
	project    Project
	integrator Agent
	actor      taskMutationActor
}

func (s *Service) RequestAccept(ctx context.Context, input AcceptInput) (Task, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return Task{}, err
	}
	selectedIntegrator := strings.TrimSpace(input.IntegrationAgentID)
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, IntegrationAgentID, AckIDs string }{
		taskID, selectedIntegrator, strings.Join(ackIDs, "\x00"),
	})
	if err != nil {
		return Task{}, err
	}
	dedupe := requestDedupe{"", "task.accept", requestID, inputHash}
	var snapshot acceptFacts
	var task Task
	replayed := false
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
			replayed = err == nil
			return err
		}
		snapshot, err = s.acceptFacts(tx, input.Token, taskID, selectedIntegrator, actor)
		return err
	})
	if err != nil || replayed {
		return task, err
	}
	selectedIntegrator = snapshot.integrator.ID
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return Task{}, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	actualCanonical, err := s.projectGit.Resolve(ctx, snapshot.project.ControlRepoPath, snapshot.project.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve actual canonical ref", false, err)
	}
	actualTaskHead, err := executor.ResolveTaskRef(ctx, GitTaskRefIntent{
		ProjectID: snapshot.task.ProjectID, ControlRepo: snapshot.project.ControlRepoPath,
		TaskRef: snapshot.task.TaskRef, ExpectedSHA: snapshot.task.HeadSHA,
	})
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve captured task ref", false, err)
	}
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
		facts, err := s.acceptFacts(tx, input.Token, taskID, selectedIntegrator, actor)
		if err != nil {
			return err
		}
		task = facts.task
		if task.Version != snapshot.task.Version || task.TaskRef != snapshot.task.TaskRef || task.HeadSHA != snapshot.task.HeadSHA {
			return Conflict(CodeVersionConflict, "task changed while resolving Git truth", string(task.Status), task.Version)
		}
		if facts.project.Version != snapshot.project.Version ||
			facts.project.ControlRepoPath != snapshot.project.ControlRepoPath ||
			facts.project.CanonicalRef != snapshot.project.CanonicalRef {
			return Conflict(CodeVersionConflict, "project changed while resolving Git truth", string(facts.project.Status), facts.project.Version)
		}
		now := s.nowText()
		if err := s.acknowledgeForActor(tx, ackIDs, task.ProjectID, facts.actor, requestID, now); err != nil {
			return err
		}
		operationID, err := s.requiredID("op")
		if err != nil {
			return err
		}
		expectedVersion := task.Version
		task.AcceptedByKind, task.AcceptedByID, task.AcceptedAt = facts.actor.kind, facts.actor.id, now
		task.AcceptedIntegrationAgentID = selectedIntegrator
		task.PendingAction, task.PendingActionID, task.PendingActionRunID = "advance", operationID, task.HeadRunID
		task.PendingExpectedSHA, task.PendingTargetSHA, task.PendingStartedAt = actualCanonical, actualTaskHead, now
		task.Version++
		task.PendingActionVersion, task.UpdatedAt = task.Version, now
		if err := tx.UpdateTask(task, expectedVersion, TaskSubmitted); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"integration_agent_id": selectedIntegrator, "expected_sha": actualCanonical, "target_sha": actualTaskHead,
		})
		for _, kind := range []string{"task.accept_requested", "git.canonical_advance_requested"} {
			if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, kind, facts.actor.kind,
				facts.actor.id, facts.actor.runID, requestID, operationID, payload, now)); err != nil {
				return err
			}
		}
		return dedupe.record(tx, task.ID, "", now)
	})
	return task, err
}

func (s *Service) acceptFacts(
	tx Transaction, token, taskID, selectedIntegrator string, actor taskMutationActor,
) (acceptFacts, error) {
	var facts acceptFacts
	var err error
	if facts.task, err = tx.Task(taskID); err != nil {
		return facts, err
	}
	if facts.actor, err = s.authorizeTaskActor(tx, token, facts.task, actor); err != nil {
		return facts, err
	}
	if facts.actor.kind == "agent" && facts.task.ParentTaskID != facts.actor.current.ID &&
		!(facts.task.CreatedByKind == "agent" && facts.task.CreatedByID == facts.actor.id) {
		return facts, NewError(CodeScopeDenied, "only the parent or creating Agent can accept this task", false)
	}
	if err := ValidateTaskOperation(facts.task.Kind, "accept"); err != nil {
		return facts, err
	}
	if err := taskActionAvailable(facts.task); err != nil {
		return facts, err
	}
	if facts.task.Status != TaskSubmitted {
		return facts, Conflict(CodeInvalidState, "only a submitted task can be accepted", string(facts.task.Status), facts.task.Version)
	}
	if facts.task.HeadSHA == "" || facts.task.HeadRunID == "" || facts.task.TaskRef == "" {
		return facts, NewError(CodeGitInvariantViolation, "submitted task has no complete captured result", false)
	}
	if facts.project, err = tx.Project(facts.task.ProjectID); err != nil {
		return facts, err
	}
	if facts.project.Status != ProjectActive {
		return facts, Conflict(CodeInvalidState, "project is not active", string(facts.project.Status), facts.project.Version)
	}
	if selectedIntegrator == "" {
		selectedIntegrator = facts.project.IntegrationAgentID
	}
	if selectedIntegrator == "" {
		return facts, NewError(CodeIntegrationAgentRequired, "an integration Agent is required", false)
	}
	if facts.integrator, err = tx.Agent(selectedIntegrator); err != nil {
		return facts, err
	}
	if facts.integrator.Status != AgentActive {
		return facts, Conflict(CodeInvalidState, "integration Agent is not active", string(facts.integrator.Status), facts.integrator.Version)
	}
	return facts, nil
}
