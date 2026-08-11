package daemon

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRuntimeExitedReasonIncludesExitCodeAndRedactedLogTail(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "run.log")
	secret := "exited-reason-secret-canary"
	var log strings.Builder
	for i := 0; i < 30; i++ {
		log.WriteString("preparing step\n")
	}
	log.WriteString("building project: token=" + secret + "\n")
	requireNoError(t, os.WriteFile(logPath, []byte(log.String()), 0o600))

	exit := 0
	redact := newRuntimeRedaction([]string{root}, []string{secret})
	reason := runtimeExitedReason("CLI process exited", &exit, logPath, redact)
	if !strings.HasPrefix(reason, "CLI process exited (exit 0)") {
		t.Fatalf("reason = %q, want exit code prefix", reason)
	}
	if strings.Contains(reason, secret) {
		t.Fatalf("reason leaked secret: %q", reason)
	}
	if !strings.Contains(reason, "last log activity") || !strings.Contains(reason, "building project") {
		t.Fatalf("reason omits log tail: %q", reason)
	}
	if strings.Count(reason, "\n") != 0 {
		t.Fatalf("reason should be single-line: %q", reason)
	}
	if len(reason) > 1800 {
		t.Fatalf("reason too large: %d bytes", len(reason))
	}
}

func TestRuntimeExitedReasonWithoutLogOrExitCodeStaysBounded(t *testing.T) {
	if reason := runtimeExitedReason("CLI process exited", nil, filepath.Join(t.TempDir(), "missing.log"), runtimeRedaction{}); reason != "CLI process exited" {
		t.Fatalf("reason = %q, want original without tail", reason)
	}
}

func TestCompactLogTailKeepsLastNonEmptyLinesInOrder(t *testing.T) {
	input := "\nline1\nline2\n\nline3\n"
	if got, want := compactLogTail(input, 10), "line1; line2; line3"; got != want {
		t.Fatalf("compactLogTail() = %q, want %q", got, want)
	}
	if got := compactLogTail(input, 2); got != "line2; line3" {
		t.Fatalf("compactLogTail(limit 2) = %q, want %q", got, "line2; line3")
	}
	if got := compactLogTail("   \n\n  ", 10); got != "" {
		t.Fatalf("compactLogTail(blank) = %q, want empty", got)
	}
}
