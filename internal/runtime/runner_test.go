package runtime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/capability"
	"coordplane/internal/coordination"
	"coordplane/internal/delivery"
	"coordplane/internal/objects"
	"coordplane/internal/policy"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestRunnerStartsExternalFakeCLIPinsSessionAndCapturesBootstrap(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	add := addContract(t, ctx, h.coordination, "builder")

	session, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start next: %v", err)
	}
	if session.AttemptID == "" || session.Route.ID == "" || session.LeaseID == "" {
		t.Fatalf("session identifiers = %+v, want populated", session)
	}

	starts := h.fake.Starts()
	if len(starts) != 1 {
		t.Fatalf("fake starts = %d, want 1", len(starts))
	}
	start := starts[0]
	if start.AgentID != "builder" || start.AttemptID != session.AttemptID || start.LeaseID != session.LeaseID {
		t.Fatalf("captured start = %+v, want builder session ids", start)
	}
	for _, required := range []string{"Agent: builder", "Current assignment:", add.ContractID, "Available skills:", "coordplane-service"} {
		if !strings.Contains(start.BootstrapPrompt, required) {
			t.Fatalf("bootstrap prompt missing %q:\n%s", required, start.BootstrapPrompt)
		}
	}
	if start.Env["COORDPLANE_AGENT_ID"] != "builder" ||
		start.Env["COORDPLANE_RUNTIME_ID"] == "" ||
		start.Env["COORDPLANE_RUNTIME_ID"] != start.RuntimeID ||
		start.Env["COORDPLANE_LEASE_ID"] != session.LeaseID ||
		start.Env["COORDPLANE_ATTEMPT_ID"] != session.AttemptID ||
		start.Env["COORDPLANE_BACKEND_URL"] != "http://coordplane.test" {
		t.Fatalf("captured env = %#v", start.Env)
	}
	if start.Env["COORDPLANE_TOKEN"] == "" || start.Env["COORDPLANE_TRACE_ID"] == "" {
		t.Fatalf("token/trace not injected: %#v", start.Env)
	}
	assertDir(t, start.Workspace)
	assertDir(t, start.HomeDir)

	attempt := attemptRow(t, ctx, h.db, session.AttemptID)
	if attempt.Status != "running" || attempt.SessionNativeID == "" || attempt.TranscriptRef == "" {
		t.Fatalf("attempt = %+v, want running with pinned session and transcript", attempt)
	}
	route := routeRow(t, ctx, h.db, session.Route.ID)
	if route.SessionNativeID != attempt.SessionNativeID || route.Workdir != start.Workspace || route.AttemptID != session.AttemptID {
		t.Fatalf("route = %+v, want pinned start route", route)
	}
	runtimeID, routeID := leaseRuntimeAndRoute(t, ctx, h.db, session.LeaseID)
	if runtimeID != start.RuntimeID || routeID != session.Route.ID {
		t.Fatalf("lease runtime/route = %s/%s, want %s/%s", runtimeID, routeID, start.RuntimeID, session.Route.ID)
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_instances", "runtime_id = '"+start.RuntimeID+"' AND state = 'ready'"); got != 1 {
		t.Fatalf("ready runtime_instances = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_tokens", "runtime_id = '"+start.RuntimeID+"' AND state = 'active'"); got != 1 {
		t.Fatalf("active runtime_tokens = %d, want 1", got)
	}
	if got := assignmentRoute(t, ctx, h.db, add.AssignmentID); got != session.Route.ID {
		t.Fatalf("assignment route = %s, want %s", got, session.Route.ID)
	}
}

func TestPrepareLeaseIsIdempotentAndRefreshesTTL(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	claimed := addAndClaim(t, ctx, h.coordination, "builder")
	now := time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)

	first, err := h.runner.AcquirePrepareLease(ctx, cpruntime.PrepareLeaseInput{
		LeaseID: claimed.Lease.ID,
		AgentID: "builder",
		Owner:   "runner-a",
		TTL:     time.Minute,
		Now:     now,
	})
	if err != nil {
		t.Fatalf("first prepare lease: %v", err)
	}
	second, err := h.runner.AcquirePrepareLease(ctx, cpruntime.PrepareLeaseInput{
		LeaseID: claimed.Lease.ID,
		AgentID: "builder",
		Owner:   "runner-a",
		TTL:     2 * time.Minute,
		Now:     now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("refresh prepare lease: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("prepare lease id = %s, want original %s", second.ID, first.ID)
	}
	if !second.ExpiresAt.Equal(now.Add(150 * time.Second)) {
		t.Fatalf("prepare lease expires_at = %s, want refreshed TTL", second.ExpiresAt)
	}
	if got := countRowsWhere(t, ctx, h.db, "prepare_leases", "lease_id = '"+claimed.Lease.ID+"' AND state = 'active'"); got != 1 {
		t.Fatalf("active prepare leases = %d, want 1", got)
	}
	_, err = h.runner.AcquirePrepareLease(ctx, cpruntime.PrepareLeaseInput{
		LeaseID: claimed.Lease.ID,
		AgentID: "builder",
		Owner:   "runner-b",
		TTL:     time.Minute,
		Now:     now.Add(time.Minute),
	})
	if !errors.Is(err, cpruntime.ErrActiveResource) {
		t.Fatalf("other owner prepare lease error = %v, want ErrActiveResource", err)
	}
	if err := h.runner.ReleasePrepareLease(ctx, first.ID); err != nil {
		t.Fatalf("release prepare lease: %v", err)
	}
	if got := countRowsWhere(t, ctx, h.db, "prepare_leases", "lease_id = '"+claimed.Lease.ID+"' AND state = 'active'"); got != 0 {
		t.Fatalf("active prepare leases after release = %d, want 0", got)
	}
}

func TestRunnerActiveSessionGuardPreventsDuplicateStart(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "builder")

	first, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("duplicate start: %v", err)
	}
	if second.AttemptID != first.AttemptID || second.Route.ID != first.Route.ID || second.LeaseID != first.LeaseID {
		t.Fatalf("duplicate start session = %+v, want original %+v", second, first)
	}
	if starts := h.fake.Starts(); len(starts) != 1 {
		t.Fatalf("fake starts = %d, want duplicate guarded to one start", len(starts))
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "lease_id = '"+first.LeaseID+"'"); got != 1 {
		t.Fatalf("attempts for lease = %d, want 1", got)
	}
}

func TestRunnerFinishWaitingKeepsRouteTokenAndGuardsResumable(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	callAccepted[coordination.Assignment](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.wait",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"reason":           "waiting for resumable work",
			"waiting_for_ref":  "contract:pending-child",
			"session_route_id": coordinator.Route.ID,
		}),
	})
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     coordinator.AttemptID,
		Status:        "waiting",
		Summary:       "waiting for resume",
		TranscriptRef: "waiting-transcript",
	}); err != nil {
		t.Fatalf("finish coordinator waiting: %v", err)
	}

	if state := routeState(t, ctx, h.db, coordinator.Route.ID); state != "waiting" {
		t.Fatalf("coordinator route state = %s, want waiting", state)
	}
	if status := attemptRow(t, ctx, h.db, coordinator.AttemptID).Status; status != "waiting" {
		t.Fatalf("coordinator attempt status = %s, want waiting", status)
	}
	if state := leaseState(t, ctx, h.db, coordinator.LeaseID); state != "active" {
		t.Fatalf("coordinator lease state = %s, want active for resume", state)
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_tokens", "attempt_id = '"+coordinator.AttemptID+"' AND state = 'active'"); got != 1 {
		t.Fatalf("active runtime tokens for waiting attempt = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "active_guards", "attempt_id = '"+coordinator.AttemptID+"' AND state = 'active'"); got != 2 {
		t.Fatalf("active guards for waiting attempt = %d, want 2", got)
	}
}

func TestRunnerDoesNotMarkRunningWhenExternalRuntimeIsNotReady(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, false)
	add := addContract(t, ctx, h.coordination, "builder")

	if _, err := h.runner.StartNext(ctx, "builder"); err == nil {
		t.Fatal("start next succeeded with not-ready external runtime")
	}
	if len(h.fake.Starts()) != 0 {
		t.Fatalf("fake adapter was started despite not-ready runtime")
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'running'"); got != 0 {
		t.Fatalf("running attempts = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'failed'"); got != 1 {
		t.Fatalf("failed attempts = %d, want 1", got)
	}
	if got := countActiveLeases(t, ctx, h.db, add.AssignmentID); got != 0 {
		t.Fatalf("active leases after prepare failure = %d, want 0", got)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment state after prepare failure = %s, want queued", got)
	}
}

func TestRunnerCancellationUsesIndependentCleanupContext(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	add := addContract(t, ctx, h.coordination, "builder")
	adapter := &blockingCLIAdapter{}
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:         h.store,
		Coordination:  h.coordination,
		TeamConfig:    runtimeTeamConfig(),
		Runtime:       cpruntime.ExternalRuntime{ID: "external_cancel", WorkspaceRoot: t.TempDir(), HomeRoot: t.TempDir(), Ready: true},
		Adapter:       adapter,
		BackendURL:    "http://coordplane.test",
		WorkspaceName: "cancel-test",
	})
	if err != nil {
		t.Fatalf("new cancellation runner: %v", err)
	}

	startCtx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if _, err := runner.StartNext(startCtx, "builder"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start cancellation error = %v, want context deadline exceeded", err)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment after cancelled start = %s, want queued", got)
	}
	if got := countActiveLeases(t, ctx, h.db, add.AssignmentID); got != 0 {
		t.Fatalf("active leases after cancelled start = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'failed'"); got != 1 {
		t.Fatalf("failed attempts after cancelled start = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_tokens", "state = 'active'"); got != 0 {
		t.Fatalf("active tokens after cancelled start = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "active_guards", "state = 'active'"); got != 0 {
		t.Fatalf("active guards after cancelled start = %d, want 0", got)
	}
}

func TestRunnerProcessesFallbackResumeQueueWithPinnedRouteAndDuplicateRecoveryIsIdle(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builderAdd := callAccepted[coordination.AddContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.add",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"title":           "build resumable child",
			"objective":       "produce report",
			"target_agent_id": "builder",
		}),
	})
	callAccepted[coordination.Assignment](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.wait",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"reason":           "waiting for builder",
			"waiting_for_ref":  "contract:" + builderAdd.ContractID,
			"session_route_id": coordinator.Route.ID,
		}),
	})
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     coordinator.AttemptID,
		Status:        "waiting",
		Summary:       "waiting for child",
		TranscriptRef: "waiting-transcript",
	}); err != nil {
		t.Fatalf("finish coordinator waiting: %v", err)
	}
	if state := routeState(t, ctx, h.db, coordinator.Route.ID); state != "waiting" {
		t.Fatalf("coordinator route state = %s, want waiting", state)
	}

	builder, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	builderReport := callAccepted[coordination.Evidence](t, h.dispatcher, capability.Call{
		CapabilityName: "report.submit",
		Subject:        agentSubject("builder"),
		Scope:          mustRaw(t, map[string]any{"lease_id": builder.LeaseID}),
		Input:          mustRaw(t, map[string]any{"summary": "builder done", "content": "done"}),
	})
	callAccepted[coordination.CompleteContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.complete",
		Subject:        agentSubject("builder"),
		Scope:          mustRaw(t, map[string]any{"lease_id": builder.LeaseID}),
		Input:          mustRaw(t, map[string]any{"evidence_ids": []string{builderReport.ID}, "summary": "done"}),
	})
	mailbox := firstMailbox(t, h.dispatcher, "coordinator")
	if mailbox.SessionRouteID != coordinator.Route.ID {
		t.Fatalf("mailbox session route = %s, want coordinator route %s", mailbox.SessionRouteID, coordinator.Route.ID)
	}
	deliverySvc, err := delivery.NewService(h.store, h.runner)
	if err != nil {
		t.Fatalf("new delivery service: %v", err)
	}
	delivered, err := deliverySvc.NotifyMailbox(ctx, mailbox.ID)
	if err != nil {
		t.Fatalf("notify mailbox: %v", err)
	}
	if delivered.State != "fallback" || delivered.QueueItemID == "" {
		t.Fatalf("delivery result = %+v, want fallback queue", delivered)
	}

	resumed, err := h.runner.ProcessResumeQueue(ctx, "resume-worker")
	if err != nil {
		t.Fatalf("process resume queue: %v", err)
	}
	if resumed.State != "resumed" || resumed.RouteID != coordinator.Route.ID || resumed.MailboxID != mailbox.ID {
		t.Fatalf("resume result = %+v, want coordinator mailbox resume", resumed)
	}
	if resumed.Env["COORDPLANE_TOKEN"] == "" || resumed.Env["COORDPLANE_TOKEN"] == coordinator.Env["COORDPLANE_TOKEN"] {
		t.Fatalf("resume env token = %q, want fresh token different from initial session", resumed.Env["COORDPLANE_TOKEN"])
	}
	resumes := h.fake.Resumes()
	if len(resumes) != 1 {
		t.Fatalf("fake resumes = %d, want 1", len(resumes))
	}
	if resumes[0].Route.ID != coordinator.Route.ID ||
		len(resumes[0].MailboxIDs) != 1 || resumes[0].MailboxIDs[0] != mailbox.ID ||
		resumes[0].Reason != "mailbox.resume" {
		t.Fatalf("resume request = %+v", resumes[0])
	}
	rawResume, err := json.Marshal(resumes[0])
	if err != nil {
		t.Fatalf("marshal resume request: %v", err)
	}
	if strings.Contains(string(rawResume), "builder done") || strings.Contains(string(rawResume), "produce report") {
		t.Fatalf("resume request leaked mailbox content: %s", rawResume)
	}
	if state := queueItemState(t, ctx, h.db, delivered.QueueItemID); state != "done" {
		t.Fatalf("resume queue state = %s, want done", state)
	}
	if state := routeState(t, ctx, h.db, coordinator.Route.ID); state != "active" {
		t.Fatalf("coordinator route after resume = %s, want active", state)
	}
	if attempt := attemptRow(t, ctx, h.db, coordinator.AttemptID); attempt.Status != "running" {
		t.Fatalf("coordinator attempt after resume = %+v, want running", attempt)
	}
	state, followup := mailboxState(t, ctx, h.db, mailbox.ID)
	if state != "pending" || !strings.Contains(followup, "child_contract:") {
		t.Fatalf("mailbox after resume = %s/%s, want pending with durable child followup", state, followup)
	}

	idle, err := h.runner.ProcessResumeQueue(ctx, "resume-worker")
	if err != nil {
		t.Fatalf("duplicate process resume queue: %v", err)
	}
	if !idle.Idle {
		t.Fatalf("duplicate resume queue result = %+v, want idle", idle)
	}
	if resumes := h.fake.Resumes(); len(resumes) != 1 {
		t.Fatalf("duplicate recovery emitted resumes: %+v", resumes)
	}
	duplicateRouteResume, err := h.runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{
		RouteID:    coordinator.Route.ID,
		Reason:     "duplicate",
		MailboxIDs: []string{mailbox.ID},
	})
	if err != nil {
		t.Fatalf("duplicate route resume: %v", err)
	}
	if duplicateRouteResume.State != "already_resumed" {
		t.Fatalf("duplicate route resume = %+v, want already_resumed", duplicateRouteResume)
	}
	if resumes := h.fake.Resumes(); len(resumes) != 1 {
		t.Fatalf("duplicate route resume called adapter again: %+v", resumes)
	}
}

func TestRunnerProcessesDistinctMailboxResumeForSameRoute(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	callAccepted[coordination.Assignment](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.wait",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"reason":           "waiting for builder messages",
			"waiting_for_ref":  "mailbox:builder-feedback",
			"session_route_id": coordinator.Route.ID,
		}),
	})
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     coordinator.AttemptID,
		Status:        "waiting",
		Summary:       "waiting for feedback",
		TranscriptRef: "waiting-transcript",
	}); err != nil {
		t.Fatalf("finish coordinator waiting: %v", err)
	}

	builder := addAndClaim(t, ctx, h.coordination, "builder")
	first := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "feedback",
		Body:             "first feedback",
	})
	if first.Status != capability.StatusAccepted || first.Data == nil || first.Data.MailboxID == "" {
		t.Fatalf("first message.send = %+v, want accepted mailbox", first)
	}
	second := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "feedback",
		Body:             "second feedback",
	})
	if second.Status != capability.StatusAccepted || second.Data == nil || second.Data.MailboxID == "" {
		t.Fatalf("second message.send = %+v, want accepted mailbox", second)
	}

	deliverySvc, err := delivery.NewService(h.store, h.runner)
	if err != nil {
		t.Fatalf("new delivery service: %v", err)
	}
	firstDelivery, err := deliverySvc.NotifyMailbox(ctx, first.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify first mailbox: %v", err)
	}
	secondDelivery, err := deliverySvc.NotifyMailbox(ctx, second.Data.MailboxID)
	if err != nil {
		t.Fatalf("notify second mailbox: %v", err)
	}
	if firstDelivery.State != "fallback" || secondDelivery.State != "fallback" {
		t.Fatalf("delivery states = %s/%s, want fallback for both distinct mailboxes", firstDelivery.State, secondDelivery.State)
	}
	if firstDelivery.QueueItemID == "" || secondDelivery.QueueItemID == "" || firstDelivery.QueueItemID == secondDelivery.QueueItemID {
		t.Fatalf("queue ids = %q/%q, want distinct queue items", firstDelivery.QueueItemID, secondDelivery.QueueItemID)
	}

	firstResume, err := h.runner.ProcessResumeQueue(ctx, "resume-worker")
	if err != nil {
		t.Fatalf("process first resume queue: %v", err)
	}
	if firstResume.State != "resumed" || firstResume.RouteID != coordinator.Route.ID || firstResume.MailboxID != first.Data.MailboxID {
		t.Fatalf("first resume result = %+v, want first mailbox resumed on coordinator route", firstResume)
	}
	secondResume, err := h.runner.ProcessResumeQueue(ctx, "resume-worker")
	if err != nil {
		t.Fatalf("process second resume queue: %v", err)
	}
	if secondResume.State != "resumed" || secondResume.RouteID != coordinator.Route.ID || secondResume.MailboxID != second.Data.MailboxID {
		t.Fatalf("second resume result = %+v, want distinct mailbox to resume rather than route-level already_resumed", secondResume)
	}

	resumes := h.fake.Resumes()
	if len(resumes) != 2 {
		t.Fatalf("fake resumes = %d, want one adapter resume per distinct pending mailbox: %+v", len(resumes), resumes)
	}
	if got := resumes[0].MailboxIDs; len(got) != 1 || got[0] != first.Data.MailboxID {
		t.Fatalf("first adapter resume mailboxes = %+v, want %s", got, first.Data.MailboxID)
	}
	if got := resumes[1].MailboxIDs; len(got) != 1 || got[0] != second.Data.MailboxID {
		t.Fatalf("second adapter resume mailboxes = %+v, want %s", got, second.Data.MailboxID)
	}
	if firstState, firstFollowup := mailboxState(t, ctx, h.db, first.Data.MailboxID); firstState != "pending" || firstFollowup != "" {
		t.Fatalf("first mailbox after resume = %s/%s, want pending without implicit resolution", firstState, firstFollowup)
	}
	if secondState, secondFollowup := mailboxState(t, ctx, h.db, second.Data.MailboxID); secondState != "pending" || secondFollowup != "" {
		t.Fatalf("second mailbox after resume = %s/%s, want pending without implicit resolution", secondState, secondFollowup)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type = 'session.resumed' AND aggregate_id = '"+coordinator.Route.ID+"'"); got != 2 {
		t.Fatalf("session.resumed events for route = %d, want distinct events for both mailboxes", got)
	}
}

func TestResumeRouteRejectsTerminalAndStaleRoutesWithoutAdapterCall(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "builder")
	terminal, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start terminal session: %v", err)
	}
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     terminal.AttemptID,
		Status:        "completed",
		Summary:       "done",
		TranscriptRef: "terminal",
	}); err != nil {
		t.Fatalf("finish completed: %v", err)
	}
	if _, err := h.runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{RouteID: terminal.Route.ID, Reason: "terminal"}); err == nil {
		t.Fatal("resume terminal route succeeded")
	}
	if resumes := h.fake.Resumes(); len(resumes) != 0 {
		t.Fatalf("terminal resume called adapter: %+v", resumes)
	}

	h = newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "builder")
	stale, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start stale session: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE session_routes SET state = 'stale' WHERE id = ?`, stale.Route.ID); err != nil {
		t.Fatalf("mark route stale: %v", err)
	}
	if _, err := h.runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{RouteID: stale.Route.ID, Reason: "stale"}); err == nil {
		t.Fatal("resume stale route succeeded")
	}
	if resumes := h.fake.Resumes(); len(resumes) != 0 {
		t.Fatalf("stale resume called adapter: %+v", resumes)
	}
}

func TestCleanupGuardRejectsActiveLeaseAttemptSessionAndWorkspace(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "builder")
	session, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	start := h.fake.Starts()[0]
	for _, target := range []cpruntime.CleanupTarget{
		{ResourceKind: "lease", ResourceRef: session.LeaseID},
		{ResourceKind: "attempt", ResourceRef: session.AttemptID},
		{ResourceKind: "session_route", ResourceRef: session.Route.ID},
		{ResourceKind: "workspace", ResourceRef: start.Workspace},
		{ResourceKind: "home", ResourceRef: start.HomeDir},
	} {
		if err := h.runner.GuardCleanup(ctx, target); !errors.Is(err, cpruntime.ErrActiveResource) {
			t.Fatalf("cleanup guard for %+v = %v, want ErrActiveResource", target, err)
		}
	}

	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "completed",
		Summary:       "done",
		TranscriptRef: "terminal",
	}); err != nil {
		t.Fatalf("finish builder: %v", err)
	}
	for _, target := range []cpruntime.CleanupTarget{
		{ResourceKind: "lease", ResourceRef: session.LeaseID},
		{ResourceKind: "attempt", ResourceRef: session.AttemptID},
		{ResourceKind: "session_route", ResourceRef: session.Route.ID},
		{ResourceKind: "workspace", ResourceRef: start.Workspace},
		{ResourceKind: "home", ResourceRef: start.HomeDir},
	} {
		if err := h.runner.GuardCleanup(ctx, target); err != nil {
			t.Fatalf("cleanup guard after terminal for %+v = %v, want allowed", target, err)
		}
	}
}

func TestRunnerSteerMailboxCapturesLightweightPayloadAndLeavesMailboxPending(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "coordinator")
	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builder := addAndClaim(t, ctx, h.coordination, "builder")
	sent := h.coordination.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          builder.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "question",
		Body:             "full mailbox body must stay behind capability access",
	})
	if sent.Status != capability.StatusAccepted || sent.Data == nil || sent.Data.MailboxID == "" {
		t.Fatalf("message.send = %+v, want mailbox", sent)
	}

	if err := h.runner.SteerMailbox(ctx, coordinator.Route.ID, sent.Data.MailboxID); err != nil {
		t.Fatalf("steer mailbox: %v", err)
	}
	steers := h.fake.Steers()
	if len(steers) != 1 {
		t.Fatalf("fake steers = %d, want 1", len(steers))
	}
	payload := steers[0].Payload
	if payload.MailboxID != sent.Data.MailboxID || payload.Reason != "question" || payload.AgentID != "coordinator" {
		t.Fatalf("steer payload = %+v, want lightweight mailbox signal", payload)
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal steer payload: %v", err)
	}
	if strings.Contains(string(rawPayload), "full mailbox body") {
		t.Fatalf("steer payload leaked mailbox body: %s", rawPayload)
	}
	state, followup := mailboxState(t, ctx, h.db, sent.Data.MailboxID)
	if state != "pending" || followup != "" {
		t.Fatalf("mailbox after steer = %s/%s, want pending/empty followup", state, followup)
	}
}

func TestSessionFinishCapturesTerminalReportAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	workAdd := addContract(t, ctx, h.coordination, "builder")
	session, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}

	attempt, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "completed",
		Summary:       "session ended",
		TranscriptRef: "terminal-transcript",
	})
	if err != nil {
		t.Fatalf("finish session: %v", err)
	}
	if attempt.Status != "completed" || attempt.EndedAt == nil || attempt.TranscriptRef != "terminal-transcript" {
		t.Fatalf("finished attempt = %+v", attempt)
	}
	if got := contractStatus(t, ctx, h.db, workAdd.ContractID); got != "open" {
		t.Fatalf("session.finish changed contract status = %s, want open", got)
	}
	if got := leaseState(t, ctx, h.db, session.LeaseID); got != "released" {
		t.Fatalf("lease after finish = %s, want released", got)
	}
	if got := assignmentState(t, ctx, h.db, workAdd.AssignmentID); got != "returned" {
		t.Fatalf("assignment after finish = %s, want returned", got)
	}
	if reports := h.fake.TerminalReports(); len(reports) != 1 || reports[0].AttemptID != session.AttemptID {
		t.Fatalf("terminal reports = %+v, want one report for attempt", reports)
	}

	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "completed",
		Summary:       "duplicate",
		TranscriptRef: "duplicate-transcript",
	}); err != nil {
		t.Fatalf("duplicate finish: %v", err)
	}
	if reports := h.fake.TerminalReports(); len(reports) != 1 {
		t.Fatalf("duplicate finish emitted terminal report: %+v", reports)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type = 'session.finished'"); got != 1 {
		t.Fatalf("session.finished events = %d, want 1", got)
	}
}

func TestSessionFinishStoresTranscriptObjectAndDoesNotInlineObjectBodiesIntoPrompt(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	addContract(t, ctx, h.coordination, "builder")
	session, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	transcriptBody := "sensitive transcript body must stay behind object.read"

	attempt, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:         session.AttemptID,
		Status:            "completed",
		Summary:           "session ended",
		TranscriptContent: transcriptBody,
	})
	if err != nil {
		t.Fatalf("finish session: %v", err)
	}
	if !strings.HasPrefix(attempt.TranscriptRef, "obj_sha256_") {
		t.Fatalf("transcript ref = %q, want object ref", attempt.TranscriptRef)
	}
	var transcriptRows int
	if err := h.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM transcripts
WHERE attempt_id = ? AND object_ref = ?`,
		session.AttemptID, attempt.TranscriptRef,
	).Scan(&transcriptRows); err != nil {
		t.Fatalf("query transcript rows: %v", err)
	}
	if transcriptRows != 1 {
		t.Fatalf("transcript rows = %d, want 1", transcriptRows)
	}
	reports := h.fake.TerminalReports()
	if len(reports) != 1 || reports[0].TranscriptRef != attempt.TranscriptRef || reports[0].TranscriptContent != "" {
		t.Fatalf("terminal report sent to adapter = %+v, want sanitized object ref", reports)
	}
	readTranscript := h.coordination.ObjectStore().Read(ctx, agentSubject("builder"), attempt.TranscriptRef)
	if readTranscript.Status != capability.StatusAccepted || readTranscript.Data == nil || readTranscript.Data.Content != transcriptBody {
		t.Fatalf("transcript object.read = %+v, want transcript content", readTranscript)
	}

	artifactBody := "artifact body must not appear in bootstrap prompt"
	artifact, err := h.coordination.ObjectStore().PutArtifact(ctx, objects.PutArtifactInput{
		OwnerAgent:  "builder",
		Content:     []byte(artifactBody),
		ContentType: "text/plain",
		Metadata:    map[string]string{"name": "artifact.txt"},
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	inspectArtifact := h.coordination.ObjectStore().Inspect(ctx, agentSubject("builder"), artifact.ObjectRef)
	if inspectArtifact.Status != capability.StatusAccepted || inspectArtifact.Data == nil {
		t.Fatalf("artifact inspect = %+v, want metadata", inspectArtifact)
	}
	rawInspect, err := json.Marshal(inspectArtifact.Data)
	if err != nil {
		t.Fatalf("marshal artifact inspect: %v", err)
	}
	if strings.Contains(string(rawInspect), artifactBody) {
		t.Fatalf("artifact inspect leaked content: %s", rawInspect)
	}

	addContract(t, ctx, h.coordination, "validator")
	if _, err := h.runner.StartNext(ctx, "validator"); err != nil {
		t.Fatalf("start validator: %v", err)
	}
	starts := h.fake.Starts()
	if len(starts) != 2 {
		t.Fatalf("fake starts = %d, want builder and validator", len(starts))
	}
	prompt := starts[1].BootstrapPrompt
	for _, forbidden := range []string{transcriptBody, artifactBody} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("bootstrap prompt leaked object body %q:\n%s", forbidden, prompt)
		}
	}
}

func TestSimplifiedTA13FakeCLIMultiAgentLoop(t *testing.T) {
	ctx := context.Background()
	h := newRuntimeHarness(t, true)
	root := addContract(t, ctx, h.coordination, "coordinator")

	coordinator, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	builderAdd := callAccepted[coordination.AddContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.add",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"title":           "build minimal feature",
			"objective":       "produce builder report",
			"target_agent_id": "builder",
		}),
	})
	callAccepted[coordination.Assignment](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.wait",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"reason":           "waiting for builder",
			"waiting_for_ref":  "contract:" + builderAdd.ContractID,
			"session_route_id": coordinator.Route.ID,
		}),
	})

	builder, err := h.runner.StartNext(ctx, "builder")
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	builderReport := callAccepted[coordination.Evidence](t, h.dispatcher, capability.Call{
		CapabilityName: "report.submit",
		Subject:        agentSubject("builder"),
		Scope:          mustRaw(t, map[string]any{"lease_id": builder.LeaseID}),
		Input:          mustRaw(t, map[string]any{"summary": "builder done", "content": "implemented"}),
	})
	callAccepted[coordination.CompleteContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.complete",
		Subject:        agentSubject("builder"),
		Scope:          mustRaw(t, map[string]any{"lease_id": builder.LeaseID}),
		Input:          mustRaw(t, map[string]any{"evidence_ids": []string{builderReport.ID}, "summary": "done"}),
	})
	builderMailbox := firstMailbox(t, h.dispatcher, "coordinator")
	if err := h.runner.SteerMailbox(ctx, coordinator.Route.ID, builderMailbox.ID); err != nil {
		t.Fatalf("steer builder mailbox: %v", err)
	}
	callAccepted[coordination.MailboxItem](t, h.dispatcher, capability.Call{
		CapabilityName: "mailbox.resolve",
		Subject:        agentSubject("coordinator"),
		Input:          mustRaw(t, map[string]any{"mailbox_id": builderMailbox.ID, "followup_ref": "contract:validator"}),
	})

	validatorAdd := callAccepted[coordination.AddContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.add",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input: mustRaw(t, map[string]any{
			"title":           "validate builder result",
			"objective":       "produce terminal validation report",
			"target_agent_id": "validator",
		}),
	})
	validator, err := h.runner.StartNext(ctx, "validator")
	if err != nil {
		t.Fatalf("start validator: %v", err)
	}
	validatorReport := callAccepted[coordination.Evidence](t, h.dispatcher, capability.Call{
		CapabilityName: "report.submit",
		Subject:        agentSubject("validator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": validator.LeaseID}),
		Input:          mustRaw(t, map[string]any{"summary": "validation passed", "content": "checked builder report"}),
	})
	callAccepted[coordination.CompleteContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.complete",
		Subject:        agentSubject("validator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": validator.LeaseID}),
		Input:          mustRaw(t, map[string]any{"evidence_ids": []string{validatorReport.ID}, "summary": "validated"}),
	})
	validatorMailbox := firstMailbox(t, h.dispatcher, "coordinator")
	if validatorMailbox.ID == builderMailbox.ID {
		t.Fatalf("validator mailbox reused builder mailbox: %+v", validatorMailbox)
	}
	if err := h.runner.SteerMailbox(ctx, coordinator.Route.ID, validatorMailbox.ID); err != nil {
		t.Fatalf("steer validator mailbox: %v", err)
	}
	callAccepted[coordination.MailboxItem](t, h.dispatcher, capability.Call{
		CapabilityName: "mailbox.resolve",
		Subject:        agentSubject("coordinator"),
		Input:          mustRaw(t, map[string]any{"mailbox_id": validatorMailbox.ID, "followup_ref": "report:root"}),
	})

	rootReport := callAccepted[coordination.Evidence](t, h.dispatcher, capability.Call{
		CapabilityName: "report.submit",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input:          mustRaw(t, map[string]any{"summary": "root complete", "content": "builder and validator reports accepted"}),
	})
	rootComplete := callAccepted[coordination.CompleteContractResult](t, h.dispatcher, capability.Call{
		CapabilityName: "contract.complete",
		Subject:        agentSubject("coordinator"),
		Scope:          mustRaw(t, map[string]any{"lease_id": coordinator.LeaseID}),
		Input:          mustRaw(t, map[string]any{"evidence_ids": []string{rootReport.ID}, "summary": "root done"}),
	})
	if rootComplete.ContractID != root.ContractID || rootComplete.Status != "satisfied" {
		t.Fatalf("root complete = %+v, want satisfied root %s", rootComplete, root.ContractID)
	}

	for _, session := range []cpruntime.AssignmentSession{builder, validator, coordinator} {
		if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
			AttemptID:     session.AttemptID,
			Status:        "completed",
			Summary:       "terminal report",
			TranscriptRef: "terminal-" + session.AttemptID,
		}); err != nil {
			t.Fatalf("finish %s: %v", session.AttemptID, err)
		}
	}

	if got := contractStatus(t, ctx, h.db, root.ContractID); got != "satisfied" {
		t.Fatalf("root status = %s, want satisfied", got)
	}
	if got := contractStatus(t, ctx, h.db, builderAdd.ContractID); got != "satisfied" {
		t.Fatalf("builder child status = %s, want satisfied", got)
	}
	if got := contractStatus(t, ctx, h.db, validatorAdd.ContractID); got != "satisfied" {
		t.Fatalf("validator child status = %s, want satisfied", got)
	}
	if starts := h.fake.Starts(); len(starts) != 3 {
		t.Fatalf("fake starts = %d, want 3", len(starts))
	}
	if steers := h.fake.Steers(); len(steers) != 2 {
		t.Fatalf("fake steers = %d, want 2", len(steers))
	}
	if reports := h.fake.TerminalReports(); len(reports) != 0 {
		t.Fatalf("terminal reports = %d, want no duplicate adapter finish after contract closeout", len(reports))
	}
	if got := countRowsWhere(t, ctx, h.db, "delivery_attempts", "1 = 1"); got != 0 {
		t.Fatalf("delivery attempts = %d, want 0 because delivery service is out of scope", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'completed'"); got != 3 {
		t.Fatalf("completed attempts = %d, want 3", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "session_routes", "state = 'active'"); got != 0 {
		t.Fatalf("active routes after closeout = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_tokens", "state = 'active'"); got != 0 {
		t.Fatalf("active runtime tokens after closeout = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "active_guards", "state = 'active'"); got != 0 {
		t.Fatalf("active guards after closeout = %d, want 0", got)
	}
}

type runtimeHarness struct {
	db           *sql.DB
	store        *store.Store
	coordination *coordination.Service
	dispatcher   *policy.Dispatcher
	runner       *cpruntime.Runner
	fake         *cpruntime.FakeCLIAdapter
}

type blockingCLIAdapter struct{}

func (a *blockingCLIAdapter) Start(ctx context.Context, req cpruntime.StartRequest) (cpruntime.StartResult, error) {
	<-ctx.Done()
	return cpruntime.StartResult{}, ctx.Err()
}

func (a *blockingCLIAdapter) Steer(context.Context, cpruntime.SteerRequest) error {
	return nil
}

func (a *blockingCLIAdapter) Finish(context.Context, cpruntime.TerminalReport) error {
	return nil
}

func newRuntimeHarness(t *testing.T, runtimeReady bool) runtimeHarness {
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
	cfg := runtimeTeamConfig()
	coordSvc := coordination.NewService(st)
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, coordSvc); err != nil {
		t.Fatalf("register coordination capabilities: %v", err)
	}
	if err := objects.RegisterCapabilities(registry, coordSvc.ObjectStore()); err != nil {
		t.Fatalf("register object capabilities: %v", err)
	}
	dispatcher := policy.NewDispatcher(cfg, registry)
	fake := cpruntime.NewFakeCLIAdapter()
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:         st,
		Coordination:  coordSvc,
		TeamConfig:    cfg,
		Skills:        skillRegistry,
		Runtime:       cpruntime.ExternalRuntime{ID: "external_test", WorkspaceRoot: t.TempDir(), HomeRoot: t.TempDir(), Ready: runtimeReady},
		Adapter:       fake,
		BackendURL:    "http://coordplane.test",
		WorkspaceName: "test-workspace",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return runtimeHarness{
		db:           db,
		store:        st,
		coordination: coordSvc,
		dispatcher:   dispatcher,
		runner:       runner,
		fake:         fake,
	}
}

func runtimeTeamConfig() teamconfig.Config {
	return teamconfig.Config{
		TeamID:  "runtime-test",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			runtimeAgent("coordinator", []string{"coordplane-service", "contract-delegation"}),
			runtimeAgent("builder", []string{"coordplane-service"}),
			runtimeAgent("validator", []string{"coordplane-service"}),
		},
	}
}

func runtimeAgent(id string, skillNames []string) teamconfig.AgentConfig {
	return teamconfig.AgentConfig{
		ID:             id,
		RolePrompt:     id + " role",
		RuntimeProfile: "external-debug",
		CLIBackend:     "fake",
		Skills:         skillNames,
		Capabilities:   step5Capabilities(),
	}
}

func step5Capabilities() []string {
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
		"object.inspect",
		"object.read",
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

func firstMailbox(t *testing.T, dispatcher *policy.Dispatcher, agentID string) coordination.MailboxItem {
	t.Helper()
	items := callAccepted[[]coordination.MailboxItem](t, dispatcher, capability.Call{
		CapabilityName: "mailbox.list",
		Subject:        agentSubject(agentID),
	})
	if len(items) == 0 {
		t.Fatalf("mailbox.list for %s returned no items", agentID)
	}
	return items[0]
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

func assertDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
}

func attemptRow(t *testing.T, ctx context.Context, db *sql.DB, attemptID string) cpruntime.Attempt {
	t.Helper()
	var attempt cpruntime.Attempt
	if err := db.QueryRowContext(ctx, `
SELECT id, lease_id, cli_backend, runtime_kind, COALESCE(session_native_id, ''),
  start_reason, status, COALESCE(transcript_ref, '')
FROM attempts
WHERE id = ?`, attemptID).Scan(
		&attempt.ID,
		&attempt.LeaseID,
		&attempt.CLIBackend,
		&attempt.RuntimeKind,
		&attempt.SessionNativeID,
		&attempt.StartReason,
		&attempt.Status,
		&attempt.TranscriptRef,
	); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	return attempt
}

func routeRow(t *testing.T, ctx context.Context, db *sql.DB, routeID string) cpruntime.SessionRoute {
	t.Helper()
	var route cpruntime.SessionRoute
	var raw, state string
	if err := db.QueryRowContext(ctx, `
SELECT id, agent_id, runtime_id, cli_backend, session_native_id, route_json, state
FROM session_routes
WHERE id = ?`, routeID).Scan(
		&route.ID,
		&route.AgentID,
		&route.RuntimeID,
		&route.CLIBackend,
		&route.SessionNativeID,
		&raw,
		&state,
	); err != nil {
		t.Fatalf("query route: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &route); err != nil {
		t.Fatalf("decode route json: %v", err)
	}
	route.ID = routeID
	route.State = state
	return route
}

func routeState(t *testing.T, ctx context.Context, db *sql.DB, routeID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM session_routes WHERE id = ?`, routeID).Scan(&state); err != nil {
		t.Fatalf("query route state: %v", err)
	}
	return state
}

func queueItemState(t *testing.T, ctx context.Context, db *sql.DB, queueID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM queue_items WHERE id = ?`, queueID).Scan(&state); err != nil {
		t.Fatalf("query queue item state: %v", err)
	}
	return state
}

func leaseRuntimeAndRoute(t *testing.T, ctx context.Context, db *sql.DB, leaseID string) (string, string) {
	t.Helper()
	var runtimeID, routeID string
	if err := db.QueryRowContext(ctx, `
SELECT COALESCE(runtime_id, ''), COALESCE(session_route_id, '')
FROM leases
WHERE id = ?`, leaseID).Scan(&runtimeID, &routeID); err != nil {
		t.Fatalf("query lease runtime/route: %v", err)
	}
	return runtimeID, routeID
}

func assignmentRoute(t *testing.T, ctx context.Context, db *sql.DB, assignmentID string) string {
	t.Helper()
	var routeID string
	if err := db.QueryRowContext(ctx, `
SELECT COALESCE(session_route_id, '')
FROM assignments
WHERE id = ?`, assignmentID).Scan(&routeID); err != nil {
		t.Fatalf("query assignment route: %v", err)
	}
	return routeID
}

func assignmentState(t *testing.T, ctx context.Context, db *sql.DB, assignmentID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM assignments WHERE id = ?`, assignmentID).Scan(&state); err != nil {
		t.Fatalf("query assignment state: %v", err)
	}
	return state
}

func contractStatus(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&status); err != nil {
		t.Fatalf("query contract status: %v", err)
	}
	return status
}

func leaseState(t *testing.T, ctx context.Context, db *sql.DB, leaseID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, leaseID).Scan(&state); err != nil {
		t.Fatalf("query lease state: %v", err)
	}
	return state
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

func countActiveLeases(t *testing.T, ctx context.Context, db *sql.DB, assignmentID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM leases
WHERE assignment_id = ? AND state = 'active'`, assignmentID).Scan(&count); err != nil {
		t.Fatalf("count active leases: %v", err)
	}
	return count
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}
