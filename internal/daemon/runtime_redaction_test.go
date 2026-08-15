package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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

// newRunControlDir creates a per-run control directory under a fresh root and
// returns the control root so the controller can resolve the run by ID.
func newRunControlDir(t *testing.T, runID string, files map[string]string) string {
	t.Helper()
	controlRoot := filepath.Join(t.TempDir(), "run-control")
	writeRunControl(t, controlRoot, runID, files)
	return controlRoot
}

func writeRunControl(t *testing.T, controlRoot, runID string, files map[string]string) {
	t.Helper()
	path := filepath.Join(controlRoot, runID)
	requireNoError(t, os.MkdirAll(path, 0o700))
	for name, data := range files {
		requireNoError(t, os.WriteFile(filepath.Join(path, name), []byte(data), 0o440))
	}
}

func TestRuntimeRedactionDoesNotRereadAgentInstructions(t *testing.T) {
	service := newRuntimeTestService(t)
	canary := "REDACT-CANARY-INSTRUCTIONS"
	agent := requireRuntimeValue(service.AddAgent(context.Background(), core.AddAgentInput{DisplayName: "Redaction Agent", AdapterID: "claude", Image: "agent:test", InstructionsText: canary, RequestID: "redaction-instructions-agent"}))
	run := core.Run{ID: "run-redaction", AgentID: agent.ID}
	controlRoot := newRunControlDir(t, run.ID, map[string]string{runtimeSecretsFile: "", runtimeInstructionsFile: "unrelated instructions"})
	controller := &runtimeController{service: service, controlRoot: controlRoot}

	text := controller.runtimeRedaction(run).Text("provider echoed " + canary)
	if !strings.Contains(text, canary) {
		t.Fatalf("runtime redaction re-read the mutable Agent instructions: %q", text)
	}
}

// A run whose control directory carries only a legacy bootstrap file has no
// trusted instructions lineage and must fail closed instead of extracting the
// instructions prefix from the bootstrap.
func TestRuntimeRedactionLegacyBootstrapRunFailsClosed(t *testing.T) {
	canary := "LEGACY-CANARY"
	bootstrap := "agent prompt\n" + canary + "\n\nCoordPlane Run context\nProject: p\nAgent: a\n"
	controlRoot := newRunControlDir(t, "run-legacy", map[string]string{"bootstrap": bootstrap})
	controller := &runtimeController{controlRoot: controlRoot}
	run := core.Run{ID: "run-legacy"}

	if _, ok := controller.runtimeRunInstructions(run); ok {
		t.Fatal("legacy bootstrap-only run did not fail closed")
	}
	redact := controller.runtimeRedaction(run)
	if !redact.failClosed {
		t.Fatal("legacy bootstrap-only run did not fail closed")
	}
	text := redact.Text("provider echoed " + canary)
	if strings.Contains(text, canary) || !strings.Contains(text, redactionUnavailableMarker) {
		t.Fatalf("legacy bootstrap run leaked instructions or missed the marker: %q", text)
	}
}

func TestRuntimeRedactionInstructionsFileIsCollisionSafe(t *testing.T) {
	canary := "CANARY-AFTER-SEPARATOR"
	instructions := "agent prompt header\n\nCoordPlane Run context\n" + canary
	controlRoot := newRunControlDir(t, "run-collision", map[string]string{runtimeInstructionsFile: instructions, runtimeLaunchFile: "#!/bin/sh\n", runtimeSecretsFile: ""})
	sum := sha256.Sum256([]byte(instructions))
	controller := &runtimeController{controlRoot: controlRoot}
	run := core.Run{ID: "run-collision", InstructionsHash: hex.EncodeToString(sum[:])}

	values, ok := controller.runtimeRunInstructions(run)
	if !ok || len(values) != 1 || values[0] != instructions {
		t.Fatalf("runtimeRunInstructions = %#v ok=%t, want the whole instructions %q", values, ok, instructions)
	}
	redact := controller.runtimeRedaction(run)
	if redact.failClosed {
		t.Fatal("collision-safe instructions file failed closed")
	}
	text := redact.Text("provider echoed:\n" + instructions + "\nend")
	if strings.Contains(text, canary) {
		t.Fatalf("runtime redaction leaked text after the separator collision: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("runtime redaction did not replace the echoed instructions: %q", text)
	}
}

func TestRuntimeRedactionNewLineageFailsClosedWithoutTrustedInstructions(t *testing.T) {
	canary := "UNREDACTED-CANARY"
	sum := sha256.Sum256([]byte("original prompt"))
	run := core.Run{ID: "run-fail-closed", InstructionsHash: hex.EncodeToString(sum[:])}
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "missing instructions file"},
		{name: "hash-mismatched instructions file", raw: []byte("tampered prompt")},
	} {
		t.Run(test.name, func(t *testing.T) {
			controlRoot := newRunControlDir(t, "run-fail-closed", map[string]string{runtimeLaunchFile: "#!/bin/sh\n", runtimeSecretsFile: ""})
			controller := &runtimeController{controlRoot: controlRoot}
			if test.raw != nil {
				requireNoError(t, os.WriteFile(filepath.Join(controlRoot, "run-fail-closed", runtimeInstructionsFile), test.raw, 0o440))
			}
			if _, ok := controller.runtimeRunInstructions(run); ok {
				t.Fatal("runtimeRunInstructions did not fail closed")
			}
			redact := controller.runtimeRedaction(run)
			if !redact.failClosed {
				t.Fatal("runtime redaction did not fail closed")
			}
			text := redact.Text("provider echoed " + canary)
			if strings.Contains(text, canary) || !strings.Contains(text, redactionUnavailableMarker) {
				t.Fatalf("fail-closed redaction leaked content or missed the marker: %q", text)
			}
		})
	}
}

// R5: an in-place rewrite of the instructions_file between launch and monitor
// changes the file's hash, so the run's immutable InstructionsHash no longer
// reconciles and the redaction must fail closed instead of trusting the new
// content.
func TestRuntimeRedactionInstructionsFileInPlaceRewriteFailsClosed(t *testing.T) {
	canary := "IN-PLACE-REWRITE-CANARY"
	original := "original prompt"
	replaced := "replacement prompt " + canary
	runID := "run-in-place"
	controlRoot := newRunControlDir(t, runID, map[string]string{runtimeInstructionsFile: original, runtimeLaunchFile: "#!/bin/sh\n", runtimeSecretsFile: ""})
	sum := sha256.Sum256([]byte(original))
	controller := &runtimeController{controlRoot: controlRoot}
	run := core.Run{ID: runID, InstructionsHash: hex.EncodeToString(sum[:])}

	values, ok := controller.runtimeRunInstructions(run)
	if !ok || len(values) != 1 || values[0] != original {
		t.Fatalf("baseline instructions = %#v ok=%t, want %q", values, ok, original)
	}
	requireNoError(t, os.Chmod(filepath.Join(controlRoot, runID, runtimeInstructionsFile), 0o640))
	requireNoError(t, os.WriteFile(filepath.Join(controlRoot, runID, runtimeInstructionsFile), []byte(replaced), 0o440))
	requireNoError(t, os.Chmod(filepath.Join(controlRoot, runID, runtimeInstructionsFile), 0o440))
	if _, ok := controller.runtimeRunInstructions(run); ok {
		t.Fatal("in-place rewritten instructions file did not fail closed")
	}
	redact := controller.runtimeRedaction(run)
	if !redact.failClosed {
		t.Fatal("in-place rewritten instructions file did not fail closed")
	}
	text := redact.Text("provider echoed " + canary)
	if strings.Contains(text, canary) || !strings.Contains(text, redactionUnavailableMarker) {
		t.Fatalf("in-place rewrite leaked content or missed the marker: %q", text)
	}
}

func TestParseRunSecretsFile(t *testing.T) {
	for _, test := range []struct {
		name   string
		raw    string
		want   []string
		wantOK bool
	}{
		{
			name:   "spaces and shell metacharacters",
			raw:    "ANTHROPIC_AUTH_TOKEN='sk-ant $(rm -rf /) ; | & > x*'\n",
			want:   []string{"sk-ant $(rm -rf /) ; | & > x*"},
			wantOK: true,
		},
		{
			name:   "embedded single quotes",
			raw:    "SECRET='it'\\''s \"quoted\"'\n",
			want:   []string{`it's "quoted"`},
			wantOK: true,
		},
		{
			name:   "newlines inside quotes",
			raw:    "MULTILINE='line one\nline two\nline three'\n",
			want:   []string{"line one\nline two\nline three"},
			wantOK: true,
		},
		{
			name:   "multiple entries",
			raw:    "A='one'\nB='two words'\n",
			want:   []string{"one", "two words"},
			wantOK: true,
		},
		{
			name:   "comments and blank lines are ignored",
			raw:    "# comment\n\nA='one'\n",
			want:   []string{"one"},
			wantOK: true,
		},
		{name: "unquoted value rejected", raw: "SECRET=bare-value\n", wantOK: false},
		{name: "invalid key rejected", raw: "1BAD='value'\n", wantOK: false},
		{name: "unterminated quote rejected", raw: "SECRET='oops\n", wantOK: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			values, ok := parseRunSecretsFile(test.raw)
			if ok != test.wantOK {
				t.Fatalf("parseRunSecretsFile ok = %v, want %v (values=%#v)", ok, test.wantOK, values)
			}
			if !test.wantOK {
				return
			}
			if !slices.Equal(values, test.want) {
				t.Fatalf("parseRunSecretsFile values = %#v, want %#v", values, test.want)
			}
		})
	}
}

func TestSerializeRunSecretsFileRoundTrips(t *testing.T) {
	values := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-ant $(rm -rf /) ; | & > x*",
		"QUOTED":               `it's "quoted"`,
		"MULTILINE":            "line one\nline two\nline three",
		"PLAIN":                "simple",
		"EMPTY":                "",
	}
	raw := requireRuntimeValue(serializeRunSecretsFile(values))
	parsed, ok := parseRunSecretsFile(string(raw))
	if !ok {
		t.Fatalf("serialized secrets file did not parse back: %q", raw)
	}
	want := []string{"sk-ant $(rm -rf /) ; | & > x*", `it's "quoted"`, "line one\nline two\nline three", "simple", ""}
	sort.Strings(parsed)
	sort.Strings(want)
	if !slices.Equal(parsed, want) {
		t.Fatalf("round-trip secrets = %#v, want %#v", parsed, want)
	}
}

func TestSerializeRunSecretsFileRejectsUnsafe(t *testing.T) {
	for _, secrets := range []map[string]string{
		{"1BAD": "value"},
		{"": "value"},
		{"GOOD_KEY": "ok", "BAD-KEY": "value"},
		{"KEY": "a\x00b"},
	} {
		if _, err := serializeRunSecretsFile(secrets); err == nil {
			t.Fatalf("serializeRunSecretsFile accepted unsafe secrets: %#v", secrets)
		}
	}
}

func TestRuntimeSecretsFileShellSourceSafety(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "pwned")
	canary := "sk-ant-'$(touch " + payload + ")' ; | & > *\nsecond line"
	raw := requireRuntimeValue(serializeRunSecretsFile(map[string]string{"ANTHROPIC_AUTH_TOKEN": canary}))
	secretsPath := filepath.Join(t.TempDir(), runtimeSecretsFile)
	requireNoError(t, os.WriteFile(secretsPath, raw, 0o440))

	// Source exactly like the real launcher does (allexport then dot) and print
	// the value: the shell must see the literal secret, never execute it.
	launcherPath := filepath.Join(t.TempDir(), runtimeLaunchFile)
	launcher := "#!/bin/sh\nset -a\n. \"$COORDPLANE_SECRETS_FILE\"\nset +a\nexec \"$@\"\n"
	requireNoError(t, os.WriteFile(launcherPath, []byte(launcher), 0o550))
	cmd := exec.Command(launcherPath, "sh", "-c", `printf %s "$ANTHROPIC_AUTH_TOKEN"`)
	cmd.Env = append(os.Environ(), "COORDPLANE_SECRETS_FILE="+secretsPath)
	out := requireRuntimeValue(cmd.Output())
	if string(out) != canary {
		t.Fatalf("launcher sourced secret = %q, want %q (file: %q)", out, canary, raw)
	}
	if _, statErr := os.Stat(payload); statErr == nil {
		t.Fatalf("sourcing secrets file executed a command substitution payload")
	}
}

func TestRuntimeRedactionLoadsSecretsFile(t *testing.T) {
	canary := "REDACT-SECRET-FILE-VALUE $(with shell metachars)"
	raw := "ANTHROPIC_AUTH_TOKEN='" + strings.ReplaceAll(canary, "'", "'\\''") + "'\n"
	controlRoot := newRunControlDir(t, "run-redaction-file", map[string]string{runtimeSecretsFile: raw, runtimeInstructionsFile: "unrelated instructions"})
	controller := &runtimeController{
		controlRoot: controlRoot,
		config:      config.Config{Runtime: config.RuntimeConfig{ProviderEnvAllowlist: []string{"UNRELATED_ENV"}}},
	}
	run := core.Run{ID: "run-redaction-file"}

	text := controller.runtimeRedaction(run).Text("provider echoed " + canary)
	if strings.Contains(text, canary) {
		t.Fatalf("runtime redaction retained secrets-file value: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("runtime redaction did not replace secrets-file value: %q", text)
	}
}

// R8/R9: a new-lineage run whose control directory carries the launch file but
// no trusted secrets file must fail closed. The host provider allowlist env is
// never used as a fallback for new lineages, so no unredacted content persists.
func TestRuntimeRedactionNewLineageFailsClosedWithoutTrustedSecrets(t *testing.T) {
	canary := "UNREDACTED-SECRETS-CANARY"
	t.Setenv("HOST_PROVIDER_TOKEN", canary)
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "missing secrets file"},
		{name: "corrupt secrets file", raw: []byte("HOST_PROVIDER_TOKEN=bare-canary\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			controlRoot := newRunControlDir(t, "run-secrets", map[string]string{runtimeLaunchFile: "#!/bin/sh\n"})
			controller := &runtimeController{
				controlRoot: controlRoot,
				config:      config.Config{Runtime: config.RuntimeConfig{ProviderEnvAllowlist: []string{"HOST_PROVIDER_TOKEN"}}},
			}
			run := core.Run{ID: "run-secrets"}
			if test.raw != nil {
				requireNoError(t, os.WriteFile(filepath.Join(controlRoot, "run-secrets", runtimeSecretsFile), test.raw, 0o440))
			}
			if _, ok := controller.runtimeRunSecrets(run); ok {
				t.Fatal("new-lineage runtimeRunSecrets did not fail closed")
			}
			redact := controller.runtimeRedaction(run)
			if !redact.failClosed {
				t.Fatal("new-lineage runtime redaction did not fail closed")
			}
			text := redact.Text("provider echoed " + canary)
			if strings.Contains(text, canary) || !strings.Contains(text, redactionUnavailableMarker) {
				t.Fatalf("fail-closed redaction leaked host env canary or missed the marker: %q", text)
			}
		})
	}
}

// R10: a run without a trusted secrets file fails closed even when the host
// provider allowlist env is set. The host-env fallback is removed, so no
// unredacted content can persist.
func TestRuntimeRedactionLegacyRunFailsClosedWithoutTrustedSecrets(t *testing.T) {
	canary := "HOST-PROVIDER-ENV-CANARY"
	controlRoot := newRunControlDir(t, "run-legacy-env", nil)
	t.Setenv("HOST_PROVIDER_TOKEN", canary)
	controller := &runtimeController{
		controlRoot: controlRoot,
		config:      config.Config{Runtime: config.RuntimeConfig{ProviderEnvAllowlist: []string{"HOST_PROVIDER_TOKEN"}}},
	}
	run := core.Run{ID: "run-legacy-env"}

	if _, ok := controller.runtimeRunSecrets(run); ok {
		t.Fatal("legacy runtimeRunSecrets did not fail closed")
	}
	redact := controller.runtimeRedaction(run)
	if !redact.failClosed {
		t.Fatal("legacy run did not fail closed")
	}
	text := redact.Text("provider echoed " + canary)
	if strings.Contains(text, canary) || !strings.Contains(text, redactionUnavailableMarker) {
		t.Fatalf("legacy run leaked host env canary or missed the marker: %q", text)
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
	run := core.Run{ID: "run-log-boundary", WorkspacePath: filepath.Join(root, "workspace"), HomePath: filepath.Join(root, "home"), LogPath: filepath.Join(root, "logs", "run.log")}
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, run.ID)
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, "token"), []byte(runToken+"\n"), 0o400))
	secretsRaw := requireRuntimeValue(serializeRunSecretsFile(map[string]string{providerName: secretLineA + "\n" + secretLineB}))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), secretsRaw, 0o440))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeInstructionsFile), []byte("unrelated instructions"), 0o440))
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
		config:   config.Config{DataDir: root, Runtime: config.RuntimeConfig{ProviderEnvAllowlist: []string{providerName}, WorkspaceRoot: filepath.Join(root, "workspace"), AgentHomeRoot: filepath.Join(root, "home"), LogRoot: filepath.Join(root, "logs")}},
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
	raw := requireRuntimeValue(os.ReadFile(run.LogPath))
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
