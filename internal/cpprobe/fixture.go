package cpprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	ScenarioID         = "CP-PROBE-001"
	TeamID             = "cp-probe-001-manual-service"
	TeamVersion        = 1
	CanonicalBranch    = "main"
	TinyLedgerRepoName = "tiny-ledger-probe"
)

type TinyLedgerFixture struct {
	RepoPath        string       `json:"repo_path"`
	CanonicalBranch string       `json:"canonical_branch"`
	BaseRef         string       `json:"base_ref"`
	ExpectedReport  LedgerReport `json:"expected_report"`
}

type LedgerReport struct {
	Income  int `json:"income"`
	Expense int `json:"expense"`
	Balance int `json:"balance"`
}

func GenerateTinyLedger(ctx context.Context, parentDir string) (TinyLedgerFixture, error) {
	if strings.TrimSpace(parentDir) == "" {
		return TinyLedgerFixture{}, errors.New("tiny ledger fixture parent dir is required")
	}
	repoPath := filepath.Join(parentDir, TinyLedgerRepoName)
	if err := os.RemoveAll(repoPath); err != nil {
		return TinyLedgerFixture{}, fmt.Errorf("reset tiny ledger repo dir: %w", err)
	}
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return TinyLedgerFixture{}, fmt.Errorf("create tiny ledger repo dir: %w", err)
	}
	for path, content := range tinyLedgerFiles() {
		if err := writeFixtureFile(repoPath, path, content); err != nil {
			return TinyLedgerFixture{}, err
		}
	}
	for _, step := range [][]string{
		{"init"},
		{"config", "user.name", "CP Probe"},
		{"config", "user.email", "cp-probe@example.invalid"},
		{"add", "."},
		{"commit", "-m", "initial tiny ledger"},
		{"branch", "-M", CanonicalBranch},
	} {
		if err := runGit(ctx, repoPath, step...); err != nil {
			return TinyLedgerFixture{}, err
		}
	}
	baseRef, err := gitOutput(ctx, repoPath, "rev-parse", "HEAD")
	if err != nil {
		return TinyLedgerFixture{}, err
	}
	return TinyLedgerFixture{
		RepoPath:        repoPath,
		CanonicalBranch: CanonicalBranch,
		BaseRef:         strings.TrimSpace(baseRef),
		ExpectedReport: LedgerReport{
			Income:  125,
			Expense: 40,
			Balance: 85,
		},
	}, nil
}

func tinyLedgerFiles() map[string]string {
	return map[string]string{
		"go.mod": `module tiny-ledger-probe

go 1.22
`,
		"ledger/ledger.go": `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID     int
	Type   string
	Amount int
}

type Summary struct {
	Income  int ` + "`json:\"income\"`" + `
	Expense int ` + "`json:\"expense\"`" + `
	Balance int ` + "`json:\"balance\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 3 || got[0] != "id" || got[1] != "type" || got[2] != "amount" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 3 {
			return nil, fmt.Errorf("line %d: expected 3 columns", line+2)
		}
		id, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, fmt.Errorf("line %d id: %w", line+2, err)
		}
		amount, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, fmt.Errorf("line %d amount: %w", line+2, err)
		}
		switch row[1] {
		case "income", "expense":
		default:
			return nil, fmt.Errorf("line %d type: expected income or expense", line+2)
		}
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	var summary Summary
	for _, tx := range transactions {
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`,
		"ledger/ledger_test.go": `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount\n1,income,100\n2,expense,40\n3,income,25\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	want := Summary{Income: 125, Expense: 40, Balance: 85}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}
`,
		"cmd/ledger-report/main.go": `package main

import (
	"encoding/json"
	"fmt"
	"os"

	"tiny-ledger-probe/ledger"
)

func main() {
	path := "data/transactions.csv"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()
	transactions, err := ledger.Read(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(ledger.Summarize(transactions)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`,
		"data/transactions.csv": `id,type,amount
1,income,100
2,expense,40
3,income,25
`,
		"README.md": `# tiny-ledger-probe

Tiny Ledger is a CP-PROBE-001 fixture repository. Its business rules belong to the acceptance task contract, not to CoordPlane backend code.
`,
	}
}

func writeFixtureFile(root, name, content string) error {
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create fixture file dir %s: %w", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write fixture file %s: %w", name, err)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	if _, err := gitOutput(ctx, dir, args...); err != nil {
		return err
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v failed: %w: %s", fullArgs, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
