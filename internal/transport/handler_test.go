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
		{name: "list runs", method: http.MethodGet, path: "/v1/runs", call: "list_runs"},
		{name: "show run", method: http.MethodGet, path: "/v1/runs/run-1", call: "run"},
		{name: "messages", method: http.MethodGet, path: "/v1/messages", call: "list_messages"},
		{name: "ack message", method: http.MethodPost, path: "/v1/messages/msg-1/ack", call: "ack_message"},
		{name: "events", method: http.MethodGet, path: "/v1/events", call: "list_events"},
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

	assertOK(t, invoke(t, handler, http.MethodPost, "/v1/agents/agt-7/pause", `{"request_id":"req-pause"}`, ""))
	action := operations.calls[len(operations.calls)-1].value.(agentStatusCall)
	if action.id != "agt-7" || action.status != core.AgentPaused || action.requestID != "req-pause" {
		t.Fatalf("pause action = %+v", action)
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

	assertOK(t, invoke(t, handler, http.MethodGet, "/v1/events?project_id=prj-1&entity_type=task&entity_id=tsk-1&run_id=run-1&limit=25", "", ""))
	eventFilter := operations.calls[len(operations.calls)-1].value.(core.EventFilter)
	wantEvents := core.EventFilter{ProjectID: "prj-1", EntityType: "task", EntityID: "tsk-1", RunID: "run-1", Limit: 25}
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
		name        string
		method      string
		path        string
		body        string
		call        string
		wantOutcome string
	}{
		{name: "current", method: http.MethodGet, path: "/v1/task/current", call: "current_task"},
		{name: "progress", method: http.MethodPost, path: "/v1/progress", body: `{"summary":"working","request_id":"req-progress"}`, call: "progress"},
		{name: "message", method: http.MethodPost, path: "/v1/message", body: `{"body":"result","request_id":"req-message"}`, call: "message"},
		{name: "wait outcome", method: http.MethodPost, path: "/v1/task/outcome", body: `{"outcome":"wait","reason":"review","request_id":"req-wait"}`, call: "outcome", wantOutcome: "wait"},
		{name: "fail outcome", method: http.MethodPost, path: "/v1/task/outcome", body: `{"outcome":"fail","reason":"tests failed","request_id":"req-fail"}`, call: "outcome", wantOutcome: "fail"},
		{name: "submit outcome", method: http.MethodPost, path: "/v1/task/outcome", body: `{"outcome":"submit","summary":"ready","expected_head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","request_id":"req-submit"}`, call: "outcome", wantOutcome: "submit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := &runFake{}
			recorder := invoke(t, transport.NewRunHandler(operations), test.method, test.path, test.body, "run-secret")
			assertOK(t, recorder)
			if len(operations.calls) != 1 || operations.calls[0].name != test.call || operations.calls[0].token != "run-secret" {
				t.Fatalf("run calls = %+v, want %s with bearer token", operations.calls, test.call)
			}
			if test.wantOutcome != "" {
				input := operations.calls[0].value.(core.OutcomeInput)
				if input.Outcome != test.wantOutcome {
					t.Fatalf("outcome = %q, want %q", input.Outcome, test.wantOutcome)
				}
			}
		})
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
	calls []recordedCall
	err   error
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

func (f *operatorFake) Run(_ context.Context, id string) (core.Run, error) {
	return core.Run{}, f.record("run", id)
}

func (f *operatorFake) CloseConversation(_ context.Context, id, requestID string) (core.Task, error) {
	return core.Task{}, f.record("close_conversation", actionCall{id: id, requestID: requestID})
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

func (f *operatorFake) AcknowledgeBossMessage(_ context.Context, id, requestID string) (core.Message, error) {
	return core.Message{}, f.record("ack_message", actionCall{id: id, requestID: requestID})
}

func (f *operatorFake) ListEvents(_ context.Context, filter core.EventFilter) ([]core.Event, error) {
	return []core.Event{}, f.record("list_events", filter)
}

type runFake struct {
	calls []recordedCall
	err   error
}

func (f *runFake) CurrentTask(_ context.Context, token string) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "current_task", token: token})
	return core.Task{}, f.err
}

func (f *runFake) Progress(_ context.Context, input core.ProgressInput) (core.Event, error) {
	f.calls = append(f.calls, recordedCall{name: "progress", value: input, token: input.Token})
	return core.Event{}, f.err
}

func (f *runFake) AgentMessageToBoss(_ context.Context, input core.AgentMessageInput) (core.Message, error) {
	f.calls = append(f.calls, recordedCall{name: "message", value: input, token: input.Token})
	return core.Message{}, f.err
}

func (f *runFake) RequestOutcome(_ context.Context, input core.OutcomeInput) (core.Task, error) {
	f.calls = append(f.calls, recordedCall{name: "outcome", value: input, token: input.Token})
	return core.Task{}, f.err
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
