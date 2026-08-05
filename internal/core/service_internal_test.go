package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestServiceTimestampsRemainLexicallySortable(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC),
		time.Date(2026, 7, 12, 1, 2, 3, 1, time.UTC),
		time.Date(2026, 7, 12, 1, 2, 3, 100_000_000, time.UTC),
	}
	formatted := make([]string, 0, len(instants))
	for _, instant := range instants {
		service := &Service{now: func() time.Time { return instant }}
		formatted = append(formatted, service.nowText())
	}
	for index := 1; index < len(formatted); index++ {
		if len(formatted[index]) != len(formatted[0]) || formatted[index-1] >= formatted[index] {
			t.Fatalf("timestamps are not fixed-width sortable: %q", formatted)
		}
	}
}

func TestServiceRequireReadyFencesMutationsUntilRecoveryCompletes(t *testing.T) {
	service := &Service{}
	service.SetReady(false, "startup reconciliation")

	err := service.RequireReady()
	var coreErr *Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("RequireReady() error = %v, want *Error", err)
	}
	if coreErr.Code != CodeRuntimeUnavailable || !coreErr.Retryable {
		t.Fatalf("RequireReady() error = %+v, want retryable %s", coreErr, CodeRuntimeUnavailable)
	}
	if coreErr.Message != "daemon is not ready: startup reconciliation" {
		t.Fatalf("RequireReady() message = %q", coreErr.Message)
	}

	service.SetReady(true, "")
	if err := service.RequireReady(); err != nil {
		t.Fatalf("RequireReady() after recovery = %v", err)
	}
}

func TestRunnableTasksUseCanonicalPriorityCreatedAndIDOrder(t *testing.T) {
	tasks := []Task{
		{ID: "tsk-later", Priority: 10, CreatedAt: "2026-07-12T00:00:01.000000000Z"},
		{ID: "tsk-z", Priority: 10, CreatedAt: "2026-07-12T00:00:00.000000000Z"},
		{ID: "tsk-a", Priority: 10, CreatedAt: "2026-07-12T00:00:00.000000000Z"},
		{ID: "tsk-high", Priority: 11, CreatedAt: "2026-07-12T00:00:02.000000000Z"},
	}

	sortRunnable(tasks)
	got := make([]string, 0, len(tasks))
	for _, task := range tasks {
		got = append(got, task.ID)
	}
	want := []string{"tsk-high", "tsk-a", "tsk-z", "tsk-later"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runnable order = %v, want %v", got, want)
	}
}

func TestClaimNextReturnsCandidateLookupErrors(t *testing.T) {
	lookupError := errors.New("candidate lookup failed")
	for _, failure := range []string{"project", "task", "agent"} {
		t.Run(failure, func(t *testing.T) {
			tx := &claimLookupErrorTransaction{failure: failure, err: lookupError}
			service := &Service{
				repository: claimLookupErrorRepository{tx: tx},
				now:        func() time.Time { return time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC) },
				maxRuns:    1,
			}
			if _, claimed, err := service.ClaimNext(context.Background(), "prj-1"); !errors.Is(err, lookupError) || claimed {
				t.Fatalf("ClaimNext() claimed=%t err=%v, want lookup error", claimed, err)
			}
		})
	}
}

type claimLookupErrorRepository struct {
	Repository
	tx Transaction
}

func (r claimLookupErrorRepository) Transact(ctx context.Context, fn func(Transaction) error) error {
	return fn(r.tx)
}

type claimLookupErrorTransaction struct {
	Transaction
	failure string
	err     error
}

func (tx *claimLookupErrorTransaction) RunnableTasks(string) ([]Task, error) {
	return []Task{{ID: "tsk-1", ProjectID: "prj-1", AssigneeAgentID: "agt-1", Status: TaskQueued}}, nil
}

func (tx *claimLookupErrorTransaction) Project(string) (Project, error) {
	if tx.failure == "project" {
		return Project{}, tx.err
	}
	return Project{ID: "prj-1", Status: ProjectActive}, nil
}

func (tx *claimLookupErrorTransaction) Task(string) (Task, error) {
	if tx.failure == "task" {
		return Task{}, tx.err
	}
	return Task{ID: "tsk-1", ProjectID: "prj-1", AssigneeAgentID: "agt-1", Status: TaskQueued}, nil
}

func (tx *claimLookupErrorTransaction) Agent(string) (Agent, error) {
	if tx.failure == "agent" {
		return Agent{}, tx.err
	}
	return Agent{ID: "agt-1", Status: AgentActive}, nil
}
