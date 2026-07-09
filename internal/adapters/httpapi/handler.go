package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"coordplane/internal/capability"
)

type Dispatcher interface {
	Handle(context.Context, capability.Call) capability.Response[json.RawMessage]
	ListForSubject(context.Context, capability.Subject) capability.Response[json.RawMessage]
}

type CallAuthenticator interface {
	AuthenticateCall(context.Context, *http.Request, capability.Call) (capability.Call, capability.Response[json.RawMessage])
}

type SubjectAuthenticator interface {
	AuthenticateSubject(context.Context, *http.Request, capability.Subject) (capability.Subject, capability.Response[json.RawMessage])
}

type Handler struct {
	dispatcher    Dispatcher
	authenticator CallAuthenticator
}

func New(dispatcher Dispatcher) *Handler {
	return &Handler{dispatcher: dispatcher}
}

func NewWithAuthenticator(dispatcher Dispatcher, authenticator CallAuthenticator) *Handler {
	return &Handler{dispatcher: dispatcher, authenticator: authenticator}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/call":
		h.handleCall(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/capabilities":
		h.handleList(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleCall(w http.ResponseWriter, r *http.Request) {
	var call capability.Call
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		writeResponse(w, capability.Error[json.RawMessage](
			"INVALID_CALL_REQUEST",
			"request body must be a JSON capability call",
			false,
		))
		return
	}
	if h.authenticator != nil {
		authenticated, response := h.authenticator.AuthenticateCall(r.Context(), r, call)
		if response.Status != "" {
			writeResponse(w, response)
			return
		}
		call = authenticated
	}
	writeResponse(w, h.dispatcher.Handle(r.Context(), call))
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromRequest(r)
	if authenticator, ok := h.authenticator.(SubjectAuthenticator); ok {
		authenticated, response := authenticator.AuthenticateSubject(r.Context(), r, subject)
		if response.Status != "" {
			writeResponse(w, response)
			return
		}
		subject = authenticated
	}
	writeResponse(w, h.dispatcher.ListForSubject(r.Context(), subject))
}

func writeResponse(w http.ResponseWriter, response capability.Response[json.RawMessage]) {
	w.Header().Set("Content-Type", "application/json")
	switch response.Status {
	case capability.StatusAccepted:
		w.WriteHeader(http.StatusOK)
	case capability.StatusRejected:
		w.WriteHeader(http.StatusBadRequest)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(response)
}

func subjectFromRequest(r *http.Request) capability.Subject {
	agentID := r.URL.Query().Get("agent_id")
	if agentID == "" {
		agentID = r.Header.Get("X-CoordPlane-Agent-ID")
	}
	runtimeID := r.URL.Query().Get("runtime_id")
	if runtimeID == "" {
		runtimeID = r.Header.Get("X-CoordPlane-Runtime-ID")
	}
	workspaceID := r.URL.Query().Get("workspace_id")
	if workspaceID == "" {
		workspaceID = r.Header.Get("X-CoordPlane-Workspace-ID")
	}
	return capability.Subject{
		Kind:        "agent",
		ID:          agentID,
		AgentID:     agentID,
		RuntimeID:   runtimeID,
		WorkspaceID: workspaceID,
	}
}
