package coordlinkcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

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

type runOperations struct {
	wantToken     string
	summary       string
	requestID     string
	progressCalls int
}

func (o *runOperations) CurrentTask(context.Context, string) (core.Task, error) {
	return core.Task{}, nil
}

func (o *runOperations) Progress(_ context.Context, input core.ProgressInput) (core.Event, error) {
	o.progressCalls++
	if input.Token != o.wantToken {
		return core.Event{}, core.NewError(core.CodeScopeDenied, "bad token", false)
	}
	o.summary, o.requestID = input.Summary, input.RequestID
	return core.Event{ID: 1, Kind: "task.progress", EntityID: "task-1"}, nil
}

func (o *runOperations) AgentMessageToBoss(context.Context, core.AgentMessageInput) (core.Message, error) {
	return core.Message{}, nil
}

var _ transport.RunOperations = (*runOperations)(nil)
