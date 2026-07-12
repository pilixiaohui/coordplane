package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// New creates an initializer rooted at reposRoot. The root is canonicalized so
// every later project path can be checked by construction.
func New(reposRoot string) (*Initializer, error) {
	if strings.TrimSpace(reposRoot) == "" {
		return nil, errors.New("gitrepo: repository root is required")
	}
	abs, err := filepath.Abs(reposRoot)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: resolve repository root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("gitrepo: create repository root: %w", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: canonicalize repository root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: stat repository root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("gitrepo: repository root is not a directory")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("gitrepo: find git executable: %w", err)
	}
	return &Initializer{root: root, gitPath: gitPath}, nil
}

// Preflight resolves a full local branch ref to one immutable commit without
// modifying the source repository.
func (i *Initializer) Preflight(ctx context.Context, sourcePath, sourceRef string) (SourceFact, error) {
	if i == nil {
		return SourceFact{}, errors.New("gitrepo: nil initializer")
	}
	if err := i.validateBranchRef(ctx, sourceRef); err != nil {
		return SourceFact{}, err
	}
	source, err := canonicalExistingDirectory(sourcePath)
	if err != nil {
		return SourceFact{}, fmt.Errorf("gitrepo: invalid source repository: %w", err)
	}
	if _, err := i.git(ctx, "-C", source, "rev-parse", "--git-dir"); err != nil {
		return SourceFact{}, fmt.Errorf("gitrepo: source is not a Git repository: %w", err)
	}
	sha, err := i.git(ctx, "-C", source, "rev-parse", "--verify", "--end-of-options", sourceRef+"^{commit}")
	if err != nil {
		return SourceFact{}, fmt.Errorf("gitrepo: resolve source branch %s: %w", sourceRef, err)
	}
	sha = strings.TrimSpace(sha)
	if err := validateObjectID(sha); err != nil {
		return SourceFact{}, fmt.Errorf("gitrepo: source branch returned invalid commit: %w", err)
	}
	return SourceFact{SourcePath: source, SourceRef: sourceRef, InitialSHA: sha}, nil
}

// Paths returns the deterministic partial and final paths for one pending
// initialization operation.
func (i *Initializer) Paths(projectID, operationID string) (Paths, error) {
	if i == nil {
		return Paths{}, errors.New("gitrepo: nil initializer")
	}
	if err := validateID("project", projectID); err != nil {
		return Paths{}, err
	}
	if err := validateID("operation", operationID); err != nil {
		return Paths{}, err
	}
	return Paths{
		Partial: filepath.Join(i.root, ".partial", projectID, operationID+".git"),
		Final:   filepath.Join(i.root, projectID+".git"),
	}, nil
}

// Initialize materializes the saved InitialSHA in a daemon-owned bare repo and
// creates CanonicalRef with expected-absent semantics. It is safe to call again
// after any completed phase and never re-resolves SourceRef.
func (i *Initializer) Initialize(ctx context.Context, project Project) (Fact, error) {
	paths, err := i.validateProject(ctx, project, true)
	if err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhaseIntentCommitted, project, paths); err != nil {
		return Fact{}, err
	}

	finalExists, err := pathExists(paths.Final)
	if err != nil {
		return Fact{}, err
	}
	if finalExists {
		if err := i.validateFinalPath(paths.Final); err != nil {
			return Fact{}, err
		}
		return i.verifyInitialized(ctx, project, paths.Final)
	}

	if err := i.preparePartial(project, paths); err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhasePartialPrepared, project, paths); err != nil {
		return Fact{}, err
	}

	bare, bareErr := i.isBare(ctx, paths.Partial)
	if bareErr != nil && ctx.Err() != nil {
		return Fact{}, ctx.Err()
	}
	if !bare {
		templateDir := filepath.Join(i.root, ".empty-git-template")
		if err := i.ensureDirectSubdirectories(templateDir); err != nil {
			return Fact{}, fmt.Errorf("gitrepo: create empty Git template: %w", err)
		}
		if err := requireEmptyDirectory(templateDir, "empty Git template"); err != nil {
			return Fact{}, err
		}
		if _, err := i.git(ctx, "init", "--bare", "--template="+templateDir, paths.Partial); err != nil {
			return Fact{}, fmt.Errorf("gitrepo: initialize partial bare repository: %w", err)
		}
		bare, err = i.isBare(ctx, paths.Partial)
		if err != nil || !bare {
			return Fact{}, errors.New("gitrepo: initialized repository is not bare")
		}
	}
	if err := i.afterPhase(ctx, PhaseBareInitialized, project, paths); err != nil {
		return Fact{}, err
	}

	if err := i.ensureInitialObject(ctx, project, paths.Partial); err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhaseObjectsImported, project, paths); err != nil {
		return Fact{}, err
	}

	if err := i.ensureCanonical(ctx, project, paths.Partial); err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhaseCanonicalWritten, project, paths); err != nil {
		return Fact{}, err
	}

	if err := i.validatePartialPath(paths.Partial); err != nil {
		return Fact{}, err
	}
	if _, err := i.verifyRepository(ctx, project, paths.Partial); err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhaseIntegrityVerified, project, paths); err != nil {
		return Fact{}, err
	}

	if err := i.validatePartialPath(paths.Partial); err != nil {
		return Fact{}, err
	}
	if finalExists, err := pathExists(paths.Final); err != nil {
		return Fact{}, err
	} else if finalExists {
		return Fact{}, errors.New("gitrepo: refusing to replace an existing control repository path")
	}
	if err := os.Rename(paths.Partial, paths.Final); err != nil {
		return Fact{}, fmt.Errorf("gitrepo: promote partial repository: %w", err)
	}
	if err := syncDirectory(i.root); err != nil {
		return Fact{}, fmt.Errorf("gitrepo: sync repository root after promotion: %w", err)
	}
	if err := i.validateFinalPath(paths.Final); err != nil {
		return Fact{}, err
	}
	if err := i.afterPhase(ctx, PhasePromoted, project, paths); err != nil {
		return Fact{}, err
	}
	return i.verifyInitialized(ctx, project, paths.Final)
}

// Verify checks an existing daemon-owned repository and returns its actual
// canonical SHA. It deliberately does not compare that SHA with InitialSHA or
// write any ref, so repair cannot reset an advanced canonical ref.
func (i *Initializer) Verify(ctx context.Context, project Project) (Fact, error) {
	paths, err := i.validateProject(ctx, project, false)
	if err != nil {
		return Fact{}, err
	}
	if err := i.validateFinalPath(paths.Final); err != nil {
		return Fact{}, err
	}
	return i.verifyRepository(ctx, project, paths.Final)
}

func (i *Initializer) validateProject(ctx context.Context, project Project, requireOperation bool) (Paths, error) {
	if i == nil {
		return Paths{}, errors.New("gitrepo: nil initializer")
	}
	if err := i.validateRoot(); err != nil {
		return Paths{}, err
	}
	if err := validateID("project", project.ID); err != nil {
		return Paths{}, err
	}
	if requireOperation {
		if err := validateID("operation", project.OperationID); err != nil {
			return Paths{}, err
		}
	} else if project.OperationID != "" {
		if err := validateID("operation", project.OperationID); err != nil {
			return Paths{}, err
		}
	}
	if err := i.validateBranchRef(ctx, project.SourceRef); err != nil {
		return Paths{}, fmt.Errorf("gitrepo: invalid saved source ref: %w", err)
	}
	if err := i.validateBranchRef(ctx, project.CanonicalRef); err != nil {
		return Paths{}, fmt.Errorf("gitrepo: invalid canonical ref: %w", err)
	}
	if err := validateObjectID(project.InitialSHA); err != nil {
		return Paths{}, fmt.Errorf("gitrepo: invalid initial SHA: %w", err)
	}
	if !filepath.IsAbs(project.SourcePath) || filepath.Clean(project.SourcePath) != project.SourcePath {
		return Paths{}, errors.New("gitrepo: saved source path must be canonical and absolute")
	}
	operationID := project.OperationID
	if operationID == "" {
		operationID = "verify"
	}
	paths, err := i.Paths(project.ID, operationID)
	if err != nil {
		return Paths{}, err
	}
	if project.ControlRepoPath != paths.Final {
		return Paths{}, errors.New("gitrepo: saved control repository path does not match deterministic project path")
	}
	return paths, nil
}

func (i *Initializer) validateBranchRef(ctx context.Context, ref string) error {
	if ref == "" || ref != strings.TrimSpace(ref) || !strings.HasPrefix(ref, "refs/heads/") || ref == "refs/heads/" {
		return errors.New("gitrepo: branch ref must be a full refs/heads/* ref")
	}
	if _, err := i.git(ctx, "check-ref-format", ref); err != nil {
		return fmt.Errorf("gitrepo: invalid branch ref: %w", err)
	}
	return nil
}

func (i *Initializer) preparePartial(project Project, paths Paths) error {
	if err := i.ensureDirectSubdirectories(filepath.Dir(paths.Partial)); err != nil {
		return fmt.Errorf("gitrepo: create partial parent: %w", err)
	}
	created := false
	if err := os.Mkdir(paths.Partial, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("gitrepo: create partial repository directory: %w", err)
		}
	} else {
		created = true
	}
	if err := i.validatePartialPath(paths.Partial); err != nil {
		return err
	}

	want := repositoryMarker{
		Version:      1,
		ProjectID:    project.ID,
		OperationID:  project.OperationID,
		SourcePath:   project.SourcePath,
		SourceRef:    project.SourceRef,
		InitialSHA:   project.InitialSHA,
		CanonicalRef: project.CanonicalRef,
	}
	markerPath := filepath.Join(paths.Partial, markerFilename)
	if _, err := os.Lstat(markerPath); errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(paths.Partial)
		if readErr != nil {
			return fmt.Errorf("gitrepo: inspect unmarked partial repository: %w", readErr)
		}
		if len(entries) != 0 {
			return errors.New("gitrepo: refusing to adopt unmarked partial repository")
		}
		if err := writeMarker(markerPath, want); err != nil {
			return err
		}
		if err := syncDirectory(paths.Partial); err != nil {
			return fmt.Errorf("gitrepo: sync partial marker: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("gitrepo: stat partial marker: %w", err)
	} else {
		got, readErr := readMarker(markerPath)
		if readErr != nil {
			return readErr
		}
		if got != want {
			return errors.New("gitrepo: partial repository ownership marker does not match pending operation")
		}
	}
	if created {
		if err := syncDirectory(filepath.Dir(paths.Partial)); err != nil {
			return fmt.Errorf("gitrepo: sync partial parent: %w", err)
		}
	}
	return nil
}

func (i *Initializer) ensureInitialObject(ctx context.Context, project Project, repoPath string) error {
	hasInitial, err := i.hasCommit(ctx, repoPath, project.InitialSHA)
	if err != nil {
		return err
	}
	if !hasInitial {
		if _, err := i.git(ctx,
			"-c", "protocol.file.allow=always",
			"--git-dir="+repoPath,
			"fetch", "--no-tags", "--no-write-fetch-head", project.SourcePath, project.InitialSHA,
		); err != nil {
			return fmt.Errorf("gitrepo: import saved initial commit: %w", err)
		}
		hasInitial, err = i.hasCommit(ctx, repoPath, project.InitialSHA)
		if err != nil {
			return err
		}
		if !hasInitial {
			return errors.New("gitrepo: imported repository does not contain saved initial commit")
		}
	}

	initRef := initializationRef(project)
	current, exists, err := i.resolveRef(ctx, repoPath, initRef)
	if err != nil {
		return err
	}
	if exists && current != project.InitialSHA {
		return errors.New("gitrepo: initialization ref points to a different commit")
	}
	if !exists {
		if _, err := i.git(ctx, "--git-dir="+repoPath, "update-ref", initRef, project.InitialSHA, zeroObjectID(project.InitialSHA)); err != nil {
			return fmt.Errorf("gitrepo: protect imported initial commit: %w", err)
		}
	}
	return nil
}

func (i *Initializer) ensureCanonical(ctx context.Context, project Project, repoPath string) error {
	current, exists, err := i.resolveRef(ctx, repoPath, project.CanonicalRef)
	if err != nil {
		return err
	}
	if exists && current != project.InitialSHA {
		return errors.New("gitrepo: partial canonical ref differs from saved initial commit")
	}
	if !exists {
		if _, err := i.git(ctx, "--git-dir="+repoPath, "update-ref", project.CanonicalRef, project.InitialSHA, zeroObjectID(project.InitialSHA)); err != nil {
			return fmt.Errorf("gitrepo: create canonical ref: %w", err)
		}
	}
	if _, err := i.git(ctx, "--git-dir="+repoPath, "symbolic-ref", "HEAD", project.CanonicalRef); err != nil {
		return fmt.Errorf("gitrepo: point bare HEAD at canonical ref: %w", err)
	}
	initRef := initializationRef(project)
	if _, exists, err := i.resolveRef(ctx, repoPath, initRef); err != nil {
		return err
	} else if exists {
		if _, err := i.git(ctx, "--git-dir="+repoPath, "update-ref", "-d", initRef, project.InitialSHA); err != nil {
			return fmt.Errorf("gitrepo: remove initialization ref: %w", err)
		}
	}
	return nil
}

func (i *Initializer) verifyInitialized(ctx context.Context, project Project, repoPath string) (Fact, error) {
	fact, err := i.verifyRepository(ctx, project, repoPath)
	if err != nil {
		return Fact{}, err
	}
	hasInitial, err := i.hasCommit(ctx, repoPath, project.InitialSHA)
	if err != nil {
		return Fact{}, err
	}
	if !hasInitial {
		return Fact{}, errors.New("gitrepo: initialized repository lost its initial commit")
	}
	if _, err := i.git(ctx, "--git-dir="+repoPath, "merge-base", "--is-ancestor", project.InitialSHA, fact.CanonicalSHA); err != nil {
		if ctx.Err() != nil {
			return Fact{}, ctx.Err()
		}
		return Fact{}, errors.New("gitrepo: actual canonical ref does not contain the immutable initial commit")
	}
	return fact, nil
}

func (i *Initializer) verifyRepository(ctx context.Context, project Project, repoPath string) (Fact, error) {
	marker, err := readMarker(filepath.Join(repoPath, markerFilename))
	if err != nil {
		return Fact{}, err
	}
	if marker.ProjectID != project.ID || marker.SourcePath != project.SourcePath || marker.SourceRef != project.SourceRef ||
		marker.InitialSHA != project.InitialSHA || marker.CanonicalRef != project.CanonicalRef {
		return Fact{}, errors.New("gitrepo: control repository ownership marker does not match project")
	}
	bare, err := i.isBare(ctx, repoPath)
	if err != nil {
		return Fact{}, err
	}
	if !bare {
		return Fact{}, errors.New("gitrepo: control repository is not bare")
	}
	if _, err := i.git(ctx, "--git-dir="+repoPath, "fsck", "--full", "--strict"); err != nil {
		return Fact{}, fmt.Errorf("gitrepo: control repository fsck failed: %w", err)
	}
	canonicalSHA, exists, err := i.resolveRef(ctx, repoPath, project.CanonicalRef)
	if err != nil {
		return Fact{}, err
	}
	if !exists {
		return Fact{}, errors.New("gitrepo: canonical ref is missing")
	}
	return Fact{
		ProjectID:       project.ID,
		ControlRepoPath: repoPath,
		CanonicalRef:    project.CanonicalRef,
		CanonicalSHA:    canonicalSHA,
		InitialSHA:      project.InitialSHA,
		Bare:            true,
	}, nil
}

func (i *Initializer) isBare(ctx context.Context, repoPath string) (bool, error) {
	out, err := i.git(ctx, "--git-dir="+repoPath, "rev-parse", "--is-bare-repository")
	if err != nil {
		return false, fmt.Errorf("gitrepo: inspect bare repository: %w", err)
	}
	return strings.TrimSpace(out) == "true", nil
}

func (i *Initializer) hasCommit(ctx context.Context, repoPath, sha string) (bool, error) {
	_, err := i.git(ctx, "--git-dir="+repoPath, "cat-file", "-e", sha+"^{commit}")
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var commandErr *gitCommandError
		if errors.As(err, &commandErr) {
			return false, nil
		}
		return false, err
	}
	typeName, err := i.git(ctx, "--git-dir="+repoPath, "cat-file", "-t", sha)
	if err != nil {
		return false, fmt.Errorf("gitrepo: inspect commit type: %w", err)
	}
	return strings.TrimSpace(typeName) == "commit", nil
}

func (i *Initializer) resolveRef(ctx context.Context, repoPath, ref string) (string, bool, error) {
	out, err := i.git(ctx, "--git-dir="+repoPath, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		var commandErr *gitCommandError
		if errors.As(err, &commandErr) {
			return "", false, nil
		}
		return "", false, err
	}
	sha := strings.TrimSpace(out)
	if err := validateObjectID(sha); err != nil {
		return "", false, fmt.Errorf("gitrepo: ref %s resolved invalid commit: %w", ref, err)
	}
	typeName, err := i.git(ctx, "--git-dir="+repoPath, "cat-file", "-t", sha)
	if err != nil {
		return "", false, fmt.Errorf("gitrepo: inspect ref %s target: %w", ref, err)
	}
	if strings.TrimSpace(typeName) != "commit" {
		return "", false, fmt.Errorf("gitrepo: ref %s does not point to a commit", ref)
	}
	return sha, true, nil
}

func (i *Initializer) afterPhase(ctx context.Context, phase Phase, project Project, paths Paths) error {
	fact := phaseFact{Project: project, Paths: paths}
	if contractPhaseHook != nil {
		if err := contractPhaseHook(ctx, phase, fact); err != nil {
			return fmt.Errorf("gitrepo: contract phase %s: %w", phase, err)
		}
	}
	if i.phaseHook == nil {
		return nil
	}
	if err := i.phaseHook(ctx, phase, fact); err != nil {
		return fmt.Errorf("gitrepo: phase %s: %w", phase, err)
	}
	return nil
}
