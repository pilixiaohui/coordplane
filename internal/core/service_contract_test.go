package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
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
	if _, err := activateRun(t, h, context.Background(), run1.Run.ID, "activate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{Token: run1.Token, Summary: "live\a", RequestID: "progress-live"}); err != nil {
		t.Fatal(err)
	}
	progressEvents, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, EntityType: "task", EntityID: run1.Task.ID})
	requireNoError(t, err)
	for _, event := range progressEvents {
		if event.Kind == "task.progress" && !json.Valid([]byte(event.PayloadJSON)) {
			t.Fatalf("progress payload is not valid JSON: %q", event.PayloadJSON)
		}
	}
	interrupted, err := interruptRun(t, h, context.Background(), run1.Run.ID, "test rollover", "interrupt-1")
	requireNoError(t, err)
	recordCleanupRemoved(t, h, interrupted, "interrupt-cleanup-1")
	h.clock.Advance(time.Second)
	run2, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim run2: ok=%v err=%v", ok, err)
	}
	if run2.Run.Generation != run1.Run.Generation+1 {
		t.Fatalf("run generations: old=%d new=%d", run1.Run.Generation, run2.Run.Generation)
	}
	if _, err := activateRun(t, h, context.Background(), run2.Run.ID, "activate-2"); err != nil {
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
			_, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{Token: run1.Token, RecipientKind: "boss", Body: "stale", RequestID: "stale-message"})
			return err
		}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			err := request.call()
			if !core.IsCode(err, core.CodeStaleRun) {
				t.Fatalf("error = %v, want STALE_RUN", err)
			}
			h.requireDurableSignature(t, project.ID, before)
		})
	}
	if _, err := h.service.Progress(context.Background(), core.ProgressInput{Token: run2.Token, Summary: "current", RequestID: "progress-current"}); err != nil {
		t.Fatalf("current run progress failed: %v", err)
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
	requireNoError(t, err)
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
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "activate-reply"); err != nil {
		t.Fatal(err)
	}
	beforeFirst := h.durableSignature(t, firstProject.ID)
	beforeSecond := h.durableSignature(t, secondProject.ID)
	_, err = h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "invalid reply", ReplyTo: foreign.Message.ID,
		RequestID: "cross-project-reply",
	})
	if !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("cross-project reply error = %v, want SCOPE_DENIED", err)
	}
	h.requireDurableSignature(t, firstProject.ID, beforeFirst)
	h.requireDurableSignature(t, secondProject.ID, beforeSecond)
}

func TestAgentMessageRelatedTaskCannotEscapeRunScope(t *testing.T) {
	h := newHarness(t)
	sender := h.addAgent(t, "related-sender")
	other := h.addAgent(t, "related-other")
	project := h.addProject(t, "related-project", "")
	foreignProject := h.addProject(t, "related-foreign-project", "")
	current, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: sender.ID, Kind: core.TaskWork,
		Title: "current", Priority: 100, RequestID: "related-current",
	})
	requireNoError(t, err)
	unrelated, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: other.ID, Kind: core.TaskWork,
		Title: "unrelated", RequestID: "related-unrelated",
	})
	requireNoError(t, err)
	foreign, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: foreignProject.ID, AssigneeAgentID: other.ID, Kind: core.TaskWork,
		Title: "foreign", RequestID: "related-foreign",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != current.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "related-active"); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []core.Task{unrelated, foreign} {
		beforeProject := h.durableSignature(t, project.ID)
		beforeForeign := h.durableSignature(t, foreignProject.ID)
		_, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
			Token: claim.Token, RecipientKind: "boss", RelatedTaskID: candidate.ID,
			Body: "out-of-scope association", RequestID: "related-denied-" + candidate.ID,
		})
		if !core.IsCode(err, core.CodeScopeDenied) {
			t.Fatalf("related task %s error = %v, want SCOPE_DENIED", candidate.ID, err)
		}
		h.requireDurableSignature(t, project.ID, beforeProject)
		h.requireDurableSignature(t, foreignProject.ID, beforeForeign)
	}

	privateMessage, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
		ProjectID: project.ID, AgentID: other.ID, TaskID: unrelated.ID,
		Body: "private context", RequestID: "related-private-message",
	})
	requireNoError(t, err)
	beforeProject := h.durableSignature(t, project.ID)
	_, err = h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", ReplyTo: privateMessage.ID,
		Body: "out-of-scope reply", RequestID: "related-private-reply-denied",
	})
	if !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("private reply error = %v, want SCOPE_DENIED", err)
	}
	h.requireDurableSignature(t, project.ID, beforeProject)
}

func TestCT07ConversationIsDurableReusedAndKindSafe(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "chat-agent")
	project := h.addProject(t, "chat-project", "")
	first, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "first", Wake: false, RequestID: "chat-first",
	})
	requireNoError(t, err)
	second, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "second", Wake: false, RequestID: "chat-second",
	})
	requireNoError(t, err)
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
	requireNoError(t, err)
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
	h.requireDurableSignature(t, project.ID, beforeConflict)
	work, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Title: "work", Kind: core.TaskWork, RequestID: "chat-work",
	})
	requireNoError(t, err)
	before := h.durableSignature(t, project.ID)
	if _, err := h.service.CloseConversation(context.Background(), work.ID, "close-work"); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("work close error = %v", err)
	}
	h.requireDurableSignature(t, project.ID, before)

	requireNoError(t, h.database.Close())
	reopened, err := store.Open(context.Background(), h.path)
	requireNoError(t, err)
	h.database = reopened
	h.service, err = core.NewService(reopened, h.git, core.ServiceOptions{Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"}})
	requireNoError(t, err)
	messages, err := h.service.ListMessages(context.Background(), core.MessageFilter{TaskID: first.Task.ID})
	requireNoError(t, err)
	if len(messages.Items) != 2 || messages.Items[0].Body != "first" || messages.Items[1].Body != "second" {
		t.Fatalf("messages after restart = %#v", messages)
	}
}

func TestCT07LegacyBossMessageDedupesSurviveSQLiteReopen(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		setup     func(*testing.T, *harness, core.Project, core.Agent) (core.Task, core.Message)
		inputHash func(core.Project, core.Agent, core.Task, core.Message) string
		replay    func(context.Context, *core.Service, core.Project, core.Agent, core.Task, string, string) (core.Task, core.Message, error)
	}{
		{
			name: "chat", operation: "chat.send",
			setup: func(t *testing.T, h *harness, project core.Project, agent core.Agent) (core.Task, core.Message) {
				result, err := h.service.Chat(context.Background(), core.ChatInput{
					ProjectID: project.ID, AgentID: agent.ID, Body: "legacy chat", Wake: true,
					RequestID: "legacy-chat-setup",
				})
				requireNoError(t, err)
				return result.Task, result.Message
			},
			inputHash: func(project core.Project, agent core.Agent, _ core.Task, _ core.Message) string {
				return legacyFingerprint(t, struct {
					ProjectID, AgentID, Body, RelatedTaskID, ReplyToID, AckIDs string
					Wake                                                       bool
				}{project.ID, agent.ID, "legacy chat", "", "", "", true})
			},
			replay: func(ctx context.Context, service *core.Service, project core.Project, agent core.Agent, _ core.Task, requestID, body string) (core.Task, core.Message, error) {
				result, err := service.Chat(ctx, core.ChatInput{
					ProjectID: project.ID, AgentID: agent.ID, Body: body, Wake: true, RequestID: requestID,
				})
				return result.Task, result.Message, err
			},
		},
		{
			name: "message", operation: "message.send",
			setup: func(t *testing.T, h *harness, project core.Project, agent core.Agent) (core.Task, core.Message) {
				task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
					ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
					Title: "legacy message task", RequestID: "legacy-message-task",
				})
				requireNoError(t, err)
				message, err := h.service.SendBossMessage(context.Background(), core.BossMessageInput{
					ProjectID: project.ID, AgentID: agent.ID, TaskID: task.ID, Body: "legacy message",
					Wake: true, RequestID: "legacy-message-setup",
				})
				requireNoError(t, err)
				return task, message
			},
			inputHash: func(project core.Project, agent core.Agent, task core.Task, _ core.Message) string {
				return legacyFingerprint(t, struct {
					ProjectID, AgentID, TaskID, RelatedTaskID, Body, ReplyToID, AckIDs string
					Wake                                                               bool
				}{project.ID, agent.ID, task.ID, "", "legacy message", "", "", true})
			},
			replay: func(ctx context.Context, service *core.Service, project core.Project, agent core.Agent, task core.Task, requestID, body string) (core.Task, core.Message, error) {
				message, err := service.SendBossMessage(ctx, core.BossMessageInput{
					ProjectID: project.ID, AgentID: agent.ID, TaskID: task.ID, Body: body,
					Wake: true, RequestID: requestID,
				})
				return task, message, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			agent := h.addAgent(t, "legacy-"+test.name+"-agent")
			project := h.addProject(t, "legacy-"+test.name+"-project", "")
			task, message := test.setup(t, h, project, agent)
			requestID := "legacy-" + test.name + "-replay"
			resultID, relatedID := message.ID, ""
			if test.operation == "chat.send" {
				resultID, relatedID = task.ID, message.ID
			}
			raw, err := json.Marshal(struct {
				ID        string `json:"id"`
				RelatedID string `json:"related_id,omitempty"`
				InputHash string `json:"input_hash"`
			}{resultID, relatedID, test.inputHash(project, agent, task, message)})
			requireNoError(t, err)
			if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
				return tx.PutDedupe("boss", test.operation, requestID, raw, h.clock.Now().Format(time.RFC3339Nano))
			}); err != nil {
				t.Fatal(err)
			}
			requireNoError(t, h.database.Close())
			h.database, err = store.Open(context.Background(), h.path)
			requireNoError(t, err)
			h.service, err = core.NewService(h.database, h.git, core.ServiceOptions{
				Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
			})
			requireNoError(t, err)
			before := h.durableSignature(t, project.ID)
			replayedTask, replayedMessage, err := test.replay(context.Background(), h.service, project, agent, task, requestID, message.Body)
			requireNoError(t, err)
			if replayedTask.ID != task.ID || replayedMessage.ID != message.ID {
				t.Fatalf("legacy replay = task %s message %s, want %s/%s", replayedTask.ID, replayedMessage.ID, task.ID, message.ID)
			}
			h.requireDurableSignature(t, project.ID, before)
			if _, _, err := test.replay(context.Background(), h.service, project, agent, task, requestID, message.Body+" changed"); !core.IsCode(err, core.CodeVersionConflict) {
				t.Fatalf("different-input legacy replay error = %v, want VERSION_CONFLICT", err)
			}
			h.requireDurableSignature(t, project.ID, before)
		})
	}
}

func legacyFingerprint(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	requireNoError(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestCT07KindErrorsRemainStableWhileTaskIsFinishing(t *testing.T) {
	t.Run("work close", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "finishing-work-kind-agent")
		project := h.addProject(t, "finishing-work-kind-project", "")
		claim := createActiveWorkClaim(t, h, project, agent, "finishing-work-kind")
		if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
			Token: claim.Token, Outcome: core.OutcomeWait, Reason: "finishing",
			RequestID: "finishing-work-kind-wait",
		}); err != nil {
			t.Fatal(err)
		}
		before := h.durableSignature(t, project.ID)
		if _, err := h.service.CloseConversation(context.Background(), claim.Task.ID, "finishing-work-kind-close"); !core.IsCode(err, core.CodeInvalidState) {
			t.Fatalf("finishing work close error = %v, want INVALID_STATE", err)
		}
		h.requireDurableSignature(t, project.ID, before)
	})

	t.Run("conversation accept and rework", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "finishing-conversation-kind-agent")
		project := h.addProject(t, "finishing-conversation-kind-project", "")
		chat, err := h.service.Chat(context.Background(), core.ChatInput{
			ProjectID: project.ID, AgentID: agent.ID, Body: "start", Wake: true,
			RequestID: "finishing-conversation-kind-chat",
		})
		requireNoError(t, err)
		claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
		if err != nil || !ok || claim.Task.ID != chat.Task.ID {
			t.Fatalf("conversation claim = %#v ok=%t err=%v", claim, ok, err)
		}
		if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "finishing-conversation-kind-active"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
			Token: claim.Token, Outcome: core.OutcomeWait, Reason: "finishing",
			RequestID: "finishing-conversation-kind-wait",
		}); err != nil {
			t.Fatal(err)
		}
		for _, action := range []struct {
			name string
			call func() error
		}{
			{name: "accept", call: func() error {
				_, err := h.service.RequestAccept(context.Background(), core.AcceptInput{TaskID: chat.Task.ID, RequestID: "finishing-conversation-kind-accept"})
				return err
			}},
			{name: "rework", call: func() error {
				_, err := h.service.ReworkTask(context.Background(), core.TaskActionInput{TaskID: chat.Task.ID, RequestID: "finishing-conversation-kind-rework"})
				return err
			}},
		} {
			before := h.durableSignature(t, project.ID)
			if err := action.call(); !core.IsCode(err, core.CodeInvalidState) {
				t.Fatalf("finishing conversation %s error = %v, want INVALID_STATE", action.name, err)
			}
			h.requireDurableSignature(t, project.ID, before)
		}
	})
}

func TestCT09ConversationCloseDisposesMessagesBeforeArchive(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "closing-agent")
	project := h.addProject(t, "closing-project", agent.ID)
	chat, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "do not orphan me", Wake: false, RequestID: "closing-chat",
	})
	requireNoError(t, err)
	if _, err := h.service.CloseConversation(context.Background(), chat.Task.ID, "close-conversation"); err != nil {
		t.Fatal(err)
	}
	messages, err := h.service.ListMessages(context.Background(), core.MessageFilter{TaskID: chat.Task.ID})
	requireNoError(t, err)
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
		if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "busy-active"); err != nil {
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
		requireNoError(t, err)
		if project.IntegrationAgentID != "" {
			t.Fatalf("default integrator survived archive: %#v", project)
		}
		before := h.durableSignature(t, project.ID)
		if _, err := h.service.Chat(context.Background(), core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "orphan", Wake: true, RequestID: "archived-chat"}); !core.IsCode(err, core.CodeInvalidState) {
			t.Fatalf("archived chat error = %v", err)
		}
		h.requireDurableSignature(t, project.ID, before)
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
		h.requireDurableSignature(t, persisted.ID, beforeReplay)
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
		h.requireDurableSignature(t, project.ID, beforeReplay)
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
	requireNoError(t, err)
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
	requireNoError(t, err)
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

func TestStatusPropagatesPerItemTextTruncationFromSQLite(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *harness, core.Project, core.Agent) string
		check func(*testing.T, core.TaskView)
	}{
		{
			name: "task title",
			setup: func(t *testing.T, h *harness, project core.Project, agent core.Agent) string {
				task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
					ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
					Title: strings.Repeat("title", 60), RequestID: "long-title-task",
				})
				requireNoError(t, err)
				return task.ID
			},
			check: func(t *testing.T, view core.TaskView) {
				if !view.Task.TitleTruncated || view.Task.TextTruncated {
					t.Fatalf("task truncation flags = title:%t text:%t", view.Task.TitleTruncated, view.Task.TextTruncated)
				}
			},
		},
		{
			name: "task failure",
			setup: func(t *testing.T, h *harness, project core.Project, agent core.Agent) string {
				task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
					ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
					Title: "failure task", RequestID: "long-failure-task",
				})
				requireNoError(t, err)
				claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
				if err != nil || !ok || claim.Task.ID != task.ID {
					t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
				}
				if _, err := interruptRun(t, h, context.Background(), claim.Run.ID, strings.Repeat("failure", 90), "long-failure"); err != nil {
					t.Fatal(err)
				}
				return task.ID
			},
			check: func(t *testing.T, view core.TaskView) {
				if view.Task.TitleTruncated || !view.Task.TextTruncated {
					t.Fatalf("task truncation flags = title:%t text:%t", view.Task.TitleTruncated, view.Task.TextTruncated)
				}
			},
		},
		{
			name: "current run error",
			setup: func(t *testing.T, h *harness, project core.Project, agent core.Agent) string {
				task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
					ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
					Title: "current run task", RequestID: "long-current-run-task",
				})
				requireNoError(t, err)
				claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
				if err != nil || !ok || claim.Task.ID != task.ID {
					t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
				}
				if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
					run, err := tx.Run(claim.Run.ID)
					if err != nil {
						return err
					}
					version, state := run.Version, run.State
					run.LastError = strings.Repeat("runtime error", 50)
					run.Version++
					return tx.UpdateRun(run, version, state)
				}); err != nil {
					t.Fatal(err)
				}
				return task.ID
			},
			check: func(t *testing.T, view core.TaskView) {
				if view.CurrentRun == nil || !view.CurrentRun.TextTruncated {
					t.Fatalf("current run summary = %#v", view.CurrentRun)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			agent := h.addAgent(t, "status-truncation-agent")
			project := h.addProject(t, "status-truncation-project", "")
			taskID := test.setup(t, h, project, agent)

			status, err := h.service.Status(context.Background(), project.ID)
			requireNoError(t, err)
			if len(status.Tasks) > core.StatusSnapshotLimit {
				t.Fatalf("status task count = %d, want at most %d", len(status.Tasks), core.StatusSnapshotLimit)
			}
			if !status.SummaryTruncated {
				t.Fatal("status did not propagate per-item truncation")
			}
			for _, view := range status.Tasks {
				if view.Task.ID == taskID {
					test.check(t, view)
					return
				}
			}
			t.Fatalf("status omitted task %s", taskID)
		})
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

func TestRetiredCodexAdapterIsRejectedBeforeDurableMutation(t *testing.T) {
	h := newHarness(t)
	before := h.durableSignature(t, "")
	_, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Retired adapter", AdapterID: "codex", Image: "agent:latest",
		InstructionsFile: "/instructions/retired.md", RequestID: "retired-adapter",
	})
	if !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("retired adapter error = %v, want %s", err, core.CodeInvalidArgument)
	}
	h.requireDurableSignature(t, "", before)
}

func TestQueuedTaskWithUnknownPersistedAdapterFailsClosed(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "persisted-adapter-agent")
	project := h.addProject(t, "persisted-adapter-project", "")
	if _, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "must not be silently skipped", RequestID: "persisted-adapter-task",
	}); err != nil {
		t.Fatal(err)
	}
	service, err := core.NewService(h.database, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"different-adapter"},
	})
	requireNoError(t, err)
	before := h.durableSignature(t, project.ID)
	if _, ok, err := service.ClaimNext(context.Background(), project.ID); ok || !core.IsCode(err, core.CodeRuntimeInvariantViolation) {
		t.Fatalf("claim unknown persisted adapter: ok=%t err=%v", ok, err)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coordplane.db")
	database, err := store.Open(context.Background(), path)
	requireNoError(t, err)
	clock := &testClock{value: time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC)}
	idSource := &testIDs{}
	git := &fakeGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	service, err := core.NewService(database, git, core.ServiceOptions{Now: clock.Now, NewID: idSource.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"}})
	requireNoError(t, err)
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
	requireNoError(t, err)
	return agent
}

func (h *harness) addProject(t *testing.T, name, integrator string) core.Project {
	t.Helper()
	project, err := h.service.AddProject(context.Background(), core.AddProjectInput{
		Name: name, Source: "/source", SourceRef: "refs/heads/main",
		IntegrationAgentID: integrator, RequestID: "add-project-" + name,
	})
	requireNoError(t, err)
	return project
}

func (h *harness) durableSignature(t *testing.T, projectID string) string {
	t.Helper()
	snapshot, err := h.database.Snapshot(context.Background(), projectID)
	requireNoError(t, err)
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: projectID})
	requireNoError(t, err)
	raw, err := json.Marshal(struct {
		Snapshot core.Snapshot `json:"snapshot"`
		Events   []core.Event  `json:"events"`
	}{snapshot, events})
	requireNoError(t, err)
	return string(raw)
}

func (h *harness) requireDurableSignature(t *testing.T, projectID, want string) {
	t.Helper()
	if got := h.durableSignature(t, projectID); got != want {
		t.Fatalf("durable state changed\nbefore=%s\nafter=%s", want, got)
	}
}

func prepareTestRun(t *testing.T, h *harness, ctx context.Context, runID, requestID string) (core.Run, error) {
	t.Helper()
	run, err := h.database.Run(ctx, runID)
	if err != nil {
		return core.Run{}, err
	}
	task, err := h.database.Task(ctx, run.TaskID)
	if err != nil {
		return core.Run{}, err
	}
	root := t.TempDir()
	workspace := ""
	if task.Kind != core.TaskConversation {
		workspace = filepath.Join(root, "workspace")
	}
	return h.service.BeginRunLaunch(ctx, core.RunLaunchInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: "nonce-" + run.ID,
		WorkspacePath: workspace, HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: "test-instructions",
		LaunchMode: "start", CleanupOperationID: "cleanup-" + run.ID,
		RequestID: requestID + "-prepare",
	})
}

func activateRun(t *testing.T, h *harness, ctx context.Context, runID, requestID string) (core.Run, error) {
	t.Helper()
	prepared, err := prepareTestRun(t, h, ctx, runID, requestID)
	if err != nil {
		return core.Run{}, err
	}
	fact := runtimeFact(prepared, "container-"+prepared.ID)
	fact.RequestID = requestID + "-created"
	created, err := h.service.RecordContainerCreated(ctx, fact)
	if err != nil {
		return core.Run{}, err
	}
	fact = runtimeFact(created, created.ContainerID)
	fact.RequestID = requestID + "-start"
	started, err := h.service.RecordRunStartIssued(ctx, fact)
	if err != nil {
		return core.Run{}, err
	}
	fact = runtimeFact(started, started.ContainerID)
	fact.RequestID = requestID
	return h.service.ObserveProcessAndActivateRun(ctx, fact)
}

func interruptRun(t *testing.T, h *harness, ctx context.Context, runID, reason, requestID string) (core.Run, error) {
	t.Helper()
	run, err := h.database.Run(ctx, runID)
	if err != nil {
		return core.Run{}, err
	}
	if run.LaunchNonce == "" {
		run, err = prepareTestRun(t, h, ctx, runID, requestID)
		if err != nil {
			return core.Run{}, err
		}
	}
	input := runtimeTerminalInput(run, core.RunInterrupted, requestID)
	input.TerminalReason = reason
	result, err := h.service.RecordRuntimeRunTerminal(ctx, input)
	return result.Run, err
}

func recordRunTerminal(h *harness, ctx context.Context, input core.RunTerminalInput) (core.RunTerminalResult, error) {
	run, err := h.database.Run(ctx, input.RunID)
	if err != nil {
		return core.RunTerminalResult{}, err
	}
	input.Generation = run.Generation
	input.LaunchNonce = run.LaunchNonce
	input.LaunchOperationID = run.LaunchOperationID
	input.ContainerID = run.ContainerID
	return h.service.RecordRuntimeRunTerminal(ctx, input)
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
	mu                    sync.Mutex
	sha                   string
	initializeErr         error
	initializeCalls       int
	exists                bool
	captureErr            error
	advanceErr            error
	advanceOutcome        core.GitAdvanceOutcome
	advanceActual         string
	discardWorkspaceCalls int
}

func (g *fakeGit) Preflight(context.Context, string, string) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{Source: "/source", SourceRef: "refs/heads/main", InitialSHA: g.sha, CanonicalRef: "refs/heads/main", CanonicalSHA: g.sha}, nil
}

func (g *fakeGit) ControlPath(projectID string) string { return "/control/" + projectID + ".git" }

func (g *fakeGit) Initialize(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initializeCalls++
	if g.initializeErr != nil {
		return core.ProjectGitFact{}, g.initializeErr
	}
	g.exists = true
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}

func (g *fakeGit) initializeCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.initializeCalls
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

func (g *fakeGit) Capture(_ context.Context, intent core.GitCaptureIntent) (core.GitCaptureFact, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.captureErr != nil {
		return core.GitCaptureFact{}, g.captureErr
	}
	return core.GitCaptureFact{
		HeadSHA: intent.ExpectedHead,
		TaskRef: "refs/coordplane/tasks/" + intent.TaskID + "/runs/" + intent.RunID,
	}, nil
}

func (g *fakeGit) CleanupCapture(context.Context, core.GitCaptureIntent) error { return nil }

func (g *fakeGit) Advance(_ context.Context, intent core.GitAdvanceIntent) (core.GitAdvanceFact, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.advanceErr != nil {
		return core.GitAdvanceFact{}, g.advanceErr
	}
	outcome := g.advanceOutcome
	if outcome == "" {
		outcome = core.GitAdvanceUpdated
	}
	actual := g.advanceActual
	if outcome != core.GitAdvanceStale {
		actual = intent.TargetSHA
		g.sha = actual
	} else if actual == "" {
		actual = g.sha
	}
	return core.GitAdvanceFact{Outcome: outcome, ActualSHA: actual}, nil
}

func (g *fakeGit) ResolveTaskRef(_ context.Context, intent core.GitTaskRefIntent) (string, error) {
	return intent.ExpectedSHA, nil
}

func (g *fakeGit) UseTaskRef(_ context.Context, intent core.GitTaskRefIntent, use func(string) error) error {
	return use(intent.ExpectedSHA)
}

func (g *fakeGit) Checkout(_ context.Context, intent core.GitCheckoutIntent) (core.GitCheckoutFact, error) {
	return core.GitCheckoutFact{Destination: intent.Destination, HeadSHA: intent.ExpectedSHA}, nil
}

func (g *fakeGit) WorkspaceState(_ context.Context, intent core.GitWorkspaceStateIntent) (core.GitWorkspaceStateFact, error) {
	return core.GitWorkspaceStateFact{
		Exists: true, Fingerprint: "workspace-fingerprint", HeadSHA: intent.ExpectedHead, Clean: true,
	}, nil
}

func (g *fakeGit) DiscardWorkspace(_ context.Context, intent core.GitDiscardWorkspaceIntent, authorize func() (bool, error)) (bool, error) {
	if intent.ExpectedFingerprint != "workspace-fingerprint" {
		return false, errors.New("workspace fingerprint changed before discard")
	}
	allowed, err := authorize()
	if err != nil || !allowed {
		return allowed, err
	}
	g.mu.Lock()
	g.discardWorkspaceCalls++
	g.mu.Unlock()
	return true, nil
}

func (g *fakeGit) TaskRefState(_ context.Context, intent core.GitDeleteRefIntent) (core.GitTaskRefStateFact, error) {
	return core.GitTaskRefStateFact{Exists: true, ActualSHA: intent.ExpectedSHA, Included: true}, nil
}

func (g *fakeGit) DeleteTaskRefAndPrune(_ context.Context, _ core.GitDeleteRefIntent, authorize func() (bool, error)) (bool, error) {
	return authorize()
}

func (g *fakeGit) DeleteWorkspace(_ context.Context, _ core.GitDeleteWorkspaceIntent, authorize func() (bool, error)) (bool, error) {
	return authorize()
}

func (g *fakeGit) Prune(context.Context, string, string) error { return nil }

func countEvent(events []core.Event, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
