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
	helperCalls, trackerUses, trackerCalls, integrationPos := 0, 0, 0, token.NoPos
	var trackerObject *ast.Object
	for _, statement := range function.Body.List {
		switch statement := statement.(type) {
		case *ast.ExprStmt:
			call, _ := statement.X.(*ast.CallExpr)
			if call != nil && trackerObject != nil && integrationPos == token.NoPos &&
				identifierName(call.Fun) == "trackFailure" && call.Fun.(*ast.Ident).Obj == trackerObject {
				trackerCalls++
			}
		case *ast.AssignStmt:
			if len(statement.Lhs) != 1 || len(statement.Rhs) != 1 {
				continue
			}
			call, callOK := statement.Rhs[0].(*ast.CallExpr)
			switch identifierName(statement.Lhs[0]) {
			case "trackFailure":
				if trackerObject != nil || !callOK || statement.Tok != token.DEFINE || identifierName(call.Fun) != "registerLiveFailureDiagnostics" {
					return false
				}
				trackerObject = statement.Lhs[0].(*ast.Ident).Obj
			case "integration":
				if integrationPos != token.NoPos || !callOK || statement.Tok != token.DEFINE || identifierName(call.Fun) != "waitForLiveIntegration" || len(call.Args) != 9 ||
					identifierName(call.Args[4]) != "trackFailure" || identifierName(call.Args[5]) != "taskB" {
					return false
				}
				integrationPos = statement.Pos()
			}
		}
	}
	// Before the wait, only direct tracker calls in Body.List may consume the binding.
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && identifierName(call.Fun) == "waitForLiveIntegration" {
			helperCalls++
		}
		if identifier, ok := node.(*ast.Ident); ok && identifier.Pos() < integrationPos && identifier.Obj == trackerObject {
			trackerUses++
		}
		return true
	})
	return helperCalls == 1 && integrationPos != token.NoPos && trackerObject != nil && trackerUses == trackerCalls+1
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
	for name, body := range map[string]string{
		"production":                    trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)`,
		"inline direct":                 trackerBinding + `integration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)`,
		"no-op tracker":                 trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, func(...string) {}, taskB, e, f, g)`,
		"alternate tracker":             trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, otherTracker, taskB, e, f, g)`,
		"wrong source":                  trackerBinding + `integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskA, e, f, g)`,
		"dead helper":                   trackerBinding + "if false { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\nintegration := waitForTaskWithin(a, b, c, d, taskB.IntegrationTaskID, e, f, g)",
		"closure helper":                trackerBinding + "helper := func() { integration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g) }\n_ = helper\nintegrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)",
		"integration alias":             trackerBinding + "integrationID := taskB.IntegrationTaskID\nintegration := waitForTaskWithin(a, b, c, d, integrationID, e, f, g)",
		"same-name no-op binding":       "trackFailure := func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
		"tracker reassignment":          trackerBinding + "trackFailure = func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
		"indirect tracker reassignment": trackerBinding + "*(&trackFailure) = func(...string) {}\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
		"closure direct tracker call":   trackerBinding + "_ = func() { trackFailure(a) }\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
		"defer tracker call":            trackerBinding + "defer trackFailure(a)\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
		"go tracker call":               trackerBinding + "go trackFailure(a)\nintegration := waitForLiveIntegration(a, b, c, d, trackFailure, taskB, e, f, g)",
	} {
		source := "package p\nfunc TestRealClaudeTwoAgentConvergence() {\n" + body + "\n}"
		if got := liveIntegrationWiring(parseFile(token.NewFileSet(), name, source)); got != (name == "production") {
			panic("live integration self-test failed: " + name)
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
