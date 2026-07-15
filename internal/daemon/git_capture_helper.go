package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"coordplane/internal/config"
	"coordplane/internal/gitcapture"
	"coordplane/internal/gitrepo"
	"coordplane/internal/perfobs"
	containerruntime "coordplane/internal/runtime"
)

const captureHelperContainerUID = 65532

type dockerCaptureHelper struct {
	executor   containerruntime.Executor
	config     config.GitConfig
	root       string
	executable string
	mu         sync.Mutex
}

func newDockerCaptureHelper(
	executor containerruntime.Executor,
	cfg config.GitConfig,
	root, executable string,
) (*dockerCaptureHelper, error) {
	if executor == nil {
		return nil, errors.New("capture helper: Docker executor is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("capture helper: create handoff root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(canonical) {
		return nil, errors.New("capture helper: handoff root must be canonical and absolute")
	}
	return &dockerCaptureHelper{
		executor: executor, config: cfg, root: canonical, executable: executable,
	}, nil
}

func (h *dockerCaptureHelper) Capture(ctx context.Context, request gitrepo.CaptureHelperRequest) (gitrepo.CaptureHelperFact, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	handoff := h.handoffPath(request)
	if err := h.prepareHandoff(handoff); err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	if fact, ok, err := h.readyFact(handoff, request); ok || err != nil {
		return fact, err
	}
	usage, err := directoryBytes(h.root)
	if err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	if usage+h.config.MaximumBundleBytes > h.config.MaximumHandoffBytes {
		return gitrepo.CaptureHelperFact{}, errors.New("capture helper: total handoff quota exceeded")
	}
	executable, err := canonicalHelperExecutable(h.executable)
	if err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	ref := captureRuntimeRef(request)
	if err := h.runContainer(ctx, h.containerSpec(request, handoff, executable, ref), true); err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	fact, ok, err := h.readyFact(handoff, request)
	if err != nil {
		return gitrepo.CaptureHelperFact{}, err
	}
	if !ok {
		return gitrepo.CaptureHelperFact{}, errors.New("capture helper: successful container did not publish ready handoff")
	}
	return fact, nil
}

func (h *dockerCaptureHelper) Inspect(ctx context.Context, request gitrepo.WorkspaceInspectRequest) (gitrepo.WorkspaceInspectFact, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	usage, err := directoryBytes(h.root)
	if err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	if usage+(64<<10) > h.config.MaximumHandoffBytes {
		return gitrepo.WorkspaceInspectFact{}, errors.New("capture helper: total handoff quota exceeded")
	}
	handoff, operationID, err := h.prepareInspectHandoff(request)
	if err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	executable, err := canonicalHelperExecutable(h.executable)
	if err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	ref := inspectRuntimeRef(request, operationID)
	if err := h.runContainer(ctx, h.inspectContainerSpec(request, handoff, executable, ref), false); err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	fact, err := h.readyInspectFact(handoff)
	if err != nil {
		return gitrepo.WorkspaceInspectFact{}, err
	}
	if err := os.RemoveAll(handoff); err != nil {
		return gitrepo.WorkspaceInspectFact{}, fmt.Errorf("capture helper: clean successful inspect handoff: %w", err)
	}
	return fact, nil
}

func (h *dockerCaptureHelper) Cleanup(_ context.Context, request gitrepo.CaptureHelperRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return os.RemoveAll(h.handoffPath(request))
}

func (h *dockerCaptureHelper) Recover(valid []gitrepo.CaptureHelperRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	quarantineRoot := filepath.Join(h.root, "quarantine")
	if err := os.RemoveAll(quarantineRoot); err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(valid))
	for _, request := range valid {
		wanted[h.handoffPath(request)] = struct{}{}
	}
	projects, err := os.ReadDir(h.root)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(h.root, project.Name())
		if err := h.recoverProject(projectPath, wanted); err != nil {
			return err
		}
	}
	return os.RemoveAll(quarantineRoot)
}

func (h *dockerCaptureHelper) recoverProject(projectPath string, wanted map[string]struct{}) error {
	return filepath.WalkDir(projectPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || path == projectPath {
			return nil
		}
		relative, err := filepath.Rel(projectPath, path)
		if err != nil {
			return err
		}
		if len(strings.Split(relative, string(filepath.Separator))) != 2 {
			return nil
		}
		if _, ok := wanted[path]; ok {
			_ = os.RemoveAll(filepath.Join(path, gitcapture.PartialName))
			return filepath.SkipDir
		}
		if err := h.quarantine(path); err != nil {
			return err
		}
		return filepath.SkipDir
	})
}

func (h *dockerCaptureHelper) quarantine(path string) error {
	root := filepath.Join(h.root, "quarantine")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(path + time.Now().UTC().Format(time.RFC3339Nano)))
	target := filepath.Join(root, hex.EncodeToString(digest[:12]))
	if err := os.Rename(path, target); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

func (h *dockerCaptureHelper) handoffPath(request gitrepo.CaptureHelperRequest) string {
	return filepath.Join(h.root, request.ProjectID, request.TaskID, request.RunID)
}

func (h *dockerCaptureHelper) prepareHandoff(path string) error {
	if err := os.MkdirAll(path, 0o770); err != nil {
		return fmt.Errorf("capture helper: create per-Run handoff: %w", err)
	}
	if err := os.Chmod(path, 0o770); err != nil {
		return fmt.Errorf("capture helper: set per-Run handoff access: %w", err)
	}
	return os.RemoveAll(filepath.Join(path, gitcapture.PartialName))
}

func (h *dockerCaptureHelper) prepareInspectHandoff(request gitrepo.WorkspaceInspectRequest) (string, string, error) {
	parent := filepath.Join(h.root, request.ProjectID, request.TaskID)
	if err := os.MkdirAll(parent, 0o770); err != nil {
		return "", "", fmt.Errorf("capture helper: create inspect handoff parent: %w", err)
	}
	if err := os.Chmod(parent, 0o770); err != nil {
		return "", "", fmt.Errorf("capture helper: set inspect handoff access: %w", err)
	}
	handoff, err := os.MkdirTemp(parent, "inspect-")
	if err != nil {
		return "", "", fmt.Errorf("capture helper: create inspect handoff: %w", err)
	}
	if err := os.Chmod(handoff, 0o770); err != nil {
		return "", "", fmt.Errorf("capture helper: set inspect handoff access: %w", err)
	}
	return handoff, filepath.Base(handoff), nil
}

func (h *dockerCaptureHelper) containerSpec(
	request gitrepo.CaptureHelperRequest,
	handoff, executable string,
	ref containerruntime.RuntimeRef,
) containerruntime.ContainerSpec {
	args := []string{
		"capture", "--workspace", "/workspace", "--handoff", "/handoff",
		"--expected-head", request.ExpectedHead, "--base", request.BaseSHA,
		"--max-bundle-bytes", strconv.FormatInt(h.config.MaximumBundleBytes, 10),
		"--max-objects", strconv.Itoa(h.config.MaximumObjects),
	}
	if request.SourceSHA != "" {
		args = append(args, "--source", request.SourceSHA)
	}
	return h.helperContainerSpec(request.Workspace, handoff, executable, ref, args)
}

func (h *dockerCaptureHelper) inspectContainerSpec(
	request gitrepo.WorkspaceInspectRequest,
	handoff, executable string,
	ref containerruntime.RuntimeRef,
) containerruntime.ContainerSpec {
	return h.helperContainerSpec(request.Workspace, handoff, executable, ref, []string{
		"inspect", "--workspace", "/workspace", "--handoff", "/handoff",
		"--max-objects", strconv.Itoa(h.config.MaximumObjects),
	})
}

func (h *dockerCaptureHelper) helperContainerSpec(
	workspace, handoff, executable string,
	ref containerruntime.RuntimeRef,
	args []string,
) containerruntime.ContainerSpec {
	gid := strconv.Itoa(os.Getgid())
	return containerruntime.ContainerSpec{
		Ref: ref, Image: h.config.CaptureHelperImage,
		Command:    containerruntime.CommandSpec{Executable: "/usr/local/bin/coordplane-git-helper", Args: args},
		WorkingDir: "/workspace", User: strconv.Itoa(captureHelperContainerUID) + ":" + gid,
		GroupAdd: []string{gid}, Network: "none",
		Mounts: []containerruntime.Mount{
			{Source: workspace, Target: "/workspace", ReadOnly: true},
			{Source: handoff, Target: "/handoff"},
			{Source: executable, Target: "/usr/local/bin/coordplane-git-helper", ReadOnly: true},
		},
		ReadOnlyRoot: true,
		Limits: containerruntime.ResourceLimits{
			PIDs: 64, MemoryBytes: 128 << 20, NanoCPUs: 1_000_000_000, TmpfsBytes: 128 << 20,
		},
	}
}

func (h *dockerCaptureHelper) runContainer(ctx context.Context, spec containerruntime.ContainerSpec, adopt bool) error {
	runCtx, cancel := context.WithTimeout(ctx, h.config.CaptureTimeout)
	defer cancel()
	ref := spec.Ref
	running := false
	if adopt {
		state, err := h.executor.Inspect(runCtx, ref)
		switch {
		case err == nil && state.Running:
			ref, running = state.Ref, true
		case err == nil:
			_ = h.removeContainer(runCtx, state.Ref)
		case !errors.Is(err, containerruntime.ErrNotFound):
			return fmt.Errorf("capture helper: inspect container: %w", err)
		}
	}
	if !running {
		created, err := h.executor.Create(runCtx, spec)
		if err != nil {
			return fmt.Errorf("capture helper: create container: %w", err)
		}
		ref, err = h.executor.Start(runCtx, created)
		if err != nil {
			_ = h.removeContainer(context.Background(), created)
			return fmt.Errorf("capture helper: start container: %w", err)
		}
	}
	live, err := h.executor.Inspect(runCtx, ref)
	if err != nil {
		return fmt.Errorf("capture helper: inspect started container: %w", err)
	}
	if !live.Running {
		return errors.New("capture helper: started container is not running")
	}
	role := "git_capture"
	if strings.HasPrefix(ref.ContainerName, "coordplane-git-inspect-") {
		role = "git_inspect"
	}
	perfobs.RuntimeLimit(perfobs.Fields{ProjectID: ref.ProjectID, TaskID: ref.TaskID, RunID: ref.RunID}, role, live.MemoryBytes, live.NanoCPUs, live.PIDsLimit)
	exit, waitErr := h.executor.Wait(runCtx, ref)
	logs := h.containerLogs(runCtx, ref)
	removeErr := h.removeContainer(context.Background(), ref)
	if waitErr != nil {
		return fmt.Errorf("capture helper: wait for container: %w", waitErr)
	}
	if exit.ExitCode != 0 {
		return fmt.Errorf("capture helper: container exited %d: %s", exit.ExitCode, logs)
	}
	return removeErr
}

func captureRuntimeRef(request gitrepo.CaptureHelperRequest) containerruntime.RuntimeRef {
	digest := sha256.Sum256([]byte(request.ProjectID + "\x00" + request.TaskID + "\x00" + request.RunID))
	short := hex.EncodeToString(digest[:12])
	return containerruntime.RuntimeRef{
		ContainerName: "coordplane-git-capture-" + short,
		ProjectID:     request.ProjectID, TaskID: request.TaskID, AgentID: "git-helper",
		RunID: request.RunID, Generation: 1, LaunchNonce: short,
	}
}

func inspectRuntimeRef(request gitrepo.WorkspaceInspectRequest, operationID string) containerruntime.RuntimeRef {
	digest := sha256.Sum256([]byte(request.ProjectID + "\x00" + request.TaskID + "\x00" + operationID))
	short := hex.EncodeToString(digest[:12])
	return containerruntime.RuntimeRef{
		ContainerName: "coordplane-git-inspect-" + short,
		ProjectID:     request.ProjectID, TaskID: request.TaskID, AgentID: "git-helper",
		RunID: operationID, Generation: 1, LaunchNonce: short,
	}
}

func (h *dockerCaptureHelper) readyFact(path string, request gitrepo.CaptureHelperRequest) (gitrepo.CaptureHelperFact, bool, error) {
	ready := filepath.Join(path, gitcapture.ReadyName)
	info, err := os.Lstat(ready)
	if errors.Is(err, os.ErrNotExist) {
		return gitrepo.CaptureHelperFact{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return gitrepo.CaptureHelperFact{}, true, errors.New("capture helper: ready handoff is not a direct directory")
	}
	factsPath := filepath.Join(ready, gitcapture.FactsName)
	raw, err := os.ReadFile(factsPath)
	if err != nil || len(raw) > 64<<10 {
		return gitrepo.CaptureHelperFact{}, true, errors.New("capture helper: ready facts are missing or oversized")
	}
	var fact gitcapture.Fact
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return gitrepo.CaptureHelperFact{}, true, fmt.Errorf("capture helper: decode ready facts: %w", err)
	}
	bundle := filepath.Join(ready, gitcapture.BundleName)
	bundleInfo, err := os.Lstat(bundle)
	if err != nil || bundleInfo.Mode()&os.ModeSymlink != 0 || !bundleInfo.Mode().IsRegular() {
		return gitrepo.CaptureHelperFact{}, true, errors.New("capture helper: ready bundle is not a direct regular file")
	}
	if fact.HeadSHA != request.ExpectedHead || fact.BundleBytes != bundleInfo.Size() ||
		fact.BundleBytes <= 0 || fact.BundleBytes > h.config.MaximumBundleBytes ||
		fact.ObjectCount <= 0 || fact.ObjectCount > h.config.MaximumObjects || !fact.Clean || fact.Unfinished {
		return gitrepo.CaptureHelperFact{}, true, errors.New("capture helper: ready handoff violates identity or quota")
	}
	return gitrepo.CaptureHelperFact{
		HeadSHA: fact.HeadSHA, ReadyBundle: bundle,
		BundleBytes: fact.BundleBytes, ObjectCount: fact.ObjectCount,
	}, true, nil
}

func (h *dockerCaptureHelper) readyInspectFact(path string) (gitrepo.WorkspaceInspectFact, error) {
	ready := filepath.Join(path, gitcapture.InspectReadyName)
	info, err := os.Lstat(ready)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return gitrepo.WorkspaceInspectFact{}, errors.New("capture helper: inspect ready handoff is not a direct directory")
	}
	raw, err := os.ReadFile(filepath.Join(ready, gitcapture.FactsName))
	if err != nil || len(raw) > 64<<10 {
		return gitrepo.WorkspaceInspectFact{}, errors.New("capture helper: inspect ready facts are missing or oversized")
	}
	var fact gitcapture.Fact
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return gitrepo.WorkspaceInspectFact{}, fmt.Errorf("capture helper: decode inspect facts: %w", err)
	}
	if fact.HeadSHA == "" || len(fact.StatusDigest) != sha256.Size*2 || fact.BundleBytes != 0 ||
		fact.ObjectCount <= 0 || fact.ObjectCount > h.config.MaximumObjects {
		return gitrepo.WorkspaceInspectFact{}, errors.New("capture helper: inspect ready facts violate identity or quota")
	}
	if _, err := hex.DecodeString(fact.StatusDigest); err != nil {
		return gitrepo.WorkspaceInspectFact{}, errors.New("capture helper: inspect ready status digest is invalid")
	}
	return gitrepo.WorkspaceInspectFact{
		HeadSHA: fact.HeadSHA, StatusDigest: fact.StatusDigest, ObjectCount: fact.ObjectCount,
		Clean: fact.Clean, Unfinished: fact.Unfinished,
	}, nil
}

func (h *dockerCaptureHelper) containerLogs(ctx context.Context, ref containerruntime.RuntimeRef) string {
	reader, err := h.executor.Logs(ctx, ref, false)
	if err != nil {
		return "logs unavailable"
	}
	defer reader.Close()
	raw, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
	return strings.TrimSpace(string(raw))
}

func (h *dockerCaptureHelper) removeContainer(ctx context.Context, ref containerruntime.RuntimeRef) error {
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := h.executor.Remove(removeCtx, ref); err != nil {
		return fmt.Errorf("capture helper: remove container: %w", err)
	}
	return nil
}

func canonicalHelperExecutable(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return "", errors.New("capture helper: executable path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("capture helper: resolve executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("capture helper: executable is missing or not executable")
	}
	return resolved, nil
}

func directoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func resolveGitCaptureHelperExecutable() string {
	if configured := strings.TrimSpace(os.Getenv("COORDPLANE_GIT_CAPTURE_HELPER")); configured != "" {
		return configured
	}
	executable, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(executable), "coordplane-git-helper")
	}
	return "/usr/local/bin/coordplane-git-helper"
}

var _ gitrepo.CaptureHelper = (*dockerCaptureHelper)(nil)
