package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"coordplane/internal/core"
)

func TestCT02EntityCASRejectsStaleVersionAndStateWithoutSideEffects(t *testing.T) {
	type casCase struct {
		name       string
		state      string
		wrongState func(core.Transaction, casFixture) error
		wrongVer   func(core.Transaction, casFixture) error
	}
	cases := []casCase{
		{
			name: "project", state: string(core.ProjectActive),
			wrongState: func(tx core.Transaction, fixture casFixture) error {
				project := fixture.project
				project.Version++
				return tx.UpdateProject(project, fixture.project.Version, core.ProjectError)
			},
			wrongVer: func(tx core.Transaction, fixture casFixture) error {
				project := fixture.project
				project.Version++
				return tx.UpdateProject(project, fixture.project.Version-1, fixture.project.Status)
			},
		},
		{
			name: "agent", state: string(core.AgentActive),
			wrongState: func(tx core.Transaction, fixture casFixture) error {
				agent := fixture.agent
				agent.Version++
				return tx.UpdateAgent(agent, fixture.agent.Version, core.AgentPaused)
			},
			wrongVer: func(tx core.Transaction, fixture casFixture) error {
				agent := fixture.agent
				agent.Version++
				return tx.UpdateAgent(agent, fixture.agent.Version-1, fixture.agent.Status)
			},
		},
		{
			name: "task", state: string(core.TaskQueued),
			wrongState: func(tx core.Transaction, fixture casFixture) error {
				task := fixture.task
				task.Version++
				return tx.UpdateTask(task, fixture.task.Version, core.TaskRunning)
			},
			wrongVer: func(tx core.Transaction, fixture casFixture) error {
				task := fixture.task
				task.Version++
				return tx.UpdateTask(task, fixture.task.Version-1, fixture.task.Status)
			},
		},
		{
			name: "run", state: string(core.RunStarting),
			wrongState: func(tx core.Transaction, fixture casFixture) error {
				run := fixture.run
				run.Version++
				return tx.UpdateRun(run, fixture.run.Version, core.RunActive)
			},
			wrongVer: func(tx core.Transaction, fixture casFixture) error {
				run := fixture.run
				run.Version++
				return tx.UpdateRun(run, fixture.run.Version-1, fixture.run.State)
			},
		},
		{
			name: "message", state: string(core.MessagePending),
			wrongState: func(tx core.Transaction, fixture casFixture) error {
				message := fixture.message
				message.Version++
				return tx.UpdateMessage(message, fixture.message.Version, core.MessageDelivered)
			},
			wrongVer: func(tx core.Transaction, fixture casFixture) error {
				message := fixture.message
				message.Version++
				return tx.UpdateMessage(message, fixture.message.Version-1, fixture.message.State)
			},
		},
	}

	for _, test := range cases {
		for _, stale := range []struct {
			name  string
			apply func(core.Transaction, casFixture) error
		}{{"state", test.wrongState}, {"version", test.wrongVer}} {
			t.Run(test.name+"/stale_"+stale.name, func(t *testing.T) {
				ctx := context.Background()
				database, err := Open(ctx, filepath.Join(t.TempDir(), "cas.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				fixture := insertCASFixture(t, database)
				before, err := database.Snapshot(ctx, fixture.project.ID)
				if err != nil {
					t.Fatal(err)
				}

				err = database.Transact(ctx, func(tx core.Transaction) error {
					return stale.apply(tx, fixture)
				})
				if !core.IsCode(err, core.CodeVersionConflict) {
					t.Fatalf("stale CAS error = %v, want %s", err, core.CodeVersionConflict)
				}
				conflict := core.AsError(err)
				if conflict.State != test.state || conflict.Version != 1 {
					t.Fatalf("stale CAS current fact = state %q version %d, want %q/1", conflict.State, conflict.Version, test.state)
				}
				after, err := database.Snapshot(ctx, fixture.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("stale CAS changed durable rows\nbefore=%#v\nafter=%#v", before, after)
				}
				events, err := database.Events(ctx, core.EventFilter{ProjectID: fixture.project.ID})
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 0 {
					t.Fatalf("stale CAS wrote events: %#v", events)
				}
			})
		}
	}
}

type casFixture struct {
	project core.Project
	agent   core.Agent
	task    core.Task
	run     core.Run
	message core.Message
}

func insertCASFixture(t *testing.T, database *Store) casFixture {
	t.Helper()
	const now = "2026-07-12T00:00:00.000000000Z"
	fixture := casFixture{
		project: core.Project{
			ID: "prj_cas", Name: "CAS", Source: "/source", SourceRef: "refs/heads/main",
			InitialSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ControlRepoPath: "/control.git",
			CanonicalRef: "refs/heads/main", CanonicalSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Status: core.ProjectActive, Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		agent: core.Agent{
			ID: "agt_cas", DisplayName: "CAS", AdapterID: "one-shot", Image: "agent:latest",
			InstructionsFile: "/instructions", Status: core.AgentActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		},
		task: core.Task{
			ID: "tsk_cas", ProjectID: "prj_cas", Kind: core.TaskWork, CreatedByKind: "boss",
			AssigneeAgentID: "agt_cas", Title: "CAS", Description: "CAS", Status: core.TaskQueued,
			Generation: 1, NextRunAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		run: core.Run{
			ID: "run_cas", ProjectID: "prj_cas", TaskID: "tsk_cas", AgentID: "agt_cas",
			Generation: 1, AdapterID: "one-shot", Image: "agent:latest", State: core.RunStarting,
			TokenHash: "token-cas", CleanupState: "not_needed", LaunchPhase: "intent",
			ContainerName: "coordplane-run-cas", LaunchMode: "start", Version: 1, CreatedAt: now,
		},
		message: core.Message{
			ID: "msg_cas", ProjectID: "prj_cas", TaskID: "tsk_cas", SenderKind: "boss",
			RecipientKind: "agent", RecipientID: "agt_cas", Body: "CAS", Wake: true,
			State: core.MessagePending, MaxDeliveries: 3, NextDeliveryAt: now,
			IdempotencyKey: "cas", Version: 1, CreatedAt: now,
		},
	}
	if err := database.Transact(context.Background(), func(tx core.Transaction) error {
		if err := tx.InsertProject(fixture.project); err != nil {
			return err
		}
		if err := tx.InsertAgent(fixture.agent); err != nil {
			return err
		}
		if err := tx.InsertTask(fixture.task); err != nil {
			return err
		}
		if err := tx.InsertRun(fixture.run); err != nil {
			return err
		}
		return tx.InsertMessage(fixture.message)
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}
