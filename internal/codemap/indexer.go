package codemap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type IndexOptions struct {
	Root         string
	ProjectID    string
	ResourceID   string
	ChangedFiles []string
	Collectors   []Collector
}

func Index(ctx context.Context, opts IndexOptions) (Snapshot, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve codemap root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat codemap root: %w", err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("codemap root %s is not a directory", root)
	}

	collectors := opts.Collectors
	if len(collectors) == 0 {
		collectors = DefaultCollectors()
	}
	modulePath, moduleErr := readModulePath(absRoot)
	var diagnostics []Diagnostic
	if moduleErr != nil {
		diagnostics = append(diagnostics, Diagnostic{
			Severity:   DiagnosticWarning,
			Code:       "CODMAP_GO_MODULE_UNAVAILABLE",
			Path:       "go.mod",
			Message:    sanitizeDiagnosticMessage(absRoot, moduleErr.Error()),
			RepairHint: "run from a Go module root or pass a root containing go.mod",
		})
	}
	changedFiles := cleanChangedFiles(opts.ChangedFiles)
	mode := CollectionModeFull
	if len(changedFiles) > 0 {
		mode = CollectionModeIncremental
	}
	collectCtx := CollectContext{
		Root:         absRoot,
		ModulePath:   modulePath,
		Mode:         mode,
		ChangedFiles: changedFiles,
	}

	var nodes []Node
	var edges []Edge
	var inputs []InputFile
	collectorVersions := make([]CollectorVersion, 0, len(collectors))
	for _, collector := range collectors {
		if collector == nil {
			continue
		}
		collectorVersions = append(collectorVersions, CollectorVersion{
			Name:    collector.Name(),
			Version: collector.Version(),
		})
		collection, err := collector.Collect(ctx, collectCtx)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_COLLECTOR_FAILED",
				Message:  fmt.Sprintf("%s collector failed: %s", collector.Name(), sanitizeDiagnosticMessage(absRoot, err.Error())),
			})
			continue
		}
		nodes = append(nodes, collection.Nodes...)
		edges = append(edges, collection.Edges...)
		diagnostics = append(diagnostics, collection.Diagnostics...)
		inputs = append(inputs, collection.InputFiles...)
	}

	nodes = dedupeNodes(nodes, &diagnostics)
	edges = dedupeEdges(edges, &diagnostics)
	sortNodes(nodes)
	sortEdges(edges)
	sortDiagnostics(diagnostics)
	sortCollectorVersions(collectorVersions)
	inputDigest := inputDigest(inputs)
	rootDigest, err := computeRootDigest(nodes, edges, diagnostics)
	if err != nil {
		return Snapshot{}, err
	}
	status := SnapshotStatusReady
	if hasErrorDiagnostics(diagnostics) {
		status = SnapshotStatusPartial
	}
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		ProjectID:     strings.TrimSpace(opts.ProjectID),
		ResourceID:    strings.TrimSpace(opts.ResourceID),
		Status:        status,
		RootDigest:    rootDigest,
		GeneratedFrom: GeneratedFrom{
			Root:         ".",
			ModulePath:   modulePath,
			Mode:         mode,
			ChangedFiles: changedFiles,
			InputDigest:  inputDigest,
		},
		UpdateSemantics:   DefaultUpdateSemantics(),
		CollectorVersions: collectorVersions,
		Nodes:             nodes,
		Edges:             edges,
		Diagnostics:       diagnostics,
	}
	snapshot.SnapshotID = snapshotStableID(snapshot)
	snapshot.Indexes = BuildIndexes(snapshot)
	return snapshot, nil
}

func CanonicalizeSnapshot(snapshot Snapshot) Snapshot {
	snapshot.GeneratedFrom.ChangedFiles = cleanChangedFiles(snapshot.GeneratedFrom.ChangedFiles)
	sortCollectorVersions(snapshot.CollectorVersions)
	sortNodes(snapshot.Nodes)
	sortEdges(snapshot.Edges)
	sortDiagnostics(snapshot.Diagnostics)
	snapshot.Indexes = BuildIndexes(snapshot)
	return snapshot
}

func MarshalSnapshot(snapshot Snapshot) ([]byte, error) {
	snapshot = CanonicalizeSnapshot(snapshot)
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshot); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func DecodeSnapshot(raw []byte) (Snapshot, error) {
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Snapshot{}, errors.New("snapshot file contains more than one JSON value")
	}
	return snapshot, nil
}

func BuildIndexes(snapshot Snapshot) Indexes {
	indexes := Indexes{
		ByKind:          map[NodeKind][]string{},
		ByPath:          map[string][]string{},
		Requirements:    map[string][]string{},
		Packages:        map[string]string{},
		Entrypoints:     map[string]string{},
		Tests:           map[string]string{},
		AcceptanceGates: map[string][]string{},
	}
	for _, node := range snapshot.Nodes {
		indexes.ByKind[node.Kind] = append(indexes.ByKind[node.Kind], node.ID)
		if node.Path != "" {
			indexes.ByPath[node.Path] = append(indexes.ByPath[node.Path], node.ID)
		}
		switch node.Kind {
		case NodeKindRequirementDoc, NodeKindRequirementSection, NodeKindAcceptanceClause:
			if node.Path != "" {
				indexes.Requirements[node.Path] = append(indexes.Requirements[node.Path], node.ID)
			}
		case NodeKindGoPackage:
			indexes.Packages[node.Name] = node.ID
			if isTrueMetadata(node.Metadata, "entrypoint") {
				indexes.Entrypoints[node.Name] = node.ID
			}
		case NodeKindTestCase:
			indexes.Tests[node.Name] = node.ID
		case NodeKindReleaseGate:
			indexes.AcceptanceGates[node.Name] = append(indexes.AcceptanceGates[node.Name], node.ID)
		}
	}
	sortIndexSlices(indexes.ByKind)
	sortStringSliceMap(indexes.ByPath)
	sortStringSliceMap(indexes.Requirements)
	sortStringSliceMap(indexes.AcceptanceGates)
	return indexes
}

func ValidateSnapshot(snapshot Snapshot) []Diagnostic {
	var diagnostics []Diagnostic
	if snapshot.SchemaVersion != SchemaVersion {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_SCHEMA_VERSION_INVALID", "", fmt.Sprintf("schema_version %q is not supported", snapshot.SchemaVersion)))
	}
	if strings.TrimSpace(snapshot.SnapshotID) == "" {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_SNAPSHOT_ID_REQUIRED", "", "snapshot_id is required"))
	}
	if strings.TrimSpace(snapshot.RootDigest) == "" {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_ROOT_DIGEST_REQUIRED", "", "root_digest is required"))
	}
	if snapshot.Status != SnapshotStatusReady && snapshot.Status != SnapshotStatusPartial && snapshot.Status != SnapshotStatusBuilding {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_STATUS_INVALID", "", fmt.Sprintf("status %q is invalid", snapshot.Status)))
	}
	if snapshot.Status != "" && snapshot.Status != SnapshotStatusReady {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_SNAPSHOT_NOT_READY", "", fmt.Sprintf("snapshot status %q is not promotable", snapshot.Status)))
	}
	if pathUnsafe(snapshot.GeneratedFrom.Root) {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_PATH_LEAK", snapshot.GeneratedFrom.Root, "generated_from.root must be repo-relative and outside ignored directories"))
	}
	for _, changed := range snapshot.GeneratedFrom.ChangedFiles {
		if pathUnsafe(changed) {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_PATH_LEAK", changed, "generated_from.changed_files must be repo-relative and outside ignored directories"))
		}
	}
	scanSnapshotString(&diagnostics, "project_id", snapshot.ProjectID)
	scanSnapshotString(&diagnostics, "resource_id", snapshot.ResourceID)
	scanSnapshotString(&diagnostics, "generated_from.module_path", snapshot.GeneratedFrom.ModulePath)
	seenNodes := make(map[string]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if strings.TrimSpace(node.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_NODE_ID_REQUIRED", node.Path, "node id is required"))
			continue
		}
		if existing, ok := seenNodes[node.ID]; ok {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_DUPLICATE_NODE_ID", node.Path, fmt.Sprintf("node id %s is used by both %s and %s", node.ID, existing.Name, node.Name)))
		}
		seenNodes[node.ID] = node
		if pathUnsafe(node.Path) {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_PATH_LEAK", node.Path, "node path must be repo-relative and outside ignored directories"))
		}
		scanSnapshotString(&diagnostics, "node.name", node.Name)
		scanSnapshotString(&diagnostics, "node.visibility", node.Visibility)
		scanSnapshotString(&diagnostics, "node.source", node.Source)
		scanMetadataValue(&diagnostics, "node.metadata", node.Metadata)
	}
	seenEdges := make(map[string]bool, len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		if strings.TrimSpace(edge.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_EDGE_ID_REQUIRED", "", "edge id is required"))
			continue
		}
		if seenEdges[edge.ID] {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_DUPLICATE_EDGE_ID", "", fmt.Sprintf("edge id %s is duplicated", edge.ID)))
		}
		seenEdges[edge.ID] = true
		if _, ok := seenNodes[edge.FromID]; !ok {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_EDGE_FROM_MISSING", "", fmt.Sprintf("edge %s references missing from_id %s", edge.ID, edge.FromID)))
		}
		if _, ok := seenNodes[edge.ToID]; !ok {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_EDGE_TO_MISSING", "", fmt.Sprintf("edge %s references missing to_id %s", edge.ID, edge.ToID)))
		}
		for _, evidence := range edge.Evidence {
			if pathUnsafe(evidence.Path) {
				diagnostics = append(diagnostics, validationDiagnostic("CODMAP_PATH_LEAK", evidence.Path, "evidence path must be repo-relative and outside ignored directories"))
			}
			scanSnapshotString(&diagnostics, "edge.evidence.collector", evidence.Collector)
		}
		scanMetadataValue(&diagnostics, "edge.metadata", edge.Metadata)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.Severity == DiagnosticError {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_SNAPSHOT_HAS_ERROR_DIAGNOSTIC", diagnostic.Path, fmt.Sprintf("snapshot contains error diagnostic %s", diagnostic.Code)))
		}
		if pathUnsafe(diagnostic.Path) {
			diagnostics = append(diagnostics, validationDiagnostic("CODMAP_PATH_LEAK", diagnostic.Path, "diagnostic path must be repo-relative and outside ignored directories"))
		}
		scanSnapshotString(&diagnostics, "diagnostic.code", diagnostic.Code)
		scanSnapshotString(&diagnostics, "diagnostic.node_id", diagnostic.NodeID)
		scanSnapshotString(&diagnostics, "diagnostic.message", diagnostic.Message)
		scanSnapshotString(&diagnostics, "diagnostic.repair_hint", diagnostic.RepairHint)
	}
	canonical := CanonicalizeSnapshot(snapshot)
	rootDigest, err := computeRootDigest(canonical.Nodes, canonical.Edges, canonical.Diagnostics)
	if err != nil {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_ROOT_DIGEST_RECOMPUTE_FAILED", "", err.Error()))
	} else if canonical.RootDigest != "" && canonical.RootDigest != rootDigest {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_ROOT_DIGEST_DRIFT", "", "root_digest does not match nodes, edges and diagnostics"))
	}
	expectedID := snapshotStableID(canonical)
	if canonical.SnapshotID != "" && canonical.SnapshotID != expectedID {
		diagnostics = append(diagnostics, validationDiagnostic("CODMAP_SNAPSHOT_ID_DRIFT", "", "snapshot_id does not match canonical snapshot content"))
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validationDiagnostic(code, path, message string) Diagnostic {
	return Diagnostic{
		Severity: DiagnosticError,
		Code:     code,
		Path:     cleanRelPath(path),
		Message:  message,
	}
}

func snapshotStableID(snapshot Snapshot) string {
	return StableSnapshotID(
		snapshot.SchemaVersion,
		snapshot.RootDigest,
		snapshot.GeneratedFrom.ModulePath,
		snapshot.GeneratedFrom.InputDigest,
		snapshot.GeneratedFrom.Mode,
		snapshot.GeneratedFrom.ChangedFiles,
		snapshot.ProjectID,
		snapshot.ResourceID,
		snapshot.CollectorVersions,
	)
}

func dedupeNodes(nodes []Node, diagnostics *[]Diagnostic) []Node {
	byID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		if existing, ok := byID[node.ID]; ok {
			if existing.Kind != node.Kind || existing.Name != node.Name || existing.Path != node.Path {
				*diagnostics = append(*diagnostics, Diagnostic{
					Severity: DiagnosticError,
					Code:     "CODMAP_NODE_ID_COLLISION",
					Path:     node.Path,
					NodeID:   node.ID,
					Message:  fmt.Sprintf("node id collision between %s and %s", existing.Name, node.Name),
				})
			}
			continue
		}
		byID[node.ID] = node
	}
	out := make([]Node, 0, len(byID))
	for _, node := range byID {
		out = append(out, node)
	}
	return out
}

func dedupeEdges(edges []Edge, diagnostics *[]Diagnostic) []Edge {
	byID := make(map[string]Edge, len(edges))
	for _, edge := range edges {
		if existing, ok := byID[edge.ID]; ok {
			if existing.FromID != edge.FromID || existing.ToID != edge.ToID || existing.Kind != edge.Kind {
				*diagnostics = append(*diagnostics, Diagnostic{
					Severity: DiagnosticError,
					Code:     "CODMAP_EDGE_ID_COLLISION",
					Message:  fmt.Sprintf("edge id collision on %s", edge.ID),
				})
			}
			continue
		}
		byID[edge.ID] = edge
	}
	out := make([]Edge, 0, len(byID))
	for _, edge := range byID {
		out = append(out, edge)
	}
	return out
}

func computeRootDigest(nodes []Node, edges []Edge, diagnostics []Diagnostic) (string, error) {
	payload := struct {
		Nodes       []Node       `json:"nodes"`
		Edges       []Edge       `json:"edges"`
		Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	}{Nodes: nodes, Edges: edges, Diagnostics: diagnostics}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return DigestBytes(raw), nil
}

func inputDigest(inputs []InputFile) string {
	byPath := make(map[string]string, len(inputs))
	for _, input := range inputs {
		if input.Path == "" || input.Digest == "" {
			continue
		}
		byPath[cleanRelPath(input.Path)] = input.Digest
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	for _, path := range paths {
		buf.WriteString(path)
		buf.WriteByte('\x00')
		buf.WriteString(byPath[path])
		buf.WriteByte('\n')
	}
	return DigestBytes(buf.Bytes())
}

func hasErrorDiagnostics(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticError {
			return true
		}
	}
	return false
}

func pathUnsafe(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if filepath.IsAbs(value) {
		return true
	}
	clean := cleanRelPath(value)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(clean), "/") {
		if deniedPathSegment(segment) {
			return true
		}
	}
	return false
}

func deniedPathSegment(segment string) bool {
	if segment == "" || segment == "." {
		return false
	}
	if strings.HasPrefix(segment, ".coordplane-release-health") {
		return true
	}
	switch segment {
	case ".git", ".multica", ".agents", ".codex", ".agent_context", "vendor":
		return true
	default:
		return false
	}
}

func scanMetadataValue(diagnostics *[]Diagnostic, field string, value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		scanSnapshotString(diagnostics, field, typed)
	case []string:
		for _, item := range typed {
			scanSnapshotString(diagnostics, field, item)
		}
	case []any:
		for _, item := range typed {
			scanMetadataValue(diagnostics, field, item)
		}
	case map[string]any:
		for key, item := range typed {
			scanSnapshotString(diagnostics, field+".key", key)
			scanMetadataValue(diagnostics, field+"."+key, item)
		}
	case map[string]string:
		for key, item := range typed {
			scanSnapshotString(diagnostics, field+".key", key)
			scanSnapshotString(diagnostics, field+"."+key, item)
		}
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return
		}
		scanSnapshotString(diagnostics, field, string(raw))
	}
}

func scanSnapshotString(diagnostics *[]Diagnostic, field, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if containsHostAbsolutePath(value) {
		*diagnostics = append(*diagnostics, validationDiagnostic("CODMAP_OUTPUT_PATH_LEAK", "", fmt.Sprintf("%s contains a host absolute path", field)))
	}
	if containsSecretMarker(value) {
		*diagnostics = append(*diagnostics, validationDiagnostic("CODMAP_OUTPUT_SECRET_LEAK", "", fmt.Sprintf("%s contains a secret-like marker", field)))
	}
}

func containsHostAbsolutePath(value string) bool {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', ',', ';', '(', ')', '[', ']', '{', '}':
			return true
		default:
			return false
		}
	})
	for _, field := range fields {
		token := strings.TrimRight(field, ":.")
		if strings.HasPrefix(token, "/home/") ||
			strings.HasPrefix(token, "/tmp/") ||
			strings.HasPrefix(token, "/var/") ||
			strings.HasPrefix(token, "/Users/") ||
			strings.HasPrefix(token, "/private/") ||
			strings.HasPrefix(token, "/workspace/") ||
			strings.HasPrefix(token, "/mnt/") {
			return true
		}
		if len(token) >= 3 && ((token[0] >= 'A' && token[0] <= 'Z') || (token[0] >= 'a' && token[0] <= 'z')) && token[1] == ':' && (token[2] == '\\' || token[2] == '/') {
			return true
		}
	}
	return false
}

func containsSecretMarker(value string) bool {
	markers := []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----",
		"github_pat_",
		"ghp_",
		"xoxb-",
		"xoxp-",
		"AKIA",
		"AIza",
	}
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	if containsLongSKToken(value) {
		return true
	}
	return false
}

func containsLongSKToken(value string) bool {
	for start := 0; start < len(value); {
		index := strings.Index(value[start:], "sk-")
		if index < 0 {
			return false
		}
		tokenStart := start + index + len("sk-")
		count := 0
		for i := tokenStart; i < len(value); i++ {
			if !isTokenChar(value[i]) {
				break
			}
			count++
		}
		if count >= 20 {
			return true
		}
		start = tokenStart
	}
	return false
}

func isTokenChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func isTrueMetadata(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	value, ok := metadata[key]
	if !ok {
		return false
	}
	if asBool, ok := value.(bool); ok {
		return asBool
	}
	return false
}

func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		if nodes[i].Path != nodes[j].Path {
			return nodes[i].Path < nodes[j].Path
		}
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if edges[i].FromID != edges[j].FromID {
			return edges[i].FromID < edges[j].FromID
		}
		if edges[i].ToID != edges[j].ToID {
			return edges[i].ToID < edges[j].ToID
		}
		return edges[i].ID < edges[j].ID
	})
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Severity != diagnostics[j].Severity {
			return diagnostics[i].Severity < diagnostics[j].Severity
		}
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
}

func sortCollectorVersions(versions []CollectorVersion) {
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Name != versions[j].Name {
			return versions[i].Name < versions[j].Name
		}
		return versions[i].Version < versions[j].Version
	})
}

func sortIndexSlices[K comparable](values map[K][]string) {
	for key := range values {
		sort.Strings(values[key])
	}
}

func sortStringSliceMap(values map[string][]string) {
	for key := range values {
		sort.Strings(values[key])
	}
}
