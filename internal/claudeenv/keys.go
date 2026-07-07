package claudeenv

var RuntimeKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
	"CLAUDE_CODE_EFFORT_LEVEL",
}

func Allowed(key string) bool {
	for _, allowed := range RuntimeKeys {
		if key == allowed {
			return true
		}
	}
	return false
}
