package cpprobe_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/backend"
	cpcapability "coordplane/internal/capability"
	"coordplane/internal/codemanagement"
	"coordplane/internal/coordination"
	"coordplane/internal/cpprobe"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/validation"
)

func TestCPProbeConcurrentGitGateCoversStaleConflictRetryAndNegativeMatrix(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tinyLedger, err := cpprobe.GenerateTinyLedger(ctx, dir)
	if err != nil {
		t.Fatalf("generate tiny ledger fixture: %v", err)
	}
	app, err := backend.Open(ctx, backend.Config{
		DBPath:               filepath.Join(dir, "coordplane.db"),
		ListenAddr:           "127.0.0.1:0",
		TeamConfigPath:       cpProbeTeamConfigPath(t),
		RuntimeWorkspaceRoot: filepath.Join(dir, "runtime-workspaces"),
		RuntimeHomeRoot:      filepath.Join(dir, "runtime-homes"),
	})
	if err != nil {
		t.Fatalf("open backend with CP-PROBE fixture: %v", err)
	}
	defer app.Close()
	link := coordlink.New(app.Dispatcher)
	trace := cpprobe.ManualTrace{Scenario: cpprobe.ScenarioID}
	developers := prepareCPProbeDeveloperWorkspaces(t, ctx, app, link, &trace, tinyLedger, dir)
	baseRef := tinyLedger.BaseRef

	writeWorkspaceFiles(t, developers.B.Workspace.Path, transactionCountOnlyFiles())
	bCommit := commitWorkspace(t, ctx, link, &trace, "developer-b", developers.B.Workspace.ID, "add transaction count", tinyLedgerPaths(), baseRef)
	bSubmit := submitChangeSet(t, ctx, link, &trace, "developer-b", developers.B.Workspace.ID, developers.B.Workspace.ContractID, "transaction_count report output", bCommit.CommitSHA)
	bOldPreview := acceptedCapability[codemanagement.MergePreviewResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.merge_preview", "preview developer-b transaction_count from old base", map[string]string{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": baseRef,
	}))
	if bOldPreview.MergeAttempt.State != "clean" {
		t.Fatalf("developer-b old-base preview state = %s, want clean", bOldPreview.MergeAttempt.State)
	}

	writeWorkspaceFiles(t, developers.A.Workspace.Path, categoryOnlyFiles())
	aCommit := commitWorkspace(t, ctx, link, &trace, "developer-a", developers.A.Workspace.ID, "add category summary", tinyLedgerPaths(), baseRef)
	aSubmit := submitChangeSet(t, ctx, link, &trace, "developer-a", developers.A.Workspace.ID, developers.A.Workspace.ContractID, "category summary", aCommit.CommitSHA)
	aPreview := acceptedCapability[codemanagement.MergePreviewResult](t, callCapability(t, ctx, link, &trace, "developer-a", "git.merge_preview", "preview developer-a category changeset", map[string]string{
		"changeset_id":        aSubmit.ChangeSet.ID,
		"expected_target_ref": baseRef,
	}))
	aApply := acceptedCapability[codemanagement.MergeApplyResult](t, callCapability(t, ctx, link, &trace, "developer-a", "git.merge_apply", "apply developer-a category changeset", map[string]string{
		"merge_attempt_id":    aPreview.MergeAttempt.ID,
		"expected_target_ref": baseRef,
	}))
	canonicalAfterA := gitRef(t, tinyLedger.RepoPath, "main")
	if aApply.AppliedRef != canonicalAfterA || canonicalAfterA == baseRef {
		t.Fatalf("developer-a apply ref = %s canonical=%s base=%s", aApply.AppliedRef, canonicalAfterA, baseRef)
	}

	staleApply := callCapability(t, ctx, link, &trace, "developer-b", "git.merge_apply", "developer-b stale apply of old-base preview", map[string]string{
		"merge_attempt_id": bOldPreview.MergeAttempt.ID,
	})
	assertRejectedOperation(t, ctx, app.DB, staleApply, "STALE_TARGET_REF", "git.merge_apply", canonicalAfterA, canonicalAfterA, "workspace.sync", "git.merge_preview")
	if got := gitRef(t, tinyLedger.RepoPath, "main"); got != canonicalAfterA {
		t.Fatalf("canonical after stale apply = %s, want unchanged %s", got, canonicalAfterA)
	}

	conflictPreview := callCapability(t, ctx, link, &trace, "developer-b", "git.merge_preview", "developer-b conflict preview after category merge", map[string]string{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": canonicalAfterA,
	})
	assertRejectedOperation(t, ctx, app.DB, conflictPreview, "MERGE_CONFLICTS_FOUND", "git.merge_preview", canonicalAfterA, canonicalAfterA, "git.conflicts", "git.resolve", "git.abort")
	conflicts := acceptedCapability[codemanagement.ConflictListResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.conflicts", "inspect developer-b conflict set", map[string]string{
		"merge_attempt_id": conflictPreview.CanonicalIDs["merge_attempt_id"],
	}))
	if conflicts.ConflictSet.State != "open" || !containsName(conflicts.ConflictSet.Files, "ledger/ledger.go") {
		t.Fatalf("conflict set = %+v, want open ledger/ledger.go conflict", conflicts.ConflictSet)
	}
	aborted := acceptedCapability[codemanagement.AbortMergeResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.abort", "abort first conflict attempt", map[string]string{
		"merge_attempt_id": conflictPreview.CanonicalIDs["merge_attempt_id"],
		"reason":           "exercise conflict abort before retrying with manual resolution",
	}))
	if aborted.MergeAttempt.State != "aborted" || aborted.ConflictSet == nil || aborted.ConflictSet.State != "abandoned" {
		t.Fatalf("abort result = %+v, want aborted attempt and abandoned conflict set", aborted)
	}
	assertOperationState(t, ctx, app.DB, aborted.Operation.ID, "git.abort", "succeeded", canonicalAfterA, canonicalAfterA)
	if got := gitRef(t, tinyLedger.RepoPath, "main"); got != canonicalAfterA {
		t.Fatalf("canonical after conflict abort = %s, want unchanged %s", got, canonicalAfterA)
	}

	writeWorkspaceFiles(t, developers.B.Workspace.Path, resolvedCategoryAndCountFiles())
	bResolveCommit := commitWorkspace(t, ctx, link, &trace, "developer-b", developers.B.Workspace.ID, "resolve category and transaction count", tinyLedgerPaths(), bCommit.CommitSHA)
	retryConflict := callCapability(t, ctx, link, &trace, "developer-b", "git.merge_preview", "retry developer-b conflict preview for manual resolution", map[string]string{
		"changeset_id":        bSubmit.ChangeSet.ID,
		"expected_target_ref": canonicalAfterA,
	})
	assertRejectedOperation(t, ctx, app.DB, retryConflict, "MERGE_CONFLICTS_FOUND", "git.merge_preview", canonicalAfterA, canonicalAfterA, "git.conflicts", "git.resolve", "git.abort")
	resolved := acceptedCapability[codemanagement.ResolveMergeResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.resolve", "resolve developer-b conflict with same-session commit", map[string]string{
		"merge_attempt_id":    retryConflict.CanonicalIDs["merge_attempt_id"],
		"resolved_head_ref":   bResolveCommit.CommitSHA,
		"expected_target_ref": canonicalAfterA,
	}))
	if resolved.MergeAttempt.State != "resolved" || resolved.ConflictSet.State != "resolved" || resolved.ConflictSet.ResolvedBy != "developer-b" {
		t.Fatalf("resolve result = %+v, want resolved by developer-b", resolved)
	}
	bApply := acceptedCapability[codemanagement.MergeApplyResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.merge_apply", "apply developer-b resolved changeset", map[string]string{
		"merge_attempt_id":    resolved.MergeAttempt.ID,
		"expected_target_ref": canonicalAfterA,
	}))
	canonicalAfterB := gitRef(t, tinyLedger.RepoPath, "main")
	if bApply.AppliedRef != canonicalAfterB || canonicalAfterB == canonicalAfterA {
		t.Fatalf("developer-b apply ref = %s canonical=%s afterA=%s", bApply.AppliedRef, canonicalAfterB, canonicalAfterA)
	}
	assertTinyLedgerFinalBehavior(t, tinyLedger.RepoPath)

	missingBefore := gitRef(t, developers.B.Workspace.Path, "HEAD")
	if err := os.WriteFile(filepath.Join(developers.B.Workspace.Path, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}
	missingPath := callCapability(t, ctx, link, &trace, "developer-b", "git.commit", "reject missing explicit commit paths", map[string]string{
		"workspace_id":      developers.B.Workspace.ID,
		"message":           "missing paths",
		"expected_head_ref": missingBefore,
	})
	assertRejectedOperation(t, ctx, app.DB, missingPath, "GIT_COMMIT_PATHS_REQUIRED", "git.commit", missingBefore, missingBefore, "git.status", "git.diff")
	if got := gitRef(t, developers.B.Workspace.Path, "HEAD"); got != missingBefore {
		t.Fatalf("workspace head after missing path reject = %s, want %s", got, missingBefore)
	}

	abandon := exerciseChangeSetAbandon(t, ctx, link, &trace, developers.A.Repository.ID, tinyLedger.RepoPath, canonicalAfterB, dir)
	if abandon.ChangeSet.State != "abandoned" {
		t.Fatalf("abandoned changeset state = %s, want abandoned", abandon.ChangeSet.State)
	}
	if got := gitRef(t, tinyLedger.RepoPath, "main"); got != canonicalAfterB {
		t.Fatalf("canonical after changeset abandon = %s, want unchanged %s", got, canonicalAfterB)
	}

	rollback := exerciseRollback(t, ctx, link, &trace, developers.A.Repository.ID, tinyLedger.RepoPath, canonicalAfterB, dir)
	if rollback.RollbackPoint.State != "used" || rollback.RestoredRef != canonicalAfterB {
		t.Fatalf("rollback result = %+v, want used rollback point restored to %s", rollback, canonicalAfterB)
	}
	if got := gitRef(t, tinyLedger.RepoPath, "main"); got != canonicalAfterB {
		t.Fatalf("canonical after rollback = %s, want restored %s", got, canonicalAfterB)
	}

	aReport := submitReport(t, ctx, link, &trace, "developer-a", developers.DeveloperALeaseID, "developer-a category summary merged", "category summary changeset="+aSubmit.ChangeSet.ID)
	acceptedCapability[coordination.CompleteContractResult](t, callCapability(t, ctx, link, &trace, "developer-a", "contract.complete", "complete developer-a contract", map[string]any{
		"lease_id":     developers.DeveloperALeaseID,
		"evidence_ids": []string{aReport.ID},
		"summary":      "developer-a category summary merged through controlled Git",
	}))
	bReport := submitReport(t, ctx, link, &trace, "developer-b", developers.DeveloperBLeaseID, "developer-b transaction_count merged", "transaction_count changeset="+bSubmit.ChangeSet.ID)
	acceptedCapability[coordination.CompleteContractResult](t, callCapability(t, ctx, link, &trace, "developer-b", "contract.complete", "complete developer-b contract", map[string]any{
		"lease_id":     developers.DeveloperBLeaseID,
		"evidence_ids": []string{bReport.ID},
		"summary":      "developer-b repaired stale/conflict feedback and merged transaction_count",
	}))

	verifierContract := acceptedCapability[coordination.AddContractResult](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.add", "dispatch verifier validation", map[string]any{
		"lease_id":        developers.CoordinatorLeaseID,
		"title":           "Verifier: CP-PROBE-001 non-Docker closeout",
		"objective":       "Assess Tiny Ledger final state and CP-PROBE gate evidence through validation.assessment.",
		"target_agent_id": "verifier",
		"completion_requirements": []string{
			"validation_assessment",
		},
	}))
	acceptedCapability[coordination.Assignment](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.wait", "wait for verifier validation", map[string]string{
		"lease_id":         developers.CoordinatorLeaseID,
		"reason":           "waiting for verifier validation.assessment",
		"waiting_for_ref":  verifierContract.ContractID,
		"session_route_id": "cp-probe-001-concurrent-git",
	}))
	verifierSession := startCPProbeSession(t, ctx, app, &trace, "verifier")
	acceptedCapability[coordination.ContractContext](t, callCapabilityWithRuntime(t, ctx, link, &trace, "verifier", verifierSession.Route.RuntimeID, "contract.context", "verifier reads validation contract context", map[string]string{
		"lease_id": verifierSession.LeaseID,
	}, map[string]string{
		"lease_id": verifierSession.LeaseID,
	}))
	verifierReport := acceptedCapability[coordination.Evidence](t, callCapabilityWithRuntime(t, ctx, link, &trace, "verifier", verifierSession.Route.RuntimeID, "report.submit", "verifier submits durable review report", map[string]string{
		"lease_id": verifierSession.LeaseID,
		"summary":  "CP-PROBE-001 non-Docker gate reviewed",
		"content":  "final_ref=" + canonicalAfterB + "\nchangesets=" + aSubmit.ChangeSet.ID + "," + bSubmit.ChangeSet.ID,
	}, map[string]string{
		"lease_id": verifierSession.LeaseID,
	}))
	assessment := acceptedCapability[validation.Result](t, callCapabilityWithRuntime(t, ctx, link, &trace, "verifier", verifierSession.Route.RuntimeID, "validation.assessment", "verifier records canonical pass assessment", map[string]any{
		"lease_id":             verifierSession.LeaseID,
		"assessed_contract_id": developers.RootContractID,
		"verdict":              "pass",
		"reason":               "non-Docker CP-PROBE workflow completed with durable Git gate evidence and final Tiny Ledger behavior",
		"summary":              "CP-PROBE-001 non-Docker closeout passed",
		"checked_refs": []map[string]string{
			{"kind": "evidence", "id": verifierReport.ID},
		},
	}, map[string]string{
		"lease_id": verifierSession.LeaseID,
	}))
	if assessment.Verdict != "pass" || assessment.EvidenceID == "" || assessment.AssessedContractID != developers.RootContractID {
		t.Fatalf("validation assessment = %+v, want pass for root", assessment)
	}
	acceptedCapability[coordination.CompleteContractResult](t, callCapabilityWithRuntime(t, ctx, link, &trace, "verifier", verifierSession.Route.RuntimeID, "contract.complete", "complete verifier contract with validation evidence", map[string]any{
		"lease_id":     verifierSession.LeaseID,
		"evidence_ids": []string{assessment.EvidenceID},
		"summary":      "validation.assessment submitted pass verdict",
	}, map[string]string{
		"lease_id": verifierSession.LeaseID,
	}))
	coordinatorMail := acceptedCapability[[]coordination.MailboxItem](t, callCapability(t, ctx, link, &trace, "coordinator", "mailbox.list", "coordinator reads child completion mailbox", nil))
	verifierMailbox := requireMailboxForContract(t, coordinatorMail, verifierContract.ContractID)
	acceptedCapability[coordination.MailboxItem](t, callCapability(t, ctx, link, &trace, "coordinator", "mailbox.get", "coordinator opens verifier validation feedback", map[string]string{
		"mailbox_id": verifierMailbox.ID,
	}))
	acceptedCapability[coordination.MailboxItem](t, callCapability(t, ctx, link, &trace, "coordinator", "mailbox.resolve", "coordinator resolves verifier validation feedback", map[string]string{
		"mailbox_id":   verifierMailbox.ID,
		"followup_ref": "validation_assessment:" + assessment.AssessmentID,
	}))
	rootReport := submitReport(t, ctx, link, &trace, "coordinator", developers.CoordinatorLeaseID, "CP-PROBE-001 root completion report", "validation_evidence="+assessment.EvidenceID+"\nfinal_ref="+canonicalAfterB)
	rootComplete := acceptedCapability[coordination.CompleteContractResult](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.complete", "complete CP-PROBE root contract", map[string]any{
		"lease_id":     developers.CoordinatorLeaseID,
		"evidence_ids": []string{rootReport.ID},
		"summary":      "coordinator read verifier validation feedback and completed root contract",
	}))
	if rootComplete.Status != "satisfied" || rootComplete.ContractID != developers.RootContractID {
		t.Fatalf("root completion = %+v, want satisfied root", rootComplete)
	}
	if got := contractStatus(t, ctx, app.DB, developers.RootContractID); got != "satisfied" {
		t.Fatalf("root contract status = %s, want satisfied", got)
	}

	failureMatrix := cpprobe.FailureMatrix{
		Scenario: cpprobe.ScenarioID,
		Items: []cpprobe.FailureMatrixItem{
			{
				ID:                 "concurrent-stale-target",
				Status:             "covered",
				Capability:         "git.merge_apply",
				ExpectedErrorCodes: []string{"STALE_TARGET_REF"},
				ActualErrorCode:    staleApply.ErrorCode,
				StateAssertion:     "old-base merge apply is rejected and canonical branch remains at developer-a ref",
			},
			{
				ID:                 "conflict-abort",
				Status:             "covered",
				Capability:         "git.merge_preview/git.abort",
				ExpectedErrorCodes: []string{"MERGE_CONFLICTS_FOUND"},
				ActualErrorCode:    conflictPreview.ErrorCode,
				StateAssertion:     "conflict set is abandoned and target ref is unchanged after abort",
			},
			{
				ID:                 "same-session-conflict-resolve",
				Status:             "covered",
				Capability:         "git.resolve/git.merge_apply",
				ExpectedErrorCodes: []string{"MERGE_CONFLICTS_FOUND"},
				ActualErrorCode:    retryConflict.ErrorCode,
				StateAssertion:     "developer-b resolves a fresh conflict attempt and final canonical branch contains category summary plus transaction_count",
			},
			{
				ID:                 "missing-path-scope",
				Status:             "covered",
				Capability:         "git.commit",
				ExpectedErrorCodes: []string{"GIT_COMMIT_PATHS_REQUIRED"},
				ActualErrorCode:    missingPath.ErrorCode,
				StateAssertion:     "commit is rejected, workspace HEAD is unchanged, and rejected GitOperation is durable",
			},
			{
				ID:             "changeset-abandon",
				Status:         "covered",
				Capability:     "changeset.abandon",
				StateAssertion: "submitted changeset moves to abandoned and canonical branch is unchanged",
			},
			{
				ID:             "rollback",
				Status:         "covered",
				Capability:     "git.rollback",
				StateAssertion: "applied throwaway merge is restored to its before ref and rollback point becomes used",
			},
			{
				ID:             "validation-root-closeout",
				Status:         "covered",
				Capability:     "validation.assessment/contract.complete",
				StateAssertion: "verifier records a canonical pass assessment, coordinator reads child completion feedback, and root contract is satisfied",
			},
		},
	}
	if err := failureMatrix.Validate(); err != nil {
		t.Fatalf("failure matrix schema invalid: %v", err)
	}
	inspect, err := app.Inspect(ctx)
	if err != nil {
		t.Fatalf("inspect backend: %v", err)
	}
	gitSummary := cpprobe.GitOperationSummary{
		Scenario: cpprobe.ScenarioID,
		Repositories: []cpprobe.RepositorySummary{
			{ID: developers.A.Repository.ID, CanonicalBranch: developers.A.Repository.CanonicalBranch, Status: developers.A.Repository.Status},
		},
		Workspaces:     workspaceSummaries(t, ctx, app.DB),
		Operations:     gitOperationBriefs(t, ctx, app.DB),
		RollbackPoints: rollbackPointBriefs(t, ctx, app.DB),
		NoActiveLocks:  countRowsWhere(t, ctx, app.DB, "git_locks", "state = 'active'") == 0,
	}
	if err := gitSummary.Validate(); err != nil {
		t.Fatalf("git summary schema invalid: %v", err)
	}
	assertGitSummaryIncludes(t, gitSummary, "changeset.abandon")
	assertGitSummaryIncludes(t, gitSummary, "git.rollback")
	if len(gitSummary.RollbackPoints) == 0 {
		t.Fatal("git summary rollback_points is empty, want rollback point evidence")
	}
	redacted := cpprobe.RedactedInspect{
		Scenario:       cpprobe.ScenarioID,
		Status:         inspect.Status,
		TeamID:         inspect.TeamID,
		RootContractID: developers.RootContractID,
		Counts:         inspect.Counts,
		Contracts:      contractSummaries(t, ctx, app.DB),
		Workspaces:     gitSummary.Workspaces,
		GitOperations:  gitSummary.Operations,
		Redacted:       true,
		DenylistChecks: []string{
			"coordplane.db",
			"host home",
			"token value",
			"other agent workspace path",
		},
	}
	if err := redacted.Validate(); err != nil {
		t.Fatalf("redacted inspect schema invalid: %v", err)
	}
	conclusion := cpprobe.ConclusionReport{
		Scenario:         cpprobe.ScenarioID,
		Status:           cpprobe.ConclusionPassed,
		ManualTraceRef:   cpprobe.ManualTraceArtifact,
		InspectRef:       cpprobe.InspectRedactedArtifact,
		GitSummaryRef:    cpprobe.GitOperationSummaryArtifact,
		FailureMatrixRef: cpprobe.FailureMatrixArtifact,
		Covered: []string{
			"backend startup and TeamConfig load",
			"root/developer/verifier contract flow",
			"concurrent stale/conflict Git gate and same-session retry",
			"negative failure matrix for missing path, stale target, conflict abort, abandon, and rollback",
			"validation.assessment canonical pass",
			"coordinator mailbox feedback read and root completion",
			"physical trace/report artifact writer",
		},
		NextSteps: []string{
			"replay the same CP-PROBE contract flow with real Docker/Claude agents",
		},
	}
	if err := conclusion.Validate(); err != nil {
		t.Fatalf("conclusion schema invalid: %v", err)
	}
	artifactDir := filepath.Join(dir, "cp-probe-artifacts")
	if err := cpprobe.WriteReportArtifacts(artifactDir, cpprobe.ReportArtifacts{
		ManualTrace:   trace,
		Inspect:       redacted,
		GitSummary:    gitSummary,
		FailureMatrix: failureMatrix,
		Conclusion:    conclusion,
	}); err != nil {
		t.Fatalf("write CP-PROBE reports: %v", err)
	}
	assertReportArtifacts(t, artifactDir, dir)
	if got := countRowsWhere(t, ctx, app.DB, "git_locks", "state = 'active'"); got != 0 {
		t.Fatalf("active git locks after concurrent gate = %d, want 0", got)
	}
	if err := trace.Validate(); err != nil {
		t.Fatalf("manual trace schema invalid: %v", err)
	}
}

type preparedDevelopers struct {
	RootContractID       string
	CoordinatorLeaseID   string
	DeveloperAContractID string
	DeveloperBContractID string
	DeveloperALeaseID    string
	DeveloperBLeaseID    string
	A                    codemanagement.WorkspacePrepareResult
	B                    codemanagement.WorkspacePrepareResult
}

func prepareCPProbeDeveloperWorkspaces(t *testing.T, ctx context.Context, app *backend.Backend, link *coordlink.Adapter, trace *cpprobe.ManualTrace, tinyLedger cpprobe.TinyLedgerFixture, dir string) preparedDevelopers {
	t.Helper()
	repo, err := app.CodeManagement.RegisterRepository(ctx, codemanagement.RegisterRepositoryInput{
		RepoPath:        tinyLedger.RepoPath,
		Alias:           "cp-probe-concurrent-test",
		CanonicalBranch: tinyLedger.CanonicalBranch,
	})
	if err != nil {
		t.Fatalf("register tiny ledger repository: %v", err)
	}
	root, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "CP-PROBE-001 concurrent git root",
		Objective:     "Drive Tiny Ledger through concurrent category and transaction_count changes.",
		TargetAgentID: "coordinator",
		CompletionRequirements: []string{
			"report",
		},
	})
	if err != nil {
		t.Fatalf("create root contract: %v", err)
	}
	trace.Steps = append(trace.Steps, cpprobe.TraceStep{
		Actor:        "operator",
		EntryPoint:   "go-service",
		Capability:   "contract.add",
		InputSummary: "create concurrent git root contract",
		Status:       string(cpcapability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"contract_id": root.ContractID,
		},
	})
	coordinator := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, trace, "coordinator", "assignment.next", "claim concurrent git root", nil))
	developerAContract := acceptedCapability[coordination.AddContractResult](t, callCapability(t, ctx, link, trace, "coordinator", "contract.add", "dispatch developer-a category work", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer A: add category summary",
		"objective":       "Add category summary to Tiny Ledger and merge through controlled Git.",
		"target_agent_id": "developer-a",
		"completion_requirements": []string{
			"report",
		},
	}))
	developerBContract := acceptedCapability[coordination.AddContractResult](t, callCapability(t, ctx, link, trace, "coordinator", "contract.add", "dispatch developer-b transaction_count work", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer B: add transaction_count",
		"objective":       "Add transaction_count from the old base and handle stale/conflict feedback.",
		"target_agent_id": "developer-b",
		"completion_requirements": []string{
			"report",
		},
	}))
	acceptedCapability[coordination.Assignment](t, callCapability(t, ctx, link, trace, "coordinator", "contract.wait", "wait for concurrent developer contracts", map[string]string{
		"lease_id":         coordinator.Lease.ID,
		"reason":           "waiting for concurrent git gate",
		"waiting_for_ref":  developerAContract.ContractID + "," + developerBContract.ContractID,
		"session_route_id": "cp-probe-001-concurrent-git",
	}))
	developerA := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, trace, "developer-a", "assignment.next", "claim category contract", nil))
	developerB := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, trace, "developer-b", "assignment.next", "claim transaction_count contract", nil))
	if developerA.Contract.ID != developerAContract.ContractID || developerB.Contract.ID != developerBContract.ContractID {
		t.Fatalf("developer assignments = %+v/%+v, want dispatched contracts", developerA, developerB)
	}
	preparedA := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, trace, "developer-a", "workspace.prepare", "prepare category workspace", map[string]string{
		"repo_id":          repo.ID,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(dir, "workspaces", "developer-a"),
		"contract_id":      developerA.Contract.ID,
	}))
	preparedB := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, trace, "developer-b", "workspace.prepare", "prepare transaction_count workspace", map[string]string{
		"repo_id":          repo.ID,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(dir, "workspaces", "developer-b"),
		"contract_id":      developerB.Contract.ID,
	}))
	if preparedA.Workspace.BaseRef != tinyLedger.BaseRef || preparedB.Workspace.BaseRef != tinyLedger.BaseRef {
		t.Fatalf("developer base refs = %s/%s, want %s", preparedA.Workspace.BaseRef, preparedB.Workspace.BaseRef, tinyLedger.BaseRef)
	}
	return preparedDevelopers{
		RootContractID:       root.ContractID,
		CoordinatorLeaseID:   coordinator.Lease.ID,
		DeveloperAContractID: developerAContract.ContractID,
		DeveloperBContractID: developerBContract.ContractID,
		DeveloperALeaseID:    developerA.Lease.ID,
		DeveloperBLeaseID:    developerB.Lease.ID,
		A:                    preparedA,
		B:                    preparedB,
	}
}

func commitWorkspace(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, workspaceID, message string, paths []string, expectedHead string) codemanagement.GitCommitResult {
	t.Helper()
	return acceptedCapability[codemanagement.GitCommitResult](t, callCapability(t, ctx, link, trace, actor, "git.commit", message, map[string]any{
		"workspace_id":      workspaceID,
		"message":           message,
		"paths":             paths,
		"expected_head_ref": expectedHead,
	}))
}

func submitChangeSet(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, workspaceID, contractID, summary, expectedHead string) codemanagement.SubmitChangeSetResult {
	t.Helper()
	return acceptedCapability[codemanagement.SubmitChangeSetResult](t, callCapability(t, ctx, link, trace, actor, "changeset.submit", summary, map[string]string{
		"workspace_id":      workspaceID,
		"contract_id":       contractID,
		"summary":           summary,
		"expected_head_ref": expectedHead,
	}))
}

func submitReport(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, leaseID, summary, content string) coordination.Evidence {
	t.Helper()
	return acceptedCapability[coordination.Evidence](t, callCapability(t, ctx, link, trace, actor, "report.submit", summary, map[string]string{
		"lease_id": leaseID,
		"summary":  summary,
		"content":  content,
	}))
}

func startCPProbeSession(t *testing.T, ctx context.Context, app *backend.Backend, trace *cpprobe.ManualTrace, agentID string) cpruntime.AssignmentSession {
	t.Helper()
	session, err := app.Runner.StartNext(ctx, agentID)
	if err != nil {
		t.Fatalf("start %s session: %v", agentID, err)
	}
	trace.Steps = append(trace.Steps, cpprobe.TraceStep{
		Actor:        agentID,
		EntryPoint:   "runtime.runner",
		Capability:   "session.start",
		InputSummary: "start non-Docker external runtime session",
		Status:       string(cpcapability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"attempt_id":       session.AttemptID,
			"lease_id":         session.LeaseID,
			"session_route_id": session.Route.ID,
			"runtime_id":       session.Route.RuntimeID,
		},
	})
	return session
}

func requireMailboxForContract(t *testing.T, items []coordination.MailboxItem, contractID string) coordination.MailboxItem {
	t.Helper()
	for _, item := range items {
		if strings.Contains(item.FollowupRef, "child_contract:"+contractID) {
			return item
		}
	}
	t.Fatalf("mailbox for contract %s not found in %+v", contractID, items)
	return coordination.MailboxItem{}
}

func exerciseChangeSetAbandon(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, repoID, repoPath, expectedCanonical, dir string) codemanagement.AbandonChangeSetResult {
	t.Helper()
	prepared := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, trace, "developer-a", "workspace.prepare", "prepare abandon workspace", map[string]string{
		"repo_id":          repoID,
		"canonical_branch": "main",
		"workspace_root":   filepath.Join(dir, "workspaces", "abandon"),
		"contract_id":      "cp-probe-abandon",
	}))
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "abandoned.txt"), []byte("abandoned\n"), 0o644); err != nil {
		t.Fatalf("write abandoned file: %v", err)
	}
	commit := commitWorkspace(t, ctx, link, trace, "developer-a", prepared.Workspace.ID, "add abandoned changeset", []string{"abandoned.txt"}, prepared.Workspace.HeadRef)
	submitted := submitChangeSet(t, ctx, link, trace, "developer-a", prepared.Workspace.ID, prepared.Workspace.ContractID, "abandon me", commit.CommitSHA)
	result := acceptedCapability[codemanagement.AbandonChangeSetResult](t, callCapability(t, ctx, link, trace, "developer-a", "changeset.abandon", "abandon unmerged changeset", map[string]string{
		"changeset_id":      submitted.ChangeSet.ID,
		"expected_head_ref": commit.CommitSHA,
		"reason":            "negative matrix abandon",
	}))
	if got := gitRef(t, repoPath, "main"); got != expectedCanonical {
		t.Fatalf("canonical after abandon = %s, want %s", got, expectedCanonical)
	}
	return result
}

func exerciseRollback(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, repoID, repoPath, expectedCanonical, dir string) codemanagement.RollbackResult {
	t.Helper()
	prepared := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, trace, "developer-a", "workspace.prepare", "prepare rollback workspace", map[string]string{
		"repo_id":          repoID,
		"canonical_branch": "main",
		"workspace_root":   filepath.Join(dir, "workspaces", "rollback"),
		"contract_id":      "cp-probe-rollback",
	}))
	if prepared.Workspace.BaseRef != expectedCanonical {
		t.Fatalf("rollback workspace base = %s, want %s", prepared.Workspace.BaseRef, expectedCanonical)
	}
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "rollback.txt"), []byte("rollback\n"), 0o644); err != nil {
		t.Fatalf("write rollback file: %v", err)
	}
	commit := commitWorkspace(t, ctx, link, trace, "developer-a", prepared.Workspace.ID, "add rollback target", []string{"rollback.txt"}, prepared.Workspace.HeadRef)
	submitted := submitChangeSet(t, ctx, link, trace, "developer-a", prepared.Workspace.ID, prepared.Workspace.ContractID, "rollback target", commit.CommitSHA)
	preview := acceptedCapability[codemanagement.MergePreviewResult](t, callCapability(t, ctx, link, trace, "developer-a", "git.merge_preview", "preview rollback target", map[string]string{
		"changeset_id":        submitted.ChangeSet.ID,
		"expected_target_ref": expectedCanonical,
	}))
	applied := acceptedCapability[codemanagement.MergeApplyResult](t, callCapability(t, ctx, link, trace, "developer-a", "git.merge_apply", "apply rollback target", map[string]string{
		"merge_attempt_id":    preview.MergeAttempt.ID,
		"expected_target_ref": expectedCanonical,
	}))
	if applied.AppliedRef == expectedCanonical {
		t.Fatalf("rollback target did not advance canonical ref")
	}
	return acceptedCapability[codemanagement.RollbackResult](t, callCapability(t, ctx, link, trace, "developer-a", "git.rollback", "rollback applied target", map[string]string{
		"operation_id":        applied.Operation.ID,
		"expected_target_ref": applied.AppliedRef,
	}))
}

func assertRejectedOperation(t *testing.T, ctx context.Context, db *sql.DB, response cpcapability.Response[json.RawMessage], code, operationType, beforeRef, afterRef string, allowedNextActions ...string) {
	t.Helper()
	if response.Status != cpcapability.StatusRejected || response.ErrorCode != code {
		t.Fatalf("response = %+v, want rejected %s", response, code)
	}
	for _, action := range allowedNextActions {
		if !containsName(response.AllowedNextActions, action) {
			t.Fatalf("rejected %s next actions = %v, want %s", code, response.AllowedNextActions, action)
		}
	}
	operationID := response.CanonicalIDs["operation_id"]
	if operationID == "" {
		t.Fatalf("rejected response missing operation_id: %+v", response)
	}
	assertOperationState(t, ctx, db, operationID, operationType, "rejected", beforeRef, afterRef)
}

func assertOperationState(t *testing.T, ctx context.Context, db *sql.DB, operationID, operationType, state, beforeRef, afterRef string) {
	t.Helper()
	var gotType, gotState, gotBefore, gotAfter string
	if err := db.QueryRowContext(ctx, `
SELECT operation_type, state, before_ref, after_ref
FROM git_operations
WHERE id = ?`, operationID).Scan(&gotType, &gotState, &gotBefore, &gotAfter); err != nil {
		t.Fatalf("query operation %s: %v", operationID, err)
	}
	if gotType != operationType || gotState != state || gotBefore != beforeRef || gotAfter != afterRef {
		t.Fatalf("operation %s = %s/%s/%s/%s, want %s/%s/%s/%s", operationID, gotType, gotState, gotBefore, gotAfter, operationType, state, beforeRef, afterRef)
	}
}

func assertTinyLedgerFinalBehavior(t *testing.T, repoPath string) {
	t.Helper()
	runCommand(t, repoPath, "go", "test", "./...")
	report := runCommand(t, repoPath, "go", "run", "./cmd/ledger-report")
	var decoded struct {
		Income           int            `json:"income"`
		Expense          int            `json:"expense"`
		Balance          int            `json:"balance"`
		TransactionCount int            `json:"transaction_count"`
		Categories       map[string]int `json:"categories"`
	}
	if err := json.Unmarshal([]byte(report), &decoded); err != nil {
		t.Fatalf("decode ledger report %q: %v", report, err)
	}
	if decoded.Income != 125 || decoded.Expense != 40 || decoded.Balance != 85 || decoded.TransactionCount != 3 ||
		decoded.Categories["sales"] != 100 || decoded.Categories["ops"] != 40 || decoded.Categories["services"] != 25 {
		t.Fatalf("ledger report = %+v, want category summary and transaction_count", decoded)
	}
}

func contractStatus(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&status); err != nil {
		t.Fatalf("query contract status %s: %v", contractID, err)
	}
	return status
}

func contractSummaries(t *testing.T, ctx context.Context, db *sql.DB) []cpprobe.ContractSummary {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
SELECT id, COALESCE(issuer_contract_id, ''), target_id, status
FROM work_contracts
ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query contract summaries: %v", err)
	}
	defer rows.Close()
	var out []cpprobe.ContractSummary
	for rows.Next() {
		var item cpprobe.ContractSummary
		if err := rows.Scan(&item.ID, &item.IssuerContract, &item.TargetID, &item.Status); err != nil {
			t.Fatalf("scan contract summary: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate contract summaries: %v", err)
	}
	return out
}

func workspaceSummaries(t *testing.T, ctx context.Context, db *sql.DB) []cpprobe.WorkspaceSummary {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
SELECT id, repo_id, agent_id, COALESCE(contract_id, ''), base_ref, head_ref, state
FROM git_workspaces
ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query workspace summaries: %v", err)
	}
	defer rows.Close()
	var out []cpprobe.WorkspaceSummary
	for rows.Next() {
		var item cpprobe.WorkspaceSummary
		if err := rows.Scan(&item.ID, &item.RepoID, &item.AgentID, &item.ContractID, &item.BaseRef, &item.HeadRef, &item.State); err != nil {
			t.Fatalf("scan workspace summary: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate workspace summaries: %v", err)
	}
	return out
}

func gitOperationBriefs(t *testing.T, ctx context.Context, db *sql.DB) []cpprobe.GitOperationBrief {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
SELECT id, operation_type, actor_agent_id, COALESCE(workspace_id, ''), repo_id,
  subject_kind, runtime_id, execution_location, before_ref, after_ref, state
FROM git_operations
ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query git operation summaries: %v", err)
	}
	defer rows.Close()
	var out []cpprobe.GitOperationBrief
	for rows.Next() {
		var item cpprobe.GitOperationBrief
		if err := rows.Scan(&item.ID, &item.OperationType, &item.ActorAgentID, &item.WorkspaceID, &item.RepoID, &item.SubjectKind, &item.RuntimeID, &item.ExecutionLocation, &item.BeforeRef, &item.AfterRef, &item.State); err != nil {
			t.Fatalf("scan git operation summary: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate git operation summaries: %v", err)
	}
	return out
}

func rollbackPointBriefs(t *testing.T, ctx context.Context, db *sql.DB) []cpprobe.RollbackPointBrief {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
SELECT id, operation_id, repo_id, COALESCE(workspace_id, ''), target_ref, before_ref, after_ref, state
FROM git_rollback_points
ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query rollback point summaries: %v", err)
	}
	defer rows.Close()
	var out []cpprobe.RollbackPointBrief
	for rows.Next() {
		var item cpprobe.RollbackPointBrief
		if err := rows.Scan(&item.ID, &item.OperationID, &item.RepoID, &item.WorkspaceID, &item.TargetRef, &item.BeforeRef, &item.AfterRef, &item.State); err != nil {
			t.Fatalf("scan rollback point summary: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rollback point summaries: %v", err)
	}
	return out
}

func assertGitSummaryIncludes(t *testing.T, summary cpprobe.GitOperationSummary, operationType string) {
	t.Helper()
	for _, op := range summary.Operations {
		if op.OperationType == operationType {
			return
		}
	}
	t.Fatalf("git summary missing operation type %s: %+v", operationType, summary.Operations)
}

func assertReportArtifacts(t *testing.T, dir, forbiddenPath string) {
	t.Helper()
	for _, name := range []string{
		cpprobe.ManualTraceArtifact,
		cpprobe.InspectRedactedArtifact,
		cpprobe.GitOperationSummaryArtifact,
		cpprobe.FailureMatrixArtifact,
		cpprobe.ConclusionReportArtifact,
	} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read artifact %s: %v", name, err)
		}
		if len(body) == 0 {
			t.Fatalf("artifact %s is empty", name)
		}
		if strings.Contains(string(body), forbiddenPath) {
			t.Fatalf("artifact %s leaked forbidden path: %s", name, body)
		}
	}
	var inspect cpprobe.RedactedInspect
	if err := readArtifactJSON(dir, cpprobe.InspectRedactedArtifact, &inspect); err != nil {
		t.Fatalf("decode inspect artifact: %v", err)
	}
	if !inspect.Redacted || inspect.RootContractID == "" {
		t.Fatalf("inspect artifact = %+v, want redacted root summary", inspect)
	}
	var gitSummary cpprobe.GitOperationSummary
	if err := readArtifactJSON(dir, cpprobe.GitOperationSummaryArtifact, &gitSummary); err != nil {
		t.Fatalf("decode git summary artifact: %v", err)
	}
	assertGitSummaryIncludes(t, gitSummary, "changeset.abandon")
	assertGitSummaryIncludes(t, gitSummary, "git.rollback")
	if len(gitSummary.RollbackPoints) == 0 {
		t.Fatal("git summary artifact missing rollback_points")
	}
	conclusion, err := os.ReadFile(filepath.Join(dir, cpprobe.ConclusionReportArtifact))
	if err != nil {
		t.Fatalf("read conclusion artifact: %v", err)
	}
	if !strings.Contains(string(conclusion), "Status: passed") {
		t.Fatalf("conclusion artifact missing passed status:\n%s", conclusion)
	}
}

func readArtifactJSON(dir, name string, target any) error {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func writeWorkspaceFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create dir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func gitRef(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, dir, "rev-parse", ref))
}

func runCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v in %s failed: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func tinyLedgerPaths() []string {
	return cpprobe.TinyLedgerPaths()
}

func categoryOnlyFiles() map[string]string {
	return cpprobe.CategoryOnlyFiles()
}

func transactionCountOnlyFiles() map[string]string {
	return cpprobe.TransactionCountOnlyFiles()
}

func resolvedCategoryAndCountFiles() map[string]string {
	return cpprobe.ResolvedCategoryAndCountFiles()
}

const categorizedTransactionsCSV = `id,type,amount,category
1,income,100,sales
2,expense,40,ops
3,income,25,services
`

const categoryOnlyLedgerGo = `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID       int
	Type     string
	Amount   int
	Category string
}

type Summary struct {
	Income     int            ` + "`json:\"income\"`" + `
	Expense    int            ` + "`json:\"expense\"`" + `
	Balance    int            ` + "`json:\"balance\"`" + `
	Categories map[string]int ` + "`json:\"categories\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 4 || got[0] != "id" || got[1] != "type" || got[2] != "amount" || got[3] != "category" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 columns", line+2)
		}
		id, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, fmt.Errorf("line %d id: %w", line+2, err)
		}
		amount, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, fmt.Errorf("line %d amount: %w", line+2, err)
		}
		switch row[1] {
		case "income", "expense":
		default:
			return nil, fmt.Errorf("line %d type: expected income or expense", line+2)
		}
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount, Category: row[3]})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	summary := Summary{Categories: map[string]int{}}
	for _, tx := range transactions {
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
		if tx.Category != "" {
			summary.Categories[tx.Category] += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`

const categoryOnlyLedgerTestGo = `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount,category\n1,income,100,sales\n2,expense,40,ops\n3,income,25,services\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	if got.Income != 125 || got.Expense != 40 || got.Balance != 85 {
		t.Fatalf("summary totals = %+v", got)
	}
	if got.Categories["sales"] != 100 || got.Categories["ops"] != 40 || got.Categories["services"] != 25 {
		t.Fatalf("categories = %+v", got.Categories)
	}
}
`

const categoryOnlyMainGo = `package main

import (
	"encoding/json"
	"fmt"
	"os"

	"tiny-ledger-probe/ledger"
)

func main() {
	path := "data/transactions.csv"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()
	transactions, err := ledger.Read(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(ledger.Summarize(transactions)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

const transactionCountOnlyLedgerGo = `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID     int
	Type   string
	Amount int
}

type Summary struct {
	Income           int ` + "`json:\"income\"`" + `
	Expense          int ` + "`json:\"expense\"`" + `
	Balance          int ` + "`json:\"balance\"`" + `
	TransactionCount int ` + "`json:\"transaction_count\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 3 || got[0] != "id" || got[1] != "type" || got[2] != "amount" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 3 {
			return nil, fmt.Errorf("line %d: expected 3 columns", line+2)
		}
		id, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, fmt.Errorf("line %d id: %w", line+2, err)
		}
		amount, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, fmt.Errorf("line %d amount: %w", line+2, err)
		}
		switch row[1] {
		case "income", "expense":
		default:
			return nil, fmt.Errorf("line %d type: expected income or expense", line+2)
		}
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	var summary Summary
	for _, tx := range transactions {
		summary.TransactionCount++
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`

const transactionCountOnlyLedgerTestGo = `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount\n1,income,100\n2,expense,40\n3,income,25\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	want := Summary{Income: 125, Expense: 40, Balance: 85, TransactionCount: 3}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}
`

const transactionCountOnlyMainGo = `package main

import (
	"encoding/json"
	"fmt"
	"os"

	"tiny-ledger-probe/ledger"
)

func main() {
	path := "data/transactions.csv"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()
	transactions, err := ledger.Read(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(ledger.Summarize(transactions)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

const resolvedLedgerGo = `package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

type Transaction struct {
	ID       int
	Type     string
	Amount   int
	Category string
}

type Summary struct {
	Income           int            ` + "`json:\"income\"`" + `
	Expense          int            ` + "`json:\"expense\"`" + `
	Balance          int            ` + "`json:\"balance\"`" + `
	TransactionCount int            ` + "`json:\"transaction_count\"`" + `
	Categories       map[string]int ` + "`json:\"categories\"`" + `
}

func Read(r io.Reader) ([]Transaction, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("transactions csv is empty")
	}
	if got := rows[0]; len(got) != 4 || got[0] != "id" || got[1] != "type" || got[2] != "amount" || got[3] != "category" {
		return nil, fmt.Errorf("unexpected transactions header")
	}
	transactions := make([]Transaction, 0, len(rows)-1)
	for line, row := range rows[1:] {
		if len(row) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 columns", line+2)
		}
		id, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, fmt.Errorf("line %d id: %w", line+2, err)
		}
		amount, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, fmt.Errorf("line %d amount: %w", line+2, err)
		}
		switch row[1] {
		case "income", "expense":
		default:
			return nil, fmt.Errorf("line %d type: expected income or expense", line+2)
		}
		transactions = append(transactions, Transaction{ID: id, Type: row[1], Amount: amount, Category: row[3]})
	}
	return transactions, nil
}

func Summarize(transactions []Transaction) Summary {
	summary := Summary{Categories: map[string]int{}}
	for _, tx := range transactions {
		summary.TransactionCount++
		switch tx.Type {
		case "income":
			summary.Income += tx.Amount
		case "expense":
			summary.Expense += tx.Amount
		}
		if tx.Category != "" {
			summary.Categories[tx.Category] += tx.Amount
		}
	}
	summary.Balance = summary.Income - summary.Expense
	return summary
}
`

const resolvedLedgerTestGo = `package ledger

import (
	"strings"
	"testing"
)

func TestReadAndSummarizeTinyLedgerFixture(t *testing.T) {
	transactions, err := Read(strings.NewReader("id,type,amount,category\n1,income,100,sales\n2,expense,40,ops\n3,income,25,services\n"))
	if err != nil {
		t.Fatalf("read transactions: %v", err)
	}
	got := Summarize(transactions)
	if got.Income != 125 || got.Expense != 40 || got.Balance != 85 || got.TransactionCount != 3 {
		t.Fatalf("summary totals/count = %+v", got)
	}
	if got.Categories["sales"] != 100 || got.Categories["ops"] != 40 || got.Categories["services"] != 25 {
		t.Fatalf("categories = %+v", got.Categories)
	}
}
`
