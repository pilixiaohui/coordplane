package validation_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/adapters/httpapi"
	"coordplane/internal/capability"
	"coordplane/internal/commandrun"
	"coordplane/internal/coordination"
	"coordplane/internal/objects"
	"coordplane/internal/policy"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/sessionauth"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
	"coordplane/internal/validation"

	_ "modernc.org/sqlite"
)

const testTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func TestValidationAssessmentThroughHTTPRecordsLedgerEvidenceAndDoesNotCompleteContracts(t *testing.T) {
	ctx := context.Background()
	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{{
		ProcessRef: "exec-validation",
		ExitCode:   0,
		Stdout:     []byte("SECRET_STDOUT_BODY\n"),
	}}}
	h := newHarness(t, exec, true)
	flow := h.startSiblingSessions(t, ctx)
	commandResult := callCommandRun(t, h.dispatcher, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("developer", flow.Developer.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": flow.Developer.LeaseID}),
		IdempotencyKey: "validation-positive-command",
		Input:          raw(t, map[string]any{"argv": []string{"sh", "-lc", "printf SECRET_STDOUT_BODY"}}),
	})
	report, err := h.coordination.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: flow.Developer.LeaseID,
		AgentID: "developer",
		Summary: "developer report",
		Content: "developer report body",
	})
	if err != nil {
		t.Fatalf("submit report: %v", err)
	}
	artifact, err := h.objects.PutArtifact(ctx, objects.PutArtifactInput{
		OwnerAgent:  "developer",
		Content:     []byte("artifact body must not appear in validation inspect"),
		ContentType: "text/plain",
		Metadata:    map[string]string{"contract_id": flow.DeveloperContractID, "name": "validation-artifact"},
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	changesetID := insertChangeSet(t, ctx, h.db, flow.DeveloperContractID)
	server := httptest.NewServer(httpapi.NewWithAuthenticator(h.dispatcher, sessionauth.New(h.db, "command.run", "validation.assessment")))
	defer server.Close()

	response := postCallHTTP(t, server.URL, runtimeTokenForAgent(t, h, "verifier"), capability.Call{
		CapabilityName: "validation.assessment",
		Subject:        agentSubject("verifier", flow.Verifier.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": flow.Verifier.LeaseID}),
		IdempotencyKey: "validation-positive",
		Input: raw(t, map[string]any{
			"assessed_contract_id": flow.DeveloperContractID,
			"verdict":              "pass",
			"reason":               "bounded command and submitted evidence were reviewed",
			"summary":              "developer output passed validation",
			"checked_refs": []map[string]string{
				{"kind": "command_run", "id": commandResult.CommandRunID},
				{"kind": "changeset", "id": changesetID},
				{"kind": "evidence", "id": report.ID},
				{"kind": "object", "ref": commandResult.StdoutRef},
				{"kind": "artifact", "id": artifact.ID},
			},
		}),
	}, nil)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("validation response = %+v, want accepted", response)
	}
	var result validation.Result
	if err := json.Unmarshal(*response.Data, &result); err != nil {
		t.Fatalf("decode validation result: %v", err)
	}
	if result.Verdict != "pass" || result.AssessmentID == "" || result.EvidenceID == "" ||
		result.VerifierAgentID != "verifier" || result.AssessedContractID != flow.DeveloperContractID ||
		result.CheckedRefCount != 5 {
		t.Fatalf("validation result = %+v, want canonical pass assessment", result)
	}
	if got := countRowsWhere(t, ctx, h.db, "validation_assessments", "verdict = 'pass'"); got != 1 {
		t.Fatalf("pass validation_assessments = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'validation_assessment' AND verdict = 'pass'"); got != 1 {
		t.Fatalf("validation evidence rows = %d, want 1", got)
	}
	for _, eventType := range []string{
		"validation.assessment_requested",
		"validation.assessment_submitted",
		"validation.assessment_passed",
		"evidence.validation_assessment_recorded",
	} {
		if got := countRowsWhere(t, ctx, h.db, "events", "event_type = '"+eventType+"'"); got != 1 {
			t.Fatalf("%s events = %d, want 1", eventType, got)
		}
	}
	assessments, err := validation.ListAssessments(ctx, h.db)
	if err != nil {
		t.Fatalf("list assessments: %v", err)
	}
	if len(assessments) != 1 || len(assessments[0].RefSnapshot) != 5 {
		t.Fatalf("assessments = %+v, want one assessment with five snapshots", assessments)
	}
	assertSnapshotKinds(t, assessments[0].RefSnapshot, []string{"artifact", "changeset", "command_run", "evidence", "object"})
	inspectJSON := mustJSON(t, assessments)
	for _, forbidden := range []string{
		"SECRET_STDOUT_BODY",
		"artifact body must not appear",
		runtimeTokenForAgent(t, h, "verifier"),
		"/tmp/backend.db",
		"/var/run/docker.sock",
	} {
		if strings.Contains(inspectJSON, forbidden) {
			t.Fatalf("validation inspect leaked forbidden value %q: %s", forbidden, inspectJSON)
		}
	}
	if got := contractStatus(t, ctx, h.db, flow.DeveloperContractID); got != "open" {
		t.Fatalf("developer contract status = %s, want open", got)
	}
	if got := contractStatus(t, ctx, h.db, flow.VerifierContractID); got != "open" {
		t.Fatalf("verifier contract status = %s, want open", got)
	}
	if tableExists(t, ctx, h.db, "release_acceptance") {
		t.Fatal("release_acceptance table exists/written; validation.assessment must not advance release acceptance")
	}
}

func TestValidationAssessmentPublicBoundaryRejectsForgedOrMissingRuntimeToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &recordingExecutor{}, true)
	flow := h.startSiblingSessions(t, ctx)
	server := httptest.NewServer(httpapi.NewWithAuthenticator(h.dispatcher, sessionauth.New(h.db, "validation.assessment")))
	defer server.Close()

	validCall := capability.Call{
		CapabilityName: "validation.assessment",
		Subject:        agentSubject("verifier", flow.Verifier.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": flow.Verifier.LeaseID}),
		Input: raw(t, map[string]any{
			"assessed_contract_id": flow.DeveloperContractID,
			"verdict":              "fail",
			"reason":               "not run by auth tests",
			"summary":              "not run by auth tests",
			"checked_refs":         []map[string]string{{"kind": "evidence", "id": "ev_missing"}},
		}),
	}
	cases := []struct {
		name    string
		token   string
		call    capability.Call
		headers map[string]string
		code    string
	}{
		{name: "missing token", call: validCall, code: "AUTH_TOKEN_REQUIRED"},
		{name: "wrong token", token: "tok_wrong", call: validCall, code: "AUTH_TOKEN_REJECTED"},
		{
			name:  "other agent token with leaked verifier lease",
			token: runtimeTokenForAgent(t, h, "developer"),
			call: capability.Call{
				CapabilityName: "validation.assessment",
				Subject:        agentSubject("developer", flow.Developer.Route.RuntimeID),
				Scope:          validCall.Scope,
				Input:          validCall.Input,
			},
			code: "AUTH_SCOPE_MISMATCH",
		},
		{
			name:  "body subject forged",
			token: runtimeTokenForAgent(t, h, "verifier"),
			call: capability.Call{
				CapabilityName: "validation.assessment",
				Subject:        agentSubject("developer", flow.Verifier.Route.RuntimeID),
				Scope:          validCall.Scope,
				Input:          validCall.Input,
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
		{
			name:  "runtime id forged in body",
			token: runtimeTokenForAgent(t, h, "verifier"),
			call: capability.Call{
				CapabilityName: "validation.assessment",
				Subject:        agentSubject("verifier", "rt_docker_forged"),
				Scope:          validCall.Scope,
				Input:          validCall.Input,
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
		{
			name:  "runtime id forged in header",
			token: runtimeTokenForAgent(t, h, "verifier"),
			call:  validCall,
			headers: map[string]string{
				"X-CoordPlane-Runtime-ID": "rt_docker_header_forged",
			},
			code: "AUTH_SUBJECT_MISMATCH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postCallHTTP(t, server.URL, tc.token, tc.call, tc.headers)
			if response.Status != capability.StatusRejected || response.ErrorCode != tc.code {
				t.Fatalf("response = %+v, want rejected %s", response, tc.code)
			}
			if response.Data != nil {
				t.Fatalf("rejected auth response leaked data: %+v", response)
			}
		})
	}
	assertNoValidationSideEffects(t, ctx, h.db)
}

func TestValidationAssessmentRejectsUnauthorizedCapabilityAndOutOfScopeRefs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &recordingExecutor{}, false)
	flow := h.startSiblingSessions(t, ctx)
	unauthorized := coordlink.New(h.dispatcher).Call(ctx, validationCall("verifier", flow.Verifier.Route.RuntimeID, flow.Verifier.LeaseID, flow.DeveloperContractID, []map[string]string{{"kind": "evidence", "id": "ev_missing"}}))
	if unauthorized.Status != capability.StatusRejected || unauthorized.ErrorCode != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("unauthorized validation response = %+v, want TeamConfig rejection", unauthorized)
	}
	assertNoValidationSideEffects(t, ctx, h.db)

	exec := &recordingExecutor{results: []cpruntime.ContainerExecResult{{ProcessRef: "other", ExitCode: 0}}}
	h = newHarness(t, exec, true)
	flow = h.startSiblingSessions(t, ctx)
	other := h.startDirectSessionFor(t, ctx, "other")
	otherCommand := callCommandRun(t, h.dispatcher, capability.Call{
		CapabilityName: "command.run",
		Subject:        agentSubject("other", other.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": other.LeaseID}),
		Input:          raw(t, map[string]any{"argv": []string{"true"}}),
	})
	outOfScopeCommand := coordlink.New(h.dispatcher).Call(ctx, validationCall("verifier", flow.Verifier.Route.RuntimeID, flow.Verifier.LeaseID, flow.DeveloperContractID, []map[string]string{{"kind": "command_run", "id": otherCommand.CommandRunID}}))
	if outOfScopeCommand.Status != capability.StatusRejected || outOfScopeCommand.ErrorCode != "VALIDATION_REF_SCOPE_REJECTED" {
		t.Fatalf("out-of-scope command ref response = %+v, want scope rejection", outOfScopeCommand)
	}
	unlinked, err := h.objects.Put(ctx, objects.PutInput{OwnerAgent: "developer", Content: []byte("unlinked secret"), ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("put unlinked object: %v", err)
	}
	outOfScopeObject := coordlink.New(h.dispatcher).Call(ctx, validationCall("verifier", flow.Verifier.Route.RuntimeID, flow.Verifier.LeaseID, flow.DeveloperContractID, []map[string]string{{"kind": "object", "ref": unlinked.Ref}}))
	if outOfScopeObject.Status != capability.StatusRejected || outOfScopeObject.ErrorCode != "VALIDATION_REF_SCOPE_REJECTED" {
		t.Fatalf("out-of-scope object ref response = %+v, want scope rejection", outOfScopeObject)
	}
	assertNoValidationSideEffects(t, ctx, h.db)
}

func TestValidationAssessmentIdempotencyAndBlockedVerdict(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &recordingExecutor{}, true)
	flow := h.startSiblingSessions(t, ctx)
	report, err := h.coordination.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: flow.Developer.LeaseID,
		AgentID: "developer",
		Summary: "developer evidence",
	})
	if err != nil {
		t.Fatalf("submit report: %v", err)
	}
	call := validationCall("verifier", flow.Verifier.Route.RuntimeID, flow.Verifier.LeaseID, flow.DeveloperContractID, []map[string]string{{"kind": "evidence", "id": report.ID}})
	call.IdempotencyKey = "same-validation"
	first := callValidation(t, h.dispatcher, call)
	second := callValidation(t, h.dispatcher, call)
	if first.AssessmentID != second.AssessmentID || first.EvidenceID != second.EvidenceID {
		t.Fatalf("idempotent validation differs: first=%+v second=%+v", first, second)
	}
	if got := countRowsWhere(t, ctx, h.db, "validation_assessments", "idempotency_key = 'same-validation'"); got != 1 {
		t.Fatalf("idempotent validation rows = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'validation_assessment'"); got != 1 {
		t.Fatalf("validation evidence rows = %d, want 1", got)
	}

	blockedCall := validationCall("verifier", flow.Verifier.Route.RuntimeID, flow.Verifier.LeaseID, flow.DeveloperContractID, []map[string]string{{"kind": "evidence", "id": report.ID}})
	blockedCall.IdempotencyKey = "blocked-validation"
	blockedCall.Input = raw(t, map[string]any{
		"assessed_contract_id": flow.DeveloperContractID,
		"verdict":              "blocked",
		"reason":               "missing environment required for final verification",
		"summary":              "validation blocked",
		"blockers":             []string{"environment unavailable"},
		"checked_refs":         []map[string]string{{"kind": "evidence", "id": report.ID}},
	})
	blocked := callValidation(t, h.dispatcher, blockedCall)
	if blocked.Verdict != "blocked" {
		t.Fatalf("blocked validation = %+v, want blocked verdict", blocked)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "event_type = 'validation.assessment_blocked'"); got != 1 {
		t.Fatalf("blocked events = %d, want 1", got)
	}
}

func TestReportSubmitCannotImpersonateValidationAssessment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &recordingExecutor{}, true)
	flow := h.startSiblingSessions(t, ctx)
	response := coordlink.New(h.dispatcher).Call(ctx, capability.Call{
		CapabilityName: "report.submit",
		Subject:        agentSubject("verifier", flow.Verifier.Route.RuntimeID),
		Scope:          raw(t, map[string]any{"lease_id": flow.Verifier.LeaseID}),
		Input: raw(t, map[string]any{
			"summary": "verdict: pass",
			"content": `{"kind":"validation_assessment","verdict":"pass","checked_refs":[{"kind":"evidence","id":"fake"}]}`,
		}),
	})
	if response.Status != capability.StatusAccepted {
		t.Fatalf("report.submit response = %+v, want accepted report", response)
	}
	if got := countRowsWhere(t, ctx, h.db, "validation_assessments", "1 = 1"); got != 0 {
		t.Fatalf("validation rows after report impersonation = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'validation_assessment'"); got != 0 {
		t.Fatalf("validation evidence after report impersonation = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "evidence", "kind = 'report' AND verdict IS NULL"); got != 1 {
		t.Fatalf("report evidence without verdict = %d, want 1", got)
	}
	if got := contractStatus(t, ctx, h.db, flow.VerifierContractID); got != "open" {
		t.Fatalf("verifier contract status after report = %s, want open", got)
	}
}

func TestValidationAssessmentRealVerifierCoordlinkGateRequiresExplicitEnvironment(t *testing.T) {
	if os.Getenv("COORDPLANE_VALIDATION_GATE") != "1" {
		t.Skip("set COORDPLANE_VALIDATION_GATE=1 with COORDPLANE_COORDLINK_PATH and Docker available to run the real coordlink validation.assessment gate")
	}
	coordlinkPath := os.Getenv("COORDPLANE_COORDLINK_PATH")
	if coordlinkPath == "" {
		t.Fatal("COORDPLANE_COORDLINK_PATH is required for the real coordlink validation gate")
	}
	if _, err := osexec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI unavailable: %v", err)
	}
	image := os.Getenv("COORDPLANE_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	network := os.Getenv("COORDPLANE_DOCKER_NETWORK")
	dispatcher := &deferredDispatcher{}
	authenticator := sessionauth.New(nil, "validation.assessment")
	server := httptest.NewServer(httpapi.NewWithAuthenticator(dispatcher, authenticator))
	defer server.Close()

	h := newHarnessWithDocker(t, cpruntime.DockerExecClient{}, true, nil, image, network, coordlinkPath, server.URL)
	dispatcher.inner = h.dispatcher
	authenticator.SetDB(h.db)
	t.Cleanup(func() {
		cleanupValidationContainers(t, h.db)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	flow := h.startSiblingSessions(t, ctx)
	report, err := h.coordination.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: flow.Developer.LeaseID,
		AgentID: "developer",
		Summary: "developer live evidence",
	})
	if err != nil {
		t.Fatalf("submit report: %v", err)
	}
	instance := runtimeInstanceForRoute(t, ctx, h.db, flow.Verifier.Route.RuntimeID)
	input := mustJSON(t, map[string]any{
		"assessed_contract_id": flow.DeveloperContractID,
		"verdict":              "pass",
		"reason":               "live verifier coordlink submitted assessment",
		"summary":              "live verifier accepted report evidence",
		"checked_refs":         []map[string]string{{"kind": "evidence", "id": report.ID}},
	})
	outer := cpruntime.DockerExecClient{}
	result, err := outer.Exec(ctx, cpruntime.ContainerExecSpec{
		ContainerName: instance.ContainerName,
		Workdir:       cpruntime.ContainerWorkspacePath,
		HomeDir:       cpruntime.ContainerHomePath,
		Command: []string{
			cpruntime.ContainerCoordlinkPath,
			"call", "validation.assessment",
			"--idempotency-key", "real-verifier-validation",
			"--input", input,
		},
		Timeout: 30 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("container coordlink validation.assessment failed: exit=%d err=%v stdout=%s stderr=%s", result.ExitCode, err, result.Stdout, result.Stderr)
	}
	response := decodeRawResponse(t, result.Stdout)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("container coordlink validation response = %+v stdout=%s stderr=%s", response, result.Stdout, result.Stderr)
	}
	if got := countRowsWhere(t, ctx, h.db, "validation_assessments", "idempotency_key = 'real-verifier-validation' AND verdict = 'pass'"); got != 1 {
		t.Fatalf("validation rows = %d, want one real coordlink assessment", got)
	}
}

type harness struct {
	db           *sql.DB
	store        *store.Store
	coordination *coordination.Service
	dispatcher   *policy.Dispatcher
	objects      *objects.Store
	runner       *cpruntime.Runner
	fake         *cpruntime.FakeCLIAdapter
}

type siblingFlow struct {
	Root                cpruntime.AssignmentSession
	Developer           cpruntime.AssignmentSession
	Verifier            cpruntime.AssignmentSession
	RootContractID      string
	DeveloperContractID string
	VerifierContractID  string
}

func newHarness(t *testing.T, exec cpruntime.ContainerExecutor, grantValidation bool) harness {
	coordlinkPath := filepath.Join(t.TempDir(), "coordlink")
	if err := os.WriteFile(coordlinkPath, []byte("fake coordlink"), 0o755); err != nil {
		t.Fatalf("write coordlink fixture: %v", err)
	}
	return newHarnessWithDocker(t, exec, grantValidation, &recordingDockerClient{}, "alpine:3.20", "", coordlinkPath, "http://coordplane.test")
}

func newHarnessWithDocker(t *testing.T, exec cpruntime.ContainerExecutor, grantValidation bool, docker cpruntime.DockerClient, image, network, coordlinkPath, backendURL string) harness {
	t.Helper()
	ctx := context.Background()
	if image == "" {
		image = "alpine:3.20"
	}
	if backendURL == "" {
		backendURL = "http://coordplane.test"
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	coordSvc := coordination.NewService(st)
	commandSvc, err := commandrun.NewService(commandrun.Config{Store: st, Executor: exec})
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	validationSvc, err := validation.NewService(validation.Config{Store: st})
	if err != nil {
		t.Fatalf("new validation service: %v", err)
	}
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, coordSvc); err != nil {
		t.Fatalf("register coordination: %v", err)
	}
	if err := commandrun.RegisterCapabilities(registry, commandSvc); err != nil {
		t.Fatalf("register commandrun: %v", err)
	}
	if err := validation.RegisterCapabilities(registry, validationSvc); err != nil {
		t.Fatalf("register validation: %v", err)
	}
	cfg := teamConfig(grantValidation)
	dispatcher := policy.NewDispatcher(cfg, registry)
	dockerRuntime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
		DB:            db,
		ProfileName:   "docker-default",
		TeamID:        "validation-test",
		Image:         image,
		Network:       network,
		RuntimeRoot:   filepath.Join(t.TempDir(), "docker-runtime"),
		CoordlinkPath: coordlinkPath,
		DBPath:        filepath.Join(t.TempDir(), "coordplane.db"),
		Ready:         true,
		Docker:        docker,
	})
	fake := cpruntime.NewFakeCLIAdapter()
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      cfg,
		Skills:          skillRegistry,
		RuntimeBackends: map[string]cpruntime.RuntimeBackend{"docker-default": dockerRuntime},
		Adapter:         fake,
		BackendURL:      backendURL,
		WorkspaceName:   "validation-test",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return harness{
		db:           db,
		store:        st,
		coordination: coordSvc,
		dispatcher:   dispatcher,
		objects:      objects.NewStore(st),
		runner:       runner,
		fake:         fake,
	}
}

func (h *harness) startSiblingSessions(t *testing.T, ctx context.Context) siblingFlow {
	t.Helper()
	root, err := h.coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "root coordination",
		Objective:     "coordinate developer and verifier",
		TargetAgentID: "coordinator",
	})
	if err != nil {
		t.Fatalf("add root contract: %v", err)
	}
	rootSession, err := h.runner.StartNext(ctx, "coordinator")
	if err != nil {
		t.Fatalf("start coordinator session: %v", err)
	}
	developer, err := h.coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerLeaseID: rootSession.LeaseID,
		IssuerAgentID: "coordinator",
		Title:         "developer work",
		Objective:     "produce implementation evidence",
		TargetAgentID: "developer",
	})
	if err != nil {
		t.Fatalf("add developer contract: %v", err)
	}
	verifier, err := h.coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerLeaseID: rootSession.LeaseID,
		IssuerAgentID: "coordinator",
		Title:         "verifier work",
		Objective:     "validate developer evidence",
		TargetAgentID: "verifier",
	})
	if err != nil {
		t.Fatalf("add verifier contract: %v", err)
	}
	developerSession, err := h.runner.StartNext(ctx, "developer")
	if err != nil {
		t.Fatalf("start developer session: %v", err)
	}
	verifierSession, err := h.runner.StartNext(ctx, "verifier")
	if err != nil {
		t.Fatalf("start verifier session: %v", err)
	}
	return siblingFlow{
		Root:                rootSession,
		Developer:           developerSession,
		Verifier:            verifierSession,
		RootContractID:      root.ContractID,
		DeveloperContractID: developer.ContractID,
		VerifierContractID:  verifier.ContractID,
	}
}

func (h *harness) startDirectSessionFor(t *testing.T, ctx context.Context, agentID string) cpruntime.AssignmentSession {
	t.Helper()
	if _, err := h.coordination.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         agentID + " direct work",
		Objective:     "produce unrelated evidence",
		TargetAgentID: agentID,
	}); err != nil {
		t.Fatalf("add direct contract for %s: %v", agentID, err)
	}
	session, err := h.runner.StartNext(ctx, agentID)
	if err != nil {
		t.Fatalf("start %s session: %v", agentID, err)
	}
	return session
}

func teamConfig(grantValidation bool) teamconfig.Config {
	verifierCaps := []string{"assignment.next", "assignment.watch", "contract.current", "contract.context", "report.submit"}
	if grantValidation {
		verifierCaps = append(verifierCaps, "validation.assessment")
	}
	return teamconfig.Config{
		TeamID:  "validation-test",
		Version: 1,
		RuntimeProfiles: map[string]teamconfig.RuntimeProfile{
			"docker-default": {Kind: "docker", Image: "alpine:3.20", WorkspaceMode: "isolated"},
		},
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "coordinator",
				RolePrompt:     "coordinator role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service", "contract-delegation"},
				Capabilities:   []string{"assignment.next", "contract.current", "contract.add", "contract.context"},
			},
			{
				ID:             "developer",
				RolePrompt:     "developer role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   []string{"assignment.next", "assignment.watch", "contract.current", "contract.context", "command.run", "report.submit"},
			},
			{
				ID:             "verifier",
				RolePrompt:     "verifier role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   verifierCaps,
			},
			{
				ID:             "other",
				RolePrompt:     "other role",
				RuntimeProfile: "docker-default",
				CLIBackend:     "fake",
				Skills:         []string{"coordplane-service"},
				Capabilities:   []string{"assignment.next", "command.run", "contract.current"},
			},
		},
	}
}

type recordingExecutor struct {
	err     error
	results []cpruntime.ContainerExecResult
	specs   []cpruntime.ContainerExecSpec
}

func (e *recordingExecutor) Exec(ctx context.Context, spec cpruntime.ContainerExecSpec) (cpruntime.ContainerExecResult, error) {
	e.specs = append(e.specs, spec)
	if e.err != nil && len(e.results) == 0 {
		return cpruntime.ContainerExecResult{}, e.err
	}
	if len(e.results) == 0 {
		return cpruntime.ContainerExecResult{ProcessRef: "default", ExitCode: 0}, nil
	}
	result := e.results[0]
	e.results = e.results[1:]
	return result, nil
}

type recordingDockerClient struct{}

func (c *recordingDockerClient) PrepareContainer(ctx context.Context, spec cpruntime.DockerContainerSpec) (cpruntime.DockerContainerResult, error) {
	return cpruntime.DockerContainerResult{
		ContainerID: "container-" + spec.ContainerName,
		Checks: map[string]bool{
			"backend_reachable":      true,
			"workspace_writable":     true,
			"home_writable":          true,
			"git_workspace_writable": true,
			"cli_user_consistent":    true,
		},
	}, nil
}

type deferredDispatcher struct {
	inner *policy.Dispatcher
}

func (d *deferredDispatcher) Handle(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	if d.inner == nil {
		return capability.Error[json.RawMessage]("TEST_DISPATCHER_NOT_READY", "dispatcher is not ready", true)
	}
	return d.inner.Handle(ctx, call)
}

func (d *deferredDispatcher) ListForSubject(ctx context.Context, subject capability.Subject) capability.Response[json.RawMessage] {
	if d.inner == nil {
		return capability.Error[json.RawMessage]("TEST_DISPATCHER_NOT_READY", "dispatcher is not ready", true)
	}
	return d.inner.ListForSubject(ctx, subject)
}

func validationCall(agentID, runtimeID, leaseID, assessedContractID string, refs []map[string]string) capability.Call {
	return capability.Call{
		CapabilityName: "validation.assessment",
		Subject:        agentSubject(agentID, runtimeID),
		Scope:          mustRaw(map[string]any{"lease_id": leaseID}),
		Input: mustRaw(map[string]any{
			"assessed_contract_id": assessedContractID,
			"verdict":              "fail",
			"reason":               "reviewed durable refs",
			"summary":              "validation failed",
			"checked_refs":         refs,
		}),
	}
}

func callValidation(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) validation.Result {
	t.Helper()
	response := coordlink.New(dispatcher).Call(context.Background(), call)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("validation response = %+v, want accepted", response)
	}
	var out validation.Result
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		t.Fatalf("decode validation result: %v\nraw=%s", err, string(*response.Data))
	}
	return out
}

func callCommandRun(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) commandrun.Result {
	t.Helper()
	response := coordlink.New(dispatcher).Call(context.Background(), call)
	if response.Status != capability.StatusAccepted || response.Data == nil {
		t.Fatalf("command.run response = %+v, want accepted", response)
	}
	var out commandrun.Result
	if err := json.Unmarshal(*response.Data, &out); err != nil {
		t.Fatalf("decode command.run result: %v\nraw=%s", err, string(*response.Data))
	}
	return out
}

func insertChangeSet(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	now := time.Now().UTC().Format(testTimeLayout)
	for _, stmt := range []string{
		`INSERT INTO git_repositories (id, source_path, canonical_branch, status, created_at, updated_at) VALUES ('repo_validation', '/repo/src', 'main', 'active', ?, ?)`,
		`INSERT INTO git_workspaces (id, repo_id, agent_id, contract_id, path, base_ref, head_ref, state, created_at, updated_at) VALUES ('ws_validation', 'repo_validation', 'developer', ?, '/workspace', 'base', 'head', 'ready', ?, ?)`,
		`INSERT INTO changesets (id, workspace_id, repo_id, contract_id, base_ref, head_ref, commit_ids_json, summary, evidence_refs_json, state, created_at, updated_at) VALUES ('chg_validation', 'ws_validation', 'repo_validation', ?, 'base', 'head', '["abc123"]', 'changeset summary', '[]', 'submitted', ?, ?)`,
	} {
		var err error
		if strings.Contains(stmt, "git_repositories") {
			_, err = db.ExecContext(ctx, stmt, now, now)
		} else {
			_, err = db.ExecContext(ctx, stmt, contractID, now, now)
		}
		if err != nil {
			t.Fatalf("insert changeset fixture: %v", err)
		}
	}
	return "chg_validation"
}

func runtimeTokenForAgent(t *testing.T, h harness, agentID string) string {
	t.Helper()
	starts := h.fake.Starts()
	for i := len(starts) - 1; i >= 0; i-- {
		if starts[i].AgentID == agentID {
			token := starts[i].Env["COORDPLANE_TOKEN"]
			if token == "" {
				t.Fatalf("fake CLI start for %s missing COORDPLANE_TOKEN: %+v", agentID, starts[i].Env)
			}
			return token
		}
	}
	t.Fatalf("no fake CLI start recorded for %s", agentID)
	return ""
}

func runtimeInstanceForRoute(t *testing.T, ctx context.Context, db *sql.DB, runtimeID string) cpruntime.RuntimeInstance {
	t.Helper()
	instances, err := cpruntime.ListRuntimeInstances(ctx, db)
	if err != nil {
		t.Fatalf("list runtime instances: %v", err)
	}
	for _, instance := range instances {
		if instance.RuntimeID == runtimeID {
			return instance
		}
	}
	t.Fatalf("runtime instance %s not found in %+v", runtimeID, instances)
	return cpruntime.RuntimeInstance{}
}

func postCallHTTP(t *testing.T, serverURL, token string, call capability.Call, headers map[string]string) capability.Response[json.RawMessage] {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, serverURL+"/call", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post /call: %v", err)
	}
	defer resp.Body.Close()
	var response capability.Response[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode /call response: %v", err)
	}
	return response
}

func decodeRawResponse(t *testing.T, raw []byte) capability.Response[json.RawMessage] {
	t.Helper()
	var response capability.Response[json.RawMessage]
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode response: %v\nraw=%s", err, string(raw))
	}
	return response
}

func assertSnapshotKinds(t *testing.T, snapshots []validation.RefSnapshot, want []string) {
	t.Helper()
	got := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		got[snapshot.Kind] = true
	}
	for _, kind := range want {
		if !got[kind] {
			t.Fatalf("snapshot kinds = %+v, missing %s", snapshots, kind)
		}
	}
}

func assertNoValidationSideEffects(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for table, where := range map[string]string{
		"validation_assessments": "1 = 1",
		"evidence":               "kind = 'validation_assessment'",
		"events":                 "aggregate_type = 'validation_assessment' OR capability_name = 'validation.assessment' OR event_type LIKE 'validation.%' OR event_type = 'evidence.validation_assessment_recorded'",
	} {
		if got := countRowsWhere(t, ctx, db, table, where); got != 0 {
			t.Fatalf("%s side effects = %d, want 0", table, got)
		}
	}
}

func cleanupValidationContainers(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT container_name FROM runtime_instances WHERE container_name <> ''`)
	if err != nil {
		t.Logf("list runtime containers for cleanup: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var containerName string
		if err := rows.Scan(&containerName); err != nil {
			t.Logf("scan runtime container for cleanup: %v", err)
			continue
		}
		_ = osexec.Command("docker", "rm", "-f", containerName).Run()
	}
	if err := rows.Err(); err != nil {
		t.Logf("iterate runtime containers for cleanup: %v", err)
	}
}

func agentSubject(agentID, runtimeID string) capability.Subject {
	return capability.Subject{Kind: "agent", ID: agentID, AgentID: agentID, RuntimeID: runtimeID}
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	return mustRaw(value)
}

func mustRaw(value any) json.RawMessage {
	out, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return out
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func contractStatus(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&status); err != nil {
		t.Fatalf("query contract status: %v", err)
	}
	return status
}

func tableExists(t *testing.T, ctx context.Context, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if errorsIsNoRows(err) {
		return false
	}
	if err != nil {
		t.Fatalf("query table %s: %v", table, err)
	}
	return name == table
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
