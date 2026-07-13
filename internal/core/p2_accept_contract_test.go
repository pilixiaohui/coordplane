package core_test

import (
	"context"
	"sync"
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
	if err != nil {
		t.Fatal(err)
	}
	task = makeSubmittedTask(t, h, task, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	gitCalls := h.git.initializeCallCount()

	accepted, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, RequestID: "accept-intent",
	})
	if err != nil {
		t.Fatal(err)
	}
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
	beforeReplay := h.durableSignature(t, project.ID)
	replayed, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
		TaskID: task.ID, RequestID: "accept-intent",
	})
	if err != nil || replayed.ID != accepted.ID || replayed.PendingActionID != accepted.PendingActionID {
		t.Fatalf("accept replay = %#v err=%v", replayed, err)
	}
	if after := h.durableSignature(t, project.ID); after != beforeReplay {
		t.Fatal("accept replay changed durable state")
	}
}

func TestCT08AcceptRacesConvergeWithReworkAndCancel(t *testing.T) {
	for _, competitor := range []struct {
		name string
		run  func(*harness, string) error
	}{
		{name: "rework", run: func(h *harness, taskID string) error {
			_, err := h.service.ReworkTask(context.Background(), core.TaskActionInput{
				TaskID: taskID, Reason: "needs changes", RequestID: "race-rework",
			})
			return err
		}},
		{name: "cancel", run: func(h *harness, taskID string) error {
			_, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
				TaskID: taskID, Reason: "discard result", RequestID: "race-cancel",
			})
			return err
		}},
	} {
		t.Run(competitor.name, func(t *testing.T) {
			h := newHarness(t)
			integrator := h.addAgent(t, "race-integrator-"+competitor.name)
			worker := h.addAgent(t, "race-worker-"+competitor.name)
			project := h.addProject(t, "race-project-"+competitor.name, integrator.ID)
			task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
				Title: "race result", RequestID: "race-task-" + competitor.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			task = makeSubmittedTask(t, h, task, "cccccccccccccccccccccccccccccccccccccccc")

			type raceResult struct {
				operation string
				err       error
			}
			start := make(chan struct{})
			results := make(chan raceResult, 2)
			var ready sync.WaitGroup
			ready.Add(2)
			go func() {
				ready.Done()
				<-start
				_, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
					TaskID: task.ID, RequestID: "race-accept-" + competitor.name,
				})
				results <- raceResult{operation: "accept", err: err}
			}()
			go func() {
				ready.Done()
				<-start
				results <- raceResult{operation: competitor.name, err: competitor.run(h, task.ID)}
			}()
			ready.Wait()
			close(start)
			first, second := <-results, <-results
			byOperation := map[string]error{first.operation: first.err, second.operation: second.err}
			successes := 0
			for _, result := range []error{first.err, second.err} {
				if result == nil {
					successes++
					continue
				}
				if !core.IsCode(result, core.CodeActionInProgress) && !core.IsCode(result, core.CodeInvalidState) {
					t.Fatalf("race error = %v", result)
				}
			}
			if successes != 1 {
				t.Fatalf("race successes = %d, results = [%+v, %+v]", successes, first, second)
			}
			persisted, err := h.database.Task(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.PendingAction == "advance" {
				if persisted.Status != core.TaskSubmitted || persisted.AcceptedIntegrationAgentID != integrator.ID {
					t.Fatalf("accept winner projection = %#v", persisted)
				}
				if byOperation["accept"] != nil || !core.IsCode(byOperation[competitor.name], core.CodeActionInProgress) {
					t.Fatalf("accept winner errors = %#v", byOperation)
				}
			} else if competitor.name == "rework" && persisted.Status != core.TaskQueued {
				t.Fatalf("rework winner projection = %#v", persisted)
			} else if competitor.name == "cancel" && persisted.Status != core.TaskCancelled {
				t.Fatalf("cancel winner projection = %#v", persisted)
			} else if byOperation[competitor.name] != nil || !core.IsCode(byOperation["accept"], core.CodeInvalidState) {
				t.Fatalf("%s winner errors = %#v", competitor.name, byOperation)
			}
		})
	}
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
	if err != nil {
		t.Fatal(err)
	}
	target, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
		Title: "readable assignment", Priority: 10, RequestID: "scope-target",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != controller.ID {
		t.Fatalf("controller claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "scope-active"); err != nil {
		t.Fatal(err)
	}
	target = makeSubmittedTask(t, h, target, "dddddddddddddddddddddddddddddddddddddddd")
	before := h.durableSignature(t, project.ID)
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
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("scope-denied accept/rework changed durable state")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return task
}
