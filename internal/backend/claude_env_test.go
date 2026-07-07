package backend

import (
	"reflect"
	"testing"

	"coordplane/internal/claudeenv"
)

func TestEffectiveClaudeEnvKeysDefaultsToBuiltInClaudeCLIContract(t *testing.T) {
	got := effectiveClaudeEnvKeys(nil)
	if !reflect.DeepEqual(got, claudeenv.RuntimeKeys) {
		t.Fatalf("effectiveClaudeEnvKeys(nil) = %#v, want built-in Claude CLI env contract %#v", got, claudeenv.RuntimeKeys)
	}
	got[0] = "mutated"
	if claudeenv.RuntimeKeys[0] == "mutated" {
		t.Fatalf("effectiveClaudeEnvKeys returned mutable package slice")
	}
}

func TestEffectiveClaudeEnvKeysAllowsExplicitSubsetOverride(t *testing.T) {
	got := effectiveClaudeEnvKeys([]string{"ANTHROPIC_AUTH_TOKEN"})
	want := []string{"ANTHROPIC_AUTH_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effectiveClaudeEnvKeys override = %#v, want %#v", got, want)
	}
}
