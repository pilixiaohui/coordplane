//go:build e2e

package e2e_test

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPFValidatorRejectsMissingDuplicateAndNegativeEvidence(t *testing.T) {
	valid := validatorFixture()
	if err := validateObserver(&valid); err != nil {
		t.Fatalf("valid raw ledger rejected: %v", err)
	}
	tests := map[string]func(*pfReport){
		"missing commit":    func(report *pfReport) { report.Observer = append(report.Observer[:2], report.Observer[3:]...) },
		"duplicate stage":   func(report *pfReport) { report.Observer = append(report.Observer, report.Observer[634]) },
		"negative duration": func(report *pfReport) { report.Samples[0].RunFacts[0].CleanupNS = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			report := cloneReport(valid)
			mutate(&report)
			if err := validateObserver(&report); err == nil {
				t.Fatal("invalid raw ledger passed")
			}
		})
	}
}

func TestPFBaselineRequiresCompleteKeysButAllowsNewRevision(t *testing.T) {
	baseline := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: "release", Result: "PASS", Revision: "old",
		Environment: map[string]string{"environment_fingerprint": "machine", "statistics_version": pfStatisticsVersion, "image_digest": "old-image"},
		Fixture:     map[string]any{"generator_sha256": "fixture"}, Statistics: map[string]int64{"T_queue_p50": 100, "T_queue_max": 200},
		Baseline: map[string]string{"owner_approved": "true"},
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	writePerfReport(t, path, &baseline)
	t.Setenv("PF01_BASELINE_REPORT", path)
	current := cloneReport(baseline)
	current.Result, current.Revision, current.Environment["image_digest"], current.Baseline = "", "new", "new-image", nil
	if result := releasePFResult(&current); result != "PASS" {
		t.Fatalf("same environment across implementation revisions = %s", result)
	}
	delete(current.Statistics, "T_queue_max")
	if result := releasePFResult(&current); result != "FAIL" {
		t.Fatalf("sparse statistics baseline comparison = %s", result)
	}
}

func validatorFixture() pfReport {
	tasks := []string{"source-a", "source-b", "source-c", "source-d", "integration-b", "integration-c", "integration-d"}
	runs := []string{"run-a", "run-b", "run-c", "run-d", "run-ib", "run-ic", "run-id"}
	facts := make([]pfRunFact, len(tasks))
	for index := range tasks {
		role := []string{"source", "integration"}[index/4]
		facts[index] = pfRunFact{TaskID: tasks[index], RunID: runs[index], Role: role, TerminalState: "exited",
			TerminalObservedUnixNS: int64(index + 1), ContainerAbsentNS: 1, CleanupNS: 2, ContainerAbsent: true, ResourcesAbsent: true}
	}
	report := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: "smoke", Statistics: map[string]int64{}, Resources: map[string]int64{},
		Samples: []pfSample{{ID: "concurrent-01-wave-01", BatchID: "concurrent-01", Parallelism: 4,
			WaveNS: 1, WorkNS: 1, TaskIDs: tasks[:4], RunIDs: runs[:4], IntegrationTaskIDs: tasks[4:], IntegrationRunIDs: runs[4:],
			QueueNS: []int64{1, 1, 1, 1}, FanoutNS: 1, DirectCASNS: 1, IntegrationNS: []int64{1, 1, 1}, Integrations3NS: 1, DiskBytes: 1, RunFacts: facts}},
		Faults: []pfFaultRow{validFault("live", 1, 4), validFault("capture", 1, 1), validFault("cas", 1, 1)},
	}
	point := func(id, request, task, run, message string, offset int64) map[string]any {
		return map[string]any{"schema_version": float64(1), "record_type": "point", "daemon_origin_id": "origin", "sample_id": "concurrent-01",
			"request_id": request, "task_id": task, "run_id": run, "message_id": message, "point_id": id, "mono_offset_ns": float64(offset), "result": "success"}
	}
	for index := 0; index < 200; index++ {
		task, run, request := tasks[index%4], runs[index%4], "p5-progress-"+strconv.Itoa(index)
		report.Observer = append(report.Observer,
			map[string]any{"schema_version": float64(1), "record_type": "client", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "request_id": request, "task_id": task, "run_id": run, "operation": "post progress", "duration_ns": float64(1), "result": "success"},
			point("api.progress.received", request, task, run, "", int64(index+1)), point("core.progress.committed", request, task, run, "", int64(index+201)))
	}
	for index := range tasks {
		report.Observer = append(report.Observer, point("core.outcome.accepted_commit", "outcome-"+tasks[index], tasks[index], runs[index], "", 1),
			point("git.capture.submitted_commit", "submit-"+tasks[index], tasks[index], runs[index], "", 2))
	}
	for index := 0; index < 10; index++ {
		message := "message-" + string(rune('a'+index))
		report.Observer = append(report.Observer, point("core.message.created_commit", "pf01-peer-"+message, tasks[0], runs[0], message, 1),
			point("core.message.acknowledged_commit", "ack-"+message, tasks[3], runs[3], message, 2))
	}
	stages := []string{"git.clone.lock_wait", "git.clone.prepare", "runtime.container.create_start", "git.capture.freeze", "git.capture.handoff", "git.capture.lock_wait", "git.capture.import", "git.capture.fsck", "git.capture.ref", "git.advance.lock_wait", "git.advance.ancestry", "git.advance.update_ref", "runtime.cleanup"}
	for _, stage := range stages {
		count := 7
		if stage == "git.advance.update_ref" {
			count = 4
		}
		for index := 0; index < count; index++ {
			report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "stage_start", "daemon_origin_id": "origin", "sample_id": "concurrent-01",
				"task_id": tasks[index], "run_id": runs[index], "stage_id": stage, "stage_key_sha256": stage + runs[index], "attempt_index": float64(0), "start_offset_ns": float64(1), "start_unix_ns": float64(1)})
			report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "stage", "daemon_origin_id": "origin", "sample_id": "concurrent-01",
				"task_id": tasks[index], "run_id": runs[index], "stage_id": stage, "stage_key_sha256": stage + runs[index], "attempt_index": float64(0), "start_unix_ns": float64(1), "duration_ns": float64(1), "result": "success"})
		}
	}
	for index := range runs {
		report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "runtime_limit", "daemon_origin_id": "origin", "sample_id": "concurrent-01",
			"task_id": tasks[index], "run_id": runs[index], "memory_bytes": float64(512 << 20), "nano_cpus": float64(1_000_000_000), "pids_limit": float64(256)})
	}
	report.Observer = append(report.Observer,
		map[string]any{"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "mono_offset_ns": float64(1), "rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1)},
		map[string]any{"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "mono_offset_ns": float64(100000001), "rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1)})
	return report
}

func validFault(kind string, index, runs int) pfFaultRow {
	ids := []string{kind + "a", kind + "b", kind + "c", kind + "d"}[:runs]
	row := pfFaultRow{SampleID: "fault-" + kind, Kind: kind, Index: index, FreshDataDirID: "root-" + kind, RecoveryNS: 1,
		RunIDsBefore: ids, RunIDsAfter: append([]string(nil), ids...), PreRestart: "durable", FinalSHA: "sha", GitFSCK: "pass", Cleanup: "absent", Result: "PASS"}
	if kind == "live" {
		row.DurableUnacked, row.Counts.PeerMessages, row.Counts.PeerAcknowledged, row.Counts.PeerAckEvent = 4, 10, 10, 10
	}
	return row
}

func cloneReport(report pfReport) (clone pfReport) {
	raw, _ := json.Marshal(report)
	_ = json.Unmarshal(raw, &clone)
	return clone
}
