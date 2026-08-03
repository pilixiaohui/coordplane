package core

import (
	"fmt"
)

func (s *Service) disposeUnresolvedMessages(
	tx Transaction,
	task Task,
	actorKind, actorID, runID, requestID, now string,
) error {
	messages, err := tx.MessagesForTask(task.ID)
	if err != nil {
		return err
	}
	project, projectErr := tx.Project(task.ProjectID)
	if projectErr != nil {
		return projectErr
	}
	for _, message := range messages {
		if message.RecipientKind != "agent" || (message.State != MessagePending && message.State != MessageDelivered) {
			continue
		}
		expectedVersion, expectedState := message.Version, message.State
		conversation, conversationErr := tx.Conversation(task.ProjectID, message.RecipientID)
		agent, agentErr := tx.Agent(message.RecipientID)
		if agentErr != nil {
			return agentErr
		}
		if conversationErr != nil && !IsCode(conversationErr, CodeNotFound) {
			return conversationErr
		}
		canReroute := project.Status == ProjectActive && agent.Status != AgentArchived && conversationErr == nil &&
			conversation.ID != task.ID && taskAcceptsDelivery(conversation)
		if canReroute {
			deliveryEligible := message.MaxDeliveries == 0 || message.DeliveryCount < message.MaxDeliveries
			deliveryAt := message.NextDeliveryAt
			if message.State == MessageDelivered || deliveryAt == "" || deliveryAt < now {
				deliveryAt = now
			}
			if message.Wake && deliveryEligible {
				expectedStatus := conversation.Status
				shouldQueue := expectedStatus == TaskWaiting ||
					(expectedStatus == TaskQueued && (conversation.NextRunAt == "" || deliveryAt < conversation.NextRunAt))
				if shouldQueue {
					expectedTaskVersion := conversation.Version
					conversation.Status = TaskQueued
					conversation.NextRunAt = deliveryAt
					conversation.Version++
					conversation.UpdatedAt = now
					if err := tx.UpdateTask(conversation, expectedTaskVersion, expectedStatus); err != nil {
						return err
					}
					payload := eventPayload(map[string]any{"reason": "message_rerouted", "message_id": message.ID})
					if _, err := tx.AppendEvent(event(task.ProjectID, "task", conversation.ID, "task.requeued", actorKind, actorID, runID, requestID, "", payload, now)); err != nil {
						return err
					}
				}
			}
			message.TaskID = conversation.ID
			message.RelatedTaskID = task.ID
			message.State = MessagePending
			message.DeliveredRunID = ""
			message.LastDeliveryError = "rerouted from non-delivery task"
			if deliveryEligible {
				message.NextDeliveryAt = deliveryAt
			} else {
				message.NextDeliveryAt = ""
			}
			message.Version++
			if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"from_task_id": task.ID, "to_task_id": conversation.ID})
			if _, err := tx.AppendEvent(event(task.ProjectID, "message", message.ID, "message.rerouted", actorKind, actorID, runID, requestID, "", payload, now)); err != nil {
				return err
			}
			continue
		}
		message.State = MessageCancelled
		message.DeliveredRunID = ""
		message.LastDeliveryError = "delivery task can no longer receive a run"
		message.NextDeliveryAt = ""
		message.Version++
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"task_id": task.ID, "reason": "no_delivery_conversation"})
		if _, err := tx.AppendEvent(event(task.ProjectID, "message", message.ID, "message.cancelled", actorKind, actorID, runID, requestID, "", payload, now)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) notifyParentOfChild(
	tx Transaction,
	child Task,
	actorKind, actorID, runID, requestID, now string,
) error {
	if child.ParentTaskID == "" {
		return nil
	}
	parent, parentErr := tx.Task(child.ParentTaskID)
	project, projectErr := tx.Project(child.ProjectID)
	if projectErr != nil {
		return projectErr
	}
	recipientKind, recipientID := "boss", ""
	deliveryTaskID := child.ID
	if parentErr == nil {
		deliveryTaskID = parent.ID
	} else if !IsCode(parentErr, CodeNotFound) {
		return parentErr
	}
	wake := false
	if parentErr == nil && project.Status == ProjectActive && taskAcceptsDelivery(parent) && parent.AssigneeAgentID != "" {
		agent, agentErr := tx.Agent(parent.AssigneeAgentID)
		if agentErr != nil {
			return agentErr
		}
		if agent.Status != AgentArchived {
			recipientKind, recipientID = "agent", parent.AssigneeAgentID
			wake = true
			if parent.Status == TaskWaiting {
				expectedVersion := parent.Version
				parent.Status = TaskQueued
				parent.NextRunAt = now
				parent.Version++
				parent.UpdatedAt = now
				if err := tx.UpdateTask(parent, expectedVersion, TaskWaiting); err != nil {
					return err
				}
				payload := eventPayload(map[string]any{"reason": "child_result", "child_task_id": child.ID})
				if _, err := tx.AppendEvent(event(parent.ProjectID, "task", parent.ID, "task.requeued", actorKind, actorID, runID, requestID, "", payload, now)); err != nil {
					return err
				}
			}
		}
	}

	body := boundedDurableText(fmt.Sprintf(
		"Child task %s is %s. Result: %s. Head: %s. Task ref: %s.",
		child.ID, child.Status, child.ResultSummary, child.HeadSHA, child.TaskRef,
	), MaximumMessageBodyBytes)
	maxDeliveries := 1
	recipientParticipantID := ""
	if recipientKind == "agent" {
		maxDeliveries = 3
		recipientParticipantID = parent.AssigneeParticipantID
	} else if parentErr == nil && parent.AssigneeParticipantID != "" {
		recipientParticipantID = parent.AssigneeParticipantID
	}
	payload := eventPayload(map[string]any{"child_task_id": child.ID, "child_status": child.Status})
	_, err := s.insertMessage(tx, messageInsert{
		projectID: child.ProjectID, taskID: deliveryTaskID, relatedTaskID: child.ID,
		senderKind: "system", recipientKind: recipientKind, recipientID: recipientID,
		recipientParticipantID: recipientParticipantID,
		systemCode:             "child_result", body: body, wake: wake, maxDeliveries: maxDeliveries,
		idempotencyKey: "child-result:" + child.ID + ":" + string(child.Status),
		actor:          taskMutationActor{kind: actorKind, id: actorID, runID: runID},
		requestID:      requestID, now: now, payload: payload,
	})
	return err
}
