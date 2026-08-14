package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"coordplane/internal/core"
)

func budgetPage[T any](items []T, limit int, key func(T) (string, string)) ([]T, string, error) {
	candidateCount := len(items)
	if candidateCount > limit {
		candidateCount = limit
	}
	bounded := make([]T, 0, candidateCount)
	used := 64
	for _, item := range items[:candidateCount] {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, "", core.WrapError(core.CodeInternal, "encode history page item", false, err)
		}
		if len(bounded) > 0 && used+len(raw)+1 > pageDataJSONBudget {
			break
		}
		if len(bounded) == 0 && used+len(raw) > pageDataJSONBudget {
			return nil, "", core.NewError(core.CodeInternal, "history item exceeds response budget", false)
		}
		bounded = append(bounded, item)
		used += len(raw) + 1
	}
	hasMore := len(items) > len(bounded)
	if !hasMore {
		return bounded, "", nil
	}
	createdAt, id := key(bounded[len(bounded)-1])
	return bounded, encodeCursor(createdAt, id), nil
}

func pageInputs(limit int, rawCursor string) (int, historyCursor, error) {
	normalized, err := core.NormalizePageLimit(limit)
	if err != nil {
		return 0, historyCursor{}, err
	}
	cursor, err := decodeCursor(strings.TrimSpace(rawCursor))
	if err != nil {
		return 0, historyCursor{}, err
	}
	return normalized, cursor, nil
}

func addCursor(query string, args []any, cursor historyCursor) (string, []any) {
	if cursor.ID == "" {
		return query, args
	}
	query += ` AND (created_at>? OR (created_at=? AND id>?))`
	return query, append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
}

func encodeCursor(createdAt, id string) string {
	raw, _ := json.Marshal(historyCursor{CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(raw string) (historyCursor, error) {
	if raw == "" {
		return historyCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, invalidCursor()
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor historyCursor
	if err := decoder.Decode(&cursor); err != nil {
		return historyCursor{}, invalidCursor()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return historyCursor{}, invalidCursor()
	}
	if strings.TrimSpace(cursor.ID) == "" || strings.TrimSpace(cursor.CreatedAt) == "" {
		return historyCursor{}, invalidCursor()
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
		return historyCursor{}, invalidCursor()
	}
	return cursor, nil
}

func invalidCursor() error {
	return core.NewError(core.CodeInvalidArgument, "cursor is invalid", false)
}

var _ core.Repository = (*Store)(nil)

const roleSelect = `SELECT id,name,description,capabilities,version,created_at,updated_at FROM roles`
const participantSelect = `SELECT id,kind,display_name,status,credential_id,adapter_id,image,instructions_file,model,subagent_model,base_url,effort,instructions_text,version,created_at,updated_at FROM participants`

type rowScanner interface {
	Scan(dest ...any) error
}
