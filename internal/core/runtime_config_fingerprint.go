package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// RuntimeConfigFingerprintInput is the complete non-secret runtime
// configuration captured by a Run. The fingerprint is deliberately
// opaque: it never contains provider credentials or prompt text.
type RuntimeConfigFingerprintInput struct {
	AdapterID        string
	Image            string
	Model            string
	SubagentModel    string
	BaseURL          string
	Effort           string
	InstructionsHash string
}

// RuntimeConfigFingerprint returns the canonical SHA-256 fingerprint for a
// runtime configuration. Every input is normalized by trimming surrounding
// whitespace before serialization; the JSON field order is fixed by the
// struct below, so the result is stable and idempotent for the same
// configuration and changes when any component changes.
func RuntimeConfigFingerprint(input RuntimeConfigFingerprintInput) (string, error) {
	normalized := runtimeConfigFingerprintParts{
		AdapterID:        strings.TrimSpace(input.AdapterID),
		Image:            strings.TrimSpace(input.Image),
		Model:            strings.TrimSpace(input.Model),
		SubagentModel:    strings.TrimSpace(input.SubagentModel),
		BaseURL:          strings.TrimSpace(input.BaseURL),
		Effort:           strings.TrimSpace(input.Effort),
		InstructionsHash: strings.TrimSpace(input.InstructionsHash),
	}
	if normalized.AdapterID == "" || normalized.Image == "" || normalized.InstructionsHash == "" {
		return "", NewError(CodeInvalidArgument, "runtime config fingerprint requires adapter, image, and instructions hash", false)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", WrapError(CodeInternal, "serialize runtime config fingerprint", false, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type runtimeConfigFingerprintParts struct {
	AdapterID        string `json:"adapter_id"`
	Image            string `json:"image"`
	Model            string `json:"model"`
	SubagentModel    string `json:"subagent_model"`
	BaseURL          string `json:"base_url"`
	Effort           string `json:"effort"`
	InstructionsHash string `json:"instructions_hash"`
}
