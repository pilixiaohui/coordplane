package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestReadInstructionsSupportsFileAndTextWithIdenticalHashSemantics(t *testing.T) {
	prompt := "Follow these instructions.\nDo not claim completion without evidence."
	want := sha256.Sum256([]byte(prompt))
	wantHex := hex.EncodeToString(want[:])

	text, hash, err := readInstructions(core.Agent{InstructionsText: prompt})
	if err != nil || text != prompt || hash != wantHex {
		t.Fatalf("text instructions = %q %q %v", text, hash, err)
	}

	file := filepath.Join(t.TempDir(), "instructions.md")
	requireNoError(t, os.WriteFile(file, []byte(prompt), 0o600))
	text, hash, err = readInstructions(core.Agent{InstructionsFile: file})
	if err != nil || text != prompt || hash != wantHex {
		t.Fatalf("file instructions = %q %q %v", text, hash, err)
	}
}

func TestReadInstructionsFailsClosedAndNeverLeaksPromptText(t *testing.T) {
	prompt := "TOP-SECRET instructions must not leak"
	tests := []struct {
		name  string
		agent core.Agent
	}{
		{"both sources", core.Agent{InstructionsFile: "/instructions/agent.md", InstructionsText: prompt}},
		{"missing source", core.Agent{}},
		{"relative file", core.Agent{InstructionsFile: "instructions/agent.md"}},
		{"non-canonical file", core.Agent{InstructionsFile: "/instructions/../agent.md"}},
		{"oversized text", core.Agent{InstructionsText: strings.Repeat("x", core.MaximumInstructionsBytes+1)}},
		{"invalid UTF-8 text", core.Agent{InstructionsText: string([]byte{0xff, 0xfe})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, hash, err := readInstructions(test.agent)
			if err == nil || text != "" || hash != "" {
				t.Fatalf("error = %v, result = %q/%q", err, text, hash)
			}
			if strings.Contains(err.Error(), prompt) {
				t.Fatalf("instructions error leaked prompt text: %v", err)
			}
		})
	}

	oversizedFile := filepath.Join(t.TempDir(), "oversized.md")
	requireNoError(t, os.WriteFile(oversizedFile, []byte(strings.Repeat("x", core.MaximumInstructionsBytes+1)), 0o600))
	if _, _, err := readInstructions(core.Agent{InstructionsFile: oversizedFile}); err == nil {
		t.Fatal("oversized instructions file was accepted")
	}
}
