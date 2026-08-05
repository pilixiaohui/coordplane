package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type testEvent struct {
	Action string
	Test   string
}

func main() {
	if err := validate(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "real multi-Agent gate evidence: %v\n", err)
		os.Exit(1)
	}
}

func validate(input io.Reader) error {
	const top, scenario = "TestRealMultiAgentScenarios", "TestRealMultiAgentScenarios/RMA-02"
	want := map[string]bool{top: true, scenario: true}
	ran, passed := map[string]int{}, map[string]int{}
	decoder := json.NewDecoder(input)
	for {
		var event testEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid go test JSON: %w", err)
		}
		if event.Test == "" {
			continue
		}
		if !want[event.Test] {
			return fmt.Errorf("unexpected live test %s", event.Test)
		}
		if event.Action == "skip" || event.Action == "fail" {
			return fmt.Errorf("live test %s ended with %s", event.Test, event.Action)
		}
		if event.Action == "run" {
			ran[event.Test]++
		}
		if event.Action == "pass" {
			passed[event.Test]++
		}
	}
	for name := range want {
		if ran[name] != 1 || passed[name] != 1 {
			return fmt.Errorf("live test %s did not run and pass exactly once", name)
		}
	}
	return nil
}
