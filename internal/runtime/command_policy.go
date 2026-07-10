package runtime

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

const (
	FailureClassRuntimeApprovalBlocked = "runtime_approval_blocked"
	FailureClassRuntimeCommandDenied   = "runtime_command_policy_denied"
	FailureClassRuntimeExecTimeout     = "runtime_exec_timeout"

	TerminalReasonApprovalPolicyUnavailable = "RUNTIME_APPROVAL_POLICY_UNAVAILABLE"
	TerminalReasonCommandPolicyDenied       = "RUNTIME_COMMAND_POLICY_DENIED"
	TerminalReasonRuntimeExecTimeout        = "RUNTIME_EXEC_TIMEOUT"
)

type RuntimeCommandPolicy struct {
	NonInteractiveApproval     bool
	AllowCoordlinkCapabilities []string
}

func (p RuntimeCommandPolicy) Configured() bool {
	return p.NonInteractiveApproval || len(p.AllowCoordlinkCapabilities) > 0
}

type ClassifiedError interface {
	error
	FailureClass() string
	TerminalReason() string
}

type runtimePolicyError struct {
	failureClass   string
	terminalReason string
	message        string
	cause          error
}

func NewRuntimeApprovalPolicyUnavailable(message string) error {
	if strings.TrimSpace(message) == "" {
		message = "command runtime provider is not configured for non-interactive approval"
	}
	return runtimePolicyError{
		failureClass:   FailureClassRuntimeApprovalBlocked,
		terminalReason: TerminalReasonApprovalPolicyUnavailable,
		message:        message,
	}
}

func NewRuntimeCommandPolicyDenied(message string) error {
	if strings.TrimSpace(message) == "" {
		message = "runtime command is not allowed by command_policy"
	}
	return runtimePolicyError{
		failureClass:   FailureClassRuntimeCommandDenied,
		terminalReason: TerminalReasonCommandPolicyDenied,
		message:        message,
	}
}

func NewRuntimeExecTimeout(message string, cause error) error {
	if strings.TrimSpace(message) == "" {
		message = "runtime command execution timed out"
	}
	return runtimePolicyError{
		failureClass:   FailureClassRuntimeExecTimeout,
		terminalReason: TerminalReasonRuntimeExecTimeout,
		message:        message,
		cause:          cause,
	}
}

func (e runtimePolicyError) Error() string {
	if e.message == "" {
		return e.terminalReason
	}
	return e.terminalReason + ": " + e.message
}

func (e runtimePolicyError) Unwrap() error {
	return e.cause
}

func (e runtimePolicyError) FailureClass() string {
	return e.failureClass
}

func (e runtimePolicyError) TerminalReason() string {
	return e.terminalReason
}

func ErrorFailureClass(err error) (string, bool) {
	var classified ClassifiedError
	if errors.As(err, &classified) {
		return classified.FailureClass(), true
	}
	return "", false
}

func ErrorTerminalReason(err error) (string, bool) {
	var classified ClassifiedError
	if errors.As(err, &classified) {
		return classified.TerminalReason(), true
	}
	return "", false
}

func EvaluateCommandPolicy(command []string, policy RuntimeCommandPolicy) error {
	if !policy.NonInteractiveApproval || len(policy.AllowCoordlinkCapabilities) == 0 {
		return NewRuntimeApprovalPolicyUnavailable("runtime profile command_policy must declare non_interactive_approval and an explicit coordlink capability allowlist")
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return NewRuntimeCommandPolicyDenied("empty runtime command is denied by command_policy")
	}
	if forbiddenCommandBinary(command[0]) {
		return NewRuntimeCommandPolicyDenied("runtime command binary is denied by command_policy")
	}
	if !isCoordlinkCommand(command) {
		return NewRuntimeCommandPolicyDenied("runtime command_policy only allows coordlink capability calls")
	}
	if len(command) < 3 || command[1] != "call" {
		return NewRuntimeCommandPolicyDenied("runtime command_policy only allows coordlink call invocations")
	}
	if containsForbiddenCommandMarker(command[1:]) {
		return NewRuntimeCommandPolicyDenied("runtime command contains denied token, host path, DB, Docker, shell, header, or network marker")
	}
	capabilityName := strings.TrimSpace(command[2])
	if capabilityName == "" || !slices.Contains(policy.AllowCoordlinkCapabilities, capabilityName) {
		return NewRuntimeCommandPolicyDenied("coordlink capability is not in the runtime command_policy allowlist")
	}
	return nil
}

func isCoordlinkCommand(command []string) bool {
	if len(command) == 0 {
		return false
	}
	binary := filepath.Base(strings.TrimSpace(command[0]))
	return binary == "coordlink"
}

func forbiddenCommandBinary(binary string) bool {
	switch filepath.Base(strings.TrimSpace(binary)) {
	case "sh", "bash", "dash", "zsh", "fish",
		"sqlite3", "psql", "mysql",
		"docker", "podman",
		"env", "printenv",
		"curl", "wget", "nc", "netcat", "ssh",
		"python", "python3", "node":
		return true
	default:
		return false
	}
}

func containsForbiddenCommandMarker(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		for _, marker := range []string{
			"authorization:",
			"bearer ",
			"coordplane_token",
			"operator_token",
			"anthropic_auth",
			"api_key",
			"secret=",
			"token=",
			"password=",
			"/var/run/docker.sock",
			"/var/lib/",
			"/home/",
			"/tmp/",
			"coordplane.db",
			"sqlite",
			"docker ",
			"curl ",
			"wget ",
			"http://",
			"https://",
		} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

func LooksLikeRuntimeApprovalPolicyFailure(text string) bool {
	return strings.Contains(text, TerminalReasonApprovalPolicyUnavailable) ||
		strings.Contains(text, TerminalReasonCommandPolicyDenied)
}
