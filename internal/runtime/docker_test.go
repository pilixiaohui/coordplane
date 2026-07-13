package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDockerExecutorPreservesCallerCancellation(t *testing.T) {
	executor := &DockerExecutor{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := executor.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Docker request error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("caller cancellation was misclassified as Docker unavailable: %v", err)
	}
}

func TestContainerSpecRejectsRootAndSymlinkMounts(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	spec := validTestSpec(real)
	spec.User = "0:0"
	if err := validateContainerSpec(&spec); err == nil {
		t.Fatal("root container user was accepted")
	}
	spec = validTestSpec(link)
	if err := validateContainerSpec(&spec); err == nil {
		t.Fatal("symlink mount source was accepted")
	}
}

func TestInspectFactRequiresEveryOwnershipFence(t *testing.T) {
	ref := testRef()
	payload := dockerInspect{ID: "container-id", Name: "/" + ref.ContainerName}
	payload.Config.Labels = labelsFor(ref)
	payload.Config.Image = "agent:test"
	payload.Config.Entrypoint = []string{"/runtime-fixture"}
	payload.Config.Cmd = []string{"hold"}
	payload.Config.Env = []string{"HOME=/home/agent", "PROVIDER_TOKEN=inspect-secret"}
	payload.State.Status = "created"
	payload.HostConfig.RestartPolicy.Name = "no"
	init := true
	payload.HostConfig.Init = &init
	payload.HostConfig.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,size=8388608"}
	state, err := inspectFact(payload, ref)
	if err != nil {
		t.Fatalf("matching ownership rejected: %v", err)
	}
	if !equalStrings(state.Entrypoint, payload.Config.Entrypoint) ||
		!equalStrings(state.CommandArgs, payload.Config.Cmd) || !state.Init ||
		state.Tmpfs["/tmp"] != payload.HostConfig.Tmpfs["/tmp"] {
		t.Fatalf("inspect omitted adoption facts: %#v", state)
	}
	if rendered := fmt.Sprintf("%#v", state); strings.Contains(rendered, "inspect-secret") {
		t.Fatalf("inspect fact leaked environment plaintext: %s", rendered)
	}
	for label, value := range labelsFor(ref) {
		t.Run(label, func(t *testing.T) {
			changed := payload
			changed.Config.Labels = labelsFor(ref)
			changed.Config.Labels[label] = value + "-wrong"
			if _, err := inspectFact(changed, ref); !errors.Is(err, ErrOwnership) {
				t.Fatalf("mismatched %s error = %v", label, err)
			}
		})
	}
}

func TestValidateOwnershipRequiresCompleteDurableIdentity(t *testing.T) {
	expected := testRef()
	expected.ContainerID = "container-id"
	valid := func() RuntimeRef { return expected }
	tests := map[string]func(*RuntimeRef){
		"container ID":   func(ref *RuntimeRef) { ref.ContainerID = "other-container" },
		"container name": func(ref *RuntimeRef) { ref.ContainerName = "other-name" },
		"project":        func(ref *RuntimeRef) { ref.ProjectID = "other-project" },
		"task":           func(ref *RuntimeRef) { ref.TaskID = "other-task" },
		"agent":          func(ref *RuntimeRef) { ref.AgentID = "other-agent" },
		"run":            func(ref *RuntimeRef) { ref.RunID = "other-run" },
		"generation":     func(ref *RuntimeRef) { ref.Generation++ },
		"launch nonce":   func(ref *RuntimeRef) { ref.LaunchNonce = "other-nonce" },
	}
	if err := ValidateOwnership(expected, valid()); err != nil {
		t.Fatalf("matching runtime ownership rejected: %v", err)
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			actual := valid()
			mutate(&actual)
			if err := ValidateOwnership(expected, actual); !errors.Is(err, ErrOwnership) {
				t.Fatalf("ownership drift error = %v", err)
			}
		})
	}
	expected.ContainerID = ""
	if err := ValidateOwnership(expected, valid()); err != nil {
		t.Fatalf("create-before-ID-persistence adoption rejected: %v", err)
	}
}

func TestValidateAdoptionRejectsIsolationDrift(t *testing.T) {
	source := t.TempDir()
	spec := validTestSpec(source)
	valid := func() LiveState {
		environment, err := inspectEnvironment([]string{
			"HOME=/home/agent",
			"COORDPLANE_RUN_SOCKET=/run/coordplane/api.sock",
			"PROVIDER_TOKEN=provider-secret",
			"IMAGE_DEFAULT=allowed",
		})
		if err != nil {
			t.Fatal(err)
		}
		return LiveState{
			Ref: spec.Ref, Image: spec.Image,
			Entrypoint: []string{spec.Command.Executable}, CommandArgs: append([]string(nil), spec.Command.Args...),
			Environment: environment, WorkingDir: spec.WorkingDir, User: spec.User,
			GroupAdd: append([]string(nil), spec.GroupAdd...), Network: spec.Network,
			RestartPolicy: "no", ReadonlyRootfs: true, CapDrop: []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"}, PIDsLimit: spec.Limits.PIDs,
			MemoryBytes: spec.Limits.MemoryBytes, NanoCPUs: spec.Limits.NanoCPUs,
			Init: true, Tmpfs: map[string]string{"/tmp": "nodev,size=8388608,rw,nosuid"},
			Mounts: []MountFact{{
				Type: "bind", Source: source, Destination: "/home/agent", ReadWrite: true, Propagation: "rprivate",
			}},
		}
	}
	if err := ValidateAdoption(spec, valid()); err != nil {
		t.Fatalf("matching isolation rejected: %v", err)
	}
	rotatedSecret := valid()
	setEnvironmentDigest(&rotatedSecret, "PROVIDER_TOKEN", "rotated-provider-secret")
	if err := ValidateAdoption(spec, rotatedSecret); err != nil {
		t.Fatalf("rotated sensitive environment value rejected: %v", err)
	}
	tests := map[string]func(*LiveState){
		"image":               func(state *LiveState) { state.Image = "other:image" },
		"entrypoint":          func(state *LiveState) { state.Entrypoint = []string{"/bin/sh"} },
		"command arguments":   func(state *LiveState) { state.CommandArgs = []string{"other"} },
		"fixed environment":   func(state *LiveState) { setEnvironmentDigest(state, "HOME", "/root") },
		"missing environment": func(state *LiveState) { removeEnvironmentFact(state, "PROVIDER_TOKEN") },
		"duplicate environment": func(state *LiveState) {
			state.Environment = append(state.Environment, state.Environment[0])
		},
		"working directory": func(state *LiveState) { state.WorkingDir = "/tmp" },
		"root user":         func(state *LiveState) { state.User = "0:0" },
		"wrong user":        func(state *LiveState) { state.User = "65531:65532" },
		"wrong network":     func(state *LiveState) { state.Network = "host" },
		"auto remove":       func(state *LiveState) { state.AutoRemove = true },
		"restart":           func(state *LiveState) { state.RestartPolicy = "always" },
		"privileged":        func(state *LiveState) { state.Privileged = true },
		"cap add":           func(state *LiveState) { state.CapAdd = []string{"SYS_ADMIN"} },
		"cap drop":          func(state *LiveState) { state.CapDrop = nil },
		"extra cap drop":    func(state *LiveState) { state.CapDrop = append(state.CapDrop, "NET_RAW") },
		"writable root":     func(state *LiveState) { state.ReadonlyRootfs = false },
		"new privileges":    func(state *LiveState) { state.SecurityOpt = nil },
		"extra security option": func(state *LiveState) {
			state.SecurityOpt = append(state.SecurityOpt, "seccomp=unconfined")
		},
		"published port":      func(state *LiveState) { state.PublishedPorts = 1 },
		"PID limit":           func(state *LiveState) { state.PIDsLimit++ },
		"memory limit":        func(state *LiveState) { state.MemoryBytes++ },
		"CPU limit":           func(state *LiveState) { state.NanoCPUs++ },
		"init process":        func(state *LiveState) { state.Init = false },
		"tmpfs missing":       func(state *LiveState) { state.Tmpfs = nil },
		"tmpfs extra path":    func(state *LiveState) { state.Tmpfs["/var/tmp"] = "rw" },
		"tmpfs size":          func(state *LiveState) { state.Tmpfs["/tmp"] = "rw,nosuid,nodev,size=1" },
		"tmpfs options":       func(state *LiveState) { state.Tmpfs["/tmp"] = "rw,nosuid,size=8388608" },
		"tmpfs duplicate":     func(state *LiveState) { state.Tmpfs["/tmp"] = "rw,rw,nosuid,nodev,size=8388608" },
		"extra mount":         func(state *LiveState) { state.Mounts = append(state.Mounts, state.Mounts[0]) },
		"mount source":        func(state *LiveState) { state.Mounts[0].Source = filepath.Dir(source) },
		"mount target":        func(state *LiveState) { state.Mounts[0].Destination = "/workspace" },
		"mount access":        func(state *LiveState) { state.Mounts[0].ReadWrite = false },
		"mount type":          func(state *LiveState) { state.Mounts[0].Type = "volume" },
		"mount propagation":   func(state *LiveState) { state.Mounts[0].Propagation = "rshared" },
		"supplementary group": func(state *LiveState) { state.GroupAdd = append(state.GroupAdd, "1234") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			state := valid()
			mutate(&state)
			if err := ValidateAdoption(spec, state); !errors.Is(err, ErrOwnership) {
				t.Fatalf("drift error = %v, want ownership rejection", err)
			}
		})
	}
}

func validTestSpec(source string) ContainerSpec {
	return ContainerSpec{
		Ref: testRef(), Image: "alpine:3.20",
		Command: CommandSpec{
			Executable: "/runtime-fixture", Args: []string{"hold"},
			Env: map[string]string{
				"HOME": "/home/agent", "COORDPLANE_RUN_SOCKET": "/run/coordplane/api.sock",
				"PROVIDER_TOKEN": "provider-secret",
			},
		},
		SensitiveEnvKeys: []string{"PROVIDER_TOKEN"}, WorkingDir: "/home/agent",
		User: "65532:65532", GroupAdd: []string{"65532"}, Network: "none",
		Mounts: []Mount{{Source: source, Target: "/home/agent"}}, ReadOnlyRoot: true,
		Limits: ResourceLimits{PIDs: 32, MemoryBytes: 128 << 20, NanoCPUs: 500_000_000, TmpfsBytes: 8 << 20},
	}
}

func setEnvironmentDigest(state *LiveState, name, value string) {
	for index := range state.Environment {
		if state.Environment[index].Name == name {
			state.Environment[index].ValueDigest = environmentValueDigest(value)
			return
		}
	}
}

func removeEnvironmentFact(state *LiveState, name string) {
	for index := range state.Environment {
		if state.Environment[index].Name == name {
			state.Environment = append(state.Environment[:index], state.Environment[index+1:]...)
			return
		}
	}
}

func testRef() RuntimeRef {
	return RuntimeRef{
		ContainerName: "coordplane-run-run-a", ProjectID: "project-a", TaskID: "task-a",
		AgentID: "agent-a", RunID: "run-a", Generation: 3, LaunchNonce: "nonce-a",
	}
}
