package core_test

import (
	"context"
	"testing"
	"time"

	"coordplane/internal/core"
)

// RP-07: a human Task converges only through `task complete`. evidence_type=
// human_confirm with an empty head_sha; the daemon never claims it; submit
// (code capture) is unavailable; agent tasks, repeats, and missing
// task.complete are all rejected.
func TestRP07HumanTaskCompleteConvergesWithHumanConfirmEvidence(t *testing.T) {
	h := newHarness(t)
	project := h.addProject(t, "rp07-project", "")
	agent := h.addAgent(t, "rp07-agent")

	agentTask, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Title: "agent work", RequestID: "rp07-agent",
	})
	requireNoError(t, err)
	if agentTask.Status != core.TaskQueued || agentTask.WaitReason != "" {
		t.Fatalf("agent task initial state = %#v, want queued without wait_reason", agentTask)
	}
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeParticipantID: core.DefaultHumanParticipantID,
		Title: "human review", RequestID: "rp07-create",
	})
	requireNoError(t, err)
	if task.AssigneeAgentID != "" || task.AssigneeParticipantID != core.DefaultHumanParticipantID {
		t.Fatalf("human assignee = agent %q / participant %q", task.AssigneeAgentID, task.AssigneeParticipantID)
	}
	if task.Status != core.TaskWaiting || task.WaitReason != "human_assigned" || task.CurrentRunID != "" {
		t.Fatalf("human task initial state = %#v, want waiting with wait_reason=human_assigned", task)
	}
	if _, err := h.service.WakeTask(context.Background(), core.TaskActionInput{
		TaskID: task.ID, RequestID: "rp07-wake-human",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("wake on human task error = %v, want INVALID_STATE", err)
	}
	if _, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: agentTask.ID, Summary: "nope", RequestID: "rp07-agent-complete",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("complete on agent task error = %v", err)
	}
	queuedHuman, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeParticipantID: core.DefaultHumanParticipantID,
		Title: "legacy queued human", RequestID: "rp07-queued-human",
	})
	requireNoError(t, err)
	if queuedHuman.Status != core.TaskWaiting {
		t.Fatalf("queued-human fixture created as %s, want waiting", queuedHuman.Status)
	}
	if err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		persisted, err := tx.Task(queuedHuman.ID)
		if err != nil {
			return err
		}
		expectedVersion, expectedStatus := persisted.Version, persisted.Status
		persisted.Status = core.TaskQueued
		persisted.WaitReason = ""
		persisted.Version++
		persisted.UpdatedAt = time.Date(2026, 7, 12, 1, 2, 4, 0, time.UTC).Format(time.RFC3339Nano)
		return tx.UpdateTask(persisted, expectedVersion, expectedStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: queuedHuman.ID, Summary: "must not complete from queued", RequestID: "rp07-queued-complete",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("complete queued human task error = %v, want INVALID_STATE", err)
	}
	if claim, ok, err := h.service.ClaimNext(context.Background(), project.ID); err != nil || (ok && claim.Task.ID == task.ID) {
		t.Fatalf("daemon claimed human task: %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Outcome: core.OutcomeSubmit, Summary: "captured?", ExpectedHead: "deadbeef",
	}); !core.IsCode(err, core.CodeScopeDenied) && !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("submit on human task error = %v", err)
	}
	completed, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: task.ID, Summary: "reviewed and confirmed", RequestID: "rp07-complete",
	})
	requireNoError(t, err)
	if completed.Status != core.TaskCompleted || completed.EvidenceType != string(core.EvidenceHumanConfirm) ||
		completed.HeadSHA != "" || completed.HeadRunID != "" || completed.TaskRef != "" ||
		completed.WaitReason != "" || completed.ClosedAt == "" ||
		completed.ResultSummary != "reviewed and confirmed" {
		t.Fatalf("completed = %#v", completed)
	}
	detail, err := h.service.Task(context.Background(), task.ID)
	requireNoError(t, err)
	if detail.Task.EvidenceType != string(core.EvidenceHumanConfirm) || detail.Task.ClosedAt == "" ||
		detail.Task.WaitReason != "" || detail.Task.HeadSHA != "" {
		t.Fatalf("task detail after complete = %#v", detail.Task)
	}
	if _, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: task.ID, Summary: "again", RequestID: "rp07-repeat",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("repeat complete error = %v", err)
	}
	// Without task.complete the operation is SCOPE_DENIED. The owner's global
	// binding carries every capability, so both scopes are swapped while
	// keeping participant.manage (last-admin guard permits the swap).
	denied, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp07-no-complete", Description: "management caps, no task.complete",
		Capabilities: []string{"participant.manage", "role.bind"},
		RequestID:    "rp07-role",
	})
	requireNoError(t, err)
	owner, err := h.service.Role(context.Background(), core.DefaultOwnerRoleID)
	requireNoError(t, err)
	humanTask, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeParticipantID: core.DefaultHumanParticipantID,
		Title: "human work", RequestID: "rp07-human-task",
	})
	requireNoError(t, err)
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: denied.ID, RequestID: "rp07-bind-global",
	}); err != nil {
		t.Fatal(err)
	}
	for _, unbind := range []core.BindRoleInput{
		{ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: owner.ID, RequestID: "rp07-unbind-global"},
		{ParticipantID: core.DefaultHumanParticipantID, ProjectID: project.ID, RoleID: owner.ID, RequestID: "rp07-unbind"},
	} {
		if err := h.service.UnbindParticipantRole(context.Background(), unbind); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: project.ID, RoleID: denied.ID, RequestID: "rp07-bind",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CompleteTask(context.Background(), core.CompleteTaskInput{
		TaskID: humanTask.ID, Summary: "blocked", RequestID: "rp07-denied",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("complete without task.complete error = %v", err)
	}
}
