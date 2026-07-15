package core

import (
	"context"
	"strings"

	"coordplane/internal/perfobs"
)

func (s *Service) RequestOutcome(ctx context.Context, input OutcomeInput) (OutcomeResult, error) {
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return OutcomeResult{}, err
	}
	reason, err := optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return OutcomeResult{}, err
	}
	summary, err := optionalTextWithin("summary", input.Summary, MaximumOutcomeTextBytes)
	if err != nil {
		return OutcomeResult{}, err
	}
	expectedHead, err := optionalTextWithin("expected_head", input.ExpectedHead, 256)
	if err != nil {
		return OutcomeResult{}, err
	}
	if err := validateOutcomeFields(input.Outcome, reason, summary, expectedHead); err != nil {
		return OutcomeResult{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return OutcomeResult{}, err
	}
	inputHash, err := inputFingerprint(struct {
		Outcome                           Outcome
		Reason, Summary, Expected, AckIDs string
	}{input.Outcome, reason, summary, expectedHead, strings.Join(ackIDs, "\x00")})
	if err != nil {
		return OutcomeResult{}, err
	}
	dedupe := requestDedupe{"", "task.outcome", requestID, inputHash}

	var result OutcomeResult
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		actorScope := "run:" + scope.ID
		dedupe.scope = actorScope
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			result.Run, err = tx.Run(replay.ID)
			if err != nil {
				return err
			}
			result.Task, err = tx.Task(replay.RelatedID)
			if err != nil {
				return err
			}
			result.Acknowledged, err = loadMessages(tx, ackIDs)
			return err
		}
		run, task, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		if err := ValidateTaskOperation(task.Kind, string(input.Outcome)); err != nil {
			return err
		}
		if task.PendingAction != "" {
			return Conflict(CodeActionInProgress, "task action is in progress", string(task.Status), task.Version)
		}
		now := s.nowText()
		acknowledged, err := s.acknowledgeAgentMessages(tx, ackIDs, task.ProjectID, run.AgentID, run.ID, requestID, now)
		if err != nil {
			return err
		}

		runVersion, runState := run.Version, run.State
		run.RequestedOutcome = string(input.Outcome)
		if input.Outcome == OutcomeSubmit {
			run.RequestedSummary = summary
			run.ExpectedHead = expectedHead
		} else {
			run.RequestedSummary = reason
			run.ExpectedHead = ""
		}
		run.RequestedAt = now
		run.TokenRevokedAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, runState); err != nil {
			return err
		}

		taskVersion, taskStatus := task.Version, task.Status
		task.Status = TaskFinishing
		task.Version++
		task.UpdatedAt = now
		operationID := ""
		if input.Outcome == OutcomeSubmit {
			operationID, err = s.requiredID("op")
			if err != nil {
				return err
			}
			task.PendingAction = "capture"
			task.PendingActionID = operationID
			task.PendingActionVersion = task.Version
			task.PendingActionRunID = run.ID
			task.PendingExpectedSHA = expectedHead
			task.PendingStartedAt = now
		}
		if err := tx.UpdateTask(task, taskVersion, taskStatus); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"outcome": input.Outcome})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.finishing", "agent", run.AgentID, run.ID, requestID, operationID, payload, now)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "run", run.ID, "run.outcome_requested", "agent", run.AgentID, run.ID, requestID, operationID, payload, now)); err != nil {
			return err
		}
		if input.Outcome == OutcomeSubmit {
			capturePayload := eventPayload(map[string]any{"expected_head": expectedHead})
			if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.submit_requested", "agent", run.AgentID, run.ID, requestID, operationID, capturePayload, now)); err != nil {
				return err
			}
		}
		if err := dedupe.record(tx, run.ID, task.ID, now); err != nil {
			return err
		}
		result = OutcomeResult{Task: task, Run: run, Acknowledged: acknowledged}
		return nil
	})
	if err == nil {
		fields := perfobs.Fields{
			RequestID: requestID, OperationID: result.Task.PendingActionID,
			ProjectID: result.Task.ProjectID, TaskID: result.Task.ID, RunID: result.Run.ID,
		}
		perfobs.Point("core.outcome.accepted_commit", fields, "success")
		if result.Task.PendingActionID != "" {
			perfobs.StartStage("git.capture.freeze", result.Run.ID, fields)
		}
	}
	return result, err
}

func validateOutcomeFields(outcome Outcome, reason, summary, expectedHead string) error {
	switch outcome {
	case OutcomeWait, OutcomeFail:
		if reason == "" {
			return NewError(CodeInvalidArgument, "reason is required", false)
		}
		if summary != "" || expectedHead != "" {
			return NewError(CodeInvalidArgument, "summary and expected_head are only valid for submit", false)
		}
	case OutcomeSubmit:
		if summary == "" || expectedHead == "" {
			return NewError(CodeInvalidArgument, "summary and expected_head are required for submit", false)
		}
		if reason != "" {
			return NewError(CodeInvalidArgument, "reason is not valid for submit", false)
		}
	default:
		return NewError(CodeInvalidArgument, "outcome must be wait, submit, or fail", false)
	}
	return nil
}
