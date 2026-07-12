package core_test

import (
	"context"
	"sync"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

// INV-04: independent scheduler connections must converge on one Run claim.
func TestCT02ConcurrentClaimAcrossSQLiteConnectionsCreatesOneRun(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "cross-connection-agent")
	project := h.addProject(t, "cross-connection-project", "")
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, Kind: core.TaskWork, AssigneeAgentID: agent.ID,
		Title: "claim once", RequestID: "cross-connection-task",
	})
	if err != nil {
		t.Fatal(err)
	}

	secondStore, err := store.Open(context.Background(), h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	secondService, err := core.NewService(secondStore, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		claim core.Claim
		ok    bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, service := range []*core.Service{h.service, secondService} {
		go func(service *core.Service) {
			ready.Done()
			<-start
			claim, ok, err := service.ClaimNext(context.Background(), project.ID)
			results <- claimResult{claim: claim, ok: ok, err: err}
		}(service)
	}
	ready.Wait()
	close(start)

	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("claim contender returned an error: %v", result.err)
		}
		if result.ok {
			winners++
			if result.claim.Task.ID != task.ID || result.claim.Run.TaskID != task.ID {
				t.Fatalf("claim winner targeted the wrong Task: %#v", result.claim)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", winners)
	}

	snapshot, err := h.database.Snapshot(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimedTask := taskWithID(t, snapshot, task.ID)
	if len(snapshot.Runs) != 1 || claimedTask.Generation != 1 || claimedTask.CurrentRunID != snapshot.Runs[0].ID {
		t.Fatalf("durable claim did not converge: Task=%#v Runs=%#v", claimedTask, snapshot.Runs)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, "task.claimed") != 1 || countEvent(events, "run.created") != 1 {
		t.Fatalf("claim Events were duplicated: %#v", events)
	}
}
