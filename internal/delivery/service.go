package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Steerer interface {
	SteerMailbox(ctx context.Context, routeID, mailboxID string) error
}

type SteerCapabilityProvider interface {
	SameTurnSteerCapability(ctx context.Context, routeID string) (cliBackend string, supportsSameTurnSteer bool, known bool, err error)
}

type Service struct {
	db            *sql.DB
	store         *store.Store
	steerer       Steerer
	communication teamconfig.CommunicationConfig
}

type Result struct {
	MailboxID         string `json:"mailbox_id"`
	DeliveryAttemptID string `json:"delivery_attempt_id"`
	State             string `json:"state"`
	RouteID           string `json:"route_id,omitempty"`
	QueueItemID       string `json:"queue_item_id,omitempty"`
}

type Attempt struct {
	ID         string          `json:"id"`
	MailboxID  string          `json:"mailbox_id"`
	RouteID    string          `json:"route_id,omitempty"`
	SignalJSON json.RawMessage `json:"signal_json"`
	State      string          `json:"state"`
	LastError  string          `json:"last_error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type mailbox struct {
	ID               string
	RecipientAgentID string
	EnvelopeID       string
	EnvelopeKind     string
	EnvelopeSummary  string
	EnvelopeBody     string
	Reason           string
	ThreadID         string
	MessageID        string
	ContractID       string
	SessionRouteID   string
	State            string
	TriggerTurn      bool
}

type route struct {
	ID         string
	AgentID    string
	CLIBackend string
	State      string
}

type steerCapability struct {
	CLIBackend            string
	SupportsSameTurnSteer bool
	Known                 bool
}

func NewService(s *store.Store, steerer Steerer) (*Service, error) {
	return NewServiceWithCommunication(s, steerer, teamconfig.DefaultCommunicationConfig())
}

func NewServiceWithCommunication(s *store.Store, steerer Steerer, communication teamconfig.CommunicationConfig) (*Service, error) {
	if s == nil {
		return nil, errors.New("delivery service: store is nil")
	}
	if steerer == nil {
		return nil, errors.New("delivery service: steerer is nil")
	}
	return &Service{db: s.DB(), store: s, steerer: steerer, communication: communication.Normalized()}, nil
}

func (s *Service) NotifyMailbox(ctx context.Context, mailboxID string) (Result, error) {
	if mailboxID == "" {
		return Result{}, errors.New("delivery: mailbox id is required")
	}
	item, err := s.mailbox(ctx, mailboxID)
	if err != nil {
		return Result{}, err
	}
	signal, err := renderSignal(item, s.communication)
	if err != nil {
		return Result{}, err
	}
	if !item.TriggerTurn {
		attemptID, err := s.recordAttempt(ctx, item.ID, "", signal, "suppressed", "trigger_turn disabled")
		if err != nil {
			return Result{}, err
		}
		return Result{MailboxID: item.ID, DeliveryAttemptID: attemptID, State: "suppressed"}, nil
	}
	activeRoute, ok, err := s.activeRoute(ctx, item)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		attemptID, err := s.recordAttempt(ctx, item.ID, "", signal, "fallback", "no active route")
		if err != nil {
			return Result{}, err
		}
		fallbackRoute, _, err := s.fallbackRoute(ctx, item)
		if err != nil {
			return Result{}, err
		}
		capability := s.steerCapability(ctx, fallbackRoute.ID)
		queueID, err := s.enqueueFallback(ctx, item, fallbackRoute.ID, "no_active_route", capability)
		if err != nil {
			return Result{}, err
		}
		return Result{MailboxID: item.ID, DeliveryAttemptID: attemptID, State: "fallback", QueueItemID: queueID}, nil
	}
	attemptID, err := s.recordAttempt(ctx, item.ID, activeRoute.ID, signal, "queued", "")
	if err != nil {
		return Result{}, err
	}
	if err := s.steerer.SteerMailbox(ctx, activeRoute.ID, item.ID); err != nil {
		if updateErr := s.updateAttempt(ctx, attemptID, "failed", err.Error()); updateErr != nil {
			return Result{}, updateErr
		}
		capability := s.steerCapability(ctx, activeRoute.ID)
		reason := "steer_failed"
		if capability.Known && !capability.SupportsSameTurnSteer {
			reason = "steer_unsupported"
		}
		queueID, queueErr := s.enqueueFallback(ctx, item, activeRoute.ID, reason, capability)
		if queueErr != nil {
			return Result{}, queueErr
		}
		return Result{MailboxID: item.ID, DeliveryAttemptID: attemptID, State: "failed", RouteID: activeRoute.ID, QueueItemID: queueID}, nil
	}
	if err := s.updateAttempt(ctx, attemptID, "accepted", ""); err != nil {
		return Result{}, err
	}
	return Result{MailboxID: item.ID, DeliveryAttemptID: attemptID, State: "accepted", RouteID: activeRoute.ID}, nil
}

func (s *Service) Attempt(ctx context.Context, attemptID string) (Attempt, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, mailbox_item_id, COALESCE(route_id, ''), signal_json, state,
  COALESCE(last_error, ''), created_at, updated_at
FROM delivery_attempts
WHERE id = ?`, attemptID)
	var out Attempt
	var signalRaw string
	var createdRaw, updatedRaw string
	if err := row.Scan(
		&out.ID,
		&out.MailboxID,
		&out.RouteID,
		&signalRaw,
		&out.State,
		&out.LastError,
		&createdRaw,
		&updatedRaw,
	); err != nil {
		return Attempt{}, err
	}
	out.SignalJSON = json.RawMessage(signalRaw)
	createdAt, err := parseTime(createdRaw)
	if err != nil {
		return Attempt{}, err
	}
	updatedAt, err := parseTime(updatedRaw)
	if err != nil {
		return Attempt{}, err
	}
	out.CreatedAt = createdAt
	out.UpdatedAt = updatedAt
	return out, nil
}

func (s *Service) mailbox(ctx context.Context, mailboxID string) (mailbox, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT m.id, COALESCE(m.recipient_agent_id, ''), COALESCE(m.envelope_id, ''),
  COALESCE(e.kind, ''), COALESCE(e.summary, ''), COALESCE(e.body_inline, ''),
  m.reason, COALESCE(m.thread_id, ''), COALESCE(m.message_id, ''),
  COALESCE(m.contract_id, ''), COALESCE(m.session_route_id, ''), m.state,
  COALESCE(m.trigger_turn, 1)
FROM mailbox_items m
LEFT JOIN agent_communication_envelopes e ON e.id = m.envelope_id
WHERE m.id = ?`, mailboxID)
	var item mailbox
	var triggerTurn int
	err := row.Scan(
		&item.ID,
		&item.RecipientAgentID,
		&item.EnvelopeID,
		&item.EnvelopeKind,
		&item.EnvelopeSummary,
		&item.EnvelopeBody,
		&item.Reason,
		&item.ThreadID,
		&item.MessageID,
		&item.ContractID,
		&item.SessionRouteID,
		&item.State,
		&triggerTurn,
	)
	item.TriggerTurn = triggerTurn != 0
	return item, err
}

func (s *Service) activeRoute(ctx context.Context, item mailbox) (route, bool, error) {
	if item.SessionRouteID != "" {
		active, ok, err := s.routeByID(ctx, item.SessionRouteID, item.RecipientAgentID)
		if err != nil || !ok || active.State != "active" {
			return route{}, false, err
		}
		return active, true, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, agent_id, state
FROM session_routes
WHERE agent_id = ? AND state = 'active'
ORDER BY updated_at DESC, created_at DESC
LIMIT 1`, item.RecipientAgentID)
	if err != nil {
		return route{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return route{}, false, rows.Err()
	}
	var active route
	if err := rows.Scan(&active.ID, &active.AgentID, &active.State); err != nil {
		return route{}, false, err
	}
	return active, true, rows.Err()
}

func (s *Service) routeByID(ctx context.Context, routeID, agentID string) (route, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, agent_id, cli_backend, state
FROM session_routes
WHERE id = ? AND agent_id = ?`, routeID, agentID)
	var active route
	if err := row.Scan(&active.ID, &active.AgentID, &active.CLIBackend, &active.State); errors.Is(err, sql.ErrNoRows) {
		return route{}, false, nil
	} else if err != nil {
		return route{}, false, err
	}
	return active, true, nil
}

func (s *Service) fallbackRoute(ctx context.Context, item mailbox) (route, bool, error) {
	if item.SessionRouteID == "" {
		return route{}, false, nil
	}
	return s.routeByID(ctx, item.SessionRouteID, item.RecipientAgentID)
}

func (s *Service) steerCapability(ctx context.Context, routeID string) steerCapability {
	if routeID == "" {
		return steerCapability{}
	}
	provider, ok := s.steerer.(SteerCapabilityProvider)
	if !ok {
		return steerCapability{}
	}
	backend, supports, known, err := provider.SameTurnSteerCapability(ctx, routeID)
	if err != nil || !known {
		return steerCapability{CLIBackend: backend}
	}
	return steerCapability{CLIBackend: backend, SupportsSameTurnSteer: supports, Known: true}
}

func renderSignal(item mailbox, communication teamconfig.CommunicationConfig) (json.RawMessage, error) {
	policy := communication.Normalized()
	summary := safeSignalPreview(item.EnvelopeSummary, policy.SignalSummaryLimit())
	bodyPreview := safeSignalPreview(item.EnvelopeBody, policy.SignalBodyLimit())
	raw, err := json.Marshal(map[string]any{
		"type":            "coordplane.mailbox_signal",
		"reason":          "new_mailbox_items",
		"mailbox_count":   1,
		"mailbox_id":      item.ID,
		"envelope_id":     item.EnvelopeID,
		"envelope_kind":   item.EnvelopeKind,
		"summary":         summary,
		"body_preview":    bodyPreview,
		"mailbox_reason":  item.Reason,
		"trigger_turn":    item.TriggerTurn,
		"required_action": "call mailbox.list/get and communication.read",
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

var sensitiveSignalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bcoordplane_token\b\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)\b(token|secret|password|api[_-]?key)\b\s*[:=]\s*\S+`),
	regexp.MustCompile(`/home/[^\s,;]+`),
	regexp.MustCompile(`/tmp/[^\s,;]+`),
	regexp.MustCompile(`[^\s,;]+\.db\b`),
}

func safeSignalPreview(value string, limit int) string {
	return limitString(redactSensitivePreview(value), limit)
}

func redactSensitivePreview(value string) string {
	redacted := value
	for _, pattern := range sensitiveSignalPatterns {
		redacted = pattern.ReplaceAllString(redacted, "[redacted]")
	}
	for _, marker := range []string{
		"COORDPLANE_TOKEN",
		"coordplane_token",
		"SECRET",
		"secret",
		"TOKEN",
		"token",
		"/home/",
		"/tmp/",
		".db",
	} {
		redacted = strings.ReplaceAll(redacted, marker, "[redacted]")
	}
	return redacted
}

func limitString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (s *Service) recordAttempt(ctx context.Context, mailboxID, routeID string, signal json.RawMessage, state, lastError string) (string, error) {
	attemptID, err := ids.New("del")
	if err != nil {
		return "", err
	}
	now := formatTime(time.Now())
	_, err = s.db.ExecContext(ctx, `
INSERT INTO delivery_attempts (
  id, tenant_id, mailbox_item_id, route_id, signal_json, state, last_error,
  created_at, updated_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?)`,
		attemptID, mailboxID, nullable(routeID), string(signal), state, nullable(lastError), now, now,
	)
	return attemptID, err
}

func (s *Service) updateAttempt(ctx context.Context, attemptID, state, lastError string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE delivery_attempts SET state = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		state, nullable(lastError), formatTime(time.Now()), attemptID,
	)
	return err
}

func (s *Service) enqueueFallback(ctx context.Context, item mailbox, routeID, reason string, capability steerCapability) (string, error) {
	itemID, err := ids.New("queue")
	if err != nil {
		return "", err
	}
	payload := "mailbox:" + item.ID
	idempotencyKey := "fallback:" + item.ID
	now := formatTime(time.Now())
	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO queue_items (
  id, tenant_id, queue_name, kind, payload_ref, state, next_run_at,
  idempotency_key, priority, created_at, updated_at
) VALUES (?, 'default', 'runtime.resume', 'mailbox.resume', ?, 'queued', ?, ?, 0, ?, ?)`,
		itemID, payload, now, idempotencyKey, now, now,
	)
	if err != nil {
		return "", err
	}
	if rows, _ := result.RowsAffected(); rows == 1 {
		if err := s.recordResumeQueued(ctx, item, routeID, reason, itemID, capability); err != nil {
			return "", err
		}
		return itemID, nil
	}
	var existing string
	if err := s.db.QueryRowContext(ctx, `
SELECT id FROM queue_items
WHERE queue_name = 'runtime.resume' AND idempotency_key = ?`,
		idempotencyKey,
	).Scan(&existing); err != nil {
		return "", err
	}
	if err := s.recordResumeQueued(ctx, item, routeID, reason, existing, capability); err != nil {
		return "", err
	}
	return existing, nil
}

func (s *Service) recordResumeQueued(ctx context.Context, item mailbox, routeID, reason, queueID string, capability steerCapability) error {
	payload := map[string]any{
		"mailbox_id":    item.ID,
		"route_id":      routeID,
		"reason":        reason,
		"queue_item_id": queueID,
		"cli_backend":   capability.CLIBackend,
	}
	if capability.Known {
		payload["supports_same_turn_steer"] = capability.SupportsSameTurnSteer
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.store.AppendEvent(ctx, events.Event{
		TenantID:       "default",
		SubjectKind:    "agent",
		SubjectID:      item.RecipientAgentID,
		AgentID:        item.RecipientAgentID,
		CapabilityName: "delivery.resume",
		Type:           "delivery.resume_queued",
		AggregateType:  "mailbox_item",
		AggregateID:    item.ID,
		PayloadJSON:    raw,
	})
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}
