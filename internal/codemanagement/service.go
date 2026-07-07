package codemanagement

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/events"
	"coordplane/internal/ids"
	"coordplane/internal/store"
)

const (
	timeLayout       = "2006-01-02T15:04:05.000000000Z07:00"
	defaultLockTTL   = 5 * time.Minute
	defaultLogMax    = 20
	canonicalTenant  = "default"
	defaultGitAuthor = "CoordPlane"
	defaultGitEmail  = "coordplane@example.invalid"
)

type Service struct {
	db *sql.DB
}

type Repository struct {
	ID              string    `json:"id"`
	SourcePath      string    `json:"source_path"`
	CanonicalBranch string    `json:"canonical_branch"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Workspace struct {
	ID         string    `json:"id"`
	RepoID     string    `json:"repo_id"`
	AgentID    string    `json:"agent_id"`
	RuntimeID  string    `json:"runtime_id,omitempty"`
	ContractID string    `json:"contract_id,omitempty"`
	Path       string    `json:"path"`
	AgentPath  string    `json:"agent_path,omitempty"`
	BaseRef    string    `json:"base_ref"`
	HeadRef    string    `json:"head_ref"`
	Dirty      bool      `json:"dirty"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type GitOperation struct {
	ID             string    `json:"id"`
	OperationType  string    `json:"operation_type"`
	ActorAgentID   string    `json:"actor_agent_id"`
	WorkspaceID    string    `json:"workspace_id,omitempty"`
	RepoID         string    `json:"repo_id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	BeforeRef      string    `json:"before_ref"`
	AfterRef       string    `json:"after_ref"`
	Stdout         string    `json:"stdout,omitempty"`
	Stderr         string    `json:"stderr,omitempty"`
	ExitCode       int       `json:"exit_code"`
	State          string    `json:"state"`
	Feedback       Feedback  `json:"feedback"`
	CreatedAt      time.Time `json:"created_at"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
}

type Feedback struct {
	Message     string   `json:"message"`
	NextActions []string `json:"next_actions,omitempty"`
}

type WorkspacePrepareInput struct {
	RepoID          string `json:"repo_id,omitempty"`
	RepoPath        string `json:"repo_path,omitempty"`
	CanonicalBranch string `json:"canonical_branch,omitempty"`
	WorkspaceRoot   string `json:"workspace_root"`
	AgentID         string `json:"agent_id,omitempty"`
	RuntimeID       string `json:"runtime_id,omitempty"`
	ContractID      string `json:"contract_id,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
}

type WorkspacePrepareResult struct {
	Repository  Repository `json:"repository"`
	Workspace   Workspace  `json:"workspace"`
	OperationID string     `json:"operation_id"`
	Feedback    Feedback   `json:"feedback"`
}

type WorkspaceStatusInput struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id,omitempty"`
}

type WorkspaceStatus struct {
	WorkspaceID  string   `json:"workspace_id"`
	RepoID       string   `json:"repo_id"`
	BaseRef      string   `json:"base_ref"`
	HeadRef      string   `json:"head_ref"`
	CanonicalRef string   `json:"canonical_ref"`
	Dirty        bool     `json:"dirty"`
	StaleBase    bool     `json:"stale_base"`
	State        string   `json:"state"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
}

type WorkspaceSyncInput struct {
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id,omitempty"`
	ExpectedHeadRef string `json:"expected_head_ref,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
}

type WorkspaceSyncResult struct {
	Workspace    Workspace    `json:"workspace"`
	Operation    GitOperation `json:"operation"`
	CanonicalRef string       `json:"canonical_ref"`
	Feedback     Feedback     `json:"feedback"`
}

type GitStatusInput struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id,omitempty"`
}

type GitStatusResult struct {
	WorkspaceID  string   `json:"workspace_id"`
	Porcelain    string   `json:"porcelain"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	Dirty        bool     `json:"dirty"`
}

type GitDiffInput struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	ChangeSetID string `json:"changeset_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
}

type GitDiffResult struct {
	WorkspaceID string `json:"workspace_id"`
	ChangeSetID string `json:"changeset_id,omitempty"`
	Diff        string `json:"diff"`
}

type GitLogInput struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id,omitempty"`
	MaxCount    int    `json:"max_count,omitempty"`
}

type GitLogEntry struct {
	SHA     string `json:"sha"`
	Summary string `json:"summary"`
}

type GitLogResult struct {
	WorkspaceID string        `json:"workspace_id"`
	Entries     []GitLogEntry `json:"entries"`
}

type GitCommitInput struct {
	WorkspaceID     string   `json:"workspace_id"`
	AgentID         string   `json:"agent_id,omitempty"`
	Message         string   `json:"message"`
	Paths           []string `json:"paths"`
	ExpectedHeadRef string   `json:"expected_head_ref,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
}

type GitCommitResult struct {
	Workspace Workspace    `json:"workspace"`
	Operation GitOperation `json:"operation"`
	CommitSHA string       `json:"commit_sha"`
	Feedback  Feedback     `json:"feedback"`
}

type SubmitChangeSetInput struct {
	WorkspaceID     string   `json:"workspace_id"`
	AgentID         string   `json:"agent_id,omitempty"`
	ContractID      string   `json:"contract_id,omitempty"`
	Summary         string   `json:"summary"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
	ExpectedHeadRef string   `json:"expected_head_ref,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
}

type ChangeSet struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspace_id"`
	RepoID       string    `json:"repo_id"`
	ContractID   string    `json:"contract_id,omitempty"`
	BaseRef      string    `json:"base_ref"`
	HeadRef      string    `json:"head_ref"`
	CommitIDs    []string  `json:"commit_ids"`
	Summary      string    `json:"summary"`
	EvidenceRefs []string  `json:"evidence_refs,omitempty"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type SubmitChangeSetResult struct {
	ChangeSet ChangeSet    `json:"changeset"`
	Operation GitOperation `json:"operation"`
	Feedback  Feedback     `json:"feedback"`
}

type AbandonChangeSetInput struct {
	ChangeSetID     string `json:"changeset_id"`
	AgentID         string `json:"agent_id,omitempty"`
	ExpectedHeadRef string `json:"expected_head_ref,omitempty"`
	Reason          string `json:"reason,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
}

type AbandonChangeSetResult struct {
	ChangeSet ChangeSet    `json:"changeset"`
	Operation GitOperation `json:"operation"`
	Feedback  Feedback     `json:"feedback"`
}

type MergeAttempt struct {
	ID              string    `json:"id"`
	ChangeSetID     string    `json:"changeset_id"`
	RepoID          string    `json:"repo_id"`
	WorkspaceID     string    `json:"workspace_id"`
	TargetRef       string    `json:"target_ref"`
	IntegrationPath string    `json:"integration_path,omitempty"`
	BaseBefore      string    `json:"base_before"`
	ResultRef       string    `json:"result_ref,omitempty"`
	State           string    `json:"state"`
	ConflictSetID   string    `json:"conflict_set_id,omitempty"`
	OperationID     string    `json:"operation_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type ConflictSet struct {
	ID             string    `json:"id"`
	MergeAttemptID string    `json:"merge_attempt_id"`
	Files          []string  `json:"files"`
	Summary        string    `json:"summary"`
	State          string    `json:"state"`
	ResolvedBy     string    `json:"resolved_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type RollbackPoint struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	RepoID      string    `json:"repo_id"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	TargetRef   string    `json:"target_ref"`
	BeforeRef   string    `json:"before_ref"`
	AfterRef    string    `json:"after_ref"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type MergePreviewInput struct {
	ChangeSetID       string `json:"changeset_id"`
	AgentID           string `json:"agent_id,omitempty"`
	TargetRef         string `json:"target_ref,omitempty"`
	ExpectedTargetRef string `json:"expected_target_ref,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
}

type MergePreviewResult struct {
	MergeAttempt MergeAttempt `json:"merge_attempt"`
	Operation    GitOperation `json:"operation"`
	Feedback     Feedback     `json:"feedback"`
}

type MergeApplyInput struct {
	MergeAttemptID    string `json:"merge_attempt_id"`
	AgentID           string `json:"agent_id,omitempty"`
	ExpectedTargetRef string `json:"expected_target_ref,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
}

type MergeApplyResult struct {
	MergeAttempt  MergeAttempt  `json:"merge_attempt"`
	RollbackPoint RollbackPoint `json:"rollback_point"`
	Operation     GitOperation  `json:"operation"`
	AppliedRef    string        `json:"applied_ref"`
	Feedback      Feedback      `json:"feedback"`
}

type ConflictListInput struct {
	MergeAttemptID string `json:"merge_attempt_id,omitempty"`
	ConflictSetID  string `json:"conflict_set_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
}

type ConflictListResult struct {
	MergeAttempt MergeAttempt `json:"merge_attempt"`
	ConflictSet  ConflictSet  `json:"conflict_set"`
}

type ResolveMergeInput struct {
	MergeAttemptID    string `json:"merge_attempt_id"`
	AgentID           string `json:"agent_id,omitempty"`
	ResolvedHeadRef   string `json:"resolved_head_ref,omitempty"`
	ExpectedTargetRef string `json:"expected_target_ref,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
}

type ResolveMergeResult struct {
	MergeAttempt MergeAttempt `json:"merge_attempt"`
	ConflictSet  ConflictSet  `json:"conflict_set"`
	Operation    GitOperation `json:"operation"`
	Feedback     Feedback     `json:"feedback"`
}

type AbortMergeInput struct {
	MergeAttemptID string `json:"merge_attempt_id"`
	AgentID        string `json:"agent_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type AbortMergeResult struct {
	MergeAttempt MergeAttempt `json:"merge_attempt"`
	ConflictSet  *ConflictSet `json:"conflict_set,omitempty"`
	Operation    GitOperation `json:"operation"`
	Feedback     Feedback     `json:"feedback"`
}

type RollbackInput struct {
	RollbackPointID   string `json:"rollback_point_id,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	ExpectedTargetRef string `json:"expected_target_ref,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
}

type RollbackResult struct {
	RollbackPoint RollbackPoint `json:"rollback_point"`
	Operation     GitOperation  `json:"operation"`
	RestoredRef   string        `json:"restored_ref"`
	Feedback      Feedback      `json:"feedback"`
}

type RecoverOperationsInput struct {
	OperationID string `json:"operation_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
}

type RecoverOperationsResult struct {
	RecoveredOperations []GitOperation `json:"recovered_operations"`
	Feedback            Feedback       `json:"feedback"`
}

type gitRun struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type writeOperation struct {
	ID            string
	OperationType string
	AgentID       string
	WorkspaceID   string
	RepoID        string
	BeforeRef     string
	CreatedAt     time.Time
}

func writeOperationFromGitOperation(op GitOperation) writeOperation {
	return writeOperation{
		ID:            op.ID,
		OperationType: op.OperationType,
		AgentID:       op.ActorAgentID,
		WorkspaceID:   op.WorkspaceID,
		RepoID:        op.RepoID,
		BeforeRef:     op.BeforeRef,
		CreatedAt:     op.CreatedAt,
	}
}

func NewService(s *store.Store) *Service {
	return &Service{db: s.DB()}
}

func (s *Service) WorkspacePrepare(ctx context.Context, in WorkspacePrepareInput) capability.Response[WorkspacePrepareResult] {
	if in.AgentID == "" {
		return rejected[WorkspacePrepareResult]("GIT_AGENT_REQUIRED", "agent identity is required", []string{"workspace.prepare"}, false)
	}
	workspaceRoot, agentRoot, bridgeResp := s.resolveWorkspacePrepareRoot(ctx, in)
	if bridgeResp != nil {
		return *bridgeResp
	}
	in.WorkspaceRoot = workspaceRoot
	if in.CanonicalBranch == "" {
		in.CanonicalBranch = "main"
	}
	repo, needsInsert, err := s.resolveRepoForPrepare(ctx, in)
	if err != nil {
		return errored[WorkspacePrepareResult]("GIT_REPO_PREPARE_FAILED", err)
	}
	canonicalRef, err := gitHead(ctx, repo.SourcePath, repo.CanonicalBranch)
	if err != nil {
		return errored[WorkspacePrepareResult]("GIT_CANONICAL_REF_FAILED", err)
	}
	workspaceID, err := ids.New("ws")
	if err != nil {
		return errored[WorkspacePrepareResult]("WORKSPACE_ID_FAILED", err)
	}
	workspacePath := filepath.Join(in.WorkspaceRoot, workspaceID)
	workspace := Workspace{
		ID:         workspaceID,
		RepoID:     repo.ID,
		AgentID:    in.AgentID,
		RuntimeID:  in.RuntimeID,
		ContractID: in.ContractID,
		Path:       workspacePath,
		AgentPath:  agentWorkspacePath(agentRoot, workspaceID),
		BaseRef:    canonicalRef,
		HeadRef:    canonicalRef,
		State:      "preparing",
	}
	op, resp := s.startPrepareWrite(ctx, in.AgentID, repo, needsInsert, workspace, in.IdempotencyKey)
	if resp != nil {
		return copyRejected[WorkspacePrepareResult](*resp)
	}
	if err := os.MkdirAll(in.WorkspaceRoot, 0o755); err != nil {
		completed := s.completeWrite(ctx, op, "failed", "", gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace root could not be created"})
		return failed[WorkspacePrepareResult]("WORKSPACE_PREPARE_FAILED", err.Error(), completed)
	}
	run := runGit(ctx, "", "clone", "--quiet", "--branch", repo.CanonicalBranch, repo.SourcePath, workspace.Path)
	if run.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", "", run, Feedback{Message: "git clone failed"})
		_ = s.markWorkspaceState(ctx, workspace.ID, "error", canonicalRef, canonicalRef, false)
		return failed[WorkspacePrepareResult]("WORKSPACE_PREPARE_FAILED", run.Stderr, completed)
	}
	head, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", "", gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace HEAD could not be read"})
		_ = s.markWorkspaceState(ctx, workspace.ID, "error", canonicalRef, canonicalRef, false)
		return failed[WorkspacePrepareResult]("WORKSPACE_PREPARE_FAILED", err.Error(), completed)
	}
	workspace.BaseRef = head
	workspace.HeadRef = head
	workspace.State = "ready"
	if err := s.markWorkspaceState(ctx, workspace.ID, "ready", head, head, false); err != nil {
		completed := s.completeWrite(ctx, op, "failed", head, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace state could not be persisted"})
		return failed[WorkspacePrepareResult]("WORKSPACE_PREPARE_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", head, run, Feedback{Message: "workspace prepared", NextActions: []string{"workspace.status", "git.status"}})
	return capability.Accepted(WorkspacePrepareResult{
		Repository:  repo,
		Workspace:   workspace,
		OperationID: completed.ID,
		Feedback:    completed.Feedback,
	})
}

func (s *Service) resolveWorkspacePrepareRoot(ctx context.Context, in WorkspacePrepareInput) (string, string, *capability.Response[WorkspacePrepareResult]) {
	if in.RuntimeID == "" {
		if in.WorkspaceRoot == "" {
			resp := rejected[WorkspacePrepareResult]("WORKSPACE_ROOT_REQUIRED", "workspace_root is required", []string{"workspace.prepare"}, false)
			return "", "", &resp
		}
		return in.WorkspaceRoot, "", nil
	}
	var runtimeKind, state, workspacePath, hostWorkspace string
	err := s.db.QueryRowContext(ctx, `
SELECT runtime_kind, state, workspace_path, host_workspace_ref
FROM runtime_instances
WHERE runtime_id = ? AND agent_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`, in.RuntimeID, in.AgentID).Scan(&runtimeKind, &state, &workspacePath, &hostWorkspace)
	if errors.Is(err, sql.ErrNoRows) {
		resp := rejected[WorkspacePrepareResult](
			"WORKSPACE_BRIDGE_UNAVAILABLE",
			"runtime workspace bridge was not found for this agent",
			[]string{"assignment.next"},
			true,
		)
		return "", "", &resp
	}
	if err != nil {
		resp := capability.Error[WorkspacePrepareResult]("WORKSPACE_BRIDGE_LOOKUP_FAILED", err.Error(), true)
		return "", "", &resp
	}
	if runtimeKind != "docker" {
		if in.WorkspaceRoot == "" {
			resp := rejected[WorkspacePrepareResult]("WORKSPACE_ROOT_REQUIRED", "workspace_root is required", []string{"workspace.prepare"}, false)
			return "", "", &resp
		}
		return in.WorkspaceRoot, "", nil
	}
	if state != "ready" || hostWorkspace == "" || workspacePath == "" {
		resp := rejected[WorkspacePrepareResult](
			"WORKSPACE_BRIDGE_UNAVAILABLE",
			"docker runtime workspace bridge is not ready",
			[]string{"assignment.next"},
			true,
		)
		return "", "", &resp
	}
	requestedRoot := path.Clean(strings.TrimSpace(in.WorkspaceRoot))
	if requestedRoot == "." || requestedRoot == "" {
		requestedRoot = workspacePath
	}
	containerRoot := path.Clean(workspacePath)
	if requestedRoot != containerRoot && !strings.HasPrefix(requestedRoot, containerRoot+"/") {
		resp := rejected[WorkspacePrepareResult](
			"WORKSPACE_ROOT_REJECTED",
			"docker workspace_root must be inside the active runtime workspace",
			[]string{"workspace.prepare"},
			false,
		)
		return "", "", &resp
	}
	rel := "."
	if requestedRoot != containerRoot {
		rel = strings.TrimPrefix(requestedRoot, containerRoot+"/")
	}
	hostRoot := filepath.Clean(hostWorkspace)
	if rel != "." {
		hostRoot = filepath.Join(hostRoot, filepath.FromSlash(rel))
	}
	return hostRoot, requestedRoot, nil
}

func agentWorkspacePath(root, workspaceID string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return path.Join(path.Clean(root), workspaceID)
}

func (s *Service) WorkspaceStatus(ctx context.Context, in WorkspaceStatusInput) capability.Response[WorkspaceStatus] {
	workspace, repo, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[WorkspaceStatus](*resp)
	}
	head, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		return errored[WorkspaceStatus]("WORKSPACE_STATUS_FAILED", err)
	}
	canonicalRef, err := gitHead(ctx, repo.SourcePath, repo.CanonicalBranch)
	if err != nil {
		return errored[WorkspaceStatus]("WORKSPACE_STATUS_FAILED", err)
	}
	changed, dirty, err := gitChangedPaths(ctx, workspace.Path)
	if err != nil {
		return errored[WorkspaceStatus]("WORKSPACE_STATUS_FAILED", err)
	}
	return capability.Accepted(WorkspaceStatus{
		WorkspaceID:  workspace.ID,
		RepoID:       workspace.RepoID,
		BaseRef:      workspace.BaseRef,
		HeadRef:      head,
		CanonicalRef: canonicalRef,
		Dirty:        dirty,
		StaleBase:    workspace.BaseRef != canonicalRef,
		State:        workspace.State,
		ChangedPaths: changed,
	})
}

func (s *Service) WorkspaceSync(ctx context.Context, in WorkspaceSyncInput) capability.Response[WorkspaceSyncResult] {
	workspace, repo, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[WorkspaceSyncResult](*resp)
	}
	before, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		return errored[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "workspace.sync", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[WorkspaceSyncResult](*startResp)
	}
	if reject := s.rejectExpectedHead(ctx, op, before, in.ExpectedHeadRef); reject != nil {
		return copyRejected[WorkspaceSyncResult](*reject)
	}
	changed, dirty, err := gitChangedPaths(ctx, workspace.Path)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace dirty state could not be read"})
		return failed[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", err.Error(), completed)
	}
	if dirty {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "workspace has uncommitted changes", NextActions: []string{"git.diff", "git.commit"}})
		resp := capability.Rejected[WorkspaceSyncResult](
			"WORKSPACE_DIRTY",
			"workspace has uncommitted changes",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("workspace_id", workspace.ID),
			capability.WithRepairHint("commit or inspect the dirty paths before syncing"),
			capability.WithAllowedNextActions("git.diff", "git.commit"),
			capability.WithRetryable(true),
		)
		_ = changed
		return resp
	}
	if before != workspace.BaseRef {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "workspace has local commits; sync will not merge silently", NextActions: []string{"changeset.submit", "git.diff"}})
		return capability.Rejected[WorkspaceSyncResult](
			"NO_SILENT_MERGE",
			"workspace has local commits; workspace.sync will not merge or discard them",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("workspace_id", workspace.ID),
			capability.WithRepairHint("submit or abandon the local changeset before syncing"),
			capability.WithAllowedNextActions("changeset.submit", "git.diff", "changeset.abandon"),
			capability.WithRetryable(false),
		)
	}
	fetch := runGit(ctx, workspace.Path, "fetch", "origin", repo.CanonicalBranch)
	if fetch.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", before, fetch, Feedback{Message: "git fetch failed"})
		return failed[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", fetch.Stderr, completed)
	}
	reset := runGit(ctx, workspace.Path, "reset", "--hard", "origin/"+repo.CanonicalBranch)
	if reset.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", before, reset, Feedback{Message: "git reset failed"})
		return failed[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", reset.Stderr, completed)
	}
	after, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace HEAD could not be read after sync"})
		return failed[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", err.Error(), completed)
	}
	if err := s.markWorkspaceState(ctx, workspace.ID, "ready", after, after, false); err != nil {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace sync state could not be persisted"})
		return failed[WorkspaceSyncResult]("WORKSPACE_SYNC_FAILED", err.Error(), completed)
	}
	workspace.BaseRef = after
	workspace.HeadRef = after
	workspace.Dirty = false
	workspace.State = "ready"
	completed := s.completeWrite(ctx, op, "succeeded", after, gitRun{Stdout: fetch.Stdout + reset.Stdout, Stderr: fetch.Stderr + reset.Stderr}, Feedback{Message: "workspace synced", NextActions: []string{"workspace.status", "git.status"}})
	return capability.Accepted(WorkspaceSyncResult{Workspace: workspace, Operation: completed, CanonicalRef: after, Feedback: completed.Feedback})
}

func (s *Service) GitStatus(ctx context.Context, in GitStatusInput) capability.Response[GitStatusResult] {
	workspace, _, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[GitStatusResult](*resp)
	}
	changed, dirty, err := gitChangedPaths(ctx, workspace.Path)
	if err != nil {
		return errored[GitStatusResult]("GIT_STATUS_FAILED", err)
	}
	run := runGit(ctx, workspace.Path, "status", "--porcelain")
	if run.ExitCode != 0 {
		return capability.Error[GitStatusResult]("GIT_STATUS_FAILED", run.Stderr, false)
	}
	return capability.Accepted(GitStatusResult{WorkspaceID: workspace.ID, Porcelain: run.Stdout, ChangedPaths: changed, Dirty: dirty})
}

func (s *Service) GitDiff(ctx context.Context, in GitDiffInput) capability.Response[GitDiffResult] {
	if in.ChangeSetID != "" {
		return s.gitChangeSetDiff(ctx, in)
	}
	workspace, _, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[GitDiffResult](*resp)
	}
	run := runGit(ctx, workspace.Path, "diff")
	if run.ExitCode != 0 {
		return capability.Error[GitDiffResult]("GIT_DIFF_FAILED", run.Stderr, false)
	}
	return capability.Accepted(GitDiffResult{WorkspaceID: workspace.ID, Diff: run.Stdout})
}

func (s *Service) GitLog(ctx context.Context, in GitLogInput) capability.Response[GitLogResult] {
	workspace, _, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[GitLogResult](*resp)
	}
	maxCount := in.MaxCount
	if maxCount <= 0 || maxCount > 100 {
		maxCount = defaultLogMax
	}
	run := runGit(ctx, workspace.Path, "log", "--format=%H%x00%s", "-n", strconv.Itoa(maxCount))
	if run.ExitCode != 0 {
		return capability.Error[GitLogResult]("GIT_LOG_FAILED", run.Stderr, false)
	}
	return capability.Accepted(GitLogResult{WorkspaceID: workspace.ID, Entries: parseLog(run.Stdout)})
}

func (s *Service) GitCommit(ctx context.Context, in GitCommitInput) capability.Response[GitCommitResult] {
	workspace, repo, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[GitCommitResult](*resp)
	}
	before, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		return errored[GitCommitResult]("GIT_COMMIT_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.commit", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[GitCommitResult](*startResp)
	}
	if reject := s.rejectExpectedHead(ctx, op, before, in.ExpectedHeadRef); reject != nil {
		return copyRejected[GitCommitResult](*reject)
	}
	if strings.TrimSpace(in.Message) == "" {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "commit message is required", NextActions: []string{"git.status", "git.diff"}})
		return capability.Rejected[GitCommitResult](
			"GIT_COMMIT_MESSAGE_REQUIRED",
			"git.commit requires a commit message",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithRepairHint("retry git.commit with a non-empty message"),
			capability.WithAllowedNextActions("git.status", "git.diff"),
			capability.WithRetryable(false),
		)
	}
	if len(in.Paths) == 0 {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "explicit paths are required", NextActions: []string{"git.status", "git.diff"}})
		return capability.Rejected[GitCommitResult](
			"GIT_COMMIT_PATHS_REQUIRED",
			"git.commit requires explicit paths",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithRepairHint("retry with the relative paths to include in the commit"),
			capability.WithAllowedNextActions("git.status", "git.diff"),
			capability.WithRetryable(false),
		)
	}
	if err := validatePaths(in.Paths); err != nil {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: err.Error(), NextActions: []string{"git.status", "git.diff"}})
		return capability.Rejected[GitCommitResult](
			"GIT_COMMIT_PATHS_INVALID",
			err.Error(),
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithRepairHint("paths must be relative workspace paths"),
			capability.WithAllowedNextActions("git.status", "git.diff"),
			capability.WithRetryable(false),
		)
	}
	addArgs := append([]string{"add", "--"}, in.Paths...)
	add := runGit(ctx, workspace.Path, addArgs...)
	if add.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", before, add, Feedback{Message: "git add failed"})
		return failed[GitCommitResult]("GIT_COMMIT_FAILED", add.Stderr, completed)
	}
	commitArgs := []string{"-c", "user.name=" + defaultGitAuthor, "-c", "user.email=" + defaultGitEmail, "commit", "--only", "-m", in.Message, "--"}
	commitArgs = append(commitArgs, in.Paths...)
	commit := runGit(ctx, workspace.Path, commitArgs...)
	if commit.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "rejected", before, commit, Feedback{Message: "git commit did not create a commit", NextActions: []string{"git.status", "git.diff"}})
		return capability.Rejected[GitCommitResult](
			"GIT_COMMIT_REJECTED",
			firstNonEmpty(commit.Stderr, commit.Stdout, "git commit did not create a commit"),
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("workspace_id", workspace.ID),
			capability.WithRepairHint("inspect git.status and retry with changed paths"),
			capability.WithAllowedNextActions("git.status", "git.diff", "git.commit"),
			capability.WithRetryable(true),
		)
	}
	after, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "commit HEAD could not be read"})
		return failed[GitCommitResult]("GIT_COMMIT_FAILED", err.Error(), completed)
	}
	changed, dirty, err := gitChangedPaths(ctx, workspace.Path)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace dirty state could not be read after commit"})
		return failed[GitCommitResult]("GIT_COMMIT_FAILED", err.Error(), completed)
	}
	if err := s.markWorkspaceState(ctx, workspace.ID, "ready", workspace.BaseRef, after, dirty); err != nil {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace commit state could not be persisted"})
		return failed[GitCommitResult]("GIT_COMMIT_FAILED", err.Error(), completed)
	}
	workspace.HeadRef = after
	workspace.Dirty = dirty
	completed := s.completeWrite(ctx, op, "succeeded", after, gitRun{Stdout: add.Stdout + commit.Stdout, Stderr: add.Stderr + commit.Stderr}, Feedback{Message: "commit created", NextActions: []string{"changeset.submit", "git.log"}})
	_ = changed
	return capability.Accepted(GitCommitResult{Workspace: workspace, Operation: completed, CommitSHA: after, Feedback: completed.Feedback})
}

func (s *Service) SubmitChangeSet(ctx context.Context, in SubmitChangeSetInput) capability.Response[SubmitChangeSetResult] {
	workspace, repo, resp := s.workspaceForAgent(ctx, in.WorkspaceID, in.AgentID)
	if resp != nil {
		return copyRejected[SubmitChangeSetResult](*resp)
	}
	before, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		return errored[SubmitChangeSetResult]("CHANGESET_SUBMIT_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "changeset.submit", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[SubmitChangeSetResult](*startResp)
	}
	if reject := s.rejectExpectedHead(ctx, op, before, in.ExpectedHeadRef); reject != nil {
		return copyRejected[SubmitChangeSetResult](*reject)
	}
	if strings.TrimSpace(in.Summary) == "" {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "changeset summary is required", NextActions: []string{"git.log", "git.diff"}})
		return capability.Rejected[SubmitChangeSetResult](
			"CHANGESET_SUMMARY_REQUIRED",
			"changeset.submit requires a summary",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithRepairHint("retry with a concise changeset summary"),
			capability.WithAllowedNextActions("git.log", "git.diff"),
			capability.WithRetryable(false),
		)
	}
	_, dirty, err := gitChangedPaths(ctx, workspace.Path)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace dirty state could not be read"})
		return failed[SubmitChangeSetResult]("CHANGESET_SUBMIT_FAILED", err.Error(), completed)
	}
	if dirty {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "dirty workspace cannot be submitted", NextActions: []string{"git.status", "git.commit"}})
		return capability.Rejected[SubmitChangeSetResult](
			"WORKSPACE_DIRTY",
			"changeset.submit requires a clean workspace",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("workspace_id", workspace.ID),
			capability.WithRepairHint("commit or abandon uncommitted changes before submitting"),
			capability.WithAllowedNextActions("git.status", "git.diff", "git.commit"),
			capability.WithRetryable(true),
		)
	}
	commits, err := gitCommitRange(ctx, workspace.Path, workspace.BaseRef, before)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "changeset commit range could not be read"})
		return failed[SubmitChangeSetResult]("CHANGESET_SUBMIT_FAILED", err.Error(), completed)
	}
	if len(commits) == 0 {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "no commits to submit", NextActions: []string{"git.status", "git.commit"}})
		return capability.Rejected[SubmitChangeSetResult](
			"NO_CHANGESET_COMMITS",
			"changeset.submit requires at least one workspace commit beyond base_ref",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithRepairHint("create a commit first"),
			capability.WithAllowedNextActions("git.status", "git.commit"),
			capability.WithRetryable(false),
		)
	}
	changeset, err := s.insertChangeSet(ctx, workspace, in, commits, before)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "changeset could not be persisted"})
		return failed[SubmitChangeSetResult]("CHANGESET_SUBMIT_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", before, gitRun{}, Feedback{Message: "changeset submitted", NextActions: []string{"changeset.abandon"}})
	return capability.Accepted(SubmitChangeSetResult{ChangeSet: changeset, Operation: completed, Feedback: completed.Feedback})
}

func (s *Service) AbandonChangeSet(ctx context.Context, in AbandonChangeSetInput) capability.Response[AbandonChangeSetResult] {
	changeset, workspace, repo, resp := s.changeSetForAgent(ctx, in.ChangeSetID, in.AgentID)
	if resp != nil {
		return copyRejected[AbandonChangeSetResult](*resp)
	}
	before, err := gitHead(ctx, workspace.Path, "HEAD")
	if err != nil {
		return errored[AbandonChangeSetResult]("CHANGESET_ABANDON_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "changeset.abandon", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[AbandonChangeSetResult](*startResp)
	}
	if reject := s.rejectExpectedHead(ctx, op, before, in.ExpectedHeadRef); reject != nil {
		return copyRejected[AbandonChangeSetResult](*reject)
	}
	if changeset.State == "abandoned" {
		completed := s.completeWrite(ctx, op, "succeeded", before, gitRun{}, Feedback{Message: "changeset already abandoned"})
		return capability.Accepted(AbandonChangeSetResult{ChangeSet: changeset, Operation: completed, Feedback: completed.Feedback})
	}
	if changeset.State != "submitted" && changeset.State != "draft" {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "changeset state cannot be abandoned"})
		return capability.Rejected[AbandonChangeSetResult](
			"CHANGESET_STATE_INVALID",
			"only draft or submitted changesets can be abandoned",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("changeset_id", changeset.ID),
			capability.WithRepairHint("inspect the changeset state before abandoning it"),
			capability.WithAllowedNextActions("git.log", "git.diff"),
			capability.WithRetryable(false),
		)
	}
	updated, err := s.updateChangeSetState(ctx, changeset.ID, "abandoned")
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "changeset state could not be persisted"})
		return failed[AbandonChangeSetResult]("CHANGESET_ABANDON_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", before, gitRun{}, Feedback{Message: firstNonEmpty(in.Reason, "changeset abandoned"), NextActions: []string{"workspace.status", "git.diff"}})
	return capability.Accepted(AbandonChangeSetResult{ChangeSet: updated, Operation: completed, Feedback: completed.Feedback})
}

func (s *Service) MergePreview(ctx context.Context, in MergePreviewInput) capability.Response[MergePreviewResult] {
	changeset, workspace, repo, resp := s.changeSetForAgent(ctx, in.ChangeSetID, in.AgentID)
	if resp != nil {
		return copyRejected[MergePreviewResult](*resp)
	}
	targetRef := firstNonEmpty(in.TargetRef, repo.CanonicalBranch)
	before, err := gitHead(ctx, repo.SourcePath, targetRef)
	if err != nil {
		return errored[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.merge_preview", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[MergePreviewResult](*startResp)
	}
	if reject := s.rejectExpectedTarget(ctx, op, targetRef, before, in.ExpectedTargetRef); reject != nil {
		return copyRejected[MergePreviewResult](*reject)
	}
	integrationPath, resultRef, conflictFiles, run, err := createMergePreview(ctx, repo, workspace, changeset.HeadRef, targetRef)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge preview could not be created"})
		return failed[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err.Error(), completed)
	}
	if len(conflictFiles) > 0 {
		attempt, err := s.insertMergeAttempt(ctx, MergeAttempt{
			ChangeSetID:     changeset.ID,
			RepoID:          repo.ID,
			WorkspaceID:     workspace.ID,
			TargetRef:       targetRef,
			IntegrationPath: integrationPath,
			BaseBefore:      before,
			State:           "conflicted",
			OperationID:     op.ID,
		})
		if err != nil {
			completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "conflicted merge attempt could not be persisted"})
			return failed[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err.Error(), completed)
		}
		conflicts, err := s.insertConflictSet(ctx, attempt.ID, conflictFiles, "merge conflicts require manual resolution")
		if err != nil {
			completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "conflict set could not be persisted"})
			return failed[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err.Error(), completed)
		}
		attempt, err = s.updateMergeAttemptConflict(ctx, attempt.ID, conflicts.ID)
		if err != nil {
			completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge attempt conflict link could not be persisted"})
			return failed[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err.Error(), completed)
		}
		completed := s.completeWrite(ctx, op, "rejected", before, run, Feedback{Message: "merge conflicts found", NextActions: []string{"git.conflicts", "git.resolve", "git.abort"}})
		return capability.Rejected[MergePreviewResult](
			"MERGE_CONFLICTS_FOUND",
			"merge preview found conflicts and did not update the target ref",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("changeset_id", changeset.ID),
			capability.WithCanonicalID("merge_attempt_id", attempt.ID),
			capability.WithCanonicalID("conflict_set_id", conflicts.ID),
			capability.WithRepairHint("inspect git.conflicts, commit a manual resolution, then call git.resolve or git.abort"),
			capability.WithAllowedNextActions("git.conflicts", "git.resolve", "git.abort"),
			capability.WithRetryable(false),
		)
	}
	attempt, err := s.insertMergeAttempt(ctx, MergeAttempt{
		ChangeSetID:     changeset.ID,
		RepoID:          repo.ID,
		WorkspaceID:     workspace.ID,
		TargetRef:       targetRef,
		IntegrationPath: integrationPath,
		BaseBefore:      before,
		ResultRef:       resultRef,
		State:           "clean",
		OperationID:     op.ID,
	})
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge attempt could not be persisted"})
		return failed[MergePreviewResult]("GIT_MERGE_PREVIEW_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", resultRef, run, Feedback{Message: "merge preview is clean", NextActions: []string{"git.merge_apply", "git.abort"}})
	return capability.Accepted(MergePreviewResult{MergeAttempt: attempt, Operation: completed, Feedback: completed.Feedback})
}

func (s *Service) MergeApply(ctx context.Context, in MergeApplyInput) capability.Response[MergeApplyResult] {
	attempt, changeset, workspace, repo, resp := s.mergeAttemptForAgent(ctx, in.MergeAttemptID, in.AgentID)
	if resp != nil {
		return copyRejected[MergeApplyResult](*resp)
	}
	if attempt.State != "clean" && attempt.State != "resolved" {
		return rejected[MergeApplyResult]("MERGE_ATTEMPT_STATE_INVALID", "only clean or resolved merge attempts can be applied", []string{"git.merge_preview", "git.resolve"}, false)
	}
	if attempt.ResultRef == "" || attempt.IntegrationPath == "" {
		return rejected[MergeApplyResult]("MERGE_RESULT_REQUIRED", "merge apply requires a durable preview result", []string{"git.merge_preview"}, false)
	}
	before, err := gitHead(ctx, repo.SourcePath, attempt.TargetRef)
	if err != nil {
		return errored[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.merge_apply", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[MergeApplyResult](*startResp)
	}
	if reject := s.rejectExpectedTarget(ctx, op, attempt.TargetRef, before, in.ExpectedTargetRef); reject != nil {
		return copyRejected[MergeApplyResult](*reject)
	}
	if before != attempt.BaseBefore {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "target ref changed after merge preview", NextActions: []string{"workspace.sync", "git.merge_preview"}})
		return capability.Rejected[MergeApplyResult](
			"STALE_TARGET_REF",
			"target ref changed after merge preview; merge was not applied",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("merge_attempt_id", attempt.ID),
			capability.WithRepairHint("sync the workspace if needed, then run a new merge preview against the current target ref"),
			capability.WithAllowedNextActions("workspace.sync", "git.merge_preview"),
			capability.WithRetryable(true),
		)
	}
	run := fetchAndUpdateRef(ctx, repo.SourcePath, attempt.IntegrationPath, attempt.ResultRef, attempt.TargetRef, before)
	if run.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", before, run, Feedback{Message: "target ref could not be updated"})
		return failed[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", firstNonEmpty(run.Stderr, run.Stdout), completed)
	}
	after, err := gitHead(ctx, repo.SourcePath, attempt.TargetRef)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "target ref could not be read after merge apply"})
		return failed[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", err.Error(), completed)
	}
	if after != attempt.ResultRef {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: "target ref did not move to reviewed merge result", ExitCode: 1}, Feedback{Message: "target ref did not match reviewed merge result"})
		return failed[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", "target ref did not match reviewed merge result", completed)
	}
	rollback, err := s.insertRollbackPoint(ctx, RollbackPoint{
		OperationID: op.ID,
		RepoID:      repo.ID,
		WorkspaceID: workspace.ID,
		TargetRef:   attempt.TargetRef,
		BeforeRef:   before,
		AfterRef:    after,
		State:       "available",
	})
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "rollback point could not be persisted"})
		return failed[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", err.Error(), completed)
	}
	attempt, err = s.updateMergeAttemptState(ctx, attempt.ID, "applied", after, attempt.IntegrationPath)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", after, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge attempt state could not be persisted"})
		return failed[MergeApplyResult]("GIT_MERGE_APPLY_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", after, run, Feedback{Message: "merge result applied", NextActions: []string{"git.rollback", "git.log"}})
	_ = changeset
	return capability.Accepted(MergeApplyResult{MergeAttempt: attempt, RollbackPoint: rollback, Operation: completed, AppliedRef: after, Feedback: completed.Feedback})
}

func (s *Service) Conflicts(ctx context.Context, in ConflictListInput) capability.Response[ConflictListResult] {
	attempt, _, _, _, resp := s.mergeAttemptForAgent(ctx, in.MergeAttemptID, in.AgentID)
	if resp != nil {
		return copyRejected[ConflictListResult](*resp)
	}
	if in.ConflictSetID != "" && attempt.ConflictSetID != in.ConflictSetID {
		return rejected[ConflictListResult]("CONFLICT_SET_NOT_FOUND", "conflict set does not belong to the merge attempt", []string{"git.conflicts"}, false)
	}
	conflicts, err := s.conflictSetByID(ctx, attempt.ConflictSetID)
	if err != nil {
		return errored[ConflictListResult]("CONFLICT_SET_LOAD_FAILED", err)
	}
	return capability.Accepted(ConflictListResult{MergeAttempt: attempt, ConflictSet: conflicts})
}

func (s *Service) ResolveMerge(ctx context.Context, in ResolveMergeInput) capability.Response[ResolveMergeResult] {
	attempt, changeset, workspace, repo, resp := s.mergeAttemptForAgent(ctx, in.MergeAttemptID, in.AgentID)
	if resp != nil {
		return copyRejected[ResolveMergeResult](*resp)
	}
	if attempt.State != "conflicted" {
		return rejected[ResolveMergeResult]("MERGE_ATTEMPT_STATE_INVALID", "only conflicted merge attempts can be resolved", []string{"git.merge_preview", "git.conflicts"}, false)
	}
	conflicts, err := s.conflictSetByID(ctx, attempt.ConflictSetID)
	if err != nil {
		return errored[ResolveMergeResult]("CONFLICT_SET_LOAD_FAILED", err)
	}
	before, err := gitHead(ctx, repo.SourcePath, attempt.TargetRef)
	if err != nil {
		return errored[ResolveMergeResult]("GIT_RESOLVE_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.resolve", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[ResolveMergeResult](*startResp)
	}
	if reject := s.rejectExpectedTarget(ctx, op, attempt.TargetRef, before, in.ExpectedTargetRef); reject != nil {
		return copyRejected[ResolveMergeResult](*reject)
	}
	resolvedHead := in.ResolvedHeadRef
	if resolvedHead == "" {
		resolvedHead, err = gitHead(ctx, workspace.Path, "HEAD")
		if err != nil {
			completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "resolved workspace head could not be read"})
			return failed[ResolveMergeResult]("GIT_RESOLVE_FAILED", err.Error(), completed)
		}
	}
	integrationPath, resultRef, run, err := createResolvedIntegration(ctx, repo, workspace, changeset, attempt.TargetRef, resolvedHead)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "manual merge resolution could not be created"})
		return failed[ResolveMergeResult]("GIT_RESOLVE_FAILED", err.Error(), completed)
	}
	attempt, err = s.updateMergeAttemptState(ctx, attempt.ID, "resolved", resultRef, integrationPath)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge attempt resolution could not be persisted"})
		return failed[ResolveMergeResult]("GIT_RESOLVE_FAILED", err.Error(), completed)
	}
	conflicts, err = s.updateConflictSetState(ctx, conflicts.ID, "resolved", in.AgentID)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "conflict set resolution could not be persisted"})
		return failed[ResolveMergeResult]("GIT_RESOLVE_FAILED", err.Error(), completed)
	}
	completed := s.completeWrite(ctx, op, "succeeded", resultRef, run, Feedback{Message: "manual merge resolution recorded", NextActions: []string{"git.merge_apply", "git.abort"}})
	return capability.Accepted(ResolveMergeResult{MergeAttempt: attempt, ConflictSet: conflicts, Operation: completed, Feedback: completed.Feedback})
}

func (s *Service) AbortMerge(ctx context.Context, in AbortMergeInput) capability.Response[AbortMergeResult] {
	attempt, _, workspace, repo, resp := s.mergeAttemptForAgent(ctx, in.MergeAttemptID, in.AgentID)
	if resp != nil {
		return copyRejected[AbortMergeResult](*resp)
	}
	if attempt.State == "applied" {
		return rejected[AbortMergeResult]("MERGE_ATTEMPT_ALREADY_APPLIED", "applied merge attempts cannot be aborted; use git.rollback", []string{"git.rollback"}, false)
	}
	before, err := gitHead(ctx, repo.SourcePath, attempt.TargetRef)
	if err != nil {
		return errored[AbortMergeResult]("GIT_ABORT_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.abort", in.AgentID, repo.ID, workspace.ID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[AbortMergeResult](*startResp)
	}
	attempt, err = s.updateMergeAttemptState(ctx, attempt.ID, "aborted", attempt.ResultRef, attempt.IntegrationPath)
	if err != nil {
		completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "merge attempt abort could not be persisted"})
		return failed[AbortMergeResult]("GIT_ABORT_FAILED", err.Error(), completed)
	}
	var conflicts *ConflictSet
	if attempt.ConflictSetID != "" {
		updated, err := s.updateConflictSetState(ctx, attempt.ConflictSetID, "abandoned", in.AgentID)
		if err != nil {
			completed := s.completeWrite(ctx, op, "failed", before, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "conflict set abort could not be persisted"})
			return failed[AbortMergeResult]("GIT_ABORT_FAILED", err.Error(), completed)
		}
		conflicts = &updated
	}
	completed := s.completeWrite(ctx, op, "succeeded", before, gitRun{}, Feedback{Message: firstNonEmpty(in.Reason, "merge attempt aborted"), NextActions: []string{"git.merge_preview"}})
	return capability.Accepted(AbortMergeResult{MergeAttempt: attempt, ConflictSet: conflicts, Operation: completed, Feedback: completed.Feedback})
}

func (s *Service) Rollback(ctx context.Context, in RollbackInput) capability.Response[RollbackResult] {
	rollback, err := s.rollbackPointForInput(ctx, in)
	if err != nil {
		return rejected[RollbackResult]("ROLLBACK_POINT_NOT_FOUND", "rollback point was not found", []string{"git.log"}, false)
	}
	if rollback.State != "available" {
		return rejected[RollbackResult]("ROLLBACK_POINT_STATE_INVALID", "rollback point is not available", []string{"git.log"}, false)
	}
	repo, err := s.repoByID(ctx, rollback.RepoID)
	if err != nil {
		return errored[RollbackResult]("GIT_ROLLBACK_FAILED", err)
	}
	before, err := gitHead(ctx, repo.SourcePath, rollback.TargetRef)
	if err != nil {
		return errored[RollbackResult]("GIT_ROLLBACK_FAILED", err)
	}
	op, startResp := s.startWrite(ctx, "git.rollback", in.AgentID, repo.ID, rollback.WorkspaceID, in.IdempotencyKey, before)
	if startResp != nil {
		return copyRejected[RollbackResult](*startResp)
	}
	if reject := s.rejectExpectedTarget(ctx, op, rollback.TargetRef, before, in.ExpectedTargetRef); reject != nil {
		return copyRejected[RollbackResult](*reject)
	}
	if before != rollback.AfterRef {
		completed := s.completeWrite(ctx, op, "rejected", before, gitRun{}, Feedback{Message: "target ref no longer matches rollback point", NextActions: []string{"git.log"}})
		return capability.Rejected[RollbackResult](
			"ROLLBACK_REF_MISMATCH",
			"target ref no longer matches the rollback point after_ref",
			capability.WithCanonicalID("operation_id", completed.ID),
			capability.WithCanonicalID("rollback_point_id", rollback.ID),
			capability.WithRepairHint("inspect git.log before deciding whether a later rollback is safe"),
			capability.WithAllowedNextActions("git.log"),
			capability.WithRetryable(false),
		)
	}
	run := updateRef(ctx, repo.SourcePath, rollback.TargetRef, rollback.BeforeRef, before)
	if run.ExitCode != 0 {
		completed := s.completeWrite(ctx, op, "failed", before, run, Feedback{Message: "target ref could not be rolled back"})
		return failed[RollbackResult]("GIT_ROLLBACK_FAILED", firstNonEmpty(run.Stderr, run.Stdout), completed)
	}
	if err := s.markRollbackPointState(ctx, rollback.ID, "used"); err != nil {
		completed := s.completeWrite(ctx, op, "failed", rollback.BeforeRef, gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "rollback point state could not be persisted"})
		return failed[RollbackResult]("GIT_ROLLBACK_FAILED", err.Error(), completed)
	}
	rollback, _ = s.rollbackPointByID(ctx, rollback.ID)
	completed := s.completeWrite(ctx, op, "succeeded", rollback.BeforeRef, run, Feedback{Message: "target ref rolled back", NextActions: []string{"git.log", "workspace.sync"}})
	return capability.Accepted(RollbackResult{RollbackPoint: rollback, Operation: completed, RestoredRef: rollback.BeforeRef, Feedback: completed.Feedback})
}

func (s *Service) RecoverOperations(ctx context.Context, in RecoverOperationsInput) capability.Response[RecoverOperationsResult] {
	ops, err := s.runningOperations(ctx, in.OperationID, in.AgentID)
	if err != nil {
		return errored[RecoverOperationsResult]("GIT_RECOVER_FAILED", err)
	}
	recovered := make([]GitOperation, 0, len(ops))
	for _, op := range ops {
		completed := s.completeWrite(ctx, writeOperationFromGitOperation(op), "failed", op.BeforeRef, gitRun{}, Feedback{Message: "running operation recovered as failed", NextActions: []string{"workspace.status", "git.log"}})
		recovered = append(recovered, completed)
	}
	return capability.Accepted(RecoverOperationsResult{
		RecoveredOperations: recovered,
		Feedback:            Feedback{Message: "running Git operations recovered", NextActions: []string{"workspace.status", "git.log"}},
	})
}

func (s *Service) resolveRepoForPrepare(ctx context.Context, in WorkspacePrepareInput) (Repository, bool, error) {
	if in.RepoID != "" {
		repo, err := s.repoByID(ctx, in.RepoID)
		return repo, false, err
	}
	if in.RepoPath == "" {
		return Repository{}, false, errors.New("repo_path is required when repo_id is not provided")
	}
	absPath, err := filepath.Abs(in.RepoPath)
	if err != nil {
		return Repository{}, false, err
	}
	if _, err := os.Stat(filepath.Join(absPath, ".git")); err != nil {
		return Repository{}, false, fmt.Errorf("repo_path must point at a Git working tree: %w", err)
	}
	var repo Repository
	err = s.db.QueryRowContext(ctx, `
SELECT id, source_path, canonical_branch, status, created_at, updated_at
FROM git_repositories
WHERE source_path = ? AND canonical_branch = ?`, absPath, in.CanonicalBranch).Scan(
		&repo.ID, &repo.SourcePath, &repo.CanonicalBranch, &repo.Status, scanTime(&repo.CreatedAt), scanTime(&repo.UpdatedAt),
	)
	if err == nil {
		return repo, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Repository{}, false, err
	}
	id, err := ids.New("repo")
	if err != nil {
		return Repository{}, false, err
	}
	return Repository{
		ID:              id,
		SourcePath:      absPath,
		CanonicalBranch: in.CanonicalBranch,
		Status:          "active",
	}, true, nil
}

func (s *Service) repoByID(ctx context.Context, repoID string) (Repository, error) {
	var repo Repository
	if err := s.db.QueryRowContext(ctx, `
SELECT id, source_path, canonical_branch, status, created_at, updated_at
FROM git_repositories
WHERE id = ?`, repoID).Scan(
		&repo.ID, &repo.SourcePath, &repo.CanonicalBranch, &repo.Status, scanTime(&repo.CreatedAt), scanTime(&repo.UpdatedAt),
	); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

func insertRepoTx(ctx context.Context, tx *sql.Tx, repo Repository) error {
	now := formatTime(time.Now())
	_, err := tx.ExecContext(ctx, `
INSERT INTO git_repositories (
  id, tenant_id, source_path, canonical_branch, status, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		repo.ID, canonicalTenant, repo.SourcePath, repo.CanonicalBranch, repo.Status, now, now,
	)
	return err
}

func insertWorkspaceTx(ctx context.Context, tx *sql.Tx, workspace Workspace) error {
	now := formatTime(time.Now())
	_, err := tx.ExecContext(ctx, `
INSERT INTO git_workspaces (
  id, tenant_id, repo_id, agent_id, runtime_id, contract_id, path, base_ref,
  head_ref, dirty, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		workspace.ID, canonicalTenant, workspace.RepoID, workspace.AgentID, workspace.RuntimeID,
		workspace.ContractID, workspace.Path, workspace.BaseRef, workspace.HeadRef, boolInt(workspace.Dirty),
		workspace.State, now, now,
	)
	return err
}

func (s *Service) markWorkspaceState(ctx context.Context, workspaceID, state, baseRef, headRef string, dirty bool) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE git_workspaces
SET state = ?, base_ref = ?, head_ref = ?, dirty = ?, updated_at = ?
WHERE id = ?`, state, baseRef, headRef, boolInt(dirty), formatTime(time.Now()), workspaceID)
	return err
}

func (s *Service) workspaceForAgent(ctx context.Context, workspaceID, agentID string) (Workspace, Repository, *capability.Response[json.RawMessage]) {
	if workspaceID == "" {
		resp := rejectedRaw("WORKSPACE_ID_REQUIRED", "workspace_id is required", []string{"workspace.prepare"}, false)
		return Workspace{}, Repository{}, &resp
	}
	var workspace Workspace
	var dirty int
	if err := s.db.QueryRowContext(ctx, `
SELECT id, repo_id, agent_id, COALESCE(runtime_id, ''), COALESCE(contract_id, ''),
  path, base_ref, head_ref, dirty, state, created_at, updated_at
FROM git_workspaces
WHERE id = ? AND agent_id = ?`, workspaceID, agentID).Scan(
		&workspace.ID, &workspace.RepoID, &workspace.AgentID, &workspace.RuntimeID,
		&workspace.ContractID, &workspace.Path, &workspace.BaseRef, &workspace.HeadRef,
		&dirty, &workspace.State, scanTime(&workspace.CreatedAt), scanTime(&workspace.UpdatedAt),
	); err != nil {
		resp := rejectedRaw("WORKSPACE_NOT_FOUND", "workspace was not found for this agent", []string{"workspace.prepare"}, false)
		return Workspace{}, Repository{}, &resp
	}
	workspace.Dirty = dirty != 0
	repo, err := s.repoByID(ctx, workspace.RepoID)
	if err != nil {
		resp := capability.Error[json.RawMessage]("GIT_REPO_LOAD_FAILED", err.Error(), false)
		return Workspace{}, Repository{}, &resp
	}
	return workspace, repo, nil
}

func (s *Service) changeSetForAgent(ctx context.Context, changesetID, agentID string) (ChangeSet, Workspace, Repository, *capability.Response[json.RawMessage]) {
	if changesetID == "" {
		resp := rejectedRaw("CHANGESET_ID_REQUIRED", "changeset_id is required", []string{"changeset.submit"}, false)
		return ChangeSet{}, Workspace{}, Repository{}, &resp
	}
	changeset, err := s.changeSetByID(ctx, changesetID)
	if err != nil {
		resp := rejectedRaw("CHANGESET_NOT_FOUND", "changeset was not found", []string{"changeset.submit"}, false)
		return ChangeSet{}, Workspace{}, Repository{}, &resp
	}
	workspace, repo, resp := s.workspaceForAgent(ctx, changeset.WorkspaceID, agentID)
	if resp != nil {
		return ChangeSet{}, Workspace{}, Repository{}, resp
	}
	return changeset, workspace, repo, nil
}

func (s *Service) mergeAttemptForAgent(ctx context.Context, mergeAttemptID, agentID string) (MergeAttempt, ChangeSet, Workspace, Repository, *capability.Response[json.RawMessage]) {
	if mergeAttemptID == "" {
		resp := rejectedRaw("MERGE_ATTEMPT_ID_REQUIRED", "merge_attempt_id is required", []string{"git.merge_preview"}, false)
		return MergeAttempt{}, ChangeSet{}, Workspace{}, Repository{}, &resp
	}
	attempt, err := s.mergeAttemptByID(ctx, mergeAttemptID)
	if err != nil {
		resp := rejectedRaw("MERGE_ATTEMPT_NOT_FOUND", "merge attempt was not found", []string{"git.merge_preview"}, false)
		return MergeAttempt{}, ChangeSet{}, Workspace{}, Repository{}, &resp
	}
	changeset, err := s.changeSetByID(ctx, attempt.ChangeSetID)
	if err != nil {
		resp := rejectedRaw("CHANGESET_NOT_FOUND", "changeset was not found", []string{"changeset.submit"}, false)
		return MergeAttempt{}, ChangeSet{}, Workspace{}, Repository{}, &resp
	}
	workspace, repo, resp := s.workspaceForAgent(ctx, attempt.WorkspaceID, agentID)
	if resp != nil {
		return MergeAttempt{}, ChangeSet{}, Workspace{}, Repository{}, resp
	}
	return attempt, changeset, workspace, repo, nil
}

func (s *Service) startPrepareWrite(ctx context.Context, agentID string, repo Repository, insertRepo bool, workspace Workspace, idempotencyKey string) (writeOperation, *capability.Response[json.RawMessage]) {
	opID, err := ids.New("gitop")
	if err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_ID_FAILED", err.Error(), false)
		return writeOperation{}, &resp
	}
	now := time.Now()
	op := writeOperation{
		ID:            opID,
		OperationType: "workspace.prepare",
		AgentID:       agentID,
		WorkspaceID:   workspace.ID,
		RepoID:        repo.ID,
		BeforeRef:     "",
		CreatedAt:     now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if insertRepo {
		if err := insertRepoTx(ctx, tx, repo); err != nil {
			resp := capability.Error[json.RawMessage]("GIT_REPO_PREPARE_FAILED", err.Error(), true)
			return writeOperation{}, &resp
		}
	}
	if err := insertOperationTx(ctx, tx, op, idempotencyKey, "running", "", "", gitRun{}, Feedback{Message: "workspace.prepare started"}); err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	if err := insertLockTx(ctx, tx, "repo", repo.ID, op, now); err != nil {
		if completeErr := completeOperationTx(ctx, tx, op, "rejected", "", gitRun{}, Feedback{Message: "repo lock is held", NextActions: []string{"workspace.status"}}); completeErr != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", completeErr.Error(), true)
			return writeOperation{}, &resp
		}
		if err := tx.Commit(); err != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", err.Error(), true)
			return writeOperation{}, &resp
		}
		resp := lockConflictResponse(op, "repo")
		return writeOperation{}, &resp
	}
	if err := insertWorkspaceTx(ctx, tx, workspace); err != nil {
		if _, relErr := tx.ExecContext(ctx, `UPDATE git_locks SET state = 'released', updated_at = ? WHERE operation_id = ? AND state = 'active'`, formatTime(now), op.ID); relErr != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_RELEASE_FAILED", relErr.Error(), true)
			return writeOperation{}, &resp
		}
		if completeErr := completeOperationTx(ctx, tx, op, "failed", "", gitRun{Stderr: err.Error(), ExitCode: 1}, Feedback{Message: "workspace record could not be persisted"}); completeErr != nil {
			resp := capability.Error[json.RawMessage]("WORKSPACE_RECORD_FAILED", completeErr.Error(), true)
			return writeOperation{}, &resp
		}
		if err := tx.Commit(); err != nil {
			resp := capability.Error[json.RawMessage]("WORKSPACE_RECORD_FAILED", err.Error(), true)
			return writeOperation{}, &resp
		}
		resp := capability.Error[json.RawMessage]("WORKSPACE_RECORD_FAILED", err.Error(), false)
		return writeOperation{}, &resp
	}
	if err := insertLockTx(ctx, tx, "workspace", workspace.ID, op, now); err != nil {
		if _, relErr := tx.ExecContext(ctx, `UPDATE git_locks SET state = 'released', updated_at = ? WHERE operation_id = ? AND state = 'active'`, formatTime(now), op.ID); relErr != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_RELEASE_FAILED", relErr.Error(), true)
			return writeOperation{}, &resp
		}
		if completeErr := completeOperationTx(ctx, tx, op, "rejected", "", gitRun{}, Feedback{Message: "workspace lock is held", NextActions: []string{"workspace.status"}}); completeErr != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", completeErr.Error(), true)
			return writeOperation{}, &resp
		}
		if err := tx.Commit(); err != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", err.Error(), true)
			return writeOperation{}, &resp
		}
		resp := lockConflictResponse(op, "workspace")
		return writeOperation{}, &resp
	}
	if err := tx.Commit(); err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	return op, nil
}

func (s *Service) startWrite(ctx context.Context, operationType, agentID, repoID, workspaceID, idempotencyKey, beforeRef string) (writeOperation, *capability.Response[json.RawMessage]) {
	opID, err := ids.New("gitop")
	if err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_ID_FAILED", err.Error(), false)
		return writeOperation{}, &resp
	}
	now := time.Now()
	op := writeOperation{
		ID:            opID,
		OperationType: operationType,
		AgentID:       agentID,
		WorkspaceID:   workspaceID,
		RepoID:        repoID,
		BeforeRef:     beforeRef,
		CreatedAt:     now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := insertOperationTx(ctx, tx, op, idempotencyKey, "running", beforeRef, "", gitRun{}, Feedback{Message: operationType + " started"}); err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	if err := insertLockTx(ctx, tx, "repo", repoID, op, now); err != nil {
		if completeErr := completeOperationTx(ctx, tx, op, "rejected", beforeRef, gitRun{}, Feedback{Message: "repo lock is held", NextActions: []string{"workspace.status"}}); completeErr != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", completeErr.Error(), true)
			return writeOperation{}, &resp
		}
		if err := tx.Commit(); err != nil {
			resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", err.Error(), true)
			return writeOperation{}, &resp
		}
		resp := lockConflictResponse(op, "repo")
		return writeOperation{}, &resp
	}
	if workspaceID != "" {
		if err := insertLockTx(ctx, tx, "workspace", workspaceID, op, now); err != nil {
			if _, relErr := tx.ExecContext(ctx, `UPDATE git_locks SET state = 'released', updated_at = ? WHERE operation_id = ? AND state = 'active'`, formatTime(now), op.ID); relErr != nil {
				resp := capability.Error[json.RawMessage]("GIT_LOCK_RELEASE_FAILED", relErr.Error(), true)
				return writeOperation{}, &resp
			}
			if completeErr := completeOperationTx(ctx, tx, op, "rejected", beforeRef, gitRun{}, Feedback{Message: "workspace lock is held", NextActions: []string{"workspace.status"}}); completeErr != nil {
				resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", completeErr.Error(), true)
				return writeOperation{}, &resp
			}
			if err := tx.Commit(); err != nil {
				resp := capability.Error[json.RawMessage]("GIT_LOCK_CONFLICT_RECORD_FAILED", err.Error(), true)
				return writeOperation{}, &resp
			}
			resp := lockConflictResponse(op, "workspace")
			return writeOperation{}, &resp
		}
	}
	if err := tx.Commit(); err != nil {
		resp := capability.Error[json.RawMessage]("GIT_OPERATION_START_FAILED", err.Error(), true)
		return writeOperation{}, &resp
	}
	return op, nil
}

func (s *Service) completeWrite(ctx context.Context, op writeOperation, state, afterRef string, run gitRun, feedback Feedback) GitOperation {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitOperation{ID: op.ID, OperationType: op.OperationType, ActorAgentID: op.AgentID, WorkspaceID: op.WorkspaceID, RepoID: op.RepoID, BeforeRef: op.BeforeRef, AfterRef: afterRef, State: "failed", Stderr: err.Error(), Feedback: Feedback{Message: "operation completion could not start"}}
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := completeOperationTx(ctx, tx, op, state, afterRef, run, feedback); err != nil {
		return GitOperation{ID: op.ID, OperationType: op.OperationType, ActorAgentID: op.AgentID, WorkspaceID: op.WorkspaceID, RepoID: op.RepoID, BeforeRef: op.BeforeRef, AfterRef: afterRef, State: "failed", Stderr: err.Error(), Feedback: Feedback{Message: "operation completion failed"}}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE git_locks SET state = 'released', updated_at = ? WHERE operation_id = ? AND state = 'active'`, formatTime(time.Now()), op.ID); err != nil {
		return GitOperation{ID: op.ID, OperationType: op.OperationType, ActorAgentID: op.AgentID, WorkspaceID: op.WorkspaceID, RepoID: op.RepoID, BeforeRef: op.BeforeRef, AfterRef: afterRef, State: "failed", Stderr: err.Error(), Feedback: Feedback{Message: "operation locks could not be released"}}
	}
	if err := tx.Commit(); err != nil {
		return GitOperation{ID: op.ID, OperationType: op.OperationType, ActorAgentID: op.AgentID, WorkspaceID: op.WorkspaceID, RepoID: op.RepoID, BeforeRef: op.BeforeRef, AfterRef: afterRef, State: "failed", Stderr: err.Error(), Feedback: Feedback{Message: "operation completion could not commit"}}
	}
	operation, err := s.operationByID(ctx, op.ID)
	if err != nil {
		return GitOperation{ID: op.ID, OperationType: op.OperationType, ActorAgentID: op.AgentID, WorkspaceID: op.WorkspaceID, RepoID: op.RepoID, BeforeRef: op.BeforeRef, AfterRef: afterRef, State: state, Feedback: feedback}
	}
	return operation
}

func (s *Service) rejectExpectedHead(ctx context.Context, op writeOperation, currentHead, expectedHead string) *capability.Response[json.RawMessage] {
	if expectedHead == "" || expectedHead == currentHead {
		return nil
	}
	completed := s.completeWrite(ctx, op, "rejected", currentHead, gitRun{}, Feedback{Message: "expected head did not match current workspace head", NextActions: []string{"workspace.status", "git.log"}})
	resp := capability.Rejected[json.RawMessage](
		"EXPECTED_HEAD_MISMATCH",
		"workspace HEAD does not match expected_head_ref",
		capability.WithCanonicalID("operation_id", completed.ID),
		capability.WithCanonicalID("workspace_id", op.WorkspaceID),
		capability.WithRepairHint("reload workspace.status and retry from the current head"),
		capability.WithAllowedNextActions("workspace.status", "git.log"),
		capability.WithRetryable(true),
	)
	return &resp
}

func (s *Service) rejectExpectedTarget(ctx context.Context, op writeOperation, targetRef, currentRef, expectedRef string) *capability.Response[json.RawMessage] {
	if expectedRef == "" || expectedRef == currentRef {
		return nil
	}
	completed := s.completeWrite(ctx, op, "rejected", currentRef, gitRun{}, Feedback{Message: "expected target ref did not match current target", NextActions: []string{"git.log", "git.merge_preview"}})
	resp := capability.Rejected[json.RawMessage](
		"STALE_EXPECTED_REF",
		"target ref does not match expected_target_ref",
		capability.WithCanonicalID("operation_id", completed.ID),
		capability.WithCanonicalID("repo_id", op.RepoID),
		capability.WithRepairHint("reload the target ref and retry from the current sha"),
		capability.WithAllowedNextActions("git.log", "git.merge_preview"),
		capability.WithRetryable(true),
	)
	if targetRef != "" {
		resp.CanonicalIDs["target_ref"] = targetRef
	}
	return &resp
}

func insertOperationTx(ctx context.Context, tx *sql.Tx, op writeOperation, idempotencyKey, state, beforeRef, afterRef string, run gitRun, feedback Feedback) error {
	feedbackJSON, err := json.Marshal(feedback)
	if err != nil {
		return err
	}
	now := formatTime(op.CreatedAt)
	_, err = tx.ExecContext(ctx, `
INSERT INTO git_operations (
  id, tenant_id, operation_type, actor_agent_id, workspace_id, repo_id,
  idempotency_key, before_ref, after_ref, stdout, stderr, exit_code,
  state, feedback_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, canonicalTenant, op.OperationType, op.AgentID, nullable(op.WorkspaceID), op.RepoID,
		idempotencyKey, beforeRef, afterRef, run.Stdout, run.Stderr, run.ExitCode, state, string(feedbackJSON), now,
	)
	return err
}

func insertLockTx(ctx context.Context, tx *sql.Tx, scopeKind, scopeID string, op writeOperation, now time.Time) error {
	lockID, err := ids.New("gitlock")
	if err != nil {
		return err
	}
	formattedNow := formatTime(now)
	_, err = tx.ExecContext(ctx, `
INSERT INTO git_locks (
  id, tenant_id, scope_kind, scope_id, operation_id, owner_agent_id,
  state, created_at, updated_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lockID, canonicalTenant, scopeKind, scopeID, op.ID, op.AgentID, "active",
		formattedNow, formattedNow, formatTime(now.Add(defaultLockTTL)),
	)
	return err
}

func completeOperationTx(ctx context.Context, tx *sql.Tx, op writeOperation, state, afterRef string, run gitRun, feedback Feedback) error {
	feedbackJSON, err := json.Marshal(feedback)
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `
UPDATE git_operations
SET after_ref = ?, stdout = ?, stderr = ?, exit_code = ?, state = ?,
  feedback_json = ?, completed_at = ?
WHERE id = ?`,
		afterRef, run.Stdout, run.Stderr, run.ExitCode, state, string(feedbackJSON), formatTime(now), op.ID,
	); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"operation_id":   op.ID,
		"operation_type": op.OperationType,
		"workspace_id":   op.WorkspaceID,
		"repo_id":        op.RepoID,
		"state":          state,
		"before_ref":     op.BeforeRef,
		"after_ref":      afterRef,
		"feedback":       feedback,
	})
	_, err = store.AppendEventTx(ctx, tx, events.Event{
		TenantID:       canonicalTenant,
		SubjectKind:    "agent",
		SubjectID:      op.AgentID,
		AgentID:        op.AgentID,
		CapabilityName: op.OperationType,
		Type:           "git.operation." + state,
		AggregateType:  "git_operation",
		AggregateID:    op.ID,
		PayloadJSON:    payload,
		OccurredAt:     now,
	})
	return err
}

func (s *Service) operationByID(ctx context.Context, id string) (GitOperation, error) {
	var op GitOperation
	var feedbackJSON, completedAt string
	if err := s.db.QueryRowContext(ctx, `
SELECT id, operation_type, actor_agent_id, COALESCE(workspace_id, ''), repo_id,
  COALESCE(idempotency_key, ''), before_ref, after_ref, stdout, stderr, exit_code,
  state, feedback_json, created_at, COALESCE(completed_at, '')
FROM git_operations
WHERE id = ?`, id).Scan(
		&op.ID, &op.OperationType, &op.ActorAgentID, &op.WorkspaceID, &op.RepoID,
		&op.IdempotencyKey, &op.BeforeRef, &op.AfterRef, &op.Stdout, &op.Stderr,
		&op.ExitCode, &op.State, &feedbackJSON, scanTime(&op.CreatedAt), &completedAt,
	); err != nil {
		return GitOperation{}, err
	}
	_ = json.Unmarshal([]byte(feedbackJSON), &op.Feedback)
	if completedAt != "" {
		parsed, err := time.Parse(timeLayout, completedAt)
		if err == nil {
			op.CompletedAt = parsed
		}
	}
	return op, nil
}

func (s *Service) insertChangeSet(ctx context.Context, workspace Workspace, in SubmitChangeSetInput, commits []string, headRef string) (ChangeSet, error) {
	id, err := ids.New("cs")
	if err != nil {
		return ChangeSet{}, err
	}
	if in.ContractID == "" {
		in.ContractID = workspace.ContractID
	}
	commitsJSON, err := json.Marshal(commits)
	if err != nil {
		return ChangeSet{}, err
	}
	evidenceJSON, err := json.Marshal(in.EvidenceRefs)
	if err != nil {
		return ChangeSet{}, err
	}
	now := formatTime(time.Now())
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO changesets (
  id, tenant_id, workspace_id, repo_id, contract_id, base_ref, head_ref,
  commit_ids_json, summary, evidence_refs_json, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, canonicalTenant, workspace.ID, workspace.RepoID, in.ContractID, workspace.BaseRef,
		headRef, string(commitsJSON), in.Summary, string(evidenceJSON), "submitted", now, now,
	); err != nil {
		return ChangeSet{}, err
	}
	return s.changeSetByID(ctx, id)
}

func (s *Service) changeSetByID(ctx context.Context, id string) (ChangeSet, error) {
	var cs ChangeSet
	var commitJSON, evidenceJSON string
	if err := s.db.QueryRowContext(ctx, `
SELECT id, workspace_id, repo_id, COALESCE(contract_id, ''), base_ref, head_ref,
  commit_ids_json, summary, evidence_refs_json, state, created_at, updated_at
FROM changesets
WHERE id = ?`, id).Scan(
		&cs.ID, &cs.WorkspaceID, &cs.RepoID, &cs.ContractID, &cs.BaseRef, &cs.HeadRef,
		&commitJSON, &cs.Summary, &evidenceJSON, &cs.State, scanTime(&cs.CreatedAt), scanTime(&cs.UpdatedAt),
	); err != nil {
		return ChangeSet{}, err
	}
	_ = json.Unmarshal([]byte(commitJSON), &cs.CommitIDs)
	_ = json.Unmarshal([]byte(evidenceJSON), &cs.EvidenceRefs)
	return cs, nil
}

func (s *Service) updateChangeSetState(ctx context.Context, id, state string) (ChangeSet, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE changesets SET state = ?, updated_at = ? WHERE id = ?`, state, formatTime(time.Now()), id); err != nil {
		return ChangeSet{}, err
	}
	return s.changeSetByID(ctx, id)
}

func (s *Service) insertMergeAttempt(ctx context.Context, attempt MergeAttempt) (MergeAttempt, error) {
	id, err := ids.New("merge")
	if err != nil {
		return MergeAttempt{}, err
	}
	now := formatTime(time.Now())
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO git_merge_attempts (
  id, tenant_id, changeset_id, repo_id, workspace_id, target_ref,
  integration_path, base_before, result_ref, state, conflict_set_id,
  operation_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, canonicalTenant, attempt.ChangeSetID, attempt.RepoID, attempt.WorkspaceID,
		attempt.TargetRef, attempt.IntegrationPath, attempt.BaseBefore, attempt.ResultRef,
		attempt.State, nullable(attempt.ConflictSetID), attempt.OperationID, now, now,
	); err != nil {
		return MergeAttempt{}, err
	}
	return s.mergeAttemptByID(ctx, id)
}

func (s *Service) mergeAttemptByID(ctx context.Context, id string) (MergeAttempt, error) {
	var attempt MergeAttempt
	if err := s.db.QueryRowContext(ctx, `
SELECT id, changeset_id, repo_id, workspace_id, target_ref, integration_path,
  base_before, result_ref, state, COALESCE(conflict_set_id, ''), operation_id,
  created_at, updated_at
FROM git_merge_attempts
WHERE id = ?`, id).Scan(
		&attempt.ID, &attempt.ChangeSetID, &attempt.RepoID, &attempt.WorkspaceID,
		&attempt.TargetRef, &attempt.IntegrationPath, &attempt.BaseBefore,
		&attempt.ResultRef, &attempt.State, &attempt.ConflictSetID, &attempt.OperationID,
		scanTime(&attempt.CreatedAt), scanTime(&attempt.UpdatedAt),
	); err != nil {
		return MergeAttempt{}, err
	}
	return attempt, nil
}

func (s *Service) updateMergeAttemptConflict(ctx context.Context, attemptID, conflictSetID string) (MergeAttempt, error) {
	if _, err := s.db.ExecContext(ctx, `
UPDATE git_merge_attempts
SET conflict_set_id = ?, updated_at = ?
WHERE id = ?`, conflictSetID, formatTime(time.Now()), attemptID); err != nil {
		return MergeAttempt{}, err
	}
	return s.mergeAttemptByID(ctx, attemptID)
}

func (s *Service) updateMergeAttemptState(ctx context.Context, attemptID, state, resultRef, integrationPath string) (MergeAttempt, error) {
	if _, err := s.db.ExecContext(ctx, `
UPDATE git_merge_attempts
SET state = ?, result_ref = ?, integration_path = ?, updated_at = ?
WHERE id = ?`, state, resultRef, integrationPath, formatTime(time.Now()), attemptID); err != nil {
		return MergeAttempt{}, err
	}
	return s.mergeAttemptByID(ctx, attemptID)
}

func (s *Service) insertConflictSet(ctx context.Context, mergeAttemptID string, files []string, summary string) (ConflictSet, error) {
	id, err := ids.New("conflict")
	if err != nil {
		return ConflictSet{}, err
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return ConflictSet{}, err
	}
	now := formatTime(time.Now())
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO git_conflict_sets (
  id, tenant_id, merge_attempt_id, files_json, summary, state, resolved_by,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'open', NULL, ?, ?)`,
		id, canonicalTenant, mergeAttemptID, string(filesJSON), summary, now, now,
	); err != nil {
		return ConflictSet{}, err
	}
	return s.conflictSetByID(ctx, id)
}

func (s *Service) conflictSetByID(ctx context.Context, id string) (ConflictSet, error) {
	var set ConflictSet
	var filesJSON string
	if err := s.db.QueryRowContext(ctx, `
SELECT id, merge_attempt_id, files_json, summary, state, COALESCE(resolved_by, ''),
  created_at, updated_at
FROM git_conflict_sets
WHERE id = ?`, id).Scan(
		&set.ID, &set.MergeAttemptID, &filesJSON, &set.Summary, &set.State,
		&set.ResolvedBy, scanTime(&set.CreatedAt), scanTime(&set.UpdatedAt),
	); err != nil {
		return ConflictSet{}, err
	}
	_ = json.Unmarshal([]byte(filesJSON), &set.Files)
	return set, nil
}

func (s *Service) updateConflictSetState(ctx context.Context, id, state, resolvedBy string) (ConflictSet, error) {
	if _, err := s.db.ExecContext(ctx, `
UPDATE git_conflict_sets
SET state = ?, resolved_by = ?, updated_at = ?
WHERE id = ?`, state, nullable(resolvedBy), formatTime(time.Now()), id); err != nil {
		return ConflictSet{}, err
	}
	return s.conflictSetByID(ctx, id)
}

func (s *Service) insertRollbackPoint(ctx context.Context, point RollbackPoint) (RollbackPoint, error) {
	id, err := ids.New("rollback")
	if err != nil {
		return RollbackPoint{}, err
	}
	now := formatTime(time.Now())
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO git_rollback_points (
  id, tenant_id, operation_id, repo_id, workspace_id, target_ref, before_ref,
  after_ref, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, canonicalTenant, point.OperationID, point.RepoID, nullable(point.WorkspaceID),
		point.TargetRef, point.BeforeRef, point.AfterRef, point.State, now, now,
	); err != nil {
		return RollbackPoint{}, err
	}
	return s.rollbackPointByID(ctx, id)
}

func (s *Service) rollbackPointForInput(ctx context.Context, in RollbackInput) (RollbackPoint, error) {
	if in.RollbackPointID != "" {
		return s.rollbackPointByID(ctx, in.RollbackPointID)
	}
	if in.OperationID != "" {
		return s.rollbackPointByOperationID(ctx, in.OperationID)
	}
	return RollbackPoint{}, sql.ErrNoRows
}

func (s *Service) rollbackPointByID(ctx context.Context, id string) (RollbackPoint, error) {
	var point RollbackPoint
	if err := s.db.QueryRowContext(ctx, `
SELECT id, operation_id, repo_id, COALESCE(workspace_id, ''), target_ref,
  before_ref, after_ref, state, created_at, updated_at
FROM git_rollback_points
WHERE id = ?`, id).Scan(
		&point.ID, &point.OperationID, &point.RepoID, &point.WorkspaceID,
		&point.TargetRef, &point.BeforeRef, &point.AfterRef, &point.State,
		scanTime(&point.CreatedAt), scanTime(&point.UpdatedAt),
	); err != nil {
		return RollbackPoint{}, err
	}
	return point, nil
}

func (s *Service) rollbackPointByOperationID(ctx context.Context, operationID string) (RollbackPoint, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `
SELECT id
FROM git_rollback_points
WHERE operation_id = ?
ORDER BY created_at DESC
LIMIT 1`, operationID).Scan(&id); err != nil {
		return RollbackPoint{}, err
	}
	return s.rollbackPointByID(ctx, id)
}

func (s *Service) markRollbackPointState(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE git_rollback_points
SET state = ?, updated_at = ?
WHERE id = ?`, state, formatTime(time.Now()), id)
	return err
}

func (s *Service) runningOperations(ctx context.Context, operationID, agentID string) ([]GitOperation, error) {
	query := `
SELECT id
FROM git_operations
WHERE state = 'running'`
	var args []any
	if operationID != "" {
		query += ` AND id = ?`
		args = append(args, operationID)
	}
	if agentID != "" {
		query += ` AND actor_agent_id = ?`
		args = append(args, agentID)
	}
	query += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	ops := make([]GitOperation, 0, len(ids))
	for _, id := range ids {
		op, err := s.operationByID(ctx, id)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (s *Service) gitChangeSetDiff(ctx context.Context, in GitDiffInput) capability.Response[GitDiffResult] {
	cs, workspace, _, resp := s.changeSetForAgent(ctx, in.ChangeSetID, in.AgentID)
	if resp != nil {
		return copyRejected[GitDiffResult](*resp)
	}
	run := runGit(ctx, workspace.Path, "diff", cs.BaseRef+".."+cs.HeadRef)
	if run.ExitCode != 0 {
		return capability.Error[GitDiffResult]("GIT_DIFF_FAILED", run.Stderr, false)
	}
	return capability.Accepted(GitDiffResult{WorkspaceID: workspace.ID, ChangeSetID: cs.ID, Diff: run.Stdout})
}

func gitHead(ctx context.Context, dir, ref string) (string, error) {
	run := runGit(ctx, dir, "rev-parse", ref)
	if run.ExitCode != 0 {
		return "", errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git rev-parse failed"))
	}
	return strings.TrimSpace(run.Stdout), nil
}

func gitChangedPaths(ctx context.Context, dir string) ([]string, bool, error) {
	run := runGit(ctx, dir, "status", "--porcelain")
	if run.ExitCode != 0 {
		return nil, false, errors.New(run.Stderr)
	}
	paths := parseStatusPaths(run.Stdout)
	return paths, len(paths) > 0, nil
}

func gitCommitRange(ctx context.Context, dir, baseRef, headRef string) ([]string, error) {
	if baseRef == headRef {
		return nil, nil
	}
	run := runGit(ctx, dir, "rev-list", "--reverse", baseRef+".."+headRef)
	if run.ExitCode != 0 {
		return nil, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git rev-list failed"))
	}
	lines := strings.Fields(run.Stdout)
	return lines, nil
}

func createMergePreview(ctx context.Context, repo Repository, workspace Workspace, sourceHeadRef, targetRef string) (string, string, []string, gitRun, error) {
	integrationPath, err := os.MkdirTemp(filepath.Dir(repo.SourcePath), ".coordplane-merge-*")
	if err != nil {
		return "", "", nil, gitRun{}, err
	}
	if run := runGit(ctx, "", "clone", "--quiet", repo.SourcePath, integrationPath); run.ExitCode != 0 {
		return integrationPath, "", nil, run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git clone failed"))
	}
	if run := runGit(ctx, integrationPath, "checkout", "--detach", targetRef); run.ExitCode != 0 {
		return integrationPath, "", nil, run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git checkout target failed"))
	}
	if run := runGit(ctx, integrationPath, "fetch", "--quiet", workspace.Path, sourceHeadRef); run.ExitCode != 0 {
		return integrationPath, "", nil, run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git fetch changeset failed"))
	}
	merge := runGit(ctx, integrationPath, "merge", "--no-ff", "--no-commit", "FETCH_HEAD")
	if merge.ExitCode != 0 {
		files := conflictFiles(ctx, integrationPath)
		return integrationPath, "", files, merge, nil
	}
	commit := runGit(ctx, integrationPath, "-c", "user.name="+defaultGitAuthor, "-c", "user.email="+defaultGitEmail, "commit", "-m", "Merge changeset")
	if commit.ExitCode != 0 {
		return integrationPath, "", nil, commit, errors.New(firstNonEmpty(commit.Stderr, commit.Stdout, "git commit merge failed"))
	}
	resultRef, err := gitHead(ctx, integrationPath, "HEAD")
	if err != nil {
		return integrationPath, "", nil, gitRun{Stderr: err.Error(), ExitCode: 1}, err
	}
	return integrationPath, resultRef, nil, gitRun{Stdout: merge.Stdout + commit.Stdout, Stderr: merge.Stderr + commit.Stderr}, nil
}

func createResolvedIntegration(ctx context.Context, repo Repository, workspace Workspace, changeset ChangeSet, targetRef, resolvedHeadRef string) (string, string, gitRun, error) {
	files, err := changedFilesInRange(ctx, workspace.Path, changeset.BaseRef, resolvedHeadRef)
	if err != nil {
		return "", "", gitRun{Stderr: err.Error(), ExitCode: 1}, err
	}
	if len(files) == 0 {
		return "", "", gitRun{}, errors.New("manual resolution did not include changed files")
	}
	integrationPath, err := os.MkdirTemp(filepath.Dir(repo.SourcePath), ".coordplane-resolve-*")
	if err != nil {
		return "", "", gitRun{}, err
	}
	if run := runGit(ctx, "", "clone", "--quiet", repo.SourcePath, integrationPath); run.ExitCode != 0 {
		return integrationPath, "", run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git clone failed"))
	}
	if run := runGit(ctx, integrationPath, "checkout", "--detach", targetRef); run.ExitCode != 0 {
		return integrationPath, "", run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git checkout target failed"))
	}
	if err := copyWorkspaceFiles(workspace.Path, integrationPath, files); err != nil {
		return integrationPath, "", gitRun{Stderr: err.Error(), ExitCode: 1}, err
	}
	addArgs := append([]string{"add", "--all", "--"}, files...)
	if run := runGit(ctx, integrationPath, addArgs...); run.ExitCode != 0 {
		return integrationPath, "", run, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git add resolved files failed"))
	}
	commit := runGit(ctx, integrationPath, "-c", "user.name="+defaultGitAuthor, "-c", "user.email="+defaultGitEmail, "commit", "-m", "Resolve merge conflicts")
	if commit.ExitCode != 0 {
		return integrationPath, "", commit, errors.New(firstNonEmpty(commit.Stderr, commit.Stdout, "git commit resolved files failed"))
	}
	resultRef, err := gitHead(ctx, integrationPath, "HEAD")
	if err != nil {
		return integrationPath, "", gitRun{Stderr: err.Error(), ExitCode: 1}, err
	}
	return integrationPath, resultRef, commit, nil
}

func changedFilesInRange(ctx context.Context, dir, baseRef, headRef string) ([]string, error) {
	run := runGit(ctx, dir, "diff", "--name-only", baseRef+".."+headRef)
	if run.ExitCode != 0 {
		return nil, errors.New(firstNonEmpty(run.Stderr, run.Stdout, "git diff --name-only failed"))
	}
	files := strings.Fields(run.Stdout)
	sort.Strings(files)
	return files, nil
}

func conflictFiles(ctx context.Context, dir string) []string {
	run := runGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if run.ExitCode != 0 {
		return nil
	}
	files := strings.Fields(run.Stdout)
	sort.Strings(files)
	return files
}

func copyWorkspaceFiles(srcRoot, dstRoot string, files []string) error {
	for _, file := range files {
		if err := validatePaths([]string{file}); err != nil {
			return err
		}
		src := filepath.Join(srcRoot, filepath.FromSlash(file))
		dst := filepath.Join(dstRoot, filepath.FromSlash(file))
		content, err := os.ReadFile(src)
		if errors.Is(err, os.ErrNotExist) {
			if removeErr := os.Remove(dst); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fetchAndUpdateRef(ctx context.Context, repoPath, integrationPath, resultRef, targetRef, expectedOldRef string) gitRun {
	fetch := runGit(ctx, repoPath, "fetch", "--quiet", integrationPath, resultRef)
	if fetch.ExitCode != 0 {
		return fetch
	}
	update := updateRef(ctx, repoPath, targetRef, "FETCH_HEAD", expectedOldRef)
	return gitRun{Stdout: fetch.Stdout + update.Stdout, Stderr: fetch.Stderr + update.Stderr, ExitCode: update.ExitCode}
}

func updateRef(ctx context.Context, repoPath, targetRef, newRef, expectedOldRef string) gitRun {
	update := runGit(ctx, repoPath, "update-ref", canonicalTargetRef(targetRef), newRef, expectedOldRef)
	if update.ExitCode != 0 {
		return update
	}
	reset := runGit(ctx, repoPath, "reset", "--hard", targetRef)
	return gitRun{Stdout: update.Stdout + reset.Stdout, Stderr: update.Stderr + reset.Stderr, ExitCode: reset.ExitCode}
}

func canonicalTargetRef(targetRef string) string {
	if strings.HasPrefix(targetRef, "refs/") {
		return targetRef
	}
	return "refs/heads/" + targetRef
}

func runGit(ctx context.Context, dir string, args ...string) gitRun {
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			stderr.WriteString(err.Error())
		}
	}
	return gitRun{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}
}

func parseStatusPaths(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) >= 4 {
			out = append(out, strings.TrimSpace(line[3:]))
			continue
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

func parseLog(raw string) []GitLogEntry {
	var out []GitLogEntry
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 2)
		entry := GitLogEntry{SHA: parts[0]}
		if len(parts) > 1 {
			entry.Summary = parts[1]
		}
		out = append(out, entry)
	}
	return out
}

func validatePaths(paths []string) error {
	for _, path := range paths {
		if path == "" || filepath.IsAbs(path) || strings.Contains(path, "..") {
			return fmt.Errorf("invalid relative path %q", path)
		}
	}
	return nil
}

func lockConflictResponse(op writeOperation, scope string) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		"GIT_LOCK_CONFLICT",
		scope+" lock is already held by another Git operation",
		capability.WithCanonicalID("operation_id", op.ID),
		capability.WithCanonicalID("repo_id", op.RepoID),
		capability.WithCanonicalID("workspace_id", op.WorkspaceID),
		capability.WithRepairHint("retry after the active Git operation finishes"),
		capability.WithAllowedNextActions("workspace.status"),
		capability.WithRetryable(true),
	)
}

func rejected[T any](code, message string, next []string, retryable bool) capability.Response[T] {
	return capability.Rejected[T](
		code,
		message,
		capability.WithRepairHint("inspect the response and retry with corrected input"),
		capability.WithAllowedNextActions(next...),
		capability.WithRetryable(retryable),
	)
}

func rejectedRaw(code, message string, next []string, retryable bool) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		code,
		message,
		capability.WithRepairHint("inspect the response and retry with corrected input"),
		capability.WithAllowedNextActions(next...),
		capability.WithRetryable(retryable),
	)
}

func errored[T any](code string, err error) capability.Response[T] {
	return capability.Error[T](code, err.Error(), false)
}

func failed[T any](code, message string, op GitOperation) capability.Response[T] {
	return capability.Error[T](code, messageWithOperation(message, op.ID), false)
}

func copyRejected[T any](response capability.Response[json.RawMessage]) capability.Response[T] {
	return capability.Response[T]{
		OK:                 response.OK,
		Status:             response.Status,
		ErrorCode:          response.ErrorCode,
		Message:            response.Message,
		RepairHint:         response.RepairHint,
		CanonicalIDs:       cloneStringMap(response.CanonicalIDs),
		Missing:            append([]capability.MissingRequirement(nil), response.Missing...),
		AllowedNextActions: append([]string(nil), response.AllowedNextActions...),
		Retryable:          response.Retryable,
	}
}

func messageWithOperation(message, operationID string) string {
	if operationID == "" {
		return message
	}
	return message + " (operation_id=" + operationID + ")"
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func scanTime(target *time.Time) sql.Scanner {
	return timeScanner{target: target}
}

type timeScanner struct {
	target *time.Time
}

func (s timeScanner) Scan(value any) error {
	switch typed := value.(type) {
	case string:
		parsed, err := time.Parse(timeLayout, typed)
		if err != nil {
			return err
		}
		*s.target = parsed
	case []byte:
		parsed, err := time.Parse(timeLayout, string(typed))
		if err != nil {
			return err
		}
		*s.target = parsed
	case time.Time:
		*s.target = typed
	default:
		return fmt.Errorf("unsupported time value %T", value)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
