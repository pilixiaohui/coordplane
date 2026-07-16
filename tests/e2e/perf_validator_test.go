//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPFValidatorRejectsMissingDuplicateAndNegativeEvidence(t *testing.T) {
	valid := validatorFixture()
	if err := validateObserver(&valid); err != nil {
		t.Fatalf("valid raw ledger rejected: %v", err)
	}
	tests := map[string]func(*pfReport){
		"missing commit":               func(report *pfReport) { report.Observer = append(report.Observer[:2], report.Observer[3:]...) },
		"missing formal binary digest": func(report *pfReport) { delete(report.Binaries, "coordplane-git-helper") },
		"duplicate stage": func(report *pfReport) {
			record := firstPFRecord(report, "record_type", "stage")
			report.Observer = append(report.Observer, record)
		},
		"negative duration":               func(report *pfReport) { report.Samples[0].RunFacts[0].CleanupNS = -1 },
		"illegal terminal":                func(report *pfReport) { report.Samples[0].RunFacts[0].TerminalState = "active" },
		"cleanup before container absent": func(report *pfReport) { report.Samples[0].RunFacts[0].CleanupNS = 0 },
		"zero capture fault signature": func(report *pfReport) {
			report.Faults[1].Counts.PeerMessages = 0
			report.Faults[1].Counts.PeerAcknowledged = 0
			report.Faults[1].Counts.PeerAckEvent = 0
		},
		"progress crosses origin": func(report *pfReport) {
			firstPFRecord(report, "point_id", "core.progress.committed")["daemon_origin_id"] = "other-origin"
		},
		"interrupted has duration": func(report *pfReport) {
			record := firstPFRecord(report, "record_type", "stage")
			record["record_type"] = "stage_interrupted"
			record["result"] = "interrupted"
			record["interrupted_by_origin_id"] = "other-origin"
		},
		"soft process limit without hard cgroup": func(report *pfReport) {
			firstPFRecord(report, "record_type", "process_limit")["memory_max_bytes"] = float64(0)
		},
		"missing disk boundary": func(report *pfReport) { dropFirstPFRecord(report, "record_type", "disk_boundary_marker") },
		"disk interval overrun": func(report *pfReport) {
			for index := range report.Disk {
				if report.Disk[index].Boundary == "interval" {
					report.Disk[index].EndedUnixNS += int64(2 * time.Second)
					report.Disk[index].ObservedUnixNS = report.Disk[index].EndedUnixNS
					report.Disk[index].OverrunNS = int64(time.Second)
					break
				}
			}
		},
		"complete scored wave deleted": func(report *pfReport) {
			dropPFSampleGroup(report, "concurrent-01-wave-02")
		},
		"complete warmup deleted": func(report *pfReport) { dropPFSampleGroup(report, "concurrent-01-warmup") },
		"complete serial group deleted": func(report *pfReport) {
			dropPFSampleGroup(report, "serial-01-wave-01")
		},
		"complete fault group deleted": func(report *pfReport) { dropPFFaultGroup(report, "fault-live-01") },
		"complete durable group deleted": func(report *pfReport) {
			report.ObjectCounts = slices.DeleteFunc(report.ObjectCounts, func(counts pfObjectCounts) bool { return counts.SampleID == "serial-01" })
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
		"fault loses complete stage group": func(report *pfReport) {
			dropFirstPFRecord(report, "stage_id", "git.capture.ref", "sample_id", "fault-capture-01")
			dropFirstPFRecord(report, "stage_id", "git.capture.ref", "sample_id", "fault-capture-01")
		},
		"fault loses complete origin group": func(report *pfReport) {
			report.Observer = slices.DeleteFunc(report.Observer, func(record map[string]any) bool {
				return record["daemon_origin_id"] == "origin-fault-capture-01-before"
			})
		},
		"fault names fake replacement origin": func(report *pfReport) {
			firstPFRecord(report, "record_type", "stage_interrupted")["interrupted_by_origin_id"] = "fake-origin"
		},
		"fault adds unknown complete stage group": func(report *pfReport) {
			start := maps.Clone(firstPFRecord(report, "record_type", "stage_start"))
			start["task_id"], start["run_id"], start["stage_key_sha256"] = "unknown-task", "unknown-run", "unknown-key"
			end := maps.Clone(start)
			end["record_type"], end["duration_ns"], end["result"] = "stage", float64(1), "success"
			report.Observer = append(report.Observer, start, end)
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

func TestPFValidatorRejectsCompleteSoakWaveDeletion(t *testing.T) {
	report := validatorFixtureProfile("release")
	if err := validateObserver(&report); err != nil {
		t.Fatalf("valid release ledger rejected: %v", err)
	}
	dropPFSampleGroup(&report, "concurrent-01-soak-08")
	if err := validateObserver(&report); err == nil {
		t.Fatal("release ledger without a complete soak wave passed")
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

func dropFirstPFRecord(report *pfReport, key, value string, identity ...string) {
	for index, record := range report.Observer {
		if record[key] == value && (len(identity) == 0 || record[identity[0]] == identity[1]) {
			report.Observer = append(report.Observer[:index], report.Observer[index+1:]...)
			return
		}
	}
	panic("PF fixture record not found: " + key + "=" + value)
}

func dropPFSampleGroup(report *pfReport, sampleID string) {
	tasks := map[string]bool{}
	for _, sample := range report.Samples {
		if sample.ID == sampleID {
			for _, task := range append(sample.TaskIDs, sample.IntegrationTaskIDs...) {
				tasks[task] = true
			}
		}
	}
	report.Samples = slices.DeleteFunc(report.Samples, func(sample pfSample) bool { return sample.ID == sampleID })
	report.Observer = slices.DeleteFunc(report.Observer, func(record map[string]any) bool { return tasks[textField(record, "task_id")] })
	report.Disk = slices.DeleteFunc(report.Disk, func(sample pfDiskSample) bool { return tasks[sample.TaskID] })
}

func dropPFFaultGroup(report *pfReport, sampleID string) {
	report.Faults = slices.DeleteFunc(report.Faults, func(row pfFaultRow) bool { return row.SampleID == sampleID })
	report.ObjectCounts = slices.DeleteFunc(report.ObjectCounts, func(counts pfObjectCounts) bool { return counts.SampleID == sampleID })
	report.Observer = slices.DeleteFunc(report.Observer, func(record map[string]any) bool { return record["sample_id"] == sampleID })
	report.Disk = slices.DeleteFunc(report.Disk, func(sample pfDiskSample) bool { return sample.SampleID == sampleID })
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
	return validatorFixtureProfile("smoke")
}

func validatorFixtureProfile(profile string) pfReport {
	digest := "sha256:" + strings.Repeat("d", 64)
	manifest, _, _ := fixedPFProfile(profile, digest)
	report := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: profile, Statistics: map[string]int64{}, Resources: map[string]int64{}, Manifest: manifest,
		Environment: map[string]string{"image_digest": digest}, Binaries: pfBinaryDigestFixture(),
	}
	faultIndex := map[string]int{}
	for _, want := range manifest.Samples {
		if strings.HasPrefix(want.Class, "fault_") {
			kind := strings.TrimPrefix(want.Class, "fault_")
			faultIndex[kind]++
			report.Faults = append(report.Faults, validFault(kind, faultIndex[kind], map[string]int{"fault_live": 4, "fault_capture": 1, "fault_cas": 1}[want.Class]))
			continue
		}
		tasks, runs := make([]string, want.Tasks), make([]string, want.Runs)
		for index := range tasks {
			tasks[index], runs[index] = want.ID+"-task-"+strconv.Itoa(index), want.ID+"-run-"+strconv.Itoa(index)
		}
		sample := pfSample{ID: want.ID, BatchID: want.BatchID, Parallelism: want.Parallelism, Warmup: strings.HasSuffix(want.Class, "_warmup"), Soak: want.Class == "soak",
			TaskIDs: tasks[:4], RunIDs: runs[:4], IntegrationTaskIDs: tasks[4:], IntegrationRunIDs: runs[4:]}
		if want.Class == "scored" {
			sample.WaveNS, sample.WorkNS, sample.QueueNS = 1, 1, []int64{1, 1, 1, 1}
			sample.FanoutNS, sample.DirectCASNS, sample.IntegrationNS, sample.Integrations3NS, sample.DiskBytes = 1, 1, []int64{1, 1, 1}, 1, 1
			for index := range tasks {
				role := []string{"source", "integration"}[index/4]
				sample.RunFacts = append(sample.RunFacts, pfRunFact{TaskID: tasks[index], RunID: runs[index], Role: role, TerminalState: "exited", TerminalObservedUnixNS: int64(index + 1), ContainerID: "container-" + runs[index], ContainerAbsentNS: 1, CleanupNS: 2, ContainerAbsent: true, ResourcesAbsent: true})
			}
			appendPFScoredEvidence(&report, sample, "root-"+want.BatchID, "project-"+want.BatchID, "origin-"+want.BatchID)
		}
		report.Samples = append(report.Samples, sample)
	}
	for _, want := range manifest.Durable {
		counts := pfObjectCounts{SampleID: want.SampleID, FreshDataDirID: "root-" + want.SampleID, ProjectID: "project-" + want.SampleID,
			Tasks: want.Tasks, Runs: want.Runs, Messages: want.Messages, PeerMessages: want.PeerMessages, PeerAcknowledged: want.PeerMessages, PeerAckEvent: want.PeerMessages}
		for index := range report.Faults {
			if report.Faults[index].SampleID == want.SampleID {
				counts = report.Faults[index].Counts
			}
		}
		report.ObjectCounts = append(report.ObjectCounts, counts)
		for index, boundary := range []string{"daemon_ready", "interval", "gc_complete"} {
			observed := int64(index + 1)
			report.Disk = append(report.Disk, pfDiskSample{SampleID: counts.SampleID, DataDirID: counts.FreshDataDirID, Boundary: boundary,
				ObservedUnixNS: observed, ScheduledUnixNS: observed, StartedUnixNS: observed, EndedUnixNS: observed, Bytes: 1})
		}
		origins := []string{"origin-" + want.SampleID}
		if want.Origins == 2 {
			origins = []string{"origin-" + want.SampleID + "-before", "origin-" + want.SampleID + "-after"}
		}
		appendPFOrigins(&report, want.SampleID, counts.FreshDataDirID, origins...)
	}
	for _, fault := range report.Faults {
		if fault.Kind != "capture" {
			continue
		}
		origins := []string{"origin-" + fault.SampleID + "-before", "origin-" + fault.SampleID + "-after"}
		start := pfStageRecord("stage_start", "git.capture.ref", fault.TaskIDs[0], fault.RunIDs[0])
		end := pfStageRecord("stage_interrupted", "git.capture.ref", fault.TaskIDs[0], fault.RunIDs[0])
		for _, record := range []map[string]any{start, end} {
			record["sample_id"], record["data_dir_id"], record["project_id"], record["daemon_origin_id"] = fault.SampleID, fault.FreshDataDirID, fault.Counts.ProjectID, origins[0]
		}
		end["result"], end["interrupted_by_origin_id"] = "interrupted", origins[1]
		delete(end, "duration_ns")
		report.Observer = append(report.Observer, start, end)
	}
	for index := range report.Samples {
		if len(report.Samples[index].RunFacts) > 0 {
			report.Samples[0], report.Samples[index] = report.Samples[index], report.Samples[0]
			break
		}
	}
	return report
}

func appendPFScoredEvidence(report *pfReport, sample pfSample, dataDirID, projectID, origin string) {
	point := func(id, request, task, run, message string, offset int64) map[string]any {
		return map[string]any{"schema_version": float64(1), "record_type": "point", "daemon_origin_id": origin, "sample_id": sample.BatchID, "data_dir_id": dataDirID,
			"project_id": projectID, "request_id": request, "task_id": task, "run_id": run, "message_id": message, "point_id": id, "mono_offset_ns": float64(offset), "result": "success"}
	}
	tasks := append(append([]string(nil), sample.TaskIDs...), sample.IntegrationTaskIDs...)
	runs := append(append([]string(nil), sample.RunIDs...), sample.IntegrationRunIDs...)
	for index := 0; index < 200; index++ {
		task, run, request := tasks[index%4], runs[index%4], "p5-progress-"+sample.ID+"-"+strconv.Itoa(index)
		report.Observer = append(report.Observer,
			map[string]any{"schema_version": float64(1), "record_type": "client", "daemon_origin_id": origin, "sample_id": sample.BatchID, "data_dir_id": dataDirID, "project_id": projectID, "request_id": request, "task_id": task, "run_id": run, "operation": "post progress", "duration_ns": float64(1), "result": "success"},
			point("api.progress.received", request, task, run, "", int64(index+1)), point("core.progress.committed", request, task, run, "", int64(index+201)))
	}
	for index := range tasks {
		report.Observer = append(report.Observer, point("core.outcome.accepted_commit", "outcome-"+tasks[index], tasks[index], runs[index], "", 1),
			point("git.capture.submitted_commit", "submit-"+tasks[index], tasks[index], runs[index], "", 2))
	}
	for index := 0; index < 10; index++ {
		message := sample.ID + "-message-" + strconv.Itoa(index)
		report.Observer = append(report.Observer, point("core.message.created_commit", "pf01-peer-"+message, tasks[0], runs[0], message, 1),
			point("core.message.acknowledged_commit", "ack-"+message, tasks[3], runs[3], message, 2))
	}
	for _, stage := range []string{"git.clone.lock_wait", "git.clone.prepare", "runtime.container.create_start", "git.capture.freeze", "git.capture.handoff", "git.capture.lock_wait", "git.capture.import", "git.capture.fsck", "git.capture.ref", "git.advance.lock_wait", "git.advance.ancestry", "git.advance.update_ref", "runtime.cleanup"} {
		indices := []int{0, 1, 2, 3, 4, 5, 6}
		if stage == "git.advance.update_ref" {
			indices = []int{0, 4, 5, 6}
		}
		for _, index := range indices {
			start := pfStageRecord("stage_start", stage, tasks[index], runs[index])
			end := pfStageRecord("stage", stage, tasks[index], runs[index])
			for _, record := range []map[string]any{start, end} {
				record["daemon_origin_id"], record["sample_id"], record["data_dir_id"], record["project_id"] = origin, sample.BatchID, dataDirID, projectID
			}
			report.Observer = append(report.Observer, start, end)
			if stage == "git.capture.freeze" || stage == "git.capture.handoff" || stage == "git.capture.import" {
				for _, boundary := range []string{"start", "end"} {
					marker := pfDiskBoundaryRecord(start, boundary)
					report.Observer = append(report.Observer, marker)
					report.Disk = append(report.Disk, diskSampleFromMarker(marker, 2, 1, ""))
				}
			}
		}
	}
	for index := range runs {
		report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "runtime_limit", "daemon_origin_id": origin, "sample_id": sample.BatchID, "data_dir_id": dataDirID, "project_id": projectID,
			"task_id": tasks[index], "run_id": runs[index], "runtime_role": "agent", "memory_bytes": float64(512 << 20), "nano_cpus": float64(1_000_000_000), "pids_limit": float64(256)})
	}
	for index := range runs {
		report.Observer = append(report.Observer, map[string]any{"schema_version": float64(1), "record_type": "runtime_limit", "daemon_origin_id": origin, "sample_id": sample.BatchID, "data_dir_id": dataDirID, "project_id": projectID,
			"task_id": tasks[index], "run_id": runs[index], "runtime_role": "git_capture", "memory_bytes": float64(128 << 20), "nano_cpus": float64(1_000_000_000), "pids_limit": float64(64)})
	}
}

func pfBinaryDigestFixture() map[string]string {
	return map[string]string{"coordplane": strings.Repeat("a", 64), "coordlink": strings.Repeat("b", 64), "coordplane-git-helper": strings.Repeat("c", 64)}
}

func appendPFOrigins(report *pfReport, sample, dataDir string, origins ...string) {
	for _, origin := range origins {
		report.Observer = append(report.Observer, pfProcessLimitRecord(origin, sample, dataDir), map[string]any{
			"schema_version": float64(1), "record_type": "resource", "daemon_origin_id": origin,
			"sample_id": sample, "data_dir_id": dataDir, "mono_offset_ns": float64(1),
			"rss_bytes": float64(1), "goroutines": float64(1), "open_fds": float64(1),
		})
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

func pfProcessLimitRecord(origin, sample, dataDir string) map[string]any {
	return map[string]any{
		"schema_version": float64(1), "record_type": "process_limit", "daemon_origin_id": origin, "sample_id": sample, "data_dir_id": dataDir,
		"daemon_cgroup_id": "cgroup-" + origin, "cgroup_path": "/", "cpu_quota_us": float64(300000), "cpu_period_us": float64(100000),
		"memory_max_bytes": float64(384 << 20), "gomaxprocs": float64(3), "go_memory_limit_bytes": float64(384 << 20),
	}
}

func validFault(kind string, index, runs int) pfFaultRow {
	id := fmt.Sprintf("fault-%s-%02d", kind, index)
	total := runs + map[string]int{"live": 3}[kind]
	tasks, ids := make([]string, total), make([]string, total)
	for position := range ids {
		tasks[position], ids[position] = id+"-task-"+strconv.Itoa(position), id+"-run-"+strconv.Itoa(position)
	}
	row := pfFaultRow{SampleID: id, Kind: kind, Index: index, FreshDataDirID: "root-" + id, RecoveryNS: 1,
		TaskIDs: tasks, RunIDs: ids,
		RunIDsBefore: ids[:runs], RunIDsAfter: append([]string(nil), ids[:runs]...), PreRestart: "durable", FinalSHA: "sha", GitFSCK: "pass", Cleanup: "absent", Result: "PASS"}
	row.Counts.SampleID, row.Counts.FreshDataDirID, row.Counts.ProjectID = row.SampleID, row.FreshDataDirID, "project-"+kind
	row.Counts.Tasks, row.Counts.Runs = total, total
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
