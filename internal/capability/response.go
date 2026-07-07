package capability

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Status string

const (
	StatusAccepted Status = "accepted"
	StatusRejected Status = "rejected"
	StatusError    Status = "error"
)

type MissingRequirement struct {
	Kind   string `json:"kind"`
	Action string `json:"action"`
}

type Response[T any] struct {
	OK                 bool                 `json:"ok"`
	Status             Status               `json:"status"`
	Data               *T                   `json:"data,omitempty"`
	ErrorCode          string               `json:"error_code,omitempty"`
	Message            string               `json:"message,omitempty"`
	RepairHint         string               `json:"repair_hint,omitempty"`
	CanonicalIDs       map[string]string    `json:"canonical_ids,omitempty"`
	Missing            []MissingRequirement `json:"missing,omitempty"`
	AllowedNextActions []string             `json:"allowed_next_actions,omitempty"`
	Retryable          *bool                `json:"retryable,omitempty"`
}

func Accepted[T any](data T) Response[T] {
	return Response[T]{
		OK:     true,
		Status: StatusAccepted,
		Data:   &data,
	}
}

func AcceptedJSON(data any) (Response[json.RawMessage], error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Response[json.RawMessage]{}, fmt.Errorf("marshal accepted response data: %w", err)
	}
	return AcceptedRaw(raw), nil
}

func AcceptedRaw(data json.RawMessage) Response[json.RawMessage] {
	copied := append(json.RawMessage(nil), data...)
	return Response[json.RawMessage]{
		OK:     true,
		Status: StatusAccepted,
		Data:   &copied,
	}
}

type rejectedFields struct {
	repairHint         string
	canonicalIDs       map[string]string
	missing            []MissingRequirement
	allowedNextActions []string
	retryable          *bool
}

type RejectedOption func(*rejectedFields)

func Rejected[T any](code, message string, opts ...RejectedOption) Response[T] {
	fields := rejectedFields{}
	for _, opt := range opts {
		opt(&fields)
	}
	return Response[T]{
		OK:                 false,
		Status:             StatusRejected,
		ErrorCode:          code,
		Message:            message,
		RepairHint:         fields.repairHint,
		CanonicalIDs:       fields.canonicalIDs,
		Missing:            fields.missing,
		AllowedNextActions: fields.allowedNextActions,
		Retryable:          fields.retryable,
	}
}

func Error[T any](code, message string, retryable bool) Response[T] {
	return Response[T]{
		OK:        false,
		Status:    StatusError,
		ErrorCode: code,
		Message:   message,
		Retryable: boolPtr(retryable),
	}
}

func WithRepairHint(hint string) RejectedOption {
	return func(fields *rejectedFields) {
		fields.repairHint = hint
	}
}

func WithCanonicalID(key, value string) RejectedOption {
	return func(fields *rejectedFields) {
		if fields.canonicalIDs == nil {
			fields.canonicalIDs = make(map[string]string)
		}
		fields.canonicalIDs[key] = value
	}
}

func WithMissing(kind, action string) RejectedOption {
	return func(fields *rejectedFields) {
		fields.missing = append(fields.missing, MissingRequirement{Kind: kind, Action: action})
	}
}

func WithAllowedNextActions(actions ...string) RejectedOption {
	return func(fields *rejectedFields) {
		fields.allowedNextActions = append(fields.allowedNextActions, actions...)
	}
}

func WithRetryable(retryable bool) RejectedOption {
	return func(fields *rejectedFields) {
		fields.retryable = boolPtr(retryable)
	}
}

func (r Response[T]) Validate() error {
	switch r.Status {
	case StatusAccepted:
		if !r.OK {
			return errors.New("accepted response must set ok=true")
		}
		if r.Data == nil {
			return errors.New("accepted response must include data")
		}
		if r.ErrorCode != "" || r.Message != "" {
			return errors.New("accepted response must not include error fields")
		}
	case StatusRejected:
		if r.OK {
			return errors.New("rejected response must set ok=false")
		}
		if r.ErrorCode == "" {
			return errors.New("rejected response requires error_code")
		}
		if r.Message == "" {
			return errors.New("rejected response requires message")
		}
		if r.Retryable == nil {
			return errors.New("rejected response requires retryable")
		}
		if r.RepairHint == "" && len(r.Missing) == 0 && len(r.AllowedNextActions) == 0 {
			return errors.New("rejected response requires repair guidance")
		}
	case StatusError:
		if r.OK {
			return errors.New("error response must set ok=false")
		}
		if r.ErrorCode == "" || r.Message == "" {
			return errors.New("error response requires error_code and message")
		}
	default:
		return fmt.Errorf("unknown response status %q", r.Status)
	}
	return nil
}

func boolPtr(value bool) *bool {
	return &value
}
