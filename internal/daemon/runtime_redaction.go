package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"coordplane/internal/core"
)

const (
	redactedHostPath = "[REDACTED_HOST_PATH]"
	redactedSecret   = "[REDACTED_SECRET]"

	runtimeSecretsFile         = "secrets"
	runtimeInstructionsFile    = "instructions"
	shellSingleQuoteEscape     = "'\\''"
	redactionUnavailableMarker = "[coordplane: redaction unavailable; content suppressed]"
)

type runtimeRedaction struct {
	values     []runtimeRedactionValue
	failClosed bool
}

type runtimeRedactionValue struct {
	value       string
	replacement string
}

func (c *runtimeController) runtimeRedaction(run core.Run, extraPaths ...string) runtimeRedaction {
	paths := []string{
		c.config.DataDir,
		c.config.OperatorSocket,
		c.config.Runtime.WorkspaceRoot,
		c.config.Runtime.AgentHomeRoot,
		c.config.Runtime.LogRoot,
		c.controlRoot,
		c.coordlink,
		run.WorkspacePath,
		run.HomePath,
		run.LogPath,
		"/var/run/docker.sock",
	}
	paths = append(paths, extraPaths...)
	if dockerHost := strings.TrimSpace(os.Getenv("DOCKER_HOST")); strings.HasPrefix(dockerHost, "unix://") {
		paths = append(paths, strings.TrimPrefix(dockerHost, "unix://"))
	}
	secrets, ok := c.runtimeRunSecrets(run)
	if run.ID != "" && c.controlRoot != "" {
		if token, err := os.ReadFile(filepath.Join(c.controlRoot, run.ID, "token")); err == nil {
			secrets = append(secrets, strings.TrimSpace(string(token)))
		}
	}
	redaction := newRuntimeRedaction(paths, secrets)
	if !ok {
		redaction.failClosed = true
		return redaction
	}
	instructions, ok := c.runtimeRunInstructions(run)
	if !ok {
		redaction.failClosed = true
		return redaction
	}
	if len(instructions) > 0 {
		redaction = redaction.withExactValues(instructions...)
	}
	return redaction
}

// runtimeRunSecrets returns the run-scoped provider secrets and whether they are
// trustworthy. Every adopted container is launcher-lineage (ValidateAdoption
// enforces the exact launch entrypoint), so a missing or malformed secrets file
// reports ok=false and the caller fails closed instead of falling back to the
// unredacted host provider allowlist env. It never re-reads the mutable Agent.
func (c *runtimeController) runtimeRunSecrets(run core.Run) ([]string, bool) {
	if c == nil || c.controlRoot == "" || strings.TrimSpace(run.ID) == "" {
		return nil, true
	}
	return c.readRunSecretsFile(run.ID)
}

// readRunSecretsFile parses the run's secrets file (shell-sourceable
// NAME='value' lines written by the launch path) and returns the unquoted
// values. A missing or malformed file reports ok=false; a present but empty
// file is a valid no-secret run and reports ok=true.
func (c *runtimeController) readRunSecretsFile(runID string) ([]string, bool) {
	if c.controlRoot == "" || strings.TrimSpace(runID) == "" {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(c.controlRoot, runID, runtimeSecretsFile))
	if err != nil {
		return nil, false
	}
	return parseRunSecretsFile(string(raw))
}

// runtimeRunInstructions returns the Agent instructions text captured for the
// run so provider output that echoes the prompt is redacted from run logs. The
// lookup never re-reads the mutable Agent. Every adopted container is
// launcher-lineage and writes a dedicated instructions file during prepare;
// that file is read whole (construction-based, no separator parsing) and
// reconciled against the run's immutable InstructionsHash. A missing or
// hash-mismatched file fails closed (ok=false) so no unredacted content can be
// persisted.
func (c *runtimeController) runtimeRunInstructions(run core.Run) ([]string, bool) {
	if c == nil || c.controlRoot == "" || strings.TrimSpace(run.ID) == "" {
		return nil, true
	}
	raw, err := os.ReadFile(filepath.Join(c.controlRoot, run.ID, runtimeInstructionsFile))
	if err != nil {
		return nil, false
	}
	if run.InstructionsHash != "" {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != run.InstructionsHash {
			return nil, false
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, true
	}
	return []string{text}, true
}

// parseRunSecretsFile decodes the shell-sourceable secrets file written by the
// launch path. Each entry is NAME='value' with single quotes and '\” escapes;
// values may contain any byte including spaces, shell metacharacters, and
// newlines, so scanning is quote-aware rather than line-based. A malformed or
// non-single-quoted entry rejects the whole file so callers fall back to the
// host allowlist env instead of trusting an unquoted value.
func parseRunSecretsFile(raw string) ([]string, bool) {
	var values []string
	for pos := 0; pos < len(raw); {
		skipShellWhitespace := func() {
			for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t' || raw[pos] == '\r' || raw[pos] == '\n') {
				pos++
			}
		}
		skipShellWhitespace()
		if pos >= len(raw) {
			break
		}
		if raw[pos] == '#' {
			for pos < len(raw) && raw[pos] != '\n' {
				pos++
			}
			continue
		}
		keyStart := pos
		for pos < len(raw) && raw[pos] != '=' && raw[pos] != ' ' && raw[pos] != '\t' && raw[pos] != '\n' && raw[pos] != '\r' {
			pos++
		}
		key := raw[keyStart:pos]
		for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t') {
			pos++
		}
		if pos >= len(raw) || raw[pos] != '=' {
			return nil, false
		}
		pos++
		for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t') {
			pos++
		}
		if pos >= len(raw) || raw[pos] != '\'' {
			return nil, false
		}
		value, remainder, ok := scanShellSingleQuoted(raw[pos:])
		if !ok {
			return nil, false
		}
		pos += len(raw[pos:]) - len(remainder)
		for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t') {
			pos++
		}
		if pos < len(raw) && raw[pos] != '\n' && raw[pos] != '#' {
			return nil, false
		}
		if !validRunSecretKey(key) {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

// scanShellSingleQuoted reads a shell single-quoted string starting with '.
// Inside single quotes every byte is literal except the '\” escape, which
// yields a single quote. It returns the decoded value and the unconsumed tail.
func scanShellSingleQuoted(raw string) (value, rest string, ok bool) {
	if !strings.HasPrefix(raw, "'") {
		return "", raw, false
	}
	var builder strings.Builder
	pos := 1
	for pos < len(raw) {
		switch {
		case strings.HasPrefix(raw[pos:], shellSingleQuoteEscape):
			builder.WriteByte('\'')
			pos += len(shellSingleQuoteEscape)
		case raw[pos] == '\'':
			return builder.String(), raw[pos+1:], true
		default:
			builder.WriteByte(raw[pos])
			pos++
		}
	}
	return "", raw, false
}

// validRunSecretKey mirrors the Docker env-name rule so a secrets file line can
// only ever name a syntactically valid environment variable.
func validRunSecretKey(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_' ||
			(index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

// serializeRunSecretsFile renders the run-scoped provider secrets as a
// shell-sourceable file. Each entry is a validated NAME='value' line whose
// value is single-quoted with interior quotes escaped as '\” so sourcing the
// file can never execute metacharacters; keys are sorted for deterministic
// output. A key that is not a valid environment name, or a value containing a
// NUL byte, rejects the whole file rather than emitting an unsafe line.
func serializeRunSecretsFile(secrets map[string]string) ([]byte, error) {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	// A no-secret run is legitimate, but the reconcile lineage gate treats an
	// empty control file as corrupt, so the file always carries a header line.
	builder.WriteString("# coordplane-managed run secrets\n")
	for _, name := range names {
		if !validRunSecretKey(name) {
			return nil, fmt.Errorf("run secret key %q is not a valid environment name", name)
		}
		value := secrets[name]
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("run secret %q contains a NUL byte", name)
		}
		builder.WriteString(name)
		builder.WriteString("='")
		builder.WriteString(strings.ReplaceAll(value, "'", shellSingleQuoteEscape))
		builder.WriteString("'\n")
	}
	return []byte(builder.String()), nil
}

func newRuntimeRedaction(paths, secrets []string) runtimeRedaction {
	unique := make(map[string]string, len(paths)+len(secrets))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		unique[filepath.Clean(path)] = redactedHostPath
	}
	for _, secret := range secrets {
		for _, candidate := range append([]string{secret, strings.TrimSpace(secret)}, strings.Fields(secret)...) {
			if candidate == "" {
				continue
			}
			unique[candidate] = redactedSecret
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(candidate)))
			unique[digest] = redactedSecret
			unique[strings.ToUpper(digest)] = redactedSecret
		}
	}
	values := make([]runtimeRedactionValue, 0, len(unique))
	for value, replacement := range unique {
		values = append(values, runtimeRedactionValue{value: value, replacement: replacement})
	}
	sort.Slice(values, func(left, right int) bool {
		if len(values[left].value) != len(values[right].value) {
			return len(values[left].value) > len(values[right].value)
		}
		return values[left].value < values[right].value
	})
	return runtimeRedaction{values: values}
}

func (r runtimeRedaction) Text(value string) string {
	if r.failClosed {
		return redactionUnavailableMarker
	}
	for _, item := range r.values {
		value = strings.ReplaceAll(value, item.value, item.replacement)
	}
	return value
}

// withExactValues appends whole-string secrets (for example the full
// instructions text) without expanding them into fields. Field expansion is
// reserved for provider credentials; expanding an instruction sentence would
// over-redact ordinary words such as "on" or "the" inside unrelated output.
func (r runtimeRedaction) withExactValues(values ...string) runtimeRedaction {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		r.values = append(r.values, runtimeRedactionValue{value: value, replacement: redactedSecret})
	}
	sort.Slice(r.values, func(left, right int) bool {
		if len(r.values[left].value) != len(r.values[right].value) {
			return len(r.values[left].value) > len(r.values[right].value)
		}
		return r.values[left].value < r.values[right].value
	})
	return r
}
