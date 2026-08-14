package daemon

import (
	"crypto/sha256"
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

	runtimeSecretsFile    = "secrets"
	runtimeBootstrapFile  = "bootstrap"
	runContextSeparator   = "\n\nCoordPlane Run context\n"
	shellSingleQuoteEscape = "'\\''"
)

type runtimeRedaction struct {
	values []runtimeRedactionValue
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
	secrets := c.runtimeRunSecrets(run)
	if run.ID != "" && c.controlRoot != "" {
		if token, err := os.ReadFile(filepath.Join(c.controlRoot, run.ID, "token")); err == nil {
			secrets = append(secrets, strings.TrimSpace(string(token)))
		}
	}
	redaction := newRuntimeRedaction(paths, secrets)
	if exact := c.runtimeRunInstructions(run); len(exact) > 0 {
		redaction = redaction.withExactValues(exact...)
	}
	return redaction
}

// runtimeRunSecrets returns the run-scoped provider secrets: values read from
// the run's immutable secrets file when it exists, falling back to the host
// provider allowlist env for legacy runs adopted before the file was written.
// It never re-reads the mutable Agent.
func (c *runtimeController) runtimeRunSecrets(run core.Run) []string {
	if values, ok := c.readRunSecretsFile(run.ID); ok {
		return values
	}
	secrets := make([]string, 0, len(c.config.Runtime.ProviderEnvAllowlist))
	for _, name := range c.config.Runtime.ProviderEnvAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

// readRunSecretsFile parses the run's secrets file (shell-sourceable
// NAME='value' lines written by the launch path) and returns the unquoted
// values. A missing, empty, or malformed file reports ok=false so callers fall
// back to the host allowlist env.
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

// runtimeRunInstructions returns the Agent instructions text captured in the
// run's immutable bootstrap file so provider output that echoes the prompt is
// redacted from run logs. The lookup is best-effort and never re-reads the
// mutable Agent: a missing or malformed bootstrap file skips exact-value
// redaction rather than failing monitor creation.
func (c *runtimeController) runtimeRunInstructions(run core.Run) []string {
	if c == nil || c.controlRoot == "" || strings.TrimSpace(run.ID) == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(c.controlRoot, run.ID, runtimeBootstrapFile))
	if err != nil {
		return nil
	}
	text := strings.TrimSpace(bootstrapInstructionsPrefix(string(raw)))
	if text == "" {
		return nil
	}
	return []string{text}
}

// bootstrapInstructionsPrefix extracts the instructions text that buildBootstrap
// placed before the run-context separator.
func bootstrapInstructionsPrefix(bootstrap string) string {
	if index := strings.Index(bootstrap, runContextSeparator); index >= 0 {
		return bootstrap[:index]
	}
	return bootstrap
}

// parseRunSecretsFile decodes the shell-sourceable secrets file written by the
// launch path. Each entry is NAME='value' with single quotes and '\'' escapes;
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
// Inside single quotes every byte is literal except the '\'' escape, which
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
// value is single-quoted with interior quotes escaped as '\'' so sourcing the
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
