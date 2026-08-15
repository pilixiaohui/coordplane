//go:build docker

package daemon

import (
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	workspacePath := requireRuntimeValue(fixture.components.runtime.workspaces.Path(project.ID, task.ID))
	launch := requireRuntimeValue(fixture.components.service.RuntimeLaunchContext(fixture.ctx, claim.Run.ID))
	instructions, instructionsHash, err := readInstructions(launch.Agent)
	requireNoError(t, err)
	workspaceSpec := requireRuntimeValue(gitWorkspaceSpec(task))
	prepared := requireRuntimeValue(fixture.components.service.BeginRunLaunch(fixture.ctx, core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: "nonce-insecure-adoption",
		WorkspacePath: workspacePath, HomePath: filepath.Join(fixture.components.config.Runtime.AgentHomeRoot, agent.ID),
		LogPath:          filepath.Join(fixture.components.config.Runtime.LogRoot, claim.Run.ID, "run.log"),
		InstructionsHash: instructionsHash, LaunchMode: "start",
		ConfigFingerprint:  launch.ConfigFingerprint,
		CleanupOperationID: "cleanup-insecure-adoption", RequestID: runtimeRequest(claim.Run, "prepare"),
	}))
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
	network := cmp.Or(fixture.components.config.Runtime.DockerNetwork, "none")
	args := []string{
		"create", "--name", prepared.ContainerName, "--network", network,
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
	persisted := requireRuntimeValue(fixture.components.service.Run(fixture.ctx, prepared.ID))
	if persisted.ContainerID == "" || persisted.LaunchPhase != core.LaunchCreated || persisted.State != core.RunStarting {
		t.Fatalf("durable Run did not stop at the rejected adoption boundary: %#v", persisted)
	}
}

// R13-R15: a reconcile whose control lineage is missing or corrupt must fail
// closed. The launch and secrets files are checked by the lineage gate before
// the container starts; a tampered instructions file passes the gate (its mode
// is intact) but breaks the hash reconciliation, so the failure surfaces at
// monitor construction after start is issued. Every path must terminate the run
// without persisting unredacted content.
func TestReconcileFailsClosedOnCorruptControlLineage(t *testing.T) {
	canary := "CORRUPT-LINEAGE-CANARY"
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		phase  string
	}{
		{
			name: "launch missing",
			mutate: func(t *testing.T, controlPath string) {
				requireNoError(t, os.Remove(filepath.Join(controlPath, runtimeLaunchFile)))
			},
			phase: core.LaunchCreated,
		},
		{
			name: "secrets corrupt",
			mutate: func(t *testing.T, controlPath string) {
				requireNoError(t, os.Chmod(filepath.Join(controlPath, runtimeSecretsFile), 0o640))
			},
			phase: core.LaunchCreated,
		},
		{
			name: "instructions hash mismatch",
			mutate: func(t *testing.T, controlPath string) {
				path := filepath.Join(controlPath, runtimeInstructionsFile)
				requireNoError(t, os.Chmod(path, 0o640))
				requireNoError(t, os.WriteFile(path, []byte("tampered prompt "+canary), 0o440))
				requireNoError(t, os.Chmod(path, 0o440))
			},
			phase: core.LaunchStartIssued,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "lineage-provider-canary")
			fixture := newP3DockerFixtureWithProviderEnv(t, []string{"ANTHROPIC_AUTH_TOKEN"})
			agent := fixture.addAgent(t, "Lineage Fail Closed")
			project := fixture.addProject(t, agent.ID)
			fixture.addTask(t, project.ID, agent.ID, "corrupt lineage", 0)
			created := claimPreparedRun(t, fixture, project.ID)
			controlPath := filepath.Join(fixture.components.runtime.controlRoot, created.ID)
			test.mutate(t, controlPath)

			err := fixture.components.runtime.Reconcile(fixture.ctx)
			if err == nil {
				t.Fatal("corrupt control lineage reconcile did not fail")
			}
			healthy, reason := fixture.components.runtime.Healthy()
			if healthy || reason == "" {
				t.Fatalf("corrupt lineage did not degrade runtime: healthy=%t reason=%q", healthy, reason)
			}
			if _, err := fixture.executor.Inspect(fixture.ctx, runtimeRef(created)); !errors.Is(err, containerruntime.ErrNotFound) {
				t.Fatalf("corrupt lineage container still present: %v", err)
			}
			persisted := requireRuntimeValue(fixture.components.service.Run(fixture.ctx, created.ID))
			if !core.IsRunTerminal(persisted.State) || persisted.State != core.RunFailed ||
				persisted.RuntimeErrorCode != runtimeSecretsFailureCode ||
				persisted.CleanupState != core.CleanupRemoved || persisted.LaunchPhase != test.phase {
				t.Fatalf("corrupt lineage durable Run = %#v, want failed %s/%s phase %s",
					persisted, runtimeSecretsFailureCode, core.CleanupRemoved, test.phase)
			}
			if raw, readErr := os.ReadFile(persisted.LogPath); readErr == nil && strings.Contains(string(raw), canary) {
				t.Fatalf("corrupt lineage leaked canary into run.log: %q", raw)
			}
		})
	}
}
