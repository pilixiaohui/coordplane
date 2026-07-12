package core

import "fmt"

func ValidateProjectTransition(from, to ProjectStatus) error {
	allowed := map[ProjectStatus]map[ProjectStatus]bool{
		ProjectCreating: {ProjectActive: true, ProjectError: true},
		ProjectError:    {ProjectCreating: true, ProjectArchived: true},
		ProjectActive:   {ProjectError: true, ProjectArchived: true},
	}
	return validateTransition("project", string(from), string(to), allowed[from][to])
}

func ValidateAgentTransition(from, to AgentStatus) error {
	allowed := map[AgentStatus]map[AgentStatus]bool{
		AgentActive: {AgentPaused: true, AgentArchived: true},
		AgentPaused: {AgentActive: true, AgentArchived: true},
	}
	return validateTransition("agent", string(from), string(to), allowed[from][to])
}

func ValidateRunTransition(from, to RunState) error {
	allowed := map[RunState]map[RunState]bool{
		RunStarting: {
			RunActive: true, RunExited: true, RunFailed: true, RunInterrupted: true,
			RunCancelled: true, RunTimedOut: true,
		},
		RunActive: {
			RunExited: true, RunInterrupted: true, RunCancelled: true, RunTimedOut: true,
		},
	}
	return validateTransition("run", string(from), string(to), allowed[from][to])
}

func ValidateMessageTransition(from, to MessageState) error {
	allowed := map[MessageState]map[MessageState]bool{
		MessagePending: {
			MessageDelivered: true, MessageAcknowledged: true, MessageCancelled: true,
		},
		MessageDelivered: {
			MessagePending: true, MessageAcknowledged: true, MessageCancelled: true,
		},
	}
	return validateTransition("message", string(from), string(to), allowed[from][to])
}

func ValidateTaskTransition(kind TaskKind, from, to TaskStatus) error {
	allowed := map[TaskStatus]map[TaskStatus]bool{
		TaskQueued: {
			TaskRunning: true, TaskFailed: true, TaskCancelled: true,
		},
		TaskRunning: {
			TaskFinishing: true, TaskFailed: true, TaskCancelled: true,
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
	valid := allowed[from][to]
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

func ValidateTaskOperation(kind TaskKind, operation string) error {
	allowed := map[TaskKind]map[string]bool{
		TaskConversation: {
			"message": true, "progress": true, "wait": true, "fail": true, "close": true,
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
