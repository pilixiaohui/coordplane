package coordination

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestHighRiskCapabilitySchemaPropertiesMatchDecodedFields(t *testing.T) {
	tests := []struct {
		name   string
		schema json.RawMessage
		target any
	}{
		{name: "report.submit", schema: reportSubmitInputSchema, target: reportSubmitCallInput{}},
		{name: "contract.complete", schema: contractCompleteInputSchema, target: contractCompleteCallInput{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tc.schema, &schema); err != nil {
				t.Fatalf("decode schema: %v", err)
			}
			want := make([]string, 0, len(schema.Properties))
			for name := range schema.Properties {
				want = append(want, name)
			}
			sort.Strings(want)

			targetType := reflect.TypeOf(tc.target)
			got := make([]string, 0, targetType.NumField())
			for i := 0; i < targetType.NumField(); i++ {
				name := strings.Split(targetType.Field(i).Tag.Get("json"), ",")[0]
				if name != "" && name != "-" {
					got = append(got, name)
				}
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded JSON fields = %v, schema properties = %v", got, want)
			}
		})
	}
}
