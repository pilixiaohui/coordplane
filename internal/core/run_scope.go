package core

import (
	"context"
	"strings"
)

func (s *Service) TaskForRun(ctx context.Context, token, taskID string) (Task, error) {
	taskID, err := requireText("task_id", taskID)
	if err != nil {
		return Task{}, err
	}
	var target Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		run, current, err := s.authenticateRun(tx, token)
		if err != nil {
			return err
		}
		target, err = tx.Task(taskID)
		if err != nil {
			return err
		}
		if target.ProjectID != current.ProjectID {
			return NewError(CodeScopeDenied, "task is outside the current run project", false)
		}
		allowed := target.ID == current.ID || target.ID == current.ParentTaskID ||
			target.ParentTaskID == current.ID || target.AssigneeAgentID == run.AgentID ||
			(target.CreatedByKind == "agent" && target.CreatedByID == run.AgentID)
		if !allowed {
			return NewError(CodeScopeDenied, "task is outside the current run scope", false)
		}
		return nil
	})
	return target, err
}

func (s *Service) Inbox(ctx context.Context, token string) ([]Message, error) {
	var inbox []Message
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		run, task, err := s.authenticateRun(tx, token)
		if err != nil {
			return err
		}
		messages, err := tx.MessagesForRecipient("agent", run.AgentID)
		if err != nil {
			return err
		}
		inbox = make([]Message, 0, len(messages))
		for _, message := range messages {
			if message.ProjectID == task.ProjectID && (message.State == MessagePending || message.State == MessageDelivered) {
				inbox = append(inbox, message)
			}
		}
		return nil
	})
	return inbox, err
}

func (s *Service) InboxMessage(ctx context.Context, token, messageID string) (Message, error) {
	messageID, err := requireText("message_id", messageID)
	if err != nil {
		return Message{}, err
	}
	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		run, task, err := s.authenticateRun(tx, token)
		if err != nil {
			return err
		}
		message, err = tx.Message(messageID)
		if err != nil {
			return err
		}
		if message.ProjectID != task.ProjectID || message.RecipientKind != "agent" || message.RecipientID != run.AgentID {
			return NewError(CodeScopeDenied, "message is outside the current run scope", false)
		}
		return nil
	})
	return message, err
}

func (s *Service) AcknowledgeAgentMessages(ctx context.Context, input AcknowledgeMessagesInput) ([]Message, error) {
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
	inputHash, err := inputFingerprint(struct{ MessageIDs string }{strings.Join(messageIDs, "\x00")})
	if err != nil {
		return nil, err
	}
	dedupe := requestDedupe{"", "message.ack", requestID, inputHash}
	var acknowledged []Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		actorScope := "run:" + scope.ID
		dedupe.scope = actorScope
		if _, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			acknowledged, err = loadMessages(tx, messageIDs)
			return err
		}
		run, task, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		now := s.nowText()
		acknowledged, err = s.acknowledgeAgentMessages(tx, messageIDs, task.ProjectID, run.AgentID, run.ID, requestID, now)
		if err != nil {
			return err
		}
		return dedupe.record(tx, scope.ID, "", now)
	})
	return acknowledged, err
}
