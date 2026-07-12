package core

import "testing"

func TestCanonicalFSMTransitions(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"project creating to active", ValidateProjectTransition(ProjectCreating, ProjectActive)},
		{"project error to creating", ValidateProjectTransition(ProjectError, ProjectCreating)},
		{"project archived is terminal", ValidateProjectTransition(ProjectArchived, ProjectActive)},
		{"agent active to paused", ValidateAgentTransition(AgentActive, AgentPaused)},
		{"agent archived is terminal", ValidateAgentTransition(AgentArchived, AgentActive)},
		{"run starting to active", ValidateRunTransition(RunStarting, RunActive)},
		{"run terminal cannot revive", ValidateRunTransition(RunExited, RunActive)},
		{"message delivered can redeliver", ValidateMessageTransition(MessageDelivered, MessagePending)},
		{"message acknowledged is terminal", ValidateMessageTransition(MessageAcknowledged, MessagePending)},
		{"work running to finishing", ValidateTaskTransition(TaskWork, TaskRunning, TaskFinishing)},
		{"work submitted to completed", ValidateTaskTransition(TaskWork, TaskSubmitted, TaskCompleted)},
		{"conversation waiting to completed", ValidateTaskTransition(TaskConversation, TaskWaiting, TaskCompleted)},
		{"conversation cannot submit", ValidateTaskTransition(TaskConversation, TaskFinishing, TaskSubmitted)},
		{"work cannot close from waiting", ValidateTaskTransition(TaskWork, TaskWaiting, TaskCompleted)},
		{"closed task cannot reopen", ValidateTaskTransition(TaskWork, TaskCompleted, TaskQueued)},
	}

	wantError := map[string]bool{
		"project archived is terminal":     true,
		"agent archived is terminal":       true,
		"run terminal cannot revive":       true,
		"message acknowledged is terminal": true,
		"conversation cannot submit":       true,
		"work cannot close from waiting":   true,
		"closed task cannot reopen":        true,
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if wantError[test.name] && !IsCode(test.err, CodeInvalidState) {
				t.Fatalf("error = %v, want %s", test.err, CodeInvalidState)
			}
			if !wantError[test.name] && test.err != nil {
				t.Fatalf("unexpected error: %v", test.err)
			}
		})
	}
}

func TestStableErrorCarriesConflictState(t *testing.T) {
	err := Conflict(CodeVersionConflict, "task changed", string(TaskRunning), 7)
	if err.Code != CodeVersionConflict || err.Retryable != true {
		t.Fatalf("error = %#v", err)
	}
	if err.State != string(TaskRunning) || err.Version != 7 {
		t.Fatalf("conflict context = %#v", err)
	}
}
