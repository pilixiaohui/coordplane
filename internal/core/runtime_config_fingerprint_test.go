package core

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestRuntimeConfigFingerprintIsNormalizedAndStable(t *testing.T) {
	base := RuntimeConfigFingerprintInput{
		AdapterID:        "codex",
		Image:            "coordplane-agent:latest",
		Model:            "model-a",
		SubagentModel:    "subagent-a",
		BaseURL:          "https://example.invalid/v1",
		Effort:           "high",
		InstructionsHash: strings.Repeat("a", 64),
	}
	first, err := RuntimeConfigFingerprint(base)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	padded := base
	padded.AdapterID = "  " + base.AdapterID + " "
	padded.Model = "\t" + base.Model + "\n"
	second, err := RuntimeConfigFingerprint(padded)
	if err != nil {
		t.Fatalf("normalized fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("whitespace changed fingerprint: %q != %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("fingerprint length = %d, want 64", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}
	for name, changed := range map[string]RuntimeConfigFingerprintInput{
		"adapter":      withField(base, "AdapterID", "claude"),
		"image":        withField(base, "Image", "other:latest"),
		"model":        withField(base, "Model", "model-b"),
		"subagent":     withField(base, "SubagentModel", "subagent-b"),
		"base_url":     withField(base, "BaseURL", "https://other.example/v1"),
		"effort":       withField(base, "Effort", "low"),
		"instructions": withField(base, "InstructionsHash", strings.Repeat("b", 64)),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RuntimeConfigFingerprint(changed)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if got == first {
				t.Fatalf("changed %s did not change fingerprint", name)
			}
		})
	}
}

func TestRuntimeConfigFingerprintRejectsIncompleteConfiguration(t *testing.T) {
	valid := RuntimeConfigFingerprintInput{
		AdapterID:        "codex",
		Image:            "image",
		InstructionsHash: strings.Repeat("a", 64),
	}
	for name, changed := range map[string]RuntimeConfigFingerprintInput{
		"adapter":           {AdapterID: "", Image: valid.Image, InstructionsHash: valid.InstructionsHash},
		"image":             {AdapterID: valid.AdapterID, InstructionsHash: valid.InstructionsHash},
		"instructions_hash": {AdapterID: valid.AdapterID, Image: valid.Image},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RuntimeConfigFingerprint(changed); !IsCode(err, CodeInvalidArgument) {
				t.Fatalf("error = %v, want %s", err, CodeInvalidArgument)
			}
		})
	}
}

func withField(input RuntimeConfigFingerprintInput, name, value string) RuntimeConfigFingerprintInput {
	switch name {
	case "AdapterID":
		input.AdapterID = value
	case "Image":
		input.Image = value
	case "Model":
		input.Model = value
	case "SubagentModel":
		input.SubagentModel = value
	case "BaseURL":
		input.BaseURL = value
	case "Effort":
		input.Effort = value
	case "InstructionsHash":
		input.InstructionsHash = value
	}
	return input
}
