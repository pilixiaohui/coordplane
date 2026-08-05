package main

import (
	"strings"
	"testing"
)

func TestValidateRequiresOneTopLevelAndOneRMA02Execution(t *testing.T) {
	const top = "TestRealMultiAgentScenarios"
	const child = top + "/RMA-02"
	exact := `{"Action":"run","Test":"` + top + `"}` + "\n" +
		`{"Action":"run","Test":"` + child + `"}` + "\n" +
		`{"Action":"pass","Test":"` + child + `"}` + "\n" +
		`{"Action":"pass","Test":"` + top + `"}`
	tests := []struct {
		name, input string
		pass        bool
	}{
		{name: "exact", input: exact, pass: true},
		{name: "missing scenario", input: `{"Action":"run","Test":"` + top + `"}` + "\n" + `{"Action":"pass","Test":"` + top + `"}`},
		{name: "duplicate scenario", input: exact + "\n" + `{"Action":"run","Test":"` + child + `"}`},
		{name: "scenario skip", input: exact + "\n" + `{"Action":"skip","Test":"` + child + `"}`},
		{name: "top failure", input: `{"Action":"fail","Test":"` + top + `"}`},
		{name: "unexpected test", input: exact + "\n" + `{"Action":"run","Test":"TestOther"}`},
		{name: "invalid json", input: "{"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validate(strings.NewReader(test.input))
			if (err == nil) != test.pass {
				t.Fatalf("validate() error = %v, pass=%t", err, test.pass)
			}
		})
	}
}
