package codemap

const SchemaVersion = "coordplane.codemap.snapshot.v1"

type SnapshotStatus string

const (
	SnapshotStatusBuilding SnapshotStatus = "building"
	SnapshotStatusPartial  SnapshotStatus = "partial"
	SnapshotStatusReady    SnapshotStatus = "ready"
)

type NodeKind string

const (
	NodeKindRoot               NodeKind = "root"
	NodeKindRequirementDoc     NodeKind = "requirement_doc"
	NodeKindRequirementSection NodeKind = "requirement_section"
	NodeKindAcceptanceClause   NodeKind = "acceptance_clause"
	NodeKindGoPackage          NodeKind = "go_package"
	NodeKindGoFile             NodeKind = "go_file"
	NodeKindGoType             NodeKind = "go_type"
	NodeKindGoFunc             NodeKind = "go_func"
	NodeKindTestCase           NodeKind = "test_case"
	NodeKindTeamConfig         NodeKind = "team_config"
	NodeKindFixture            NodeKind = "fixture"
	NodeKindScript             NodeKind = "script"
	NodeKindMakeTarget         NodeKind = "make_target"
	NodeKindReleaseGate        NodeKind = "release_gate"
)

type EdgeKind string

const (
	EdgeKindContains           EdgeKind = "contains"
	EdgeKindImports            EdgeKind = "imports"
	EdgeKindDefines            EdgeKind = "defines"
	EdgeKindCoveredByTest      EdgeKind = "covered_by_test"
	EdgeKindParticipatesInGate EdgeKind = "participates_in_gate"
)

type DiagnosticSeverity string

const (
	DiagnosticInfo    DiagnosticSeverity = "info"
	DiagnosticWarning DiagnosticSeverity = "warning"
	DiagnosticError   DiagnosticSeverity = "error"
)

type Snapshot struct {
	SchemaVersion     string             `json:"schema_version"`
	SnapshotID        string             `json:"snapshot_id"`
	ProjectID         string             `json:"project_id,omitempty"`
	ResourceID        string             `json:"resource_id,omitempty"`
	Status            SnapshotStatus     `json:"status"`
	RootDigest        string             `json:"root_digest"`
	GeneratedFrom     GeneratedFrom      `json:"generated_from"`
	UpdateSemantics   UpdateSemantics    `json:"update_semantics"`
	CollectorVersions []CollectorVersion `json:"collector_versions"`
	Nodes             []Node             `json:"nodes"`
	Edges             []Edge             `json:"edges"`
	Diagnostics       []Diagnostic       `json:"diagnostics,omitempty"`
	Indexes           Indexes            `json:"indexes"`
}

type GeneratedFrom struct {
	Root         string   `json:"root"`
	ModulePath   string   `json:"module_path,omitempty"`
	Mode         string   `json:"mode"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	InputDigest  string   `json:"input_digest"`
}

type UpdateSemantics struct {
	Consistency           string   `json:"consistency"`
	ReadySnapshot         string   `json:"ready_snapshot"`
	QueryVisibility       string   `json:"query_visibility"`
	SupportedTriggers     []string `json:"supported_triggers"`
	IncrementalUnits      []string `json:"incremental_units"`
	PartialSnapshotPolicy string   `json:"partial_snapshot_policy"`
}

type CollectorVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Node struct {
	ID         string         `json:"id"`
	Kind       NodeKind       `json:"kind"`
	Name       string         `json:"name"`
	Path       string         `json:"path,omitempty"`
	Span       *Span          `json:"span,omitempty"`
	Digest     string         `json:"digest,omitempty"`
	Visibility string         `json:"visibility"`
	Source     string         `json:"source"`
	Confidence float64        `json:"confidence"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type Edge struct {
	ID         string         `json:"id"`
	FromID     string         `json:"from_id"`
	ToID       string         `json:"to_id"`
	Kind       EdgeKind       `json:"kind"`
	Evidence   []Evidence     `json:"evidence,omitempty"`
	Confidence float64        `json:"confidence"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type Evidence struct {
	Path      string `json:"path,omitempty"`
	Span      *Span  `json:"span,omitempty"`
	Collector string `json:"collector"`
}

type Span struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line,omitempty"`
}

type Diagnostic struct {
	Severity   DiagnosticSeverity `json:"severity"`
	Code       string             `json:"code"`
	Path       string             `json:"path,omitempty"`
	NodeID     string             `json:"node_id,omitempty"`
	Message    string             `json:"message"`
	RepairHint string             `json:"repair_hint,omitempty"`
}

type Indexes struct {
	ByKind          map[NodeKind][]string `json:"by_kind,omitempty"`
	ByPath          map[string][]string   `json:"by_path,omitempty"`
	Requirements    map[string][]string   `json:"requirements,omitempty"`
	Packages        map[string]string     `json:"packages,omitempty"`
	Entrypoints     map[string]string     `json:"entrypoints,omitempty"`
	Tests           map[string]string     `json:"tests,omitempty"`
	AcceptanceGates map[string][]string   `json:"acceptance_gates,omitempty"`
}

func DefaultUpdateSemantics() UpdateSemantics {
	return UpdateSemantics{
		Consistency:           "eventual_consistency",
		ReadySnapshot:         "atomic_ready_snapshot",
		QueryVisibility:       "queries read latest_ready_snapshot unless a snapshot_id is pinned",
		SupportedTriggers:     []string{"manual", "local_watcher", "git_hook", "ci", "daemon_event"},
		IncrementalUnits:      []string{"file_digest", "markdown_file", "go_package", "team_config_fixture", "make_target", "script"},
		PartialSnapshotPolicy: "partial snapshots are diagnostic-only and are not promoted to latest_ready_snapshot",
	}
}
