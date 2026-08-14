package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRuntimeRedactionDoesNotRereadAgentInstructions(t *testing.T) {
	service := newRuntimeTestService(t)
	canary := "REDACT-CANARY-INSTRUCTIONS"
	agent := requireRuntimeValue(service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Redaction Agent", AdapterID: "claude", Image: "agent:test",
		InstructionsText: canary, RequestID: "redaction-instructions-agent",
	}))
	controller := &runtimeController{service: service, controlRoot: t.TempDir()}
	run := core.Run{ID: "run-redaction", AgentID: agent.ID}

	text := controller.runtimeRedaction(run).Text("provider echoed " + canary)
	if !strings.Contains(text, canary) {
		t.Fatalf("runtime redaction re-read the mutable Agent instructions: %q", text)
	}
}

func TestRuntimeRedactionIncludesBootstrapInstructions(t *testing.T) {
	canary := "REDACT-CANARY-INSTRUCTIONS"
	root := t.TempDir()
	runID := "run-redaction"
	controlPath := filepath.Join(root, "run-control", runID)
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	bootstrap := canary + "\n\nCoordPlane Run context\nProject: p\nAgent: a\nTask: t\nRun: " + runID + "\n"
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, "bootstrap"), []byte(bootstrap), 0o440))
	controller := &runtimeController{controlRoot: filepath.Join(root, "run-control")}
	run := core.Run{ID: runID}

	text := controller.runtimeRedaction(run).Text("provider echoed " + canary)
	if strings.Contains(text, canary) {
		t.Fatalf("runtime redaction retained instructions text: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("runtime redaction did not replace instructions text: %q", text)
	}
}

func TestRuntimeRedactionInstructionsFileIsCollisionSafe(t *testing.T) {
	canary := "CANARY-AFTER-SEPARATOR"
	instructions := "agent prompt header\n\nCoordPlane Run context\n" + canary
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-collision")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeInstructionsFile), []byte(instructions), 0o440))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte("#!/bin/sh\n"), 0o550))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), []byte(""), 0o440))
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

func TestRuntimeRedactionInstructionsFileMatchesLegacyBootstrap(t *testing.T) {
	instructions := "system prompt with a normal body\nno separator line here\nkeep this whole text"
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-parity")
	legacyPath := filepath.Join(controlRoot, "run-legacy-parity")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.MkdirAll(legacyPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeInstructionsFile), []byte(instructions), 0o440))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte("#!/bin/sh\n"), 0o550))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), []byte(""), 0o440))
	requireNoError(t, os.WriteFile(filepath.Join(legacyPath, runtimeBootstrapFile), []byte(instructions+"\n\nCoordPlane Run context\nProject: p\n"), 0o440))
	sum := sha256.Sum256([]byte(instructions))
	controller := &runtimeController{controlRoot: controlRoot}
	newLineage := controller.runtimeRedaction(core.Run{ID: "run-parity", InstructionsHash: hex.EncodeToString(sum[:])})
	legacy := controller.runtimeRedaction(core.Run{ID: "run-legacy-parity"})
	if newLineage.failClosed || legacy.failClosed {
		t.Fatal("parity redaction failed closed")
	}
	input := "provider echoed " + instructions
	if got, want := newLineage.Text(input), legacy.Text(input); got != want {
		t.Fatalf("new-lineage redaction %q differs from legacy bootstrap redaction %q", got, want)
	}
}

func TestRuntimeRedactionLegacyRunFallsBackToBootstrap(t *testing.T) {
	canary := "LEGACY-CANARY"
	bootstrap := "agent prompt\n" + canary + "\n\nCoordPlane Run context\nProject: p\nAgent: a\n"
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-legacy")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeBootstrapFile), []byte(bootstrap), 0o440))
	controller := &runtimeController{controlRoot: controlRoot}
	run := core.Run{ID: "run-legacy"}

	values, ok := controller.runtimeRunInstructions(run)
	if !ok || len(values) != 1 || values[0] != "agent prompt\n"+canary {
		t.Fatalf("legacy runtimeRunInstructions = %#v ok=%t, want the bootstrap instructions prefix", values, ok)
	}
	redact := controller.runtimeRedaction(run)
	if redact.failClosed {
		t.Fatal("legacy run failed closed")
	}
	text := redact.Text("provider echoed " + values[0])
	if strings.Contains(text, canary) {
		t.Fatalf("legacy bootstrap fallback leaked instructions: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("legacy bootstrap fallback did not redact instructions: %q", text)
	}
}

func TestRuntimeRedactionNewLineageFailsClosedWithoutTrustedInstructions(t *testing.T) {
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-fail-closed")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte("#!/bin/sh\n"), 0o550))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), []byte(""), 0o440))
	controller := &runtimeController{controlRoot: controlRoot}
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
			if test.raw != nil {
				requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeInstructionsFile), test.raw, 0o440))
			}
			if _, ok := controller.runtimeRunInstructions(run); ok {
				t.Fatalf("runtimeRunInstructions did not fail closed")
			}
			redact := controller.runtimeRedaction(run)
			if !redact.failClosed {
				t.Fatal("runtime redaction did not fail closed")
			}
			text := redact.Text("provider echoed " + canary)
			if strings.Contains(text, canary) {
				t.Fatalf("fail-closed redaction leaked content: %q", text)
			}
			if !strings.Contains(text, redactionUnavailableMarker) {
				t.Fatalf("fail-closed redaction did not emit the unavailable marker: %q", text)
			}
		})
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
		{
			name:   "unquoted value rejected",
			raw:    "SECRET=bare-value\n",
			wantOK: false,
		},
		{
			name:   "invalid key rejected",
			raw:    "1BAD='value'\n",
			wantOK: false,
		},
		{
			name:   "unterminated quote rejected",
			raw:    "SECRET='oops\n",
			wantOK: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			values, ok := parseRunSecretsFile(test.raw)
			if ok != test.wantOK {
				t.Fatalf("parseRunSecretsFile ok = %v, want %v (values=%#v)", ok, test.wantOK, values)
			}
			if !test.wantOK {
				return
			}
			if len(values) != len(test.want) {
				t.Fatalf("parseRunSecretsFile values = %#v, want %#v", values, test.want)
			}
			for index := range test.want {
				if values[index] != test.want[index] {
					t.Fatalf("parseRunSecretsFile values = %#v, want %#v", values, test.want)
				}
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
	raw, err := serializeRunSecretsFile(values)
	requireNoError(t, err)
	parsed, ok := parseRunSecretsFile(string(raw))
	if !ok {
		t.Fatalf("serialized secrets file did not parse back: %q", raw)
	}
	got := append([]string(nil), parsed...)
	want := []string{"sk-ant $(rm -rf /) ; | & > x*", `it's "quoted"`, "line one\nline two\nline three", "simple", ""}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("round-trip secrets = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("round-trip secrets = %#v, want %#v", got, want)
		}
	}
}

func TestSerializeRunSecretsFileRejectsInvalidKey(t *testing.T) {
	for _, secrets := range []map[string]string{
		{"1BAD": "value"},
		{"": "value"},
		{"GOOD_KEY": "ok", "BAD-KEY": "value"},
	} {
		if _, err := serializeRunSecretsFile(secrets); err == nil {
			t.Fatalf("serializeRunSecretsFile accepted unsafe keys: %#v", secrets)
		}
	}
}

func TestSerializeRunSecretsFileRejectsNUL(t *testing.T) {
	if _, err := serializeRunSecretsFile(map[string]string{"KEY": "a\x00b"}); err == nil {
		t.Fatal("serializeRunSecretsFile accepted a NUL byte in a value")
	}
}

func TestRuntimeSecretsFileShellSourceSafety(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "pwned")
	canary := "sk-ant-'$(touch " + payload + ")' ; | & > *\nsecond line"
	raw, err := serializeRunSecretsFile(map[string]string{"ANTHROPIC_AUTH_TOKEN": canary})
	requireNoError(t, err)
	secretsPath := filepath.Join(t.TempDir(), runtimeSecretsFile)
	requireNoError(t, os.WriteFile(secretsPath, raw, 0o440))

	// Source exactly like the real launcher does (allexport then dot) and print
	// the value: the shell must see the literal secret, never execute it.
	launcherPath := filepath.Join(t.TempDir(), runtimeLaunchFile)
	launcher := "#!/bin/sh\nset -a\n. \"$COORDPLANE_SECRETS_FILE\"\nset +a\nexec \"$@\"\n"
	requireNoError(t, os.WriteFile(launcherPath, []byte(launcher), 0o550))
	cmd := exec.Command(launcherPath, "sh", "-c", `printf %s "$ANTHROPIC_AUTH_TOKEN"`)
	cmd.Env = append(os.Environ(), "COORDPLANE_SECRETS_FILE="+secretsPath)
	out, err := cmd.Output()
	requireNoError(t, err)
	if string(out) != canary {
		t.Fatalf("launcher sourced secret = %q, want %q (file: %q)", out, canary, raw)
	}
	if _, statErr := os.Stat(payload); statErr == nil {
		t.Fatalf("sourcing secrets file executed a command substitution payload")
	}
}

func TestRuntimeRedactionLoadsSecretsFile(t *testing.T) {
	canary := "REDACT-SECRET-FILE-VALUE $(with shell metachars)"
	root := t.TempDir()
	runID := "run-redaction-file"
	controlPath := filepath.Join(root, "run-control", runID)
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	raw := "ANTHROPIC_AUTH_TOKEN='" + strings.ReplaceAll(canary, "'", "'\\''") + "'\n"
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, "secrets"), []byte(raw), 0o440))
	controller := &runtimeController{
		controlRoot: filepath.Join(root, "run-control"),
		config: config.Config{Runtime: config.RuntimeConfig{
			ProviderEnvAllowlist: []string{"UNRELATED_ENV"},
		}},
	}
	run := core.Run{ID: runID}

	text := controller.runtimeRedaction(run).Text("provider echoed " + canary)
	if strings.Contains(text, canary) {
		t.Fatalf("runtime redaction retained secrets-file value: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("runtime redaction did not replace secrets-file value: %q", text)
	}
}

// R8: a new-lineage run whose control directory carries the launch file but no
// secrets file must fail closed. The host provider allowlist env is never used
// as a fallback for new lineages, so no unredacted content can be persisted.
func TestRuntimeRedactionNewLineageFailsClosedWithoutSecretsFile(t *testing.T) {
	canary := "UNREDACTED-SECRETS-CANARY"
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-secrets-missing")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte("#!/bin/sh\n"), 0o550))
	t.Setenv("HOST_PROVIDER_TOKEN", canary)
	controller := &runtimeController{
		controlRoot: controlRoot,
		config: config.Config{Runtime: config.RuntimeConfig{
			ProviderEnvAllowlist: []string{"HOST_PROVIDER_TOKEN"},
		}},
	}
	run := core.Run{ID: "run-secrets-missing"}

	if _, ok := controller.runtimeRunSecrets(run); ok {
		t.Fatal("new-lineage runtimeRunSecrets without a secrets file did not fail closed")
	}
	redact := controller.runtimeRedaction(run)
	if !redact.failClosed {
		t.Fatal("new-lineage runtime redaction did not fail closed")
	}
	text := redact.Text("provider echoed " + canary)
	if strings.Contains(text, canary) {
		t.Fatalf("fail-closed redaction leaked host env canary: %q", text)
	}
	if !strings.Contains(text, redactionUnavailableMarker) {
		t.Fatalf("fail-closed redaction did not emit the unavailable marker: %q", text)
	}
}

// R9: a new-lineage run whose secrets file exists but is not shell-sourceable
// must fail closed instead of trusting a malformed value.
func TestRuntimeRedactionNewLineageFailsClosedOnCorruptSecretsFile(t *testing.T) {
	canary := "UNREDACTED-CORRUPT-CANARY"
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-secrets-corrupt")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeLaunchFile), []byte("#!/bin/sh\n"), 0o550))
	requireNoError(t, os.WriteFile(filepath.Join(controlPath, runtimeSecretsFile), []byte("HOST_PROVIDER_TOKEN=bare-canary\n"), 0o440))
	controller := &runtimeController{controlRoot: controlRoot}
	run := core.Run{ID: "run-secrets-corrupt"}

	if _, ok := controller.runtimeRunSecrets(run); ok {
		t.Fatal("new-lineage runtimeRunSecrets with a corrupt secrets file did not fail closed")
	}
	redact := controller.runtimeRedaction(run)
	if !redact.failClosed {
		t.Fatal("new-lineage runtime redaction did not fail closed on a corrupt secrets file")
	}
	if text := redact.Text("provider echoed " + canary); !strings.Contains(text, redactionUnavailableMarker) {
		t.Fatalf("fail-closed redaction did not emit the unavailable marker: %q", text)
	}
}

// R10: a legacy adopted run (no launch file) keeps the host provider allowlist
// env fallback, so its redaction remains available and redacts provider values.
func TestRuntimeRedactionLegacyRunFallsBackToHostProviderEnv(t *testing.T) {
	canary := "HOST-PROVIDER-ENV-CANARY"
	root := t.TempDir()
	controlRoot := filepath.Join(root, "run-control")
	controlPath := filepath.Join(controlRoot, "run-legacy-env")
	requireNoError(t, os.MkdirAll(controlPath, 0o700))
	t.Setenv("HOST_PROVIDER_TOKEN", canary)
	controller := &runtimeController{
		controlRoot: controlRoot,
		config: config.Config{Runtime: config.RuntimeConfig{
			ProviderEnvAllowlist: []string{"HOST_PROVIDER_TOKEN"},
		}},
	}
	run := core.Run{ID: "run-legacy-env"}

	values, ok := controller.runtimeRunSecrets(run)
	if !ok {
		t.Fatal("legacy runtimeRunSecrets failed closed instead of falling back to host provider env")
	}
	if len(values) != 1 || values[0] != canary {
		t.Fatalf("legacy runtimeRunSecrets = %#v, want the host provider env canary", values)
	}
	redact := controller.runtimeRedaction(run)
	if redact.failClosed {
		t.Fatal("legacy run failed closed")
	}
	text := redact.Text("provider echoed " + canary)
	if strings.Contains(text, canary) {
		t.Fatalf("legacy host env fallback did not redact the provider value: %q", text)
	}
	if !strings.Contains(text, redactedSecret) {
		t.Fatalf("legacy host env fallback did not replace the provider value: %q", text)
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
