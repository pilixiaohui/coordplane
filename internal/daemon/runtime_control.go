package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

const (
	runControlMarkerVersion = 1
	runControlMarkerName    = "identity.json"
	runControlFileMode      = 0o440
	runControlDirectoryMode = 0o750
	maxRunControlFileBytes  = 64 << 10
)

type runControlMarker struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"project_id"`
	TaskID      string `json:"task_id"`
	AgentID     string `json:"agent_id"`
	RunID       string `json:"run_id"`
	Generation  int64  `json:"generation"`
	LaunchNonce string `json:"launch_nonce"`
}

type runScopeAuthorizer func(context.Context, string, core.RunScope) error

func markerForRun(run core.Run) runControlMarker {
	return runControlMarker{
		Version: runControlMarkerVersion, ProjectID: run.ProjectID, TaskID: run.TaskID,
		AgentID: run.AgentID, RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
	}
}

func writeRunControlMarker(controlPath string, run core.Run) error {
	raw, err := json.Marshal(markerForRun(run))
	if err != nil {
		return errors.New("encode run control identity")
	}
	return writeRuntimeFile(filepath.Join(controlPath, runControlMarkerName), raw, runControlFileMode)
}

func validateRunControl(
	ctx context.Context,
	controlRoot string,
	run core.Run,
	authorize runScopeAuthorizer,
) error {
	if err := validateRunControlIdentity(controlRoot, run); err != nil {
		return err
	}
	path := filepath.Join(controlRoot, run.ID)
	tokenRaw, err := readOwnedRunControlFile(filepath.Join(path, "token"))
	if err != nil {
		return controlOwnershipError(err)
	}
	token, err := decodeRunControlToken(tokenRaw)
	if err != nil {
		return controlOwnershipError(err)
	}
	if authorize == nil {
		return controlOwnershipError(errors.New("run control token authorizer is unavailable"))
	}
	scope := core.RunScope{
		ProjectID: run.ProjectID, AgentID: run.AgentID, TaskID: run.TaskID,
		RunID: run.ID, Generation: run.Generation,
	}
	if err := authorize(ctx, token, scope); err != nil {
		return controlOwnershipError(errors.New("run control token does not match the durable Run"))
	}
	return nil
}

func validateRunControlIdentity(controlRoot string, run core.Run) error {
	path := filepath.Join(controlRoot, run.ID)
	if err := validateRunControlDirectory(controlRoot, path, run.ID); err != nil {
		return controlOwnershipError(err)
	}
	markerRaw, err := readOwnedRunControlFile(filepath.Join(path, runControlMarkerName))
	if err != nil {
		return controlOwnershipError(err)
	}
	marker, err := decodeRunControlMarker(markerRaw)
	if err != nil || marker != markerForRun(run) {
		return controlOwnershipError(errors.New("run control identity does not match the durable Run"))
	}
	return nil
}

func validateRunControlDirectory(root, path, runID string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !filepath.IsAbs(root) || path != filepath.Join(root, runID) {
		return errors.New("run control directory is not deterministic")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("run control directory escaped its root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("run control directory is missing")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != runControlDirectoryMode {
		return errors.New("run control directory has invalid type or mode")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("run control directory traverses a symlink")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getgid()) {
		return errors.New("run control directory has invalid ownership")
	}
	return nil
}

func readOwnedRunControlFile(path string) ([]byte, error) {
	file, err := openOwnedRunControlFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxRunControlFileBytes+1))
	if err != nil || len(raw) > maxRunControlFileBytes {
		return nil, errors.New("run control file is unreadable or too large")
	}
	return raw, nil
}

func validateOwnedRunControlFile(path string) error {
	file, err := openOwnedRunControlFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 {
		return errors.New("run control file is empty or unreadable")
	}
	return nil
}

func openOwnedRunControlFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("run control file is missing or indirect")
	}
	file := os.NewFile(uintptr(fd), "run-control-file")
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("run control file descriptor is invalid")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, errors.New("inspect opened run control file")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("run control file changed during validation")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getgid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("run control file has invalid ownership")
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != runControlFileMode {
		_ = file.Close()
		return nil, errors.New("run control file has invalid type or mode")
	}
	return file, nil
}

func decodeRunControlMarker(raw []byte) (runControlMarker, error) {
	var marker runControlMarker
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return runControlMarker{}, errors.New("invalid run control identity")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return runControlMarker{}, errors.New("run control identity has trailing content")
	}
	return marker, nil
}

func decodeRunControlToken(raw []byte) (string, error) {
	if len(raw) < 2 || raw[len(raw)-1] != '\n' {
		return "", errors.New("run control token has invalid framing")
	}
	token := string(raw[:len(raw)-1])
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("run control token has invalid framing")
	}
	return token, nil
}

func controlOwnershipError(cause error) error {
	return fmt.Errorf("%w: %v", containerruntime.ErrOwnership, cause)
}
