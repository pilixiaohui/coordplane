package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// operatorParticipant is the deterministic human participant attributed to
// operator CLI requests until credential binding lands. Permission checks
// always resolve its roles; the v3 migration seeds it with the owner role.
const operatorParticipant = DefaultHumanParticipantID

// requireOperatorCapability fails the operation unless the default human
// participant holds the capability in the project scope or in the reserved
// global scope. It is the permission gate for operator-facing operations;
// run-token fencing for agents remains orthogonal.
func (s *Service) requireOperatorCapability(ctx context.Context, capability Capability, projectID string) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		if _, err := tx.Participant(operatorParticipant); err != nil {
			return NewError(CodeScopeDenied, "operator participant is not available", false)
		}
		bindings, err := tx.ParticipantRoles(operatorParticipant)
		if err != nil {
			return err
		}
		effective := ParticipantCapabilities(bindings, projectID)
		if !HasCapability(effective, capability) {
			return NewError(CodeScopeDenied, fmt.Sprintf("operator participant lacks capability %q in this scope", capability), false)
		}
		return nil
	})
}

// CreateRole creates a fully configurable role from the static capability
// vocabulary. Requires role.manage in the global scope.
func (s *Service) CreateRole(ctx context.Context, input RoleInput) (Role, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityRoleManage, GlobalProjectID); err != nil {
		return Role{}, err
	}
	name := strings.TrimSpace(input.Name)
	if err := validateRoleName(name); err != nil {
		return Role{}, err
	}
	capabilities, err := ParseCapabilities(input.Capabilities)
	if err != nil {
		return Role{}, err
	}
	inputHash, err := inputFingerprint(struct {
		Name, Description string
		Capabilities      []string
	}{name, strings.TrimSpace(input.Description), input.Capabilities})
	if err != nil {
		return Role{}, err
	}
	dedupe := requestDedupe{operatorParticipant, "role.create", input.RequestID, inputHash}
	var role Role
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			role, err = tx.Role(replay.ID)
			return err
		}
		existing, err := tx.Roles()
		if err != nil {
			return err
		}
		for _, candidate := range existing {
			if candidate.Name == name {
				return NewError(CodeInvalidState, fmt.Sprintf("role %q already exists", name), false)
			}
		}
		id, err := s.requiredID("role")
		if err != nil {
			return err
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		role = Role{ID: id, Name: name, Description: strings.TrimSpace(input.Description), Capabilities: capabilities, Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := tx.InsertRole(role); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{
			ProjectID: GlobalProjectID, EntityType: "role", EntityID: role.ID,
			Kind: "role.created", ActorKind: "daemon", ActorID: operatorParticipant,
			RequestID: input.RequestID, PayloadJSON: "{}", CreatedAt: now,
		}); err != nil {
			return err
		}
		return dedupe.record(tx, role.ID, "", now)
	})
	if err != nil {
		return Role{}, err
	}
	return role, nil
}

// UpdateRole renames or re-scopes a role. Requires role.manage in the global
// scope; concurrent updates conflict on the role version.
func (s *Service) UpdateRole(ctx context.Context, input RoleUpdateInput) (Role, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityRoleManage, GlobalProjectID); err != nil {
		return Role{}, err
	}
	if err := validateRoleName(input.Name); err != nil {
		return Role{}, err
	}
	capabilities, err := ParseCapabilities(input.Capabilities)
	if err != nil {
		return Role{}, err
	}
	var updated Role
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		current, err := tx.Role(input.RoleID)
		if err != nil {
			return err
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		updated = current
		updated.Name = strings.TrimSpace(input.Name)
		updated.Description = strings.TrimSpace(input.Description)
		updated.Capabilities = capabilities
		updated.Version = current.Version + 1
		updated.UpdatedAt = now
		if err := tx.UpdateRole(updated, current.Version); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{
			ProjectID: GlobalProjectID, EntityType: "role", EntityID: updated.ID,
			Kind: "role.updated", ActorKind: "daemon", ActorID: operatorParticipant,
			RequestID: input.RequestID, PayloadJSON: "{}", CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return Role{}, err
	}
	return updated, nil
}

// DeleteRole removes an unbound role. Roles referenced by any participant
// binding cannot be deleted (IN_USE). Requires role.manage in the global
// scope.
func (s *Service) DeleteRole(ctx context.Context, roleID, requestID string) error {
	if err := s.requireOperatorCapability(ctx, CapabilityRoleManage, GlobalProjectID); err != nil {
		return err
	}
	return s.repository.Transact(ctx, func(tx Transaction) error {
		if _, err := tx.Role(roleID); err != nil {
			return err
		}
		bindings, err := tx.RoleBindingCount(roleID)
		if err != nil {
			return err
		}
		if bindings > 0 {
			return NewError(CodeInUse, "role is bound to a participant and cannot be deleted", false)
		}
		if err := tx.DeleteRole(roleID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{
			ProjectID: GlobalProjectID, EntityType: "role", EntityID: roleID,
			Kind: "role.deleted", ActorKind: "daemon", ActorID: operatorParticipant,
			RequestID: requestID, PayloadJSON: "{}", CreatedAt: s.now().UTC().Format(time.RFC3339Nano),
		})
		return err
	})
}

// ListRoles returns every configured role in name order.
func (s *Service) ListRoles(ctx context.Context) ([]Role, error) {
	return s.repository.Roles(ctx)
}

// Role returns one role.
func (s *Service) Role(ctx context.Context, id string) (Role, error) {
	return s.repository.Role(ctx, id)
}

// ListParticipants returns every participant (humans and CLI agents).
func (s *Service) ListParticipants(ctx context.Context) ([]Participant, error) {
	return s.repository.Participants(ctx)
}

// Participant returns one participant.
func (s *Service) Participant(ctx context.Context, id string) (Participant, error) {
	return s.repository.Participant(ctx, id)
}

// BindParticipantRole binds one role to a participant inside a project scope
// (or the reserved global scope). Requires role.bind in the global scope.
func (s *Service) BindParticipantRole(ctx context.Context, input BindRoleInput) (ParticipantRoleBinding, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityRoleBind, GlobalProjectID); err != nil {
		return ParticipantRoleBinding{}, err
	}
	var binding ParticipantRoleBinding
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		participant, err := tx.Participant(input.ParticipantID)
		if err != nil {
			return err
		}
		role, err := tx.Role(input.RoleID)
		if err != nil {
			return err
		}
		projectID := strings.TrimSpace(input.ProjectID)
		if projectID == "" {
			return NewError(CodeInvalidArgument, "project scope is required", false)
		}
		if projectID != GlobalProjectID {
			if _, err := tx.Project(projectID); err != nil {
				return NewError(CodeNotFound, fmt.Sprintf("project %q was not found", projectID), false)
			}
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		binding = ParticipantRoleBinding{
			ParticipantID: participant.ID, ProjectID: projectID, RoleID: role.ID,
			RoleName: role.Name, Capabilities: role.Capabilities, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertParticipantRole(binding); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{
			ProjectID: projectID, EntityType: "participant", EntityID: participant.ID,
			Kind: "participant.role_bound", ActorKind: "daemon", ActorID: operatorParticipant,
			RequestID: input.RequestID, PayloadJSON: "{}", CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return ParticipantRoleBinding{}, err
	}
	return binding, nil
}

// UnbindParticipantRole removes one role binding. Requires role.bind in the
// global scope. Unbinding the last participant.manage holder is rejected so
// the system never loses its administrator.
func (s *Service) UnbindParticipantRole(ctx context.Context, input BindRoleInput) error {
	if err := s.requireOperatorCapability(ctx, CapabilityRoleBind, GlobalProjectID); err != nil {
		return err
	}
	return s.repository.Transact(ctx, func(tx Transaction) error {
		if _, err := tx.Participant(input.ParticipantID); err != nil {
			return err
		}
		role, err := tx.Role(input.RoleID)
		if err != nil {
			return err
		}
		projectID := strings.TrimSpace(input.ProjectID)
		if projectID == "" {
			return NewError(CodeInvalidArgument, "project scope is required", false)
		}
		// Management capability is granted through global-scope bindings only,
		// so the last-holder protection applies only to global unbinds.
		if projectID == GlobalProjectID && HasCapability(role.Capabilities, CapabilityParticipantManage) {
			holders, err := s.participantManageHolders(ctx, tx)
			if err != nil {
				return err
			}
			if len(holders) == 1 && holders[0] == input.ParticipantID {
				// The participant is the only holder. It must retain manage
				// through another binding, otherwise the system loses its
				// administrator.
				retains, err := s.participantRetainsGlobalCapability(ctx, tx, input.ParticipantID, input.RoleID, CapabilityParticipantManage)
				if err != nil {
					return err
				}
				if !retains {
					return NewError(CodeInUse, "cannot remove the last participant.manage holder", false)
				}
			}
		}
		if err := tx.DeleteParticipantRole(input.ParticipantID, projectID, input.RoleID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{
			ProjectID: projectID, EntityType: "participant", EntityID: input.ParticipantID,
			Kind: "participant.role_unbound", ActorKind: "daemon", ActorID: operatorParticipant,
			RequestID: input.RequestID, PayloadJSON: "{}", CreatedAt: s.now().UTC().Format(time.RFC3339Nano),
		})
		return err
	})
}

func (s *Service) participantManageHolders(ctx context.Context, tx Transaction) ([]string, error) {
	participants, err := tx.Participants()
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, participant := range participants {
		bindings, err := tx.ParticipantRoles(participant.ID)
		if err != nil {
			return nil, err
		}
		if HasCapability(ParticipantCapabilities(bindings, GlobalProjectID), CapabilityParticipantManage) {
			holders = append(holders, participant.ID)
		}
	}
	return holders, nil
}

// participantRetainsGlobalCapability reports whether the participant keeps the
// capability through its remaining global-scope bindings after roleID is
// removed.
func (s *Service) participantRetainsGlobalCapability(ctx context.Context, tx Transaction, participantID, roleID string, capability Capability) (bool, error) {
	bindings, err := tx.ParticipantRoles(participantID)
	if err != nil {
		return false, err
	}
	var remaining []Capability
	for _, binding := range bindings {
		if binding.ProjectID != GlobalProjectID || binding.RoleID == roleID {
			continue
		}
		remaining = append(remaining, binding.Capabilities...)
	}
	return HasCapability(remaining, capability), nil
}
