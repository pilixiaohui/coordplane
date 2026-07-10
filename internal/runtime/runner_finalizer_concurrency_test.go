package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"coordplane/internal/capability"
	"coordplane/internal/coordination"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestOneShotFinalizerCannotOverwriteConcurrentContractComplete(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate SQLite: %v", err)
	}
	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register skills: %v", err)
	}
	coordinationService := coordination.NewService(st)
	cfg := teamconfig.Config{
		TeamID:  "finalizer-race",
		Version: 1,
		Agents: []teamconfig.AgentConfig{{
			ID:             "builder",
			RolePrompt:     "complete work before the one-shot process exits",
			RuntimeProfile: "external-debug",
			CLIBackend:     "fake",
			Skills:         []string{"coordplane-service"},
			Capabilities:   []string{"contract.current", "report.submit", "contract.complete"},
		}},
	}
	runner, err := NewRunner(RunnerConfig{
		Store:         st,
		Coordination:  coordinationService,
		TeamConfig:    cfg,
		Skills:        skillRegistry,
		Runtime:       ExternalRuntime{ID: "external_finalizer_race", WorkspaceRoot: t.TempDir(), HomeRoot: t.TempDir(), Ready: true},
		Adapter:       NewFakeCLIAdapter(),
		BackendURL:    "http://coordplane.test",
		WorkspaceName: "finalizer-race",
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	added, err := coordinationService.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "complete/finalizer race",
		Objective:     "completion must win over a stale one-shot finalizer decision",
		TargetAgentID: "builder",
	})
	if err != nil {
		t.Fatalf("add contract: %v", err)
	}
	session, err := runner.StartAssignment(ctx, "builder", added.AssignmentID)
	if err != nil {
		t.Fatalf("start assignment: %v", err)
	}
	report, err := coordinationService.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: session.LeaseID,
		AgentID: "builder",
		Summary: "completed before process exit",
		Content: "durable result",
	})
	if err != nil {
		t.Fatalf("submit report: %v", err)
	}

	snapshotRead := make(chan error, 1)
	allowFinalizer := make(chan struct{})
	finalizerDone := make(chan error, 1)
	go func() {
		var leaseState, assignmentState string
		err := db.QueryRowContext(ctx, `
SELECT l.state, a.state
FROM leases l
JOIN assignments a ON a.id = l.assignment_id
WHERE l.id = ?`, session.LeaseID).Scan(&leaseState, &assignmentState)
		if err == nil && (leaseState != "active" || assignmentState != "claimed") {
			err = fmt.Errorf("stale finalizer snapshot = %s/%s, want active/claimed", leaseState, assignmentState)
		}
		snapshotRead <- err
		if err != nil {
			return
		}
		<-allowFinalizer
		finalizerDone <- runner.failAttempt(ctx, session.AttemptID, NewAgentExitedWithoutTerminalAction("stale one-shot finalizer").Error())
	}()
	if err := <-snapshotRead; err != nil {
		t.Fatal(err)
	}
	completed := coordinationService.CompleteContract(ctx, coordination.CompleteContractInput{
		LeaseID:     session.LeaseID,
		AgentID:     "builder",
		EvidenceIDs: []string{report.ID},
		Summary:     "completed concurrently with provider exit",
	})
	if completed.Status != capability.StatusAccepted {
		t.Fatalf("contract.complete = %+v, want accepted", completed)
	}
	close(allowFinalizer)
	if err := <-finalizerDone; err != nil {
		t.Fatalf("stale finalizer convergence: %v", err)
	}

	assertFinalizerRaceState(t, ctx, db, added.ContractID, added.AssignmentID, session)
}

func assertFinalizerRaceState(t *testing.T, ctx context.Context, db *sql.DB, contractID, assignmentID string, session AssignmentSession) {
	t.Helper()
	var contractState, assignmentState, leaseState, attemptState, routeState string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&contractState); err != nil {
		t.Fatalf("read contract: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM assignments WHERE id = ?`, assignmentID).Scan(&assignmentState); err != nil {
		t.Fatalf("read assignment: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, session.LeaseID).Scan(&leaseState); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM attempts WHERE id = ?`, session.AttemptID).Scan(&attemptState); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM session_routes WHERE id = ?`, session.Route.ID).Scan(&routeState); err != nil {
		t.Fatalf("read route: %v", err)
	}
	if contractState != "satisfied" || assignmentState != "returned" || leaseState != "released" || attemptState != "completed" || routeState != "completed" {
		t.Fatalf("final states = contract:%s assignment:%s lease:%s attempt:%s route:%s, want satisfied/returned/released/completed/completed",
			contractState, assignmentState, leaseState, attemptState, routeState)
	}
	for _, check := range []struct {
		table string
		where string
		want  int
	}{
		{table: "runtime_tokens", where: "attempt_id = ? AND state = 'active'", want: 0},
		{table: "active_guards", where: "attempt_id = ? AND state = 'active'", want: 0},
		{table: "events", where: "aggregate_id = ? AND event_type = 'session.failed'", want: 0},
		{table: "events", where: "aggregate_id = ? AND event_type = 'contract.satisfied'", want: 1},
	} {
		id := session.AttemptID
		if check.table == "events" && check.want == 1 {
			id = contractID
		}
		var got int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+check.table+" WHERE "+check.where, id).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", check.table, err)
		}
		if got != check.want {
			t.Fatalf("%s where %s = %d, want %d", check.table, check.where, got, check.want)
		}
	}
}
