package core

import "fmt"

var projectTransitions = map[ProjectStatus]map[ProjectStatus]bool{
	ProjectCreating: {ProjectActive: true, ProjectError: true},
	ProjectError:    {ProjectCreating: true, ProjectArchived: true},
	ProjectActive:   {ProjectError: true, ProjectArchived: true},
}

var agentTransitions = map[AgentStatus]map[AgentStatus]bool{
	AgentActive: {AgentPaused: true, AgentArchived: true},
	AgentPaused: {AgentActive: true, AgentArchived: true},
}

var runTransitions = map[RunState]map[RunState]bool{
	RunStarting: {
		RunActive: true, RunExited: true, RunFailed: true, RunInterrupted: true,
		RunCancelled: true, RunTimedOut: true,
	},
	RunActive: {
		RunExited: true, RunInterrupted: true, RunCancelled: true, RunTimedOut: true,
	},
}

var messageTransitions = map[MessageState]map[MessageState]bool{
	MessagePending: {
		MessageDelivered: true, MessageAcknowledged: true, MessageCancelled: true,
	},
	MessageDelivered: {
		MessagePending: true, MessageAcknowledged: true, MessageCancelled: true,
	},
}

var taskTransitions = map[TaskStatus]map[TaskStatus]bool{
	TaskQueued: {
		TaskRunning: true, TaskFailed: true, TaskCancelled: true,
	},
	TaskRunning: {
		TaskFinishing: true, TaskQueued: true, TaskFailed: true, TaskCancelled: true,
	},
	TaskFinishing: {
		TaskWaiting: true, TaskQueued: true, TaskSubmitted: true, TaskFailed: true,
	},
	TaskWaiting: {
		TaskQueued: true, TaskFailed: true, TaskCancelled: true,
	},
	TaskSubmitted: {
		TaskQueued: true, TaskCompleted: true, TaskCancelled: true,
	},
	TaskFailed: {
		TaskQueued: true, TaskCancelled: true,
	},
}

func ValidateProjectTransition(from, to ProjectStatus) error {
	return validateTransition("project", string(from), string(to), projectTransitions[from][to])
}

func ValidateAgentTransition(from, to AgentStatus) error {
	return validateTransition("agent", string(from), string(to), agentTransitions[from][to])
}

func ValidateRunTransition(from, to RunState) error {
	return validateTransition("run", string(from), string(to), runTransitions[from][to])
}

func ValidateMessageTransition(from, to MessageState) error {
	return validateTransition("message", string(from), string(to), messageTransitions[from][to])
}

func ValidateTaskTransition(kind TaskKind, from, to TaskStatus) error {
	if kind != TaskConversation && kind != TaskWork && kind != TaskIntegration {
		return validateTransition("task", string(from), string(to), false)
	}
	valid := taskTransitions[from][to]
	if kind == TaskConversation {
		if to == TaskSubmitted || from == TaskSubmitted {
			valid = false
		}
		if from == TaskWaiting && to == TaskCompleted {
			valid = true
		}
	} else if from == TaskWaiting && to == TaskCompleted {
		valid = false
	}
	return validateTransition("task", string(from), string(to), valid)
}

type taskRetryDecision struct {
	status     TaskStatus
	retryCount int
}

// runtimeRetryDecision is the only max_retries boundary used by Run failure
// projection. retry_count records retries that were actually scheduled.
func runtimeRetryDecision(retryCount, maxRetries int) taskRetryDecision {
	if retryCount < maxRetries {
		return taskRetryDecision{status: TaskQueued, retryCount: retryCount + 1}
	}
	return taskRetryDecision{status: TaskFailed, retryCount: retryCount}
}

func ValidateTaskOperation(kind TaskKind, operation string) error {
	allowed := map[TaskKind]map[string]bool{
		TaskConversation: {
			"message": true, "progress": true, "wait": true, "fail": true, "close": true,
			"retry": true, "cancel": true, "wake": true,
		},
		TaskWork: {
			"message": true, "progress": true, "wait": true, "submit": true, "fail": true,
			"accept": true, "rework": true, "retry": true, "cancel": true, "wake": true,
		},
		TaskIntegration: {
			"message": true, "progress": true, "wait": true, "submit": true, "fail": true,
			"accept": true, "rework": true, "retry": true, "cancel": true, "wake": true,
		},
	}
	if allowed[kind][operation] {
		return nil
	}
	return NewError(CodeInvalidState, fmt.Sprintf("%s task does not support %s", kind, operation), false)
}

func validateTransition(entity, from, to string, allowed bool) error {
	if allowed {
		return nil
	}
	return NewError(CodeInvalidState, fmt.Sprintf("invalid %s transition %s -> %s", entity, from, to), false)
}
