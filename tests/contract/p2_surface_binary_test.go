package contract_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
	"coordplane/internal/transport"
)

func TestP2CoordlinkBinaryFixedSurfacePersistsSuccessfulCoordination(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	gitFacts := &contractGit{sha: strings.Repeat("a", 40), root: filepath.Join(root, "repos")}
	service, err := core.NewService(database, gitFacts, core.ServiceOptions{MaxParallelRuns: 4, AdapterIDs: []string{"one-shot"}})
	if err != nil {
		t.Fatal(err)
	}
	addAgent := func(name string) core.Agent {
		t.Helper()
		agent, err := service.AddAgent(ctx, core.AddAgentInput{
			DisplayName: name, AdapterID: "one-shot", Image: "agent:latest",
			InstructionsFile: "/instructions", RequestID: "p2-surface-agent-" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	parentAgent := addAgent("parent")
	workerAgent := addAgent("worker")
	integrationAgent := addAgent("integrator")
	project, err := service.AddProject(ctx, core.AddProjectInput{
		Name: "P2 surface", Source: "/source", SourceRef: "refs/heads/main",
		IntegrationAgentID: integrationAgent.ID, RequestID: "p2-surface-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := service.CreateTask(ctx, core.CreateTaskInput{
		ProjectID: project.ID, Kind: core.TaskWork, AssigneeAgentID: parentAgent.ID,
		Title: "coordinate children", Priority: 100, RequestID: "p2-surface-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := service.SendBossMessage(ctx, core.BossMessageInput{
		ProjectID: project.ID, AgentID: parentAgent.ID, TaskID: parent.ID,
		Body: "review these requirements", RequestID: "p2-surface-incoming",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := service.ClaimNext(ctx, project.ID)
	if err != nil || !ok || claim.Task.ID != parent.ID {
		t.Fatalf("parent claim = %#v ok=%t err=%v", claim, ok, err)
	}
	activateContractRuntimeRun(t, ctx, service, claim, "p2-surface")

	socket := filepath.Join(root, "run.sock")
	server, err := transport.NewUnixServer(root, socket, transport.NewRunHandler(service))
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() {
		_ = server.Close()
		<-serveDone
	}()

	currentRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"task", "current", "--output", "json")
	var current core.CurrentTaskResult
	decodeJSON(t, currentRaw, &current)
	if current.Task.ID != parent.ID || current.Run.ID != claim.Run.ID ||
		current.Run.State != core.RunActive || current.UnreadMessageCount != 1 {
		t.Fatalf("task current = %#v", current)
	}

	inboxRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"inbox", "list", "--output", "json")
	var inbox []core.Message
	decodeJSON(t, inboxRaw, &inbox)
	if len(inbox) != 1 || inbox[0].ID != incoming.ID || inbox[0].State != core.MessagePending {
		t.Fatalf("inbox list = %#v", inbox)
	}
	readRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"inbox", "read", incoming.ID, "--output", "json")
	var read core.Message
	decodeJSON(t, readRaw, &read)
	if read.ID != incoming.ID || read.Body != incoming.Body || read.State != core.MessagePending {
		t.Fatalf("inbox read = %#v", read)
	}
	ackRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"inbox", "ack", "--ack-message", incoming.ID,
		"--request-id", "p2-surface-ack", "--output", "json")
	var acknowledged []core.Message
	decodeJSON(t, ackRaw, &acknowledged)
	if len(acknowledged) != 1 || acknowledged[0].ID != incoming.ID ||
		acknowledged[0].State != core.MessageAcknowledged || acknowledged[0].AcknowledgedAt == "" {
		t.Fatalf("inbox ack = %#v", acknowledged)
	}
	currentRaw = runP2SurfaceCoordlink(t, socket, claim.Token,
		"task", "current", "--output", "json")
	decodeJSON(t, currentRaw, &current)
	if current.UnreadMessageCount != 0 {
		t.Fatalf("task current unread count after ack = %d", current.UnreadMessageCount)
	}

	replyRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"message", "send", "--to-boss", "--task", parent.ID,
		"--reply-to", incoming.ID, "--body", "requirements understood",
		"--request-id", "p2-surface-reply", "--output", "json")
	var reply core.Message
	decodeJSON(t, replyRaw, &reply)
	if reply.TaskID != parent.ID || reply.SenderKind != "agent" || reply.SenderID != parentAgent.ID ||
		reply.RecipientKind != "boss" || reply.ReplyToMessageID != incoming.ID ||
		reply.State != core.MessagePending {
		t.Fatalf("message send = %#v", reply)
	}

	createChild := func(title, requestID string) core.Task {
		t.Helper()
		raw := runP2SurfaceCoordlink(t, socket, claim.Token,
			"task", "create", "--agent", workerAgent.ID, "--title", title,
			"--description", "binary contract child", "--priority", "25", "--max-retries", "2",
			"--request-id", requestID, "--output", "json")
		var child core.Task
		decodeJSON(t, raw, &child)
		if child.ProjectID != project.ID || child.ParentTaskID != parent.ID ||
			child.CreatedByKind != "agent" || child.CreatedByID != parentAgent.ID ||
			child.AssigneeAgentID != workerAgent.ID || child.Status != core.TaskQueued ||
			child.Priority != 25 || child.MaxRetries != 2 {
			t.Fatalf("task create = %#v", child)
		}
		return child
	}
	acceptChild := createChild("accept result", "p2-surface-create-accept")
	reworkChild := createChild("rework result", "p2-surface-create-rework")
	seedP2SurfaceSubmittedTask(t, database, acceptChild, strings.Repeat("b", 40))
	seedP2SurfaceSubmittedTask(t, database, reworkChild, strings.Repeat("c", 40))

	acceptRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"task", "accept", acceptChild.ID, "--integration-agent", integrationAgent.ID,
		"--request-id", "p2-surface-accept", "--output", "json")
	var accepted core.Task
	decodeJSON(t, acceptRaw, &accepted)
	if accepted.ID != acceptChild.ID || accepted.Status != core.TaskSubmitted ||
		accepted.AcceptedByKind != "agent" || accepted.AcceptedByID != parentAgent.ID ||
		accepted.AcceptedIntegrationAgentID != integrationAgent.ID || accepted.PendingAction != "advance" ||
		accepted.PendingActionID == "" || accepted.PendingActionVersion != accepted.Version {
		t.Fatalf("task accept = %#v", accepted)
	}

	reworkRaw := runP2SurfaceCoordlink(t, socket, claim.Token,
		"task", "rework", reworkChild.ID, "--reason", "add focused coverage",
		"--request-id", "p2-surface-rework", "--output", "json")
	var reworked core.Task
	decodeJSON(t, reworkRaw, &reworked)
	if reworked.ID != reworkChild.ID || reworked.Status != core.TaskQueued ||
		reworked.NextRunAt == "" || reworked.PendingAction != "" {
		t.Fatalf("task rework = %#v", reworked)
	}

	snapshot, err := database.Snapshot(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 3 || len(snapshot.Runs) != 1 || len(snapshot.Messages) != 2 {
		t.Fatalf("durable P2 snapshot = %#v", snapshot)
	}
	persistedIncoming := p2SurfaceMessageWithID(t, snapshot, incoming.ID)
	persistedReply := p2SurfaceMessageWithID(t, snapshot, reply.ID)
	if persistedIncoming.State != core.MessageAcknowledged || persistedIncoming.AcknowledgedAt == "" ||
		persistedReply.State != core.MessagePending || persistedReply.RecipientKind != "boss" {
		t.Fatalf("durable messages incoming=%#v reply=%#v", persistedIncoming, persistedReply)
	}
	if got := contractTaskWithID(t, snapshot, acceptChild.ID); got.PendingAction != "advance" || got.AcceptedByID != parentAgent.ID {
		t.Fatalf("durable accepted child = %#v", got)
	}
	if got := contractTaskWithID(t, snapshot, reworkChild.ID); got.Status != core.TaskQueued || got.PendingAction != "" {
		t.Fatalf("durable reworked child = %#v", got)
	}
}

func runP2SurfaceCoordlink(t *testing.T, socket, token string, args ...string) []byte {
	t.Helper()
	raw, err := runCoordlink(t, socket, token, args...)
	if err != nil {
		t.Fatalf("coordlink %v: %v\n%s", args, err, raw)
	}
	return raw
}

func seedP2SurfaceSubmittedTask(t *testing.T, database *store.Store, task core.Task, head string) {
	t.Helper()
	err := database.Transact(context.Background(), func(tx core.Transaction) error {
		persisted, err := tx.Task(task.ID)
		if err != nil {
			return err
		}
		expectedVersion, expectedStatus := persisted.Version, persisted.Status
		persisted.Status = core.TaskSubmitted
		persisted.ResultSummary = "captured fixture"
		persisted.HeadSHA = head
		persisted.HeadRunID = "run-captured-" + task.ID
		persisted.TaskRef = "refs/coordplane/tasks/" + task.ID + "/runs/fixture"
		persisted.SubmittedAt = "2026-07-13T00:00:00.000000000Z"
		persisted.UpdatedAt = persisted.SubmittedAt
		persisted.Version++
		return tx.UpdateTask(persisted, expectedVersion, expectedStatus)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func p2SurfaceMessageWithID(t *testing.T, snapshot core.Snapshot, id string) core.Message {
	t.Helper()
	for _, message := range snapshot.Messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("Message %q not found in snapshot", id)
	return core.Message{}
}
