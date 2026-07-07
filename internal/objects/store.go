package objects

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/ids"
	"coordplane/internal/store"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Store struct {
	db *sql.DB
}

type ObjectMeta struct {
	Ref         string    `json:"object_ref"`
	OwnerAgent  string    `json:"owner_agent_id,omitempty"`
	Checksum    string    `json:"checksum"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

type ObjectContent struct {
	ObjectMeta
	Content string `json:"content"`
}

type PutInput struct {
	OwnerAgent  string
	Content     []byte
	ContentType string
}

type PutArtifactInput struct {
	OwnerAgent  string
	Content     []byte
	ContentType string
	Metadata    map[string]string
}

type Artifact struct {
	ID          string            `json:"id"`
	OwnerAgent  string            `json:"owner_agent_id"`
	ObjectRef   string            `json:"object_ref"`
	Checksum    string            `json:"checksum"`
	SizeBytes   int64             `json:"size_bytes"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type PutTranscriptInput struct {
	AttemptID   string
	Content     []byte
	ContentType string
}

type Transcript struct {
	ID        string    `json:"id"`
	AttemptID string    `json:"attempt_id"`
	ObjectRef string    `json:"object_ref"`
	Checksum  string    `json:"checksum"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type refInput struct {
	ObjectRef string `json:"object_ref"`
	Ref       string `json:"ref,omitempty"`
}

func NewStore(s *store.Store) *Store {
	return &Store{db: s.DB()}
}

func RegisterCapabilities(registry *capability.Registry, service *Store) error {
	if registry == nil {
		return errors.New("object capabilities: registry is nil")
	}
	if service == nil {
		return errors.New("object capabilities: service is nil")
	}
	for _, spec := range []struct {
		def     capability.Definition
		handler capability.HandlerFunc
	}{
		{definition("object.inspect"), service.handleInspect},
		{definition("object.read"), service.handleRead},
	} {
		if err := registry.Register(spec.def, spec.handler); err != nil {
			return err
		}
	}
	return nil
}

func definition(name string) capability.Definition {
	return capability.Definition{
		Name:           name,
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		RejectedSchema: json.RawMessage(`{"type":"object"}`),
		SideEffect:     capability.SideEffectRead,
		RequiredScope:  "agent_object",
		Idempotency:    false,
		SkillRefs:      []string{"coordplane-service"},
	}
}

func (s *Store) Put(ctx context.Context, in PutInput) (ObjectMeta, error) {
	var out ObjectMeta
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		out, err = s.PutTx(ctx, tx, in)
		return err
	})
	return out, err
}

func (s *Store) PutTx(ctx context.Context, tx *sql.Tx, in PutInput) (ObjectMeta, error) {
	if s == nil || s.db == nil {
		return ObjectMeta{}, errors.New("object store: nil database")
	}
	if tx == nil {
		return ObjectMeta{}, errors.New("object store: nil transaction")
	}
	if in.ContentType == "" {
		in.ContentType = "application/octet-stream"
	}
	content := append([]byte(nil), in.Content...)
	if content == nil {
		content = []byte{}
	}
	checksum := checksum(content)
	ref := "obj_sha256_" + checksum
	now := formatTime(time.Now())
	_, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO object_blobs (
  object_ref, tenant_id, owner_agent_id, checksum, size_bytes,
  content_type, content, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?)`,
		ref, nullable(in.OwnerAgent), checksum, len(content), in.ContentType, content, now,
	)
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("put object: %w", err)
	}
	return s.objectMetaTx(ctx, tx, ref)
}

func (s *Store) PutArtifact(ctx context.Context, in PutArtifactInput) (Artifact, error) {
	var out Artifact
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		out, err = s.PutArtifactTx(ctx, tx, in)
		return err
	})
	return out, err
}

func (s *Store) PutArtifactTx(ctx context.Context, tx *sql.Tx, in PutArtifactInput) (Artifact, error) {
	if in.OwnerAgent == "" {
		return Artifact{}, errors.New("artifact: owner agent is required")
	}
	meta, err := s.PutTx(ctx, tx, PutInput{
		OwnerAgent:  in.OwnerAgent,
		Content:     in.Content,
		ContentType: in.ContentType,
	})
	if err != nil {
		return Artifact{}, err
	}
	artifactID, err := ids.New("art")
	if err != nil {
		return Artifact{}, err
	}
	rawMetadata := []byte(`{}`)
	if in.Metadata != nil {
		rawMetadata, err = json.Marshal(in.Metadata)
	}
	if err != nil {
		return Artifact{}, err
	}
	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO artifacts (
  id, tenant_id, owner_agent_id, object_ref, checksum, size_bytes,
  content_type, metadata_json, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?)`,
		artifactID, in.OwnerAgent, meta.Ref, meta.Checksum, meta.SizeBytes,
		meta.ContentType, string(rawMetadata), now,
	); err != nil {
		return Artifact{}, fmt.Errorf("insert artifact: %w", err)
	}
	return Artifact{
		ID:          artifactID,
		OwnerAgent:  in.OwnerAgent,
		ObjectRef:   meta.Ref,
		Checksum:    meta.Checksum,
		SizeBytes:   meta.SizeBytes,
		ContentType: meta.ContentType,
		Metadata:    cloneStringMap(in.Metadata),
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func (s *Store) PutTranscript(ctx context.Context, in PutTranscriptInput) (Transcript, error) {
	var out Transcript
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		out, err = s.PutTranscriptTx(ctx, tx, in)
		return err
	})
	return out, err
}

func (s *Store) PutTranscriptTx(ctx context.Context, tx *sql.Tx, in PutTranscriptInput) (Transcript, error) {
	if in.AttemptID == "" {
		return Transcript{}, errors.New("transcript: attempt id is required")
	}
	ownerAgent, err := agentForAttempt(ctx, tx, in.AttemptID)
	if err != nil {
		return Transcript{}, err
	}
	meta, err := s.PutTx(ctx, tx, PutInput{
		OwnerAgent:  ownerAgent,
		Content:     in.Content,
		ContentType: in.ContentType,
	})
	if err != nil {
		return Transcript{}, err
	}
	transcriptID, err := ids.New("trn")
	if err != nil {
		return Transcript{}, err
	}
	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO transcripts (
  id, tenant_id, attempt_id, object_ref, checksum, size_bytes, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?)`,
		transcriptID, in.AttemptID, meta.Ref, meta.Checksum, meta.SizeBytes, now,
	); err != nil {
		return Transcript{}, fmt.Errorf("insert transcript: %w", err)
	}
	return Transcript{
		ID:        transcriptID,
		AttemptID: in.AttemptID,
		ObjectRef: meta.Ref,
		Checksum:  meta.Checksum,
		SizeBytes: meta.SizeBytes,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *Store) Inspect(ctx context.Context, subject capability.Subject, objectRef string) capability.Response[ObjectMeta] {
	meta, allowed, response := s.authorizedMeta(ctx, subject, objectRef)
	if response.Status != "" {
		return response
	}
	if !allowed {
		return accessDenied[ObjectMeta](objectRef)
	}
	return capability.Accepted(meta)
}

func (s *Store) Read(ctx context.Context, subject capability.Subject, objectRef string) capability.Response[ObjectContent] {
	meta, allowed, response := s.authorizedMeta(ctx, subject, objectRef)
	if response.Status != "" {
		return capability.Response[ObjectContent]{
			OK:                 response.OK,
			Status:             response.Status,
			ErrorCode:          response.ErrorCode,
			Message:            response.Message,
			RepairHint:         response.RepairHint,
			CanonicalIDs:       response.CanonicalIDs,
			Missing:            response.Missing,
			AllowedNextActions: response.AllowedNextActions,
			Retryable:          response.Retryable,
		}
	}
	if !allowed {
		return accessDenied[ObjectContent](objectRef)
	}
	row := s.db.QueryRowContext(ctx, `SELECT content FROM object_blobs WHERE object_ref = ?`, objectRef)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return capability.Error[ObjectContent]("OBJECT_READ_FAILED", err.Error(), false)
	}
	return capability.Accepted(ObjectContent{ObjectMeta: meta, Content: string(raw)})
}

func (s *Store) handleInspect(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	ref, err := decodeRef(call.Input)
	if err != nil {
		return invalidInput("object.inspect", err)
	}
	return responseToRaw(s.Inspect(ctx, call.Subject, ref))
}

func (s *Store) handleRead(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	ref, err := decodeRef(call.Input)
	if err != nil {
		return invalidInput("object.read", err)
	}
	return responseToRaw(s.Read(ctx, call.Subject, ref))
}

func (s *Store) authorizedMeta(ctx context.Context, subject capability.Subject, objectRef string) (ObjectMeta, bool, capability.Response[ObjectMeta]) {
	if objectRef == "" {
		return ObjectMeta{}, false, capability.Rejected[ObjectMeta](
			"OBJECT_REF_REQUIRED",
			"object_ref is required",
			capability.WithRepairHint("retry with an object_ref returned by report.submit, artifact upload, or session transcript"),
			capability.WithAllowedNextActions("object.inspect", "object.read"),
			capability.WithRetryable(false),
		)
	}
	meta, err := s.objectMeta(ctx, objectRef)
	if errors.Is(err, sql.ErrNoRows) {
		return ObjectMeta{}, false, notFound[ObjectMeta](objectRef)
	}
	if err != nil {
		return ObjectMeta{}, false, capability.Error[ObjectMeta]("OBJECT_INSPECT_FAILED", err.Error(), false)
	}
	return meta, s.canAccess(ctx, subject, meta), capability.Response[ObjectMeta]{}
}

func (s *Store) objectMeta(ctx context.Context, objectRef string) (ObjectMeta, error) {
	return scanObjectMeta(s.db.QueryRowContext(ctx, `
SELECT object_ref, COALESCE(owner_agent_id, ''), checksum, size_bytes, content_type, created_at
FROM object_blobs
WHERE object_ref = ?`, objectRef))
}

func (s *Store) objectMetaTx(ctx context.Context, tx *sql.Tx, objectRef string) (ObjectMeta, error) {
	return scanObjectMeta(tx.QueryRowContext(ctx, `
SELECT object_ref, COALESCE(owner_agent_id, ''), checksum, size_bytes, content_type, created_at
FROM object_blobs
WHERE object_ref = ?`, objectRef))
}

func (s *Store) canAccess(ctx context.Context, subject capability.Subject, meta ObjectMeta) bool {
	if subject.Kind == "operator" || subject.Kind == "debug" || subject.Kind == "system" {
		return true
	}
	agentID := subject.AgentID
	if agentID == "" && subject.Kind == "agent" {
		agentID = subject.ID
	}
	if agentID == "" {
		return false
	}
	if meta.OwnerAgent == agentID {
		return true
	}
	var found string
	err := s.db.QueryRowContext(ctx, `
SELECT object_ref
FROM artifacts
WHERE object_ref = ? AND owner_agent_id = ?
UNION
SELECT object_ref
FROM evidence
WHERE content_ref = ? AND produced_by = ?
UNION
SELECT t.object_ref
FROM transcripts t
JOIN attempts a ON a.id = t.attempt_id
JOIN leases l ON l.id = a.lease_id
WHERE t.object_ref = ? AND l.agent_id = ?
LIMIT 1`,
		meta.Ref, agentID,
		meta.Ref, agentID,
		meta.Ref, agentID,
	).Scan(&found)
	return err == nil
}

func scanObjectMeta(row interface{ Scan(...any) error }) (ObjectMeta, error) {
	var meta ObjectMeta
	var createdAt string
	if err := row.Scan(
		&meta.Ref,
		&meta.OwnerAgent,
		&meta.Checksum,
		&meta.SizeBytes,
		&meta.ContentType,
		&createdAt,
	); err != nil {
		return ObjectMeta{}, err
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return ObjectMeta{}, err
	}
	meta.CreatedAt = parsed
	return meta, nil
}

func decodeRef(raw json.RawMessage) (string, error) {
	var input refInput
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	if input.ObjectRef != "" {
		return input.ObjectRef, nil
	}
	return input.Ref, nil
}

func checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func agentForAttempt(ctx context.Context, tx *sql.Tx, attemptID string) (string, error) {
	var agentID string
	if err := tx.QueryRowContext(ctx, `
SELECT l.agent_id
FROM attempts a
JOIN leases l ON l.id = a.lease_id
WHERE a.id = ?`, attemptID).Scan(&agentID); err != nil {
		return "", fmt.Errorf("transcript: resolve attempt agent: %w", err)
	}
	return agentID, nil
}

func notFound[T any](objectRef string) capability.Response[T] {
	return capability.Rejected[T](
		"OBJECT_NOT_FOUND",
		"object_ref does not exist",
		capability.WithCanonicalID("object_ref", objectRef),
		capability.WithRepairHint("retry with a durable object_ref returned by CoordPlane"),
		capability.WithAllowedNextActions("object.inspect"),
		capability.WithRetryable(false),
	)
}

func accessDenied[T any](objectRef string) capability.Response[T] {
	return capability.Rejected[T](
		"OBJECT_ACCESS_DENIED",
		"subject is not allowed to read this object_ref",
		capability.WithCanonicalID("object_ref", objectRef),
		capability.WithRepairHint("ask the producing agent to send an authorized summary or artifact reference"),
		capability.WithAllowedNextActions("message.send", "object.inspect"),
		capability.WithRetryable(false),
	)
}

func invalidInput(capabilityName string, err error) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		"INVALID_CAPABILITY_INPUT",
		fmt.Sprintf("%s input is invalid: %v", capabilityName, err),
		capability.WithRepairHint("retry with a JSON object containing object_ref"),
		capability.WithAllowedNextActions(capabilityName),
		capability.WithRetryable(false),
	)
}

func responseToRaw[T any](response capability.Response[T]) capability.Response[json.RawMessage] {
	raw := capability.Response[json.RawMessage]{
		OK:                 response.OK,
		Status:             response.Status,
		ErrorCode:          response.ErrorCode,
		Message:            response.Message,
		RepairHint:         response.RepairHint,
		CanonicalIDs:       cloneStringMap(response.CanonicalIDs),
		Missing:            append([]capability.MissingRequirement(nil), response.Missing...),
		AllowedNextActions: append([]string(nil), response.AllowedNextActions...),
		Retryable:          response.Retryable,
	}
	if response.Data != nil {
		data, err := json.Marshal(response.Data)
		if err != nil {
			return capability.Error[json.RawMessage]("CAPABILITY_RESPONSE_ENCODE_FAILED", err.Error(), false)
		}
		rawData := json.RawMessage(data)
		raw.Data = &rawData
	}
	return raw
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
