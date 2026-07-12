package core

import (
	"context"
	"strings"
)

func (s *Service) RecordMessagesDelivered(ctx context.Context, input MessageDeliveryInput) ([]Message, error) {
	runID, err := requireText("run_id", input.RunID)
	if err != nil {
		return nil, err
	}
	messageIDs, err := canonicalMessageIDs(input.MessageIDs)
	if err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return nil, NewError(CodeInvalidArgument, "at least one message ID is required", false)
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return nil, err
	}
	operationID := strings.TrimSpace(input.OperationID)
	inputHash, err := inputFingerprint(struct{ RunID, MessageIDs, OperationID string }{
		runID, strings.Join(messageIDs, "\x00"), operationID,
	})
	if err != nil {
		return nil, err
	}

	var delivered []Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		run, err := tx.Run(runID)
		if err != nil {
			return err
		}
		actorScope := "daemon:run:" + run.ID
		if raw, ok, err := tx.Dedupe(actorScope, "message.deliver", requestID); err != nil {
			return err
		} else if ok {
			if _, err := decodeDedupe(raw, inputHash); err != nil {
				return err
			}
			delivered, err = loadMessages(tx, messageIDs)
			return err
		}
		if IsRunTerminal(run.State) {
			return Conflict(CodeInvalidState, "terminal run cannot accept message input", string(run.State), run.Version)
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if task.CurrentRunID != run.ID || task.Generation != run.Generation || task.AssigneeAgentID != run.AgentID {
			return Conflict(CodeVersionConflict, "run delivery fence changed", string(task.Status), task.Version)
		}
		now := s.nowText()
		delivered = make([]Message, 0, len(messageIDs))
		for _, messageID := range messageIDs {
			message, err := tx.Message(messageID)
			if err != nil {
				return err
			}
			if message.ProjectID != run.ProjectID || message.RecipientKind != "agent" || message.RecipientID != run.AgentID {
				return NewError(CodeScopeDenied, "message cannot be delivered to this run", false)
			}
			deliveryTask, err := tx.Task(message.TaskID)
			if err != nil {
				return err
			}
			validTarget := deliveryTask.ID == run.TaskID ||
				(deliveryTask.Kind == TaskConversation && deliveryTask.ProjectID == run.ProjectID && deliveryTask.AssigneeAgentID == run.AgentID)
			if !validTarget || !taskAcceptsDelivery(deliveryTask) {
				return Conflict(CodeInvalidState, "message delivery task cannot receive run input", string(deliveryTask.Status), deliveryTask.Version)
			}
			if message.State == MessageAcknowledged || (message.State == MessageDelivered && message.DeliveredRunID == run.ID) {
				delivered = append(delivered, message)
				continue
			}
			if message.State != MessagePending {
				return Conflict(CodeInvalidState, "message cannot be marked delivered", string(message.State), message.Version)
			}
			if message.NextDeliveryAt == "" || message.NextDeliveryAt > now ||
				(message.MaxDeliveries > 0 && message.DeliveryCount >= message.MaxDeliveries) {
				return Conflict(CodeInvalidState, "message delivery is not currently eligible", string(message.State), message.Version)
			}
			expectedVersion := message.Version
			message.State = MessageDelivered
			message.DeliveredRunID = run.ID
			message.DeliveryCount++
			message.DeliveredAt = now
			message.LastDeliveryError = ""
			message.Version++
			if err := tx.UpdateMessage(message, expectedVersion, MessagePending); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"run_id": run.ID, "delivery_count": message.DeliveryCount})
			if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.delivered", "daemon", "", run.ID, requestID, operationID, payload, now)); err != nil {
				return err
			}
			delivered = append(delivered, message)
		}
		raw, err := encodeDedupe(run.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe(actorScope, "message.deliver", requestID, raw, now)
	})
	return delivered, err
}
