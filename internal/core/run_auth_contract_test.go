package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestRunScopeAuthorizationAllowsStartingAndRejectsCrossScopeWithoutWrites(t *testing.T) {
	h := newHarness(t)
	agentA := h.addAgent(t, "scope-a")
	agentB := h.addAgent(t, "scope-b")
	project := h.addProject(t, "scope-project", "")
	for _, input := range []core.CreateTaskInput{
		{ProjectID: project.ID, AssigneeAgentID: agentA.ID, Kind: core.TaskWork, Title: "scope a", Priority: 20, RequestID: "scope-task-a"},
		{ProjectID: project.ID, AssigneeAgentID: agentB.ID, Kind: core.TaskWork, Title: "scope b", Priority: 10, RequestID: "scope-task-b"},
	} {
		if _, err := h.service.CreateTask(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	claimA, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim A: ok=%t err=%v", ok, err)
	}
	claimB, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim B: ok=%t err=%v", ok, err)
	}
	scopeA := runScope(claimA.Run)
	scopeB := runScope(claimB.Run)

	before := h.durableSignature(t, project.ID)
	if err := h.service.AuthorizeRunScope(context.Background(), claimA.Token, scopeA); err != nil {
		t.Fatalf("authorize matching starting Run: %v", err)
	}
	if err := h.service.AuthorizeRunScope(context.Background(), claimA.Token, scopeB); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("cross-scope authorization error = %v, want %s", err, core.CodeScopeDenied)
	}
	if _, err := h.service.CurrentTask(context.Background(), claimA.Token); !core.IsCode(err, core.CodeRunStarting) {
		t.Fatalf("starting operation error = %v, want %s", err, core.CodeRunStarting)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func TestStartingOutcomeDoesNotConsumeRequestIDBeforeActiveRetry(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "starting-outcome-agent")
	project := h.addProject(t, "starting-outcome-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "retry outcome after active", RequestID: "starting-outcome-task",
	})
	requireNoError(t, err)
	message, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
		ProjectID: project.ID, AgentID: agent.ID, TaskID: task.ID,
		Body: "ack only after outcome admission", Wake: true, RequestID: "starting-outcome-message",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim starting outcome Run: ok=%t err=%v", ok, err)
	}
	input := core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "retry after active",
		AckMessageIDs: []string{message.ID}, RequestID: "starting-outcome-request",
	}
	before := h.durableSignature(t, project.ID)
	if _, err := h.service.RequestOutcome(context.Background(), input); !core.IsCode(err, core.CodeRunStarting) {
		t.Fatalf("starting outcome error = %v, want %s", err, core.CodeRunStarting)
	} else if typed := core.AsError(err); !typed.Retryable || typed.State != string(core.RunStarting) || typed.Version != claim.Run.Version {
		t.Fatalf("starting outcome error fields = %#v", typed)
	}
	h.requireDurableSignature(t, project.ID, before)

	active, err := activateRun(t, h, context.Background(), claim.Run.ID, "starting-outcome-active")
	requireNoError(t, err)
	result, err := h.service.RequestOutcome(context.Background(), input)
	if err != nil {
		t.Fatalf("retry same outcome request after active: %v", err)
	}
	if result.Run.ID != active.ID || result.Run.RequestedOutcome != string(core.OutcomeWait) ||
		result.Task.Status != core.TaskFinishing || len(result.Acknowledged) != 1 ||
		result.Acknowledged[0].ID != message.ID || result.Acknowledged[0].State != core.MessageAcknowledged {
		t.Fatalf("active outcome retry result = %#v", result)
	}
}

func runScope(run core.Run) core.RunScope {
	return core.RunScope{
		ProjectID:  run.ProjectID,
		AgentID:    run.AgentID,
		TaskID:     run.TaskID,
		RunID:      run.ID,
		Generation: run.Generation,
	}
}
