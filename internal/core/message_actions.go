package core

import (
	"context"
)

func (s *Service) RetryMessage(ctx context.Context, messageID, requestID string) (Message, error) {
	messageID, err := requireText("message_id", messageID)
	if err != nil {
		return Message{}, err
	}
	requestID, err = s.requestID(requestID)
	if err != nil {
		return Message{}, err
	}
	inputHash, err := inputFingerprint(struct{ MessageID string }{messageID})
	if err != nil {
		return Message{}, err
	}
	dedupe := requestDedupe{"boss", "message.retry", requestID, inputHash}
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			message, err = tx.Message(replay.ID)
			return err
		}
		message, err = tx.Message(messageID)
		if err != nil {
			return err
		}
		if message.RecipientKind != "agent" {
			return NewError(CodeScopeDenied, "only an Agent message can be retried", false)
		}
		if message.State != MessagePending {
			return Conflict(CodeInvalidState, "only a pending message can be retried", string(message.State), message.Version)
		}
		task, err := tx.Task(message.TaskID)
		if err != nil {
			return err
		}
		if !taskAcceptsDelivery(task) {
			return Conflict(CodeInvalidState, "message delivery task cannot receive a run", string(task.Status), task.Version)
		}
		now := s.nowText()
		expectedVersion := message.Version
		message.DeliveryCount = 0
		message.NextDeliveryAt = now
		message.LastDeliveryError = ""
		message.Version++
		if err := tx.UpdateMessage(message, expectedVersion, MessagePending); err != nil {
			return err
		}
		if message.Wake && task.Status == TaskWaiting {
			expectedTaskVersion := task.Version
			task.Status = TaskQueued
			task.NextRunAt = now
			task.Version++
			task.UpdatedAt = now
			if err := tx.UpdateTask(task, expectedTaskVersion, TaskWaiting); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.requeued", "boss", "", "", requestID, "", eventPayload(map[string]any{"reason": "message_retry"}), now)); err != nil {
				return err
			}
		}
		if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.retry_enabled", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
		return dedupe.record(tx, message.ID, "", now)
	})
	return message, err
}
