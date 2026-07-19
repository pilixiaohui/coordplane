package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
)

func main() {
	checkSelfTest()
	if len(os.Args) == 3 && os.Args[1] == "--live-integration" {
		if !liveIntegrationWiring(parseFile(token.NewFileSet(), os.Args[2], nil)) {
			panic("live integration wiring guard failed")
		}
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		for _, line := range statementLines(scanner.Text(), nil) {
			fmt.Printf("%s:%d\n", scanner.Text(), line)
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func liveIntegrationWiring(file *ast.File) bool {
	function := file.Scope.Lookup("TestRealClaudeTwoAgentConvergence").Decl.(*ast.FuncDecl)
	integrations, helperCalls, trackerUses, integrationPos := 0, 0, 0, token.NoPos
	var trackerObject *ast.Object
	for _, statement := range function.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			continue
		}
		call, callOK := assignment.Rhs[0].(*ast.CallExpr)
		switch identifierName(assignment.Lhs[0]) {
		case "trackFailure":
			if trackerObject != nil || !callOK || assignment.Tok != token.DEFINE || identifierName(call.Fun) != "registerLiveFailureDiagnostics" {
				return false
			}
			trackerObject = assignment.Lhs[0].(*ast.Ident).Obj
		case "integration":
			integrations++
			integrationPos = assignment.Pos()
			if !callOK || assignment.Tok != token.DEFINE || identifierName(call.Fun) != "waitForLiveIntegration" || len(call.Args) != 9 {
				return false
			}
			if identifierName(call.Args[4]) != "trackFailure" || identifierName(call.Args[5]) != "taskB" {
				return false
			}
		}
	}
	// Before the wait, only direct tracker calls may consume the canonical binding.
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if identifierName(call.Fun) == "waitForLiveIntegration" {
				helperCalls++
			}
			if callee, ok := call.Fun.(*ast.Ident); ok && call.Pos() < integrationPos && callee.Obj == trackerObject {
				trackerUses--
			}
		}
		if identifier, ok := node.(*ast.Ident); ok && identifier.Pos() < integrationPos && identifier.Obj == trackerObject {
			trackerUses++
		}
		return true
	})
	return helperCalls == 1 && integrations == 1 && trackerObject != nil && trackerUses == 1
}

func identifierName(expression ast.Expr) string {
	if identifier, ok := expression.(*ast.Ident); ok {
		return identifier.Name
	}
	return ""
}

func statementLines(name string, source any) (lines []int) {
	set := token.NewFileSet()
	check := func(statements []ast.Stmt) {
		for index := 1; index < len(statements); index++ {
			if set.Position(statements[index].Pos()).Line == set.Position(statements[index-1].End()).Line {
				lines = append(lines, set.Position(statements[index].Pos()).Line)
			}
		}
	}
	ast.Inspect(parseFile(set, name, source), func(node ast.Node) bool {
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
	slices.Sort(lines)
	return slices.Compact(lines)
}

func checkSelfTest() {
	for source, want := range map[string]bool{
		"func f(){a();b()}": true, "func f(){v:=struct{}{};_ = v}": true,
		"func f(){\na()\nb()\n}": false, "func f(){for i:=0;i<1;i++ {}}": false,
		"func f(){if x:=g();x {}}": false, "func f(){switch x:=g();x {case true:}}": false,
	} {
		if (len(statementLines("self.go", "package p\n"+source)) > 0) != want {
			panic("locguard self-test failed")
		}
	}
	const trackerBinding = "trackFailure := registerLiveFailureDiagnostics(a)\n"
	for _, test := range []struct {
		name, body string
		want       bool
	}{
		{name: "production", body: trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)`, want: true},
		{name: "inline direct", body: trackerBinding + `integration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)`},
		{name: "no-op tracker", body: trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, func(...string) {}, taskB, e, f, g)`},
		{name: "alternate tracker", body: trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, otherTracker, taskB, e, f, g)`},
		{name: "wrong source", body: trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskA, e, f, g)`},
		{name: "dead helper", body: trackerBinding + "if false { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\nintegration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)"},
		{name: "closure helper", body: trackerBinding + "helper := func() { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\n_ = helper\nintegrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)"},
		{name: "integration alias", body: trackerBinding + "integrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)"},
		{name: "same-name no-op binding", body: "trackFailure := func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)"},
		{name: "tracker reassignment", body: trackerBinding + "trackFailure = func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)"},
		{name: "indirect tracker reassignment", body: trackerBinding + "*(&trackFailure) = func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)"},
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
