package core

import (
	"context"
	"strings"
)

func (s *Service) SendBossMessage(ctx context.Context, input BossMessageInput) (Message, error) {
	projectID, err := requireText("project_id", input.ProjectID)
	if err != nil {
		return Message{}, err
	}
	agentID, err := requireText("agent_id", input.AgentID)
	if err != nil {
		return Message{}, err
	}
	body, err := requireText("body", input.Body)
	if err != nil {
		return Message{}, err
	}
	ackIDs, err := canonicalMessageIDs(input.AckMessageIDs)
	if err != nil {
		return Message{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Message{}, err
	}
	taskID := strings.TrimSpace(input.TaskID)
	relatedTaskID := strings.TrimSpace(input.RelatedTaskID)
	replyToID := strings.TrimSpace(input.ReplyTo)
	inputHash, err := inputFingerprint(struct {
		ProjectID, AgentID, TaskID, RelatedTaskID, Body, ReplyToID, AckIDs string
		Wake                                                               bool
	}{projectID, agentID, taskID, relatedTaskID, body, replyToID, strings.Join(ackIDs, "\x00"), input.Wake})
	if err != nil {
		return Message{}, err
	}

	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", "message.send", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			message, err = tx.Message(result.ID)
			return err
		}
		project, err := tx.Project(projectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		recipient, err := tx.Agent(agentID)
		if err != nil {
			return err
		}
		if recipient.Status == AgentArchived {
			return Conflict(CodeInvalidState, "archived agent cannot receive messages", string(recipient.Status), recipient.Version)
		}
		if relatedTaskID != "" {
			related, err := tx.Task(relatedTaskID)
			if err != nil {
				return err
			}
			if related.ProjectID != projectID {
				return NewError(CodeScopeDenied, "related task belongs to another project", false)
			}
		}
		if replyToID != "" {
			repliedTo, err := tx.Message(replyToID)
			if err != nil {
				return err
			}
			if repliedTo.ProjectID != projectID {
				return NewError(CodeScopeDenied, "reply message belongs to another project", false)
			}
		}
		now := s.nowText()
		if err := s.acknowledgeForActor(tx, ackIDs, projectID, taskMutationActor{kind: "boss"}, requestID, now); err != nil {
			return err
		}
		deliveryTask, relatedTaskID, err := s.agentDeliveryTask(
			tx, projectID, "", recipient, taskID, relatedTaskID, input.Wake,
			"boss", "", "", "boss_message", requestID, now,
		)
		if err != nil {
			return err
		}
		messageID, err := s.requiredID("msg")
		if err != nil {
			return err
		}
		message = Message{
			ID: messageID, ProjectID: projectID, TaskID: deliveryTask.ID,
			RelatedTaskID: relatedTaskID, SenderKind: "boss", RecipientKind: "agent",
			RecipientID: agentID, ReplyToMessageID: replyToID, Body: body, Wake: input.Wake,
			State: MessagePending, MaxDeliveries: 3, NextDeliveryAt: now,
			IdempotencyKey: requestID, Version: 1, CreatedAt: now,
		}
		if err := tx.InsertMessage(message); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(projectID, "message", message.ID, "message.created", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(message.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe("boss", "message.send", requestID, raw, now)
	})
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
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", "message.read", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			message, err = tx.Message(result.ID)
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
		raw, err := encodeDedupe(message.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe("boss", "message.read", requestID, raw, now)
	})
	return message, err
}
