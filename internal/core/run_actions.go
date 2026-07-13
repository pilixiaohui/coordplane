package core

import (
	"context"
	"strings"
)

func (s *Service) RequestRunStop(ctx context.Context, input RunStopInput) (Run, error) {
	return s.requestRunStop(ctx, input, "boss")
}

func (s *Service) RequestRuntimeStop(ctx context.Context, input RunStopInput) (Run, error) {
	return s.requestRunStop(ctx, input, "daemon")
}

func (s *Service) requestRunStop(ctx context.Context, input RunStopInput, actor string) (Run, error) {
	runID, err := requireText("run_id", input.RunID)
	if err != nil {
		return Run{}, err
	}
	reason := boundedDurableText(input.Reason, MaximumTerminalTextBytes)
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	requestedOperationID := strings.TrimSpace(input.OperationID)
	inputHash, err := inputFingerprint(struct{ RunID, Reason, OperationID string }{runID, reason, requestedOperationID})
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		actorScope := actor
		if raw, ok, err := tx.Dedupe(actorScope, "run.stop", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			run, err = tx.Run(result.ID)
			return err
		}
		run, err = tx.Run(runID)
		if err != nil {
			return err
		}
		if !IsRunLive(run.State) {
			return Conflict(CodeInvalidState, "terminal run cannot be stopped", string(run.State), run.Version)
		}
		now := s.nowText()
		if run.StopRequestedAt == "" {
			operationID := requestedOperationID
			if operationID == "" {
				operationID, err = s.requiredID("op")
				if err != nil {
					return err
				}
			}
			expectedVersion, expectedState := run.Version, run.State
			run.StopRequestedAt = now
			run.StopReason = reason
			run.StopOperationID = operationID
			if run.TokenRevokedAt == "" {
				run.TokenRevokedAt = now
			}
			run.Version++
			if err := tx.UpdateRun(run, expectedVersion, expectedState); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.stop_requested", actor, "", run.ID, requestID, operationID, eventPayload(map[string]any{"reason": reason}), now)); err != nil {
				return err
			}
		} else if run.StopReason != reason || (requestedOperationID != "" && run.StopOperationID != requestedOperationID) {
			return Conflict(CodeActionInProgress, "a different run stop is already requested", string(run.State), run.Version)
		}
		raw, err := encodeDedupe(run.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe(actorScope, "run.stop", requestID, raw, now)
	})
	return run, err
}
