package core

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

const (
	LaunchIntent          = "intent"
	LaunchCreated         = "created"
	LaunchStartIssued     = "start_issued"
	LaunchProcessObserved = "process_observed"

	CleanupNotNeeded = "not_needed"
	CleanupPending   = "pending"
	CleanupRemoved   = "removed"
	CleanupBlocked   = "blocked"
)

type RunLaunchInput struct {
	RunID                 string
	Generation            int64
	LaunchNonce           string
	WorkspacePath         string
	HomePath              string
	LogPath               string
	InstructionsHash      string
	LaunchMode            string
	ResumedFromRunID      string
	ResumeNativeSessionID string
	CleanupOperationID    string
	DeadlineAt            string
	RequestID             string
}

type RunRuntimeFactInput struct {
	RunID             string
	Generation        int64
	LaunchNonce       string
	LaunchOperationID string
	ContainerID       string
	RequestID         string
}

type RunSessionInput struct {
	RunRuntimeFactInput
	NativeSessionID string
}

type RunHeartbeatInput struct {
	RunRuntimeFactInput
	ObservedAt string
}

type RunCleanupInput struct {
	RunRuntimeFactInput
	CleanupOperationID string
	State              string
	LastError          string
}

type RunLaunchContext struct {
	Project  Project
	Agent    Agent
	Task     Task
	Run      Run
	Messages []Message
}

func (s *Service) RuntimeLaunchContext(ctx context.Context, runID string) (RunLaunchContext, error) {
	var result RunLaunchContext
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		result.Run, err = tx.Run(strings.TrimSpace(runID))
		if err != nil {
			return err
		}
		result.Task, err = tx.Task(result.Run.TaskID)
		if err != nil {
			return err
		}
		result.Project, err = tx.Project(result.Run.ProjectID)
		if err != nil {
			return err
		}
		result.Agent, err = tx.Agent(result.Run.AgentID)
		if err != nil {
			return err
		}
		if result.Run.State != RunStarting || result.Task.Status != TaskQueued ||
			result.Task.CurrentRunID != result.Run.ID || result.Task.Generation != result.Run.Generation ||
			result.Task.AssigneeAgentID != result.Run.AgentID {
			return Conflict(CodeStaleRun, "runtime launch context fence changed", string(result.Run.State), result.Run.Version)
		}
		messages, err := tx.MessagesForRecipient("agent", result.Run.AgentID)
		if err != nil {
			return err
		}
		now := s.nowText()
		for _, message := range messages {
			if message.ProjectID != result.Run.ProjectID || message.State != MessagePending ||
				message.NextDeliveryAt == "" || message.NextDeliveryAt > now ||
				(message.MaxDeliveries > 0 && message.DeliveryCount >= message.MaxDeliveries) {
				continue
			}
			deliveryTask, err := tx.Task(message.TaskID)
			if err != nil {
				return err
			}
			validTarget := deliveryTask.ID == result.Run.TaskID ||
				(deliveryTask.Kind == TaskConversation && deliveryTask.ProjectID == result.Run.ProjectID &&
					deliveryTask.AssigneeAgentID == result.Run.AgentID)
			if validTarget && taskAcceptsDelivery(deliveryTask) {
				result.Messages = append(result.Messages, message)
			}
		}
		return nil
	})
	return result, err
}

func (s *Service) BeginRunLaunch(ctx context.Context, input RunLaunchInput) (Run, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	input.LaunchNonce = strings.TrimSpace(input.LaunchNonce)
	input.CleanupOperationID = strings.TrimSpace(input.CleanupOperationID)
	input.InstructionsHash = strings.TrimSpace(input.InstructionsHash)
	input.ResumedFromRunID = strings.TrimSpace(input.ResumedFromRunID)
	input.ResumeNativeSessionID = strings.TrimSpace(input.ResumeNativeSessionID)
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	if input.RunID == "" || input.Generation < 1 || input.LaunchNonce == "" || input.CleanupOperationID == "" {
		return Run{}, NewError(CodeInvalidArgument, "run launch identity is incomplete", false)
	}
	if input.LaunchMode != "start" && input.LaunchMode != "resume" {
		return Run{}, NewError(CodeInvalidArgument, "launch_mode must be start or resume", false)
	}
	if input.LaunchMode == "start" && input.ResumeNativeSessionID != "" {
		return Run{}, NewError(CodeInvalidArgument, "start launch cannot carry a native resume session", false)
	}
	if input.LaunchMode == "resume" && (input.ResumedFromRunID == "" || input.ResumeNativeSessionID == "") {
		return Run{}, NewError(CodeInvalidArgument, "resume launch requires source Run and native session", false)
	}
	for name, path := range map[string]string{"home_path": input.HomePath, "log_path": input.LogPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return Run{}, NewError(CodeInvalidArgument, name+" must be canonical and absolute", false)
		}
	}
	if input.WorkspacePath != "" && (!filepath.IsAbs(input.WorkspacePath) || filepath.Clean(input.WorkspacePath) != input.WorkspacePath) {
		return Run{}, NewError(CodeInvalidArgument, "workspace_path must be canonical and absolute", false)
	}
	if input.DeadlineAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, input.DeadlineAt); err != nil {
			return Run{}, NewError(CodeInvalidArgument, "deadline_at must be RFC3339", false)
		}
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(input.RunID)
		if err != nil {
			return err
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if run.State != RunStarting || run.LaunchPhase != LaunchIntent || run.ContainerID != "" ||
			task.Status != TaskQueued || task.CurrentRunID != run.ID || task.Generation != run.Generation ||
			run.Generation != input.Generation {
			return Conflict(CodeStaleRun, "run launch fence changed", string(run.State), run.Version)
		}
		if task.Kind == TaskConversation && input.WorkspacePath != "" {
			return NewError(CodeInvalidArgument, "conversation Run cannot have a workspace", false)
		}
		if task.Kind != TaskConversation && input.WorkspacePath == "" {
			return NewError(CodeInvalidArgument, "code Run requires a workspace", false)
		}
		resumeFallback := false
		if input.LaunchMode == "start" && input.ResumedFromRunID != "" {
			previous, previousErr := tx.Run(input.ResumedFromRunID)
			if previousErr != nil {
				return previousErr
			}
			if previous.TaskID != run.TaskID || previous.AgentID != run.AgentID ||
				!IsRunTerminal(previous.State) || previous.RuntimeErrorCode != string(CodeResumeUnavailable) {
				return Conflict(CodeResumeUnavailable, "fresh start fallback source is not a failed resume Run", string(previous.State), previous.Version)
			}
			resumeFallback = true
		}
		if run.LaunchNonce != "" {
			if sameLaunchIntent(run, input) {
				return nil
			}
			return Conflict(CodeActionInProgress, "different run launch intent already exists", string(run.State), run.Version)
		}
		expectedVersion := run.Version
		run.LaunchNonce = input.LaunchNonce
		run.WorkspacePath = input.WorkspacePath
		run.HomePath = input.HomePath
		run.LogPath = input.LogPath
		run.InstructionsHash = input.InstructionsHash
		run.LaunchMode = input.LaunchMode
		run.ResumedFromRunID = input.ResumedFromRunID
		run.ResumeNativeSessionID = input.ResumeNativeSessionID
		run.CleanupOperationID = input.CleanupOperationID
		run.CleanupState = CleanupPending
		run.DeadlineAt = input.DeadlineAt
		run.Version++
		if err := tx.UpdateRun(run, expectedVersion, RunStarting); err != nil {
			return err
		}
		if resumeFallback {
			payload := eventPayload(map[string]any{"resumed_from_run_id": run.ResumedFromRunID})
			if _, err := tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.resume_fallback", "daemon", "", run.ID, requestID, run.LaunchOperationID, payload, s.nowText())); err != nil {
				return err
			}
		}
		payload := eventPayload(map[string]any{"launch_mode": run.LaunchMode, "phase": run.LaunchPhase})
		_, err = tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.launch_prepared", "daemon", "", run.ID, requestID, run.LaunchOperationID, payload, s.nowText()))
		return err
	})
	return run, err
}

func sameLaunchIntent(run Run, input RunLaunchInput) bool {
	return run.Generation == input.Generation && run.LaunchNonce == input.LaunchNonce &&
		run.WorkspacePath == input.WorkspacePath && run.HomePath == input.HomePath && run.LogPath == input.LogPath &&
		run.InstructionsHash == input.InstructionsHash && run.LaunchMode == input.LaunchMode &&
		run.ResumedFromRunID == input.ResumedFromRunID && run.ResumeNativeSessionID == input.ResumeNativeSessionID &&
		run.CleanupOperationID == input.CleanupOperationID && run.DeadlineAt == input.DeadlineAt
}

func (s *Service) RecordContainerCreated(ctx context.Context, input RunRuntimeFactInput) (Run, error) {
	input.ContainerID = strings.TrimSpace(input.ContainerID)
	if input.ContainerID == "" {
		return Run{}, NewError(CodeInvalidArgument, "container_id is required", false)
	}
	return s.advanceRunPhase(ctx, input, LaunchIntent, LaunchCreated, "run.container_created", func(run *Run) error {
		if run.ContainerID != "" && run.ContainerID != input.ContainerID {
			return Conflict(CodeRuntimeInvariantViolation, "run container identity changed", string(run.State), run.Version)
		}
		run.ContainerID = input.ContainerID
		return nil
	})
}

func (s *Service) RecordRunStartIssued(ctx context.Context, input RunRuntimeFactInput) (Run, error) {
	return s.advanceRunPhase(ctx, input, LaunchCreated, LaunchStartIssued, "run.start_issued", nil)
}

func (s *Service) ObserveProcessAndActivateRun(ctx context.Context, input RunRuntimeFactInput) (Run, error) {
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(input.RunID))
		if err != nil {
			return err
		}
		if run.State == RunActive && run.LaunchPhase == LaunchProcessObserved {
			return runtimeFactFence(run, input)
		}
		if run.State != RunStarting || run.LaunchPhase != LaunchStartIssued {
			return Conflict(CodeInvalidState, "run is not ready for process observation", string(run.State), run.Version)
		}
		if err := runtimeFactFence(run, input); err != nil {
			return err
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if task.Status != TaskQueued || task.CurrentRunID != run.ID || task.Generation != run.Generation {
			return Conflict(CodeVersionConflict, "task claim fence changed", string(task.Status), task.Version)
		}
		now := s.nowText()
		runVersion := run.Version
		run.State = RunActive
		run.LaunchPhase = LaunchProcessObserved
		run.StartedAt = now
		run.LastObservedAt = now
		run.HeartbeatAt = now
		run.Version++
		if err := tx.UpdateRun(run, runVersion, RunStarting); err != nil {
			return err
		}
		taskVersion := task.Version
		task.Status = TaskRunning
		task.Version++
		task.UpdatedAt = now
		if err := tx.UpdateTask(task, taskVersion, TaskQueued); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(task.ProjectID, "run", run.ID, "run.active", "daemon", "", run.ID, requestID, run.LaunchOperationID, "{}", now)); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(task.ProjectID, "task", task.ID, "task.running", "daemon", "", run.ID, requestID, "", "{}", now))
		return err
	})
	return run, err
}

func (s *Service) RecordRunSession(ctx context.Context, input RunSessionInput) (Run, error) {
	input.NativeSessionID = boundedDurableText(input.NativeSessionID, 1024)
	if strings.TrimSpace(input.NativeSessionID) == "" {
		return Run{}, NewError(CodeInvalidArgument, "native_session_id is required", false)
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(input.RunID))
		if err != nil {
			return err
		}
		if err := runtimeFactFence(run, input.RunRuntimeFactInput); err != nil {
			return err
		}
		if run.State != RunStarting && run.State != RunActive {
			return Conflict(CodeInvalidState, "terminal run cannot record a session", string(run.State), run.Version)
		}
		if run.NativeSessionID != "" {
			if run.NativeSessionID == input.NativeSessionID {
				return nil
			}
			return Conflict(CodeInvalidState, "native session identity cannot change", string(run.State), run.Version)
		}
		expectedVersion, expectedState := run.Version, run.State
		run.NativeSessionID = input.NativeSessionID
		run.Version++
		if err := tx.UpdateRun(run, expectedVersion, expectedState); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.session_recorded", "daemon", "", run.ID, requestID, run.LaunchOperationID, "{}", s.nowText()))
		return err
	})
	return run, err
}

func (s *Service) RecordRunHeartbeat(ctx context.Context, input RunHeartbeatInput) (Run, error) {
	observed := strings.TrimSpace(input.ObservedAt)
	if observed == "" {
		observed = s.nowText()
	} else if _, err := time.Parse(time.RFC3339Nano, observed); err != nil {
		return Run{}, NewError(CodeInvalidArgument, "observed_at must be RFC3339", false)
	}
	var run Run
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(input.RunID))
		if err != nil {
			return err
		}
		if err := runtimeFactFence(run, input.RunRuntimeFactInput); err != nil {
			return err
		}
		if run.State != RunActive || run.LaunchPhase != LaunchProcessObserved {
			return Conflict(CodeInvalidState, "only an observed active run can heartbeat", string(run.State), run.Version)
		}
		expectedVersion := run.Version
		run.HeartbeatAt = observed
		run.LastObservedAt = observed
		run.Version++
		return tx.UpdateRun(run, expectedVersion, RunActive)
	})
	return run, err
}

func (s *Service) RecordRunCleanup(ctx context.Context, input RunCleanupInput) (Run, error) {
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	input.CleanupOperationID = strings.TrimSpace(input.CleanupOperationID)
	input.LastError = boundedDurableText(input.LastError, MaximumTerminalTextBytes)
	if input.State != CleanupPending && input.State != CleanupBlocked && input.State != CleanupRemoved {
		return Run{}, NewError(CodeInvalidArgument, "invalid cleanup state", false)
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(input.RunID))
		if err != nil {
			return err
		}
		if err := runtimeFactFence(run, input.RunRuntimeFactInput); err != nil {
			return err
		}
		if run.CleanupOperationID == "" || run.CleanupOperationID != input.CleanupOperationID {
			return Conflict(CodeStaleRun, "cleanup operation fence changed", string(run.State), run.Version)
		}
		if run.CleanupState == input.State && (input.State != CleanupBlocked || run.LastError == input.LastError) {
			return nil
		}
		if !validCleanupTransition(run.CleanupState, input.State, IsRunTerminal(run.State)) {
			return Conflict(CodeInvalidState, "invalid cleanup transition", string(run.State), run.Version)
		}
		expectedVersion, expectedState := run.Version, run.State
		run.CleanupState = input.State
		if input.State == CleanupBlocked {
			run.LastError = input.LastError
		}
		run.Version++
		if err := tx.UpdateRun(run, expectedVersion, expectedState); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{"cleanup_state": input.State})
		_, err = tx.AppendEvent(event(run.ProjectID, "run", run.ID, "run.cleanup_"+input.State, "daemon", "", run.ID, requestID, input.CleanupOperationID, payload, s.nowText()))
		return err
	})
	return run, err
}

func (s *Service) LiveRuns(ctx context.Context) ([]Run, error) {
	return s.repository.LiveRuns(ctx)
}

func (s *Service) RunsNeedingCleanup(ctx context.Context) ([]Run, error) {
	return s.repository.RunsNeedingCleanup(ctx)
}

func (s *Service) LatestTerminalRun(ctx context.Context, taskID, agentID string) (Run, error) {
	return s.repository.LatestTerminalRun(ctx, strings.TrimSpace(taskID), strings.TrimSpace(agentID))
}

func (s *Service) TaskHasStartedRun(ctx context.Context, taskID string) (bool, error) {
	return s.repository.TaskHasStartedRun(ctx, strings.TrimSpace(taskID))
}

func (s *Service) advanceRunPhase(
	ctx context.Context,
	input RunRuntimeFactInput,
	expected, next, eventKind string,
	mutate func(*Run) error,
) (Run, error) {
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		run, err = tx.Run(strings.TrimSpace(input.RunID))
		if err != nil {
			return err
		}
		if err := runtimeFactFence(run, input); err != nil {
			return err
		}
		if launchPhaseOrder(run.LaunchPhase) > launchPhaseOrder(expected) {
			if mutate != nil {
				return mutate(&run)
			}
			return nil
		}
		if run.State != RunStarting || run.LaunchPhase != expected {
			return Conflict(CodeInvalidState, "run launch phase changed", string(run.State), run.Version)
		}
		if mutate != nil {
			if err := mutate(&run); err != nil {
				return err
			}
		}
		expectedVersion := run.Version
		run.LaunchPhase = next
		run.Version++
		if err := tx.UpdateRun(run, expectedVersion, RunStarting); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(run.ProjectID, "run", run.ID, eventKind, "daemon", "", run.ID, requestID, run.LaunchOperationID, eventPayload(map[string]any{"phase": next}), s.nowText()))
		return err
	})
	return run, err
}

func runtimeFactFence(run Run, input RunRuntimeFactInput) error {
	if run.ID != strings.TrimSpace(input.RunID) || run.Generation != input.Generation ||
		run.LaunchNonce == "" || run.LaunchNonce != strings.TrimSpace(input.LaunchNonce) ||
		run.LaunchOperationID == "" || run.LaunchOperationID != strings.TrimSpace(input.LaunchOperationID) {
		return Conflict(CodeStaleRun, "runtime fact fence changed", string(run.State), run.Version)
	}
	containerID := strings.TrimSpace(input.ContainerID)
	switch {
	case run.ContainerID != "" && run.ContainerID != containerID:
		return Conflict(CodeStaleRun, "runtime container fence changed", string(run.State), run.Version)
	case run.ContainerID == "" && containerID != "" &&
		(run.State != RunStarting || run.LaunchPhase != LaunchIntent):
		return Conflict(CodeStaleRun, "runtime container fence changed", string(run.State), run.Version)
	}
	return nil
}

func launchPhaseOrder(phase string) int {
	switch phase {
	case LaunchIntent:
		return 0
	case LaunchCreated:
		return 1
	case LaunchStartIssued:
		return 2
	case LaunchProcessObserved:
		return 3
	default:
		return -1
	}
}

func validCleanupTransition(current, next string, terminal bool) bool {
	switch {
	case current == CleanupNotNeeded && next == CleanupPending:
		return true
	case current == CleanupPending && next == CleanupBlocked:
		return true
	case current == CleanupBlocked && next == CleanupPending:
		return true
	case current == CleanupPending && next == CleanupRemoved && terminal:
		return true
	default:
		return false
	}
}
