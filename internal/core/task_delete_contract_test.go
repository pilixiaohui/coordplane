package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestDeleteTaskRemovesTaskAndAllChildren(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "delete-agent")
	project := h.addProject(t, "delete-project", "")
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || ok {
		t.Fatalf("expected no claim before task, ok=%t err=%v", ok, err)
	}
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "to be deleted", RequestID: "delete-task",
	})
	requireNoError(t, err)
	claim, ok, err = h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "delete-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
		Token: claim.Token, RecipientKind: "boss", Body: "delete me too",
		RequestID: "delete-message",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, Reason: "cancel before delete", RequestID: "delete-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, claim.Run.ID, "delete-terminal")

	if err := h.service.DeleteTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, Reason: "cleanup", RequestID: "delete",
	}); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	if _, err := h.database.Task(context.Background(), task.ID); !core.IsCode(err, core.CodeNotFound) {
		t.Fatalf("task still readable after delete: err=%v", err)
	}
	if _, err := h.database.Run(context.Background(), claim.Run.ID); !core.IsCode(err, core.CodeNotFound) {
		t.Fatalf("run still readable after delete: err=%v", err)
	}
	messages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: task.ID})
	requireNoError(t, err)
	if len(messages.Items) != 0 {
		t.Fatalf("messages survived task delete: %#v", messages.Items)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, EntityType: "task", EntityID: task.ID})
	requireNoError(t, err)
	if len(events) != 1 || events[0].Kind != "task.deleted" {
		t.Fatalf("task audit events after delete = %#v", events)
	}

	// Replaying the same delete request is a success (dedupe) even though the
	// task is gone, so operator retries never surface spurious errors.
	if err := h.service.DeleteTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, Reason: "cleanup", RequestID: "delete",
	}); err != nil {
		t.Fatalf("DeleteTask replay: %v", err)
	}
}

func TestDeleteTaskRejectsTaskThatIsNotClosed(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "delete-open-agent")
	project := h.addProject(t, "delete-open-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "still open", RequestID: "delete-open-task",
	})
	requireNoError(t, err)
	err = h.service.DeleteTask(context.Background(), core.TaskActionInput{TaskID: task.ID, RequestID: "delete-open"})
	if !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("DeleteTask on queued task error = %v, want INVALID_STATE", err)
	}
	if _, err := h.database.Task(context.Background(), task.ID); err != nil {
		t.Fatalf("queued task must survive rejected delete: %v", err)
	}
}

func TestDeleteTaskRejectsClosedTaskWithLiveCurrentRun(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "delete-live-agent")
	project := h.addProject(t, "delete-live-project", "")
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || ok {
		t.Fatalf("unexpected pre-task claim: ok=%t err=%v", ok, err)
	}
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "live after cancel", RequestID: "delete-live-task",
	})
	requireNoError(t, err)
	claim, ok, err = h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "delete-live-active"); err != nil {
		t.Fatal(err)
	}
	// Cancelling with a live run keeps current_run_id set until the runtime
	// terminates; DeleteTask must refuse while the run is still live.
	if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, Reason: "cancel but run live", RequestID: "delete-live-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	err = h.service.DeleteTask(context.Background(), core.TaskActionInput{TaskID: task.ID, RequestID: "delete-live"})
	if !core.IsCode(err, core.CodeActionInProgress) {
		t.Fatalf("DeleteTask with live current run error = %v, want ACTION_IN_PROGRESS", err)
	}
	if _, err := h.database.Task(context.Background(), task.ID); err != nil {
		t.Fatalf("task must survive rejected delete with live run: %v", err)
	}
}

func TestDeleteTaskUnknownTaskIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.addProject(t, "delete-missing-project", "")
	err := h.service.DeleteTask(context.Background(), core.TaskActionInput{TaskID: "does-not-exist", RequestID: "delete-missing"})
	if !core.IsCode(err, core.CodeNotFound) {
		t.Fatalf("DeleteTask unknown task error = %v, want NOT_FOUND", err)
	}
}
