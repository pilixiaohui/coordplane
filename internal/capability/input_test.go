package capability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeWithScopeStrictRejectsExplicitNullObjects(t *testing.T) {
	tests := []struct {
		name  string
		scope json.RawMessage
		input json.RawMessage
		want  string
	}{
		{name: "null input", scope: json.RawMessage(`{"lease_id":"lease_1"}`), input: json.RawMessage(`null`), want: "input"},
		{name: "null scope", scope: json.RawMessage(`null`), input: json.RawMessage(`{"summary":"done"}`), want: "scope"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var target struct {
				LeaseID string `json:"lease_id"`
				Summary string `json:"summary"`
			}
			err := DecodeWithScopeStrict(tc.scope, tc.input, &target)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "object") {
				t.Fatalf("DecodeWithScopeStrict error = %v, want %s object rejection", err, tc.want)
			}
		})
	}
}

func TestDecodeStrictRejectsExplicitNull(t *testing.T) {
	var target struct {
		Summary string `json:"summary"`
	}
	if err := DecodeStrict(json.RawMessage(`null`), &target); err == nil {
		t.Fatal("DecodeStrict accepted explicit null")
	}
}
