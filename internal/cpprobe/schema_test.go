package cpprobe_test

import (
	"strings"
	"testing"

	"coordplane/internal/cpprobe"
)

func TestGitOperationSummaryRequiresRuntimeIDForAgentOperations(t *testing.T) {
	summary := cpprobe.GitOperationSummary{
		Scenario: cpprobe.ScenarioID,
		Operations: []cpprobe.GitOperationBrief{{
			ID:                "gitop_missing_runtime",
			OperationType:     "workspace.prepare",
			ActorAgentID:      "developer-a",
			WorkspaceID:       "ws_1",
			RepoID:            "repo_1",
			ExecutionLocation: "runtime_container",
			AfterRef:          "abc123",
			State:             "succeeded",
		}},
	}
	err := summary.Validate()
	if err == nil || !strings.Contains(err.Error(), "requires runtime_id") {
		t.Fatalf("Validate() error = %v, want missing runtime_id rejection", err)
	}

	summary.Operations[0].SubjectKind = "operator_debug"
	if err := summary.Validate(); err != nil {
		t.Fatalf("operator/debug operation without runtime_id should be explicit and valid: %v", err)
	}
}
