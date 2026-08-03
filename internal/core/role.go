package core

import (
	"strings"
)

// Role is a fully configurable capability set. Roles are data: they are
// created, renamed and re-scoped at runtime through the operator CLI; only the
// capability vocabulary itself is static code fact (see capability.go).
type Role struct {
	ID           string
	Name         string
	Description  string
	Capabilities []Capability
	Version      int64
	CreatedAt    string
	UpdatedAt    string
}

// RoleInput is the create operation payload.
type RoleInput struct {
	Name         string
	Description  string
	Capabilities []string
	RequestID    string
}

// RoleUpdateInput is the update operation payload.
type RoleUpdateInput struct {
	RoleID       string
	Name         string
	Description  string
	Capabilities []string
	RequestID    string
}

func validateRoleName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return NewError(CodeInvalidArgument, "role name is required", false)
	}
	if len(name) > 64 {
		return NewError(CodeInvalidArgument, "role name must be at most 64 characters", false)
	}
	return nil
}
