package core

import (
	"context"
	"strings"
	"time"
)

func (s *Service) ClaimNext(ctx context.Context, projectID string) (Claim, bool, error) {
	var claim Claim
	claimed := false
	nowLimit := s.nowText()
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		tasks, err := tx.RunnableTasks(strings.TrimSpace(projectID))
		if err != nil {
			return err
		}
		sortRunnable(tasks)
		for _, candidate := range tasks {
			if candidate.NextRunAt > nowLimit {
				continue
			}
			project, err := tx.Project(candidate.ProjectID)
			if err != nil {
				return err
			}
			if project.Status != ProjectActive {
				continue
			}
			task, err := tx.Task(candidate.ID)
			if err != nil {
				return err
			}
			if task.Status != TaskQueued || task.CurrentRunID != "" {
				continue
			}
			agent, err := tx.Agent(task.AssigneeAgentID)
			if err != nil {
				return err
			}
			if agent.Status != AgentActive {
				continue
			}
			if _, allowed := s.adapters[agent.AdapterID]; !allowed {
				return NewError(CodeRuntimeInvariantViolation, "queued task references an unregistered adapter", false)
			}
			agentRuns, err := tx.AgentRuntimeOccupancy(agent.ID)
			if err != nil {
				return err
			}
			globalRuns, err := tx.LiveRunCount("", "")
			if err != nil {
				return err
			}
			if agentRuns != 0 || globalRuns >= s.maxRuns {
				continue
			}
			runID, err := s.requiredID("run")
			if err != nil {
				return err
			}
			token, err := s.requiredID("token")
			if err != nil {
				return err
			}
			operationID, err := s.requiredID("op")
			if err != nil {
				return err
			}
			now := s.nowText()
			expectedVersion := task.Version
			task.Generation++
			task.CurrentRunID = runID
			task.Version++
			task.UpdatedAt = now
			run := Run{
				ID: runID, ProjectID: task.ProjectID, TaskID: task.ID, AgentID: agent.ID,
				Generation: task.Generation, AdapterID: agent.AdapterID, Image: agent.Image,
				State: RunStarting, TokenHash: hashToken(token), CleanupState: "not_needed",
				LaunchOperationID: operationID, LaunchPhase: "intent", LaunchMode: "start",
				ContainerName: "coordplane-run-" + runID, Version: 1, CreatedAt: now,
			}
			if err := tx.InsertRun(run); err != nil {
				return err
			}
			if err := tx.UpdateTask(task, expectedVersion, TaskQueued); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.claimed", "daemon", "", run.ID, "", operationID, eventPayload(map[string]any{"generation": task.Generation}), now)); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(task.ProjectID, "run", run.ID, "run.created", "daemon", "", run.ID, "", operationID, "{}", now)); err != nil {
				return err
			}
			claim = Claim{Task: task, Run: run, Token: token}
			claimed = true
			return nil
		}
		return nil
	})
	return claim, claimed, err
}

func (s *Service) projectRuntimeFailure(tx Transaction, task *Task, run Run, requestID, now string) error {
	decision := runtimeRetryDecision(task.RetryCount, task.MaxRetries)
	if decision.status != task.Status {
		if err := ValidateTaskTransition(task.Kind, task.Status, decision.status); err != nil {
			return err
		}
	}
	expectedVersion, expectedStatus := task.Version, task.Status
	task.Status = decision.status
	task.CurrentRunID = ""
	task.RetryCount = decision.retryCount
	task.FailureReason = runtimeFailureReason(run)
	if decision.status == TaskQueued {
		task.NextRunAt = runtimeRetryAt(now, task.RetryCount)
	}
	task.Version++
	task.UpdatedAt = now
	if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
		return err
	}
	eventKind := "task.failed"
	if decision.status == TaskQueued {
		eventKind = "task.requeued"
	}
	_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, eventKind, "daemon", "", run.ID, requestID, "", eventPayload(map[string]any{
		"reason": task.FailureReason, "retry_count": task.RetryCount, "max_retries": task.MaxRetries,
	}), now))
	return err
}

func runtimeFailureReason(run Run) string {
	code := "RUN_" + strings.ToUpper(string(run.State))
	if run.State == RunExited {
		code = "NO_TASK_OUTCOME"
	} else if run.State == RunFailed {
		code = "RUN_START_FAILED"
	}
	detail := strings.TrimSpace(run.TerminalReason)
	if detail == "" {
		detail = strings.TrimSpace(run.LastError)
	}
	if detail == "" {
		return code
	}
	return code + ": " + detail

}

func runtimeRetryAt(now string, retryCount int) string {
	instant, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return now
	}
	shift := retryCount - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 5 {
		shift = 5
	}
	delay := time.Second * time.Duration(1<<shift)
	return instant.Add(delay).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (s *Service) CurrentTask(ctx context.Context, token string) (CurrentTaskResult, error) {
	var result CurrentTaskResult
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		run, current, err := s.authenticateRun(tx, token)
		if err != nil {
			return err
		}
		messages, err := tx.MessagesForRecipient("agent", run.AgentID)
		if err != nil {
			return err
		}
		unread := 0
		for _, message := range messages {
			if message.ProjectID == current.ProjectID && (message.State == MessagePending || message.State == MessageDelivered) {
				unread++
			}
		}
		result = CurrentTaskResult{Task: current, Run: run, UnreadMessageCount: unread}
		return nil
	})
	return result, err
}

func (s *Service) Progress(ctx context.Context, input ProgressInput) (Event, error) {
	summary, err := requireText("summary", input.Summary)
	if err != nil {
		return Event{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Event{}, err
	}
	var progress Event
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		run, task, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		progress, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.progress", "agent", run.AgentID, run.ID, requestID, "", eventPayload(map[string]any{"summary": summary}), s.nowText()))
		return err
	})
	return progress, err
}

func (s *Service) authenticateRun(tx Transaction, token string) (Run, Task, error) {
	run, err := scopedRun(tx, token)
	if err != nil {
		return Run{}, Task{}, err
	}
	task, err := tx.Task(run.TaskID)
	if err != nil {
		return Run{}, Task{}, err
	}
	if run.TokenRevokedAt != "" || IsRunTerminal(run.State) || task.CurrentRunID != run.ID || task.Generation != run.Generation || task.AssigneeAgentID != run.AgentID {
		return Run{}, Task{}, Conflict(CodeStaleRun, "run token is stale", string(run.State), run.Version)
	}
	if run.State == RunStarting {
		return Run{}, Task{}, Conflict(CodeRunStarting, "run is still starting", string(run.State), run.Version)
	}
	if run.State != RunActive || task.Status != TaskRunning {
		return Run{}, Task{}, Conflict(CodeStaleRun, "run scope no longer owns a running task", string(run.State), run.Version)
	}
	return run, task, nil
}
