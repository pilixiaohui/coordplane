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
			if err != nil || project.Status != ProjectActive {
				continue
			}
			task, err := tx.Task(candidate.ID)
			if err != nil || task.Status != TaskQueued || task.CurrentRunID != "" {
				continue
			}
			agent, err := tx.Agent(task.AssigneeAgentID)
			if err != nil || agent.Status != AgentActive {
				continue
			}
			agentRuns, err := tx.LiveRunCount("", agent.ID)
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

func (s *Service) ActivateRun(ctx context.Context, runID, requestID string) (Run, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(runID))
		if err != nil {
			return err
		}
		if err := ValidateRunTransition(run.State, RunActive); err != nil {
			return Conflict(CodeInvalidState, "run cannot become active", string(run.State), run.Version)
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if task.Status != TaskQueued || task.CurrentRunID != run.ID || task.Generation != run.Generation {
			return Conflict(CodeVersionConflict, "task claim fence changed", string(task.Status), task.Version)
		}
		now := s.nowText()
		runVersion := run.Version
		run.State = RunActive
		run.StartedAt = now
		run.LastObservedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, RunStarting); err != nil {
			return err
		}
		taskVersion := task.Version
		task.Status = TaskRunning
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, taskVersion, TaskQueued); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "run", run.ID, "run.active", "daemon", "", run.ID, requestID, run.LaunchOperationID, "{}", now)); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.running", "daemon", "", run.ID, requestID, "", "{}", now))
		return err
	})
	return run, err
}

func (s *Service) InterruptRun(ctx context.Context, runID, reason, requestID string) (Run, error) {
	runID, err := requireText("run_id", runID)
	if err != nil {
		return Run{}, err
	}
	requestID, err = s.requestID(requestID)
	if err != nil {
		return Run{}, err
	}
	reason = boundedDurableText(reason, MaximumTerminalTextBytes)

	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(runID)
		if err != nil {
			return err
		}
		if IsRunTerminal(run.State) {
			if run.State == RunInterrupted && run.TerminalReason == reason {
				return nil
			}
			return Conflict(CodeInvalidState, "terminal run fact cannot change", string(run.State), run.Version)
		}
		if err := ValidateRunTransition(run.State, RunInterrupted); err != nil {
			return Conflict(CodeInvalidState, "run cannot be interrupted", string(run.State), run.Version)
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}

		now := s.nowText()
		runVersion, runState := run.Version, run.State
		run.State = RunInterrupted
		run.TerminalReason = reason
		if run.TokenRevokedAt == "" {
			run.TokenRevokedAt = now
		}
		run.EndedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, runState); err != nil {
			return err
		}
		if task.CurrentRunID == run.ID && task.Generation == run.Generation &&
			(task.Status == TaskQueued || task.Status == TaskRunning) {
			if err := s.projectRuntimeFailure(tx, &task, run, requestID, now); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(event(
			run.ProjectID, "run", run.ID, "run.interrupted", "daemon", "", run.ID,
			requestID, "", eventPayload(map[string]any{"reason": run.TerminalReason}), now,
		))
		return err
	})
	return run, err
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

func (s *Service) CurrentTask(ctx context.Context, token string) (Task, error) {
	var task Task
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		_, current, err := s.authenticateRun(tx, token)
		if err != nil {
			return err
		}
		task = current
		return nil
	})
	return task, err
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

func (s *Service) AgentMessageToBoss(ctx context.Context, input AgentMessageInput) (Message, error) {
	body, err := requireText("body", input.Body)
	if err != nil {
		return Message{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Message{}, err
	}
	replyToID := strings.TrimSpace(input.ReplyTo)
	inputHash, err := inputFingerprint(struct{ Body, ReplyToID string }{body, replyToID})
	if err != nil {
		return Message{}, err
	}
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scopedRun, err := tx.RunByTokenHash(hashToken(input.Token))
		if err != nil {
			if IsCode(err, CodeNotFound) {
				return NewError(CodeScopeDenied, "run scope is not valid", false)
			}
			return err
		}
		actorScope := "run:" + scopedRun.ID
		if raw, ok, err := tx.Dedupe(actorScope, "message.send", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			message, err = tx.Message(result.ID)
			return err
		}
		run, task, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, "message"); err != nil {
			return err
		}
		if replyToID != "" {
			repliedTo, err := tx.Message(replyToID)
			if err != nil {
				return err
			}
			if repliedTo.ProjectID != task.ProjectID {
				return NewError(CodeScopeDenied, "reply message belongs to another project", false)
			}
		}
		messageID, err := s.requiredID("msg")
		if err != nil {
			return err
		}
		now := s.nowText()
		message = Message{
			ID: messageID, ProjectID: task.ProjectID, TaskID: task.ID,
			SenderKind: "agent", SenderID: run.AgentID, RecipientKind: "boss",
			ReplyToMessageID: replyToID, Body: body,
			State: MessagePending, MaxDeliveries: 1, NextDeliveryAt: now,
			IdempotencyKey: requestID, Version: 1, CreatedAt: now,
		}
		if err := tx.InsertMessage(message); err != nil {
			return err
		}
		if _, err = tx.AppendEvent(event(task.ProjectID, "message", message.ID, "message.created", "agent", run.AgentID, run.ID, requestID, "", "{}", now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(message.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe(actorScope, "message.send", requestID, raw, now)
	})
	return message, err
}

func (s *Service) authenticateRun(tx Transaction, token string) (Run, Task, error) {
	if strings.TrimSpace(token) == "" {
		return Run{}, Task{}, NewError(CodeScopeDenied, "run token is required", false)
	}
	run, err := tx.RunByTokenHash(hashToken(token))
	if err != nil {
		if IsCode(err, CodeNotFound) {
			return Run{}, Task{}, NewError(CodeScopeDenied, "run scope is not valid", false)
		}
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
