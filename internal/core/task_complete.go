package core

import (
	"context"
	"strings"
)

// EvidenceType distinguishes how a task's terminal result was established.
// captured is a CLI-agent workspace capture (head_sha/task_ref present);
// human_confirm is an explicit human confirmation with no captured workspace
// result (head_sha stays empty and must never be displayed as captured).
type EvidenceType string

const (
	EvidenceCaptured     EvidenceType = "captured"
	EvidenceHumanConfirm EvidenceType = "human_confirm"
)

// CompleteTask converges a human-assigned task with explicit confirmation.
// Per the unified participant framework a human executes without a workspace:
// the task must not be bound to a CLI agent and must not have a live run; the
// terminal result is recorded with evidence_type=human_confirm and an empty
// head_sha. The capability gate is task.complete in the task's project scope.
func (s *Service) CompleteTask(ctx context.Context, input CompleteTaskInput) (Task, error) {
	taskID := strings.TrimSpace(input.TaskID)
	if _, err := requireText("task_id", taskID); err != nil {
		return Task{}, err
	}
	summary, err := optionalTextWithin("summary", input.Summary, MaximumOutcomeTextBytes)
	if err != nil {
		return Task{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Task{}, err
	}
	var projectID string
	if err := s.repository.Transact(ctx, func(tx Transaction) error {
		task, err := tx.Task(taskID)
		if err != nil {
			return err
		}
		projectID = task.ProjectID
		return nil
	}); err != nil {
		return Task{}, err
	}
	if err := s.requireOperatorCapability(ctx, CapabilityTaskComplete, projectID); err != nil {
		return Task{}, err
	}
	inputHash, err := inputFingerprint(struct {
		TaskID, Summary, RequestID string
	}{taskID, summary, requestID})
	if err != nil {
		return Task{}, err
	}
	dedupe := requestDedupe{operatorParticipant, "task.complete", requestID, inputHash}

	var task Task
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			task, err = tx.Task(replay.ID)
			return err
		}
		task, err = tx.Task(taskID)
		if err != nil {
			return err
		}
		if task.ProjectID != projectID {
			return NewError(CodeScopeDenied, "task project changed while completing", false)
		}
		if task.CurrentRunID != "" {
			return Conflict(CodeInvalidState, "task with a live run cannot be human-completed", string(task.Status), task.Version)
		}
		if task.AssigneeAgentID != "" {
			return Conflict(CodeInvalidState, "task assigned to a CLI agent converges through its captured run", string(task.Status), task.Version)
		}
		if task.AssigneeParticipantID == "" {
			return Conflict(CodeInvalidState, "task has no human assignee participant", string(task.Status), task.Version)
		}
		if task.Status != TaskQueued && task.Status != TaskWaiting {
			return Conflict(CodeInvalidState, "task cannot be completed from this status", string(task.Status), task.Version)
		}
		if task.PendingAction != "" {
			return Conflict(CodeActionInProgress, "task action is in progress", string(task.Status), task.Version)
		}
		now := s.nowText()
		expectedVersion, expectedStatus := task.Version, task.Status
		task.Status = TaskCompleted
		task.ResultSummary = summary
		task.EvidenceType = string(EvidenceHumanConfirm)
		task.CompletedAt = now
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, expectedVersion, expectedStatus); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"evidence": EvidenceHumanConfirm, "summary": summary})
		if _, err := tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.completed", "boss", operatorParticipant, "", requestID, "", payload, now)); err != nil {
			return err
		}
		if task.ParentTaskID != "" {
			if err := s.notifyParentOfChild(tx, task, "boss", operatorParticipant, "", requestID, now); err != nil {
				return err
			}
		}
		return dedupe.record(tx, task.ID, "", now)
	})
	return task, err
}
