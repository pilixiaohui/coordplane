package daemon

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"coordplane/internal/adapter"
	"coordplane/internal/config"
	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
)

func TestRuntimeBuildToDeleteStepsComeFromStaticLists(t *testing.T) {
	prepareNames := make([]string, 0, len(runtimePrepareSteps))
	for _, step := range runtimePrepareSteps {
		if step.name == "" || step.failureCode == "" || step.run == nil {
			t.Fatalf("invalid runtime prepare step: %#v", step)
		}
		prepareNames = append(prepareNames, step.name)
	}
	wantPrepare := []string{
		"prepareWorkspace", "prepareAgentHome", "writeRunToken", "writeBootstrap",
		"writeInstructions", "writeSecrets", "writeLaunch",
		"openRunAPISocket", "createContainer", "attachStreams", "startCLI", "verifyLive",
	}
	if !reflect.DeepEqual(prepareNames, wantPrepare) {
		t.Fatalf("runtime prepare steps = %v, want %v", prepareNames, wantPrepare)
	}

	cleanupNames := make([]string, 0, len(runtimeCleanupSteps))
	for _, step := range runtimeCleanupSteps {
		if step.name == "" || step.run == nil {
			t.Fatalf("invalid runtime cleanup step: %#v", step)
		}
		cleanupNames = append(cleanupNames, step.name)
	}
	wantCleanup := []string{"stopContainer", "removeContainer", "closeRunAPISocket", "removeRunControl"}
	if !reflect.DeepEqual(cleanupNames, wantCleanup) {
		t.Fatalf("runtime cleanup steps = %v, want %v", cleanupNames, wantCleanup)
	}
}

func TestBootstrapPreservesEveryMessageIDWithinBoundedUTF8Bodies(t *testing.T) {
	launch := core.RunLaunchContext{
		Project: core.Project{ID: "project-bootstrap"},
		Agent:   core.Agent{ID: "agent-bootstrap"},
		Task: core.Task{
			ID: "task-bootstrap", ProjectID: "project-bootstrap", Kind: core.TaskConversation,
			Title: "all message IDs",
		},
	}
	for index := 0; index < core.MessagePageLimit+5; index++ {
		launch.Messages = append(launch.Messages, core.Message{
			ID: fmt.Sprintf("message-%02d", index), TaskID: "task-bootstrap",
			SenderKind: "boss", Body: strings.Repeat("界", core.MaximumMessageBodyBytes/3),
		})
	}
	bootstrap := buildBootstrap(launch, core.Run{ID: "run-bootstrap", Generation: 1}, "instructions", "", gitrepo.WorkspaceSpec{})
	if !utf8.ValidString(bootstrap) {
		t.Fatal("bootstrap is not valid UTF-8")
	}
	for index := range launch.Messages {
		id := fmt.Sprintf("[message-%02d]", index)
		if strings.Count(bootstrap, id) != 1 {
			t.Fatalf("bootstrap Message ID %s count = %d", id, strings.Count(bootstrap, id))
		}
	}
	bodyBytes := strings.Count(bootstrap, "界") * len("界")
	if bodyBytes == 0 || bodyBytes > runtimeBootstrapMessageTotalLimit {
		t.Fatalf("bootstrap Message body bytes = %d, want 1..%d", bodyBytes, runtimeBootstrapMessageTotalLimit)
	}
	if !strings.Contains(bootstrap, "[body truncated]") || !strings.Contains(bootstrap, "[body omitted: aggregate limit]") {
		t.Fatalf("bootstrap does not disclose per-item and aggregate body truncation:\n%s", bootstrap)
	}
}

func TestBootstrapAdvertisesTheImportedSourceConvenienceRef(t *testing.T) {
	source := &gitrepo.WorkspaceSource{
		TaskID: "source-task", RunID: "source-run",
		TaskRef: "refs/coordplane/tasks/source-task/runs/source-run",
		HeadSHA: strings.Repeat("b", 40),
	}
	launch := core.RunLaunchContext{
		Project: core.Project{ID: "project-a"},
		Agent:   core.Agent{ID: "agent-a"},
		Messages: []core.Message{{
			ID: "message-a", TaskID: "conversation-task", RelatedTaskID: "review-task",
			SenderKind: "boss", Body: "Review the fixed source.",
		}},
		Task: core.Task{
			ID: "review-task", ProjectID: "project-a", Kind: core.TaskWork,
			Title: "Review", BaseSHA: strings.Repeat("a", 40),
			SourceTaskID: source.TaskID, SourceRunID: source.RunID,
			SourceTaskRef: source.TaskRef, SourceHeadSHA: source.HeadSHA,
		},
	}
	run := core.Run{ID: "run-a", Generation: 1}
	bootstrap := buildBootstrap(
		launch,
		run,
		"Follow the instructions.",
		"/host/path/must-not-render",
		gitrepo.WorkspaceSpec{ProjectID: "project-a", TaskID: "review-task", Source: source},
	)
	want := "Source convenience ref: " + source.ConvenienceRef()
	if !strings.Contains(bootstrap, want) {
		t.Fatalf("bootstrap omitted imported source ref %q:\n%s", want, bootstrap)
	}
	if strings.Contains(bootstrap, "Source convenience ref: refs/coordplane/source/") {
		t.Fatalf("bootstrap retained the obsolete source ref:\n%s", bootstrap)
	}
	if messageScope := "[message-a] delivery_task=conversation-task related_task=review-task"; !strings.Contains(bootstrap, messageScope) {
		t.Fatalf("bootstrap omitted durable Message scope %q:\n%s", messageScope, bootstrap)
	}
}

func TestContainerSpecExcludesProviderSecretsFromEnvironment(t *testing.T) {
	providerEnv := config.ProviderEnvCatalog()
	for _, name := range append(providerEnv, "ANTHROPIC_API_KEY", "HOME", "COORDPLANE_RUN_SOCKET", "COORDPLANE_RUN_TOKEN_FILE") {
		t.Setenv(name, "/untrusted/provider-value")
	}
	coordlink := requireRuntimeValue(os.Executable())
	controller := &runtimeController{
		config: config.Config{Runtime: config.RuntimeConfig{
			DockerNetwork:        "none",
			ProviderEnvAllowlist: providerEnv,
		}},
		coordlink: coordlink,
	}
	run := core.Run{
		ID: "run-env", ProjectID: "project-env", TaskID: "task-env", AgentID: "agent-env",
		Generation: 1, LaunchNonce: "nonce-env", ContainerName: "coordplane-run-env",
		Image: "agent:test", HomePath: "/runtime/agent-home",
	}
	spec := requireRuntimeValue(controller.containerSpec(run, core.TaskConversation, adapter.CommandSpec{
		Executable: "claude",
		Env:        map[string]string{"HOME": "/home/agent"},
	}, "/runtime/run-control/run-env"))
	if !reflect.DeepEqual(spec.SensitiveEnvKeys, controller.config.Runtime.ProviderEnvAllowlist) {
		t.Fatalf("sensitive environment keys = %v", spec.SensitiveEnvKeys)
	}
	if spec.Command.Executable != runtimeLaunchExecutable {
		t.Fatalf("container executable = %q, want launcher %q", spec.Command.Executable, runtimeLaunchExecutable)
	}
	for _, name := range append(providerEnv, "ANTHROPIC_API_KEY") {
		if _, exists := spec.Command.Env[name]; exists {
			t.Errorf("container env leaked allowlist value for %s", name)
		}
	}
	for name, value := range map[string]string{
		"HOME": "/home/agent", "COORDPLANE_RUN_SOCKET": "/run/coordplane/api.sock",
		"COORDPLANE_RUN_TOKEN_FILE": "/run/coordplane/token", "COORDPLANE_SECRETS_FILE": "/run/coordplane/secrets",
	} {
		if spec.Command.Env[name] != value {
			t.Errorf("container %s = %q, want trusted %q", name, spec.Command.Env[name], value)
		}
	}
	if spec.Limits.PIDs != 256 || spec.Limits.MemoryBytes != 512<<20 || spec.Limits.NanoCPUs != 1_000_000_000 {
		t.Fatalf("Agent resource contract = %#v, want 1 CPU / 512 MiB / 256 PIDs", spec.Limits)
	}
}
