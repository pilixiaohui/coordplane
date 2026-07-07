package cpprobe

type ResolvedLedgerReport struct {
	Income           int            `json:"income"`
	Expense          int            `json:"expense"`
	Balance          int            `json:"balance"`
	TransactionCount int            `json:"transaction_count"`
	Categories       map[string]int `json:"categories"`
}

func TinyLedgerPaths() []string {
	return []string{
		"ledger/ledger.go",
		"ledger/ledger_test.go",
		"cmd/ledger-report/main.go",
		"data/transactions.csv",
	}
}

func CategoryOnlyFiles() map[string]string {
	return cloneFiles(map[string]string{
		"ledger/ledger.go":          categoryOnlyLedgerGo,
		"ledger/ledger_test.go":     categoryOnlyLedgerTestGo,
		"cmd/ledger-report/main.go": categoryOnlyMainGo,
		"data/transactions.csv":     categorizedTransactionsCSV,
	})
}

func TransactionCountOnlyFiles() map[string]string {
	return cloneFiles(map[string]string{
		"ledger/ledger.go":          transactionCountOnlyLedgerGo,
		"ledger/ledger_test.go":     transactionCountOnlyLedgerTestGo,
		"cmd/ledger-report/main.go": transactionCountOnlyMainGo,
	})
}

func ResolvedCategoryAndCountFiles() map[string]string {
	return cloneFiles(map[string]string{
		"ledger/ledger.go":          resolvedLedgerGo,
		"ledger/ledger_test.go":     resolvedLedgerTestGo,
		"cmd/ledger-report/main.go": transactionCountOnlyMainGo,
		"data/transactions.csv":     categorizedTransactionsCSV,
	})
}

func ExpectedResolvedLedgerReport() ResolvedLedgerReport {
	return ResolvedLedgerReport{
		Income:           125,
		Expense:          40,
		Balance:          85,
		TransactionCount: 3,
		Categories: map[string]int{
			"sales":    100,
			"ops":      40,
			"services": 25,
		},
	}
}

func cloneFiles(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for path, content := range in {
		out[path] = content
	}
	return out
}

const categorizedTransactionsCSV = `id,type,amount,category
1,income,100,sales
2,expense,40,ops
3,income,25,services
`

const categoryOnlyLedgerGo = `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID       int
	Type     string
	Amount   int
	Category string
}

type Summary struct {
	Income     int            ` + "`json:\"income\"`" + `
	Expense    int            ` + "`json:\"expense\"`" + `
	Balance    int            ` + "`json:\"balance\"`" + `
	Categories map[string]int ` + "`json:\"categories\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 4 || got[0] != "id" || got[1] != "type" || got[2] != "amount" || got[3] != "category" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 columns", line+2)
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
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount, Category: row[3]})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	summary := Summary{Categories: map[string]int{}}
	for _, tx := range transactions {
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
		if tx.Category != "" {
			summary.Categories[tx.Category] += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`

const categoryOnlyLedgerTestGo = `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount,category\n1,income,100,sales\n2,expense,40,ops\n3,income,25,services\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	if got.Income != 125 || got.Expense != 40 || got.Balance != 85 {
		t.Fatalf("summary totals = %+v", got)
	}
	if got.Categories["sales"] != 100 || got.Categories["ops"] != 40 || got.Categories["services"] != 25 {
		t.Fatalf("categories = %+v", got.Categories)
	}
}
`

const categoryOnlyMainGo = `package main

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
`

const transactionCountOnlyLedgerGo = `package ledger

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
	Income           int ` + "`json:\"income\"`" + `
	Expense          int ` + "`json:\"expense\"`" + `
	Balance          int ` + "`json:\"balance\"`" + `
	TransactionCount int ` + "`json:\"transaction_count\"`" + `
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
		summary.TransactionCount++
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
`

const transactionCountOnlyLedgerTestGo = `package ledger

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
	want := Summary{Income: 125, Expense: 40, Balance: 85, TransactionCount: 3}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}
`

const transactionCountOnlyMainGo = `package main

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
	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(ledger.Summarize(transactions)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

const resolvedLedgerGo = `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID       int
	Type     string
	Amount   int
	Category string
}

type Summary struct {
	Income           int            ` + "`json:\"income\"`" + `
	Expense          int            ` + "`json:\"expense\"`" + `
	Balance          int            ` + "`json:\"balance\"`" + `
	TransactionCount int            ` + "`json:\"transaction_count\"`" + `
	Categories       map[string]int ` + "`json:\"categories\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 4 || got[0] != "id" || got[1] != "type" || got[2] != "amount" || got[3] != "category" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 columns", line+2)
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
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount, Category: row[3]})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	summary := Summary{Categories: map[string]int{}}
	for _, tx := range transactions {
		summary.TransactionCount++
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
		if tx.Category != "" {
			summary.Categories[tx.Category] += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`

const resolvedLedgerTestGo = `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount,category\n1,income,100,sales\n2,expense,40,ops\n3,income,25,services\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	if got.Income != 125 || got.Expense != 40 || got.Balance != 85 || got.TransactionCount != 3 {
		t.Fatalf("summary totals/count = %+v", got)
	}
	if got.Categories["sales"] != 100 || got.Categories["ops"] != 40 || got.Categories["services"] != 25 {
		t.Fatalf("categories = %+v", got.Categories)
	}
}
`
