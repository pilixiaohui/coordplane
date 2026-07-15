package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCommandsIgnoreAmbientRepositoryObjectAndConfigOverrides(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	poison, _ := newSourceRepository(t)
	poisonHead := commitFile(t, poison, "poison.txt", "poison\n", "make poison history distinct")
	if poisonHead == initial {
		t.Fatal("poison repository unexpectedly has the source commit")
	}

	hookRoot := t.TempDir()
	sentinel := filepath.Join(hookRoot, "hook-ran")
	hook := filepath.Join(hookRoot, "reference-transaction")
	requireNoError(t, os.WriteFile(hook, []byte(fmt.Sprintf("#!/bin/sh\n: > %q\n", sentinel)), 0o700))
	home := t.TempDir()
	requireNoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(fmt.Sprintf("[core]\n\thooksPath = %s\n", hookRoot)), 0o600))

	poisonGitDir := filepath.Join(poison, ".git")
	poisonVariables := map[string]string{
		"HOME":                             home,
		"GIT_DIR":                          poisonGitDir,
		"GIT_WORK_TREE":                    poison,
		"GIT_COMMON_DIR":                   poisonGitDir,
		"GIT_OBJECT_DIRECTORY":             filepath.Join(poisonGitDir, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(poisonGitDir, "objects"),
		"GIT_INDEX_FILE":                   filepath.Join(poisonGitDir, "index"),
		"GIT_NAMESPACE":                    "poison",
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 "core.hooksPath",
		"GIT_CONFIG_VALUE_0":               hookRoot,
		"GIT_ALLOW_PROTOCOL":               "ext",
	}
	for name, value := range poisonVariables {
		t.Setenv(name, value)
	}

	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Preflight with poisoned process environment: %v", err)
	}
	if preflight.InitialSHA != initial || preflight.SourcePath != source {
		t.Fatalf("poisoned preflight = %+v, want source commit %s", preflight, initial)
	}
	project := testProject(t, initializer, preflight, "project-poison", "operation-poison")
	fact, err := initializer.Initialize(ctx, project)
	if err != nil {
		t.Fatalf("Initialize with poisoned process environment: %v", err)
	}
	if fact.CanonicalSHA != initial {
		t.Fatalf("canonical SHA = %s, want %s", fact.CanonicalSHA, initial)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient Git config executed a hook: %v", err)
	}
	if got, err := initializer.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef); err != nil || got != initial {
		t.Fatalf("actual canonical = %s, want %s: %v", got, initial, err)
	}

	for _, item := range gitEnvironment() {
		name, _, _ := strings.Cut(item, "=")
		if name == "HOME" || (strings.HasPrefix(name, "GIT_") && !allowedFixedGitEnvironment[name]) {
			t.Errorf("sanitized Git environment retained %s", name)
		}
	}
}

func TestInitializeRejectsNonEmptyGitTemplateBeforeHooksCanRun(t *testing.T) {
	ctx := context.Background()
	source, _ := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	requireNoError(t, err)
	project := testProject(t, initializer, preflight, "project-template", "operation-template")
	templateHooks := filepath.Join(initializer.root, ".empty-git-template", "hooks")
	requireNoError(t, os.MkdirAll(templateHooks, 0o700))
	sentinel := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(templateHooks, "reference-transaction")
	requireNoError(t, os.WriteFile(hook, []byte(fmt.Sprintf("#!/bin/sh\n: > %q\n", sentinel)), 0o700))

	if _, err := initializer.Initialize(ctx, project); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("Initialize error = %v, want non-empty template rejection", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untrusted template hook ran: %v", err)
	}
	if _, err := os.Stat(project.ControlRepoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final repository exists after template rejection: %v", err)
	}
}

var allowedFixedGitEnvironment = map[string]bool{
	"GIT_CONFIG_GLOBAL":      true,
	"GIT_CONFIG_SYSTEM":      true,
	"GIT_CONFIG_NOSYSTEM":    true,
	"GIT_ATTR_NOSYSTEM":      true,
	"GIT_NO_REPLACE_OBJECTS": true,
	"GIT_OPTIONAL_LOCKS":     true,
	"GIT_TERMINAL_PROMPT":    true,
	"GIT_ALLOW_PROTOCOL":     true,
}

func TestInitializeAndVerifyRejectValidRepositoryReachedThroughFinalSymlink(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	requireNoError(t, err)
	project := testProject(t, initializer, preflight, "project-final-link", "operation-final-link")
	if _, err := initializer.Initialize(ctx, project); err != nil {
		t.Fatal(err)
	}
	outdir := filepath.Join(t.TempDir(), "owned.git")
	requireNoError(t, os.Rename(project.ControlRepoPath, outdir))
	requireNoError(t, os.Symlink(outdir, project.ControlRepoPath))

	for name, call := range map[string]func() error{
		"initialize": func() error {
			_, err := initializer.Initialize(ctx, project)
			return err
		},
		"verify": func() error {
			_, err := initializer.Verify(ctx, project)
			return err
		},
		"resolve": func() error {
			_, err := initializer.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("%s error = %v, want symlink rejection", name, err)
			}
		})
	}
	if initializer.Exists(project.ControlRepoPath) {
		t.Fatal("Exists accepted a symlinked final repository")
	}
	if got := gitDirOutput(t, outdir, "rev-parse", project.CanonicalRef+"^{commit}"); got != initial {
		t.Fatalf("rejected target canonical changed to %s, want %s", got, initial)
	}
}

func TestInitializeRejectsSymlinkedPartialRepositoryAndParent(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Initializer, Paths) string
	}{
		{
			name: "repository",
			setup: func(t *testing.T, initializer *Initializer, paths Paths) string {
				t.Helper()
				requireNoError(t, initializer.ensureDirectSubdirectories(filepath.Dir(paths.Partial)))
				outside := t.TempDir()
				requireNoError(t, os.Symlink(outside, paths.Partial))
				return outside
			},
		},
		{
			name: "parent",
			setup: func(t *testing.T, initializer *Initializer, paths Paths) string {
				t.Helper()
				partialRoot := filepath.Join(initializer.root, ".partial")
				requireNoError(t, initializer.ensureDirectSubdirectories(partialRoot))
				outside := t.TempDir()
				requireNoError(t, os.Symlink(outside, filepath.Dir(paths.Partial)))
				return outside
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			source, _ := newSourceRepository(t)
			initializer := newTestInitializer(t)
			preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
			requireNoError(t, err)
			project := testProject(t, initializer, preflight, "project-partial-link", "operation-partial-link")
			paths, err := initializer.Paths(project.ID, project.OperationID)
			requireNoError(t, err)
			outside := test.setup(t, initializer, paths)
			if _, err := initializer.Initialize(ctx, project); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("Initialize error = %v, want symlink rejection", err)
			}
			entries, err := os.ReadDir(outside)
			requireNoError(t, err)
			if len(entries) != 0 {
				t.Fatalf("symlink target was mutated: %v", entries)
			}
			if _, err := os.Lstat(paths.Final); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("final repository exists after rejection: %v", err)
			}
		})
	}
}

func TestInitializeRejectsRepositoryRootReplacedBySymlink(t *testing.T) {
	ctx := context.Background()
	source, _ := newSourceRepository(t)
	initializer := newTestInitializer(t)
	preflight, err := initializer.Preflight(ctx, source, "refs/heads/main")
	requireNoError(t, err)
	project := testProject(t, initializer, preflight, "project-root-link", "operation-root-link")
	originalRoot := initializer.root + ".moved"
	requireNoError(t, os.Rename(initializer.root, originalRoot))
	outside := t.TempDir()
	requireNoError(t, os.Symlink(outside, initializer.root))
	if _, err := initializer.Initialize(ctx, project); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Initialize error = %v, want replaced root rejection", err)
	}
	entries, err := os.ReadDir(outside)
	requireNoError(t, err)
	if len(entries) != 0 {
		t.Fatalf("replacement root was mutated: %v", entries)
	}
}

func TestPathsAndProjectValidationRejectContainmentEscapes(t *testing.T) {
	initializer := newTestInitializer(t)
	for name, values := range map[string][2]string{
		"project traversal":   {"../project", "operation"},
		"operation traversal": {"project", "../operation"},
		"project separator":   {"nested/project", "operation"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := initializer.Paths(values[0], values[1]); err == nil {
				t.Fatalf("Paths(%q, %q) accepted containment escape", values[0], values[1])
			}
		})
	}
}
