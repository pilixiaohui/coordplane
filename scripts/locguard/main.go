package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
)

func main() {
	checkSelfTest()
	if len(os.Args) == 3 && os.Args[1] == "--live-integration" {
		file := parseFile(token.NewFileSet(), os.Args[2], nil)
		if !liveIntegrationWiring(file) {
			panic("live integration wiring guard failed")
		}
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		path := scanner.Text()
		set := token.NewFileSet()
		file := parseFile(set, path, nil)
		for _, line := range statementLines(set, file) {
			fmt.Printf("%s:%d\n", path, line)
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func liveIntegrationWiring(file *ast.File) bool {
	function := file.Scope.Lookup("TestRealClaudeTwoAgentConvergence").Decl.(*ast.FuncDecl)
	helperCalls := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && callName(call) == "waitForLiveIntegration" {
			helperCalls++
		}
		return true
	})
	integrationAssignments, validAssignments := 0, 0
	for _, statement := range function.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			continue
		}
		target, ok := assignment.Lhs[0].(*ast.Ident)
		if !ok || target.Name != "integration" {
			continue
		}
		integrationAssignments++
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || assignment.Tok != token.DEFINE || callName(call) != "waitForLiveIntegration" || len(call.Args) != 9 {
			continue
		}
		tracker, trackerOK := call.Args[4].(*ast.Ident)
		source, sourceOK := call.Args[5].(*ast.Ident)
		if trackerOK && sourceOK && tracker.Name == "trackFailure" && source.Name == "taskB" {
			validAssignments++
		}
	}
	return helperCalls == 1 && integrationAssignments == 1 && validAssignments == 1
}

func callName(call *ast.CallExpr) string {
	if callee, ok := call.Fun.(*ast.Ident); ok {
		return callee.Name
	}
	return ""
}

func statementLines(set *token.FileSet, node ast.Node) []int {
	seen := map[int]bool{}
	check := func(statements []ast.Stmt) {
		for index := 1; index < len(statements); index++ {
			line := set.Position(statements[index].Pos()).Line
			if line == set.Position(statements[index-1].End()).Line {
				seen[line] = true
			}
		}
	}
	ast.Inspect(node, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.BlockStmt:
			check(value.List)
		case *ast.CaseClause:
			check(value.Body)
		case *ast.CommClause:
			check(value.Body)
		}
		return true
	})
	lines := make([]int, 0, len(seen))
	for line := range seen {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	return lines
}

func checkSelfTest() {
	for source, want := range map[string]bool{
		"func f(){a();b()}": true, "func f(){v:=struct{}{};_ = v}": true,
		"func f(){\na()\nb()\n}": false, "func f(){for i:=0;i<1;i++ {}}": false,
		"func f(){if x:=g();x {}}": false, "func f(){switch x:=g();x {case true:}}": false,
	} {
		set := token.NewFileSet()
		if (len(statementLines(set, parseFile(set, "self.go", "package p\n"+source))) > 0) != want {
			panic("locguard self-test failed")
		}
	}
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{name: "production", body: `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)`, want: true},
		{name: "inline direct", body: `integration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)`},
		{name: "no-op tracker", body: `integration := waitForLiveIntegration(a, b, c, d, func(...string) {}, taskB, e, f, g)`},
		{name: "alternate tracker", body: `integration := waitForLiveIntegration(a, b, c, d, otherTracker, taskB, e, f, g)`},
		{name: "wrong source", body: `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskA, e, f, g)`},
		{name: "dead helper", body: "if false { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\nintegration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)"},
		{name: "closure helper", body: "helper := func() { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\n_ = helper\nintegrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)"},
		{name: "integration alias", body: "integrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)"},
	} {
		source := "package p\nfunc TestRealClaudeTwoAgentConvergence() {\n" + test.body + "\n}"
		if got := liveIntegrationWiring(parseFile(token.NewFileSet(), test.name, source)); got != test.want {
			panic("live integration self-test failed: " + test.name)
		}
	}
}

func parseFile(set *token.FileSet, name string, source any) *ast.File {
	file, err := parser.ParseFile(set, name, source, 0)
	if err != nil {
		panic(err)
	}
	return file
}
