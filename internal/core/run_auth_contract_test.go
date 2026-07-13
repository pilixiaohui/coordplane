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
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("Run scope authorization changed durable state")
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
