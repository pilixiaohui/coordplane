package daemon

import (
	"testing"
	"time"

	"coordplane/internal/config"
	"coordplane/internal/core"
)

func TestRunDeadlineAtPicksTaskBudgetAndGlobalBackstop(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		budgetSec   int64
		runTimeout  time.Duration
		wantAfter   time.Duration
		wantDisable bool
	}{
		{name: "task budget only", budgetSec: 300, runTimeout: 0, wantAfter: 300 * time.Second},
		{name: "global backstop only", budgetSec: 0, runTimeout: 5 * time.Minute, wantAfter: 5 * time.Minute},
		{name: "task budget tighter than backstop", budgetSec: 300, runTimeout: 10 * time.Minute, wantAfter: 300 * time.Second},
		{name: "backstop tighter than task budget", budgetSec: 900, runTimeout: 2 * time.Minute, wantAfter: 2 * time.Minute},
		{name: "neither set disables the wall-clock cap", budgetSec: 0, runTimeout: 0, wantDisable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			deadline := runDeadlineAt(test.budgetSec, test.runTimeout, now)
			if test.wantDisable {
				if deadline != "" {
					t.Fatalf("deadline = %q, want disabled", deadline)
				}
				return
			}
			if deadline == "" {
				t.Fatal("deadline is empty, want a wall-clock cap")
			}
			parsed, err := time.Parse(time.RFC3339Nano, deadline)
			if err != nil {
				t.Fatalf("deadline %q is not RFC3339Nano: %v", deadline, err)
			}
			if !parsed.Equal(now.Add(test.wantAfter)) {
				t.Fatalf("deadline = %s, want now + %s", parsed, test.wantAfter)
			}
		})
	}
}

func TestRunStalledUsesHeartbeatThenObservationThenStart(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	old := now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	for _, test := range []struct {
		name    string
		timeout time.Duration
		run     core.Run
		want    bool
	}{
		{name: "zero stall timeout disables detection", timeout: 0, run: core.Run{HeartbeatAt: old}, want: false},
		{name: "fresh heartbeat is not stalled", timeout: time.Minute, run: core.Run{HeartbeatAt: recent}, want: false},
		{name: "stale heartbeat is stalled", timeout: time.Minute, run: core.Run{HeartbeatAt: old}, want: true},
		{name: "no heartbeat falls back to observation", timeout: time.Minute, run: core.Run{LastObservedAt: old}, want: true},
		{name: "recent observation with no heartbeat is not stalled", timeout: time.Minute, run: core.Run{LastObservedAt: recent}, want: false},
		{name: "no heartbeat or observation falls back to start", timeout: time.Minute, run: core.Run{StartedAt: old}, want: true},
		{name: "start is the only timestamp and it is fresh", timeout: time.Minute, run: core.Run{StartedAt: recent}, want: false},
		{name: "no timestamps at all is not stalled", timeout: time.Minute, run: core.Run{}, want: false},
		{name: "malformed timestamp is not stalled", timeout: time.Minute, run: core.Run{HeartbeatAt: "not-a-time"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller := &runtimeController{config: config.Config{Runtime: config.RuntimeConfig{StallTimeout: test.timeout}}}
			if got := controller.runStalled(test.run); got != test.want {
				t.Fatalf("runStalled(%+v) = %v, want %v", test.run, got, test.want)
			}
		})
	}
}
