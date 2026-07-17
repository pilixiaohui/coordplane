package operatorcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestFixedClientCommandsUseOperatorRoutes(t *testing.T) {
	status := testStatus()
	tests := []struct {
		name        string
		args        []string
		method      string
		path        string
		input       any
		outputHas   string
		outputLacks string
	}{
		{
			name:   "status",
			args:   []string{"status", "--project", "project-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/status?project_id=project-1",
			outputHas: `"daemon_ready":true`,
		},
		{
			name:   "project add",
			args:   []string{"project", "add", "--name", "One", "--repo", "/src/one", "--ref", "refs/heads/main", "--integration-agent", "agent-1", "--request-id", "req-project-add", "--output", "json"},
			method: http.MethodPost, path: "/v1/projects",
			input:     core.AddProjectInput{Name: "One", Source: "/src/one", SourceRef: "refs/heads/main", IntegrationAgentID: "agent-1", RequestID: "req-project-add"},
			outputHas: `"id":"project-1"`,
		},
		{
			name:   "project list",
			args:   []string{"project", "list", "--output", "json"},
			method: http.MethodGet, path: "/v1/projects",
			outputHas: `"id":"project-2"`,
		},
		{
			name:   "project show",
			args:   []string{"project", "show", "project-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/projects/project-1",
			outputHas: `"id":"project-1"`, outputLacks: `"id":"project-2"`,
		},
		{
			name:   "project repair",
			args:   []string{"project", "repair", "project-1", "--request-id", "req-project-repair", "--output", "json"},
			method: http.MethodPost, path: "/v1/projects/project-1/repair",
			input: actionRequest{RequestID: "req-project-repair"}, outputHas: `"id":"project-1"`,
		},
		{
			name:   "project archive",
			args:   []string{"project", "archive", "project-1", "--request-id", "req-project-archive", "--output", "json"},
			method: http.MethodPost, path: "/v1/projects/project-1/archive",
			input: actionRequest{RequestID: "req-project-archive"}, outputHas: `"id":"project-1"`,
		},
		{
			name:   "agent add",
			args:   []string{"agent", "add", "--id", "agent-1", "--display-name", "Agent One", "--adapter", "claude", "--image", "coordplane:test", "--instructions-file", "/instructions/one.md", "--request-id", "req-agent-add", "--output", "json"},
			method: http.MethodPost, path: "/v1/agents",
			input:     core.AddAgentInput{ID: "agent-1", DisplayName: "Agent One", AdapterID: "claude", Image: "coordplane:test", InstructionsFile: "/instructions/one.md", RequestID: "req-agent-add"},
			outputHas: `"id":"agent-1"`,
		},
		{
			name:   "agent list",
			args:   []string{"agent", "list", "--output", "json"},
			method: http.MethodGet, path: "/v1/agents",
			outputHas: `"id":"agent-2"`,
		},
		{
			name:   "agent show",
			args:   []string{"agent", "show", "agent-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/agents/agent-1",
			outputHas: `"id":"agent-1"`, outputLacks: `"id":"agent-2"`,
		},
		{
			name:   "agent pause",
			args:   []string{"agent", "pause", "agent-1", "--request-id", "req-agent-pause", "--output", "json"},
			method: http.MethodPost, path: "/v1/agents/agent-1/pause",
			input: actionRequest{RequestID: "req-agent-pause"}, outputHas: `"id":"agent-1"`,
		},
		{
			name:   "agent resume",
			args:   []string{"agent", "resume", "agent-1", "--request-id", "req-agent-resume", "--output", "json"},
			method: http.MethodPost, path: "/v1/agents/agent-1/resume",
			input: actionRequest{RequestID: "req-agent-resume"}, outputHas: `"id":"agent-1"`,
		},
		{
			name:   "agent archive",
			args:   []string{"agent", "archive", "agent-1", "--request-id", "req-agent-archive", "--output", "json"},
			method: http.MethodPost, path: "/v1/agents/agent-1/archive",
			input: actionRequest{RequestID: "req-agent-archive"}, outputHas: `"id":"agent-1"`,
		},
		{
			name:   "chat",
			args:   []string{"chat", "--project", "project-1", "--agent", "agent-1", "--body", "hello", "--related-task", "task-1", "--reply-to", "message-0", "--wake=false", "--ack-message", "message-0", "--request-id", "req-chat", "--output", "json"},
			method: http.MethodPost, path: "/v1/chat",
			input:     core.ChatInput{ProjectID: "project-1", AgentID: "agent-1", Body: "hello", RelatedTask: "task-1", ReplyTo: "message-0", Wake: false, AckMessageIDs: []string{"message-0"}, RequestID: "req-chat"},
			outputHas: `"message"`,
		},
		{
			name:   "message list",
			args:   []string{"message", "list", "--project", "project-1", "--task", "task-1", "--recipient-kind", "boss", "--recipient-id", "boss-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/messages?project_id=project-1&recipient_id=boss-1&recipient_kind=boss&task_id=task-1",
			outputHas: `"id":"message-1"`,
		},
		{
			name: "message send",
			args: []string{
				"message", "send", "--project", "project-1", "--agent", "agent-1", "--task", "task-1",
				"--related-task", "task-source", "--body", "direct", "--wake=false", "--reply-to", "message-0",
				"--ack-message", "message-0", "--request-id", "req-message-send", "--output", "json",
			},
			method: http.MethodPost, path: "/v1/messages",
			input: core.BossMessageInput{
				ProjectID: "project-1", AgentID: "agent-1", TaskID: "task-1", RelatedTaskID: "task-source",
				Body: "direct", Wake: false, ReplyTo: "message-0", AckMessageIDs: []string{"message-0"}, RequestID: "req-message-send",
			},
			outputHas: `"id":"message-1"`,
		},
		{
			name:   "message read",
			args:   []string{"message", "read", "message-1", "--request-id", "req-message-read", "--output", "json"},
			method: http.MethodPost, path: "/v1/messages/message-1/read",
			input: actionRequest{RequestID: "req-message-read"}, outputHas: `"id":"message-1"`,
		},
		{
			name:   "message ack",
			args:   []string{"message", "ack", "message-1", "--request-id", "req-message-ack", "--output", "json"},
			method: http.MethodPost, path: "/v1/messages/message-1/ack",
			input: actionRequest{RequestID: "req-message-ack"}, outputHas: `"id":"message-1"`,
		},
		{
			name:   "message retry",
			args:   []string{"message", "retry", "message-1", "--request-id", "req-message-retry", "--output", "json"},
			method: http.MethodPost, path: "/v1/messages/message-1/retry",
			input: actionRequest{RequestID: "req-message-retry"}, outputHas: `"id":"message-1"`,
		},
		{
			name:   "task create",
			args:   []string{"task", "create", "--project", "project-1", "--kind", "work", "--agent", "agent-1", "--title", "Implement", "--description", "Do the work", "--priority", "7", "--max-retries", "3", "--source-task", "task-source", "--retry-of", "task-closed", "--ack-message", "message-1", "--request-id", "req-task-create", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks",
			input:     core.CreateTaskInput{ProjectID: "project-1", Kind: core.TaskWork, AssigneeAgentID: "agent-1", Title: "Implement", Description: "Do the work", Priority: 7, MaxRetries: 3, SourceTaskID: "task-source", RetryOfTaskID: "task-closed", AckMessageIDs: []string{"message-1"}, RequestID: "req-task-create"},
			outputHas: `"id":"task-1"`,
		},
		{
			name:   "task list",
			args:   []string{"task", "list", "--project", "project-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/tasks?project_id=project-1",
			outputHas: `"id":"task-2"`,
		},
		{
			name:   "task show",
			args:   []string{"task", "show", "task-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/tasks/task-1",
			outputHas: `"id":"task-1"`, outputLacks: `"id":"task-2"`,
		},
		{
			name:   "task checkout",
			args:   []string{"task", "checkout", "task-1", "--dest", "/tmp/review-task-1", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks/task-1/checkout",
			input:     core.TaskCheckoutInput{TaskID: "task-1", Destination: "/tmp/review-task-1"},
			outputHas: `"head_sha":"captured-head"`,
		},
		{
			name:   "task close",
			args:   []string{"task", "close", "task-1", "--request-id", "req-task-close", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks/task-1/close",
			input: actionRequest{RequestID: "req-task-close"}, outputHas: `"id":"task-1"`,
		},
		{
			name: "task wake",
			args: []string{
				"task", "wake", "task-1", "--reason", "new input", "--ack-message", "message-1",
				"--ack-message", "message-2", "--request-id", "req-task-wake", "--output", "json",
			},
			method: http.MethodPost, path: "/v1/tasks/task-1/wake",
			input: core.TaskActionInput{
				Reason: "new input", AckMessageIDs: []string{"message-1", "message-2"}, RequestID: "req-task-wake",
			},
			outputHas: `"id":"task-1"`,
		},
		{
			name:   "task retry",
			args:   []string{"task", "retry", "task-1", "--reason", "retry runtime", "--request-id", "req-task-retry", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks/task-1/retry",
			input:     core.TaskActionInput{Reason: "retry runtime", RequestID: "req-task-retry"},
			outputHas: `"id":"task-1"`,
		},
		{
			name:   "task cancel",
			args:   []string{"task", "cancel", "task-1", "--reason", "superseded", "--request-id", "req-task-cancel", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks/task-1/cancel",
			input:     core.TaskActionInput{Reason: "superseded", RequestID: "req-task-cancel"},
			outputHas: `"id":"task-1"`,
		},
		{
			name:   "task rework",
			args:   []string{"task", "rework", "task-1", "--reason", "address review", "--request-id", "req-task-rework", "--output", "json"},
			method: http.MethodPost, path: "/v1/tasks/task-1/rework",
			input:     core.TaskActionInput{Reason: "address review", RequestID: "req-task-rework"},
			outputHas: `"id":"task-1"`,
		},
		{
			name: "task accept",
			args: []string{
				"task", "accept", "task-1", "--integration-agent", "agent-2", "--ack-message", "message-1",
				"--request-id", "req-task-accept", "--output", "json",
			},
			method: http.MethodPost, path: "/v1/tasks/task-1/accept",
			input: core.AcceptInput{
				IntegrationAgentID: "agent-2", AckMessageIDs: []string{"message-1"}, RequestID: "req-task-accept",
			},
			outputHas: `"id":"task-1"`,
		},
		{
			name:   "run list",
			args:   []string{"run", "list", "--project", "project-1", "--task", "task-1", "--agent", "agent-1", "--cursor", "next-run", "--limit", "25", "--output", "json"},
			method: http.MethodGet, path: "/v1/runs?agent_id=agent-1&cursor=next-run&limit=25&project_id=project-1&task_id=task-1",
			outputHas: `"id":"run-1"`,
		},
		{
			name:   "run show",
			args:   []string{"run", "show", "run-1", "--output", "json"},
			method: http.MethodGet, path: "/v1/runs/run-1",
			outputHas: `"id":"run-1"`,
		},
		{
			name:   "run stop",
			args:   []string{"run", "stop", "run-1", "--reason", "operator request", "--request-id", "req-run-stop", "--output", "json"},
			method: http.MethodPost, path: "/v1/runs/run-1/stop",
			input:     core.RunStopInput{Reason: "operator request", RequestID: "req-run-stop"},
			outputHas: `"id":"run-1"`,
		},
		{
			name:   "events tail",
			args:   []string{"events", "tail", "--project", "project-1", "--entity-type", "task", "--entity-id", "task-1", "--run-id", "run-1", "--cursor", "next-event", "--limit", "25", "--output", "json"},
			method: http.MethodGet, path: "/v1/events?cursor=next-event&entity_id=task-1&entity_type=task&limit=25&project_id=project-1&run_id=run-1",
			outputHas: `"id":1`,
		},
		{
			name: "gc preview", args: []string{"gc", "preview", "--output", "json"},
			method: http.MethodGet, path: "/v1/gc/preview", outputHas: `"workspaces"`,
		},
		{
			name: "gc run", args: []string{"gc", "run", "--confirm", "--request-id", "gc-run", "--output", "json"},
			method: http.MethodPost, path: "/v1/gc/run",
			input: core.GCRunInput{Confirm: true, RequestID: "gc-run"}, outputHas: `"completed":true`,
		},
		{
			name:   "gc discard workspace",
			args:   []string{"gc", "discard-workspace", "--task", "task-1", "--expected-fingerprint", "fp-1", "--request-id", "gc-workspace", "--output", "json"},
			method: http.MethodPost, path: "/v1/gc/discard-workspace",
			input:     core.GCDiscardWorkspaceInput{TaskID: "task-1", ExpectedFingerprint: "fp-1", RequestID: "gc-workspace"},
			outputHas: `"discarded":true`,
		},
		{
			name:   "gc discard task ref",
			args:   []string{"gc", "discard-task-ref", "--task", "task-1", "--run", "run-1", "--expected-sha", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "--request-id", "gc-ref", "--output", "json"},
			method: http.MethodPost, path: "/v1/gc/discard-task-ref",
			input:     core.GCDiscardTaskRefInput{TaskID: "task-1", RunID: "run-1", ExpectedSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RequestID: "gc-ref"},
			outputHas: `"discarded":true`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingClient{status: status}
			factoryCalls := 0
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), test.args, socketEnv("/tmp/from-env.sock"), &stdout, &stderr, func(socket string) (jsonClient, error) {
				factoryCalls++
				if socket != "/tmp/from-env.sock" {
					t.Fatalf("socket = %q", socket)
				}
				return client, nil
			}, func(context.Context, string) error {
				t.Fatal("client command invoked daemon runner")
				return nil
			})
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if factoryCalls != 1 || client.calls != 1 || client.closed != 1 {
				t.Fatalf("factory=%d JSON=%d close=%d", factoryCalls, client.calls, client.closed)
			}
			if client.method != test.method || client.path != test.path {
				t.Fatalf("request = %s %s, want %s %s", client.method, client.path, test.method, test.path)
			}
			if !reflect.DeepEqual(client.input, test.input) {
				t.Fatalf("input = %#v, want %#v", client.input, test.input)
			}
			if !strings.Contains(stdout.String(), test.outputHas) {
				t.Fatalf("stdout=%s, want substring %q", stdout.String(), test.outputHas)
			}
			if test.outputLacks != "" && strings.Contains(stdout.String(), test.outputLacks) {
				t.Fatalf("stdout=%s, unwanted substring %q", stdout.String(), test.outputLacks)
			}
		})
	}
}

func TestSocketEnvironmentAndOverride(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		environment string
		wantSocket  string
	}{
		{name: "environment default", args: []string{"status"}, environment: "  /tmp/environment.sock  ", wantSocket: "/tmp/environment.sock"},
		{name: "flag override", args: []string{"status", "--socket", "/tmp/override.sock"}, environment: "/tmp/environment.sock", wantSocket: "/tmp/override.sock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingClient{status: testStatus()}
			var gotSocket string
			code := run(context.Background(), test.args, socketEnv(test.environment), ioDiscard{}, ioDiscard{}, func(socket string) (jsonClient, error) {
				gotSocket = socket
				return client, nil
			}, nil)
			if code != 0 || gotSocket != test.wantSocket {
				t.Fatalf("code=%d socket=%q, want %q", code, gotSocket, test.wantSocket)
			}
		})
	}
}

func TestLegacyAndFutureCommandsFailBeforeClientSetup(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "task run", args: []string{"task", "run", "--socket", "/tmp/operator.sock"}},
		{name: "release health", args: []string{"release-health"}},
		{name: "backend URL", args: []string{"task", "create", "--backend-url", "http://old.invalid", "--socket", "/tmp/operator.sock"}},
		{name: "teamconfig client", args: []string{"status", "--teamconfig", "old.yaml", "--socket", "/tmp/operator.sock"}},
		{name: "teamconfig serve", args: []string{"serve", "--config", "new.yaml", "--teamconfig", "old.yaml"}},
		{name: "future project update", args: []string{"project", "update", "project-1", "--socket", "/tmp/operator.sock"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factoryCalls, runnerCalls := 0, 0
			var stderr bytes.Buffer
			code := run(context.Background(), test.args, socketEnv("/tmp/environment.sock"), ioDiscard{}, &stderr, func(string) (jsonClient, error) {
				factoryCalls++
				return &recordingClient{status: testStatus()}, nil
			}, func(context.Context, string) error {
				runnerCalls++
				return nil
			})
			if code == 0 {
				t.Fatalf("command unexpectedly succeeded; stderr=%s", stderr.String())
			}
			if factoryCalls != 0 || runnerCalls != 0 {
				t.Fatalf("factory=%d runner=%d", factoryCalls, runnerCalls)
			}
		})
	}
}

func TestBlankIDFailsBeforeClientSetup(t *testing.T) {
	factoryCalls := 0
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"project", "show", "   ", "--socket", "/tmp/operator.sock"}, nil, ioDiscard{}, &stderr, func(string) (jsonClient, error) {
		factoryCalls++
		return &recordingClient{}, nil
	}, nil)
	if code == 0 || factoryCalls != 0 || !strings.Contains(stderr.String(), "ID is required") {
		t.Fatalf("code=%d factory=%d stderr=%s", code, factoryCalls, stderr.String())
	}
}

func TestBlankAtomicAckIDFailsBeforeClientSetup(t *testing.T) {
	factoryCalls := 0
	var stderr bytes.Buffer
	code := run(context.Background(), []string{
		"task", "rework", "task-1", "--ack-message", "   ", "--socket", "/tmp/operator.sock",
	}, nil, ioDiscard{}, &stderr, func(string) (jsonClient, error) {
		factoryCalls++
		return &recordingClient{}, nil
	}, nil)
	if code == 0 || factoryCalls != 0 || !strings.Contains(stderr.String(), "message ID must not be blank") {
		t.Fatalf("code=%d factory=%d stderr=%s", code, factoryCalls, stderr.String())
	}
}

func TestServeUsesProvidedContextAndConfig(t *testing.T) {
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("test"), "value")
	var gotContext context.Context
	var gotPath string
	code := run(ctx, []string{"serve", "--config", "  daemon.yaml  "}, nil, ioDiscard{}, ioDiscard{}, func(string) (jsonClient, error) {
		t.Fatal("serve invoked client factory")
		return nil, nil
	}, func(callContext context.Context, path string) error {
		gotContext, gotPath = callContext, path
		return nil
	})
	if code != 0 || gotContext != ctx || gotPath != "daemon.yaml" {
		t.Fatalf("code=%d context=%v path=%q", code, gotContext == ctx, gotPath)
	}
}

func TestOutputModesAndInvalidOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		outputFlag []string
		want       string
	}{
		{name: "human", want: `"actual_canonical_sha": "sha-one"`},
		{name: "json", outputFlag: []string{"--output", "json"}, want: `"id":"project-1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"project", "show", "project-1"}, test.outputFlag...)
			client := &recordingClient{status: testStatus()}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, socketEnv("/tmp/operator.sock"), &stdout, &stderr, func(string) (jsonClient, error) {
				return client, nil
			}, nil)
			if code != 0 || !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
		})
	}

	factoryCalls := 0
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"status", "--output", "yaml"}, socketEnv("/tmp/operator.sock"), ioDiscard{}, &stderr, func(string) (jsonClient, error) {
		factoryCalls++
		return &recordingClient{}, nil
	}, nil)
	if code == 0 || factoryCalls != 0 || !strings.Contains(stderr.String(), "--output must be human or json") {
		t.Fatalf("code=%d factory=%d stderr=%s", code, factoryCalls, stderr.String())
	}
}

func TestStatusAndShowCommandsKeepActualTruthProjection(t *testing.T) {
	status := testStatus()
	status.ActualRefs[0].ActualSHA = "actual-one"
	status.Tasks[0].ActualCanonicalSHA = "actual-one"
	status.Tasks[0].PendingMessageCount = 3
	status.Tasks[0].CurrentRun = &core.RunSummary{ID: "run-1", State: core.RunActive}
	status.Tasks[0].LatestProgress = &core.Event{ID: 9, Kind: "task.progress"}
	status.Snapshot.Tasks[0].ParentTaskID = "task-parent"
	status.Snapshot.Tasks[0].ResultSummary = "complete result"
	status.Snapshot.Tasks[0].FailureReason = "prior failure"
	status.Snapshot.Tasks[0].BaseSHA = "base-sha"
	status.Snapshot.Tasks[0].HeadSHA = "head-sha"
	status.Snapshot.Tasks[0].TaskRef = "refs/coordplane/tasks/task-1"
	status.Snapshot.Tasks[0].Version = 7

	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "project show", args: []string{"project", "show", "project-1", "--output", "json"}, want: []string{`"actual_canonical_sha":"actual-one"`}},
		{name: "task show", args: []string{"task", "show", "task-1", "--output", "json"}, want: []string{`"current_run":{"id":"run-1"`, `"pending_message_count":3`, `"actual_canonical_sha":"actual-one"`, `"latest_progress":{"id":9`, `"parent_task_id":"task-parent"`, `"result_summary":"complete result"`, `"failure_reason":"prior failure"`, `"base_sha":"base-sha"`, `"head_sha":"head-sha"`, `"task_ref":"refs/coordplane/tasks/task-1"`, `"version":7`}},
		{name: "human status", args: []string{"status"}, want: []string{`"actual_sha": "actual-one"`, `"id": "run-1"`, `"pending_message_count": 3`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &recordingClient{status: status}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), test.args, socketEnv("/tmp/operator.sock"), &stdout, &stderr, func(string) (jsonClient, error) {
				return client, nil
			}, nil)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout=%s, want substring %q", stdout.String(), want)
				}
			}
		})
	}
}

func TestVersionDoesNotInitializeDaemonOrClient(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"version"}, nil, &stdout, &stderr, func(string) (jsonClient, error) {
		t.Fatal("version invoked client factory")
		return nil, nil
	}, func(context.Context, string) error {
		t.Fatal("version invoked daemon runner")
		return nil
	})
	if code != 0 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

type recordingClient struct {
	status core.Status
	method string
	path   string
	input  any
	calls  int
	closed int
}

func (c *recordingClient) JSON(_ context.Context, method, path string, input, output any) error {
	c.calls++
	c.method, c.path, c.input = method, path, input
	switch target := output.(type) {
	case *core.Status:
		*target = c.status
	case *core.Project:
		*target = c.status.Snapshot.Projects[0]
	case *core.ProjectDetail:
		*target = core.ProjectDetail{Project: c.status.Snapshot.Projects[0], ActualCanonicalSHA: c.status.ActualRefs[0].ActualSHA, ActualCanonicalError: c.status.ActualRefs[0].Error}
	case *core.ProjectPage:
		for _, project := range c.status.Snapshot.Projects {
			target.Items = append(target.Items, core.ProjectSummary{ID: project.ID, Name: project.Name, CanonicalSHA: project.CanonicalSHA, Status: project.Status})
		}
	case *core.Agent:
		*target = c.status.Snapshot.Agents[0]
	case *core.AgentPage:
		for _, agent := range c.status.Snapshot.Agents {
			target.Items = append(target.Items, core.AgentSummary{ID: agent.ID, DisplayName: agent.DisplayName, AdapterID: agent.AdapterID, Status: agent.Status})
		}
	case *core.ChatResult:
		*target = core.ChatResult{Task: c.status.Snapshot.Tasks[0], Message: c.status.Snapshot.Messages[0]}
	case *core.MessagePage:
		*target = core.MessagePage{Items: c.status.Snapshot.Messages}
	case *core.Message:
		*target = c.status.Snapshot.Messages[0]
	case *core.Task:
		*target = c.status.Snapshot.Tasks[0]
	case *core.TaskPage:
		for _, task := range c.status.Snapshot.Tasks {
			target.Items = append(target.Items, core.TaskSummary{ID: task.ID, ProjectID: task.ProjectID, Kind: task.Kind, AssigneeAgentID: task.AssigneeAgentID, Title: task.Title, Status: task.Status})
		}
	case *core.TaskDetail:
		view := c.status.Tasks[0]
		detail := core.TaskDetail{
			Task: c.status.Snapshot.Tasks[0], LatestProgress: view.LatestProgress,
			PendingMessageCount: view.PendingMessageCount, DeliveredMessageCount: view.DeliveredMessageCount,
			ActualCanonicalSHA: view.ActualCanonicalSHA, ActualCanonicalError: view.ActualCanonicalError,
			Stale: view.Stale, Derived: true,
		}
		if view.CurrentRun != nil {
			detail.CurrentRun = &core.Run{ID: view.CurrentRun.ID, State: view.CurrentRun.State}
		}
		*target = detail
	case *core.GitCheckoutFact:
		*target = core.GitCheckoutFact{Destination: "/tmp/review-task-1", HeadSHA: "captured-head"}
	case *core.RunPage:
		*target = core.RunPage{Items: []core.RunSummary{{ID: "run-1", ProjectID: "project-1", TaskID: "task-1", AgentID: "agent-1", State: core.RunExited}}}
	case *core.Run:
		*target = core.Run{ID: "run-1", ProjectID: "project-1", TaskID: "task-1", AgentID: "agent-1", State: core.RunExited}
	case *core.EventPage:
		*target = core.EventPage{Items: c.status.Snapshot.Events, NextCursor: "next-event"}
	case *core.GCPreview:
		*target = core.GCPreview{Workspaces: []core.GCWorkspaceTarget{}, TaskRefs: []core.GCTaskRefTarget{}}
	case *core.GCRunResult:
		*target = core.GCRunResult{Completed: true}
	case *core.GCDiscardResult:
		*target = core.GCDiscardResult{TaskID: "task-1", RunID: "run-1", Discarded: true}
	}
	return nil
}

func (c *recordingClient) CloseIdleConnections() {
	c.closed++
}

func testStatus() core.Status {
	status := core.Status{
		DaemonReady: true,
		Snapshot: core.Snapshot{
			Projects: []core.Project{
				{ID: "project-1", Name: "One", Status: core.ProjectActive, CanonicalSHA: "sha-one"},
				{ID: "project-2", Name: "Two", Status: core.ProjectError, CanonicalSHA: "sha-two"},
			},
			Agents: []core.Agent{
				{ID: "agent-1", DisplayName: "Agent One", Status: core.AgentActive, AdapterID: "claude"},
				{ID: "agent-2", DisplayName: "Agent Two", Status: core.AgentPaused, AdapterID: "claude"},
			},
			Tasks: []core.Task{
				{ID: "task-1", Kind: core.TaskWork, Status: core.TaskQueued, AssigneeAgentID: "agent-1", Title: "Implement"},
				{ID: "task-2", Kind: core.TaskConversation, Status: core.TaskWaiting, AssigneeAgentID: "agent-2", Title: "Chat"},
			},
			Messages: []core.Message{{ID: "message-1", State: core.MessagePending, SenderKind: "agent", SenderID: "agent-1", Body: "done"}},
			Events:   []core.Event{{ID: 1, Kind: "task.created", EntityType: "task", EntityID: "task-1"}},
		},
	}
	status.ActualRefs = []core.GitState{
		{ProjectID: "project-1", CanonicalRef: "refs/heads/main", ActualSHA: "sha-one"},
		{ProjectID: "project-2", CanonicalRef: "refs/heads/main", Error: "missing"},
	}
	status.Tasks = []core.TaskView{
		{Task: core.TaskSummary{ID: "task-1", ProjectID: "project-1", Kind: core.TaskWork, Status: core.TaskQueued, AssigneeAgentID: "agent-1", Title: "Implement"}, ActualCanonicalSHA: "sha-one", Derived: true},
		{Task: core.TaskSummary{ID: "task-2", ProjectID: "project-2", Kind: core.TaskConversation, Status: core.TaskWaiting, AssigneeAgentID: "agent-2", Title: "Chat"}, ActualCanonicalError: "missing", Derived: true},
	}
	return status
}

func socketEnv(socket string) environment {
	return func(name string) string {
		if name == socketEnvironment {
			return socket
		}
		return ""
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) {
	return len(data), nil
}
