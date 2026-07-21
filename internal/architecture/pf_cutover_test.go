package architecture_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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
	command := exec.Command("git", "-C", root, "ls-files", "-z")
	raw, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Split(string(bytes.TrimSuffix(raw, []byte{0})), "\x00")
	var maintained []string
	for _, relative := range files {
		if relative != "internal/architecture/pf_cutover_test.go" && !strings.HasPrefix(relative, "need/") {
			maintained = append(maintained, relative)
		}
	}
	offenders, err := scanPF01Tokens(root, maintained, forbidden)
	if err != nil {
		t.Fatal(err)
	}
	for _, offender := range offenders {
		t.Errorf("obsolete PF-01 token remains in %s", offender)
	}
}

func TestPF01GuardScansMaintainedRootIngress(t *testing.T) {
	for _, relative := range []string{"Makefile", "Dockerfile"} {
		t.Run(relative, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, relative), []byte("PF01_REINTRODUCED\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			offenders, err := scanPF01Tokens(root, []string{relative}, []string{"PF01_"})
			if err != nil {
				t.Fatal(err)
			}
			if len(offenders) == 0 {
				t.Fatal("maintained root ingress escaped PF-01 guard")
			}
		})
	}
}

func scanPF01Tokens(root string, files, forbidden []string) ([]string, error) {
	var offenders []string
	for _, relative := range files {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, err
		}
		for _, token := range forbidden {
			if strings.Contains(string(raw), token) {
				offenders = append(offenders, relative+":"+token)
			}
		}
	}
	return offenders, nil
}
