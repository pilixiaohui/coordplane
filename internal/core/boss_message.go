package core

import "context"

func (s *Service) SendBossMessage(ctx context.Context, input BossMessageInput) (Message, error) {
	request, err := s.bossAgentMessage(input.ProjectID, input.AgentID, input.TaskID, input.RelatedTaskID,
		input.Body, input.ReplyTo, input.Wake, input.AckMessageIDs, input.RequestID, "message.send")
	if err != nil {
		return Message{}, err
	}
	_, message, err := s.sendBossAgentMessage(ctx, request)
	return message, err
}

func (s *Service) ReadBossMessage(ctx context.Context, messageID, requestID string) (Message, error) {
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
	dedupe := requestDedupe{"boss", "message.read", requestID, inputHash}
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
		if message.RecipientKind != "boss" {
			return NewError(CodeScopeDenied, "message is not addressed to Boss", false)
		}
		now := s.nowText()
		if message.State == MessagePending {
			expectedVersion := message.Version
			message.State = MessageDelivered
			message.DeliveredRunID = ""
			message.DeliveredAt = now
			message.Version++
			if err := tx.UpdateMessage(message, expectedVersion, MessagePending); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.delivered", "boss", "", "", requestID, "", "{}", now)); err != nil {
				return err
			}
		}
		return dedupe.record(tx, message.ID, "", now)
	})
	return message, err
}
