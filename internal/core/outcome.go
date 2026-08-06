package core

import (
	"context"
	"strings"
)

type outcomeFields struct {
	reason, summary, expectedHead string
}

type outcomeSnapshot struct {
	task Task
	run  Run
}

func (s *Service) RequestOutcome(ctx context.Context, input OutcomeInput) (OutcomeResult, error) {
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return OutcomeResult{}, err
	}
	fields, err := s.validateOutcomeRequest(input)
	if err != nil {
		return OutcomeResult{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return OutcomeResult{}, err
	}
	// The dedupe input hash deliberately covers the original caller-supplied
	// expected_head (possibly a short prefix), not the expanded full SHA, so an
	// expansion failure never becomes a poisoned idempotency key.
	inputHash, err := inputFingerprint(struct {
		Outcome                           Outcome
		Reason, Summary, Expected, AckIDs string
	}{input.Outcome, fields.reason, fields.summary, fields.expectedHead, strings.Join(ackIDs, "\x00")})
	if err != nil {
		return OutcomeResult{}, err
	}
	dedupe := requestDedupe{"", "task.outcome", requestID, inputHash}

	var snapshot outcomeSnapshot
	var result OutcomeResult
	replayed := false
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		dedupe.scope = "run:" + scope.ID
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
			replayed = err == nil
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
		snapshot = outcomeSnapshot{task: task, run: run}
		return nil
	})
	if err != nil || replayed {
		return result, err
	}

	expandedHead := ""
	if input.Outcome == OutcomeSubmit {
		expandedHead, err = s.resolveSubmitHead(ctx, snapshot, fields)
		if err != nil {
			return OutcomeResult{}, err
		}
	}

	err = s.repository.Transact(ctx, func(tx Transaction) error {
		result, err = s.applyOutcome(tx, input, ackIDs, snapshot, fields, expandedHead, requestID, dedupe)
		return err
	})
	return result, err
}

func (s *Service) validateOutcomeRequest(input OutcomeInput) (outcomeFields, error) {
	reason, err := optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return outcomeFields{}, err
	}
	summary, err := optionalTextWithin("summary", input.Summary, MaximumOutcomeTextBytes)
	if err != nil {
		return outcomeFields{}, err
	}
	expectedHead, err := optionalTextWithin("expected_head", input.ExpectedHead, 256)
	if err != nil {
		return outcomeFields{}, err
	}
	if err := validateOutcomeFields(input.Outcome, reason, summary, expectedHead); err != nil {
		return outcomeFields{}, err
	}
	return outcomeFields{reason: reason, summary: summary, expectedHead: expectedHead}, nil
}

// resolveSubmitHead resolves a short expected_head prefix to its full commit SHA
// outside the write transaction. Failure is a clear retryable error: the Run
// token stays valid and no Task state changes, so the Agent can correct the
// prefix and resubmit. Only capture itself reports an invariant violation.
func (s *Service) resolveSubmitHead(ctx context.Context, snapshot outcomeSnapshot, fields outcomeFields) (string, error) {
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return "", NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	var source *GitSource
	if snapshot.task.SourceTaskID != "" {
		if snapshot.task.SourceRunID == "" || snapshot.task.SourceTaskRef == "" || snapshot.task.SourceHeadSHA == "" {
			return "", NewError(CodeGitInvariantViolation, "source task workspace inputs are incomplete", false)
		}
		source = &GitSource{
			TaskID: snapshot.task.SourceTaskID, RunID: snapshot.task.SourceRunID,
			TaskRef: snapshot.task.SourceTaskRef, HeadSHA: snapshot.task.SourceHeadSHA,
		}
	}
	full, expandErr := executor.ExpandHead(ctx, GitExpandHeadIntent{
		ProjectID:     snapshot.task.ProjectID,
		TaskID:        snapshot.task.ID,
		RunID:         snapshot.run.ID,
		WorkspacePath: snapshot.run.WorkspacePath,
		BaseSHA:       snapshot.task.BaseSHA,
		ExpectedHead:  fields.expectedHead,
		Source:        source,
	})
	if expandErr != nil {
		return "", WrapError(CodeInvalidArgument, "expand expected_head in workspace: "+expandErr.Error(), false, expandErr)
	}
	return full, nil
}

func (s *Service) applyOutcome(tx Transaction, input OutcomeInput, ackIDs []string, snapshot outcomeSnapshot, fields outcomeFields, expandedHead, requestID string, dedupe requestDedupe) (OutcomeResult, error) {
	scope, err := scopedRun(tx, input.Token)
	if err != nil {
		return OutcomeResult{}, err
	}
	dedupe.scope = "run:" + scope.ID
	if replay, ok, err := dedupe.replay(tx); err != nil {
		return OutcomeResult{}, err
	} else if ok {
		var result OutcomeResult
		result.Run, err = tx.Run(replay.ID)
		if err != nil {
			return OutcomeResult{}, err
		}
		result.Task, err = tx.Task(replay.RelatedID)
		if err != nil {
			return OutcomeResult{}, err
		}
		result.Acknowledged, err = loadMessages(tx, ackIDs)
		return result, err
	}
	run, task, err := s.authenticateRun(tx, input.Token)
	if err != nil {
		return OutcomeResult{}, err
	}
	if task.Version != snapshot.task.Version || task.Status != snapshot.task.Status ||
		run.Version != snapshot.run.Version || run.State != snapshot.run.State {
		return OutcomeResult{}, Conflict(CodeVersionConflict, "task changed while resolving expected head", string(task.Status), task.Version)
	}
	if err := ValidateTaskOperation(task.Kind, string(input.Outcome)); err != nil {
		return OutcomeResult{}, err
	}
	if task.PendingAction != "" {
		return OutcomeResult{}, Conflict(CodeActionInProgress, "task action is in progress", string(task.Status), task.Version)
	}
	now := s.nowText()
	acknowledged, err := s.acknowledgeAgentMessages(tx, ackIDs, task.ProjectID, run.AgentID, run.ID, requestID, now)
	if err != nil {
		return OutcomeResult{}, err
	}

	runVersion, runState := run.Version, run.State
	run.RequestedOutcome = string(input.Outcome)
	if input.Outcome == OutcomeSubmit {
		run.RequestedSummary = fields.summary
		run.ExpectedHead = expandedHead
	} else {
		run.RequestedSummary = fields.reason
		run.ExpectedHead = ""
	}
	run.RequestedAt = now
	run.TokenRevokedAt = now
	run.Version++
	if err := tx.UpdateRun(run, runVersion, runState); err != nil {
		return OutcomeResult{}, err
	}

	taskVersion, taskStatus := task.Version, task.Status
	task.Status = TaskFinishing
	task.Version++
	task.UpdatedAt = now
	operationID := ""
	if input.Outcome == OutcomeSubmit {
		operationID, err = s.requiredID("op")
		if err != nil {
			return OutcomeResult{}, err
		}
		task.PendingAction = "capture"
		task.PendingActionID = operationID
		task.PendingActionVersion = task.Version
		task.PendingActionRunID = run.ID
		task.PendingExpectedSHA = expandedHead
		task.PendingStartedAt = now
	}
	if err := tx.UpdateTask(task, taskVersion, taskStatus); err != nil {
		return OutcomeResult{}, err
	}
	payload := eventPayload(map[string]any{"outcome": input.Outcome})
	if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.finishing", "agent", run.AgentID, run.ID, requestID, operationID, payload, now)); err != nil {
		return OutcomeResult{}, err
	}
	if _, err := tx.AppendEvent(event(task.ProjectID, "run", run.ID, "run.outcome_requested", "agent", run.AgentID, run.ID, requestID, operationID, payload, now)); err != nil {
		return OutcomeResult{}, err
	}
	if input.Outcome == OutcomeSubmit {
		capturePayload := eventPayload(map[string]any{"expected_head": expandedHead})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.submit_requested", "agent", run.AgentID, run.ID, requestID, operationID, capturePayload, now)); err != nil {
			return OutcomeResult{}, err
		}
	}
	if err := dedupe.record(tx, run.ID, task.ID, now); err != nil {
		return OutcomeResult{}, err
	}
	return OutcomeResult{Task: task, Run: run, Acknowledged: acknowledged}, nil
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
