package coordlinkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

func TestScopedClientReadsTokenFile(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "api.sock")
	operations := &runOperations{wantToken: "file-token"}
	startCLIServer(t, root, socket, transport.NewRunHandler(operations))
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	getenv := scopedEnvironment(socket, tokenFile)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{
		"progress", "--summary", "from file", "--request-id", "token-file-progress", "--output", "json",
	}, getenv, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if operations.progressCalls != 1 {
		t.Fatalf("progress calls = %d, want 1", operations.progressCalls)
	}
}

func TestRetryingClientRetriesOnlyTransientErrorsWithStableRequestID(t *testing.T) {
	transient := []error{
		fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
		core.NewError(core.CodeRunStarting, "starting", true),
		core.NewError(core.CodeRuntimeUnavailable, "recovering", true),
	}
	next := &retryScriptClient{errors: transient}
	client := &retryingClient{next: next, maxAttempts: 4}
	input := core.ProgressInput{Summary: "working", RequestID: "stable-request"}
	var output core.Event
	if err := client.JSON(context.Background(), http.MethodPost, "/v1/progress", input, &output); err != nil {
		t.Fatal(err)
	}
	if next.calls != 4 {
		t.Fatalf("calls = %d, want 4", next.calls)
	}
	for index, requestID := range next.requestIDs {
		if requestID != input.RequestID {
			t.Fatalf("attempt %d request ID = %q, want %q", index, requestID, input.RequestID)
		}
	}

	denied := &retryScriptClient{errors: []error{core.NewError(core.CodeScopeDenied, "denied", false)}}
	client = &retryingClient{next: denied, maxAttempts: 3}
	if err := client.JSON(context.Background(), http.MethodPost, "/v1/progress", input, &output); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("scope error = %v, want %s", err, core.CodeScopeDenied)
	}
	if denied.calls != 1 {
		t.Fatalf("scope denial retried %d times", denied.calls)
	}

	exhausted := &retryScriptClient{errors: []error{
		fmt.Errorf("dial 1: %w", syscall.ENOENT),
		fmt.Errorf("dial 2: %w", syscall.ENOENT),
		fmt.Errorf("dial 3: %w", syscall.ENOENT),
	}}
	client = &retryingClient{next: exhausted, maxAttempts: 3}
	if err := client.JSON(context.Background(), http.MethodPost, "/v1/progress", input, &output); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("exhausted error = %v, want ENOENT", err)
	}
	if exhausted.calls != 3 {
		t.Fatalf("exhausted calls = %d, want 3", exhausted.calls)
	}
}

type retryScriptClient struct {
	errors     []error
	calls      int
	requestIDs []string
}

func (c *retryScriptClient) JSON(_ context.Context, _ string, _ string, input, output any) error {
	c.calls++
	if progress, ok := input.(core.ProgressInput); ok {
		c.requestIDs = append(c.requestIDs, progress.RequestID)
	}
	if c.calls <= len(c.errors) && c.errors[c.calls-1] != nil {
		return c.errors[c.calls-1]
	}
	if event, ok := output.(*core.Event); ok {
		*event = core.Event{Kind: "task.progress"}
	}
	return nil
}

func (c *retryScriptClient) CloseIdleConnections() {}

func TestLegacyCommandsAreRejectedBeforeSocketLookup(t *testing.T) {
	for _, args := range [][]string{{"capability", "list"}, {"skill", "list"}, {"call", "anything"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, func(string) string { return "" }, nil, &stdout, &stderr)
		if code == 0 || !bytes.Contains(stderr.Bytes(), []byte("unknown coordlink command")) {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
		if bytes.Contains(stderr.Bytes(), []byte(socketEnvironment+" is required")) {
			t.Fatalf("legacy command attempted socket setup: %s", stderr.String())
		}
	}
}

func TestProgressUsesPerRunSocketAndBearerToken(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "api.sock")
	operations := &runOperations{wantToken: "run-token"}
	startCLIServer(t, root, socket, transport.NewRunHandler(operations))
	tokenFile := writeRunTokenFile(t, "run-token")
	getenv := scopedEnvironment(socket, tokenFile)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"progress", "--summary", "must-not-commit", "--request-id", "req-invalid", "--output", "xml"}, getenv, nil, &stdout, &stderr)
	if code == 0 || !bytes.Contains(stderr.Bytes(), []byte("--output must be human or json")) {
		t.Fatalf("invalid output code=%d stderr=%s", code, stderr.String())
	}
	if operations.progressCalls != 0 {
		t.Fatalf("invalid output performed %d progress mutations", operations.progressCalls)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"progress", "--summary", "working", "--request-id", "req-1", "--output", "json"}, getenv, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if operations.summary != "working" || operations.requestID != "req-1" {
		t.Fatalf("progress input = %q/%q", operations.summary, operations.requestID)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"kind":"task.progress"`)) {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestP2CommandsUseOnlyTheFixedPerRunSurface(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		method     string
		path       string
		bodyValues map[string]any
	}{
		{name: "task current", args: []string{"task", "current", "--output", "json"}, method: http.MethodGet, path: "/v1/task/current"},
		{name: "task show", args: []string{"task", "show", "task/child", "--output", "json"}, method: http.MethodGet, path: "/v1/task/task%2Fchild"},
		{
			name:   "task create",
			args:   []string{"task", "create", "--agent", "agent-b", "--title", "review", "--description", "inspect", "--priority", "7", "--max-retries", "2", "--budget", "1800", "--source-task", "task-source", "--request-id", "req-create", "--ack-message", "msg-1", "--ack-message", "msg-2", "--output", "json"},
			method: http.MethodPost,
			path:   "/v1/task/create",
			bodyValues: map[string]any{
				"assignee_agent_id": "agent-b", "title": "review", "description": "inspect",
				"priority": float64(7), "max_retries": float64(2), "budget_seconds": float64(1800), "source_task_id": "task-source", "request_id": "req-create", "ack_message_ids": []any{"msg-1", "msg-2"},
			},
		},
		{
			name:       "task wait",
			args:       []string{"task", "wait", "--reason", "children running", "--request-id", "req-wait", "--ack-message", "msg-3", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/task/outcome",
			bodyValues: map[string]any{"outcome": "wait", "reason": "children running", "request_id": "req-wait", "ack_message_ids": []any{"msg-3"}},
		},
		{
			name:       "task submit",
			args:       []string{"task", "submit", "--summary", "ready", "--expected-head", "abc123", "--request-id", "req-submit", "--ack-message", "msg-4", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/task/outcome",
			bodyValues: map[string]any{"outcome": "submit", "summary": "ready", "expected_head": "abc123", "request_id": "req-submit", "ack_message_ids": []any{"msg-4"}},
		},
		{
			name:       "task fail",
			args:       []string{"task", "fail", "--reason", "cannot proceed", "--request-id", "req-fail", "--ack-message", "msg-5", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/task/outcome",
			bodyValues: map[string]any{"outcome": "fail", "reason": "cannot proceed", "request_id": "req-fail", "ack_message_ids": []any{"msg-5"}},
		},
		{
			name:       "task accept",
			args:       []string{"task", "accept", "task/child", "--integration-agent", "agent-i", "--request-id", "req-accept", "--ack-message", "msg-accept", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/task/task%2Fchild/accept",
			bodyValues: map[string]any{"integration_agent_id": "agent-i", "request_id": "req-accept", "ack_message_ids": []any{"msg-accept"}},
		},
		{
			name:       "task rework",
			args:       []string{"task", "rework", "task/child", "--reason", "needs changes", "--request-id", "req-rework", "--ack-message", "msg-rework", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/task/task%2Fchild/rework",
			bodyValues: map[string]any{"reason": "needs changes", "request_id": "req-rework", "ack_message_ids": []any{"msg-rework"}},
		},
		{name: "inbox list", args: []string{"inbox", "list", "--output", "json"}, method: http.MethodGet, path: "/v1/inbox"},
		{name: "inbox read", args: []string{"inbox", "read", "msg/read", "--output", "json"}, method: http.MethodGet, path: "/v1/inbox/msg%2Fread"},
		{
			name:       "inbox ack",
			args:       []string{"inbox", "ack", "--ack-message", "msg-6", "--ack-message", "msg-7", "--request-id", "req-ack", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/inbox/ack",
			bodyValues: map[string]any{"message_ids": []any{"msg-6", "msg-7"}, "request_id": "req-ack"},
		},
		{
			name:       "message to boss",
			args:       []string{"message", "send", "--to-boss", "--task", "task-parent", "--body", "result", "--reply-to", "msg-8", "--request-id", "req-boss", "--ack-message", "msg-8", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/message",
			bodyValues: map[string]any{"recipient_kind": "boss", "task_id": "task-parent", "body": "result", "reply_to_message_id": "msg-8", "request_id": "req-boss", "ack_message_ids": []any{"msg-8"}},
		},
		{
			name:       "message to agent",
			args:       []string{"message", "send", "--to-agent", "agent-c", "--task", "task-child", "--wake", "--body", "question", "--request-id", "req-agent", "--output", "json"},
			method:     http.MethodPost,
			path:       "/v1/message",
			bodyValues: map[string]any{"recipient_kind": "agent", "recipient_id": "agent-c", "task_id": "task-child", "wake": true, "body": "question", "request_id": "req-agent"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := runRecordedCommand(t, test.args)
			if request.method != test.method || request.path != test.path {
				t.Fatalf("request = %s %s, want %s %s", request.method, request.path, test.method, test.path)
			}
			if request.authorization != "Bearer run-token" {
				t.Fatalf("Authorization = %q, want scoped bearer", request.authorization)
			}
			for key, want := range test.bodyValues {
				if got := request.body[key]; !jsonValuesEqual(got, want) {
					t.Errorf("body[%q] = %#v, want %#v; body=%#v", key, got, want, request.body)
				}
			}
		})
	}
}

func TestMessageSendRejectsAmbiguousRecipientBeforeMutation(t *testing.T) {
	tokenFile := writeRunTokenFile(t, "run-token")
	for _, args := range [][]string{
		{"message", "send", "--body", "missing recipient"},
		{"message", "send", "--to-boss", "--to-agent", "agent-b", "--body", "ambiguous"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, scopedEnvironment(filepath.Join(t.TempDir(), "missing.sock"), tokenFile), nil, &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "exactly one of --to-boss or --to-agent") {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

type recordedRequest struct {
	method        string
	path          string
	authorization string
	body          map[string]any
}

func runRecordedCommand(t *testing.T, args []string) recordedRequest {
	t.Helper()
	root := t.TempDir()
	socket := filepath.Join(root, "api.sock")
	requests := make(chan recordedRequest, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		record := recordedRequest{method: request.Method, path: request.URL.EscapedPath(), authorization: request.Header.Get("Authorization")}
		if request.Body != nil {
			defer request.Body.Close()
			if raw, err := io.ReadAll(request.Body); err != nil {
				t.Errorf("read request body: %v", err)
			} else if len(bytes.TrimSpace(raw)) > 0 {
				if err := json.Unmarshal(raw, &record.body); err != nil {
					t.Errorf("decode request body %q: %v", raw, err)
				}
			}
		}
		requests <- record
		writer.Header().Set("Content-Type", "application/json")
		data := `{}`
		if request.URL.Path == "/v1/inbox" || request.URL.Path == "/v1/inbox/ack" {
			data = `[]`
		}
		_, _ = io.WriteString(writer, `{"ok":true,"data":`+data+`,"error":null}`)
	})
	startCLIServer(t, root, socket, handler)
	tokenFile := writeRunTokenFile(t, "run-token")
	getenv := scopedEnvironment(socket, tokenFile)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), args, getenv, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(%v) code=%d stderr=%s", args, code, stderr.String())
	}
	return <-requests
}

func scopedEnvironment(socket, tokenFile string) func(string) string {
	return func(name string) string {
		switch name {
		case socketEnvironment:
			return socket
		case tokenFileEnvironment:
			return tokenFile
		default:
			return ""
		}
	}
}

func startCLIServer(t *testing.T, root, socket string, handler http.Handler) {
	t.Helper()
	server, err := transport.NewUnixServer(root, socket, handler)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
}

func writeRunTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

type runOperations struct {
	wantToken     string
	summary       string
	requestID     string
	progressCalls int
}

func (o *runOperations) RequireReady() error { return nil }

func (o *runOperations) CurrentTask(context.Context, string) (core.CurrentTaskResult, error) {
	return core.CurrentTaskResult{}, nil
}

func (o *runOperations) TaskForRun(context.Context, string, string) (core.Task, error) {
	return core.Task{}, nil
}

func (o *runOperations) CreateChildTask(context.Context, core.CreateChildTaskInput) (core.Task, error) {
	return core.Task{}, nil
}

func (o *runOperations) RequestOutcome(context.Context, core.OutcomeInput) (core.OutcomeResult, error) {
	return core.OutcomeResult{}, nil
}

func (o *runOperations) RequestAccept(context.Context, core.AcceptInput) (core.Task, error) {
	return core.Task{}, nil
}

func (o *runOperations) ReworkTask(context.Context, core.TaskActionInput) (core.Task, error) {
	return core.Task{}, nil
}

func (o *runOperations) Inbox(context.Context, string) ([]core.Message, error) {
	return nil, nil
}

func (o *runOperations) InboxMessage(context.Context, string, string) (core.Message, error) {
	return core.Message{}, nil
}

func (o *runOperations) AcknowledgeAgentMessages(context.Context, core.AcknowledgeMessagesInput) ([]core.Message, error) {
	return nil, nil
}

func (o *runOperations) SendAgentMessage(context.Context, core.SendMessageInput) (core.Message, error) {
	return core.Message{}, nil
}

func (o *runOperations) Progress(_ context.Context, input core.ProgressInput) (core.Event, error) {
	o.progressCalls++
	if input.Token != o.wantToken {
		return core.Event{}, core.NewError(core.CodeScopeDenied, "bad token", false)
	}
	o.summary, o.requestID = input.Summary, input.RequestID
	return core.Event{ID: 1, Kind: "task.progress", EntityID: "task-1"}, nil
}

var _ transport.RunOperations = (*runOperations)(nil)
