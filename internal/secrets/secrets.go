package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"coordplane/internal/claudeenv"
)

type Request struct {
	TeamID     string
	AgentID    string
	RuntimeID  string
	AttemptID  string
	CLIBackend string
}

type Material struct {
	Source      string
	Env         map[string]string
	IdentityRef string
}

type Provider interface {
	RuntimeAuth(context.Context, Request) (Material, error)
	Redact(string) string
}

type EnvProvider struct {
	Keys   []string
	Lookup func(string) (string, bool)
}

func NewEnvProvider(keys []string) *EnvProvider {
	return &EnvProvider{Keys: keys, Lookup: os.LookupEnv}
}

func (p *EnvProvider) RuntimeAuth(ctx context.Context, req Request) (Material, error) {
	select {
	case <-ctx.Done():
		return Material{}, ctx.Err()
	default:
	}
	if req.CLIBackend != "claude" {
		return Material{}, nil
	}
	if p == nil {
		return Material{}, nil
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	keys, err := NormalizeClaudeEnvKeys(p.Keys)
	if err != nil {
		return Material{}, err
	}
	env := make(map[string]string)
	for _, key := range keys {
		value, ok := lookup(key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return Material{}, fmt.Errorf("secret provider: value for %s contains unsupported control characters", key)
		}
		env[key] = value
	}
	if len(env) == 0 {
		return Material{}, nil
	}
	return Material{
		Source:      "secret_provider_env",
		Env:         env,
		IdentityRef: identityRef(env),
	}, nil
}

func (p *EnvProvider) Redact(value string) string {
	if p == nil {
		return value
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	keys, err := NormalizeClaudeEnvKeys(p.Keys)
	if err != nil {
		return value
	}
	out := value
	for _, key := range keys {
		secret, ok := lookup(key)
		if !ok || secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, "<redacted:"+key+">")
	}
	return out
}

func NormalizeClaudeEnvKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		if strings.ContainsAny(key, "=\x00\r\n") {
			return nil, fmt.Errorf("secret provider: invalid env key %q", raw)
		}
		if !claudeenv.Allowed(key) {
			return nil, fmt.Errorf("secret provider: env key %q is not allowlisted for claude auth", key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func ValidateMaterial(material Material) error {
	if len(material.Env) == 0 {
		return nil
	}
	if material.Source == "" {
		return errors.New("secret provider: auth material source is required")
	}
	for key, value := range material.Env {
		normalized, err := NormalizeClaudeEnvKeys([]string{key})
		if err != nil {
			return err
		}
		if len(normalized) != 1 || normalized[0] != key {
			return fmt.Errorf("secret provider: invalid env key %q", key)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("secret provider: value for %s is empty", key)
		}
	}
	return nil
}

func EnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func identityRef(env map[string]string) string {
	keys := EnvKeys(env)
	sum := sha256.Sum256([]byte(strings.Join(keys, ",")))
	return "auth_env_keys_sha256_" + hex.EncodeToString(sum[:])
}
