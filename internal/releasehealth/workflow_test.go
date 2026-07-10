package releasehealth

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/capability"
	cpruntime "coordplane/internal/runtime"
)

func TestPrepareChangesetWorkspaceRootUsesContainerPathForDockerSessions(t *testing.T) {
	state := &workflowState{
		cfg: CPAccept001Config{WorkDir: filepath.Join(t.TempDir(), "release-health")},
		developer: cpruntime.AssignmentSession{
			ContainerName: "coordplane-cp-accept-developer",
			Route: cpruntime.SessionRoute{
				Workdir: cpruntime.ContainerWorkspacePath,
			},
		},
	}

	if got := state.prepareChangesetWorkspaceRoot(); got != cpruntime.ContainerWorkspacePath {
		t.Fatalf("prepareChangeset workspace root = %q, want container-visible Docker root %q", got, cpruntime.ContainerWorkspacePath)
	}
}

func TestPrepareChangesetWorkspaceRootFallsBackToReleaseHealthGitWorkspaces(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "release-health")
	state := &workflowState{cfg: CPAccept001Config{WorkDir: workDir}}

	want := filepath.Join(workDir, "git-workspaces")
	if got := state.prepareChangesetWorkspaceRoot(); got != want {
		t.Fatalf("prepareChangeset workspace root = %q, want %q", got, want)
	}
}

func TestDriverCallPassesActiveLeaseIDToCoordlink(t *testing.T) {
	exec := &recordingWorkflowExecutor{
		result: cpruntime.ContainerExecResult{
			ExitCode: 0,
			Stdout: mustJSON(t, typedResponse{
				OK:     true,
				Status: capability.StatusAccepted,
			}),
		},
	}
	driver := &driver{executor: exec}
	session := cpruntime.AssignmentSession{
		LeaseID:       "lease_active",
		ContainerName: "coordplane-cp-accept-coordinator",
		Env:           map[string]string{"COORDPLANE_LEASE_ID": "lease_active"},
	}

	if _, err := driver.callAny(context.Background(), session, "contract.add", map[string]any{
		"title":           "child",
		"objective":       "child work",
		"target_agent_id": "developer",
	}, "rh-add-child"); err != nil {
		t.Fatalf("driver call: %v", err)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("exec specs = %d, want 1", len(exec.specs))
	}
	command := strings.Join(exec.specs[0].Command, "\x00")
	if !strings.Contains(command, "\x00--lease-id\x00lease_active") {
		t.Fatalf("coordlink command missing explicit lease id: %#v", exec.specs[0].Command)
	}
}

type recordingWorkflowExecutor struct {
	result cpruntime.ContainerExecResult
	err    error
	specs  []cpruntime.ContainerExecSpec
}

func (e *recordingWorkflowExecutor) Exec(ctx context.Context, spec cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	e.specs = append(e.specs, spec)
	return e.result, e.err
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}
