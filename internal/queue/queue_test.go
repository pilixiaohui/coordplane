package queue_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"coordplane/internal/queue"
	"coordplane/internal/store"

	_ "modernc.org/sqlite"
)

func TestQueueLeaseAndIdempotency(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	now := time.Date(2026, 7, 4, 3, 15, 0, 0, time.UTC)

	first, created, err := q.Enqueue(ctx, queue.NewItem{
		QueueName:      "assignment",
		Kind:           "start_assignment",
		PayloadRef:     "assignment:asg_1",
		IdempotencyKey: "idem-start-asg-1",
	}, now)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if !created {
		t.Fatal("first enqueue returned created=false")
	}

	second, created, err := q.Enqueue(ctx, queue.NewItem{
		QueueName:      "assignment",
		Kind:           "start_assignment",
		PayloadRef:     "assignment:asg_1",
		IdempotencyKey: "idem-start-asg-1",
	}, now)
	if err != nil {
		t.Fatalf("idempotent enqueue: %v", err)
	}
	if created {
		t.Fatal("duplicate enqueue returned created=true")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate enqueue id = %s, want original %s", second.ID, first.ID)
	}

	workerA, err := q.Claim(ctx, "assignment", "worker-a", time.Minute, now)
	if err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}
	if workerA == nil {
		t.Fatal("worker-a claim returned nil")
	}
	if workerA.LeaseOwner != "worker-a" || workerA.AttemptCount != 1 {
		t.Fatalf("worker-a item = %+v, want owner worker-a and attempt_count 1", workerA)
	}

	workerB, err := q.Claim(ctx, "assignment", "worker-b", time.Minute, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("worker-b early claim: %v", err)
	}
	if workerB != nil {
		t.Fatalf("worker-b claimed active lease: %+v", workerB)
	}

	reclaimed, err := q.Claim(ctx, "assignment", "worker-b", time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("worker-b expired claim: %v", err)
	}
	if reclaimed == nil {
		t.Fatal("worker-b did not claim expired lease")
	}
	if reclaimed.LeaseOwner != "worker-b" || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed item = %+v, want owner worker-b and attempt_count 2", reclaimed)
	}

	if err := q.Complete(ctx, reclaimed.ID, "worker-a"); !errors.Is(err, queue.ErrLeaseNotOwned) {
		t.Fatalf("stale owner complete error = %v, want ErrLeaseNotOwned", err)
	}
	if err := q.Complete(ctx, reclaimed.ID, "worker-b"); err != nil {
		t.Fatalf("complete by current owner: %v", err)
	}

	afterDone, err := q.Claim(ctx, "assignment", "worker-c", time.Minute, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("claim after done: %v", err)
	}
	if afterDone != nil {
		t.Fatalf("done item was claimable: %+v", afterDone)
	}
}

func TestQueueFailRetriesThenDeadWithoutDroppingCanonicalPayload(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	now := time.Date(2026, 7, 4, 3, 30, 0, 0, time.UTC)

	enqueued, _, err := q.Enqueue(ctx, queue.NewItem{
		QueueName:  "delivery",
		Kind:       "deliver_mailbox",
		PayloadRef: "mailbox:mbx_1",
	}, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := q.Claim(ctx, "delivery", "worker-a", time.Minute, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("claim returned nil")
	}

	failed, err := q.Fail(ctx, claimed.ID, "worker-a", 2, queue.FixedBackoff(time.Minute), now, errors.New("steer unavailable"))
	if err != nil {
		t.Fatalf("first fail: %v", err)
	}
	if failed.State != "queued" {
		t.Fatalf("state after first fail = %s, want queued", failed.State)
	}
	if failed.PayloadRef != enqueued.PayloadRef {
		t.Fatalf("payload_ref = %s, want %s", failed.PayloadRef, enqueued.PayloadRef)
	}
	if failed.LastError != "steer unavailable" {
		t.Fatalf("last_error = %q", failed.LastError)
	}
	if !failed.NextRunAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("next_run_at = %s, want %s", failed.NextRunAt, now.Add(time.Minute))
	}

	retry, err := q.Claim(ctx, "delivery", "worker-b", time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if retry == nil {
		t.Fatal("retry claim returned nil")
	}
	dead, err := q.Fail(ctx, retry.ID, "worker-b", 2, queue.FixedBackoff(time.Minute), now.Add(2*time.Minute), errors.New("still unavailable"))
	if err != nil {
		t.Fatalf("second fail: %v", err)
	}
	if dead.State != "dead" {
		t.Fatalf("state after retry limit = %s, want dead", dead.State)
	}
	if dead.PayloadRef != enqueued.PayloadRef {
		t.Fatalf("dead item lost payload_ref = %s, want %s", dead.PayloadRef, enqueued.PayloadRef)
	}
}

func newTestQueue(t *testing.T) *queue.Queue {
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
	return queue.New(db)
}
