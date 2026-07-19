package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/adapter"
	"coordplane/internal/config"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func TestRuntimeRedaction(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "task")
	secret := "provider-secret-canary"
	multiline := "provider-secret-header\r\n\r\n provider-secret-body \nprovider-secret-footer"
	for _, test := range []struct {
		name, input, want string
		paths, secrets    []string
	}{
		{name: "secret and host paths", input: "open " + workspace + ": token=" + secret, want: "open " + redactedHostPath + ": token=" + redactedSecret, paths: []string{root, workspace}, secrets: []string{secret}},
		{name: "secret classification wins", input: filepath.Join(root, "credential"), want: redactedSecret, paths: []string{root}, secrets: []string{filepath.Join(root, "credential")}},
		{name: "complete multiline secret", input: multiline, want: redactedSecret, secrets: []string{multiline}},
		{name: "provider-secret-header", input: "runtime output: provider-secret-header", want: "runtime output: " + redactedSecret, secrets: []string{multiline}},
		{name: "provider-secret-body", input: "runtime output: provider-secret-body", want: "runtime output: " + redactedSecret, secrets: []string{multiline}},
		{name: "provider-secret-footer", input: "runtime output: provider-secret-footer", want: "runtime output: " + redactedSecret, secrets: []string{multiline}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result := newRuntimeRedaction(test.paths, test.secrets).Text(test.input); result != test.want {
				t.Fatalf("runtime redaction = %q, want %q", result, test.want)
			}
		})
	}
}

func TestRuntimeLogBoundaryRedactsBoundsAndReplaysFromZero(t *testing.T) {
	root := t.TempDir()
	const (
		providerName = "ANTHROPIC_AUTH_TOKEN"
		secretLineA  = "provider-secret-line-a"
		secretLineB  = "provider-secret-line-b"
		runToken     = "run-token-log-boundary"
	)
	t.Setenv(providerName, secretLineA+"\n"+secretLineB)
	run := core.Run{
		ID: "run-log-boundary", WorkspacePath: filepath.Join(root, "workspace"),
		HomePath: filepath.Join(root, "home"), LogPath: filepath.Join(root, "logs", "run.log"),
	}
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, run.ID)
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, "token"), []byte(runToken+"\n"), 0o400))
	requireNoError(t, os.MkdirAll(filepath.Dir(run.LogPath), 0o700))
	firstLine := `{"type":"assistant","message":"canary ` + secretLineA + " " + secretLineB + " " + runToken + " " + root + `"}` + "\n"
	filler := `{"type":"assistant","message":"` + strings.Repeat("log-data-", 128) + `"}` + "\n"
	var payload strings.Builder
	payload.WriteString(firstLine)
	for payload.Len() <= runtimeLogLimit+(1<<20) {
		payload.WriteString(filler)
	}
	executor := &replayLogExecutor{payload: payload.String()}
	controller := &runtimeController{
		config: config.Config{DataDir: root, Runtime: config.RuntimeConfig{
			ProviderEnvAllowlist: []string{providerName}, WorkspaceRoot: filepath.Join(root, "workspace"),
			AgentHomeRoot: filepath.Join(root, "home"), LogRoot: filepath.Join(root, "logs"),
		}},
		executor: executor, controlRoot: controlRoot,
	}
	monitor := &runMonitor{redact: controller.runtimeRedaction(run)}
	entry, ok := adapter.Production().Lookup("claude")
	if !ok {
		t.Fatal("Claude adapter is not registered")
	}
	for attempt := 0; attempt < 2; attempt++ {
		requireNoError(t, controller.streamLogs(context.Background(), run, containerruntime.RuntimeRef{}, entry, monitor))
	}
	raw, err := os.ReadFile(run.LogPath)
	requireNoError(t, err)
	text := string(raw)
	if len(raw) > runtimeLogLimit {
		t.Fatalf("runtime log bytes = %d, limit = %d", len(raw), runtimeLogLimit)
	}
	if strings.Count(text, runtimeLogTruncatedMarker) != 1 {
		t.Fatalf("runtime log truncation marker count = %d", strings.Count(text, runtimeLogTruncatedMarker))
	}
	for _, forbidden := range []string{secretLineA, secretLineB, runToken, root} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("runtime log retained sensitive value %q", forbidden)
		}
	}
	if strings.Count(text, "canary ") != 1 {
		t.Fatalf("runtime log replay appended duplicate content: canary count=%d", strings.Count(text, "canary "))
	}
	if executor.logCalls != 2 {
		t.Fatalf("Docker log replay calls = %d, want 2", executor.logCalls)
	}
}

type replayLogExecutor struct {
	containerruntime.Executor
	payload  string
	logCalls int
}

func (e *replayLogExecutor) Logs(context.Context, containerruntime.RuntimeRef, bool) (io.ReadCloser, error) {
	e.logCalls++
	return io.NopCloser(strings.NewReader(e.payload)), nil
}
