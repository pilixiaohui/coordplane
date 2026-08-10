package core

import (
	"context"
	"errors"
	"fmt"
)

var contractCaptureFinalizedHook func(context.Context, GitCaptureIntent) error

// ReconcileGit consumes durable capture/advance intents. Git operations run
// outside SQLite transactions; every projection transaction repeats the full
// durable fence before it can change task state.
func (s *Service) ReconcileGit(ctx context.Context) error {
	tasks, err := s.repository.PendingGitTasks(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	var result error
	for _, snapshot := range tasks {
		var reconcileErr error
		switch snapshot.PendingAction {
		case "capture":
			reconcileErr = s.reconcileCapture(ctx, executor, snapshot)
		case "advance":
			reconcileErr = s.reconcileAdvance(ctx, executor, snapshot)
		}
		if reconcileErr != nil {
			result = errors.Join(result, fmt.Errorf("reconcile Git task %s: %w", snapshot.ID, reconcileErr))
		}
	}
	return result
}

// ReconcileGitGC deletes only canonical-contained refs whose complete durable
// retention predicate remains true under the Git maintenance lock.
func (s *Service) ReconcileGitGC(ctx context.Context, closedBefore string) error {
	candidates, err := s.repository.TaskRefCandidates(ctx, closedBefore)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	var result error
	for _, task := range candidates {
		project, err := s.repository.Project(ctx, task.ProjectID)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		_, err = executor.DeleteTaskRefAndPrune(ctx, GitDeleteRefIntent{
			ProjectID: task.ProjectID, ControlRepo: project.ControlRepoPath,
			CanonicalRef: project.CanonicalRef, TaskRef: task.TaskRef, ExpectedSHA: task.HeadSHA,
		}, func() (bool, error) {
			return s.repository.TaskRefEligible(ctx, task.ID, task.TaskRef, closedBefore)
		})
		if err != nil {
			result = errors.Join(result, fmt.Errorf("GC task ref %s: %w", task.ID, err))
		}
	}
	return result
}

func (s *Service) ReconcileWorkspaceGC(ctx context.Context, closedBefore string) error {
	return s.reconcileWorkspaceGC(ctx, closedBefore, "")
}

func (s *Service) reconcileWorkspaceGC(ctx context.Context, closedBefore, requestID string) error {
	candidates, err := s.repository.WorkspaceCandidates(ctx, closedBefore)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	var result error
	for _, task := range candidates {
		expectedHead := task.HeadSHA
		if expectedHead == "" {
			expectedHead = task.BaseSHA
		}
		intent := GitDeleteWorkspaceIntent{
			ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA, ExpectedHead: expectedHead,
		}
		if task.SourceTaskID != "" {
			intent.Source = &GitSource{
				TaskID: task.SourceTaskID, RunID: task.SourceRunID,
				TaskRef: task.SourceTaskRef, HeadSHA: task.SourceHeadSHA,
			}
		}
		deleted, err := executor.DeleteWorkspace(ctx, intent, func() (bool, error) {
			return s.repository.WorkspaceEligible(ctx, task.ID, closedBefore)
		})
		if err != nil {
			result = errors.Join(result, fmt.Errorf("GC workspace %s: %w", task.ID, err))
			continue
		}
		if deleted {
			if err := s.releaseSourceReference(ctx, task.ID, requestID); err != nil {
				result = errors.Join(result, fmt.Errorf("release source ref for %s: %w", task.ID, err))
			}
		}
	}
	return result
}

func (s *Service) reconcileCapture(ctx context.Context, executor TaskGit, snapshot Task) error {
	task, err := s.repository.Task(ctx, snapshot.ID)
	if err != nil || task.PendingAction != "capture" {
		return err
	}
	run, err := s.repository.Run(ctx, task.PendingActionRunID)
	if err != nil {
		return err
	}
	if !IsRunTerminal(run.State) {
		return nil
	}
	project, err := s.repository.Project(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	intent := GitCaptureIntent{
		ProjectID: task.ProjectID, TaskID: task.ID, RunID: run.ID,
		WorkspacePath: run.WorkspacePath, ControlRepo: project.ControlRepoPath,
		BaseSHA: task.BaseSHA, ExpectedHead: task.PendingExpectedSHA,
		OperationID: task.PendingActionID,
	}
	if task.SourceTaskID != "" {
		intent.Source = &GitSource{
			TaskID: task.SourceTaskID, RunID: task.SourceRunID,
			TaskRef: task.SourceTaskRef, HeadSHA: task.SourceHeadSHA,
		}
	}
	fact, captureErr := executor.Capture(ctx, intent)
	if captureErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.projectCaptureFailure(ctx, task, run, captureErr)
	}
	actualCanonical := ""
	if task.Kind == TaskIntegration {
		actualCanonical, err = s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
		if err != nil {
			return WrapError(CodeGitInvariantViolation, "resolve canonical after integration capture", false, err)
		}
	}
	if err := s.finalizeCapture(ctx, task, run, fact, actualCanonical); err != nil {
		return err
	}
	if contractCaptureFinalizedHook != nil {
		if err := contractCaptureFinalizedHook(ctx, intent); err != nil {
			return err
		}
	}
	if err := executor.CleanupCapture(ctx, intent); err != nil {
		return WrapError(CodeGitInvariantViolation, "clean finalized capture handoff", false, err)
	}
	return nil
}

func (s *Service) finalizeCapture(ctx context.Context, intent Task, intentRun Run, fact GitCaptureFact, actualCanonical string) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(intent.ID)
		if err != nil {
			return err
		}
		run, err := tx.Run(intentRun.ID)
		if err != nil {
			return err
		}
		if !sameCaptureFence(task, run, intent) {
			return Conflict(CodeStaleRun, "capture intent fence changed", string(task.Status), task.Version)
		}
		if fact.HeadSHA != task.PendingExpectedSHA || fact.TaskRef == "" {
			return NewError(CodeGitInvariantViolation, "capture fact does not match durable intent", false)
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}

		now := s.nowText()
		expectedVersion := task.Version
		task.HeadSHA = fact.HeadSHA
		task.HeadRunID = run.ID
		task.TaskRef = fact.TaskRef
		task.ResultSummary = run.RequestedSummary
		task.CurrentRunID = ""
		task.Status = TaskSubmitted
		task.SubmittedAt = now
		clearTaskAction(&task)
		task.Version++
		task.UpdatedAt = now

		if task.Kind == TaskIntegration {
			source, sourceErr := tx.Task(task.SourceTaskID)
			if sourceErr != nil {
				return sourceErr
			}
			if err := integrationSourceFence(source, task); err != nil {
				return err
			}
			if actualCanonical == "" {
				return NewError(CodeGitInvariantViolation, "integration capture has no actual canonical SHA", false)
			}
			task.AcceptedByKind = "system"
			task.AcceptedByID = ""
			task.AcceptedAt = now
			task.AcceptedIntegrationAgentID = task.AssigneeAgentID
			task.PendingAction = "advance"
			task.PendingActionID, err = s.requiredID("op")
			if err != nil {
				return err
			}
			task.PendingActionRunID = run.ID
			task.PendingExpectedSHA = actualCanonical
			task.PendingTargetSHA = fact.HeadSHA
			task.PendingStartedAt = now
			task.PendingActionVersion = task.Version
		}
		if err := tx.UpdateTask(task, expectedVersion, TaskFinishing); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"head_sha": fact.HeadSHA, "task_ref": fact.TaskRef})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.result_captured", "daemon", "", run.ID, "", intent.PendingActionID, payload, now)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.submitted", "daemon", "", run.ID, "", intent.PendingActionID, payload, now)); err != nil {
			return err
		}
		if err := s.disposeUnresolvedMessages(tx, task, "daemon", "", run.ID, "", now); err != nil {
			return err
		}
		if task.Kind == TaskIntegration {
			advancePayload := eventPayload(map[string]any{"expected_sha": actualCanonical, "target_sha": fact.HeadSHA})
			_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.canonical_advance_requested", "system", "", run.ID, "", task.PendingActionID, advancePayload, now))
			return err
		}
		return s.notifyParentOfChild(tx, task, "daemon", "", run.ID, "", now)
	})
}

func sameCaptureFence(task Task, run Run, intent Task) bool {
	return task.Status == TaskFinishing && task.PendingAction == "capture" &&
		task.PendingActionID == intent.PendingActionID && task.Version == intent.PendingActionVersion &&
		task.PendingActionVersion == intent.PendingActionVersion &&
		task.PendingActionRunID == run.ID && task.CurrentRunID == run.ID &&
		task.Generation == run.Generation && IsRunTerminal(run.State) &&
		run.RequestedOutcome == string(OutcomeSubmit) && run.ExpectedHead == task.PendingExpectedSHA
}

func (s *Service) projectCaptureFailure(ctx context.Context, intent Task, intentRun Run, cause error) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(intent.ID)
		if err != nil {
			return err
		}
		run, err := tx.Run(intentRun.ID)
		if err != nil {
			return err
		}
		if !sameCaptureFence(task, run, intent) {
			return nil
		}
		now := s.nowText()
		expectedVersion := task.Version
		task.CurrentRunID = ""
		task.FailureReason = boundedDurableText("GIT_CAPTURE_FAILED: "+cause.Error(), MaximumTerminalTextBytes)
		clearTaskAction(&task)
		invariant := isGitInvariant(cause)
		if invariant {
			task.Status = TaskFailed
		} else {
			decision := runtimeRetryDecision(task.RetryCount, task.MaxRetries)
			task.Status = decision.status
			task.RetryCount = decision.retryCount
			if task.Status == TaskQueued {
				task.NextRunAt = runtimeRetryAt(now, task.RetryCount)
			}
		}
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, TaskFinishing); err != nil {
			return err
		}
		if invariant {
			project, err := tx.Project(task.ProjectID)
			if err != nil {
				return err
			}
			if project.Status == ProjectActive {
				expectedProjectVersion := project.Version
				project.Status = ProjectError
				project.LastError = task.FailureReason
				project.Version++
				project.UpdatedAt = now
				if err := tx.UpdateProject(project, expectedProjectVersion, ProjectActive); err != nil {
					return err
				}
			}
		}
		kind := "task.requeued"
		if task.Status == TaskFailed {
			kind = "task.failed"
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, kind, "daemon", "", run.ID, "", intent.PendingActionID, eventPayload(map[string]any{"reason": task.FailureReason}), now)); err != nil {
			return err
		}
		if task.Status == TaskFailed {
			if err := s.disposeUnresolvedMessages(tx, task, "daemon", "", run.ID, "", now); err != nil {
				return err
			}
			return s.notifyParentOfChild(tx, task, "daemon", "", run.ID, "", now)
		}
		return nil
	})
}

func (s *Service) reconcileAdvance(ctx context.Context, executor TaskGit, snapshot Task) error {
	task, err := s.repository.Task(ctx, snapshot.ID)
	if err != nil || task.PendingAction != "advance" {
		return err
	}
	project, err := s.repository.Project(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	fact, advanceErr := executor.Advance(ctx, GitAdvanceIntent{
		ProjectID: task.ProjectID, TaskID: task.ID, RunID: task.HeadRunID,
		OperationID: task.PendingActionID, ControlRepo: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: task.TaskRef,
		ExpectedOldSHA: task.PendingExpectedSHA, TargetSHA: task.PendingTargetSHA,
	})
	if advanceErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isGitInvariant(advanceErr) {
			return s.projectAdvanceInvariantFailure(ctx, task, advanceErr)
		}
		return advanceErr
	}
	if fact.Outcome == GitAdvanceStale {
		return s.finalizeStaleAdvance(ctx, task, fact)
	}
	if fact.Outcome != GitAdvanceUpdated && fact.Outcome != GitAdvanceIncluded {
		return NewError(CodeGitInvariantViolation, "unknown Git advance outcome", false)
	}
	return s.finalizeSuccessfulAdvance(ctx, task, fact)
}

func (s *Service) finalizeSuccessfulAdvance(ctx context.Context, intent Task, fact GitAdvanceFact) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(intent.ID)
		if err != nil {
			return err
		}
		if err := sameAdvanceFence(task, intent); err != nil {
			return err
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive || fact.ActualSHA == "" {
			return Conflict(CodeInvalidState, "project cannot finalize Git advance", string(project.Status), project.Version)
		}
		now := s.nowText()
		if project.CanonicalSHA != fact.ActualSHA {
			expectedProjectVersion := project.Version
			project.CanonicalSHA = fact.ActualSHA
			project.Version++
			project.UpdatedAt = now
			if err := tx.UpdateProject(project, expectedProjectVersion, ProjectActive); err != nil {
				return err
			}
		}
		if task.Kind == TaskIntegration {
			source, err := tx.Task(task.SourceTaskID)
			if err != nil {
				return err
			}
			if err := integrationSourceFence(source, task); err != nil {
				return err
			}
			expectedSourceVersion := source.Version
			source.Status = TaskCompleted
			source.FinalCanonicalSHA = fact.ActualSHA
			source.CompletedAt = now
			source.ClosedAt = now
			source.Version++
			source.UpdatedAt = now
			if err := tx.UpdateTask(source, expectedSourceVersion, TaskSubmitted); err != nil {
				return err
			}
			sourcePayload := eventPayload(map[string]any{"canonical_sha": fact.ActualSHA, "integration_task_id": task.ID})
			if _, err := tx.AppendEvent(event(source.ProjectID, "task", source.ID, "git.canonical_advanced", "daemon", "", task.HeadRunID, "", intent.PendingActionID, sourcePayload, now)); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(source.ProjectID, "task", source.ID, "task.completed", "daemon", "", task.HeadRunID, "", intent.PendingActionID, sourcePayload, now)); err != nil {
				return err
			}
			if err := s.disposeUnresolvedMessages(tx, source, "daemon", "", task.HeadRunID, "", now); err != nil {
				return err
			}
			if err := s.notifyParentOfChild(tx, source, "daemon", "", task.HeadRunID, "", now); err != nil {
				return err
			}
		}
		expectedVersion := task.Version
		task.Status = TaskCompleted
		task.FinalCanonicalSHA = fact.ActualSHA
		task.CompletedAt = now
		task.ClosedAt = now
		clearTaskAction(&task)
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, TaskSubmitted); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"canonical_sha": fact.ActualSHA, "outcome": fact.Outcome})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.canonical_advanced", "daemon", "", task.HeadRunID, "", intent.PendingActionID, payload, now)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.completed", "daemon", "", task.HeadRunID, "", intent.PendingActionID, payload, now)); err != nil {
			return err
		}
		if err := s.disposeUnresolvedMessages(tx, task, "daemon", "", task.HeadRunID, "", now); err != nil {
			return err
		}
		if task.Kind != TaskIntegration {
			return s.notifyParentOfChild(tx, task, "daemon", "", task.HeadRunID, "", now)
		}
		return nil
	})
}

func (s *Service) finalizeStaleAdvance(ctx context.Context, intent Task, fact GitAdvanceFact) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(intent.ID)
		if err != nil {
			return err
		}
		if err := sameAdvanceFence(task, intent); err != nil {
			return err
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		now := s.nowText()
		if project.CanonicalSHA != fact.ActualSHA {
			expectedProjectVersion := project.Version
			project.CanonicalSHA = fact.ActualSHA
			project.Version++
			project.UpdatedAt = now
			if err := tx.UpdateProject(project, expectedProjectVersion, ProjectActive); err != nil {
				return err
			}
		}
		if task.Kind == TaskIntegration {
			source, err := tx.Task(task.SourceTaskID)
			if err != nil {
				return err
			}
			if err := integrationSourceFence(source, task); err != nil {
				return err
			}
			expectedVersion := task.Version
			task.Status = TaskQueued
			task.CurrentRunID = ""
			task.NextRunAt = now
			clearTaskAction(&task)
			task.Version++
			task.UpdatedAt = now
			if err := tx.UpdateTask(task, expectedVersion, TaskSubmitted); err != nil {
				return err
			}
			if err := s.insertGitTaskMessage(tx, task, "integration_stale", fmt.Sprintf("Canonical advanced to %s. Integrate it into the existing workspace and submit again.", fact.ActualSHA), now); err != nil {
				return err
			}
			_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.integration_requeued", "daemon", "", task.HeadRunID, "", intent.PendingActionID, eventPayload(map[string]any{"canonical_sha": fact.ActualSHA}), now))
			return err
		}

		integrator, err := tx.Agent(task.AcceptedIntegrationAgentID)
		if err != nil {
			return err
		}
		if integrator.Status == AgentArchived {
			return NewError(CodeGitInvariantViolation, "selected integration Agent was archived", false)
		}
		integrationID, err := s.requiredID("tsk")
		if err != nil {
			return err
		}
		expectedSourceVersion := task.Version
		linkedVersion := task.Version + 1
		task.IntegrationTaskID = integrationID
		clearTaskAction(&task)
		task.Version = linkedVersion
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedSourceVersion, TaskSubmitted); err != nil {
			return err
		}
		integration := Task{
			ID: integrationID, ProjectID: task.ProjectID, Kind: TaskIntegration,
			CreatedByKind: "system", AssigneeAgentID: task.AcceptedIntegrationAgentID,
			Title: "Integrate " + task.ID, Description: "Integrate the fixed source task ref into the current canonical branch.",
			Priority: task.Priority, Status: TaskQueued, NextRunAt: now,
			MaxRetries: task.MaxRetries, BaseSHA: fact.ActualSHA,
			SourceTaskID: task.ID, SourceRunID: task.HeadRunID,
			SourceTaskRef: task.TaskRef, SourceHeadSHA: task.HeadSHA,
			SourceAcceptVersion: linkedVersion, ObservedCanonicalSHA: fact.ActualSHA,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertTask(integration); err != nil {
			return err
		}
		if err := s.insertGitTaskMessage(tx, integration, "integration_required", fmt.Sprintf("Integrate source %s at %s into canonical %s.", task.ID, task.HeadSHA, fact.ActualSHA), now); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"source_task_id": task.ID, "source_task_ref": task.TaskRef, "canonical_sha": fact.ActualSHA})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", integration.ID, "task.created", "system", "", "", "", intent.PendingActionID, payload, now)); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "git.integration_required", "daemon", "", task.HeadRunID, "", intent.PendingActionID, payload, now))
		return err
	})
}

func sameAdvanceFence(task Task, intent Task) error {
	if task.Status != TaskSubmitted || task.PendingAction != "advance" ||
		task.PendingActionID != intent.PendingActionID || task.Version != intent.PendingActionVersion ||
		task.PendingActionVersion != intent.PendingActionVersion ||
		task.PendingExpectedSHA != intent.PendingExpectedSHA || task.PendingTargetSHA != intent.PendingTargetSHA ||
		task.TaskRef != intent.TaskRef || task.HeadSHA != intent.HeadSHA ||
		task.AcceptedIntegrationAgentID != intent.AcceptedIntegrationAgentID {
		return Conflict(CodeVersionConflict, "advance intent fence changed", string(task.Status), task.Version)
	}
	return nil
}

func integrationSourceFence(source, integration Task) error {
	if integration.Kind != TaskIntegration || source.Status != TaskSubmitted ||
		source.Version != integration.SourceAcceptVersion || source.IntegrationTaskID != integration.ID ||
		source.TaskRef != integration.SourceTaskRef || source.HeadSHA != integration.SourceHeadSHA ||
		source.HeadRunID != integration.SourceRunID ||
		source.AcceptedIntegrationAgentID == "" || source.AcceptedIntegrationAgentID != integration.AssigneeAgentID {
		return Conflict(CodeVersionConflict, "integration source fence changed", string(source.Status), source.Version)
	}
	return nil
}

func (s *Service) insertGitTaskMessage(tx Transaction, task Task, code, body, now string) error {
	_, err := s.insertMessage(tx, messageInsert{
		projectID: task.ProjectID, taskID: task.ID, senderKind: "system",
		recipientKind: "agent", recipientID: task.AssigneeAgentID,
		systemCode: code, body: boundedDurableText(body, MaximumMessageBodyBytes), wake: true,
		maxDeliveries: 3, idempotencyKey: code + ":" + task.ID + ":" + fmt.Sprint(task.Version),
		actor: taskMutationActor{kind: "system"}, now: now,
		payload: eventPayload(map[string]any{"task_id": task.ID, "system_code": code}),
	})
	return err
}

func (s *Service) projectAdvanceInvariantFailure(ctx context.Context, intent Task, cause error) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(intent.ID)
		if err != nil {
			return err
		}
		if err := sameAdvanceFence(task, intent); err != nil {
			return nil
		}
		project, err := tx.Project(task.ProjectID)
		if err != nil {
			return err
		}
		now := s.nowText()
		reason := boundedDurableText("GIT_ADVANCE_FAILED: "+cause.Error(), MaximumTerminalTextBytes)
		expectedTaskVersion := task.Version
		task.Status = TaskFailed
		task.FailureReason = reason
		clearTaskAction(&task)
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedTaskVersion, TaskSubmitted); err != nil {
			return err
		}
		if project.Status == ProjectActive {
			expectedProjectVersion := project.Version
			project.Status = ProjectError
			project.LastError = reason
			project.Version++
			project.UpdatedAt = now
			if err := tx.UpdateProject(project, expectedProjectVersion, ProjectActive); err != nil {
				return err
			}
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.failed", "daemon", "", task.HeadRunID, "", intent.PendingActionID, eventPayload(map[string]any{"reason": reason}), now)); err != nil {
			return err
		}
		if err := s.disposeUnresolvedMessages(tx, task, "daemon", "", task.HeadRunID, "", now); err != nil {
			return err
		}
		return s.notifyParentOfChild(tx, task, "daemon", "", task.HeadRunID, "", now)
	})
}

func isGitInvariant(err error) bool {
	var marker interface{ GitInvariant() bool }
	return errors.As(err, &marker) && marker.GitInvariant()
}
