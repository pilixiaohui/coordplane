package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the complete v1 daemon configuration surface.
type Config struct {
	DataDir         string          `yaml:"data_dir"`
	OperatorSocket  string          `yaml:"operator_socket"`
	MaxParallelRuns int             `yaml:"max_parallel_runs"`
	Retention       RetentionConfig `yaml:"retention"`
	Runtime         RuntimeConfig   `yaml:"runtime"`
}

type RetentionConfig struct {
	CompletedWorkspace time.Duration `yaml:"completed_workspace"`
	TerminalTaskRef    time.Duration `yaml:"terminal_task_ref"`
	RunLog             time.Duration `yaml:"run_log"`
}

type RuntimeConfig struct {
	DockerNetwork        string        `yaml:"docker_network"`
	WorkspaceRoot        string        `yaml:"workspace_root"`
	AgentHomeRoot        string        `yaml:"agent_home_root"`
	LogRoot              string        `yaml:"log_root"`
	DefaultImage         string        `yaml:"default_image"`
	ProviderEnvAllowlist []string      `yaml:"provider_env_allowlist"`
	RunTimeout           time.Duration `yaml:"-"`
}

type fileConfig struct {
	DataDir         string              `yaml:"data_dir"`
	OperatorSocket  string              `yaml:"operator_socket"`
	MaxParallelRuns int                 `yaml:"max_parallel_runs"`
	Retention       fileRetentionConfig `yaml:"retention"`
	Runtime         fileRuntimeConfig   `yaml:"runtime"`
}

type fileRetentionConfig struct {
	CompletedWorkspace *yamlDuration `yaml:"completed_workspace"`
	TerminalTaskRef    *yamlDuration `yaml:"terminal_task_ref"`
	RunLog             *yamlDuration `yaml:"run_log"`
}

type fileRuntimeConfig struct {
	DockerNetwork        string        `yaml:"docker_network"`
	WorkspaceRoot        string        `yaml:"workspace_root"`
	AgentHomeRoot        string        `yaml:"agent_home_root"`
	LogRoot              string        `yaml:"log_root"`
	DefaultImage         string        `yaml:"default_image"`
	ProviderEnvAllowlist []string      `yaml:"provider_env_allowlist"`
	RunTimeout           *yamlDuration `yaml:"run_timeout,omitempty"`
}

type yamlDuration time.Duration

func (d *yamlDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string or 0")
	}
	if node.Value == "0" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", node.Value, err)
	}
	*d = yamlDuration(parsed)
	return nil
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var raw fileConfig
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("decode config: document is empty")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Config{}, fmt.Errorf("decode config: %w", err)
		}
		return Config{}, errors.New("decode config: multiple YAML documents are not allowed")
	}
	if raw.Retention.CompletedWorkspace == nil || raw.Retention.TerminalTaskRef == nil || raw.Retention.RunLog == nil {
		return Config{}, errors.New("validate config: all retention fields are required; use 0 for immediate eligibility")
	}
	cfg := Config{
		DataDir:         raw.DataDir,
		OperatorSocket:  raw.OperatorSocket,
		MaxParallelRuns: raw.MaxParallelRuns,
		Retention: RetentionConfig{
			CompletedWorkspace: time.Duration(*raw.Retention.CompletedWorkspace),
			TerminalTaskRef:    time.Duration(*raw.Retention.TerminalTaskRef),
			RunLog:             time.Duration(*raw.Retention.RunLog),
		},
		Runtime: RuntimeConfig{
			DockerNetwork: raw.Runtime.DockerNetwork,
			WorkspaceRoot: raw.Runtime.WorkspaceRoot, AgentHomeRoot: raw.Runtime.AgentHomeRoot,
			LogRoot: raw.Runtime.LogRoot, DefaultImage: raw.Runtime.DefaultImage,
			ProviderEnvAllowlist: append([]string(nil), raw.Runtime.ProviderEnvAllowlist...),
		},
	}
	if raw.Runtime.RunTimeout != nil {
		cfg.Runtime.RunTimeout = time.Duration(*raw.Runtime.RunTimeout)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate normalizes configured paths and rejects values outside the v1
// configuration contract.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("validate config: config is nil")
	}

	var err error
	c.DataDir, err = canonicalPath("data_dir", c.DataDir)
	if err != nil {
		return err
	}
	if c.DataDir == filepath.VolumeName(c.DataDir)+string(filepath.Separator) {
		return errors.New("validate config: data_dir must not be a filesystem root")
	}

	paths := []struct {
		name   string
		target *string
	}{
		{name: "operator_socket", target: &c.OperatorSocket},
		{name: "runtime.workspace_root", target: &c.Runtime.WorkspaceRoot},
		{name: "runtime.agent_home_root", target: &c.Runtime.AgentHomeRoot},
		{name: "runtime.log_root", target: &c.Runtime.LogRoot},
	}
	for _, path := range paths {
		name, target := path.name, path.target
		*target, err = canonicalPath(name, *target)
		if err != nil {
			return err
		}
		if err := requireDescendant(c.DataDir, name, *target); err != nil {
			return err
		}
	}

	if c.MaxParallelRuns <= 0 {
		return errors.New("validate config: max_parallel_runs must be positive")
	}
	retentions := []struct {
		name  string
		value time.Duration
	}{
		{name: "retention.completed_workspace", value: c.Retention.CompletedWorkspace},
		{name: "retention.terminal_task_ref", value: c.Retention.TerminalTaskRef},
		{name: "retention.run_log", value: c.Retention.RunLog},
	}
	for _, retention := range retentions {
		name, value := retention.name, retention.value
		if value < 0 {
			return fmt.Errorf("validate config: %s must be a positive duration or 0", name)
		}
	}

	c.Runtime.DockerNetwork = strings.TrimSpace(c.Runtime.DockerNetwork)
	c.Runtime.DefaultImage = strings.TrimSpace(c.Runtime.DefaultImage)
	if c.Runtime.DockerNetwork == "" {
		return errors.New("validate config: runtime.docker_network is required")
	}
	if c.Runtime.DefaultImage == "" {
		return errors.New("validate config: runtime.default_image is required")
	}
	if c.Runtime.RunTimeout < 0 {
		return errors.New("validate config: runtime.run_timeout must be a positive duration or 0")
	}

	seenEnv := make(map[string]struct{}, len(c.Runtime.ProviderEnvAllowlist))
	for i, name := range c.Runtime.ProviderEnvAllowlist {
		name = strings.TrimSpace(name)
		if !validEnvName(name) {
			return fmt.Errorf("validate config: runtime.provider_env_allowlist[%d] is not a valid environment variable name", i)
		}
		if reservedRuntimeEnvironment(name) {
			return fmt.Errorf("validate config: runtime.provider_env_allowlist[%d] cannot override reserved environment variable %q", i, name)
		}
		if _, exists := seenEnv[name]; exists {
			return fmt.Errorf("validate config: runtime.provider_env_allowlist contains duplicate %q", name)
		}
		seenEnv[name] = struct{}{}
		c.Runtime.ProviderEnvAllowlist[i] = name
	}
	return nil
}

func reservedRuntimeEnvironment(name string) bool {
	switch name {
	case "HOME", "CODEX_HOME", "COORDPLANE_RUN_SOCKET", "COORDPLANE_RUN_TOKEN_FILE":
		return true
	default:
		return false
	}
}

func canonicalPath(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("validate config: %s is required", name)
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("validate config: %s must be an absolute path", name)
	}

	current := filepath.Clean(value)
	var suffix []string
	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("validate config: resolve %s: %w", name, err)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		case !errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("validate config: inspect %s: %w", name, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("validate config: resolve %s: no existing path ancestor", name)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func requireDescendant(dataDir, name, target string) error {
	relative, err := filepath.Rel(dataDir, target)
	if err != nil {
		return fmt.Errorf("validate config: compare %s with data_dir: %w", name, err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("validate config: %s must be inside data_dir", name)
	}
	return nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, char := range name {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_' || (i > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}
