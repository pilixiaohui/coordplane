package capability_test

import (
	"encoding/json"
	"testing"

	"coordplane/internal/capability"
)

func TestRejectedResponseIsRepairableTypedEnvelope(t *testing.T) {
	resp := capability.Rejected[map[string]string](
		"MISSING_REQUIRED_EVIDENCE",
		"contract.complete requires one report evidence before this contract can be satisfied",
		capability.WithCanonicalID("contract_id", "ctr_123"),
		capability.WithCanonicalID("lease_id", "lease_456"),
		capability.WithMissing("report", "report.submit"),
		capability.WithAllowedNextActions("report.submit", "contract.context", "message.send"),
		capability.WithRepairHint("submit a report evidence with report.submit, then retry contract.complete"),
		capability.WithRetryable(true),
	)

	if err := resp.Validate(); err != nil {
		t.Fatalf("validate rejected response: %v", err)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal rejected response: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal rejected response: %v", err)
	}

	if decoded["ok"] != false {
		t.Fatalf("ok = %v, want false", decoded["ok"])
	}
	if decoded["status"] != "rejected" {
		t.Fatalf("status = %v, want rejected", decoded["status"])
	}
	if decoded["error_code"] != "MISSING_REQUIRED_EVIDENCE" {
		t.Fatalf("error_code = %v", decoded["error_code"])
	}
	if decoded["retryable"] != true {
		t.Fatalf("retryable = %v, want true", decoded["retryable"])
	}
	if _, ok := decoded["data"]; ok {
		t.Fatalf("rejected response should not include data: %s", raw)
	}
	hint, ok := decoded["repair_hint"].(string)
	if !ok || hint == "" {
		t.Fatalf("repair_hint missing from %s", raw)
	}
}

func TestRejectedResponseValidationRequiresRepairGuidance(t *testing.T) {
	resp := capability.Rejected[string](
		"UNAUTHORIZED_SCOPE",
		"this agent cannot access the requested assignment",
		capability.WithRetryable(false),
	)
	if err := resp.Validate(); err == nil {
		t.Fatal("Validate returned nil for rejected response without repair guidance")
	}
}

func TestAcceptedResponseCarriesTypedData(t *testing.T) {
	resp := capability.Accepted(struct {
		ContractID string `json:"contract_id"`
	}{ContractID: "ctr_123"})
	if err := resp.Validate(); err != nil {
		t.Fatalf("validate accepted response: %v", err)
	}
	if resp.Data == nil || resp.Data.ContractID != "ctr_123" {
		t.Fatalf("accepted data = %+v", resp.Data)
	}
}
