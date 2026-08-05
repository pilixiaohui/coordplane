package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

// RP-08: bidirectional dispatch. A CLI agent creates a child task assigned to
// the human participant; the daemon never claims it; the human converges it
// with task complete and the parent's agent is woken with a child_result
// message mirroring its participant id. Dual/missing assignees are rejected.
func TestRP08AgentDispatchesChildTaskToHumanParticipant(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "rp08-agent")
	project := h.addProject(t, "rp08-project", "")

	if _, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, AssigneeParticipantID: core.DefaultHumanParticipantID,
		Title: "ambiguous", RequestID: "rp08-both",
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("dual assignee error = %v", err)
	}
	if _, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, Title: "no assignee", RequestID: "rp08-none",
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("missing assignee error = %v", err)
	}
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Title: "implement", RequestID: "rp08-parent",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	requireNoError(t, err)
	if !ok || claim.Task.ID != parent.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	activateRun(t, h, context.Background(), claim.Run.ID, "rp08-active")
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: claim.Token, AssigneeParticipantID: core.DefaultHumanParticipantID,
		Title: "review the result", Description: "human review", RequestID: "rp08-child",
	})
	requireNoError(t, err)
	if child.AssigneeAgentID != "" || child.AssigneeParticipantID != core.DefaultHumanParticipantID || child.ParentTaskID != parent.ID {
		t.Fatalf("child = agent %q participant %q parent %q", child.AssigneeAgentID, child.AssigneeParticipantID, child.ParentTaskID)
	}
	if _, ok, err := h.service.ClaimNext(context.Background(), project.ID); err != nil || ok {
		t.Fatalf("ClaimNext claimed human child task: ok=%t err=%v", ok, err)
	}
	done, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: child.ID, Summary: "review ok", RequestID: "rp08-complete",
	})
	requireNoError(t, err)
	if done.EvidenceType != string(core.EvidenceHumanConfirm) {
		t.Fatalf("child evidence_type = %q", done.EvidenceType)
	}
	messages, err := h.service.ListMessages(context.Background(), core.MessageFilter{ProjectID: project.ID, RecipientKind: "agent"})
	requireNoError(t, err)
	for _, message := range messages.Items {
		if message.RelatedTaskID == child.ID && message.SystemCode == "child_result" {
			if message.RecipientID != agent.ID || message.RecipientParticipantID != agent.ID {
				t.Fatalf("agent notification = %q / %q", message.RecipientID, message.RecipientParticipantID)
			}
			return
		}
	}
	t.Fatal("agent inbox missing child_result notification for the human review")
}
