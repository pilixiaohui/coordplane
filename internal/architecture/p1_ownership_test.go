package architecture_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestP1ProductionDoesNotOwnDeferredOutcomeOrRedeliveryPaths(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		"OutcomeInput",
		"RequestOutcome",
		"RecordRunTerminal",
		"NewRunServer",
		"/v1/task/outcome",
		"message.redelivered",
		"message.rerouted",
	}
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range forbidden {
			if strings.Contains(string(raw), token) {
				offenders = append(offenders, filepath.ToSlash(rel)+": "+token)
			}
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("P1 production owns deferred outcome/redelivery paths:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestP1RunProjectionDoesNotMutateMessages(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, "internal", "core", "run.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "UpdateMessage") {
		t.Fatal("internal/core/run.go mutates message delivery; ownership belongs to a later stage")
	}
}
