package core

import (
	"context"
	"strings"
)

type bossAgentMessage struct {
	projectID, agentID, taskID, relatedTaskID string
	body, replyTo                             string
	wake                                      bool
	ackIDs                                    []string
	requestID, operation                      string
}

func (s *Service) Chat(ctx context.Context, input ChatInput) (ChatResult, error) {
	request, err := s.bossAgentMessage(input.ProjectID, input.AgentID, "", input.RelatedTask,
		input.Body, input.ReplyTo, input.Wake, input.AckMessageIDs, input.RequestID, "chat.send")
	if err != nil {
		return ChatResult{}, err
	}
	task, message, err := s.sendBossAgentMessage(ctx, request)
	return ChatResult{Task: task, Message: message}, err
}

func (s *Service) bossAgentMessage(
	projectID, agentID, taskID, relatedTaskID, body, replyTo string,
	wake bool, ackIDs []string, requestID, operation string,
) (bossAgentMessage, error) {
	var result bossAgentMessage
	var err error
	if result.projectID, err = requireText("project_id", projectID); err != nil {
		return result, err
	}
	if result.agentID, err = requireText("agent_id", agentID); err != nil {
		return result, err
	}
	if result.body, err = requireText("body", body); err != nil {
		return result, err
	}
	if result.ackIDs, err = canonicalMessageIDs(ackIDs); err != nil {
		return result, err
	}
	if result.requestID, err = s.requestID(requestID); err != nil {
		return result, err
	}
	result.taskID, result.relatedTaskID = strings.TrimSpace(taskID), strings.TrimSpace(relatedTaskID)
	result.replyTo, result.wake, result.operation = strings.TrimSpace(replyTo), wake, operation
	return result, nil
}

func (s *Service) sendBossAgentMessage(ctx context.Context, input bossAgentMessage) (Task, Message, error) {
	inputHash, err := inputFingerprint(struct {
		ProjectID, AgentID, TaskID, RelatedTaskID, Body, ReplyTo, AckIDs string
		Wake                                                             bool
	}{input.projectID, input.agentID, input.taskID, input.relatedTaskID, input.body,
		input.replyTo, strings.Join(input.ackIDs, "\x00"), input.wake})
	if err != nil {
		return Task{}, Message{}, err
	}
	dedupe := requestDedupe{"boss", input.operation, input.requestID, inputHash}
	var task Task
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			message, err = tx.Message(replay.ID)
			if err == nil {
				task, err = tx.Task(message.TaskID)
			}
			return err
		}
		project, err := tx.Project(input.projectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectActive {
			return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
		}
		agent, err := tx.Agent(input.agentID)
		if err != nil {
			return err
		}
		if agent.Status == AgentArchived {
			return Conflict(CodeInvalidState, "archived agent cannot receive messages", string(agent.Status), agent.Version)
		}
		if err := bossMessageScope(tx, input.projectID, input.relatedTaskID, input.replyTo); err != nil {
			return err
		}
		now := s.nowText()
		actor := taskMutationActor{kind: "boss"}
		if err := s.acknowledgeForActor(tx, input.ackIDs, input.projectID, actor, input.requestID, now); err != nil {
			return err
		}
		task, input.relatedTaskID, err = s.agentDeliveryTask(tx, input.projectID, "", agent,
			input.taskID, input.relatedTaskID, input.wake, "boss", "", "", "boss_message", input.requestID, now)
		if err != nil {
			return err
		}
		message, err = s.insertMessage(tx, messageInsert{
			projectID: input.projectID, taskID: task.ID, relatedTaskID: input.relatedTaskID,
			senderKind: "boss", recipientKind: "agent", recipientID: input.agentID,
			replyTo: input.replyTo, body: input.body, wake: input.wake, maxDeliveries: 3,
			idempotencyKey: input.requestID, actor: actor, requestID: input.requestID, now: now,
		})
		if err != nil {
			return err
		}
		return dedupe.record(tx, message.ID, task.ID, now)
	})
	return task, message, err
}

func bossMessageScope(tx Transaction, projectID, relatedTaskID, replyTo string) error {
	if relatedTaskID != "" {
		task, err := tx.Task(relatedTaskID)
		if err != nil {
			return err
		}
		if task.ProjectID != projectID {
			return NewError(CodeScopeDenied, "related task belongs to another project", false)
		}
	}
	if replyTo != "" {
		message, err := tx.Message(replyTo)
		if err != nil {
			return err
		}
		if message.ProjectID != projectID {
			return NewError(CodeScopeDenied, "reply message belongs to another project", false)
		}
	}
	return nil
}

func (s *Service) AcknowledgeBossMessage(ctx context.Context, messageID, requestID string) (Message, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Message{}, err
	}
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
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
		message.State, message.AcknowledgedAt, message.Version = MessageAcknowledged, now, message.Version+1
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(message.ProjectID, "message", message.ID, "message.acknowledged",
			"boss", "", "", requestID, "", "{}", now))
		return err
	})
	return message, err
}
