package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"coordplane/internal/core"
)

func (s *Store) EventsPage(ctx context.Context, filter core.EventFilter) (core.EventPage, error) {
	limit, err := core.NormalizeEventPageLimit(filter.Limit)
	if err != nil {
		return core.EventPage{}, err
	}
	beforeID, err := decodeEventCursor(strings.TrimSpace(filter.Cursor))
	if err != nil {
		return core.EventPage{}, err
	}
	query, args := eventQuery(filter)
	if beforeID > 0 {
		query += ` AND id<?`
		args = append(args, beforeID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return core.EventPage{}, err
	}
	items, err := collectEvents(rows)
	if err != nil {
		return core.EventPage{}, err
	}
	bounded, next, err := budgetEventPage(items, limit)
	if err != nil {
		return core.EventPage{}, err
	}
	return core.EventPage{Items: bounded, NextCursor: next}, nil
}

func budgetEventPage(items []core.Event, limit int) ([]core.Event, string, error) {
	candidateCount := len(items)
	if candidateCount > limit {
		candidateCount = limit
	}
	bounded := make([]core.Event, 0, candidateCount)
	used := 64
	for _, item := range items[:candidateCount] {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, "", core.WrapError(core.CodeInternal, "encode event page item", false, err)
		}
		if len(bounded) > 0 && used+len(raw)+1 > pageDataJSONBudget {
			break
		}
		if len(bounded) == 0 && used+len(raw) > pageDataJSONBudget {
			return nil, "", core.NewError(core.CodeInternal, "event exceeds response budget", false)
		}
		bounded = append(bounded, item)
		used += len(raw) + 1
	}
	if len(items) > len(bounded) {
		next := encodeEventCursor(bounded[len(bounded)-1].ID)
		reverseEvents(bounded)
		return bounded, next, nil
	}
	reverseEvents(bounded)
	return bounded, "", nil
}

func reverseEvents(events []core.Event) {
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
}

func encodeEventCursor(beforeID int64) string {
	raw, _ := json.Marshal(eventCursor{BeforeID: beforeID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeEventCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, invalidCursor()
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor eventCursor
	if err := decoder.Decode(&cursor); err != nil {
		return 0, invalidCursor()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || cursor.BeforeID <= 0 {
		return 0, invalidCursor()
	}
	return cursor.BeforeID, nil
}

func (s *Store) Events(ctx context.Context, filter core.EventFilter) ([]core.Event, error) {
	if strings.TrimSpace(filter.Cursor) != "" {
		page, err := s.EventsPage(ctx, filter)
		return page.Items, err
	}
	query, args := eventQuery(filter)
	if filter.Limit > 0 {
		query += ` ORDER BY id DESC LIMIT ?`
		args = append(args, filter.Limit)
	} else {
		query += ` ORDER BY id`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	events, err := collectEvents(rows)
	if err != nil {
		return nil, err
	}
	if filter.Limit > 0 {
		reverseEvents(events)
	}
	return events, nil
}

func eventQuery(filter core.EventFilter) (string, []any) {
	query := eventSelect + ` WHERE 1=1`
	var args []any
	for _, value := range []struct{ column, value string }{
		{"project_id", filter.ProjectID}, {"entity_type", filter.EntityType}, {"entity_id", filter.EntityID},
		{"run_id", filter.RunID}, {"kind", filter.Kind},
	} {
		if trimmed := strings.TrimSpace(value.value); trimmed != "" {
			query += ` AND ` + value.column + `=?`
			args = append(args, trimmed)
		}
	}
	return query, args
}
