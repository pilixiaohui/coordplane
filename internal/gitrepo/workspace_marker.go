package gitrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type workspaceMarker struct {
	Version       int    `json:"version"`
	ProjectID     string `json:"project_id"`
	TaskID        string `json:"task_id"`
	BaseSHA       string `json:"base_sha"`
	SourceTaskID  string `json:"source_task_id,omitempty"`
	SourceRunID   string `json:"source_run_id,omitempty"`
	SourceTaskRef string `json:"source_task_ref,omitempty"`
	SourceHeadSHA string `json:"source_head_sha,omitempty"`
}

func markerForSpec(spec WorkspaceSpec) workspaceMarker {
	marker := workspaceMarker{
		Version: workspaceMarkerVersion, ProjectID: spec.ProjectID, TaskID: spec.TaskID, BaseSHA: spec.BaseSHA,
	}
	if spec.Source != nil {
		marker.SourceTaskID = spec.Source.TaskID
		marker.SourceRunID = spec.Source.RunID
		marker.SourceTaskRef = spec.Source.TaskRef
		marker.SourceHeadSHA = spec.Source.HeadSHA
	}
	return marker
}

func ensureWorkspaceMarker(path string, want workspaceMarker) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		got, err := readWorkspaceMarker(path)
		if err != nil {
			return false, err
		}
		if got != want {
			return false, errors.New("existing workspace ownership marker does not match immutable Task inputs")
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect workspace ownership marker: %w", err)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return false, fmt.Errorf("encode workspace ownership marker: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create workspace ownership marker: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("write workspace ownership marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("sync workspace ownership marker: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("close workspace ownership marker: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("sync workspace ownership marker parent: %w", err)
	}
	return true, nil
}

func readWorkspaceMarker(path string) (workspaceMarker, error) {
	marker, err := readStrictMarker[workspaceMarker](path, "workspace ownership marker")
	if err != nil {
		return workspaceMarker{}, err
	}
	if marker.Version != workspaceMarkerVersion {
		return workspaceMarker{}, errors.New("unsupported workspace ownership marker version")
	}
	return marker, nil
}
