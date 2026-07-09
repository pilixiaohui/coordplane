package codemap

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type DocsCollector struct{}

func (DocsCollector) Name() string    { return "docs" }
func (DocsCollector) Version() string { return "v1" }

func (collector DocsCollector) Collect(ctx context.Context, collectCtx CollectContext) (Collection, error) {
	files, err := walkRelativeFiles(collectCtx.Root, func(rel string) bool {
		return rel == "README.md" || (strings.HasPrefix(rel, "need/") && strings.HasSuffix(strings.ToLower(rel), ".md"))
	})
	if err != nil {
		return Collection{}, err
	}
	var collection Collection
	for _, rel := range files {
		select {
		case <-ctx.Done():
			return Collection{}, ctx.Err()
		default:
		}
		fullPath := filepath.Join(collectCtx.Root, filepath.FromSlash(rel))
		raw, digest, err := readFileDigest(fullPath)
		if err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_DOC_READ_FAILED",
				Path:     rel,
				Message:  err.Error(),
			})
			continue
		}
		collection.InputFiles = append(collection.InputFiles, InputFile{Path: rel, Digest: digest})
		docID := StableNodeID(NodeKindRequirementDoc, rel, rel)
		collection.Nodes = append(collection.Nodes, Node{
			ID:         docID,
			Kind:       NodeKindRequirementDoc,
			Name:       rel,
			Path:       rel,
			Digest:     digest,
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata: map[string]any{
				"category": requirementCategory(rel),
			},
		})
		collection.Nodes = append(collection.Nodes, collector.headingNodes(rel, docID, string(raw))...)
		collection.Edges = append(collection.Edges, collector.headingEdges(rel, docID, string(raw))...)
	}
	return collection, nil
}

func (DocsCollector) headingNodes(rel, docID, body string) []Node {
	var nodes []Node
	anchors := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(body))
	line := 0
	for scanner.Scan() {
		line++
		level, heading, ok := parseMarkdownHeading(scanner.Text())
		if !ok {
			continue
		}
		anchor := uniqueAnchor(anchors, MarkdownAnchor(heading))
		kind := NodeKindRequirementSection
		if rel == "need/验收合同.md" {
			kind = NodeKindAcceptanceClause
		}
		nodeID := StableNodeID(kind, rel, anchor)
		nodes = append(nodes, Node{
			ID:         nodeID,
			Kind:       kind,
			Name:       heading,
			Path:       rel,
			Span:       &Span{StartLine: line},
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata: map[string]any{
				"anchor":   anchor,
				"level":    level,
				"category": requirementCategory(rel),
			},
		})
	}
	return nodes
}

func (DocsCollector) headingEdges(rel, docID, body string) []Edge {
	var edges []Edge
	anchors := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(body))
	line := 0
	for scanner.Scan() {
		line++
		_, heading, ok := parseMarkdownHeading(scanner.Text())
		if !ok {
			continue
		}
		anchor := uniqueAnchor(anchors, MarkdownAnchor(heading))
		kind := NodeKindRequirementSection
		if rel == "need/验收合同.md" {
			kind = NodeKindAcceptanceClause
		}
		toID := StableNodeID(kind, rel, anchor)
		evidence := []Evidence{{Path: rel, Span: &Span{StartLine: line}, Collector: "docs"}}
		edges = append(edges, Edge{
			ID:         StableEdgeID(EdgeKindContains, docID, toID, evidence),
			FromID:     docID,
			ToID:       toID,
			Kind:       EdgeKindContains,
			Evidence:   evidence,
			Confidence: 1,
		})
	}
	return edges
}

func parseMarkdownHeading(line string) (int, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	heading := strings.TrimSpace(line[level+1:])
	heading = strings.Trim(heading, "# ")
	if heading == "" {
		return 0, "", false
	}
	return level, heading, true
}

func uniqueAnchor(seen map[string]int, anchor string) string {
	seen[anchor]++
	if seen[anchor] == 1 {
		return anchor
	}
	return fmt.Sprintf("%s-%d", anchor, seen[anchor])
}

func requirementCategory(rel string) string {
	if rel == "README.md" || rel == "need/README.md" {
		return "overview"
	}
	if rel == "need/验收合同.md" {
		return "acceptance"
	}
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "need" {
		return parts[1]
	}
	return "docs"
}
