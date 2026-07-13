package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const workspaceMarkerVersion = 1

// WorkspaceSource is the immutable controller-owned identity of an optional
// captured source result. TaskRef must match TaskID and RunID exactly.
type WorkspaceSource struct {
	TaskID  string
	RunID   string
	TaskRef string
	HeadSHA string
}

// ConvenienceRef is the movable local branch created for an imported source
// result. The saved HeadSHA remains authoritative.
func (s WorkspaceSource) ConvenienceRef() string {
	return "refs/heads/coordplane/source/" + s.TaskID
}

// WorkspaceSpec contains only durable Task inputs. It deliberately contains
// no caller-provided host path or branch name.
type WorkspaceSpec struct {
	ProjectID string
	TaskID    string
	BaseSHA   string
	Source    *WorkspaceSource
}

// WorkspaceFact is a verified filesystem/Git observation. Path is a
// daemon-internal host path and must not be rendered to an Agent. TaskBranch
// and SourceRef name the initially-created convenience refs; only HeadSHA is
// resolved on every Verify because an Agent may move or leave those refs.
type WorkspaceFact struct {
	Path       string
	HeadSHA    string
	TaskBranch string
	SourceRef  string
}

// WorkspaceManager owns private clone materialization and verification. The
// caller owns lifecycle authorization and decides whether Materialize or
// Verify is legal for the current durable Run history.
type WorkspaceManager struct {
	initializer *Initializer
	root        string
}

// NewWorkspaceManager binds private workspaces to one daemon-owned repository
// initializer. The roots must be disjoint so a workspace mount can never
// contain a control repository by configuration.
func NewWorkspaceManager(initializer *Initializer, workspaceRoot string) (*WorkspaceManager, error) {
	if initializer == nil {
		return nil, errors.New("gitrepo: workspace initializer is required")
	}
	if err := initializer.validateRoot(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("gitrepo: workspace root is required")
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: resolve workspace root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("gitrepo: create workspace root: %w", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: canonicalize workspace root: %w", err)
	}
	if err := validateDirectDirectory(root, "workspace root"); err != nil {
		return nil, err
	}
	if pathsOverlap(root, initializer.root) {
		return nil, errors.New("gitrepo: workspace and control repository roots must be disjoint")
	}
	return &WorkspaceManager{initializer: initializer, root: root}, nil
}

// Path returns the only valid host workspace path for a Task.
func (m *WorkspaceManager) Path(projectID, taskID string) (string, error) {
	if m == nil {
		return "", errors.New("gitrepo: nil workspace manager")
	}
	if err := validateID("project", projectID); err != nil {
		return "", err
	}
	if err := validateID("task", taskID); err != nil {
		return "", err
	}
	return filepath.Join(m.root, projectID, taskID), nil
}

// Materialize creates a new private clone and atomically publishes it at the
// deterministic Task path. It never adopts or mutates an existing workspace.
func (m *WorkspaceManager) Materialize(ctx context.Context, spec WorkspaceSpec) (WorkspaceFact, error) {
	if err := m.validateSpec(ctx, spec); err != nil {
		return WorkspaceFact{}, m.publicError("materialize", err)
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return WorkspaceFact{}, m.publicError("materialize", err)
	}
	defer unlock()
	fact, err := m.materializeLocked(ctx, spec)
	if err != nil {
		return WorkspaceFact{}, m.publicError("materialize", err)
	}
	return fact, nil
}

// Verify validates an existing workspace without checkout, reset, clean, ref
// repair, or config mutation. It never creates a missing workspace.
func (m *WorkspaceManager) Verify(ctx context.Context, spec WorkspaceSpec) (WorkspaceFact, error) {
	if err := m.validateSpec(ctx, spec); err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	defer unlock()
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	exists, err := pathExists(path)
	if err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	if !exists {
		return WorkspaceFact{}, m.publicError("verify", errors.New("workspace does not exist"))
	}
	if err := m.validateFinalWorkspacePath(path, spec); err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	fact, err := m.inspect(ctx, path, spec, false)
	if err != nil {
		return WorkspaceFact{}, m.publicError("verify", err)
	}
	return fact, nil
}

func (m *WorkspaceManager) materializeLocked(ctx context.Context, spec WorkspaceSpec) (fact WorkspaceFact, resultErr error) {
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return WorkspaceFact{}, err
	}
	if exists, err := pathExists(path); err != nil {
		return WorkspaceFact{}, err
	} else if exists {
		return WorkspaceFact{}, errors.New("workspace already exists; use Verify")
	}
	controlPath, err := m.verifyControlInputs(ctx, spec)
	if err != nil {
		return WorkspaceFact{}, err
	}
	projectRoot := filepath.Dir(path)
	partialRoot := filepath.Join(m.root, ".partial", spec.ProjectID)
	if err := m.ensureSubdirectories(projectRoot); err != nil {
		return WorkspaceFact{}, err
	}
	if err := m.ensureSubdirectories(partialRoot); err != nil {
		return WorkspaceFact{}, err
	}
	templateRoot := filepath.Join(m.root, ".empty-git-template")
	if err := m.ensureSubdirectories(templateRoot); err != nil {
		return WorkspaceFact{}, err
	}
	if err := requireEmptyDirectory(templateRoot, "empty workspace Git template"); err != nil {
		return WorkspaceFact{}, err
	}

	partial, err := os.MkdirTemp(partialRoot, "."+spec.TaskID+"-")
	if err != nil {
		return WorkspaceFact{}, fmt.Errorf("create partial workspace: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(partial); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove partial workspace: %w", err))
		}
	}()
	if err := validateDirectDirectory(partial, "partial workspace"); err != nil {
		return WorkspaceFact{}, err
	}
	if _, err := m.git(ctx, "clone private workspace",
		"clone", "--no-local", "--no-hardlinks", "--no-tags", "--no-checkout",
		"--no-recurse-submodules", "--template="+templateRoot, "--", controlPath, partial,
	); err != nil {
		return WorkspaceFact{}, err
	}
	if _, err := m.git(ctx, "checkout exact workspace base",
		"-c", "submodule.recurse=false", "-C", partial, "checkout", "--detach", spec.BaseSHA,
	); err != nil {
		return WorkspaceFact{}, err
	}
	branch := taskBranch(spec.TaskID)
	if _, err := m.git(ctx, "create task workspace branch",
		"-C", partial, "switch", "-c", strings.TrimPrefix(branch, "refs/heads/"),
	); err != nil {
		return WorkspaceFact{}, err
	}
	if spec.Source != nil {
		if err := m.importSource(ctx, controlPath, partialRoot, partial, *spec.Source); err != nil {
			return WorkspaceFact{}, err
		}
	}
	if _, err := m.git(ctx, "remove control origin", "-C", partial, "remote", "remove", "origin"); err != nil {
		return WorkspaceFact{}, err
	}
	if _, err := m.git(ctx, "configure private group sharing",
		"-C", partial, "config", "--local", "core.sharedRepository", "group",
	); err != nil {
		return WorkspaceFact{}, err
	}
	if err := removeInitialWorkspaceReflogs(partial); err != nil {
		return WorkspaceFact{}, err
	}
	if err := normalizeWorkspaceAccess(partial); err != nil {
		return WorkspaceFact{}, err
	}
	if _, err := m.inspect(ctx, partial, spec, true); err != nil {
		return WorkspaceFact{}, err
	}

	markerPath, err := m.markerPath(spec.ProjectID, spec.TaskID)
	if err != nil {
		return WorkspaceFact{}, err
	}
	markerCreated, err := ensureWorkspaceMarker(markerPath, markerForSpec(spec))
	if err != nil {
		return WorkspaceFact{}, err
	}
	markerOwned := markerCreated
	defer func() {
		if resultErr == nil || !markerOwned {
			return
		}
		if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove workspace ownership marker: %w", err))
		}
		if err := syncDirectory(filepath.Dir(markerPath)); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("sync workspace marker parent after rollback: %w", err))
		}
	}()

	if exists, err := pathExists(path); err != nil {
		return WorkspaceFact{}, err
	} else if exists {
		return WorkspaceFact{}, errors.New("workspace appeared while materialization was in progress")
	}
	if err := os.Rename(partial, path); err != nil {
		return WorkspaceFact{}, fmt.Errorf("publish private workspace: %w", err)
	}
	published := true
	defer func() {
		if resultErr == nil || !published {
			return
		}
		if err := os.RemoveAll(path); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove failed published workspace: %w", err))
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("sync workspace parent after rollback: %w", err))
		}
	}()
	if err := syncDirectory(projectRoot); err != nil {
		return WorkspaceFact{}, fmt.Errorf("sync workspace parent after publication: %w", err)
	}
	if err := m.validateFinalWorkspacePath(path, spec); err != nil {
		return WorkspaceFact{}, err
	}
	fact, err = m.inspect(ctx, path, spec, true)
	if err != nil {
		return WorkspaceFact{}, err
	}
	published = false
	markerOwned = false
	return fact, nil
}

func (m *WorkspaceManager) verifyControlInputs(ctx context.Context, spec WorkspaceSpec) (string, error) {
	controlPath := filepath.Join(m.initializer.root, spec.ProjectID+".git")
	if err := m.initializer.validateFinalPath(controlPath); err != nil {
		return "", err
	}
	bare, err := m.initializer.isBare(ctx, controlPath)
	if err != nil {
		return "", err
	}
	if !bare {
		return "", errors.New("control repository is not bare")
	}
	hasBase, err := m.initializer.hasCommit(ctx, controlPath, spec.BaseSHA)
	if err != nil {
		return "", err
	}
	if !hasBase {
		return "", errors.New("control repository does not contain immutable workspace base")
	}
	if spec.Source != nil {
		actual, exists, err := m.initializer.resolveRef(ctx, controlPath, spec.Source.TaskRef)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", errors.New("saved source task ref is missing")
		}
		if actual != spec.Source.HeadSHA {
			return "", errors.New("saved source task ref does not match saved source head")
		}
	}
	return controlPath, nil
}

func (m *WorkspaceManager) importSource(ctx context.Context, controlPath, bundleRoot, workspace string, source WorkspaceSource) (resultErr error) {
	bundle, err := os.CreateTemp(bundleRoot, ".source-*.bundle")
	if err != nil {
		return fmt.Errorf("create source bundle path: %w", err)
	}
	bundlePath := bundle.Name()
	if err := bundle.Close(); err != nil {
		_ = os.Remove(bundlePath)
		return fmt.Errorf("close source bundle placeholder: %w", err)
	}
	if err := os.Remove(bundlePath); err != nil {
		return fmt.Errorf("prepare source bundle path: %w", err)
	}
	defer func() {
		if err := os.Remove(bundlePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary source bundle: %w", err))
		}
	}()
	if _, err := m.git(ctx, "create exact source bundle",
		"--git-dir="+controlPath, "bundle", "create", bundlePath, source.TaskRef,
	); err != nil {
		return err
	}
	if _, err := m.git(ctx, "verify exact source bundle", "-C", workspace, "bundle", "verify", bundlePath); err != nil {
		return err
	}
	convenience := source.ConvenienceRef()
	if _, err := m.git(ctx, "import exact source bundle",
		"-C", workspace, "fetch", "--no-tags", "--no-write-fetch-head", bundlePath,
		source.TaskRef+":"+convenience,
	); err != nil {
		return err
	}
	actual, err := m.workspaceRef(ctx, workspace, convenience)
	if err != nil {
		return err
	}
	if actual != source.HeadSHA {
		return errors.New("imported source convenience ref does not match saved source head")
	}
	return nil
}

func (m *WorkspaceManager) inspect(ctx context.Context, path string, spec WorkspaceSpec, initial bool) (WorkspaceFact, error) {
	if initial {
		if err := validateWorkspaceAccess(path); err != nil {
			return WorkspaceFact{}, err
		}
	} else if err := validateWorkspaceRootAccess(path); err != nil {
		return WorkspaceFact{}, err
	}
	gitDir := filepath.Join(path, ".git")
	if err := validateDirectDirectory(gitDir, "workspace Git directory"); err != nil {
		return WorkspaceFact{}, err
	}
	top, err := m.git(ctx, "resolve workspace top level", "-C", path, "rev-parse", "--show-toplevel")
	if err != nil {
		return WorkspaceFact{}, err
	}
	resolvedTop, err := filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil || resolvedTop != path {
		return WorkspaceFact{}, errors.New("workspace Git top level escapes deterministic workspace")
	}
	common, err := m.git(ctx, "resolve workspace common Git directory", "-C", path, "rev-parse", "--git-common-dir")
	if err != nil {
		return WorkspaceFact{}, err
	}
	commonPath := strings.TrimSpace(common)
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(path, commonPath)
	}
	resolvedCommon, err := filepath.EvalSymlinks(commonPath)
	if err != nil || resolvedCommon != gitDir {
		return WorkspaceFact{}, errors.New("workspace uses a shared or external common Git directory")
	}
	for name, forbidden := range map[string]string{
		"commondir":  filepath.Join(gitDir, "commondir"),
		"alternates": filepath.Join(gitDir, "objects", "info", "alternates"),
	} {
		if exists, err := pathExists(forbidden); err != nil {
			return WorkspaceFact{}, err
		} else if exists {
			return WorkspaceFact{}, fmt.Errorf("workspace contains forbidden %s metadata", name)
		}
	}
	remotes, err := m.git(ctx, "list workspace remotes", "-C", path, "remote")
	if err != nil {
		return WorkspaceFact{}, err
	}
	for _, remote := range strings.Fields(remotes) {
		if remote == "origin" {
			return WorkspaceFact{}, errors.New("workspace origin must remain removed")
		}
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return WorkspaceFact{}, fmt.Errorf("read workspace Git config: %w", err)
	}
	controlPath := filepath.Join(m.initializer.root, spec.ProjectID+".git")
	if bytes.Contains(config, []byte(m.initializer.root)) || bytes.Contains(config, []byte(controlPath)) {
		return WorkspaceFact{}, errors.New("workspace Git config contains a control repository path")
	}
	head, err := m.workspaceRef(ctx, path, "HEAD")
	if err != nil {
		return WorkspaceFact{}, err
	}
	if _, err := m.workspaceRef(ctx, path, spec.BaseSHA); err != nil {
		return WorkspaceFact{}, fmt.Errorf("workspace lost immutable base commit: %w", err)
	}
	branch := taskBranch(spec.TaskID)
	if initial {
		if head != spec.BaseSHA {
			return WorkspaceFact{}, errors.New("new workspace HEAD does not equal immutable base")
		}
		current, err := m.git(ctx, "resolve task workspace branch", "-C", path, "symbolic-ref", "HEAD")
		if err != nil {
			return WorkspaceFact{}, err
		}
		if strings.TrimSpace(current) != branch {
			return WorkspaceFact{}, errors.New("new workspace is not on its deterministic task branch")
		}
		if status, err := m.git(ctx, "inspect new workspace status",
			"-C", path, "status", "--porcelain=v1", "--untracked-files=all",
		); err != nil {
			return WorkspaceFact{}, err
		} else if strings.TrimSpace(status) != "" {
			return WorkspaceFact{}, errors.New("new workspace is not clean")
		}
		if spec.Source != nil {
			actual, err := m.workspaceRef(ctx, path, spec.Source.ConvenienceRef())
			if err != nil {
				return WorkspaceFact{}, err
			}
			if actual != spec.Source.HeadSHA {
				return WorkspaceFact{}, errors.New("new workspace source convenience ref is incorrect")
			}
		}
	}
	fact := WorkspaceFact{Path: path, HeadSHA: head, TaskBranch: branch}
	if spec.Source != nil {
		fact.SourceRef = spec.Source.ConvenienceRef()
	}
	return fact, nil
}

func (m *WorkspaceManager) workspaceRef(ctx context.Context, path, ref string) (string, error) {
	out, err := m.git(ctx, "resolve workspace commit",
		"-C", path, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}",
	)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if err := validateObjectID(sha); err != nil {
		return "", fmt.Errorf("workspace ref resolved invalid commit: %w", err)
	}
	typeName, err := m.git(ctx, "inspect workspace commit type", "-C", path, "cat-file", "-t", sha)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(typeName) != "commit" {
		return "", errors.New("workspace ref does not point to a commit")
	}
	return sha, nil
}

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
		wantRef := taskResultRef(spec.Source.TaskID, spec.Source.RunID)
		if spec.Source.TaskRef != wantRef {
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
	relative, err := filepath.Rel(m.root, target)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errors.New("workspace path escapes workspace root")
	}
	current := m.root
	if relative == "." {
		return nil
	}
	for _, element := range strings.Split(relative, string(os.PathSeparator)) {
		if element == "" || element == "." || element == ".." {
			return errors.New("workspace path contains invalid component")
		}
		parent := current
		current = filepath.Join(current, element)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create workspace directory: %w", err)
		}
		if err := validateDirectDirectory(current, "workspace directory"); err != nil {
			return err
		}
		if err := syncDirectory(parent); err != nil {
			return fmt.Errorf("sync workspace directory parent: %w", err)
		}
	}
	return nil
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
	cmd := exec.CommandContext(ctx, m.initializer.gitPath, args...)
	cmd.Env = gitEnvironment()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		detail := boundedText(stderr.String())
		if detail == "" {
			return "", fmt.Errorf("%s failed: %w", operation, err)
		}
		return "", fmt.Errorf("%s failed: %w: %s", operation, err, detail)
	}
	return stdout.String(), nil
}

type workspacePublicError struct {
	message string
	cause   error
}

func (e *workspacePublicError) Error() string { return e.message }
func (e *workspacePublicError) Is(target error) bool {
	return errors.Is(e.cause, target)
}

func (m *WorkspaceManager) publicError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	message := cause.Error()
	var replacements []struct{ old, new string }
	if m != nil {
		replacements = append(replacements, struct{ old, new string }{m.root, "<workspace-root>"})
		if m.initializer != nil {
			replacements = append(replacements, struct{ old, new string }{m.initializer.root, "<control-root>"})
		}
	}
	sort.SliceStable(replacements, func(a, b int) bool {
		return len(replacements[a].old) > len(replacements[b].old)
	})
	for _, replacement := range replacements {
		if replacement.old != "" {
			message = strings.ReplaceAll(message, replacement.old, replacement.new)
		}
	}
	var safeCause error
	switch {
	case errors.Is(cause, context.Canceled):
		safeCause = context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		safeCause = context.DeadlineExceeded
	}
	return &workspacePublicError{
		message: "gitrepo: workspace " + operation + ": " + message,
		cause:   safeCause,
	}
}

func taskBranch(taskID string) string {
	return "refs/heads/coordplane/task/" + taskID
}

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
