package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type testEvent struct{ Action, Test string }

var expected = map[string]bool{"TestRealClaudeAdapterSmoke": true, "TestRealClaudeTwoAgentConvergence": true}

func main() {
	decoder := json.NewDecoder(os.Stdin)
	ran, passed := make(map[string]int), make(map[string]int)
	for {
		var event testEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			fail("invalid go test JSON: %v", err)
		}
		if event.Test == "" {
			continue
		}
		top := strings.SplitN(event.Test, "/", 2)[0]
		if !expected[top] {
			fail("unexpected live test %s", event.Test)
		}
		if event.Action == "skip" || event.Action == "fail" {
			fail("live test %s ended with %s", event.Test, event.Action)
		}
		if event.Test == top && event.Action == "run" {
			ran[top]++
		}
		if event.Test == top && event.Action == "pass" {
			passed[top]++
		}
	}
	for name := range expected {
		if ran[name] != 1 || passed[name] != 1 {
			fail("live test %s did not run and pass exactly once", name)
		}
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "real gate evidence: "+format+"\n", args...)
	os.Exit(1)
}
