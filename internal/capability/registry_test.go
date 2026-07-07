package capability_test

import (
	"context"
	"encoding/json"
	"testing"

	"coordplane/internal/capability"
)

func TestRegistryRejectsUnknownCapabilityWithRepairableResponse(t *testing.T) {
	registry := capability.NewRegistry()

	resp := registry.Handle(context.Background(), capability.Call{CapabilityName: "missing.capability"})
	if err := resp.Validate(); err != nil {
		t.Fatalf("validate response: %v", err)
	}
	if resp.Status != capability.StatusRejected {
		t.Fatalf("status = %s, want rejected", resp.Status)
	}
	if resp.ErrorCode != "UNKNOWN_CAPABILITY" {
		t.Fatalf("error_code = %s, want UNKNOWN_CAPABILITY", resp.ErrorCode)
	}
	if resp.Retryable == nil || *resp.Retryable {
		t.Fatalf("retryable = %v, want false", resp.Retryable)
	}
	if len(resp.AllowedNextActions) != 1 || resp.AllowedNextActions[0] != "capability.list" {
		t.Fatalf("allowed_next_actions = %v, want capability.list", resp.AllowedNextActions)
	}
}

func TestRegistryReturnsDefinitionsWithoutHandlerState(t *testing.T) {
	registry := capability.NewRegistry()
	inputSchema := json.RawMessage(`{"type":"object","properties":{"contract_id":{"type":"string"}}}`)
	outputSchema := json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}}}`)
	rejectedSchema := json.RawMessage(`{"type":"object","required":["error_code","message","retryable"]}`)
	skillRefs := []string{"coordplane-service"}
	if err := registry.Register(capability.Definition{
		Name:           "contract.current",
		InputSchema:    inputSchema,
		OutputSchema:   outputSchema,
		RejectedSchema: rejectedSchema,
		SideEffect:     capability.SideEffectRead,
		RequiredScope:  "agent_lease",
		SkillRefs:      skillRefs,
	}, func(context.Context, capability.Call) capability.Response[json.RawMessage] {
		return capability.AcceptedRaw(json.RawMessage(`{"contract_id":"ctr_1"}`))
	}); err != nil {
		t.Fatalf("register contract.current: %v", err)
	}
	inputSchema[0] = '['
	outputSchema[0] = '['
	rejectedSchema[0] = '['
	skillRefs[0] = "mutated-source"

	list := registry.List()
	if len(list) != 1 {
		t.Fatalf("definitions length = %d, want 1", len(list))
	}
	if list[0].Name != "contract.current" || list[0].RequiredScope != "agent_lease" {
		t.Fatalf("definition = %+v", list[0])
	}
	if string(list[0].InputSchema) != `{"type":"object","properties":{"contract_id":{"type":"string"}}}` {
		t.Fatalf("input schema leaked source mutation: %s", list[0].InputSchema)
	}
	if string(list[0].OutputSchema) != `{"type":"object","properties":{"status":{"type":"string"}}}` {
		t.Fatalf("output schema leaked source mutation: %s", list[0].OutputSchema)
	}
	if string(list[0].RejectedSchema) != `{"type":"object","required":["error_code","message","retryable"]}` {
		t.Fatalf("rejected schema leaked source mutation: %s", list[0].RejectedSchema)
	}
	if list[0].SkillRefs[0] != "coordplane-service" {
		t.Fatalf("skill refs leaked source mutation: %+v", list[0].SkillRefs)
	}

	list[0].SkillRefs[0] = "mutated"
	list[0].InputSchema[0] = '['
	list[0].OutputSchema[0] = '['
	list[0].RejectedSchema[0] = '['
	again := registry.List()
	if again[0].SkillRefs[0] != "coordplane-service" {
		t.Fatalf("registry leaked mutable definition state: %+v", again[0])
	}
	if string(again[0].InputSchema) != `{"type":"object","properties":{"contract_id":{"type":"string"}}}` {
		t.Fatalf("registry leaked mutable input schema state: %s", again[0].InputSchema)
	}
	if string(again[0].OutputSchema) != `{"type":"object","properties":{"status":{"type":"string"}}}` {
		t.Fatalf("registry leaked mutable output schema state: %s", again[0].OutputSchema)
	}
	if string(again[0].RejectedSchema) != `{"type":"object","required":["error_code","message","retryable"]}` {
		t.Fatalf("registry leaked mutable rejected schema state: %s", again[0].RejectedSchema)
	}
}
