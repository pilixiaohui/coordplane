package core_test

import (
	"context"
	"testing"

	"coordplane/internal/core"
)

func TestCT08AcceptIntentFreezesDecisionWithoutAdvancingGit(t *testing.T) {
	h := newHarness(t)
	integrator := h.addAgent(t, "accept-integrator")
	worker := h.addAgent(t, "accept-worker")
	project := h.addProject(t, "accept-project", integrator.ID)
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "submitted result", RequestID: "accept-task",
	})
	requireNoError(t, err)
	task = makeSubmittedTask(t, h, task, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	gitCalls := h.git.initializeCallCount()

	accepted, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, RequestID: "accept-intent",
	})
	requireNoError(t, err)
	if accepted.Status != core.TaskSubmitted || accepted.AcceptedByKind != "boss" ||
		accepted.AcceptedIntegrationAgentID != integrator.ID || accepted.PendingAction != "advance" ||
		accepted.PendingActionID == "" || accepted.PendingActionVersion != accepted.Version ||
		accepted.PendingExpectedSHA != project.CanonicalSHA || accepted.PendingTargetSHA != task.HeadSHA ||
		accepted.PendingActionRunID != task.HeadRunID {
		t.Fatalf("accept intent = %#v", accepted)
	}
	if h.git.initializeCallCount() != gitCalls {
		t.Fatal("accept intent performed a Git side effect")
	}
	if _, err := h.service.ArchiveAgent(context.Background(), integrator.ID, "archive-accepted-integrator"); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("accepted integration Agent archive error = %v", err)
	}
	beforeReplay := durableSignature(t, h.database, project.ID)
	replayed, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, RequestID: "accept-intent",
	})
	if err != nil || replayed.ID != accepted.ID || replayed.PendingActionID != accepted.PendingActionID {
		t.Fatalf("accept replay = %#v err=%v", replayed, err)
	}
	h.requireDurableSignature(t, project.ID, beforeReplay)
}

func TestCT08ReadableAssignmentDoesNotAuthorizeAgentAcceptOrRework(t *testing.T) {
	h := newHarness(t)
	integrator := h.addAgent(t, "scope-integrator")
	worker := h.addAgent(t, "scope-worker")
	project := h.addProject(t, "scope-project", integrator.ID)
	controller, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "controller", Priority: 100, RequestID: "scope-controller",
	})
	requireNoError(t, err)
	target, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "readable assignment", Priority: 10, RequestID: "scope-target",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != controller.ID {
		t.Fatalf("controller claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "scope-active"); err != nil {
		t.Fatal(err)
	}
	target = makeSubmittedTask(t, h, target, "dddddddddddddddddddddddddddddddddddddddd")
	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		Token: claim.Token, TaskID: target.ID, RequestID: "scope-accept",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("assigned task accept error = %v", err)
	}
	if _, err := h.service.ReworkTask(context.Background(), core.TaskActionInput{
		Token: claim.Token, TaskID: target.ID, RequestID: "scope-rework",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("assigned task rework error = %v", err)
	}
	h.requireDurableSignature(t, project.ID, before)
}

func makeSubmittedTask(t *testing.T, h *harness, task core.Task, head string) core.Task {
	t.Helper()
	err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		persisted, err := tx.Task(task.ID)
		if err != nil {
			return err
		}
		expectedVersion, expectedStatus := persisted.Version, persisted.Status
		persisted.Status = core.TaskSubmitted
		persisted.HeadSHA = head
		persisted.HeadRunID = "run-captured-" + task.ID
		persisted.TaskRef = "refs/coordplane/tasks/" + task.ID + "/runs/fixture"
		persisted.SubmittedAt = h.clock.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
		persisted.Version++
		persisted.UpdatedAt = persisted.SubmittedAt
		if err := tx.UpdateTask(persisted, expectedVersion, expectedStatus); err != nil {
			return err
		}
		task = persisted
		return nil
	})
	requireNoError(t, err)
	return task
}
