package codemanagement_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/capability"
	"coordplane/internal/codemanagement"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestWorkspaceReadOnlyCapabilitiesAndSyncUseRealGitRepo(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	if countRows(t, ctx, db, "git_operations") != 1 {
		t.Fatalf("prepare operations = %d, want 1", countRows(t, ctx, db, "git_operations"))
	}
	assertOperationAudit(t, ctx, db, prepared.OperationID, "workspace.prepare", "succeeded", "", prepared.Workspace.HeadRef)
	assertReleasedLocks(t, ctx, db, prepared.OperationID, prepared.Workspace.ID, prepared.Workspace.RepoID)

	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "README.md"), []byte("initial\nworkspace edit\n"), 0o644); err != nil {
		t.Fatalf("write workspace README edit: %v", err)
	}
	status := acceptedData(t, svc.WorkspaceStatus(ctx, codemanagement.WorkspaceStatusInput{
		WorkspaceID: prepared.Workspace.ID,
		AgentID:     "builder",
	}))
	if !status.Dirty || !contains(status.ChangedPaths, "README.md") {
		t.Fatalf("workspace.status = %+v, want dirty README.md", status)
	}
	gitStatus := acceptedData(t, svc.GitStatus(ctx, codemanagement.GitStatusInput{
		WorkspaceID: prepared.Workspace.ID,
		AgentID:     "builder",
	}))
	if !gitStatus.Dirty || !strings.Contains(gitStatus.Porcelain, "README.md") {
		t.Fatalf("git.status = %+v, want README.md", gitStatus)
	}
	diff := acceptedData(t, svc.GitDiff(ctx, codemanagement.GitDiffInput{
		WorkspaceID: prepared.Workspace.ID,
		AgentID:     "builder",
	}))
	if !strings.Contains(diff.Diff, "+workspace edit") {
		t.Fatalf("git.diff = %q, want tracked README edit", diff.Diff)
	}
	log := acceptedData(t, svc.GitLog(ctx, codemanagement.GitLogInput{
		WorkspaceID: prepared.Workspace.ID,
		AgentID:     "builder",
		MaxCount:    5,
	}))
	if len(log.Entries) == 0 || log.Entries[0].Summary != "initial" {
		t.Fatalf("git.log = %+v, want initial commit", log)
	}
	if countRows(t, ctx, db, "git_operations") != 1 {
		t.Fatalf("read-only operations wrote git_operations rows")
	}

	git(t, prepared.Workspace.Path, "checkout", "--", "README.md")
	advanceRepo(t, repoPath, "upstream.txt", "upstream\n", "upstream change")
	stale := acceptedData(t, svc.WorkspaceStatus(ctx, codemanagement.WorkspaceStatusInput{
		WorkspaceID: prepared.Workspace.ID,
		AgentID:     "builder",
	}))
	if !stale.StaleBase {
		t.Fatalf("workspace.status stale_base = false after canonical advanced: %+v", stale)
	}
	synced := acceptedData(t, svc.WorkspaceSync(ctx, codemanagement.WorkspaceSyncInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		ExpectedHeadRef: prepared.Workspace.HeadRef,
	}))
	if synced.Workspace.BaseRef == prepared.Workspace.BaseRef || synced.Workspace.HeadRef != synced.CanonicalRef {
		t.Fatalf("workspace.sync = %+v, want updated base/head to canonical", synced)
	}
	assertOperationAudit(t, ctx, db, synced.Operation.ID, "workspace.sync", "succeeded", prepared.Workspace.HeadRef, synced.Workspace.HeadRef)
}

func TestGitCommitRecordsDurableOperationLocksRefsAndEvent(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	before := prepared.Workspace.HeadRef
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	committed := acceptedData(t, svc.GitCommit(ctx, codemanagement.GitCommitInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		Message:         "add feature",
		Paths:           []string{"feature.txt"},
		ExpectedHeadRef: before,
	}))
	if committed.CommitSHA == before || committed.Workspace.HeadRef != committed.CommitSHA {
		t.Fatalf("git.commit = %+v, want new commit sha", committed)
	}
	assertOperationAudit(t, ctx, db, committed.Operation.ID, "git.commit", "succeeded", before, committed.CommitSHA)
	assertReleasedLocks(t, ctx, db, committed.Operation.ID, prepared.Workspace.ID, prepared.Workspace.RepoID)
	if got := git(t, prepared.Workspace.Path, "status", "--porcelain"); strings.TrimSpace(got) != "" {
		t.Fatalf("workspace dirty after commit: %q", got)
	}
	if got := git(t, prepared.Workspace.Path, "log", "-1", "--format=%s"); strings.TrimSpace(got) != "add feature" {
		t.Fatalf("latest commit = %q, want add feature", got)
	}
}

func TestGitCommitOnlyCommitsExplicitPathsWhenUnrelatedFileAlreadyStaged(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	before := prepared.Workspace.HeadRef
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "unrelated.txt"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	git(t, prepared.Workspace.Path, "add", "unrelated.txt")
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	committed := acceptedData(t, svc.GitCommit(ctx, codemanagement.GitCommitInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		Message:         "add scoped feature",
		Paths:           []string{"feature.txt"},
		ExpectedHeadRef: before,
	}))
	committedFiles := git(t, prepared.Workspace.Path, "show", "--name-only", "--format=", "HEAD")
	if !containsLine(committedFiles, "feature.txt") {
		t.Fatalf("scoped commit files = %q, want feature.txt", committedFiles)
	}
	if containsLine(committedFiles, "unrelated.txt") {
		t.Fatalf("scoped commit included unrelated staged file: %q", committedFiles)
	}
	staged := git(t, prepared.Workspace.Path, "diff", "--cached", "--name-only")
	if !containsLine(staged, "unrelated.txt") {
		t.Fatalf("unrelated staged file was not preserved after scoped commit; staged=%q", staged)
	}
	assertOperationAudit(t, ctx, db, committed.Operation.ID, "git.commit", "succeeded", before, committed.CommitSHA)
	if !committed.Workspace.Dirty {
		t.Fatalf("workspace dirty = false, want unrelated staged file to remain visible")
	}
}

func TestChangesetSubmitAndAbandonAreAudited(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "change.txt", "change\n", "add change")

	submitted := acceptedData(t, svc.SubmitChangeSet(ctx, codemanagement.SubmitChangeSetInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		ContractID:      "contract_123",
		Summary:         "change summary",
		EvidenceRefs:    []string{"report_123"},
		ExpectedHeadRef: commit.CommitSHA,
	}))
	if submitted.ChangeSet.State != "submitted" || submitted.ChangeSet.HeadRef != commit.CommitSHA {
		t.Fatalf("changeset.submit = %+v, want submitted at commit", submitted)
	}
	if len(submitted.ChangeSet.CommitIDs) != 1 || submitted.ChangeSet.CommitIDs[0] != commit.CommitSHA {
		t.Fatalf("changeset commits = %+v, want %s", submitted.ChangeSet.CommitIDs, commit.CommitSHA)
	}
	assertOperationAudit(t, ctx, db, submitted.Operation.ID, "changeset.submit", "succeeded", commit.CommitSHA, commit.CommitSHA)
	assertReleasedLocks(t, ctx, db, submitted.Operation.ID, prepared.Workspace.ID, prepared.Workspace.RepoID)

	abandoned := acceptedData(t, svc.AbandonChangeSet(ctx, codemanagement.AbandonChangeSetInput{
		ChangeSetID:     submitted.ChangeSet.ID,
		AgentID:         "builder",
		ExpectedHeadRef: commit.CommitSHA,
		Reason:          "superseded",
	}))
	if abandoned.ChangeSet.State != "abandoned" {
		t.Fatalf("changeset.abandon = %+v, want abandoned", abandoned)
	}
	assertOperationAudit(t, ctx, db, abandoned.Operation.ID, "changeset.abandon", "succeeded", commit.CommitSHA, commit.CommitSHA)
	assertReleasedLocks(t, ctx, db, abandoned.Operation.ID, prepared.Workspace.ID, prepared.Workspace.RepoID)
}

func TestGitCommitRejectsLockConflictWithDurableOperation(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	before := prepared.Workspace.HeadRef
	insertActiveLock(t, ctx, db, prepared.Workspace.ID, "builder")
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "locked.txt"), []byte("locked\n"), 0o644); err != nil {
		t.Fatalf("write locked: %v", err)
	}

	resp := svc.GitCommit(ctx, codemanagement.GitCommitInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		Message:         "locked commit",
		Paths:           []string{"locked.txt"},
		ExpectedHeadRef: before,
	})
	if resp.Status != capability.StatusRejected || resp.ErrorCode != "GIT_LOCK_CONFLICT" {
		t.Fatalf("git.commit lock conflict = %+v, want GIT_LOCK_CONFLICT", resp)
	}
	opID := resp.CanonicalIDs["operation_id"]
	assertOperationAudit(t, ctx, db, opID, "git.commit", "rejected", before, before)
	if got := strings.TrimSpace(git(t, prepared.Workspace.Path, "rev-parse", "HEAD")); got != before {
		t.Fatalf("HEAD after lock conflict = %s, want %s", got, before)
	}
	if active := activeLockCount(t, ctx, db, opID); active != 0 {
		t.Fatalf("new operation active locks = %d, want 0", active)
	}
}

func TestExpectedHeadAndDirtyTreeRejectsAreDurable(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	before := prepared.Workspace.HeadRef
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}

	submit := svc.SubmitChangeSet(ctx, codemanagement.SubmitChangeSetInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		Summary:         "dirty submit",
		ExpectedHeadRef: before,
	})
	if submit.Status != capability.StatusRejected || submit.ErrorCode != "WORKSPACE_DIRTY" {
		t.Fatalf("dirty changeset.submit = %+v, want WORKSPACE_DIRTY", submit)
	}
	assertOperationAudit(t, ctx, db, submit.CanonicalIDs["operation_id"], "changeset.submit", "rejected", before, before)
	if got := countRows(t, ctx, db, "changesets"); got != 0 {
		t.Fatalf("changesets after dirty reject = %d, want 0", got)
	}

	commit := svc.GitCommit(ctx, codemanagement.GitCommitInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		Message:         "wrong expected",
		Paths:           []string{"dirty.txt"},
		ExpectedHeadRef: "deadbeef",
	})
	if commit.Status != capability.StatusRejected || commit.ErrorCode != "EXPECTED_HEAD_MISMATCH" {
		t.Fatalf("expected-head commit = %+v, want EXPECTED_HEAD_MISMATCH", commit)
	}
	assertOperationAudit(t, ctx, db, commit.CanonicalIDs["operation_id"], "git.commit", "rejected", before, before)
	if got := strings.TrimSpace(git(t, prepared.Workspace.Path, "rev-parse", "HEAD")); got != before {
		t.Fatalf("HEAD after expected mismatch = %s, want %s", got, before)
	}
}

func TestWorkspacePrepareRepoLockConflictDoesNotLeavePreparingWorkspace(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	insertActiveRepoLock(t, ctx, db, prepared.Workspace.RepoID, "builder")
	beforeWorkspaces := countRows(t, ctx, db, "git_workspaces")

	resp := svc.WorkspacePrepare(ctx, codemanagement.WorkspacePrepareInput{
		RepoPath:        repoPath,
		CanonicalBranch: "main",
		WorkspaceRoot:   t.TempDir(),
		AgentID:         "builder",
		ContractID:      "contract_builder_2",
	})
	if resp.Status != capability.StatusRejected || resp.ErrorCode != "GIT_LOCK_CONFLICT" {
		t.Fatalf("workspace.prepare repo lock conflict = %+v, want GIT_LOCK_CONFLICT", resp)
	}
	opID := resp.CanonicalIDs["operation_id"]
	assertOperationAudit(t, ctx, db, opID, "workspace.prepare", "rejected", "", "")
	if active := activeLockCount(t, ctx, db, opID); active != 0 {
		t.Fatalf("new prepare operation active locks = %d, want 0", active)
	}
	if got := countRows(t, ctx, db, "git_workspaces"); got != beforeWorkspaces {
		t.Fatalf("workspace rows after rejected prepare = %d, want %d", got, beforeWorkspaces)
	}
	if preparing := countPreparingWorkspaces(t, ctx, db, prepared.Workspace.RepoID); preparing != 0 {
		t.Fatalf("preparing workspaces after rejected prepare = %d, want 0", preparing)
	}
}

func TestWorkspacePrepareMapsDockerRuntimeWorkspaceToHostAndReturnsAgentPath(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	hostRoot := t.TempDir()
	insertDockerRuntimeBridge(t, ctx, db, "rt_developer_a", "developer-a", hostRoot)

	prepared := acceptedData(t, svc.WorkspacePrepare(ctx, codemanagement.WorkspacePrepareInput{
		RepoPath:        repoPath,
		CanonicalBranch: "main",
		WorkspaceRoot:   cpruntime.ContainerWorkspacePath,
		AgentID:         "developer-a",
		RuntimeID:       "rt_developer_a",
		ContractID:      "contract_developer_a",
	}))
	if !strings.HasPrefix(prepared.Workspace.Path, hostRoot+string(os.PathSeparator)) {
		t.Fatalf("workspace host path = %s, want under %s", prepared.Workspace.Path, hostRoot)
	}
	wantAgentPrefix := cpruntime.ContainerWorkspacePath + "/"
	if !strings.HasPrefix(prepared.Workspace.AgentPath, wantAgentPrefix) {
		t.Fatalf("workspace agent path = %s, want under %s", prepared.Workspace.AgentPath, cpruntime.ContainerWorkspacePath)
	}
	if strings.Contains(prepared.Workspace.AgentPath, hostRoot) {
		t.Fatalf("workspace agent path leaked host root: %s", prepared.Workspace.AgentPath)
	}
	if _, err := os.Stat(filepath.Join(prepared.Workspace.Path, ".git")); err != nil {
		t.Fatalf("backend host workspace was not cloned: %v", err)
	}
}

func TestWorkspaceSyncRejectsLocalCommitsWithoutSilentMerge(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "local.txt", "local\n", "local change")
	advanceRepo(t, repoPath, "upstream.txt", "upstream\n", "upstream change")

	sync := svc.WorkspaceSync(ctx, codemanagement.WorkspaceSyncInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         "builder",
		ExpectedHeadRef: commit.CommitSHA,
	})
	if sync.Status != capability.StatusRejected || sync.ErrorCode != "NO_SILENT_MERGE" {
		t.Fatalf("workspace.sync with local commits = %+v, want NO_SILENT_MERGE", sync)
	}
	assertOperationAudit(t, ctx, db, sync.CanonicalIDs["operation_id"], "workspace.sync", "rejected", commit.CommitSHA, commit.CommitSHA)
	if got := strings.TrimSpace(git(t, prepared.Workspace.Path, "rev-parse", "HEAD")); got != commit.CommitSHA {
		t.Fatalf("HEAD after rejected sync = %s, want local commit %s", got, commit.CommitSHA)
	}
	if got := git(t, prepared.Workspace.Path, "log", "--format=%s", "-1"); strings.TrimSpace(got) != "local change" {
		t.Fatalf("latest workspace commit after rejected sync = %q, want local change", got)
	}
}

func TestMergePreviewApplyAndRollbackUseReviewedRefs(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "feature.txt", "feature\n", "add merge feature")
	submitted := submitChangeSet(t, ctx, svc, prepared, commit.CommitSHA, "merge feature")
	targetBefore := strings.TrimSpace(git(t, repoPath, "rev-parse", "main"))

	preview := acceptedData(t, svc.MergePreview(ctx, codemanagement.MergePreviewInput{
		ChangeSetID:       submitted.ChangeSet.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	}))
	if preview.MergeAttempt.State != "clean" || preview.MergeAttempt.ResultRef == "" {
		t.Fatalf("merge preview = %+v, want clean result ref", preview)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetBefore {
		t.Fatalf("canonical ref after preview = %s, want unchanged %s", got, targetBefore)
	}
	assertOperationAudit(t, ctx, db, preview.Operation.ID, "git.merge_preview", "succeeded", targetBefore, preview.MergeAttempt.ResultRef)
	assertReleasedLocks(t, ctx, db, preview.Operation.ID, prepared.Workspace.ID, prepared.Workspace.RepoID)

	applied := acceptedData(t, svc.MergeApply(ctx, codemanagement.MergeApplyInput{
		MergeAttemptID:    preview.MergeAttempt.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	}))
	if applied.AppliedRef != preview.MergeAttempt.ResultRef {
		t.Fatalf("merge apply applied_ref = %s, want reviewed result %s", applied.AppliedRef, preview.MergeAttempt.ResultRef)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != preview.MergeAttempt.ResultRef {
		t.Fatalf("canonical ref after apply = %s, want %s", got, preview.MergeAttempt.ResultRef)
	}
	if applied.RollbackPoint.BeforeRef != targetBefore || applied.RollbackPoint.AfterRef != applied.AppliedRef || applied.RollbackPoint.State != "available" {
		t.Fatalf("rollback point = %+v, want available %s -> %s", applied.RollbackPoint, targetBefore, applied.AppliedRef)
	}
	assertOperationAudit(t, ctx, db, applied.Operation.ID, "git.merge_apply", "succeeded", targetBefore, applied.AppliedRef)
	assertReleasedLocks(t, ctx, db, applied.Operation.ID, prepared.Workspace.ID, prepared.Workspace.RepoID)

	rolledBack := acceptedData(t, svc.Rollback(ctx, codemanagement.RollbackInput{
		OperationID:       applied.Operation.ID,
		AgentID:           "builder",
		ExpectedTargetRef: applied.AppliedRef,
	}))
	if rolledBack.RestoredRef != targetBefore || rolledBack.RollbackPoint.State != "used" {
		t.Fatalf("rollback = %+v, want restored target and used rollback point", rolledBack)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetBefore {
		t.Fatalf("canonical ref after rollback = %s, want %s", got, targetBefore)
	}
	assertOperationAudit(t, ctx, db, rolledBack.Operation.ID, "git.rollback", "succeeded", applied.AppliedRef, targetBefore)
}

func TestMergePreviewConflictFailClosedAndAbort(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "README.md", "builder\n", "builder readme")
	submitted := submitChangeSet(t, ctx, svc, prepared, commit.CommitSHA, "builder readme")
	targetAfter := advanceRepo(t, repoPath, "README.md", "upstream\n", "upstream readme")

	resp := svc.MergePreview(ctx, codemanagement.MergePreviewInput{
		ChangeSetID:       submitted.ChangeSet.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetAfter,
	})
	if resp.Status != capability.StatusRejected || resp.ErrorCode != "MERGE_CONFLICTS_FOUND" {
		t.Fatalf("merge preview conflict = %+v, want MERGE_CONFLICTS_FOUND", resp)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetAfter {
		t.Fatalf("canonical ref after conflict preview = %s, want unchanged %s", got, targetAfter)
	}
	assertOperationAudit(t, ctx, db, resp.CanonicalIDs["operation_id"], "git.merge_preview", "rejected", targetAfter, targetAfter)
	conflicts := acceptedData(t, svc.Conflicts(ctx, codemanagement.ConflictListInput{
		MergeAttemptID: resp.CanonicalIDs["merge_attempt_id"],
		AgentID:        "builder",
	}))
	if conflicts.ConflictSet.State != "open" || !contains(conflicts.ConflictSet.Files, "README.md") {
		t.Fatalf("conflict set = %+v, want open README.md", conflicts.ConflictSet)
	}

	aborted := acceptedData(t, svc.AbortMerge(ctx, codemanagement.AbortMergeInput{
		MergeAttemptID: resp.CanonicalIDs["merge_attempt_id"],
		AgentID:        "builder",
		Reason:         "superseded",
	}))
	if aborted.MergeAttempt.State != "aborted" || aborted.ConflictSet == nil || aborted.ConflictSet.State != "abandoned" {
		t.Fatalf("abort = %+v, want aborted attempt and abandoned conflict set", aborted)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetAfter {
		t.Fatalf("canonical ref after abort = %s, want unchanged %s", got, targetAfter)
	}
	assertOperationAudit(t, ctx, db, aborted.Operation.ID, "git.abort", "succeeded", targetAfter, targetAfter)
}

func TestManualResolveCanApplyReviewedResolution(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	first := commitWorkspaceFile(t, ctx, svc, prepared, "README.md", "builder\n", "builder readme")
	submitted := submitChangeSet(t, ctx, svc, prepared, first.CommitSHA, "builder readme")
	targetAfter := advanceRepo(t, repoPath, "README.md", "upstream\n", "upstream readme")
	conflict := svc.MergePreview(ctx, codemanagement.MergePreviewInput{
		ChangeSetID:       submitted.ChangeSet.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetAfter,
	})
	if conflict.Status != capability.StatusRejected || conflict.ErrorCode != "MERGE_CONFLICTS_FOUND" {
		t.Fatalf("merge preview conflict = %+v, want MERGE_CONFLICTS_FOUND", conflict)
	}

	resolution := commitWorkspaceFile(t, ctx, svc, prepared, "README.md", "upstream\nbuilder\n", "manual resolution")
	resolved := acceptedData(t, svc.ResolveMerge(ctx, codemanagement.ResolveMergeInput{
		MergeAttemptID:    conflict.CanonicalIDs["merge_attempt_id"],
		AgentID:           "builder",
		ResolvedHeadRef:   resolution.CommitSHA,
		ExpectedTargetRef: targetAfter,
	}))
	if resolved.MergeAttempt.State != "resolved" || resolved.ConflictSet.State != "resolved" || resolved.MergeAttempt.ResultRef == "" {
		t.Fatalf("resolve = %+v, want resolved merge result", resolved)
	}
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetAfter {
		t.Fatalf("canonical ref after resolve = %s, want unchanged %s", got, targetAfter)
	}
	assertOperationAudit(t, ctx, db, resolved.Operation.ID, "git.resolve", "succeeded", targetAfter, resolved.MergeAttempt.ResultRef)

	applied := acceptedData(t, svc.MergeApply(ctx, codemanagement.MergeApplyInput{
		MergeAttemptID:    resolved.MergeAttempt.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetAfter,
	}))
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != resolved.MergeAttempt.ResultRef || got != applied.AppliedRef {
		t.Fatalf("canonical ref after resolved apply = %s, want %s", got, resolved.MergeAttempt.ResultRef)
	}
	content, err := os.ReadFile(filepath.Join(repoPath, "README.md"))
	if err != nil {
		t.Fatalf("read resolved README: %v", err)
	}
	if string(content) != "upstream\nbuilder\n" {
		t.Fatalf("resolved README = %q, want manual resolution", content)
	}
	assertOperationAudit(t, ctx, db, applied.Operation.ID, "git.merge_apply", "succeeded", targetAfter, applied.AppliedRef)
}

func TestMergeApplyRejectsLockConflictAndStaleTarget(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "feature.txt", "feature\n", "add feature")
	submitted := submitChangeSet(t, ctx, svc, prepared, commit.CommitSHA, "feature")
	targetBefore := strings.TrimSpace(git(t, repoPath, "rev-parse", "main"))
	preview := acceptedData(t, svc.MergePreview(ctx, codemanagement.MergePreviewInput{
		ChangeSetID:       submitted.ChangeSet.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	}))

	insertActiveRepoLock(t, ctx, db, prepared.Workspace.RepoID, "builder")
	locked := svc.MergeApply(ctx, codemanagement.MergeApplyInput{
		MergeAttemptID:    preview.MergeAttempt.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	})
	if locked.Status != capability.StatusRejected || locked.ErrorCode != "GIT_LOCK_CONFLICT" {
		t.Fatalf("merge apply lock conflict = %+v, want GIT_LOCK_CONFLICT", locked)
	}
	assertOperationAudit(t, ctx, db, locked.CanonicalIDs["operation_id"], "git.merge_apply", "rejected", targetBefore, targetBefore)
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetBefore {
		t.Fatalf("canonical ref after locked apply = %s, want %s", got, targetBefore)
	}
}

func TestMergeApplyRejectsStaleTargetWithoutSilentMerge(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	repoPath := newGitRepo(t)
	prepared := prepareWorkspace(t, ctx, svc, repoPath, "builder")
	commit := commitWorkspaceFile(t, ctx, svc, prepared, "feature.txt", "feature\n", "add feature")
	submitted := submitChangeSet(t, ctx, svc, prepared, commit.CommitSHA, "feature")
	targetBefore := strings.TrimSpace(git(t, repoPath, "rev-parse", "main"))
	preview := acceptedData(t, svc.MergePreview(ctx, codemanagement.MergePreviewInput{
		ChangeSetID:       submitted.ChangeSet.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	}))
	targetAfter := advanceRepo(t, repoPath, "upstream.txt", "upstream\n", "upstream change")

	stale := svc.MergeApply(ctx, codemanagement.MergeApplyInput{
		MergeAttemptID:    preview.MergeAttempt.ID,
		AgentID:           "builder",
		ExpectedTargetRef: targetBefore,
	})
	if stale.Status != capability.StatusRejected || stale.ErrorCode != "STALE_EXPECTED_REF" {
		t.Fatalf("merge apply stale expected = %+v, want STALE_EXPECTED_REF", stale)
	}
	assertOperationAudit(t, ctx, db, stale.CanonicalIDs["operation_id"], "git.merge_apply", "rejected", targetAfter, targetAfter)
	if got := strings.TrimSpace(git(t, repoPath, "rev-parse", "main")); got != targetAfter {
		t.Fatalf("canonical ref after stale apply = %s, want %s", got, targetAfter)
	}

	staleNoExpected := svc.MergeApply(ctx, codemanagement.MergeApplyInput{
		MergeAttemptID: preview.MergeAttempt.ID,
		AgentID:        "builder",
	})
	if staleNoExpected.Status != capability.StatusRejected || staleNoExpected.ErrorCode != "STALE_TARGET_REF" {
		t.Fatalf("merge apply stale target = %+v, want STALE_TARGET_REF", staleNoExpected)
	}
	assertOperationAudit(t, ctx, db, staleNoExpected.CanonicalIDs["operation_id"], "git.merge_apply", "rejected", targetAfter, targetAfter)
}

func TestGitOperationRecoveryFailsRunningOperationAndReleasesLocks(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	prepared := prepareWorkspace(t, ctx, svc, newGitRepo(t), "builder")
	insertActiveLock(t, ctx, db, prepared.Workspace.ID, "builder")

	recovered := acceptedData(t, svc.RecoverOperations(ctx, codemanagement.RecoverOperationsInput{
		OperationID: "gitop_active_lock",
		AgentID:     "builder",
	}))
	if len(recovered.RecoveredOperations) != 1 || recovered.RecoveredOperations[0].State != "failed" {
		t.Fatalf("recover = %+v, want one failed operation", recovered)
	}
	if active := activeLockCount(t, ctx, db, "gitop_active_lock"); active != 0 {
		t.Fatalf("recovered operation active locks = %d, want 0", active)
	}
	assertOperationAudit(t, ctx, db, "gitop_active_lock", "test.lock", "failed", "", "")
}

func newService(t *testing.T) (*codemanagement.Service, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	s := store.New(db)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return codemanagement.NewService(s), db
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.name", "Test User")
	git(t, dir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-m", "initial")
	git(t, dir, "branch", "-M", "main")
	return dir
}

func prepareWorkspace(t *testing.T, ctx context.Context, svc *codemanagement.Service, repoPath, agentID string) codemanagement.WorkspacePrepareResult {
	t.Helper()
	return acceptedData(t, svc.WorkspacePrepare(ctx, codemanagement.WorkspacePrepareInput{
		RepoPath:        repoPath,
		CanonicalBranch: "main",
		WorkspaceRoot:   t.TempDir(),
		AgentID:         agentID,
		ContractID:      "contract_" + agentID,
	}))
}

func insertDockerRuntimeBridge(t *testing.T, ctx context.Context, db *sql.DB, runtimeID, agentID, hostWorkspace string) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO runtime_instances (
  id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id,
  lease_id, container_id, container_name, image, network, state,
  workspace_path, home_path, host_workspace_ref, host_home_ref,
  checks_json, env_keys_json, created_at, updated_at
) VALUES (?, ?, 'docker-default', 'docker', ?, 'att_bridge',
  'lease_bridge', 'container_bridge', 'coordplane-bridge',
  'coordplane/claude-runtime:release-health', 'coordplane-release-health', 'ready',
  ?, ?, ?, ?, '{}', '[]', ?, ?)`,
		"rti_"+runtimeID, runtimeID, agentID, cpruntime.ContainerWorkspacePath,
		cpruntime.ContainerHomePath, hostWorkspace, filepath.Join(t.TempDir(), "home"), now, now,
	); err != nil {
		t.Fatalf("insert docker runtime bridge: %v", err)
	}
}

func commitWorkspaceFile(t *testing.T, ctx context.Context, svc *codemanagement.Service, prepared codemanagement.WorkspacePrepareResult, path, content, message string) codemanagement.GitCommitResult {
	t.Helper()
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, path), []byte(content), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	return acceptedData(t, svc.GitCommit(ctx, codemanagement.GitCommitInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         prepared.Workspace.AgentID,
		Message:         message,
		Paths:           []string{path},
		ExpectedHeadRef: strings.TrimSpace(git(t, prepared.Workspace.Path, "rev-parse", "HEAD")),
	}))
}

func submitChangeSet(t *testing.T, ctx context.Context, svc *codemanagement.Service, prepared codemanagement.WorkspacePrepareResult, expectedHead, summary string) codemanagement.SubmitChangeSetResult {
	t.Helper()
	return acceptedData(t, svc.SubmitChangeSet(ctx, codemanagement.SubmitChangeSetInput{
		WorkspaceID:     prepared.Workspace.ID,
		AgentID:         prepared.Workspace.AgentID,
		ContractID:      prepared.Workspace.ContractID,
		Summary:         summary,
		ExpectedHeadRef: expectedHead,
	}))
}

func advanceRepo(t *testing.T, repoPath, path, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoPath, path), []byte(content), 0o644); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	git(t, repoPath, "add", path)
	git(t, repoPath, "commit", "-m", message)
	return strings.TrimSpace(git(t, repoPath, "rev-parse", "HEAD"))
}

func acceptedData[T any](t *testing.T, response capability.Response[T]) T {
	t.Helper()
	if response.Status != capability.StatusAccepted || !response.OK || response.Data == nil {
		t.Fatalf("response = %+v, want accepted", response)
	}
	return *response.Data
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", fullArgs, err, out)
	}
	return string(out)
}

func assertOperationAudit(t *testing.T, ctx context.Context, db *sql.DB, operationID, operationType, state, beforeRef, afterRef string) {
	t.Helper()
	var gotType, gotState, gotBefore, gotAfter, feedback string
	if err := db.QueryRowContext(ctx, `
SELECT operation_type, state, before_ref, after_ref, feedback_json
FROM git_operations
WHERE id = ?`, operationID).Scan(&gotType, &gotState, &gotBefore, &gotAfter, &feedback); err != nil {
		t.Fatalf("query operation %s: %v", operationID, err)
	}
	if gotType != operationType || gotState != state || gotBefore != beforeRef || gotAfter != afterRef {
		t.Fatalf("operation %s = type:%s state:%s before:%s after:%s, want %s/%s/%s/%s", operationID, gotType, gotState, gotBefore, gotAfter, operationType, state, beforeRef, afterRef)
	}
	if !strings.Contains(feedback, "message") {
		t.Fatalf("operation %s feedback = %s, want structured message", operationID, feedback)
	}
	var events int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM events
WHERE aggregate_type = 'git_operation' AND aggregate_id = ? AND event_type = ?`, operationID, "git.operation."+state).Scan(&events); err != nil {
		t.Fatalf("count operation events: %v", err)
	}
	if events != 1 {
		t.Fatalf("operation %s events = %d, want 1", operationID, events)
	}
}

func assertReleasedLocks(t *testing.T, ctx context.Context, db *sql.DB, operationID, workspaceID, repoID string) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM git_locks
WHERE operation_id = ? AND state = 'released'
  AND ((scope_kind = 'workspace' AND scope_id = ?) OR (scope_kind = 'repo' AND scope_id = ?))`, operationID, workspaceID, repoID).Scan(&count); err != nil {
		t.Fatalf("count released locks: %v", err)
	}
	if count != 2 {
		t.Fatalf("released locks for %s = %d, want workspace+repo", operationID, count)
	}
}

func insertActiveLock(t *testing.T, ctx context.Context, db *sql.DB, workspaceID, agentID string) {
	t.Helper()
	var repoID string
	if err := db.QueryRowContext(ctx, `SELECT repo_id FROM git_workspaces WHERE id = ?`, workspaceID).Scan(&repoID); err != nil {
		t.Fatalf("query workspace repo: %v", err)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_operations (
  id, tenant_id, operation_type, actor_agent_id, workspace_id, repo_id,
  before_ref, after_ref, stdout, stderr, exit_code, state, feedback_json, created_at
) VALUES (?, 'default', 'test.lock', ?, ?, ?, '', '', '', '', 0, 'running', '{"message":"test lock"}', ?)`,
		"gitop_active_lock", agentID, workspaceID, repoID, now); err != nil {
		t.Fatalf("insert active lock operation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_locks (
  id, tenant_id, scope_kind, scope_id, operation_id, owner_agent_id,
  state, created_at, updated_at, expires_at
) VALUES (?, 'default', 'workspace', ?, 'gitop_active_lock', ?, 'active', ?, ?, ?)`,
		"gitlock_active_workspace", workspaceID, agentID, now, now, now); err != nil {
		t.Fatalf("insert active workspace lock: %v", err)
	}
}

func insertActiveRepoLock(t *testing.T, ctx context.Context, db *sql.DB, repoID, agentID string) {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_operations (
  id, tenant_id, operation_type, actor_agent_id, workspace_id, repo_id,
  before_ref, after_ref, stdout, stderr, exit_code, state, feedback_json, created_at
) VALUES (?, 'default', 'test.repo_lock', ?, NULL, ?, '', '', '', '', 0, 'running', '{"message":"test repo lock"}', ?)`,
		"gitop_active_repo_lock", agentID, repoID, now); err != nil {
		t.Fatalf("insert active repo lock operation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO git_locks (
  id, tenant_id, scope_kind, scope_id, operation_id, owner_agent_id,
  state, created_at, updated_at, expires_at
) VALUES (?, 'default', 'repo', ?, 'gitop_active_repo_lock', ?, 'active', ?, ?, ?)`,
		"gitlock_active_repo", repoID, agentID, now, now, now); err != nil {
		t.Fatalf("insert active repo lock: %v", err)
	}
}

func activeLockCount(t *testing.T, ctx context.Context, db *sql.DB, operationID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_locks WHERE operation_id = ? AND state = 'active'`, operationID).Scan(&count); err != nil {
		t.Fatalf("count active locks: %v", err)
	}
	return count
}

func countPreparingWorkspaces(t *testing.T, ctx context.Context, db *sql.DB, repoID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_workspaces WHERE repo_id = ? AND state = 'preparing'`, repoID).Scan(&count); err != nil {
		t.Fatalf("count preparing workspaces: %v", err)
	}
	return count
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count rows in %s: %v", table, err)
	}
	return count
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsLine(raw, want string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
