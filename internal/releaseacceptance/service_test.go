package releaseacceptance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"coordplane/internal/backend"
	"coordplane/internal/releaseacceptance"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

const testTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func TestEvaluateFailsClosedAndIsIdempotentWhenEvidenceIsMissing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedTeamConfig(t, ctx, h.store)
	insertRootContract(t, ctx, h.db, "open")

	first, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "missing",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("evaluate missing evidence: %v", err)
	}
	if first.Status != "failed" {
		t.Fatalf("status = %s, want failed", first.Status)
	}
	assertPredicates(t, first, map[string]string{
		"root_contract_present":        "passed",
		"command_run_public_evidence":  "failed",
		"validation_assessment_pass":   "failed",
		"root_contract_completed":      "failed",
		"mailbox_resume_steer":         "failed",
		"git_changeset_evidence_chain": "failed",
	})
	if first.FailureSummary == "" || strings.Contains(mustJSON(t, first), "SECRET") {
		t.Fatalf("failed acceptance = %+v, want failure summary and no sensitive body", first)
	}
	second, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "missing",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("second evaluate: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent evaluation IDs = %s/%s, want same row", first.ID, second.ID)
	}
	if got := countRowsWhere(t, ctx, h.db, "release_acceptances", "run_label = 'missing'"); got != 1 {
		t.Fatalf("release_acceptances rows = %d, want 1", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "events", "aggregate_type = 'release_acceptance'"); got == 0 {
		t.Fatalf("release acceptance events = %d, want audit events", got)
	}
}

func TestEvaluatePassesOnlyWithCompleteCanonicalEvidenceMatrix(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedPassingEvidence(t, ctx, h)

	acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "complete",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("evaluate complete evidence: %v", err)
	}
	if acceptance.Status != "passed" {
		t.Fatalf("acceptance = %+v, want passed", acceptance)
	}
	for _, predicate := range acceptance.PredicateResults {
		if predicate.Status != "passed" {
			t.Fatalf("predicate %s = %+v, want passed", predicate.Name, predicate)
		}
	}
	if len(acceptance.EvidenceRefs) == 0 || acceptance.InspectSummary.PassedCount != len(acceptance.PredicateResults) {
		t.Fatalf("acceptance summary = %+v refs=%v, want all predicates counted with refs", acceptance.InspectSummary, acceptance.EvidenceRefs)
	}
	for _, eventType := range []string{
		"release_acceptance.evaluation_requested",
		"release_acceptance.predicate_passed",
		"release_acceptance.passed",
	} {
		if got := countRowsWhere(t, ctx, h.db, "events", "event_type = '"+eventType+"'"); got == 0 {
			t.Fatalf("%s events = %d, want audit event", eventType, got)
		}
	}
	summary, err := releaseacceptance.LatestSummary(ctx, h.db)
	if err != nil {
		t.Fatalf("latest summary: %v", err)
	}
	if summary.Latest == nil || summary.Latest.ID != acceptance.ID || summary.Counts["passed"] != 1 {
		t.Fatalf("latest summary = %+v, want passed latest", summary)
	}
	inspectJSON := mustJSON(t, summary)
	for _, forbidden := range []string{
		"SECRET_OUTPUT_BODY",
		"SECRET_TRANSCRIPT_BODY",
		"tok_",
		"/var/run/docker.sock",
		"/tmp/backend.db",
	} {
		if strings.Contains(inspectJSON, forbidden) {
			t.Fatalf("release inspect summary leaked %q: %s", forbidden, inspectJSON)
		}
	}
}

func TestEvaluateMailboxResumeSteerAcceptsDurableFallbackResume(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedPassingEvidence(t, ctx, h)
	forceMailboxFallbackResumeEvidence(t, ctx, h.db, fallbackOptions{
		AdapterCapabilityKnown: true,
		ResumeQueueDone:        true,
		SessionResumed:         true,
		ResumeCLISession:       true,
	})

	acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "fallback-resume",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("evaluate fallback resume evidence: %v", err)
	}
	if acceptance.Status != "passed" {
		t.Fatalf("acceptance = %+v, want passed with complete fallback resume evidence", acceptance)
	}
	assertPredicates(t, acceptance, map[string]string{"mailbox_resume_steer": "passed"})
}

func TestEvaluateMailboxResumeSteerIgnoresClaimedTaskAssignmentMailboxes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedPassingEvidence(t, ctx, h)
	insertClaimedTaskAssignmentMailbox(t, ctx, h.db, "mbx_task_root", "coordinator", "ctr_root", "lease:lease_root")
	insertClaimedTaskAssignmentMailbox(t, ctx, h.db, "mbx_task_developer", "developer", "ctr_developer", "lease:lease_dev")
	insertClaimedTaskAssignmentMailbox(t, ctx, h.db, "mbx_task_verifier", "verifier", "ctr_verifier", "lease:lease_ver")

	acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "task-assignment-mailboxes",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("evaluate with task assignment mailboxes: %v", err)
	}
	if acceptance.Status != "passed" {
		t.Fatalf("acceptance = %+v, want passed when only claimed task assignment mailboxes lack delivery attempts", acceptance)
	}
	assertPredicates(t, acceptance, map[string]string{"mailbox_resume_steer": "passed"})
}

func TestEvaluateMailboxResumeSteerFailsClosedForIncompleteFallbackResume(t *testing.T) {
	cases := []struct {
		name string
		opts fallbackOptions
	}{
		{
			name: "unknown adapter capability",
			opts: fallbackOptions{ResumeQueueDone: true, SessionResumed: true, ResumeCLISession: true},
		},
		{
			name: "runtime resume queue not done",
			opts: fallbackOptions{AdapterCapabilityKnown: true, SessionResumed: true, ResumeCLISession: true},
		},
		{
			name: "session not resumed for mailbox",
			opts: fallbackOptions{AdapterCapabilityKnown: true, ResumeQueueDone: true, ResumeCLISession: true},
		},
		{
			name: "resume cli session missing",
			opts: fallbackOptions{AdapterCapabilityKnown: true, ResumeQueueDone: true, SessionResumed: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			seedPassingEvidence(t, ctx, h)
			forceMailboxFallbackResumeEvidence(t, ctx, h.db, tc.opts)

			acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
				RootContractID: "ctr_root",
				TeamID:         "release-test",
				TeamVersion:    1,
				RunLabel:       strings.ReplaceAll(tc.name, " ", "-"),
				CreatedBy:      "operator-test",
			})
			if err != nil {
				t.Fatalf("evaluate fallback negative: %v", err)
			}
			if acceptance.Status == "passed" {
				t.Fatalf("acceptance = %+v, want fail closed for incomplete fallback resume", acceptance)
			}
			assertPredicates(t, acceptance, map[string]string{"mailbox_resume_steer": "failed"})
		})
	}
}

func TestEvaluateFailsClosedWhenTeamVersionDoesNotOwnRootEvidence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedPassingEvidence(t, ctx, h)
	seedOtherTeamConfig(t, ctx, h.store)

	acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "other-release-test",
		TeamVersion:    1,
		RunLabel:       "wrong-team",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("evaluate wrong team: %v", err)
	}
	if acceptance.Status == "passed" {
		t.Fatalf("acceptance = %+v, want fail closed for mismatched TeamConfig scope", acceptance)
	}
	assertPredicates(t, acceptance, map[string]string{"team_scope_binding": "failed"})
	if got := countRowsWhere(t, ctx, h.db, "release_acceptances", "run_label = 'wrong-team' AND status = 'passed'"); got != 0 {
		t.Fatalf("wrong-team passed release_acceptances = %d, want 0", got)
	}
	if got := countRowsWhere(t, ctx, h.db, "release_acceptances", "run_label = 'wrong-team' AND status = 'failed'"); got != 1 {
		t.Fatalf("wrong-team failed release_acceptances = %d, want 1 failed matrix", got)
	}
}

func TestEvaluateFailsClosedForNamedNegativeEvidenceGates(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*testing.T, context.Context, *sql.DB)
		predicate string
	}{
		{
			name: "fake CLI",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE cli_sessions SET cli_backend = 'fake' WHERE id = 'cli_dev_start'`)
			},
			predicate: "real_cli_session_lifecycle",
		},
		{
			name: "external runtime",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE runtime_instances SET runtime_kind = 'external' WHERE id = 'rti_developer'`)
			},
			predicate: "docker_runtime_evidence",
		},
		{
			name: "docker runtime writable checks missing",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE runtime_instances SET checks_json = '{}' WHERE id = 'rti_developer'`)
			},
			predicate: "docker_runtime_evidence",
		},
		{
			name: "docker claude auth probe missing",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE runtime_instances SET checks_json = '{"workspace_writable":true,"home_writable":true,"git_workspace_writable":true,"cli_user_consistent":true,"home_private":true,"home_persistent":true,"claude_present":true,"claude_auth_configured":true,"claude_auth_probe_redacted":true}' WHERE id = 'rti_developer'`)
			},
			predicate: "docker_runtime_evidence",
		},
		{
			name: "internal-only command run",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM capability_calls WHERE capability_name = 'command.run'`)
			},
			predicate: "command_run_public_evidence",
		},
		{
			name: "command run borrows other lease public call",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE capability_calls SET scope_json = '{"lease_id":"lease_other"}' WHERE capability_name = 'command.run'`)
			},
			predicate: "command_run_public_evidence",
		},
		{
			name: "missing command run",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM command_runs`)
			},
			predicate: "command_run_public_evidence",
		},
		{
			name: "validation fail verdict",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE validation_assessments SET verdict = 'fail'`)
				execSQL(t, ctx, db, `UPDATE evidence SET verdict = 'fail' WHERE kind = 'validation_assessment'`)
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "validation blocked verdict",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE validation_assessments SET verdict = 'blocked'`)
				execSQL(t, ctx, db, `UPDATE evidence SET verdict = 'blocked' WHERE kind = 'validation_assessment'`)
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "validation borrows other lease public call",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE capability_calls SET scope_json = '{"lease_id":"lease_other"}' WHERE capability_name = 'validation.assessment'`)
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "validation checked refs cross root",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				insertOtherRoot(t, ctx, db)
				insertOtherCheckedRefs(t, ctx, db)
				execSQL(t, ctx, db, `UPDATE validation_assessments SET checked_refs_json = '[{"kind":"command_run","id":"cmd_other"},{"kind":"changeset","id":"chg_other"},{"kind":"evidence","id":"ev_other"}]'`)
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "report impersonates assessment",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM validation_assessments`)
				execSQL(t, ctx, db, `DELETE FROM evidence WHERE kind = 'validation_assessment'`)
				insertEvidence(t, ctx, db, "ev_fake_report", "report", "ctr_verifier", "verifier", "", "verdict: pass checked_refs: command_run", "")
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "root not complete",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE work_contracts SET status = 'open' WHERE id = 'ctr_root'`)
			},
			predicate: "root_contract_completed",
		},
		{
			name: "root complete borrows other lease public call",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `UPDATE capability_calls SET scope_json = '{"lease_id":"lease_other"}' WHERE capability_name = 'contract.complete'`)
			},
			predicate: "root_contract_completed",
		},
		{
			name: "no mailbox resume steer",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM mailbox_items`)
				execSQL(t, ctx, db, `DELETE FROM delivery_attempts`)
				execSQL(t, ctx, db, `DELETE FROM queue_items`)
				execSQL(t, ctx, db, `DELETE FROM events WHERE event_type IN ('session.steer_sent', 'session.resumed', 'cli.adapter_capabilities', 'delivery.resume_queued')`)
			},
			predicate: "mailbox_resume_steer",
		},
		{
			name: "same-turn mailbox missing adapter capability",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM events WHERE event_type = 'cli.adapter_capabilities'`)
			},
			predicate: "mailbox_resume_steer",
		},
		{
			name: "no changeset evidence chain",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM changesets`)
			},
			predicate: "git_changeset_evidence_chain",
		},
		{
			name: "cross root validation evidence",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				insertOtherRoot(t, ctx, db)
				execSQL(t, ctx, db, `UPDATE validation_assessments SET contract_id = 'ctr_other', assessed_contract_id = 'ctr_other'`)
				execSQL(t, ctx, db, `UPDATE evidence SET contract_id = 'ctr_other' WHERE kind = 'validation_assessment'`)
			},
			predicate: "validation_assessment_pass",
		},
		{
			name: "missing team binding",
			mutate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				execSQL(t, ctx, db, `DELETE FROM contract_team_scopes`)
			},
			predicate: "team_scope_binding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			seedPassingEvidence(t, ctx, h)
			tc.mutate(t, ctx, h.db)

			acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
				RootContractID: "ctr_root",
				TeamID:         "release-test",
				TeamVersion:    1,
				RunLabel:       strings.ReplaceAll(tc.name, " ", "-"),
				CreatedBy:      "operator-test",
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if acceptance.Status == "passed" {
				t.Fatalf("acceptance = %+v, want fail closed", acceptance)
			}
			assertPredicates(t, acceptance, map[string]string{tc.predicate: "failed"})
		})
	}
}

func TestReleaseAcceptanceIsInternalOnlyAndInspectShowsRedactedSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	teamConfigPath := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(teamConfigPath, []byte(testHTTPDiscoveryTeamConfigYAML), 0o644); err != nil {
		t.Fatalf("write TeamConfig: %v", err)
	}
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: teamConfigPath,
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer app.Close()

	token := insertRuntimeAuthSession(t, ctx, app.DB, "verifier")
	capabilityEnvelope := getJSONWithBearer(t, app.Handler, "/capabilities", token, http.StatusOK)
	if containsString(capabilityNames(t, capabilityEnvelope), "release_acceptance.evaluate") {
		t.Fatalf("public capability discovery exposed release_acceptance.evaluate: %#v", capabilityEnvelope)
	}
	callEnvelope := postJSON(t, app.Handler, "/call", `{
		"capability":"release_acceptance.evaluate",
		"subject":{"kind":"agent","id":"verifier","agent_id":"verifier"},
		"input":{"root_contract_id":"ctr_root"}
	}`, http.StatusBadRequest)
	if callEnvelope["status"] != "rejected" {
		t.Fatalf("public release acceptance call = %#v, want rejected", callEnvelope)
	}
	if got := countRowsWhere(t, ctx, app.DB, "release_acceptances", "1 = 1"); got != 0 {
		t.Fatalf("public call wrote release_acceptances = %d, want 0", got)
	}

	acceptance, err := app.EvaluateReleaseAcceptance(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_missing",
		RunLabel:       "inspect",
		CreatedBy:      "operator-test",
	})
	if err != nil {
		t.Fatalf("internal evaluate: %v", err)
	}
	if acceptance.Status != "failed" {
		t.Fatalf("internal missing-root acceptance = %+v, want failed matrix", acceptance)
	}
	inspect := getJSON(t, app.Handler, "/inspect", http.StatusOK)
	release, ok := inspect["release_acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("inspect release_acceptance = %#v, want object", inspect["release_acceptance"])
	}
	latest, ok := release["latest"].(map[string]any)
	if !ok || latest["status"] != "failed" {
		t.Fatalf("inspect release latest = %#v, want failed latest", release["latest"])
	}
	rawInspect := mustJSON(t, inspect)
	for _, forbidden := range []string{"COORDPLANE_TOKEN", "SECRET", "/var/run/docker.sock"} {
		if strings.Contains(rawInspect, forbidden) {
			t.Fatalf("inspect leaked %q: %s", forbidden, rawInspect)
		}
	}
}

type harness struct {
	db      *sql.DB
	store   *store.Store
	service *releaseacceptance.Service
	dbPath  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "release.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	st := store.New(db)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	service, err := releaseacceptance.NewService(releaseacceptance.Config{
		Store:  st,
		DBPath: dbPath,
		Capabilities: []string{
			"command.run",
			"validation.assessment",
			"contract.complete",
			"mailbox.resolve",
			"changeset.submit",
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &harness{db: db, store: st, service: service, dbPath: dbPath}
}

func seedPassingEvidence(t *testing.T, ctx context.Context, h *harness) {
	t.Helper()
	seedTeamConfig(t, ctx, h.store)
	insertRootContract(t, ctx, h.db, "satisfied")
	now := nowString()
	for _, contract := range []struct {
		id     string
		issuer string
		target string
	}{
		{"ctr_developer", "ctr_root", "developer"},
		{"ctr_verifier", "ctr_root", "verifier"},
	} {
		execSQL(t, ctx, h.db, `
INSERT INTO work_contracts (
  id, title, objective, issuer_agent_id, issuer_contract_id, target_kind,
  target_id, status, completion_requirements_json, acceptance_policy_json,
  created_at, updated_at
) VALUES (?, ?, 'objective', 'coordinator', ?, 'agent', ?, 'open', '{}', '{}', ?, ?)`,
			contract.id, contract.id, contract.issuer, contract.target, now, now)
		insertContractTeamScope(t, ctx, h.db, contract.id, "release-test", 1)
	}
	for _, row := range []struct {
		assignment string
		contract   string
		agent      string
		lease      string
		attempt    string
		route      string
		runtime    string
	}{
		{"asg_root", "ctr_root", "coordinator", "lease_root", "att_root", "route_root", "rt_root"},
		{"asg_dev", "ctr_developer", "developer", "lease_dev", "att_dev", "route_dev", "rt_dev"},
		{"asg_ver", "ctr_verifier", "verifier", "lease_ver", "att_ver", "route_ver", "rt_ver"},
	} {
		runtimeChecks := `{"workspace_writable":true,"home_writable":true,"git_workspace_writable":true,"cli_user_consistent":true,"home_private":true,"home_persistent":true,"claude_present":true,"claude_auth_configured":true,"claude_auth_probe_passed":true,"claude_auth_probe_redacted":true}`
		execSQL(t, ctx, h.db, `
INSERT INTO assignments (
  id, contract_id, assignee_agent_id, state, reason, session_route_id,
  created_at, updated_at
) VALUES (?, ?, ?, 'claimed', 'release-test', ?, ?, ?)`,
			row.assignment, row.contract, row.agent, row.route, now, now)
		execSQL(t, ctx, h.db, `
INSERT INTO leases (
  id, assignment_id, agent_id, runtime_id, session_route_id, state,
  expires_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
			row.lease, row.assignment, row.agent, row.runtime, row.route, now, now, now)
		execSQL(t, ctx, h.db, `
INSERT INTO attempts (
  id, lease_id, cli_backend, runtime_kind, session_native_id, start_reason,
  status, transcript_ref, started_at
) VALUES (?, ?, 'claude', 'docker', ?, 'start', 'running', ?, ?)`,
			row.attempt, row.lease, "native_"+row.agent, "obj_transcript_"+row.agent, now)
		execSQL(t, ctx, h.db, `
INSERT INTO session_routes (
  id, agent_id, runtime_id, cli_backend, session_native_id, route_json,
  state, created_at, updated_at
) VALUES (?, ?, ?, 'claude', ?, '{}', 'active', ?, ?)`,
			row.route, row.agent, row.runtime, "native_"+row.agent, now, now)
		execSQL(t, ctx, h.db, `
INSERT INTO runtime_instances (
  id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id,
  lease_id, container_id, container_name, image, network, state,
  workspace_path, home_path, checks_json, env_keys_json, created_at, updated_at
) VALUES (?, ?, 'docker-default', 'docker', ?, ?, ?, ?, ?, 'alpine:3.20',
  'host', 'ready', '/workspace/project', '/home/agent', ?, '[]', ?, ?)`,
			"rti_"+row.agent, row.runtime, row.agent, row.attempt, row.lease,
			"container_"+row.agent, "coordplane_"+row.agent, runtimeChecks, now, now)
	}
	insertCLISession(t, ctx, h.db, "cli_coord_start", "att_root", "rt_root", "coordinator", "start", "finished", "obj_transcript_coord")
	insertCLISession(t, ctx, h.db, "cli_dev_start", "att_dev", "rt_dev", "developer", "start", "finished", "obj_transcript_dev")
	insertCLISession(t, ctx, h.db, "cli_ver_resume", "att_ver", "rt_ver", "verifier", "resume", "finished", "obj_transcript_ver")
	insertObject(t, ctx, h.db, "developer", "SECRET_OUTPUT_BODY", "text/plain")
	insertObjectWithRef(t, ctx, h.db, "developer", "obj_stderr", "stderr body", "text/plain")
	insertObjectWithRef(t, ctx, h.db, "developer", "obj_report", "report body", "text/plain")
	insertEvidence(t, ctx, h.db, "ev_cmd", "command_run", "ctr_developer", "developer", "command_run:cmd_1", "command run passed", "succeeded")
	insertEvidence(t, ctx, h.db, "ev_report", "report", "ctr_developer", "developer", "obj_report", "developer report", "")
	execSQL(t, ctx, h.db, `
INSERT INTO command_runs (
  id, agent_id, lease_id, assignment_id, contract_id, attempt_id,
  session_route_id, runtime_id, container_id, container_name, cwd, argv_json,
  env_keys_json, status, exit_code, stdout_ref, stderr_ref, stdout_bytes,
  stderr_bytes, timeout_seconds, duration_ms, evidence_id, created_at,
  started_at, ended_at, updated_at
) VALUES ('cmd_1', 'developer', 'lease_dev', 'asg_dev', 'ctr_developer',
  'att_dev', 'route_dev', 'rt_dev', 'container_developer', 'coordplane_developer',
  '.', '["sh","-lc","go test ./..."]', '[]', 'succeeded', 0,
  ?, 'obj_stderr', 18, 11, 30, 120, 'ev_cmd', ?, ?, ?, ?)`,
		objectRef("SECRET_OUTPUT_BODY"), now, now, now, now)
	checkedRefs := `[{"kind":"command_run","id":"cmd_1"},{"kind":"changeset","id":"chg_1"},{"kind":"evidence","id":"ev_cmd"}]`
	insertEvidence(t, ctx, h.db, "ev_val", "validation_assessment", "ctr_verifier", "verifier", "validation_assessment:val_1", "validation passed", "pass")
	execSQL(t, ctx, h.db, `
INSERT INTO validation_assessments (
  id, verifier_agent_id, lease_id, assignment_id, contract_id, attempt_id,
  session_route_id, runtime_id, assessed_contract_id, verdict, reason,
  summary, checked_refs_json, ref_snapshot_json, evidence_id,
  idempotency_key, created_at
) VALUES ('val_1', 'verifier', 'lease_ver', 'asg_ver', 'ctr_verifier',
  'att_ver', 'route_ver', 'rt_ver', 'ctr_developer', 'pass',
  'reviewed durable evidence', 'validation passed', ?, '[]', 'ev_val',
  'val-key', ?)`, checkedRefs, now)
	insertGitEvidence(t, ctx, h.db)
	insertMailboxResumeSteerEvidence(t, ctx, h.db)
	insertCapabilityCall(t, ctx, h.db, "cap_cmd", "command.run", "developer", "lease_dev")
	insertCapabilityCall(t, ctx, h.db, "cap_val", "validation.assessment", "verifier", "lease_ver")
	insertCapabilityCall(t, ctx, h.db, "cap_complete", "contract.complete", "coordinator", "lease_root")
	insertEventPayload(t, ctx, h.db, "contract.satisfied", "work_contract", "ctr_root", "contract.complete", map[string]string{"lease_id": "lease_root"})
}

func seedTeamConfig(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if _, err := teamconfig.NewRepository(st).SaveYAML(ctx, []byte(testTeamConfigYAML)); err != nil {
		t.Fatalf("save TeamConfig: %v", err)
	}
}

func seedOtherTeamConfig(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if _, err := teamconfig.NewRepository(st).SaveYAML(ctx, []byte(testOtherTeamConfigYAML)); err != nil {
		t.Fatalf("save other TeamConfig: %v", err)
	}
}

func insertRootContract(t *testing.T, ctx context.Context, db *sql.DB, status string) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO work_contracts (
  id, title, objective, issuer_agent_id, issuer_contract_id, target_kind,
  target_id, status, completion_requirements_json, acceptance_policy_json,
  created_at, updated_at
) VALUES ('ctr_root', 'root', 'root objective', 'operator', '', 'agent',
  'coordinator', ?, '{}', '{}', ?, ?)`, status, now, now)
	insertContractTeamScope(t, ctx, db, "ctr_root", "release-test", 1)
}

func insertContractTeamScope(t *testing.T, ctx context.Context, db *sql.DB, contractID, teamID string, teamVersion int) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO contract_team_scopes (
  contract_id, team_id, team_version, source, created_at
) VALUES (?, ?, ?, 'test-fixture', ?)`,
		contractID, teamID, teamVersion, now)
}

func insertCLISession(t *testing.T, ctx context.Context, db *sql.DB, id, attemptID, runtimeID, agentID, reason, state, transcriptRef string) {
	t.Helper()
	now := nowString()
	resumeOf := ""
	if reason == "resume" {
		resumeOf = "cli_" + agentID + "_start"
	}
	execSQL(t, ctx, db, `
INSERT INTO cli_sessions (
  id, attempt_id, runtime_id, agent_id, cli_backend, profile_name,
  session_native_id, container_id, container_name, process_ref, state,
  start_reason, resume_of, transcript_ref, command_json, env_keys_json,
  started_at, ended_at, updated_at
) VALUES (?, ?, ?, ?, 'claude', 'claude', ?, ?, ?, ?, ?, ?, ?,
  ?, '["claude"]', '["COORDPLANE_BACKEND_URL"]', ?, ?, ?)`,
		id, attemptID, runtimeID, agentID, "native_"+agentID, "container_"+agentID,
		"coordplane_"+agentID, "proc_"+id, state, reason, resumeOf, transcriptRef, now, now, now)
}

func insertObject(t *testing.T, ctx context.Context, db *sql.DB, owner, content, contentType string) string {
	t.Helper()
	ref := objectRef(content)
	insertObjectWithRef(t, ctx, db, owner, ref, content, contentType)
	return ref
}

func insertObjectWithRef(t *testing.T, ctx context.Context, db *sql.DB, owner, ref, content, contentType string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	now := nowString()
	execSQL(t, ctx, db, `
INSERT OR IGNORE INTO object_blobs (
  object_ref, owner_agent_id, checksum, size_bytes, content_type, content, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ref, owner, hex.EncodeToString(sum[:]), len(content), contentType, []byte(content), now)
}

func objectRef(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "obj_sha256_" + hex.EncodeToString(sum[:])
}

func insertEvidence(t *testing.T, ctx context.Context, db *sql.DB, id, kind, contractID, producedBy, contentRef, summary, verdict string) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO evidence (
  id, kind, contract_id, produced_by, content_ref, inline_content,
  summary, verdict, created_at
) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`,
		id, kind, contractID, producedBy, nullable(contentRef), summary, nullable(verdict), now)
}

func insertGitEvidence(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO git_repositories (id, source_path, canonical_branch, status, created_at, updated_at)
VALUES ('repo_1', '/repo/source', 'main', 'active', ?, ?)`, now, now)
	execSQL(t, ctx, db, `
INSERT INTO git_workspaces (
  id, repo_id, agent_id, runtime_id, contract_id, path, base_ref, head_ref,
  dirty, state, created_at, updated_at
) VALUES ('ws_1', 'repo_1', 'developer', 'rt_dev', 'ctr_developer',
  '/workspace/project', 'base', 'head', 0, 'ready', ?, ?)`, now, now)
	execSQL(t, ctx, db, `
INSERT INTO git_operations (
  id, operation_type, actor_agent_id, workspace_id, repo_id, before_ref,
  after_ref, state, feedback_json, created_at, completed_at
) VALUES ('gitop_1', 'changeset.submit', 'developer', 'ws_1', 'repo_1',
  'base', 'head', 'succeeded', '{}', ?, ?)`, now, now)
	execSQL(t, ctx, db, `
INSERT INTO changesets (
  id, workspace_id, repo_id, contract_id, base_ref, head_ref,
  commit_ids_json, summary, evidence_refs_json, state, created_at, updated_at
) VALUES ('chg_1', 'ws_1', 'repo_1', 'ctr_developer', 'base', 'head',
  '["abc123"]', 'release changeset', '["ev_cmd","ev_report"]',
  'submitted', ?, ?)`, now, now)
}

func insertMailboxResumeSteerEvidence(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO mailbox_items (
  id, recipient_agent_id, reason, contract_id, session_route_id, state,
  followup_ref, created_at, updated_at
) VALUES ('mbx_1', 'coordinator', 'child_completed', 'ctr_root',
  'route_root', 'resolved', 'validation:val_1', ?, ?)`, now, now)
	execSQL(t, ctx, db, `
INSERT INTO delivery_attempts (
  id, mailbox_item_id, route_id, signal_json, state, created_at, updated_at
) VALUES ('del_1', 'mbx_1', 'route_root', '{"type":"coordplane.mailbox_signal"}',
  'accepted', ?, ?)`, now, now)
	execSQL(t, ctx, db, `
INSERT INTO queue_items (
  id, queue_name, kind, payload_ref, state, next_run_at, idempotency_key,
  created_at, updated_at
) VALUES ('queue_1', 'runtime.resume', 'mailbox.resume', 'mailbox:mbx_1',
  'done', ?, 'fallback:mbx_1', ?, ?)`, now, now, now)
	insertEvent(t, ctx, db, "session.steer_sent", "mailbox_item", "mbx_1", "session.steer")
	insertEventPayload(t, ctx, db, "cli.adapter_capabilities", "session_route", "route_root", "runtime", map[string]any{"cli_backend": "claude", "supports_same_turn_steer": true})
	insertEvent(t, ctx, db, "session.resumed", "session_route", "route_root", "session.resume")
}

func insertClaimedTaskAssignmentMailbox(t *testing.T, ctx context.Context, db *sql.DB, mailboxID, recipientAgentID, contractID, followupRef string) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO mailbox_items (
  id, recipient_agent_id, reason, contract_id, state, trigger_turn,
  followup_ref, created_at, updated_at
) VALUES (?, ?, 'task_assigned', ?, 'resolved', 1, ?, ?, ?)`,
		mailboxID, recipientAgentID, contractID, followupRef, now, now)
}

type fallbackOptions struct {
	AdapterCapabilityKnown bool
	ResumeQueueDone        bool
	SessionResumed         bool
	ResumeCLISession       bool
}

func forceMailboxFallbackResumeEvidence(t *testing.T, ctx context.Context, db *sql.DB, opts fallbackOptions) {
	t.Helper()
	execSQL(t, ctx, db, `DELETE FROM events WHERE event_type IN ('session.steer_sent', 'session.resumed', 'cli.adapter_capabilities', 'delivery.resume_queued')`)
	execSQL(t, ctx, db, `UPDATE delivery_attempts SET state = 'failed', last_error = 'same-turn steer unsupported', route_id = 'route_root' WHERE id = 'del_1'`)
	queueState := "queued"
	if opts.ResumeQueueDone {
		queueState = "done"
	}
	execSQL(t, ctx, db, `UPDATE queue_items SET state = ? WHERE id = 'queue_1'`, queueState)
	payload := map[string]any{
		"mailbox_id":    "mbx_1",
		"route_id":      "route_root",
		"reason":        "steer_unsupported",
		"queue_item_id": "queue_1",
		"cli_backend":   "claude",
	}
	if opts.AdapterCapabilityKnown {
		payload["supports_same_turn_steer"] = false
		insertEventPayload(t, ctx, db, "cli.adapter_capabilities", "session_route", "route_root", "runtime", map[string]any{"cli_backend": "claude", "supports_same_turn_steer": false})
	}
	insertEventPayload(t, ctx, db, "delivery.resume_queued", "mailbox_item", "mbx_1", "delivery.resume", payload)
	if opts.SessionResumed {
		insertEventPayload(t, ctx, db, "session.resumed", "session_route", "route_root", "session.resume", map[string]any{
			"attempt_id":  "att_root",
			"mailbox_ids": []string{"mbx_1"},
			"reason":      "mailbox.resume",
		})
	}
	if opts.ResumeCLISession {
		insertCLISession(t, ctx, db, "cli_coord_resume", "att_root", "rt_root", "coordinator", "resume", "finished", "obj_transcript_coord_resume")
	}
}

func insertCapabilityCall(t *testing.T, ctx context.Context, db *sql.DB, id, capabilityName, subjectID, leaseID string) {
	t.Helper()
	now := nowString()
	scope := "{}"
	if leaseID != "" {
		scope = `{"lease_id":"` + leaseID + `"}`
	}
	execSQL(t, ctx, db, `
INSERT INTO capability_calls (
  id, trace_id, capability_name, subject_kind, subject_id, scope_json,
  status, created_at
) VALUES (?, ?, ?, 'agent', ?, ?, 'accepted', ?)`,
		id, id, capabilityName, subjectID, scope, now)
}

func insertEvent(t *testing.T, ctx context.Context, db *sql.DB, eventType, aggregateType, aggregateID, capabilityName string) {
	t.Helper()
	insertEventPayload(t, ctx, db, eventType, aggregateType, aggregateID, capabilityName, map[string]string{})
}

func insertEventPayload(t *testing.T, ctx context.Context, db *sql.DB, eventType, aggregateType, aggregateID, capabilityName string, payload any) {
	t.Helper()
	now := nowString()
	eventID := "evt_" + strings.ReplaceAll(eventType+"_"+aggregateID, ".", "_")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	execSQL(t, ctx, db, `
INSERT INTO events (
  id, subject_kind, subject_id, capability_name, event_type,
  aggregate_type, aggregate_id, payload_json, occurred_at
) VALUES (?, 'agent', 'system', ?, ?, ?, ?, ?, ?)`,
		eventID, capabilityName, eventType, aggregateType, aggregateID, string(raw), now)
}

func insertOtherRoot(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := nowString()
	execSQL(t, ctx, db, `
INSERT INTO work_contracts (
  id, title, objective, target_kind, target_id, status,
  completion_requirements_json, acceptance_policy_json, created_at, updated_at
) VALUES ('ctr_other', 'other', 'other', 'agent', 'other', 'open', '{}', '{}', ?, ?)`, now, now)
}

func insertOtherCheckedRefs(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := nowString()
	insertEvidence(t, ctx, db, "ev_other", "command_run", "ctr_other", "developer", "command_run:cmd_other", "other command", "succeeded")
	execSQL(t, ctx, db, `
INSERT INTO command_runs (
  id, agent_id, lease_id, assignment_id, contract_id, attempt_id,
  session_route_id, runtime_id, container_id, container_name, cwd, argv_json,
  env_keys_json, status, exit_code, stdout_ref, stderr_ref, stdout_bytes,
  stderr_bytes, timeout_seconds, duration_ms, evidence_id, created_at,
  started_at, ended_at, updated_at
) VALUES ('cmd_other', 'developer', 'lease_dev', 'asg_dev', 'ctr_other',
  'att_dev', 'route_dev', 'rt_dev', 'container_developer', 'coordplane_developer',
  '.', '["sh","-lc","true"]', '[]', 'succeeded', 0,
  'obj_other_stdout', 'obj_other_stderr', 0, 0, 30, 1, 'ev_other',
  ?, ?, ?, ?)`, now, now, now, now)
	execSQL(t, ctx, db, `
INSERT INTO changesets (
  id, workspace_id, repo_id, contract_id, base_ref, head_ref,
  commit_ids_json, summary, evidence_refs_json, state, created_at, updated_at
) VALUES ('chg_other', 'ws_1', 'repo_1', 'ctr_other', 'base', 'head',
  '["def456"]', 'other changeset', '["ev_other"]', 'submitted', ?, ?)`, now, now)
}

func assertPredicates(t *testing.T, acceptance releaseacceptance.Acceptance, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, predicate := range acceptance.PredicateResults {
		got[predicate.Name] = predicate.Status
	}
	for name, status := range want {
		if got[name] != status {
			t.Fatalf("predicate %s = %s in %+v, want %s", name, got[name], acceptance.PredicateResults, status)
		}
	}
}

func capabilityNames(t *testing.T, envelope map[string]any) []string {
	t.Helper()
	rawData, ok := envelope["data"].([]any)
	if !ok {
		t.Fatalf("capability data = %#v, want array", envelope["data"])
	}
	var out []string
	for _, raw := range rawData {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("capability definition = %#v, want object", raw)
		}
		name, ok := item["name"].(string)
		if !ok {
			t.Fatalf("capability name = %#v, want string", item["name"])
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func getJSON(t *testing.T, handler http.Handler, path string, wantStatus int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return decodeObject(t, rec.Body.Bytes())
}

func getJSONWithBearer(t *testing.T, handler http.Handler, path, token string, wantStatus int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode GET %s: %v; body=%s", path, err, rec.Body.String())
	}
	return out
}

func insertRuntimeAuthSession(t *testing.T, ctx context.Context, db *sql.DB, agentID string) string {
	t.Helper()
	now := nowString()
	token := "tok_" + agentID + "_discovery"
	runtimeID := "rt_" + agentID + "_discovery"
	attemptID := "att_" + agentID + "_discovery"
	leaseID := "lease_" + agentID + "_discovery"
	assignmentID := "asg_" + agentID + "_discovery"
	routeID := "route_" + agentID + "_discovery"
	contractID := "ctr_" + agentID + "_discovery"
	execSQL(t, ctx, db, `
INSERT INTO work_contracts (
  id, title, objective, issuer_agent_id, issuer_contract_id, target_kind,
  target_id, status, completion_requirements_json, acceptance_policy_json,
  created_at, updated_at
) VALUES (?, 'auth', 'auth discovery', 'operator', '', 'agent', ?, 'active', '{}', '{}', ?, ?)`,
		contractID, agentID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO assignments (
  id, contract_id, assignee_agent_id, assignee_role, state, priority, reason,
  session_route_id, created_at, updated_at
) VALUES (?, ?, ?, '', 'claimed', 0, 'auth discovery', ?, ?, ?)`,
		assignmentID, contractID, agentID, routeID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO session_routes (
  id, agent_id, runtime_id, cli_backend, session_native_id, route_json, state,
  created_at, updated_at
) VALUES (?, ?, ?, 'fake', ?, '{}', 'active', ?, ?)`,
		routeID, agentID, runtimeID, "native_"+agentID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO leases (
  id, assignment_id, agent_id, runtime_id, session_route_id, state, expires_at,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		leaseID, assignmentID, agentID, runtimeID, routeID, time.Now().Add(time.Hour).UTC().Format(testTimeLayout), now, now)
	execSQL(t, ctx, db, `
INSERT INTO attempts (
  id, lease_id, cli_backend, runtime_kind, session_native_id, start_reason,
  status, transcript_ref, started_at
) VALUES (?, ?, 'fake', 'external', ?, 'initial', 'running', '', ?)`,
		attemptID, leaseID, "native_"+agentID, now)
	execSQL(t, ctx, db, `
INSERT INTO runtime_instances (
  id, runtime_id, runtime_profile, runtime_kind, agent_id, attempt_id,
  lease_id, state, workspace_path, home_path, checks_json, env_keys_json,
  created_at, updated_at
) VALUES (?, ?, 'external-local', 'external', ?, ?, ?, 'ready', '/workspace', '/home/agent', '{}', '[]', ?, ?)`,
		"rti_"+agentID+"_discovery", runtimeID, agentID, attemptID, leaseID, now, now)
	execSQL(t, ctx, db, `
INSERT INTO runtime_tokens (
  token_hash, agent_id, runtime_id, attempt_id, lease_id, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`,
		cpruntime.RuntimeTokenHash(token), agentID, runtimeID, attemptID, leaseID, now, now)
	return token
}

func postJSON(t *testing.T, handler http.Handler, path, body string, wantStatus int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("POST %s status = %d, want %d; body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return decodeObject(t, rec.Body.Bytes())
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON: %v; raw=%s", err, raw)
	}
	return out
}

func execSQL(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec SQL failed: %v\nquery=%s\nargs=%v", err, query, args)
	}
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nowString() string {
	return time.Now().UTC().Format(testTimeLayout)
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func TestPredicateResultNamesAreStable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	seedPassingEvidence(t, ctx, h)
	acceptance, err := h.service.Evaluate(ctx, releaseacceptance.EvaluateInput{
		RootContractID: "ctr_root",
		TeamID:         "release-test",
		TeamVersion:    1,
		RunLabel:       "stable",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var names []string
	for _, predicate := range acceptance.PredicateResults {
		names = append(names, predicate.Name)
	}
	want := []string{
		"backend_durable_ready",
		"command_run_public_evidence",
		"docker_runtime_evidence",
		"git_changeset_evidence_chain",
		"mailbox_resume_steer",
		"real_cli_session_lifecycle",
		"root_contract_completed",
		"root_contract_present",
		"team_scope_binding",
		"teamconfig_validation_policy",
		"validation_assessment_pass",
	}
	sort.Strings(names)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("predicate names = %v, want %v", names, want)
	}
}

const testTeamConfigYAML = `
team_id: release-test
version: 1
runtime_profiles:
  docker-default:
    kind: docker
    image: alpine:3.20
    workspace_mode: isolated
termination:
  terminal_contract_type: root
  accepted_by_capability: validation.assessment
agents:
  - id: coordinator
    role_prompt: "coordinate release work"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - contract.complete
      - mailbox.resolve
  - id: developer
    role_prompt: "produce release evidence"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - command.run
      - changeset.submit
      - report.submit
  - id: verifier
    role_prompt: "verify release evidence"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - validation.assessment
      - report.submit
`

const testHTTPDiscoveryTeamConfigYAML = `
team_id: release-test
version: 1
runtime_profiles:
  external-local:
    kind: external
agents:
  - id: verifier
    role_prompt: "verify release evidence"
    runtime_profile: external-local
    cli_backend: fake
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - validation.assessment
      - report.submit
`

const testOtherTeamConfigYAML = `
team_id: other-release-test
version: 1
runtime_profiles:
  docker-default:
    kind: docker
    image: alpine:3.20
    workspace_mode: isolated
termination:
  terminal_contract_type: root
  accepted_by_capability: validation.assessment
agents:
  - id: coordinator
    role_prompt: "coordinate other release work"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - contract.complete
      - mailbox.resolve
  - id: developer
    role_prompt: "produce other release evidence"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - command.run
      - changeset.submit
      - report.submit
  - id: verifier
    role_prompt: "verify other release evidence"
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
    capabilities:
      - assignment.next
      - validation.assessment
      - report.submit
`
