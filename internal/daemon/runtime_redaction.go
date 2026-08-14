package daemon

import (
	"context"
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
	secrets := make([]string, 0, len(c.config.Runtime.ProviderEnvAllowlist)+1)
	for _, name := range c.config.Runtime.ProviderEnvAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			secrets = append(secrets, value)
		}
	}
	if run.ID != "" {
		token, err := os.ReadFile(filepath.Join(c.controlRoot, run.ID, "token"))
		if err == nil {
			secrets = append(secrets, strings.TrimSpace(string(token)))
		}
	}
	return newRuntimeRedaction(paths, secrets).withExactValues(c.runtimeInstructionSecrets(run)...)
}

// runtimeInstructionSecrets returns the Agent instructions text so provider
// output that echoes the prompt is redacted from run logs. The lookup is
// best-effort: monitor creation must never fail because instructions cannot be
// re-read (the launch path already validated them).
func (c *runtimeController) runtimeInstructionSecrets(run core.Run) []string {
	if c == nil || c.service == nil || strings.TrimSpace(run.AgentID) == "" {
		return nil
	}
	agent, err := c.service.Agent(context.Background(), run.AgentID)
	if err != nil {
		return nil
	}
	text, _, err := readInstructions(agent)
	if err != nil || text == "" {
		return nil
	}
	return []string{text}
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
