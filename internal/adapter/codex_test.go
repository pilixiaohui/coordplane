package adapter

import (
	"errors"
	"reflect"
	"testing"
)

func TestProductionRegistryContainsOneStaticOneShotAdapter(t *testing.T) {
	registry := Production()
	if got, want := registry.Names(), []string{"codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("production adapters = %v, want %v", got, want)
	}
	entry, ok := registry.Lookup("codex")
	if !ok {
		t.Fatal("Codex adapter is not registered")
	}
	metadata := entry.Metadata()
	if metadata.ExecutionModel != ExecutionOneShot || !metadata.SupportsResume || metadata.SupportsInject {
		t.Fatalf("Codex metadata = %#v", metadata)
	}
	if _, err := entry.BuildInjectInput(MessageInput{Body: "ignored"}); !errors.Is(err, ErrInjectUnsupported) {
		t.Fatalf("Codex inject error = %v", err)
	}
}

func TestCodexBuildsStructuredStartAndResumeCommands(t *testing.T) {
	entry := Codex{}
	launch := LaunchSpec{
		Bootstrap:     "Task input; do not infer completion from exit.",
		ContainerHome: "/home/agent", ContainerWork: "/workspace/project",
	}
	start, err := entry.BuildStartCommand(launch)
	if err != nil {
		t.Fatal(err)
	}
	wantStart := []string{
		"exec", "--json", "--color", "never", "--dangerously-bypass-approvals-and-sandbox",
		"--", launch.Bootstrap,
	}
	if start.Executable != "codex" || !reflect.DeepEqual(start.Args, wantStart) {
		t.Fatalf("start command = %#v", start)
	}
	resume, err := entry.BuildResumeCommand(ResumeSpec{LaunchSpec: launch, NativeSessionID: "0190a1b2-session"})
	if err != nil {
		t.Fatal(err)
	}
	wantResume := []string{
		"exec", "resume", "--json", "--dangerously-bypass-approvals-and-sandbox",
		"0190a1b2-session", "--", launch.Bootstrap,
	}
	if resume.Executable != "codex" || !reflect.DeepEqual(resume.Args, wantResume) {
		t.Fatalf("resume command = %#v", resume)
	}
	for _, command := range []CommandSpec{start, resume} {
		for _, arg := range command.Args {
			if arg == "sh" || arg == "-c" {
				t.Fatalf("provider command introduced a shell: %#v", command)
			}
		}
	}
}

func TestCodexProtocolConformanceRecordsSessionAndResumeFailure(t *testing.T) {
	entry := Codex{}
	session, err := entry.ParseEvent([]byte(`{"type":"thread.started","thread_id":"0190f8b7-acde"}`))
	if err != nil {
		t.Fatal(err)
	}
	if session.Kind != EventSessionStarted || session.NativeSessionID != "0190f8b7-acde" {
		t.Fatalf("session event = %#v", session)
	}
	resumeFailure, err := entry.ParseEvent([]byte(`{"type":"turn.failed","error":{"message":"session not found"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resumeFailure.Kind != EventResumeUnavailable || resumeFailure.Message != "session not found" {
		t.Fatalf("resume failure = %#v", resumeFailure)
	}
	providerFailure, err := entry.ParseEvent([]byte(`{"type":"error","message":"provider unavailable"}`))
	if err != nil {
		t.Fatal(err)
	}
	if providerFailure.Kind != EventProviderError {
		t.Fatalf("provider failure = %#v", providerFailure)
	}
}

func TestCodexResumeCompatibilityFencesTaskAgentAndWorkspace(t *testing.T) {
	entry := Codex{}
	base := SessionContext{AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"}
	if !entry.ResumeCompatible(base, base) {
		t.Fatal("identical session context was not resumable")
	}
	for name, changed := range map[string]SessionContext{
		"adapter":   {AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"agent":     {AdapterID: "codex", AgentID: "agent-b", TaskID: "task-a", WorkspaceID: "workspace-a"},
		"task":      {AdapterID: "codex", AgentID: "agent-a", TaskID: "task-b", WorkspaceID: "workspace-a"},
		"workspace": {AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "workspace-b"},
	} {
		t.Run(name, func(t *testing.T) {
			if entry.ResumeCompatible(base, changed) {
				t.Fatalf("incompatible context accepted: %#v", changed)
			}
		})
	}
}
