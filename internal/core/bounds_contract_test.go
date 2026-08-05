package core_test

import (
	"context"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestMessageAndProgressLimitPlusOneHaveNoDurableSideEffects(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "bounded-agent")
	project := h.addProject(t, "bounded-project", "")

	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "valid",
		Wake: true, RequestID: strings.Repeat("r", 257),
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized request ID error = %v, want INVALID_ARGUMENT", err)
	}
	h.requireDurableSignature(t, project.ID, before)
	if _, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "oversized", Description: strings.Repeat("d", core.MaximumTaskDescriptionBytes+1),
		RequestID: "oversized-description",
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized task description error = %v, want INVALID_ARGUMENT", err)
	}
	h.requireDurableSignature(t, project.ID, before)

	chatRequestID := "bounded-chat"
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID,
		Body: strings.Repeat("m", core.MaximumMessageBodyBytes+1), Wake: true, RequestID: chatRequestID,
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized Boss message error = %v, want INVALID_ARGUMENT", err)
	}
	h.requireDurableSignature(t, project.ID, before)
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID,
		Body: strings.Repeat("m", core.MaximumMessageBodyBytes), Wake: true, RequestID: chatRequestID,
	})
	if err != nil {
		t.Fatalf("exact-limit Boss message failed with the rejected request ID: %v", err)
	}

	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != chat.Task.ID {
		t.Fatalf("claim bounded conversation: claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "activate-bounded"); err != nil {
		t.Fatal(err)
	}

	before = durableSignature(t, h.database, project.ID)
	progressRequestID := "bounded-progress"
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{
		Token: claim.Token, Summary: strings.Repeat("p", core.MaximumProgressSummaryBytes+1), RequestID: progressRequestID,
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized progress error = %v, want INVALID_ARGUMENT", err)
	}
	h.requireDurableSignature(t, project.ID, before)
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{
		Token: claim.Token, Summary: strings.Repeat("p", core.MaximumProgressSummaryBytes), RequestID: progressRequestID,
	}); err != nil {
		t.Fatalf("exact-limit progress failed with the rejected request ID: %v", err)
	}

	before = durableSignature(t, h.database, project.ID)
	agentMessageRequestID := "bounded-agent-message"
	if _, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: strings.Repeat("a", core.MaximumMessageBodyBytes+1), RequestID: agentMessageRequestID,
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("oversized Agent message error = %v, want INVALID_ARGUMENT", err)
	}
	h.requireDurableSignature(t, project.ID, before)
	if _, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: strings.Repeat("a", core.MaximumMessageBodyBytes), RequestID: agentMessageRequestID,
	}); err != nil {
		t.Fatalf("exact-limit Agent message failed with the rejected request ID: %v", err)
	}
}
