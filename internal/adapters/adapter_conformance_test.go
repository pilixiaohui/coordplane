package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/adapters/httpapi"
	"coordplane/internal/capability"
	"coordplane/internal/policy"
	"coordplane/internal/teamconfig"
)

func TestHTTPAndCoordlinkAdaptersUseSameCapabilityHandler(t *testing.T) {
	var handled int32
	dispatcher := newConformanceDispatcher(t, func(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
		atomic.AddInt32(&handled, 1)
		return capability.AcceptedRaw(json.RawMessage(`{"handled_by":"registry","contract_id":"ctr_123"}`))
	})

	call := conformanceCall(json.RawMessage(`{"contract_id":"ctr_123"}`))
	httpEnvelope, httpStatus := callHTTPAdapter(t, dispatcher, call)
	coordEnvelope := callCoordlinkAdapter(t, dispatcher, call)

	if httpStatus != http.StatusOK {
		t.Fatalf("http status = %d, want 200", httpStatus)
	}
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("adapter responses differ\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	if atomic.LoadInt32(&handled) != 2 {
		t.Fatalf("handler calls = %d, want both adapters to use registry handler", handled)
	}
	if httpEnvelope["status"] != "accepted" || httpEnvelope["ok"] != true {
		t.Fatalf("typed accepted envelope = %#v", httpEnvelope)
	}
}

func TestHTTPAndCoordlinkAdaptersReturnSameRejectedResponse(t *testing.T) {
	dispatcher := newConformanceDispatcher(t, func(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
		return capability.Rejected[json.RawMessage](
			"MISSING_REQUIRED_EVIDENCE",
			"contract.complete requires one report evidence before this contract can be satisfied",
			capability.WithCanonicalID("contract_id", "ctr_123"),
			capability.WithMissing("report", "report.submit"),
			capability.WithRepairHint("submit report evidence, then retry contract.complete"),
			capability.WithAllowedNextActions("report.submit", "contract.context"),
			capability.WithRetryable(true),
		)
	})

	call := conformanceCall(json.RawMessage(`{"contract_id":"ctr_123"}`))
	httpEnvelope, httpStatus := callHTTPAdapter(t, dispatcher, call)
	coordEnvelope := callCoordlinkAdapter(t, dispatcher, call)

	if httpStatus != http.StatusBadRequest {
		t.Fatalf("http status = %d, want 400 for rejected response", httpStatus)
	}
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("rejected adapter responses differ\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	if httpEnvelope["status"] != "rejected" || httpEnvelope["ok"] != false {
		t.Fatalf("typed rejected envelope = %#v", httpEnvelope)
	}
	if httpEnvelope["error_code"] != "MISSING_REQUIRED_EVIDENCE" || httpEnvelope["retryable"] != true {
		t.Fatalf("repairable rejected fields = %#v", httpEnvelope)
	}
	if httpEnvelope["message"] != "contract.complete requires one report evidence before this contract can be satisfied" {
		t.Fatalf("message not preserved: %#v", httpEnvelope)
	}
	if httpEnvelope["repair_hint"] != "submit report evidence, then retry contract.complete" {
		t.Fatalf("repair_hint not preserved: %#v", httpEnvelope)
	}
	canonicalIDs := objectField(t, httpEnvelope, "canonical_ids")
	if canonicalIDs["contract_id"] != "ctr_123" {
		t.Fatalf("canonical_ids not preserved: %#v", canonicalIDs)
	}
	missing := arrayField(t, httpEnvelope, "missing")
	if len(missing) != 1 {
		t.Fatalf("missing length = %d, want 1: %#v", len(missing), missing)
	}
	missingRequirement, ok := missing[0].(map[string]any)
	if !ok {
		t.Fatalf("missing[0] = %#v, want object", missing[0])
	}
	if missingRequirement["kind"] != "report" || missingRequirement["action"] != "report.submit" {
		t.Fatalf("missing requirement not preserved: %#v", missingRequirement)
	}
	actions := arrayField(t, httpEnvelope, "allowed_next_actions")
	if !reflect.DeepEqual(actions, []any{"report.submit", "contract.context"}) {
		t.Fatalf("allowed_next_actions not preserved: %#v", actions)
	}
}

func TestInvalidHandlerResponseIsConvertedToTypedErrorThroughAllAdapters(t *testing.T) {
	dispatcher := newConformanceDispatcher(t, func(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
		return capability.Response[json.RawMessage]{
			OK:     false,
			Status: capability.StatusAccepted,
		}
	})

	call := conformanceCall(json.RawMessage(`{"contract_id":"ctr_123"}`))
	httpEnvelope, httpStatus := callHTTPAdapter(t, dispatcher, call)
	coordEnvelope := callCoordlinkAdapter(t, dispatcher, call)

	if httpStatus != http.StatusInternalServerError {
		t.Fatalf("http status = %d, want 500 for invalid handler response", httpStatus)
	}
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("invalid-response adapter envelopes differ\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	if httpEnvelope["ok"] != false || httpEnvelope["status"] != "error" {
		t.Fatalf("typed error envelope = %#v", httpEnvelope)
	}
	if httpEnvelope["error_code"] != "INVALID_CAPABILITY_RESPONSE" {
		t.Fatalf("error_code = %v, want INVALID_CAPABILITY_RESPONSE", httpEnvelope["error_code"])
	}
	if _, ok := httpEnvelope["data"]; ok {
		t.Fatalf("invalid handler response must not include accepted data: %#v", httpEnvelope)
	}
}

func TestCapabilityListIsServedThroughBothAdapters(t *testing.T) {
	dispatcher := newConformanceDispatcher(t, func(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
		return capability.AcceptedRaw(json.RawMessage(`{"ok":true}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/capabilities?agent_id=agent_builder", nil)
	rec := httptest.NewRecorder()
	httpapi.New(dispatcher).ServeHTTP(rec, req)

	var httpEnvelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &httpEnvelope); err != nil {
		t.Fatalf("decode http list response: %v", err)
	}

	coordResp := coordlink.New(dispatcher).List(context.Background(), conformanceSubject())
	coordEnvelope := marshalEnvelope(t, coordResp)
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("list adapter responses differ\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	definitions := definitionsFromEnvelope(t, httpEnvelope)
	if len(definitions) != 1 {
		t.Fatalf("definition count = %d, want 1", len(definitions))
	}
	definition := definitions[0]
	expectedKeys := map[string]bool{
		"name":            true,
		"input_schema":    true,
		"output_schema":   true,
		"rejected_schema": true,
		"side_effect":     true,
		"required_scope":  true,
		"idempotency":     true,
		"skill_refs":      true,
	}
	for key := range definition {
		if !expectedKeys[key] {
			t.Fatalf("public discovery leaked unexpected field %q in %#v", key, definition)
		}
	}
	for _, forbidden := range []string{"handler", "private", "wrapper"} {
		if _, ok := definition[forbidden]; ok {
			t.Fatalf("public discovery leaked %q field: %#v", forbidden, definition)
		}
	}
	if definition["name"] != "contract.complete" || definition["required_scope"] != "agent_lease" {
		t.Fatalf("definition identity fields = %#v", definition)
	}
	if !reflect.DeepEqual(objectField(t, definition, "input_schema"), map[string]any{"type": "object"}) {
		t.Fatalf("input_schema not preserved: %#v", definition["input_schema"])
	}
	if !reflect.DeepEqual(objectField(t, definition, "output_schema"), map[string]any{"type": "object"}) {
		t.Fatalf("output_schema not preserved: %#v", definition["output_schema"])
	}
	if !reflect.DeepEqual(objectField(t, definition, "rejected_schema"), map[string]any{"type": "object"}) {
		t.Fatalf("rejected_schema not preserved: %#v", definition["rejected_schema"])
	}
	if !reflect.DeepEqual(arrayField(t, definition, "skill_refs"), []any{"coordplane-service"}) {
		t.Fatalf("skill_refs not preserved: %#v", definition["skill_refs"])
	}
}

func TestAdapterDiscoverySchemaMatchesRegistryDefinition(t *testing.T) {
	registry := capability.NewRegistry()
	definition := capability.Definition{
		Name:           "communication.read",
		InputSchema:    json.RawMessage(`{"type":"object","properties":{"envelope_id":{"type":"string"}}}`),
		OutputSchema:   json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"}}}`),
		RejectedSchema: json.RawMessage(`{"type":"object","properties":{"error_code":{"type":"string"}}}`),
		SideEffect:     capability.SideEffectRead,
		RequiredScope:  "agent",
		Idempotency:    false,
		SkillRefs:      []string{"coordplane-service"},
	}
	if err := registry.Register(definition, func(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
		return capability.AcceptedRaw(json.RawMessage(`{"kind":"message"}`))
	}); err != nil {
		t.Fatalf("register communication.read: %v", err)
	}
	dispatcher := policy.NewDispatcher(teamconfig.Config{
		TeamID:  "adapter-schema-test",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "agent_builder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   []string{"communication.read"},
			},
		},
	}, registry)

	httpEnvelope, httpStatus := listHTTPAdapter(t, dispatcher, "agent_builder")
	if httpStatus != http.StatusOK {
		t.Fatalf("http status = %d, want 200", httpStatus)
	}
	coordEnvelope := marshalEnvelope(t, coordlink.New(dispatcher).List(context.Background(), conformanceSubject()))
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("adapter discovery differs\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	definitions := definitionsFromEnvelope(t, httpEnvelope)
	registryDefs := registry.List()
	if len(definitions) != 1 || len(registryDefs) != 1 {
		t.Fatalf("definition lengths = adapter:%d registry:%d", len(definitions), len(registryDefs))
	}
	adapterRaw, err := json.Marshal(definitions[0])
	if err != nil {
		t.Fatalf("marshal adapter definition: %v", err)
	}
	registryRaw, err := json.Marshal(registryDefs[0])
	if err != nil {
		t.Fatalf("marshal registry definition: %v", err)
	}
	var adapterDefinition, registryDefinition map[string]any
	if err := json.Unmarshal(adapterRaw, &adapterDefinition); err != nil {
		t.Fatalf("decode adapter definition: %v", err)
	}
	if err := json.Unmarshal(registryRaw, &registryDefinition); err != nil {
		t.Fatalf("decode registry definition: %v", err)
	}
	if !reflect.DeepEqual(adapterDefinition, registryDefinition) {
		t.Fatalf("adapter schema drifted from registry\nadapter: %#v\nregistry:%#v", adapterDefinition, registryDefinition)
	}
}

func TestPublicDiscoveryIsScopedByTeamConfigThroughHTTPAndCoordlink(t *testing.T) {
	registry := capability.NewRegistry()
	for _, definition := range []capability.Definition{
		{Name: "contract.current", SideEffect: capability.SideEffectRead, RequiredScope: "agent_lease"},
		{Name: "contract.add", SideEffect: capability.SideEffectWrite, RequiredScope: "agent_lease"},
		{Name: "session.steer", SideEffect: capability.SideEffectWrite, RequiredScope: "runner_internal"},
	} {
		if err := registry.Register(definition, func(context.Context, capability.Call) capability.Response[json.RawMessage] {
			return capability.AcceptedRaw(json.RawMessage(`{"ok":true}`))
		}); err != nil {
			t.Fatalf("register %s: %v", definition.Name, err)
		}
	}
	dispatcher := policy.NewDispatcher(teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "builder",
				RolePrompt:     "Renamed role prompt does not change policy.",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   []string{"contract.current"},
			},
		},
	}, registry)

	httpEnvelope, httpStatus := listHTTPAdapter(t, dispatcher, "builder")
	coordEnvelope := marshalEnvelope(t, coordlink.New(dispatcher).List(context.Background(), capability.Subject{
		Kind:    "agent",
		ID:      "builder",
		AgentID: "builder",
	}))
	if httpStatus != http.StatusOK {
		t.Fatalf("http status = %d, want 200", httpStatus)
	}
	if !reflect.DeepEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("scoped list differs\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	definitions := definitionsFromEnvelope(t, httpEnvelope)
	if len(definitions) != 1 || definitions[0]["name"] != "contract.current" {
		t.Fatalf("scoped definitions = %#v, want only contract.current", definitions)
	}

	unknownHTTP, unknownStatus := listHTTPAdapter(t, dispatcher, "intruder")
	if unknownStatus != http.StatusBadRequest {
		t.Fatalf("unknown-agent http status = %d, want 400", unknownStatus)
	}
	if unknownHTTP["status"] != "rejected" || unknownHTTP["error_code"] != "UNAUTHORIZED_CAPABILITY_DISCOVERY" {
		t.Fatalf("unknown-agent envelope = %#v", unknownHTTP)
	}
	if _, ok := unknownHTTP["data"]; ok {
		t.Fatalf("unknown agent received data: %#v", unknownHTTP)
	}
	unknownCoord := marshalEnvelope(t, coordlink.New(dispatcher).List(context.Background(), capability.Subject{
		Kind:    "agent",
		ID:      "intruder",
		AgentID: "intruder",
	}))
	if !reflect.DeepEqual(unknownHTTP, unknownCoord) {
		t.Fatalf("unknown-agent envelopes differ\nhttp:     %#v\ncoordlink:%#v", unknownHTTP, unknownCoord)
	}
}

func newConformanceDispatcher(t *testing.T, handler capability.HandlerFunc) *policy.Dispatcher {
	t.Helper()
	registry := capability.NewRegistry()
	if err := registry.Register(capability.Definition{
		Name:           "contract.complete",
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		RejectedSchema: json.RawMessage(`{"type":"object"}`),
		SideEffect:     capability.SideEffectWrite,
		RequiredScope:  "agent_lease",
		Idempotency:    true,
		SkillRefs:      []string{"coordplane-service"},
	}, handler); err != nil {
		t.Fatalf("register capability: %v", err)
	}
	return policy.NewDispatcher(teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "agent_builder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   []string{"contract.complete"},
			},
		},
	}, registry)
}

func conformanceCall(input json.RawMessage) capability.Call {
	return capability.Call{
		CapabilityName: "contract.complete",
		TraceID:        "trace_123",
		IdempotencyKey: "complete_ctr_123",
		Subject: capability.Subject{
			TenantID: "default",
			Kind:     "agent",
			ID:       "agent_builder",
			AgentID:  "agent_builder",
		},
		Scope: json.RawMessage(`{"lease_id":"lease_123"}`),
		Input: input,
	}
}

func conformanceSubject() capability.Subject {
	return capability.Subject{
		TenantID: "default",
		Kind:     "agent",
		ID:       "agent_builder",
		AgentID:  "agent_builder",
	}
}

func callHTTPAdapter(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) (map[string]any, int) {
	t.Helper()
	raw, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/call", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	httpapi.New(dispatcher).ServeHTTP(rec, req)

	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode http response: %v; body=%s", err, rec.Body.String())
	}
	return envelope, rec.Code
}

func callCoordlinkAdapter(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) map[string]any {
	t.Helper()
	response := coordlink.New(dispatcher).Call(context.Background(), call)
	return marshalEnvelope(t, response)
}

func listHTTPAdapter(t *testing.T, dispatcher *policy.Dispatcher, agentID string) (map[string]any, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/capabilities?agent_id="+agentID, nil)
	rec := httptest.NewRecorder()
	httpapi.New(dispatcher).ServeHTTP(rec, req)
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list response: %v; body=%s", err, rec.Body.String())
	}
	return envelope, rec.Code
}

func marshalEnvelope(t *testing.T, response capability.Response[json.RawMessage]) map[string]any {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return envelope
}

func definitionsFromEnvelope(t *testing.T, envelope map[string]any) []map[string]any {
	t.Helper()
	data := arrayField(t, envelope, "data")
	definitions := make([]map[string]any, 0, len(data))
	for _, item := range data {
		definition, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("definition item = %#v, want object", item)
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func objectField(t *testing.T, envelope map[string]any, field string) map[string]any {
	t.Helper()
	value, ok := envelope[field].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", field, envelope[field])
	}
	return value
}

func arrayField(t *testing.T, envelope map[string]any, field string) []any {
	t.Helper()
	value, ok := envelope[field].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", field, envelope[field])
	}
	return value
}
