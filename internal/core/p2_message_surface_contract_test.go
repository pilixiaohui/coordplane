package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestP2BossMessageSendTargetsWorkAndReroutesClosedTask(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "boss-send-agent")
	project := h.addProject(t, "boss-send-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "boss-send-target")
	waitAndTerminate(t, h, claim, "boss-send-wait")

	direct, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
		ProjectID: project.ID, AgentID: agent.ID, TaskID: claim.Task.ID,
		Body: "wake this work", Wake: true, RequestID: "boss-send-direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if direct.TaskID != claim.Task.ID || direct.RelatedTaskID != "" || direct.RecipientID != agent.ID {
		t.Fatalf("direct Boss message = %#v", direct)
	}
	woken, err := h.database.Task(context.Background(), claim.Task.ID)
	if err != nil || woken.Status != core.TaskQueued {
		t.Fatalf("direct Boss message did not wake work: %#v err=%v", woken, err)
	}
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: claim.Task.ID, Reason: "close old work", RequestID: "boss-send-cancel",
	}); err != nil {
		t.Fatal(err)
	}

	rerouted, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
		ProjectID: project.ID, AgentID: agent.ID, TaskID: claim.Task.ID,
		Body: "discuss closed work", Wake: false, RequestID: "boss-send-reroute",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rerouted.TaskID == claim.Task.ID || rerouted.RelatedTaskID != claim.Task.ID || rerouted.RecipientID != agent.ID {
		t.Fatalf("rerouted Boss message = %#v", rerouted)
	}
	conversation, err := h.database.Task(context.Background(), rerouted.TaskID)
	if err != nil || conversation.Kind != core.TaskConversation || conversation.Status != core.TaskWaiting {
		t.Fatalf("reroute conversation = %#v err=%v", conversation, err)
	}
}

func TestP2BossReadMarksDeliveredWithoutPretendingAcknowledgement(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "boss-read-agent")
	project := h.addProject(t, "boss-read-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "boss-read-source")
	message, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "read me",
		RequestID: "boss-read-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := h.service.ReadBossMessage(context.Background(), message.ID, "boss-read")
	if err != nil {
		t.Fatal(err)
	}
	if delivered.State != core.MessageDelivered || delivered.DeliveredRunID != "" ||
		delivered.DeliveredAt == "" || delivered.DeliveryCount != 0 || delivered.AcknowledgedAt != "" {
		t.Fatalf("Boss read projection = %#v", delivered)
	}
	beforeReplay := h.durableSignature(t, project.ID)
	if _, err := h.service.ReadBossMessage(context.Background(), message.ID, "boss-read"); err != nil {
		t.Fatal(err)
	}
	if after := h.durableSignature(t, project.ID); after != beforeReplay {
		t.Fatal("Boss read replay changed durable state")
	}
}

func TestP2AgentInboxReadUsesCurrentRunScope(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "inbox-read-agent")
	other := h.addAgent(t, "inbox-read-other")
	project := h.addProject(t, "inbox-read-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "scoped inbox",
		Wake: true, RequestID: "inbox-read-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != chat.Task.ID {
		t.Fatalf("inbox claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "inbox-read-active"); err != nil {
		t.Fatal(err)
	}
	current, err := h.service.CurrentTask(context.Background(), claim.Token)
	if err != nil || current.Task.ID != chat.Task.ID || current.Run.ID != claim.Run.ID || current.UnreadMessageCount != 1 {
		t.Fatalf("current task result = %#v err=%v", current, err)
	}
	read, err := h.service.InboxMessage(context.Background(), claim.Token, chat.Message.ID)
	if err != nil || read.ID != chat.Message.ID || read.State != core.MessagePending {
		t.Fatalf("inbox read = %#v err=%v", read, err)
	}
	otherTask, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: other.ID, Kind: core.TaskWork,
		Title: "other", Priority: 100, RequestID: "inbox-read-other-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || otherClaim.Task.ID != otherTask.ID {
		t.Fatalf("other claim = %#v ok=%t err=%v", otherClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), otherClaim.Run.ID, "inbox-read-other-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.InboxMessage(context.Background(), otherClaim.Token, chat.Message.ID); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("cross-Agent inbox read error = %v", err)
	}
}
