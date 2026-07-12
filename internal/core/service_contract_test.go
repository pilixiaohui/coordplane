package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func TestCT02ConcurrentClaimCreatesExactlyOneRun(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "agent-one")
	project := h.addProject(t, "project-one", "")
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "start", Wake: true, RequestID: "chat-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		claim core.Claim
		ok    bool
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
			results <- result{claim: claim, ok: ok, err: err}
		}()
	}
	close(start)
	var winners []core.Claim
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.ok {
			winners = append(winners, result.claim)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("claim winners = %d, want 1", len(winners))
	}
	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 1 || snapshot.Tasks[0].Generation != 1 {
		t.Fatalf("snapshot after claim = %#v", snapshot)
	}
	if snapshot.Tasks[0].ID != chat.Task.ID || snapshot.Tasks[0].CurrentRunID != snapshot.Runs[0].ID {
		t.Fatalf("task/run claim fence mismatch: task=%#v run=%#v", snapshot.Tasks[0], snapshot.Runs[0])
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, "task.claimed") != 1 || countEvent(events, "run.created") != 1 {
		t.Fatalf("claim events = %#v", events)
	}
	before := len(events)
	if _, ok, err := h.service.ClaimNext(context.Background(), project.ID); err != nil || ok {
		t.Fatalf("second claim while agent busy: ok=%v err=%v", ok, err)
	}
	events, _ = h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if len(events) != before {
		t.Fatalf("losing claim wrote events: before=%d after=%d", before, len(events))
	}
}

func TestCT02ClaimOrderingAndOneLiveRunPerAgent(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "ordered-agent")
	project := h.addProject(t, "ordered-project", "")
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "conversation", Wake: true, RequestID: "chat-order",
	}); err != nil {
		t.Fatal(err)
	}
	high, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "high priority", Priority: 100, RequestID: "task-high",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claim.Task.ID != high.ID {
		t.Fatalf("claimed task %s, want high-priority %s", claim.Task.ID, high.ID)
	}
	if _, ok, err := h.service.ClaimNext(context.Background(), project.ID); err != nil || ok {
		t.Fatalf("second task for same live agent claimed: ok=%v err=%v", ok, err)
	}
}

func TestCT03StaleRunCannotWriteThroughAgentEntry(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "fenced-agent")
	project := h.addProject(t, "fenced-project", "")
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "run", Wake: true, RequestID: "fence-chat",
	}); err != nil {
		t.Fatal(err)
	}
	run1, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim run1: ok=%v err=%v", ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), run1.Run.ID, "activate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{Token: run1.Token, Summary: "live\a", RequestID: "progress-live"}); err != nil {
		t.Fatal(err)
	}
	progressEvents, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, EntityType: "task", EntityID: run1.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range progressEvents {
		if event.Kind == "task.progress" && !json.Valid([]byte(event.PayloadJSON)) {
			t.Fatalf("progress payload is not valid JSON: %q", event.PayloadJSON)
		}
	}
	if _, err := h.service.InterruptRun(context.Background(), run1.Run.ID, "test rollover", "interrupt-1"); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	run2, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim run2: ok=%v err=%v", ok, err)
	}
	if run2.Run.Generation != run1.Run.Generation+1 {
		t.Fatalf("run generations: old=%d new=%d", run1.Run.Generation, run2.Run.Generation)
	}
	if _, err := h.service.ActivateRun(context.Background(), run2.Run.ID, "activate-2"); err != nil {
		t.Fatal(err)
	}
	before := h.durableSignature(t, project.ID)
	requests := []struct {
		name string
		call func() error
	}{
		{"progress", func() error {
			_, err := h.service.Progress(context.Background(), core.ProgressInput{Token: run1.Token, Summary: "stale", RequestID: "stale-progress"})
			return err
		}},
		{"message", func() error {
			_, err := h.service.AgentMessageToBoss(context.Background(), core.AgentMessageInput{Token: run1.Token, Body: "stale", RequestID: "stale-message"})
			return err
		}},
		{"submit", func() error {
			_, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{Token: run1.Token, Outcome: "submit", Summary: "stale", ExpectedHead: h.git.sha, RequestID: "stale-submit"})
			return err
		}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			err := request.call()
			if !core.IsCode(err, core.CodeStaleRun) {
				t.Fatalf("error = %v, want STALE_RUN", err)
			}
			if after := h.durableSignature(t, project.ID); after != before {
				t.Fatalf("stale request changed durable state\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{Token: run2.Token, Summary: "current", RequestID: "progress-current"}); err != nil {
		t.Fatalf("current run progress failed: %v", err)
	}
}

func TestOutcomeReplayIsIdempotentAndRejectsChangedInputAfterTokenRevocation(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "outcome-agent")
	project := h.addProject(t, "outcome-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "submit once", RequestID: "task-outcome",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claim.Task.ID != task.ID {
		t.Fatalf("claimed task %s, want %s", claim.Task.ID, task.ID)
	}
	if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, "activate-outcome"); err != nil {
		t.Fatal(err)
	}

	input := core.OutcomeInput{
		Token: claim.Token, Outcome: "submit", Summary: "ready",
		ExpectedHead: h.git.sha, RequestID: "submit-outcome",
	}
	first, err := h.service.RequestOutcome(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != core.TaskFinishing {
		t.Fatalf("outcome task status = %s, want %s", first.Status, core.TaskFinishing)
	}
	before := h.durableSignature(t, project.ID)

	replayed, err := h.service.RequestOutcome(context.Background(), input)
	if err != nil {
		t.Fatalf("idempotent replay after token revocation: %v", err)
	}
	if replayed.ID != first.ID || replayed.Version != first.Version {
		t.Fatalf("replayed task = %#v, want original %#v", replayed, first)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatalf("idempotent replay changed durable state\nbefore=%s\nafter=%s", before, after)
	}

	input.Summary = "must not replace the accepted payload"
	input.ExpectedHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := h.service.RequestOutcome(context.Background(), input); !core.IsCode(err, core.CodeVersionConflict) {
		t.Fatalf("changed idempotency input error = %v, want VERSION_CONFLICT", err)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatalf("conflicting replay changed durable state\nbefore=%s\nafter=%s", before, after)
	}
}

func TestAgentMessageReplyCannotCrossProject(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "reply-agent")
	firstProject := h.addProject(t, "reply-project-one", "")
	secondProject := h.addProject(t, "reply-project-two", "")
	foreign, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: secondProject.ID, AgentID: agent.ID, Body: "foreign",
		Wake: false, RequestID: "foreign-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: firstProject.ID, AgentID: agent.ID, Body: "current",
		Wake: true, RequestID: "current-message",
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), firstProject.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, "activate-reply"); err != nil {
		t.Fatal(err)
	}
	beforeFirst := h.durableSignature(t, firstProject.ID)
	beforeSecond := h.durableSignature(t, secondProject.ID)
	_, err = h.service.AgentMessageToBoss(context.Background(), core.AgentMessageInput{
		Token: claim.Token, Body: "invalid reply", ReplyTo: foreign.Message.ID,
		RequestID: "cross-project-reply",
	})
	if !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("cross-project reply error = %v, want SCOPE_DENIED", err)
	}
	if after := h.durableSignature(t, firstProject.ID); after != beforeFirst {
		t.Fatal("cross-project reply changed the sender project")
	}
	if after := h.durableSignature(t, secondProject.ID); after != beforeSecond {
		t.Fatal("cross-project reply changed the referenced project")
	}
}

func TestCT07ConversationIsDurableReusedAndKindSafe(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "chat-agent")
	project := h.addProject(t, "chat-project", "")
	first, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "first", Wake: false, RequestID: "chat-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "second", Wake: false, RequestID: "chat-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Task.ID != second.Task.ID || first.Task.Kind != core.TaskConversation {
		t.Fatalf("conversation was not reused: first=%#v second=%#v", first.Task, second.Task)
	}
	if first.Task.Status != core.TaskWaiting {
		t.Fatalf("wake=false conversation status = %s", first.Task.Status)
	}
	eventsBefore, _ := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	replayed, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "second", Wake: false, RequestID: "chat-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Message.ID != second.Message.ID {
		t.Fatalf("idempotent replay returned message %s, want %s", replayed.Message.ID, second.Message.ID)
	}
	eventsAfter, _ := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatal("idempotent chat replay emitted another event")
	}
	beforeConflict := h.durableSignature(t, project.ID)
	if _, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "different body", Wake: true, RequestID: "chat-second",
	}); !core.IsCode(err, core.CodeVersionConflict) {
		t.Fatalf("conflicting idempotency replay error = %v, want VERSION_CONFLICT", err)
	}
	if after := h.durableSignature(t, project.ID); after != beforeConflict {
		t.Fatal("conflicting idempotency replay changed durable state")
	}
	work, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Title: "work", Kind: core.TaskWork, RequestID: "chat-work",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := h.durableSignature(t, project.ID)
	if _, err := h.service.CloseConversation(context.Background(), work.ID, "close-work"); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("work close error = %v", err)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("invalid work close changed durable state")
	}

	if err := h.database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.database = reopened
	h.service, err = core.NewService(reopened, h.git, core.ServiceOptions{Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := h.service.ListMessages(context.Background(), core.MessageFilter{TaskID: first.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Items) != 2 || messages.Items[0].Body != "first" || messages.Items[1].Body != "second" {
		t.Fatalf("messages after restart = %#v", messages)
	}
}

func TestCT09ConversationCloseDisposesMessagesBeforeArchive(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "closing-agent")
	project := h.addProject(t, "closing-project", agent.ID)
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "do not orphan me", Wake: false, RequestID: "closing-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CloseConversation(context.Background(), chat.Task.ID, "close-conversation"); err != nil {
		t.Fatal(err)
	}
	messages, err := h.service.ListMessages(context.Background(), core.MessageFilter{TaskID: chat.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages.Items) != 1 || messages.Items[0].State != core.MessageCancelled {
		t.Fatalf("messages after close = %#v, want one cancelled message", messages)
	}
	if _, err := h.service.ArchiveAgent(context.Background(), agent.ID, "archive-closing-agent"); err != nil {
		t.Fatalf("archive agent after message disposition: %v", err)
	}
	archived, err := h.service.ArchiveProject(context.Background(), project.ID, "archive-closing-project")
	if err != nil || archived.Status != core.ProjectArchived {
		t.Fatalf("archive project = %#v err=%v", archived, err)
	}
}

func TestCT09LifecycleGuardsAndRepair(t *testing.T) {
	t.Run("pause does not stop and open work blocks archive", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "busy-agent")
		project := h.addProject(t, "busy-project", "")
		if _, err := h.service.Chat(context.Background(), core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "run", Wake: true, RequestID: "busy-chat"}); err != nil {
			t.Fatal(err)
		}
		claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, "busy-active"); err != nil {
			t.Fatal(err)
		}
		paused, err := h.service.SetAgentStatus(context.Background(), agent.ID, core.AgentPaused, "pause")
		if err != nil || paused.Status != core.AgentPaused {
			t.Fatalf("pause = %#v err=%v", paused, err)
		}
		snapshot, _ := h.database.Snapshot(context.Background(), project.ID)
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].State != core.RunActive {
			t.Fatalf("pause stopped run: %#v", snapshot.Runs)
		}
		if _, err := h.service.ArchiveAgent(context.Background(), agent.ID, "archive-busy"); !core.IsCode(err, core.CodeInvalidState) {
			t.Fatalf("archive busy error = %v", err)
		}
		if _, err := h.service.ArchiveProject(context.Background(), project.ID, "archive-project-busy"); !core.IsCode(err, core.CodeInvalidState) {
			t.Fatalf("archive project error = %v", err)
		}
	})

	t.Run("archive clears default integrator and rejects future messages", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "integrator")
		project := h.addProject(t, "integrator-project", agent.ID)
		archived, err := h.service.ArchiveAgent(context.Background(), agent.ID, "archive-integrator")
		if err != nil || archived.Status != core.AgentArchived {
			t.Fatalf("archive = %#v err=%v", archived, err)
		}
		project, err = h.database.Project(context.Background(), project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if project.IntegrationAgentID != "" {
			t.Fatalf("default integrator survived archive: %#v", project)
		}
		before := h.durableSignature(t, project.ID)
		if _, err := h.service.Chat(context.Background(), core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "orphan", Wake: true, RequestID: "archived-chat"}); !core.IsCode(err, core.CodeInvalidState) {
			t.Fatalf("archived chat error = %v", err)
		}
		if after := h.durableSignature(t, project.ID); after != before {
			t.Fatal("archived recipient created orphan message")
		}
		project, err = h.service.ArchiveProject(context.Background(), project.ID, "archive-clean-project")
		if err != nil || project.Status != core.ProjectArchived {
			t.Fatalf("archive clean project = %#v err=%v", project, err)
		}
	})

	t.Run("error remains fail closed until repair", func(t *testing.T) {
		h := newHarness(t)
		h.git.initializeErr = errors.New("injected Git failure")
		project, err := h.service.AddProject(context.Background(), core.AddProjectInput{Name: "broken", Source: "/source", SourceRef: "refs/heads/main", RequestID: "broken-add"})
		if !core.IsCode(err, core.CodeGitInvariantViolation) {
			t.Fatalf("add error = %v", err)
		}
		persisted, err := h.database.Project(context.Background(), project.ID)
		if err != nil || persisted.Status != core.ProjectError {
			t.Fatalf("persisted project = %#v err=%v", persisted, err)
		}
		if _, ok, err := h.service.ClaimNext(context.Background(), persisted.ID); err != nil || ok {
			t.Fatalf("error project scheduled: ok=%v err=%v", ok, err)
		}
		h.git.initializeErr = nil
		repaired, err := h.service.RepairProject(context.Background(), persisted.ID, "repair")
		if err != nil || repaired.Status != core.ProjectActive {
			t.Fatalf("repair = %#v err=%v", repaired, err)
		}
		beforeReplay := h.durableSignature(t, persisted.ID)
		replayed, err := h.service.RepairProject(context.Background(), persisted.ID, "repair")
		if err != nil || replayed.ID != repaired.ID || replayed.Status != core.ProjectActive {
			t.Fatalf("repair replay = %#v err=%v", replayed, err)
		}
		if after := h.durableSignature(t, persisted.ID); after != beforeReplay {
			t.Fatal("successful repair replay changed durable state")
		}
	})

	t.Run("failed repair replay remains a stable failure", func(t *testing.T) {
		h := newHarness(t)
		h.git.initializeErr = errors.New("initial registration failure")
		project, err := h.service.AddProject(context.Background(), core.AddProjectInput{
			Name: "repair-failure", Source: "/source", SourceRef: "refs/heads/main", RequestID: "repair-failure-add",
		})
		if !core.IsCode(err, core.CodeGitInvariantViolation) {
			t.Fatalf("registration error = %v", err)
		}
		h.git.initializeErr = errors.New("repair still failing")
		if _, err := h.service.RepairProject(context.Background(), project.ID, "repair-failure-request"); !core.IsCode(err, core.CodeGitInvariantViolation) {
			t.Fatalf("first repair error = %v", err)
		}
		beforeReplay := h.durableSignature(t, project.ID)
		if _, err := h.service.RepairProject(context.Background(), project.ID, "repair-failure-request"); !core.IsCode(err, core.CodeGitInvariantViolation) {
			t.Fatalf("failed repair replay error = %v", err)
		}
		if after := h.durableSignature(t, project.ID); after != beforeReplay {
			t.Fatal("failed repair replay changed durable state")
		}
	})
}

func TestP1MutationsEmitCanonicalEvents(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "event-agent")
	project := h.addProject(t, "event-project", agent.ID)
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "event coverage",
		Wake: false, RequestID: "event-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.SetAgentStatus(context.Background(), agent.ID, core.AgentPaused, "event-pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.SetAgentStatus(context.Background(), agent.ID, core.AgentActive, "event-resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CloseConversation(context.Background(), chat.Task.ID, "event-close"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ArchiveAgent(context.Background(), agent.ID, "event-agent-archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ArchiveProject(context.Background(), project.ID, "event-project-archive"); err != nil {
		t.Fatal(err)
	}

	events, err := h.database.Events(context.Background(), core.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"agent.created": false, "project.creating": false, "project.active": false,
		"task.created": false, "message.created": false, "agent.paused": false,
		"agent.resumed": false, "message.cancelled": false, "task.completed": false,
		"project.updated": false, "agent.archived": false, "project.archived": false,
	}
	for _, event := range events {
		if _, ok := want[event.Kind]; ok {
			want[event.Kind] = true
		}
		if (event.ActorKind == "boss" || event.ActorKind == "agent") && event.RequestID == "" {
			t.Fatalf("mutation event %s omitted request ID: %#v", event.Kind, event)
		}
		if (event.Kind == "project.creating" || event.Kind == "project.active") && event.OperationID == "" {
			t.Fatalf("project external-action event omitted operation ID: %#v", event)
		}
	}
	for kind, found := range want {
		if !found {
			t.Errorf("canonical mutation event %q was not emitted", kind)
		}
	}
}

type harness struct {
	t        *testing.T
	path     string
	database *store.Store
	service  *core.Service
	git      *fakeGit
	clock    *testClock
	ids      *testIDs
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coordplane.db")
	database, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{value: time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC)}
	idSource := &testIDs{}
	git := &fakeGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	service, err := core.NewService(database, git, core.ServiceOptions{Now: clock.Now, NewID: idSource.New, MaxParallelRuns: 4})
	if err != nil {
		t.Fatal(err)
	}
	service.SetReady(true, "")
	h := &harness{t: t, path: path, database: database, service: service, git: git, clock: clock, ids: idSource}
	t.Cleanup(func() { _ = h.database.Close() })
	return h
}

func (h *harness) addAgent(t *testing.T, name string) core.Agent {
	t.Helper()
	agent, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: name, AdapterID: "one-shot", Image: "agent:latest",
		InstructionsFile: "/instructions/agent.md", RequestID: "add-agent-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func (h *harness) addProject(t *testing.T, name, integrator string) core.Project {
	t.Helper()
	project, err := h.service.AddProject(context.Background(), core.AddProjectInput{
		Name: name, Source: "/source", SourceRef: "refs/heads/main",
		IntegrationAgentID: integrator, RequestID: "add-project-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func (h *harness) durableSignature(t *testing.T, projectID string) string {
	t.Helper()
	snapshot, err := h.database.Snapshot(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Snapshot core.Snapshot `json:"snapshot"`
		Events   []core.Event  `json:"events"`
	}{snapshot, events})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type testClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(time.Microsecond)
	return c.value
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(duration)
}

type testIDs struct {
	mu   sync.Mutex
	next int
}

func (i *testIDs) New(prefix string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.next++
	return fmt.Sprintf("%s_%06d", prefix, i.next), nil
}

type fakeGit struct {
	mu            sync.Mutex
	sha           string
	initializeErr error
	exists        bool
}

func (g *fakeGit) Preflight(context.Context, string, string) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{Source: "/source", SourceRef: "refs/heads/main", InitialSHA: g.sha, CanonicalRef: "refs/heads/main", CanonicalSHA: g.sha}, nil
}

func (g *fakeGit) ControlPath(projectID string) string { return "/control/" + projectID + ".git" }

func (g *fakeGit) Initialize(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.initializeErr != nil {
		return core.ProjectGitFact{}, g.initializeErr
	}
	g.exists = true
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}

func (g *fakeGit) Verify(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}

func (g *fakeGit) Exists(string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exists
}

func (g *fakeGit) Resolve(context.Context, string, string) (string, error) { return g.sha, nil }

func countEvent(events []core.Event, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
