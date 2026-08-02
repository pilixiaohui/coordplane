package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

// TestCT06CaptureSubmittedDisposesUnresolvedAgentMessages is the lowest-layer
// regression for the CT-06 contract gap caught by RMA-02 live #10: capture
// completion (task enters submitted) never disposed unresolved agent messages.
//
// Shape is the real evidence: an integration task carries an
// integration_required system message (recipient=agent, wake=true,
// max_deliveries=3); the integration run delivers it and terminates with
// submit before the agent acknowledges, so CT-06 reverts it to pending with
// delivery backoff. When capture then completes (task -> submitted), the
// message must be rerouted to the recipient conversation or cancelled in the
// same transaction — it must not stay pending and permanently block GC /
// agent archive.
func TestCT06CaptureSubmittedDisposesUnresolvedAgentMessages(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "ct06-capture-worker")
	integrator := h.addAgent(t, "ct06-capture-integrator")
	project := h.addProject(t, "ct06-capture-project", integrator.ID)

	integration, message := driveIntegrationTaskWithUnackedMessage(t, h, project, worker, integrator, "ct06-capture")
	// No conversation exists for the integrator agent here, so the contract
	// falls back to the cancel branch (CT-06: reroute to recipient
	// conversation or cancel).
	disposed := taskMessages(t, h, integration.ID)
	if len(disposed) != 1 || disposed[0].ID != message.ID || disposed[0].State != core.MessageCancelled {
		t.Fatalf("unresolved message disposition on capture submitted = %#v", disposed)
	}
}

// TestCT06AdvanceCompletedDisposesUnresolvedAgentMessages is the second
// missing CT-06 path: advance completion (task enters completed, including
// the accept cascade that completes the integration source) never disposed
// unresolved agent messages. Same evidence shape as the capture regression:
// integration_required-style message (recipient=agent, wake=true,
// max_deliveries=3) still delivered-unacked when the advance completes.
func TestCT06AdvanceCompletedDisposesUnresolvedAgentMessages(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "ct06-advance-worker")
	integrator := h.addAgent(t, "ct06-advance-integrator")
	project := h.addProject(t, "ct06-advance-project", integrator.ID)

	integration, _ := driveIntegrationTaskWithUnackedMessage(t, h, project, worker, integrator, "ct06-advance")

	// As in the live scenario, the integrator agent owns a conversation, so a
	// still-unresolved message reroutes there instead of being cancelled.
	seed, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
		ProjectID: project.ID, AgentID: integrator.ID, Body: "conversation seed",
		RequestID: "ct06-advance-seed",
	})
	requireNoError(t, err)
	conversation, err := h.database.Task(context.Background(), seed.TaskID)
	requireNoError(t, err)
	if conversation.Kind != core.TaskConversation || conversation.Status != core.TaskWaiting {
		t.Fatalf("integrator conversation = %#v", conversation)
	}

	// A delivered-unacked agent message still owned by the integration task
	// when the advance completes (the delivered shape observed in RMA-02).
	fixtureID, err := h.ids.New("msg")
	requireNoError(t, err)
	now := h.clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	fixture := core.Message{
		ID: fixtureID, ProjectID: project.ID, TaskID: integration.ID,
		SenderKind: "system", RecipientKind: "agent", RecipientID: integrator.ID,
		SystemCode: "integration_required", Body: "Integrate source task into canonical.",
		Wake: true, State: core.MessageDelivered, DeliveredRunID: integration.HeadRunID,
		DeliveryCount: 1, MaxDeliveries: 3, IdempotencyKey: "fixture:" + integration.ID,
		Version: 1, CreatedAt: now, DeliveredAt: now,
	}
	requireNoError(t, h.database.Transact(context.Background(), func(tx core.Transaction) error {
		return tx.InsertMessage(fixture)
	}))

	h.git.mu.Lock()
	h.git.advanceOutcome = core.GitAdvanceUpdated
	h.git.mu.Unlock()
	requireNoError(t, h.service.ReconcileGit(context.Background()))

	integrationAfter, err := h.database.Task(context.Background(), integration.ID)
	requireNoError(t, err)
	if integrationAfter.Status != core.TaskCompleted {
		t.Fatalf("integration status after advance = %#v", integrationAfter)
	}
	if messages := taskMessages(t, h, integration.ID); len(messages) != 0 {
		t.Fatalf("messages still owned by completed integration task = %#v", messages)
	}
	rerouted := taskMessages(t, h, fixtureID)
	if len(rerouted) != 1 || rerouted[0].TaskID != conversation.ID ||
		rerouted[0].State != core.MessagePending ||
		rerouted[0].LastDeliveryError != "rerouted from non-delivery task" {
		t.Fatalf("unresolved message disposition on advance completed = %#v", rerouted)
	}
	conversationAfter, err := h.database.Task(context.Background(), conversation.ID)
	requireNoError(t, err)
	if conversationAfter.Status != core.TaskQueued {
		t.Fatalf("conversation after reroute = %#v", conversationAfter)
	}
}

// driveIntegrationTaskWithUnackedMessage reproduces the RMA-02 evidence
// chain: source task submitted, Boss accept, stale advance creates the
// integration task with an integration_required message, the integration run
// delivers it, the agent submits without acknowledging (CT-06 reverts it to
// pending with backoff), then capture completes and the integration task
// enters submitted.
func driveIntegrationTaskWithUnackedMessage(t *testing.T, h *harness, project core.Project, worker, integrator core.Agent, suffix string) (core.Task, core.Message) {
	t.Helper()
	source := createAndSubmitCodeTask(t, h, project, worker, "cccccccccccccccccccccccccccccccccccccccc", suffix+"-source")
	requireNoError(t, h.service.ReconcileGit(context.Background()))
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: source.ID, IntegrationAgentID: integrator.ID, RequestID: suffix + "-accept",
	}); err != nil {
		t.Fatal(err)
	}
	h.git.mu.Lock()
	h.git.advanceOutcome = core.GitAdvanceStale
	h.git.mu.Unlock()
	requireNoError(t, h.service.ReconcileGit(context.Background()))

	integration := integrationTaskFor(t, h, source)
	messages := taskMessages(t, h, integration.ID)
	if len(messages) != 1 || messages[0].SystemCode != "integration_required" ||
		messages[0].RecipientKind != "agent" || messages[0].RecipientID != integrator.ID ||
		!messages[0].Wake || messages[0].MaxDeliveries != 3 || messages[0].State != core.MessagePending {
		t.Fatalf("integration_required message = %#v", messages)
	}
	message := messages[0]

	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != integration.ID {
		t.Fatalf("integration claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, suffix+"-active"); err != nil {
		t.Fatal(err)
	}
	delivered, err := h.service.RecordMessagesDelivered(context.Background(), core.MessageDeliveryInput{
		RunID: claim.Run.ID, MessageIDs: []string{message.ID}, RequestID: suffix + "-delivered",
	})
	requireNoError(t, err)
	if len(delivered) != 1 || delivered[0].State != core.MessageDelivered || delivered[0].DeliveryCount != 1 {
		t.Fatalf("delivered message = %#v", delivered)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "integration captured " + suffix,
		ExpectedHead: integration.BaseSHA, RequestID: suffix + "-submit",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, claim.Run.ID, suffix+"-terminal")
	reverted := taskMessages(t, h, integration.ID)
	if len(reverted) != 1 || reverted[0].State != core.MessagePending ||
		reverted[0].DeliveryCount != 1 || reverted[0].NextDeliveryAt == "" {
		t.Fatalf("CT-06 revert shape = %#v", reverted)
	}
	requireNoError(t, h.service.ReconcileGit(context.Background()))
	integration, err = h.database.Task(context.Background(), integration.ID)
	requireNoError(t, err)
	if integration.Status != core.TaskSubmitted {
		t.Fatalf("integration status after capture = %#v", integration)
	}
	return integration, message
}

func integrationTaskFor(t *testing.T, h *harness, source core.Task) core.Task {
	t.Helper()
	sourceAfter, err := h.database.Task(context.Background(), source.ID)
	requireNoError(t, err)
	if sourceAfter.IntegrationTaskID == "" {
		t.Fatalf("source has no integration task: %#v", sourceAfter)
	}
	integration, err := h.database.Task(context.Background(), sourceAfter.IntegrationTaskID)
	requireNoError(t, err)
	return integration
}

func taskMessages(t *testing.T, h *harness, taskID string) []core.Message {
	t.Helper()
	result, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: taskID})
	requireNoError(t, err)
	return result.Items
}
