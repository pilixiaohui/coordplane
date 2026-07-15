package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CaptureSpec contains only durable task/run inputs and deterministic daemon
// paths. ExpectedHead is an assertion; Capture always reads the actual HEAD.
type CaptureSpec struct {
	Workspace       WorkspaceSpec
	RunID           string
	ExpectedHead    string
	ControlRepoPath string
}

type CaptureFact struct {
	HeadSHA string
	TaskRef string
}

// CaptureHelper is the only production boundary allowed to derive Git facts
// from an Agent-owned workspace. Implementations must isolate repository
// config, use a read-only workspace, and atomically publish trusted facts.
type CaptureHelper interface {
	Capture(context.Context, CaptureHelperRequest) (CaptureHelperFact, error)
	Inspect(context.Context, WorkspaceInspectRequest) (WorkspaceInspectFact, error)
	Cleanup(context.Context, CaptureHelperRequest) error
}

type CaptureHelperRequest struct {
	ProjectID    string
	TaskID       string
	RunID        string
	Workspace    string
	ExpectedHead string
	BaseSHA      string
	SourceSHA    string
}

type CaptureHelperFact struct {
	HeadSHA     string
	ReadyBundle string
	BundleBytes int64
	ObjectCount int
}

type WorkspaceInspectRequest struct {
	ProjectID string
	TaskID    string
	Workspace string
}

type WorkspaceInspectFact struct {
	HeadSHA      string
	StatusDigest string
	ObjectCount  int
	Clean        bool
	Unfinished   bool
}

type capturePhase string

const (
	capturePhaseIntentChecked    capturePhase = "intent_checked"
	capturePhaseHandoffReady     capturePhase = "handoff_ready"
	capturePhaseBundleVerified   capturePhase = "bundle_verified"
	capturePhaseObjectsImported  capturePhase = "objects_imported"
	capturePhaseTaskRefWritten   capturePhase = "task_ref_written"
	capturePhaseIntegrityChecked capturePhase = "integrity_checked"
)

var contractCapturePhaseHook func(context.Context, capturePhase, CaptureSpec) error
var contractTaskRefUseHook func(context.Context, string, string) error

type AdvanceOutcome string

const (
	AdvanceUpdated  AdvanceOutcome = "updated"
	AdvanceIncluded AdvanceOutcome = "included"
	AdvanceStale    AdvanceOutcome = "stale"
)

type AdvanceSpec struct {
	ProjectID       string
	ControlRepoPath string
	CanonicalRef    string
	TaskRef         string
	ExpectedOldSHA  string
	TargetSHA       string
}

type AdvanceFact struct {
	Outcome   AdvanceOutcome
	ActualSHA string
}

type CheckoutSpec struct {
	ProjectID       string
	ControlRepoPath string
	TaskRef         string
	ExpectedHead    string
	Destination     string
}

type CheckoutFact struct {
	Destination string
	HeadSHA     string
}

type DeleteTaskRefSpec struct {
	ProjectID       string
	ControlRepoPath string
	CanonicalRef    string
	TaskRef         string
	ExpectedHead    string
	AllowDiscard    bool
}

type TaskRefStateFact struct {
	Exists    bool
	ActualSHA string
	Included  bool
}

// InvariantError identifies failures where controller Git truth can no longer
// be reconciled by retrying an Agent task.
type InvariantError struct {
	message string
	cause   error
}

func (e *InvariantError) Error() string {
	if e.cause == nil {
		return "gitrepo: " + e.message
	}
	return "gitrepo: " + e.message + ": " + e.cause.Error()
}

func (e *InvariantError) Unwrap() error      { return e.cause }
func (e *InvariantError) GitInvariant() bool { return true }

// TaskRef is the only task/run ref shape owned by the controller.
func TaskRef(taskID, runID string) (string, error) {
	if err := validateID("task", taskID); err != nil {
		return "", err
	}
	if err := validateID("run", runID); err != nil {
		return "", err
	}
	return "refs/coordplane/tasks/" + taskID + "/runs/" + runID, nil
}

// Capture freezes an actual private-workspace commit behind an immutable
// controller ref. The caller must first establish that the Run is terminal.
func (m *WorkspaceManager) Capture(ctx context.Context, spec CaptureSpec) (fact CaptureFact, resultErr error) {
	if m == nil || m.initializer == nil {
		return CaptureFact{}, errors.New("gitrepo: nil workspace manager")
	}
	defer func() {
		if resultErr != nil {
			resultErr = m.publicError("capture", resultErr)
		}
	}()
	if err := m.validateSpec(ctx, spec.Workspace); err != nil {
		return CaptureFact{}, err
	}
	if err := validateID("run", spec.RunID); err != nil {
		return CaptureFact{}, err
	}
	if err := validateObjectID(spec.ExpectedHead); err != nil {
		return CaptureFact{}, fmt.Errorf("invalid expected head: %w", err)
	}
	if err := m.initializer.validateFinalPath(spec.ControlRepoPath); err != nil {
		return CaptureFact{}, err
	}
	wantControl := filepath.Join(m.initializer.root, spec.Workspace.ProjectID+".git")
	if spec.ControlRepoPath != wantControl {
		return CaptureFact{}, errors.New("control repository does not match workspace project")
	}
	taskRef, err := TaskRef(spec.Workspace.TaskID, spec.RunID)
	if err != nil {
		return CaptureFact{}, err
	}

	path, err := m.Path(spec.Workspace.ProjectID, spec.Workspace.TaskID)
	if err != nil {
		return CaptureFact{}, err
	}
	if err := m.validateFinalWorkspacePath(path, spec.Workspace); err != nil {
		return CaptureFact{}, err
	}
	if m.capture == nil {
		return CaptureFact{}, errors.New("trusted capture helper is not configured")
	}
	if err := runCapturePhaseHook(ctx, capturePhaseIntentChecked, spec); err != nil {
		return CaptureFact{}, err
	}

	unlock, err := m.initializer.maintenance.lock(ctx, spec.Workspace.ProjectID)
	if err != nil {
		return CaptureFact{}, err
	}
	actualRef, exists, err := m.initializer.resolveRef(ctx, spec.ControlRepoPath, taskRef)
	if err != nil {
		unlock()
		return CaptureFact{}, err
	}
	if exists {
		if actualRef != spec.ExpectedHead {
			unlock()
			return CaptureFact{}, &InvariantError{message: "task ref does not match expected head"}
		}
		if err := m.validateExistingCapture(ctx, spec, actualRef); err != nil {
			unlock()
			return CaptureFact{}, err
		}
		if err := m.cleanupCaptureImportRef(ctx, spec); err != nil {
			unlock()
			return CaptureFact{}, err
		}
		unlock()
		return CaptureFact{HeadSHA: actualRef, TaskRef: taskRef}, nil
	}
	unlock()

	helperRequest := CaptureHelperRequest{
		ProjectID: spec.Workspace.ProjectID, TaskID: spec.Workspace.TaskID, RunID: spec.RunID,
		Workspace: path, ExpectedHead: spec.ExpectedHead, BaseSHA: spec.Workspace.BaseSHA,
	}
	if spec.Workspace.Source != nil {
		helperRequest.SourceSHA = spec.Workspace.Source.HeadSHA
	}
	handoff, err := m.capture.Capture(ctx, helperRequest)
	if err != nil {
		return CaptureFact{}, err
	}
	if handoff.HeadSHA != spec.ExpectedHead || handoff.ObjectCount <= 0 || handoff.BundleBytes <= 0 {
		return CaptureFact{}, &InvariantError{message: "capture helper facts do not match durable intent"}
	}
	if err := runCapturePhaseHook(ctx, capturePhaseHandoffReady, spec); err != nil {
		return CaptureFact{}, err
	}
	info, err := os.Lstat(handoff.ReadyBundle)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return CaptureFact{}, errors.New("capture handoff is not a direct regular file")
	}
	if info.Size() != handoff.BundleBytes {
		return CaptureFact{}, &InvariantError{message: "capture bundle size does not match helper fact"}
	}

	unlock, err = m.initializer.maintenance.lock(ctx, spec.Workspace.ProjectID)
	if err != nil {
		return CaptureFact{}, err
	}
	defer unlock()
	actualRef, exists, err = m.initializer.resolveRef(ctx, spec.ControlRepoPath, taskRef)
	if err != nil {
		return CaptureFact{}, err
	}
	if exists {
		if actualRef != spec.ExpectedHead {
			return CaptureFact{}, &InvariantError{message: "task ref does not match expected head"}
		}
		if err := m.validateExistingCapture(ctx, spec, actualRef); err != nil {
			return CaptureFact{}, err
		}
		if err := m.cleanupCaptureImportRef(ctx, spec); err != nil {
			return CaptureFact{}, err
		}
		return CaptureFact{HeadSHA: actualRef, TaskRef: taskRef}, nil
	}
	if _, err := m.git(ctx, "verify capture bundle", "--git-dir="+spec.ControlRepoPath, "bundle", "verify", handoff.ReadyBundle); err != nil {
		return CaptureFact{}, &InvariantError{message: "capture bundle verification failed", cause: err}
	}
	if err := runCapturePhaseHook(ctx, capturePhaseBundleVerified, spec); err != nil {
		return CaptureFact{}, err
	}

	importRef := "refs/coordplane/imports/" + spec.Workspace.TaskID + "/" + spec.RunID
	if _, err := m.git(ctx, "import capture handoff",
		"-c", "protocol.file.allow=always", "--git-dir="+spec.ControlRepoPath,
		"fetch", "--force", "--no-tags", "--no-write-fetch-head", handoff.ReadyBundle, "HEAD:"+importRef,
	); err != nil {
		return CaptureFact{}, &InvariantError{message: "import capture handoff", cause: err}
	}
	defer func() {
		_, _ = m.initializer.git(context.Background(), "--git-dir="+spec.ControlRepoPath, "update-ref", "-d", importRef, spec.ExpectedHead)
	}()
	imported, exists, err := m.initializer.resolveRef(ctx, spec.ControlRepoPath, importRef)
	if err != nil || !exists || imported != spec.ExpectedHead {
		return CaptureFact{}, &InvariantError{message: "imported capture does not match actual head", cause: err}
	}
	typeName, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "cat-file", "-t", imported)
	if err != nil || strings.TrimSpace(typeName) != "commit" {
		return CaptureFact{}, &InvariantError{message: "imported capture is not a commit", cause: err}
	}
	if err := runCapturePhaseHook(ctx, capturePhaseObjectsImported, spec); err != nil {
		return CaptureFact{}, err
	}
	if err := m.validateCapturedAncestry(ctx, spec.ControlRepoPath, spec.Workspace, imported); err != nil {
		return CaptureFact{}, err
	}
	if _, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "update-ref", taskRef, imported, zeroObjectID(imported)); err != nil {
		current, exists, resolveErr := m.initializer.resolveRef(ctx, spec.ControlRepoPath, taskRef)
		if resolveErr != nil || !exists || current != imported {
			return CaptureFact{}, &InvariantError{message: "create immutable task ref", cause: err}
		}
	}
	if err := runCapturePhaseHook(ctx, capturePhaseTaskRefWritten, spec); err != nil {
		return CaptureFact{}, err
	}
	current, exists, err := m.initializer.resolveRef(ctx, spec.ControlRepoPath, taskRef)
	if err != nil || !exists || current != imported {
		return CaptureFact{}, &InvariantError{message: "task ref read-back mismatch", cause: err}
	}
	if _, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "fsck", "--connectivity-only", "--strict"); err != nil {
		return CaptureFact{}, &InvariantError{message: "control repository fsck failed", cause: err}
	}
	if err := runCapturePhaseHook(ctx, capturePhaseIntegrityChecked, spec); err != nil {
		return CaptureFact{}, err
	}
	return CaptureFact{HeadSHA: current, TaskRef: taskRef}, nil
}

// CleanupCapture removes only the per-Run handoff after the caller has
// durably finalized the captured result.
func (m *WorkspaceManager) CleanupCapture(ctx context.Context, projectID, taskID, runID string) error {
	if m == nil || m.capture == nil {
		return errors.New("gitrepo: trusted capture helper is not configured")
	}
	if err := validateID("project", projectID); err != nil {
		return err
	}
	if err := validateID("task", taskID); err != nil {
		return err
	}
	if err := validateID("run", runID); err != nil {
		return err
	}
	return m.capture.Cleanup(ctx, CaptureHelperRequest{ProjectID: projectID, TaskID: taskID, RunID: runID})
}

func runCapturePhaseHook(ctx context.Context, phase capturePhase, spec CaptureSpec) error {
	if contractCapturePhaseHook == nil {
		return nil
	}
	return contractCapturePhaseHook(ctx, phase, spec)
}

func (m *WorkspaceManager) validateExistingCapture(ctx context.Context, spec CaptureSpec, head string) error {
	typeName, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "cat-file", "-t", head)
	if err != nil || strings.TrimSpace(typeName) != "commit" {
		return &InvariantError{message: "existing task ref is not a commit", cause: err}
	}
	if err := m.validateCapturedAncestry(ctx, spec.ControlRepoPath, spec.Workspace, head); err != nil {
		return err
	}
	if _, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "fsck", "--connectivity-only", "--strict"); err != nil {
		return &InvariantError{message: "control repository fsck failed during capture recovery", cause: err}
	}
	return nil
}

func (m *WorkspaceManager) cleanupCaptureImportRef(ctx context.Context, spec CaptureSpec) error {
	importRef := "refs/coordplane/imports/" + spec.Workspace.TaskID + "/" + spec.RunID
	actual, exists, err := m.initializer.resolveRef(ctx, spec.ControlRepoPath, importRef)
	if err != nil || !exists {
		return err
	}
	if actual != spec.ExpectedHead {
		return &InvariantError{message: "capture import ref points to a different commit"}
	}
	if _, err := m.initializer.git(ctx, "--git-dir="+spec.ControlRepoPath, "update-ref", "-d", importRef, actual); err != nil {
		return &InvariantError{message: "remove recovered capture import ref", cause: err}
	}
	return nil
}

func (m *WorkspaceManager) validateCapturedAncestry(ctx context.Context, controlPath string, spec WorkspaceSpec, head string) error {
	ancestor, err := m.initializer.isAncestor(ctx, controlPath, true, spec.BaseSHA, head)
	if err != nil {
		return &InvariantError{message: "validate captured base ancestry", cause: err}
	}
	if !ancestor {
		return errors.New("captured head does not contain immutable base")
	}
	if spec.Source != nil {
		ancestor, err = m.initializer.isAncestor(ctx, controlPath, true, spec.Source.HeadSHA, head)
		if err != nil {
			return &InvariantError{message: "validate captured source ancestry", cause: err}
		}
		if !ancestor {
			return errors.New("captured integration head does not contain source head")
		}
	}
	return nil
}

// Advance updates canonical only when the actual ref is an ancestor of the
// captured task head. Every write carries the actual old object ID.
func (i *Initializer) Advance(ctx context.Context, spec AdvanceSpec) (AdvanceFact, error) {
	if i == nil {
		return AdvanceFact{}, errors.New("gitrepo: nil initializer")
	}
	if err := validateID("project", spec.ProjectID); err != nil {
		return AdvanceFact{}, err
	}
	if err := i.validateFinalPath(spec.ControlRepoPath); err != nil {
		return AdvanceFact{}, err
	}
	if spec.ControlRepoPath != filepath.Join(i.root, spec.ProjectID+".git") {
		return AdvanceFact{}, errors.New("gitrepo: control repository does not match project")
	}
	if err := i.validateBranchRef(ctx, spec.CanonicalRef); err != nil {
		return AdvanceFact{}, err
	}
	for name, sha := range map[string]string{"expected old SHA": spec.ExpectedOldSHA, "target SHA": spec.TargetSHA} {
		if err := validateObjectID(sha); err != nil {
			return AdvanceFact{}, fmt.Errorf("gitrepo: invalid %s: %w", name, err)
		}
	}
	if _, err := i.git(ctx, "check-ref-format", spec.TaskRef); err != nil || !strings.HasPrefix(spec.TaskRef, "refs/coordplane/tasks/") {
		return AdvanceFact{}, errors.New("gitrepo: invalid task ref")
	}

	unlock, err := i.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return AdvanceFact{}, err
	}
	defer unlock()

	target, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.TaskRef)
	if err != nil {
		return AdvanceFact{}, err
	}
	if !exists || target != spec.TargetSHA {
		return AdvanceFact{}, &InvariantError{message: "task ref does not match advance target"}
	}
	for attempt := 0; attempt < 3; attempt++ {
		actual, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.CanonicalRef)
		if err != nil {
			return AdvanceFact{}, err
		}
		if !exists {
			return AdvanceFact{}, &InvariantError{message: "canonical ref is missing"}
		}
		if actual == target {
			return AdvanceFact{Outcome: AdvanceIncluded, ActualSHA: actual}, nil
		}
		included, err := i.isAncestor(ctx, spec.ControlRepoPath, true, target, actual)
		if err != nil {
			return AdvanceFact{}, &InvariantError{message: "inspect target ancestry", cause: err}
		}
		if included {
			return AdvanceFact{Outcome: AdvanceIncluded, ActualSHA: actual}, nil
		}
		fastForward, err := i.isAncestor(ctx, spec.ControlRepoPath, true, actual, target)
		if err != nil {
			return AdvanceFact{}, &InvariantError{message: "inspect canonical ancestry", cause: err}
		}
		if !fastForward {
			return AdvanceFact{Outcome: AdvanceStale, ActualSHA: actual}, nil
		}
		if _, err := i.git(ctx, "--git-dir="+spec.ControlRepoPath, "update-ref", spec.CanonicalRef, target, actual); err != nil {
			if ctx.Err() != nil {
				return AdvanceFact{}, ctx.Err()
			}
			continue
		}
		readBack, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.CanonicalRef)
		if err != nil || !exists {
			return AdvanceFact{}, &InvariantError{message: "canonical read-back failed", cause: err}
		}
		if readBack == target {
			return AdvanceFact{Outcome: AdvanceUpdated, ActualSHA: readBack}, nil
		}
	}
	actual, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.CanonicalRef)
	if err != nil || !exists {
		return AdvanceFact{}, &InvariantError{message: "canonical ref disappeared after CAS", cause: err}
	}
	if actual == target {
		return AdvanceFact{Outcome: AdvanceIncluded, ActualSHA: actual}, nil
	}
	included, ancestryErr := i.isAncestor(ctx, spec.ControlRepoPath, true, target, actual)
	if ancestryErr != nil {
		return AdvanceFact{}, &InvariantError{message: "classify canonical after CAS", cause: ancestryErr}
	}
	if included {
		return AdvanceFact{Outcome: AdvanceIncluded, ActualSHA: actual}, nil
	}
	return AdvanceFact{Outcome: AdvanceStale, ActualSHA: actual}, nil
}

// ResolveTaskRef reads an exact controller-owned task ref while holding the
// same maintenance lock used by capture, checkout, and ref deletion.
func (i *Initializer) ResolveTaskRef(ctx context.Context, projectID, controlPath, ref, expected string) (string, error) {
	var actual string
	err := i.UseTaskRef(ctx, projectID, controlPath, ref, expected, func(value string) error {
		actual = value
		return nil
	})
	return actual, err
}

// UseTaskRef keeps the project maintenance lock through the caller's commit
// callback. Source-task creation and checkout use this to close the ref-GC
// race without holding a SQLite transaction while waiting for the lock.
func (i *Initializer) UseTaskRef(ctx context.Context, projectID, controlPath, ref, expected string, use func(string) error) error {
	if err := validateID("project", projectID); err != nil {
		return err
	}
	if err := i.validateFinalPath(controlPath); err != nil {
		return err
	}
	if controlPath != filepath.Join(i.root, projectID+".git") {
		return errors.New("gitrepo: control repository does not match project")
	}
	if !strings.HasPrefix(ref, "refs/coordplane/tasks/") {
		return errors.New("gitrepo: invalid task ref")
	}
	if _, err := i.git(ctx, "check-ref-format", ref); err != nil {
		return errors.New("gitrepo: invalid task ref")
	}
	if err := validateObjectID(expected); err != nil {
		return fmt.Errorf("gitrepo: invalid expected task head: %w", err)
	}
	if use == nil {
		return errors.New("gitrepo: task ref callback is required")
	}
	unlock, err := i.maintenance.lock(ctx, projectID)
	if err != nil {
		return err
	}
	defer unlock()
	actual, exists, err := i.resolveRef(ctx, controlPath, ref)
	if err != nil {
		return err
	}
	if !exists || actual != expected {
		return &InvariantError{message: "task ref does not match saved task head"}
	}
	if contractTaskRefUseHook != nil {
		if err := contractTaskRefUseHook(ctx, projectID, ref); err != nil {
			return err
		}
	}
	return use(actual)
}

// Checkout exports an exact task ref into a standalone worktree without any
// remote or object sharing with the controller repository.
func (i *Initializer) Checkout(ctx context.Context, spec CheckoutSpec) (fact CheckoutFact, resultErr error) {
	if strings.TrimSpace(spec.Destination) == "" || !filepath.IsAbs(spec.Destination) || filepath.Clean(spec.Destination) != spec.Destination {
		return CheckoutFact{}, errors.New("gitrepo: checkout destination must be canonical and absolute")
	}
	parent, err := canonicalExistingDirectory(filepath.Dir(spec.Destination))
	if err != nil || parent != filepath.Dir(spec.Destination) {
		return CheckoutFact{}, errors.New("gitrepo: checkout destination parent must be a direct existing directory")
	}
	if exists, err := pathExists(spec.Destination); err != nil {
		return CheckoutFact{}, err
	} else if exists {
		return CheckoutFact{}, errors.New("gitrepo: checkout destination already exists")
	}

	err = i.UseTaskRef(ctx, spec.ProjectID, spec.ControlRepoPath, spec.TaskRef, spec.ExpectedHead, func(actual string) (callbackErr error) {
		handoffRoot := filepath.Join(i.root, ".handoff")
		if err := i.ensureDirectSubdirectories(handoffRoot); err != nil {
			return err
		}
		bundle, err := os.CreateTemp(handoffRoot, ".checkout-*.bundle")
		if err != nil {
			return err
		}
		bundlePath := bundle.Name()
		if err := bundle.Close(); err != nil {
			_ = os.Remove(bundlePath)
			return err
		}
		if err := os.Remove(bundlePath); err != nil {
			return err
		}
		defer os.Remove(bundlePath)
		if _, err := i.git(ctx, "--git-dir="+spec.ControlRepoPath, "bundle", "create", bundlePath, spec.TaskRef); err != nil {
			return fmt.Errorf("gitrepo: create checkout bundle: %w", err)
		}
		if err := os.Mkdir(spec.Destination, 0o700); err != nil {
			return fmt.Errorf("gitrepo: create checkout destination: %w", err)
		}
		published := false
		defer func() {
			if !published {
				callbackErr = errors.Join(callbackErr, os.RemoveAll(spec.Destination))
			}
		}()
		templateRoot := filepath.Join(i.root, ".empty-checkout-template")
		if err := i.ensureDirectSubdirectories(templateRoot); err != nil {
			return err
		}
		if err := requireEmptyDirectory(templateRoot, "empty checkout template"); err != nil {
			return err
		}
		if _, err := i.git(ctx, "init", "--initial-branch=coordplane-task", "--template="+templateRoot, spec.Destination); err != nil {
			return fmt.Errorf("gitrepo: initialize checkout: %w", err)
		}
		if _, err := i.git(ctx, "-C", spec.Destination, "fetch", "--no-tags", "--no-write-fetch-head", bundlePath, spec.TaskRef+":refs/coordplane/import"); err != nil {
			return fmt.Errorf("gitrepo: import checkout bundle: %w", err)
		}
		if _, err := i.git(ctx, "-C", spec.Destination, "checkout", "--force", "-b", "coordplane-task", actual); err != nil {
			return fmt.Errorf("gitrepo: checkout task head: %w", err)
		}
		if _, err := i.git(ctx, "-C", spec.Destination, "update-ref", "-d", "refs/coordplane/import", actual); err != nil {
			return fmt.Errorf("gitrepo: remove checkout import ref: %w", err)
		}
		head, err := i.git(ctx, "-C", spec.Destination, "rev-parse", "HEAD^{commit}")
		if err != nil || strings.TrimSpace(head) != actual {
			return &InvariantError{message: "exported checkout head mismatch", cause: err}
		}
		status, err := i.git(ctx, "-C", spec.Destination, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil || strings.TrimSpace(status) != "" {
			return errors.New("gitrepo: exported checkout is not clean")
		}
		remotes, err := i.git(ctx, "-C", spec.Destination, "remote")
		if err != nil || strings.TrimSpace(remotes) != "" {
			return errors.New("gitrepo: exported checkout contains a remote")
		}
		published = true
		fact = CheckoutFact{Destination: spec.Destination, HeadSHA: actual}
		return nil
	})
	return fact, err
}

// DeleteTaskRefAndPrune owns the complete final DB-check, expected-old delete,
// reachability check, and prune window under one project maintenance lock.
func (i *Initializer) DeleteTaskRefAndPrune(ctx context.Context, spec DeleteTaskRefSpec, authorize func() (bool, error)) (bool, error) {
	if authorize == nil {
		return false, errors.New("gitrepo: task ref GC authorization is required")
	}
	if err := validateID("project", spec.ProjectID); err != nil {
		return false, err
	}
	if err := i.validateFinalPath(spec.ControlRepoPath); err != nil {
		return false, err
	}
	if spec.ControlRepoPath != filepath.Join(i.root, spec.ProjectID+".git") {
		return false, errors.New("gitrepo: control repository does not match project")
	}
	if err := i.validateBranchRef(ctx, spec.CanonicalRef); err != nil {
		return false, err
	}
	if err := validateObjectID(spec.ExpectedHead); err != nil {
		return false, err
	}
	if !strings.HasPrefix(spec.TaskRef, "refs/coordplane/tasks/") {
		return false, errors.New("gitrepo: invalid task ref")
	}
	if _, err := i.git(ctx, "check-ref-format", spec.TaskRef); err != nil {
		return false, errors.New("gitrepo: invalid task ref")
	}
	unlock, err := i.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return false, err
	}
	defer unlock()
	allowed, err := authorize()
	if err != nil || !allowed {
		return false, err
	}
	actual, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.TaskRef)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, i.pruneLocked(ctx, spec.ControlRepoPath)
	}
	if actual != spec.ExpectedHead {
		return false, &InvariantError{message: "task ref changed before GC"}
	}
	canonical, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.CanonicalRef)
	if err != nil || !exists {
		return false, &InvariantError{message: "canonical ref missing during GC", cause: err}
	}
	included, err := i.isAncestor(ctx, spec.ControlRepoPath, true, actual, canonical)
	if err != nil {
		return false, &InvariantError{message: "verify task ref reachability during GC", cause: err}
	}
	if !included && !spec.AllowDiscard {
		return false, nil
	}
	if _, err := i.git(ctx, "--git-dir="+spec.ControlRepoPath, "update-ref", "-d", spec.TaskRef, actual); err != nil {
		current, stillExists, resolveErr := i.resolveRef(ctx, spec.ControlRepoPath, spec.TaskRef)
		if resolveErr != nil || (stillExists && current == actual) {
			return false, &InvariantError{message: "delete task ref with expected old SHA", cause: err}
		}
	}
	_, exists, err = i.resolveRef(ctx, spec.ControlRepoPath, spec.TaskRef)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	return true, i.pruneLocked(ctx, spec.ControlRepoPath)
}

func (i *Initializer) TaskRefState(ctx context.Context, spec DeleteTaskRefSpec) (TaskRefStateFact, error) {
	if err := validateID("project", spec.ProjectID); err != nil {
		return TaskRefStateFact{}, err
	}
	if err := i.validateFinalPath(spec.ControlRepoPath); err != nil {
		return TaskRefStateFact{}, err
	}
	if spec.ControlRepoPath != filepath.Join(i.root, spec.ProjectID+".git") {
		return TaskRefStateFact{}, errors.New("gitrepo: control repository does not match project")
	}
	if err := i.validateBranchRef(ctx, spec.CanonicalRef); err != nil {
		return TaskRefStateFact{}, err
	}
	unlock, err := i.maintenance.lock(ctx, spec.ProjectID)
	if err != nil {
		return TaskRefStateFact{}, err
	}
	defer unlock()
	actual, exists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.TaskRef)
	if err != nil || !exists {
		return TaskRefStateFact{Exists: exists}, err
	}
	canonical, canonicalExists, err := i.resolveRef(ctx, spec.ControlRepoPath, spec.CanonicalRef)
	if err != nil || !canonicalExists {
		return TaskRefStateFact{}, &InvariantError{message: "canonical ref missing during GC preview", cause: err}
	}
	included, err := i.isAncestor(ctx, spec.ControlRepoPath, true, actual, canonical)
	if err != nil {
		return TaskRefStateFact{}, err
	}
	return TaskRefStateFact{Exists: true, ActualSHA: actual, Included: included}, nil
}

func (i *Initializer) pruneLocked(ctx context.Context, controlPath string) error {
	output, err := i.git(ctx, "--git-dir="+controlPath, "count-objects", "-v")
	if err != nil {
		return err
	}
	needsGC := false
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok || (key != "count" && key != "garbage") {
			continue
		}
		count, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return &InvariantError{message: "invalid git count-objects output", cause: parseErr}
		}
		needsGC = needsGC || count > 0
	}
	if needsGC {
		if _, err := i.git(ctx, "--git-dir="+controlPath, "gc", "--prune=now"); err != nil {
			return &InvariantError{message: "git gc failed", cause: err}
		}
	}
	if _, err := i.git(ctx, "--git-dir="+controlPath, "fsck", "--full", "--strict"); err != nil {
		return &InvariantError{message: "post-GC fsck failed", cause: err}
	}
	return nil
}

func (i *Initializer) isAncestor(ctx context.Context, repoPath string, bare bool, ancestor, descendant string) (bool, error) {
	for _, sha := range []string{ancestor, descendant} {
		if err := validateObjectID(sha); err != nil {
			return false, err
		}
	}
	args := []string{"-C", repoPath, "merge-base", "--is-ancestor", ancestor, descendant}
	if bare {
		args = []string{"--git-dir=" + repoPath, "merge-base", "--is-ancestor", ancestor, descendant}
	}
	_, err := i.git(ctx, args...)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var commandErr *gitCommandError
	var exitErr *exec.ExitError
	if errors.As(err, &commandErr) && errors.As(commandErr.err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
