package core

import (
	"context"
	"strings"
)

type workTaskRequest struct {
	token, projectID, agentID, title, description string
	sourceTaskID, retryOfTaskID                   string
	priority, maxRetries                          int
	ackIDs                                        []string
	requestID                                     string
	dedupe                                        requestDedupe
}

type taskCreationSnapshot struct {
	project Project
	source  Task
	retry   Task
	parent  Task
	actor   taskMutationActor
}

func (s *Service) normalizeWorkTask(
	agentID, title, description, sourceTaskID string,
	priority, maxRetries int, ackMessageIDs []string, requestID string,
) (workTaskRequest, error) {
	var input workTaskRequest
	var err error
	if input.agentID, err = requireText("assignee_agent_id", agentID); err != nil {
		return input, err
	}
	if input.title, err = requireText("title", title); err != nil {
		return input, err
	}
	if input.description, err = optionalTextWithin("description", description, MaximumTaskDescriptionBytes); err != nil {
		return input, err
	}
	if maxRetries < 0 {
		return input, NewError(CodeInvalidArgument, "max_retries cannot be negative", false)
	}
	if input.ackIDs, err = canonicalMessageIDs(ackMessageIDs); err != nil {
		return input, err
	}
	if input.requestID, err = s.requestID(requestID); err != nil {
		return input, err
	}
	input.sourceTaskID = strings.TrimSpace(sourceTaskID)
	input.priority, input.maxRetries = priority, maxRetries
	return input, nil
}

func (s *Service) createWorkTask(ctx context.Context, input workTaskRequest) (Task, error) {
	var snapshot taskCreationSnapshot
	var task Task
	replayed := false
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		actor, err := s.taskActorForDedupe(tx, input.token)
		if err != nil {
			return err
		}
		input.dedupe.scope = actor.dedupeScope
		if replay, ok, err := input.dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			task, err = tx.Task(replay.ID)
			replayed = err == nil
			return err
		}
		if actor.kind == "agent" {
			actor.run, snapshot.parent, err = s.authenticateRun(tx, input.token)
			if err != nil {
				return err
			}
			actor.id, actor.runID, actor.current = actor.run.AgentID, actor.run.ID, snapshot.parent
			input.projectID = snapshot.parent.ProjectID
		}
		snapshot.actor = actor
		snapshot.project, err = tx.Project(input.projectID)
		if err != nil {
			return err
		}
		if snapshot.project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(snapshot.project.Status), snapshot.project.Version)
		}
		if input.sourceTaskID != "" {
			snapshot.source, err = tx.Task(input.sourceTaskID)
			if err != nil {
				return err
			}
			if actor.kind == "agent" && !runCanReadTask(actor.run, snapshot.parent, snapshot.source) {
				return NewError(CodeScopeDenied, "source task is outside the current run scope", false)
			}
			if err := validateSourceTask(input.projectID, snapshot.source); err != nil {
				return err
			}
		}
		if input.retryOfTaskID != "" {
			snapshot.retry, err = tx.Task(input.retryOfTaskID)
			if err != nil {
				return err
			}
			return validateRetryTarget(input.projectID, snapshot.retry)
		}
		return nil
	})
	if err != nil || replayed {
		return task, err
	}
	baseSHA, err := s.projectGit.Resolve(ctx, snapshot.project.ControlRepoPath, snapshot.project.CanonicalRef)
	if err != nil {
		return Task{}, WrapError(CodeGitInvariantViolation, "resolve canonical ref", false, err)
	}
	mutate := func() error {
		return s.repository.Transact(ctx, func(tx Transaction) error {
			actor, err := s.taskActorForDedupe(tx, input.token)
			if err != nil {
				return err
			}
			input.dedupe.scope = actor.dedupeScope
			if replay, ok, err := input.dedupe.replay(tx); err != nil {
				return err
			} else if ok {
				task, err = tx.Task(replay.ID)
				return err
			}
			var parent Task
			if actor.kind == "agent" {
				actor.run, parent, err = s.authenticateRun(tx, input.token)
				if err != nil {
					return err
				}
				actor.id, actor.runID, actor.current = actor.run.AgentID, actor.run.ID, parent
				input.projectID = parent.ProjectID
			}
			project, err := tx.Project(input.projectID)
			if err != nil {
				return err
			}
			if project.Status != ProjectActive {
				return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
			}
			if actor.kind == "agent" && (project.Version != snapshot.project.Version || project.CanonicalRef != snapshot.project.CanonicalRef) {
				return Conflict(CodeVersionConflict, "project changed while creating child task", string(project.Status), project.Version)
			}
			var source Task
			if input.sourceTaskID != "" {
				source, err = tx.Task(input.sourceTaskID)
				if err != nil {
					return err
				}
				if actor.kind == "agent" && !runCanReadTask(actor.run, parent, source) {
					return NewError(CodeScopeDenied, "source task is outside the current run scope", false)
				}
				if source.Version != snapshot.source.Version || source.TaskRef != snapshot.source.TaskRef || source.HeadSHA != snapshot.source.HeadSHA {
					return Conflict(CodeVersionConflict, "source task changed while creating task", string(source.Status), source.Version)
				}
				if err := validateSourceTask(input.projectID, source); err != nil {
					return err
				}
			}
			var retry Task
			if input.retryOfTaskID != "" {
				retry, err = tx.Task(input.retryOfTaskID)
				if err != nil {
					return err
				}
				if retry.Version != snapshot.retry.Version || retry.Status != snapshot.retry.Status {
					return Conflict(CodeVersionConflict, "retry target changed while creating task", string(retry.Status), retry.Version)
				}
				if err := validateRetryTarget(input.projectID, retry); err != nil {
					return err
				}
			}
			task, err = s.insertWorkTask(tx, input, actor, parent, source, retry, baseSHA)
			return err
		})
	}
	if input.sourceTaskID == "" {
		err = mutate()
	} else if executor, ok := s.projectGit.(TaskGit); !ok {
		err = NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	} else {
		err = executor.UseTaskRef(ctx, GitTaskRefIntent{
			ProjectID: input.projectID, ControlRepo: snapshot.project.ControlRepoPath,
			TaskRef: snapshot.source.TaskRef, ExpectedSHA: snapshot.source.HeadSHA,
		}, func(actual string) error {
			if actual != snapshot.source.HeadSHA {
				return NewError(CodeGitInvariantViolation, "source task ref changed", false)
			}
			return mutate()
		})
	}
	if err != nil && isGitInvariant(err) {
		err = WrapError(CodeGitInvariantViolation, "use source task ref", false, err)
	}
	return task, err
}

func (s *Service) insertWorkTask(
	tx Transaction, input workTaskRequest, actor taskMutationActor,
	parent, source, retry Task, baseSHA string,
) (Task, error) {
	agent, err := tx.Agent(input.agentID)
	if err != nil {
		return Task{}, err
	}
	if agent.Status == AgentArchived {
		return Task{}, Conflict(CodeInvalidState, "archived agent cannot receive a task", string(agent.Status), agent.Version)
	}
	now := s.nowText()
	if err := s.acknowledgeForActor(tx, input.ackIDs, input.projectID, actor, input.requestID, now); err != nil {
		return Task{}, err
	}
	id, err := s.requiredID("tsk")
	if err != nil {
		return Task{}, err
	}
	task := Task{
		ID: id, ProjectID: input.projectID, Kind: TaskWork, ParentTaskID: parent.ID,
		CreatedByKind: actor.kind, CreatedByID: actor.id, AssigneeAgentID: input.agentID,
		Title: input.title, Description: input.description, Priority: input.priority,
		Status: TaskQueued, NextRunAt: now, MaxRetries: input.maxRetries, BaseSHA: baseSHA,
		RetryOfTaskID: retry.ID, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if source.ID != "" {
		task.SourceTaskID, task.SourceRunID = source.ID, source.HeadRunID
		task.SourceTaskRef, task.SourceHeadSHA = source.TaskRef, source.HeadSHA
	}
	if err := tx.InsertTask(task); err != nil {
		return Task{}, err
	}
	payload := eventPayload(map[string]any{
		"kind": task.Kind, "parent_task_id": parent.ID, "base_sha": baseSHA,
		"source_task_id": source.ID, "retry_of_task_id": retry.ID,
	})
	if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.created",
		actor.kind, actor.id, actor.runID, input.requestID, "", payload, now)); err != nil {
		return Task{}, err
	}
	return task, input.dedupe.record(tx, task.ID, "", now)
}
