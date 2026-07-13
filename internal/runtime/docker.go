package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const dockerAPIVersion = "v1.44"

type DockerExecutor struct {
	client *http.Client
}

func NewDockerExecutor(socketPath string) (*DockerExecutor, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("runtime: Docker socket path must be absolute")
	}
	socketPath = filepath.Clean(socketPath)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &DockerExecutor{client: &http.Client{Transport: transport}}, nil
}

func NewDockerExecutorFromEnvironment() (*DockerExecutor, error) {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host == "" {
		return NewDockerExecutor("")
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Scheme != "unix" || parsed.Path == "" {
		return nil, errors.New("runtime: only a local unix Docker host is supported")
	}
	return NewDockerExecutor(parsed.Path)
}

func (e *DockerExecutor) Ping(ctx context.Context) error {
	response, err := e.call(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return dockerResponseError(response, "ping Docker")
	}
	return nil
}

func (e *DockerExecutor) Create(ctx context.Context, spec ContainerSpec) (RuntimeRef, error) {
	if err := validateContainerSpec(&spec); err != nil {
		return RuntimeRef{}, err
	}
	if state, err := e.Inspect(ctx, spec.Ref); err == nil {
		if err := ValidateAdoption(spec, state); err != nil {
			return RuntimeRef{}, err
		}
		return state.Ref, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RuntimeRef{}, err
	}

	payload := dockerCreateRequest(spec)
	query := url.Values{"name": []string{spec.Ref.ContainerName}}
	response, err := e.call(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/create?"+query.Encode(), payload, nil)
	if err != nil {
		return RuntimeRef{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		state, inspectErr := e.Inspect(ctx, spec.Ref)
		if inspectErr == nil {
			if err := ValidateAdoption(spec, state); err != nil {
				return RuntimeRef{}, err
			}
			return state.Ref, nil
		}
		if errors.Is(inspectErr, ErrOwnership) {
			return RuntimeRef{}, inspectErr
		}
		return RuntimeRef{}, dockerResponseError(response, "create container")
	}
	if response.StatusCode != http.StatusCreated {
		return RuntimeRef{}, dockerResponseError(response, "create container")
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := decodeResponse(response.Body, &created); err != nil {
		return RuntimeRef{}, fmt.Errorf("runtime: decode Docker create response: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return RuntimeRef{}, errors.New("runtime: Docker create response omitted container ID")
	}
	ref := spec.Ref
	ref.ContainerID = created.ID
	state, err := e.Inspect(ctx, ref)
	if err != nil {
		return RuntimeRef{}, err
	}
	if err := ValidateAdoption(spec, state); err != nil {
		return RuntimeRef{}, err
	}
	return state.Ref, nil
}

func (e *DockerExecutor) Attach(ctx context.Context, ref RuntimeRef) (RuntimeRef, error) {
	state, err := e.Inspect(ctx, ref)
	if err != nil {
		return RuntimeRef{}, err
	}
	return state.Ref, nil
}

func (e *DockerExecutor) Start(ctx context.Context, ref RuntimeRef) (RuntimeRef, error) {
	state, err := e.Inspect(ctx, ref)
	if err != nil {
		return RuntimeRef{}, err
	}
	if state.Status == StatusRunning {
		return state.Ref, nil
	}
	if state.Status == StatusExited {
		return RuntimeRef{}, ErrExited
	}
	response, err := e.call(ctx, http.MethodPost, containerPath(state.Ref, "/start"), nil, nil)
	if err != nil {
		return RuntimeRef{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotModified {
		if response.StatusCode == http.StatusNotFound {
			return RuntimeRef{}, ErrNotFound
		}
		return RuntimeRef{}, dockerResponseError(response, "start container")
	}
	started, err := e.Inspect(ctx, state.Ref)
	if err != nil {
		return RuntimeRef{}, err
	}
	return started.Ref, nil
}

func (e *DockerExecutor) Inject(context.Context, RuntimeRef, []byte) (InjectResult, error) {
	return InjectResult{}, ErrUnsupported
}

func (e *DockerExecutor) Inspect(ctx context.Context, ref RuntimeRef) (LiveState, error) {
	lookup := strings.TrimSpace(ref.ContainerID)
	if lookup == "" {
		lookup = strings.TrimSpace(ref.ContainerName)
	}
	if lookup == "" {
		return LiveState{}, errors.New("runtime: container ID or name is required")
	}
	response, err := e.call(ctx, http.MethodGet, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(lookup)+"/json", nil, nil)
	if err != nil {
		return LiveState{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return LiveState{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return LiveState{}, dockerResponseError(response, "inspect container")
	}
	var payload dockerInspect
	if err := decodeResponse(response.Body, &payload); err != nil {
		return LiveState{}, fmt.Errorf("runtime: decode Docker inspect response: %w", err)
	}
	state, err := inspectFact(payload, ref)
	if err != nil {
		return LiveState{}, err
	}
	return state, nil
}

func (e *DockerExecutor) Wait(ctx context.Context, ref RuntimeRef) (ExitFact, error) {
	state, err := e.Inspect(ctx, ref)
	if err != nil {
		return ExitFact{}, err
	}
	if state.Status == StatusExited && state.ExitCode != nil {
		return ExitFact{Ref: state.Ref, ExitCode: *state.ExitCode}, nil
	}
	response, err := e.call(ctx, http.MethodPost, containerPath(state.Ref, "/wait?condition=not-running"), nil, nil)
	if err != nil {
		return ExitFact{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ExitFact{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return ExitFact{}, dockerResponseError(response, "wait for container")
	}
	var payload struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := decodeResponse(response.Body, &payload); err != nil {
		return ExitFact{}, fmt.Errorf("runtime: decode Docker wait response: %w", err)
	}
	if payload.Error != nil && strings.TrimSpace(payload.Error.Message) != "" {
		return ExitFact{}, fmt.Errorf("runtime: Docker wait failed: %s", bounded(payload.Error.Message))
	}
	return ExitFact{Ref: state.Ref, ExitCode: payload.StatusCode}, nil
}

func (e *DockerExecutor) Logs(ctx context.Context, ref RuntimeRef, follow bool) (io.ReadCloser, error) {
	state, err := e.Inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"stdout": []string{"1"}, "stderr": []string{"1"}, "timestamps": []string{"0"},
		"since": []string{"0"}, "follow": []string{strconv.FormatBool(follow)},
	}
	response, err := e.call(ctx, http.MethodGet, containerPath(state.Ref, "/logs?"+query.Encode()), nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return nil, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, dockerResponseError(response, "read container logs")
	}
	return &demuxReadCloser{reader: bufio.NewReader(response.Body), body: response.Body}, nil
}

func (e *DockerExecutor) Stop(ctx context.Context, ref RuntimeRef, grace time.Duration) (StopResult, error) {
	state, err := e.Inspect(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return StopResult{AlreadyStopped: true}, nil
	}
	if err != nil {
		return StopResult{}, err
	}
	if !state.Running {
		return StopResult{AlreadyStopped: true}, nil
	}
	seconds := int64((grace + time.Second - 1) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	response, err := e.call(ctx, http.MethodPost, containerPath(state.Ref, "/stop?t="+strconv.FormatInt(seconds, 10)), nil, nil)
	if err != nil {
		return StopResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNotModified {
		return StopResult{AlreadyStopped: true}, nil
	}
	if response.StatusCode != http.StatusNoContent {
		return StopResult{}, dockerResponseError(response, "stop container")
	}
	return StopResult{}, nil
}

func (e *DockerExecutor) Remove(ctx context.Context, ref RuntimeRef) (RemoveResult, error) {
	state, err := e.Inspect(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return RemoveResult{AlreadyAbsent: true}, nil
	}
	if err != nil {
		return RemoveResult{}, err
	}
	response, err := e.call(ctx, http.MethodDelete, containerPath(state.Ref, "?v=1"), nil, nil)
	if err != nil {
		return RemoveResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return RemoveResult{AlreadyAbsent: true}, nil
	}
	if response.StatusCode != http.StatusNoContent {
		return RemoveResult{}, dockerResponseError(response, "remove container")
	}
	return RemoveResult{}, nil
}

func (e *DockerExecutor) Managed(ctx context.Context) ([]LiveState, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {LabelManaged + "=true", LabelContract + "=v1"}})
	query := url.Values{"all": []string{"1"}, "filters": []string{string(filters)}}
	response, err := e.call(ctx, http.MethodGet, "/"+dockerAPIVersion+"/containers/json?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, dockerResponseError(response, "list managed containers")
	}
	var rows []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	if err := decodeResponse(response.Body, &rows); err != nil {
		return nil, fmt.Errorf("runtime: decode Docker container list: %w", err)
	}
	states := make([]LiveState, 0, len(rows))
	for _, row := range rows {
		ref, err := refFromLabels(row.ID, firstName(row.Names), row.Labels)
		if err != nil {
			return nil, err
		}
		state, err := e.Inspect(ctx, ref)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	sort.Slice(states, func(left, right int) bool { return states[left].Ref.RunID < states[right].Ref.RunID })
	return states, nil
}

type dockerCreate struct {
	Image        string            `json:"Image"`
	Entrypoint   []string          `json:"Entrypoint"`
	Cmd          []string          `json:"Cmd"`
	Env          []string          `json:"Env,omitempty"`
	WorkingDir   string            `json:"WorkingDir"`
	User         string            `json:"User"`
	Labels       map[string]string `json:"Labels"`
	AttachStdout bool              `json:"AttachStdout"`
	AttachStderr bool              `json:"AttachStderr"`
	OpenStdin    bool              `json:"OpenStdin"`
	Tty          bool              `json:"Tty"`
	HostConfig   dockerHostConfig  `json:"HostConfig"`
}

type dockerHostConfig struct {
	AutoRemove     bool                     `json:"AutoRemove"`
	Privileged     bool                     `json:"Privileged"`
	RestartPolicy  dockerRestartPolicy      `json:"RestartPolicy"`
	NetworkMode    string                   `json:"NetworkMode"`
	Mounts         []dockerMount            `json:"Mounts,omitempty"`
	CapDrop        []string                 `json:"CapDrop"`
	SecurityOpt    []string                 `json:"SecurityOpt"`
	PidsLimit      int64                    `json:"PidsLimit"`
	Memory         int64                    `json:"Memory"`
	NanoCPUs       int64                    `json:"NanoCpus"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	Tmpfs          map[string]string        `json:"Tmpfs,omitempty"`
	GroupAdd       []string                 `json:"GroupAdd,omitempty"`
	Init           *bool                    `json:"Init"`
	PortBindings   map[string][]interface{} `json:"PortBindings"`
}

type dockerRestartPolicy struct {
	Name string `json:"Name"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Target      string `json:"Target"`
	ReadOnly    bool   `json:"ReadOnly"`
	BindOptions struct {
		Propagation string `json:"Propagation"`
	} `json:"BindOptions"`
}

type dockerInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image      string            `json:"Image"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Env        []string          `json:"Env"`
		WorkingDir string            `json:"WorkingDir"`
		User       string            `json:"User"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		PID      int    `json:"Pid"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
	HostConfig struct {
		AutoRemove     bool                `json:"AutoRemove"`
		Privileged     bool                `json:"Privileged"`
		ReadonlyRootfs bool                `json:"ReadonlyRootfs"`
		NetworkMode    string              `json:"NetworkMode"`
		GroupAdd       []string            `json:"GroupAdd"`
		CapAdd         []string            `json:"CapAdd"`
		CapDrop        []string            `json:"CapDrop"`
		SecurityOpt    []string            `json:"SecurityOpt"`
		PidsLimit      int64               `json:"PidsLimit"`
		Memory         int64               `json:"Memory"`
		NanoCPUs       int64               `json:"NanoCpus"`
		Init           *bool               `json:"Init"`
		Tmpfs          map[string]string   `json:"Tmpfs"`
		PortBindings   map[string][]any    `json:"PortBindings"`
		RestartPolicy  dockerRestartPolicy `json:"RestartPolicy"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]any `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
		Propagation string `json:"Propagation"`
	} `json:"Mounts"`
}

func dockerCreateRequest(spec ContainerSpec) dockerCreate {
	env := make([]string, 0, len(spec.Command.Env))
	for key, value := range spec.Command.Env {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	mounts := make([]dockerMount, 0, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		item := dockerMount{Type: "bind", Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly}
		item.BindOptions.Propagation = "rprivate"
		mounts = append(mounts, item)
	}
	init := true
	return dockerCreate{
		Image: spec.Image, Entrypoint: []string{spec.Command.Executable}, Cmd: append([]string(nil), spec.Command.Args...),
		Env: env, WorkingDir: spec.WorkingDir, User: spec.User, Labels: labelsFor(spec.Ref),
		HostConfig: dockerHostConfig{
			AutoRemove: false, RestartPolicy: dockerRestartPolicy{Name: "no"}, NetworkMode: spec.Network,
			Mounts: mounts, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			PidsLimit: spec.Limits.PIDs, Memory: spec.Limits.MemoryBytes, NanoCPUs: spec.Limits.NanoCPUs,
			ReadonlyRootfs: spec.ReadOnlyRoot, GroupAdd: append([]string(nil), spec.GroupAdd...), Init: &init,
			Tmpfs:        map[string]string{"/tmp": "rw,nosuid,nodev,size=" + strconv.FormatInt(spec.Limits.TmpfsBytes, 10)},
			PortBindings: map[string][]interface{}{},
		},
	}
}

func validateContainerSpec(spec *ContainerSpec) error {
	if err := validateRef(spec.Ref); err != nil {
		return err
	}
	if strings.TrimSpace(spec.Image) == "" || strings.ContainsRune(spec.Image, '\x00') {
		return errors.New("runtime: container image is required")
	}
	if strings.TrimSpace(spec.Command.Executable) == "" || strings.ContainsRune(spec.Command.Executable, '\x00') {
		return errors.New("runtime: command executable is required")
	}
	if !filepath.IsAbs(spec.WorkingDir) {
		return errors.New("runtime: container working directory must be absolute")
	}
	uid := strings.SplitN(spec.User, ":", 2)[0]
	if uid == "" || uid == "0" {
		return errors.New("runtime: container user must be a nonroot numeric UID")
	}
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		return errors.New("runtime: container user must be a nonroot numeric UID")
	}
	if strings.TrimSpace(spec.Network) == "" {
		return errors.New("runtime: Docker network is required")
	}
	if spec.Limits.PIDs <= 0 {
		spec.Limits.PIDs = 256
	}
	if spec.Limits.MemoryBytes <= 0 {
		spec.Limits.MemoryBytes = 1 << 30
	}
	if spec.Limits.NanoCPUs <= 0 {
		spec.Limits.NanoCPUs = 1_000_000_000
	}
	if spec.Limits.TmpfsBytes <= 0 {
		spec.Limits.TmpfsBytes = 64 << 20
	}
	seenTargets := make(map[string]struct{}, len(spec.Mounts))
	for index, mount := range spec.Mounts {
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Target) {
			return fmt.Errorf("runtime: mount %d paths must be absolute", index)
		}
		source := filepath.Clean(mount.Source)
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || resolved != source {
			return fmt.Errorf("runtime: mount %d source is missing or traverses a symlink", index)
		}
		if _, exists := seenTargets[mount.Target]; exists {
			return fmt.Errorf("runtime: duplicate mount target %s", mount.Target)
		}
		seenTargets[mount.Target] = struct{}{}
		spec.Mounts[index].Source = source
	}
	for key, value := range spec.Command.Env {
		if !validEnvName(key) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("runtime: invalid environment entry %q", key)
		}
	}
	seenSensitive := make(map[string]struct{}, len(spec.SensitiveEnvKeys))
	for _, key := range spec.SensitiveEnvKeys {
		if !validEnvName(key) {
			return fmt.Errorf("runtime: invalid sensitive environment name %q", key)
		}
		if _, exists := seenSensitive[key]; exists {
			return fmt.Errorf("runtime: duplicate sensitive environment name %q", key)
		}
		seenSensitive[key] = struct{}{}
	}
	return nil
}

func validateRef(ref RuntimeRef) error {
	values := []struct {
		name, value string
	}{
		{"container_name", ref.ContainerName}, {"project_id", ref.ProjectID}, {"task_id", ref.TaskID},
		{"agent_id", ref.AgentID}, {"run_id", ref.RunID}, {"launch_nonce", ref.LaunchNonce},
	}
	for _, value := range values {
		if strings.TrimSpace(value.value) == "" || value.value != strings.TrimSpace(value.value) || strings.ContainsRune(value.value, '\x00') {
			return fmt.Errorf("runtime: %s is required", value.name)
		}
	}
	if ref.Generation < 1 {
		return errors.New("runtime: generation must be positive")
	}
	return nil
}

func labelsFor(ref RuntimeRef) map[string]string {
	return map[string]string{
		LabelManaged: "true", LabelContract: "v1", LabelProjectID: ref.ProjectID, LabelTaskID: ref.TaskID,
		LabelAgentID: ref.AgentID, LabelRunID: ref.RunID,
		LabelGeneration: strconv.FormatInt(ref.Generation, 10), LabelLaunchNonce: ref.LaunchNonce,
	}
}

func inspectFact(payload dockerInspect, expected RuntimeRef) (LiveState, error) {
	name := strings.TrimPrefix(payload.Name, "/")
	actual, err := refFromLabels(payload.ID, name, payload.Config.Labels)
	if err != nil {
		return LiveState{}, err
	}
	if actual.ContainerName != expected.ContainerName || actual.ProjectID != expected.ProjectID ||
		actual.TaskID != expected.TaskID || actual.AgentID != expected.AgentID || actual.RunID != expected.RunID ||
		actual.Generation != expected.Generation || actual.LaunchNonce != expected.LaunchNonce ||
		(expected.ContainerID != "" && expected.ContainerID != actual.ContainerID) {
		return LiveState{}, ErrOwnership
	}
	status := ContainerStatus(payload.State.Status)
	switch status {
	case StatusCreated, StatusRunning, StatusExited:
	default:
		if payload.State.Running {
			status = StatusRunning
		} else {
			status = StatusExited
		}
	}
	environment, err := inspectEnvironment(payload.Config.Env)
	if err != nil {
		return LiveState{}, err
	}
	state := LiveState{
		Ref: actual, Image: payload.Config.Image,
		Entrypoint:  append([]string(nil), payload.Config.Entrypoint...),
		CommandArgs: append([]string(nil), payload.Config.Cmd...), Environment: environment,
		WorkingDir: payload.Config.WorkingDir, User: payload.Config.User,
		GroupAdd: append([]string(nil), payload.HostConfig.GroupAdd...), Network: payload.HostConfig.NetworkMode,
		Status: status, Running: payload.State.Running, PID: payload.State.PID,
		AutoRemove: payload.HostConfig.AutoRemove, RestartPolicy: payload.HostConfig.RestartPolicy.Name,
		Privileged: payload.HostConfig.Privileged, ReadonlyRootfs: payload.HostConfig.ReadonlyRootfs,
		CapAdd: append([]string(nil), payload.HostConfig.CapAdd...), CapDrop: append([]string(nil), payload.HostConfig.CapDrop...),
		SecurityOpt: append([]string(nil), payload.HostConfig.SecurityOpt...),
		PIDsLimit:   payload.HostConfig.PidsLimit, MemoryBytes: payload.HostConfig.Memory, NanoCPUs: payload.HostConfig.NanoCPUs,
		Init: payload.HostConfig.Init != nil && *payload.HostConfig.Init,
	}
	if len(payload.HostConfig.Tmpfs) > 0 {
		state.Tmpfs = make(map[string]string, len(payload.HostConfig.Tmpfs))
		for path, options := range payload.HostConfig.Tmpfs {
			state.Tmpfs[path] = options
		}
	}
	if !payload.State.Running && status == StatusExited {
		exit := payload.State.ExitCode
		state.ExitCode = &exit
	}
	for _, bindings := range payload.HostConfig.PortBindings {
		state.PublishedPorts += len(bindings)
	}
	for _, bindings := range payload.NetworkSettings.Ports {
		state.PublishedPorts += len(bindings)
	}
	for _, mount := range payload.Mounts {
		state.Mounts = append(state.Mounts, MountFact{
			Type: mount.Type, Source: mount.Source, Destination: mount.Destination,
			ReadWrite: mount.RW, Propagation: mount.Propagation,
		})
	}
	return state, nil
}

func inspectEnvironment(values []string) ([]EnvironmentFact, error) {
	facts := make([]EnvironmentFact, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvName(name) {
			return nil, isolationMismatch("environment")
		}
		if _, exists := seen[name]; exists {
			return nil, isolationMismatch("environment")
		}
		seen[name] = struct{}{}
		facts = append(facts, EnvironmentFact{Name: name, ValueDigest: environmentValueDigest(value)})
	}
	sort.Slice(facts, func(left, right int) bool { return facts[left].Name < facts[right].Name })
	return facts, nil
}

func environmentValueDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func refFromLabels(id, name string, labels map[string]string) (RuntimeRef, error) {
	generation, err := strconv.ParseInt(labels[LabelGeneration], 10, 64)
	if err != nil || generation < 1 || labels[LabelManaged] != "true" || labels[LabelContract] != "v1" {
		return RuntimeRef{}, ErrOwnership
	}
	ref := RuntimeRef{
		ContainerID: id, ContainerName: name, ProjectID: labels[LabelProjectID], TaskID: labels[LabelTaskID],
		AgentID: labels[LabelAgentID], RunID: labels[LabelRunID], Generation: generation,
		LaunchNonce: labels[LabelLaunchNonce],
	}
	if err := validateRef(ref); err != nil {
		return RuntimeRef{}, ErrOwnership
	}
	return ref, nil
}

func (e *DockerExecutor) call(ctx context.Context, method, path string, input any, headers http.Header) (*http.Response, error) {
	if e == nil || e.client == nil {
		return nil, ErrUnavailable
	}
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("runtime: encode Docker request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return nil, fmt.Errorf("runtime: create Docker request: %w", err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := e.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return response, nil
}

func containerPath(ref RuntimeRef, suffix string) string {
	lookup := ref.ContainerID
	if lookup == "" {
		lookup = ref.ContainerName
	}
	return "/" + dockerAPIVersion + "/containers/" + url.PathEscape(lookup) + suffix
}

func dockerResponseError(response *http.Response, action string) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &payload)
	message := bounded(payload.Message)
	if message == "" {
		message = bounded(strings.TrimSpace(string(raw)))
	}
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("runtime: %s: %s", action, message)
}

func decodeResponse(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 4<<20))
	return decoder.Decode(target)
}

func bounded(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		return value[:2048] + "...[truncated]"
	}
	return value
}

func validEnvName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_' ||
			(index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

type demuxReadCloser struct {
	reader    *bufio.Reader
	body      io.ReadCloser
	remaining uint32
}

func (r *demuxReadCloser) Read(target []byte) (int, error) {
	if r.remaining == 0 {
		header := make([]byte, 8)
		if _, err := io.ReadFull(r.reader, header); err != nil {
			return 0, err
		}
		r.remaining = binary.BigEndian.Uint32(header[4:])
		if r.remaining == 0 {
			return r.Read(target)
		}
	}
	if uint32(len(target)) > r.remaining {
		target = target[:r.remaining]
	}
	count, err := r.reader.Read(target)
	r.remaining -= uint32(count)
	return count, err
}

func (r *demuxReadCloser) Close() error { return r.body.Close() }

var _ Executor = (*DockerExecutor)(nil)
