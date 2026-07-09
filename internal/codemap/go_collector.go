package codemap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type GoCollector struct{}

func (GoCollector) Name() string    { return "go" }
func (GoCollector) Version() string { return "v1" }

type goListPackage struct {
	Dir          string
	ImportPath   string
	Name         string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	Module       *struct {
		Path string
		Dir  string
	}
}

func (collector GoCollector) Collect(ctx context.Context, collectCtx CollectContext) (Collection, error) {
	packages, diagnostics := runGoList(ctx, collectCtx.Root)
	var collection Collection
	collection.Diagnostics = append(collection.Diagnostics, diagnostics...)
	if len(packages) == 0 {
		return collection, nil
	}
	packageIDs := make(map[string]string, len(packages))
	packagePaths := make(map[string]string, len(packages))
	for _, pkg := range packages {
		relDir, err := relPath(collectCtx.Root, pkg.Dir)
		if err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_GO_PACKAGE_PATH_FAILED",
				Path:     pkg.Dir,
				Message:  err.Error(),
			})
			continue
		}
		nodeID := StableNodeID(NodeKindGoPackage, relDir, pkg.ImportPath)
		packageIDs[pkg.ImportPath] = nodeID
		packagePaths[pkg.ImportPath] = relDir
		metadata := map[string]any{
			"package_name": pkg.Name,
			"go_files":     len(pkg.GoFiles),
			"imports":      sortedStrings(pkg.Imports),
		}
		if strings.HasPrefix(relDir, "cmd/") {
			metadata["entrypoint"] = true
		}
		collection.Nodes = append(collection.Nodes, Node{
			ID:         nodeID,
			Kind:       NodeKindGoPackage,
			Name:       pkg.ImportPath,
			Path:       relDir,
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata:   metadata,
		})
	}

	for _, pkg := range packages {
		pkgID := packageIDs[pkg.ImportPath]
		if pkgID == "" {
			continue
		}
		relDir := packagePaths[pkg.ImportPath]
		for _, importPath := range sortedStrings(pkg.Imports) {
			toID := packageIDs[importPath]
			if toID == "" {
				continue
			}
			evidence := []Evidence{{Path: relDir, Collector: "go"}}
			collection.Edges = append(collection.Edges, Edge{
				ID:         StableEdgeID(EdgeKindImports, pkgID, toID, evidence),
				FromID:     pkgID,
				ToID:       toID,
				Kind:       EdgeKindImports,
				Evidence:   evidence,
				Confidence: 1,
			})
		}
		for _, file := range sortedStrings(pkg.GoFiles) {
			collector.collectGoFile(collectCtx.Root, pkg, pkgID, file, &collection)
		}
	}
	return collection, nil
}

func (GoCollector) collectGoFile(root string, pkg goListPackage, pkgID, file string, collection *Collection) {
	fullPath := filepath.Join(pkg.Dir, file)
	rel, err := relPath(root, fullPath)
	if err != nil {
		collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
			Severity: DiagnosticError,
			Code:     "CODMAP_GO_FILE_PATH_FAILED",
			Path:     fullPath,
			Message:  err.Error(),
		})
		return
	}
	raw, digest, err := readFileDigest(fullPath)
	if err != nil {
		collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
			Severity: DiagnosticError,
			Code:     "CODMAP_GO_FILE_READ_FAILED",
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
			"package": pkg.ImportPath,
		},
	})
	fileEvidence := []Evidence{{Path: rel, Collector: "go"}}
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
			Code:       "CODMAP_GO_PARSE_FAILED",
			Path:       rel,
			Message:    err.Error(),
			RepairHint: "fix Go syntax before relying on function/type-level codemap entries",
		})
		return
	}
	for _, decl := range parsed.Decls {
		switch typed := decl.(type) {
		case *ast.GenDecl:
			if typed.Tok != token.TYPE {
				continue
			}
			for _, spec := range typed.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				line := fileSet.Position(typeSpec.Pos()).Line
				name := pkg.ImportPath + "." + typeSpec.Name.Name
				nodeID := StableNodeID(NodeKindGoType, rel, name)
				collection.Nodes = append(collection.Nodes, Node{
					ID:         nodeID,
					Kind:       NodeKindGoType,
					Name:       name,
					Path:       rel,
					Span:       &Span{StartLine: line},
					Visibility: "repo",
					Source:     "evidence",
					Confidence: 1,
					Metadata: map[string]any{
						"package": pkg.ImportPath,
					},
				})
				addDefineEdge(collection, fileID, nodeID, rel, line, "go")
			}
		case *ast.FuncDecl:
			line := fileSet.Position(typed.Pos()).Line
			name := pkg.ImportPath + "." + functionName(typed)
			nodeID := StableNodeID(NodeKindGoFunc, rel, name)
			collection.Nodes = append(collection.Nodes, Node{
				ID:         nodeID,
				Kind:       NodeKindGoFunc,
				Name:       name,
				Path:       rel,
				Span:       &Span{StartLine: line},
				Visibility: "repo",
				Source:     "evidence",
				Confidence: 1,
				Metadata: map[string]any{
					"package": pkg.ImportPath,
				},
			})
			addDefineEdge(collection, fileID, nodeID, rel, line, "go")
		}
	}
}

func runGoList(ctx context.Context, root string) ([]goListPackage, []Diagnostic) {
	cmd := exec.CommandContext(ctx, "go", "list", "-buildvcs=false", "-json", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, []Diagnostic{{
			Severity:   DiagnosticError,
			Code:       "CODMAP_GO_LIST_FAILED",
			Message:    sanitizeDiagnosticMessage(root, message),
			RepairHint: "ensure go is installed and run from a Go module root",
		}}
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var packages []goListPackage
	for {
		var pkg goListPackage
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			return packages, []Diagnostic{{
				Severity: DiagnosticError,
				Code:     "CODMAP_GO_LIST_DECODE_FAILED",
				Message:  sanitizeDiagnosticMessage(root, err.Error()),
			}}
		}
		packages = append(packages, pkg)
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].ImportPath < packages[j].ImportPath
	})
	return packages, nil
}

func sanitizeDiagnosticMessage(root, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return message
	}
	absRoot, err := filepath.Abs(root)
	if err == nil {
		message = strings.ReplaceAll(message, filepath.ToSlash(absRoot), ".")
		message = strings.ReplaceAll(message, absRoot, ".")
	}
	return message
}

func readModulePath(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1], nil
			}
		}
	}
	return "", fmt.Errorf("go.mod missing module directive")
}

func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := receiverName(fn.Recv.List[0].Type)
	if recv == "" {
		return fn.Name.Name
	}
	return recv + "." + fn.Name.Name
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return "*" + receiverName(typed.X)
	case *ast.IndexExpr:
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	default:
		return ""
	}
}

func addDefineEdge(collection *Collection, fromID, toID, rel string, line int, collector string) {
	evidence := []Evidence{{Path: rel, Span: &Span{StartLine: line}, Collector: collector}}
	collection.Edges = append(collection.Edges, Edge{
		ID:         StableEdgeID(EdgeKindDefines, fromID, toID, evidence),
		FromID:     fromID,
		ToID:       toID,
		Kind:       EdgeKindDefines,
		Evidence:   evidence,
		Confidence: 1,
	})
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
