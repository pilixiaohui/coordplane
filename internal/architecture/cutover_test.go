package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

type productionSource struct {
	rel  string
	raw  []byte
	file *ast.File
}

func TestV1SchemaBusinessTableAllowlist(t *testing.T) {
	root := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "store", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	allowed := stringSet(
		"projects", "agents", "tasks", "runs", "messages", "events",
		"schema_migrations", "request_dedupes",
	)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllSubmatch(raw, -1) {
		name := strings.ToLower(string(match[1]))
		if !allowed[name] {
			t.Errorf("schema contains legacy or unowned table %q", name)
		}
		seen[name] = true
	}
	for _, table := range []string{"projects", "agents", "tasks", "runs", "messages", "events"} {
		if !seen[table] {
			t.Errorf("schema is missing business table %q", table)
		}
	}
}

func TestV1ProductionOwnershipBoundaries(t *testing.T) {
	sources := productionSources(t)
	t.Run("canonical DDL owner", func(t *testing.T) {
		ddl := regexp.MustCompile(`(?i)CREATE\s+TABLE`)
		var offenders []string
		for _, source := range sources {
			if source.rel != "internal/store/schema.go" && ddl.Match(source.raw) {
				offenders = append(offenders, source.rel)
			}
		}
		failOffenders(t, "production DDL exists outside internal/store/schema.go", offenders)
	})
	t.Run("database SQL owner", func(t *testing.T) {
		var offenders []string
		for _, source := range sources {
			if !strings.HasPrefix(source.rel, "internal/store/") && sourceImports(source, "database/sql") {
				offenders = append(offenders, source.rel)
			}
		}
		failOffenders(t, "database/sql is owned only by internal/store", offenders)
	})
	t.Run("concrete owner composition", func(t *testing.T) {
		forbidden := stringSet(
			"coordplane/internal/gitrepo",
			"coordplane/internal/store",
		)
		var offenders []string
		for _, source := range sources {
			if strings.HasPrefix(source.rel, "internal/daemon/") {
				continue
			}
			for path := range forbidden {
				if sourceImports(source, path) {
					offenders = append(offenders, source.rel+": "+path)
				}
			}
		}
		failOffenders(t, "concrete store/Git owners may only be composed by internal/daemon", offenders)
	})
	t.Run("Core has no concrete owner", func(t *testing.T) {
		forbidden := stringSet(
			"coordplane/internal/daemon",
			"coordplane/internal/gitrepo",
			"coordplane/internal/store",
			"coordplane/internal/transport",
		)
		var offenders []string
		for _, source := range sources {
			if !strings.HasPrefix(source.rel, "internal/core/") {
				continue
			}
			for path := range forbidden {
				if sourceImports(source, path) {
					offenders = append(offenders, source.rel+": "+path)
				}
			}
		}
		failOffenders(t, "Core imports concrete owners", offenders)
	})
	t.Run("Core owns transactions", func(t *testing.T) {
		var offenders []string
		for _, source := range sources {
			if strings.HasPrefix(source.rel, "internal/core/") {
				continue
			}
			ast.Inspect(source.file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "Transact" {
					offenders = append(offenders, source.rel)
				}
				return true
			})
		}
		failOffenders(t, "only internal/core may orchestrate production transactions", offenders)
	})
}

func TestLegacyProductPackagesAndEntrypointsAreDeleted(t *testing.T) {
	root := repositoryRoot(t)
	packages := []string{
		"adapters", "backend", "capability", "codemanagement", "commandrun",
		"coordination", "cpprobe", "delivery", "objects", "operator", "policy",
		"queue", "releaseacceptance", "releasehealth", "skills", "teamconfig", "validation",
	}
	for _, name := range packages {
		if found, err := containsGoFile(filepath.Join(root, "internal", name)); err != nil {
			t.Fatal(err)
		} else if found {
			t.Errorf("legacy production package still exists: internal/%s", name)
		}
	}
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
	forbidden := stringSet(
		"/call", "/capabilities", "/skills", "/operator/tasks",
		"COORDPLANE_BACKEND_URL", "release-health", "teamconfig", "backend-url",
		"COORDPLANE_PROVIDER_POLICY_MODE", "COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES",
		"strict_coordlink_call",
	)
	var offenders []string
	for _, source := range productionSources(t) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for token := range forbidden {
				if strings.Contains(value, token) {
					offenders = append(offenders, source.rel+": "+token)
				}
			}
			return true
		})
	}
	failOffenders(t, "legacy production routes or CLI tokens remain", offenders)
}

func TestNeedDirectoryHasExactlyFiveAuthorities(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repositoryRoot(t), "need"))
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

func productionSources(t *testing.T, roots ...string) []productionSource {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{"cmd", "internal"}
	}
	root := repositoryRoot(t)
	docker := build.Default
	docker.BuildTags = append(append([]string(nil), docker.BuildTags...), "docker")
	var result []productionSource
	for _, relativeRoot := range roots {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relativeRoot)), func(path string, entry os.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			defaultMatch, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			dockerMatch, err := docker.MatchFile(filepath.Dir(path), filepath.Base(path))
			if err != nil || (!defaultMatch && !dockerMatch) {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result = append(result, productionSource{filepath.ToSlash(rel), raw, parsed})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func sourceImports(source productionSource, path string) bool {
	for _, specification := range source.file.Imports {
		value, err := strconv.Unquote(specification.Path.Value)
		if err == nil && value == path {
			return true
		}
	}
	return false
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
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		found = found || (!entry.IsDir() && filepath.Ext(entry.Name()) == ".go")
		return nil
	})
	return found, err
}

func sourceWords(value string) []string {
	runes := []rune(value)
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, strings.ToLower(string(runes[start:end])))
		}
		start = -1
	}
	for index, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			flush(index)
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		previous := runes[index-1]
		nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
		if unicode.IsUpper(current) && (unicode.IsLower(previous) || unicode.IsDigit(previous) ||
			unicode.IsUpper(previous) && nextLower) {
			flush(index)
			start = index
		}
	}
	flush(len(runes))
	return words
}

func stringSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func failOffenders(t *testing.T, message string, offenders []string) {
	t.Helper()
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	unique := offenders[:1]
	for _, offender := range offenders[1:] {
		if offender != unique[len(unique)-1] {
			unique = append(unique, offender)
		}
	}
	t.Fatalf("%s:\n%s", message, strings.Join(unique, "\n"))
}
