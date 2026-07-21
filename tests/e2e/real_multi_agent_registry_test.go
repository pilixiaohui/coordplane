//go:build e2e

package e2e_test

import (
	"os"
	"testing"
)

type scenarioRequirements struct {
	Sources, DirectCAS, Integrations, Restarts, AckTransitions int
}

type scenarioSpec struct {
	ID           string
	Requirements scenarioRequirements
	Run          func(*testing.T)
}

var realMultiAgentScenarios = []scenarioSpec{{
	ID: "RMA-02",
	Requirements: scenarioRequirements{
		Sources: 4, DirectCAS: 1, Integrations: 3, Restarts: 1, AckTransitions: 1,
	},
	Run: runRMA02,
}}

func TestRealMultiAgentScenarios(t *testing.T) {
	if os.Getenv("E2E_REAL_MULTI_AGENT") != "1" {
		t.Fatal("real multi-Agent gate may only be entered through scripts/e2e-real-multi-agent.sh")
	}
	runScenarioSpecs(t, realMultiAgentScenarios)
}

func runScenarioSpecs(t *testing.T, scenarios []scenarioSpec) {
	t.Helper()
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.ID, scenario.Run)
	}
}
