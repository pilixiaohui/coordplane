package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

func TestOperatorHandlerHasOnlyTheFixedRouteSurface(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		call   string
	}{
		{name: "status", method: http.MethodGet, path: "/v1/status?project_id=prj-1", call: "status"},
		{name: "add project", method: http.MethodPost, path: "/v1/projects", body: `{}`, call: "add_project"},
		{name: "list projects", method: http.MethodGet, path: "/v1/projects", call: "list_projects"},
		{name: "show project", method: http.MethodGet, path: "/v1/projects/prj-1", call: "project"},
		{name: "repair project", method: http.MethodPost, path: "/v1/projects/prj-1/repair", call: "repair_project"},
		{name: "archive project", method: http.MethodPost, path: "/v1/projects/prj-1/archive", body: `{}`, call: "archive_project"},
		{name: "add agent", method: http.MethodPost, path: "/v1/agents", body: `{}`, call: "add_agent"},
		{name: "list agents", method: http.MethodGet, path: "/v1/agents", call: "list_agents"},
		{name: "show agent", method: http.MethodGet, path: "/v1/agents/agt-1", call: "agent"},
		{name: "pause agent", method: http.MethodPost, path: "/v1/agents/agt-1/pause", call: "set_agent_status"},
		{name: "resume agent", method: http.MethodPost, path: "/v1/agents/agt-1/resume", body: `{}`, call: "set_agent_status"},
		{name: "archive agent", method: http.MethodPost, path: "/v1/agents/agt-1/archive", call: "archive_agent"},
		{name: "chat", method: http.MethodPost, path: "/v1/chat", body: `{}`, call: "chat"},
		{name: "create task", method: http.MethodPost, path: "/v1/tasks", body: `{}`, call: "create_task"},
		{name: "list tasks", method: http.MethodGet, path: "/v1/tasks", call: "list_tasks"},
		{name: "show task", method: http.MethodGet, path: "/v1/tasks/tsk-1", call: "task"},
		{name: "close task", method: http.MethodPost, path: "/v1/tasks/tsk-1/close", call: "close_conversation"},
		{name: "wake task", method: http.MethodPost, path: "/v1/tasks/tsk-1/wake", body: `{}`, call: "wake_task"},
		{name: "retry task", method: http.MethodPost, path: "/v1/tasks/tsk-1/retry", body: `{}`, call: "retry_task"},
		{name: "cancel task", method: http.MethodPost, path: "/v1/tasks/tsk-1/cancel", body: `{}`, call: "cancel_task"},
		{name: "accept task", method: http.MethodPost, path: "/v1/tasks/tsk-1/accept", body: `{}`, call: "accept_task"},
		{name: "rework task", method: http.MethodPost, path: "/v1/tasks/tsk-1/rework", body: `{}`, call: "rework_task"},
		{name: "list runs", method: http.MethodGet, path: "/v1/runs", call: "list_runs"},
		{name: "show run", method: http.MethodGet, path: "/v1/runs/run-1", call: "run"},
		{name: "stop run", method: http.MethodPost, path: "/v1/runs/run-1/stop", body: `{}`, call: "stop_run"},
		{name: "messages", method: http.MethodGet, path: "/v1/messages", call: "list_messages"},
		{name: "send message", method: http.MethodPost, path: "/v1/messages", body: `{}`, call: "send_boss_message"},
		{name: "read message", method: http.MethodPost, path: "/v1/messages/msg-1/read", call: "read_message"},
		{name: "ack message", method: http.MethodPost, path: "/v1/messages/msg-1/ack", call: "ack_message"},
		{name: "retry message", method: http.MethodPost, path: "/v1/messages/msg-1/retry", call: "retry_message"},
		{name: "events", method: http.MethodGet, path: "/v1/events", call: "list_events"},
		{name: "gc preview", method: http.MethodGet, path: "/v1/gc/preview", call: "gc_preview"},
		{name: "gc run", method: http.MethodPost, path: "/v1/gc/run", body: `{"confirm":true}`, call: "gc_run"},
		{name: "gc discard workspace", method: http.MethodPost, path: "/v1/gc/discard-workspace", body: `{}`, call: "gc_discard_workspace"},
		{name: "gc discard task ref", method: http.MethodPost, path: "/v1/gc/discard-task-ref", body: `{}`, call: "gc_discard_task_ref"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := &operatorFake{}
			recorder := invoke(t, transport.NewOperatorHandler(operations), test.method, test.path, test.body, "")
			envelope := decodeEnvelope(t, recorder)
			if recorder.Code != http.StatusOK || !envelope.OK || envelope.Error != nil {
				t.Fatalf("response = status:%d envelope:%+v body:%s", recorder.Code, envelope, recorder.Body.String())
			}
			if got := operations.callNames(); !reflect.DeepEqual(got, []string{test.call}) {
				t.Fatalf("operation calls = %v, want [%s]", got, test.call)
			}
		})
	}

	operations := &operatorFake{}
	handler := transport.NewOperatorHandler(operations)
	for _, path := range []string{"/call", "/capabilities", "/skills", "/v1/unknown"} {
		recorder := invoke(t, handler, http.MethodGet, path, "", "")
		envelope := decodeEnvelope(t, recorder)
		if recorder.Code != http.StatusNotFound || envelope.OK || envelope.Error == nil || envelope.Error.Code != core.CodeNotFound {
			t.Fatalf("legacy route %s response = status:%d envelope:%+v", path, recorder.Code, envelope)
		}
	}
	if len(operations.calls) != 0 {
		t.Fatalf("unknown routes invoked operations: %+v", operations.calls)
	}

	recorder := invoke(t, handler, http.MethodPut, "/v1/projects", "", "")
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusMethodNotAllowed || envelope.Error == nil || envelope.Error.Code != core.CodeInvalidArgument {
		t.Fatalf("method mismatch response = status:%d envelope:%+v", recorder.Code, envelope)
	}
}

func TestOperatorHandlerMapsBodiesPathsAndQueriesWithoutGenericDispatch(t *testing.T) {
	operations := &operatorFake{}
	handler := transport.NewOperatorHandler(operations)

	projectBody := `{"name":"demo","source":"/repo","source_ref":"refs/heads/main","request_id":"req-project"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/projects", projectBody, ""))
	projectInput := operations.calls[len(operations.calls)-1].value.(core.AddProjectInput)
	if projectInput.Name != "demo" || projectInput.SourceRef != "refs/heads/main" || projectInput.RequestID != "req-project" {
		t.Fatalf("project input = %+v", projectInput)
	}

	chatBody := `{"project_id":"prj-1","agent_id":"agt-1","body":"reply","ack_message_ids":["msg-1"],"request_id":"req-chat"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/chat", chatBody, ""))
	chat := operations.calls[len(operations.calls)-1].value.(core.ChatInput)
	if chat.ProjectID != "prj-1" || chat.RequestID != "req-chat" || !reflect.DeepEqual(chat.AckMessageIDs, []string{"msg-1"}) {
		t.Fatalf("chat input = %+v", chat)
	}

	createBody := `{"project_id":"prj-1","assignee_agent_id":"agt-1","title":"follow-up","retry_of_task_id":"tsk-closed","ack_message_ids":["msg-2"],"request_id":"req-create"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/tasks", createBody, ""))
	create := operations.calls[len(operations.calls)-1].value.(core.CreateTaskInput)
	if create.ProjectID != "prj-1" || create.RetryOfTaskID != "tsk-closed" || create.RequestID != "req-create" || !reflect.DeepEqual(create.AckMessageIDs, []string{"msg-2"}) {
		t.Fatalf("create task input = %+v", create)
	}

	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/agents/agt-7/pause", `{"request_id":"req-pause"}`, ""))
	action := operations.calls[len(operations.calls)-1].value.(agentStatusCall)
	if action.id != "agt-7" || action.status != core.AgentPaused || action.requestID != "req-pause" {
		t.Fatalf("pause action = %+v", action)
	}

	taskActionBody := `{"reason":"try again","ack_message_ids":["msg-1"],"request_id":"req-rework"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/tasks/tsk-7/rework", taskActionBody, ""))
	taskAction := operations.calls[len(operations.calls)-1].value.(core.TaskActionInput)
	if taskAction.TaskID != "tsk-7" || taskAction.Reason != "try again" || taskAction.RequestID != "req-rework" || !reflect.DeepEqual(taskAction.AckMessageIDs, []string{"msg-1"}) {
		t.Fatalf("task action = %+v", taskAction)
	}

	acceptBody := `{"integration_agent_id":"agt-integrator","ack_message_ids":["msg-2"],"request_id":"req-accept"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/tasks/tsk-8/accept", acceptBody, ""))
	accept := operations.calls[len(operations.calls)-1].value.(core.AcceptInput)
	if accept.TaskID != "tsk-8" || accept.IntegrationAgentID != "agt-integrator" || accept.RequestID != "req-accept" || !reflect.DeepEqual(accept.AckMessageIDs, []string{"msg-2"}) {
		t.Fatalf("accept input = %+v", accept)
	}

	stopBody := `{"reason":"operator requested","request_id":"req-stop"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/runs/run-7/stop", stopBody, ""))
	stop := operations.calls[len(operations.calls)-1].value.(core.RunStopInput)
	if stop.RunID != "run-7" || stop.Reason != "operator requested" || stop.RequestID != "req-stop" {
		t.Fatalf("run stop input = %+v", stop)
	}

	messageBody := `{"project_id":"prj-1","agent_id":"agt-1","task_id":"tsk-1","body":"direct","wake":true,"ack_message_ids":["msg-3"],"request_id":"req-message"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/messages", messageBody, ""))
	message := operations.calls[len(operations.calls)-1].value.(core.BossMessageInput)
	if message.ProjectID != "prj-1" || message.TaskID != "tsk-1" || !message.Wake || !reflect.DeepEqual(message.AckMessageIDs, []string{"msg-3"}) {
		t.Fatalf("Boss message input = %+v", message)
	}

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/tasks?project_id=prj-1&cursor=opaque-task&limit=25", "", ""))
	taskFilter := operations.calls[len(operations.calls)-1].value.(core.TaskFilter)
	if want := (core.TaskFilter{ProjectID: "prj-1", Cursor: "opaque-task", Limit: 25}); taskFilter != want {
		t.Fatalf("task filter = %+v, want %+v", taskFilter, want)
	}

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/runs?project_id=prj-1&task_id=tsk-1&agent_id=agt-1&cursor=opaque-run&limit=30", "", ""))
	runFilter := operations.calls[len(operations.calls)-1].value.(core.RunFilter)
	if want := (core.RunFilter{ProjectID: "prj-1", TaskID: "tsk-1", AgentID: "agt-1", Cursor: "opaque-run", Limit: 30}); runFilter != want {
		t.Fatalf("run filter = %+v, want %+v", runFilter, want)
	}

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/messages?project_id=prj-1&task_id=tsk-1&recipient_kind=boss&recipient_id=owner&cursor=opaque-message&limit=20", "", ""))
	messageFilter := operations.calls[len(operations.calls)-1].value.(core.MessageFilter)
	wantMessages := core.MessageFilter{ProjectID: "prj-1", TaskID: "tsk-1", RecipientKind: "boss", RecipientID: "owner", Cursor: "opaque-message", Limit: 20}
	if messageFilter != wantMessages {
		t.Fatalf("message filter = %+v, want %+v", messageFilter, wantMessages)
	}

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/events?project_id=prj-1&entity_type=task&entity_id=tsk-1&run_id=run-1&cursor=opaque-event&limit=25", "", ""))
	eventFilter := operations.calls[len(operations.calls)-1].value.(core.EventFilter)
	wantEvents := core.EventFilter{ProjectID: "prj-1", EntityType: "task", EntityID: "tsk-1", RunID: "run-1", Cursor: "opaque-event", Limit: 25}
	if eventFilter != wantEvents {
		t.Fatalf("event filter = %+v, want %+v", eventFilter, wantEvents)
	}

	before := len(operations.calls)
	recorder := invoke(t, handler, http.MethodGet, "/v1/events?limit=not-a-number", "", "")
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != core.CodeInvalidArgument {
		t.Fatalf("invalid query response = status:%d envelope:%+v", recorder.Code, envelope)
	}
	if len(operations.calls) != before {
		t.Fatal("invalid query reached core operations")
	}
}

func TestRunHandlerForwardsOnlyBearerTokenToCore(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		call   string
	}{
		{name: "current", method: http.MethodGet, path: "/v1/task/current", call: "current_task"},
		{name: "show task", method: http.MethodGet, path: "/v1/task/task-child", call: "task_for_run"},
		{name: "create child", method: http.MethodPost, path: "/v1/task/create", body: `{}`, call: "create_child_task"},
		{name: "outcome", method: http.MethodPost, path: "/v1/task/outcome", body: `{"outcome":"wait"}`, call: "request_outcome"},
		{name: "accept child", method: http.MethodPost, path: "/v1/task/task-child/accept", body: `{}`, call: "accept_task"},
		{name: "rework child", method: http.MethodPost, path: "/v1/task/task-child/rework", body: `{}`, call: "rework_task"},
		{name: "inbox", method: http.MethodGet, path: "/v1/inbox", call: "inbox"},
		{name: "read inbox", method: http.MethodGet, path: "/v1/inbox/msg-1", call: "inbox_message"},
		{name: "ack inbox", method: http.MethodPost, path: "/v1/inbox/ack", body: `{}`, call: "ack_agent_messages"},
		{name: "progress", method: http.MethodPost, path: "/v1/progress", body: `{"summary":"working","request_id":"req-progress"}`, call: "progress"},
		{name: "message", method: http.MethodPost, path: "/v1/message", body: `{"recipient_kind":"boss","body":"result","request_id":"req-message"}`, call: "send_agent_message"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := &runFake{}
			recorder := invoke(t, transport.NewRunHandler(operations), test.method, test.path, test.body, "run-secret")
			assertOK(t, recorder)
			if len(operations.calls) != 1 || operations.calls[0].name != test.call || operations.calls[0].token != "run-secret" {
				t.Fatalf("run calls = %+v, want %s with bearer token", operations.calls, test.call)
			}
		})
	}
}

func TestRunHandlerMapsP2BodiesWithoutAllowingBodyScopeOverride(t *testing.T) {
	operations := &runFake{}
	handler := transport.NewRunHandler(operations)

	createBody := `{"assignee_agent_id":"agent-b","title":"child","description":"work","priority":7,"max_retries":2,"ack_message_ids":["msg-1"],"request_id":"req-create"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/task/create", createBody, "run-secret"))
	create := operations.calls[len(operations.calls)-1].value.(core.CreateChildTaskInput)
	if create.Token != "run-secret" || create.AssigneeAgentID != "agent-b" || create.Title != "child" || create.Priority != 7 || !reflect.DeepEqual(create.AckMessageIDs, []string{"msg-1"}) {
		t.Fatalf("create input = %+v", create)
	}

	outcomeBody := `{"outcome":"submit","summary":"ready","expected_head":"abc123","ack_message_ids":["msg-2"],"request_id":"req-submit"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/task/outcome", outcomeBody, "run-secret"))
	outcome := operations.calls[len(operations.calls)-1].value.(core.OutcomeInput)
	if outcome.Token != "run-secret" || outcome.Outcome != core.OutcomeSubmit || outcome.Summary != "ready" || outcome.ExpectedHead != "abc123" || !reflect.DeepEqual(outcome.AckMessageIDs, []string{"msg-2"}) {
		t.Fatalf("outcome input = %+v", outcome)
	}

	ackBody := `{"message_ids":["msg-3","msg-4"],"request_id":"req-ack"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/inbox/ack", ackBody, "run-secret"))
	ack := operations.calls[len(operations.calls)-1].value.(core.AcknowledgeMessagesInput)
	if ack.Token != "run-secret" || ack.RequestID != "req-ack" || !reflect.DeepEqual(ack.MessageIDs, []string{"msg-3", "msg-4"}) {
		t.Fatalf("ack input = %+v", ack)
	}

	messageBody := `{"recipient_kind":"agent","recipient_id":"agent-c","task_id":"task-c","related_task_id":"task-source","body":"question","wake":true,"reply_to_message_id":"msg-5","ack_message_ids":["msg-5"],"request_id":"req-message"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/message", messageBody, "run-secret"))
	message := operations.calls[len(operations.calls)-1].value.(core.SendMessageInput)
	if message.Token != "run-secret" || message.RecipientKind != "agent" || message.RecipientID != "agent-c" || message.TaskID != "task-c" || message.RelatedTaskID != "task-source" || !message.Wake || !reflect.DeepEqual(message.AckMessageIDs, []string{"msg-5"}) {
		t.Fatalf("message input = %+v", message)
	}

	acceptBody := `{"task_id":"forged","integration_agent_id":"agent-i","ack_message_ids":["msg-6"],"request_id":"req-accept"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/task/task-child/accept", acceptBody, "run-secret"))
	accept := operations.calls[len(operations.calls)-1].value.(core.AcceptInput)
	if accept.Token != "run-secret" || accept.TaskID != "task-child" || accept.IntegrationAgentID != "agent-i" || !reflect.DeepEqual(accept.AckMessageIDs, []string{"msg-6"}) {
		t.Fatalf("accept input = %+v", accept)
	}

	reworkBody := `{"task_id":"forged","reason":"needs changes","ack_message_ids":["msg-7"],"request_id":"req-rework"}`
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/task/task-child/rework", reworkBody, "run-secret"))
	rework := operations.calls[len(operations.calls)-1].value.(core.TaskActionInput)
	if rework.Token != "run-secret" || rework.TaskID != "task-child" || rework.Reason != "needs changes" || !reflect.DeepEqual(rework.AckMessageIDs, []string{"msg-7"}) {
		t.Fatalf("rework input = %+v", rework)
	}

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/task/task-child", "", "run-secret"))
	show := operations.calls[len(operations.calls)-1]
	if show.token != "run-secret" || show.value != "task-child" {
		t.Fatalf("task show call = %+v", show)
	}

	before := len(operations.calls)
	for _, path := range []string{"/call", "/capabilities", "/skills", "/v1/raw"} {
		recorder := invoke(t, handler, http.MethodPost, path, `{}`, "run-secret")
		envelope := decodeEnvelope(t, recorder)
		if recorder.Code != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != core.CodeNotFound {
			t.Fatalf("generic path %s response = status:%d envelope:%+v", path, recorder.Code, envelope)
		}
	}
	if len(operations.calls) != before {
		t.Fatalf("generic paths reached Core: %+v", operations.calls[before:])
	}
}

func TestRunHandlerReturnsCoreErrorsAndRejectsMalformedJSONBeforeSideEffects(t *testing.T) {
	operations := &runFake{err: core.Conflict(core.CodeStaleRun, "stale", "exited", 3)}
	handler := transport.NewRunHandler(operations)
	recorder := invoke(t, handler, http.MethodPost, "/v1/progress", `{"summary":"late"}`, "old-token")
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusConflict || envelope.OK || envelope.Error == nil || envelope.Error.Code != core.CodeStaleRun {
		t.Fatalf("core error response = status:%d envelope:%+v", recorder.Code, envelope)
	}
	if envelope.Error.State != "exited" || envelope.Error.Version != 3 {
		t.Fatalf("core conflict detail was lost: %+v", envelope.Error)
	}

	operations = &runFake{}
	handler = transport.NewRunHandler(operations)
	recorder = invoke(t, handler, http.MethodPost, "/v1/progress", `{"summary":"work","token":"forged"}`, "real-token")
	envelope = decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != core.CodeInvalidArgument {
		t.Fatalf("forged body token response = status:%d envelope:%+v", recorder.Code, envelope)
	}
	if len(operations.calls) != 0 {
		t.Fatalf("malformed request reached core: %+v", operations.calls)
	}

	operations.err = core.NewError(core.CodeScopeDenied, "scope denied", false)
	recorder = invoke(t, handler, http.MethodGet, "/v1/task/current", "", "Basic ignored")
	envelope = decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusForbidden || envelope.Error == nil || envelope.Error.Code != core.CodeScopeDenied {
		t.Fatalf("missing bearer response = status:%d envelope:%+v", recorder.Code, envelope)
	}
	if len(operations.calls) != 1 || operations.calls[0].token != "" {
		t.Fatalf("non-bearer authorization was not reduced to empty token: %+v", operations.calls)
	}
}

func TestHandlersFenceEveryMutationUntilDaemonReady(t *testing.T) {
	operatorRoutes := []string{
		"/v1/projects", "/v1/projects/prj-1/repair", "/v1/projects/prj-1/archive",
		"/v1/agents", "/v1/agents/agt-1/pause", "/v1/agents/agt-1/resume", "/v1/agents/agt-1/archive",
		"/v1/chat", "/v1/tasks", "/v1/tasks/tsk-1/checkout", "/v1/tasks/tsk-1/close",
		"/v1/tasks/tsk-1/wake", "/v1/tasks/tsk-1/retry", "/v1/tasks/tsk-1/cancel",
		"/v1/tasks/tsk-1/accept", "/v1/tasks/tsk-1/rework", "/v1/runs/run-1/stop",
		"/v1/messages", "/v1/messages/msg-1/read", "/v1/messages/msg-1/ack", "/v1/messages/msg-1/retry",
		"/v1/gc/run", "/v1/gc/discard-workspace", "/v1/gc/discard-task-ref",
	}
	for _, path := range operatorRoutes {
		t.Run("operator "+path, func(t *testing.T) {
			operations := &operatorFake{readyErr: core.NewError(core.CodeRuntimeUnavailable, "recovering", true)}
			recorder := invoke(t, transport.NewOperatorHandler(operations), http.MethodPost, path, `{}`, "")
			assertNotReady(t, recorder)
			if len(operations.calls) != 0 || operations.readyChecks != 1 {
				t.Fatalf("calls=%+v ready checks=%d, want no operation and one readiness check", operations.calls, operations.readyChecks)
			}
		})
	}

	runRoutes := []string{
		"/v1/task/create", "/v1/task/outcome", "/v1/task/task-1/accept", "/v1/task/task-1/rework",
		"/v1/inbox/ack", "/v1/progress", "/v1/message",
	}
	for _, path := range runRoutes {
		t.Run("run "+path, func(t *testing.T) {
			operations := &runFake{readyErr: core.NewError(core.CodeRuntimeUnavailable, "recovering", true)}
			recorder := invoke(t, transport.NewRunHandler(operations), http.MethodPost, path, `{}`, "run-secret")
			assertNotReady(t, recorder)
			if len(operations.calls) != 0 || operations.readyChecks != 1 {
				t.Fatalf("calls=%+v ready checks=%d, want no operation and one readiness check", operations.calls, operations.readyChecks)
			}
		})
	}
}

func TestHandlersKeepReadsAvailableAndForwardReadyMutationOnce(t *testing.T) {
	operator := &operatorFake{}
	handler := transport.NewOperatorHandler(operator)
	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/status", "", ""))
	if operator.readyChecks != 0 || !reflect.DeepEqual(operator.callNames(), []string{"status"}) {
		t.Fatalf("GET ready checks=%d calls=%v", operator.readyChecks, operator.callNames())
	}
	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/projects", `{}`, ""))
	if operator.readyChecks != 1 || !reflect.DeepEqual(operator.callNames(), []string{"status", "add_project"}) {
		t.Fatalf("ready POST checks=%d calls=%v", operator.readyChecks, operator.callNames())
	}

	run := &runFake{}
	runHandler := transport.NewRunHandler(run)
	assertOK(t, invoke(t, runHandler, http.MethodGet, "/v1/task/current", "", "run-secret"))
	if run.readyChecks != 0 || len(run.calls) != 1 || run.calls[0].name != "current_task" {
		t.Fatalf("run GET ready checks=%d calls=%+v", run.readyChecks, run.calls)
	}
	assertOK(t, invoke(t, runHandler, http.MethodPost, "/v1/progress", `{}`, "run-secret"))
	if run.readyChecks != 1 || len(run.calls) != 2 || run.calls[1].name != "progress" {
		t.Fatalf("run POST ready checks=%d calls=%+v", run.readyChecks, run.calls)
	}
}

func TestCoreErrorsAlwaysMapToNonSuccessHTTPStatuses(t *testing.T) {
	tests := []struct {
		code core.ErrorCode
		want int
	}{
		{code: core.CodeInvalidArgument, want: http.StatusBadRequest},
		{code: core.CodeNotFound, want: http.StatusNotFound},
		{code: core.CodeScopeDenied, want: http.StatusForbidden},
		{code: core.CodeInvalidState, want: http.StatusConflict},
		{code: core.CodeActionInProgress, want: http.StatusConflict},
		{code: core.CodeVersionConflict, want: http.StatusConflict},
		{code: core.CodeStaleRun, want: http.StatusConflict},
		{code: core.CodeRunStarting, want: http.StatusConflict},
		{code: core.CodeAgentBusy, want: http.StatusConflict},
		{code: core.CodeGitDirty, want: http.StatusConflict},
		{code: core.CodeGitStale, want: http.StatusConflict},
		{code: core.CodeIntegrationAgentRequired, want: http.StatusConflict},
		{code: core.CodeRuntimeUnavailable, want: http.StatusServiceUnavailable},
		{code: core.CodeResumeUnavailable, want: http.StatusServiceUnavailable},
		{code: core.CodeLegacySchemaRebuildRequired, want: http.StatusServiceUnavailable},
		{code: core.CodeGitInvariantViolation, want: http.StatusInternalServerError},
		{code: core.CodeInternal, want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			operations := &operatorFake{err: core.NewError(test.code, "test error", false)}
			recorder := invoke(t, transport.NewOperatorHandler(operations), http.MethodGet, "/v1/status", "", "")
			envelope := decodeEnvelope(t, recorder)
			if recorder.Code != test.want || recorder.Code < 300 || envelope.OK || envelope.Error == nil || envelope.Error.Code != test.code {
				t.Fatalf("error %s response = status:%d envelope:%+v", test.code, recorder.Code, envelope)
			}
		})
	}

	recorder := invoke(t, transport.NewOperatorHandler(nil), http.MethodGet, "/v1/status", "", "")
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusInternalServerError || envelope.Error == nil || envelope.Error.Code != core.CodeInternal {
		t.Fatalf("nil operations response = status:%d envelope:%+v", recorder.Code, envelope)
	}
}

type recordedCall struct {
	name  string
	value any
	token string
}

type agentStatusCall struct {
	id        string
	status    core.AgentStatus
	requestID string
}

type actionCall struct {
	id        string
	requestID string
}

type operatorFake struct {
	calls       []recordedCall
	err         error
	readyErr    error
	readyChecks int
}

func (f *operatorFake) RequireReady() error {
	f.readyChecks++
	return f.readyErr
}

func (f *operatorFake) record(name string, value any) error {
	f.calls = append(f.calls, recordedCall{name: name, value: value})
	return f.err
}

func (f *operatorFake) callNames() []string {
	names := make([]string, len(f.calls))
	for index := range f.calls {
		names[index] = f.calls[index].name
	}
	return names
}

func (f *operatorFake) Status(_ context.Context, projectID string) (core.Status, error) {
	return core.Status{}, f.record("status", projectID)
}

func (f *operatorFake) Project(_ context.Context, id string) (core.ProjectDetail, error) {
	return core.ProjectDetail{}, f.record("project", id)
}

func (f *operatorFake) Agent(_ context.Context, id string) (core.Agent, error) {
	return core.Agent{}, f.record("agent", id)
}

func (f *operatorFake) ListProjects(_ context.Context, filter core.ProjectFilter) (core.ProjectPage, error) {
	return core.ProjectPage{}, f.record("list_projects", filter)
}

func (f *operatorFake) ListAgents(_ context.Context, filter core.AgentFilter) (core.AgentPage, error) {
	return core.AgentPage{}, f.record("list_agents", filter)
}

func (f *operatorFake) AddProject(_ context.Context, input core.AddProjectInput) (core.Project, error) {
	return core.Project{}, f.record("add_project", input)
}

func (f *operatorFake) RepairProject(_ context.Context, id, requestID string) (core.Project, error) {
	return core.Project{}, f.record("repair_project", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) ArchiveProject(_ context.Context, id, requestID string) (core.Project, error) {
	return core.Project{}, f.record("archive_project", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) AddAgent(_ context.Context, input core.AddAgentInput) (core.Agent, error) {
	return core.Agent{}, f.record("add_agent", input)
}

func (f *operatorFake) SetAgentStatus(_ context.Context, id string, status core.AgentStatus, requestID string) (core.Agent, error) {
	return core.Agent{}, f.record("set_agent_status", agentStatusCall{id: id, status: status, requestID: requestID})
}

func (f *operatorFake) ArchiveAgent(_ context.Context, id, requestID string) (core.Agent, error) {
	return core.Agent{}, f.record("archive_agent", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) Chat(_ context.Context, input core.ChatInput) (core.ChatResult, error) {
	return core.ChatResult{}, f.record("chat", input)
}

func (f *operatorFake) CreateTask(_ context.Context, input core.CreateTaskInput) (core.Task, error) {
	return core.Task{}, f.record("create_task", input)
}

func (f *operatorFake) Task(_ context.Context, id string) (core.TaskDetail, error) {
	return core.TaskDetail{}, f.record("task", id)
}

func (f *operatorFake) CheckoutTask(_ context.Context, input core.TaskCheckoutInput) (core.GitCheckoutFact, error) {
	return core.GitCheckoutFact{}, f.record("checkout_task", input)
}

func (f *operatorFake) GCPreview(context.Context) (core.GCPreview, error) {
	f.record("gc_preview", nil)
	return core.GCPreview{}, nil
}

func (f *operatorFake) GCRun(_ context.Context, input core.GCRunInput) (core.GCRunResult, error) {
	f.record("gc_run", input)
	return core.GCRunResult{Completed: true}, nil
}

func (f *operatorFake) GCDiscardWorkspace(_ context.Context, input core.GCDiscardWorkspaceInput) (core.GCDiscardResult, error) {
	f.record("gc_discard_workspace", input)
	return core.GCDiscardResult{TaskID: input.TaskID, Discarded: true}, nil
}

func (f *operatorFake) GCDiscardTaskRef(_ context.Context, input core.GCDiscardTaskRefInput) (core.GCDiscardResult, error) {
	f.record("gc_discard_task_ref", input)
	return core.GCDiscardResult{TaskID: input.TaskID, RunID: input.RunID, Discarded: true}, nil
}

func (f *operatorFake) Run(_ context.Context, id string) (core.Run, error) {
	return core.Run{}, f.record("run", id)
}

func (f *operatorFake) CloseConversation(_ context.Context, id, requestID string) (core.Task, error) {
	return core.Task{}, f.record("close_conversation", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) WakeTask(_ context.Context, input core.TaskActionInput) (core.Task, error) {
	return core.Task{}, f.record("wake_task", input)
}

func (f *operatorFake) RetryTask(_ context.Context, input core.TaskActionInput) (core.Task, error) {
	return core.Task{}, f.record("retry_task", input)
}

func (f *operatorFake) CancelTask(_ context.Context, input core.TaskActionInput) (core.Task, error) {
	return core.Task{}, f.record("cancel_task", input)
}

func (f *operatorFake) RequestAccept(_ context.Context, input core.AcceptInput) (core.Task, error) {
	return core.Task{}, f.record("accept_task", input)
}

func (f *operatorFake) ReworkTask(_ context.Context, input core.TaskActionInput) (core.Task, error) {
	return core.Task{}, f.record("rework_task", input)
}

func (f *operatorFake) ListTasks(_ context.Context, filter core.TaskFilter) (core.TaskPage, error) {
	return core.TaskPage{}, f.record("list_tasks", filter)
}

func (f *operatorFake) ListRuns(_ context.Context, filter core.RunFilter) (core.RunPage, error) {
	return core.RunPage{}, f.record("list_runs", filter)
}

func (f *operatorFake) ListMessages(_ context.Context, filter core.MessageFilter) (core.MessagePage, error) {
	return core.MessagePage{}, f.record("list_messages", filter)
}

func (f *operatorFake) SendBossMessage(_ context.Context, input core.BossMessageInput) (core.Message, error) {
	return core.Message{}, f.record("send_boss_message", input)
}

func (f *operatorFake) ReadBossMessage(_ context.Context, id, requestID string) (core.Message, error) {
	return core.Message{}, f.record("read_message", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) AcknowledgeBossMessage(_ context.Context, id, requestID string) (core.Message, error) {
	return core.Message{}, f.record("ack_message", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) RetryMessage(_ context.Context, id, requestID string) (core.Message, error) {
	return core.Message{}, f.record("retry_message", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) RequestRunStop(_ context.Context, input core.RunStopInput) (core.Run, error) {
	return core.Run{}, f.record("stop_run", input)
}

func (f *operatorFake) ListEvents(_ context.Context, filter core.EventFilter) (core.EventPage, error) {
	return core.EventPage{Items: []core.Event{}}, f.record("list_events", filter)
}

type runFake struct {
	calls       []recordedCall
	err         error
	readyErr    error
	readyChecks int
}

func (f *runFake) RequireReady() error {
	f.readyChecks++
	return f.readyErr
}

func (f *runFake) CurrentTask(_ context.Context, token string) (core.CurrentTaskResult, error) {
	f.calls = append(f.calls, recordedCall{name: "current_task", token: token})
	return core.CurrentTaskResult{}, f.err
}

func (f *runFake) TaskForRun(_ context.Context, token, taskID string) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "task_for_run", value: taskID, token: token})
	return core.Task{}, f.err
}

func (f *runFake) CreateChildTask(_ context.Context, input core.CreateChildTaskInput) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "create_child_task", value: input, token: input.Token})
	return core.Task{}, f.err
}

func (f *runFake) RequestOutcome(_ context.Context, input core.OutcomeInput) (core.OutcomeResult, error) {
	f.calls = append(f.calls, recordedCall{name: "request_outcome", value: input, token: input.Token})
	return core.OutcomeResult{}, f.err
}

func (f *runFake) RequestAccept(_ context.Context, input core.AcceptInput) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "accept_task", value: input, token: input.Token})
	return core.Task{}, f.err
}

func (f *runFake) ReworkTask(_ context.Context, input core.TaskActionInput) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "rework_task", value: input, token: input.Token})
	return core.Task{}, f.err
}

func (f *runFake) Inbox(_ context.Context, token string) ([]core.Message, error) {
	f.calls = append(f.calls, recordedCall{name: "inbox", token: token})
	return nil, f.err
}

func (f *runFake) InboxMessage(_ context.Context, token, messageID string) (core.Message, error) {
	f.calls = append(f.calls, recordedCall{name: "inbox_message", value: messageID, token: token})
	return core.Message{}, f.err
}

func (f *runFake) AcknowledgeAgentMessages(_ context.Context, input core.AcknowledgeMessagesInput) ([]core.Message, error) {
	f.calls = append(f.calls, recordedCall{name: "ack_agent_messages", value: input, token: input.Token})
	return nil, f.err
}

func (f *runFake) SendAgentMessage(_ context.Context, input core.SendMessageInput) (core.Message, error) {
	f.calls = append(f.calls, recordedCall{name: "send_agent_message", value: input, token: input.Token})
	return core.Message{}, f.err
}

func (f *runFake) Progress(_ context.Context, input core.ProgressInput) (core.Event, error) {
	f.calls = append(f.calls, recordedCall{name: "progress", value: input, token: input.Token})
	return core.Event{}, f.err
}

func invoke(t *testing.T, handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		if token == "Basic ignored" {
			request.Header.Set("Authorization", token)
		} else {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func assertOK(t *testing.T, recorder *httptest.ResponseRecorder) transport.Envelope {
	t.Helper()
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusOK || !envelope.OK || envelope.Error != nil {
		t.Fatalf("response = status:%d envelope:%+v body:%s", recorder.Code, envelope, recorder.Body.String())
	}
	return envelope
}

func assertNotReady(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	envelope := decodeEnvelope(t, recorder)
	if recorder.Code != http.StatusServiceUnavailable || envelope.OK || envelope.Error == nil ||
		envelope.Error.Code != core.CodeRuntimeUnavailable || !envelope.Error.Retryable {
		t.Fatalf("not-ready response = status:%d envelope:%+v body:%s", recorder.Code, envelope, recorder.Body.String())
	}
}

func decodeEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) transport.Envelope {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var envelope transport.Envelope
	decoder := json.NewDecoder(recorder.Body)
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, recorder.Body.String())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected trailing JSON content: %v", err)
	}
	return envelope
}
