package codemap

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

type TestCollector struct{}

func (TestCollector) Name() string    { return "test" }
func (TestCollector) Version() string { return "v1" }

func (collector TestCollector) Collect(ctx context.Context, collectCtx CollectContext) (Collection, error) {
	packages, diagnostics := runGoList(ctx, collectCtx.Root)
	var collection Collection
	collection.Diagnostics = append(collection.Diagnostics, diagnostics...)
	for _, pkg := range packages {
		relDir, err := relPath(collectCtx.Root, pkg.Dir)
		if err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_TEST_PACKAGE_PATH_FAILED",
				Path:     pkg.Dir,
				Message:  err.Error(),
			})
			continue
		}
		pkgID := StableNodeID(NodeKindGoPackage, relDir, pkg.ImportPath)
		for _, file := range sortedStrings(append(append([]string(nil), pkg.TestGoFiles...), pkg.XTestGoFiles...)) {
			select {
			case <-ctx.Done():
				return Collection{}, ctx.Err()
			default:
			}
			collector.collectTestFile(collectCtx.Root, pkg, pkgID, file, &collection)
		}
	}
	return collection, nil
}

func (TestCollector) collectTestFile(root string, pkg goListPackage, pkgID, file string, collection *Collection) {
	fullPath := filepath.Join(pkg.Dir, file)
	rel, err := relPath(root, fullPath)
	if err != nil {
		collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
			Severity: DiagnosticError,
			Code:     "CODMAP_TEST_FILE_PATH_FAILED",
			Path:     fullPath,
			Message:  err.Error(),
		})
		return
	}
	raw, digest, err := readFileDigest(fullPath)
	if err != nil {
		collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
			Severity: DiagnosticError,
			Code:     "CODMAP_TEST_FILE_READ_FAILED",
			Path:     rel,
			Message:  err.Error(),
		})
		return
	}
	collection.InputFiles = append(collection.InputFiles, InputFile{Path: rel, Digest: digest})
	fileID := StableNodeID(NodeKindGoFile, rel, pkg.ImportPath+"/"+file)
	collection.Nodes = append(collection.Nodes, Node{
		ID:         fileID,
		Kind:       NodeKindGoFile,
		Name:       file,
		Path:       rel,
		Digest:     digest,
		Visibility: "repo",
		Source:     "evidence",
		Confidence: 1,
		Metadata: map[string]any{
			"package":   pkg.ImportPath,
			"test_file": true,
		},
	})
	fileEvidence := []Evidence{{Path: rel, Collector: "test"}}
	collection.Edges = append(collection.Edges, Edge{
		ID:         StableEdgeID(EdgeKindContains, pkgID, fileID, fileEvidence),
		FromID:     pkgID,
		ToID:       fileID,
		Kind:       EdgeKindContains,
		Evidence:   fileEvidence,
		Confidence: 1,
	})

	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, rel, raw, 0)
	if err != nil {
		collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
			Severity:   DiagnosticWarning,
			Code:       "CODMAP_TEST_PARSE_FAILED",
			Path:       rel,
			Message:    err.Error(),
			RepairHint: "fix Go test syntax before relying on test case codemap entries",
		})
		return
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !isGoTestFunc(fn.Name.Name) {
			continue
		}
		line := fileSet.Position(fn.Pos()).Line
		name := pkg.ImportPath + "." + fn.Name.Name
		nodeID := StableNodeID(NodeKindTestCase, rel, name)
		collection.Nodes = append(collection.Nodes, Node{
			ID:         nodeID,
			Kind:       NodeKindTestCase,
			Name:       name,
			Path:       rel,
			Span:       &Span{StartLine: line},
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata: map[string]any{
				"package": pkg.ImportPath,
				"kind":    testFuncKind(fn.Name.Name),
			},
		})
		addDefineEdge(collection, fileID, nodeID, rel, line, "test")
		evidence := []Evidence{{Path: rel, Span: &Span{StartLine: line}, Collector: "test"}}
		collection.Edges = append(collection.Edges, Edge{
			ID:         StableEdgeID(EdgeKindCoveredByTest, pkgID, nodeID, evidence),
			FromID:     pkgID,
			ToID:       nodeID,
			Kind:       EdgeKindCoveredByTest,
			Evidence:   evidence,
			Confidence: 1,
		})
	}
}

func isGoTestFunc(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Example")
}

func testFuncKind(name string) string {
	switch {
	case strings.HasPrefix(name, "Benchmark"):
		return "benchmark"
	case strings.HasPrefix(name, "Example"):
		return "example"
	default:
		return "test"
	}
}
