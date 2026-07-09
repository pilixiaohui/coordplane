package cpprobe_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/backend"
	cpcapability "coordplane/internal/capability"
	"coordplane/internal/codemanagement"
	"coordplane/internal/coordination"
	"coordplane/internal/cpprobe"
)

func TestCPProbeManualServiceProtocolSkeletonPreparesIsolatedDeveloperWorkspaces(t *testing.T) {
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
	if !app.TeamConfigLoaded || app.TeamConfig.TeamID != cpprobe.TeamID {
		t.Fatalf("backend TeamConfig = loaded:%v %+v, want CP-PROBE fixture", app.TeamConfigLoaded, app.TeamConfig)
	}

	link := coordlink.New(app.Dispatcher)
	trace := cpprobe.ManualTrace{Scenario: cpprobe.ScenarioID}
	repo, err := app.CodeManagement.RegisterRepository(ctx, codemanagement.RegisterRepositoryInput{
		RepoPath:        tinyLedger.RepoPath,
		Alias:           "cp-probe-manual-test",
		CanonicalBranch: tinyLedger.CanonicalBranch,
	})
	if err != nil {
		t.Fatalf("register tiny ledger repository: %v", err)
	}
	root, err := app.Coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "CP-PROBE-001 tiny-ledger root",
		Objective:     "Drive tiny-ledger-probe through the manual service protocol skeleton.",
		TargetAgentID: "coordinator",
		CompletionRequirements: []string{
			"validation.assessment",
		},
	})
	if err != nil {
		t.Fatalf("create root contract: %v", err)
	}
	trace.Steps = append(trace.Steps, cpprobe.TraceStep{
		Actor:        "operator",
		EntryPoint:   "go-service",
		Capability:   "contract.add",
		InputSummary: "create root contract for coordinator",
		Status:       string(cpcapability.StatusAccepted),
		CanonicalIDs: map[string]string{
			"contract_id":   root.ContractID,
			"assignment_id": root.AssignmentID,
		},
	})

	coordinator := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, &trace, "coordinator", "assignment.next", "claim root contract", nil))
	if coordinator.Idle || coordinator.Contract.ID != root.ContractID || coordinator.Lease.ID == "" {
		t.Fatalf("coordinator assignment = %+v, want claimed root contract", coordinator)
	}
	setLastTraceIDs(&trace, map[string]string{
		"contract_id":   coordinator.Contract.ID,
		"assignment_id": coordinator.Assignment.ID,
		"lease_id":      coordinator.Lease.ID,
	})
	acceptedCapability[coordination.Contract](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.current", "read current root contract", map[string]string{
		"lease_id": coordinator.Lease.ID,
	}))
	acceptedCapability[coordination.ContractContext](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.context", "read root context", map[string]string{
		"lease_id": coordinator.Lease.ID,
	}))

	developerAContract := acceptedCapability[coordination.AddContractResult](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.add", "dispatch developer-a", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer A: prepare isolated Tiny Ledger workspace",
		"objective":       "Prepare a workspace from the CP-PROBE Tiny Ledger canonical repo and inspect status/log.",
		"target_agent_id": "developer-a",
		"completion_requirements": []string{
			"report",
		},
	}))
	setLastTraceIDs(&trace, map[string]string{
		"contract_id":   developerAContract.ContractID,
		"assignment_id": developerAContract.AssignmentID,
	})
	developerBContract := acceptedCapability[coordination.AddContractResult](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.add", "dispatch developer-b", map[string]any{
		"lease_id":        coordinator.Lease.ID,
		"title":           "Developer B: prepare isolated Tiny Ledger workspace",
		"objective":       "Prepare a workspace from the same CP-PROBE Tiny Ledger canonical repo and inspect status/log.",
		"target_agent_id": "developer-b",
		"completion_requirements": []string{
			"report",
		},
	}))
	setLastTraceIDs(&trace, map[string]string{
		"contract_id":   developerBContract.ContractID,
		"assignment_id": developerBContract.AssignmentID,
	})
	waiting := acceptedCapability[coordination.Assignment](t, callCapability(t, ctx, link, &trace, "coordinator", "contract.wait", "wait for both developer contracts", map[string]string{
		"lease_id":         coordinator.Lease.ID,
		"reason":           "waiting for developer-a and developer-b workspace prepare evidence",
		"waiting_for_ref":  developerAContract.ContractID + "," + developerBContract.ContractID,
		"session_route_id": "cp-probe-001-manual",
	}))
	if waiting.State != "waiting" {
		t.Fatalf("coordinator wait state = %+v, want waiting", waiting)
	}

	developerA := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, &trace, "developer-a", "assignment.next", "claim developer-a contract", nil))
	developerB := acceptedCapability[coordination.AssignmentNextResult](t, callCapability(t, ctx, link, &trace, "developer-b", "assignment.next", "claim developer-b contract", nil))
	if developerA.Contract.ID != developerAContract.ContractID || developerB.Contract.ID != developerBContract.ContractID {
		t.Fatalf("developer assignments = %+v/%+v, want dispatched child contracts", developerA, developerB)
	}

	preparedA := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, &trace, "developer-a", "workspace.prepare", "prepare developer-a workspace", map[string]string{
		"repo_id":          repo.ID,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(dir, "workspaces", "developer-a"),
		"contract_id":      developerA.Contract.ID,
	}))
	setLastTraceIDs(&trace, map[string]string{
		"repo_id":       preparedA.Repository.ID,
		"workspace_id":  preparedA.Workspace.ID,
		"operation_id":  preparedA.OperationID,
		"contract_id":   developerA.Contract.ID,
		"workspace_ref": preparedA.Workspace.BaseRef,
	})
	preparedB := acceptedCapability[codemanagement.WorkspacePrepareResult](t, callCapability(t, ctx, link, &trace, "developer-b", "workspace.prepare", "prepare developer-b workspace", map[string]string{
		"repo_id":          repo.ID,
		"canonical_branch": tinyLedger.CanonicalBranch,
		"workspace_root":   filepath.Join(dir, "workspaces", "developer-b"),
		"contract_id":      developerB.Contract.ID,
	}))
	setLastTraceIDs(&trace, map[string]string{
		"repo_id":       preparedB.Repository.ID,
		"workspace_id":  preparedB.Workspace.ID,
		"operation_id":  preparedB.OperationID,
		"contract_id":   developerB.Contract.ID,
		"workspace_ref": preparedB.Workspace.BaseRef,
	})
	if preparedA.Repository.ID != preparedB.Repository.ID {
		t.Fatalf("prepared repos = %s/%s, want shared canonical repo", preparedA.Repository.ID, preparedB.Repository.ID)
	}
	if preparedA.Workspace.ID == preparedB.Workspace.ID || preparedA.Workspace.Path == preparedB.Workspace.Path {
		t.Fatalf("developer workspaces are not isolated: %+v %+v", preparedA.Workspace, preparedB.Workspace)
	}
	if preparedA.Workspace.BaseRef != tinyLedger.BaseRef || preparedB.Workspace.BaseRef != tinyLedger.BaseRef {
		t.Fatalf("workspace base refs = %s/%s, want fixture base %s", preparedA.Workspace.BaseRef, preparedB.Workspace.BaseRef, tinyLedger.BaseRef)
	}

	statusA := acceptedCapability[codemanagement.WorkspaceStatus](t, callCapability(t, ctx, link, &trace, "developer-a", "workspace.status", "read developer-a workspace status", map[string]string{
		"workspace_id": preparedA.Workspace.ID,
	}))
	statusB := acceptedCapability[codemanagement.WorkspaceStatus](t, callCapability(t, ctx, link, &trace, "developer-b", "workspace.status", "read developer-b workspace status", map[string]string{
		"workspace_id": preparedB.Workspace.ID,
	}))
	if statusA.BaseRef != statusB.BaseRef || statusA.BaseRef != tinyLedger.BaseRef {
		t.Fatalf("status base refs = %s/%s, want %s", statusA.BaseRef, statusB.BaseRef, tinyLedger.BaseRef)
	}
	logA := acceptedCapability[codemanagement.GitLogResult](t, callCapability(t, ctx, link, &trace, "developer-a", "git.log", "read developer-a git log", map[string]any{
		"workspace_id": preparedA.Workspace.ID,
		"max_count":    3,
	}))
	logB := acceptedCapability[codemanagement.GitLogResult](t, callCapability(t, ctx, link, &trace, "developer-b", "git.log", "read developer-b git log", map[string]any{
		"workspace_id": preparedB.Workspace.ID,
		"max_count":    3,
	}))
	if len(logA.Entries) == 0 || len(logB.Entries) == 0 || logA.Entries[0].SHA != tinyLedger.BaseRef || logB.Entries[0].SHA != tinyLedger.BaseRef {
		t.Fatalf("developer git logs = %+v/%+v, want fixture base ref", logA, logB)
	}

	crossStatus := callCapability(t, ctx, link, &trace, "developer-a", "workspace.status", "developer-a attempts developer-b workspace", map[string]string{
		"workspace_id": preparedB.Workspace.ID,
	})
	assertWorkspaceIsolationReject(t, crossStatus, preparedB.Workspace.Path)
	crossLog := callCapability(t, ctx, link, &trace, "developer-b", "git.log", "developer-b attempts developer-a workspace", map[string]string{
		"workspace_id": preparedA.Workspace.ID,
	})
	assertWorkspaceIsolationReject(t, crossLog, preparedA.Workspace.Path)

	if err := trace.Validate(); err != nil {
		t.Fatalf("manual trace schema invalid: %v", err)
	}
	if got := countRows(t, ctx, app.DB, "work_contracts"); got != 3 {
		t.Fatalf("work_contracts = %d, want root plus two developers", got)
	}
	if got := countRows(t, ctx, app.DB, "agent_communication_envelopes"); got != 3 {
		t.Fatalf("agent_communication_envelopes = %d, want root plus two developer task envelopes", got)
	}
	if got, total := countRowsWhere(t, ctx, app.DB, "mailbox_items", "COALESCE(envelope_id, '') <> ''"), countRows(t, ctx, app.DB, "mailbox_items"); got != total {
		t.Fatalf("mailbox envelope traces = %d/%d, want every mailbox to trace an envelope", got, total)
	}
	if got := countRows(t, ctx, app.DB, "git_repositories"); got != 1 {
		t.Fatalf("git_repositories = %d, want one registered Tiny Ledger repo", got)
	}
	if got := countRows(t, ctx, app.DB, "git_workspaces"); got != 2 {
		t.Fatalf("git_workspaces = %d, want two developer workspaces", got)
	}
	if got := countRows(t, ctx, app.DB, "git_operations"); got != 2 {
		t.Fatalf("git_operations = %d, want two workspace.prepare operations only", got)
	}
	if got := countRowsWhere(t, ctx, app.DB, "git_operations", "operation_type = 'workspace.prepare' AND state = 'succeeded'"); got != 2 {
		t.Fatalf("succeeded workspace.prepare operations = %d, want 2", got)
	}
	if got := countRowsWhere(t, ctx, app.DB, "contract_team_scopes", "team_id = 'cp-probe-001-manual-service'"); got != 3 {
		t.Fatalf("contract_team_scopes = %d, want root plus two child scopes", got)
	}

	inspect, err := app.Inspect(ctx)
	if err != nil {
		t.Fatalf("inspect backend: %v", err)
	}
	redacted := cpprobe.RedactedInspect{
		Scenario:       cpprobe.ScenarioID,
		Status:         inspect.Status,
		TeamID:         inspect.TeamID,
		RootContractID: root.ContractID,
		Counts: map[string]int64{
			"work_contracts":                countRows(t, ctx, app.DB, "work_contracts"),
			"agent_communication_envelopes": countRows(t, ctx, app.DB, "agent_communication_envelopes"),
			"mailbox_items":                 countRows(t, ctx, app.DB, "mailbox_items"),
			"git_repositories":              countRows(t, ctx, app.DB, "git_repositories"),
			"git_workspaces":                countRows(t, ctx, app.DB, "git_workspaces"),
			"git_operations":                countRows(t, ctx, app.DB, "git_operations"),
		},
		Workspaces: []cpprobe.WorkspaceSummary{
			workspaceSummary(preparedA.Workspace),
			workspaceSummary(preparedB.Workspace),
		},
		Redacted: true,
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
	gitSummary := cpprobe.GitOperationSummary{
		Scenario: cpprobe.ScenarioID,
		Repositories: []cpprobe.RepositorySummary{
			{ID: preparedA.Repository.ID, CanonicalBranch: preparedA.Repository.CanonicalBranch, Status: preparedA.Repository.Status},
		},
		Workspaces: []cpprobe.WorkspaceSummary{
			workspaceSummary(preparedA.Workspace),
			workspaceSummary(preparedB.Workspace),
		},
		Operations: []cpprobe.GitOperationBrief{
			{ID: preparedA.OperationID, OperationType: "workspace.prepare", SubjectKind: "operator_debug", ActorAgentID: "developer-a", WorkspaceID: preparedA.Workspace.ID, RepoID: preparedA.Repository.ID, ExecutionLocation: "backend_control_plane", BeforeRef: "", AfterRef: preparedA.Workspace.HeadRef, State: "succeeded"},
			{ID: preparedB.OperationID, OperationType: "workspace.prepare", SubjectKind: "operator_debug", ActorAgentID: "developer-b", WorkspaceID: preparedB.Workspace.ID, RepoID: preparedB.Repository.ID, ExecutionLocation: "backend_control_plane", BeforeRef: "", AfterRef: preparedB.Workspace.HeadRef, State: "succeeded"},
		},
		NoActiveLocks: countRowsWhere(t, ctx, app.DB, "git_locks", "state = 'active'") == 0,
	}
	if err := gitSummary.Validate(); err != nil {
		t.Fatalf("git summary schema invalid: %v", err)
	}
	failureMatrix := cpprobe.FailureMatrix{
		Scenario: cpprobe.ScenarioID,
		Items: []cpprobe.FailureMatrixItem{
			{
				ID:                 "workspace-isolation",
				Status:             "covered",
				Capability:         "workspace.status/git.log",
				ExpectedErrorCodes: []string{"WORKSPACE_NOT_FOUND"},
				ActualErrorCode:    crossStatus.ErrorCode,
				StateAssertion:     "cross-agent workspace access is rejected without exposing the other workspace path",
			},
			{
				ID:             "stale-target-conflict-retry",
				Status:         "pending",
				Capability:     "git.merge_preview/git.merge_apply/git.resolve",
				StateAssertion: "not covered by the minimal skeleton",
				NextStep:       "extend from two workspace prepares into concurrent changeset merge/retry",
			},
		},
	}
	if err := failureMatrix.Validate(); err != nil {
		t.Fatalf("failure matrix schema invalid: %v", err)
	}
	conclusion := cpprobe.ConclusionReport{
		Scenario:         cpprobe.ScenarioID,
		Status:           cpprobe.ConclusionFailed,
		ManualTraceRef:   cpprobe.ManualTraceArtifact,
		InspectRef:       cpprobe.InspectRedactedArtifact,
		GitSummaryRef:    cpprobe.GitOperationSummaryArtifact,
		FailureMatrixRef: cpprobe.FailureMatrixArtifact,
		Covered: []string{
			"backend startup",
			"TeamConfig load",
			"root contract creation",
			"coordinator dispatch",
			"developer workspace prepare/status/git.log",
			"workspace isolation rejection",
		},
		NotCovered: []string{
			"Docker/Claude runtime",
			"concurrent merge/stale/conflict repair",
			"rollback/recovery negative matrix",
		},
		NextSteps: []string{
			"add Developer A/B changeset merge and stale/conflict repair path",
			"promote the same protocol into the Docker agent release-health entrypoint",
		},
	}
	if err := conclusion.Validate(); err != nil {
		t.Fatalf("conclusion schema invalid: %v", err)
	}
}

func callCapability(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, capabilityName, inputSummary string, input any) cpcapability.Response[json.RawMessage] {
	t.Helper()
	return callCapabilityWithSubjectKind(t, ctx, link, trace, actor, "operator_debug", "", capabilityName, inputSummary, input, nil)
}

func callCapabilityWithRuntime(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, runtimeID, capabilityName, inputSummary string, input, scope any) cpcapability.Response[json.RawMessage] {
	t.Helper()
	return callCapabilityWithSubjectKind(t, ctx, link, trace, actor, "agent", runtimeID, capabilityName, inputSummary, input, scope)
}

func callCapabilityWithSubjectKind(t *testing.T, ctx context.Context, link *coordlink.Adapter, trace *cpprobe.ManualTrace, actor, subjectKind, runtimeID, capabilityName, inputSummary string, input, scope any) cpcapability.Response[json.RawMessage] {
	t.Helper()
	raw := json.RawMessage(`{}`)
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal %s input: %v", capabilityName, err)
		}
		raw = encoded
	}
	scopeRaw := json.RawMessage(`{}`)
	if scope != nil {
		encoded, err := json.Marshal(scope)
		if err != nil {
			t.Fatalf("marshal %s scope: %v", capabilityName, err)
		}
		scopeRaw = encoded
	}
	response := link.Call(ctx, cpcapability.Call{
		CapabilityName: capabilityName,
		TraceID:        "cp-probe-001-manual",
		Subject: cpcapability.Subject{
			Kind:      subjectKind,
			ID:        actor,
			AgentID:   actor,
			RuntimeID: runtimeID,
		},
		Input: raw,
		Scope: scopeRaw,
	})
	trace.Steps = append(trace.Steps, cpprobe.TraceStep{
		Actor:        actor,
		EntryPoint:   "coordlink.adapter",
		Capability:   capabilityName,
		InputSummary: inputSummary,
		Status:       string(response.Status),
		ErrorCode:    response.ErrorCode,
		CanonicalIDs: cloneStringMap(response.CanonicalIDs),
		NextActions:  append([]string(nil), response.AllowedNextActions...),
	})
	return response
}

func acceptedCapability[T any](t *testing.T, response cpcapability.Response[json.RawMessage]) T {
	t.Helper()
	if response.Status != cpcapability.StatusAccepted || !response.OK || response.Data == nil {
		t.Fatalf("capability response = %+v, want accepted", response)
	}
	var out T
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		t.Fatalf("decode capability data: %v; raw=%s", err, string(*response.Data))
	}
	return out
}

func assertWorkspaceIsolationReject(t *testing.T, response cpcapability.Response[json.RawMessage], forbiddenPath string) {
	t.Helper()
	if response.Status != cpcapability.StatusRejected || response.ErrorCode != "WORKSPACE_NOT_FOUND" {
		t.Fatalf("cross-workspace response = %+v, want WORKSPACE_NOT_FOUND rejected", response)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal rejected response: %v", err)
	}
	if strings.Contains(string(raw), forbiddenPath) {
		t.Fatalf("cross-workspace rejected response leaked forbidden workspace path %q: %s", forbiddenPath, raw)
	}
}

func setLastTraceIDs(trace *cpprobe.ManualTrace, ids map[string]string) {
	if len(trace.Steps) == 0 {
		return
	}
	trace.Steps[len(trace.Steps)-1].CanonicalIDs = cloneStringMap(ids)
}

func workspaceSummary(workspace codemanagement.Workspace) cpprobe.WorkspaceSummary {
	return cpprobe.WorkspaceSummary{
		ID:         workspace.ID,
		RepoID:     workspace.RepoID,
		AgentID:    workspace.AgentID,
		ContractID: workspace.ContractID,
		BaseRef:    workspace.BaseRef,
		HeadRef:    workspace.HeadRef,
		State:      workspace.State,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}
