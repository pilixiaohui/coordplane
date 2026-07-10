package runtime_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/coordination"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestCommandCLIAdapterExitWithoutTerminalActionConvergesFailure(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-start",
		ExitCode:   0,
		Stdout:     []byte(`{"status":"accepted"}`),
	}}}
	h := newCommandCLIHarness(t, exec)
	addContract(t, ctx, h.coordination, "developer")

	_, err := h.runner.StartNext(ctx, "developer")
	if err == nil || !strings.Contains(err.Error(), "AGENT_EXITED_WITHOUT_TERMINAL_ACTION") {
		t.Fatalf("start command CLI error = %v, want terminal-action convergence failure", err)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("container exec calls = %d, want 1", len(exec.specs))
	}
	spec := exec.specs[0]
	if spec.ContainerName == "" || spec.Workdir != cpruntime.ContainerWorkspacePath || spec.HomeDir != cpruntime.ContainerHomePath {
		t.Fatalf("exec spec paths = %+v, want docker container paths", spec)
	}
	if len(spec.Command) == 0 || spec.Command[0] != "/usr/local/bin/claude" || !contains(spec.Command, "--session-id") {
		t.Fatalf("exec command = %#v, want claude --session-id", spec.Command)
	}
	if !contains(spec.Command, "--verbose") || !contains(spec.Command, "stream-json") {
		t.Fatalf("exec command = %#v, want full stream-json provider transcript", spec.Command)
	}
	if !strings.Contains(spec.Stdin, cpruntime.ContainerCoordlinkPath) {
		t.Fatalf("bootstrap stdin missing coordlink protocol: %s", spec.Stdin)
	}
	if strings.TrimSpace(spec.Stdin) == "" {
		t.Fatal("bootstrap stdin is empty")
	}
	if spec.Env["HOME"] != cpruntime.ContainerHomePath ||
		spec.Env["COORDPLANE_AGENT_ID"] != "developer" ||
		spec.Env["COORDPLANE_TOKEN"] == "" {
		t.Fatalf("exec env = %#v, want scoped runtime identity and container HOME", spec.Env)
	}

	var attemptID, leaseID, assignmentID, attemptStatus, leaseStatus, assignmentStatus, transcriptRef string
	if err := h.db.QueryRowContext(ctx, `
SELECT att.id, l.id, l.assignment_id, att.status, l.state, a.state, COALESCE(att.transcript_ref, '')
FROM attempts att
JOIN leases l ON l.id = att.lease_id
JOIN assignments a ON a.id = l.assignment_id
ORDER BY att.started_at DESC, att.id DESC
LIMIT 1`).Scan(&attemptID, &leaseID, &assignmentID, &attemptStatus, &leaseStatus, &assignmentStatus, &transcriptRef); err != nil {
		t.Fatalf("read converged one-shot state: %v", err)
	}
	if attemptStatus != "failed" || leaseStatus != "released" || assignmentStatus != "queued" || !strings.HasPrefix(transcriptRef, "obj_sha256_") {
		t.Fatalf("one-shot state = attempt:%s lease:%s assignment:%s transcript:%s", attemptStatus, leaseStatus, assignmentStatus, transcriptRef)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 {
		t.Fatalf("cli sessions = %+v, want one start session", cliSessions)
	}
	cli := cliSessions[0]
	if cli.State != "exited" || cli.StartReason != "start" ||
		cli.TranscriptRef != transcriptRef ||
		cli.ContainerName == "" ||
		cli.ExitCode == nil || *cli.ExitCode != 0 {
		t.Fatalf("cli session = %+v, want exited start evidence linked to attempt", cli)
	}
	raw := mustJSON(t, cli)
	for _, forbidden := range []string{spec.Env["COORDPLANE_TOKEN"], "complete work"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("cli inspect evidence leaked forbidden value %q: %s", forbidden, raw)
		}
	}
	if got := countRowsWhere(t, ctx, h.db, "runtime_tokens", "attempt_id = '"+attemptID+"' AND state = 'active'"); got != 0 {
		t.Fatalf("active runtime tokens after one-shot exit = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "active_guards", "attempt_id = '"+attemptID+"' AND state = 'active'"); got != 0 {
		t.Fatalf("active guards after one-shot exit = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type IN ('cli.start_requested', 'cli.process_started', 'cli.session_id_captured', 'cli.exited')"); got != 4 {
		t.Fatalf("cli events = %d, want start/process/session/exited", got)
	}
}

func TestCommandCLIAdapterResumeUsesLightweightMailboxSignal(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{
		{ProcessRef: "exec-start", ExitCode: 0, Stdout: []byte("started")},
		{ProcessRef: "exec-resume", ExitCode: 0, Stdout: []byte("resumed")},
	}}
	var coordinationService *coordination.Service
	exec.onExec = func(spec cpruntime.ContainerExecSpec) {
		if coordinationService == nil {
			return
		}
		if _, err := coordinationService.WaitContract(ctx, coordination.WaitContractInput{
			LeaseID: spec.Env["COORDPLANE_LEASE_ID"],
			AgentID: spec.Env["COORDPLANE_AGENT_ID"],
			Reason:  "waiting for resumable work",
		}); err != nil {
			t.Fatalf("record contract.wait during one-shot exec: %v", err)
		}
	}
	h := newCommandCLIHarness(t, exec)
	coordinationService = h.coordination
	addContract(t, ctx, h.coordination, "developer")

	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start command CLI: %v", err)
	}
	if _, err := h.runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "waiting",
		Summary:       "waiting",
		TranscriptRef: "waiting-ref",
	}); err != nil {
		t.Fatalf("finish waiting: %v", err)
	}
	resumed, err := h.runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{
		RouteID:    session.Route.ID,
		Reason:     "mailbox.resume",
		MailboxIDs: []string{"mbox_secret"},
	})
	if err != nil {
		t.Fatalf("resume route: %v", err)
	}
	if resumed.State != "resumed" {
		t.Fatalf("resume result = %+v, want resumed", resumed)
	}
	if len(exec.specs) != 2 {
		t.Fatalf("exec calls = %d, want start and resume", len(exec.specs))
	}
	resume := exec.specs[1]
	if !contains(resume.Command, "--resume") || !contains(resume.Command, session.Route.SessionNativeID) {
		t.Fatalf("resume command = %#v, want --resume native session", resume.Command)
	}
	if !strings.Contains(resume.Stdin, "mbox_secret") || strings.Contains(resume.Stdin, "full mailbox body") {
		t.Fatalf("resume stdin = %q, want mailbox id signal only", resume.Stdin)
	}
	if strings.TrimSpace(resume.Stdin) == "" {
		t.Fatal("resume stdin is empty")
	}
	if resume.Env["COORDPLANE_TOKEN"] == exec.specs[0].Env["COORDPLANE_TOKEN"] {
		t.Fatalf("resume reused start token, want fresh scoped env")
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 2 || cliSessions[1].StartReason != "resume" || cliSessions[1].ResumeOf != cliSessions[0].ID {
		t.Fatalf("cli sessions = %+v, want linked resume evidence", cliSessions)
	}
	if state := routeState(t, ctx, h.db, session.Route.ID); state != "active" {
		t.Fatalf("route after resume = %s, want active", state)
	}
}

func TestCommandCLIAdapterPromptPlaceholderKeepsPromptOutOfArgvAndEvidence(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{
		{ProcessRef: "exec-start", ExitCode: 0, Stdout: []byte("ready")},
		{ProcessRef: "exec-resume", ExitCode: 0, Stdout: []byte("resumed")},
	}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_prompt_arg", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_prompt_arg")
	insertReadyRuntimeInstance(t, ctx, db, "rt_prompt_arg", "att_prompt_arg", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:      "claude",
			Backend:   "claude",
			Binary:    "/usr/local/bin/claude",
			StartArgs: []string{"--session-id", "{{session_id}}", "--print", "{{prompt}}\nFIXED_START_SMOKE_ARG"},
			ResumeArgs: []string{
				"--resume", "{{session_id}}", "--print", "{{prompt}}\nFIXED_RESUME_SMOKE_ARG",
			},
			Timeout: timeSecond(),
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	startEnv := runtimeEnv(t, "developer", "rt_prompt_arg", "att_prompt_arg")
	startEnv["COORDPLANE_TOKEN"] = "COORDPLANE_TOKEN_SENTINEL_START"
	startEnv["ANTHROPIC_AUTH_TOKEN"] = "ANTHROPIC_AUTH_TOKEN_SENTINEL_START"
	start, err := adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_prompt_arg",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_prompt_arg",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             startEnv,
		BootstrapPrompt: "BOOTSTRAP_PROMPT_SENTINEL",
	})
	if err != nil {
		t.Fatalf("start with prompt arg: %v", err)
	}
	resumeEnv := runtimeEnv(t, "developer", "rt_prompt_arg", "att_prompt_arg")
	resumeEnv["COORDPLANE_TOKEN"] = "COORDPLANE_TOKEN_SENTINEL_RESUME"
	resumeEnv["ANTHROPIC_AUTH_TOKEN"] = "ANTHROPIC_AUTH_TOKEN_SENTINEL_RESUME"
	if err := adapter.Resume(ctx, cpruntime.ResumeRequest{
		Route: cpruntime.SessionRoute{
			ID:              "route_prompt_arg",
			AgentID:         "developer",
			RuntimeID:       "rt_prompt_arg",
			CLIBackend:      "claude",
			SessionNativeID: start.SessionNativeID,
			Workdir:         cpruntime.ContainerWorkspacePath,
			HomeDir:         cpruntime.ContainerHomePath,
			AttemptID:       "att_prompt_arg",
			LeaseID:         "lease_missing_prompt",
			AssignmentID:    "asg_missing_prompt",
		},
		Reason:     "RESUME_REASON_SENTINEL",
		MailboxIDs: []string{"mbox_prompt_sentinel"},
		Env:        resumeEnv,
	}); err != nil {
		t.Fatalf("resume with prompt arg: %v", err)
	}
	if len(exec.specs) != 2 {
		t.Fatalf("exec specs = %+v, want start and resume", exec.specs)
	}
	assertExecCommandDoesNotLeak := func(label string, spec cpruntime.ContainerExecSpec, fixedArg string, wantsStdin []string) {
		t.Helper()
		command := strings.Join(spec.Command, "\n")
		if !strings.Contains(command, fixedArg) {
			t.Fatalf("%s command = %q, want fixed non-sensitive arg %q", label, command, fixedArg)
		}
		for _, want := range wantsStdin {
			if !strings.Contains(spec.Stdin, want) {
				t.Fatalf("%s stdin = %q, missing prompt sentinel %q", label, spec.Stdin, want)
			}
		}
		for _, forbidden := range []string{
			"BOOTSTRAP_PROMPT_SENTINEL",
			"RESUME_REASON_SENTINEL",
			"COORDPLANE_TOKEN_SENTINEL_START",
			"COORDPLANE_TOKEN_SENTINEL_RESUME",
			"ANTHROPIC_AUTH_TOKEN_SENTINEL_START",
			"ANTHROPIC_AUTH_TOKEN_SENTINEL_RESUME",
			"CoordPlane runtime protocol",
		} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("%s command argv leaked %q: %q", label, forbidden, command)
			}
		}
	}
	assertExecCommandDoesNotLeak("start", exec.specs[0], "FIXED_START_SMOKE_ARG", []string{"BOOTSTRAP_PROMPT_SENTINEL", "CoordPlane runtime protocol"})
	assertExecCommandDoesNotLeak("resume", exec.specs[1], "FIXED_RESUME_SMOKE_ARG", []string{"RESUME_REASON_SENTINEL", "mbox_prompt_sentinel"})
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 2 ||
		!contains(cliSessions[0].Command, "FIXED_START_SMOKE_ARG") ||
		!contains(cliSessions[1].Command, "FIXED_RESUME_SMOKE_ARG") {
		t.Fatalf("persisted command = %+v, want fixed non-sensitive prompt args", cliSessions)
	}
	evidence := mustJSON(t, cliSessions) + "\n" + eventPayloadsJSON(t, ctx, db)
	for _, forbidden := range []string{
		"BOOTSTRAP_PROMPT_SENTINEL",
		"RESUME_REASON_SENTINEL",
		"COORDPLANE_TOKEN_SENTINEL_START",
		"COORDPLANE_TOKEN_SENTINEL_RESUME",
		"ANTHROPIC_AUTH_TOKEN_SENTINEL_START",
		"ANTHROPIC_AUTH_TOKEN_SENTINEL_RESUME",
		"CoordPlane runtime protocol",
	} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("CLI session/event evidence leaked %q: %s", forbidden, evidence)
		}
	}
}

func TestCommandCLIAdapterMissingBootstrapPromptFailsClosedWithSessionEvidence(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "should-not-run",
		ExitCode:   0,
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_missing_prompt", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_missing_prompt")
	insertReadyRuntimeInstance(t, ctx, db, "rt_missing_prompt", "att_missing_prompt", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	env := runtimeEnv(t, "developer", "rt_missing_prompt", "att_missing_prompt")
	_, err = adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_missing_prompt",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_missing_prompt",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             env,
		BootstrapPrompt: "",
	})
	if err == nil || !strings.Contains(err.Error(), "bootstrap prompt is required") {
		t.Fatalf("Start error = %v, want missing bootstrap prompt rejection", err)
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor specs = %+v, want no docker exec without prompt", exec.specs)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 ||
		cliSessions[0].State != "failed" ||
		!strings.Contains(cliSessions[0].LastError, "bootstrap prompt is required") ||
		!strings.HasPrefix(cliSessions[0].TranscriptRef, "obj_sha256_") {
		t.Fatalf("cli sessions = %+v, want failed session with transcript evidence", cliSessions)
	}
	if got := countRowsWhere(t, ctx, db, "events", "event_type IN ('cli.start_requested', 'cli.failed')"); got != 2 {
		t.Fatalf("cli prompt failure events = %d, want start_requested + failed", got)
	}
	if got := countRowsWhere(t, ctx, db, "transcripts", "attempt_id = 'att_missing_prompt'"); got != 1 {
		t.Fatalf("failure transcripts = %d, want one auditable failure transcript", got)
	}
}

func TestCommandCLIAdapterCommandPolicyAllowsConfiguredCoordlinkCapability(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-coordlink",
		ExitCode:   0,
		Stdout:     []byte(`{"status":"accepted"}`),
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_policy_allow", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_policy_allow")
	insertReadyRuntimeInstance(t, ctx, db, "rt_policy_allow", "att_policy_allow", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:       "coordlink-direct",
			Backend:    "coordlink",
			Binary:     cpruntime.ContainerCoordlinkPath,
			StartArgs:  []string{"call", "contract.current"},
			ResumeArgs: []string{"call", "contract.current"},
			Timeout:    timeSecond(),
			RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
				"docker-default": {
					NonInteractiveApproval:     true,
					AllowCoordlinkCapabilities: []string{"contract.current", "contract.add"},
				},
			},
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, err = adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_policy_allow",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_policy_allow",
		CLIBackend:      "coordlink",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_policy_allow", "att_policy_allow"),
		BootstrapPrompt: "policy allowlist direct coordlink call",
	})
	if err != nil {
		t.Fatalf("Start allowed coordlink command: %v", err)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec specs = %+v, want one allowed coordlink command", exec.specs)
	}
	if got := exec.specs[0].Command; len(got) != 3 || got[0] != cpruntime.ContainerCoordlinkPath || got[1] != "call" || got[2] != "contract.current" {
		t.Fatalf("exec command = %#v, want allowed coordlink call contract.current", got)
	}
}

func TestCommandCLIAdapterCommandPolicyDeniesUnauthorizedCommandsWithoutExecutorSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		binary    string
		startArgs []string
		forbidden []string
	}{
		{
			name:      "shell escape",
			binary:    "/bin/sh",
			startArgs: []string{"-lc", "/usr/local/bin/coordlink call contract.current"},
			forbidden: []string{"/usr/local/bin/coordlink call contract.current"},
		},
		{
			name:      "raw db",
			binary:    "/usr/bin/sqlite3",
			startArgs: []string{"/workspace/project/coordplane.db"},
			forbidden: []string{"coordplane.db"},
		},
		{
			name:      "token env dump",
			binary:    "/usr/bin/env",
			startArgs: []string{"COORDPLANE_TOKEN"},
			forbidden: []string{"COORDPLANE_TOKEN_DENY_SENTINEL"},
		},
		{
			name:      "docker",
			binary:    "/usr/bin/docker",
			startArgs: []string{"ps"},
			forbidden: []string{"docker"},
		},
		{
			name:      "network",
			binary:    "/usr/bin/curl",
			startArgs: []string{"https://example.invalid"},
			forbidden: []string{"https://example.invalid"},
		},
		{
			name:      "unauthorized coordlink capability",
			binary:    cpruntime.ContainerCoordlinkPath,
			startArgs: []string{"call", "command.run"},
			forbidden: []string{"command.run"},
		},
		{
			name:      "authorization header in argv",
			binary:    cpruntime.ContainerCoordlinkPath,
			startArgs: []string{"call", "contract.current", "--input", `{"header":"Authorization: Bearer SECRET_HEADER_SENTINEL"}`},
			forbidden: []string{"SECRET_HEADER_SENTINEL", "Authorization"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{ProcessRef: "should-not-run", ExitCode: 0}}}
			db, st, _, _ := newCommandCLIBase(t)
			insertAttemptOwnershipRows(t, ctx, db, "att_policy_deny", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_policy_deny")
			insertReadyRuntimeInstance(t, ctx, db, "rt_policy_deny", "att_policy_deny", "developer")
			adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
				Store: st,
				Profile: cpruntime.CommandCLIProfile{
					Name:       "policy-deny",
					Backend:    "coordlink",
					Binary:     tc.binary,
					StartArgs:  tc.startArgs,
					ResumeArgs: []string{"call", "contract.current"},
					Timeout:    timeSecond(),
					RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
						"docker-default": {
							NonInteractiveApproval:     true,
							AllowCoordlinkCapabilities: []string{"contract.current", "contract.add"},
						},
					},
				},
				Executor: exec,
			})
			if err != nil {
				t.Fatalf("new command adapter: %v", err)
			}
			env := runtimeEnv(t, "developer", "rt_policy_deny", "att_policy_deny")
			env["COORDPLANE_TOKEN"] = "COORDPLANE_TOKEN_DENY_SENTINEL"
			_, startErr := adapter.Start(ctx, cpruntime.StartRequest{
				AgentID:         "developer",
				AttemptID:       "att_policy_deny",
				AssignmentID:    "asg_missing_prompt",
				LeaseID:         "lease_missing_prompt",
				ContractID:      "ctr_missing_prompt",
				RuntimeID:       "rt_policy_deny",
				CLIBackend:      "coordlink",
				Workspace:       cpruntime.ContainerWorkspacePath,
				HomeDir:         cpruntime.ContainerHomePath,
				Env:             env,
				BootstrapPrompt: "policy denied command should not execute",
			})
			if startErr == nil || !strings.Contains(startErr.Error(), cpruntime.TerminalReasonCommandPolicyDenied) {
				t.Fatalf("Start error = %v, want command policy denial", startErr)
			}
			if len(exec.specs) != 0 {
				t.Fatalf("executor specs = %+v, want no exec for denied command", exec.specs)
			}
			cliSessions, err := cpruntime.ListCLISessions(ctx, db)
			if err != nil {
				t.Fatalf("list cli sessions: %v", err)
			}
			if len(cliSessions) != 0 {
				t.Fatalf("cli sessions = %+v, want no session side effect for denied command", cliSessions)
			}
			for _, marker := range tc.forbidden {
				if marker != "" && strings.Contains(startErr.Error(), marker) {
					t.Fatalf("denial leaked forbidden marker %q: %v", marker, startErr)
				}
			}
			if strings.Contains(startErr.Error(), "COORDPLANE_TOKEN_DENY_SENTINEL") {
				t.Fatalf("denial leaked runtime token: %v", startErr)
			}
		})
	}
}

func TestCommandCLIAdapterAppliesClaudeProviderPolicyAndRequiresAcceptedCapabilityCall(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "fake-claude-coordlink",
		ExitCode:   0,
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	exec.onExec = func(spec cpruntime.ContainerExecSpec) {
		insertAcceptedCapabilityCall(t, ctx, db, spec.Env["COORDPLANE_TRACE_ID"], "developer", "contract.current")
	}
	insertAttemptOwnershipRows(t, ctx, db, "att_provider_policy", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_provider_policy")
	insertReadyRuntimeInstance(t, ctx, db, "rt_provider_policy", "att_provider_policy", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
			RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
				"docker-default": {
					NonInteractiveApproval:     true,
					AllowCoordlinkCapabilities: []string{"contract.current", "contract.add"},
				},
			},
			AgentCapabilities: map[string][]string{
				"developer": {"contract.current"},
			},
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, err = adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_provider_policy",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_provider_policy",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_provider_policy", "att_provider_policy"),
		BootstrapPrompt: "provider uses allowlisted coordlink calls",
	})
	if err != nil {
		t.Fatalf("Start with provider policy: %v", err)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("executor specs = %+v, want one Claude provider exec", exec.specs)
	}
	command := exec.specs[0].Command
	joined := strings.Join(command, "\n")
	for _, want := range []string{
		"--safe-mode",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--permission-mode\ndontAsk",
		"--tools\nBash",
		"Bash(" + cpruntime.ContainerCoordlinkPath + " call contract.current *)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("provider command = %#v, missing %q", command, want)
		}
	}
	for _, forbidden := range []string{"bypassPermissions", "dangerously-skip-permissions", "Bash(*)", "Authorization", "Bearer"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("provider command leaked or widened policy with %q: %#v", forbidden, command)
		}
	}
	env := exec.specs[0].Env
	if env["COORDPLANE_PROVIDER_POLICY_MODE"] != "strict_coordlink_call" {
		t.Fatalf("provider policy mode env = %q, want strict_coordlink_call", env["COORDPLANE_PROVIDER_POLICY_MODE"])
	}
	if strings.Contains(joined, "call contract.add") {
		t.Fatalf("provider command widened beyond agent capabilities: %#v", command)
	}
	if env["COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES"] != "contract.current" {
		t.Fatalf("provider allowlist env = %q, want agent/profile intersection", env["COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES"])
	}
	if env["COORDPLANE_PROVIDER_AUDIT_TRACE_ID"] == "" ||
		env["COORDPLANE_PROVIDER_AUDIT_AGENT_ID"] != "developer" ||
		env["COORDPLANE_PROVIDER_AUDIT_LEASE_ID"] != "lease_missing_prompt" {
		t.Fatalf("provider audit env = trace:%q agent:%q lease:%q, want same trace/agent/lease audit anchors",
			env["COORDPLANE_PROVIDER_AUDIT_TRACE_ID"],
			env["COORDPLANE_PROVIDER_AUDIT_AGENT_ID"],
			env["COORDPLANE_PROVIDER_AUDIT_LEASE_ID"])
	}
	for _, forbidden := range []string{"COORDPLANE_TOKEN_ARGV_SENTINEL", "Authorization", "Bearer", "/home/", "/tmp/"} {
		if strings.Contains(env["COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES"], forbidden) {
			t.Fatalf("provider allowlist env leaked forbidden marker %q: %q", forbidden, env["COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES"])
		}
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "exited" {
		t.Fatalf("cli sessions = %+v, want exited provider-policy session", cliSessions)
	}
}

func TestCommandCLIAdapterRejectsProviderProgressWithoutMatchingLease(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "fake-claude-wrong-lease",
		ExitCode:   0,
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	exec.onExec = func(spec cpruntime.ContainerExecSpec) {
		insertAcceptedCapabilityCallWithLease(t, ctx, db, spec.Env["COORDPLANE_TRACE_ID"], "developer", "contract.current", "lease_attacker")
	}
	insertAttemptOwnershipRows(t, ctx, db, "att_policy_wrong_lease", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_policy_wrong_lease")
	insertReadyRuntimeInstance(t, ctx, db, "rt_policy_wrong_lease", "att_policy_wrong_lease", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
			RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
				"docker-default": {
					NonInteractiveApproval:     true,
					AllowCoordlinkCapabilities: []string{"contract.current", "contract.add"},
				},
			},
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, startErr := adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_policy_wrong_lease",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_policy_wrong_lease",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_policy_wrong_lease", "att_policy_wrong_lease"),
		BootstrapPrompt: "provider call with wrong lease must not count",
	})
	if startErr == nil || !strings.Contains(startErr.Error(), cpruntime.TerminalReasonApprovalPolicyUnavailable) {
		t.Fatalf("Start error = %v, want runtime approval unavailable for wrong lease audit", startErr)
	}
	if class, ok := cpruntime.ErrorFailureClass(startErr); !ok || class != cpruntime.FailureClassRuntimeApprovalBlocked {
		t.Fatalf("failure class = %s/%v, want runtime_approval_blocked", class, ok)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "failed" {
		t.Fatalf("cli sessions = %+v, want failed wrong-lease provider session", cliSessions)
	}
}

func TestCommandCLIAdapterFailsFastWhenClaudeProviderPolicyIsUnavailable(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "should-not-run",
		ExitCode:   0,
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_policy_unavailable", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_policy_unavailable")
	insertReadyRuntimeInstance(t, ctx, db, "rt_policy_unavailable", "att_policy_unavailable", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
			RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
				"docker-default": {NonInteractiveApproval: true},
			},
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, startErr := adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_policy_unavailable",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_policy_unavailable",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_policy_unavailable", "att_policy_unavailable"),
		BootstrapPrompt: "provider policy is unavailable",
	})
	if startErr == nil || !strings.Contains(startErr.Error(), cpruntime.TerminalReasonApprovalPolicyUnavailable) {
		t.Fatalf("Start error = %v, want runtime approval unavailable", startErr)
	}
	if class, ok := cpruntime.ErrorFailureClass(startErr); !ok || class != cpruntime.FailureClassRuntimeApprovalBlocked {
		t.Fatalf("failure class = %s/%v, want runtime_approval_blocked", class, ok)
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor specs = %+v, want no Claude exec without auditable provider approval config", exec.specs)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 0 {
		t.Fatalf("cli sessions = %+v, want no ordinary Claude session before provider policy is configured", cliSessions)
	}
}

func TestCommandCLIAdapterRejectsClaudeProviderExitWithoutAcceptedCapabilityCall(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "fake-claude-zero-without-side-effects",
		ExitCode:   0,
		Stdout:     []byte("no coordlink calls; Authorization: Bearer SECRET_HEADER_SENTINEL /home/zxh/private"),
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_policy_no_progress", "lease_missing_prompt", "asg_missing_prompt", "ctr_missing_prompt", "developer", "rt_policy_no_progress")
	insertReadyRuntimeInstance(t, ctx, db, "rt_policy_no_progress", "att_policy_no_progress", "developer")
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
			RuntimeCommandPolicies: map[string]cpruntime.RuntimeCommandPolicy{
				"docker-default": {
					NonInteractiveApproval:     true,
					AllowCoordlinkCapabilities: []string{"contract.current", "contract.add"},
				},
			},
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, startErr := adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_policy_no_progress",
		AssignmentID:    "asg_missing_prompt",
		LeaseID:         "lease_missing_prompt",
		ContractID:      "ctr_missing_prompt",
		RuntimeID:       "rt_policy_no_progress",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_policy_no_progress", "att_policy_no_progress"),
		BootstrapPrompt: "provider exits without allowlisted coordlink progress",
	})
	if startErr == nil || !strings.Contains(startErr.Error(), cpruntime.TerminalReasonApprovalPolicyUnavailable) {
		t.Fatalf("Start error = %v, want runtime approval unavailable", startErr)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("executor specs = %+v, want provider attempted once under scoped policy", exec.specs)
	}
	if class, ok := cpruntime.ErrorFailureClass(startErr); !ok || class != cpruntime.FailureClassRuntimeApprovalBlocked {
		t.Fatalf("failure class = %s/%v, want runtime_approval_blocked", class, ok)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "failed" ||
		!strings.Contains(cliSessions[0].LastError, cpruntime.TerminalReasonApprovalPolicyUnavailable) {
		t.Fatalf("cli sessions = %+v, want failed no-progress provider session", cliSessions)
	}
	for _, forbidden := range []string{"SECRET_HEADER_SENTINEL", "/home/zxh/private", "Do you want to proceed"} {
		if strings.Contains(startErr.Error(), forbidden) {
			t.Fatalf("approval policy error leaked %q: %v", forbidden, startErr)
		}
	}
}

func TestDockerExecClientAttachesStdinWhenProvided(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	stdinPath := filepath.Join(dir, "stdin.txt")
	envPathPath := filepath.Join(dir, "env-path.txt")
	envModePath := filepath.Join(dir, "env-mode.txt")
	envCopyPath := filepath.Join(dir, "env-copy.txt")
	dockerPath := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %s
prev=''
for arg in "$@"; do
  if [ "$prev" = "--env-file" ]; then
    printf '%%s' "$arg" > %s
    stat -c '%%a' "$arg" > %s
    cat "$arg" > %s
  fi
  prev="$arg"
done
cat > %s
printf 'ok'
`, argsPath, envPathPath, envModePath, envCopyPath, stdinPath)
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker binary: %v", err)
	}
	result, err := cpruntime.DockerExecClient{Binary: dockerPath}.Exec(context.Background(), cpruntime.ContainerExecSpec{
		ContainerName: "coordplane-test",
		Workdir:       cpruntime.ContainerWorkspacePath,
		HomeDir:       cpruntime.ContainerHomePath,
		Env: map[string]string{
			"COORDPLANE_AGENT_ID":  "developer",
			"COORDPLANE_TOKEN":     "COORDPLANE_TOKEN_ARGV_SENTINEL",
			"ANTHROPIC_AUTH_TOKEN": "ANTHROPIC_AUTH_TOKEN_ARGV_SENTINEL",
		},
		Command: []string{"sh", "-lc", "cat"},
		Stdin:   "bootstrap prompt over stdin\n",
		Timeout: timeSecond(),
	})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "ok" {
		t.Fatalf("docker exec result = %+v err=%v", result, err)
	}
	argsRaw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake docker args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")
	if len(args) < 2 || args[0] != "exec" || args[1] != "-i" {
		t.Fatalf("docker args = %q, want exec -i before workdir when stdin is present", argsRaw)
	}
	if !contains(args, "--env-file") {
		t.Fatalf("docker args = %q, want --env-file instead of --env KEY=value", argsRaw)
	}
	for _, forbidden := range []string{"COORDPLANE_TOKEN_ARGV_SENTINEL", "ANTHROPIC_AUTH_TOKEN_ARGV_SENTINEL", "bootstrap prompt over stdin"} {
		if strings.Contains(string(argsRaw), forbidden) {
			t.Fatalf("docker exec argv leaked %q: %s", forbidden, argsRaw)
		}
	}
	envMode, err := os.ReadFile(envModePath)
	if err != nil {
		t.Fatalf("read fake docker env-file mode: %v", err)
	}
	if strings.TrimSpace(string(envMode)) != "600" {
		t.Fatalf("env-file mode = %q, want 600", envMode)
	}
	envCopy, err := os.ReadFile(envCopyPath)
	if err != nil {
		t.Fatalf("read fake docker env-file copy: %v", err)
	}
	for _, want := range []string{"COORDPLANE_TOKEN=COORDPLANE_TOKEN_ARGV_SENTINEL", "ANTHROPIC_AUTH_TOKEN=ANTHROPIC_AUTH_TOKEN_ARGV_SENTINEL"} {
		if !strings.Contains(string(envCopy), want) {
			t.Fatalf("env-file copy = %q, missing %q", envCopy, want)
		}
	}
	envPath, err := os.ReadFile(envPathPath)
	if err != nil {
		t.Fatalf("read fake docker env-file path: %v", err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(envPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env-file path %q still exists or stat failed unexpectedly: %v", strings.TrimSpace(string(envPath)), err)
	}
	stdinRaw, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read fake docker stdin: %v", err)
	}
	if string(stdinRaw) != "bootstrap prompt over stdin\n" {
		t.Fatalf("docker stdin = %q, want prompt content", stdinRaw)
	}
}

func TestDockerExecClientCleansEnvFileWhenProcessFails(t *testing.T) {
	dir := t.TempDir()
	envPathPath := filepath.Join(dir, "env-path.txt")
	dockerPath := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
prev=''
for arg in "$@"; do
  if [ "$prev" = "--env-file" ]; then
    printf '%%s' "$arg" > %s
  fi
  prev="$arg"
done
exit 17
`, envPathPath)
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker binary: %v", err)
	}
	result, err := cpruntime.DockerExecClient{Binary: dockerPath}.Exec(context.Background(), cpruntime.ContainerExecSpec{
		ContainerName: "coordplane-test",
		Workdir:       cpruntime.ContainerWorkspacePath,
		HomeDir:       cpruntime.ContainerHomePath,
		Env:           map[string]string{"COORDPLANE_TOKEN": "COORDPLANE_TOKEN_FAILURE_SENTINEL"},
		Command:       []string{"false"},
		Timeout:       timeSecond(),
	})
	if err != nil || result.ExitCode != 17 {
		t.Fatalf("docker exec failure result = %+v err=%v, want captured process exit code", result, err)
	}
	envPath, err := os.ReadFile(envPathPath)
	if err != nil {
		t.Fatalf("read fake docker env-file path: %v", err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(envPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env-file path %q still exists or stat failed unexpectedly: %v", strings.TrimSpace(string(envPath)), err)
	}
}

func TestCommandCLIAdapterExitFailureFailsClosedWithoutRunningAttempt(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-failed",
		ExitCode:   2,
		Stderr:     []byte("auth missing"),
	}}}
	h := newCommandCLIHarness(t, exec)
	add := addContract(t, ctx, h.coordination, "developer")

	if _, err := h.runner.StartNext(ctx, "developer"); err == nil {
		t.Fatal("StartNext succeeded with failing CLI process")
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'running'"); got != 0 {
		t.Fatalf("running attempts = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'failed'"); got != 1 {
		t.Fatalf("failed attempts = %d, want 1", got)
	}
	if got := countActiveLeases(t, ctx, h.db, add.AssignmentID); got != 0 {
		t.Fatalf("active leases after CLI failure = %d, want released", got)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "failed" ||
		cliSessions[0].ExitCode == nil || *cliSessions[0].ExitCode != 2 ||
		!strings.HasPrefix(cliSessions[0].TranscriptRef, "obj_sha256_") {
		t.Fatalf("cli session after failure = %+v, want failed with transcript ref", cliSessions)
	}
}

func TestCommandCLIAdapterTimeoutPreservesDiagnosticTranscript(t *testing.T) {
	ctx := context.Background()
	timeoutErr := cpruntime.NewRuntimeExecTimeout("docker exec timed out after 10ms", context.DeadlineExceeded)
	exec := &recordingContainerExecutor{
		err: timeoutErr,
		errResult: cpruntime.ContainerExecResult{
			ProcessRef: "docker-exec:coordplane-timeout",
			ExitCode:   -1,
			Stdout:     []byte("partial provider stdout"),
			Stderr:     []byte("partial provider stderr"),
		},
	}
	h := newCommandCLIHarness(t, exec)
	add := addContract(t, ctx, h.coordination, "developer")

	_, err := h.runner.StartNext(ctx, "developer")
	if err == nil {
		t.Fatal("StartNext succeeded with timed out CLI process")
	}
	if class, ok := cpruntime.ErrorFailureClass(err); !ok || class != cpruntime.FailureClassRuntimeExecTimeout {
		t.Fatalf("failure class = %s/%v, want runtime_exec_timeout", class, ok)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StartNext error = %v, want deadline cause", err)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment after timeout = %s, want queued for retry", got)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 ||
		cliSessions[0].State != "failed" ||
		cliSessions[0].ProcessRef != "docker-exec:coordplane-timeout" ||
		cliSessions[0].ExitCode == nil || *cliSessions[0].ExitCode != -1 ||
		!strings.Contains(cliSessions[0].LastError, cpruntime.TerminalReasonRuntimeExecTimeout) ||
		!strings.HasPrefix(cliSessions[0].TranscriptRef, "obj_sha256_") {
		t.Fatalf("cli session after timeout = %+v, want failed timeout with process/ref diagnostics", cliSessions)
	}
	transcript := transcriptContentForAttempt(t, ctx, h.db, cliSessions[0].AttemptID)
	if !strings.Contains(transcript, "partial provider stdout") || !strings.Contains(transcript, "partial provider stderr") {
		t.Fatalf("timeout transcript = %q, want executor stdout/stderr diagnostics", transcript)
	}
}

func TestCommandCLIAdapterCancelledDeadlinePersistsDiagnosticsAndConvergesRunner(t *testing.T) {
	exec := &deadlineContainerExecutor{
		started: make(chan struct{}),
		result: cpruntime.ContainerExecResult{
			ProcessRef: "docker-exec:coordplane-cancelled-deadline",
			ExitCode:   -1,
			Stdout:     []byte("partial stdout before deadline"),
			Stderr:     []byte("partial stderr before deadline"),
		},
	}
	h := newCommandCLIHarness(t, exec)
	add := addContract(t, context.Background(), h.coordination, "developer")

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := h.runner.StartNext(deadlineCtx, "developer")
	if err == nil {
		t.Fatal("StartNext succeeded after execution deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StartNext error = %v, want deadline cause", err)
	}
	if class, ok := cpruntime.ErrorFailureClass(err); !ok || class != cpruntime.FailureClassRuntimeExecTimeout {
		t.Fatalf("failure class = %s/%v, want runtime_exec_timeout", class, ok)
	}
	select {
	case <-exec.started:
	default:
		t.Fatal("deadline expired before executor boundary")
	}

	ctx := context.Background()
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 ||
		cliSessions[0].State != "failed" ||
		cliSessions[0].ProcessRef != exec.result.ProcessRef ||
		cliSessions[0].ExitCode == nil || *cliSessions[0].ExitCode != -1 ||
		!strings.Contains(cliSessions[0].LastError, cpruntime.TerminalReasonRuntimeExecTimeout) ||
		!strings.HasPrefix(cliSessions[0].TranscriptRef, "obj_sha256_") {
		t.Fatalf("cli session after cancelled deadline = %+v, want failed with durable diagnostics", cliSessions)
	}
	transcript := transcriptContentForAttempt(t, ctx, h.db, cliSessions[0].AttemptID)
	if !strings.Contains(transcript, "partial stdout before deadline") || !strings.Contains(transcript, "partial stderr before deadline") {
		t.Fatalf("cancelled deadline transcript = %q, want partial stdout/stderr", transcript)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment after deadline = %s, want queued", got)
	}
	for _, check := range []struct {
		table string
		where string
		want  int
	}{
		{table: "attempts", where: "status = 'failed'", want: 1},
		{table: "leases", where: "state = 'released'", want: 1},
		{table: "session_routes", where: "state = 'failed'", want: 1},
		{table: "runtime_tokens", where: "state = 'active'", want: 0},
		{table: "runtime_tokens", where: "state = 'revoked'", want: 1},
		{table: "active_guards", where: "state = 'active'", want: 0},
		{table: "active_guards", where: "state = 'released'", want: 2},
		{table: "prepare_leases", where: "state = 'released'", want: 1},
	} {
		if got := countRowsWhere(t, ctx, h.db, check.table, check.where); got != check.want {
			t.Fatalf("%s where %s = %d, want %d", check.table, check.where, got, check.want)
		}
	}
}

func TestCommandCLIAdapterExecErrorIncludesDiagnosticPersistenceFailure(t *testing.T) {
	execFailure := errors.New("executor failed after partial output")
	var db *sql.DB
	exec := &recordingContainerExecutor{
		err: execFailure,
		errResult: cpruntime.ContainerExecResult{
			ProcessRef: "docker-exec:coordplane-persistence-failure",
			ExitCode:   -1,
			Stdout:     []byte("partial output"),
		},
		onExec: func(cpruntime.ContainerExecSpec) {
			if _, err := db.Exec(`
CREATE TRIGGER reject_cli_session_failure
BEFORE UPDATE ON cli_sessions
WHEN NEW.state = 'failed'
BEGIN
  SELECT RAISE(FAIL, 'forced cli diagnostic persistence failure');
END`); err != nil {
				t.Fatalf("install cli persistence failure trigger: %v", err)
			}
		},
	}
	h := newCommandCLIHarness(t, exec)
	db = h.db
	add := addContract(t, context.Background(), h.coordination, "developer")

	_, err := h.runner.StartNext(context.Background(), "developer")
	if !errors.Is(err, execFailure) {
		t.Fatalf("StartNext error = %v, want original executor error", err)
	}
	if !strings.Contains(err.Error(), "forced cli diagnostic persistence failure") {
		t.Fatalf("StartNext error = %v, want joined diagnostic persistence error", err)
	}
	if got := assignmentState(t, context.Background(), h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment after diagnostic persistence failure = %s, want queued", got)
	}
	if got := countRowsWhere(t, context.Background(), h.db, "attempts", "status = 'failed'"); got != 1 {
		t.Fatalf("failed attempts = %d, want 1", got)
	}
}

func TestCommandCLIAdapterMissingAuthProbeFailsClosedBeforeExecutor(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "should-not-run",
		ExitCode:   0,
	}}}
	db, st, _, _ := newCommandCLIBase(t)
	insertAttemptOwnershipRows(t, ctx, db, "att_missing_auth", "lease_missing_auth", "asg_missing_auth", "ctr_missing_auth", "developer", "rt_missing_auth")
	insertReadyRuntimeInstanceWithChecks(t, ctx, db, "rt_missing_auth", "att_missing_auth", "developer", `{"workspace_writable":true,"home_writable":true,"git_workspace_writable":true,"cli_user_consistent":true,"home_private":true,"home_persistent":true}`)
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	_, err = adapter.Start(ctx, cpruntime.StartRequest{
		AgentID:         "developer",
		AttemptID:       "att_missing_auth",
		AssignmentID:    "asg_missing_auth",
		LeaseID:         "lease_missing_auth",
		ContractID:      "ctr_missing_auth",
		RuntimeID:       "rt_missing_auth",
		CLIBackend:      "claude",
		Workspace:       cpruntime.ContainerWorkspacePath,
		HomeDir:         cpruntime.ContainerHomePath,
		Env:             runtimeEnv(t, "developer", "rt_missing_auth", "att_missing_auth"),
		BootstrapPrompt: "bootstrap without auth evidence",
	})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("Start error = %v, want typed missing auth rejection", err)
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor specs = %+v, want no docker exec before auth evidence", exec.specs)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "failed" || !strings.Contains(cliSessions[0].LastError, "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("cli sessions = %+v, want failed auth session", cliSessions)
	}
}

func TestCommandCLIAdapterRedactsClaudeAuthOutputOnLateFailure(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-auth-failed",
		ExitCode:   1,
		Stdout:     []byte("Not logged in · Please run /login\nSECRET_AUTH_STDOUT"),
	}}}
	h := newCommandCLIHarness(t, exec)
	add := addContract(t, ctx, h.coordination, "developer")

	if _, err := h.runner.StartNext(ctx, "developer"); err == nil || !strings.Contains(err.Error(), "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("StartNext error = %v, want typed auth failure", err)
	}
	if got := countRowsWhere(t, ctx, h.db, "attempts", "status = 'running'"); got != 0 {
		t.Fatalf("running attempts = %d, want 0 after auth failure", got)
	}
	if got := countActiveLeases(t, ctx, h.db, add.AssignmentID); got != 0 {
		t.Fatalf("active leases after auth failure = %d, want 0", got)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, h.db)
	if err != nil {
		t.Fatalf("list cli sessions: %v", err)
	}
	if len(cliSessions) != 1 || cliSessions[0].State != "failed" || !strings.Contains(cliSessions[0].LastError, "CLAUDE_AUTH_REQUIRED") {
		t.Fatalf("cli sessions = %+v, want failed auth session", cliSessions)
	}
	transcript := transcriptContentForAttempt(t, ctx, h.db, cliSessions[0].AttemptID)
	if strings.Contains(transcript, "Not logged in") || strings.Contains(transcript, "SECRET_AUTH_STDOUT") {
		t.Fatalf("auth failure transcript leaked raw auth output: %q", transcript)
	}
}

func TestCommandCLIRejectsHostRuntimeBeforeExecutor(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{}
	h := newExternalCommandCLIHarness(t, exec)
	add := addContract(t, ctx, h.coordination, "developer")

	if _, err := h.runner.StartNext(ctx, "developer"); err == nil {
		t.Fatal("StartNext succeeded with host external runtime for real CLI")
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor called for host runtime: %+v", exec.specs)
	}
	if got := countRowsWhere(t, ctx, h.db, "cli_sessions", "1 = 1"); got != 0 {
		t.Fatalf("cli_sessions after host rejection = %d, want 0", got)
	}
	if got := assignmentState(t, ctx, h.db, add.AssignmentID); got != "queued" {
		t.Fatalf("assignment after host rejection = %s, want queued", got)
	}
}

func TestRealClaudeCLIGateRequiresExplicitEnvironment(t *testing.T) {
	if os.Getenv("COORDPLANE_REAL_CLI_GATE") != "1" {
		t.Skip("set COORDPLANE_REAL_CLI_GATE=1 with COORDPLANE_CLAUDE_BIN and a Docker image containing that binary to run the real CLI gate")
	}
	claudeBin := os.Getenv("COORDPLANE_CLAUDE_BIN")
	image := os.Getenv("COORDPLANE_REAL_CLI_IMAGE")
	coordlinkPath := os.Getenv("COORDPLANE_COORDLINK_PATH")
	backendURL := os.Getenv("COORDPLANE_BACKEND_URL")
	if claudeBin == "" {
		t.Fatal("COORDPLANE_CLAUDE_BIN is required for the real CLI gate")
	}
	if image == "" {
		t.Fatal("COORDPLANE_REAL_CLI_IMAGE is required for the real CLI gate")
	}
	if coordlinkPath == "" {
		t.Fatal("COORDPLANE_COORDLINK_PATH is required for the real CLI gate")
	}
	if backendURL == "" {
		t.Fatal("COORDPLANE_BACKEND_URL is required for the real CLI gate")
	}

	ctx := context.Background()
	db, st, coordSvc, skillRegistry := newCommandCLIBase(t)
	defer cleanupRuntimeContainers(t, db)
	network := os.Getenv("COORDPLANE_DOCKER_NETWORK")
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-real-cli",
		TeamID:        "runtime-real-cli-gate",
		Image:         image,
		Network:       network,
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
	})
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  claudeBin,
			StartArgs: []string{
				"--session-id", "{{session_id}}",
				"--print",
				"--permission-mode", "bypassPermissions",
				"--allowedTools", "Bash",
				"{{prompt}}\nReal CLI gate: run /usr/local/bin/coordlink capability list and then summarize the typed response status.",
			},
			ResumeArgs: []string{
				"--resume", "{{session_id}}",
				"--print",
				"--permission-mode", "bypassPermissions",
				"--allowedTools", "Bash",
				"{{prompt}}\nReal CLI gate resume: run /usr/local/bin/coordlink skill list and then summarize the typed response status.",
			},
			Timeout: 3 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("new real command adapter: %v", err)
	}
	registry := cpruntime.NewCLIAdapterRegistry(db, []cpruntime.CLIAdapterRegistration{{Name: "claude", Kind: "command", Ready: true, Adapter: adapter}})
	cfg := commandCLITeamConfig("docker-real-cli")
	cfg.RuntimeProfiles = map[string]teamconfig.RuntimeProfile{
		"docker-real-cli": {Kind: "docker", Image: image, WorkspaceMode: "isolated"},
	}
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      cfg,
		Skills:          skillRegistry,
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-real-cli": dockerRuntime},
		Adapter:         registry,
		BackendURL:      backendURL,
		WorkspaceName:   "real-cli-gate",
	})
	if err != nil {
		t.Fatalf("new real CLI runner: %v", err)
	}
	addContract(t, ctx, coordSvc, "developer")
	session, err := runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("real CLI start failed: %v", err)
	}
	if _, err := runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "waiting",
		Summary:       "real CLI gate waiting before resume",
		TranscriptRef: "real-cli-start",
	}); err != nil {
		t.Fatalf("real CLI finish waiting: %v", err)
	}
	if _, err := runner.ResumeRoute(ctx, cpruntime.ResumeRouteInput{
		RouteID:    session.Route.ID,
		Reason:     "real_cli_gate",
		MailboxIDs: []string{"real_cli_gate_mailbox_signal"},
	}); err != nil {
		t.Fatalf("real CLI resume failed: %v", err)
	}
	if _, err := runner.FinishSession(ctx, cpruntime.TerminalReport{
		AttemptID:     session.AttemptID,
		Status:        "completed",
		Summary:       "real CLI gate completed",
		TranscriptRef: "real-cli-finished",
	}); err != nil {
		t.Fatalf("real CLI finish completed: %v", err)
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, db)
	if err != nil {
		t.Fatalf("list real CLI sessions: %v", err)
	}
	if len(cliSessions) < 2 {
		t.Fatalf("real CLI sessions = %+v, want start and resume evidence", cliSessions)
	}
}

type commandCLIHarness struct {
	db           *sql.DB
	store        *store.Store
	coordination *coordination.Service
	runner       *cpruntime.Runner
	docker       *recordingDockerClient
}

func newCommandCLIHarness(t *testing.T, exec cpruntime.ContainerExecutor) commandCLIHarness {
	t.Helper()
	db, st, coordSvc, skillRegistry := newCommandCLIBase(t)
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	docker := &recordingDockerClient{}
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-default",
		TeamID:        "runtime-command-cli-test",
		Image:         "alpine:3.20",
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
		Docker:        docker,
		AuthProbe:     &recordingClaudeAuthProbe{},
	})
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	registry := cpruntime.NewCLIAdapterRegistry(db, []cpruntime.CLIAdapterRegistration{{
		Name:    "claude",
		Kind:    "command",
		Ready:   true,
		Adapter: adapter,
	}})
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      commandCLITeamConfig("docker-default"),
		Skills:          skillRegistry,
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-default": dockerRuntime},
		Adapter:         registry,
		BackendURL:      "http://coordplane.test",
		WorkspaceName:   "test-workspace",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return commandCLIHarness{db: db, store: st, coordination: coordSvc, runner: runner, docker: docker}
}

func newExternalCommandCLIHarness(t *testing.T, exec cpruntime.ContainerExecutor) commandCLIHarness {
	t.Helper()
	db, st, coordSvc, skillRegistry := newCommandCLIBase(t)
	adapter, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
		Store: st,
		Profile: cpruntime.CommandCLIProfile{
			Name:    "claude",
			Backend: "claude",
			Binary:  "/usr/local/bin/claude",
			Timeout: timeSecond(),
		},
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("new command adapter: %v", err)
	}
	registry := cpruntime.NewCLIAdapterRegistry(db, []cpruntime.CLIAdapterRegistration{{Name: "claude", Kind: "command", Ready: true, Adapter: adapter}})
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:         st,
		Coordination:  coordSvc,
		TeamConfig:    commandCLITeamConfig("external-debug"),
		Skills:        skillRegistry,
		Runtime:       cpruntime.ExternalRuntime{ID: "external-debug", WorkspaceRoot: t.TempDir(), HomeRoot: t.TempDir(), Ready: true},
		Adapter:       registry,
		BackendURL:    "http://coordplane.test",
		WorkspaceName: "test-workspace",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return commandCLIHarness{db: db, store: st, coordination: coordSvc, runner: runner}
}

func newCommandCLIBase(t *testing.T) (*sql.DB, *store.Store, *coordination.Service, *skills.Registry) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register builtin skills: %v", err)
	}
	return db, st, coordination.NewService(st), skillRegistry
}

func commandCLITeamConfig(runtimeProfile string) teamconfig.Config {
	runtimeProfiles := map[string]teamconfig.RuntimeProfile{}
	if runtimeProfile == "docker-default" {
		runtimeProfiles[runtimeProfile] = teamconfig.RuntimeProfile{Kind: "docker", Image: "alpine:3.20", WorkspaceMode: "isolated"}
	}
	return teamconfig.Config{
		TeamID:          "runtime-command-cli-test",
		Version:         1,
		RuntimeProfiles: runtimeProfiles,
		Agents: []teamconfig.AgentConfig{{
			ID:             "developer",
			RolePrompt:     "developer role",
			RuntimeProfile: runtimeProfile,
			CLIBackend:     "claude",
			Skills:         []string{"coordplane-service"},
			Capabilities:   step5Capabilities(),
		}},
	}
}

type recordingContainerExecutor struct {
	err       error
	errResult cpruntime.ContainerExecResult
	results   []cpruntime.ContainerExecResult
	specs     []cpruntime.ContainerExecSpec
	onExec    func(cpruntime.ContainerExecSpec)
}

type deadlineContainerExecutor struct {
	started chan struct{}
	result  cpruntime.ContainerExecResult
}

func (e *deadlineContainerExecutor) Exec(ctx context.Context, _ cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	close(e.started)
	<-ctx.Done()
	return e.result, cpruntime.NewRuntimeExecTimeout("docker exec cancelled by execution deadline", ctx.Err())
}

func (e *recordingContainerExecutor) Exec(ctx context.Context, spec cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	select {
	case <-ctx.Done():
		return cpruntime.ContainerExecResult{}, ctx.Err()
	default:
	}
	e.specs = append(e.specs, cloneExecSpec(spec))
	if e.onExec != nil {
		e.onExec(cloneExecSpec(spec))
	}
	if e.err != nil {
		return e.errResult, e.err
	}
	if len(e.results) == 0 {
		return cpruntime.ContainerExecResult{}, errors.New("no fake exec result")
	}
	result := e.results[0]
	e.results = e.results[1:]
	return result, nil
}

func cloneExecSpec(spec cpruntime.ContainerExecSpec) cpruntime.ContainerExecSpec {
	cloned := spec
	cloned.Env = cloneStringMap(spec.Env)
	cloned.Command = append([]string(nil), spec.Command...)
	return cloned
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func timeSecond() time.Duration {
	return time.Second
}

func cleanupRuntimeContainers(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT container_name FROM runtime_instances WHERE container_name <> ''`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		_ = osexec.Command("docker", "rm", "-f", name).Run()
	}
}

func insertAttemptOwnershipRows(t *testing.T, ctx context.Context, db *sql.DB, attemptID, leaseID, assignmentID, contractID, agentID, runtimeID string) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO agents (
  id, role, runtime_kind, cli_backend, created_at, updated_at
) VALUES (?, 'developer', 'docker', 'claude', ?, ?)`,
		agentID, now, now,
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO work_contracts (
  id, title, objective, target_kind, target_id, status,
  completion_requirements_json, acceptance_policy_json, created_at, updated_at
) VALUES (?, 'missing prompt contract', 'verify prompt fail closed', 'agent', ?, 'assigned', '{}', '{}', ?, ?)`,
		contractID, agentID, now, now,
	); err != nil {
		t.Fatalf("insert work contract: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO assignments (
  id, contract_id, assignee_agent_id, state, reason, created_at, updated_at
) VALUES (?, ?, ?, 'claimed', 'missing-prompt-test', ?, ?)`,
		assignmentID, contractID, agentID, now, now,
	); err != nil {
		t.Fatalf("insert assignment: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO leases (
  id, assignment_id, agent_id, runtime_id, state, expires_at, created_at, updated_at
) VALUES (?, ?, ?, ?, 'active', ?, ?, ?)`,
		leaseID, assignmentID, agentID, runtimeID, now, now, now,
	); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO attempts (
  id, lease_id, cli_backend, runtime_kind, start_reason, status, started_at
) VALUES (?, ?, 'claude', 'docker', 'start', 'preparing', ?)`,
		attemptID, leaseID, now,
	); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func insertReadyRuntimeInstance(t *testing.T, ctx context.Context, db *sql.DB, runtimeID, attemptID, agentID string) {
	t.Helper()
	checks := `{"workspace_writable":true,"home_writable":true,"git_workspace_writable":true,"cli_user_consistent":true,"home_private":true,"home_persistent":true,"claude_present":true,"claude_auth_configured":true,"claude_auth_probe_passed":true,"claude_auth_probe_redacted":true}`
	insertReadyRuntimeInstanceWithChecks(t, ctx, db, runtimeID, attemptID, agentID, checks)
}

func insertReadyRuntimeInstanceWithChecks(t *testing.T, ctx context.Context, db *sql.DB, runtimeID, attemptID, agentID, checks string) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO runtime_instances (
  id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id,
  lease_id, container_id, container_name, image, network, state,
  workspace_path, home_path, checks_json, env_keys_json, created_at, updated_at
) VALUES (?, ?, 'docker-default', 'docker', ?, ?,
  'lease_missing_prompt', 'container_missing_prompt', 'coordplane-missing-prompt',
  'alpine:3.20', 'host', 'ready', ?, ?, ?, '[]', ?, ?)`,
		"rti_"+runtimeID, runtimeID, agentID, attemptID, cpruntime.ContainerWorkspacePath,
		cpruntime.ContainerHomePath, checks, now, now,
	); err != nil {
		t.Fatalf("insert ready runtime instance: %v", err)
	}
}

func runtimeEnv(t *testing.T, agentID, runtimeID, attemptID string) map[string]string {
	t.Helper()
	env, err := cpruntime.BuildRuntimeEnv(cpruntime.EnvironmentInput{
		BackendURL:    "http://coordplane.test",
		AgentID:       agentID,
		RuntimeID:     runtimeID,
		AttemptID:     attemptID,
		AssignmentID:  "asg_missing_prompt",
		LeaseID:       "lease_missing_prompt",
		Workspace:     cpruntime.ContainerWorkspacePath,
		CLIBackend:    "claude",
		TeamID:        "runtime-command-cli-test",
		WorkspaceName: "test-workspace",
	})
	if err != nil {
		t.Fatalf("build runtime env: %v", err)
	}
	return env
}

func insertAcceptedCapabilityCall(t *testing.T, ctx context.Context, db *sql.DB, traceID, agentID, capabilityName string) {
	t.Helper()
	insertAcceptedCapabilityCallWithLease(t, ctx, db, traceID, agentID, capabilityName, "lease_missing_prompt")
}

func insertAcceptedCapabilityCallWithLease(t *testing.T, ctx context.Context, db *sql.DB, traceID, agentID, capabilityName, leaseID string) {
	t.Helper()
	if traceID == "" {
		t.Fatal("trace id is required for accepted capability call audit")
	}
	scope := `{"lease_id":"` + leaseID + `"}`
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO capability_calls (
  id, tenant_id, trace_id, capability_name, subject_kind, subject_id,
  scope_json, status, idempotency_key, created_at
) VALUES (?, 'default', ?, ?, 'agent', ?, ?, 'accepted', '', ?)`,
		"capcall_"+capabilityName+"_"+traceID+"_"+leaseID, traceID, capabilityName, agentID, scope, now,
	); err != nil {
		t.Fatalf("insert accepted capability call: %v", err)
	}
}

func transcriptContentForAttempt(t *testing.T, ctx context.Context, db *sql.DB, attemptID string) string {
	t.Helper()
	var content []byte
	if err := db.QueryRowContext(ctx, `
SELECT ob.content
FROM transcripts tr
JOIN object_blobs ob ON ob.object_ref = tr.object_ref
WHERE tr.attempt_id = ?
ORDER BY tr.created_at DESC, tr.id DESC
LIMIT 1`, attemptID).Scan(&content); err != nil {
		t.Fatalf("query transcript content for %s: %v", attemptID, err)
	}
	return string(content)
}
