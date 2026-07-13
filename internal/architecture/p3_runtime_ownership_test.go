package architecture_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"coordplane/internal/adapter"
)

func TestP3RuntimeAndAdapterCannotOwnDatabaseOrGit(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	for _, sourceRoot := range []string{"internal/runtime", "internal/adapter"} {
		walkP3ProductionGo(t, root, sourceRoot, func(rel string, file *ast.File) {
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("decode import in %s: %v", rel, err)
				}
				if forbiddenRuntimeOwnerImport(importPath) {
					offenders = append(offenders, rel+": "+importPath)
				}
			}
		})
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("Runtime/adapters must report facts through Core and cannot own SQLite or Git:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestP3LegacyRuntimeProductsCannotReturn(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	walkP3ProductionGo(t, root, "", func(rel string, file *ast.File) {
		inRuntimeBoundary := strings.HasPrefix(rel, "internal/runtime/") || strings.HasPrefix(rel, "internal/adapter/")
		pathWords := sourceWords(filepath.ToSlash(rel))
		if hasLegacyRuntimeConcept(pathWords) || (inRuntimeBoundary && hasForbiddenRuntimeEntry(pathWords)) {
			offenders = append(offenders, rel+": legacy path")
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				words := sourceWords(typed.Name)
				if hasLegacyRuntimeConcept(words) || (inRuntimeBoundary && hasForbiddenRuntimeEntry(words)) {
					offenders = append(offenders, rel+": identifier "+typed.Name)
				}
			case *ast.BasicLit:
				if typed.Kind != token.STRING {
					break
				}
				value, err := strconv.Unquote(typed.Value)
				if err != nil {
					break
				}
				if legacyRuntimeLiteral(value) || (inRuntimeBoundary && forbiddenRuntimeEntryName(value)) {
					offenders = append(offenders, rel+": legacy literal "+strconv.Quote(value))
				}
			}
			return true
		})
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		offenders = compactStrings(offenders)
		t.Fatalf("legacy runtime products, writers, services, or tables returned:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestP3ProductionAdapterRegistryIsOneStaticOneShotList(t *testing.T) {
	root := repositoryRoot(t)
	listName, entryCount := staticAdapterList(t, root)
	if entryCount != 1 {
		t.Fatalf("compile-time production adapter list %q contains %d entries, want exactly one", listName, entryCount)
	}
	assertStaticAdapterListIsImmutable(t, root, listName)
	assertNoPublicAdapterRegistrationAPI(t, root)

	registry := adapter.Production()
	names := registry.Names()
	if len(names) != 1 {
		t.Fatalf("production registry names = %v, want exactly one", names)
	}
	production, ok := registry.Lookup(names[0])
	if !ok || production == nil {
		t.Fatalf("production registry cannot resolve its only name %q", names[0])
	}
	if metadata := production.Metadata(); metadata.ExecutionModel != adapter.ExecutionOneShot {
		t.Fatalf("production adapter %q execution model = %q, want %q", names[0], metadata.ExecutionModel, adapter.ExecutionOneShot)
	}

	assertRunnerHasNoAdapterNameCases(t, root, names)
}

func TestP3DockerTagSelectsRuntimeIntegrationTest(t *testing.T) {
	root := repositoryRoot(t)
	runtimeRoot := filepath.Join(root, "internal", "runtime")
	defaultContext := build.Default
	dockerContext := build.Default
	dockerContext.BuildTags = append(append([]string(nil), dockerContext.BuildTags...), "docker")

	var selected []string
	err := filepath.WalkDir(runtimeRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_integration_test.go") {
			return nil
		}
		defaultMatch, err := defaultContext.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		dockerMatch, err := dockerContext.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		if defaultMatch || !dockerMatch {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		if hasDockerIntegrationTest(parsed) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			selected = append(selected, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) == 0 {
		t.Fatal("the docker build tag selects no Runtime integration test that exercises DockerExecutor")
	}
}

func TestP3EvidenceFreeRunWritersCannotReturn(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := map[string]bool{
		"ActivateRun":       true,
		"InterruptRun":      true,
		"RecordRunTerminal": true,
	}
	var offenders []string
	walkP3ProductionGo(t, root, "", func(rel string, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if forbidden[typed.Name.Name] {
					offenders = append(offenders, rel+": declaration "+typed.Name.Name)
				}
			case *ast.SelectorExpr:
				if forbidden[typed.Sel.Name] {
					offenders = append(offenders, rel+": call "+typed.Sel.Name)
				}
			}
			return true
		})
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("evidence-free Run writers returned; use fenced lifecycle ingress:\n%s", strings.Join(compactStrings(offenders), "\n"))
	}
}

func forbiddenRuntimeOwnerImport(importPath string) bool {
	if importPath == "database/sql" || importPath == "coordplane/internal/store" ||
		strings.HasPrefix(importPath, "coordplane/internal/store/") || importPath == "coordplane/internal/gitrepo" ||
		strings.HasPrefix(importPath, "coordplane/internal/gitrepo/") {
		return true
	}
	for _, segment := range strings.Split(strings.ToLower(importPath), "/") {
		if strings.Contains(segment, "sqlite") {
			return true
		}
	}
	return false
}

func hasLegacyRuntimeConcept(words []string) bool {
	hasProvider, hasAudit := false, false
	for index, word := range words {
		hasProvider = hasProvider || word == "provider"
		hasAudit = hasAudit || word == "audit"
		if word == "transcript" || word == "commandrun" || word == "provideraudit" {
			return true
		}
		if index+1 < len(words) && ((word == "provider" && words[index+1] == "audit") ||
			(word == "command" && words[index+1] == "run")) {
			return true
		}
	}
	return hasProvider && hasAudit
}

func hasForbiddenRuntimeEntry(words []string) bool {
	entryWords := map[string]bool{"external": true, "host": true, "fake": true, "scripted": true}
	roleWords := map[string]bool{
		"adapter": true, "backend": true, "cli": true, "executor": true, "provider": true,
		"registration": true, "registry": true, "runner": true, "runtime": true,
	}
	hasEntry, hasRole := false, false
	for _, word := range words {
		hasEntry = hasEntry || entryWords[word]
		hasRole = hasRole || roleWords[word]
	}
	return hasEntry && hasRole
}

func forbiddenRuntimeEntryName(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "external" || value == "host" || value == "fake" || value == "scripted" {
		return true
	}
	words := sourceWords(value)
	return len(words) > 1 && (words[0] == "external" || words[0] == "host" || words[0] == "fake" || words[0] == "scripted")
}

func legacyRuntimeLiteral(value string) bool {
	lower := strings.ToLower(value)
	words := sourceWords(value)
	hasProvider, hasAudit, hasTranscript := false, false, false
	for _, word := range words {
		hasProvider = hasProvider || word == "provider"
		hasAudit = hasAudit || word == "audit"
		hasTranscript = hasTranscript || word == "transcript"
	}
	if hasTranscript || (hasProvider && hasAudit) {
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

func staticAdapterList(t *testing.T, root string) (string, int) {
	t.Helper()
	type candidate struct {
		name  string
		count int
	}
	var candidates []candidate
	walkP3ProductionGo(t, root, "internal/adapter", func(_ string, file *ast.File) {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				values := specification.(*ast.ValueSpec)
				for index, value := range values.Values {
					literal, ok := value.(*ast.CompositeLit)
					if !ok || !isCLIListType(literal.Type) || index >= len(values.Names) {
						continue
					}
					candidates = append(candidates, candidate{name: values.Names[index].Name, count: len(literal.Elts)})
				}
			}
		}
	})
	if len(candidates) != 1 {
		t.Fatalf("found %d compile-time []CLI adapter lists, want exactly one", len(candidates))
	}
	if ast.IsExported(candidates[0].name) {
		t.Fatalf("production adapter list %q must not be public", candidates[0].name)
	}
	if !productionFunctionUsesList(t, root, candidates[0].name) {
		t.Fatalf("Production does not use compile-time adapter list %q", candidates[0].name)
	}
	return candidates[0].name, candidates[0].count
}

func isCLIListType(expression ast.Expr) bool {
	array, ok := expression.(*ast.ArrayType)
	if !ok {
		return false
	}
	element, ok := array.Elt.(*ast.Ident)
	return ok && element.Name == "CLI"
}

func productionFunctionUsesList(t *testing.T, root, listName string) bool {
	t.Helper()
	found := false
	walkP3ProductionGo(t, root, "internal/adapter", func(_ string, file *ast.File) {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Name.Name != "Production" || function.Type.Params.NumFields() != 0 {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok && identifier.Name == listName {
					found = true
				}
				return !found
			})
		}
	})
	return found
}

func assertStaticAdapterListIsImmutable(t *testing.T, root, listName string) {
	t.Helper()
	var offenders []string
	walkP3ProductionGo(t, root, "internal/adapter", func(rel string, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for _, target := range typed.Lhs {
					if expressionRootName(target) == listName {
						offenders = append(offenders, rel+": assignment")
					}
				}
			case *ast.CallExpr:
				function, ok := typed.Fun.(*ast.Ident)
				if ok && function.Name == "append" && len(typed.Args) > 0 && expressionRootName(typed.Args[0]) == listName {
					offenders = append(offenders, rel+": append")
				}
			}
			return true
		})
	})
	if len(offenders) > 0 {
		t.Fatalf("compile-time adapter list %q is mutated: %s", listName, strings.Join(compactStrings(offenders), ", "))
	}
}

func assertNoPublicAdapterRegistrationAPI(t *testing.T, root string) {
	t.Helper()
	var offenders []string
	walkP3ProductionGo(t, root, "internal/adapter", func(rel string, file *ast.File) {
		for _, declaration := range file.Decls {
			switch typed := declaration.(type) {
			case *ast.GenDecl:
				if typed.Tok != token.TYPE && typed.Tok != token.VAR {
					continue
				}
				for _, specification := range typed.Specs {
					switch named := specification.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(named.Name.Name) && strings.Contains(strings.ToLower(named.Name.Name), "registration") {
							offenders = append(offenders, rel+": "+named.Name.Name)
						}
						if named.Name.Name == "Registry" {
							if structure, ok := named.Type.(*ast.StructType); ok {
								for _, field := range structure.Fields.List {
									for _, name := range field.Names {
										if ast.IsExported(name.Name) {
											offenders = append(offenders, rel+": Registry."+name.Name)
										}
									}
								}
							}
						}
					case *ast.ValueSpec:
						for index, name := range named.Names {
							isCLIList := isCLIListType(named.Type)
							if index < len(named.Values) {
								if literal, ok := named.Values[index].(*ast.CompositeLit); ok {
									isCLIList = isCLIList || isCLIListType(literal.Type)
								}
							}
							if ast.IsExported(name.Name) && isCLIList {
								offenders = append(offenders, rel+": "+name.Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(typed.Name.Name) {
					continue
				}
				if receiverName(typed) == "Registry" && typed.Name.Name != "Lookup" && typed.Name.Name != "Names" {
					offenders = append(offenders, rel+": Registry."+typed.Name.Name)
					continue
				}
				lower := strings.ToLower(typed.Name.Name)
				if strings.Contains(lower, "register") || strings.Contains(lower, "registration") ||
					(strings.HasPrefix(typed.Name.Name, "New") && strings.Contains(lower, "registry")) ||
					(typed.Name.Name != "Production" && functionAcceptsCLIList(typed.Type)) {
					offenders = append(offenders, rel+": "+typed.Name.Name)
				}
			}
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("adapter package exposes a production registration API:\n%s", strings.Join(offenders, "\n"))
	}
}

func assertRunnerHasNoAdapterNameCases(t *testing.T, root string, names []string) {
	t.Helper()
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	var offenders []string
	for _, sourceRoot := range []string{"internal/runtime", "internal/daemon"} {
		walkP3ProductionGo(t, root, sourceRoot, func(rel string, file *ast.File) {
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if ok {
					if name := adapterStringLiteral(literal, known); name != "" {
						offenders = append(offenders, rel+": hard-coded adapter name "+name)
					}
				}
				return true
			})
		})
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("runner paths special-case production adapter names:\n%s", strings.Join(compactStrings(offenders), "\n"))
	}
}

func adapterStringLiteral(expression ast.Expr, known map[string]bool) string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil || !known[value] {
		return ""
	}
	return value
}

func functionAcceptsCLIList(function *ast.FuncType) bool {
	if function == nil || function.Params == nil {
		return false
	}
	for _, field := range function.Params.List {
		if containsCLIType(field.Type) {
			return true
		}
	}
	return false
}

func containsCLIType(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name == "CLI"
	case *ast.ArrayType:
		return containsCLIType(typed.Elt)
	case *ast.Ellipsis:
		return containsCLIType(typed.Elt)
	case *ast.StarExpr:
		return containsCLIType(typed.X)
	}
	return false
}

func hasDockerIntegrationTest(file *ast.File) bool {
	hasTest, exercisesDocker := false, false
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "TestDocker") {
			hasTest = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			exercisesDocker = exercisesDocker || strings.HasPrefix(typed.Name, "NewDockerExecutor")
		case *ast.SelectorExpr:
			exercisesDocker = exercisesDocker || strings.HasPrefix(typed.Sel.Name, "NewDockerExecutor")
		}
		return !(hasTest && exercisesDocker)
	})
	return hasTest && exercisesDocker
}

func walkP3ProductionGo(t *testing.T, root, relativeRoot string, visit func(rel string, file *ast.File)) {
	t.Helper()
	roots := []string{relativeRoot}
	if relativeRoot == "" {
		roots = []string{"cmd", "internal"}
	}
	for _, sourceRoot := range roots {
		absoluteRoot := filepath.Join(root, filepath.FromSlash(sourceRoot))
		dockerContext := build.Default
		dockerContext.BuildTags = append(append([]string(nil), dockerContext.BuildTags...), "docker")
		err := filepath.WalkDir(absoluteRoot, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			selectedByDefault, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			selectedByDocker, err := dockerContext.MatchFile(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			if !selectedByDefault && !selectedByDocker {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			visit(filepath.ToSlash(rel), parsed)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func sourceWords(value string) []string {
	runes := []rune(value)
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, strings.ToLower(string(runes[start:end])))
		}
		start = -1
	}
	for index, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			flush(index)
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		previous := runes[index-1]
		nextIsLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
		if unicode.IsUpper(current) && (unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextIsLower)) {
			flush(index)
			start = index
		}
	}
	flush(len(runes))
	return words
}

func expressionRootName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return expressionRootName(typed.X)
	case *ast.IndexListExpr:
		return expressionRootName(typed.X)
	case *ast.SelectorExpr:
		return expressionRootName(typed.X)
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

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
