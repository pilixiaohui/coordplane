package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

const (
	FailureClassRuntimeApprovalBlocked = "runtime_approval_blocked"
	FailureClassRuntimeCommandDenied   = "runtime_command_policy_denied"
	FailureClassRuntimeExecTimeout     = "runtime_exec_timeout"
	FailureClassAgentExited            = "agent_exited_without_terminal_action"

	TerminalReasonApprovalPolicyUnavailable = "RUNTIME_APPROVAL_POLICY_UNAVAILABLE"
	TerminalReasonCommandPolicyDenied       = "RUNTIME_COMMAND_POLICY_DENIED"
	TerminalReasonRuntimeExecTimeout        = "RUNTIME_EXEC_TIMEOUT"
	TerminalReasonAgentExitedWithoutAction  = "AGENT_EXITED_WITHOUT_TERMINAL_ACTION"
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

func NewAgentExitedWithoutTerminalAction(message string) error {
	if strings.TrimSpace(message) == "" {
		message = "one-shot provider exited without a terminal contract action"
	}
	return runtimePolicyError{
		failureClass:   FailureClassAgentExited,
		terminalReason: TerminalReasonAgentExitedWithoutAction,
		message:        message,
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
	capabilityName, err := validateCoordlinkPolicyCallArgs(command[1:])
	if err != nil {
		return err
	}
	if capabilityName == "" || !slices.Contains(policy.AllowCoordlinkCapabilities, capabilityName) {
		return NewRuntimeCommandPolicyDenied("coordlink capability is not in the runtime command_policy allowlist")
	}
	return nil
}

func validateCoordlinkPolicyCallArgs(args []string) (string, error) {
	if len(args) < 2 || args[0] != "call" {
		return "", NewRuntimeCommandPolicyDenied("runtime command_policy only allows coordlink call invocations")
	}
	capabilityName := strings.TrimSpace(args[1])
	if capabilityName == "" || strings.HasPrefix(capabilityName, "-") {
		return "", NewRuntimeCommandPolicyDenied("runtime command_policy requires a capability name")
	}
	seenInput := false
	seenIdempotency := false
	for index := 2; index < len(args); {
		flag := args[index]
		if index+1 >= len(args) {
			return "", NewRuntimeCommandPolicyDenied("runtime coordlink call flag requires a value")
		}
		value := args[index+1]
		switch flag {
		case "--input":
			if seenInput || !validJSONObject(value) {
				return "", NewRuntimeCommandPolicyDenied("runtime coordlink --input must be one JSON object")
			}
			seenInput = true
		case "--idempotency-key":
			if seenIdempotency || !validPolicyIdempotencyKey(value) {
				return "", NewRuntimeCommandPolicyDenied("runtime coordlink idempotency key is invalid")
			}
			seenIdempotency = true
		default:
			return "", NewRuntimeCommandPolicyDenied("runtime coordlink call contains an unsupported flag or positional argument")
		}
		index += 2
	}
	return capabilityName, nil
}

func validJSONObject(raw string) bool {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	object, ok := value.(map[string]any)
	return ok && object != nil
}

func validPolicyIdempotencyKey(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '.', char == '_', char == '-', char == ':':
		default:
			return false
		}
	}
	return true
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

func LooksLikeRuntimeApprovalPolicyFailure(text string) bool {
	return strings.Contains(text, TerminalReasonApprovalPolicyUnavailable) ||
		strings.Contains(text, TerminalReasonCommandPolicyDenied)
}
