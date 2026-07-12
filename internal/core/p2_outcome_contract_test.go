package core_test

import (
	"context"
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
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := h.service.Chat(context.Background(), core.ChatInput{
		ProjectID: project.ID, AgentID: foreignAgent.ID, Body: "foreign ack",
		Wake: false, RequestID: "rollback-foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: agent.ID, Kind: core.TaskWork,
		Title: "atomic outcome", Priority: 100, RequestID: "rollback-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, "rollback-active"); err != nil {
		t.Fatal(err)
	}
	before := h.durableSignature(t, project.ID)
	if _, err := h.service.RequestOutcome(context.Background(), core.OutcomeInput{
		Token: claim.Token, Outcome: core.OutcomeWait, Reason: "waiting",
		AckMessageIDs: []string{foreign.Message.ID, valid.Message.ID}, RequestID: "rollback-outcome",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("foreign bundled ack error = %v", err)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("failed bundled outcome changed durable state")
	}

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
		if err != nil {
			t.Fatal(err)
		}
		if requested.Task.Status != core.TaskFinishing || requested.Run.State != core.RunActive {
			t.Fatalf("premature fail projection = %#v", requested)
		}
		terminal, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
			RunID: claim.Run.ID, State: core.RunExited,
			ExitCode: intPointer(1), RequestID: "terminal-fail-fact",
		})
		if err != nil {
			t.Fatal(err)
		}
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
		if err != nil {
			t.Fatal(err)
		}
		if requested.Task.Status != core.TaskFinishing || requested.Task.PendingAction != "capture" ||
			requested.Task.PendingActionRunID != claim.Run.ID || requested.Task.PendingActionVersion != requested.Task.Version {
			t.Fatalf("submit intent = %#v", requested.Task)
		}
		terminal, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
			RunID: claim.Run.ID, State: core.RunExited,
			ExitCode: intPointer(0), RequestID: "terminal-submit-fact",
		})
		if err != nil {
			t.Fatal(err)
		}
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
			if err != nil {
				t.Fatal(err)
			}
			sender, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
				ProjectID: project.ID, AssigneeAgentID: senderAgent.ID, Kind: core.TaskWork,
				Title: "sender", Priority: 90, RequestID: "finishing-sender-" + test.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			targetClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
			if err != nil || !ok || targetClaim.Task.ID != target.ID {
				t.Fatalf("target claim = %#v ok=%t err=%v", targetClaim, ok, err)
			}
			if _, err := h.service.ActivateRun(context.Background(), targetClaim.Run.ID, "finishing-target-active-"+test.name); err != nil {
				t.Fatal(err)
			}
			senderClaim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
			if err != nil || !ok || senderClaim.Task.ID != sender.ID {
				t.Fatalf("sender claim = %#v ok=%t err=%v", senderClaim, ok, err)
			}
			if _, err := h.service.ActivateRun(context.Background(), senderClaim.Run.ID, "finishing-sender-active-"+test.name); err != nil {
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
			terminal, err := h.service.RecordRunTerminal(context.Background(), core.RunTerminalInput{
				RunID: targetClaim.Run.ID, State: core.RunExited,
				ExitCode: intPointer(0), RequestID: "finishing-terminal-" + test.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Task.Status != test.wantStatus || terminal.Task.CurrentRunID != "" {
				t.Fatalf("finishing terminal projection = %#v", terminal.Task)
			}
			claim, claimed, err := h.service.ClaimNext(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
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
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != task.ID {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := h.service.ActivateRun(context.Background(), claim.Run.ID, requestPrefix+"-active"); err != nil {
		t.Fatal(err)
	}
	return claim
}
