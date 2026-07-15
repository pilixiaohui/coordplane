package contract_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
	"coordplane/internal/transport"
)

func TestRT01PerRunSocketsRequireMatchingTokenScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
	requireNoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	service, err := core.NewService(database, &contractGit{
		sha: strings.Repeat("a", 40), root: filepath.Join(root, "repos"),
	}, core.ServiceOptions{MaxParallelRuns: 2, AdapterIDs: []string{"one-shot"}})
	requireNoError(t, err)
	agentA, err := service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Socket A", AdapterID: "one-shot", Image: "agent:latest",
		InstructionsFile: "/instructions/a", RequestID: "socket-agent-a",
	})
	requireNoError(t, err)
	agentB, err := service.AddAgent(ctx, core.AddAgentInput{
		DisplayName: "Socket B", AdapterID: "one-shot", Image: "agent:latest",
		InstructionsFile: "/instructions/b", RequestID: "socket-agent-b",
	})
	requireNoError(t, err)
	project, err := service.AddProject(ctx, core.AddProjectInput{
		Name: "Socket Project", Source: "/source", SourceRef: "refs/heads/main", RequestID: "socket-project",
	})
	requireNoError(t, err)
	for _, input := range []core.CreateTaskInput{
		{ProjectID: project.ID, AssigneeAgentID: agentA.ID, Kind: core.TaskWork, Title: "socket A", Priority: 20, RequestID: "socket-task-a"},
		{ProjectID: project.ID, AssigneeAgentID: agentB.ID, Kind: core.TaskWork, Title: "socket B", Priority: 10, RequestID: "socket-task-b"},
	} {
		if _, err := service.CreateTask(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	claimA, ok, err := service.ClaimNext(ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim A: ok=%t err=%v", ok, err)
	}
	claimB, ok, err := service.ClaimNext(ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim B: ok=%t err=%v", ok, err)
	}

	socketA := startScopedRunSocket(t, root, "a", service, scopeForRun(claimA.Run))
	socketB := startScopedRunSocket(t, root, "b", service, scopeForRun(claimB.Run))
	for _, socket := range []string{socketA, socketB} {
		info, err := os.Stat(socket)
		requireNoError(t, err)
		if info.Mode().Perm() != 0o660 {
			t.Fatalf("Run socket %s mode = %o, want 660", socket, info.Mode().Perm())
		}
	}

	clientA, err := transport.NewUnixClient(socketA, transport.WithBearerToken(claimA.Token))
	requireNoError(t, err)
	defer clientA.CloseIdleConnections()
	crossClient, err := transport.NewUnixClient(socketB, transport.WithBearerToken(claimA.Token))
	requireNoError(t, err)
	defer crossClient.CloseIdleConnections()

	before := durableSignature(t, database, project.ID)
	if err := clientA.JSON(ctx, http.MethodGet, "/v1/task/current", nil, &core.CurrentTaskResult{}); !core.IsCode(err, core.CodeRunStarting) {
		t.Fatalf("matching starting socket error = %v, want %s", err, core.CodeRunStarting)
	}
	if err := crossClient.JSON(ctx, http.MethodPost, "/v1/progress", core.ProgressInput{
		Summary: "must not commit", RequestID: "cross-socket-progress",
	}, &core.Event{}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("cross-socket error = %v, want %s", err, core.CodeScopeDenied)
	}
	if after := durableSignature(t, database, project.ID); after != before {
		t.Fatal("cross-socket request changed durable state")
	}
}

func startScopedRunSocket(t *testing.T, root, name string, service *core.Service, scope core.RunScope) string {
	t.Helper()
	controlDir := filepath.Join(root, "run-control", name)
	requireNoError(t, os.MkdirAll(controlDir, 0o750))
	socket := filepath.Join(controlDir, "api.sock")
	server, err := transport.NewUnixServerWithMode(
		root, socket, 0o660, transport.NewScopedRunHandler(service, scope),
	)
	requireNoError(t, err)
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return socket
}

func scopeForRun(run core.Run) core.RunScope {
	return core.RunScope{
		ProjectID:  run.ProjectID,
		AgentID:    run.AgentID,
		TaskID:     run.TaskID,
		RunID:      run.ID,
		Generation: run.Generation,
	}
}
