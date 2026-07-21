package core

import (
	"context"
	"strings"
)

func (s *Service) SendAgentMessage(ctx context.Context, input SendMessageInput) (Message, error) {
	body, err := requireText("body", input.Body)
	if err != nil {
		return Message{}, err
	}
	recipientKind := strings.ToLower(strings.TrimSpace(input.RecipientKind))
	if recipientKind != "boss" && recipientKind != "agent" {
		return Message{}, NewError(CodeInvalidArgument, "recipient_kind must be boss or agent", false)
	}
	recipientID := strings.TrimSpace(input.RecipientID)
	if recipientKind == "agent" && recipientID == "" {
		return Message{}, NewError(CodeInvalidArgument, "recipient_id is required for an Agent message", false)
	}
	if recipientKind == "boss" && recipientID != "" {
		return Message{}, NewError(CodeInvalidArgument, "recipient_id must be empty for a Boss message", false)
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
		RecipientKind, RecipientID, TaskID, RelatedTaskID, Body, ReplyToID, AckIDs string
		Wake                                                                       bool
	}{recipientKind, recipientID, taskID, relatedTaskID, body, replyToID, strings.Join(ackIDs, "\x00"), input.Wake})
	if err != nil {
		return Message{}, err
	}
	dedupe := requestDedupe{"", "message.send", requestID, inputHash}

	var message Message
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		scope, err := scopedRun(tx, input.Token)
		if err != nil {
			return err
		}
		actorScope := "run:" + scope.ID
		dedupe.scope = actorScope
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			message, err = tx.Message(replay.ID)
			return err
		}
		run, current, err := s.authenticateRun(tx, input.Token)
		if err != nil {
			return err
		}
		if err := ValidateTaskOperation(current.Kind, "message"); err != nil {
			return err
		}
		if relatedTaskID != "" {
			related, err := tx.Task(relatedTaskID)
			if err != nil {
				return err
			}
			if !runCanReadTask(run, current, related) {
				return NewError(CodeScopeDenied, "related task is outside the current run scope", false)
			}
		}
		if recipientKind == "agent" && taskID == "" && relatedTaskID != "" && relatedTaskID != current.ID {
			return NewError(CodeScopeDenied, "direct Agent message must relate to the current task", false)
		}
		if replyToID != "" {
			repliedTo, err := tx.Message(replyToID)
			if err != nil {
				return err
			}
			allowed, err := runCanReadMessage(tx, run, current, repliedTo)
			if err != nil {
				return err
			}
			if !allowed {
				return NewError(CodeScopeDenied, "reply message is outside the current run scope", false)
			}
		}
		now := s.nowText()
		if _, err := s.acknowledgeAgentMessages(tx, ackIDs, current.ProjectID, run.AgentID, run.ID, requestID, now); err != nil {
			return err
		}

		deliveryTask := current
		if recipientKind == "boss" {
			if taskID != "" {
				deliveryTask, err = tx.Task(taskID)
				if err != nil {
					return err
				}
				if !runCanReadTask(run, current, deliveryTask) {
					return NewError(CodeScopeDenied, "delivery task is outside the current run scope", false)
				}
			}
		} else {
			recipient, err := tx.Agent(recipientID)
			if err != nil {
				return err
			}
			if recipient.Status == AgentArchived {
				return Conflict(CodeInvalidState, "archived agent cannot receive messages", string(recipient.Status), recipient.Version)
			}
			project, err := tx.Project(current.ProjectID)
			if err != nil {
				return err
			}
			if project.Status != ProjectActive {
				return Conflict(CodeInvalidState, "project is not active", string(project.Status), project.Version)
			}
			deliveryTask, relatedTaskID, err = s.agentDeliveryTask(
				tx, current.ProjectID, current.ID, recipient, taskID, relatedTaskID, input.Wake,
				"agent", run.AgentID, run.ID, "agent_message", requestID, now,
			)
			if err != nil {
				return err
			}
		}

		maxDeliveries := 1
		if recipientKind == "agent" {
			maxDeliveries = 3
		}
		message, err = s.insertMessage(tx, messageInsert{
			projectID: current.ProjectID, taskID: deliveryTask.ID, relatedTaskID: relatedTaskID,
			senderKind: "agent", senderID: run.AgentID, recipientKind: recipientKind, recipientID: recipientID,
			replyTo: replyToID, body: body, wake: input.Wake, maxDeliveries: maxDeliveries,
			idempotencyKey: requestID, actor: taskMutationActor{kind: "agent", id: run.AgentID, runID: run.ID},
			requestID: requestID, now: now,
		})
		if err != nil {
			return err
		}
		return dedupe.record(tx, message.ID, "", now)
	})
	return message, err
}

func runCanReadTask(run Run, current, target Task) bool {
	if target.ProjectID != current.ProjectID {
		return false
	}
	return target.ID == current.ID || target.ID == current.ParentTaskID ||
		target.ParentTaskID == current.ID || target.AssigneeAgentID == run.AgentID ||
		(target.CreatedByKind == "agent" && target.CreatedByID == run.AgentID)
}

func runCanReadMessage(tx Transaction, run Run, current Task, message Message) (bool, error) {
	if message.ProjectID != current.ProjectID {
		return false, nil
	}
	if (message.SenderKind == "agent" && message.SenderID == run.AgentID) ||
		(message.RecipientKind == "agent" && message.RecipientID == run.AgentID) {
		return true, nil
	}
	task, err := tx.Task(message.TaskID)
	if err != nil {
		return false, err
	}
	if runCanReadTask(run, current, task) {
		return true, nil
	}
	if message.RelatedTaskID == "" {
		return false, nil
	}
	related, err := tx.Task(message.RelatedTaskID)
	if err != nil {
		return false, err
	}
	return runCanReadTask(run, current, related), nil
}

func (s *Service) agentDeliveryTask(
	tx Transaction,
	projectID, defaultRelatedTaskID string,
	recipient Agent,
	explicitTaskID, relatedTaskID string,
	wake bool,
	actorKind, actorID, runID, reason, requestID, now string,
) (Task, string, error) {
	if explicitTaskID != "" {
		target, err := tx.Task(explicitTaskID)
		if err != nil {
			return Task{}, "", err
		}
		if target.ProjectID != projectID || target.AssigneeAgentID != recipient.ID {
			return Task{}, "", NewError(CodeScopeDenied, "delivery task is not owned by the recipient", false)
		}
		if taskAcceptsDelivery(target) {
			if err := s.wakeDeliveryTask(tx, &target, wake, reason, actorKind, actorID, runID, requestID, now); err != nil {
				return Task{}, "", err
			}
			return target, relatedTaskID, nil
		}
		relatedTaskID = target.ID
	}

	conversation, err := tx.Conversation(projectID, recipient.ID)
	if IsCode(err, CodeNotFound) {
		conversationID, idErr := s.requiredID("tsk")
		if idErr != nil {
			return Task{}, "", idErr
		}
		status := TaskWaiting
		if wake {
			status = TaskQueued
		}
		conversation = Task{
			ID: conversationID, ProjectID: projectID, Kind: TaskConversation,
			CreatedByKind: actorKind, CreatedByID: actorID, AssigneeAgentID: recipient.ID,
			Title: "Conversation with " + recipient.DisplayName, Description: "Persistent Agent conversation",
			Status: status, NextRunAt: now, MaxRetries: 3, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertTask(conversation); err != nil {
			return Task{}, "", err
		}
		if _, err := tx.AppendEvent(event(projectID, "task", conversation.ID, "task.created", actorKind, actorID, runID, requestID, "", eventPayload(map[string]any{"kind": TaskConversation}), now)); err != nil {
			return Task{}, "", err
		}
	} else if err != nil {
		return Task{}, "", err
	} else if !taskAcceptsDelivery(conversation) || conversation.Status == TaskFailed {
		return Task{}, "", Conflict(CodeInvalidState, "conversation cannot receive messages", string(conversation.Status), conversation.Version)
	} else if err := s.wakeDeliveryTask(tx, &conversation, wake, reason, actorKind, actorID, runID, requestID, now); err != nil {
		return Task{}, "", err
	}
	if defaultRelatedTaskID != "" {
		relatedTaskID = defaultRelatedTaskID
	}
	return conversation, relatedTaskID, nil
}

func (s *Service) wakeDeliveryTask(
	tx Transaction,
	task *Task,
	wake bool,
	reason, actorKind, actorID, runID, requestID, now string,
) error {
	if !wake || task.Status != TaskWaiting {
		return nil
	}
	expectedVersion := task.Version
	task.Status = TaskQueued
	task.NextRunAt = now
	task.Version++
	task.UpdatedAt = now
	if err := tx.UpdateTask(*task, expectedVersion, TaskWaiting); err != nil {
		return err
	}
	_, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.requeued", actorKind, actorID, runID, requestID, "", eventPayload(map[string]any{"reason": reason}), now))
	return err
}
