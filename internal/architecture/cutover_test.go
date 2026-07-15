package architecture_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestV1SchemaBusinessTableAllowlist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal", "store", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	allowed := map[string]bool{"projects": true, "agents": true, "tasks": true, "runs": true, "messages": true, "events": true, "schema_migrations": true, "request_dedupes": true}
	seen := map[string]bool{}
	for _, match := range pattern.FindAllSubmatch(raw, -1) {
		name := strings.ToLower(string(match[1]))
		if !allowed[name] {
			t.Errorf("schema contains unowned table %q", name)
		}
		seen[name] = true
	}
	if !maps.Equal(seen, allowed) {
		t.Errorf("schema table set = %v, want %v", seen, allowed)
	}
}

func TestSchemaDDLExistsOnlyInCanonicalStoreSchema(t *testing.T) {
	assertProductionTokenScope(t, []string{"CREATE TABLE"}, func(rel string) bool { return rel == "internal/store/schema.go" })
}

func TestDatabaseSQLHasOneProductionOwner(t *testing.T) {
	assertProductionTokenScope(t, []string{`"database/sql"`}, func(rel string) bool { return strings.HasPrefix(rel, "internal/store/") })
}

func TestLegacyProductPackagesAreDeleted(t *testing.T) {
	legacy := []string{"adapters", "backend", "capability", "codemanagement", "commandrun", "coordination", "cpprobe", "delivery", "objects", "operator", "policy", "queue", "releaseacceptance", "releasehealth", "skills", "teamconfig", "validation"}
	for index := range legacy {
		legacy[index] = filepath.Join("internal", legacy[index])
	}
	assertNoGoFiles(t, legacy...)
}

func TestLegacyRuntimeEntrypointsAreDeleted(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"command_cli.go", "command_policy.go", "provider_audit.go"} {
		if _, err := os.Stat(filepath.Join(root, "internal", "runtime", name)); err == nil {
			t.Errorf("legacy runtime entry returned: %s", name)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestProductionSourcesDoNotReintroduceLegacyRoutesOrCLI(t *testing.T) {
	tokens := []string{`"/call"`, `"/capabilities"`, `"/skills"`, `"/operator/tasks`, "COORDPLANE_BACKEND_URL", "release-health", "teamconfig", "backend-url", "COORDPLANE_PROVIDER_POLICY_MODE", "COORDPLANE_PROVIDER_ALLOWED_CAPABILITIES", "strict_coordlink_call"}
	assertProductionTokenScope(t, tokens, func(string) bool { return false })
}

func TestStoreAndGitConcreteOwnersAreImportedOnlyByDaemonComposition(t *testing.T) {
	tokens := []string{`"coordplane/internal/gitrepo"`, `"coordplane/internal/store"`}
	assertProductionTokenScope(t, tokens, func(rel string) bool { return strings.HasPrefix(rel, "internal/daemon/") })
}

func TestCoreDoesNotImportConcreteOwners(t *testing.T) {
	tokens := []string{`"coordplane/internal/daemon"`, `"coordplane/internal/gitrepo"`, `"coordplane/internal/store"`, `"coordplane/internal/transport"`}
	assertProductionTokenScope(t, tokens, func(rel string) bool { return !strings.HasPrefix(rel, "internal/core/") })
}

func TestCoreIsTheOnlyProductionTransactionOrchestrator(t *testing.T) {
	assertProductionTokenScope(t, []string{".Transact("}, func(rel string) bool { return strings.HasPrefix(rel, "internal/core/") })
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
	if strings.Join(names, "\n") != "README.md\nacceptance.md\ncore.md\ngit.md\nruntime.md" {
		t.Fatalf("need authority set = %v", names)
	}
}

func assertProductionTokenScope(t *testing.T, tokens []string, allowed func(string) bool) {
	assertProductionTextScope(t, tokens, false, allowed)
}

func assertProductionTextScope(t *testing.T, tokens []string, lower bool, allowed func(string) bool) {
	t.Helper()
	root := repositoryRoot(t)
	var offenders []string
	walkProductionGo(t, root, func(path string, raw []byte) {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if allowed(rel) {
			return
		}
		text := string(raw)
		if lower {
			text = strings.ToLower(text)
		}
		for _, token := range tokens {
			if strings.Contains(text, token) {
				offenders = append(offenders, rel+": "+token)
			}
		}
	})
	if len(offenders) != 0 {
		sort.Strings(offenders)
		t.Fatalf("production token ownership violation: %s", strings.Join(offenders, ", "))
	}
}

func walkProductionGo(t *testing.T, root string, visit func(path string, raw []byte)) {
	t.Helper()
	for _, top := range []string{"cmd", "internal"} {
		if err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err == nil {
				visit(path, raw)
			}
			return err
		}); err != nil {
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

func assertNoGoFiles(t *testing.T, paths ...string) {
	t.Helper()
	root := repositoryRoot(t)
	for _, path := range paths {
		if entries, _ := filepath.Glob(filepath.Join(root, path, "*.go")); len(entries) != 0 {
			t.Errorf("legacy production package returned: %s", path)
		}
	}
}
