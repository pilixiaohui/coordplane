package core

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeNotFound                    ErrorCode = "NOT_FOUND"
	CodeInvalidArgument             ErrorCode = "INVALID_ARGUMENT"
	CodeInvalidState                ErrorCode = "INVALID_STATE"
	CodeActionInProgress            ErrorCode = "ACTION_IN_PROGRESS"
	CodeVersionConflict             ErrorCode = "VERSION_CONFLICT"
	CodeScopeDenied                 ErrorCode = "SCOPE_DENIED"
	CodeStaleRun                    ErrorCode = "STALE_RUN"
	CodeRunStarting                 ErrorCode = "RUN_STARTING"
	CodeAgentBusy                   ErrorCode = "AGENT_BUSY"
	CodeRuntimeUnavailable          ErrorCode = "RUNTIME_UNAVAILABLE"
	CodeRuntimeInvariantViolation   ErrorCode = "RUNTIME_INVARIANT_VIOLATION"
	CodeResumeUnavailable           ErrorCode = "RESUME_UNAVAILABLE"
	CodeGitDirty                    ErrorCode = "GIT_DIRTY"
	CodeGitStale                    ErrorCode = "GIT_STALE"
	CodeIntegrationAgentRequired    ErrorCode = "INTEGRATION_AGENT_REQUIRED"
	CodeGitInvariantViolation       ErrorCode = "GIT_INVARIANT_VIOLATION"
	CodeLegacySchemaRebuildRequired ErrorCode = "LEGACY_SCHEMA_REBUILD_REQUIRED"
	CodeInternal                    ErrorCode = "INTERNAL"
)

type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	State     string    `json:"state,omitempty"`
	Version   int64     `json:"version,omitempty"`
	Cause     error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.State != "" {
		return fmt.Sprintf("%s: %s (state=%s version=%d)", e.Code, e.Message, e.State, e.Version)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewError(code ErrorCode, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

func WrapError(code ErrorCode, message string, retryable bool, cause error) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable, Cause: cause}
}

func Conflict(code ErrorCode, message, state string, version int64) *Error {
	return &Error{Code: code, Message: message, Retryable: true, State: state, Version: version}
}

func IsCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return WrapError(CodeInternal, "internal error", false, err)
}
