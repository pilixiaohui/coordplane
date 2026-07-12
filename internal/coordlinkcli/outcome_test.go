package coordlinkcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

func TestTaskOutcomeCommandsUseTheFixedRunHandlerContract(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantOutcome  string
		wantReason   string
		wantSummary  string
		wantExpected string
	}{
		{
			name: "wait", args: []string{"task", "wait", "--reason", "awaiting review", "--request-id", "wait-1", "--output", "json"},
			wantOutcome: "wait", wantReason: "awaiting review",
		},
		{
			name: "fail", args: []string{"task", "fail", "--reason", "tests failed", "--request-id", "fail-1", "--output", "json"},
			wantOutcome: "fail", wantReason: "tests failed",
		},
		{
			name: "submit", args: []string{"task", "submit", "--summary", "ready", "--expected-head", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--request-id", "submit-1", "--output", "json"},
			wantOutcome: "submit", wantSummary: "ready", wantExpected: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			socket := filepath.Join(root, "api.sock")
			operations := &outcomeOperations{wantToken: "run-token"}
			server, err := transport.NewUnixServer(root, socket, transport.NewRunHandler(operations))
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve() }()
			t.Cleanup(func() {
				_ = server.Close()
				<-serveDone
			})
			getenv := func(name string) string {
				switch name {
				case socketEnvironment:
					return socket
				case tokenEnvironment:
					return "run-token"
				default:
					return ""
				}
			}
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), test.args, getenv, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if operations.calls != 1 {
				t.Fatalf("outcome calls = %d", operations.calls)
			}
			got := operations.input
			if got.Token != "run-token" || got.Outcome != test.wantOutcome || got.Reason != test.wantReason || got.Summary != test.wantSummary || got.ExpectedHead != test.wantExpected {
				t.Fatalf("outcome input = %#v", got)
			}
			if !bytes.Contains(stdout.Bytes(), []byte(`"status":"finishing"`)) {
				t.Fatalf("stdout=%s", stdout.String())
			}
		})
	}
}

type outcomeOperations struct {
	wantToken string
	input     core.OutcomeInput
	calls     int
}

func (o *outcomeOperations) CurrentTask(context.Context, string) (core.Task, error) {
	return core.Task{}, nil
}

func (o *outcomeOperations) Progress(context.Context, core.ProgressInput) (core.Event, error) {
	return core.Event{}, nil
}

func (o *outcomeOperations) AgentMessageToBoss(context.Context, core.AgentMessageInput) (core.Message, error) {
	return core.Message{}, nil
}

func (o *outcomeOperations) RequestOutcome(_ context.Context, input core.OutcomeInput) (core.Task, error) {
	o.calls++
	o.input = input
	if input.Token != o.wantToken {
		return core.Task{}, core.NewError(core.CodeScopeDenied, "bad token", false)
	}
	return core.Task{ID: "task-1", Status: core.TaskFinishing}, nil
}

var _ transport.RunOperations = (*outcomeOperations)(nil)
