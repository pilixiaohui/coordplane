package architecture_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPF01ReleasePlatformIsDeleted(t *testing.T) {
	root := repositoryRoot()
	removed := []string{
		"internal/perfobs",
		"internal/coordlinkcli/perf_observer.go",
		"internal/coordlinkcli/perf_observer_default.go",
		"scripts/perf-v1.sh",
		"tests/e2e/perf_test.go",
		"tests/e2e/perf_fault_test.go",
		"tests/e2e/perf_report_test.go",
		"tests/e2e/perf_validator_test.go",
		"tests/e2e/testdata/runtime/fixturecheck.go",
	}
	for _, relative := range removed {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err == nil {
			t.Errorf("obsolete PF-01 path still exists: %s", relative)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	forbidden := []string{
		"coordplane/internal/perfobs", "PF01_", "COORDPLANE_PERF_", "BASELINE_BOOTSTRAP",
		"pf01=true", "withPerfObserver", "pf01-fixturecheck", "perf-v1.sh",
		"go:build perf", "coordplane.perf.",
	}
	for _, top := range []string{"cmd", "internal", "scripts", "tests"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || filepath.ToSlash(relative) == "internal/architecture/pf_cutover_test.go" {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, token := range forbidden {
				if strings.Contains(string(raw), token) {
					t.Errorf("obsolete PF-01 token %q remains in %s", token, filepath.ToSlash(relative))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
