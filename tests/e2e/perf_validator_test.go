//go:build e2e

package e2e_test

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPFValidatorRejectsMissingDuplicateAndNegativeEvidence(t *testing.T) {
	valid := validatorFixture()
	if err := validateObserver(&valid); err != nil {
		t.Fatalf("valid raw ledger rejected: %v", err)
	}
	tests := map[string]func(*pfReport){
		"missing commit": func(report *pfReport) {
			report.Observer = append(report.Observer[:2], report.Observer[3:]...)
		},
		"missing formal binary digest": func(report *pfReport) {
			delete(report.Binaries, "coordplane-git-helper")
		},
		"duplicate stage": func(report *pfReport) {
			record := firstPFRecord(report, "record_type", "stage")
			report.Observer = append(report.Observer, record)
		},
		"negative duration": func(report *pfReport) {
			report.Samples[0].RunFacts[0].CleanupNS = -1
		},
		"illegal terminal": func(report *pfReport) {
			report.Samples[0].RunFacts[0].TerminalState = "active"
		},
		"cleanup before container absent": func(report *pfReport) {
			report.Samples[0].RunFacts[0].CleanupNS = 0
		},
		"zero capture fault signature": func(report *pfReport) {
			report.Faults[1].Counts.PeerMessages = 0
			report.Faults[1].Counts.PeerAcknowledged = 0
			report.Faults[1].Counts.PeerAckEvent = 0
		},
		"progress crosses origin": func(report *pfReport) {
			firstPFRecord(report, "point_id", "core.progress.committed")["daemon_origin_id"] = "other-origin"
		},
		"stage crosses origin": func(report *pfReport) {
			firstPFRecord(report, "record_type", "stage")["daemon_origin_id"] = "other-origin"
		},
		"stage replaces inventory identity": func(report *pfReport) {
			for _, record := range report.Observer {
				if record["stage_id"] == "git.clone.prepare" && record["task_id"] == "source-a" {
					record["task_id"] = "source-b"
				}
			}
		},
		"interrupted has duration": func(report *pfReport) {
			record := firstPFRecord(report, "record_type", "stage")
			record["record_type"] = "stage_interrupted"
			record["result"] = "interrupted"
			record["interrupted_by_origin_id"] = "other-origin"
		},
		"missing process limit": func(report *pfReport) {
			dropFirstPFRecord(report, "record_type", "process_limit")
		},
		"soft process limit without hard cgroup": func(report *pfReport) {
			firstPFRecord(report, "record_type", "process_limit")["memory_max_bytes"] = float64(0)
		},
		"missing disk boundary": func(report *pfReport) {
			dropFirstPFRecord(report, "record_type", "disk_boundary_marker")
		},
		"disk response replaces Run identity": func(report *pfReport) {
			for index := range report.Disk {
				if report.Disk[index].StageID != "" {
					report.Disk[index].RunID = "run-b"
					break
				}
			}
		},
		"fault duplicates Run identity": func(report *pfReport) {
			report.Faults[0].RunIDsBefore[1] = report.Faults[0].RunIDsBefore[0]
			report.Faults[0].RunIDsAfter[1] = report.Faults[0].RunIDsAfter[0]
		},
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

func firstPFRecord(report *pfReport, key, value string) map[string]any {
	for _, record := range report.Observer {
		if record[key] == value {
			return record
		}
	}
	panic("PF fixture record not found: " + key + "=" + value)
}

func dropFirstPFRecord(report *pfReport, key, value string) {
	for index, record := range report.Observer {
		if record[key] == value {
			report.Observer = append(report.Observer[:index], report.Observer[index+1:]...)
			return
		}
	}
	panic("PF fixture record not found: " + key + "=" + value)
}

func TestPFBaselineRequiresCompleteKeysButAllowsNewRevision(t *testing.T) {
	baseline := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: "release", Result: "PASS", Revision: "old",
		Environment: map[string]string{"environment_fingerprint": "machine", "statistics_version": pfStatisticsVersion, "image_digest": "old-image"},
		Binaries:    pfBinaryDigestFixture(),
		Fixture:     map[string]any{"generator_sha256": "fixture"}, Statistics: map[string]int64{"T_queue_p50": 100, "T_queue_max": 200},
		Baseline: map[string]string{"owner_approved": "true"},
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	writePerfReport(t, path, &baseline)
	t.Setenv("PF01_BASELINE_REPORT", path)
	current := cloneReport(baseline)
	current.Result, current.Revision, current.Environment["image_digest"], current.Baseline = "", "new", "new-image", nil
	if result := releasePFResult(&current); result != "FAIL" {
		t.Fatalf("different image digest baseline comparison = %s", result)
	}
	current.Environment["image_digest"] = "old-image"
	if result := releasePFResult(&current); result != "PASS" {
		t.Fatalf("same environment across implementation revisions = %s", result)
	}
	delete(current.Statistics, "T_queue_max")
	if result := releasePFResult(&current); result != "FAIL" {
		t.Fatalf("sparse statistics baseline comparison = %s", result)
	}
}

func validatorFixture() pfReport {
	const projectID, dataDirID = "project", "root-concurrent"
	tasks := []string{"source-a", "source-b", "source-c", "source-d", "integration-b", "integration-c", "integration-d"}
	runs := []string{"run-a", "run-b", "run-c", "run-d", "run-ib", "run-ic", "run-id"}
	facts := make([]pfRunFact, len(tasks))
	for index := range tasks {
		role := []string{"source", "integration"}[index/4]
		facts[index] = pfRunFact{TaskID: tasks[index], RunID: runs[index], Role: role, TerminalState: "exited",
			TerminalObservedUnixNS: int64(index + 1), ContainerID: "container-" + runs[index],
			ContainerAbsentNS: 1, CleanupNS: 2, ContainerAbsent: true, ResourcesAbsent: true}
	}
	report := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: "smoke", Statistics: map[string]int64{}, Resources: map[string]int64{},
		Binaries: pfBinaryDigestFixture(),
		Samples: []pfSample{{ID: "concurrent-01-wave-01", BatchID: "concurrent-01", Parallelism: 4,
			WaveNS: 1, WorkNS: 1, TaskIDs: tasks[:4], RunIDs: runs[:4], IntegrationTaskIDs: tasks[4:], IntegrationRunIDs: runs[4:],
			QueueNS: []int64{1, 1, 1, 1}, FanoutNS: 1, DirectCASNS: 1, IntegrationNS: []int64{1, 1, 1}, Integrations3NS: 1, DiskBytes: 1, RunFacts: facts}},
		Faults: []pfFaultRow{validFault("live", 1, 4), validFault("capture", 1, 1), validFault("cas", 1, 1)},
	}
	report.ObjectCounts = append(report.ObjectCounts, pfObjectCounts{SampleID: "concurrent-01", FreshDataDirID: dataDirID, ProjectID: projectID})
	for index := range report.Faults {
		report.ObjectCounts = append(report.ObjectCounts, report.Faults[index].Counts)
	}
	for _, counts := range report.ObjectCounts {
		report.Disk = append(report.Disk,
			pfDiskSample{SampleID: counts.SampleID, DataDirID: counts.FreshDataDirID, Boundary: "daemon_ready", ObservedUnixNS: 1, Bytes: 1},
			pfDiskSample{SampleID: counts.SampleID, DataDirID: counts.FreshDataDirID, Boundary: "gc_complete", ObservedUnixNS: 2, Bytes: 1})
	}
	point := func(id, request, task, run, message string, offset int64) map[string]any {
		return map[string]any{"schema_version": float64(1), "record_type": "point", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID,
			"project_id": projectID, "request_id": request, "task_id": task, "run_id": run, "message_id": message, "point_id": id, "mono_offset_ns": float64(offset), "result": "success"}
	}
	for index := 0; index < 200; index++ {
		task, run, request := tasks[index%4], runs[index%4], "p5-progress-"+strconv.Itoa(index)
		report.Observer = append(report.Observer,
			map[string]any{"schema_version": float64(1), "record_type": "client", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID, "project_id": projectID, "request_id": request, "task_id": task, "run_id": run, "operation": "post progress", "duration_ns": float64(1), "result": "success"},
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
		indices := []int{0, 1, 2, 3, 4, 5, 6}
		if stage == "git.advance.update_ref" {
			indices = []int{0, 4, 5, 6}
		}
		for _, index := range indices {
			start, end := pfStageRecord("stage_start", stage, tasks[index], runs[index]), pfStageRecord("stage", stage, tasks[index], runs[index])
			report.Observer = append(report.Observer,
				start, end,
			)
			if stage == "git.capture.freeze" || stage == "git.capture.handoff" || stage == "git.capture.import" {
				for _, boundary := range []string{"start", "end"} {
					marker := pfDiskBoundaryRecord(start, boundary)
					report.Observer = append(report.Observer, marker)
					report.Disk = append(report.Disk, pfDiskBoundarySample(marker))
				}
			}
		}
	}
	for index := range runs {
		report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "runtime_limit", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID, "project_id": projectID,
			"task_id": tasks[index], "run_id": runs[index], "runtime_role": "agent", "memory_bytes": float64(512 << 20), "nano_cpus": float64(1_000_000_000), "pids_limit": float64(256)})
	}
	for index := range runs {
		report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "runtime_limit", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID, "project_id": projectID,
			"task_id": tasks[index], "run_id": runs[index], "runtime_role": "git_capture", "memory_bytes": float64(128 << 20), "nano_cpus": float64(1_000_000_000), "pids_limit": float64(64)})
	}
	report.Observer = append(report.Observer, pfProcessLimitRecord("origin", "concurrent-01", dataDirID),
		map[string]any{"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID, "mono_offset_ns": float64(1), "rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1)},
		map[string]any{"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": dataDirID, "mono_offset_ns": float64(100000001), "rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1)})
	for _, fault := range report.Faults {
		origin := "origin-" + fault.Kind
		report.Observer = append(report.Observer, pfProcessLimitRecord(origin, fault.SampleID, fault.FreshDataDirID),
			map[string]any{"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": origin, "sample_id": fault.SampleID, "data_dir_id": fault.FreshDataDirID, "mono_offset_ns": float64(1), "rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1)})
	}
	return report
}

func pfBinaryDigestFixture() map[string]string {
	return map[string]string{
		"coordplane": strings.Repeat("a", 64), "coordlink": strings.Repeat("b", 64),
		"coordplane-git-helper": strings.Repeat("c", 64),
	}
}

func pfStageRecord(kind, stage, task, run string) map[string]any {
	record := map[string]any{
		"schema_version": float64(1),
		"record_type":    kind, "daemon_origin_id": "origin", "sample_id": "concurrent-01", "data_dir_id": "root-concurrent", "project_id": "project",
		"task_id": task, "run_id": run, "operation_id": "operation-" + run, "stage_id": stage,
		"stage_key_sha256": stage + run, "attempt_index": float64(0),
		"start_unix_ns": float64(1),
	}
	if stage == "git.clone.lock_wait" || stage == "git.clone.prepare" {
		record["run_id"], record["operation_id"] = nil, nil
	}
	if kind == "stage_start" {
		record["start_offset_ns"] = float64(1)
	} else {
		record["duration_ns"], record["result"] = float64(1), "success"
	}
	return record
}

func pfDiskBoundaryRecord(stage map[string]any, boundary string) map[string]any {
	record := maps.Clone(stage)
	record["record_type"], record["boundary"], record["marker_unix_ns"] = "disk_boundary_marker", boundary, float64(1)
	delete(record, "start_offset_ns")
	return record
}

func pfDiskBoundarySample(marker map[string]any) pfDiskSample {
	return diskSampleFromMarker(marker, 2, 1, "")
}

func pfProcessLimitRecord(origin, sample, dataDir string) map[string]any {
	return map[string]any{
		"schema_version": float64(1), "record_type": "process_limit", "daemon_origin_id": origin, "sample_id": sample, "data_dir_id": dataDir,
		"daemon_cgroup_id": "cgroup-" + origin, "cgroup_path": "/", "cpu_quota_us": float64(300000), "cpu_period_us": float64(100000),
		"memory_max_bytes": float64(384 << 20), "gomaxprocs": float64(3), "go_memory_limit_bytes": float64(384 << 20),
	}
}

func validFault(kind string, index, runs int) pfFaultRow {
	ids := []string{kind + "a", kind + "b", kind + "c", kind + "d"}[:runs]
	row := pfFaultRow{SampleID: "fault-" + kind, Kind: kind, Index: index, FreshDataDirID: "root-" + kind, RecoveryNS: 1,
		RunIDsBefore: ids, RunIDsAfter: append([]string(nil), ids...), PreRestart: "durable", FinalSHA: "sha", GitFSCK: "pass", Cleanup: "absent", Result: "PASS"}
	row.Counts.SampleID, row.Counts.FreshDataDirID, row.Counts.ProjectID = row.SampleID, row.FreshDataDirID, "project-"+kind
	row.Counts.PeerMessages, row.Counts.PeerAcknowledged, row.Counts.PeerAckEvent = 4, 4, 4
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
