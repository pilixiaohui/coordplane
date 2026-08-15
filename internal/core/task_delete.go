package core

import (
	"context"
	"strings"
)

type deletedTaskPlan struct {
	taskID    string
	projectID string
	baseSHA   string
	headSHA   string
	taskRef   string
	headRunID string
}

// DeleteTask permanently removes a terminal task and all of its associated
// records (Runs, Messages, Events) in a single transaction, then best-effort
// releases the task's on-disk resources (workspace and task ref). The deleted
// task keeps exactly one audit Event ("task.deleted") so operators can still
// trace that it existed.
func (s *Service) DeleteTask(ctx context.Context, input TaskActionInput) error {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return err
	}
	input.Reason, err = optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return err
	}
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, Reason, AckIDs string }{taskID, input.Reason, strings.Join(ackIDs, "\x00")})
	if err != nil {
		return err
	}
	dedupe := requestDedupe{"", "task.delete", requestID, inputHash}
	var plan deletedTaskPlan
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		actor, err := s.taskActorForDedupe(tx, input.Token)
		if err != nil {
			return err
		}
		dedupe.scope = actor.dedupeScope
		if _, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			// The task is already deleted; replaying the request is a success.
			return nil
		}
		task, err := tx.Task(taskID)
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
		if err := taskActionAvailable(task); err != nil {
			return err
		}
		if !IsTaskClosed(task.Status) {
			return Conflict(CodeInvalidState, "only a completed or cancelled task can be deleted", string(task.Status), task.Version)
		}
		if task.CurrentRunID != "" {
			return Conflict(CodeActionInProgress, "task has a current Run", string(task.Status), task.Version)
		}
		if task.Kind == TaskIntegration && task.SourceTaskID != "" {
			if err := s.releaseIntegrationSource(tx, &task, actor, requestID, now); err != nil {
				return err
			}
		}
		runs, err := tx.DeleteRunsByTask(task.ID)
		if err != nil {
			return err
		}
		for _, run := range runs {
			if IsRunLive(run.State) {
				return Conflict(CodeActionInProgress, "task has a live Run", string(run.State), run.Version)
			}
		}
		if err := tx.DeleteMessagesByTask(task.ID); err != nil {
			return err
		}
		if err := tx.DeleteEventsByTask(task.ID); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"reason": boundedDurableText(input.Reason, MaximumOutcomeTextBytes), "run_count": len(runs),
		})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.deleted", actor.kind, actor.id, actor.runID, requestID, "", payload, now)); err != nil {
			return err
		}
		if err := tx.DeleteTask(task.ID); err != nil {
			return err
		}
		if err := dedupe.record(tx, task.ID, "", now); err != nil {
			return err
		}
		plan = deletedTaskPlan{
			taskID: task.ID, projectID: task.ProjectID, baseSHA: task.BaseSHA,
			headSHA: task.HeadSHA, taskRef: task.TaskRef, headRunID: task.HeadRunID,
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.cleanupDeletedTaskResources(ctx, plan)
	return nil
}

func (s *Service) releaseIntegrationSource(tx Transaction, task *Task, actor taskMutationActor, requestID, now string) error {
	source, err := tx.Task(task.SourceTaskID)
	if err != nil {
		if IsCode(err, CodeNotFound) {
			return nil
		}
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
	_, err = tx.AppendEvent(event(source.ProjectID, "task", source.ID, "git.integration_released", actor.kind, actor.id, actor.runID, requestID, "", payload, now))
	return err
}

func (s *Service) cleanupDeletedTaskResources(ctx context.Context, plan deletedTaskPlan) {
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return
	}
	if plan.baseSHA != "" {
		expectedHead := plan.headSHA
		if expectedHead == "" {
			expectedHead = plan.baseSHA
		}
		intent := GitDeleteWorkspaceIntent{
			ProjectID: plan.projectID, TaskID: plan.taskID, BaseSHA: plan.baseSHA, ExpectedHead: expectedHead,
		}
		if _, err := executor.DeleteWorkspace(ctx, intent, func() (bool, error) { return true, nil }); err != nil {
			return
		}
	}
	if plan.taskRef == "" || plan.headSHA == "" || plan.headRunID == "" {
		return
	}
	project, err := s.repository.Project(ctx, plan.projectID)
	if err != nil {
		return
	}
	intent := GitDeleteRefIntent{
		ProjectID: plan.projectID, ControlRepo: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: plan.taskRef,
		ExpectedSHA: plan.headSHA, AllowDiscard: true,
	}
	if _, err := executor.DeleteTaskRefAndPrune(ctx, intent, func() (bool, error) { return true, nil }); err != nil {
		return
	}
}
