package core

import "testing"

func TestCanonicalProjectAgentAndMessageFSMCompleteMatrices(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		states := []ProjectStatus{ProjectCreating, ProjectActive, ProjectError, ProjectArchived}
		allowed := transitionPairs[ProjectStatus]{
			ProjectCreating: {ProjectActive, ProjectError},
			ProjectActive:   {ProjectError, ProjectArchived},
			ProjectError:    {ProjectCreating, ProjectArchived},
		}
		assertTransitionMatrix(t, states, allowed, ValidateProjectTransition)
	})

	t.Run("agent", func(t *testing.T) {
		states := []AgentStatus{AgentActive, AgentPaused, AgentArchived}
		allowed := transitionPairs[AgentStatus]{
			AgentActive: {AgentPaused, AgentArchived},
			AgentPaused: {AgentActive, AgentArchived},
		}
		assertTransitionMatrix(t, states, allowed, ValidateAgentTransition)
	})

	t.Run("message", func(t *testing.T) {
		states := []MessageState{MessagePending, MessageDelivered, MessageAcknowledged, MessageCancelled}
		allowed := transitionPairs[MessageState]{
			MessagePending:   {MessageDelivered, MessageAcknowledged, MessageCancelled},
			MessageDelivered: {MessagePending, MessageAcknowledged, MessageCancelled},
		}
		assertTransitionMatrix(t, states, allowed, ValidateMessageTransition)
	})
}

func TestCanonicalTaskFSMCompleteMatrix(t *testing.T) {
	states := []TaskStatus{
		TaskQueued, TaskRunning, TaskFinishing, TaskWaiting,
		TaskSubmitted, TaskCompleted, TaskFailed, TaskCancelled,
	}
	ordinary := transitionPairs[TaskStatus]{
		TaskQueued:    {TaskRunning, TaskFailed, TaskCancelled},
		TaskRunning:   {TaskFinishing, TaskQueued, TaskFailed, TaskCancelled},
		TaskFinishing: {TaskWaiting, TaskQueued, TaskSubmitted, TaskFailed},
		TaskWaiting:   {TaskQueued, TaskFailed, TaskCancelled},
		TaskSubmitted: {TaskQueued, TaskCompleted, TaskCancelled},
		TaskFailed:    {TaskQueued, TaskCancelled},
	}
	conversation := transitionPairs[TaskStatus]{
		TaskQueued:    {TaskRunning, TaskFailed, TaskCancelled},
		TaskRunning:   {TaskFinishing, TaskQueued, TaskFailed, TaskCancelled},
		TaskFinishing: {TaskWaiting, TaskQueued, TaskFailed},
		TaskWaiting:   {TaskQueued, TaskCompleted, TaskFailed, TaskCancelled},
		TaskFailed:    {TaskQueued, TaskCancelled},
	}
	tests := []struct {
		kind    TaskKind
		allowed transitionPairs[TaskStatus]
	}{
		{kind: TaskConversation, allowed: conversation},
		{kind: TaskWork, allowed: ordinary},
		{kind: TaskIntegration, allowed: ordinary},
	}
	for _, test := range tests {
		assertTransitionMatrix(t, states, test.allowed, func(from, to TaskStatus) error {
			return ValidateTaskTransition(test.kind, from, to)
		})
	}
}

func TestCanonicalRunFSMCompleteMatrix(t *testing.T) {
	states := []RunState{RunStarting, RunActive, RunExited, RunFailed, RunInterrupted, RunCancelled, RunTimedOut}
	allowed := transitionPairs[RunState]{
		RunStarting: {RunActive, RunExited, RunFailed, RunInterrupted, RunCancelled, RunTimedOut},
		RunActive:   {RunExited, RunInterrupted, RunCancelled, RunTimedOut},
	}
	assertTransitionMatrix(t, states, allowed, ValidateRunTransition)
}

func TestRuntimeRetryDecisionHonorsZeroAndExactBoundary(t *testing.T) {
	tests := []struct {
		name       string
		count      int
		max        int
		wantStatus TaskStatus
		wantCount  int
	}{
		{name: "zero retries", count: 0, max: 0, wantStatus: TaskFailed, wantCount: 0},
		{name: "before boundary", count: 0, max: 1, wantStatus: TaskQueued, wantCount: 1},
		{name: "on boundary", count: 1, max: 1, wantStatus: TaskFailed, wantCount: 1},
		{name: "past boundary fails closed", count: 2, max: 1, wantStatus: TaskFailed, wantCount: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := runtimeRetryDecision(test.count, test.max)
			if decision.status != test.wantStatus || decision.retryCount != test.wantCount {
				t.Fatalf("decision = %#v, want status=%s count=%d", decision, test.wantStatus, test.wantCount)
			}
		})
	}
}

func TestCanonicalTaskOperationMatrix(t *testing.T) {
	operations := []string{"message", "progress", "wait", "submit", "fail", "accept", "rework", "retry", "cancel", "wake", "close"}
	allowed := map[TaskKind]map[string]bool{
		TaskConversation: {
			"message": true, "progress": true, "wait": true, "fail": true,
			"retry": true, "cancel": true, "wake": true, "close": true,
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
	for _, kind := range []TaskKind{TaskConversation, TaskWork, TaskIntegration} {
		for _, operation := range operations {
			t.Run(string(kind)+"/"+operation, func(t *testing.T) {
				got := ValidateTaskOperation(kind, operation) == nil
				if got != allowed[kind][operation] {
					t.Fatalf("%s operation %s allowed=%v", kind, operation, got)
				}
			})
		}
	}
}

type transitionPairs[T comparable] map[T][]T

func assertTransitionMatrix[T comparable](t *testing.T, states []T, allowed transitionPairs[T], validate func(T, T) error) {
	t.Helper()
	for _, from := range states {
		for _, to := range states {
			if got := validate(from, to) == nil; got != allowed.has(from, to) {
				t.Errorf("transition %v -> %v allowed=%v", from, to, got)
			}
		}
	}
}

func (pairs transitionPairs[T]) has(from, to T) bool {
	for _, candidate := range pairs[from] {
		if candidate == to {
			return true
		}
	}
	return false
}
