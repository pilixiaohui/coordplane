package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestV1SchemaBusinessTableAllowlist(t *testing.T) {
	root := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "store", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	allowed := map[string]bool{
		"projects":          true,
		"agents":            true,
		"tasks":             true,
		"runs":              true,
		"messages":          true,
		"events":            true,
		"schema_migrations": true,
		"request_dedupes":   true,
	}
	var unexpected []string
	for _, match := range pattern.FindAllSubmatch(raw, -1) {
		name := strings.ToLower(string(match[1]))
		if !allowed[name] {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Fatalf("schema contains legacy or unowned tables: %s", strings.Join(unexpected, ", "))
	}
	for _, table := range []string{"projects", "agents", "tasks", "runs", "messages", "events"} {
		if !patternForTable(table).Match(raw) {
			t.Errorf("schema is missing business table %q", table)
		}
	}
}

func TestSchemaDDLExistsOnlyInCanonicalStoreSchema(t *testing.T) {
	root := repositoryRoot(t)
	ddl := regexp.MustCompile(`(?i)CREATE\s+TABLE`)
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		if !ddl.Match(raw) {
			return
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if rel != "internal/store/schema.go" {
			offenders = append(offenders, rel)
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("production DDL exists outside internal/store/schema.go: %s", strings.Join(offenders, ", "))
	}
}

func TestDatabaseSQLHasOneProductionOwner(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"database/sql"`) && !strings.HasPrefix(filepath.ToSlash(rel), "internal/store/") {
			offenders = append(offenders, filepath.ToSlash(rel))
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("database/sql is owned only by internal/store; offenders: %s", strings.Join(offenders, ", "))
	}
}

func TestLegacyProductPackagesAreDeleted(t *testing.T) {
	root := repositoryRoot(t)
	for _, rel := range []string{
		"internal/adapters",
		"internal/backend",
		"internal/capability",
		"internal/codemanagement",
		"internal/commandrun",
		"internal/coordination",
		"internal/cpprobe",
		"internal/delivery",
		"internal/objects",
		"internal/operator",
		"internal/policy",
		"internal/queue",
		"internal/releaseacceptance",
		"internal/releasehealth",
		"internal/skills",
		"internal/teamconfig",
		"internal/validation",
	} {
		exists, err := containsGoFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("legacy production package still exists: %s", rel)
		}
	}
}

func TestLegacyRuntimeEntrypointsAreDeleted(t *testing.T) {
	root := repositoryRoot(t)
	for _, rel := range []string{
		"internal/runtime/command_cli.go",
		"internal/runtime/command_policy.go",
		"internal/runtime/provider_audit.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			t.Errorf("legacy runtime entry file still exists: %s", rel)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestProductionSourcesDoNotReintroduceLegacyRoutesOrCLI(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		`"/call"`,
		`"/capabilities"`,
		`"/skills"`,
		`"/operator/tasks`,
		`COORDPLANE_BACKEND_URL`,
		`release-health`,
		`teamconfig`,
		`backend-url`,
		`COORDPLANE_PROVIDER_POLICY_MODE`,
		`COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES`,
		`strict_coordlink_call`,
	}
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		text := string(raw)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					t.Fatal(err)
				}
				offenders = append(offenders, filepath.ToSlash(rel)+": "+token)
			}
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("legacy production routes or CLI tokens remain:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestStoreAndGitConcreteOwnersAreImportedOnlyByDaemonComposition(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		`"coordplane/internal/gitrepo"`,
		`"coordplane/internal/store"`,
	}
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/daemon/") {
			return
		}
		for _, importPath := range forbidden {
			if strings.Contains(string(raw), importPath) {
				offenders = append(offenders, rel+": "+importPath)
			}
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("concrete store/Git owners may only be composed by internal/daemon:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestCoreDoesNotImportConcreteOwners(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		`"coordplane/internal/daemon"`,
		`"coordplane/internal/gitrepo"`,
		`"coordplane/internal/store"`,
		`"coordplane/internal/transport"`,
	}
	coreRoot := filepath.Join(root, "internal", "core")
	_ = filepath.WalkDir(coreRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, token := range forbidden {
			if strings.Contains(string(raw), token) {
				t.Errorf("%s imports concrete owner %s", path, token)
			}
		}
		return nil
	})
}

func TestCoreIsTheOnlyProductionTransactionOrchestrator(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/core/") {
			return
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		callsTransact := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "Transact" {
				callsTransact = true
			}
			return true
		})
		if callsTransact {
			offenders = append(offenders, rel)
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("only internal/core may orchestrate production transactions: %s", strings.Join(offenders, ", "))
	}
}

func TestNeedDirectoryHasExactlyFiveAuthorities(t *testing.T) {
	root := repositoryRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "need"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	want := []string{"README.md", "acceptance.md", "core.md", "git.md", "runtime.md"}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("need authority set = %v, want %v", names, want)
	}
}

func patternForTable(table string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + regexp.QuoteMeta(table) + `\s*\(`)
}

func walkProductionGo(t *testing.T, root string, visit func(path string, raw []byte)) {
	t.Helper()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			visit(path, raw)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func containsGoFile(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".go" {
			found = true
		}
		return nil
	})
	return found, err
}
