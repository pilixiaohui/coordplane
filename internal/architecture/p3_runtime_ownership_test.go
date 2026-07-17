package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"coordplane/internal/adapter"
)

func TestP3RuntimeAndAdapterCannotOwnDatabaseOrGit(t *testing.T) {
	var offenders []string
	for _, source := range productionSources(t, "internal/runtime", "internal/adapter") {
		for _, specification := range source.file.Imports {
			path, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				t.Fatalf("decode import in %s: %v", source.rel, err)
			}
			lower := strings.ToLower(path)
			if path == "database/sql" || strings.HasPrefix(path, "coordplane/internal/store") ||
				strings.HasPrefix(path, "coordplane/internal/gitrepo") || strings.Contains(lower, "sqlite") {
				offenders = append(offenders, source.rel+": "+path)
			}
		}
	}
	failOffenders(t, "Runtime/adapters cannot own SQLite or Git", offenders)
}

func TestP3LegacyRuntimeProductsCannotReturn(t *testing.T) {
	var offenders []string
	for _, source := range productionSources(t) {
		boundary := strings.HasPrefix(source.rel, "internal/runtime/") ||
			strings.HasPrefix(source.rel, "internal/adapter/")
		if legacyRuntimeWords(sourceWords(source.rel)) || boundary && forbiddenRuntimeEntry(sourceWords(source.rel)) {
			offenders = append(offenders, source.rel+": legacy path")
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				words := sourceWords(typed.Name)
				if legacyRuntimeWords(words) || boundary && forbiddenRuntimeEntry(words) {
					offenders = append(offenders, source.rel+": identifier "+typed.Name)
				}
			case *ast.BasicLit:
				if typed.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(typed.Value)
				if err == nil && (legacyRuntimeLiteral(value) || boundary && forbiddenRuntimeEntryName(value)) {
					offenders = append(offenders, source.rel+": literal "+strconv.Quote(value))
				}
			}
			return true
		})
	}
	failOffenders(t, "legacy runtime products, writers, services, or tables returned", offenders)
}

func TestP3ProductionAdapterRegistryIsOneStaticOneShotList(t *testing.T) {
	sources := productionSources(t, "internal/adapter")
	listName, entries := adapterList(t, sources)
	if entries != 1 {
		t.Fatalf("compile-time production adapter list %q contains %d entries", listName, entries)
	}
	foundProductionUse := false
	var offenders []string
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if function.Recv == nil && function.Name.Name == "Production" {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					identifier, ok := node.(*ast.Ident)
					foundProductionUse = foundProductionUse || ok && identifier.Name == listName
					return true
				})
			}
			if exportedAdapterRegistration(function) {
				offenders = append(offenders, source.rel+": "+function.Name.Name)
			}
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for _, target := range typed.Lhs {
					if expressionRoot(target) == listName {
						offenders = append(offenders, source.rel+": adapter list assignment")
					}
				}
			case *ast.CallExpr:
				function, ok := typed.Fun.(*ast.Ident)
				if ok && function.Name == "append" && len(typed.Args) > 0 && expressionRoot(typed.Args[0]) == listName {
					offenders = append(offenders, source.rel+": adapter list append")
				}
			case *ast.TypeSpec:
				if ast.IsExported(typed.Name.Name) && strings.Contains(strings.ToLower(typed.Name.Name), "registration") {
					offenders = append(offenders, source.rel+": "+typed.Name.Name)
				}
				if typed.Name.Name == "Registry" {
					structure, _ := typed.Type.(*ast.StructType)
					if structure != nil {
						for _, field := range structure.Fields.List {
							for _, name := range field.Names {
								if ast.IsExported(name.Name) {
									offenders = append(offenders, source.rel+": Registry."+name.Name)
								}
							}
						}
					}
				}
			case *ast.ValueSpec:
				for index, name := range typed.Names {
					if ast.IsExported(name.Name) && valueIsCLIList(typed, index) {
						offenders = append(offenders, source.rel+": "+name.Name)
					}
				}
			}
			return true
		})
	}
	if !foundProductionUse {
		offenders = append(offenders, "Production does not use "+listName)
	}
	registry := adapter.Production()
	names := registry.Names()
	if len(names) != 1 || names[0] != "claude" {
		t.Fatalf("production registry names = %v, want [claude]", names)
	}
	production, ok := registry.Lookup(names[0])
	if !ok || production.Metadata().ExecutionModel != adapter.ExecutionOneShot {
		t.Fatalf("production adapter %q is not one-shot", names[0])
	}
	known := stringSet(names...)
	for _, source := range productionSources(t, "internal/runtime", "internal/daemon") {
		ast.Inspect(source.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && literal.Kind == token.STRING {
				value, err := strconv.Unquote(literal.Value)
				if err == nil && known[value] {
					offenders = append(offenders, source.rel+": hard-coded adapter "+value)
				}
			}
			return true
		})
	}
	failOffenders(t, "production adapter registry is mutable or exposes registration", offenders)
}

func TestP3DockerTagSelectsRuntimeIntegrationTest(t *testing.T) {
	root := repositoryRoot(t)
	docker := build.Default
	docker.BuildTags = append(append([]string(nil), docker.BuildTags...), "docker")
	found := false
	err := filepath.WalkDir(filepath.Join(root, "internal", "runtime"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), "_integration_test.go") {
			return err
		}
		defaultMatch, err := build.Default.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil {
			return err
		}
		dockerMatch, err := docker.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil || defaultMatch || !dockerMatch {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(function.Name.Name, "TestDocker") {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				found = found || ok && strings.HasPrefix(identifier.Name, "NewDockerExecutor")
				return !found
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("docker tag selects no TestDocker function that exercises DockerExecutor")
	}
}

func TestP3EvidenceFreeRunWritersCannotReturn(t *testing.T) {
	forbidden := stringSet("ActivateRun", "InterruptRun", "RecordRunTerminal")
	var offenders []string
	for _, source := range productionSources(t) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if forbidden[typed.Name.Name] {
					offenders = append(offenders, source.rel+": declaration "+typed.Name.Name)
				}
			case *ast.SelectorExpr:
				if forbidden[typed.Sel.Name] {
					offenders = append(offenders, source.rel+": call "+typed.Sel.Name)
				}
			}
			return true
		})
	}
	failOffenders(t, "evidence-free Run writers returned", offenders)
}

func legacyRuntimeWords(words []string) bool {
	provider, audit := false, false
	for index, word := range words {
		provider = provider || word == "provider"
		audit = audit || word == "audit"
		if word == "transcript" || word == "commandrun" || word == "provideraudit" ||
			index+1 < len(words) && word == "command" && words[index+1] == "run" {
			return true
		}
	}
	return provider && audit
}

func forbiddenRuntimeEntry(words []string) bool {
	entries := stringSet("external", "host", "fake", "scripted")
	roles := stringSet(
		"adapter", "backend", "cli", "executor", "provider",
		"registration", "registry", "runner", "runtime",
	)
	hasEntry, hasRole := false, false
	for _, word := range words {
		hasEntry = hasEntry || entries[word]
		hasRole = hasRole || roles[word]
	}
	return hasEntry && hasRole
}

func forbiddenRuntimeEntryName(value string) bool {
	words := sourceWords(strings.TrimSpace(value))
	if len(words) == 1 {
		return stringSet("external", "host", "fake", "scripted")[words[0]]
	}
	return len(words) > 1 && stringSet("external", "host", "fake", "scripted")[words[0]]
}

func legacyRuntimeLiteral(value string) bool {
	lower := strings.ToLower(value)
	if legacyRuntimeWords(sourceWords(value)) {
		return true
	}
	for _, token := range []string{
		"provider_audit", "provider_tool_outcomes", "command.run", "command_runs",
		"transcript_ref", "transcripts", "runtime_instances", "cli_sessions", "runtime_tokens",
	} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func adapterList(t *testing.T, sources []productionSource) (string, int) {
	t.Helper()
	type candidate struct {
		name  string
		count int
	}
	var candidates []candidate
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				values := specification.(*ast.ValueSpec)
				for index, value := range values.Values {
					literal, ok := value.(*ast.CompositeLit)
					if ok && cliListType(literal.Type) && index < len(values.Names) {
						candidates = append(candidates, candidate{values.Names[index].Name, len(literal.Elts)})
					}
				}
			}
		}
	}
	if len(candidates) != 1 || ast.IsExported(candidates[0].name) {
		t.Fatalf("compile-time unexported []CLI lists = %#v", candidates)
	}
	return candidates[0].name, candidates[0].count
}

func exportedAdapterRegistration(function *ast.FuncDecl) bool {
	if !ast.IsExported(function.Name.Name) {
		return false
	}
	receiver := receiverName(function)
	if receiver == "Registry" {
		return function.Name.Name != "Lookup" && function.Name.Name != "Names"
	}
	lower := strings.ToLower(function.Name.Name)
	return strings.Contains(lower, "register") || strings.Contains(lower, "registration") ||
		strings.HasPrefix(function.Name.Name, "New") && strings.Contains(lower, "registry") ||
		function.Name.Name != "Production" && functionAcceptsCLI(function.Type)
}

func valueIsCLIList(specification *ast.ValueSpec, index int) bool {
	if cliListType(specification.Type) {
		return true
	}
	if index >= len(specification.Values) {
		return false
	}
	literal, ok := specification.Values[index].(*ast.CompositeLit)
	return ok && cliListType(literal.Type)
}

func cliListType(expression ast.Expr) bool {
	array, ok := expression.(*ast.ArrayType)
	identifier, elementOK := ast.Unparen(arrayElement(array)).(*ast.Ident)
	return ok && elementOK && identifier.Name == "CLI"
}

func arrayElement(array *ast.ArrayType) ast.Expr {
	if array == nil {
		return nil
	}
	return array.Elt
}

func functionAcceptsCLI(function *ast.FuncType) bool {
	if function == nil || function.Params == nil {
		return false
	}
	for _, field := range function.Params.List {
		if expressionContainsCLI(field.Type) {
			return true
		}
	}
	return false
}

func expressionContainsCLI(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name == "CLI"
	case *ast.ArrayType:
		return expressionContainsCLI(typed.Elt)
	case *ast.Ellipsis:
		return expressionContainsCLI(typed.Elt)
	case *ast.StarExpr:
		return expressionContainsCLI(typed.X)
	}
	return false
}

func expressionRoot(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return expressionRoot(typed.X)
	case *ast.IndexListExpr:
		return expressionRoot(typed.X)
	case *ast.SelectorExpr:
		return expressionRoot(typed.X)
	}
	return ""
}

func receiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	expression := function.Recv.List[0].Type
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}
