package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestP2BossTaskCreateAndChatReplyBundleMessageAcknowledgement(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "boss-ack-agent")
	project := h.addProject(t, "boss-ack-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "boss-ack-source")
	first, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "please create follow-up",
		RequestID: "boss-ack-first-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "follow-up", AckMessageIDs: []string{first.ID}, RequestID: "boss-ack-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("Boss task create returned no Task")
	}
	assertMessageState(t, h, first.ID, core.MessageAcknowledged)

	second, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "please reply",
		RequestID: "boss-ack-second-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "replying now", ReplyTo: second.ID,
		Wake: false, AckMessageIDs: []string{second.ID}, RequestID: "boss-ack-reply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Message.ReplyToMessageID != second.ID || reply.Message.State != core.MessagePending {
		t.Fatalf("Boss reply = %#v", reply)
	}
	assertMessageState(t, h, second.ID, core.MessageAcknowledged)
}

func TestP2BossChatFailureRollsBackBundledAcknowledgement(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "boss-ack-rollback-agent")
	project := h.addProject(t, "boss-ack-rollback-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "start conversation",
		Wake: true, RequestID: "boss-ack-rollback-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != chat.Task.ID {
		t.Fatalf("conversation claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "boss-ack-rollback-active"); err != nil {
		t.Fatal(err)
	}
	message, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "cannot continue",
		RequestID: "boss-ack-rollback-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeFail,
		Reason: "conversation failed", RequestID: "boss-ack-rollback-fail",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunExited,
		ExitCode: intPointer(1), RequestID: "boss-ack-rollback-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	before := h.durableSignature(t, project.ID)
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "failed reply", ReplyTo: message.ID,
		Wake: true, AckMessageIDs: []string{message.ID}, RequestID: "boss-ack-rollback-reply",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("failed conversation reply error = %v", err)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("failed Boss reply committed its bundled acknowledgement")
	}
	assertMessageState(t, h, message.ID, core.MessagePending)
}

func assertMessageState(t *testing.T, h *harness, messageID string, want core.MessageState) {
	t.Helper()
	messages, err := h.database.Messages(context.Background(), core.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages.Items {
		if message.ID == messageID {
			if message.State != want {
				t.Fatalf("message %s state = %s, want %s", messageID, message.State, want)
			}
			return
		}
	}
	t.Fatalf("message %s was not found", messageID)
}
