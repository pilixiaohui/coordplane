package commandrun_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/adapters/httpapi"
	"coordplane/internal/capability"
	"coordplane/internal/commandrun"
	"coordplane/internal/coordination"
	"coordplane/internal/coordlinkcli"
	"coordplane/internal/objects"
	"coordplane/internal/policy"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/sessionauth"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestCommandRunExecutesInCurrentDockerWorkspaceAndCreatesEvidence(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-ok",
		ExitCode:   0,
		Stdout:     []byte("ok\n"),
	}}}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)

	result := callCommandRun(t, h.dispatcher, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
		IdempotencyKey: "cmd-ok",
		Input: raw(t, map[string]any{
			"argv": []string{"sh", "-lc", "printf ok"},
			"cwd":  ".",
		}),
	})
	if result.Status != "succeeded" || result.ExitCode != 0 ||
		result.CommandRunID == "" || result.EvidenceID == "" ||
		!strings.HasPrefix(result.StdoutRef, "obj_sha256_") ||
		!strings.HasPrefix(result.StderrRef, "obj_sha256_") {
		t.Fatalf("command.run result = %+v, want succeeded with durable refs", result)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(exec.specs))
	}
	spec := exec.specs[0]
	if spec.ContainerName == "" || spec.Workdir != cpruntime.ContainerWorkspacePath ||
		spec.HomeDir != cpruntime.ContainerHomePath {
		t.Fatalf("exec spec = %+v, want current Docker workspace", spec)
	}
	if strings.Join(spec.Command, "\x00") != "sh\x00-lc\x00printf ok" {
		t.Fatalf("exec command = %#v, want explicit argv", spec.Command)
	}
	if len(spec.Env) != 0 {
		t.Fatalf("exec env = %#v, want no coordplane token/env leakage by default", spec.Env)
	}
	read := h.objects.Read(ctx, agentSubject("developer", ""), result.StdoutRef)
	if read.Status != capability.StatusAccepted || read.Data == nil || read.Data.Content != "ok\n" {
		t.Fatalf("stdout object read = %+v, want captured output", read)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "status = 'succeeded'"); got != 1 {
		t.Fatalf("succeeded command_runs = %d, want 1", got)
	}
	runs, err := commandrun.ListRuns(ctx, h.db)
	if err != nil {
		t.Fatalf("list command runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != result.CommandRunID || runs[0].StdoutRef != result.StdoutRef {
		t.Fatalf("inspect command runs = %+v, want successful command run projection", runs)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'command_run' AND contract_id = '"+h.contractID+"'"); got != 1 {
		t.Fatalf("command_run evidence rows = %d, want 1", got)
	}
	if got := contractStatus(t, ctx, h.db, h.contractID); got != "open" {
		t.Fatalf("contract status after command.run = %s, want open", got)
	}
	for _, eventType := range []string{"command.run_requested", "command.exec_started", "command.output_captured", "command.succeeded", "evidence.command_run_recorded"} {
		if got := countRowsWhere(t, ctx, h.db, "events", "event_type = '"+eventType+"'"); got != 1 {
			t.Fatalf("%s events = %d, want 1", eventType, got)
		}
	}
}

func TestCommandRunThroughCoordlinkCLIHTTPBoundaryCreatesDurableEvidence(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-cli",
		ExitCode:   0,
		Stdout:     []byte("/workspace/project/subdir\n"),
	}}}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)
	token := runtimeTokenFromFakeStart(t, h)
	server := httptest.NewServer(httpapi.NewWithAuthenticator(h.dispatcher, sessionauth.New(h.db, "command.run")))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := coordlinkcli.Run(ctx, []string{
		"call", "command.run",
		"--lease-id", session.LeaseID,
		"--idempotency-key", "cli-http-command",
		"--input", `{"argv":["sh","-lc","pwd"],"cwd":"subdir","env":{"USER_FLAG":"1"},"max_output_bytes":128}`,
	}, mapEnv(map[string]string{
		"COORDPLANE_BACKEND_URL": server.URL,
		"COORDPLANE_AGENT_ID":    "developer",
		"COORDPLANE_RUNTIME_ID":  session.Route.RuntimeID,
		"COORDPLANE_TOKEN":       token,
	}), strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("coordlink command.run exit = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	response := decodeRawResponse(t, stdout.Bytes())
	if response.Status != capability.StatusAccepted || !response.OK || response.Data == nil {
		t.Fatalf("coordlink command.run response = %+v, want accepted", response)
	}
	var result commandrun.Result
	if err := json.Unmarshal(*response.Data, &result); err != nil {
		t.Fatalf("decode command.run result: %v\nraw=%s", err, string(*response.Data))
	}
	if result.Status != "succeeded" || result.ExitCode != 0 || result.EvidenceID == "" ||
		!strings.HasPrefix(result.StdoutRef, "obj_sha256_") {
		t.Fatalf("command.run result = %+v, want succeeded with evidence/object refs", result)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(exec.specs))
	}
	spec := exec.specs[0]
	if spec.Workdir != cpruntime.ContainerWorkspacePath+"/subdir" ||
		spec.Env["USER_FLAG"] != "1" ||
		spec.Env["COORDPLANE_TOKEN"] != "" ||
		spec.ContainerName == "" {
		t.Fatalf("exec spec = %+v, want scoped container workdir and allowlisted env only", spec)
	}
	read := h.objects.Read(ctx, agentSubject("developer", ""), result.StdoutRef)
	if read.Status != capability.StatusAccepted || read.Data == nil || !strings.Contains(read.Data.Content, "/workspace/project/subdir") {
		t.Fatalf("stdout object read = %+v, want captured command output", read)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "idempotency_key = 'cli-http-command' AND status = 'succeeded'"); got != 1 {
		t.Fatalf("command_runs = %d, want one durable CLI command run", got)
	}
	runs, err := commandrun.ListRuns(ctx, h.db)
	if err != nil {
		t.Fatalf("list command runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != result.CommandRunID || runs[0].CWD != "subdir" {
		t.Fatalf("inspect command runs = %+v, want coordlink command run projection", runs)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'command_run' AND content_ref = 'command_run:"+result.CommandRunID+"'"); got != 1 {
		t.Fatalf("command_run evidence rows = %d, want 1", got)
	}
	if got := contractStatus(t, ctx, h.db, h.contractID); got != "open" {
		t.Fatalf("contract status after coordlink command.run = %s, want open", got)
	}
}

func TestCommandRunPublicBoundaryRejectsForgedOrMissingRuntimeToken(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{}
	h := newHarness(t, exec, true)
	developer := h.startSessionFor(t, ctx, "developer")
	developerToken := runtimeTokenForAgent(t, h, "developer")
	verifier := h.startSessionFor(t, ctx, "verifier")
	verifierToken := runtimeTokenForAgent(t, h, "verifier")
	server := httptest.NewServer(httpapi.NewWithAuthenticator(h.dispatcher, sessionauth.New(h.db, "command.run")))
	defer server.Close()

	validCall := capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", developer.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": developer.LeaseID}),
		Input:          raw(t, map[string]any{"argv": []string{"true"}}),
	}
	cases := []struct {
		name    string
		token   string
		call    capability.Call
		headers map[string]string
		code    string
	}{
		{
			name:  "missing token",
			call:  validCall,
			code:  "AUTH_TOKEN_REQUIRED",
			token: "",
		},
		{
			name:  "wrong token",
			call:  validCall,
			code:  "AUTH_TOKEN_REJECTED",
			token: "tok_wrong",
		},
		{
			name:  "other agent token with leaked active lease",
			token: verifierToken,
			call: capability.Call{
				CapabilityName: "command.run",
				Subject:        agentSubject("verifier", verifier.Route.RuntimeID),
				Scope:          raw(t, map[string]any{"lease_id": developer.LeaseID}),
				Input:          raw(t, map[string]any{"argv": []string{"true"}}),
			},
			code: "AUTH_SCOPE_MISMATCH",
		},
		{
			name:  "body subject forged",
			token: developerToken,
			call: capability.Call{
				CapabilityName: "command.run",
				Subject:        agentSubject("verifier", developer.Route.RuntimeID),
				Scope:          raw(t, map[string]any{"lease_id": developer.LeaseID}),
				Input:          raw(t, map[string]any{"argv": []string{"true"}}),
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
		{
			name:  "body runtime forged",
			token: developerToken,
			call: capability.Call{
				CapabilityName: "command.run",
				Subject:        agentSubject("developer", "rt_docker_forged"),
				Scope:          raw(t, map[string]any{"lease_id": developer.LeaseID}),
				Input:          raw(t, map[string]any{"argv": []string{"true"}}),
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
		{
			name:  "header runtime forged",
			token: developerToken,
			call:  validCall,
			headers: map[string]string{
				"X-CoordPlane-Runtime-ID": "rt_docker_header_forged",
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postCallHTTP(t, server.URL, tc.token, tc.call, tc.headers)
			if response.Status != capability.StatusRejected || response.ErrorCode != tc.code {
				t.Fatalf("response = %+v, want rejected %s", response, tc.code)
			}
			if response.Data != nil {
				t.Fatalf("rejected auth response leaked data: %+v", response)
			}
		})
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor called for rejected public auth requests: %+v", exec.specs)
	}
	assertNoCommandRunSideEffects(t, ctx, h.db)
}

func TestCommandRunPublicBoundaryIgnoresInputIdentityOverridesAndInspectStaysRedacted(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-override",
		ExitCode:   0,
		Stdout:     []byte("SECRET_OUTPUT\n"),
	}}}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)
	token := runtimeTokenForAgent(t, h, "developer")
	instance := runtimeInstanceForRoute(t, ctx, h.db, session.Route.RuntimeID)
	server := httptest.NewServer(httpapi.NewWithAuthenticator(h.dispatcher, sessionauth.New(h.db, "command.run")))
	defer server.Close()

	response := postCallHTTP(t, server.URL, token, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{}),
		Input: raw(t, map[string]any{
			"lease_id":       session.LeaseID,
			"argv":           []string{"sh", "-lc", "printf ok"},
			"cwd":            ".",
			"container_id":   "evil-container-id",
			"container_name": "evil-container",
			"runtime_id":     "rt_docker_evil",
			"attempt_id":     "att_evil",
			"host_path":      "/var/run/docker.sock",
			"db_path":        "/tmp/backend.db",
		}),
	}, nil)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("response = %+v, want accepted command.run", response)
	}
	var result commandrun.Result
	if err := json.Unmarshal(*response.Data, &result); err != nil {
		t.Fatalf("decode command.run result: %v", err)
	}
	if result.RuntimeID != session.Route.RuntimeID || result.AttemptID != session.AttemptID {
		t.Fatalf("result = %+v, want DB-derived runtime/attempt", result)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(exec.specs))
	}
	if exec.specs[0].ContainerName != instance.ContainerName ||
		exec.specs[0].Workdir != cpruntime.ContainerWorkspacePath {
		t.Fatalf("exec spec = %+v, want DB-derived container/workdir", exec.specs[0])
	}
	runs, err := commandrun.ListRuns(ctx, h.db)
	if err != nil {
		t.Fatalf("list command runs: %v", err)
	}
	if len(runs) != 1 || runs[0].RuntimeID != session.Route.RuntimeID ||
		runs[0].AttemptID != session.AttemptID ||
		runs[0].ContainerName != instance.ContainerName {
		t.Fatalf("command runs = %+v, want DB-derived identity", runs)
	}
	inspectJSON := mustJSON(t, runs)
	for _, forbidden := range []string{
		token,
		"SECRET_OUTPUT",
		"evil-container",
		"rt_docker_evil",
		"att_evil",
		"/var/run/docker.sock",
		"/tmp/backend.db",
	} {
		if strings.Contains(inspectJSON, forbidden) {
			t.Fatalf("command run inspect leaked forbidden value %q: %s", forbidden, inspectJSON)
		}
	}
}

func TestCommandRunRejectsUnauthorizedAndInvalidScopeBeforeExec(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{}
	h := newHarness(t, exec, false)
	session := h.startSession(t, ctx)

	unauthorized := coordlink.New(h.dispatcher).Call(ctx, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
		Input:          raw(t, map[string]any{"argv": []string{"true"}}),
	})
	if unauthorized.Status != capability.StatusRejected || unauthorized.ErrorCode != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("unauthorized response = %+v, want policy rejected", unauthorized)
	}

	h = newHarness(t, exec, true)
	session = h.startSession(t, ctx)
	wrongLease := coordlink.New(h.dispatcher).Call(ctx, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": "lease_other"}),
		Input:          raw(t, map[string]any{"argv": []string{"true"}}),
	})
	if wrongLease.Status != capability.StatusRejected || wrongLease.ErrorCode != "COMMAND_SCOPE_REJECTED" {
		t.Fatalf("wrong lease response = %+v, want COMMAND_SCOPE_REJECTED", wrongLease)
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor called on rejected requests: %+v", exec.specs)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "1 = 1"); got != 0 {
		t.Fatalf("command_runs after rejected scope = %d, want 0", got)
	}
}

func TestCommandRunRejectsUnsafeCWDEnvAndDenylistedCommand(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)

	cases := []struct {
		name  string
		input map[string]any
		code  string
	}{
		{
			name:  "cwd escape",
			input: map[string]any{"argv": []string{"true"}, "cwd": "../outside"},
			code:  "COMMAND_CWD_REJECTED",
		},
		{
			name:  "forbidden env",
			input: map[string]any{"argv": []string{"true"}, "env": map[string]string{"COORDPLANE_TOKEN": "override"}},
			code:  "COMMAND_ENV_REJECTED",
		},
		{
			name:  "denylisted binary",
			input: map[string]any{"argv": []string{"docker", "ps"}},
			code:  "COMMAND_ARGV_REJECTED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := coordlink.New(h.dispatcher).Call(ctx, capability.Call{
				CapabilityName: "command.run",
				Subject:        agentSubject("developer", session.Route.RuntimeID),
				Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
				Input:          raw(t, tc.input),
			})
			if response.Status != capability.StatusRejected || response.ErrorCode != tc.code {
				t.Fatalf("response = %+v, want rejected %s", response, tc.code)
			}
		})
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor called for rejected commands: %+v", exec.specs)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "1 = 1"); got != 0 {
		t.Fatalf("command_runs after policy rejections = %d, want 0", got)
	}
}

func TestCommandRunIdempotencyDoesNotReexecute(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{
		{ProcessRef: "first", ExitCode: 0, Stdout: []byte("first")},
		{ProcessRef: "second", ExitCode: 0, Stdout: []byte("second")},
	}}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)
	call := capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
		IdempotencyKey: "same-key",
		Input:          raw(t, map[string]any{"argv": []string{"printf", "first"}}),
	}

	first := callCommandRun(t, h.dispatcher, call)
	second := callCommandRun(t, h.dispatcher, call)
	if first.CommandRunID != second.CommandRunID || first.StdoutRef != second.StdoutRef {
		t.Fatalf("idempotent results differ: first=%+v second=%+v", first, second)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec calls = %d, want 1 after duplicate idempotency key", len(exec.specs))
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "idempotency_key = 'same-key'"); got != 1 {
		t.Fatalf("idempotent command_runs = %d, want 1", got)
	}
}

func TestCommandRunCapturesNonZeroTimeoutAndOutputCap(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{
		{ProcessRef: "nonzero", ExitCode: 2, Stdout: []byte("abcdef"), Stderr: []byte("problem")},
	}, err: context.DeadlineExceeded}
	h := newHarness(t, exec, true)
	session := h.startSession(t, ctx)

	nonzero := callCommandRun(t, h.dispatcher, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
		Input: raw(t, map[string]any{
			"argv":             []string{"sh", "-lc", "exit 2"},
			"max_output_bytes": 3,
		}),
	})
	if nonzero.Status != "failed" || nonzero.ExitCode != 2 || nonzero.StdoutBytes != 3 || !nonzero.StdoutTruncated {
		t.Fatalf("nonzero result = %+v, want failed exit 2 with truncated stdout", nonzero)
	}
	read := h.objects.Read(ctx, agentSubject("developer", ""), nonzero.StdoutRef)
	if read.Status != capability.StatusAccepted || read.Data == nil || read.Data.Content != "abc" {
		t.Fatalf("truncated stdout read = %+v, want abc", read)
	}

	timedOut := callCommandRun(t, h.dispatcher, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", session.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": session.LeaseID}),
		Input: raw(t, map[string]any{
			"argv":            []string{"sleep", "10"},
			"timeout_seconds": 1,
		}),
	})
	if timedOut.Status != "timed_out" || timedOut.ExitCode != -1 {
		t.Fatalf("timeout result = %+v, want timed_out exit -1", timedOut)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "status = 'failed'"); got != 1 {
		t.Fatalf("failed command_runs = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "status = 'timed_out'"); got != 1 {
		t.Fatalf("timed_out command_runs = %d, want 1", got)
	}
}

func TestCommandRunRealContainerCoordlinkGateRequiresExplicitEnvironment(t *testing.T) {
	if os.Getenv("COORDPLANE_COMMAND_RUN_GATE") != "1" {
		t.Skip("set COORDPLANE_COMMAND_RUN_GATE=1 with COORDPLANE_COORDLINK_PATH and Docker available to run the real coordlink command.run gate")
	}
	coordlinkPath := os.Getenv("COORDPLANE_COORDLINK_PATH")
	if coordlinkPath == "" {
		t.Fatal("COORDPLANE_COORDLINK_PATH is required for the real coordlink command.run gate")
	}
	if _, err := osexec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	network := os.Getenv("COORDPLANE_DOCKER_NETWORK")
	dispatcher := &deferredDispatcher{}
	authenticator := sessionauth.New(nil, "command.run")
	server := httptest.NewServer(httpapi.NewWithAuthenticator(dispatcher, authenticator))
	defer server.Close()

	h := newHarnessWithDocker(t, cpruntime.DockerExecClient{}, true, nil, image, network, coordlinkPath, server.URL)
	dispatcher.inner = h.dispatcher
	authenticator.SetDB(h.db)
	t.Cleanup(func() {
		cleanupCommandRunContainers(t, h.db)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	session := h.startSession(t, ctx)
	instance := runtimeInstanceForRoute(t, ctx, h.db, session.Route.RuntimeID)

	outer := cpruntime.DockerExecClient{}
	result, err := outer.Exec(ctx, cpruntime.ContainerExecSpec{
		ContainerName: instance.ContainerName,
		Workdir:       cpruntime.ContainerWorkspacePath,
		HomeDir:       cpruntime.ContainerHomePath,
		Command: []string{
			cpruntime.ContainerCoordlinkPath,
			"call", "command.run",
			"--idempotency-key", "real-container-coordlink-command",
			"--input", `{"argv":["sh","-lc","printf real-container-coordlink && pwd"],"max_output_bytes":256}`,
		},
		Timeout: 30 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("container coordlink command.run failed: exit=%d err=%v stdout=%s stderr=%s", result.ExitCode, err, result.Stdout, result.Stderr)
	}
	response := decodeRawResponse(t, result.Stdout)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("container coordlink command.run response = %+v stdout=%s stderr=%s", response, result.Stdout, result.Stderr)
	}
	var commandResult commandrun.Result
	if err := json.Unmarshal(*response.Data, &commandResult); err != nil {
		t.Fatalf("decode command.run result: %v\nraw=%s", err, string(*response.Data))
	}
	if commandResult.Status != "succeeded" || commandResult.StdoutRef == "" || commandResult.EvidenceID == "" {
		t.Fatalf("command.run result = %+v, want succeeded with object/evidence refs", commandResult)
	}
	read := h.objects.Read(ctx, agentSubject("developer", ""), commandResult.StdoutRef)
	if read.Status != capability.StatusAccepted || read.Data == nil || !strings.Contains(read.Data.Content, "real-container-coordlink") {
		t.Fatalf("stdout object read = %+v, want real coordlink command output", read)
	}
	if got := countRowsWhere(t, ctx, h.db, "command_runs", "idempotency_key = 'real-container-coordlink-command'"); got != 1 {
		t.Fatalf("command_runs = %d, want one real coordlink command run", got)
	}
}

type harness struct {
	db           *sql.DB
	store        *store.Store
	coordination *coordination.Service
	dispatcher   *policy.Dispatcher
	objects      *objects.Store
	runner       *cpruntime.Runner
	fake         *cpruntime.FakeCLIAdapter
	contractID   string
}

func newHarness(t *testing.T, exec cpruntime.ContainerExecutor, grantCommandRun bool) harness {
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	return newHarnessWithDocker(t, exec, grantCommandRun, &recordingDockerClient{}, "alpine:3.20", "", coordlinkPath, "http://coordplane.test")
}

func newHarnessWithDocker(t *testing.T, exec cpruntime.ContainerExecutor, grantCommandRun bool, docker cpruntime.DockerClient, image, network, coordlinkPath, backendURL string) harness {
	t.Helper()
	ctx := context.Background()
	if image == "" {
		image = "alpine:3.20"
	}
	if backendURL == "" {
		backendURL = "http://coordplane.test"
	}
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
		t.Fatalf("register builtins: %v", err)
	}
	coordSvc := coordination.NewService(st)
	commandSvc, err := commandrun.NewService(commandrun.Config{Store: st, Executor: exec})
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, coordSvc); err != nil {
		t.Fatalf("register coordination: %v", err)
	}
	if err := commandrun.RegisterCapabilities(registry, commandSvc); err != nil {
		t.Fatalf("register commandrun: %v", err)
	}
	cfg := teamConfig(grantCommandRun)
	dispatcher := policy.NewDispatcher(cfg, registry)
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-default",
		TeamID:        "commandrun-test",
		Image:         image,
		Network:       network,
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
		Docker:        docker,
	})
	fake := cpruntime.NewFakeCLIAdapter()
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      cfg,
		Skills:          skillRegistry,
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-default": dockerRuntime},
		Adapter:         fake,
		BackendURL:      backendURL,
		WorkspaceName:   "commandrun-test",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return harness{
		db:           db,
		store:        st,
		coordination: coordSvc,
		dispatcher:   dispatcher,
		objects:      objects.NewStore(st),
		runner:       runner,
		fake:         fake,
	}
}

func (h *harness) startSession(t *testing.T, ctx context.Context) cpruntime.AssignmentSession {
	t.Helper()
	return h.startSessionFor(t, ctx, "developer")
}

func (h *harness) startSessionFor(t *testing.T, ctx context.Context, agentID string) cpruntime.AssignmentSession {
	t.Helper()
	add, err := h.coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         agentID + " command task",
		Objective:     "run a bounded command",
		TargetAgentID: agentID,
	})
	if err != nil {
		t.Fatalf("add contract: %v", err)
	}
	h.contractID = add.ContractID
	session, err := h.runner.StartNext(ctx, agentID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return session
}

func teamConfig(grantCommandRun bool) teamconfig.Config {
	caps := []string{"assignment.next", "contract.current"}
	if grantCommandRun {
		caps = append(caps, "command.run")
	}
	return teamconfig.Config{
		TeamID:  "commandrun-test",
		Version: 1,
		RuntimeProfiles: map[string]teamconfig.RuntimeProfile{
			"docker-default": {Kind: "docker", Image: "alpine:3.20", WorkspaceMode: "isolated"},
		},
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "developer",
				RolePrompt:     "developer role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   caps,
			},
			{
				ID:             "verifier",
				RolePrompt:     "verifier role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   caps,
			},
		},
	}
}

type recordingExecutor struct {
	err     error
	results []cpruntime.ContainerExecResult
	specs   []cpruntime.ContainerExecSpec
}

func (e *recordingExecutor) Exec(ctx context.Context, spec cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	e.specs = append(e.specs, cloneSpec(spec))
	if e.err != nil && len(e.results) == 0 {
		return cpruntime.ContainerExecResult{}, e.err
	}
	if len(e.results) == 0 {
		return cpruntime.ContainerExecResult{ProcessRef: "default", ExitCode: 0}, nil
	}
	result := e.results[0]
	e.results = e.results[1:]
	return result, nil
}

type recordingDockerClient struct{}

func (c *recordingDockerClient) PrepareContainer(ctx context.Context, spec cpruntime.DockerContainerSpec) (cpruntime.DockerContainerResult, error) {
	return cpruntime.DockerContainerResult{
		ContainerID: "container-" + spec.ContainerName,
		Checks: map[string]bool{
			"backend_reachable":      true,
			"workspace_writable":     true,
			"home_writable":          true,
			"git_workspace_writable": true,
			"cli_user_consistent":    true,
		},
	}, nil
}

type deferredDispatcher struct {
	inner *policy.Dispatcher
}

func (d *deferredDispatcher) Handle(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	if d.inner == nil {
		return capability.Error[json.RawMessage]("TEST_DISPATCHER_NOT_READY", "dispatcher is not ready", true)
	}
	return d.inner.Handle(ctx, call)
}

func (d *deferredDispatcher) ListForSubject(ctx context.Context, subject capability.Subject) capability.Response[json.RawMessage] {
	if d.inner == nil {
		return capability.Error[json.RawMessage]("TEST_DISPATCHER_NOT_READY", "dispatcher is not ready", true)
	}
	return d.inner.ListForSubject(ctx, subject)
}

func cloneSpec(spec cpruntime.ContainerExecSpec) cpruntime.ContainerExecSpec {
	cloned := spec
	cloned.Command = append([]string(nil), spec.Command...)
	cloned.Env = make(map[string]string, len(spec.Env))
	for key, value := range spec.Env {
		cloned.Env[key] = value
	}
	return cloned
}

func runtimeInstanceForRoute(t *testing.T, ctx context.Context, db *sql.DB, runtimeID string) cpruntime.RuntimeInstance {
	t.Helper()
	instances, err := cpruntime.ListRuntimeInstances(ctx, db)
	if err != nil {
		t.Fatalf("list runtime instances: %v", err)
	}
	for _, instance := range instances {
		if instance.RuntimeID == runtimeID {
			return instance
		}
	}
	t.Fatalf("runtime instance %s not found in %+v", runtimeID, instances)
	return cpruntime.RuntimeInstance{}
}

func cleanupCommandRunContainers(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT container_name FROM runtime_instances WHERE container_name <> ''`)
	if err != nil {
		t.Logf("list runtime containers for cleanup: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var containerName string
		if err := rows.Scan(&containerName); err != nil {
			t.Logf("scan runtime container for cleanup: %v", err)
			continue
		}
		_ = osexec.Command("docker", "rm", "-f", containerName).Run()
	}
	if err := rows.Err(); err != nil {
		t.Logf("iterate runtime containers for cleanup: %v", err)
	}
}

func callCommandRun(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) commandrun.Result {
	t.Helper()
	response := coordlink.New(dispatcher).Call(context.Background(), call)
	if response.Status != capability.StatusAccepted || !response.OK || response.Data == nil {
		t.Fatalf("command.run response = %+v, want accepted", response)
	}
	var out commandrun.Result
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		t.Fatalf("decode command.run result: %v\nraw=%s", err, string(*response.Data))
	}
	return out
}

func postCallHTTP(t *testing.T, serverURL, token string, call capability.Call, headers map[string]string) capability.Response[json.RawMessage] {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, serverURL+"/call", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post /call: %v", err)
	}
	defer resp.Body.Close()
	var response capability.Response[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode /call response: %v", err)
	}
	return response
}

func decodeRawResponse(t *testing.T, raw []byte) capability.Response[json.RawMessage] {
	t.Helper()
	var response capability.Response[json.RawMessage]
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode response: %v\nraw=%s", err, string(raw))
	}
	return response
}

func runtimeTokenFromFakeStart(t *testing.T, h harness) string {
	t.Helper()
	starts := h.fake.Starts()
	if len(starts) == 0 {
		t.Fatal("no fake CLI starts recorded")
	}
	token := starts[len(starts)-1].Env["COORDPLANE_TOKEN"]
	if token == "" {
		t.Fatalf("fake CLI start missing COORDPLANE_TOKEN: %+v", starts[len(starts)-1].Env)
	}
	return token
}

func runtimeTokenForAgent(t *testing.T, h harness, agentID string) string {
	t.Helper()
	starts := h.fake.Starts()
	for i := len(starts) - 1; i >= 0; i-- {
		if starts[i].AgentID == agentID {
			token := starts[i].Env["COORDPLANE_TOKEN"]
			if token == "" {
				t.Fatalf("fake CLI start for %s missing COORDPLANE_TOKEN: %+v", agentID, starts[i].Env)
			}
			return token
		}
	}
	t.Fatalf("no fake CLI start recorded for %s", agentID)
	return ""
}

func assertNoCommandRunSideEffects(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for table, where := range map[string]string{
		"command_runs": "1 = 1",
		"object_blobs": "1 = 1",
		"evidence":     "kind = 'command_run'",
		"events":       "aggregate_type = 'command_run' OR capability_name = 'command.run' OR event_type LIKE 'command.%' OR event_type = 'evidence.command_run_recorded'",
	} {
		if got := countRowsWhere(t, ctx, db, table, where); got != 0 {
			t.Fatalf("%s side effects = %d, want 0", table, got)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func agentSubject(agentID, runtimeID string) capability.Subject {
	return capability.Subject{Kind: "agent", ID: agentID, AgentID: agentID, RuntimeID: runtimeID}
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return out
}

func contractStatus(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&status); err != nil {
		t.Fatalf("query contract status: %v", err)
	}
	return status
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}
