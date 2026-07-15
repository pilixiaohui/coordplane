//go:build docker

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func TestReconcileRefusesToStartInsecureSameLabelContainer(t *testing.T) {
	fixture := newP3DockerFixture(t)
	agent := fixture.addAgent(t, "Adoption Fence")
	project := fixture.addProject(t, agent.ID)
	task := fixture.addTask(t, project.ID, agent.ID, "reject insecure adoption", 0)
	claim, ok, err := fixture.components.service.ClaimNext(fixture.ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("claim = %#v ok=%t err=%v", claim, ok, err)
	}
	workspacePath, err := fixture.components.runtime.workspaces.Path(project.ID, task.ID)
	requireNoError(t, err)
	launch, err := fixture.components.service.RuntimeLaunchContext(fixture.ctx, claim.Run.ID)
	requireNoError(t, err)
	instructions, instructionsHash, err := readInstructions(launch.Agent.InstructionsFile)
	requireNoError(t, err)
	workspaceSpec, err := gitWorkspaceSpec(task)
	requireNoError(t, err)
	prepared, err := fixture.components.service.BeginRunLaunch(fixture.ctx, core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-insecure-adoption",
		WorkspacePath: workspacePath, HomePath: filepath.Join(fixture.components.config.Runtime.AgentHomeRoot, agent.ID),
		LogPath:          filepath.Join(fixture.components.config.Runtime.LogRoot, claim.Run.ID, "run.log"),
		InstructionsHash: instructionsHash, LaunchMode: "start",
		CleanupOperationID: "cleanup-insecure-adoption", RequestID: runtimeRequest(claim.Run, "prepare"),
	})
	requireNoError(t, err)
	requireNoError(t, fixture.components.runtime.prepareWorkspace(fixture.ctx, prepared, workspaceSpec))
	controlPath := filepath.Join(fixture.components.runtime.controlRoot, prepared.ID)
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{
		{prepared.HomePath, 0o2770},
		{filepath.Dir(prepared.LogPath), 0o700},
		{controlPath, runControlDirectoryMode},
	} {
		requireNoError(t, ensureRuntimeDirectory(directory.path, directory.mode))
	}
	bootstrap := buildBootstrap(launch, prepared, instructions, workspacePath, workspaceSpec)
	requireNoError(t, writeRunControlMarker(controlPath, prepared))
	requireNoError(t, writeRuntimeFile(filepath.Join(controlPath, "token"), []byte(claim.Token+"\n"), runControlFileMode))
	requireNoError(t, writeRuntimeFile(filepath.Join(controlPath, "bootstrap"), []byte(bootstrap), runControlFileMode))
	labels := map[string]string{
		containerruntime.LabelManaged:     "true",
		containerruntime.LabelContract:    "v1",
		containerruntime.LabelProjectID:   prepared.ProjectID,
		containerruntime.LabelTaskID:      prepared.TaskID,
		containerruntime.LabelAgentID:     prepared.AgentID,
		containerruntime.LabelRunID:       prepared.ID,
		containerruntime.LabelGeneration:  strconv.FormatInt(prepared.Generation, 10),
		containerruntime.LabelLaunchNonce: prepared.LaunchNonce,
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := []string{
		"create", "--name", prepared.ContainerName, "--network", preparedNetwork(fixture),
		"--workdir", "/workspace/project", "--user", "0:0", "--privileged",
	}
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, fixture.image, "/bin/sleep", "30")
	create := exec.CommandContext(fixture.ctx, "docker", args...)
	create.Env = os.Environ()
	if raw, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create insecure same-label container: %v\n%s", err, raw)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = fixture.executor.Remove(cleanup, runtimeRef(prepared))
	})
	before, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(prepared))
	if err != nil || before.Status != containerruntime.StatusCreated || before.Running {
		t.Fatalf("malicious fixture before reconcile = %#v err=%v", before, err)
	}
	requireNoError(t, fixture.components.runtime.Reconcile(fixture.ctx))
	healthy, reason := fixture.components.runtime.Healthy()
	if healthy || reason == "" {
		t.Fatalf("insecure adoption did not degrade runtime: healthy=%t reason=%q", healthy, reason)
	}
	after, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(prepared))
	if err != nil || after.Status != containerruntime.StatusCreated || after.Running {
		t.Fatalf("insecure container was started: %#v err=%v", after, err)
	}
	persisted, err := fixture.components.service.Run(fixture.ctx, prepared.ID)
	requireNoError(t, err)
	if persisted.ContainerID == "" || persisted.LaunchPhase != core.LaunchCreated || persisted.State != core.RunStarting {
		t.Fatalf("durable Run did not stop at the rejected adoption boundary: %#v", persisted)
	}
}

func preparedNetwork(fixture *p3DockerFixture) string {
	if fixture.components.config.Runtime.DockerNetwork == "" {
		return "none"
	}
	return fixture.components.config.Runtime.DockerNetwork
}
