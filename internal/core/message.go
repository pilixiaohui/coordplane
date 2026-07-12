package core

import (
	"context"
	"strings"
)

func (s *Service) Chat(ctx context.Context, input ChatInput) (ChatResult, error) {
	projectID, err := requireText("project_id", input.ProjectID)
	if err != nil {
		return ChatResult{}, err
	}
	agentID, err := requireText("agent_id", input.AgentID)
	if err != nil {
		return ChatResult{}, err
	}
	body, err := requireText("body", input.Body)
	if err != nil {
		return ChatResult{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return ChatResult{}, err
	}
	relatedTaskID := strings.TrimSpace(input.RelatedTask)
	replyToID := strings.TrimSpace(input.ReplyTo)
	inputHash, err := inputFingerprint(struct {
		ProjectID, AgentID, Body, RelatedTaskID, ReplyToID string
		Wake                                               bool
	}{projectID, agentID, body, relatedTaskID, replyToID, input.Wake})
	if err != nil {
		return ChatResult{}, err
	}
	var result ChatResult
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", "chat.send", requestID); err != nil {
			return err
		} else if ok {
			dedupe, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			result.Task, err = tx.Task(dedupe.ID)
			if err != nil {
				return err
			}
			result.Message, err = tx.Message(dedupe.RelatedID)
			return err
		}
		project, err := tx.Project(projectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		agent, err := tx.Agent(agentID)
		if err != nil {
			return err
		}
		if agent.Status == AgentArchived {
			return Conflict(CodeInvalidState, "archived agent cannot receive messages", string(agent.Status), agent.Version)
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
		conversation, err := tx.Conversation(projectID, agentID)
		if IsCode(err, CodeNotFound) {
			taskID, idErr := s.requiredID("tsk")
			if idErr != nil {
				return idErr
			}
			status := TaskWaiting
			if input.Wake {
				status = TaskQueued
			}
			conversation = Task{
				ID: taskID, ProjectID: projectID, Kind: TaskConversation,
				CreatedByKind: "boss", AssigneeAgentID: agentID,
				Title:       "Conversation with " + agent.DisplayName,
				Description: "Persistent Boss conversation", Status: status,
				NextRunAt: now, MaxRetries: 3, Version: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.InsertTask(conversation); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"kind": TaskConversation})
			if _, err := tx.AppendEvent(event(projectID, "task", conversation.ID, "task.created", "boss", "", "", requestID, "", payload, now)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if conversation.Status == TaskFailed {
			return Conflict(CodeInvalidState, "failed conversation must be retried or cancelled", string(conversation.Status), conversation.Version)
		} else if conversation.Status == TaskWaiting && input.Wake {
			expectedVersion := conversation.Version
			conversation.Status = TaskQueued
			conversation.Version++
			conversation.UpdatedAt = now
			conversation.NextRunAt = now
			if err := tx.UpdateTask(conversation, expectedVersion, TaskWaiting); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(event(projectID, "task", conversation.ID, "task.requeued", "boss", "", "", requestID, "", `{"reason":"boss_message"}`, now)); err != nil {
				return err
			}
		}
		messageID, err := s.requiredID("msg")
		if err != nil {
			return err
		}
		message := Message{
			ID: messageID, ProjectID: projectID, TaskID: conversation.ID,
			RelatedTaskID: relatedTaskID, SenderKind: "boss",
			RecipientKind: "agent", RecipientID: agentID, ReplyToMessageID: replyToID,
			Body: body, Wake: input.Wake, State: MessagePending, MaxDeliveries: 3,
			NextDeliveryAt: now, IdempotencyKey: requestID, Version: 1, CreatedAt: now,
		}
		if err := tx.InsertMessage(message); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(projectID, "message", message.ID, "message.created", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(conversation.ID, message.ID, inputHash)
		if err != nil {
			return err
		}
		if err := tx.PutDedupe("boss", "chat.send", requestID, raw, now); err != nil {
			return err
		}
		result = ChatResult{Task: conversation, Message: message}
		return nil
	})
	return result, err
}

func (s *Service) AcknowledgeBossMessage(ctx context.Context, messageID, requestID string) (Message, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Message{}, err
	}
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		message, err = tx.Message(strings.TrimSpace(messageID))
		if err != nil {
			return err
		}
		if message.RecipientKind != "boss" {
			return NewError(CodeScopeDenied, "message is not addressed to Boss", false)
		}
		if message.State == MessageAcknowledged {
			return nil
		}
		if err := ValidateMessageTransition(message.State, MessageAcknowledged); err != nil {
			return Conflict(CodeInvalidState, "message cannot be acknowledged", string(message.State), message.Version)
		}
		now := s.nowText()
		expectedVersion, expectedState := message.Version, message.State
		message.State = MessageAcknowledged
		message.AcknowledgedAt = now
		message.Version++
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.acknowledged", "boss", "", "", requestID, "", "{}", now))
		return err
	})
	return message, err
}
