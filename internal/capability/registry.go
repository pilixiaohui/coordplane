package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type SideEffect string

const (
	SideEffectNone         SideEffect = "none"
	SideEffectRead         SideEffect = "read"
	SideEffectWrite        SideEffect = "write"
	SideEffectExternalExec SideEffect = "external_exec"
)

type Subject struct {
	TenantID    string `json:"tenant_id,omitempty"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	AgentID     string `json:"agent_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

type Call struct {
	CapabilityName string          `json:"capability"`
	TraceID        string          `json:"trace_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Subject        Subject         `json:"subject"`
	Scope          json.RawMessage `json:"scope,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
}

type Definition struct {
	Name           string          `json:"name"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema   json.RawMessage `json:"output_schema,omitempty"`
	RejectedSchema json.RawMessage `json:"rejected_schema,omitempty"`
	SideEffect     SideEffect      `json:"side_effect"`
	RequiredScope  string          `json:"required_scope"`
	Idempotency    bool            `json:"idempotency"`
	SkillRefs      []string        `json:"skill_refs,omitempty"`
}

type HandlerFunc func(context.Context, Call) Response[json.RawMessage]

type Registry struct {
	capabilities map[string]registeredCapability
}

type registeredCapability struct {
	definition Definition
	handler    HandlerFunc
}

func NewRegistry() *Registry {
	return &Registry{capabilities: make(map[string]registeredCapability)}
}

func (r *Registry) Register(definition Definition, handler HandlerFunc) error {
	if r == nil {
		return errors.New("capability registry is nil")
	}
	if r.capabilities == nil {
		r.capabilities = make(map[string]registeredCapability)
	}
	if err := definition.Validate(); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("capability %q handler is nil", definition.Name)
	}
	if _, exists := r.capabilities[definition.Name]; exists {
		return fmt.Errorf("capability %q already registered", definition.Name)
	}
	r.capabilities[definition.Name] = registeredCapability{
		definition: cloneDefinition(definition),
		handler:    handler,
	}
	return nil
}

func (r *Registry) Handle(ctx context.Context, call Call) Response[json.RawMessage] {
	if r == nil {
		return Error[json.RawMessage]("CAPABILITY_REGISTRY_UNAVAILABLE", "capability registry is not configured", true)
	}
	registered, ok := r.capabilities[call.CapabilityName]
	if !ok {
		return Rejected[json.RawMessage](
			"UNKNOWN_CAPABILITY",
			fmt.Sprintf("capability %q is not registered", call.CapabilityName),
			WithRepairHint("call capability.list and retry with a registered capability name"),
			WithAllowedNextActions("capability.list"),
			WithRetryable(false),
		)
	}
	response := registered.handler(ctx, call)
	if err := response.Validate(); err != nil {
		return Error[json.RawMessage](
			"INVALID_CAPABILITY_RESPONSE",
			fmt.Sprintf("capability %q returned an invalid typed response: %v", call.CapabilityName, err),
			false,
		)
	}
	return response
}

func (r *Registry) List() []Definition {
	if r == nil || len(r.capabilities) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.capabilities))
	for name := range r.capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Definition, 0, len(names))
	for _, name := range names {
		out = append(out, cloneDefinition(r.capabilities[name].definition))
	}
	return out
}

func (d Definition) Validate() error {
	if d.Name == "" {
		return errors.New("capability name is required")
	}
	if d.SideEffect == "" {
		return fmt.Errorf("capability %q side_effect is required", d.Name)
	}
	switch d.SideEffect {
	case SideEffectNone, SideEffectRead, SideEffectWrite, SideEffectExternalExec:
	default:
		return fmt.Errorf("capability %q side_effect %q is invalid", d.Name, d.SideEffect)
	}
	if d.RequiredScope == "" {
		return fmt.Errorf("capability %q required_scope is required", d.Name)
	}
	return nil
}

func cloneDefinition(definition Definition) Definition {
	cloned := definition
	cloned.InputSchema = cloneRaw(definition.InputSchema)
	cloned.OutputSchema = cloneRaw(definition.OutputSchema)
	cloned.RejectedSchema = cloneRaw(definition.RejectedSchema)
	cloned.SkillRefs = append([]string(nil), definition.SkillRefs...)
	return cloned
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
