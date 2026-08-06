package core_test

import (
	"context"
	"strings"
	"testing"

	"coordplane/internal/core"
)

// TestP2GitSubmittedChildWakesParent is the core "submit must wake parent"
// regression: a child task that reaches submitted through the capture reconcile
// path must notify its waiting parent as an agent (parent -> queued) in the
// same transaction. Previously only the runtime/interrupt and accept paths
// notified, so a child finishing with submit left the parent waiting forever.
func TestP2GitSubmittedChildWakesParent(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "git-submit-parent")
	childAgent := h.addAgent(t, "git-submit-child")
	project := h.addProject(t, "git-submit-project", "")
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Kind: core.TaskWork,
		Title: "parent", Priority: 20, RequestID: "git-submit-parent-task",
	})
	requireNoError(t, err)
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), parentClaim.Run.ID, "git-submit-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "child",
		Priority: 10, RequestID: "git-submit-child-task",
	})
	requireNoError(t, err)
	waitAndTerminate(t, h, parentClaim, "git-submit-parent-wait")

	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim = %#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), childClaim.Run.ID, "git-submit-child-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: childClaim.Token, Outcome: core.OutcomeSubmit, Summary: "child result",
		ExpectedHead: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RequestID: "git-submit-child-outcome",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, childClaim.Run.ID, "git-submit-child-terminal")
	requireNoError(t, h.service.ReconcileGit(context.Background()))

	childAfter, err := h.database.Task(context.Background(), child.ID)
	requireNoError(t, err)
	if childAfter.Status != core.TaskSubmitted ||
		childAfter.HeadSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("child after capture = %#v", childAfter)
	}
	parentAfter, err := h.database.Task(context.Background(), parent.ID)
	requireNoError(t, err)
	if parentAfter.Status != core.TaskQueued {
		t.Fatalf("parent wake projection = %#v", parentAfter)
	}
	parentMessages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: parent.ID})
	requireNoError(t, err)
	if !hasChildResult(parentMessages.Items, child.ID, "agent", parentAgent.ID) {
		t.Fatalf("parent agent notification missing: %#v", parentMessages.Items)
	}
}

// TestP2GitCaptureFailedChildFallsBackToBoss pins the failed-path twin: a Git
// capture invariant failure takes the child to failed and the project to error
// in the same transaction, so the child-result notify falls back to Boss
// (recipientKind "boss", recipientID "") instead of waking the parent as an
// agent. The parent must remain waiting — no agent run is created for a
// project that just left active.
func TestP2GitCaptureFailedChildFallsBackToBoss(t *testing.T) {
	h := newHarness(t)
	parentAgent := h.addAgent(t, "git-capture-fail-parent")
	childAgent := h.addAgent(t, "git-capture-fail-child")
	project := h.addProject(t, "git-capture-fail-project", "")
	parent, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: parentAgent.ID, Kind: core.TaskWork,
		Title: "parent", Priority: 20, RequestID: "git-capture-fail-parent-task",
	})
	requireNoError(t, err)
	parentClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || parentClaim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", parentClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), parentClaim.Run.ID, "git-capture-fail-parent-active"); err != nil {
		t.Fatal(err)
	}
	child, err := h.service.CreateChildTask(context.Background(), core.CreateChildTaskInput{
		Token: parentClaim.Token, AssigneeAgentID: childAgent.ID, Title: "child",
		Priority: 10, RequestID: "git-capture-fail-child-task",
	})
	requireNoError(t, err)
	waitAndTerminate(t, h, parentClaim, "git-capture-fail-parent-wait")

	childClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || childClaim.Task.ID != child.ID {
		t.Fatalf("child claim = %#v ok=%t err=%v", childClaim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), childClaim.Run.ID, "git-capture-fail-child-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: childClaim.Token, Outcome: core.OutcomeSubmit, Summary: "child result",
		ExpectedHead: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RequestID: "git-capture-fail-child-outcome",
	}); err != nil {
		t.Fatal(err)
	}
	terminalActiveRun(t, h, childClaim.Run.ID, "git-capture-fail-child-terminal")
	h.git.mu.Lock()
	h.git.captureErr = invariantGitFailure{message: "capture ref not found"}
	h.git.mu.Unlock()
	requireNoError(t, h.service.ReconcileGit(context.Background()))

	childAfter, err := h.database.Task(context.Background(), child.ID)
	requireNoError(t, err)
	if childAfter.Status != core.TaskFailed ||
		!strings.HasPrefix(childAfter.FailureReason, "GIT_CAPTURE_FAILED: ") {
		t.Fatalf("child after failed capture = %#v", childAfter)
	}
	projectAfter, err := h.database.Project(context.Background(), project.ID)
	requireNoError(t, err)
	if projectAfter.Status != core.ProjectError {
		t.Fatalf("project after failed capture = %#v", projectAfter)
	}
	parentAfter, err := h.database.Task(context.Background(), parent.ID)
	requireNoError(t, err)
	if parentAfter.Status != core.TaskWaiting {
		t.Fatalf("Boss fallback must not requeue the parent: %#v", parentAfter)
	}
	parentMessages, err := h.database.Messages(context.Background(), core.MessageFilter{TaskID: parent.ID})
	requireNoError(t, err)
	if !hasChildResult(parentMessages.Items, child.ID, "boss", "") {
		t.Fatalf("Boss fallback notification missing: %#v", parentMessages.Items)
	}
}

// TestP2GitAdvanceInvariantFailureIsSafeWithoutParent pins the third notify
// site (projectAdvanceInvariantFailure): the only tasks that reach the advance
// reconcile are integration tasks, which structurally have no parent, so the
// child-result notify is a no-op that must not error the reconcile.
func TestP2GitAdvanceInvariantFailureIsSafeWithoutParent(t *testing.T) {
	h := newHarness(t)
	worker := h.addAgent(t, "git-advance-fail-worker")
	integrator := h.addAgent(t, "git-advance-fail-integrator")
	project := h.addProject(t, "git-advance-fail-project", integrator.ID)
	integration, _ := driveIntegrationTaskWithUnackedMessage(t, h, project, worker, integrator, "git-advance-fail")
	h.git.mu.Lock()
	h.git.advanceErr = invariantGitFailure{message: "advance ref mismatch"}
	h.git.mu.Unlock()
	requireNoError(t, h.service.ReconcileGit(context.Background()))

	integrationAfter, err := h.database.Task(context.Background(), integration.ID)
	requireNoError(t, err)
	if integrationAfter.Status != core.TaskFailed ||
		!strings.HasPrefix(integrationAfter.FailureReason, "GIT_ADVANCE_FAILED: ") {
		t.Fatalf("integration after failed advance = %#v", integrationAfter)
	}
	projectAfter, err := h.database.Project(context.Background(), project.ID)
	requireNoError(t, err)
	if projectAfter.Status != core.ProjectError {
		t.Fatalf("project after failed advance = %#v", projectAfter)
	}
}
