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
	sourceTaskID := strings.TrimSpace(input.SourceTaskID)
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct {
		AgentID, Title, Description, SourceTaskID, AckIDs string
		Priority, MaxRetries                              int
	}{agentID, title, description, sourceTaskID, strings.Join(ackIDs, "\x00"), input.Priority, input.MaxRetries})
	if err != nil {
		return Task{}, err
	}

	var project Project
	var sourceSnapshot Task
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
		run, parent, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		project, err = tx.Project(parent.ProjectID)
		if err != nil {
			return err
		}
		if sourceTaskID != "" {
			sourceSnapshot, err = tx.Task(sourceTaskID)
			if err != nil {
				return err
			}
			if !runCanReadTask(run, parent, sourceSnapshot) {
				return NewError(CodeScopeDenied, "source task is outside the current run scope", false)
			}
			return validateSourceTask(parent.ProjectID, sourceSnapshot)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	if replayed {
		return child, nil
	}
	var executor TaskGit
	if sourceTaskID != "" {
		var ok bool
		executor, ok = s.projectGit.(TaskGit)
		if !ok {
			return Task{}, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
		}
	}
	baseSHA, err := s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve canonical ref", false, err)
	}

	create := func() error {
		return s.repository.Transact(ctx, func(tx Transaction) error {
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
			var source Task
			if sourceTaskID != "" {
				source, err = tx.Task(sourceTaskID)
				if err != nil {
					return err
				}
				if !runCanReadTask(run, parent, source) {
					return NewError(CodeScopeDenied, "source task is outside the current run scope", false)
				}
				if source.Version != sourceSnapshot.Version || source.TaskRef != sourceSnapshot.TaskRef || source.HeadSHA != sourceSnapshot.HeadSHA {
					return Conflict(CodeVersionConflict, "source task changed while creating child task", string(source.Status), source.Version)
				}
				if err := validateSourceTask(parent.ProjectID, source); err != nil {
					return err
				}
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
			if sourceTaskID != "" {
				child.SourceTaskID = source.ID
				child.SourceRunID = source.HeadRunID
				child.SourceTaskRef = source.TaskRef
				child.SourceHeadSHA = source.HeadSHA
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
	}
	if sourceTaskID == "" {
		err = create()
	} else {
		err = executor.UseTaskRef(ctx, GitTaskRefIntent{
			ProjectID: project.ID, ControlRepo: project.ControlRepoPath,
			TaskRef: sourceSnapshot.TaskRef, ExpectedSHA: sourceSnapshot.HeadSHA,
		}, func(actual string) error {
			if actual != sourceSnapshot.HeadSHA {
				return NewError(CodeGitInvariantViolation, "source task ref changed", false)
			}
			return create()
		})
	}
	if err != nil && isGitInvariant(err) {
		err = WrapError(CodeGitInvariantViolation, "use source task ref", false, err)
	}
	return child, err
}
