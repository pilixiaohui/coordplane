package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestP2RuntimeRetryExhaustionDisposesMessagesAndNotifiesParent(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "runtime-parent")
	childAgent := h.addAgent(t, "runtime-child")
	project := h.addProject(t, "runtime-exhaustion-project", "")
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Kind: core.TaskWork,
		Title: "parent", Priority: 20, RequestID: "runtime-parent-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), parentClaim.Run.ID, "runtime-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "child",
		Priority: 10, MaxRetries: 0, RequestID: "runtime-child-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: parentClaim.Token, RecipientKind: "agent", RecipientID: childAgent.ID,
		TaskID: child.ID, Body: "pending child input", Wake: true, RequestID: "runtime-child-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitAndTerminate(t, h, parentClaim, "runtime-parent-wait")

	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim = %#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), childClaim.Run.ID, "runtime-child-active"); err != nil {
		t.Fatal(err)
	}
	terminal, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: childClaim.Run.ID, State: core.RunInterrupted,
		TerminalReason: "runtime lost", RequestID: "runtime-child-terminal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Task.Status != core.TaskFailed || terminal.Task.CurrentRunID != "" {
		t.Fatalf("exhausted child projection = %#v", terminal.Task)
	}
	message, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Items) != 1 || message.Items[0].ID != unresolved.ID || message.Items[0].State != core.MessageCancelled {
		t.Fatalf("unresolved child message disposition = %#v", message.Items)
	}
	parentAfter, err := h.database.Task(context.Background(), parent.ID)
	if err != nil || parentAfter.Status != core.TaskQueued {
		t.Fatalf("parent wake projection = %#v err=%v", parentAfter, err)
	}
	parentMessages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !hasChildResult(parentMessages.Items, child.ID, "agent", parentAgent.ID) {
		t.Fatalf("parent notification missing: %#v", parentMessages.Items)
	}
}

func TestP2ClosedParentChildResultFallsBackToBoss(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "fallback-parent")
	childAgent := h.addAgent(t, "fallback-child")
	project := h.addProject(t, "fallback-project", "")
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Kind: core.TaskWork,
		Title: "parent", Priority: 20, RequestID: "fallback-parent-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), parentClaim.Run.ID, "fallback-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "child",
		Priority: 10, RequestID: "fallback-child-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitAndTerminate(t, h, parentClaim, "fallback-parent-wait")
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: parent.ID, Reason: "parent closed", RequestID: "fallback-parent-cancel",
	}); err != nil {
		t.Fatal(err)
	}

	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim = %#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), childClaim.Run.ID, "fallback-child-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: childClaim.Token, Outcome: core.OutcomeFail,
		Reason: "child failed", RequestID: "fallback-child-fail",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: childClaim.Run.ID, State: core.RunExited,
		ExitCode: intPointer(1), RequestID: "fallback-child-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !hasChildResult(messages.Items, child.ID, "boss", "") {
		t.Fatalf("Boss fallback missing parent/child association: %#v", messages.Items)
	}
}

func TestP2CancelPreservesExistingRunStopOperation(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "stop-cancel-agent")
	project := h.addProject(t, "stop-cancel-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "stop-cancel")
	stopped, err := h.service.RequestRunStop(context.Background(), core.RunStopInput{
		RunID: claim.Run.ID, Reason: "operator stop", OperationID: "op-existing-stop",
		RequestID: "stop-before-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: claim.Task.ID, Reason: "cancel responsibility", RequestID: "cancel-after-stop",
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.database.Task(context.Background(), claim.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != core.TaskCancelled || cancelled.CurrentRunID != claim.Run.ID ||
		cancelled.Generation != claim.Run.Generation+1 {
		t.Fatalf("cancelled task lost live Run ownership: %#v", cancelled)
	}
	persisted, err := h.database.Run(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.StopOperationID != stopped.StopOperationID || persisted.StopRequestedAt != stopped.StopRequestedAt ||
		persisted.StopReason != stopped.StopReason {
		t.Fatalf("cancel replaced existing stop identity: before=%#v after=%#v", stopped, persisted)
	}
	if persisted.TokenRevokedAt == "" {
		t.Fatal("cancelled Run token remained valid")
	}
	terminal, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunCancelled,
		TerminalReason: "cancelled by operator", RequestID: "cancel-terminal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Task.Status != core.TaskCancelled || terminal.Task.CurrentRunID != "" {
		t.Fatalf("terminal cancellation projection = %#v", terminal.Task)
	}
}

func waitAndTerminate(t *testing.T, h *harness, claim core.Claim, requestPrefix string) {
	t.Helper()
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait,
		Reason: "waiting for child", RequestID: requestPrefix,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
		RunID: claim.Run.ID, State: core.RunExited,
		ExitCode: intPointer(0), RequestID: requestPrefix + "-terminal",
	}); err != nil {
		t.Fatal(err)
	}
}

func hasChildResult(messages []core.Message, childID, recipientKind, recipientID string) bool {
	for _, message := range messages {
		if message.SystemCode == "child_result" && message.RelatedTaskID == childID &&
			message.RecipientKind == recipientKind && message.RecipientID == recipientID {
			return true
		}
	}
	return false
}
