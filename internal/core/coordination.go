package core

import (
	"sort"
	"strings"
)

const maximumBundledMessageIDs = 256

func canonicalMessageIDs(values []string) ([]string, error) {
	if len(values) > maximumBundledMessageIDs {
		return nil, NewError(CodeInvalidArgument, "message ID list exceeds 256 items", false)
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, NewError(CodeInvalidArgument, "message IDs must not be empty", false)
		}
		unique[value] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func scopedRun(tx Transaction, token string) (Run, error) {
	if strings.TrimSpace(token) == "" {
		return Run{}, NewError(CodeScopeDenied, "run token is required", false)
	}
	run, err := tx.RunByTokenHash(hashToken(token))
	if err != nil {
		if IsCode(err, CodeNotFound) {
			return Run{}, NewError(CodeScopeDenied, "run scope is not valid", false)
		}
		return Run{}, err
	}
	return run, nil
}

func loadMessages(tx Transaction, ids []string) ([]Message, error) {
	messages := make([]Message, 0, len(ids))
	for _, id := range ids {
		message, err := tx.Message(id)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func (s *Service) acknowledgeAgentMessages(
	tx Transaction,
	ids []string,
	projectID, agentID, runID, requestID, now string,
) ([]Message, error) {
	messages := make([]Message, 0, len(ids))
	for _, id := range ids {
		message, err := tx.Message(id)
		if err != nil {
			return nil, err
		}
		if message.ProjectID != projectID || message.RecipientKind != "agent" || message.RecipientID != agentID {
			return nil, NewError(CodeScopeDenied, "message is outside the current run scope", false)
		}
		if message.State == MessageAcknowledged {
			messages = append(messages, message)
			continue
		}
		if message.State != MessagePending && message.State != MessageDelivered {
			return nil, Conflict(CodeInvalidState, "message cannot be acknowledged", string(message.State), message.Version)
		}
		expectedVersion, expectedState := message.Version, message.State
		message.State = MessageAcknowledged
		message.AcknowledgedAt = now
		message.Version++
		if err := tx.UpdateMessage(message, expectedVersion, expectedState); err != nil {
			return nil, err
		}
		if _, err := tx.AppendEvent(event(
			message.ProjectID, "message", message.ID, "message.acknowledged",
			"agent", agentID, runID, requestID, "", "{}", now,
		)); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func taskAcceptsDelivery(task Task) bool {
	switch task.Status {
	case TaskQueued, TaskRunning, TaskFinishing, TaskWaiting:
		return true
	default:
		return false
	}
}

func taskActionAvailable(task Task) error {
	if task.Status == TaskFinishing || task.PendingAction != "" || task.IntegrationTaskID != "" {
		return Conflict(CodeActionInProgress, "task action is in progress", string(task.Status), task.Version)
	}
	return nil
}

func clearTaskAction(task *Task) {
	task.PendingAction = ""
	task.PendingActionID = ""
	task.PendingActionVersion = 0
	task.PendingActionRunID = ""
	task.PendingExpectedSHA = ""
	task.PendingTargetSHA = ""
	task.PendingStartedAt = ""
}
