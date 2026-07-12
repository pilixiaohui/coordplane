package gitrepo

import "context"

// Phase identifies an externally observable initialization boundary. The hook
// is intentionally private to the package and exists only for fault-injection
// tests.
type Phase string

const (
	PhasePartialPrepared   Phase = "partial_prepared"
	PhaseBareInitialized   Phase = "bare_initialized"
	PhaseObjectsImported   Phase = "objects_imported"
	PhaseCanonicalWritten  Phase = "canonical_written"
	PhaseIntegrityVerified Phase = "integrity_verified"
	PhasePromoted          Phase = "promoted"
)

// SourceFact is the immutable result of the read-only source preflight.
type SourceFact struct {
	SourcePath string
	SourceRef  string
	InitialSHA string
}

// Project contains the durable inputs required to initialize or verify one
// daemon-owned repository. Initialize never resolves SourceRef again.
type Project struct {
	ID              string
	OperationID     string
	SourcePath      string
	SourceRef       string
	InitialSHA      string
	ControlRepoPath string
	CanonicalRef    string
}

// Paths are derived solely from the configured repository root and durable
// IDs. A caller can persist Final before invoking Initialize.
type Paths struct {
	Partial string
	Final   string
}

// Fact reports verified Git state. CanonicalSHA always comes from the actual
// canonical ref, never from a cached database value.
type Fact struct {
	ProjectID       string
	ControlRepoPath string
	CanonicalRef    string
	CanonicalSHA    string
	InitialSHA      string
	Bare            bool
}

type phaseFact struct {
	Project Project
	Paths   Paths
}

type phaseHook func(context.Context, Phase, phaseFact) error

// Initializer owns the trusted Git process boundary for project repository
// initialization and verification.
type Initializer struct {
	root      string
	gitPath   string
	phaseHook phaseHook
}
