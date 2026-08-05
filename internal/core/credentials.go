package core

import (
	"context"
	"strings"
)

// CredentialKind distinguishes human identity credentials. operator_token is
type CredentialKind string

const (
	CredentialKindOperatorToken CredentialKind = "operator_token"
	CredentialKindSSHKey        CredentialKind = "ssh_key"
)

// CredentialStatus marks an issued credential active or revoked. A revoked
type CredentialStatus string

const (
	CredentialActive  CredentialStatus = "active"
	CredentialRevoked CredentialStatus = "revoked"
)

type Credential struct {
	ID            string           `json:"id"`
	ParticipantID string           `json:"participant_id"`
	Kind          CredentialKind   `json:"kind"`
	SecretHash    string           `json:"secret_hash"`
	Status        CredentialStatus `json:"status"`
	CreatedAt     string           `json:"created_at"`
	RevokedAt     string           `json:"revoked_at,omitempty"`
}

// AddCredentialInput issues a credential for a participant. Only one active
type AddCredentialInput struct {
	ParticipantID string
	Kind          CredentialKind
	Secret        string
	RequestID     string
}

// AddCredential stores only the SHA-256 hash of the secret; the plaintext is
func (s *Service) AddCredential(ctx context.Context, input AddCredentialInput) (Credential, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityParticipantManage, GlobalProjectID); err != nil {
		return Credential{}, err
	}
	participantID, kind, secret, requestID, dedupe, err := s.credentialInput(input, "credential.add")
	if err != nil {
		return Credential{}, err
	}
	var credential Credential
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			credential, err = tx.Credential(replay.ID)
			return err
		}
		if _, err := tx.Participant(participantID); err != nil {
			return err
		}
		existing, err := tx.Credentials(participantID)
		if err != nil {
			return err
		}
		for _, candidate := range existing {
			if candidate.Status == CredentialActive {
				return Conflict(CodeInvalidState, "participant already has an active credential; rotate instead", string(candidate.Status), 1)
			}
		}
		credential, err = s.issueCredential(tx, participantID, kind, secret, requestID, "credential.added")
		return err
	})
	return credential, err
}

// RotateCredential revokes the active credential and issues a new one with
func (s *Service) RotateCredential(ctx context.Context, input AddCredentialInput) (Credential, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityParticipantManage, GlobalProjectID); err != nil {
		return Credential{}, err
	}
	participantID, kind, secret, requestID, _, err := s.credentialInput(input, "")
	if err != nil {
		return Credential{}, err
	}
	var credential Credential
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		now := s.nowText()
		existing, err := tx.Credentials(participantID)
		if err != nil {
			return err
		}
		var active Credential
		for _, candidate := range existing {
			if candidate.Status == CredentialActive {
				active = candidate
			}
		}
		if active.ID == "" {
			return Conflict(CodeInvalidState, "participant has no active credential to rotate", "none", 1)
		}
		active.Status = CredentialRevoked
		active.RevokedAt = now
		if err := tx.UpdateCredential(active, CredentialActive); err != nil {
			return err
		}
		credential, err = s.issueCredential(tx, participantID, kind, secret, requestID, "credential.rotated")
		return err
	})
	return credential, err
}

// RevokeCredential revokes the participant's active credential. Every
func (s *Service) RevokeCredential(ctx context.Context, participantID, requestID string) (Credential, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityParticipantManage, GlobalProjectID); err != nil {
		return Credential{}, err
	}
	participantID, err := requireText("participant_id", participantID)
	if err != nil {
		return Credential{}, err
	}
	requestID, err = s.requestID(requestID)
	if err != nil {
		return Credential{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ParticipantID, RequestID string
	}{participantID, requestID})
	if err != nil {
		return Credential{}, err
	}
	dedupe := requestDedupe{operatorParticipant, "credential.revoke", requestID, inputHash}

	var revoked Credential
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			revoked, err = tx.Credential(replay.ID)
			return err
		}
		now := s.nowText()
		existing, err := tx.Credentials(participantID)
		if err != nil {
			return err
		}
		for _, candidate := range existing {
			if candidate.Status == CredentialActive {
				candidate.Status = CredentialRevoked
				candidate.RevokedAt = now
				if err := tx.UpdateCredential(candidate, CredentialActive); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(event(GlobalProjectID, "participant", participantID, "credential.revoked", "daemon", operatorParticipant, "", requestID, "", "{}", now)); err != nil {
					return err
				}
				revoked = candidate
				return dedupe.record(tx, candidate.ID, "", now)
			}
		}
		return Conflict(CodeInvalidState, "participant has no active credential to revoke", "none", 1)
	})
	return revoked, err
}

// credentialInput validates the shared add/rotate fields and builds the
func (s *Service) credentialInput(input AddCredentialInput, operation string) (string, CredentialKind, string, string, requestDedupe, error) {
	participantID, err := requireText("participant_id", input.ParticipantID)
	if err != nil {
		return "", "", "", "", requestDedupe{}, err
	}
	if input.Kind != CredentialKindOperatorToken {
		return "", "", "", "", requestDedupe{}, NewError(CodeInvalidArgument, "only operator_token credentials are issued", false)
	}
	secret := strings.TrimSpace(input.Secret)
	if len(secret) < 16 {
		return "", "", "", "", requestDedupe{}, NewError(CodeInvalidArgument, "credential secret must be at least 16 characters", false)
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return "", "", "", "", requestDedupe{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ParticipantID, Kind, Secret, RequestID string
	}{participantID, string(input.Kind), secret, requestID})
	if err != nil {
		return "", "", "", "", requestDedupe{}, err
	}
	return participantID, input.Kind, secret, requestID, requestDedupe{operatorParticipant, operation, requestID, inputHash}, nil
}

// issueCredential inserts a new active credential and pins it on the
func (s *Service) issueCredential(tx Transaction, participantID string, kind CredentialKind, secret, requestID, eventKind string) (Credential, error) {
	now := s.nowText()
	id, err := s.requiredID("crd")
	if err != nil {
		return Credential{}, err
	}
	credential := Credential{
		ID: id, ParticipantID: participantID, Kind: kind,
		SecretHash: hashToken(secret), Status: CredentialActive, CreatedAt: now,
	}
	if err := tx.InsertCredential(credential); err != nil {
		return Credential{}, err
	}
	if err := tx.SetParticipantCredential(participantID, id); err != nil {
		return Credential{}, err
	}
	if _, err := tx.AppendEvent(event(GlobalProjectID, "participant", participantID, eventKind, "daemon", operatorParticipant, "", requestID, "", eventPayload(map[string]any{"kind": kind}), now)); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// AuthenticateOperatorCredential is the operator identity gate. When the
func (s *Service) AuthenticateOperator(ctx context.Context, secret string) error {
	return s.repository.Transact(ctx, func(tx Transaction) error {
		credentials, err := tx.Credentials(operatorParticipant)
		if err != nil {
			return err
		}
		if len(credentials) == 0 {
			return nil
		}
		presented := hashToken(strings.TrimSpace(secret))
		for _, credential := range credentials {
			if credential.Status == CredentialActive && credential.SecretHash == presented {
				return nil
			}
		}
		return NewError(CodeScopeDenied, "operator credential is missing, revoked, or invalid", false)
	})
}
