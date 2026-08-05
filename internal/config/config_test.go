package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/config"
	"coordplane/tests/testsupport"
)

func TestLoadAcceptsStrictMinimalConfigAndZeroRetention(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg, err := config.Load(writeConfig(t, validConfig(dataDir)))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DataDir != dataDir || cfg.OperatorSocket != filepath.Join(dataDir, "operator.sock") {
		t.Fatalf("paths = data_dir %q operator_socket %q", cfg.DataDir, cfg.OperatorSocket)
	}
	if cfg.MaxParallelRuns != 4 {
		t.Fatalf("max_parallel_runs = %d, want 4", cfg.MaxParallelRuns)
	}
	if cfg.Retention.CompletedWorkspace != 0 || cfg.Retention.TerminalTaskRef != 168*time.Hour || cfg.Retention.RunLog != 0 {
		t.Fatalf("retention = %+v", cfg.Retention)
	}
	if strings.Join(cfg.Runtime.ProviderEnvAllowlist, ",") != "ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,ANTHROPIC_DEFAULT_OPUS_MODEL,ANTHROPIC_DEFAULT_SONNET_MODEL,ANTHROPIC_DEFAULT_HAIKU_MODEL,CLAUDE_CODE_SUBAGENT_MODEL,CLAUDE_CODE_EFFORT_LEVEL" {
		t.Fatalf("provider allowlist = %#v", cfg.Runtime.ProviderEnvAllowlist)
	}
	if cfg.Runtime.RunTimeout != 0 {
		t.Fatalf("default run timeout = %s, want disabled", cfg.Runtime.RunTimeout)
	}
	if cfg.Runtime.ShutdownGrace != config.DefaultShutdownGrace {
		t.Fatalf("default shutdown grace = %s, want %s", cfg.Runtime.ShutdownGrace, config.DefaultShutdownGrace)
	}
	if cfg.Git.CaptureHelperImage != cfg.Runtime.DefaultImage || cfg.Git.CaptureTimeout != config.DefaultCaptureTimeout ||
		cfg.Git.MaximumBundleBytes != config.DefaultCaptureBundleSize || cfg.Git.MaximumObjects != config.DefaultCaptureObjects ||
		cfg.Git.MaximumHandoffBytes != config.DefaultHandoffSize {
		t.Fatalf("default Git capture config = %+v", cfg.Git)
	}
}

func TestLoadRejectsInvalidConfigWithoutFallback(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	valid := validConfig(dataDir)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown top-level field", raw: valid + "unexpected: true\n", want: "field unexpected not found"},
		{name: "unknown nested field", raw: strings.Replace(valid, "  default_image:", "  unexpected: true\n  default_image:", 1), want: "field unexpected not found"},
		{name: "second document", raw: valid + "---\n" + valid, want: "multiple YAML documents"},
		{name: "empty second document", raw: valid + "---\n", want: "multiple YAML documents"},
		{name: "negative retention", raw: strings.Replace(valid, "completed_workspace: 0", "completed_workspace: -1s", 1), want: "positive duration or 0"},
		{name: "missing retention is not implicit zero", raw: strings.Replace(valid, "  run_log: 0\n", "", 1), want: "all retention fields are required"},
		{name: "non-positive parallelism", raw: strings.Replace(valid, "max_parallel_runs: 4", "max_parallel_runs: 0", 1), want: "must be positive"},
		{name: "relative data directory", raw: strings.Replace(valid, "data_dir: "+dataDir, "data_dir: relative/data", 1), want: "absolute path"},
		{name: "operator socket outside", raw: strings.Replace(valid, filepath.Join(dataDir, "operator.sock"), filepath.Join(base, "operator.sock"), 1), want: "inside data_dir"},
		{name: "workspace traversal outside", raw: strings.Replace(valid, filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "..", "workspaces"), 1), want: "inside data_dir"},
		{name: "invalid provider env", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN=value", 1), want: "valid environment variable name"},
		{name: "duplicate provider env", raw: strings.Replace(valid, "    - ANTHROPIC_AUTH_TOKEN\n", "    - ANTHROPIC_AUTH_TOKEN\n    - ANTHROPIC_AUTH_TOKEN\n", 1), want: "contains duplicate"},
		{name: "retired API key", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", 1), want: "Claude provider environment catalog"},
		{name: "proxy credential", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "HTTPS_PROXY", 1), want: "Claude provider environment catalog"},
		{name: "arbitrary provider env", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "PROVIDER_EXTRA", 1), want: "Claude provider environment catalog"},
		{name: "reserved HOME", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "HOME", 1), want: "reserved environment variable"},
		{name: "reserved Run socket", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "COORDPLANE_RUN_SOCKET", 1), want: "reserved environment variable"},
		{name: "reserved token file", raw: strings.Replace(valid, "ANTHROPIC_AUTH_TOKEN", "COORDPLANE_RUN_TOKEN_FILE", 1), want: "reserved environment variable"},
		{name: "negative run timeout", raw: strings.Replace(valid, "  default_image:", "  run_timeout: -1s\n  default_image:", 1), want: "runtime.run_timeout must be a positive duration or 0"},
		{name: "zero shutdown grace", raw: strings.Replace(valid, "  default_image:", "  shutdown_grace: 0\n  default_image:", 1), want: "runtime.shutdown_grace must be a positive duration"},
		{name: "invalid capture timeout", raw: valid + "git:\n  capture_timeout: 0\n", want: "git.capture_timeout must be a positive duration"},
		{name: "handoff below bundle", raw: valid + "git:\n  maximum_bundle_bytes: 20\n  maximum_handoff_bytes: 10\n", want: "at least maximum_bundle_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadAcceptsRuntimeRunTimeout(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	raw := strings.Replace(validConfig(dataDir), "  default_image:", "  run_timeout: 45s\n  default_image:", 1)
	cfg, err := config.Load(writeConfig(t, raw))
	testsupport.RequireNoError(t, err)
	if cfg.Runtime.RunTimeout != 45*time.Second {
		t.Fatalf("run timeout = %s, want 45s", cfg.Runtime.RunTimeout)
	}
}

func TestLoadAcceptsConfigurableShutdownGrace(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	raw := strings.Replace(validConfig(dataDir), "  default_image:", "  shutdown_grace: 275ms\n  default_image:", 1)
	cfg, err := config.Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("load shutdown grace: %v", err)
	}
	if cfg.Runtime.ShutdownGrace != 275*time.Millisecond {
		t.Fatalf("shutdown grace = %s, want 275ms", cfg.Runtime.ShutdownGrace)
	}
}

func TestLoadAcceptsBoundedGitCaptureHelperConfig(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	raw := validConfig(dataDir) + `git:
  capture_helper_image: coordplane-git-helper:test
  capture_timeout: 45s
  maximum_bundle_bytes: 1048576
  maximum_objects: 5000
  maximum_handoff_bytes: 4194304
`
	cfg, err := config.Load(writeConfig(t, raw))
	testsupport.RequireNoError(t, err)
	if cfg.Git.CaptureHelperImage != "coordplane-git-helper:test" || cfg.Git.CaptureTimeout != 45*time.Second ||
		cfg.Git.MaximumBundleBytes != 1<<20 || cfg.Git.MaximumObjects != 5000 || cfg.Git.MaximumHandoffBytes != 4<<20 {
		t.Fatalf("Git capture config = %+v", cfg.Git)
	}
}

func TestLoadRejectsRuntimeRootEscapingThroughSymlink(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "escaped")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	raw := strings.Replace(validConfig(dataDir), filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "escaped", "workspaces"), 1)
	_, err := config.Load(writeConfig(t, raw))
	if err == nil || !strings.Contains(err.Error(), "inside data_dir") {
		t.Fatalf("Load() error = %v, want symlink escape rejection", err)
	}
}

func validConfig(dataDir string) string {
	return string(testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: filepath.Join(dataDir, "operator.sock"), MaxParallelRuns: 4, CompletedWorkspace: "0", TerminalTaskRef: "168h", RunLog: "0", DockerNetwork: "coordplane", DefaultImage: "coordplane-agent:latest", ProviderEnv: config.ClaudeProviderEnvCatalog()}))
}

func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	return testsupport.WriteFile(t, filepath.Join(t.TempDir(), "coordplane.yaml"), []byte(raw), 0o600)
}
