package adapter

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"coordplane/tests/testsupport"
)

func TestProductionRegistryContainsOnlyClaude(t *testing.T) {
	requireClaude(t, reflect.DeepEqual(Production().Names(), []string{"claude"}), "production adapters = ", Production().Names())
	entry, ok := Production().Lookup("claude")
	requireClaude(t, ok, "Claude adapter is not registered")
	requireClaude(t, entry.Metadata().ExecutionModel == ExecutionOneShot && entry.Metadata().SupportsResume && !entry.Metadata().SupportsInject, "Claude metadata = ", entry.Metadata())
	_, err := entry.BuildInjectInput(MessageInput{})
	requireClaude(t, errors.Is(err, ErrInjectUnsupported), "Claude inject error = ", err)
}

func TestClaudeBuildsExactStartAndResumeCommands(t *testing.T) {
	launch := LaunchSpec{BootstrapPath: ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}
	start, err := (Claude{}).BuildStartCommand(launch)
	testsupport.RequireNoError(t, err)
	common := []string{"-p", "--bare", "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	requireClaude(t, start.Executable == "claude" && reflect.DeepEqual(start.Args, append(append([]string(nil), common...), "--", bootstrapReferencePrompt())) && reflect.DeepEqual(start.Env, map[string]string{"HOME": "/home/agent"}), "start command = ", start)
	resume, err := (Claude{}).BuildResumeCommand(ResumeSpec{LaunchSpec: launch, NativeSessionID: "0190a1b2-session"})
	testsupport.RequireNoError(t, err)
	requireClaude(t, resume.Executable == "claude" && reflect.DeepEqual(resume.Args, append(append([]string(nil), common...), "--resume", "0190a1b2-session", "--", bootstrapReferencePrompt())) && reflect.DeepEqual(resume.Env, start.Env), "resume command = ", resume)
	for _, command := range []CommandSpec{start, resume} {
		for _, forbidden := range []string{"--continue", "--fork-session", "--session-id", "--no-session-persistence", "sh -c"} {
			requireClaude(t, !strings.Contains(strings.Join(command.Args, " "), forbidden), "provider command contains ", forbidden, ": ", command)
		}
	}
}

func TestClaudeTypedStreamJSONConformance(t *testing.T) {
	for _, test := range []struct {
		name, frame, session, message, forbidden string
		kind                                     EventKind
		reject, noRaw                            bool
	}{
		{name: "init", frame: `{"type":"system","subtype":"init","session_id":"0190f8b7-acde"}`, kind: EventSessionStarted, session: "0190f8b7-acde"},
		{name: "success", frame: `{"type":"result","subtype":"success","is_error":false,"result":"done"}`, kind: EventProtocol},
		{name: "error result", frame: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"provider unavailable"}`, kind: EventProviderError, message: "provider unavailable"},
		{name: "error envelope", frame: `{"type":"error","error":{"message":"invalid request"}}`, kind: EventProviderError, message: "invalid request"},
		{name: "assistant text is not resume evidence", frame: `{"type":"assistant","message":"session not found"}`, kind: EventProtocol},
		{name: "assistant object", frame: `{"type":"assistant","message":{"type":"message","id":"msg-1","model":"claude","role":"assistant","content":[{"type":"thinking","thinking":"private reasoning","signature":"private signature"},{"type":"text","text":"visible text"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"}}],"usage":{"input_tokens":1}}}`, kind: EventProtocol, forbidden: "thinking"},
		{name: "missing message", frame: `{"type":"assistant"}`, reject: true},
		{name: "null message", frame: `{"type":"assistant","message":null}`, reject: true},
		{name: "empty object message", frame: `{"type":"assistant","message":{}}`, reject: true},
		{name: "empty array message", frame: `{"type":"assistant","message":[]}`, reject: true},
		{name: "unknown block", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"unknown"}]}}`, reject: true},
		{name: "thinking without signature dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private"}]}}`, kind: EventProtocol, noRaw: true},
		{name: "signature only thinking dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","signature":"sig"}]}}`, kind: EventProtocol, noRaw: true},
		{name: "hollow thinking dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"visible"}]}}`, kind: EventProtocol, forbidden: "sig"},
		{name: "thinking with extra text field dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig","text":"cross-variant"},{"type":"text","text":"visible"}]}}`, kind: EventProtocol, forbidden: "cross-variant"},
		{name: "blank text dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":" "},{"type":"text","text":"visible"}]}}`, kind: EventProtocol, forbidden: `"text":" "`},
		{name: "empty text dropped", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"visible"}]}}`, kind: EventProtocol, forbidden: `"text":""`},
		{name: "text with extra id field", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"visible","id":"cross-variant"}]}}`, kind: EventProtocol, forbidden: "cross-variant"},
		{name: "text with null citations", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"visible","citations":null}]}}`, kind: EventProtocol, forbidden: "citations"},
		{name: "text with citation objects", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"visible","citations":[{"type":"page_location","url":"https://example.com"}]}]}}`, kind: EventProtocol, forbidden: "citations"},
		{name: "tool use with partial field", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"},"partial":true}]}}`, kind: EventProtocol, forbidden: "partial"},
		{name: "tool blank id", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":" ","name":"Read","input":{}}]}}`, reject: true},
		{name: "tool missing name", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","input":{}}]}}`, reject: true},
		{name: "tool null input", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":null}]}}`, reject: true},
		{name: "tool string input", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":"path"}]}}`, reject: true},
		{name: "tool number input", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":1}]}}`, reject: true},
		{name: "tool array input", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":[]}]}}`, reject: true},
		{name: "text non-string", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":123}]}}`, reject: true},
		{name: "thinking non-string", frame: `{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":123}]}}`, reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := (Claude{}).ParseEvent([]byte(test.frame))
			if test.reject {
				requireClaude(t, err != nil, "malformed Claude assistant message was accepted")
				return
			}
			testsupport.RequireNoError(t, err)
			rawOK := len(event.Raw) > 0
			if test.noRaw {
				rawOK = len(event.Raw) == 0
			}
			requireClaude(t, event.Kind == test.kind && event.NativeSessionID == test.session && event.Message == test.message && rawOK && (test.forbidden == "" || !strings.Contains(string(event.Raw), test.forbidden)), "event = ", event)
		})
	}
}

func TestClaudeResumeCompatibilityFencesTaskAgentAndWorkspace(t *testing.T) {
	base := SessionContext{AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"}
	requireClaude(t, (Claude{}).ResumeCompatible(base, base), "identical session context was not resumable")
	for name, changed := range map[string]SessionContext{
		"adapter":   {AdapterID: "other", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"agent":     {AdapterID: "claude", AgentID: "agent-b", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"task":      {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-b", WorkspaceID: "workspace-a"},
		"workspace": {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-b"},
	} {
		t.Run(name, func(t *testing.T) {
			requireClaude(t, !(Claude{}).ResumeCompatible(base, changed), "incompatible context accepted: ", changed)
		})
	}
}

func requireClaude(t *testing.T, condition bool, details ...any) {
	t.Helper()
	if !condition {
		t.Fatal(details...)
	}
}
