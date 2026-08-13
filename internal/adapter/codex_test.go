package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coordplane/tests/testsupport"
)

func TestCodexBuildsExactStartAndResumeCommands(t *testing.T) {
	launch := LaunchSpec{
		BootstrapPath: ContainerBootstrapPath, Conversation: false,
		ContainerHome: "/home/agent", ContainerWork: "/workspace/project",
		Provider: ProviderConfig{
			Model: "gpt-5", SubagentModel: "subagent", BaseURL: "https://example.invalid/v1", Effort: "high",
		},
	}
	start, err := (Codex{}).BuildStartCommand(launch)
	testsupport.RequireNoError(t, err)
	wantStart := []string{
		"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "-C", "/workspace/project", "-m", "gpt-5",
		"-c", "default_subagent_model=subagent", "-c", "model_reasoning_effort=high",
		"-c", "model_providers.codex.name=codex", "-c", "model_providers.codex.base_url=https://example.invalid/v1",
		"--", bootstrapReferencePrompt(),
	}
	requireCodex(t, start.Executable == "codex" && reflect.DeepEqual(start.Args, wantStart), "start command = ", start)
	wantEnv := map[string]string{"HOME": "/home/agent", "CODEX_HOME": "/home/agent/.codex"}
	requireCodex(t, reflect.DeepEqual(start.Env, wantEnv), "start env = ", start.Env)

	resume, err := (Codex{}).BuildResumeCommand(ResumeSpec{LaunchSpec: launch, NativeSessionID: "thread-123"})
	testsupport.RequireNoError(t, err)
	wantResume := []string{
		"exec", "resume", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "-c", "default_subagent_model=subagent", "-c", "model_reasoning_effort=high",
		"-c", "model_providers.codex.name=codex", "-c", "model_providers.codex.base_url=https://example.invalid/v1",
		"thread-123", "--", bootstrapReferencePrompt(),
	}
	requireCodex(t, resume.Executable == "codex" && reflect.DeepEqual(resume.Args, wantResume) && reflect.DeepEqual(resume.Env, wantEnv), "resume command = ", resume)

	empty, err := (Codex{}).BuildStartCommand(LaunchSpec{
		BootstrapPath: ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project",
	})
	testsupport.RequireNoError(t, err)
	wantEmpty := []string{
		"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "-C", "/workspace/project", "--", bootstrapReferencePrompt(),
	}
	requireCodex(t, reflect.DeepEqual(empty.Args, wantEmpty), "empty provider start command = ", empty.Args)
	emptyResume, err := (Codex{}).BuildResumeCommand(ResumeSpec{
		LaunchSpec: LaunchSpec{
			BootstrapPath: ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project",
		},
		NativeSessionID: "thread-123",
	})
	testsupport.RequireNoError(t, err)
	wantEmptyResume := []string{
		"exec", "resume", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "thread-123", "--", bootstrapReferencePrompt(),
	}
	requireCodex(t, reflect.DeepEqual(emptyResume.Args, wantEmptyResume), "empty provider resume command = ", emptyResume.Args)
	for _, command := range []CommandSpec{start, resume, empty, emptyResume} {
		for _, forbidden := range []string{"sh -c", "bash -c", ";", "&&", "$(", "`"} {
			requireCodex(t, !strings.Contains(strings.Join(command.Args, " "), forbidden), "provider command contains ", forbidden, ": ", command)
		}
	}
}

func TestCodexPartialGoldenTranscriptFromLocalCLI(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "codex-0.146.0-partial-golden.jsonl"))
	testsupport.RequireNoError(t, err)
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		frame := append([]byte(nil), scanner.Bytes()...)
		event, err := (Codex{}).ParseEvent(frame)
		testsupport.RequireNoError(t, err)
		switch lineNumber {
		case 1:
			requireCodex(t, event.Kind == EventSessionStarted && event.NativeSessionID == "019ffc5e-bde3-7520-bbf0-294176234154", "golden thread frame = ", event)
			requireCodex(t, !strings.Contains(string(event.Raw), "message"), "thread frame leaked fields: ", string(event.Raw))
		case 2:
			requireCodex(t, event.Kind == EventProtocol && bytes.Equal(bytes.TrimSpace(event.Raw), []byte(`{"type":"turn.started"}`)), "golden turn frame = ", event)
		default:
			requireCodex(t, event.Kind == EventProviderError && event.Message != "", "golden error frame = ", event)
			requireCodex(t, !strings.Contains(string(event.Raw), "usage"), "error frame leaked fields: ", string(event.Raw))
		}
	}
	testsupport.RequireNoError(t, scanner.Err())
	if lineNumber != 7 {
		t.Fatalf("golden transcript has %d frames, want 7", lineNumber)
	}
}

func TestCodexJSONLWhitelistAndPrivateFieldDropping(t *testing.T) {
	for _, test := range []struct {
		name, frame, session, message, forbidden string
		kind                                     EventKind
		reject, noRaw                            bool
	}{
		{name: "thread started", frame: `{"type":"thread.started","thread_id":"thread-1","extra":"dropped"}`, kind: EventSessionStarted, session: "thread-1", forbidden: "extra"},
		{name: "turn completed drops usage", frame: `{"type":"turn.completed","usage":{"input_tokens":5},"extra":"dropped"}`, kind: EventProtocol, forbidden: "usage"},
		{name: "thread completed drops usage", frame: `{"type":"thread.completed","thread_id":"thread-1","usage":{"output_tokens":5}}`, kind: EventProtocol, forbidden: "usage"},
		{name: "visible agent message", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","role":"assistant","content":[{"type":"reasoning","encrypted_content":"private"},{"type":"output_text","text":"visible","annotations":[]},{"type":"output_text","text":" "},{"type":"reasoning","summary":[{"text":"private"}]}]}}`, kind: EventProtocol, forbidden: "encrypted_content"},
		{name: "agent message only private content", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","role":"assistant","content":[{"type":"reasoning","encrypted_content":"private"}]}}`, noRaw: true},
		{name: "empty started agent message", frame: `{"type":"item.started","item":{"id":"item_1","type":"agent_message","role":"assistant","content":[]}}`, noRaw: true},
		{name: "nested tool call", frame: `{"type":"item.completed","item":{"id":"item_1","type":"tool_call","tool_call":{"id":"call-1","name":"Read","arguments":{"path":"README.md"},"secret":"dropped"}}}`, kind: EventProtocol, forbidden: "secret"},
		{name: "direct tool call", frame: `{"type":"item.started","item":{"id":"item_1","type":"tool_call","tool_call_id":"call-1","name":"Read"}}`, kind: EventProtocol, forbidden: "arguments"},
		{name: "private reasoning item", frame: `{"type":"item.completed","item":{"id":"item_1","type":"reasoning","encrypted_content":"private"}}`, noRaw: true},
		{name: "unknown event", frame: `{"type":"future.event","secret":"dropped"}`, noRaw: true},
		{name: "resume session not found", frame: `{"type":"error","message":"Session not found for thread_id: thread-1"}`, kind: EventResumeUnavailable, message: "Session not found for thread_id: thread-1"},
		{name: "provider error", frame: `{"type":"error","message":"provider unavailable"}`, kind: EventProviderError, message: "provider unavailable"},
		{name: "error item", frame: `{"type":"item.completed","item":{"id":"item_1","type":"error","message":"provider unavailable"}}`, kind: EventProviderError, message: "provider unavailable"},
		{name: "bad json", frame: `not-json`, reject: true},
		{name: "array frame", frame: `[]`, reject: true},
		{name: "missing type", frame: `{"thread_id":"thread-1"}`, reject: true},
		{name: "numeric type", frame: `{"type":1}`, reject: true},
		{name: "thread missing id", frame: `{"type":"thread.started"}`, reject: true},
		{name: "item missing", frame: `{"type":"item.completed"}`, reject: true},
		{name: "item missing type", frame: `{"type":"item.completed","item":{"id":"item_1"}}`, reject: true},
		{name: "agent missing role", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","content":[]}}`, reject: true},
		{name: "content not array", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","role":"assistant","content":{}}}`, reject: true},
		{name: "content block not object", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","role":"assistant","content":[1]}}`, reject: true},
		{name: "output text non-string", frame: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","role":"assistant","content":[{"type":"output_text","text":1}]}}`, reject: true},
		{name: "tool call malformed", frame: `{"type":"item.completed","item":{"id":"item_1","type":"tool_call","tool_call":{"name":"Read","arguments":"path"}}}`, reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := (Codex{}).ParseEvent([]byte(test.frame))
			if test.reject {
				requireCodex(t, err != nil, "malformed Codex event was accepted")
				return
			}
			testsupport.RequireNoError(t, err)
			rawOK := len(event.Raw) > 0
			if test.noRaw {
				rawOK = len(event.Raw) == 0
			}
			requireCodex(t, event.Kind == test.kind && event.NativeSessionID == test.session && event.Message == test.message && rawOK && (test.forbidden == "" || !strings.Contains(string(event.Raw), test.forbidden)), "event = ", event)
		})
	}
}

func TestCodexResumeCompatibilityFencesTaskAgentAndWorkspace(t *testing.T) {
	base := SessionContext{AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"}
	requireCodex(t, (Codex{}).ResumeCompatible(base, base), "identical session context was not resumable")
	for name, changed := range map[string]SessionContext{
		"adapter":   {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"agent":     {AdapterID: "codex", AgentID: "agent-b", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"task":      {AdapterID: "codex", AgentID: "agent-a", TaskID: "task-b", WorkspaceID: "workspace-a"},
		"workspace": {AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-b"},
	} {
		t.Run(name, func(t *testing.T) {
			requireCodex(t, !(Codex{}).ResumeCompatible(base, changed), "incompatible context accepted: ", changed)
		})
	}
}

func TestCodexMetadataAndInjectSurface(t *testing.T) {
	metadata := (Codex{}).Metadata()
	requireCodex(t, metadata.Name == "codex" && metadata.ExecutionModel == ExecutionOneShot && metadata.SupportsResume && !metadata.SupportsInject, "Codex metadata = ", metadata)
	requireCodex(t, reflect.DeepEqual(metadata.AllowedEfforts, []string{"low", "medium", "high", "xhigh", "max", "ultra"}), "Codex allowed efforts = ", metadata.AllowedEfforts)
	_, err := (Codex{}).BuildInjectInput(MessageInput{})
	requireCodex(t, errors.Is(err, ErrInjectUnsupported), "Codex inject error = ", err)
}

func TestCodexRequiresValidLaunchPathsAndSession(t *testing.T) {
	valid := LaunchSpec{BootstrapPath: ContainerBootstrapPath, ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}
	if _, err := (Codex{}).BuildStartCommand(valid); err != nil {
		t.Fatalf("valid launch rejected: %v", err)
	}
	if _, err := (Codex{}).BuildResumeCommand(ResumeSpec{LaunchSpec: valid}); err == nil {
		t.Fatal("empty session ID accepted for resume")
	}
	if _, err := (Codex{}).BuildStartCommand(LaunchSpec{BootstrapPath: "/tmp/bootstrap", ContainerHome: "/home/agent", ContainerWork: "/workspace/project"}); err == nil {
		t.Fatal("arbitrary bootstrap path accepted")
	}
}

func requireCodex(t *testing.T, condition bool, details ...any) {
	t.Helper()
	if !condition {
		t.Fatal(details...)
	}
}
