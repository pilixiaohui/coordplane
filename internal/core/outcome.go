package core

import (
	"context"
	"strings"
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
	switch input.Outcome {
	case OutcomeWait, OutcomeFail:
		if reason == "" {
			return OutcomeResult{}, NewError(CodeInvalidArgument, "reason is required", false)
		}
		if summary != "" || expectedHead != "" {
			return OutcomeResult{}, NewError(CodeInvalidArgument, "summary and expected_head are only valid for submit", false)
		}
	case OutcomeSubmit:
		if summary == "" || expectedHead == "" {
			return OutcomeResult{}, NewError(CodeInvalidArgument, "summary and expected_head are required for submit", false)
		}
		if reason != "" {
			return OutcomeResult{}, NewError(CodeInvalidArgument, "reason is not valid for submit", false)
		}
	default:
		return OutcomeResult{}, NewError(CodeInvalidArgument, "outcome must be wait, submit, or fail", false)
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

	var result OutcomeResult
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		actorScope := "run:" + scope.ID
		operation := "task.outcome"
		if raw, ok, err := tx.Dedupe(actorScope, operation, requestID); err != nil {
			return err
		} else if ok {
			dedupe, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			result.Run, err = tx.Run(dedupe.ID)
			if err != nil {
				return err
			}
			result.Task, err = tx.Task(dedupe.RelatedID)
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
		raw, err := encodeDedupe(run.ID, task.ID, inputHash)
		if err != nil {
			return err
		}
		if err := tx.PutDedupe(actorScope, operation, requestID, raw, now); err != nil {
			return err
		}
		result = OutcomeResult{Task: task, Run: run, Acknowledged: acknowledged}
		return nil
	})
	return result, err
}
