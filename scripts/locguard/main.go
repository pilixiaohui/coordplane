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
	if len(os.Args) == 3 && os.Args[1] == "--live-integration" {
		file, err := parser.ParseFile(token.NewFileSet(), os.Args[2], nil, 0)
		if err != nil || !liveIntegrationWiring(file) {
			panic("live integration wiring guard failed")
		}
		return
	}
	checkSelfTest()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		path := scanner.Text()
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			panic(err)
		}
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
	tracked, direct := 0, 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if callee.Name == "waitForLiveIntegration" {
			tracked++
		}
		if callee.Name != "waitForTaskWithin" {
			return true
		}
		for _, argument := range call.Args {
			selector, ok := argument.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "IntegrationTaskID" {
				direct++
			}
		}
		return true
	})
	return tracked == 1 && direct == 0
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
	cases := map[string]bool{
		"func f(){a();b()}": true, "func f(){v:=struct{}{};_ = v}": true,
		"func f(){\na()\nb()\n}": false, "func f(){for i:=0;i<1;i++ {}}": false,
		"func f(){if x:=g();x {}}": false, "func f(){switch x:=g();x {case true:}}": false,
	}
	for source, want := range cases {
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, "self.go", "package p\n"+source, 0)
		if err != nil || (len(statementLines(set, file)) > 0) != want {
			panic("locguard self-test failed")
		}
	}
}
