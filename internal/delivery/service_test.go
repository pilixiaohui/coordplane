package delivery_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/capability"
	"coordplane/internal/coordination"
	"coordplane/internal/delivery"
	"coordplane/internal/policy"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestNotifyMailboxSameTurnRecordsAcceptedAttemptWithoutResolvingMailbox(t *testing.T) {
	ctx := context.Background()
	h := newDeliveryHarness(t)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	fullBody := strings.Repeat("full mailbox body must not be in delivery signal ", 20)
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             fullBody,
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	result, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	if result.State != "accepted" || result.RouteID != coordinator.Route.ID {
		t.Fatalf("delivery result = %+v, want accepted on coordinator route", result)
	}
	attempt, err := h.delivery.Attempt(ctx, result.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}
	if attempt.State != "accepted" || attempt.MailboxID != sent.Data.MailboxID || attempt.RouteID != coordinator.Route.ID {
		t.Fatalf("attempt = %+v, want accepted for mailbox/route", attempt)
	}
	var signal map[string]any
	if err := json.Unmarshal(attempt.SignalJSON, &signal); err != nil {
		t.Fatalf("decode delivery signal: %v", err)
	}
	if signal["envelope_id"] != sent.Data.EnvelopeID || signal["envelope_kind"] != "message" {
		t.Fatalf("delivery signal envelope projection = %#v, want message envelope %s", signal, sent.Data.EnvelopeID)
	}
	if bodyPreview, _ := signal["body_preview"].(string); bodyPreview == "" || len(bodyPreview) > 240 || bodyPreview == fullBody {
		t.Fatalf("delivery signal body preview = %q, want bounded short preview", bodyPreview)
	}
	if !strings.Contains(string(attempt.SignalJSON), "mailbox.list/get") {
		t.Fatalf("delivery signal missing mailbox read guidance: %s", attempt.SignalJSON)
	}
	steers := h.fake.Steers()
	if len(steers) != 1 {
		t.Fatalf("fake steers = %d, want 1", len(steers))
	}
	if steers[0].Payload.MailboxID != sent.Data.MailboxID || steers[0].Payload.Reason != "question" {
		t.Fatalf("steer payload = %+v", steers[0].Payload)
	}
	rawPayload, err := json.Marshal(steers[0].Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(rawPayload), "full mailbox body") {
		t.Fatalf("steer payload leaked mailbox body: %s", rawPayload)
	}
	state, followup := mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "pending" || followup != "" {
		t.Fatalf("mailbox after accepted steer = %s/%s, want pending/empty followup", state, followup)
	}

	resolved := callAccepted[coordination.MailboxItem](t, h.dispatcher, capability.Call{
		CapabilityName: "mailbox.resolve",
		Subject:        agentSubject("coordinator"),
		Input: mustRaw(t, map[string]any{
			"mailbox_id":   sent.Data.MailboxID,
			"followup_ref": "message:reply",
		}),
	})
	if resolved.State != "resolved" || resolved.FollowupRef != "message:reply" {
		t.Fatalf("resolved mailbox = %+v", resolved)
	}
	state, followup = mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "resolved" || followup != "message:reply" {
		t.Fatalf("durable resolved mailbox = %s/%s, want resolved/message:reply", state, followup)
	}
}

func TestNotifyMailboxWithoutActiveRouteCreatesFallbackResumeQueue(t *testing.T) {
	ctx := context.Background()
	h := newDeliveryHarness(t)
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             "no active route body",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	result, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	if result.State != "fallback" || result.QueueItemID == "" || result.RouteID != "" {
		t.Fatalf("delivery result = %+v, want fallback queue without route", result)
	}
	attempt, err := h.delivery.Attempt(ctx, result.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}
	if attempt.State != "fallback" || attempt.LastError != "no active route" {
		t.Fatalf("attempt = %+v, want fallback no active route", attempt)
	}
	if len(h.fake.Steers()) != 0 {
		t.Fatalf("fake steer called despite no active route")
	}
	assertResumeQueue(t, ctx, h.db, result.QueueItemID, sent.Data.MailboxID)
	resumeEvent := assertResumeQueuedEvent(t, ctx, h.db, sent.Data.MailboxID)
	if resumeEvent["queue_item_id"] != result.QueueItemID || resumeEvent["reason"] != "no_active_route" {
		t.Fatalf("delivery.resume_queued payload = %#v, want no_active_route for queue %s", resumeEvent, result.QueueItemID)
	}
	if strings.Contains(mustJSON(t, resumeEvent), "no active route body") {
		t.Fatalf("delivery.resume_queued leaked mailbox body: %#v", resumeEvent)
	}
	state, followup := mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "pending" || followup != "" {
		t.Fatalf("mailbox after fallback = %s/%s, want pending/empty followup", state, followup)
	}
}

func TestNotifyMailboxTriggerTurnFalseDoesNotSteerOrResume(t *testing.T) {
	ctx := context.Background()
	h := newDeliveryHarness(t)
	addContract(t, ctx, h.coordination, "coordinator")
	if _, err := h.runner.StartNext(ctx, "coordinator"); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "note",
		Body:             "suppressed turn body must not force steer or resume",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE mailbox_items SET trigger_turn = 0 WHERE id = ?`, sent.Data.MailboxID); err != nil {
		t.Fatalf("disable trigger_turn: %v", err)
	}

	result, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	if result.State != "suppressed" || result.QueueItemID != "" || result.RouteID != "" {
		t.Fatalf("delivery result = %+v, want suppressed without route/queue", result)
	}
	attempt, err := h.delivery.Attempt(ctx, result.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}
	if attempt.State != "suppressed" || attempt.LastError != "trigger_turn disabled" {
		t.Fatalf("attempt = %+v, want suppressed trigger_turn disabled", attempt)
	}
	if len(h.fake.Steers()) != 0 {
		t.Fatalf("fake steer called for trigger_turn=false mailbox")
	}
	if got := countRowsWhere(t, ctx, h.db, "queue_items", "queue_name = 'runtime.resume'"); got != 0 {
		t.Fatalf("runtime.resume queue rows = %d, want 0", got)
	}
}

func TestNotifyMailboxRepeatedFallbackEnqueuesSingleResumeItem(t *testing.T) {
	ctx := context.Background()
	h := newDeliveryHarness(t)
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             "duplicate fallback body",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	first, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("first notify mailbox: %v", err)
	}
	second, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("second notify mailbox: %v", err)
	}
	if first.State != "fallback" || second.State != "fallback" {
		t.Fatalf("delivery states = %s/%s, want repeated fallback", first.State, second.State)
	}
	if first.QueueItemID == "" || second.QueueItemID == "" || first.QueueItemID != second.QueueItemID {
		t.Fatalf("queue ids = %q/%q, want same fallback queue item", first.QueueItemID, second.QueueItemID)
	}
	assertResumeQueue(t, ctx, h.db, first.QueueItemID, sent.Data.MailboxID)
	if got := countResumeQueueForMailbox(t, ctx, h.db, sent.Data.MailboxID); got != 1 {
		t.Fatalf("fallback resume queue rows for mailbox = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "delivery_attempts", "mailbox_item_id = '"+sent.Data.MailboxID+"' AND state = 'fallback'"); got != 2 {
		t.Fatalf("fallback delivery attempts for mailbox = %d, want 2", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type = 'delivery.resume_queued' AND aggregate_id = '"+sent.Data.MailboxID+"'"); got != 2 {
		t.Fatalf("delivery.resume_queued events for mailbox = %d, want one per fallback attempt", got)
	}
	if len(h.fake.Steers()) != 0 {
		t.Fatalf("fake steer called despite no active route")
	}
	state, followup := mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "pending" || followup != "" {
		t.Fatalf("mailbox after repeated fallback = %s/%s, want pending/empty followup", state, followup)
	}
}

func TestNotifyMailboxSteerFailureRecordsFailedAttemptAndRetryCanSucceed(t *testing.T) {
	ctx := context.Background()
	h := newDeliveryHarness(t)
	addContract(t, ctx, h.coordination, "coordinator")
	if _, err := h.runner.StartNext(ctx, "coordinator"); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             "retry body",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	h.fake.FailSteer("turn mismatch")
	failed, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify failed steer: %v", err)
	}
	if failed.State != "failed" || failed.QueueItemID == "" {
		t.Fatalf("failed delivery result = %+v, want failed with fallback queue", failed)
	}
	failedAttempt, err := h.delivery.Attempt(ctx, failed.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("failed attempt: %v", err)
	}
	if failedAttempt.State != "failed" || !strings.Contains(failedAttempt.LastError, "turn mismatch") {
		t.Fatalf("failed attempt = %+v, want turn mismatch failure", failedAttempt)
	}
	assertResumeQueue(t, ctx, h.db, failed.QueueItemID, sent.Data.MailboxID)
	resumeEvent := assertResumeQueuedEvent(t, ctx, h.db, sent.Data.MailboxID)
	if resumeEvent["queue_item_id"] != failed.QueueItemID ||
		resumeEvent["reason"] != "steer_failed" ||
		resumeEvent["supports_same_turn_steer"] != true ||
		resumeEvent["cli_backend"] != "fake" {
		t.Fatalf("delivery.resume_queued payload = %#v, want failed steer fallback with fake capability", resumeEvent)
	}
	if got := countRowsWhere(t, ctx, h.db, "queue_items", "queue_name = 'runtime.resume'"); got != 1 {
		t.Fatalf("fallback queue rows after failed steer = %d, want 1", got)
	}

	h.fake.FailSteer("")
	accepted, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("retry notify: %v", err)
	}
	if accepted.State != "accepted" {
		t.Fatalf("retry result = %+v, want accepted", accepted)
	}
	acceptedAttempt, err := h.delivery.Attempt(ctx, accepted.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("accepted attempt: %v", err)
	}
	if acceptedAttempt.State != "accepted" {
		t.Fatalf("accepted attempt = %+v", acceptedAttempt)
	}
	if got := countRowsWhere(t, ctx, h.db, "delivery_attempts", "mailbox_item_id = '"+sent.Data.MailboxID+"'"); got != 2 {
		t.Fatalf("delivery attempts for mailbox = %d, want failed + accepted retry", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "queue_items", "queue_name = 'runtime.resume'"); got != 1 {
		t.Fatalf("fallback queue rows after retry = %d, want idempotent single row", got)
	}
	state, followup := mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "pending" || followup != "" {
		t.Fatalf("mailbox after retry accepted = %s/%s, want pending/empty followup", state, followup)
	}
}

func TestNotifyMailboxUsesCommunicationPolicySignalLimits(t *testing.T) {
	ctx := context.Background()
	cfg := deliveryTeamConfig()
	cfg.Communication = teamconfig.CommunicationConfig{
		AllowDirectMessage:    true,
		AllowFollowupTask:     true,
		TaskRequiresContract:  true,
		SignalSummaryMaxBytes: 32,
		SignalBodyMaxBytes:    64,
	}
	h := newDeliveryHarnessWithConfig(t, cfg)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	body := strings.Repeat("policy bounded preview ", 20)
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             body,
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	result, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	if result.State != "accepted" || result.RouteID != coordinator.Route.ID {
		t.Fatalf("delivery result = %+v, want accepted on coordinator route", result)
	}
	attempt, err := h.delivery.Attempt(ctx, result.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}
	var signal map[string]any
	if err := json.Unmarshal(attempt.SignalJSON, &signal); err != nil {
		t.Fatalf("decode delivery signal: %v", err)
	}
	summary, _ := signal["summary"].(string)
	bodyPreview, _ := signal["body_preview"].(string)
	if summary == "" || len(summary) > 32 {
		t.Fatalf("signal summary len=%d value=%q, want <=32", len(summary), summary)
	}
	if bodyPreview == "" || len(bodyPreview) > 64 {
		t.Fatalf("signal body preview len=%d value=%q, want <=64", len(bodyPreview), bodyPreview)
	}
	if bodyPreview == body {
		t.Fatalf("signal body preview included full body")
	}
}

func TestNotifyMailboxRedactsSensitiveMarkersFromSignalPreview(t *testing.T) {
	ctx := context.Background()
	cfg := deliveryTeamConfig()
	cfg.Communication = teamconfig.CommunicationConfig{
		AllowDirectMessage:    true,
		AllowFollowupTask:     true,
		TaskRequiresContract:  true,
		SignalSummaryMaxBytes: 32,
		SignalBodyMaxBytes:    64,
	}
	h := newDeliveryHarnessWithConfig(t, cfg)
	addContract(t, ctx, h.coordination, "coordinator")
	if _, err := h.runner.StartNext(ctx, "coordinator"); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	body := "COORDPLANE_TOKEN=tok_live SECRET=shh path=/home/zxh/work runtime=/tmp/coordplane.db"
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             body,
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want accepted mailbox", sent)
	}

	result, err := h.delivery.NotifyMailbox(ctx, sent.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	attempt, err := h.delivery.Attempt(ctx, result.DeliveryAttemptID)
	if err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}
	var signal map[string]any
	if err := json.Unmarshal(attempt.SignalJSON, &signal); err != nil {
		t.Fatalf("decode delivery signal: %v", err)
	}
	rawSignal := string(attempt.SignalJSON)
	for _, forbidden := range []string{"COORDPLANE_TOKEN", "tok_live", "SECRET", "shh", "/home/zxh", "/tmp/coordplane.db"} {
		if strings.Contains(rawSignal, forbidden) {
			t.Fatalf("delivery signal leaked sensitive marker %q: %s", forbidden, rawSignal)
		}
	}
	bodyPreview, _ := signal["body_preview"].(string)
	if bodyPreview == "" || !strings.Contains(bodyPreview, "[redacted]") || len(bodyPreview) > 64 {
		t.Fatalf("body preview = %q, want redacted bounded preview", bodyPreview)
	}
	summary, _ := signal["summary"].(string)
	if strings.Contains(summary, "COORDPLANE_TOKEN") || strings.Contains(summary, "SECRET") || len(summary) > 32 {
		t.Fatalf("summary preview = %q, want redacted bounded summary", summary)
	}
}

type deliveryHarness struct {
	db           *sql.DB
	store        *store.Store
	coordination *coordination.Service
	dispatcher   *policy.Dispatcher
	runner       *cpruntime.Runner
	fake         *cpruntime.FakeCLIAdapter
	delivery     *delivery.Service
}

func newDeliveryHarness(t *testing.T) deliveryHarness {
	t.Helper()
	return newDeliveryHarnessWithConfig(t, deliveryTeamConfig())
}

func newDeliveryHarnessWithConfig(t *testing.T, cfg teamconfig.Config) deliveryHarness {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register builtin skills: %v", err)
	}
	coordSvc := coordination.NewService(st)
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, coordSvc); err != nil {
		t.Fatalf("register coordination capabilities: %v", err)
	}
	dispatcher := policy.NewDispatcher(cfg, registry)
	fake := cpruntime.NewFakeCLIAdapter()
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:         st,
		Coordination:  coordSvc,
		TeamConfig:    cfg,
		Skills:        skillRegistry,
		Runtime:       cpruntime.ExternalRuntime{ID: "external_test", WorkspaceRoot: t.TempDir(), HomeRoot: t.TempDir(), Ready: true},
		Adapter:       fake,
		BackendURL:    "http://coordplane.test",
		WorkspaceName: "test-workspace",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	deliverySvc, err := delivery.NewServiceWithCommunication(st, runner, cfg.Communication)
	if err != nil {
		t.Fatalf("new delivery service: %v", err)
	}
	return deliveryHarness{
		db:           db,
		store:        st,
		coordination: coordSvc,
		dispatcher:   dispatcher,
		runner:       runner,
		fake:         fake,
		delivery:     deliverySvc,
	}
}

func deliveryTeamConfig() teamconfig.Config {
	return teamconfig.Config{
		TeamID:  "delivery-test",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			deliveryAgent("coordinator"),
			deliveryAgent("builder"),
		},
	}
}

func deliveryAgent(id string) teamconfig.AgentConfig {
	return teamconfig.AgentConfig{
		ID:             id,
		RolePrompt:     id + " role",
		RuntimeProfile: "external-debug",
		CLIBackend:     "fake",
		Skills:         []string{"coordplane-service"},
		Capabilities:   step6Capabilities(),
	}
}

func step6Capabilities() []string {
	return []string{
		"contract.add",
		"contract.current",
		"contract.context",
		"contract.wait",
		"contract.complete",
		"assignment.next",
		"assignment.watch",
		"message.send",
		"communication.read",
		"mailbox.list",
		"mailbox.get",
		"mailbox.resolve",
		"report.submit",
	}
}

func addContract(t *testing.T, ctx context.Context, svc *coordination.Service, targetAgent string) coordination.AddContractResult {
	t.Helper()
	add, err := svc.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "task for " + targetAgent,
		Objective:     "complete work",
		TargetAgentID: targetAgent,
	})
	if err != nil {
		t.Fatalf("contract.add: %v", err)
	}
	return add
}

type claimedWork struct {
	Contract coordination.Contract
	Lease    coordination.Lease
}

func addAndClaim(t *testing.T, ctx context.Context, svc *coordination.Service, targetAgent string) claimedWork {
	t.Helper()
	addContract(t, ctx, svc, targetAgent)
	next, err := svc.AssignmentNext(ctx, coordination.AssignmentNextInput{AgentID: targetAgent, LeaseFor: time.Hour})
	if err != nil {
		t.Fatalf("assignment.next: %v", err)
	}
	if next.Idle {
		t.Fatal("assignment.next returned idle")
	}
	return claimedWork{Contract: next.Contract, Lease: next.Lease}
}

func callAccepted[T any](t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) T {
	t.Helper()
	response := coordlink.New(dispatcher).Call(context.Background(), call)
	if response.Status != capability.StatusAccepted || !response.OK || response.Data == nil {
		t.Fatalf("%s response = %+v, want accepted", call.CapabilityName, response)
	}
	var out T
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		t.Fatalf("decode %s data: %v\nraw: %s", call.CapabilityName, err, string(*response.Data))
	}
	return out
}

func agentSubject(agentID string) capability.Subject {
	return capability.Subject{Kind: "agent", ID: agentID, AgentID: agentID}
}

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return raw
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func mailboxState(t *testing.T, ctx context.Context, db *sql.DB, mailboxID string) (string, string) {
	t.Helper()
	var state, followup string
	if err := db.QueryRowContext(ctx, `
SELECT state, COALESCE(followup_ref, '')
FROM mailbox_items
WHERE id = ?`, mailboxID).Scan(&state, &followup); err != nil {
		t.Fatalf("query mailbox state: %v", err)
	}
	return state, followup
}

func assertResumeQueue(t *testing.T, ctx context.Context, db *sql.DB, queueID, mailboxID string) {
	t.Helper()
	var queueName, kind, payloadRef, state, idempotencyKey string
	if err := db.QueryRowContext(ctx, `
SELECT queue_name, kind, payload_ref, state, idempotency_key
FROM queue_items
WHERE id = ?`, queueID).Scan(&queueName, &kind, &payloadRef, &state, &idempotencyKey); err != nil {
		t.Fatalf("query fallback queue: %v", err)
	}
	if queueName != "runtime.resume" || kind != "mailbox.resume" ||
		payloadRef != "mailbox:"+mailboxID || state != "queued" ||
		idempotencyKey != "fallback:"+mailboxID {
		t.Fatalf("queue row = %s/%s/%s/%s/%s", queueName, kind, payloadRef, state, idempotencyKey)
	}
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func countResumeQueueForMailbox(t *testing.T, ctx context.Context, db *sql.DB, mailboxID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM queue_items
WHERE queue_name = 'runtime.resume'
  AND kind = 'mailbox.resume'
  AND idempotency_key = ?`, "fallback:"+mailboxID).Scan(&count); err != nil {
		t.Fatalf("count resume queue for mailbox %s: %v", mailboxID, err)
	}
	return count
}

func assertResumeQueuedEvent(t *testing.T, ctx context.Context, db *sql.DB, mailboxID string) map[string]any {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = 'delivery.resume_queued'
  AND aggregate_type = 'mailbox_item'
  AND aggregate_id = ?
ORDER BY occurred_at DESC, id DESC
LIMIT 1`, mailboxID).Scan(&raw); err != nil {
		t.Fatalf("query delivery.resume_queued event: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode delivery.resume_queued payload: %v", err)
	}
	return payload
}
