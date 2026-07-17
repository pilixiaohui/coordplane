package adapter

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestProductionRegistryContainsOnlyClaude(t *testing.T) {
	registry := Production()
	if got, want := registry.Names(), []string{"claude"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("production adapters = %v, want %v", got, want)
	}
	entry, ok := registry.Lookup("claude")
	if !ok {
		t.Fatal("Claude adapter is not registered")
	}
	metadata := entry.Metadata()
	if metadata.ExecutionModel != ExecutionOneShot || !metadata.SupportsResume || metadata.SupportsInject {
		t.Fatalf("Claude metadata = %#v", metadata)
	}
	if _, err := entry.BuildInjectInput(MessageInput{}); !errors.Is(err, ErrInjectUnsupported) {
		t.Fatalf("Claude inject error = %v", err)
	}
}

func TestClaudeBuildsExactStartAndResumeCommands(t *testing.T) {
	entry := Claude{}
	launch := LaunchSpec{BootstrapPath: ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}
	start, err := entry.BuildStartCommand(launch)
	if err != nil {
		t.Fatal(err)
	}
	common := []string{"-p", "--bare", "--verbose", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	wantStart := append(append([]string(nil), common...), "--", bootstrapReferencePrompt())
	if start.Executable != "claude" || !reflect.DeepEqual(start.Args, wantStart) || !reflect.DeepEqual(start.Env, map[string]string{"HOME": "/home/agent"}) {
		t.Fatalf("start command = %#v", start)
	}
	resume, err := entry.BuildResumeCommand(ResumeSpec{LaunchSpec: launch, NativeSessionID: "0190a1b2-session"})
	if err != nil {
		t.Fatal(err)
	}
	wantResume := append(append([]string(nil), common...), "--resume", "0190a1b2-session", "--", bootstrapReferencePrompt())
	if resume.Executable != "claude" || !reflect.DeepEqual(resume.Args, wantResume) || !reflect.DeepEqual(resume.Env, start.Env) {
		t.Fatalf("resume command = %#v", resume)
	}
	for _, command := range []CommandSpec{start, resume} {
		joined := strings.Join(command.Args, " ")
		for _, forbidden := range []string{"--continue", "--fork-session", "--session-id", "--no-session-persistence", "sh -c"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("provider command contains %q: %#v", forbidden, command)
			}
		}
	}
}

func TestClaudeTypedStreamJSONConformance(t *testing.T) {
	entry := Claude{}
	tests := []struct {
		name, frame, session, message string
		kind                          EventKind
	}{
		{name: "init", frame: `{"type":"system","subtype":"init","session_id":"0190f8b7-acde"}`, kind: EventSessionStarted, session: "0190f8b7-acde"},
		{name: "success", frame: `{"type":"result","subtype":"success","is_error":false,"result":"done"}`, kind: EventProtocol},
		{name: "error result", frame: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"provider unavailable"}`, kind: EventProviderError, message: "provider unavailable"},
		{name: "error envelope", frame: `{"type":"error","error":{"message":"invalid request"}}`, kind: EventProviderError, message: "invalid request"},
		{name: "assistant text is not resume evidence", frame: `{"type":"assistant","message":"session not found"}`, kind: EventProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := entry.ParseEvent([]byte(test.frame))
			if err != nil {
				t.Fatal(err)
			}
			if event.Kind != test.kind || event.NativeSessionID != test.session || event.Message != test.message || len(event.Raw) == 0 {
				t.Fatalf("event = %#v", event)
			}
		})
	}
	for _, frame := range []string{"", `{`, `{"type":"system","subtype":"init"}`, `{"type":"result","subtype":"success"}`} {
		if _, err := entry.ParseEvent([]byte(frame)); err == nil {
			t.Fatalf("malformed frame accepted: %q", frame)
		}
	}
}

func TestClaudeResumeCompatibilityFencesTaskAgentAndWorkspace(t *testing.T) {
	entry := Claude{}
	base := SessionContext{AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"}
	if !entry.ResumeCompatible(base, base) {
		t.Fatal("identical session context was not resumable")
	}
	for name, changed := range map[string]SessionContext{
		"adapter":   {AdapterID: "other", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"agent":     {AdapterID: "claude", AgentID: "agent-b", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"task":      {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-b", WorkspaceID: "workspace-a"},
		"workspace": {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-b"},
	} {
		t.Run(name, func(t *testing.T) {
			if entry.ResumeCompatible(base, changed) {
				t.Fatalf("incompatible context accepted: %#v", changed)
			}
		})
	}
}
