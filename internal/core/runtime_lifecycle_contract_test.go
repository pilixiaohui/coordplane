package core_test

import (
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
)

func TestP3RuntimeFactsAdvanceMonotonicallyAndFenceEveryExternalFact(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-runtime-facts", 0)
	root := t.TempDir()
	prepared, err := h.service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-runtime-facts",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "logs", "run.log"), InstructionsHash: "sha256-instructions",
		LaunchMode: "start", CleanupOperationID: "cleanup-runtime-facts",
		RequestID: "prepare-runtime-facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.LaunchPhase != core.LaunchIntent || prepared.LaunchNonce == "" ||
		prepared.CleanupState != core.CleanupPending || prepared.WorkspacePath == "" ||
		prepared.HomePath == "" || prepared.LogPath == "" {
		t.Fatalf("prepared Run = %#v", prepared)
	}
	fact := runtimeFact(prepared, "container-runtime-facts")
	created, err := h.service.RecordContainerCreated(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	if created.LaunchPhase != core.LaunchCreated || created.ContainerID != fact.ContainerID {
		t.Fatalf("created Run = %#v", created)
	}

	beforeWrongNonce := h.durableSignature(t, project.ID)
	wrongNonce := fact
	wrongNonce.LaunchNonce = "stale-nonce"
	wrongNonce.RequestID = "wrong-nonce-start"
	if _, err := h.service.RecordRunStartIssued(context.Background(), wrongNonce); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("wrong nonce error = %v, want %s", err, core.CodeStaleRun)
	}
	if after := h.durableSignature(t, project.ID); after != beforeWrongNonce {
		t.Fatal("wrong nonce changed durable state")
	}
	tooEarly := fact
	tooEarly.RequestID = "too-early-observation"
	if _, err := h.service.ObserveProcessAndActivateRun(context.Background(), tooEarly); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("early observation error = %v, want %s", err, core.CodeInvalidState)
	}
	missingContainer := fact
	missingContainer.ContainerID = ""
	missingContainer.RequestID = "missing-container-start"
	beforeMissingContainer := h.durableSignature(t, project.ID)
	if _, err := h.service.RecordRunStartIssued(context.Background(), missingContainer); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("missing container error = %v, want %s", err, core.CodeStaleRun)
	}
	if after := h.durableSignature(t, project.ID); after != beforeMissingContainer {
		t.Fatal("missing container fact changed durable state")
	}

	fact.RequestID = "start-runtime-facts"
	started, err := h.service.RecordRunStartIssued(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	if started.LaunchPhase != core.LaunchStartIssued || started.State != core.RunStarting {
		t.Fatalf("start-issued Run = %#v", started)
	}
	fact.RequestID = "observe-runtime-facts"
	active, err := h.service.ObserveProcessAndActivateRun(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != core.RunActive || active.LaunchPhase != core.LaunchProcessObserved ||
		active.StartedAt == "" || active.HeartbeatAt == "" {
		t.Fatalf("active Run = %#v", active)
	}
	persistedTask, err := h.database.Task(context.Background(), claim.Task.ID)
	if err != nil || persistedTask.Status != core.TaskRunning || persistedTask.CurrentRunID != active.ID {
		t.Fatalf("active Task = %#v err=%v", persistedTask, err)
	}
}

func TestP3SessionFactIsImmediateImmutableAndReplaySafe(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-session", 0)
	active := prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), "p3-session")
	fact := runtimeFact(active, active.ContainerID)
	fact.RequestID = "session-record"
	session, err := h.service.RecordRunSession(context.Background(), core.RunSessionInput{
		RunRuntimeFactInput: fact, NativeSessionID: "session-native-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.NativeSessionID != "session-native-1" {
		t.Fatalf("session Run = %#v", session)
	}
	beforeReplay := h.durableSignature(t, project.ID)
	if _, err := h.service.RecordRunSession(context.Background(), core.RunSessionInput{
		RunRuntimeFactInput: fact, NativeSessionID: "session-native-1",
	}); err != nil {
		t.Fatalf("session replay: %v", err)
	}
	if after := h.durableSignature(t, project.ID); after != beforeReplay {
		t.Fatal("session replay added durable side effects")
	}
	if _, err := h.service.RecordRunSession(context.Background(), core.RunSessionInput{
		RunRuntimeFactInput: fact, NativeSessionID: "session-native-2",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("changed session error = %v, want %s", err, core.CodeInvalidState)
	}
	events, err := h.database.Events(context.Background(), core.EventFilter{ProjectID: project.ID, RunID: active.ID})
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, "run.session_recorded") != 1 {
		t.Fatalf("session Events = %#v", events)
	}
}

func TestP3CleanupRequiresTerminalAndStableOperationFence(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-cleanup", 0)
	active := prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), "p3-cleanup")
	fact := runtimeFact(active, active.ContainerID)
	fact.RequestID = "cleanup-too-early"
	if _, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: fact, CleanupOperationID: active.CleanupOperationID, State: core.CleanupRemoved,
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("live cleanup removal error = %v, want %s", err, core.CodeInvalidState)
	}
	exitCode := 0
	terminalInput := runtimeTerminalInput(active, core.RunExited, "cleanup-terminal")
	terminalInput.ExitCode = &exitCode
	terminalInput.TerminalReason = "CLI exited without a Task outcome"
	terminalInput.OperationID = active.LaunchOperationID
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), terminalInput)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Task.Status == core.TaskCompleted || terminal.Task.Status == core.TaskSubmitted {
		t.Fatalf("exit 0 completed Task: %#v", terminal.Task)
	}
	terminalFact := runtimeFact(terminal.Run, terminal.Run.ContainerID)
	terminalFact.RequestID = "cleanup-blocked"
	blocked, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: terminalFact, CleanupOperationID: terminal.Run.CleanupOperationID,
		State: core.CleanupBlocked, LastError: "Docker temporarily unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalFact.RequestID = "cleanup-retry"
	pending, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: terminalFact, CleanupOperationID: blocked.CleanupOperationID,
		State: core.CleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalFact.RequestID = "cleanup-removed"
	removed, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: terminalFact, CleanupOperationID: pending.CleanupOperationID,
		State: core.CleanupRemoved,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed.CleanupState != core.CleanupRemoved || removed.LastError != "Docker temporarily unavailable" {
		t.Fatalf("removed cleanup = %#v", removed)
	}
	beforeWrongOperation := h.durableSignature(t, project.ID)
	terminalFact.RequestID = "cleanup-wrong-operation"
	if _, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: terminalFact, CleanupOperationID: "different-cleanup",
		State: core.CleanupPending,
	}); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("wrong cleanup operation error = %v, want %s", err, core.CodeStaleRun)
	}
	if after := h.durableSignature(t, project.ID); after != beforeWrongOperation {
		t.Fatal("wrong cleanup operation changed durable state")
	}
}

func TestP3CleanupOwnershipBlocksNextAgentRunUntilEveryRuntimeResourceIsRemoved(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-cleanup-admission", 0)
	nextTask, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: claim.Run.AgentID, Kind: core.TaskWork,
		Title: "must wait for prior Run cleanup", Priority: 100, RequestID: "cleanup-admission-next-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), "p3-cleanup-admission")
	terminalInput := runtimeTerminalInput(active, core.RunExited, "cleanup-admission-terminal")
	exitCode := 1
	terminalInput.ExitCode = &exitCode
	terminalInput.TerminalReason = "provider exited"
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), terminalInput)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Run.CleanupState != core.CleanupPending || terminal.Task.Status != core.TaskFailed {
		t.Fatalf("terminal cleanup fixture = %#v", terminal)
	}
	assertAgentCannotClaimUntilCleanupRemoved(t, h, project.ID)

	fact := runtimeFact(terminal.Run, terminal.Run.ContainerID)
	fact.RequestID = "cleanup-admission-blocked"
	blocked, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: fact, CleanupOperationID: terminal.Run.CleanupOperationID,
		State: core.CleanupBlocked, LastError: "container removal is uncertain",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertAgentCannotClaimUntilCleanupRemoved(t, h, project.ID)

	fact.RequestID = "cleanup-admission-retry"
	pending, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: fact, CleanupOperationID: blocked.CleanupOperationID, State: core.CleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	fact.RequestID = "cleanup-admission-removed"
	if _, err := h.service.RecordRunCleanup(context.Background(), core.RunCleanupInput{
		RunRuntimeFactInput: fact, CleanupOperationID: pending.CleanupOperationID, State: core.CleanupRemoved,
	}); err != nil {
		t.Fatal(err)
	}
	next, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("claim after cleanup removed: ok=%v err=%v", ok, err)
	}
	if next.Task.ID != nextTask.ID || next.Run.AgentID != claim.Run.AgentID {
		t.Fatalf("claim after cleanup removed = %#v, want Task %s on Agent %s", next, nextTask.ID, claim.Run.AgentID)
	}
}

func assertAgentCannotClaimUntilCleanupRemoved(t *testing.T, h *harness, projectID string) {
	t.Helper()
	before := h.durableSignature(t, projectID)
	if claim, ok, err := h.service.ClaimNext(context.Background(), projectID); err != nil || ok {
		t.Fatalf("claim while prior Run cleanup owns Agent: claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if after := h.durableSignature(t, projectID); after != before {
		t.Fatal("rejected cleanup-blocked claim changed durable state")
	}
}

func TestP3TerminalCleanupDoesNotConsumeAnotherAgentsGlobalRunSlot(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-cleanup-global-slot", 0)
	active := prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), "p3-cleanup-global-slot")
	exitCode := 1
	terminalInput := runtimeTerminalInput(active, core.RunExited, "cleanup-global-slot-terminal")
	terminalInput.ExitCode = &exitCode
	terminalInput.TerminalReason = "provider exited"
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), terminalInput)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Run.CleanupState != core.CleanupPending {
		t.Fatalf("terminal cleanup state = %q, want %q", terminal.Run.CleanupState, core.CleanupPending)
	}

	other := h.addAgent(t, "cleanup-global-slot-other-agent")
	otherTask, err := h.service.CreateTask(context.Background(), core.CreateTaskInput{
		ProjectID: project.ID, AssigneeAgentID: other.ID, Kind: core.TaskWork,
		Title: "other Agent still has global capacity", Priority: 100,
		RequestID: "cleanup-global-slot-other-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.service, err = core.NewService(h.database, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetReady(true, "")

	next, ok, err := h.service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok {
		t.Fatalf("other Agent claim while terminal cleanup is pending: ok=%v err=%v", ok, err)
	}
	if next.Task.ID != otherTask.ID || next.Run.AgentID != other.ID {
		t.Fatalf("claimed %#v, want Task %s on Agent %s", next, otherTask.ID, other.ID)
	}
}

func TestP3RuntimeTerminalIngressRejectsEveryStaleOwnershipFact(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-terminal-fence", 0)
	active := prepareAndActivateRuntimeRun(t, h, claim, t.TempDir(), "p3-terminal-fence")

	base := runtimeTerminalInput(active, core.RunExited, "terminal-fence")
	exitCode := 0
	base.ExitCode = &exitCode
	base.TerminalReason = "provider exited"
	for name, mutate := range map[string]func(*core.RunTerminalInput){
		"generation":       func(input *core.RunTerminalInput) { input.Generation++ },
		"launch nonce":     func(input *core.RunTerminalInput) { input.LaunchNonce = "stale-nonce" },
		"launch operation": func(input *core.RunTerminalInput) { input.LaunchOperationID = "stale-operation" },
		"container":        func(input *core.RunTerminalInput) { input.ContainerID = "stale-container" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			input.RequestID += "-" + name
			mutate(&input)
			before := h.durableSignature(t, project.ID)
			if _, err := h.service.RecordRuntimeRunTerminal(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
				t.Fatalf("stale %s error = %v, want %s", name, err, core.CodeStaleRun)
			}
			if after := h.durableSignature(t, project.ID); after != before {
				t.Fatalf("stale %s changed durable state", name)
			}
		})
	}

	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Run.State != core.RunExited || terminal.Task.Status != core.TaskFailed {
		t.Fatalf("fenced terminal result = %#v", terminal)
	}
}

func TestP3RuntimeTerminalIngressAllowsOnlyNarrowPreLaunchFailure(t *testing.T) {
	h := newHarness(t)
	project, claim := createClaimedWorkRun(t, h, "p3-prelaunch-terminal", 0)
	input := runtimeTerminalInput(claim.Run, core.RunExited, "prelaunch-exited")
	input.TerminalReason = "no process was started"

	before := h.durableSignature(t, project.ID)
	if _, err := h.service.RecordRuntimeRunTerminal(context.Background(), input); !core.IsCode(err, core.CodeStaleRun) {
		t.Fatalf("pre-launch exited error = %v, want %s", err, core.CodeStaleRun)
	}
	if after := h.durableSignature(t, project.ID); after != before {
		t.Fatal("rejected pre-launch terminal fact changed durable state")
	}

	input.State = core.RunFailed
	input.RequestID = "prelaunch-failed"
	terminal, err := h.service.RecordRuntimeRunTerminal(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Run.State != core.RunFailed || terminal.Run.LaunchNonce != "" || terminal.Run.ContainerID != "" ||
		terminal.Task.Status != core.TaskFailed {
		t.Fatalf("pre-launch failure result = %#v", terminal)
	}
	beforeReplay := h.durableSignature(t, project.ID)
	input.RequestID = "prelaunch-failed-replay"
	if _, err := h.service.RecordRuntimeRunTerminal(context.Background(), input); err != nil {
		t.Fatalf("replay pre-launch failure: %v", err)
	}
	if after := h.durableSignature(t, project.ID); after != beforeReplay {
		t.Fatal("pre-launch failure replay changed durable state")
	}
}

func prepareAndActivateRuntimeRun(t *testing.T, h *harness, claim core.Claim, root, prefix string) core.Run {
	t.Helper()
	prepared := prepareRuntimeRun(t, h, claim, root, prefix)
	fact := runtimeFact(prepared, prefix+"-container")
	fact.RequestID = prefix + "-created"
	created, err := h.service.RecordContainerCreated(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	fact = runtimeFact(created, created.ContainerID)
	fact.RequestID = prefix + "-start"
	started, err := h.service.RecordRunStartIssued(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	fact = runtimeFact(started, started.ContainerID)
	fact.RequestID = prefix + "-active"
	active, err := h.service.ObserveProcessAndActivateRun(context.Background(), fact)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func prepareRuntimeRun(t *testing.T, h *harness, claim core.Claim, root, prefix string) core.Run {
	t.Helper()
	prepared, err := h.service.BeginRunLaunch(context.Background(), core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: prefix + "-nonce",
		WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: prefix + "-instructions",
		LaunchMode: "start", CleanupOperationID: prefix + "-cleanup", RequestID: prefix + "-prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func runtimeFact(run core.Run, containerID string) core.RunRuntimeFactInput {
	return core.RunRuntimeFactInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: containerID,
	}
}

func runtimeTerminalInput(run core.Run, state core.RunState, requestID string) core.RunTerminalInput {
	return core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: state, RequestID: requestID,
	}
}
