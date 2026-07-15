package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func removeInitialWorkspaceReflogs(workspace string) error {
	gitDir := filepath.Join(workspace, ".git")
	if err := validateDirectDirectory(gitDir, "workspace Git directory"); err != nil {
		return err
	}
	logs := filepath.Join(gitDir, "logs")
	info, err := os.Lstat(logs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect initial workspace reflogs: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("initial workspace reflogs must be a direct directory")
	}
	if err := os.RemoveAll(logs); err != nil {
		return fmt.Errorf("remove initial workspace reflogs: %w", err)
	}
	if err := syncDirectory(gitDir); err != nil {
		return fmt.Errorf("sync workspace Git directory after reflog removal: %w", err)
	}
	return nil
}

func normalizeWorkspaceAccess(workspace string) error {
	return filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk private workspace: %w", walkErr)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect private workspace entry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("private workspace contains an unsupported filesystem entry")
		}
		owner := info.Mode().Perm() & 0o700
		mode := owner | owner>>3
		if info.IsDir() {
			mode |= os.ModeSetgid
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("set private workspace group access: %w", err)
		}
		return nil
	})
}

func validateWorkspaceAccess(workspace string) error {
	return filepath.WalkDir(workspace, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk private workspace access: %w", walkErr)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect private workspace access: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("private workspace contains an unsupported filesystem entry")
		}
		permissions := info.Mode().Perm()
		owner := permissions & 0o700
		if permissions&0o077 != owner>>3 {
			return errors.New("private workspace access must mirror owner permissions to group and deny world access")
		}
		if info.IsDir() && info.Mode()&os.ModeSetgid == 0 {
			return errors.New("private workspace directories must preserve the daemon group")
		}
		return nil
	})
}

func validateWorkspaceRootAccess(workspace string) error {
	info, err := os.Lstat(workspace)
	if err != nil {
		return fmt.Errorf("inspect private workspace root access: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("private workspace root must be a direct directory")
	}
	if info.Mode().Perm() != 0o770 || info.Mode()&os.ModeSetgid == 0 {
		return errors.New("private workspace root must be group-rw setgid without world access")
	}
	return nil
}

func (m *WorkspaceManager) validateSpec(ctx context.Context, spec WorkspaceSpec) error {
	if m == nil || m.initializer == nil {
		return errors.New("nil workspace manager")
	}
	if err := m.validateRoot(); err != nil {
		return err
	}
	if err := validateID("project", spec.ProjectID); err != nil {
		return err
	}
	if err := validateID("task", spec.TaskID); err != nil {
		return err
	}
	if err := validateObjectID(spec.BaseSHA); err != nil {
		return fmt.Errorf("invalid workspace base SHA: %w", err)
	}
	refs := []string{taskBranch(spec.TaskID)}
	if spec.Source != nil {
		if err := validateID("source task", spec.Source.TaskID); err != nil {
			return err
		}
		if err := validateID("source run", spec.Source.RunID); err != nil {
			return err
		}
		if err := validateObjectID(spec.Source.HeadSHA); err != nil {
			return fmt.Errorf("invalid source head SHA: %w", err)
		}
		if spec.Source.TaskRef != taskResultRef(spec.Source.TaskID, spec.Source.RunID) {
			return errors.New("source task ref does not match source task and run IDs")
		}
		refs = append(refs, spec.Source.TaskRef, spec.Source.ConvenienceRef())
	}
	for _, ref := range refs {
		if _, err := m.git(ctx, "validate generated workspace ref", "check-ref-format", ref); err != nil {
			return errors.New("generated workspace ref is invalid")
		}
	}
	return nil
}

func (m *WorkspaceManager) validateRoot() error {
	if m == nil {
		return errors.New("nil workspace manager")
	}
	if err := validateDirectDirectory(m.root, "workspace root"); err != nil {
		return err
	}
	if err := m.initializer.validateRoot(); err != nil {
		return err
	}
	if pathsOverlap(m.root, m.initializer.root) {
		return errors.New("workspace and control repository roots overlap")
	}
	return nil
}

func (m *WorkspaceManager) ensureSubdirectories(target string) error {
	if err := m.validateRoot(); err != nil {
		return err
	}
	return ensureDirectSubdirectories(m.root, target, "workspace")
}

func (m *WorkspaceManager) validateFinalWorkspacePath(path string, spec WorkspaceSpec) error {
	want, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return err
	}
	if path != want || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("workspace path is not deterministic")
	}
	if err := validateDirectDirectory(filepath.Dir(path), "workspace project directory"); err != nil {
		return err
	}
	if err := validateDirectDirectory(path, "task workspace"); err != nil {
		return err
	}
	markerPath, err := m.markerPath(spec.ProjectID, spec.TaskID)
	if err != nil {
		return err
	}
	marker, err := readWorkspaceMarker(markerPath)
	if err != nil {
		return err
	}
	if marker != markerForSpec(spec) {
		return errors.New("workspace ownership marker does not match immutable Task inputs")
	}
	return nil
}

func (m *WorkspaceManager) markerPath(projectID, taskID string) (string, error) {
	path, err := m.Path(projectID, taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), ".coordplane-workspace-"+taskID+".json"), nil
}

func (m *WorkspaceManager) git(ctx context.Context, operation string, args ...string) (string, error) {
	output, err := m.initializer.git(ctx, args...)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New(operation + " failed")
	}
	return output, nil
}

type workspacePublicError struct {
	message   string
	cause     error
	invariant bool
}

func (e *workspacePublicError) Error() string { return e.message }
func (e *workspacePublicError) Is(target error) bool {
	return errors.Is(e.cause, target)
}
func (e *workspacePublicError) GitInvariant() bool { return e.invariant }

func (m *WorkspaceManager) publicError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	message := cause.Error()
	var invariantError *InvariantError
	var commandError *gitCommandError
	switch {
	case errors.As(cause, &invariantError):
		message = "gitrepo: " + invariantError.message
	case errors.As(cause, &commandError):
		message = "git operation failed"
	}
	var replacements []struct{ old, new string }
	if m != nil {
		replacements = append(replacements, struct{ old, new string }{m.root, "<workspace-root>"})
		if m.initializer != nil {
			replacements = append(replacements, struct{ old, new string }{m.initializer.root, "<control-root>"})
		}
	}
	sort.SliceStable(replacements, func(a, b int) bool { return len(replacements[a].old) > len(replacements[b].old) })
	for _, replacement := range replacements {
		if replacement.old != "" {
			message = strings.ReplaceAll(message, replacement.old, replacement.new)
		}
	}
	var safeCause error
	var invariantCause interface{ GitInvariant() bool }
	invariant := errors.As(cause, &invariantCause) && invariantCause.GitInvariant()
	switch {
	case errors.Is(cause, context.Canceled):
		safeCause = context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		safeCause = context.DeadlineExceeded
	}
	return &workspacePublicError{message: "gitrepo: workspace " + operation + ": " + message, cause: safeCause, invariant: invariant}
}

func taskBranch(taskID string) string { return "refs/heads/coordplane/task/" + taskID }

func taskResultRef(taskID, runID string) string {
	return "refs/coordplane/tasks/" + taskID + "/runs/" + runID
}

func pathsOverlap(first, second string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && (relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))))
	}
	return contains(first, second) || contains(second, first)
}
