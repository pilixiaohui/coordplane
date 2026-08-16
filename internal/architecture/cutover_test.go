package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"coordplane/tests/testsupport"
)

type productionSource struct {
	rel  string
	raw  []byte
	file *ast.File
}

var requireNoError = testsupport.RequireNoError

func TestV1SchemaBusinessTableAllowlist(t *testing.T) {
	root := repositoryRoot()
	raw, err := os.ReadFile(filepath.Join(root, "internal", "store", "schema.go"))
	requireNoError(t, err)
	pattern := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	allowed := stringSet(
		"projects", "agents", "tasks", "runs", "messages", "events",
		"schema_migrations", "request_dedupes",
		"participants", "roles", "participant_project_role", "credentials",
		// tasks_v4 is the SQLite staging table of the v4 human task lifecycle
		// migration: the tasks rebuild is created under this name, copied,
		// then renamed to tasks within the same migration transaction.
		"tasks_v4",
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
	root := repositoryRoot()
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

func TestRequirementDocumentContract(t *testing.T) {
	for _, check := range []func(*testing.T, string){
		checkRequirementDocumentSet,
		checkUserRequirementSequence,
	} {
		check(t, repositoryRoot())
	}
}

func checkRequirementDocumentSet(t *testing.T, root string) {
	entries, err := os.ReadDir(filepath.Join(root, "need"))
	requireNoError(t, err)
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	want := []string{"README.md", "acceptance.md", "core.md", "git.md", "runtime.md", "user-requirements-verbatim.md"}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("need document set = %v, want five normative documents plus one provenance record %v", names, want)
	}
}

func checkUserRequirementSequence(t *testing.T, root string) {
	raw, err := os.ReadFile(filepath.Join(root, "need", "user-requirements-verbatim.md"))
	requireNoError(t, err)
	matches := regexp.MustCompile(`(?m)^### UR-([0-9]{4})$`).FindAllSubmatch(raw, -1)
	if len(matches) == 0 {
		t.Fatal("user requirement provenance has no UR records")
	}
	for i, match := range matches {
		id, _ := strconv.Atoi(string(match[1]))
		if id != i+1 {
			t.Fatalf("user requirement provenance record %d has ID UR-%04d", i+1, id)
		}
	}
}

func productionSources(t *testing.T, roots ...string) []productionSource {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{"cmd", "internal"}
	}
	root := repositoryRoot()
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
		requireNoError(t, err)
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

var repositoryRoot = testsupport.RepositoryRoot

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
