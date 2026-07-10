package capability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeWithScopeStrict merges authenticated scope with caller input and
// rejects fields that are not declared by the target type.
func DecodeWithScopeStrict(scope, input json.RawMessage, target any) error {
	merged := make(map[string]any)
	if len(scope) > 0 {
		if err := json.Unmarshal(scope, &merged); err != nil {
			return fmt.Errorf("decode scope: %w", err)
		}
		if merged == nil {
			return errors.New("decode scope: expected a JSON object, got null")
		}
	}
	if len(input) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal(input, &decoded); err != nil {
			return err
		}
		if decoded == nil {
			return errors.New("decode input: expected a JSON object, got null")
		}
		for key, value := range decoded {
			merged[key] = value
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return DecodeStrict(raw, target)
}

func DecodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("decode input: expected a JSON object, got null")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
