package coordlinkcli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"coordplane/internal/transport"
)

func TestDeferredOutcomeCommandsAreRejectedBeforeScopedClient(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "recording.sock")
	var socketRequests atomic.Int64
	server, err := transport.NewUnixServer(root, socket, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		socketRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})

	environmentLookups := 0
	getenv := func(name string) string {
		environmentLookups++
		switch name {
		case socketEnvironment:
			return socket
		case tokenEnvironment:
			return "run-token"
		default:
			return ""
		}
	}
	for _, subcommand := range []string{"wait", "submit", "fail"} {
		t.Run(subcommand, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), []string{"task", subcommand}, getenv, nil, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), `unknown task subcommand "`+subcommand+`"`) {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q", stdout.String())
			}
		})
	}
	if environmentLookups != 0 {
		t.Fatalf("deferred commands performed %d scoped environment lookups", environmentLookups)
	}
	if got := socketRequests.Load(); got != 0 {
		t.Fatalf("deferred commands performed %d socket requests", got)
	}
}

func TestHelpDoesNotAdvertiseDeferredOutcomeCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, nil, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, command := range []string{"task wait", "task submit", "task fail"} {
		if strings.Contains(stdout.String(), command) {
			t.Fatalf("help advertises deferred command %q:\n%s", command, stdout.String())
		}
	}
}
