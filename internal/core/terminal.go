package core

import (
	"context"
	"fmt"
	"strings"
)

func (s *Service) RecordRunTerminal(ctx context.Context, input RunTerminalInput) (RunTerminalResult, error) {
	runID, err := requireText("run_id", input.RunID)
	if err != nil {
		return RunTerminalResult{}, err
	}
	if !IsRunTerminal(input.State) {
		return RunTerminalResult{}, NewError(CodeInvalidArgument, "state must be terminal", false)
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return RunTerminalResult{}, err
	}
	input.TerminalReason = boundedDurableText(input.TerminalReason, MaximumTerminalTextBytes)
	input.LastError = boundedDurableText(input.LastError, MaximumTerminalTextBytes)
	input.RuntimeErrorCode = boundedDurableText(input.RuntimeErrorCode, 256)
	input.NativeSessionID = boundedDurableText(input.NativeSessionID, 1024)
	input.OperationID = strings.TrimSpace(input.OperationID)

	var result RunTerminalResult
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		run, err := tx.Run(runID)
		if err != nil {
			return err
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if IsRunTerminal(run.State) {
			if !sameTerminalFact(run, input) {
				return Conflict(CodeInvalidState, "terminal run fact cannot change", string(run.State), run.Version)
			}
			result = RunTerminalResult{Run: run, Task: task}
			return nil
		}
		if err := ValidateRunTransition(run.State, input.State); err != nil {
			return Conflict(CodeInvalidState, "run cannot enter requested terminal state", string(run.State), run.Version)
		}
		now := s.nowText()
		runVersion, runState := run.Version, run.State
		run.State = input.State
		run.ExitCode = cloneExitCode(input.ExitCode)
		run.TerminalReason = input.TerminalReason
		run.LastError = input.LastError
		run.RuntimeErrorCode = input.RuntimeErrorCode
		if input.NativeSessionID != "" {
			run.NativeSessionID = input.NativeSessionID
		}
		if run.TokenRevokedAt == "" {
			run.TokenRevokedAt = now
		}
		run.EndedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, runState); err != nil {
			return err
		}

		redelivered, err := s.redeliverRunMessages(tx, run, requestID, input.OperationID, now)
		if err != nil {
			return err
		}
		if task.CurrentRunID == run.ID {
			if task.Status == TaskCancelled {
				expectedVersion := task.Version
				task.CurrentRunID = ""
				task.Version++
				task.UpdatedAt = now
				if err := tx.UpdateTask(task, expectedVersion, TaskCancelled); err != nil {
					return err
				}
			} else if task.Generation == run.Generation {
				if err := s.projectTerminalTask(tx, &task, run, requestID, input.OperationID, now); err != nil {
					return err
				}
			}
		}
		kind := "run." + string(run.State)
		payload := eventPayload(map[string]any{
			"reason": run.TerminalReason, "exit_code": run.ExitCode, "runtime_error_code": run.RuntimeErrorCode,
		})
		if _, err := tx.AppendEvent(event(run.ProjectID, "run", run.ID, kind, "daemon", "", run.ID, requestID, input.OperationID, payload, now)); err != nil {
			return err
		}
		result = RunTerminalResult{Run: run, Task: task, Redelivered: redelivered}
		return nil
	})
	return result, err
}

func sameTerminalFact(run Run, input RunTerminalInput) bool {
	return run.State == input.State && equalExitCode(run.ExitCode, input.ExitCode) &&
		run.TerminalReason == input.TerminalReason && run.LastError == input.LastError &&
		run.RuntimeErrorCode == input.RuntimeErrorCode &&
		(input.NativeSessionID == "" || run.NativeSessionID == input.NativeSessionID)
}

func equalExitCode(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneExitCode(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s *Service) redeliverRunMessages(tx Transaction, run Run, requestID, operationID, now string) ([]Message, error) {
	messages, err := tx.MessagesForRun(run.ID)
	if err != nil {
		return nil, err
	}
	redelivered := make([]Message, 0, len(messages))
	for _, message := range messages {
		if message.State != MessageDelivered {
			continue
		}
		expectedVersion := message.Version
		message.State = MessagePending
		message.DeliveredRunID = ""
		message.LastDeliveryError = "run terminal before acknowledgement"
		message.Version++
		exhausted := message.MaxDeliveries > 0 && message.DeliveryCount >= message.MaxDeliveries
		if exhausted {
			message.NextDeliveryAt = ""
		} else {
			message.NextDeliveryAt = runtimeRetryAt(now, message.DeliveryCount)
		}
		if err := tx.UpdateMessage(message, expectedVersion, MessageDelivered); err != nil {
			return nil, err
		}
		payload := eventPayload(map[string]any{"run_id": run.ID, "delivery_count": message.DeliveryCount})
		if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.redelivered", "daemon", "", run.ID, requestID, operationID, payload, now)); err != nil {
			return nil, err
		}
		if exhausted {
			if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.delivery_exhausted", "daemon", "", run.ID, requestID, operationID, payload, now)); err != nil {
				return nil, err
			}
		}
		redelivered = append(redelivered, message)
	}
	return redelivered, nil
}

func (s *Service) projectTerminalTask(tx Transaction, task *Task, run Run, requestID, operationID, now string) error {
	switch Outcome(run.RequestedOutcome) {
	case OutcomeWait:
		if task.Status != TaskFinishing {
			return Conflict(CodeInvalidState, "wait outcome requires a finishing task", string(task.Status), task.Version)
		}
		wakeAt, pendingWake, err := tx.PendingWakeAt(task.ID)
		if err != nil {
			return err
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.CurrentRunID = ""
		task.WaitReason = run.RequestedSummary
		task.Status = TaskWaiting
		kind := "task.waiting"
		if pendingWake {
			task.Status = TaskQueued
			task.NextRunAt = wakeAt
			if task.NextRunAt < now {
				task.NextRunAt = now
			}
			kind = "task.requeued"
		}
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, kind, "daemon", "", run.ID, requestID, operationID, eventPayload(map[string]any{"reason": task.WaitReason}), now))
		return err
	case OutcomeFail:
		if task.Status != TaskFinishing {
			return Conflict(CodeInvalidState, "fail outcome requires a finishing task", string(task.Status), task.Version)
		}
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = TaskFailed
		task.CurrentRunID = ""
		task.FailureReason = run.RequestedSummary
		clearTaskAction(task)
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(*task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.failed", "daemon", "", run.ID, requestID, operationID, eventPayload(map[string]any{"reason": task.FailureReason}), now)); err != nil {
			return err
		}
		if err := s.disposeUnresolvedMessages(tx, *task, "daemon", "", run.ID, requestID, now); err != nil {
			return err
		}
		return s.notifyParentOfChild(tx, *task, "daemon", "", run.ID, requestID, now)
	case OutcomeSubmit:
		if task.Status != TaskFinishing || task.PendingAction != "capture" || task.PendingActionRunID != run.ID {
			return Conflict(CodeActionInProgress, "submit capture intent fence changed", string(task.Status), task.Version)
		}
		return nil
	case "":
		if task.Status == TaskQueued || task.Status == TaskRunning {
			if err := s.projectRuntimeFailure(tx, task, run, requestID, now); err != nil {
				return err
			}
			if task.Status == TaskFailed {
				if err := s.disposeUnresolvedMessages(tx, *task, "daemon", "", run.ID, requestID, now); err != nil {
					return err
				}
				return s.notifyParentOfChild(tx, *task, "daemon", "", run.ID, requestID, now)
			}
			return nil
		}
		return Conflict(CodeInvalidState, "run terminal fact has no applicable task projection", string(task.Status), task.Version)
	default:
		return NewError(CodeInternal, fmt.Sprintf("unknown requested outcome %q", run.RequestedOutcome), false)
	}
}
