package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"coordplane/internal/core"
	"coordplane/internal/perfobs"
)

const maxRequestBytes = 1 << 20

type actionRequest struct {
	RequestID string `json:"request_id"`
}

// NewOperatorHandler returns the complete fixed Boss-facing HTTP surface.
func NewOperatorHandler(operations OperatorOperations) http.Handler {
	if operations == nil {
		return unavailableHandler("operator operations are required")
	}
	mux := http.NewServeMux()
	registerProjectAgentRoutes(mux, operations)
	registerTaskRoutes(mux, operations)

	mux.HandleFunc("/v1/runs", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit, err := queryInt(query.Get("limit"), "limit")
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := operations.ListRuns(r.Context(), core.RunFilter{
			ProjectID: strings.TrimSpace(query.Get("project_id")), TaskID: strings.TrimSpace(query.Get("task_id")),
			AgentID: strings.TrimSpace(query.Get("agent_id")), Cursor: strings.TrimSpace(query.Get("cursor")), Limit: limit,
		})
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/runs/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Run(r.Context(), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/runs/{id}/stop", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.RunStopInput) (any, error) {
		input.RunID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.RequestRunStop(ctx.Context, input)
	})))
	registerMessageEventGCRoutes(mux, operations)
	mux.HandleFunc("/", notFound)
	return mux
}

func registerProjectAgentRoutes(mux *http.ServeMux, operations OperatorOperations) {
	mux.HandleFunc("/v1/status", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Status(r.Context(), strings.TrimSpace(r.URL.Query().Get("project_id")))
		writeResult(w, result, err)
	}))
	addProject := decodeCall(func(ctx requestContext, input core.AddProjectInput) (any, error) {
		return operations.AddProject(ctx.Context, input)
	})
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			addProject(w, r)
		case http.MethodGet:
			limit, err := queryInt(r.URL.Query().Get("limit"), "limit")
			if err != nil {
				writeError(w, err)
				return
			}
			result, err := operations.ListProjects(r.Context(), core.ProjectFilter{Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")), Limit: limit})
			writeResult(w, result, err)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	})
	mux.HandleFunc("/v1/projects/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Project(r.Context(), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/projects/{id}/repair", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.RepairProject(ctx.Context, id, requestID)
	})))
	mux.HandleFunc("/v1/projects/{id}/archive", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.ArchiveProject(ctx.Context, id, requestID)
	})))
	addAgent := decodeCall(func(ctx requestContext, input core.AddAgentInput) (any, error) {
		return operations.AddAgent(ctx.Context, input)
	})
	mux.HandleFunc("/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			addAgent(w, r)
		case http.MethodGet:
			limit, err := queryInt(r.URL.Query().Get("limit"), "limit")
			if err != nil {
				writeError(w, err)
				return
			}
			result, err := operations.ListAgents(r.Context(), core.AgentFilter{Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")), Limit: limit})
			writeResult(w, result, err)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	})
	mux.HandleFunc("/v1/agents/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Agent(r.Context(), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/agents/{id}/pause", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.SetAgentStatus(ctx.Context, id, core.AgentPaused, requestID)
	})))
	mux.HandleFunc("/v1/agents/{id}/resume", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.SetAgentStatus(ctx.Context, id, core.AgentActive, requestID)
	})))
	mux.HandleFunc("/v1/agents/{id}/archive", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.ArchiveAgent(ctx.Context, id, requestID)
	})))
}

func registerTaskRoutes(mux *http.ServeMux, operations OperatorOperations) {
	mux.HandleFunc("/v1/chat", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.ChatInput) (any, error) {
		return operations.Chat(ctx.Context, input)
	})))
	createTask := decodeCall(func(ctx requestContext, input core.CreateTaskInput) (any, error) {
		return operations.CreateTask(ctx.Context, input)
	})
	mux.HandleFunc("/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			createTask(w, r)
		case http.MethodGet:
			limit, err := queryInt(r.URL.Query().Get("limit"), "limit")
			if err != nil {
				writeError(w, err)
				return
			}
			result, err := operations.ListTasks(r.Context(), core.TaskFilter{
				ProjectID: strings.TrimSpace(r.URL.Query().Get("project_id")),
				Cursor:    strings.TrimSpace(r.URL.Query().Get("cursor")), Limit: limit,
			})
			writeResult(w, result, err)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	})
	mux.HandleFunc("/v1/tasks/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Task(r.Context(), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/tasks/{id}/checkout", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskCheckoutInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.CheckoutTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/tasks/{id}/close", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.CloseConversation(ctx.Context, id, requestID)
	})))
	mux.HandleFunc("/v1/tasks/{id}/wake", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskActionInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.WakeTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/tasks/{id}/retry", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskActionInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.RetryTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/tasks/{id}/cancel", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskActionInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.CancelTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/tasks/{id}/accept", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.AcceptInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.RequestAccept(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/tasks/{id}/rework", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskActionInput) (any, error) {
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.ReworkTask(ctx.Context, input)
	})))
}

func registerMessageEventGCRoutes(mux *http.ServeMux, operations OperatorOperations) {
	sendBossMessage := decodeCall(func(ctx requestContext, input core.BossMessageInput) (any, error) {
		return operations.SendBossMessage(ctx.Context, input)
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			sendBossMessage(w, r)
		case http.MethodGet:
			query := r.URL.Query()
			limit, err := queryInt(query.Get("limit"), "limit")
			if err != nil {
				writeError(w, err)
				return
			}
			result, err := operations.ListMessages(r.Context(), core.MessageFilter{
				ProjectID:     strings.TrimSpace(query.Get("project_id")),
				TaskID:        strings.TrimSpace(query.Get("task_id")),
				RecipientKind: strings.TrimSpace(query.Get("recipient_kind")),
				RecipientID:   strings.TrimSpace(query.Get("recipient_id")),
				Cursor:        strings.TrimSpace(query.Get("cursor")),
				Limit:         limit,
			})
			writeResult(w, result, err)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	})
	mux.HandleFunc("/v1/messages/{id}/read", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.ReadBossMessage(ctx.Context, id, requestID)
	})))
	mux.HandleFunc("/v1/messages/{id}/ack", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.AcknowledgeBossMessage(ctx.Context, id, requestID)
	})))
	mux.HandleFunc("/v1/messages/{id}/retry", requireMethod(http.MethodPost, actionCall(func(ctx requestContext, id, requestID string) (any, error) {
		return operations.RetryMessage(ctx.Context, id, requestID)
	})))
	mux.HandleFunc("/v1/events", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit, err := queryInt(query.Get("limit"), "limit")
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := operations.ListEvents(r.Context(), core.EventFilter{
			ProjectID:  strings.TrimSpace(query.Get("project_id")),
			EntityType: strings.TrimSpace(query.Get("entity_type")),
			EntityID:   strings.TrimSpace(query.Get("entity_id")),
			RunID:      strings.TrimSpace(query.Get("run_id")),
			Cursor:     strings.TrimSpace(query.Get("cursor")),
			Limit:      limit,
		})
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/gc/preview", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.GCPreview(r.Context())
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/gc/run", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.GCRunInput) (any, error) {
		return operations.GCRun(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/gc/discard-workspace", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.GCDiscardWorkspaceInput) (any, error) {
		return operations.GCDiscardWorkspace(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/gc/discard-task-ref", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.GCDiscardTaskRefInput) (any, error) {
		return operations.GCDiscardTaskRef(ctx.Context, input)
	})))
}

// NewRunHandler returns the complete fixed per-Run HTTP surface.
func NewRunHandler(operations RunOperations) http.Handler {
	if operations == nil {
		return unavailableHandler("run operations are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/task/current", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.CurrentTask(r.Context(), bearerToken(r))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/task/create", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.CreateChildTaskInput) (any, error) {
		input.Token = ctx.Token
		return operations.CreateChildTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/task/outcome", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.OutcomeInput) (any, error) {
		input.Token = ctx.Token
		return operations.RequestOutcome(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/task/{id}/accept", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.AcceptInput) (any, error) {
		input.Token = ctx.Token
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.RequestAccept(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/task/{id}/rework", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.TaskActionInput) (any, error) {
		input.Token = ctx.Token
		input.TaskID = strings.TrimSpace(ctx.PathValue("id"))
		return operations.ReworkTask(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/task/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.TaskForRun(r.Context(), bearerToken(r), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/inbox", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.Inbox(r.Context(), bearerToken(r))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/inbox/{id}", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		result, err := operations.InboxMessage(r.Context(), bearerToken(r), strings.TrimSpace(r.PathValue("id")))
		writeResult(w, result, err)
	}))
	mux.HandleFunc("/v1/inbox/ack", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.AcknowledgeMessagesInput) (any, error) {
		input.Token = ctx.Token
		return operations.AcknowledgeAgentMessages(ctx.Context, input)
	})))
	mux.HandleFunc("/v1/progress", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.ProgressInput) (any, error) {
		input.Token = ctx.Token
		fields := perfobs.Fields{RequestID: input.RequestID}
		perfobs.Received("api.progress.received", fields, "received")
		result, err := operations.Progress(ctx.Context, input)
		if err != nil {
			perfobs.FailedReceived("api.progress.received", fields)
		}
		return result, err
	})))
	mux.HandleFunc("/v1/message", requireMethod(http.MethodPost, decodeCall(func(ctx requestContext, input core.SendMessageInput) (any, error) {
		input.Token = ctx.Token
		return operations.SendAgentMessage(ctx.Context, input)
	})))
	mux.HandleFunc("/", notFound)
	return mux
}

// NewScopedRunHandler binds the fixed Agent surface to one expected Run. The
// Core operation performs its normal authorization again, preserving token
// revocation and generation fencing across the preflight/operation race.
func NewScopedRunHandler(operations ScopedRunOperations, expected core.RunScope) http.Handler {
	if operations == nil {
		return unavailableHandler("scoped run operations are required")
	}
	next := NewRunHandler(operations)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := operations.AuthorizeRunScope(r.Context(), bearerToken(r), expected); err != nil {
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type requestContext struct {
	Context   context.Context
	Token     string
	PathValue func(string) string
}

func decodeCall[T any](call func(requestContext, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input T
		if err := decodeJSON(w, r, &input, false); err != nil {
			writeError(w, err)
			return
		}
		result, err := call(newRequestContext(r), input)
		writeResult(w, result, err)
	}
}

func actionCall(call func(requestContext, string, string) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input actionRequest
		if err := decodeJSON(w, r, &input, true); err != nil {
			writeError(w, err)
			return
		}
		result, err := call(newRequestContext(r), strings.TrimSpace(r.PathValue("id")), input.RequestID)
		writeResult(w, result, err)
	}
}

func newRequestContext(r *http.Request) requestContext {
	return requestContext{Context: r.Context(), Token: bearerToken(r), PathValue: r.PathValue}
}

func requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			methodNotAllowed(w, method)
			return
		}
		next(w, r)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeErrorStatus(w, http.StatusMethodNotAllowed, core.NewError(core.CodeInvalidArgument, "method not allowed", false))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any, optional bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return nil
		}
		return core.NewError(core.CodeInvalidArgument, fmt.Sprintf("invalid JSON request: %v", err), false)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return core.NewError(core.CodeInvalidArgument, "invalid JSON request: trailing content", false)
	}
	return nil
}

func queryInt(raw, name string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, core.NewError(core.CodeInvalidArgument, name+" must be a non-negative integer", false)
	}
	return value, nil
}

func bearerToken(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeErrorStatus(w, http.StatusNotFound, core.NewError(core.CodeNotFound, "route not found", false))
}

func unavailableHandler(message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErrorStatus(w, http.StatusInternalServerError, core.NewError(core.CodeInternal, message, false))
	})
}
