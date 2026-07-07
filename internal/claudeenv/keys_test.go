package claudeenv_test

import (
	"reflect"
	"testing"

	"coordplane/internal/claudeenv"
)

func TestRuntimeKeysAreTheExactClaudeCLIEnvironmentContract(t *testing.T) {
	want := []string{
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_EFFORT_LEVEL",
	}
	if !reflect.DeepEqual(claudeenv.RuntimeKeys, want) {
		t.Fatalf("RuntimeKeys = %#v, want exact Claude CLI env contract %#v", claudeenv.RuntimeKeys, want)
	}
}
