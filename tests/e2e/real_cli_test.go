//go:build e2e

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

const (
	liveE2ETimeout          = 15 * time.Minute
	liveDiagnosticTailBytes = 8 << 10
	realClaudeVersion       = "2.1.126 (Claude Code)"
	realProviderEnv         = "ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL"
	realTokenCanary         = "real-gate-auth-token-canary"
	realAPIKeyCanary        = "real-gate-api-key-canary"
)

func TestRealCLIGateRejectsMutableAndScriptedImagesBeforeLiveTests(t *testing.T) {
	releaseLiveDocker(t)
	dockerConfig := t.TempDir()
	build := exec.Command("docker", "build", "-q", "testdata/runtime")
	build.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	raw, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build scripted runtime: %v\n%s", err, raw)
	}
	scripted := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		remove := exec.Command("docker", "image", "rm", "-f", scripted)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		_ = remove.Run()
	})
	tests := []struct{ name, image, want string }{
		{name: "mutable tag", image: "agent:latest", want: "explicit immutable sha256 image is required"},
		{name: "missing digest", image: "sha256:" + strings.Repeat("0", 64), want: "runtime image identity does not match"},
		{name: "scripted fixture", image: scripted, want: "runtime image must contain Claude Code 2.1.126"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(filepath.Clean("../../scripts/e2e-real-cli.sh"))
			command.Env = append(os.Environ(), "E2E_RUNTIME_IMAGE="+test.image, "E2E_PROVIDER_ENV_ALLOWLIST="+realProviderEnv, "E2E_EXPECTED_PROVIDER_ENV="+realProviderEnv, "ANTHROPIC_AUTH_TOKEN="+realTokenCanary, "ANTHROPIC_API_KEY="+realAPIKeyCanary)
			output, runErr := realGateOutput(t, command)
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 77 || !strings.Contains(string(output), test.want) || strings.Contains(string(output), "PASS(") {
				t.Fatalf("gate err=%v output=%s", runErr, output)
			}
		})
	}

	realGo, err := exec.LookPath("go")
	testsupport.RequireNoError(t, err)
	stubDir, fixture := t.TempDir(), filepath.Join(t.TempDir(), "go-test.json")
	testsupport.WriteFile(t, filepath.Join(stubDir, "docker"), []byte("#!/bin/sh\ncase \"$1\" in version) exit 0;; image) printf '%s\\n' \"$5\";; run) printf '%s\\n' '"+realClaudeVersion+"';; *) exit 2;; esac\n"), 0o700)
	testsupport.WriteFile(t, filepath.Join(stubDir, "make"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	testsupport.WriteFile(t, filepath.Join(stubDir, "go"), []byte("#!/bin/sh\ncase \"$1\" in test) [ \"$E2E_PROVIDER_ENV_ALLOWLIST\" = \"$E2E_EXPECTED_PROVIDER_ENV\" ] || exit 4; cat \"$E2E_GATE_FIXTURE\";; run) exec \"$E2E_REAL_GO\" \"$@\";; *) exit 2;; esac\n"), 0o700)
	const smoke, agents = "TestRealClaudeAdapterSmoke", "TestRealClaudeTwoAgentConvergence"
	exact := `{"Action":"run","Test":"` + smoke + `"}` + "\n" + `{"Action":"run","Test":"` + smoke + `/resume"}` + "\n" + `{"Action":"pass","Test":"` + smoke + `/resume"}` + "\n" + `{"Action":"pass","Test":"` + smoke + `"}` + "\n" + `{"Action":"run","Test":"` + agents + `"}` + "\n" + `{"Action":"pass","Test":"` + agents + `"}`
	mutants := []struct {
		name, input string
		pass        bool
	}{
		{name: "exact set", input: exact, pass: true},
		{name: "zero matches", input: `{"Action":"pass"}`},
		{name: "missing test", input: `{"Action":"run","Test":"` + smoke + `"}` + "\n" + `{"Action":"pass","Test":"` + smoke + `"}`},
		{name: "top skip", input: exact + "\n" + `{"Action":"skip","Test":"` + agents + `"}`},
		{name: "child skip", input: exact + "\n" + `{"Action":"skip","Test":"` + smoke + `/child"}`},
		{name: "failure", input: `{"Action":"fail","Test":"` + smoke + `"}`},
		{name: "unexpected top level", input: exact + `
{"Action":"run","Test":"TestRenamedLiveGate"}`},
		{name: "duplicate run", input: exact + `
{"Action":"run","Test":"` + agents + `"}`},
	}
	for _, test := range mutants {
		t.Run("checker/"+test.name, func(t *testing.T) {
			testsupport.WriteFile(t, fixture, []byte(test.input), 0o600)
			command := exec.Command(filepath.Clean("../../scripts/e2e-real-cli.sh"))
			command.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"E2E_RUNTIME_IMAGE=sha256:"+strings.Repeat("a", 64), "E2E_PROVIDER_ENV_ALLOWLIST="+realProviderEnv, "E2E_EXPECTED_PROVIDER_ENV="+realProviderEnv, "ANTHROPIC_AUTH_TOKEN="+realTokenCanary, "ANTHROPIC_API_KEY="+realAPIKeyCanary,
				"HTTP_PROXY=proxy-canary", "HTTPS_PROXY=proxy-canary", "ALL_PROXY=proxy-canary", "NO_PROXY=proxy-canary", "E2E_GATE_FIXTURE="+fixture, "E2E_REAL_GO="+realGo)
			output, runErr := realGateOutput(t, command)
			if (runErr == nil) != test.pass || (!test.pass && (!strings.Contains(string(output), "real gate evidence:") || strings.Contains(string(output), "PASS("))) {
				t.Fatalf("shell gate err=%v want_pass=%t output=%s", runErr, test.pass, output)
			}
		})
	}
}

func TestRealCLIGatePreservesFailureDiagnosticsBeforeCleanupWithoutProvider(t *testing.T) {
	names := strings.Split(realProviderEnv, ",")
	var canaries []string
	for index, name := range names {
		value := fmt.Sprintf(" quoted\"slash\\%d \nfragment-%d\t tail-%d ", index, index, index)
		t.Setenv(name, value)
		for _, candidate := range append([]string{value, strings.TrimSpace(value)}, strings.Fields(value)...) {
			digest := sha256.Sum256([]byte(candidate))
			escaped := fmt.Sprintf("%q", candidate)
			canaries = append(canaries, candidate, escaped[1:len(escaped)-1], hex.EncodeToString(digest[:]), strings.ToUpper(hex.EncodeToString(digest[:])))
		}
	}
	leaked := strings.Join(canaries, " | ")
	fixtureRoot, dataDir := t.TempDir(), filepath.Join(t.TempDir(), "data")
	taskCurrent := core.TaskDetail{Task: core.Task{ID: "task-integration", Status: core.TaskFailed, FailureReason: "AUTH_FAILURE " + leaked}, CurrentRun: &core.Run{ID: "run-current", TaskID: "task-integration", State: core.RunActive, CleanupState: "pending", RuntimeErrorCode: "CURRENT " + leaked}}
	taskHistory := core.TaskDetail{Task: core.Task{ID: "task-history", Status: core.TaskFailed, FailureReason: "HISTORY_FAILURE " + leaked}}
	taskUnproven := core.TaskDetail{Task: core.Task{ID: "task-unproven", Status: core.TaskFailed, FailureReason: "UNPROVEN_FAILURE " + leaked}}
	pageOne := core.RunPage{Items: []core.RunSummary{{ID: "run-old", TaskID: "task-history", State: core.RunFailed}}, NextCursor: "page-two"}
	pageTwo := core.RunPage{Items: []core.RunSummary{{ID: "run-newest", TaskID: "task-history", State: core.RunFailed}}}
	repeatedPage := core.RunPage{Items: []core.RunSummary{{ID: "run-unproven", TaskID: "task-unproven", State: core.RunFailed}}, NextCursor: "repeat"}
	exitCode := 23
	newest := core.Run{ID: "run-newest", TaskID: "task-history", State: core.RunFailed, ExitCode: &exitCode, CleanupState: "removed", TerminalReason: "PROTOCOL_FAILURE " + leaked, RuntimeErrorCode: "PROVIDER_ERROR", LastError: "quoted " + leaked}
	paths := make([]string, 7)
	for index, value := range []any{taskCurrent, taskHistory, taskUnproven, pageOne, pageTwo, repeatedPage, newest} {
		paths[index] = filepath.Join(fixtureRoot, fmt.Sprintf("fixture-%d.json", index))
		raw, err := json.Marshal(value)
		requireNoError(t, err)
		testsupport.WriteFile(t, paths[index], raw, 0o600)
	}
	binary := filepath.Join(fixtureRoot, "coordplane")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1:$2:$3\" in\n task:show:task-integration) cat %q;;\n task:show:task-history) cat %q;;\n task:show:task-unproven) cat %q;;\n run:list:*) case \"$4:$5:$6:$7:$8\" in\n  task-history:--limit:500:--socket:*) cat %q;;\n  task-history:--limit:500:--cursor:page-two) cat %q;;\n  task-unproven:--limit:500:--socket:*) cat %q;;\n  task-unproven:--limit:500:--cursor:repeat) cat %q;;\n  *) exit 3;; esac;;\n run:show:run-newest) cat %q;;\n *) exit 2;;\nesac\n", paths[0], paths[1], paths[2], paths[3], paths[4], paths[5], paths[5], paths[6])
	testsupport.WriteFile(t, binary, []byte(script), 0o700)
	for _, runID := range []string{"run-current", "run-newest"} {
		logPath := filepath.Join(dataDir, "logs", runID, "run.log")
		requireNoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
		testsupport.WriteFile(t, logPath, []byte("discarded-marker\n"+strings.Repeat("x", liveDiagnosticTailBytes)+"diagnostic-marker "+leaked), 0o600)
	}
	boundaryPath := filepath.Join(fixtureRoot, "boundary.log")
	if liveDiagnosticTailBytes != 8192 {
		t.Fatalf("live diagnostic tail limit = %d, want 8192", liveDiagnosticTailBytes)
	}
	for _, size := range []int{8191, 8192, 8193} {
		content := []byte("A" + strings.Repeat("x", size-2) + "Z")
		testsupport.WriteFile(t, boundaryPath, content, 0o600)
		got, err := readLiveRunLogTail(boundaryPath)
		requireNoError(t, err)
		want := content[max(0, len(content)-8192):]
		if got != string(want) || len(got) != min(size, 8192) {
			t.Fatalf("tail size=%d got=%d want=%d", size, len(got), len(want))
		}
	}

	tracked := []string{"task-history", "task-unproven"}
	_ = waitForLiveIntegration(t, context.Background(), binary, "unused.sock", func(ids ...string) { tracked = append(tracked, ids...) }, core.Task{IntegrationTaskID: "task-integration"}, "dynamic integration diagnostic", time.Second, func(task core.Task) bool { return task.Status == core.TaskFailed })
	var evidence string
	requireNoError(t, preserveLiveFailureDiagnostics(
		func() string {
			return liveFailureDiagnostics(context.Background(), binary, "unused.sock", dataDir, names, tracked...)
		},
		func(value string) {
			evidence = value
			if _, err := os.Stat(filepath.Join(dataDir, "logs", "run-current", "run.log")); err != nil {
				t.Fatal("failure evidence was emitted after cleanup")
			}
		},
		func() error { return os.RemoveAll(dataDir) },
	))
	for _, want := range []string{"task=task-integration", "run=run-current source=current state=active", "task=task-history", "run=run-newest source=history state=failed exit_code=23", "task=task-unproven", "run_selection_error=", "run_log_tail=", "diagnostic-marker", "[REDACTED_SECRET]"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("failure evidence omitted %q", want)
		}
	}
	for _, canary := range canaries {
		if strings.Contains(evidence, canary) {
			t.Fatalf("failure evidence leaked provider credential material of length %d", len(canary))
		}
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) || strings.Contains(evidence, "run-old") || strings.Contains(evidence, "run-unproven") || strings.Contains(evidence, "discarded-marker") {
		t.Fatal("failure evidence was not retained before data-dir cleanup")
	}
}

func realGateOutput(t *testing.T, command *exec.Cmd) ([]byte, error) {
	t.Helper()
	output, err := command.CombinedOutput()
	if strings.Contains(string(output), realTokenCanary) || strings.Contains(string(output), realAPIKeyCanary) {
		t.Fatal("real gate output leaked a credential canary")
	}
	return output, err
}

func registerLiveFailureDiagnostics(t *testing.T, binary, socket, dataDir string, providerEnv []string, stop func() error) func(...string) {
	t.Helper()
	var taskIDs []string
	t.Cleanup(func() {
		var err error
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = preserveLiveFailureDiagnostics(
				func() string { return liveFailureDiagnostics(ctx, binary, socket, dataDir, providerEnv, taskIDs...) },
				func(value string) { t.Logf("LIVE_FAILURE_DIAGNOSTICS\n%s", value) }, stop,
			)
		} else {
			err = stop()
		}
		if err != nil {
			t.Errorf("stop live daemon: %v", err)
		}
	})
	return func(ids ...string) { taskIDs = append(taskIDs, ids...) }
}

func preserveLiveFailureDiagnostics(collect func() string, emit func(string), cleanup func() error) error {
	emit(collect())
	return cleanup()
}

func liveFailureDiagnostics(ctx context.Context, binary, socket, dataDir string, providerEnv []string, taskIDs ...string) string {
	var evidence strings.Builder
	redact := func(value string) string { return redactLiveDiagnostics(value, dataDir, providerEnv) }
	for _, taskID := range taskIDs {
		detail, taskErr := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
		fmt.Fprintf(&evidence, "task=%s status=%s current_run=%t failure_reason=%q query_error=%t\n", redact(taskID), redact(string(detail.Task.Status)), detail.CurrentRun != nil, redact(detail.Task.FailureReason), taskErr != nil)
		if taskErr != nil {
			continue
		}
		run, source, selectionErr := liveDiagnosticRun(ctx, binary, socket, taskID, detail.CurrentRun)
		if selectionErr != nil {
			fmt.Fprintf(&evidence, "run_selection_error=%q\n", redact(selectionErr.Error()))
			continue
		}
		exitCode := "null"
		if run.ExitCode != nil {
			exitCode = fmt.Sprint(*run.ExitCode)
		}
		tail, tailErr := readLiveRunLogTail(filepath.Join(dataDir, "logs", run.ID, "run.log"))
		fmt.Fprintf(&evidence, "run=%s source=%s state=%s exit_code=%s cleanup=%s runtime_error=%q reason=%q last_error=%q\nrun_log_tail=%q error=%t\n", redact(run.ID), redact(source), redact(string(run.State)), exitCode, redact(run.CleanupState), redact(run.RuntimeErrorCode), redact(run.TerminalReason), redact(run.LastError), redact(tail), tailErr != nil)
	}
	return redact(evidence.String())
}

func liveDiagnosticRun(ctx context.Context, binary, socket, taskID string, current *core.Run) (core.Run, string, error) {
	if current != nil {
		return *current, "current", nil
	}
	cursor := ""
	var latest core.RunSummary
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		args := []string{"run", "list", "--task", taskID, "--limit", "500"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		args = append(args, "--socket", socket, "--output", "json")
		page, err := commandJSON[core.RunPage](ctx, binary, args...)
		if err != nil {
			return core.Run{}, "history", fmt.Errorf("list Run history: %w", err)
		}
		for _, candidate := range page.Items {
			if core.IsRunTerminal(candidate.State) {
				latest = candidate
			}
		}
		if page.NextCursor == "" {
			if latest.ID == "" {
				return core.Run{}, "history", errors.New("Run history has no terminal Run")
			}
			run, err := commandJSON[core.Run](ctx, binary, "run", "show", latest.ID, "--socket", socket, "--output", "json")
			return run, "history", err
		}
		if page.NextCursor == cursor {
			return core.Run{}, "history", errors.New("Run history cursor did not advance")
		}
		cursor = page.NextCursor
	}
	return core.Run{}, "history", errors.New("Run history exceeds 20 pages")
}

func readLiveRunLogTail(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if len(raw) > liveDiagnosticTailBytes {
		raw = raw[len(raw)-liveDiagnosticTailBytes:]
	}
	return string(raw), err
}

func redactLiveDiagnostics(value, dataDir string, providerEnv []string) string {
	var forbidden []string
	for _, name := range providerEnv {
		secret := os.Getenv(name)
		if secret == "" {
			continue
		}
		for _, candidate := range append([]string{secret, strings.TrimSpace(secret)}, strings.Fields(secret)...) {
			if candidate == "" {
				continue
			}
			sum := sha256.Sum256([]byte(candidate))
			digest := hex.EncodeToString(sum[:])
			forbidden = append(forbidden, candidate, digest, strings.ToUpper(digest))
		}
	}
	sort.Slice(forbidden, func(left, right int) bool { return len(forbidden[left]) > len(forbidden[right]) })
	for _, item := range forbidden {
		value = strings.ReplaceAll(value, item, "[REDACTED_SECRET]")
	}
	if dataDir != "" {
		value = strings.ReplaceAll(value, dataDir, "[REDACTED_DATA_DIR]")
	}
	return value
}

func waitForLiveIntegration(t *testing.T, ctx context.Context, binary, socket string, track func(...string), source core.Task, reason string, timeout time.Duration, predicate func(core.Task) bool) core.Task {
	t.Helper()
	integrationID := source.IntegrationTaskID
	track(integrationID)
	return waitForTaskWithin(t, ctx, binary, socket, integrationID, reason, timeout, predicate)
}

func TestRealClaudeAdapterSmoke(t *testing.T) {
	requireRealCLI(t)
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	requireExecutable(t, "E2E_COORDLINK_BIN")
	image, network, providerEnv := liveRuntimeConfig(t)
	releaseLiveDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveE2ETimeout)
	defer cancel()
	root := t.TempDir()
	source, _ := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	registerLiveHomeCleanup(t, image, dataDir)
	instructions := filepath.Join(root, "smoke-instructions.md")
	testsupport.WriteFile(t, instructions, []byte(realSmokeInstructions), 0o600)
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane-live.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 1, CompletedWorkspace: "24h", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: network, DefaultImage: image, ProviderEnv: providerEnv, Tail: "  run_timeout: 12m\n  shutdown_grace: 5s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	trackFailure := registerLiveFailureDiagnostics(t, coordplane, socket, dataDir, providerEnv, func() error { return daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "live smoke daemon startup")
	agent := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--id", "agt_live_smoke", "--display-name", "Live smoke Agent",
		"--adapter", "claude", "--image", image, "--instructions-file", instructions,
		"--request-id", "live-smoke-agent", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "Live adapter smoke", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agent.ID,
		"--request-id", "live-smoke-project", "--output", "json")

	first := runJSON[core.ChatResult](t, ctx, coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", "LIVE-SMOKE-ONE", "--request-id", "live-smoke-one", "--output", "json")
	trackFailure(first.Task.ID)
	runOne, homeOne := waitForLiveRun(t, ctx, coordplane, socket, first.Task.ID, "", "LIVE-SMOKE-READY", 5*time.Minute)
	sendBossMessage(t, ctx, coordplane, socket, project.ID, agent.ID, first.Task.ID, "LIVE-SMOKE-FINISH-ONE", "live-smoke-finish-one")
	waitForTaskWithin(t, ctx, coordplane, socket, first.Task.ID, "first live wait", 5*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskWaiting && task.CurrentRunID == ""
	})
	runOne = runJSON[core.Run](t, ctx, coordplane, "run", "show", runOne.ID, "--socket", socket, "--output", "json")
	if runOne.NativeSessionID == "" || runOne.LaunchMode != "start" || !core.IsRunTerminal(runOne.State) {
		t.Fatalf("first live Run did not capture a resumable session: %#v", runOne)
	}

	second := runJSON[core.ChatResult](t, ctx, coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", "LIVE-SMOKE-TWO", "--request-id", "live-smoke-two", "--output", "json")
	if second.Task.ID != first.Task.ID {
		t.Fatalf("conversation Task changed across wake: first=%s second=%s", first.Task.ID, second.Task.ID)
	}
	runTwo, homeTwo := waitForLiveRun(t, ctx, coordplane, socket, first.Task.ID, runOne.ID, "LIVE-SMOKE-READY", 5*time.Minute)
	if runTwo.LaunchMode != "resume" || runTwo.ResumedFromRunID != runOne.ID ||
		runTwo.ResumeNativeSessionID != runOne.NativeSessionID || homeTwo != homeOne {
		t.Fatalf("resume did not preserve Task session/home: first=%#v second=%#v homes=%q/%q", runOne, runTwo, homeOne, homeTwo)
	}
	sendBossMessage(t, ctx, coordplane, socket, project.ID, agent.ID, first.Task.ID, "LIVE-SMOKE-FINISH-TWO", "live-smoke-finish-two")
	waitForTaskWithin(t, ctx, coordplane, socket, first.Task.ID, "resumed live wait", 5*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskWaiting && task.CurrentRunID == ""
	})

	runJSON[core.ChatResult](t, ctx, coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", "LIVE-SMOKE-CANCEL", "--request-id", "live-smoke-cancel", "--output", "json")
	runThree, _ := waitForLiveRun(t, ctx, coordplane, socket, first.Task.ID, runTwo.ID, "LIVE-CANCEL-READY", 5*time.Minute)
	runJSON[core.Task](t, ctx, coordplane,
		"task", "cancel", first.Task.ID, "--socket", socket, "--reason", "live cancellation assertion",
		"--request-id", "live-smoke-task-cancel", "--output", "json")
	waitForTaskWithin(t, ctx, coordplane, socket, first.Task.ID, "cancelled live conversation", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCancelled && task.CurrentRunID == ""
	})
	eventually(t, ctx, 2*time.Minute, "cancelled live Run cleanup", func() (core.Run, bool, string) {
		run, err := commandJSON[core.Run](ctx, coordplane, "run", "show", runThree.ID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		return run, run.State == core.RunCancelled && run.CleanupState == "removed", fmt.Sprintf("state=%s cleanup=%s", run.State, run.CleanupState)
	})
	waitForNoProjectContainers(t, ctx, project.ID)
	assertAgentBossMessages(t, ctx, coordplane, socket, project.ID, agent.ID, 2)
}

func TestRealClaudeTwoAgentConvergence(t *testing.T) {
	requireRealCLI(t)
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	requireExecutable(t, "E2E_COORDLINK_BIN")
	image, network, providerEnv := liveRuntimeConfig(t)
	releaseLiveDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveE2ETimeout)
	defer cancel()
	root := t.TempDir()
	source, initialSHA := createSourceRepository(t, ctx, root)
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	registerLiveHomeCleanup(t, image, dataDir)
	instructions := filepath.Join(root, "live-instructions.md")
	testsupport.WriteFile(t, instructions, []byte(realWorkInstructions), 0o600)
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane-live.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 2, CompletedWorkspace: "24h", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: network, DefaultImage: image, ProviderEnv: providerEnv, Tail: "  run_timeout: 12m\n  shutdown_grace: 5s\ngit:\n  capture_helper_image: " + image + "\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	trackFailure := registerLiveFailureDiagnostics(t, coordplane, socket, dataDir, providerEnv, func() error { return daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "live E2E daemon startup")
	agentA := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--id", "agt_live_a", "--display-name", "Live Agent A",
		"--adapter", "claude", "--image", image, "--instructions-file", instructions,
		"--request-id", "live-agent-a", "--output", "json")
	agentB := runJSON[core.Agent](t, ctx, coordplane,
		"agent", "add", "--socket", socket, "--id", "agt_live_b", "--display-name", "Live Agent B",
		"--adapter", "claude", "--image", image, "--instructions-file", instructions,
		"--request-id", "live-agent-b", "--output", "json")
	project := runJSON[core.Project](t, ctx, coordplane,
		"project", "add", "--socket", socket, "--name", "Real Claude two-Agent E2E", "--repo", source,
		"--ref", "refs/heads/main", "--integration-agent", agentA.ID,
		"--request-id", "live-project", "--output", "json")
	if project.InitialSHA != initialSHA || project.CanonicalSHA != initialSHA {
		t.Fatalf("live Project did not register C0: %#v want=%s", project, initialSHA)
	}
	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane,
			"agent", "pause", agent.ID, "--socket", socket, "--request-id", "live-pause-"+agent.ID, "--output", "json")
	}
	taskA := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agentA.ID,
		"--title", "Live work A", "--description", "live_role=A;peer_agent_id="+agentB.ID,
		"--request-id", "live-task-a", "--output", "json")
	taskB := runJSON[core.Task](t, ctx, coordplane,
		"task", "create", "--socket", socket, "--project", project.ID, "--agent", agentB.ID,
		"--title", "Live work B", "--description", "live_role=B;peer_agent_id="+agentA.ID,
		"--request-id", "live-task-b", "--output", "json")
	trackFailure(taskA.ID, taskB.ID)
	if taskA.BaseSHA != initialSHA || taskB.BaseSHA != initialSHA {
		t.Fatalf("live work Tasks do not share C0: A=%s B=%s C0=%s", taskA.BaseSHA, taskB.BaseSHA, initialSHA)
	}
	for _, agent := range []core.Agent{agentA, agentB} {
		runJSON[core.Agent](t, ctx, coordplane,
			"agent", "resume", agent.ID, "--socket", socket, "--request-id", "live-resume-"+agent.ID, "--output", "json")
	}
	runA, runB := waitForConcurrentRunsWithProgress(t, ctx, coordplane, socket, taskA.ID, taskB.ID, "LIVE-READY", 5*time.Minute)
	assertIsolatedRuns(t, dataDir, runA, runB, inspectContainer(t, ctx, runA.ContainerID), inspectContainer(t, ctx, runB.ContainerID))
	sendBossMessage(t, ctx, coordplane, socket, project.ID, agentA.ID, taskA.ID, "LIVE-GO A", "live-go-a")
	sendBossMessage(t, ctx, coordplane, socket, project.ID, agentB.ID, taskB.ID, "LIVE-GO B", "live-go-b")

	taskA = waitForTaskWithin(t, ctx, coordplane, socket, taskA.ID, "live A submission", 7*time.Minute, capturedSubmission)
	taskB = waitForTaskWithin(t, ctx, coordplane, socket, taskB.ID, "live B submission", 7*time.Minute, capturedSubmission)
	if taskA.HeadSHA == taskB.HeadSHA || taskA.HeadRunID != runA.ID || taskB.HeadRunID != runB.ID {
		t.Fatalf("live captures are not distinct/fenced: A=%#v B=%#v", taskA, taskB)
	}
	checkout := filepath.Join(root, "review-a")
	fact := runJSON[core.GitCheckoutFact](t, ctx, coordplane,
		"task", "checkout", taskA.ID, "--socket", socket, "--dest", checkout, "--output", "json")
	if fact.HeadSHA != taskA.HeadSHA || git(t, ctx, checkout, "rev-parse", "HEAD") != taskA.HeadSHA {
		t.Fatalf("live Boss checkout did not return exact A head: %#v task=%#v", fact, taskA)
	}
	assertFile(t, filepath.Join(checkout, "agent-A.txt"), "agent-A\n")

	runJSON[core.Task](t, ctx, coordplane,
		"task", "accept", taskA.ID, "--socket", socket, "--integration-agent", agentA.ID,
		"--request-id", "live-accept-a", "--output", "json")
	taskA = waitForTaskWithin(t, ctx, coordplane, socket, taskA.ID, "live A direct CAS", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == task.HeadSHA
	})
	runJSON[core.Task](t, ctx, coordplane,
		"task", "accept", taskB.ID, "--socket", socket, "--integration-agent", agentA.ID,
		"--request-id", "live-accept-b", "--output", "json")
	taskB = waitForTaskWithin(t, ctx, coordplane, socket, taskB.ID, "live B stale link", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskSubmitted && task.IntegrationTaskID != ""
	})
	integration := waitForLiveIntegration(t, ctx, coordplane, socket, trackFailure, taskB, "live integration", 7*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.HeadSHA != "" && task.FinalCanonicalSHA == task.HeadSHA
	})
	taskB = waitForTaskWithin(t, ctx, coordplane, socket, taskB.ID, "live B completion", 2*time.Minute, func(task core.Task) bool {
		return task.Status == core.TaskCompleted && task.FinalCanonicalSHA == integration.HeadSHA
	})
	if integration.SourceTaskID != taskB.ID || integration.SourceHeadSHA != taskB.HeadSHA ||
		integration.SourceTaskRef != taskB.TaskRef || integration.SourceRunID != taskB.HeadRunID {
		t.Fatalf("live integration lost B source identity: integration=%#v B=%#v", integration, taskB)
	}

	controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
	finalSHA := gitDir(t, ctx, controlRepo, "rev-parse", project.CanonicalRef)
	if finalSHA != integration.HeadSHA {
		t.Fatalf("live final canonical = %s, want integration head %s", finalSHA, integration.HeadSHA)
	}
	for _, ancestor := range []string{initialSHA, taskA.HeadSHA, taskB.HeadSHA} {
		gitDirSucceeds(t, ctx, controlRepo, "merge-base", "--is-ancestor", ancestor, finalSHA)
	}
	for _, task := range []core.Task{taskA, taskB, integration} {
		if got := gitDir(t, ctx, controlRepo, "rev-parse", task.TaskRef); got != task.HeadSHA {
			t.Fatalf("live task ref %s = %s, want %s", task.TaskRef, got, task.HeadSHA)
		}
	}
	gitDirSucceeds(t, ctx, controlRepo, "fsck", "--full", "--strict")
	finalCheckout := filepath.Join(root, "final")
	run(t, ctx, "git", "clone", "--quiet", controlRepo, finalCheckout)
	git(t, ctx, finalCheckout, "checkout", "--quiet", finalSHA)
	runIn(t, ctx, finalCheckout, "./fixture-test.sh")
	assertOneIntegrationTask(t, ctx, coordplane, socket, project.ID, integration.ID)
	waitForLiveDirectMessage(t, ctx, coordplane, socket, project.ID, agentB.ID, taskA.ID)
	waitForNoProjectContainers(t, ctx, project.ID)

	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop live daemon before recovery: %v\n%s", err, readLog(daemon.logPath))
	}
	daemon = startDaemon(t, coordplane, configPath, socket)
	waitForReady(t, ctx, coordplane, socket, "live daemon restart")
	if got := projectDetail(t, ctx, coordplane, socket, project.ID); got.Status != core.ProjectActive || got.ActualCanonicalSHA != finalSHA {
		t.Fatalf("live restart lost canonical truth: %#v", got)
	}
	for _, taskID := range []string{taskA.ID, taskB.ID, integration.ID} {
		if task := taskDetail(t, ctx, coordplane, socket, taskID).Task; task.Status != core.TaskCompleted {
			t.Fatalf("live restart Task %s = %s", taskID, task.Status)
		}
	}
}

func requireRealCLI(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E_REAL_CLI") != "1" {
		t.Skip("real CLI gate is only entered through scripts/e2e-real-cli.sh")
	}
}

func releaseLiveDocker(t *testing.T) {
	t.Helper()
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e-real", liveE2ETimeout)
	requireNoError(t, err)
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release Docker test resource: %v", err)
		}
	})
}

func liveRuntimeConfig(t *testing.T) (string, string, []string) {
	t.Helper()
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	network := strings.TrimSpace(os.Getenv("E2E_DOCKER_NETWORK"))
	digest, digestErr := hex.DecodeString(strings.TrimPrefix(image, "sha256:"))
	if !strings.HasPrefix(image, "sha256:") || digestErr != nil || len(digest) != 32 || network == "" {
		t.Fatal("immutable E2E_RUNTIME_IMAGE digest and E2E_DOCKER_NETWORK are required")
	}
	if version := strings.TrimSpace(os.Getenv("E2E_CLAUDE_VERSION")); version != realClaudeVersion {
		t.Fatalf("E2E_CLAUDE_VERSION = %q, want %q", version, realClaudeVersion)
	}
	t.Logf("real runtime image=%s claude=%s", image, realClaudeVersion)
	var providerEnv []string
	for _, name := range strings.Split(os.Getenv("E2E_PROVIDER_ENV_ALLOWLIST"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			providerEnv = append(providerEnv, name)
		}
	}
	foundToken := false
	for _, name := range providerEnv {
		foundToken = foundToken || name == "ANTHROPIC_AUTH_TOKEN"
	}
	if !foundToken || strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) == "" {
		t.Fatal("ANTHROPIC_AUTH_TOKEN must be present in the provider environment allowlist")
	}
	return image, network, providerEnv
}

func registerLiveHomeCleanup(t *testing.T, image, dataDir string) {
	t.Helper()
	t.Cleanup(func() {
		homeRoot := filepath.Join(dataDir, "agent-homes")
		if _, err := os.Stat(homeRoot); os.IsNotExist(err) {
			return
		} else if err != nil {
			t.Errorf("inspect live Agent homes before cleanup: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := commandOutput(ctx, "", "docker", "run", "--rm", "--network", "none", "--user", "0:0",
			"-v", homeRoot+":/cleanup", "--entrypoint", "sh", image,
			"-c", "find /cleanup -mindepth 1 -delete"); err != nil {
			t.Errorf("clean live Agent homes through trusted Docker boundary: %v", err)
		}
	})
}

func waitForLiveRun(
	t *testing.T,
	ctx context.Context,
	binary, socket, taskID, previousRunID, progress string,
	timeout time.Duration,
) (core.Run, string) {
	t.Helper()
	run := eventually(t, ctx, timeout, "live Run with "+progress, func() (core.Run, bool, string) {
		detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		if detail.CurrentRun == nil {
			return core.Run{}, false, "no current Run"
		}
		ready := detail.CurrentRun.ID != previousRunID && detail.CurrentRun.State == core.RunActive &&
			detail.LatestProgress != nil && strings.Contains(detail.LatestProgress.PayloadJSON, progress)
		return *detail.CurrentRun, ready, fmt.Sprintf("run=%s state=%s progress=%v", detail.CurrentRun.ID, detail.CurrentRun.State, detail.LatestProgress != nil)
	})
	inspect := inspectContainer(t, ctx, run.ContainerID)
	for _, mount := range inspect.Mounts {
		if mount.Destination == "/home/agent" {
			return run, mount.Source
		}
	}
	t.Fatalf("live Run %s has no Agent home mount", run.ID)
	return core.Run{}, ""
}

func waitForTaskWithin(
	t *testing.T,
	ctx context.Context,
	binary, socket, taskID, reason string,
	timeout time.Duration,
	predicate func(core.Task) bool,
) core.Task {
	t.Helper()
	return eventually(t, ctx, timeout, reason, func() (core.Task, bool, string) {
		detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Task{}, false, err.Error()
		}
		return detail.Task, predicate(detail.Task), fmt.Sprintf("status=%s pending=%s failure=%s", detail.Task.Status, detail.Task.PendingAction, detail.Task.FailureReason)
	})
}

func capturedSubmission(task core.Task) bool {
	return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.HeadRunID != "" && task.TaskRef != ""
}

func waitForLiveDirectMessage(t *testing.T, ctx context.Context, binary, socket, projectID, agentBID, taskAID string) {
	t.Helper()
	eventually(t, ctx, 2*time.Minute, "acknowledged live direct Message", func() (core.Message, bool, string) {
		page, err := commandJSON[core.MessagePage](ctx, binary,
			"message", "list", "--socket", socket, "--project", projectID, "--limit", "20", "--output", "json")
		if err != nil {
			return core.Message{}, false, err.Error()
		}
		for _, message := range page.Items {
			if strings.Contains(message.Body, "LIVE-DIRECT") {
				ok := message.RecipientID == agentBID && message.RelatedTaskID == taskAID && message.State == core.MessageAcknowledged
				return message, ok, fmt.Sprintf("state=%s recipient=%s related=%s", message.State, message.RecipientID, message.RelatedTaskID)
			}
		}
		return core.Message{}, false, "direct Message not found"
	})
}

func assertAgentBossMessages(t *testing.T, ctx context.Context, binary, socket, projectID, agentID string, minimum int) {
	t.Helper()
	page := runJSON[core.MessagePage](t, ctx, binary,
		"message", "list", "--socket", socket, "--project", projectID, "--limit", "20", "--output", "json")
	count := 0
	for _, message := range page.Items {
		if message.SenderKind == "agent" && message.SenderID == agentID && message.RecipientKind == "boss" {
			count++
		}
	}
	if count < minimum {
		t.Fatalf("Agent-to-Boss live Messages = %d, want at least %d", count, minimum)
	}
}

const realSmokeInstructions = `You are running the real Claude adapter smoke for CoordPlane.
Read the complete run bootstrap first. Use only the current Run's coordlink and never infer Task completion from text.
Immediately call coordlink progress with summary LIVE-SMOKE-READY, using a request ID containing the current Run ID.
List the inbox. Acknowledge every Message you process.
If the inbox contains LIVE-SMOKE-CANCEL, call progress with summary LIVE-CANCEL-READY and remain alive without requesting a Task outcome until Boss cancels the Task.
Otherwise send a Message to Boss with body LIVE-SMOKE-AGENT and a unique request ID. Poll the inbox until a Message containing LIVE-SMOKE-FINISH arrives, acknowledge it, then call coordlink task wait with a short reason and unique request ID. Remain alive briefly so the Daemon can stop the container from the durable outcome.
`

const realWorkInstructions = `You are running the real Claude two-Agent acceptance gate for CoordPlane.
Read the complete run bootstrap before acting. Use native Git only inside /workspace/project and use coordlink for every coordination action. Never treat your own text or process exit as Task completion.
Configure Git safe.directory=/workspace/project and a local test user before committing.

For a work Task, read live_role and peer_agent_id from the Task description. Immediately call coordlink progress with summary LIVE-READY. Poll coordlink inbox list until LIVE-GO is present, then acknowledge that Message.
Role A must send the peer Agent a direct Message with body LIVE-DIRECT using coordlink message send and the peer_agent_id. Role B must poll the inbox until LIVE-DIRECT arrives and acknowledge it.
Write exactly agent-A or agent-B plus a newline to agent-A.txt or agent-B.txt according to the role. Run ./fixture-test.sh, commit only that file with native Git, resolve HEAD with git rev-parse HEAD, then call coordlink task submit with that exact expected head. Remain alive briefly for Daemon shutdown.

For an integration Task, use the exact Source head from the bootstrap. Merge it into the current canonical-based workspace with native git merge --no-ff, run ./fixture-test.sh, commit if Git did not create the merge commit, resolve HEAD, and call coordlink task submit with the exact expected head. Do not create child Tasks and do not accept source Tasks yourself.
`
