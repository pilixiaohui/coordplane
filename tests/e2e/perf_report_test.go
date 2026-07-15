//go:build e2e

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type pfReferenceManifest struct {
	SchemaVersion          int    `json:"schema_version"`
	Approved               bool   `json:"approved"`
	RunnerID               string `json:"runner_id"`
	EnvironmentFingerprint string `json:"environment_fingerprint"`
	LogicalCPUs            int    `json:"logical_cpus"`
	MemoryLimitBytes       int64  `json:"memory_limit_bytes"`
	DiskLimitBytes         int64  `json:"disk_limit_bytes"`
	Filesystem             string `json:"filesystem"`
	DockerStorageDriver    string `json:"docker_storage_driver"`
}

type pfIdleSample struct {
	BatchID  string  `json:"batch_id"`
	RSSBytes []int64 `json:"rss_bytes"`
	CPUNS    int64   `json:"cpu_time_ns"`
}

func measurePFIdle(t *testing.T, batch *pfBatch) pfIdleSample {
	t.Helper()
	time.Sleep(30 * time.Second)
	pid := batch.daemon.command.Process.Pid
	_, startedTicks := readPFProcess(t, pid)
	sample := pfIdleSample{BatchID: batch.id}
	for index := 0; index < 60; index++ {
		rss, _ := readPFProcess(t, pid)
		sample.RSSBytes = append(sample.RSSBytes, rss)
		time.Sleep(time.Second)
	}
	_, endedTicks := readPFProcess(t, pid)
	ticks, err := strconv.ParseInt(commandText(batch.ctx, "getconf", "CLK_TCK"), 10, 64)
	if err != nil || ticks <= 0 || endedTicks < startedTicks {
		t.Fatal("invalid daemon CPU clock sample")
	}
	sample.CPUNS = (endedTicks - startedTicks) * int64(time.Second) / ticks
	return sample
}

func readPFProcess(t *testing.T, pid int) (int64, int64) {
	t.Helper()
	statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		t.Fatal(err)
	}
	memory := strings.Fields(string(statm))
	pages, err := strconv.ParseInt(memory[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
	userTicks, userErr := strconv.ParseInt(fields[11], 10, 64)
	systemTicks, systemErr := strconv.ParseInt(fields[12], 10, 64)
	if userErr != nil || systemErr != nil {
		t.Fatal("invalid daemon CPU stat")
	}
	return pages * int64(os.Getpagesize()), userTicks + systemTicks
}

func validateReferenceEnvironment(ctx context.Context, root, image, revision string, environment map[string]string) string {
	if runtime.GOOS != "linux" {
		return "reference runner must be native Linux"
	}
	manifestPath := strings.TrimSpace(os.Getenv("PF01_REFERENCE_MANIFEST"))
	if !filepath.IsAbs(manifestPath) {
		return "PF01_REFERENCE_MANIFEST must name an absolute owner-approved manifest"
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return "read reference manifest: " + err.Error()
	}
	var manifest pfReferenceManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.SchemaVersion != 1 || !manifest.Approved {
		return "reference manifest is invalid or not owner-approved"
	}
	cpuCount := runtime.NumCPU()
	memoryLimit, err := cgroupInt64("/sys/fs/cgroup/memory.max")
	if err != nil {
		return err.Error()
	}
	filesystem := commandText(ctx, "stat", "-f", "-c", "%T", root)
	driver := environment["docker_storage_driver"]
	dockerRoot := commandText(ctx, "docker", "info", "--format", "{{.DockerRootDir}}")
	if filesystem != "ext2/ext3" && filesystem != "ext4" && filesystem != "xfs" {
		return "data filesystem must be ext4 or xfs"
	}
	if driver != "overlay2" || dockerRoot == "" {
		return "Docker must be local overlay2"
	}
	if os.Getenv("DOCKER_HOST") != "" || commandText(ctx, "docker", "ps", "-q") != "" {
		return "remote Docker or unrelated running containers are not allowed"
	}
	if !sameDevice(root, os.TempDir()) || !sameDevice(root, dockerRoot) {
		return "data, temp, and Docker roots must share one local device"
	}
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(root, &statfs); err != nil {
		return "inspect reference disk: " + err.Error()
	}
	diskLimit := int64(statfs.Blocks) * statfs.Bsize
	runnerID := referenceRunnerID()
	facts := map[string]string{
		"runner_id": runnerID, "logical_cpus": strconv.Itoa(cpuCount),
		"memory_limit_bytes": strconv.FormatInt(memoryLimit, 10), "disk_limit_bytes": strconv.FormatInt(diskLimit, 10),
		"filesystem": filesystem, "docker_storage_driver": driver, "image_digest": environment["image_digest"],
		"revision": revision,
	}
	fingerprint := fingerprintMap(facts)
	environment["runner_id"] = runnerID
	environment["logical_cpus"] = facts["logical_cpus"]
	environment["memory_limit_bytes"] = facts["memory_limit_bytes"]
	environment["disk_limit_bytes"] = facts["disk_limit_bytes"]
	environment["filesystem"] = filesystem
	environment["environment_fingerprint"] = fingerprint
	if cpuCount != 8 || memoryLimit != 16<<30 || diskLimit != 10<<30 || manifest.LogicalCPUs != cpuCount ||
		manifest.MemoryLimitBytes != memoryLimit || manifest.DiskLimitBytes != diskLimit || manifest.Filesystem != filesystem ||
		manifest.DockerStorageDriver != driver || manifest.RunnerID != runnerID || manifest.EnvironmentFingerprint != fingerprint {
		return "actual cgroup/storage/image fingerprint does not match the approved 8 CPU / 16 GiB / 10 GiB reference manifest"
	}
	return ""
}

func validateObserver(report *pfReport) error {
	if report.Resources == nil {
		report.Resources = map[string]int64{}
	}
	allowedStages := map[string]bool{}
	for _, id := range []string{
		"git.clone.lock_wait", "git.clone.prepare", "runtime.container.create_start", "git.capture.freeze",
		"git.capture.handoff", "git.capture.lock_wait", "git.capture.import", "git.capture.fsck", "git.capture.ref",
		"git.advance.lock_wait", "git.advance.ancestry", "git.advance.update_ref", "runtime.cleanup",
	} {
		allowedStages[id] = true
	}
	stageSeen := map[string]int{}
	api, committed, clients := map[string]map[string]any{}, map[string]map[string]any{}, map[string]map[string]any{}
	report.RawMetrics = map[string][]int64{}
	taskWave := map[string]string{}
	scoredTasks, scoredWaves := map[string]bool{}, map[string]bool{}
	for _, sample := range report.Samples {
		allTasks := append(append([]string(nil), sample.TaskIDs...), sample.IntegrationTaskIDs...)
		for _, taskID := range allTasks {
			taskWave[taskID] = sample.ID
		}
		if !sample.Warmup && !sample.Soak && sample.Parallelism == 4 {
			scoredWaves[sample.ID] = true
			for _, taskID := range allTasks {
				scoredTasks[taskID] = true
			}
			report.RawMetrics["T_queue"] = append(report.RawMetrics["T_queue"], sample.QueueNS...)
			report.RawMetrics["T_fanout4"] = append(report.RawMetrics["T_fanout4"], sample.FanoutNS)
			report.RawMetrics["T_cas"] = append(report.RawMetrics["T_cas"], sample.DirectCASNS)
			report.RawMetrics["T_integration"] = append(report.RawMetrics["T_integration"], sample.IntegrationNS...)
			report.RawMetrics["T_integrations3"] = append(report.RawMetrics["T_integrations3"], sample.Integrations3NS)
			report.RawMetrics["T_work4"] = append(report.RawMetrics["T_work4"], sample.WorkNS)
			report.RawMetrics["T_container_absent"] = append(report.RawMetrics["T_container_absent"], sample.ContainerAbsentNS)
			report.RawMetrics["T_cleanup"] = append(report.RawMetrics["T_cleanup"], sample.CleanupNS)
		}
		report.RawMetrics["status"] = append(report.RawMetrics["status"], sample.StatusNS...)
		report.RawMetrics["disk_bytes"] = append(report.RawMetrics["disk_bytes"], sample.DiskBytes)
		if sample.StableRSSBytes > 0 {
			report.RawMetrics["soak_rss_bytes"] = append(report.RawMetrics["soak_rss_bytes"], sample.StableRSSBytes)
			report.RawMetrics["soak_goroutines"] = append(report.RawMetrics["soak_goroutines"], sample.StableGoroutines)
			report.RawMetrics["soak_open_fds"] = append(report.RawMetrics["soak_open_fds"], sample.StableFDs)
		}
	}
	for _, idle := range report.Idle {
		report.RawMetrics["idle_rss_bytes"] = append(report.RawMetrics["idle_rss_bytes"], idle.RSSBytes...)
		report.RawMetrics["idle_cpu_time"] = append(report.RawMetrics["idle_cpu_time"], idle.CPUNS)
	}
	type bounds struct{ start, end int64 }
	bursts := map[string]bounds{}
	outcomes, submissions := map[string]map[string]any{}, map[string]map[string]any{}
	createdMessages, acknowledgedMessages := map[string]map[string]any{}, map[string]map[string]any{}
	cloneWork := map[string]int64{}
	resourceCount := 0
	for _, record := range report.Observer {
		kind, origin := textField(record, "record_type"), textField(record, "daemon_origin_id")
		if kind == "invalid" || origin == "" || intField(record, "schema_version") != 1 {
			return fmt.Errorf("observer contains invalid schema record")
		}
		switch kind {
		case "client":
			duration := intField(record, "duration_ns")
			if duration < 0 {
				return fmt.Errorf("client duration is negative")
			}
			if strings.HasSuffix(textField(record, "operation"), "progress") && textField(record, "result") == "success" {
				request := textField(record, "request_id")
				if request == "" || clients[request] != nil {
					return fmt.Errorf("progress client identity is missing or duplicate")
				} else {
					clients[request] = record
					if strings.HasPrefix(request, "p5-progress-") && scoredTasks[textField(record, "task_id")] {
						report.RawMetrics["R_progress"] = append(report.RawMetrics["R_progress"], duration)
					}
				}
			}
		case "point":
			request, id := textField(record, "request_id"), textField(record, "point_id")
			normal := normalPFSample(textField(record, "sample_id"))
			if intField(record, "mono_offset_ns") < 0 {
				return fmt.Errorf("point offset is negative")
			}
			if id == "api.progress.received" && textField(record, "result") != "error" {
				if !storeUnique(api, request, record) {
					return fmt.Errorf("api progress identity is missing or duplicate")
				}
			}
			if id == "core.progress.committed" {
				if !storeUnique(committed, request, record) {
					return fmt.Errorf("committed progress identity is missing or duplicate")
				}
			}
			if normal && strings.HasPrefix(request, "p5-progress-") && scoredTasks[textField(record, "task_id")] {
				wave := taskWave[textField(record, "task_id")]
				boundary := bursts[wave]
				if id == "api.progress.received" && textField(record, "result") != "error" && (boundary.start == 0 || intField(record, "mono_offset_ns") < boundary.start) {
					boundary.start = intField(record, "mono_offset_ns")
				}
				if id == "core.progress.committed" && intField(record, "mono_offset_ns") > boundary.end {
					boundary.end = intField(record, "mono_offset_ns")
				}
				bursts[wave] = boundary
			}
			if normal && scoredTasks[textField(record, "task_id")] && id == "core.outcome.accepted_commit" {
				outcomes[textField(record, "run_id")] = record
			}
			if normal && scoredTasks[textField(record, "task_id")] && id == "git.capture.submitted_commit" {
				submissions[textField(record, "run_id")] = record
			}
			if normal && scoredTasks[textField(record, "task_id")] && id == "core.message.created_commit" && strings.HasPrefix(request, "pf01-peer-") {
				createdMessages[textField(record, "message_id")] = record
			}
			if normal && id == "core.message.acknowledged_commit" {
				acknowledgedMessages[textField(record, "message_id")] = record
			}
		case "stage":
			id, duration := textField(record, "stage_id"), intField(record, "duration_ns")
			if !allowedStages[id] || duration < 0 {
				return fmt.Errorf("unknown stage or negative duration")
			} else if normalPFSample(textField(record, "sample_id")) && scoredTasks[textField(record, "task_id")] {
				stageSeen[id]++
				report.RawMetrics["stage."+id] = append(report.RawMetrics["stage."+id], duration)
				if id == "git.clone.prepare" {
					cloneWork[taskWave[textField(record, "task_id")]] += duration
				}
			}
		case "resource":
			resourceCount++
			for _, field := range []string{"rss_bytes", "goroutines", "open_fds"} {
				value := intField(record, field)
				if value < 0 {
					return fmt.Errorf("resource value is negative")
				}
				if value > report.Resources[field+"_max"] {
					report.Resources[field+"_max"] = value
				}
			}
		default:
			return fmt.Errorf("unknown observer record type %s", kind)
		}
	}
	for request, client := range clients {
		if api[request] == nil || committed[request] == nil || textField(api[request], "daemon_origin_id") != textField(committed[request], "daemon_origin_id") ||
			textField(client, "run_id") != textField(committed[request], "run_id") {
			return fmt.Errorf("progress join failed for %s", request)
		}
	}
	for wave, boundary := range bursts {
		if scoredWaves[wave] && boundary.start > 0 && boundary.end > boundary.start {
			duration := boundary.end - boundary.start
			report.RawMetrics["T_progress_burst"] = append(report.RawMetrics["T_progress_burst"], duration)
			report.RawMetrics["progress_ops_per_second_milli"] = append(report.RawMetrics["progress_ops_per_second_milli"], 200_000_000_000_000/duration)
		}
	}
	for runID, start := range outcomes {
		if end := submissions[runID]; end != nil && textField(start, "daemon_origin_id") == textField(end, "daemon_origin_id") {
			report.RawMetrics["T_capture"] = append(report.RawMetrics["T_capture"], intField(end, "mono_offset_ns")-intField(start, "mono_offset_ns"))
		}
	}
	for messageID, start := range createdMessages {
		if end := acknowledgedMessages[messageID]; end != nil && textField(start, "daemon_origin_id") == textField(end, "daemon_origin_id") {
			report.RawMetrics["T_message"] = append(report.RawMetrics["T_message"], intField(end, "mono_offset_ns")-intField(start, "mono_offset_ns"))
		}
	}
	for wave, duration := range cloneWork {
		if scoredWaves[wave] {
			report.RawMetrics["git_clone_work4"] = append(report.RawMetrics["git_clone_work4"], duration)
		}
	}
	if len(clients) == 0 || resourceCount == 0 {
		return fmt.Errorf("observer has no progress or resource samples")
	}
	for id := range allowedStages {
		if stageSeen[id] == 0 {
			return fmt.Errorf("missing stage %s", id)
		}
	}
	for metric, values := range report.RawMetrics {
		if len(values) > 0 {
			report.Statistics[metric+"_p50"] = nearestRank(values, 50)
			report.Statistics[metric+"_p90"] = nearestRank(values, 90)
			report.Statistics[metric+"_p95"] = nearestRank(values, 95)
			report.Statistics[metric+"_p99"] = nearestRank(values, 99)
			report.Statistics[metric+"_max"] = nearestRank(values, 100)
		}
	}
	return nil
}

func validateReleaseThresholds(report *pfReport) error {
	limits := map[string]int64{
		"t_wave_p50": int64(60 * time.Second), "t_wave_p90": int64(90 * time.Second), "t_wave_max": int64(180 * time.Second),
		"R_progress_p95": int64(100 * time.Millisecond), "R_progress_p99": int64(250 * time.Millisecond),
		"stage.runtime.container.create_start_p90": int64(3 * time.Second), "stage.runtime.container.create_start_max": int64(5 * time.Second),
		"stage.runtime.cleanup_max": int64(10 * time.Second),
		"T_message_p95":             int64(time.Second), "T_message_max": int64(2 * time.Second),
		"status_p95": int64(200 * time.Millisecond), "status_p99": int64(500 * time.Millisecond),
		"T_queue_p90": int64(500 * time.Millisecond), "T_queue_max": int64(2 * time.Second),
		"git_clone_work4_p90": int64(8 * time.Second), "git_clone_work4_max": int64(12 * time.Second),
		"T_fanout4_p90": int64(12 * time.Second), "T_fanout4_max": int64(20 * time.Second),
		"T_capture_p90": int64(5 * time.Second), "T_capture_max": int64(10 * time.Second),
		"T_cas_p90": int64(time.Second), "T_cas_max": int64(2 * time.Second),
		"T_integration_p90": int64(15 * time.Second), "T_integration_max": int64(30 * time.Second),
		"T_integrations3_p90": int64(35 * time.Second), "T_integrations3_max": int64(60 * time.Second),
		"T_container_absent_p90": int64(5 * time.Second), "T_cleanup_max": int64(10 * time.Second),
	}
	throughput := report.RawMetrics["progress_ops_per_second_milli"]
	if len(throughput) != 20 || nearestRank(throughput, 50) < 100_000 || nearestRank(throughput, 1) < 50_000 {
		return fmt.Errorf("progress throughput sample count or threshold failed")
	}
	if len(report.RawMetrics["R_progress"]) != 4_000 {
		return fmt.Errorf("R_progress release sample count is not 4,000")
	}
	if len(report.RawMetrics["T_message"]) != 200 || len(report.RawMetrics["status"]) != 1_000 {
		return fmt.Errorf("Message or status release sample count is incomplete")
	}
	for key, limit := range limits {
		report.Thresholds[key] = limit
		if report.Statistics[key] <= 0 || report.Statistics[key] > limit {
			return fmt.Errorf("%s=%d exceeds %d", key, report.Statistics[key], limit)
		}
	}
	for _, row := range report.Faults {
		if row.RecoveryNS > int64(8*time.Second) {
			return fmt.Errorf("%s recovery exceeded 8s", row.SampleID)
		}
	}
	if report.Resources["rss_bytes_max"] > 384<<20 {
		return fmt.Errorf("daemon load RSS exceeded 384 MiB")
	}
	if len(report.Idle) != 4 {
		return fmt.Errorf("idle resource batch count is not 4")
	}
	for _, idle := range report.Idle {
		if nearestRank(idle.RSSBytes, 50) > 128<<20 || idle.CPUNS > int64(1200*time.Millisecond) {
			return fmt.Errorf("%s idle resource threshold failed", idle.BatchID)
		}
	}
	soakRSS, soakGo, soakFD := report.RawMetrics["soak_rss_bytes"], report.RawMetrics["soak_goroutines"], report.RawMetrics["soak_open_fds"]
	if len(soakRSS) != 20 || len(soakGo) != 20 || len(soakFD) != 20 {
		return fmt.Errorf("20-wave soak resource samples are incomplete")
	} else if nearestRank(soakRSS[15:], 50)-nearestRank(soakRSS[:5], 50) > 64<<20 ||
		nearestRank(soakGo[15:], 50)-nearestRank(soakGo[:5], 50) > 16 ||
		nearestRank(soakFD[15:], 50)-nearestRank(soakFD[:5], 50) > 8 || linearSlope(soakRSS) > 2<<20 {
		return fmt.Errorf("20-wave soak leak threshold failed")
	}
	if nearestRank(report.RawMetrics["disk_bytes"], 100) > 1536<<20 {
		return fmt.Errorf("data directory peak exceeded 1.5 GiB")
	}
	return nil
}

func releasePFResult(report *pfReport) string {
	path := strings.TrimSpace(os.Getenv("PF01_BASELINE_REPORT"))
	if path == "" {
		report.Baseline = map[string]string{"status": "BASELINE_BOOTSTRAP", "approval": "owner approval required"}
		return "BASELINE_BOOTSTRAP"
	}
	if !filepath.IsAbs(path) {
		return "FAIL"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "FAIL"
	}
	var baseline pfReport
	if json.Unmarshal(raw, &baseline) != nil || baseline.Result != "PASS" || baseline.Profile != "release" ||
		baseline.Baseline["owner_approved"] != "true" ||
		baseline.Environment["environment_fingerprint"] != report.Environment["environment_fingerprint"] || baseline.Fixture["generator_sha256"] != report.Fixture["generator_sha256"] {
		return "FAIL"
	}
	for key, current := range report.Statistics {
		previous := baseline.Statistics[key]
		if previous <= 0 {
			continue
		}
		if strings.HasPrefix(key, "progress_ops_per_second") {
			if current < previous*4/5 {
				return "FAIL"
			}
			continue
		}
		if strings.Contains(key, "goroutines") || strings.Contains(key, "open_fds") {
			continue
		}
		delta := int64(5 * time.Millisecond)
		if strings.Contains(key, "rss_bytes") {
			delta = 8 << 20
		} else if strings.Contains(key, "disk_bytes") {
			delta = 64 << 20
		} else if strings.Contains(key, "cpu_time") {
			delta = int64(100 * time.Millisecond)
		}
		if current > maxInt64(previous+delta, previous*5/4) {
			return "FAIL"
		}
	}
	report.Baseline = map[string]string{"status": "PASS", "revision": baseline.Revision, "owner_approved": "true"}
	return "PASS"
}

func storeUnique(values map[string]map[string]any, key string, record map[string]any) bool {
	if key == "" || values[key] != nil {
		return false
	}
	values[key] = record
	return true
}

func textField(record map[string]any, key string) string {
	value, _ := record[key].(string)
	return value
}
func intField(record map[string]any, key string) int64 {
	value, _ := record[key].(float64)
	return int64(value)
}

func cgroupInt64(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read cgroup limit: %w", err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cgroup limit: %w", err)
	}
	return value, nil
}

func sameDevice(first, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	return firstInfo.Sys().(*syscall.Stat_t).Dev == secondInfo.Sys().(*syscall.Stat_t).Dev
}

func referenceRunnerID() string {
	host, _ := os.Hostname()
	machine, _ := os.ReadFile("/etc/machine-id")
	sum := sha256.Sum256([]byte(host + "\x00" + strings.TrimSpace(string(machine))))
	return hex.EncodeToString(sum[:16])
}

func fingerprintMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		fmt.Fprintf(hash, "%s=%s\n", key, values[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func maxInt64(first, second int64) int64 {
	if first > second {
		return first
	}
	return second
}

func normalPFSample(sample string) bool {
	return strings.HasPrefix(sample, "concurrent-") || strings.HasPrefix(sample, "serial-")
}

func linearSlope(values []int64) int64 {
	var sumX, sumY, sumXY, sumXX int64
	for index, value := range values {
		x := int64(index)
		sumX += x
		sumY += value
		sumXY += x * value
		sumXX += x * x
	}
	n := int64(len(values))
	if denominator := n*sumXX - sumX*sumX; denominator != 0 {
		return (n*sumXY - sumX*sumY) / denominator
	}
	return 0
}
