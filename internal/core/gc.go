package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type gcDedupeRecord struct {
	InputHash string          `json:"input_hash"`
	Result    GCDiscardResult `json:"result"`
}

func (s *Service) GCPreview(ctx context.Context) (GCPreview, error) {
	snapshot, err := s.repository.Snapshot(ctx, "")
	if err != nil {
		return GCPreview{}, err
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return GCPreview{}, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	projects := make(map[string]Project, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		projects[project.ID] = project
	}
	workspaceCutoff := s.workspaceRetentionCutoff()
	refCutoff := s.taskRefRetentionCutoff()
	preview := GCPreview{GeneratedAt: s.nowText(), Workspaces: []GCWorkspaceTarget{}, TaskRefs: []GCTaskRefTarget{}}
	for _, task := range snapshot.Tasks {
		if task.Kind != TaskWork && task.Kind != TaskIntegration {
			continue
		}
		project, exists := projects[task.ProjectID]
		if !exists {
			return GCPreview{}, NewError(CodeGitInvariantViolation, "task project is missing during GC preview", false)
		}
		expectedHead := task.HeadSHA
		if expectedHead == "" {
			expectedHead = task.BaseSHA
		}
		workspaceIntent := gitWorkspaceIntent(task, expectedHead)
		state, err := executor.WorkspaceState(ctx, workspaceIntent)
		if err != nil {
			return GCPreview{}, WrapError(CodeGitInvariantViolation, "inspect workspace for GC preview", false, err)
		}
		workspaceEligible, err := s.repository.WorkspaceEligible(ctx, task.ID, workspaceCutoff)
		if err != nil {
			return GCPreview{}, err
		}
		workspaceReasons := gcWorkspaceReasons(project, task, state, workspaceEligible, expectedHead)
		preview.Workspaces = append(preview.Workspaces, GCWorkspaceTarget{
			TaskID: task.ID, TaskVersion: task.Version, Exists: state.Exists,
			Fingerprint: state.Fingerprint, ActualHead: state.HeadSHA,
			Eligible: state.Exists && len(workspaceReasons) == 0, Reasons: workspaceReasons,
		})

		if task.TaskRef == "" || task.HeadRunID == "" || task.HeadSHA == "" {
			continue
		}
		refState, err := executor.TaskRefState(ctx, GitDeleteRefIntent{
			ProjectID: task.ProjectID, ControlRepo: project.ControlRepoPath,
			CanonicalRef: project.CanonicalRef, TaskRef: task.TaskRef, ExpectedSHA: task.HeadSHA,
		})
		if err != nil {
			return GCPreview{}, WrapError(CodeGitInvariantViolation, "inspect task ref for GC preview", false, err)
		}
		refEligible, err := s.repository.TaskRefEligible(ctx, task.ID, task.TaskRef, refCutoff)
		if err != nil {
			return GCPreview{}, err
		}
		refReasons := gcTaskRefReasons(project, task, refState, refEligible)
		preview.TaskRefs = append(preview.TaskRefs, GCTaskRefTarget{
			TaskID: task.ID, RunID: task.HeadRunID, ActualSHA: refState.ActualSHA,
			Exists: refState.Exists, Eligible: refState.Exists && len(refReasons) == 0, Reasons: refReasons,
		})
	}
	return preview, nil
}

func (s *Service) GCRun(ctx context.Context, input GCRunInput) (GCRunResult, error) {
	if !input.Confirm {
		return GCRunResult{}, NewError(CodeInvalidArgument, "gc run requires confirm=true", false)
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return GCRunResult{}, err
	}
	inputHash, err := inputFingerprint(struct{ Confirm bool }{input.Confirm})
	if err != nil {
		return GCRunResult{}, err
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if _, ok, err := s.gcDedupe(ctx, "gc.run", requestID, inputHash); err != nil || ok {
		return GCRunResult{Completed: ok}, err
	}
	workspaceCutoff := s.workspaceRetentionCutoff()
	refCutoff := s.taskRefRetentionCutoff()
	if err := s.reconcileWorkspaceGC(ctx, workspaceCutoff, requestID); err != nil {
		return GCRunResult{}, err
	}
	if err := s.ReconcileGitGC(ctx, refCutoff); err != nil {
		return GCRunResult{}, err
	}
	result := GCDiscardResult{TaskID: "gc", Discarded: true}
	if err := s.putGCDedupe(ctx, "gc.run", requestID, inputHash, result, "", "gc.run.completed"); err != nil {
		return GCRunResult{}, err
	}
	return GCRunResult{Completed: true}, nil
}

func (s *Service) GCDiscardWorkspace(ctx context.Context, input GCDiscardWorkspaceInput) (GCDiscardResult, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	fingerprint, err := requireText("expected_fingerprint", input.ExpectedFingerprint)
	if err != nil {
		return GCDiscardResult{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, Fingerprint string }{taskID, fingerprint})
	if err != nil {
		return GCDiscardResult{}, err
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if result, ok, err := s.gcDedupe(ctx, "gc.discard_workspace", requestID, inputHash); err != nil || ok {
		return result, err
	}
	task, project, executor, err := s.gcTask(ctx, taskID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	if err := validateDiscardTask(project, task); err != nil {
		return GCDiscardResult{}, err
	}
	expectedHead := task.HeadSHA
	if expectedHead == "" {
		expectedHead = task.BaseSHA
	}
	intent := GitDiscardWorkspaceIntent{
		GitWorkspaceStateIntent: gitWorkspaceIntent(task, expectedHead), ExpectedFingerprint: fingerprint,
	}
	discarded, err := executor.DiscardWorkspace(ctx, intent, func() (bool, error) {
		current, currentProject, _, err := s.gcTask(ctx, task.ID)
		if err != nil {
			return false, err
		}
		if err := validateDiscardTask(currentProject, current); err != nil {
			return false, nil
		}
		return s.repository.WorkspaceEligible(ctx, current.ID, s.workspaceRetentionCutoff())
	})
	if err != nil {
		if strings.Contains(err.Error(), "fingerprint changed") {
			return GCDiscardResult{}, Conflict(CodeVersionConflict, "workspace fingerprint changed", string(task.Status), task.Version)
		}
		return GCDiscardResult{}, WrapError(CodeGitInvariantViolation, "discard workspace", false, err)
	}
	if !discarded {
		return GCDiscardResult{}, Conflict(CodeInvalidState, "workspace discard fences changed", string(task.Status), task.Version)
	}
	if err := s.releaseSourceReference(ctx, task.ID, requestID); err != nil {
		return GCDiscardResult{}, err
	}
	result := GCDiscardResult{TaskID: task.ID, Discarded: true}
	if err := s.putGCDedupe(ctx, "gc.discard_workspace", requestID, inputHash, result, task.ProjectID, "gc.workspace_discarded"); err != nil {
		return GCDiscardResult{}, err
	}
	return result, nil
}

func (s *Service) GCDiscardTaskRef(ctx context.Context, input GCDiscardTaskRefInput) (GCDiscardResult, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	runID, err := requireText("run_id", input.RunID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	expectedSHA, err := requireText("expected_sha", input.ExpectedSHA)
	if err != nil {
		return GCDiscardResult{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	inputHash, err := inputFingerprint(struct{ TaskID, RunID, SHA string }{taskID, runID, expectedSHA})
	if err != nil {
		return GCDiscardResult{}, err
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if result, ok, err := s.gcDedupe(ctx, "gc.discard_task_ref", requestID, inputHash); err != nil || ok {
		return result, err
	}
	task, project, executor, err := s.gcTask(ctx, taskID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	if err := validateDiscardTask(project, task); err != nil {
		return GCDiscardResult{}, err
	}
	run, err := s.repository.Run(ctx, runID)
	if err != nil {
		return GCDiscardResult{}, err
	}
	wantRef := "refs/coordplane/tasks/" + task.ID + "/runs/" + run.ID
	if run.TaskID != task.ID || task.HeadRunID != run.ID || task.TaskRef != wantRef || task.HeadSHA != expectedSHA {
		return GCDiscardResult{}, NewError(CodeInvalidArgument, "task, Run, ref, and expected SHA identity do not match", false)
	}
	intent := GitDeleteRefIntent{
		ProjectID: task.ProjectID, ControlRepo: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, TaskRef: task.TaskRef,
		ExpectedSHA: expectedSHA, AllowDiscard: true,
	}
	state, err := executor.TaskRefState(ctx, intent)
	if err != nil {
		return GCDiscardResult{}, WrapError(CodeGitInvariantViolation, "inspect task ref before discard", false, err)
	}
	if state.Exists && state.ActualSHA != expectedSHA {
		return GCDiscardResult{}, Conflict(CodeVersionConflict, "task ref SHA changed", string(task.Status), task.Version)
	}
	discarded, err := executor.DeleteTaskRefAndPrune(ctx, intent, func() (bool, error) {
		current, currentProject, _, err := s.gcTask(ctx, task.ID)
		if err != nil || validateDiscardTask(currentProject, current) != nil {
			return false, err
		}
		currentRun, err := s.repository.Run(ctx, run.ID)
		if err != nil || currentRun.TaskID != current.ID || current.HeadRunID != currentRun.ID ||
			current.TaskRef != task.TaskRef || current.HeadSHA != expectedSHA {
			return false, err
		}
		return s.repository.TaskRefEligible(ctx, current.ID, current.TaskRef, s.taskRefRetentionCutoff())
	})
	if err != nil {
		return GCDiscardResult{}, WrapError(CodeGitInvariantViolation, "discard task ref", false, err)
	}
	if !discarded {
		return GCDiscardResult{}, Conflict(CodeInvalidState, "task ref discard fences changed", string(task.Status), task.Version)
	}
	result := GCDiscardResult{TaskID: task.ID, RunID: run.ID, Discarded: true}
	if err := s.putGCDedupe(ctx, "gc.discard_task_ref", requestID, inputHash, result, task.ProjectID, "gc.task_ref_discarded"); err != nil {
		return GCDiscardResult{}, err
	}
	return result, nil
}

func (s *Service) gcTask(ctx context.Context, taskID string) (Task, Project, TaskGit, error) {
	task, err := s.repository.Task(ctx, taskID)
	if err != nil {
		return Task{}, Project{}, nil, err
	}
	project, err := s.repository.Project(ctx, task.ProjectID)
	if err != nil {
		return Task{}, Project{}, nil, err
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return Task{}, Project{}, nil, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	return task, project, executor, nil
}

func validateDiscardTask(project Project, task Task) error {
	if project.Status != ProjectActive {
		return Conflict(CodeInvalidState, "project must be active for discard", string(project.Status), project.Version)
	}
	if task.Status != TaskCompleted && task.Status != TaskCancelled {
		return Conflict(CodeInvalidState, "discard requires a completed or cancelled task", string(task.Status), task.Version)
	}
	if task.CurrentRunID != "" || task.PendingAction != "" {
		return Conflict(CodeActionInProgress, "discard cannot override current Run or pending action", string(task.Status), task.Version)
	}
	return nil
}

func (s *Service) workspaceRetentionCutoff() string {
	return s.retentionCutoff(s.completedWorkspaceRetention)
}

func (s *Service) taskRefRetentionCutoff() string {
	return s.retentionCutoff(s.terminalTaskRefRetention)
}

func (s *Service) retentionCutoff(retention time.Duration) string {
	return s.now().UTC().Add(-retention).Format("2006-01-02T15:04:05.000000000Z")
}

func gitWorkspaceIntent(task Task, expectedHead string) GitWorkspaceStateIntent {
	intent := GitDeleteWorkspaceIntent{
		ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA, ExpectedHead: expectedHead,
	}
	if task.SourceTaskID != "" {
		intent.Source = &GitSource{
			TaskID: task.SourceTaskID, RunID: task.SourceRunID,
			TaskRef: task.SourceTaskRef, HeadSHA: task.SourceHeadSHA,
		}
	}
	return GitWorkspaceStateIntent{GitDeleteWorkspaceIntent: intent, TaskVersion: task.Version}
}

func gcWorkspaceReasons(project Project, task Task, state GitWorkspaceStateFact, eligible bool, expectedHead string) []string {
	var reasons []string
	if project.Status != ProjectActive {
		reasons = append(reasons, "project_not_active")
	}
	if task.Status != TaskCompleted && task.Status != TaskCancelled {
		reasons = append(reasons, "task_not_terminal")
	}
	if task.CurrentRunID != "" || task.PendingAction != "" {
		reasons = append(reasons, "active_or_pending")
	}
	if !eligible {
		reasons = append(reasons, "durable_fence_or_retention")
	}
	if !state.Exists {
		reasons = append(reasons, "absent")
	}
	if state.Exists && state.HeadSHA != expectedHead {
		reasons = append(reasons, "head_changed")
	}
	if state.Exists && !state.Clean {
		reasons = append(reasons, "dirty")
	}
	return reasons
}

func gcTaskRefReasons(project Project, task Task, state GitTaskRefStateFact, eligible bool) []string {
	var reasons []string
	if project.Status != ProjectActive {
		reasons = append(reasons, "project_not_active")
	}
	if !eligible {
		reasons = append(reasons, "durable_reference_or_retention")
	}
	if !state.Exists {
		reasons = append(reasons, "absent")
	}
	if state.Exists && state.ActualSHA != task.HeadSHA {
		reasons = append(reasons, "sha_changed")
	}
	if state.Exists && !state.Included {
		reasons = append(reasons, "not_in_canonical")
	}
	return reasons
}

func (s *Service) releaseSourceReference(ctx context.Context, taskID, requestID string) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(taskID)
		if err != nil {
			return err
		}
		if task.SourceTaskRef == "" || task.SourceRefReleasedAt != "" {
			return nil
		}
		if (task.Status != TaskCompleted && task.Status != TaskCancelled) || task.CurrentRunID != "" || task.PendingAction != "" {
			return Conflict(CodeInvalidState, "source reference release fences changed", string(task.Status), task.Version)
		}
		now := s.nowText()
		expectedVersion, expectedStatus := task.Version, task.Status
		task.SourceRefReleasedAt = now
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "gc.source_ref_released", "boss", "", "", requestID, "", "{}", now))
		return err
	})
}

func (s *Service) gcDedupe(ctx context.Context, operation, requestID, inputHash string) (GCDiscardResult, bool, error) {
	var result GCDiscardResult
	var found bool
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		raw, ok, err := tx.Dedupe("boss", operation, requestID)
		if err != nil || !ok {
			return err
		}
		var record gcDedupeRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return NewError(CodeInternal, "invalid GC request dedupe record", false)
		}
		if record.InputHash != inputHash {
			return NewError(CodeVersionConflict, "request_id was already used with different input", false)
		}
		result, found = record.Result, true
		return nil
	})
	return result, found, err
}

func (s *Service) putGCDedupe(
	ctx context.Context,
	operation, requestID, inputHash string,
	result GCDiscardResult,
	projectID, eventKind string,
) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", operation, requestID); err != nil {
			return err
		} else if ok {
			var record gcDedupeRecord
			if json.Unmarshal(raw, &record) != nil || record.InputHash != inputHash {
				return NewError(CodeVersionConflict, "request_id was already used with different input", false)
			}
			return nil
		}
		raw, err := json.Marshal(gcDedupeRecord{InputHash: inputHash, Result: result})
		if err != nil {
			return WrapError(CodeInternal, "encode GC request dedupe", false, err)
		}
		now := s.nowText()
		if err := tx.PutDedupe("boss", operation, requestID, raw, now); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"task_id": result.TaskID, "run_id": result.RunID, "discarded": result.Discarded,
		})
		_, err = tx.AppendEvent(event(projectID, "daemon", "gc", eventKind, "boss", "", "", requestID, "", payload, now))
		return err
	})
}
