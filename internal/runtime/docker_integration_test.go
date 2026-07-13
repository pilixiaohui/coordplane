//go:build docker

package runtime

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	gostdlib "runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDockerExecutorRealLifecycleAndOwnershipFence(t *testing.T) {
	executor, err := NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}
	image := buildDockerFixtureImage(t, ctx)
	home := t.TempDir()
	ref := RuntimeRef{
		ContainerName: "coordplane-test-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		ProjectID:     "project-docker", TaskID: "task-docker", AgentID: "agent-docker",
		RunID: "run-docker", Generation: 7, LaunchNonce: "nonce-docker",
	}
	spec := ContainerSpec{
		Ref: ref, Image: image,
		Command: CommandSpec{
			Executable: "/runtime-fixture", Args: []string{"hold"},
			Env: map[string]string{"HOME": "/home/agent", "PROVIDER_TOKEN": "provider-secret"},
		},
		SensitiveEnvKeys: []string{"PROVIDER_TOKEN"},
		WorkingDir:       "/home/agent", User: "65532:" + strconv.Itoa(os.Getgid()),
		GroupAdd: []string{strconv.Itoa(os.Getgid())}, Network: "none", ReadOnlyRoot: true,
		Mounts: []Mount{{Source: home, Target: "/home/agent"}},
		Limits: ResourceLimits{PIDs: 32, MemoryBytes: 128 << 20, NanoCPUs: 500_000_000, TmpfsBytes: 8 << 20},
	}
	created, err := executor.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = executor.Stop(cleanup, created, 0)
		_, _ = executor.Remove(cleanup, created)
	})
	adopted, err := executor.Create(ctx, spec)
	if err != nil || adopted.ContainerID != created.ContainerID {
		t.Fatalf("idempotent create = %#v err=%v, first=%#v", adopted, err, created)
	}
	attached, err := executor.Attach(ctx, created)
	if err != nil || attached.ContainerID != created.ContainerID {
		t.Fatalf("attach = %#v err=%v", attached, err)
	}
	started, err := executor.Start(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	state, err := executor.Inspect(ctx, started)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Status != StatusRunning || state.PID <= 0 || state.AutoRemove ||
		state.RestartPolicy != "no" || state.Privileged || !state.ReadonlyRootfs || state.PublishedPorts != 0 ||
		!contains(state.CapDrop, "ALL") || !contains(state.SecurityOpt, "no-new-privileges") || !state.Init {
		t.Fatalf("insecure or non-live container state = %#v", state)
	}
	if err := ValidateAdoption(spec, state); err != nil {
		t.Fatalf("live container does not match its creation contract: %v", err)
	}
	logs, err := executor.Logs(ctx, started, true)
	if err != nil {
		t.Fatal(err)
	}
	line, readErr := bufio.NewReader(logs).ReadString('\n')
	closeErr := logs.Close()
	if readErr != nil || closeErr != nil || !strings.Contains(line, "runtime-ready") {
		t.Fatalf("logs=%q read=%v close=%v", line, readErr, closeErr)
	}
	for _, test := range []struct {
		name   string
		mutate func(*RuntimeRef)
	}{
		{name: "launch nonce", mutate: func(ref *RuntimeRef) { ref.LaunchNonce = "wrong-nonce" }},
		{name: "generation", mutate: func(ref *RuntimeRef) { ref.Generation++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := started
			test.mutate(&wrong)
			if _, err := executor.Stop(ctx, wrong, 0); !errors.Is(err, ErrOwnership) {
				t.Fatalf("mismatched Stop error = %v", err)
			}
			assertContainerStillRunning(t, ctx, executor, started)
			if _, err := executor.Remove(ctx, wrong); !errors.Is(err, ErrOwnership) {
				t.Fatalf("mismatched Remove error = %v", err)
			}
			assertContainerStillRunning(t, ctx, executor, started)
		})
	}
	if _, err := executor.Stop(ctx, started, time.Second); err != nil {
		t.Fatal(err)
	}
	exit, err := executor.Wait(ctx, started)
	if err != nil || exit.ExitCode == 0 {
		t.Fatalf("stopped exit = %#v err=%v", exit, err)
	}
	if _, err := executor.Remove(ctx, started); err != nil {
		t.Fatal(err)
	}
	removed, err := executor.Remove(ctx, started)
	if err != nil || !removed.AlreadyAbsent {
		t.Fatalf("idempotent remove = %#v err=%v", removed, err)
	}
}

func TestDockerExecutorRejectsMaliciousSameLabelContainer(t *testing.T) {
	executor, err := NewDockerExecutorFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := executor.Ping(ctx); err != nil {
		t.Skipf("SKIP(Docker unavailable): %v", err)
	}
	image := buildDockerFixtureImage(t, ctx)
	spec := validTestSpec(t.TempDir())
	spec.Image = image
	spec.Ref = RuntimeRef{
		ContainerName: "coordplane-drift-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		ProjectID:     "project-drift", TaskID: "task-drift", AgentID: "agent-drift",
		RunID: "run-drift", Generation: 11, LaunchNonce: "nonce-drift",
	}
	if err := validateContainerSpec(&spec); err != nil {
		t.Fatal(err)
	}
	payload := dockerCreateRequest(spec)
	payload.User = "0:0"
	payload.HostConfig.Privileged = true
	payload.HostConfig.ReadonlyRootfs = false
	extra := dockerMount{Type: "bind", Source: spec.Mounts[0].Source, Target: "/forbidden"}
	extra.BindOptions.Propagation = "rprivate"
	payload.HostConfig.Mounts = append(payload.HostConfig.Mounts, extra)
	query := url.Values{"name": []string{spec.Ref.ContainerName}}
	response, err := executor.call(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/create?"+query.Encode(), payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatal(dockerResponseError(response, "create malicious fixture"))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := decodeResponse(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	ref := spec.Ref
	ref.ContainerID = created.ID
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = executor.Remove(cleanup, ref)
	})
	if _, err := executor.Create(ctx, spec); !errors.Is(err, ErrOwnership) {
		t.Fatalf("same-label insecure container adoption error = %v", err)
	}
	after, err := executor.Inspect(ctx, ref)
	if err != nil || after.Status != StatusCreated || after.Running {
		t.Fatalf("rejected insecure container was changed: state=%#v err=%v", after, err)
	}
}

func assertContainerStillRunning(t *testing.T, ctx context.Context, executor *DockerExecutor, ref RuntimeRef) {
	t.Helper()
	state, err := executor.Inspect(ctx, ref)
	if err != nil || !state.Running || state.Status != StatusRunning {
		t.Fatalf("rejected ownership operation changed container: state=%#v err=%v", state, err)
	}
}

func buildDockerFixtureImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	_, sourceFile, _, ok := gostdlib.Caller(0)
	if !ok {
		t.Fatal("resolve Docker fixture source path")
	}
	packageRoot := filepath.Dir(sourceFile)
	repositoryRoot := filepath.Clean(filepath.Join(packageRoot, "..", ".."))
	contextRoot := t.TempDir()
	binaryPath := filepath.Join(contextRoot, "runtime-fixture")
	buildBinary := exec.CommandContext(
		ctx, "go", "build", "-buildvcs=false", "-o", binaryPath,
		"./internal/runtime/testdata/docker-fixture",
	)
	buildBinary.Dir = repositoryRoot
	buildBinary.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := buildBinary.CombinedOutput(); err != nil {
		t.Fatalf("build static Docker fixture: %v\n%s", err, output)
	}
	image := "coordplane-runtime-test:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	dockerConfig := filepath.Join(contextRoot, "docker-config")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(packageRoot, "testdata", "docker-fixture", "Dockerfile")
	buildImage := exec.CommandContext(
		ctx, "docker", "build", "--network=none", "--pull=false", "-q",
		"-t", image, "-f", dockerfile, contextRoot,
	)
	buildImage.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
	if output, err := buildImage.CombinedOutput(); err != nil {
		t.Fatalf("build hermetic Docker fixture: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		remove := exec.CommandContext(cleanup, "docker", "image", "rm", "-f", image)
		remove.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		_ = remove.Run()
	})
	return image
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
