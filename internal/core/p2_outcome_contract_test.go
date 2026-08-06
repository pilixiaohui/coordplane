package core_test

import (
	"context"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestP2OutcomeAckBundleRollsBackAsAUnit(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "rollback-agent")
	foreignAgent := h.addAgent(t, "rollback-foreign")
	project := h.addProject(t, "rollback-project", "")
	valid, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: agent.ID, Body: "valid ack",
		Wake: false, RequestID: "rollback-valid",
	})
	requireNoError(t, err)
	foreign, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: foreignAgent.ID, Body: "foreign ack",
		Wake: false, RequestID: "rollback-foreign",
	})
	requireNoError(t, err)
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "atomic outcome", Priority: 100, RequestID: "rollback-task",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, "rollback-active"); err != nil {
		t.Fatal(err)
	}
	before := durableSignature(t, h.database, project.ID)
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "waiting",
		AckMessageIDs: []string{foreign.Message.ID, valid.Message.ID}, RequestID: "rollback-outcome",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("foreign bundled ack error = %v", err)
	}
	h.requireDurableSignature(t, project.ID, before)

	result, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "waiting",
		AckMessageIDs: []string{valid.Message.ID}, RequestID: "rollback-outcome",
	})
	if err != nil {
		t.Fatalf("corrected retry with rolled-back request ID: %v", err)
	}
	if result.Task.Status != core.TaskFinishing || len(result.Acknowledged) != 1 ||
		result.Acknowledged[0].ID != valid.Message.ID || result.Acknowledged[0].State != core.MessageAcknowledged {
		t.Fatalf("corrected outcome = %#v", result)
	}
}

func TestP2FailAndSubmitRemainTwoPhaseAtRunTerminal(t *testing.T) {
	t.Run("fail projects only after terminal", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "terminal-fail-agent")
		project := h.addProject(t, "terminal-fail-project", "")
		claim := createActiveWorkClaim(t, h, project, agent, "terminal-fail")
		requested, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
			Token: claim.Token, Outcome: core.OutcomeFail,
			Reason: "cannot complete", RequestID: "terminal-fail-outcome",
		})
		requireNoError(t, err)
		if requested.Task.Status != core.TaskFinishing || requested.Run.State != core.RunActive {
			t.Fatalf("premature fail projection = %#v", requested)
		}
		terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
			RunID: claim.Run.ID, State: core.RunExited,
			ExitCode: intPointer(1), RequestID: "terminal-fail-fact",
		})
		requireNoError(t, err)
		if terminal.Task.Status != core.TaskFailed || terminal.Task.CurrentRunID != "" ||
			terminal.Task.FailureReason != "cannot complete" {
			t.Fatalf("terminal fail projection = %#v", terminal.Task)
		}
	})

	t.Run("submit waits for trusted capture", func(t *testing.T) {
		h := newHarness(t)
		agent := h.addAgent(t, "terminal-submit-agent")
		project := h.addProject(t, "terminal-submit-project", "")
		claim := createActiveWorkClaim(t, h, project, agent, "terminal-submit")
		requested, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
			Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "ready for capture",
			ExpectedHead: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", RequestID: "terminal-submit-outcome",
		})
		requireNoError(t, err)
		if requested.Task.Status != core.TaskFinishing || requested.Task.PendingAction != "capture" ||
			requested.Task.PendingActionRunID != claim.Run.ID || requested.Task.PendingActionVersion != requested.Task.Version {
			t.Fatalf("submit intent = %#v", requested.Task)
		}
		terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
			RunID: claim.Run.ID, State: core.RunExited,
			ExitCode: intPointer(0), RequestID: "terminal-submit-fact",
		})
		requireNoError(t, err)
		if terminal.Task.Status != core.TaskFinishing || terminal.Task.PendingAction != "capture" ||
			terminal.Task.CurrentRunID != claim.Run.ID || terminal.Task.Status == core.TaskSubmitted {
			t.Fatalf("submit terminal pretended capture succeeded: %#v", terminal.Task)
		}
	})
}

func TestP2FinishingWindowQueuesExactlyOnceOnlyForWakeMessage(t *testing.T) {
	for _, test := range []struct {
		name       string
		wake       bool
		wantStatus core.TaskStatus
	}{
		{name: "wake", wake: true, wantStatus: core.TaskQueued},
		{name: "no wake", wake: false, wantStatus: core.TaskWaiting},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			targetAgent := h.addAgent(t, "finishing-target-"+test.name)
			senderAgent := h.addAgent(t, "finishing-sender-"+test.name)
			project := h.addProject(t, "finishing-project-"+test.name, "")
			target, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: targetAgent.ID, Kind: core.TaskWork,
				Title: "target", Priority: 100, RequestID: "finishing-target-" + test.name,
			})
			requireNoError(t, err)
			sender, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: senderAgent.ID, Kind: core.TaskWork,
				Title: "sender", Priority: 90, RequestID: "finishing-sender-" + test.name,
			})
			requireNoError(t, err)
			targetClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
			if err != nil || !ok || targetClaim.Task.ID != target.ID {
				t.Fatalf("target claim = %#v ok=%t err=%v", targetClaim, ok, err)
			}
			if _, err := activateRun(t, h, context.Background(), targetClaim.Run.ID, "finishing-target-active-"+test.name); err != nil {
				t.Fatal(err)
			}
			senderClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
			if err != nil || !ok || senderClaim.Task.ID != sender.ID {
				t.Fatalf("sender claim = %#v ok=%t err=%v", senderClaim, ok, err)
			}
			if _, err := activateRun(t, h, context.Background(), senderClaim.Run.ID, "finishing-sender-active-"+test.name); err != nil {
				t.Fatal(err)
			}
			if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
				Token: targetClaim.Token, Outcome: core.OutcomeWait,
				Reason: "waiting for message", RequestID: "finishing-wait-" + test.name,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.service.SendAgentMessage(context.Background(), core.SendMessageInput{
				Token: senderClaim.Token, RecipientKind: "agent", RecipientID: targetAgent.ID,
				TaskID: target.ID, Body: "arrived while finishing", Wake: test.wake,
				RequestID: "finishing-message-" + test.name,
			}); err != nil {
				t.Fatal(err)
			}
			if claim, ok, err := h.service.ClaimNext(context.Background(), project.ID); err != nil || ok {
				t.Fatalf("second claim before old Run terminal = %#v ok=%t err=%v", claim, ok, err)
			}
			terminal, err := recordRunTerminal(h, context.Background(), core.RunTerminalInput{
				RunID: targetClaim.Run.ID, State: core.RunExited,
				ExitCode: intPointer(0), RequestID: "finishing-terminal-" + test.name,
			})
			requireNoError(t, err)
			if terminal.Task.Status != test.wantStatus || terminal.Task.CurrentRunID != "" {
				t.Fatalf("finishing terminal projection = %#v", terminal.Task)
			}
			recordCleanupRemoved(t, h, terminal.Run, "finishing-cleanup-"+test.name)
			claim, claimed, err := h.service.ClaimNext(context.Background(), project.ID)
			requireNoError(t, err)
			if test.wake && (!claimed || claim.Task.ID != target.ID) {
				t.Fatalf("wake message did not produce one new claim: %#v claimed=%t", claim, claimed)
			}
			if !test.wake && claimed {
				t.Fatalf("wake=false created a new Run: %#v", claim)
			}
		})
	}
}

func createActiveWorkClaim(t *testing.T, h *harness, project core.Project, agent core.Agent, requestPrefix string) core.Claim {
	t.Helper()
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: requestPrefix, RequestID: requestPrefix + "-task",
	})
	requireNoError(t, err)
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := activateRun(t, h, context.Background(), claim.Run.ID, requestPrefix+"-active"); err != nil {
		t.Fatal(err)
	}
	return claim
}

func TestP2SubmitExpandsShortHashPrefix(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "short-prefix-agent")
	project := h.addProject(t, "short-prefix-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "short-prefix")
	const fullSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	requested, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "capture this",
		ExpectedHead: "aaaa", RequestID: "short-prefix-outcome",
	})
	requireNoError(t, err)
	if requested.Run.ExpectedHead != fullSHA || requested.Task.PendingExpectedSHA != fullSHA {
		t.Fatalf("short prefix did not expand durably: run.ExpectedHead=%q task.PendingExpectedSHA=%q",
			requested.Run.ExpectedHead, requested.Task.PendingExpectedSHA)
	}
	if requested.Task.Status != core.TaskFinishing || requested.Task.PendingAction != "capture" ||
		requested.Task.PendingActionRunID != claim.Run.ID {
		t.Fatalf("submit projection = %#v", requested.Task)
	}
	terminalActiveRun(t, h, claim.Run.ID, "short-prefix-terminal")
	requireNoError(t, h.service.ReconcileGit(context.Background()))
	task, err := h.database.Task(context.Background(), claim.Task.ID)
	requireNoError(t, err)
	if task.Status != core.TaskSubmitted {
		t.Fatalf("capture did not finish submit: %#v", task)
	}
}

func TestP2SubmitShortPrefixFailureIsRetryable(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "short-prefix-fail-agent")
	project := h.addProject(t, "short-prefix-fail-project", "")
	claim := createActiveWorkClaim(t, h, project, agent, "short-prefix-fail")
	before := durableSignature(t, h.database, project.ID)
	_, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "bad head",
		ExpectedHead: "bogus", RequestID: "short-prefix-fail-outcome",
	})
	if !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("unresolvable prefix error = %v", err)
	}
	if !strings.Contains(err.Error(), "expand expected_head in workspace") ||
		!strings.Contains(err.Error(), "does not resolve to a commit") {
		t.Fatalf("error does not surface readable cause: %v", err)
	}
	h.requireDurableSignature(t, project.ID, before)
	task, err := h.database.Task(context.Background(), claim.Task.ID)
	requireNoError(t, err)
	if task.Status != core.TaskRunning || task.PendingAction != "" {
		t.Fatalf("failed submit changed task: %#v", task)
	}
	// Corrected resubmit under the same request ID proves the idempotency key was not poisoned.
	fixed, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeSubmit, Summary: "fixed head",
		ExpectedHead: "aaaa", RequestID: "short-prefix-fail-outcome",
	})
	requireNoError(t, err)
	if fixed.Task.Status != core.TaskFinishing || fixed.Task.PendingAction != "capture" ||
		fixed.Run.ExpectedHead != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("corrected submit projection = %#v", fixed)
	}
}
