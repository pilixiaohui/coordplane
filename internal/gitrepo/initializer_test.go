package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPreflightRequiresFullBranchRefAndDoesNotModifySource(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	before := snapshotSource(t, source)
	initializer := newTestInitializer(t)

	for _, ref := range []string{"main", "refs/tags/main", "refs/heads/", "refs/heads/main.lock"} {
		t.Run(strings.ReplaceAll(ref, "/", "_"), func(t *testing.T) {
			if _, err := initializer.Preflight(ctx, source, ref); err == nil {
				t.Fatalf("Preflight(%q) succeeded, want full branch ref rejection", ref)
			}
		})
	}

	fact, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if fact.SourcePath != source || fact.SourceRef != "refs/heads/main" || fact.InitialSHA != initial {
		t.Fatalf("Preflight fact = %+v, want source/main/%s", fact, initial)
	}
	assertSourceSnapshot(t, source, before)
}

func TestInitializeUsesSavedInitialSHANotMovedSourceBranch(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	moved := commitFile(t, source, "moved.txt", "moved\n", "move source branch")
	if moved == initial {
		t.Fatal("source branch did not move")
	}
	before := snapshotSource(t, source)
	project := testProject(t, initializer, preflight, "project-a", "operation-a")

	fact, err := initializer.Initialize(ctx, project)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if fact.CanonicalSHA != initial {
		t.Fatalf("canonical SHA = %s, want saved initial %s (source now %s)", fact.CanonicalSHA, initial, moved)
	}
	if got := gitOutput(t, source, "rev-parse", "refs/heads/main^{commit}"); got != moved {
		t.Fatalf("source branch = %s, want moved SHA %s", got, moved)
	}
	assertSourceSnapshot(t, source, before)

	again, err := initializer.Initialize(ctx, project)
	if err != nil {
		t.Fatalf("idempotent Initialize: %v", err)
	}
	if again != fact {
		t.Fatalf("second Initialize fact = %+v, want %+v", again, fact)
	}
	assertSourceSnapshot(t, source, before)
}

func TestInitializeReconcilesEveryCompletedPhase(t *testing.T) {
	phases := []Phase{
		PhasePartialPrepared,
		PhaseBareInitialized,
		PhaseObjectsImported,
		PhaseCanonicalWritten,
		PhaseIntegrityVerified,
		PhasePromoted,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			source, initial := newSourceRepository(t)
			initializer := newTestInitializer(t)
			preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
			if err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			project := testProject(t, initializer, preflight, "project-phase", "operation-phase")
			injected := errors.New("injected daemon interruption")
			fired := false
			initializer.phaseHook = func(_ context.Context, got Phase, _ phaseFact) error {
				if got == phase && !fired {
					fired = true
					return injected
				}
				return nil
			}

			if _, err := initializer.Initialize(ctx, project); !errors.Is(err, injected) {
				t.Fatalf("Initialize interruption error = %v, want %v", err, injected)
			}
			if !fired {
				t.Fatalf("phase hook %s did not fire", phase)
			}
			initializer.phaseHook = nil
			fact, err := initializer.Initialize(ctx, project)
			if err != nil {
				t.Fatalf("reconciled Initialize: %v", err)
			}
			if fact.CanonicalSHA != initial || !fact.Bare {
				t.Fatalf("reconciled fact = %+v, want bare canonical %s", fact, initial)
			}
			verified, err := initializer.Verify(ctx, project)
			if err != nil {
				t.Fatalf("Verify reconciled repository: %v", err)
			}
			if verified != fact {
				t.Fatalf("Verify fact = %+v, want %+v", verified, fact)
			}
		})
	}
}

func TestVerifyAndRepeatedInitializeNeverResetActualCanonical(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	project := testProject(t, initializer, preflight, "project-repair", "operation-initialize")
	if _, err := initializer.Initialize(ctx, project); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	advanced := commitFile(t, source, "advanced.txt", "advanced\n", "advance canonical")
	gitDirOutput(t, project.ControlRepoPath,
		"-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", source, advanced)
	gitDirOutput(t, project.ControlRepoPath, "update-ref", project.CanonicalRef, advanced, initial)

	repair := project
	repair.OperationID = "operation-repair"
	fact, err := initializer.Verify(ctx, repair)
	if err != nil {
		t.Fatalf("Verify repair: %v", err)
	}
	if fact.CanonicalSHA != advanced {
		t.Fatalf("Verify canonical = %s, want actual advanced %s", fact.CanonicalSHA, advanced)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != advanced {
		t.Fatalf("canonical after Verify = %s, want %s", got, advanced)
	}
	verifiedAgain, err := initializer.Verify(ctx, repair)
	if err != nil {
		t.Fatalf("idempotent Verify repair: %v", err)
	}
	if verifiedAgain != fact {
		t.Fatalf("second Verify fact = %+v, want %+v", verifiedAgain, fact)
	}

	again, err := initializer.Initialize(ctx, project)
	if err != nil {
		t.Fatalf("Initialize replay after canonical advance: %v", err)
	}
	if again.CanonicalSHA != advanced {
		t.Fatalf("Initialize replay canonical = %s, want %s", again.CanonicalSHA, advanced)
	}
	if got := gitDirOutput(t, project.ControlRepoPath, "rev-parse", project.CanonicalRef+"^{commit}"); got != advanced {
		t.Fatalf("canonical after Initialize replay = %s, want %s", got, advanced)
	}
}

func TestInitializeRefusesUnmarkedPartialRepository(t *testing.T) {
	ctx := context.Background()
	source, _ := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	project := testProject(t, initializer, preflight, "project-foreign", "operation-foreign")
	paths, err := initializer.Paths(project.ID, project.OperationID)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if err := os.MkdirAll(paths.Partial, 0o700); err != nil {
		t.Fatalf("create foreign partial: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Partial, "foreign"), []byte("foreign\n"), 0o600); err != nil {
		t.Fatalf("write foreign partial: %v", err)
	}
	if _, err := initializer.Initialize(ctx, project); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("Initialize foreign partial error = %v, want ownership rejection", err)
	}
	if _, err := os.Stat(paths.Final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final repository exists after foreign partial rejection: %v", err)
	}
}

func TestExistsOnlyAcceptsDirectDeterministicFinalPath(t *testing.T) {
	ctx := context.Background()
	source, _ := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	project := testProject(t, initializer, preflight, "project-exists", "operation-exists")
	if _, err := initializer.Initialize(ctx, project); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !initializer.Exists(project.ControlRepoPath) {
		t.Fatalf("Exists(%q) = false, want true", project.ControlRepoPath)
	}

	outside := filepath.Join(t.TempDir(), "outside.git")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("create outside directory: %v", err)
	}
	nested := filepath.Join(initializer.root, "nested", "project.git")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	partial := filepath.Join(initializer.root, ".partial", "project", "operation.git")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatalf("create partial directory: %v", err)
	}
	alias := filepath.Join(initializer.root, "alias.git")
	if err := os.Symlink(project.ControlRepoPath, alias); err != nil {
		t.Fatalf("create repository symlink: %v", err)
	}
	escaped := project.ControlRepoPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(project.ControlRepoPath)
	for name, path := range map[string]string{
		"outside": outside,
		"nested":  nested,
		"partial": partial,
		"symlink": alias,
		"escaped": escaped,
	} {
		t.Run(name, func(t *testing.T) {
			if initializer.Exists(path) {
				t.Fatalf("Exists(%q) = true, want false", path)
			}
		})
	}
}

func TestResolveReadsActualCanonicalWithoutMutatingMarker(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	project := testProject(t, initializer, preflight, "project-resolve", "operation-resolve")
	if _, err := initializer.Initialize(ctx, project); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	markerPath := filepath.Join(project.ControlRepoPath, markerFilename)
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker before Resolve: %v", err)
	}
	markerInfoBefore, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat marker before Resolve: %v", err)
	}

	sha, err := initializer.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sha != initial {
		t.Fatalf("Resolve SHA = %s, want %s", sha, initial)
	}
	markerAfter, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker after Resolve: %v", err)
	}
	markerInfoAfter, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat marker after Resolve: %v", err)
	}
	if !reflect.DeepEqual(markerAfter, markerBefore) || !markerInfoAfter.ModTime().Equal(markerInfoBefore.ModTime()) {
		t.Fatal("Resolve mutated the ownership marker")
	}

	for _, testCase := range []struct {
		name string
		path string
		ref  string
	}{
		{name: "short ref", path: project.ControlRepoPath, ref: "main"},
		{name: "tag ref", path: project.ControlRepoPath, ref: "refs/tags/main"},
		{name: "outside path", path: source, ref: project.CanonicalRef},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := initializer.Resolve(ctx, testCase.path, testCase.ref); err == nil {
				t.Fatalf("Resolve(%q, %q) succeeded, want rejection", testCase.path, testCase.ref)
			}
		})
	}
}

type sourceState struct {
	Status string
	Refs   string
	HEAD   []byte
	Config []byte
}

func newTestInitializer(t *testing.T) *Initializer {
	t.Helper()
	initializer, err := New(filepath.Join(t.TempDir(), "repos"))
	if err != nil {
		t.Fatalf("New initializer: %v", err)
	}
	return initializer
}

func newSourceRepository(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	gitCommand(t, "init", "--initial-branch=main", path)
	gitOutput(t, path, "config", "user.name", "CoordPlane Test")
	gitOutput(t, path, "config", "user.email", "coordplane@example.invalid")
	initial := commitFile(t, path, "README.md", "initial\n", "initial")
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("canonicalize source: %v", err)
	}
	return canonical, initial
}

func commitFile(t *testing.T, repoPath, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoPath, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	gitOutput(t, repoPath, "add", "--", name)
	gitOutput(t, repoPath, "commit", "-m", message)
	return gitOutput(t, repoPath, "rev-parse", "HEAD^{commit}")
}

func testProject(t *testing.T, initializer *Initializer, source SourceFact, projectID, operationID string) Project {
	t.Helper()
	paths, err := initializer.Paths(projectID, operationID)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	return Project{
		ID:              projectID,
		OperationID:     operationID,
		SourcePath:      source.SourcePath,
		SourceRef:       source.SourceRef,
		InitialSHA:      source.InitialSHA,
		ControlRepoPath: paths.Final,
		CanonicalRef:    source.SourceRef,
	}
}

func snapshotSource(t *testing.T, repoPath string) sourceState {
	t.Helper()
	head, err := os.ReadFile(filepath.Join(repoPath, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read source HEAD: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(repoPath, ".git", "config"))
	if err != nil {
		t.Fatalf("read source config: %v", err)
	}
	return sourceState{
		Status: gitOutput(t, repoPath, "status", "--porcelain=v1", "--untracked-files=all"),
		Refs:   gitOutput(t, repoPath, "for-each-ref", "--format=%(refname)%00%(objectname)"),
		HEAD:   head,
		Config: config,
	}
}

func assertSourceSnapshot(t *testing.T, repoPath string, want sourceState) {
	t.Helper()
	if got := snapshotSource(t, repoPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("source repository changed\n got: %+v\nwant: %+v", got, want)
	}
}

func gitOutput(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	return gitCommand(t, append([]string{"-C", repoPath}, args...)...)
}

func gitDirOutput(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	return gitCommand(t, append([]string{"--git-dir=" + gitDir}, args...)...)
}

func gitCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw))
}
