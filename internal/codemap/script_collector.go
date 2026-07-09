package codemap

import (
	"bufio"
	"context"
	"path/filepath"
	"strings"
)

type ScriptCollector struct{}

func (ScriptCollector) Name() string    { return "script" }
func (ScriptCollector) Version() string { return "v1" }

func (collector ScriptCollector) Collect(ctx context.Context, collectCtx CollectContext) (Collection, error) {
	var collection Collection
	makefilePath := filepath.Join(collectCtx.Root, "Makefile")
	if raw, digest, err := readFileDigest(makefilePath); err == nil {
		collection.InputFiles = append(collection.InputFiles, InputFile{Path: "Makefile", Digest: digest})
		collector.collectMakefileTargets(string(raw), &collection)
	}
	files, err := walkRelativeFiles(collectCtx.Root, func(rel string) bool {
		return strings.HasPrefix(rel, "scripts/") && strings.HasSuffix(strings.ToLower(rel), ".sh")
	})
	if err != nil {
		return Collection{}, err
	}
	for _, rel := range files {
		select {
		case <-ctx.Done():
			return Collection{}, ctx.Err()
		default:
		}
		fullPath := filepath.Join(collectCtx.Root, filepath.FromSlash(rel))
		_, digest, err := readFileDigest(fullPath)
		if err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_SCRIPT_READ_FAILED",
				Path:     rel,
				Message:  err.Error(),
			})
			continue
		}
		collection.InputFiles = append(collection.InputFiles, InputFile{Path: rel, Digest: digest})
		scriptID := StableNodeID(NodeKindScript, rel, filepath.Base(rel))
		collection.Nodes = append(collection.Nodes, Node{
			ID:         scriptID,
			Kind:       NodeKindScript,
			Name:       filepath.Base(rel),
			Path:       rel,
			Digest:     digest,
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
		})
		if isAcceptanceGateName(filepath.Base(rel)) {
			gateID := StableNodeID(NodeKindReleaseGate, rel, filepath.Base(rel))
			collection.Nodes = append(collection.Nodes, Node{
				ID:         gateID,
				Kind:       NodeKindReleaseGate,
				Name:       filepath.Base(rel),
				Path:       rel,
				Visibility: "repo",
				Source:     "evidence",
				Confidence: 1,
				Metadata: map[string]any{
					"source_kind": "script",
				},
			})
			evidence := []Evidence{{Path: rel, Collector: "script"}}
			collection.Edges = append(collection.Edges, Edge{
				ID:         StableEdgeID(EdgeKindParticipatesInGate, scriptID, gateID, evidence),
				FromID:     scriptID,
				ToID:       gateID,
				Kind:       EdgeKindParticipatesInGate,
				Evidence:   evidence,
				Confidence: 1,
			})
		}
	}
	return collection, nil
}

func (ScriptCollector) collectMakefileTargets(body string, collection *Collection) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	line := 0
	for scanner.Scan() {
		line++
		targets, ok := parseMakefileTargets(scanner.Text())
		if !ok {
			continue
		}
		for _, target := range targets {
			targetID := StableNodeID(NodeKindMakeTarget, "Makefile", target)
			collection.Nodes = append(collection.Nodes, Node{
				ID:         targetID,
				Kind:       NodeKindMakeTarget,
				Name:       target,
				Path:       "Makefile",
				Span:       &Span{StartLine: line},
				Visibility: "repo",
				Source:     "evidence",
				Confidence: 1,
			})
			if !isAcceptanceGateName(target) {
				continue
			}
			gateID := StableNodeID(NodeKindReleaseGate, "Makefile", target)
			collection.Nodes = append(collection.Nodes, Node{
				ID:         gateID,
				Kind:       NodeKindReleaseGate,
				Name:       target,
				Path:       "Makefile",
				Span:       &Span{StartLine: line},
				Visibility: "repo",
				Source:     "evidence",
				Confidence: 1,
				Metadata: map[string]any{
					"source_kind": "make_target",
				},
			})
			evidence := []Evidence{{Path: "Makefile", Span: &Span{StartLine: line}, Collector: "script"}}
			collection.Edges = append(collection.Edges, Edge{
				ID:         StableEdgeID(EdgeKindParticipatesInGate, targetID, gateID, evidence),
				FromID:     targetID,
				ToID:       gateID,
				Kind:       EdgeKindParticipatesInGate,
				Evidence:   evidence,
				Confidence: 1,
			})
		}
	}
}

func parseMakefileTargets(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(line, "\t") {
		return nil, false
	}
	index := strings.Index(trimmed, ":")
	if index <= 0 {
		return nil, false
	}
	left := strings.TrimSpace(trimmed[:index])
	if strings.Contains(left, "=") || strings.HasPrefix(left, ".") {
		return nil, false
	}
	fields := strings.Fields(left)
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

func isAcceptanceGateName(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".sh")
	return name == "test" || strings.Contains(name, "release-health") || strings.HasPrefix(name, "cp-accept") || strings.HasPrefix(name, "cp-probe")
}
