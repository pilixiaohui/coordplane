package releasehealth

import (
	"path/filepath"
	"testing"

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
