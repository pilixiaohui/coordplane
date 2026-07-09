package codemap

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStableNodeIDIsIndependentOfLineNumbers(t *testing.T) {
	first := StableNodeID(NodeKindRequirementSection, "need/runtime/../runtime/spec.md", "runtime-overview")
	second := StableNodeID(NodeKindRequirementSection, "need/runtime/spec.md", "runtime-overview")
	if first != second {
		t.Fatalf("stable node id changed for equivalent repo-relative paths: %s != %s", first, second)
	}
	renamed := StableNodeID(NodeKindRequirementSection, "need/runtime/spec.md", "runtime-details")
	if first == renamed {
		t.Fatalf("stable node id did not change when qualified name changed: %s", first)
	}
	edgeA := StableEdgeID(EdgeKindContains, first, renamed, []Evidence{{
		Path:      "need/runtime/spec.md",
		Span:      &Span{StartLine: 10},
		Collector: "docs",
	}})
	edgeB := StableEdgeID(EdgeKindContains, first, renamed, []Evidence{{
		Path:      "need/runtime/spec.md",
		Span:      &Span{StartLine: 20},
		Collector: "docs",
	}})
	if edgeA != edgeB {
		t.Fatalf("stable edge id changed for equivalent evidence on a different line: %s != %s", edgeA, edgeB)
	}
}

func TestIndexBuildsDeterministicPhaseOneSnapshot(t *testing.T) {
	root := writeCodemapFixture(t)
	first, err := Index(context.Background(), IndexOptions{Root: root})
	if err != nil {
		t.Fatalf("index fixture: %v", err)
	}
	second, err := Index(context.Background(), IndexOptions{Root: root})
	if err != nil {
		t.Fatalf("index fixture again: %v", err)
	}
	if first.SnapshotID != second.SnapshotID || first.RootDigest != second.RootDigest {
		t.Fatalf("snapshot is not deterministic: first=%s/%s second=%s/%s", first.SnapshotID, first.RootDigest, second.SnapshotID, second.RootDigest)
	}
	firstJSON, err := MarshalSnapshot(first)
	if err != nil {
		t.Fatalf("marshal first snapshot: %v", err)
	}
	secondJSON, err := MarshalSnapshot(second)
	if err != nil {
		t.Fatalf("marshal second snapshot: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("canonical JSON changed between identical inputs")
	}
	if first.Status != SnapshotStatusReady {
		t.Fatalf("snapshot status = %s, want ready; diagnostics=%v", first.Status, first.Diagnostics)
	}
	if diagnostics := ValidateSnapshot(first); len(diagnostics) > 0 {
		t.Fatalf("generated snapshot failed validation: %#v", diagnostics)
	}
	for _, kind := range []NodeKind{
		NodeKindRequirementDoc,
		NodeKindRequirementSection,
		NodeKindAcceptanceClause,
		NodeKindGoPackage,
		NodeKindGoFile,
		NodeKindGoType,
		NodeKindGoFunc,
		NodeKindTestCase,
		NodeKindFixture,
		NodeKindTeamConfig,
		NodeKindScript,
		NodeKindMakeTarget,
		NodeKindReleaseGate,
	} {
		if len(first.Indexes.ByKind[kind]) == 0 {
			t.Fatalf("snapshot missing %s nodes", kind)
		}
	}
	if _, ok := first.Indexes.Entrypoints["example.com/codemapfixture/cmd/app"]; !ok {
		t.Fatalf("entrypoint package was not indexed: %#v", first.Indexes.Entrypoints)
	}
	if _, ok := first.Indexes.Tests["example.com/codemapfixture/internal/widget.TestWidgetInvariant"]; !ok {
		t.Fatalf("test case was not indexed: %#v", first.Indexes.Tests)
	}
	if len(first.Indexes.AcceptanceGates["release-health-fixture"]) == 0 {
		t.Fatalf("release-health make target was not indexed as a gate: %#v", first.Indexes.AcceptanceGates)
	}
	if !slices.Contains(first.UpdateSemantics.SupportedTriggers, "local_watcher") ||
		first.UpdateSemantics.ReadySnapshot != "atomic_ready_snapshot" {
		t.Fatalf("update semantics do not preserve near-real-time ready snapshot contract: %#v", first.UpdateSemantics)
	}
	for _, node := range first.Nodes {
		if filepath.IsAbs(node.Path) {
			t.Fatalf("node leaked absolute path: %#v", node)
		}
		if strings.Contains(node.Path, ".multica") {
			t.Fatalf("ignored directory leaked into snapshot: %#v", node)
		}
	}
}

func TestIndexRecordsIncrementalRequestShapeWithoutWatcherImplementation(t *testing.T) {
	root := writeCodemapFixture(t)
	snapshot, err := Index(context.Background(), IndexOptions{
		Root:         root,
		ChangedFiles: []string{"internal/widget/widget.go", "./internal/widget/widget.go"},
	})
	if err != nil {
		t.Fatalf("index incremental request: %v", err)
	}
	if snapshot.GeneratedFrom.Mode != CollectionModeIncremental {
		t.Fatalf("mode = %s, want incremental_request", snapshot.GeneratedFrom.Mode)
	}
	if got := strings.Join(snapshot.GeneratedFrom.ChangedFiles, ","); got != "internal/widget/widget.go" {
		t.Fatalf("changed files = %q, want de-duplicated repo-relative path", got)
	}
}

func TestValidateSnapshotRejectsAbsolutePathLeak(t *testing.T) {
	snapshot := Snapshot{
		SchemaVersion:   SchemaVersion,
		SnapshotID:      "codemap:snapshot:placeholder",
		Status:          SnapshotStatusReady,
		RootDigest:      "placeholder",
		GeneratedFrom:   GeneratedFrom{Root: ".", Mode: CollectionModeFull, InputDigest: "placeholder"},
		UpdateSemantics: DefaultUpdateSemantics(),
		Nodes: []Node{{
			ID:         "codemap:node:absolute",
			Kind:       NodeKindRequirementDoc,
			Name:       "leak",
			Path:       filepath.Join(string(filepath.Separator), "tmp", "secret.md"),
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
		}},
	}
	diagnostics := ValidateSnapshot(snapshot)
	if !containsDiagnostic(diagnostics, "CODMAP_PATH_LEAK") {
		t.Fatalf("absolute path leak was not rejected: %#v", diagnostics)
	}
}

func TestValidateSnapshotRejectsPartialStatusErrorDiagnosticsAndLeakFields(t *testing.T) {
	snapshot := validMinimalSnapshot()
	snapshot.Status = SnapshotStatusPartial
	snapshot.Nodes[0].Path = ".multica/ignored.md"
	snapshot.Nodes[0].Name = "generated from /tmp/private/source.go"
	snapshot.Nodes[0].Metadata = map[string]any{
		"nested": map[string]any{
			"token": "github_pat_1234567890",
		},
	}
	snapshot.Edges = []Edge{{
		ID:         "codemap:edge:one",
		FromID:     snapshot.Nodes[0].ID,
		ToID:       snapshot.Nodes[0].ID,
		Kind:       EdgeKindContains,
		Confidence: 1,
		Evidence: []Evidence{{
			Path:      ".git/config",
			Collector: "test",
		}},
	}}
	snapshot.Diagnostics = []Diagnostic{{
		Severity: DiagnosticError,
		Code:     "CODMAP_FIXTURE_ERROR",
		Path:     ".agents/private.json",
		Message:  "parser failed at /home/user/workspace/private.go",
	}}
	snapshot.RootDigest = mustRootDigest(t, snapshot)
	snapshot.SnapshotID = snapshotStableID(snapshot)

	diagnostics := ValidateSnapshot(snapshot)
	for _, code := range []string{
		"CODMAP_SNAPSHOT_NOT_READY",
		"CODMAP_SNAPSHOT_HAS_ERROR_DIAGNOSTIC",
		"CODMAP_PATH_LEAK",
		"CODMAP_OUTPUT_PATH_LEAK",
		"CODMAP_OUTPUT_SECRET_LEAK",
	} {
		if !containsDiagnostic(diagnostics, code) {
			t.Fatalf("validation diagnostics missing %s: %#v", code, diagnostics)
		}
	}
}

func TestParserDiagnosticsUseRepoRelativePaths(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "internal/bad/bad.go", "package bad\n\nfunc Broken( {\n")
	pkg := goListPackage{
		Dir:        filepath.Join(root, "internal", "bad"),
		ImportPath: "example.com/codemapfixture/internal/bad",
		Name:       "bad",
		GoFiles:    []string{"bad.go"},
	}
	var collection Collection
	GoCollector{}.collectGoFile(root, pkg, StableNodeID(NodeKindGoPackage, "internal/bad", pkg.ImportPath), "bad.go", &collection)
	if len(collection.Diagnostics) == 0 {
		t.Fatal("expected parser diagnostic for malformed Go file")
	}
	if strings.Contains(collection.Diagnostics[0].Message, root) || filepath.IsAbs(collection.Diagnostics[0].Message) {
		t.Fatalf("parser diagnostic leaked root path: %#v", collection.Diagnostics[0])
	}
	if !strings.Contains(collection.Diagnostics[0].Message, "internal/bad/bad.go") {
		t.Fatalf("parser diagnostic did not use repo-relative path: %#v", collection.Diagnostics[0])
	}
}

func TestSnapshotIDIncludesInputDigestOnlyChanges(t *testing.T) {
	root := writeCodemapFixture(t)
	first, err := Index(context.Background(), IndexOptions{Root: root})
	if err != nil {
		t.Fatalf("index fixture: %v", err)
	}
	writeFixtureFile(t, root, "Makefile", ".PHONY: test release-health-fixture\n\ntest:\n\tgo test ./... -count=1\n\nrelease-health-fixture:\n\tbash scripts/release-health-fixture.sh\n")
	second, err := Index(context.Background(), IndexOptions{Root: root})
	if err != nil {
		t.Fatalf("index fixture after Makefile recipe change: %v", err)
	}
	if first.GeneratedFrom.InputDigest == second.GeneratedFrom.InputDigest {
		t.Fatalf("input digest did not change after Makefile body changed: %s", first.GeneratedFrom.InputDigest)
	}
	if first.SnapshotID == second.SnapshotID {
		t.Fatalf("snapshot id did not include input digest: %s", first.SnapshotID)
	}
}

func validMinimalSnapshot() Snapshot {
	node := Node{
		ID:         StableNodeID(NodeKindRequirementDoc, "need/README.md", "need/README.md"),
		Kind:       NodeKindRequirementDoc,
		Name:       "need/README.md",
		Path:       "need/README.md",
		Visibility: "repo",
		Source:     "evidence",
		Confidence: 1,
	}
	snapshot := Snapshot{
		SchemaVersion:     SchemaVersion,
		Status:            SnapshotStatusReady,
		GeneratedFrom:     GeneratedFrom{Root: ".", Mode: CollectionModeFull, ModulePath: "example.com/codemapfixture", InputDigest: "input"},
		UpdateSemantics:   DefaultUpdateSemantics(),
		CollectorVersions: []CollectorVersion{{Name: "test", Version: "v1"}},
		Nodes:             []Node{node},
	}
	snapshot.RootDigest, _ = computeRootDigest(snapshot.Nodes, snapshot.Edges, snapshot.Diagnostics)
	snapshot.SnapshotID = snapshotStableID(snapshot)
	return snapshot
}

func mustRootDigest(t *testing.T, snapshot Snapshot) string {
	t.Helper()
	digest, err := computeRootDigest(snapshot.Nodes, snapshot.Edges, snapshot.Diagnostics)
	if err != nil {
		t.Fatalf("compute root digest: %v", err)
	}
	return digest
}

func writeCodemapFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module example.com/codemapfixture\n\ngo 1.22\n")
	writeFixtureFile(t, root, "README.md", "# Fixture\n")
	writeFixtureFile(t, root, "need/README.md", "# Needs\n")
	writeFixtureFile(t, root, "need/runtime/README.md", "# Runtime\n")
	writeFixtureFile(t, root, "need/runtime/runtime.md", "# Runtime Requirement\n\n## Agent startup\n")
	writeFixtureFile(t, root, "need/验收合同.md", "# Acceptance\n\n## CP-ACCEPT-001\n")
	writeFixtureFile(t, root, "cmd/app/main.go", `package main

import "example.com/codemapfixture/internal/widget"

func main() {
	_ = widget.NewWidget()
}
`)
	writeFixtureFile(t, root, "internal/widget/widget.go", `package widget

type Widget struct {
	Name string
}

func NewWidget() Widget {
	return Widget{Name: "codemap"}
}
`)
	writeFixtureFile(t, root, "internal/widget/widget_test.go", `package widget

import "testing"

func TestWidgetInvariant(t *testing.T) {
	if NewWidget().Name == "" {
		t.Fatal("empty widget")
	}
}
`)
	writeFixtureFile(t, root, "team_config/fixtures/team.yaml", `team_id: codemap-fixture
version: 1
agents:
  - id: dev
    runtime_profile: local
    cli_backend: codex
    skills: [go]
    capabilities: [codemap.index]
runtime_profiles:
  local:
    kind: local
`)
	writeFixtureFile(t, root, "Makefile", ".PHONY: test release-health-fixture\n\ntest:\n\tgo test ./...\n\nrelease-health-fixture:\n\tbash scripts/release-health-fixture.sh\n")
	writeFixtureFile(t, root, "scripts/release-health-fixture.sh", "#!/usr/bin/env bash\nset -euo pipefail\n")
	writeFixtureFile(t, root, ".multica/ignored.md", "# ignored\n")
	return root
}

func writeFixtureFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture file %s: %v", rel, err)
	}
}

func containsDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
