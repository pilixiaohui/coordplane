package core_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func TestCT04RequestedOutcomeConvergesFromTrustedTerminalFactExactlyOnce(t *testing.T) {
	tests := []struct {
		name              string
		outcome           string
		reason            string
		summary           string
		expectedHead      string
		wantStatus        core.TaskStatus
		wantCurrentRun    bool
		wantTaskEvent     string
		wantCaptureIntent bool
	}{
		{name: "wait", outcome: "wait", reason: "awaiting review", wantStatus: core.TaskWaiting, wantTaskEvent: "task.waiting"},
		{name: "fail", outcome: "fail", reason: "tests failed", wantStatus: core.TaskFailed, wantTaskEvent: "task.failed"},
		{
			name: "submit", outcome: "submit", summary: "ready", expectedHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantStatus: core.TaskFinishing, wantCurrentRun: true, wantCaptureIntent: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			project, claim := createActiveWorkRun(t, h, "outcome-"+test.name, 0)
			input := core.OutcomeInput{
				Token: claim.Token, Outcome: test.outcome, Reason: test.reason,
				Summary: test.summary, ExpectedHead: test.expectedHead, RequestID: "request-" + test.name,
			}
			requested, err := h.service.RequestOutcome(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if requested.Status != core.TaskFinishing || requested.CurrentRunID != claim.Run.ID {
				t.Fatalf("requested task = %#v", requested)
			}
			if test.wantCaptureIntent {
				requestedSnapshot, err := h.database.Snapshot(context.Background(), project.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertCaptureIntent(t, requested, runWithID(t, requestedSnapshot, claim.Run.ID))
			}

			exitCode := 0
			terminalInput := core.TerminalRunInput{
				RunID: claim.Run.ID, State: core.RunExited, ExitCode: &exitCode,
				Reason: "process exited", RequestID: "terminal-" + test.name,
			}
			if _, err := h.service.RecordRunTerminal(context.Background(), terminalInput); err != nil {
				t.Fatal(err)
			}
			snapshot, err := h.database.Snapshot(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotTask := taskWithID(t, snapshot, requested.ID)
			gotRun := runWithID(t, snapshot, claim.Run.ID)
			if gotTask.Status != test.wantStatus {
				t.Fatalf("task status = %s, want %s; task=%#v", gotTask.Status, test.wantStatus, gotTask)
			}
			if (gotTask.CurrentRunID != "") != test.wantCurrentRun {
				t.Fatalf("current_run_id = %q, want retained=%v", gotTask.CurrentRunID, test.wantCurrentRun)
			}
			if test.wantCurrentRun && gotTask.CurrentRunID != claim.Run.ID {
				t.Fatalf("current_run_id = %q, want %q", gotTask.CurrentRunID, claim.Run.ID)
			}
			if gotRun.State != core.RunExited || gotRun.TokenRevokedAt == "" || gotRun.EndedAt == "" {
				t.Fatalf("terminal run = %#v", gotRun)
			}
			if test.outcome == "wait" && gotTask.WaitReason != test.reason {
				t.Fatalf("wait reason = %q", gotTask.WaitReason)
			}
			if test.outcome == "fail" && gotTask.FailureReason != test.reason {
				t.Fatalf("failure reason = %q", gotTask.FailureReason)
			}
			if test.wantCaptureIntent {
				assertCaptureIntent(t, gotTask, gotRun)
				if gotTask.HeadSHA != "" || gotTask.TaskRef != "" || gotTask.SubmittedAt != "" {
					t.Fatalf("submit terminal fact fabricated capture success: %#v", gotTask)
				}
			}

			events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
			if err != nil {
				t.Fatal(err)
			}
			if countEvent(events, "run.exited") != 1 {
				t.Fatalf("run.exited events = %d, want 1", countEvent(events, "run.exited"))
			}
			if test.wantTaskEvent != "" && countEvent(events, test.wantTaskEvent) != 1 {
				t.Fatalf("%s events = %d, want 1", test.wantTaskEvent, countEvent(events, test.wantTaskEvent))
			}
			if test.wantCaptureIntent {
				captureEvent := oneEvent(t, events, "git.task_ref_capture_requested")
				if captureEvent.OperationID == "" || captureEvent.OperationID != gotTask.PendingActionID {
					t.Fatalf("capture event/task operation mismatch: event=%#v task=%#v", captureEvent, gotTask)
				}
			}

			beforeReplay := h.durableSignature(t, project.ID)
			if _, err := h.service.RecordRunTerminal(context.Background(), terminalInput); err != nil {
				t.Fatalf("terminal replay: %v", err)
			}
			if afterReplay := h.durableSignature(t, project.ID); afterReplay != beforeReplay {
				t.Fatalf("terminal replay changed durable state\nbefore=%s\nafter=%s", beforeReplay, afterReplay)
			}
			if _, err := h.service.RecordRunTerminal(context.Background(), core.TerminalRunInput{
				RunID: claim.Run.ID, State: core.RunInterrupted, Reason: "conflicting fact",
				RequestID: "conflicting-terminal-" + test.name,
			}); !core.IsCode(err, core.CodeInvalidState) {
				t.Fatalf("conflicting terminal error = %v, want %s", err, core.CodeInvalidState)
			}
			if afterConflict := h.durableSignature(t, project.ID); afterConflict != beforeReplay {
				t.Fatalf("conflicting terminal changed durable state\nbefore=%s\nafter=%s", beforeReplay, afterConflict)
			}
			changedExit := 1
			if _, err := h.service.RecordRunTerminal(context.Background(), core.TerminalRunInput{
				RunID: claim.Run.ID, State: core.RunExited, ExitCode: &changedExit,
				Reason: "different process exit", RequestID: "changed-terminal-" + test.name,
			}); !core.IsCode(err, core.CodeInvalidState) {
				t.Fatalf("changed same-state terminal error = %v, want %s", err, core.CodeInvalidState)
			}
			if afterChangedFact := h.durableSignature(t, project.ID); afterChangedFact != beforeReplay {
				t.Fatalf("changed same-state terminal fact changed durable state\nbefore=%s\nafter=%s", beforeReplay, afterChangedFact)
			}
		})
	}
}

func TestCT04OutcomeAndTerminalRecoveryUsesOnlyDurableSQLiteState(t *testing.T) {
	h := newHarness(t)
	project, claim := createActiveWorkRun(t, h, "restart-wait", 0)
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: "wait", Reason: "resume after restart", RequestID: "restart-wait",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered, err := core.NewService(reopened, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := recovered.RecordRunTerminal(context.Background(), core.TerminalRunInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: &exitCode,
		Reason: "observed after restart", RequestID: "restart-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := taskWithID(t, snapshot, claim.Task.ID)
	run := runWithID(t, snapshot, claim.Run.ID)
	if task.Status != core.TaskWaiting || task.CurrentRunID != "" || run.State != core.RunExited {
		t.Fatalf("recovered projection mismatch: task=%#v run=%#v", task, run)
	}
}

func TestTerminalRunRedeliversUnacknowledgedMessageInTheSameTransaction(t *testing.T) {
	h := newHarness(t)
	project, claim := createActiveWorkRun(t, h, "redelivery", 0)
	now := h.clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	delivered := core.Message{
		ID: "msg_terminal_redelivery", ProjectID: project.ID, TaskID: claim.Task.ID,
		SenderKind: "boss", RecipientKind: "agent", RecipientID: claim.Run.AgentID,
		Body: "must survive the Run", Wake: true, State: core.MessageDelivered,
		DeliveredRunID: claim.Run.ID, DeliveryCount: 1, MaxDeliveries: 3,
		NextDeliveryAt: now, IdempotencyKey: "terminal-redelivery", Version: 1,
		CreatedAt: now, DeliveredAt: now,
	}
	pending := core.Message{
		ID: "msg_already_pending", ProjectID: project.ID, TaskID: claim.Task.ID,
		SenderKind: "boss", RecipientKind: "agent", RecipientID: claim.Run.AgentID,
		Body: "already pending", State: core.MessagePending, MaxDeliveries: 3,
		NextDeliveryAt: now, IdempotencyKey: "already-pending", Version: 1, CreatedAt: now,
	}
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		if err := tx.InsertMessage(delivered); err != nil {
			return err
		}
		return tx.InsertMessage(pending)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: "wait", Reason: "waiting after input", RequestID: "redelivery-wait",
	}); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := h.service.RecordRunTerminal(context.Background(), core.TerminalRunInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: &exitCode,
		Reason: "process exited", RequestID: "redelivery-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := taskWithID(t, snapshot, claim.Task.ID)
	if task.Status != core.TaskQueued || task.CurrentRunID != "" {
		t.Fatalf("pending wake did not queue the task after terminal: %#v", task)
	}
	persisted := messageWithID(t, snapshot, delivered.ID)
	if persisted.ID == "" || persisted.State != core.MessagePending || persisted.DeliveredRunID != "" ||
		persisted.DeliveredAt != "" || persisted.Version != 2 {
		t.Fatalf("terminal message projection = %#v", persisted)
	}
	untouched := messageWithID(t, snapshot, pending.ID)
	if untouched.State != core.MessagePending || untouched.Version != pending.Version ||
		untouched.LastDeliveryError != "" || untouched.DeliveredRunID != "" {
		t.Fatalf("existing pending message changed during redelivery: %#v", untouched)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, "message.redelivered") != 1 || countEvent(events, "message.cancelled") != 0 ||
		countEvent(events, "task.requeued") != 1 || countEvent(events, "run.exited") != 1 {
		t.Fatalf("terminal redelivery events = %#v", events)
	}
}

func TestFailedTaskTerminalizationCancelsEveryUndeliverableAgentMessage(t *testing.T) {
	tests := []struct {
		name          string
		requestFail   bool
		terminalState core.RunState
		terminalEvent string
	}{
		{name: "agent fail", requestFail: true, terminalState: core.RunExited, terminalEvent: "run.exited"},
		{name: "runtime retries exhausted", terminalState: core.RunInterrupted, terminalEvent: "run.interrupted"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			project, claim := createActiveWorkRun(t, h, "cancel-messages-"+test.name, 0)
			now := h.clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
			messages := []core.Message{
				{
					ID: "msg_pending_agent", ProjectID: project.ID, TaskID: claim.Task.ID,
					SenderKind: "boss", RecipientKind: "agent", RecipientID: claim.Run.AgentID,
					Body: "not yet delivered", State: core.MessagePending, MaxDeliveries: 3,
					NextDeliveryAt: now, IdempotencyKey: "pending-agent", Version: 1, CreatedAt: now,
				},
				{
					ID: "msg_delivered_agent", ProjectID: project.ID, TaskID: claim.Task.ID,
					SenderKind: "boss", RecipientKind: "agent", RecipientID: claim.Run.AgentID,
					Body: "delivered but unacknowledged", State: core.MessageDelivered,
					DeliveredRunID: claim.Run.ID, DeliveryCount: 1, MaxDeliveries: 3,
					NextDeliveryAt: now, IdempotencyKey: "delivered-agent", Version: 1,
					CreatedAt: now, DeliveredAt: now,
				},
				{
					ID: "msg_pending_boss", ProjectID: project.ID, TaskID: claim.Task.ID,
					SenderKind: "agent", SenderID: claim.Run.AgentID, RecipientKind: "boss",
					Body: "Boss can still read this", State: core.MessagePending, MaxDeliveries: 1,
					NextDeliveryAt: now, IdempotencyKey: "pending-boss", Version: 1, CreatedAt: now,
				},
			}
			if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
				for _, message := range messages {
					if err := tx.InsertMessage(message); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if test.requestFail {
				if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
					Token: claim.Token, Outcome: "fail", Reason: "cannot continue", RequestID: "agent-fail",
				}); err != nil {
					t.Fatal(err)
				}
			}
			exitCode := 0
			terminal := core.TerminalRunInput{
				RunID: claim.Run.ID, State: test.terminalState, Reason: "process terminal",
				RequestID: "terminal-failure",
			}
			if test.terminalState == core.RunExited {
				terminal.ExitCode = &exitCode
			}
			if _, err := h.service.RecordRunTerminal(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}

			snapshot, err := h.database.Snapshot(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			task := taskWithID(t, snapshot, claim.Task.ID)
			if task.Status != core.TaskFailed || task.CurrentRunID != "" {
				t.Fatalf("failed task projection = %#v", task)
			}
			for _, id := range []string{"msg_pending_agent", "msg_delivered_agent"} {
				message := messageWithID(t, snapshot, id)
				if message.State != core.MessageCancelled || message.Version != 2 || message.LastDeliveryError != "TASK_FAILED" {
					t.Fatalf("undeliverable Agent message %s was stranded: %#v", id, message)
				}
			}
			bossMessage := messageWithID(t, snapshot, "msg_pending_boss")
			if bossMessage.State != core.MessagePending || bossMessage.Version != 1 || bossMessage.LastDeliveryError != "" {
				t.Fatalf("Boss-directed message changed during Task failure: %#v", bossMessage)
			}
			for _, message := range snapshot.Messages {
				if message.TaskID == task.ID && message.RecipientKind == "agent" &&
					(message.State == core.MessagePending || message.State == core.MessageDelivered) {
					t.Fatalf("failed Task retained an undeliverable Agent message: %#v", message)
				}
			}

			events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
			if err != nil {
				t.Fatal(err)
			}
			cancelledIDs := map[string]int{}
			for _, event := range events {
				if event.Kind == "message.cancelled" {
					cancelledIDs[event.EntityID]++
				}
			}
			if countEvent(events, "message.cancelled") != 2 || countEvent(events, "message.redelivered") != 0 ||
				countEvent(events, "task.failed") != 1 || countEvent(events, test.terminalEvent) != 1 ||
				cancelledIDs["msg_pending_agent"] != 1 || cancelledIDs["msg_delivered_agent"] != 1 {
				t.Fatalf("failed Task terminal events = %#v", events)
			}
		})
	}
}

func TestTrustedTerminalFactBoundsExternalErrorsWithoutBlockingConvergence(t *testing.T) {
	h := newHarness(t)
	project, claim := createActiveWorkRun(t, h, "bounded-terminal", 0)
	largeReason := strings.Repeat("\x01", core.MaximumEventPayloadBytes)
	largeError := strings.Repeat("\x02", core.MaximumEventPayloadBytes)
	input := core.TerminalRunInput{
		RunID: claim.Run.ID, State: core.RunInterrupted, Reason: largeReason,
		LastError: largeError, RequestID: "bounded-terminal-fact",
	}
	if _, err := h.service.RecordRunTerminal(context.Background(), input); err != nil {
		t.Fatalf("large trusted terminal fact did not converge: %v", err)
	}
	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := runWithID(t, snapshot, claim.Run.ID)
	task := taskWithID(t, snapshot, claim.Task.ID)
	if run.State != core.RunInterrupted || task.Status != core.TaskFailed {
		t.Fatalf("large terminal projection = run:%#v task:%#v", run, task)
	}
	if len(run.TerminalReason) > core.MaximumTerminalTextBytes || len(run.LastError) > core.MaximumTerminalTextBytes ||
		!strings.HasSuffix(run.TerminalReason, "...[truncated]") || !strings.HasSuffix(run.LastError, "...[truncated]") {
		t.Fatalf("terminal error bounds = reason:%d error:%d", len(run.TerminalReason), len(run.LastError))
	}
	beforeReplay := h.durableSignature(t, project.ID)
	if _, err := h.service.RecordRunTerminal(context.Background(), input); err != nil {
		t.Fatalf("bounded terminal replay: %v", err)
	}
	if afterReplay := h.durableSignature(t, project.ID); afterReplay != beforeReplay {
		t.Fatalf("bounded terminal replay changed state\nbefore=%s\nafter=%s", beforeReplay, afterReplay)
	}
}

func TestRuntimeRetryBoundaryIsIdenticalForStartingAndActiveInterruptions(t *testing.T) {
	t.Run("max retries zero fails first starting run", func(t *testing.T) {
		h := newHarness(t)
		project, claim := createClaimedWorkRun(t, h, "zero-starting", 0)
		if _, err := h.service.InterruptRun(context.Background(), claim.Run.ID, "start lost", "zero-starting-terminal"); err != nil {
			t.Fatal(err)
		}
		assertRetryProjection(t, h, project.ID, claim.Task.ID, claim.Run.ID, core.TaskFailed, 0)
	})

	t.Run("max retries zero fails first active run", func(t *testing.T) {
		h := newHarness(t)
		project, claim := createActiveWorkRun(t, h, "zero-active", 0)
		if _, err := h.service.InterruptRun(context.Background(), claim.Run.ID, "process lost", "zero-active-terminal"); err != nil {
			t.Fatal(err)
		}
		assertRetryProjection(t, h, project.ID, claim.Task.ID, claim.Run.ID, core.TaskFailed, 0)
	})

	t.Run("retry before boundary then fail on boundary", func(t *testing.T) {
		h := newHarness(t)
		project, first := createClaimedWorkRun(t, h, "one-retry", 1)
		if _, err := h.service.InterruptRun(context.Background(), first.Run.ID, "start lost", "first-terminal"); err != nil {
			t.Fatal(err)
		}
		assertRetryProjection(t, h, project.ID, first.Task.ID, first.Run.ID, core.TaskQueued, 1)
		h.clock.Advance(time.Second)

		second, ok, err := h.service.ClaimNext(context.Background(), project.ID)
		if err != nil || !ok {
			t.Fatalf("second claim: ok=%v err=%v", ok, err)
		}
		if _, err := h.service.ActivateRun(context.Background(), second.Run.ID, "second-active"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.InterruptRun(context.Background(), second.Run.ID, "process lost", "second-terminal"); err != nil {
			t.Fatal(err)
		}
		assertRetryProjection(t, h, project.ID, first.Task.ID, second.Run.ID, core.TaskFailed, 1)
		events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
		if err != nil {
			t.Fatal(err)
		}
		if countEvent(events, "task.requeued") != 1 || countEvent(events, "task.failed") != 1 || countEvent(events, "run.interrupted") != 2 {
			t.Fatalf("retry terminal events = %#v", events)
		}
	})
}

func createClaimedWorkRun(t *testing.T, h *harness, name string, maxRetries int) (core.Project, core.Claim) {
	t.Helper()
	agent := h.addAgent(t, name+"-agent")
	project := h.addProject(t, name+"-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: name, MaxRetries: maxRetries, RequestID: name + "-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claim.Task.ID != task.ID {
		t.Fatalf("claimed %s, want %s", claim.Task.ID, task.ID)
	}
	return project, claim
}

func createActiveWorkRun(t *testing.T, h *harness, name string, maxRetries int) (core.Project, core.Claim) {
	t.Helper()
	project, claim := createClaimedWorkRun(t, h, name, maxRetries)
	if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, name+"-active"); err != nil {
		t.Fatal(err)
	}
	return project, claim
}

func assertRetryProjection(t *testing.T, h *harness, projectID, taskID, runID string, status core.TaskStatus, retryCount int) {
	t.Helper()
	snapshot, err := h.database.Snapshot(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	task := taskWithID(t, snapshot, taskID)
	run := runWithID(t, snapshot, runID)
	if task.Status != status || task.RetryCount != retryCount || task.CurrentRunID != "" {
		t.Fatalf("retry task = %#v, want status=%s count=%d and no current run", task, status, retryCount)
	}
	if run.State != core.RunInterrupted || run.TokenRevokedAt == "" || run.EndedAt == "" {
		t.Fatalf("interrupted run = %#v", run)
	}
	if status == core.TaskQueued && task.NextRunAt <= run.EndedAt {
		t.Fatalf("retry has no backoff: next_run_at=%s ended_at=%s", task.NextRunAt, run.EndedAt)
	}
}

func assertCaptureIntent(t *testing.T, task core.Task, run core.Run) {
	t.Helper()
	if task.PendingAction != "capture" || task.PendingActionID == "" || task.PendingActionRunID != run.ID {
		t.Fatalf("capture identity fence is incomplete: task=%#v run=%#v", task, run)
	}
	if task.PendingActionVersion != task.Version || task.PendingExpectedSHA == "" || task.PendingExpectedSHA != run.ExpectedHead {
		t.Fatalf("capture version/head fence is incomplete: task=%#v run=%#v", task, run)
	}
	if task.PendingStartedAt == "" {
		t.Fatalf("capture start time is missing: %#v", task)
	}
}

func taskWithID(t *testing.T, snapshot core.Snapshot, id string) core.Task {
	t.Helper()
	for _, task := range snapshot.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %s not found in %#v", id, snapshot.Tasks)
	return core.Task{}
}

func runWithID(t *testing.T, snapshot core.Snapshot, id string) core.Run {
	t.Helper()
	for _, run := range snapshot.Runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("run %s not found in %#v", id, snapshot.Runs)
	return core.Run{}
}

func messageWithID(t *testing.T, snapshot core.Snapshot, id string) core.Message {
	t.Helper()
	for _, message := range snapshot.Messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("message %s not found in %#v", id, snapshot.Messages)
	return core.Message{}
}

func oneEvent(t *testing.T, events []core.Event, kind string) core.Event {
	t.Helper()
	var found []core.Event
	for _, event := range events {
		if event.Kind == kind {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s events = %#v, want exactly one", kind, found)
	}
	return found[0]
}

func TestCaptureIntentSurvivesStoreReopenWithoutASecondStateMachine(t *testing.T) {
	h := newHarness(t)
	project, claim := createActiveWorkRun(t, h, "capture-restart", 0)
	requested, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: "submit", Summary: "ready", ExpectedHead: h.git.sha,
		RequestID: "capture-restart-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if _, err := h.service.RecordRunTerminal(context.Background(), core.TerminalRunInput{
		RunID: claim.Run.ID, State: core.RunExited, ExitCode: &exitCode,
		Reason: "writer stopped", RequestID: "capture-restart-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), filepath.Clean(h.path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	snapshot, err := reopened.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := taskWithID(t, snapshot, requested.ID)
	run := runWithID(t, snapshot, claim.Run.ID)
	if task.Status != core.TaskFinishing || task.CurrentRunID != run.ID || run.State != core.RunExited {
		t.Fatalf("capture recovery projection = task:%#v run:%#v", task, run)
	}
	assertCaptureIntent(t, task, run)
}
