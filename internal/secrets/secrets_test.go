package secrets_test

import (
	"context"
	"strings"
	"testing"

	"coordplane/internal/claudeenv"
	"coordplane/internal/secrets"
)

func TestEnvProviderInjectsOnlyConfiguredClaudeCLIEnvAndRedactsValues(t *testing.T) {
	provider := &secrets.EnvProvider{
		Keys: claudeenv.RuntimeKeys,
		Lookup: func(key string) (string, bool) {
			switch key {
			case "ANTHROPIC_AUTH_TOKEN":
				return "auth-token-secret", true
			case "ANTHROPIC_BASE_URL":
				return "https://anthropic.test", true
			case "ANTHROPIC_MODEL":
				return "claude-test-model", true
			default:
				return "", false
			}
		},
	}
	material, err := provider.RuntimeAuth(context.Background(), secrets.Request{
		TeamID:     "team",
		AgentID:    "developer",
		RuntimeID:  "rt",
		AttemptID:  "att",
		CLIBackend: "claude",
	})
	if err != nil {
		t.Fatalf("runtime auth: %v", err)
	}
	want := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "auth-token-secret",
		"ANTHROPIC_BASE_URL":   "https://anthropic.test",
		"ANTHROPIC_MODEL":      "claude-test-model",
	}
	if material.Source != "secret_provider_env" {
		t.Fatalf("material source = %q, want secret_provider_env", material.Source)
	}
	for key, value := range want {
		if material.Env[key] != value {
			t.Fatalf("material env[%s] = %q, want %q in %+v", key, material.Env[key], value, material.Env)
		}
	}
	if len(material.Env) != len(want) {
		t.Fatalf("material env = %+v, want only configured Claude CLI env values", material.Env)
	}
	if material.IdentityRef == "" || strings.Contains(material.IdentityRef, "auth-token-secret") {
		t.Fatalf("identity ref = %q, want non-secret stable ref", material.IdentityRef)
	}
	redacted := provider.Redact("probe failed with auth-token-secret")
	if strings.Contains(redacted, "auth-token-secret") || !strings.Contains(redacted, "<redacted:ANTHROPIC_AUTH_TOKEN>") {
		t.Fatalf("redacted = %q, want secret removed", redacted)
	}
}

func TestEnvProviderRejectsForbiddenClaudeAuthKeys(t *testing.T) {
	for _, key := range []string{
		"HOME",
		"PATH",
		"COORDPLANE_DB_PATH",
		"AWS_SECRET_ACCESS_KEY",
		"ANTHROPIC_API_KEY",
		"CLAUDE_API_KEY",
		"CLAUDE_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
	} {
		t.Run(key, func(t *testing.T) {
			provider := &secrets.EnvProvider{Keys: []string{key}, Lookup: func(string) (string, bool) {
				return "secret", true
			}}
			if _, err := provider.RuntimeAuth(context.Background(), secrets.Request{CLIBackend: "claude"}); err == nil {
				t.Fatalf("RuntimeAuth accepted forbidden key %s", key)
			}
		})
	}
}
