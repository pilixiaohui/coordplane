package cpprobe_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coordplane/internal/cpprobe"
)

func TestTinyLedgerFixtureGeneratorCreatesCanonicalGitRepo(t *testing.T) {
	ctx := context.Background()
	fixture, err := cpprobe.GenerateTinyLedger(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("generate tiny ledger fixture: %v", err)
	}
	if fixture.CanonicalBranch != "main" || fixture.BaseRef == "" {
		t.Fatalf("fixture identity = %+v, want main branch and base ref", fixture)
	}
	if fixture.ExpectedReport != (cpprobe.LedgerReport{Income: 125, Expense: 40, Balance: 85}) {
		t.Fatalf("expected report = %+v", fixture.ExpectedReport)
	}
	for _, name := range []string{
		"go.mod",
		"ledger/ledger.go",
		"ledger/ledger_test.go",
		"cmd/ledger-report/main.go",
		"data/transactions.csv",
		"README.md",
	} {
		if _, err := os.Stat(filepath.Join(fixture.RepoPath, filepath.FromSlash(name))); err != nil {
			t.Fatalf("fixture file %s missing: %v", name, err)
		}
	}
	if got := strings.TrimSpace(gitOutput(t, fixture.RepoPath, "branch", "--show-current")); got != "main" {
		t.Fatalf("fixture branch = %q, want main", got)
	}
	if got := strings.TrimSpace(gitOutput(t, fixture.RepoPath, "log", "-1", "--format=%s")); got != "initial tiny ledger" {
		t.Fatalf("initial commit = %q", got)
	}
	if got := strings.TrimSpace(gitOutput(t, fixture.RepoPath, "status", "--porcelain")); got != "" {
		t.Fatalf("fixture repo dirty after generation: %q", got)
	}
}

func TestTinyLedgerFixtureGeneratorCanRegenerateIntoSameParent(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	first, err := cpprobe.GenerateTinyLedger(ctx, parent)
	if err != nil {
		t.Fatalf("first generate tiny ledger fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.RepoPath, "untracked.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale fixture file: %v", err)
	}

	second, err := cpprobe.GenerateTinyLedger(ctx, parent)
	if err != nil {
		t.Fatalf("second generate tiny ledger fixture: %v", err)
	}
	if second.RepoPath != first.RepoPath || second.BaseRef == "" {
		t.Fatalf("second fixture = %+v, want same repo path with base ref", second)
	}
	if _, err := os.Stat(filepath.Join(second.RepoPath, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale fixture file stat err = %v, want removed", err)
	}
	if got := strings.TrimSpace(gitOutput(t, second.RepoPath, "status", "--porcelain")); got != "" {
		t.Fatalf("regenerated fixture repo dirty: %q", got)
	}
}

func TestTinyLedgerVariantFixturesDescribeConcurrentConflictAndResolvedState(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"category":          cpprobe.CategoryOnlyFiles(),
		"transaction_count": cpprobe.TransactionCountOnlyFiles(),
		"resolved":          cpprobe.ResolvedCategoryAndCountFiles(),
	} {
		if len(files) == 0 {
			t.Fatalf("%s files empty", name)
		}
		for _, path := range cpprobe.TinyLedgerPaths() {
			if _, ok := files[path]; name != "transaction_count" && !ok {
				t.Fatalf("%s files missing %s", name, path)
			}
		}
	}
	category := cpprobe.CategoryOnlyFiles()["ledger/ledger.go"]
	transactionCount := cpprobe.TransactionCountOnlyFiles()["ledger/ledger.go"]
	resolved := cpprobe.ResolvedCategoryAndCountFiles()["ledger/ledger.go"]
	if !strings.Contains(category, "Categories map[string]int") || strings.Contains(category, "TransactionCount") {
		t.Fatalf("category variant does not isolate category summary changes")
	}
	if !strings.Contains(transactionCount, "TransactionCount int") || strings.Contains(transactionCount, "Categories map[string]int") {
		t.Fatalf("transaction_count variant does not isolate transaction count changes")
	}
	if !strings.Contains(resolved, "Categories       map[string]int") || !strings.Contains(resolved, "TransactionCount int") {
		t.Fatalf("resolved variant does not combine category and transaction_count behavior")
	}
	if !reflect.DeepEqual(cpprobe.ExpectedResolvedLedgerReport().Categories, map[string]int{"sales": 100, "ops": 40, "services": 25}) {
		t.Fatalf("resolved expected categories = %+v", cpprobe.ExpectedResolvedLedgerReport().Categories)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", fullArgs, err, out)
	}
	return string(out)
}
