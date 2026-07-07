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

func TestCommandCLIAdapterStartsClaudeInsideDockerAndStoresTranscriptRef(t *testing.T) {
	ctx := context.Background()
	exec := &recordingContainerExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-start",
		ExitCode:   0,
		Stdout:     []byte(`{"status":"accepted"}`),
	}}}
	h := newCommandCLIHarness(t, exec)
	addContract(t, ctx, h.coordination, "developer")

	session, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start command CLI: %v", err)
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

	attempt := attemptRow(t, ctx, h.db, session.AttemptID)
	if attempt.CLIBackend != "claude" || attempt.RuntimeKind != "docker" ||
		attempt.Status != "running" || attempt.SessionNativeID == "" ||
		!strings.HasPrefix(attempt.TranscriptRef, "obj_sha256_") {
		t.Fatalf("attempt = %+v, want running claude docker attempt with object transcript ref", attempt)
	}
	route := routeRow(t, ctx, h.db, session.Route.ID)
	if route.CLIBackend != "claude" || route.SessionNativeID != attempt.SessionNativeID {
		t.Fatalf("route = %+v, want pinned claude native session", route)
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
		cli.SessionNativeID != attempt.SessionNativeID ||
		cli.TranscriptRef != attempt.TranscriptRef ||
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
	h := newCommandCLIHarness(t, exec)
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

func newCommandCLIHarness(t *testing.T, exec *recordingContainerExecutor) commandCLIHarness {
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

func newExternalCommandCLIHarness(t *testing.T, exec *recordingContainerExecutor) commandCLIHarness {
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
	err     error
	results []cpruntime.ContainerExecResult
	specs   []cpruntime.ContainerExecSpec
}

func (e *recordingContainerExecutor) Exec(ctx context.Context, spec cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	select {
	case <-ctx.Done():
		return cpruntime.ContainerExecResult{}, ctx.Err()
	default:
	}
	e.specs = append(e.specs, cloneExecSpec(spec))
	if e.err != nil {
		return cpruntime.ContainerExecResult{}, e.err
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
