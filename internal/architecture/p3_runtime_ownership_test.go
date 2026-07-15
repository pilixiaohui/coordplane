package architecture_test

import (
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/adapter"
)

func TestP3RuntimeAndAdapterCannotOwnDatabaseOrGit(t *testing.T) {
	tokens := []string{`"database/sql"`, `"coordplane/internal/store`, `"coordplane/internal/gitrepo`, `modernc.org/sqlite`}
	assertProductionTokenScope(t, tokens, func(rel string) bool {
		return !strings.HasPrefix(rel, "internal/runtime/") && !strings.HasPrefix(rel, "internal/adapter/")
	})
}

func TestP3LegacyRuntimeProductsCannotReturn(t *testing.T) {
	forbidden := []string{"provider_audit", "provideraudit", "transcript_ref", "transcripts", "command_runs", "commandrun", "runtime_instances", "cli_sessions", "runtime_tokens"}
	assertProductionTextScope(t, forbidden, true, func(string) bool { return false })
}

func TestP3ProductionAdapterRegistryIsOneStaticOneShotList(t *testing.T) {
	registry := adapter.Production()
	if names := registry.Names(); len(names) != 1 {
		t.Fatalf("production adapters = %v, want one", names)
	} else if entry, ok := registry.Lookup(names[0]); !ok || entry.Metadata().ExecutionModel != adapter.ExecutionOneShot {
		t.Fatalf("production adapter %q is not static one-shot", names[0])
	}
	if typeOf := reflect.TypeOf(registry); typeOf.NumMethod() != 2 || typeOf.Method(0).Name != "Lookup" || typeOf.Method(1).Name != "Names" {
		t.Fatalf("Registry public methods = %v, want only Lookup/Names", typeOf.NumMethod())
	}
	root := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "adapter", "adapter.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Count(text, "productionAdapters") != 2 || strings.Contains(text, "func Register") || strings.Contains(text, "func (r *Registry)") {
		t.Fatal("production adapter list is mutable or exposes registration")
	}
	assertProductionTokenScope(t, []string{`"codex"`}, func(rel string) bool {
		return !strings.HasPrefix(rel, "internal/runtime/") && !strings.HasPrefix(rel, "internal/daemon/")
	})
}

func TestP3DockerTagSelectsRuntimeIntegrationTest(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, "internal", "runtime", "docker_integration_test.go")
	defaultContext, dockerContext := build.Default, build.Default
	dockerContext.BuildTags = append(dockerContext.BuildTags, "docker")
	defaultMatch, defaultErr := defaultContext.MatchFile(filepath.Dir(path), filepath.Base(path))
	dockerMatch, dockerErr := dockerContext.MatchFile(filepath.Dir(path), filepath.Base(path))
	raw, readErr := os.ReadFile(path)
	if defaultErr != nil || dockerErr != nil || readErr != nil || defaultMatch || !dockerMatch || !strings.Contains(string(raw), "NewDockerExecutor") {
		t.Fatalf("docker integration selection default=%t docker=%t errors=%v/%v/%v", defaultMatch, dockerMatch, defaultErr, dockerErr, readErr)
	}
}

func TestP3EvidenceFreeRunWritersCannotReturn(t *testing.T) {
	forbidden := []string{".ActivateRun(", " ActivateRun(", ".InterruptRun(", " InterruptRun(", ".RecordRunTerminal(", " RecordRunTerminal("}
	assertProductionTokenScope(t, forbidden, func(string) bool { return false })
}
