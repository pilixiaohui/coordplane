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

func TestRuntimeRedactionRemovesSecretsAndAbsoluteHostPaths(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "task")
	secret := "provider-secret-canary"
	redaction := newRuntimeRedaction(
		[]string{root, workspace},
		[]string{secret},
	)

	result := redaction.Text("open " + workspace + ": token=" + secret)
	if strings.Contains(result, root) || strings.Contains(result, workspace) || strings.Contains(result, secret) {
		t.Fatalf("runtime text retained sensitive input: %q", result)
	}
	if !strings.Contains(result, redactedHostPath) || !strings.Contains(result, redactedSecret) {
		t.Fatalf("runtime text omitted redaction markers: %q", result)
	}
}

func TestRuntimeRedactionPrefersSecretClassificationOverContainingPath(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "credential")
	result := newRuntimeRedaction([]string{root}, []string{secret}).Text(secret)
	if result != redactedSecret {
		t.Fatalf("redacted overlapping value = %q, want %q", result, redactedSecret)
	}
}

func TestRuntimeRedactionRemovesEveryLineOfMultilineSecret(t *testing.T) {
	segments := []string{
		"provider-secret-header",
		"provider-secret-body",
		"provider-secret-footer",
	}
	secret := segments[0] + "\r\n\r\n " + segments[1] + " \n" + segments[2]
	redaction := newRuntimeRedaction(nil, []string{secret})

	if result := redaction.Text(secret); result != redactedSecret {
		t.Fatalf("redacted full secret = %q, want %q", result, redactedSecret)
	}
	for _, segment := range segments {
		if result := redaction.Text("runtime output: " + segment); result != "runtime output: "+redactedSecret {
			t.Errorf("redacted line containing %q = %q", segment, result)
		}
	}
}

func TestRuntimeLogBoundaryRedactsBoundsAndReplaysFromZero(t *testing.T) {
	root := t.TempDir()
	const (
		providerName = "COORDPLANE_TEST_PROVIDER_SECRET"
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
	firstLine := "canary " + secretLineA + " " + secretLineB + " " + runToken + " " + root + "\n"
	filler := strings.Repeat("log-data-", 128) + "\n"
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
	entry, ok := adapter.Production().Lookup("codex")
	if !ok {
		t.Fatal("codex adapter is not registered")
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
