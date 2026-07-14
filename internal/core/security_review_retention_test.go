package core_test

import (
	"context"
	"testing"
	"time"

	"coordplane/internal/core"
)

func TestSecurityReviewDiscardCannotBypassRetention(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		h := newHarness(t)
		worker := h.addAgent(t, "security-retention-workspace-worker")
		project := h.addProject(t, "security-retention-workspace-project", "")
		task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
			ProjectID: project.ID, AssigneeAgentID: worker.ID, Kind: core.TaskWork,
			Title: "fresh workspace must remain retained", RequestID: "security-retention-workspace-create",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.CancelTask(context.Background(), core.TaskActionInput{
			TaskID: task.ID, Reason: "security review", RequestID: "security-retention-workspace-cancel",
		}); err != nil {
			t.Fatal(err)
		}

		service, err := core.NewService(h.database, h.git, core.ServiceOptions{
			Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
			CompletedWorkspaceRetention: 24 * time.Hour, TerminalTaskRefRetention: 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		preview, err := service.GCPreview(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var target core.GCWorkspaceTarget
		for _, candidate := range preview.Workspaces {
			if candidate.TaskID == task.ID {
				target = candidate
			}
		}
		if target.Fingerprint == "" || target.Eligible {
			t.Fatalf("invalid review setup: target = %#v", target)
		}
		result, err := service.GCDiscardWorkspace(context.Background(), core.GCDiscardWorkspaceInput{
			TaskID: task.ID, ExpectedFingerprint: target.Fingerprint, RequestID: "security-retention-workspace-discard",
		})
		if err == nil && result.Discarded {
			t.Fatalf("fresh retained workspace was discarded: preview=%#v result=%#v", target, result)
		}
	})

	t.Run("task_ref", func(t *testing.T) {
		h := newHarness(t)
		worker := h.addAgent(t, "security-retention-ref-worker")
		integrator := h.addAgent(t, "security-retention-ref-integrator")
		project := h.addProject(t, "security-retention-ref-project", integrator.ID)
		task := createAndSubmitCodeTask(t, h, project, worker, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "security-retention-ref")
		if err := h.service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.RequestAccept(context.Background(), core.AcceptInput{
			TaskID: task.ID, IntegrationAgentID: integrator.ID, RequestID: "security-retention-ref-accept",
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.service.ReconcileGit(context.Background()); err != nil {
			t.Fatal(err)
		}
		task, err := h.database.Task(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}

		service, err := core.NewService(h.database, h.git, core.ServiceOptions{
			Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"},
			CompletedWorkspaceRetention: 24 * time.Hour, TerminalTaskRefRetention: 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		preview, err := service.GCPreview(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var target core.GCTaskRefTarget
		for _, candidate := range preview.TaskRefs {
			if candidate.TaskID == task.ID {
				target = candidate
			}
		}
		if target.ActualSHA == "" || target.Eligible {
			t.Fatalf("invalid review setup: target = %#v", target)
		}
		result, err := service.GCDiscardTaskRef(context.Background(), core.GCDiscardTaskRefInput{
			TaskID: task.ID, RunID: task.HeadRunID, ExpectedSHA: task.HeadSHA,
			RequestID: "security-retention-ref-discard",
		})
		if err == nil && result.Discarded {
			t.Fatalf("fresh retained task ref was discarded: preview=%#v result=%#v", target, result)
		}
	})
}
