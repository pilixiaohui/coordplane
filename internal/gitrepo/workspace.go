package gitrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coordplane/internal/perfobs"
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

type WorkspaceStateFact struct {
	Exists      bool
	Fingerprint string
	HeadSHA     string
	Clean       bool
}

// WorkspaceManager owns private clone materialization and verification. The
// caller owns lifecycle authorization and decides whether Materialize or
// Verify is legal for the current durable Run history.
type WorkspaceManager struct {
	initializer *Initializer
	root        string
	capture     CaptureHelper
}

// NewWorkspaceManager binds private workspaces to one daemon-owned repository
// initializer. The roots must be disjoint so a workspace mount can never
// contain a control repository by configuration.
func NewWorkspaceManager(initializer *Initializer, workspaceRoot string, helpers ...CaptureHelper) (*WorkspaceManager, error) {
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
	if len(helpers) > 1 || (len(helpers) == 1 && helpers[0] == nil) {
		return nil, errors.New("gitrepo: exactly one non-nil capture helper may be configured")
	}
	manager := &WorkspaceManager{initializer: initializer, root: root}
	if len(helpers) == 1 {
		manager.capture = helpers[0]
	}
	return manager, nil
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
	fields := perfobs.Fields{ProjectID: spec.ProjectID, TaskID: spec.TaskID}
	perfobs.StartStage("git.clone.lock_wait", spec.TaskID, fields)
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		perfobs.EndStage("git.clone.lock_wait", spec.TaskID, "error")
		return WorkspaceFact{}, m.publicError("materialize", err)
	}
	perfobs.EndStage("git.clone.lock_wait", spec.TaskID, "success")
	defer unlock()
	perfobs.StartStage("git.clone.prepare", spec.TaskID, fields)
	fact, err := m.materializeLocked(ctx, spec)
	if err != nil {
		perfobs.EndStage("git.clone.prepare", spec.TaskID, "error")
		return WorkspaceFact{}, m.publicError("materialize", err)
	}
	perfobs.EndStage("git.clone.prepare", spec.TaskID, "success")
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

// RefreshCanonical imports the current controller canonical commit into a
// movable workspace-only convenience ref before an integration Run starts.
func (m *WorkspaceManager) RefreshCanonical(ctx context.Context, spec WorkspaceSpec, controlPath, canonicalRef, expectedHead string) (string, error) {
	if spec.Source == nil {
		return "", m.publicError("refresh canonical", errors.New("canonical refresh is only valid for a source-backed workspace"))
	}
	if _, err := m.Verify(ctx, spec); err != nil {
		return "", err
	}
	if err := m.initializer.validateFinalPath(controlPath); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	if controlPath != filepath.Join(m.initializer.root, spec.ProjectID+".git") {
		return "", m.publicError("refresh canonical", errors.New("control repository does not match workspace project"))
	}
	if err := m.initializer.validateBranchRef(ctx, canonicalRef); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	if err := validateObjectID(expectedHead); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	defer unlock()
	actual, exists, err := m.initializer.resolveRef(ctx, controlPath, canonicalRef)
	if err != nil || !exists || actual != expectedHead {
		return "", m.publicError("refresh canonical", &InvariantError{message: "actual canonical does not match saved refresh head", cause: err})
	}
	handoffRoot := filepath.Join(m.initializer.root, ".handoff")
	if err := m.initializer.ensureDirectSubdirectories(handoffRoot); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	bundle, err := os.CreateTemp(handoffRoot, ".canonical-*.bundle")
	if err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	bundlePath := bundle.Name()
	if err := bundle.Close(); err != nil {
		_ = os.Remove(bundlePath)
		return "", m.publicError("refresh canonical", err)
	}
	if err := os.Remove(bundlePath); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	defer os.Remove(bundlePath)
	if _, err := m.initializer.git(ctx, "--git-dir="+controlPath, "bundle", "create", bundlePath, canonicalRef); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	const convenience = "refs/heads/coordplane/canonical"
	if _, err := m.git(ctx, "import actual canonical", "-C", path, "fetch", "--force", "--no-tags", "--no-write-fetch-head", bundlePath, canonicalRef+":"+convenience); err != nil {
		return "", m.publicError("refresh canonical", err)
	}
	imported, err := m.workspaceRef(ctx, path, convenience)
	if err != nil || imported != actual {
		return "", m.publicError("refresh canonical", &InvariantError{message: "canonical convenience ref mismatch", cause: err})
	}
	return convenience, nil
}

// Delete safely removes a terminal task workspace after the caller repeats
// its durable GC predicate under the project maintenance lock.
func (m *WorkspaceManager) Delete(ctx context.Context, spec WorkspaceSpec, expectedHead string, authorize func() (bool, error)) (bool, error) {
	if m == nil || m.initializer == nil {
		return false, errors.New("gitrepo: nil workspace manager")
	}
	if err := validateID("project", spec.ProjectID); err != nil {
		return false, err
	}
	if err := validateID("task", spec.TaskID); err != nil {
		return false, err
	}
	if err := validateObjectID(spec.BaseSHA); err != nil {
		return false, err
	}
	if err := validateObjectID(expectedHead); err != nil {
		return false, err
	}
	if authorize == nil {
		return false, errors.New("gitrepo: workspace GC authorization is required")
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return false, m.publicError("delete", err)
	}
	defer unlock()
	allowed, err := authorize()
	if err != nil || !allowed {
		return false, err
	}
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return false, err
	}
	exists, err := pathExists(path)
	if err != nil {
		return false, m.publicError("delete", err)
	}
	markerPath, err := m.markerPath(spec.ProjectID, spec.TaskID)
	if err != nil {
		return false, err
	}
	if !exists {
		if markerExists, markerErr := pathExists(markerPath); markerErr != nil {
			return false, m.publicError("delete", markerErr)
		} else if markerExists {
			marker, markerErr := readWorkspaceMarker(markerPath)
			if markerErr != nil || marker != markerForSpec(spec) {
				return false, m.publicError("delete", errors.New("orphan workspace marker does not match task identity"))
			}
			if err := os.Remove(markerPath); err != nil {
				return false, m.publicError("delete", err)
			}
		}
		return true, nil
	}
	if err := m.validateFinalWorkspacePath(path, spec); err != nil {
		return false, m.publicError("delete", err)
	}
	fact, err := m.inspectWorkspaceState(ctx, path, spec)
	if err != nil {
		return false, m.publicError("delete", err)
	}
	if fact.HeadSHA != expectedHead || !fact.Clean || fact.Unfinished {
		return false, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return false, m.publicError("delete", err)
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, m.publicError("delete", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return false, m.publicError("delete", err)
	}
	return true, nil
}

func (m *WorkspaceManager) State(ctx context.Context, spec WorkspaceSpec, expectedHead string, taskVersion int64) (WorkspaceStateFact, error) {
	if taskVersion < 1 {
		return WorkspaceStateFact{}, errors.New("gitrepo: workspace state requires a positive task version")
	}
	if err := m.validateSpec(ctx, spec); err != nil {
		return WorkspaceStateFact{}, m.publicError("state", err)
	}
	if err := validateObjectID(expectedHead); err != nil {
		return WorkspaceStateFact{}, m.publicError("state", err)
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return WorkspaceStateFact{}, err
	}
	defer unlock()
	return m.stateLocked(ctx, spec, expectedHead, taskVersion)
}

func (m *WorkspaceManager) Discard(
	ctx context.Context,
	spec WorkspaceSpec,
	expectedHead string,
	taskVersion int64,
	expectedFingerprint string,
	authorize func() (bool, error),
) (bool, error) {
	if authorize == nil {
		return false, errors.New("gitrepo: workspace discard authorization is required")
	}
	if strings.TrimSpace(expectedFingerprint) == "" {
		return false, errors.New("gitrepo: expected workspace fingerprint is required")
	}
	if err := m.validateSpec(ctx, spec); err != nil {
		return false, m.publicError("discard", err)
	}
	unlock, err := m.initializer.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return false, err
	}
	defer unlock()
	allowed, err := authorize()
	if err != nil || !allowed {
		return false, err
	}
	state, err := m.stateLocked(ctx, spec, expectedHead, taskVersion)
	if err != nil {
		return false, err
	}
	if !state.Exists {
		return true, nil
	}
	if state.Fingerprint != expectedFingerprint {
		return false, errors.New("gitrepo: workspace fingerprint changed before discard")
	}
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return false, err
	}
	markerPath, err := m.markerPath(spec.ProjectID, spec.TaskID)
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(path); err != nil {
		return false, m.publicError("discard", err)
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, m.publicError("discard", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return false, m.publicError("discard", err)
	}
	return true, nil
}

func (m *WorkspaceManager) stateLocked(ctx context.Context, spec WorkspaceSpec, expectedHead string, taskVersion int64) (WorkspaceStateFact, error) {
	path, err := m.Path(spec.ProjectID, spec.TaskID)
	if err != nil {
		return WorkspaceStateFact{}, err
	}
	exists, err := pathExists(path)
	if err != nil {
		return WorkspaceStateFact{}, err
	}
	if !exists {
		digest := sha256.Sum256([]byte(spec.TaskID + "\x00" + fmt.Sprint(taskVersion) + "\x00absent"))
		return WorkspaceStateFact{Fingerprint: hex.EncodeToString(digest[:]), Clean: true}, nil
	}
	if err := m.validateFinalWorkspacePath(path, spec); err != nil {
		return WorkspaceStateFact{}, err
	}
	fact, err := m.inspectWorkspaceState(ctx, path, spec)
	if err != nil {
		return WorkspaceStateFact{}, err
	}
	identity := strings.Join([]string{
		spec.TaskID, fmt.Sprint(taskVersion), expectedHead, fact.HeadSHA, fact.StatusDigest,
		fmt.Sprint(fact.Unfinished),
	}, "\x00")
	fingerprint := sha256.Sum256([]byte(identity))
	return WorkspaceStateFact{
		Exists: true, Fingerprint: hex.EncodeToString(fingerprint[:]),
		HeadSHA: fact.HeadSHA, Clean: fact.Clean && !fact.Unfinished,
	}, nil
}

func (m *WorkspaceManager) inspectWorkspaceState(
	ctx context.Context,
	path string,
	spec WorkspaceSpec,
) (WorkspaceInspectFact, error) {
	if m.capture == nil {
		return WorkspaceInspectFact{}, errors.New("trusted workspace inspection helper is not configured")
	}
	fact, err := m.capture.Inspect(ctx, WorkspaceInspectRequest{
		ProjectID: spec.ProjectID, TaskID: spec.TaskID, Workspace: path,
	})
	if err != nil {
		return WorkspaceInspectFact{}, err
	}
	if err := validateObjectID(fact.HeadSHA); err != nil {
		return WorkspaceInspectFact{}, fmt.Errorf("trusted workspace inspection returned invalid HEAD: %w", err)
	}
	statusDigest, err := hex.DecodeString(fact.StatusDigest)
	if err != nil || len(statusDigest) != sha256.Size || fact.ObjectCount < 1 {
		return WorkspaceInspectFact{}, errors.New("trusted workspace inspection returned invalid facts")
	}
	empty := sha256.Sum256(nil)
	if fact.Clean != (fact.StatusDigest == hex.EncodeToString(empty[:])) {
		return WorkspaceInspectFact{}, errors.New("trusted workspace inspection returned inconsistent clean state")
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
