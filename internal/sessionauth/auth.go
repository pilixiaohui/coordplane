package sessionauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"coordplane/internal/capability"
	cpruntime "coordplane/internal/runtime"
)

type Authenticator struct {
	db        *sql.DB
	protected map[string]bool
	allCalls  bool
}

type SessionBinding struct {
	AgentID      string
	RuntimeID    string
	AttemptID    string
	LeaseID      string
	AssignmentID string
}

func New(db *sql.DB, protectedCapabilities ...string) *Authenticator {
	protected := make(map[string]bool, len(protectedCapabilities))
	for _, name := range protectedCapabilities {
		if name != "" {
			protected[name] = true
		}
	}
	return &Authenticator{db: db, protected: protected}
}

func NewAll(db *sql.DB) *Authenticator {
	return &Authenticator{db: db, allCalls: true}
}

func (a *Authenticator) SetDB(db *sql.DB) {
	if a != nil {
		a.db = db
	}
}

func (a *Authenticator) AuthenticateCall(ctx context.Context, r *http.Request, call capability.Call) (capability.Call, capability.Response[json.RawMessage]) {
	if a == nil || !a.protects(call.CapabilityName) {
		return call, capability.Response[json.RawMessage]{}
	}
	token, response := bearerToken(r)
	if response.Status != "" {
		return call, response
	}
	binding, response := a.bindingForToken(ctx, token)
	if response.Status != "" {
		return call, response
	}
	if response := checkSubjectConsistency(r, call.Subject, binding); response.Status != "" {
		return call, response
	}
	scope, response := canonicalScope(call.Scope, binding.LeaseID)
	if response.Status != "" {
		return call, response
	}
	input, response := canonicalInput(call.Input, binding.LeaseID)
	if response.Status != "" {
		return call, response
	}
	call.Subject = capability.Subject{
		Kind:      "agent",
		ID:        binding.AgentID,
		AgentID:   binding.AgentID,
		RuntimeID: binding.RuntimeID,
	}
	call.Scope = scope
	call.Input = input
	return call, capability.Response[json.RawMessage]{}
}

func (a *Authenticator) protects(capabilityName string) bool {
	if a == nil {
		return false
	}
	if a.allCalls {
		return true
	}
	return a.protected[capabilityName]
}

func (a *Authenticator) AuthenticateSubject(ctx context.Context, r *http.Request, subject capability.Subject) (capability.Subject, capability.Response[json.RawMessage]) {
	token, response := bearerToken(r)
	if response.Status != "" {
		return capability.Subject{}, response
	}
	binding, response := a.discoveryBindingForToken(ctx, token)
	if response.Status != "" {
		return capability.Subject{}, response
	}
	if response := checkSubjectConsistency(r, subject, binding); response.Status != "" {
		return capability.Subject{}, response
	}
	return capability.Subject{
		Kind:      "agent",
		ID:        binding.AgentID,
		AgentID:   binding.AgentID,
		RuntimeID: binding.RuntimeID,
	}, capability.Response[json.RawMessage]{}
}

func (a *Authenticator) bindingForToken(ctx context.Context, token string) (SessionBinding, capability.Response[json.RawMessage]) {
	if a.db == nil {
		return SessionBinding{}, authError("AUTH_STORE_UNAVAILABLE", "runtime token store is unavailable", true)
	}
	var out SessionBinding
	err := a.db.QueryRowContext(ctx, `
SELECT rt.agent_id, rt.runtime_id, rt.attempt_id, rt.lease_id, l.assignment_id
FROM runtime_tokens rt
JOIN runtime_instances ri
  ON ri.runtime_id = rt.runtime_id
 AND ri.attempt_id = rt.attempt_id
 AND ri.lease_id = rt.lease_id
 AND ri.agent_id = rt.agent_id
JOIN attempts a ON a.id = rt.attempt_id AND a.lease_id = rt.lease_id
JOIN leases l ON l.id = rt.lease_id AND l.agent_id = rt.agent_id
JOIN session_routes sr
  ON sr.id = l.session_route_id
 AND sr.runtime_id = rt.runtime_id
 AND sr.agent_id = rt.agent_id
WHERE rt.token_hash = ?
  AND rt.state = 'active'
  AND ri.state = 'ready'
  AND a.status = 'running'
  AND l.state = 'active'
  AND sr.state = 'active'
ORDER BY rt.updated_at DESC
LIMIT 1`, cpruntime.RuntimeTokenHash(token)).Scan(
		&out.AgentID,
		&out.RuntimeID,
		&out.AttemptID,
		&out.LeaseID,
		&out.AssignmentID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionBinding{}, authRejected(
			"AUTH_TOKEN_REJECTED",
			"authorization token is not bound to an active runtime session",
			"retry from the coordlink process inside the active agent runtime",
		)
	}
	if err != nil {
		return SessionBinding{}, authError("AUTH_TOKEN_LOOKUP_FAILED", err.Error(), true)
	}
	return out, capability.Response[json.RawMessage]{}
}

func (a *Authenticator) discoveryBindingForToken(ctx context.Context, token string) (SessionBinding, capability.Response[json.RawMessage]) {
	if a.db == nil {
		return SessionBinding{}, authError("AUTH_STORE_UNAVAILABLE", "runtime token store is unavailable", true)
	}
	var out SessionBinding
	err := a.db.QueryRowContext(ctx, `
SELECT rt.agent_id, rt.runtime_id, rt.attempt_id, rt.lease_id, l.assignment_id
FROM runtime_tokens rt
JOIN runtime_instances ri
  ON ri.runtime_id = rt.runtime_id
 AND ri.attempt_id = rt.attempt_id
 AND ri.lease_id = rt.lease_id
 AND ri.agent_id = rt.agent_id
JOIN attempts a ON a.id = rt.attempt_id AND a.lease_id = rt.lease_id
JOIN leases l ON l.id = rt.lease_id AND l.agent_id = rt.agent_id
WHERE rt.token_hash = ?
  AND rt.state = 'active'
  AND ri.state IN ('preparing', 'ready')
  AND a.status IN ('preparing', 'ready_to_launch', 'running')
  AND l.state = 'active'
ORDER BY rt.updated_at DESC
LIMIT 1`, cpruntime.RuntimeTokenHash(token)).Scan(
		&out.AgentID,
		&out.RuntimeID,
		&out.AttemptID,
		&out.LeaseID,
		&out.AssignmentID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionBinding{}, authRejected(
			"AUTH_TOKEN_REJECTED",
			"authorization token is not bound to a runtime session",
			"retry from the coordlink process inside the active agent runtime",
		)
	}
	if err != nil {
		return SessionBinding{}, authError("AUTH_TOKEN_LOOKUP_FAILED", err.Error(), true)
	}
	return out, capability.Response[json.RawMessage]{}
}

func bearerToken(r *http.Request) (string, capability.Response[json.RawMessage]) {
	header := ""
	if r != nil {
		header = strings.TrimSpace(r.Header.Get("Authorization"))
	}
	if header == "" {
		return "", authRejected(
			"AUTH_TOKEN_REQUIRED",
			"Authorization bearer token is required",
			"retry from coordlink with COORDPLANE_TOKEN from the active runtime environment",
		)
	}
	prefix := "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", authRejected(
			"AUTH_TOKEN_REQUIRED",
			"Authorization must use Bearer token syntax",
			"retry with Authorization: Bearer <runtime token>",
		)
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", authRejected(
			"AUTH_TOKEN_REQUIRED",
			"Authorization bearer token is empty",
			"retry with the runtime token injected into this session",
		)
	}
	return token, capability.Response[json.RawMessage]{}
}

func checkSubjectConsistency(r *http.Request, subject capability.Subject, binding SessionBinding) capability.Response[json.RawMessage] {
	if subject.Kind != "" && subject.Kind != "agent" {
		return authRejected("AUTH_SUBJECT_MISMATCH", "request subject kind does not match authenticated runtime identity", "remove forged subject fields and retry from coordlink")
	}
	for label, value := range map[string]string{
		"subject.agent_id": firstNonEmpty(subject.AgentID, subject.ID),
		"header.agent_id":  headerValue(r, "X-CoordPlane-Agent-ID"),
		"query.agent_id":   queryValue(r, "agent_id"),
	} {
		if value != "" && value != binding.AgentID {
			return authRejected("AUTH_SUBJECT_MISMATCH", fmt.Sprintf("%s does not match authenticated runtime identity", label), "retry with the agent identity injected into the runtime")
		}
	}
	for label, value := range map[string]string{
		"subject.runtime_id": subject.RuntimeID,
		"header.runtime_id":  headerValue(r, "X-CoordPlane-Runtime-ID"),
		"query.runtime_id":   queryValue(r, "runtime_id"),
	} {
		if value != "" && value != binding.RuntimeID {
			return authRejected("AUTH_SUBJECT_MISMATCH", fmt.Sprintf("%s does not match authenticated runtime identity", label), "retry with the runtime identity injected into the runtime")
		}
	}
	return capability.Response[json.RawMessage]{}
}

func canonicalScope(raw json.RawMessage, leaseID string) (json.RawMessage, capability.Response[json.RawMessage]) {
	values, response := decodeObject(raw, "scope")
	if response.Status != "" {
		return nil, response
	}
	if value, ok := stringField(values, "lease_id"); ok && value != "" && value != leaseID {
		return nil, authRejected("AUTH_SCOPE_MISMATCH", "scope lease_id does not match authenticated runtime session", "retry with the lease id injected into the runtime")
	}
	values["lease_id"] = leaseID
	out, err := json.Marshal(values)
	if err != nil {
		return nil, authError("AUTH_SCOPE_ENCODE_FAILED", err.Error(), false)
	}
	return json.RawMessage(out), capability.Response[json.RawMessage]{}
}

func canonicalInput(raw json.RawMessage, leaseID string) (json.RawMessage, capability.Response[json.RawMessage]) {
	values, response := decodeObject(raw, "input")
	if response.Status != "" {
		return nil, response
	}
	if value, ok := stringField(values, "lease_id"); ok && value != "" && value != leaseID {
		return nil, authRejected("AUTH_SCOPE_MISMATCH", "input lease_id does not match authenticated runtime session", "retry with the lease id injected into the runtime")
	}
	values["lease_id"] = leaseID
	out, err := json.Marshal(values)
	if err != nil {
		return nil, authError("AUTH_INPUT_ENCODE_FAILED", err.Error(), false)
	}
	return json.RawMessage(out), capability.Response[json.RawMessage]{}
}

func decodeObject(raw json.RawMessage, label string) (map[string]any, capability.Response[json.RawMessage]) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, capability.Response[json.RawMessage]{}
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, authRejected("INVALID_AUTHENTICATED_CALL", fmt.Sprintf("%s must be a JSON object", label), "retry with JSON object scope/input")
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, capability.Response[json.RawMessage]{}
}

func stringField(values map[string]any, key string) (string, bool) {
	raw, ok := values[key]
	if !ok {
		return "", false
	}
	value, ok := raw.(string)
	if !ok {
		return "", true
	}
	return strings.TrimSpace(value), true
}

func headerValue(r *http.Request, key string) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(key))
}

func queryValue(r *http.Request, key string) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return strings.TrimSpace(r.URL.Query().Get(key))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func authRejected(code, message, repair string) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		code,
		message,
		capability.WithRepairHint(repair),
		capability.WithAllowedNextActions("capability.list"),
		capability.WithRetryable(false),
	)
}

func authError(code, message string, retryable bool) capability.Response[json.RawMessage] {
	return capability.Error[json.RawMessage](code, message, retryable)
}
