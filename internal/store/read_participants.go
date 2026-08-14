package store

import (
	"context"
	"encoding/json"

	"coordplane/internal/core"
)

const credentialSelect = `SELECT id,participant_id,kind,secret_hash,status,created_at,revoked_at FROM credentials`

func scanRole(row rowScanner) (core.Role, error) {
	var role core.Role
	var capabilities string
	if err := row.Scan(&role.ID, &role.Name, &role.Description, &capabilities, &role.Version, &role.CreatedAt, &role.UpdatedAt); err != nil {
		return core.Role{}, err
	}
	parsed, err := core.ParseCapabilities(splitJSONStrings(capabilities))
	if err != nil {
		return core.Role{}, err
	}
	role.Capabilities = parsed
	return role, nil
}

func scanParticipant(row rowScanner) (core.Participant, error) {
	var participant core.Participant
	var kind string
	if err := row.Scan(&participant.ID, &kind, &participant.DisplayName, &participant.Status,
		&participant.CredentialID, &participant.AdapterID, &participant.Image,
		&participant.InstructionsFile, &participant.Model, &participant.SubagentModel,
		&participant.BaseURL, &participant.Effort, &participant.InstructionsText,
		&participant.Version, &participant.CreatedAt, &participant.UpdatedAt); err != nil {
		return core.Participant{}, err
	}
	participant.Kind = core.ParticipantKind(kind)
	return participant, nil
}

func scanBinding(row rowScanner) (core.ParticipantRoleBinding, error) {
	var binding core.ParticipantRoleBinding
	var capabilities string
	if err := row.Scan(&binding.ParticipantID, &binding.ProjectID, &binding.RoleID, &binding.RoleName,
		&capabilities, &binding.Version, &binding.CreatedAt, &binding.UpdatedAt); err != nil {
		return core.ParticipantRoleBinding{}, err
	}
	parsed, err := core.ParseCapabilities(splitJSONStrings(capabilities))
	if err != nil {
		return core.ParticipantRoleBinding{}, err
	}
	binding.Capabilities = parsed
	return binding, nil
}

func jsonCapabilities(capabilities []core.Capability) string {
	raw, err := json.Marshal(core.CapabilityNames(capabilities))
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func splitJSONStrings(raw string) []string {
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	return names
}

func scanCredential(row rowScanner) (core.Credential, error) {
	var credential core.Credential
	err := row.Scan(
		&credential.ID, &credential.ParticipantID, &credential.Kind, &credential.SecretHash,
		&credential.Status, &credential.CreatedAt, &credential.RevokedAt,
	)
	return credential, err
}

func (s *Store) Role(ctx context.Context, id string) (core.Role, error) {
	role, err := scanRole(s.db.QueryRowContext(ctx, roleSelect+` WHERE id=?`, id))
	return role, mapNotFound("role", id, err)
}

func (s *Store) Roles(ctx context.Context) ([]core.Role, error) {
	rows, err := s.db.QueryContext(ctx, roleSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []core.Role
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func (s *Store) Participant(ctx context.Context, id string) (core.Participant, error) {
	participant, err := scanParticipant(s.db.QueryRowContext(ctx, participantSelect+` WHERE id=?`, id))
	return participant, mapNotFound("participant", id, err)
}

func (s *Store) Participants(ctx context.Context) ([]core.Participant, error) {
	rows, err := s.db.QueryContext(ctx, participantSelect+` ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var participants []core.Participant
	for rows.Next() {
		participant, err := scanParticipant(rows)
		if err != nil {
			return nil, err
		}
		participants = append(participants, participant)
	}
	return participants, rows.Err()
}

func (s *Store) ParticipantRoles(ctx context.Context, participantID string) ([]core.ParticipantRoleBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT b.participant_id,b.project_id,b.role_id,r.name,r.capabilities,b.version,b.created_at,b.updated_at
FROM participant_project_role b
JOIN roles r ON r.id=b.role_id
WHERE b.participant_id=?
ORDER BY b.project_id,b.role_id`, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []core.ParticipantRoleBinding
	for rows.Next() {
		binding, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}
