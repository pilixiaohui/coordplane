package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ReadAgentInstructions resolves the immutable Agent instructions snapshot: the
// exact bytes that back the run's InstructionsHash and ConfigFingerprint. The
// daemon mirrors this semantics at prepare time; both sides must agree so the
// claim-to-launch snapshot re-verification in BeginRunLaunch can compare
// fingerprints produced from the same source.
func ReadAgentInstructions(agent Agent) (string, string, error) {
	file := strings.TrimSpace(agent.InstructionsFile)
	text := agent.InstructionsText
	switch {
	case file != "" && text != "":
		return "", "", errors.New("Agent instructions_file and instructions_text are both set")
	case file != "":
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			return "", "", errors.New("Agent instructions file must be a canonical absolute path")
		}
		info, err := os.Lstat(file)
		if err != nil {
			return "", "", fmt.Errorf("inspect Agent instructions: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", "", errors.New("Agent instructions file is not a regular file")
		}
		handle, err := os.Open(file)
		if err != nil {
			return "", "", fmt.Errorf("open Agent instructions: %w", err)
		}
		defer handle.Close()
		raw, err := io.ReadAll(io.LimitReader(handle, MaximumInstructionsBytes+1))
		if err != nil {
			return "", "", fmt.Errorf("read Agent instructions: %w", err)
		}
		if len(raw) > MaximumInstructionsBytes {
			return "", "", errors.New("Agent instructions exceed 1 MiB")
		}
		sum := sha256.Sum256(raw)
		return string(raw), hex.EncodeToString(sum[:]), nil
	case text != "":
		if !utf8.ValidString(text) {
			return "", "", errors.New("Agent instructions text is not valid UTF-8")
		}
		if len(text) > MaximumInstructionsBytes {
			return "", "", errors.New("Agent instructions exceed 1 MiB")
		}
		sum := sha256.Sum256([]byte(text))
		return text, hex.EncodeToString(sum[:]), nil
	default:
		return "", "", errors.New("Agent instructions source is missing")
	}
}
