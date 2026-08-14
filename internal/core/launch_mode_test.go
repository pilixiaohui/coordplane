package core

import (
	"strings"
	"testing"
)

func TestSelectLaunchModeOnlyResumesIdenticalFingerprintAndCompatibleContext(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	otherFingerprint := strings.Repeat("b", 64)
	previousContext := LaunchSessionContext{
		AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "/workspace/task-a",
	}
	currentContext := previousContext
	compatible := func(previous, next LaunchSessionContext) bool {
		return previous.AdapterID == next.AdapterID && previous.AgentID == next.AgentID &&
			previous.TaskID == next.TaskID && previous.WorkspaceID == next.WorkspaceID
	}
	policy := ResumePolicy{SupportsResume: true, Compatible: compatible}
	previous := Run{
		ID: "run-prev", AdapterID: "claude", AgentID: "agent-a", TaskID: "task-a",
		WorkspacePath: "/workspace/task-a", NativeSessionID: "session-prev",
		ConfigFingerprint: fingerprint,
	}

	resume := SelectLaunchMode(policy, previous, previousContext, currentContext, fingerprint)
	if resume.Mode != "resume" || resume.ResumedFromRunID != previous.ID ||
		resume.ResumeNativeSessionID != previous.NativeSessionID {
		t.Fatalf("resume selection = %#v", resume)
	}

	tests := []struct {
		name               string
		policy             ResumePolicy
		previous           Run
		previousContext    LaunchSessionContext
		currentContext     LaunchSessionContext
		currentFingerprint string
		wantResumedFrom    string
		incompatible       bool
	}{
		{"unsupported resume", ResumePolicy{Compatible: compatible}, previous, previousContext, currentContext, fingerprint, "", false},
		{"nil compatibility", ResumePolicy{SupportsResume: true}, previous, previousContext, currentContext, fingerprint, "", false},
		{"no previous run", policy, Run{}, previousContext, currentContext, fingerprint, "", false},
		{"adapter changed", policy, previous, mutateContext(previousContext, "adapter"), currentContext, fingerprint, "", false},
		{"agent changed", policy, previous, mutateContext(previousContext, "agent"), currentContext, fingerprint, "", false},
		{"task changed", policy, previous, mutateContext(previousContext, "task"), currentContext, fingerprint, "", false},
		{"workspace changed", policy, previous, mutateContext(previousContext, "workspace"), currentContext, fingerprint, "", false},
		{"empty current fingerprint", policy, previous, previousContext, currentContext, "", "", false},
		{"empty previous fingerprint", policy, func() Run {
			value := previous
			value.ConfigFingerprint = ""
			return value
		}(), previousContext, currentContext, fingerprint, "", false},
		{"fingerprint changed", policy, previous, previousContext, currentContext, otherFingerprint, "", false},
		{"missing native session", policy, func() Run {
			value := previous
			value.NativeSessionID = ""
			return value
		}(), previousContext, currentContext, fingerprint, "", false},
		{"adapter incompatible", policy, previous, previousContext, currentContext, fingerprint, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := test.policy
			if test.incompatible {
				policy.Compatible = func(previous, next LaunchSessionContext) bool { return false }
			}
			selection := SelectLaunchMode(policy, test.previous, test.previousContext, test.currentContext, test.currentFingerprint)
			if selection.Mode != "start" || selection.ResumedFromRunID != test.wantResumedFrom ||
				selection.ResumeNativeSessionID != "" {
				t.Fatalf("selection = %#v, want fresh start", selection)
			}
		})
	}
}

func TestSelectLaunchModePreservesResumeFallbackAfterFailedResume(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	context := LaunchSessionContext{AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a", WorkspaceID: "/workspace/task-a"}
	previous := Run{
		ID: "failed-resume", AdapterID: "codex", AgentID: "agent-a", TaskID: "task-a",
		WorkspacePath: "/workspace/task-a", NativeSessionID: "missing-session",
		ConfigFingerprint: fingerprint, RuntimeErrorCode: string(CodeResumeUnavailable),
	}
	selection := SelectLaunchMode(ResumePolicy{
		SupportsResume: true,
		Compatible:     func(previous, next LaunchSessionContext) bool { return true },
	}, previous, context, context, fingerprint)
	if selection.Mode != "start" || selection.ResumedFromRunID != previous.ID || selection.ResumeNativeSessionID != "" {
		t.Fatalf("fallback selection = %#v", selection)
	}

	changed := previous
	changed.ConfigFingerprint = strings.Repeat("b", 64)
	selection = SelectLaunchMode(ResumePolicy{
		SupportsResume: true,
		Compatible:     func(previous, next LaunchSessionContext) bool { return true },
	}, changed, context, context, fingerprint)
	if selection.Mode != "start" || selection.ResumedFromRunID != "" {
		t.Fatalf("config change must not be linked to old failed resume: %#v", selection)
	}
}

func mutateContext(context LaunchSessionContext, field string) LaunchSessionContext {
	switch field {
	case "adapter":
		context.AdapterID = "codex"
	case "agent":
		context.AgentID = "agent-b"
	case "task":
		context.TaskID = "task-b"
	case "workspace":
		context.WorkspaceID = "/workspace/task-b"
	}
	return context
}
