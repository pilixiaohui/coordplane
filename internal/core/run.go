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
	return s.RecordRunTerminal(ctx, TerminalRunInput{
		RunID: runID, State: RunInterrupted, Reason: reason, RequestID: requestID,
	})
}

// RecordRunTerminal accepts only daemon/runtime-owned process evidence. The
// Run fact, Task projection, current_run_id fence, and Events commit together.
func (s *Service) RecordRunTerminal(ctx context.Context, input TerminalRunInput) (Run, error) {
	runID, err := requireText("run_id", input.RunID)
	if err != nil {
		return Run{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	if !IsRunTerminal(input.State) {
		return Run{}, NewError(CodeInvalidArgument, "state must be a terminal Run state", false)
	}
	input.Reason = boundedDurableText(input.Reason, MaximumTerminalTextBytes)
	input.LastError = boundedDurableText(input.LastError, MaximumTerminalTextBytes)

	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(runID)
		if err != nil {
			return err
		}
		if IsRunTerminal(run.State) {
			if sameTerminalFact(run, input) {
				return nil
			}
			return Conflict(CodeInvalidState, "terminal run fact cannot change", string(run.State), run.Version)
		}
		if err := ValidateRunTransition(run.State, input.State); err != nil {
			return Conflict(CodeInvalidState, "run cannot enter requested terminal state", string(run.State), run.Version)
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}

		now := s.nowText()
		runVersion, runState := run.Version, run.State
		run.State = input.State
		run.ExitCode = input.ExitCode
		run.TerminalReason = input.Reason
		run.LastError = input.LastError
		if run.TokenRevokedAt == "" {
			run.TokenRevokedAt = now
		}
		run.EndedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, runState); err != nil {
			return err
		}
		if err := s.projectTerminalRunToTask(tx, &task, run, requestID, now); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(
			run.ProjectID, "run", run.ID, "run."+string(run.State), "daemon", "", run.ID,
			requestID, "", eventPayload(map[string]any{
				"exit_code": input.ExitCode, "reason": run.TerminalReason, "last_error": run.LastError,
			}), now,
		))
		return err
	})
	return run, err
}

func sameTerminalFact(run Run, input TerminalRunInput) bool {
	if run.State != input.State || run.TerminalReason != strings.TrimSpace(input.Reason) || run.LastError != strings.TrimSpace(input.LastError) {
		return false
	}
	if run.ExitCode == nil || input.ExitCode == nil {
		return run.ExitCode == nil && input.ExitCode == nil
	}
	return *run.ExitCode == *input.ExitCode
}

func (s *Service) projectTerminalRunToTask(tx Transaction, task *Task, run Run, requestID, now string) error {
	if task.CurrentRunID != run.ID || task.Generation != run.Generation {
		return nil
	}
	messageTarget := MessagePending
	if run.RequestedOutcome == "fail" || (run.RequestedOutcome == "" && runtimeRetryDecision(task.RetryCount, task.MaxRetries).status == TaskFailed) {
		messageTarget = MessageCancelled
	}
	if err := settleRunMessages(tx, *task, run, messageTarget, requestID, now); err != nil {
		return err
	}
	if task.Status == TaskFinishing && run.RequestedOutcome != "" {
		return s.projectRequestedOutcome(tx, task, run, requestID, now)
	}
	if task.Status != TaskQueued && task.Status != TaskRunning {
		return nil
	}
	return s.projectRuntimeFailure(tx, task, run, requestID, now)
}

func settleRunMessages(tx Transaction, task Task, run Run, target MessageState, requestID, now string) error {
	messages, err := tx.MessagesForTask(task.ID)
	if err != nil {
		return err
	}
	for _, message := range messages {
		switch target {
		case MessagePending:
			if message.State != MessageDelivered || message.DeliveredRunID != run.ID {
				continue
			}
		case MessageCancelled:
			if message.RecipientKind != "agent" ||
				(message.State != MessagePending && message.State != MessageDelivered) {
				continue
			}
		default:
			return NewError(CodeInternal, "unsupported terminal message projection", false)
		}
		if err := ValidateMessageTransition(message.State, target); err != nil {
			return err
		}
		expectedVersion, expectedState := message.Version, message.State
		message.State = target
		message.Version++
		message.LastDeliveryError = "TASK_FAILED"
		kind := "message.cancelled"
		payload := eventPayload(map[string]any{"terminal_run_id": run.ID, "reason": "task_failed"})
		if target == MessagePending {
			message.DeliveredRunID = ""
			message.DeliveredAt = ""
			message.NextDeliveryAt = now
			message.LastDeliveryError = "RUN_TERMINATED_BEFORE_ACK"
			kind = "message.redelivered"
			payload = eventPayload(map[string]any{"terminal_run_id": run.ID})
		}
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(
			message.ProjectID, "message", message.ID, kind, "daemon", "", run.ID,
			requestID, "", payload, now,
		)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) projectRequestedOutcome(tx Transaction, task *Task, run Run, requestID, now string) error {
	switch run.RequestedOutcome {
	case "wait":
		target := TaskWaiting
		messages, err := tx.MessagesForTask(task.ID)
		if err != nil {
			return err
		}
		for _, message := range messages {
			if message.Wake && (message.State == MessagePending || message.State == MessageDelivered) {
				target = TaskQueued
				break
			}
		}
		if err := ValidateTaskTransition(task.Kind, task.Status, target); err != nil {
			return err
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = target
		task.CurrentRunID = ""
		if target == TaskQueued {
			task.NextRunAt = now
			task.WaitReason = ""
		}
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		eventKind := "task.waiting"
		payload := eventPayload(map[string]any{"reason": task.WaitReason})
		if target == TaskQueued {
			eventKind = "task.requeued"
			payload = `{"reason":"pending_wake"}`
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, eventKind, "daemon", "", run.ID, requestID, "", payload, now))
		return err
	case "fail":
		if err := ValidateTaskTransition(task.Kind, task.Status, TaskFailed); err != nil {
			return err
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = TaskFailed
		task.CurrentRunID = ""
		if task.FailureReason == "" {
			task.FailureReason = run.TerminalReason
		}
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.failed", "daemon", "", run.ID, requestID, "", eventPayload(map[string]any{"reason": task.FailureReason}), now))
		return err
	case "submit":
		if validCaptureIntent(*task, run) {
			return nil
		}
		return s.failInvalidOutcomeIntent(tx, task, run, requestID, now)
	default:
		return s.failInvalidOutcomeIntent(tx, task, run, requestID, now)
	}
}

func validCaptureIntent(task Task, run Run) bool {
	return task.PendingAction == "capture" && task.PendingActionID != "" &&
		task.PendingActionVersion == task.Version && task.PendingActionRunID == run.ID &&
		task.PendingExpectedSHA != "" && task.PendingExpectedSHA == run.ExpectedHead
}

func (s *Service) failInvalidOutcomeIntent(tx Transaction, task *Task, run Run, requestID, now string) error {
	if err := ValidateTaskTransition(task.Kind, task.Status, TaskFailed); err != nil {
		return err
	}
	expectedVersion, expectedStatus := task.Version, task.Status
	task.Status = TaskFailed
	task.CurrentRunID = ""
	task.FailureReason = string(CodeGitInvariantViolation) + ": invalid or missing capture intent"
	clearTaskPendingAction(task)
	task.Version++
	task.UpdatedAt = now
	if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
		return err
	}
	_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.failed", "daemon", "", run.ID, requestID, "", eventPayload(map[string]any{"reason": task.FailureReason}), now))
	return err
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

func clearTaskPendingAction(task *Task) {
	task.PendingAction = ""
	task.PendingActionID = ""
	task.PendingActionVersion = 0
	task.PendingActionRunID = ""
	task.PendingExpectedSHA = ""
	task.PendingTargetSHA = ""
	task.PendingStartedAt = ""
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

func (s *Service) RequestOutcome(ctx context.Context, input OutcomeInput) (Task, error) {
	reason, err := optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return Task{}, err
	}
	summary, err := optionalTextWithin("summary", input.Summary, MaximumOutcomeTextBytes)
	if err != nil {
		return Task{}, err
	}
	expectedHead, err := optionalTextWithin("expected_head", input.ExpectedHead, 256)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	operation := strings.TrimSpace(input.Outcome)
	inputHash, err := inputFingerprint(struct {
		Outcome, Reason, Summary, ExpectedHead string
	}{operation, reason, summary, expectedHead})
	if err != nil {
		return Task{}, err
	}
	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scopedRun, err := tx.RunByTokenHash(hashToken(input.Token))
		if err != nil {
			if IsCode(err, CodeNotFound) {
				return NewError(CodeScopeDenied, "run scope is not valid", false)
			}
			return err
		}
		actorScope := "run:" + scopedRun.ID
		operationKey := "task." + operation
		if raw, ok, err := tx.Dedupe(actorScope, operationKey, requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			task, err = tx.Task(result.ID)
			return err
		}
		run, current, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		if operation != "wait" && operation != "submit" && operation != "fail" {
			return NewError(CodeInvalidArgument, "outcome must be wait, submit, or fail", false)
		}
		if err := ValidateTaskOperation(current.Kind, operation); err != nil {
			return Conflict(CodeInvalidState, "task kind does not support outcome", string(current.Status), current.Version)
		}
		if current.Status != TaskRunning {
			return Conflict(CodeInvalidState, "task is not running", string(current.Status), current.Version)
		}
		if operation == "submit" && expectedHead == "" {
			return NewError(CodeInvalidArgument, "expected_head is required for submit", false)
		}
		if (operation == "wait" || operation == "fail") && reason == "" {
			return NewError(CodeInvalidArgument, "reason is required", false)
		}
		if err := ValidateTaskTransition(current.Kind, current.Status, TaskFinishing); err != nil {
			return Conflict(CodeInvalidState, "task cannot enter finishing", string(current.Status), current.Version)
		}
		operationID := ""
		if operation == "submit" {
			operationID, err = s.requiredID("op")
			if err != nil {
				return err
			}
		}
		now := s.nowText()
		runVersion := run.Version
		run.RequestedOutcome = operation
		run.RequestedSummary = summary
		run.ExpectedHead = expectedHead
		run.RequestedAt = now
		run.TokenRevokedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, RunActive); err != nil {
			return err
		}
		taskVersion := current.Version
		current.Status = TaskFinishing
		switch operation {
		case "wait":
			current.WaitReason = reason
		case "fail":
			current.FailureReason = reason
		case "submit":
			current.ResultSummary = summary
			current.PendingAction = "capture"
			current.PendingActionID = operationID
			current.PendingActionRunID = run.ID
			current.PendingExpectedSHA = expectedHead
			current.PendingStartedAt = now
		}
		current.Version++
		if operation == "submit" {
			current.PendingActionVersion = current.Version
		}
		current.UpdatedAt = now
		if err := tx.UpdateTask(current, taskVersion, TaskRunning); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(current.ProjectID, "run", run.ID, "run.outcome_requested", "agent", run.AgentID, run.ID, requestID, operationID, eventPayload(map[string]any{"outcome": operation}), now)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(current.ProjectID, "task", current.ID, "task.finishing", "agent", run.AgentID, run.ID, requestID, operationID, eventPayload(map[string]any{"outcome": operation}), now)); err != nil {
			return err
		}
		if operation == "submit" {
			if _, err := tx.AppendEvent(event(current.ProjectID, "task", current.ID, "git.task_ref_capture_requested", "agent", run.AgentID, run.ID, requestID, operationID, eventPayload(map[string]any{"expected_head": current.PendingExpectedSHA}), now)); err != nil {
				return err
			}
		}
		raw, err := encodeDedupe(current.ID, "", inputHash)
		if err != nil {
			return err
		}
		if err := tx.PutDedupe(actorScope, operationKey, requestID, raw, now); err != nil {
			return err
		}
		task = current
		return nil
	})
	return task, err
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
