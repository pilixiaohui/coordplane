package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestReadInstructionsFileSupportsExactLimitAndRejectsNonRegularSources(t *testing.T) {
	dir := t.TempDir()
	exact := strings.Repeat("x", core.MaximumInstructionsBytes)
	exactFile := filepath.Join(dir, "exact.md")
	requireNoError(t, os.WriteFile(exactFile, []byte(exact), 0o600))
	want := sha256.Sum256([]byte(exact))

	text, hash, err := readInstructions(core.Agent{InstructionsFile: exactFile})
	if err != nil || text != exact || hash != hex.EncodeToString(want[:]) {
		t.Fatalf("exact-limit file instructions = %d bytes %q %v", len(text), hash, err)
	}

	symlink := filepath.Join(dir, "symlink.md")
	requireNoError(t, os.Symlink(exactFile, symlink))
	if _, _, err := readInstructions(core.Agent{InstructionsFile: symlink}); err == nil {
		t.Fatal("symlinked instructions file was accepted")
	}

	directory := filepath.Join(dir, "directory")
	requireNoError(t, os.Mkdir(directory, 0o700))
	if _, _, err := readInstructions(core.Agent{InstructionsFile: directory}); err == nil {
		t.Fatal("directory instructions source was accepted")
	}
}

func TestReadInstructionsFileRejectsNamedPipeBeforeOpen(t *testing.T) {
	pipePath := filepath.Join(t.TempDir(), "instructions.pipe")
	requireNoError(t, syscall.Mkfifo(pipePath, 0o600))

	done := make(chan error, 1)
	go func() {
		_, _, err := readInstructions(core.Agent{InstructionsFile: pipePath})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("named pipe instructions source was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readInstructions blocked while opening a named pipe")
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
