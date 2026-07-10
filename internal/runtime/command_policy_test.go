package runtime_test

import (
	"strings"
	"testing"

	cpruntime "coordplane/internal/runtime"
)

func TestCommandPolicyParsesCoordlinkArgvWithoutScanningJSONValuesAsShell(t *testing.T) {
	policy := cpruntime.RuntimeCommandPolicy{
		NonInteractiveApproval:     true,
		AllowCoordlinkCapabilities: []string{"report.submit", "contract.current"},
	}
	validInput := `{"summary":"semicolon ; URL https://example.invalid/a and path /tmp/report stay data","content":"$(not-shell) | >"}`
	if err := cpruntime.EvaluateCommandPolicy([]string{
		cpruntime.ContainerCoordlinkPath,
		"call",
		"report.submit",
		"--input",
		validInput,
		"--idempotency-key",
		"report-2026.07_10",
	}, policy); err != nil {
		t.Fatalf("valid structured coordlink argv rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "null input", args: []string{"call", "report.submit", "--input", "null"}},
		{name: "array input", args: []string{"call", "report.submit", "--input", `[]`}},
		{name: "scalar input", args: []string{"call", "report.submit", "--input", `"text"`}},
		{name: "malformed input", args: []string{"call", "report.submit", "--input", `{`}},
		{name: "duplicate input", args: []string{"call", "report.submit", "--input", `{}`, "--input", `{}`}},
		{name: "input file", args: []string{"call", "report.submit", "--input-file", "/tmp/request.json"}},
		{name: "identity override", args: []string{"call", "contract.current", "--lease-id", "lease_attacker"}},
		{name: "unknown flag", args: []string{"call", "contract.current", "--backend-url", "https://attacker.invalid"}},
		{name: "shell suffix", args: []string{"call", "contract.current", ";", "curl", "https://attacker.invalid"}},
		{name: "extra positional", args: []string{"call", "contract.current", "unexpected"}},
		{name: "invalid idempotency key", args: []string{"call", "report.submit", "--idempotency-key", "bad key; suffix"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := append([]string{cpruntime.ContainerCoordlinkPath}, tc.args...)
			err := cpruntime.EvaluateCommandPolicy(command, policy)
			if err == nil || !strings.Contains(err.Error(), cpruntime.TerminalReasonCommandPolicyDenied) {
				t.Fatalf("EvaluateCommandPolicy(%#v) error = %v, want typed denial", command, err)
			}
			if strings.Contains(err.Error(), strings.Join(tc.args, " ")) {
				t.Fatalf("policy denial leaked raw argv: %v", err)
			}
		})
	}
}
