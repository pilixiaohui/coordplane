package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestP2OutcomeIntentReplaysAfterRevocationAndTerminalAppliesOnce(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "outcome-agent")
	project := h.addProject(t, "outcome-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "ack with outcome", Wake: false, RequestID: "outcome-chat",
	})
	requireNoError(t, err)
	work, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Title: "outcome work", Priority: 10,
		MaxRetries: 1, RequestID: "outcome-work",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != work.ID {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "outcome-active"); err != nil {
		t.Fatal(err)
	}
	input := core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "waiting for review",
		AckMessageIDs: []string{chat.Message.ID, chat.Message.ID}, RequestID: "wait-outcome",
	}
	requested, err := h.service.RequestOutcome(context.Background(), input)
	requireNoError(t, err)
	if requested.Task.Status != core.TaskFinishing || requested.Task.CurrentRunID != claim.Run.ID ||
		requested.Run.RequestedOutcome != "wait" || requested.Run.TokenRevokedAt == "" {
		t.Fatalf("outcome intent did not remain finishing: %#v %#v", requested.Task, requested.Run)
	}
	message, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: chat.Task.ID})
	if err != nil || len(message.Items) != 1 || message.Items[0].State != core.MessageAcknowledged {
		t.Fatalf("bundled ack = %#v err=%v", message, err)
	}
	beforeReplay := durableSignature(t, h.database, project.ID)
	if _, err := h.service.RequestOutcome(context.Background(), input); err != nil {
		t.Fatalf("dedupe replay after token revocation: %v", err)
	}
	h.requireDurableSignature(t, project.ID, beforeReplay)

	terminalInput := core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: intPointer(0),
		TerminalReason: "agent stopped after wait", RequestID: "wait-terminal",
	}
	terminal, err := recordRunTerminal(h, context.Background(), terminalInput)
	requireNoError(t, err)
	if terminal.Task.Status != core.TaskWaiting || terminal.Task.CurrentRunID != "" || terminal.Task.WaitReason != "waiting for review" {
		t.Fatalf("wait terminal projection = %#v", terminal.Task)
	}
	beforeTerminalReplay := durableSignature(t, h.database, project.ID)
	if _, err := recordRunTerminal(h, context.Background(), terminalInput); err != nil {
		t.Fatalf("terminal replay: %v", err)
	}
	h.requireDurableSignature(t, project.ID, beforeTerminalReplay)
}

func TestP2UnackedDeliveryRedeliversWithBackoffWhileAckWins(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "delivery-agent")
	project := h.addProject(t, "delivery-project", "")
	first, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "first", Wake: true, RequestID: "delivery-first",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "delivery-active"); err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "second", Wake: true, RequestID: "delivery-second",
	})
	requireNoError(t, err)
	delivered, err := h.service.RecordMessagesDelivered(context.Background(), core.MessageDeliveryInput{
		RunID: claim.Run.ID, MessageIDs: []string{first.Message.ID, second.Message.ID}, RequestID: "delivery-record",
	})
	if err != nil || len(delivered) != 2 {
		t.Fatalf("delivered=%#v err=%v", delivered, err)
	}
	if _, err := h.service.AcknowledgeAgentMessages(context.Background(), core.AcknowledgeMessagesInput{
		Token: claim.Token, MessageIDs: []string{first.Message.ID}, RequestID: "delivery-ack",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "turn complete", RequestID: "delivery-wait",
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: intPointer(0), RequestID: "delivery-terminal",
	})
	requireNoError(t, err)
	if len(terminal.Redelivered) != 1 || terminal.Redelivered[0].ID != second.Message.ID ||
		terminal.Redelivered[0].State != core.MessagePending {
		t.Fatalf("redelivered=%#v", terminal.Redelivered)
	}
	if terminal.Task.Status != core.TaskQueued || terminal.Task.NextRunAt <= terminal.Run.EndedAt {
		t.Fatalf("wake backoff projection = %#v run=%#v", terminal.Task, terminal.Run)
	}
	messages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: first.Task.ID})
	requireNoError(t, err)
	states := map[string]core.MessageState{}
	for _, message := range messages.Items {
		states[message.ID] = message.State
	}
	if states[first.Message.ID] != core.MessageAcknowledged || states[second.Message.ID] != core.MessagePending {
		t.Fatalf("message states=%v", states)
	}
}

func TestP2TerminalReroutePreservesRedeliveryBackoff(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "reroute-parent")
	childAgent := h.addAgent(t, "reroute-child")
	project := h.addProject(t, "reroute-project", "")
	conversation, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: childAgent.ID, Body: "conversation",
		Wake: false, RequestID: "reroute-conversation",
	})
	requireNoError(t, err)
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Kind: core.TaskWork,
		Title: "parent", Priority: 100, RequestID: "reroute-parent",
	})
	requireNoError(t, err)
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), parentClaim.Run.ID, "reroute-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "child",
		Priority: 90, RequestID: "reroute-child",
	})
	requireNoError(t, err)
	message, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: parentClaim.Token, RecipientKind: "agent", RecipientID: childAgent.ID,
		TaskID: child.ID, Body: "delivery input", Wake: true, RequestID: "reroute-message",
	})
	requireNoError(t, err)
	soonMessage, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: parentClaim.Token, RecipientKind: "agent", RecipientID: childAgent.ID,
		TaskID: child.ID, Body: "newer delivery input", Wake: true, RequestID: "reroute-message-soon",
	})
	requireNoError(t, err)
	waitAndTerminate(t, h, parentClaim, "reroute-parent-wait")

	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim = %#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), childClaim.Run.ID, "reroute-child-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RecordMessagesDelivered(context.Background(), core.MessageDeliveryInput{
		RunID: childClaim.Run.ID, MessageIDs: []string{message.ID, soonMessage.ID}, RequestID: "reroute-delivered",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		delivered, err := tx.Message(message.ID)
		if err != nil {
			return err
		}
		expectedVersion := delivered.Version
		delivered.DeliveryCount = 2
		delivered.Version++
		return tx.UpdateMessage(delivered, expectedVersion, core.MessageDelivered)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: childClaim.Token, Outcome: core.OutcomeFail,
		Reason: "child failed", RequestID: "reroute-fail",
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: childClaim.Run.ID, State: core.RunExited,
		ExitCode: intPointer(1), RequestID: "reroute-terminal",
	})
	requireNoError(t, err)
	if len(terminal.Redelivered) != 2 {
		t.Fatalf("redelivery backoff = %#v run=%#v", terminal.Redelivered, terminal.Run)
	}
	redelivered := make(map[string]core.Message, len(terminal.Redelivered))
	for _, candidate := range terminal.Redelivered {
		if candidate.NextDeliveryAt <= terminal.Run.EndedAt {
			t.Fatalf("redelivery bypassed backoff: %#v run=%#v", candidate, terminal.Run)
		}
		redelivered[candidate.ID] = candidate
	}
	if redelivered[message.ID].NextDeliveryAt <= redelivered[soonMessage.ID].NextDeliveryAt {
		t.Fatalf("test setup did not create distinct backoffs: %#v", terminal.Redelivered)
	}
	rerouted := make(map[string]core.Message, 2)
	err = h.database.Transact(context.Background(), func(tx core.Transaction) error {
		for _, id := range []string{message.ID, soonMessage.ID} {
			persisted, err := tx.Message(id)
			if err != nil {
				return err
			}
			rerouted[id] = persisted
		}
		return nil
	})
	requireNoError(t, err)
	for id, persisted := range rerouted {
		if persisted.TaskID != conversation.Task.ID || persisted.State != core.MessagePending ||
			persisted.NextDeliveryAt != redelivered[id].NextDeliveryAt {
			t.Fatalf("reroute bypassed redelivery backoff: %#v redelivered=%#v", persisted, redelivered[id])
		}
	}
	conversationTask, err := h.database.Task(context.Background(), conversation.Task.ID)
	requireNoError(t, err)
	if conversationTask.Status != core.TaskQueued || conversationTask.NextRunAt != rerouted[soonMessage.ID].NextDeliveryAt {
		t.Fatalf("reroute did not keep earliest delivery time: %#v messages=%#v", conversationTask, rerouted)
	}
}

func TestP2ChildCreateReplaysAfterOutcomeAndFinishingBlocksCancel(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "parent-agent")
	childAgent := h.addAgent(t, "child-agent")
	project := h.addProject(t, "child-project", "")
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Title: "parent", RequestID: "parent-task",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "parent-active"); err != nil {
		t.Fatal(err)
	}
	childInput := core.CreateChildTaskInput{
		Token: claim.Token, AssigneeAgentID: childAgent.ID, Title: "child", RequestID: "child-create",
	}
	child, err := h.service.CreateChildTask(context.Background(), childInput)
	requireNoError(t, err)
	if child.ParentTaskID != parent.ID || child.ProjectID != project.ID || child.CreatedByID != parentAgent.ID {
		t.Fatalf("child scope = %#v", child)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "waiting for child", RequestID: "parent-wait",
	}); err != nil {
		t.Fatal(err)
	}
	if replay, err := h.service.CreateChildTask(context.Background(), childInput); err != nil || replay.ID != child.ID {
		t.Fatalf("child replay after revoke=%#v err=%v", replay, err)
	}
	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: parent.ID, Reason: "competing cancel", RequestID: "parent-cancel",
	}); !core.IsCode(err, core.CodeActionInProgress) {
		t.Fatalf("finishing cancel error=%v", err)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func TestP2PendingAckWinsDelayedDeliveryRecord(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "ack-race-agent")
	project := h.addProject(t, "ack-race-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "ack before delivery", Wake: true, RequestID: "ack-race-chat",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "ack-race-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.AcknowledgeAgentMessages(context.Background(), core.AcknowledgeMessagesInput{
		Token: claim.Token, MessageIDs: []string{chat.Message.ID}, RequestID: "ack-race-ack",
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := h.service.RecordMessagesDelivered(context.Background(), core.MessageDeliveryInput{
		RunID: claim.Run.ID, MessageIDs: []string{chat.Message.ID}, RequestID: "ack-race-deliver",
	})
	if err != nil || len(messages) != 1 || messages[0].State != core.MessageAcknowledged || messages[0].DeliveryCount != 0 {
		t.Fatalf("late delivery result=%#v err=%v", messages, err)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	requireNoError(t, err)
	if countEvent(events, "message.delivered") != 0 || countEvent(events, "message.acknowledged") != 1 {
		t.Fatalf("ack race Events=%#v", events)
	}
}

func TestP2ExhaustedWakeRemainsWaitingUntilExplicitRetry(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "exhaust-agent")
	project := h.addProject(t, "exhaust-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "one automatic delivery", Wake: true, RequestID: "exhaust-chat",
	})
	requireNoError(t, err)
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		message, err := tx.Message(chat.Message.ID)
		if err != nil {
			return err
		}
		expectedVersion := message.Version
		message.MaxDeliveries = 1
		message.Version++
		return tx.UpdateMessage(message, expectedVersion, core.MessagePending)
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "exhaust-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RecordMessagesDelivered(context.Background(), core.MessageDeliveryInput{
		RunID: claim.Run.ID, MessageIDs: []string{chat.Message.ID}, RequestID: "exhaust-deliver",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "turn complete", RequestID: "exhaust-wait",
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: intPointer(0), RequestID: "exhaust-terminal",
	})
	requireNoError(t, err)
	if terminal.Task.Status != core.TaskWaiting || len(terminal.Redelivered) != 1 || terminal.Redelivered[0].NextDeliveryAt != "" {
		t.Fatalf("exhausted terminal projection=%#v messages=%#v", terminal.Task, terminal.Redelivered)
	}
	retried, err := h.service.RetryMessage(context.Background(), chat.Message.ID, "exhaust-retry")
	requireNoError(t, err)
	if retried.DeliveryCount != 0 || retried.NextDeliveryAt == "" {
		t.Fatalf("explicit retry=%#v", retried)
	}
	task, err := h.database.Task(context.Background(), chat.Task.ID)
	if err != nil || task.Status != core.TaskQueued {
		t.Fatalf("retry wake task=%#v err=%v", task, err)
	}
}

func TestP2TaskWakeReenablesExhaustedMessages(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "task-wake-agent")
	project := h.addProject(t, "task-wake-project", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "exhausted input",
		Wake: false, RequestID: "task-wake-chat",
	})
	requireNoError(t, err)
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		message, err := tx.Message(chat.Message.ID)
		if err != nil {
			return err
		}
		expectedVersion := message.Version
		message.DeliveryCount = message.MaxDeliveries
		message.NextDeliveryAt = ""
		message.LastDeliveryError = "automatic delivery exhausted"
		message.Version++
		return tx.UpdateMessage(message, expectedVersion, core.MessagePending)
	}); err != nil {
		t.Fatal(err)
	}
	woken, err := h.service.WakeTask(context.Background(), core.TaskActionInput{
		TaskID: chat.Task.ID, RequestID: "task-wake-explicit",
	})
	requireNoError(t, err)
	if woken.Status != core.TaskQueued {
		t.Fatalf("woken task = %#v", woken)
	}
	messages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: chat.Task.ID})
	requireNoError(t, err)
	if len(messages.Items) != 1 || messages.Items[0].DeliveryCount != 0 ||
		messages.Items[0].NextDeliveryAt == "" || messages.Items[0].LastDeliveryError != "" {
		t.Fatalf("task wake did not re-enable message: %#v", messages.Items)
	}
}

func TestP2ChildFailureReroutesUnresolvedMessageAndWakesParent(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "notify-parent-agent")
	childAgent := h.addAgent(t, "notify-child-agent")
	project := h.addProject(t, "notify-project", "")
	conversation, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: childAgent.ID, Body: "existing child conversation",
		Wake: false, RequestID: "notify-conversation",
	})
	requireNoError(t, err)
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Title: "notify parent",
		Priority: 20, RequestID: "notify-parent",
	})
	requireNoError(t, err)
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim=%#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), parentClaim.Run.ID, "notify-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "notify child",
		Priority: 10, RequestID: "notify-child",
	})
	requireNoError(t, err)
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: parentClaim.Token, Outcome: core.OutcomeWait, Reason: "waiting for child", RequestID: "notify-parent-wait",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: parentClaim.Run.ID, State: core.RunExited, ExitCode: intPointer(0), RequestID: "notify-parent-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim=%#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), childClaim.Run.ID, "notify-child-active"); err != nil {
		t.Fatal(err)
	}
	const unresolvedID = "msg_notify_unresolved"
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		now := h.clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
		return tx.InsertMessage(core.Message{
			ID: unresolvedID, ProjectID: project.ID, TaskID: child.ID, SenderKind: "boss",
			RecipientKind: "agent", RecipientID: childAgent.ID, Body: "must survive child failure",
			Wake: true, State: core.MessagePending, MaxDeliveries: 3, NextDeliveryAt: now,
			IdempotencyKey: "notify-unresolved", Version: 1, CreatedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: childClaim.Token, Outcome: core.OutcomeFail, Reason: "child failed", RequestID: "notify-child-fail",
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
		RunID: childClaim.Run.ID, State: core.RunExited, ExitCode: intPointer(1), RequestID: "notify-child-terminal",
	})
	requireNoError(t, err)
	if terminal.Task.Status != core.TaskFailed {
		t.Fatalf("child terminal task=%#v", terminal.Task)
	}
	rerouted, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: conversation.Task.ID})
	requireNoError(t, err)
	foundRerouted := false
	for _, message := range rerouted.Items {
		if message.ID == unresolvedID {
			foundRerouted = message.RelatedTaskID == child.ID && message.State == core.MessagePending
		}
	}
	if !foundRerouted {
		t.Fatalf("unresolved message was not rerouted: %#v", rerouted.Items)
	}
	parentAfter, err := h.database.Task(context.Background(), parent.ID)
	if err != nil || parentAfter.Status != core.TaskQueued {
		t.Fatalf("parent was not woken: %#v err=%v", parentAfter, err)
	}
	parentMessages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: parent.ID})
	requireNoError(t, err)
	foundChildResult := false
	for _, message := range parentMessages.Items {
		if message.SystemCode == "child_result" && message.RelatedTaskID == child.ID && message.RecipientID == parentAgent.ID {
			foundChildResult = true
		}
	}
	if !foundChildResult {
		t.Fatalf("parent child-result message missing: %#v", parentMessages.Items)
	}
}

func intPointer(value int) *int { return &value }
