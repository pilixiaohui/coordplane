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

	"coordplane/internal/core"
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
	StatisticsVersion      string `json:"statistics_version"`
	CPUModel               string `json:"cpu_model"`
	Kernel                 string `json:"kernel"`
	GoVersion              string `json:"go_version"`
	GitVersion             string `json:"git_version"`
	DockerVersion          string `json:"docker_version"`
	CPUQuota               string `json:"cpu_quota"`
	CPUSet                 string `json:"cpu_set"`
	MountDevice            string `json:"mount_device"`
	ImageDigest            string `json:"image_digest"`
}

type pfIdleSample struct {
	BatchID  string  `json:"batch_id"`
	RSSBytes []int64 `json:"rss_bytes"`
	CPUNS    int64   `json:"cpu_time_ns"`
}

type pfInventory struct {
	taskWave, taskSample, taskRun, runTask              map[string]string
	sampleDataDir, sampleProject                        map[string]string
	sourceTasks, scoredTasks, advanceTasks, scoredWaves map[string]bool
}

func validatePFSamples(report *pfReport) (pfInventory, error) {
	result := pfInventory{
		taskWave: map[string]string{}, taskSample: map[string]string{}, taskRun: map[string]string{}, runTask: map[string]string{},
		sampleDataDir: map[string]string{}, sampleProject: map[string]string{}, sourceTasks: map[string]bool{}, scoredTasks: map[string]bool{},
		advanceTasks: map[string]bool{}, scoredWaves: map[string]bool{},
	}
	report.RawMetrics = map[string][]int64{}
	dataDirs := map[string]bool{}
	for _, counts := range report.ObjectCounts {
		if counts.SampleID == "" || counts.FreshDataDirID == "" || counts.ProjectID == "" ||
			result.sampleDataDir[counts.SampleID] != "" || dataDirs[counts.FreshDataDirID] {
			return result, fmt.Errorf("durable count sample/data-dir identity is missing or duplicate")
		}
		result.sampleDataDir[counts.SampleID], result.sampleProject[counts.SampleID] = counts.FreshDataDirID, counts.ProjectID
		dataDirs[counts.FreshDataDirID] = true
	}
	for _, sample := range report.Samples {
		if sample.ID == "" || sample.BatchID == "" || result.sampleDataDir[sample.BatchID] == "" ||
			len(sample.TaskIDs) != len(sample.RunIDs) || len(sample.IntegrationTaskIDs) != len(sample.IntegrationRunIDs) {
			return result, fmt.Errorf("sample inventory identity is incomplete")
		}
		allTasks := append(append([]string(nil), sample.TaskIDs...), sample.IntegrationTaskIDs...)
		allRuns := append(append([]string(nil), sample.RunIDs...), sample.IntegrationRunIDs...)
		for index, taskID := range allTasks {
			if taskID == "" || result.taskWave[taskID] != "" {
				return result, fmt.Errorf("Task identity is missing or duplicated across PF samples")
			}
			runID := allRuns[index]
			if runID == "" || result.runTask[runID] != "" {
				return result, fmt.Errorf("Run identity is missing or duplicated across PF samples")
			}
			result.taskWave[taskID], result.taskSample[taskID], result.taskRun[taskID], result.runTask[runID] = sample.ID, sample.BatchID, runID, taskID
		}
		report.RawMetrics["status"] = append(report.RawMetrics["status"], sample.StatusNS...)
		if sample.StableRSSBytes > 0 {
			report.RawMetrics["soak_rss_bytes"] = append(report.RawMetrics["soak_rss_bytes"], sample.StableRSSBytes)
			report.RawMetrics["soak_goroutines"] = append(report.RawMetrics["soak_goroutines"], sample.StableGoroutines)
			report.RawMetrics["soak_open_fds"] = append(report.RawMetrics["soak_open_fds"], sample.StableFDs)
			if sample.ExternalRSSBytes <= 0 || sample.ExternalFDs <= 0 ||
				absInt64(sample.StableRSSBytes-sample.ExternalRSSBytes) > 64<<20 || absInt64(sample.StableFDs-sample.ExternalFDs) > 8 {
				return result, fmt.Errorf("observer resource sample does not match external /proc evidence")
			}
		}
		if sample.Warmup || sample.Soak || sample.Parallelism != 4 {
			continue
		}
		if len(sample.TaskIDs) != 4 || len(sample.RunIDs) != 4 || len(sample.QueueNS) != 4 ||
			len(sample.IntegrationTaskIDs) != 3 || len(sample.IntegrationRunIDs) != 3 || len(sample.IntegrationNS) != 3 || len(sample.RunFacts) != 7 {
			return result, fmt.Errorf("%s source/integration identity cardinality is incomplete", sample.ID)
		}
		result.scoredWaves[sample.ID] = true
		wantFacts := map[string]string{}
		for index, taskID := range sample.TaskIDs {
			result.sourceTasks[taskID], result.scoredTasks[taskID] = true, true
			wantFacts[taskID+"\x00"+sample.RunIDs[index]] = "source"
		}
		result.advanceTasks[sample.TaskIDs[0]] = true
		for index, taskID := range sample.IntegrationTaskIDs {
			result.scoredTasks[taskID] = true
			result.advanceTasks[taskID] = true
			wantFacts[taskID+"\x00"+sample.IntegrationRunIDs[index]] = "integration"
		}
		for _, fact := range sample.RunFacts {
			key := fact.TaskID + "\x00" + fact.RunID
			if wantFacts[key] == "" || wantFacts[key] != fact.Role || !core.IsRunTerminal(core.RunState(fact.TerminalState)) ||
				fact.ContainerID == "" || !fact.ContainerAbsent || !fact.ResourcesAbsent ||
				fact.TerminalObservedUnixNS <= 0 || fact.ContainerAbsentNS <= 0 ||
				fact.CleanupNS < fact.ContainerAbsentNS || result.runTask[fact.RunID] == "" {
				return result, fmt.Errorf("%s Run terminal/cleanup fact is missing, duplicate, or invalid", sample.ID)
			}
			delete(wantFacts, key)
		}
		if len(wantFacts) != 0 {
			return result, fmt.Errorf("%s Run cleanup exact set is incomplete", sample.ID)
		}
		for _, values := range [][]int64{sample.QueueNS, sample.IntegrationNS, []int64{sample.WaveNS, sample.WorkNS, sample.FanoutNS, sample.DirectCASNS, sample.Integrations3NS}} {
			for _, value := range values {
				if value <= 0 {
					return result, fmt.Errorf("%s contains a non-positive duration", sample.ID)
				}
			}
		}
		report.RawMetrics["T_queue"] = append(report.RawMetrics["T_queue"], sample.QueueNS...)
		report.RawMetrics["T_fanout4"] = append(report.RawMetrics["T_fanout4"], sample.FanoutNS)
		report.RawMetrics["T_cas"] = append(report.RawMetrics["T_cas"], sample.DirectCASNS)
		report.RawMetrics["T_integration"] = append(report.RawMetrics["T_integration"], sample.IntegrationNS...)
		report.RawMetrics["T_integrations3"] = append(report.RawMetrics["T_integrations3"], sample.Integrations3NS)
		report.RawMetrics["T_work4"] = append(report.RawMetrics["T_work4"], sample.WorkNS)
		for _, fact := range sample.RunFacts {
			report.RawMetrics["T_container_absent"] = append(report.RawMetrics["T_container_absent"], fact.ContainerAbsentNS)
			report.RawMetrics["T_cleanup"] = append(report.RawMetrics["T_cleanup"], fact.CleanupNS)
		}
	}
	for _, row := range report.Faults {
		for index, taskID := range row.TaskIDs {
			runID := row.RunIDs[index]
			if taskID == "" || runID == "" || result.taskWave[taskID] != "" || result.runTask[runID] != "" {
				return result, fmt.Errorf("fault Task/Run identity is missing or duplicated")
			}
			result.taskWave[taskID], result.taskSample[taskID] = row.SampleID, row.SampleID
			result.taskRun[taskID], result.runTask[runID] = runID, taskID
		}
	}
	if report.Profile == "release" && len(result.scoredWaves) != 20 {
		return result, fmt.Errorf("release scored wave exact set is not 20")
	}
	diskBySample := map[string]map[string]bool{}
	diskCadence := map[string][]int64{}
	for _, sample := range report.Disk {
		if sample.SampleID == "" || sample.DataDirID == "" || result.sampleDataDir[sample.SampleID] != sample.DataDirID ||
			sample.Boundary == "" || sample.ObservedUnixNS <= 0 || sample.Bytes < 0 || sample.Error != "" {
			return result, fmt.Errorf("disk sample is incomplete")
		}
		if diskBySample[sample.SampleID] == nil {
			diskBySample[sample.SampleID] = map[string]bool{}
		}
		diskBySample[sample.SampleID][sample.Boundary] = true
		report.RawMetrics["disk_bytes"] = append(report.RawMetrics["disk_bytes"], sample.Bytes)
		if sample.Boundary == "daemon_ready" || sample.Boundary == "interval" || sample.Boundary == "gc_complete" {
			diskCadence[sample.SampleID] = append(diskCadence[sample.SampleID], sample.ObservedUnixNS)
		}
	}
	for sampleID, times := range diskCadence {
		if len(times) < 2 {
			return result, fmt.Errorf("disk cadence has fewer than two observations for %s", sampleID)
		}
		sort.Slice(times, func(left, right int) bool { return times[left] < times[right] })
		for index := 1; index < len(times); index++ {
			if times[index]-times[index-1] > int64(1200*time.Millisecond) {
				return result, fmt.Errorf("disk cadence exceeded 1.2s for %s", sampleID)
			}
		}
	}
	for _, counts := range report.ObjectCounts {
		if !diskBySample[counts.SampleID]["daemon_ready"] || !diskBySample[counts.SampleID]["gc_complete"] {
			return result, fmt.Errorf("fresh data directory %s has no ready/GC disk boundary samples", counts.SampleID)
		}
	}
	return result, nil
}

func measurePFIdle(t *testing.T, batch *pfBatch) pfIdleSample {
	t.Helper()
	time.Sleep(30 * time.Second)
	pid := batch.daemon.PID()
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
	requireNoError(t, err)
	memory := strings.Fields(string(statm))
	pages, err := strconv.ParseInt(memory[1], 10, 64)
	requireNoError(t, err)
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	requireNoError(t, err)
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
	if environment["source_clean"] != "true" {
		return "release source tree was not clean before build"
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
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.SchemaVersion != 2 || !manifest.Approved {
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
	stat, ok := mustStat(root)
	if !ok {
		return "inspect reference mount identity"
	}
	cpuQuota := fileText("/sys/fs/cgroup/cpu.max")
	cpuSet := fileText("/sys/fs/cgroup/cpuset.cpus.effective")
	cpuModel := cpuModel()
	facts := map[string]string{
		"runner_id": runnerID, "logical_cpus": strconv.Itoa(cpuCount),
		"memory_limit_bytes": strconv.FormatInt(memoryLimit, 10), "disk_limit_bytes": strconv.FormatInt(diskLimit, 10),
		"filesystem": filesystem, "docker_storage_driver": driver, "statistics_version": pfStatisticsVersion,
		"cpu_model": cpuModel, "kernel": environment["kernel"], "go_version": environment["go"],
		"git_version": environment["git"], "docker_version": environment["docker"], "cpu_quota": cpuQuota,
		"cpu_set": cpuSet, "mount_device": strconv.FormatUint(uint64(stat.Dev), 10),
		"image_digest": environment["image_digest"],
	}
	fingerprint := fingerprintMap(facts)
	environment["runner_id"] = runnerID
	environment["logical_cpus"] = facts["logical_cpus"]
	environment["memory_limit_bytes"] = facts["memory_limit_bytes"]
	environment["disk_limit_bytes"] = facts["disk_limit_bytes"]
	environment["filesystem"] = filesystem
	for key, value := range facts {
		environment[key] = value
	}
	environment["environment_fingerprint"] = fingerprint
	if cpuCount != 8 || memoryLimit != 16<<30 || diskLimit != 10<<30 || manifest.LogicalCPUs != cpuCount ||
		manifest.MemoryLimitBytes != memoryLimit || manifest.DiskLimitBytes != diskLimit || manifest.Filesystem != filesystem ||
		manifest.DockerStorageDriver != driver || manifest.RunnerID != runnerID || manifest.EnvironmentFingerprint != fingerprint ||
		manifest.StatisticsVersion != pfStatisticsVersion || manifest.CPUModel != cpuModel || manifest.Kernel != facts["kernel"] ||
		manifest.GoVersion != facts["go_version"] || manifest.GitVersion != facts["git_version"] || manifest.DockerVersion != facts["docker_version"] ||
		manifest.CPUQuota != cpuQuota || manifest.CPUSet != cpuSet || manifest.MountDevice != facts["mount_device"] ||
		manifest.ImageDigest != facts["image_digest"] {
		return "actual cgroup/storage/image fingerprint does not match the approved 8 CPU / 16 GiB / 10 GiB reference manifest"
	}
	return ""
}

func validateObserver(report *pfReport) error {
	if !validBinaryDigests(report.Binaries) {
		return fmt.Errorf("three formal binary digests are incomplete")
	}
	if report.Resources == nil {
		report.Resources = map[string]int64{}
	}
	if err := validatePFFaults(report); err != nil {
		return err
	}
	inventory, err := validatePFSamples(report)
	if err != nil {
		return err
	}
	allowedStages := map[string]bool{}
	for _, id := range []string{
		"git.clone.lock_wait", "git.clone.prepare", "runtime.container.create_start", "git.capture.freeze",
		"git.capture.handoff", "git.capture.lock_wait", "git.capture.import", "git.capture.fsck", "git.capture.ref",
		"git.advance.lock_wait", "git.advance.ancestry", "git.advance.update_ref", "runtime.cleanup",
	} {
		allowedStages[id] = true
	}
	stageExact := map[string]map[string]int{}
	api, committed, clients := map[string]map[string]any{}, map[string]map[string]any{}, map[string]map[string]any{}
	taskWave, scoredTasks, scoredWaves := inventory.taskWave, inventory.scoredTasks, inventory.scoredWaves
	for _, idle := range report.Idle {
		report.RawMetrics["idle_rss_bytes"] = append(report.RawMetrics["idle_rss_bytes"], idle.RSSBytes...)
		report.RawMetrics["idle_cpu_time"] = append(report.RawMetrics["idle_cpu_time"], idle.CPUNS)
	}
	type bounds struct {
		start, end int64
		origin     string
	}
	bursts := map[string]bounds{}
	outcomes, submissions := map[string]map[string]any{}, map[string]map[string]any{}
	createdMessages, acknowledgedMessages := map[string]map[string]any{}, map[string]map[string]any{}
	cloneWork := map[string]int64{}
	stageAttempts := map[string]map[int64]bool{}
	stageStarts, stageLedger := map[string]map[string]any{}, map[string]map[string]any{}
	interruptedStages := map[string]bool{}
	captureInterruptions := map[string]bool{}
	resourceOffsets := map[string][]int64{}
	runtimeLimits := map[string]bool{}
	captureHelperTasks := map[string]int{}
	processLimits, originIdentity := map[string]map[string]any{}, map[string]map[string]any{}
	originsBySample := map[string]int{}
	diskMarkers := map[string]map[string]any{}
	interruptedOrigins := map[string]string{}
	var helperFacts []map[string]any
	helperLimits := 0
	resourceCount := 0
	for _, record := range report.Observer {
		kind, origin := textField(record, "record_type"), textField(record, "daemon_origin_id")
		sampleID, dataDirID := textField(record, "sample_id"), textField(record, "data_dir_id")
		if kind == "invalid" || origin == "" || sampleID == "" || dataDirID == "" ||
			inventory.sampleDataDir[sampleID] != dataDirID || intField(record, "schema_version") != 1 {
			return fmt.Errorf("observer contains invalid schema record")
		}
		if previous := originIdentity[origin]; previous != nil {
			if textField(previous, "sample_id") != sampleID || textField(previous, "data_dir_id") != dataDirID {
				return fmt.Errorf("daemon origin crosses sample/data-dir identity")
			}
		} else {
			originIdentity[origin] = record
			originsBySample[sampleID]++
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
				if boundary.origin == "" {
					boundary.origin = origin
				} else if boundary.origin != origin {
					return fmt.Errorf("progress burst crosses daemon origins for %s", wave)
				}
				if id == "api.progress.received" && textField(record, "result") != "error" && (boundary.start == 0 || intField(record, "mono_offset_ns") < boundary.start) {
					boundary.start = intField(record, "mono_offset_ns")
				}
				if id == "core.progress.committed" && intField(record, "mono_offset_ns") > boundary.end {
					boundary.end = intField(record, "mono_offset_ns")
				}
				bursts[wave] = boundary
			}
			if id == "core.outcome.accepted_commit" && inventory.taskRun[textField(record, "task_id")] != "" {
				if !storeUnique(outcomes, textField(record, "run_id"), record) {
					return fmt.Errorf("outcome point identity is missing or duplicate")
				}
			}
			if id == "git.capture.submitted_commit" && inventory.taskRun[textField(record, "task_id")] != "" {
				if !storeUnique(submissions, textField(record, "run_id"), record) {
					return fmt.Errorf("capture submission point identity is missing or duplicate")
				}
			}
			if normal && scoredTasks[textField(record, "task_id")] && id == "core.message.created_commit" && strings.HasPrefix(request, "pf01-peer-") {
				if !storeUnique(createdMessages, textField(record, "message_id"), record) {
					return fmt.Errorf("peer Message create identity is missing or duplicate")
				}
			}
			if normal && id == "core.message.acknowledged_commit" && createdMessages[textField(record, "message_id")] != nil {
				if !storeUnique(acknowledgedMessages, textField(record, "message_id"), record) {
					return fmt.Errorf("peer Message ack identity is missing or duplicate")
				}
			}
		case "stage_start", "stage", "stage_interrupted":
			id, key, duration := textField(record, "stage_id"), textField(record, "stage_key_sha256"), intField(record, "duration_ns")
			taskID := textField(record, "task_id")
			attemptValue, attemptOK := record["attempt_index"].(float64)
			attempt := int64(attemptValue)
			identity := stageAttemptIdentity(record)
			if !allowedStages[id] || key == "" || !attemptOK || attempt < 0 || intField(record, "start_unix_ns") <= 0 {
				return fmt.Errorf("unknown stage or negative duration")
			}
			if err := validateStageInventory(inventory, record, id, taskID); err != nil {
				return err
			}
			if kind == "stage_start" {
				if stageStarts[identity] != nil || stageLedger[identity] != nil {
					return fmt.Errorf("duplicate stage start")
				}
				stageStarts[identity], stageLedger[identity] = record, record
				continue
			}
			started := stageStarts[identity]
			if started == nil || textField(record, "result") == "" || !sameStageInventory(started, record) {
				return fmt.Errorf("stage completion has no start or a negative duration")
			}
			delete(stageStarts, identity)
			if kind == "stage_interrupted" {
				interruptedStages[stageAttemptIdentity(record)] = true
				interruptedBy := textField(record, "interrupted_by_origin_id")
				if interruptedBy == "" || interruptedBy == origin ||
					textField(record, "result") != "interrupted" {
					return fmt.Errorf("interrupted stage is not bound to its original daemon")
				}
				interruptedOrigins[interruptedBy] = origin
				if id == "git.capture.ref" {
					captureInterruptions[sampleID] = true
				}
				if _, exists := record["duration_ns"]; exists {
					return fmt.Errorf("interrupted stage has a cross-origin duration")
				}
			} else if duration < 0 {
				return fmt.Errorf("stage duration crosses daemon origins or is negative")
			}
			identity = sampleID + "\x00" + id + "\x00" + key
			if stageAttempts[identity] == nil {
				stageAttempts[identity] = map[int64]bool{}
			}
			if stageAttempts[identity][attempt] {
				return fmt.Errorf("duplicate stage attempt")
			}
			stageAttempts[identity][attempt] = true
			if kind == "stage" && normalPFSample(sampleID) && scoredTasks[taskID] {
				if stageExact[id] == nil {
					stageExact[id] = map[string]int{}
				}
				stageExact[id][taskID]++
				report.RawMetrics["stage."+id] = append(report.RawMetrics["stage."+id], duration)
				if id == "git.clone.prepare" && inventory.sourceTasks[textField(record, "task_id")] {
					cloneWork[taskWave[textField(record, "task_id")]] += duration
				}
			}
		case "resource":
			resourceCount++
			offset := intField(record, "mono_offset_ns")
			if offset < 0 {
				return fmt.Errorf("resource offset is negative")
			}
			resourceOffsets[origin] = append(resourceOffsets[origin], offset)
			for _, field := range []string{"rss_bytes", "goroutines", "open_fds"} {
				value := intField(record, field)
				if value < 0 {
					return fmt.Errorf("resource value is negative")
				}
				if value > report.Resources[field+"_max"] {
					report.Resources[field+"_max"] = value
				}
			}
		case "runtime_limit":
			taskID, runID := textField(record, "task_id"), textField(record, "run_id")
			role := textField(record, "runtime_role")
			if role == "agent" && (inventory.runTask[runID] == "" || runtimeLimits[runID] || intField(record, "memory_bytes") != 512<<20 ||
				intField(record, "nano_cpus") != 1_000_000_000 || intField(record, "pids_limit") != 256) {
				return fmt.Errorf("runtime cgroup fact is missing, duplicate, or incorrect")
			}
			if role == "agent" {
				if inventory.runTask[runID] != taskID || inventory.taskSample[taskID] != sampleID ||
					inventory.sampleProject[sampleID] != textField(record, "project_id") {
					return fmt.Errorf("agent runtime cgroup fact crosses inventory identity")
				}
				runtimeLimits[runID] = true
			} else if (role != "git_capture" && role != "git_inspect") || intField(record, "memory_bytes") != 128<<20 ||
				intField(record, "nano_cpus") != 1_000_000_000 || intField(record, "pids_limit") != 64 {
				return fmt.Errorf("Git helper cgroup fact is missing or incorrect")
			} else {
				helperLimits++
				helperFacts = append(helperFacts, record)
				if role == "git_capture" {
					if inventory.runTask[runID] != taskID || inventory.taskSample[taskID] != sampleID ||
						inventory.sampleProject[sampleID] != textField(record, "project_id") {
						return fmt.Errorf("capture helper cgroup fact crosses inventory identity")
					}
					captureHelperTasks[taskID]++
				} else if inventory.taskRun[taskID] == "" || inventory.taskSample[taskID] != sampleID ||
					inventory.sampleProject[sampleID] != textField(record, "project_id") {
					return fmt.Errorf("inspect helper cgroup fact crosses inventory identity")
				}
			}
		case "process_limit":
			quota, period := intField(record, "cpu_quota_us"), intField(record, "cpu_period_us")
			if processLimits[origin] != nil || quota <= 0 || period <= 0 || quota != 3*period ||
				intField(record, "memory_max_bytes") != 384<<20 || textField(record, "cgroup_path") == "" || textField(record, "error") != "" ||
				intField(record, "gomaxprocs") != 3 || intField(record, "go_memory_limit_bytes") != 384<<20 {
				return fmt.Errorf("Daemon process resource limit is incorrect")
			}
			processLimits[origin] = record
		case "disk_boundary_marker":
			stageID, boundary := textField(record, "stage_id"), textField(record, "boundary")
			identity := stageAttemptIdentity(record)
			if (stageID != "git.capture.freeze" && stageID != "git.capture.handoff" && stageID != "git.capture.import") ||
				(boundary != "start" && boundary != "end") || intField(record, "marker_unix_ns") <= 0 ||
				stageLedger[identity] == nil || !sameStageInventory(stageLedger[identity], record) {
				return fmt.Errorf("capture disk boundary marker is incomplete")
			}
			markerID := identity + "\x00" + boundary
			if diskMarkers[markerID] != nil {
				return fmt.Errorf("duplicate disk boundary marker")
			}
			diskMarkers[markerID] = record
		default:
			return fmt.Errorf("unknown observer record type %s", kind)
		}
	}
	if len(stageStarts) != 0 {
		return fmt.Errorf("observer contains unterminated stage starts")
	}
	for sampleID := range inventory.sampleDataDir {
		want := 1
		if strings.HasPrefix(sampleID, "fault-") {
			want = 2
		}
		got := originsBySample[sampleID]
		if got != want {
			return fmt.Errorf("sample %s origin exact set = %d, want %d", sampleID, got, want)
		}
	}
	wantOrigins := map[string]int{"smoke": 8, "release": 28}[report.Profile]
	if wantOrigins == 0 || len(originIdentity) != wantOrigins {
		return fmt.Errorf("%s profile origin cardinality = %d, want %d", report.Profile, len(originIdentity), wantOrigins)
	}
	for replacementID, originalID := range interruptedOrigins {
		replacement, original := originIdentity[replacementID], originIdentity[originalID]
		if replacement == nil || textField(replacement, "sample_id") != textField(original, "sample_id") ||
			textField(replacement, "data_dir_id") != textField(original, "data_dir_id") {
			return fmt.Errorf("interrupted stage replacement origin is outside its sample/data-dir closure")
		}
	}
	for request, client := range clients {
		if api[request] == nil || committed[request] == nil || textField(api[request], "daemon_origin_id") != textField(committed[request], "daemon_origin_id") ||
			textField(client, "daemon_origin_id") != textField(committed[request], "daemon_origin_id") || textField(client, "run_id") != textField(committed[request], "run_id") {
			return fmt.Errorf("progress join failed for %s", request)
		}
	}
	for wave := range scoredWaves {
		boundary := bursts[wave]
		if boundary.start <= 0 || boundary.end <= boundary.start {
			return fmt.Errorf("progress burst boundary is missing or negative for %s", wave)
		}
		duration := boundary.end - boundary.start
		report.RawMetrics["T_progress_burst"] = append(report.RawMetrics["T_progress_burst"], duration)
		report.RawMetrics["progress_ops_per_second_milli"] = append(report.RawMetrics["progress_ops_per_second_milli"], 200_000_000_000_000/duration)
	}
	for runID := range inventory.runTask {
		if !scoredTasks[inventory.runTask[runID]] {
			continue
		}
		start, end := outcomes[runID], submissions[runID]
		if start == nil || end == nil || textField(start, "daemon_origin_id") != textField(end, "daemon_origin_id") {
			return fmt.Errorf("capture point join failed for %s", runID)
		}
		duration := intField(end, "mono_offset_ns") - intField(start, "mono_offset_ns")
		if duration <= 0 {
			return fmt.Errorf("capture duration is negative for %s", runID)
		}
		report.RawMetrics["T_capture"] = append(report.RawMetrics["T_capture"], duration)
	}
	if helperLimits == 0 || len(processLimits) != len(originIdentity) {
		return fmt.Errorf("Daemon/Git helper resource facts are incomplete")
	}
	cgroups := map[string]bool{}
	for origin, limit := range processLimits {
		cgroupID := textField(limit, "daemon_cgroup_id")
		if originIdentity[origin] == nil || len(resourceOffsets[origin]) == 0 || cgroupID == "" || cgroups[cgroupID] {
			return fmt.Errorf("Daemon origin is not bound to a distinct cgroup")
		}
		cgroups[cgroupID] = true
	}
	for _, helper := range helperFacts {
		daemon := processLimits[textField(helper, "daemon_origin_id")]
		if daemon == nil || intField(daemon, "cpu_quota_us")*1_000_000_000/intField(daemon, "cpu_period_us")+intField(helper, "nano_cpus") > 4_000_000_000 ||
			intField(daemon, "memory_max_bytes")+intField(helper, "memory_bytes") > 512<<20 {
			return fmt.Errorf("Daemon/Git helper combined cgroup limit exceeds 4 CPU/512 MiB")
		}
	}
	for taskID := range inventory.scoredTasks {
		if captureHelperTasks[taskID] != 1 {
			return fmt.Errorf("Task %s capture helper cgroup exact set = %d", taskID, captureHelperTasks[taskID])
		}
	}
	for identity, start := range stageLedger {
		stageID := textField(start, "stage_id")
		if stageID != "git.capture.freeze" && stageID != "git.capture.handoff" && stageID != "git.capture.import" {
			continue
		}
		if diskMarkers[identity+"\x00start"] == nil || (!interruptedStages[identity] && diskMarkers[identity+"\x00end"] == nil) {
			return fmt.Errorf("capture stage has no complete disk marker set")
		}
	}
	responses := map[string]bool{}
	for _, sample := range report.Disk {
		if sample.StageID == "" {
			continue
		}
		identity := diskSampleIdentity(sample)
		marker := diskMarkers[identity]
		markerNS := intField(marker, "marker_unix_ns")
		if marker == nil || responses[identity] || !sameDiskInventory(marker, sample) ||
			sample.ObservedUnixNS < markerNS || sample.ObservedUnixNS-markerNS > int64(2*time.Second) {
			return fmt.Errorf("external disk boundary response is missing, duplicate, or mismatched")
		}
		responses[identity] = true
	}
	for identity, marker := range diskMarkers {
		if !responses[identity] {
			return fmt.Errorf("disk boundary marker has no external sampler response: %s/%s", textField(marker, "stage_id"), textField(marker, "boundary"))
		}
	}
	for messageID, start := range createdMessages {
		end := acknowledgedMessages[messageID]
		if end == nil || textField(start, "daemon_origin_id") != textField(end, "daemon_origin_id") {
			return fmt.Errorf("peer Message point join failed for %s", messageID)
		}
		duration := intField(end, "mono_offset_ns") - intField(start, "mono_offset_ns")
		if duration <= 0 {
			return fmt.Errorf("peer Message duration is negative for %s", messageID)
		}
		report.RawMetrics["T_message"] = append(report.RawMetrics["T_message"], duration)
	}
	for wave := range scoredWaves {
		if cloneWork[wave] <= 0 {
			return fmt.Errorf("source clone work is missing for %s", wave)
		}
		report.RawMetrics["git_clone_work4"] = append(report.RawMetrics["git_clone_work4"], cloneWork[wave])
	}
	if len(clients) == 0 || resourceCount == 0 {
		return fmt.Errorf("observer has no progress or resource samples")
	}
	for identity, attempts := range stageAttempts {
		for attempt := int64(0); attempt < int64(len(attempts)); attempt++ {
			if !attempts[attempt] {
				return fmt.Errorf("stage attempt sequence is not contiguous for %s", identity)
			}
		}
	}
	for runID := range inventory.runTask {
		if scoredTasks[inventory.runTask[runID]] && !runtimeLimits[runID] {
			return fmt.Errorf("Run %s has no actual runtime cgroup fact", runID)
		}
	}
	for origin, offsets := range resourceOffsets {
		sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
		for index := 1; index < len(offsets); index++ {
			if offsets[index]-offsets[index-1] > int64(150*time.Millisecond) {
				return fmt.Errorf("resource observer cadence exceeded 150ms for %s", origin)
			}
		}
	}
	waves := len(scoredWaves)
	metricCounts := map[string]int{
		"T_queue": 4 * waves, "T_fanout4": waves, "T_cas": waves, "T_integration": 3 * waves,
		"T_integrations3": waves, "T_work4": waves, "T_container_absent": 7 * waves,
		"T_cleanup": 7 * waves, "R_progress": 200 * waves, "T_progress_burst": waves,
		"progress_ops_per_second_milli": waves, "T_message": 10 * waves, "T_capture": 7 * waves,
		"git_clone_work4": waves,
	}
	for metric, count := range metricCounts {
		if len(report.RawMetrics[metric]) != count {
			return fmt.Errorf("%s exact sample count = %d, want %d", metric, len(report.RawMetrics[metric]), count)
		}
	}
	for id := range allowedStages {
		expected := inventory.scoredTasks
		if id == "git.advance.update_ref" {
			expected = inventory.advanceTasks
		}
		if len(stageExact[id]) != len(expected) {
			return fmt.Errorf("%s exact stage identity count = %d, want %d", id, len(stageExact[id]), len(expected))
		}
		for taskID := range expected {
			if stageExact[id][taskID] != 1 {
				return fmt.Errorf("%s Task %s stage exact set = %d", id, taskID, stageExact[id][taskID])
			}
		}
	}
	for _, row := range report.Faults {
		if row.Kind == "capture" && !captureInterruptions[row.SampleID] {
			return fmt.Errorf("capture fault %s lost its interrupted ref stage group", row.SampleID)
		}
	}
	for metric, values := range report.RawMetrics {
		if len(values) > 0 {
			for suffix, rank := range map[string]int{"p50": 50, "p90": 90, "p95": 95, "p99": 99, "max": 100} {
				report.Statistics[metric+"_"+suffix] = nearestRank(values, rank)
			}
		}
	}
	return nil
}

func validatePFFaults(report *pfReport) error {
	expected := map[string]int{"live": 1, "capture": 1, "cas": 1}
	if report.Profile == "release" {
		expected = map[string]int{"live": 5, "capture": 3, "cas": 3}
	}
	seen, samples, roots := map[string]map[int]bool{}, map[string]bool{}, map[string]bool{}
	countsBySample := map[string]pfObjectCounts{}
	for _, counts := range report.ObjectCounts {
		countsBySample[counts.SampleID] = counts
	}
	for _, row := range report.Faults {
		if seen[row.Kind] == nil {
			seen[row.Kind] = map[int]bool{}
		}
		wantRuns := 1
		if row.Kind == "live" {
			wantRuns = 4
		}
		before, after := append([]string(nil), row.RunIDsBefore...), append([]string(nil), row.RunIDsAfter...)
		sort.Strings(before)
		sort.Strings(after)
		if expected[row.Kind] == 0 || row.Index < 1 || row.Index > expected[row.Kind] || seen[row.Kind][row.Index] ||
			row.SampleID == "" || samples[row.SampleID] || row.FreshDataDirID == "" || roots[row.FreshDataDirID] ||
			len(row.TaskIDs) == 0 || len(row.TaskIDs) != len(row.RunIDs) || !uniqueStrings(row.TaskIDs) || !uniqueStrings(row.RunIDs) ||
			row.Counts.SampleID != row.SampleID || row.Counts.FreshDataDirID != row.FreshDataDirID || row.Counts.ProjectID == "" ||
			countsBySample[row.SampleID] != row.Counts || row.RecoveryNS <= 0 || row.RecoveryNS > int64(8*time.Second) ||
			len(before) != wantRuns || !uniqueStrings(before) || !uniqueStrings(after) || strings.Join(before, "\x00") != strings.Join(after, "\x00") ||
			row.PreRestart == "" || row.FinalSHA == "" || row.GitFSCK != "pass" || row.Cleanup != "absent" || row.Result != "PASS" ||
			row.Counts.OpenRuns != 0 || row.Counts.OwnedResidue != 0 || row.Counts.PeerMessages != row.Counts.PeerAcknowledged || row.Counts.PeerMessages != row.Counts.PeerAckEvent ||
			(row.Kind == "live" && (row.DurableUnacked != 4 || row.Counts.PeerMessages != 10)) ||
			(row.Kind != "live" && (row.DurableUnacked != 0 || row.Counts.PeerMessages != 4)) {
			return fmt.Errorf("fault row %s does not carry the complete durable signature", row.SampleID)
		}
		seen[row.Kind][row.Index], samples[row.SampleID], roots[row.FreshDataDirID] = true, true, true
	}
	for kind, count := range expected {
		if len(seen[kind]) != count {
			return fmt.Errorf("fault exact count for %s = %d, want %d", kind, len(seen[kind]), count)
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
	if len(report.RawMetrics["status"]) != 1_000 {
		return fmt.Errorf("status release sample count is incomplete")
	}
	for key, limit := range limits {
		report.Thresholds[key] = limit
		if report.Statistics[key] <= 0 || report.Statistics[key] > limit {
			return fmt.Errorf("%s=%d exceeds %d", key, report.Statistics[key], limit)
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
	raw, _ := os.ReadFile(path)
	var baseline pfReport
	if json.Unmarshal(raw, &baseline) != nil || baseline.SchemaVersion != report.SchemaVersion || baseline.Scenario != report.Scenario ||
		baseline.Result != "PASS" || baseline.Profile != "release" || baseline.Revision == "" || report.Revision == "" ||
		baseline.Baseline["owner_approved"] != "true" ||
		baseline.Environment["statistics_version"] != pfStatisticsVersion || report.Environment["statistics_version"] != pfStatisticsVersion ||
		baseline.Environment["environment_fingerprint"] != report.Environment["environment_fingerprint"] ||
		baseline.Environment["image_digest"] != report.Environment["image_digest"] ||
		!validBinaryDigests(baseline.Binaries) || !validBinaryDigests(report.Binaries) ||
		baseline.Fixture["generator_sha256"] != report.Fixture["generator_sha256"] || !sameMetricKeys(baseline.Statistics, report.Statistics) {
		return "FAIL"
	}
	for key, current := range report.Statistics {
		previous := baseline.Statistics[key]
		if previous <= 0 || current <= 0 {
			return "FAIL"
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
		if current > max(previous+delta, previous*5/4) {
			return "FAIL"
		}
	}
	report.Baseline = map[string]string{
		"status": "PASS", "owner_approved": "true", "baseline_revision": baseline.Revision,
		"current_revision": report.Revision, "baseline_image_digest": baseline.Environment["image_digest"],
		"current_image_digest": report.Environment["image_digest"], "statistics_version": pfStatisticsVersion,
	}
	return "PASS"
}

func sameMetricKeys(first, second map[string]int64) bool {
	if len(first) != len(second) {
		return false
	}
	for key := range first {
		if _, ok := second[key]; !ok {
			return false
		}
	}
	return true
}

func validBinaryDigests(digests map[string]string) bool {
	if len(digests) != 3 {
		return false
	}
	for _, name := range []string{"coordplane", "coordlink", "coordplane-git-helper"} {
		raw, err := hex.DecodeString(digests[name])
		if err != nil || len(raw) != sha256.Size {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) bool {
	for index, value := range values {
		if value == "" || index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}

func storeUnique(values map[string]map[string]any, key string, record map[string]any) bool {
	if key == "" || values[key] != nil {
		return false
	}
	values[key] = record
	return true
}

func stageAttemptIdentity(record map[string]any) string {
	return strings.Join([]string{
		textField(record, "sample_id"), textField(record, "data_dir_id"), textField(record, "daemon_origin_id"),
		textField(record, "stage_id"), textField(record, "stage_key_sha256"),
		strconv.FormatInt(intField(record, "attempt_index"), 10),
	}, "\x00")
}

func sameStageInventory(first, second map[string]any) bool {
	for _, field := range []string{
		"sample_id", "data_dir_id", "daemon_origin_id", "project_id", "task_id", "run_id",
		"operation_id", "request_id", "message_id", "stage_id", "stage_key_sha256",
	} {
		if textField(first, field) != textField(second, field) {
			return false
		}
	}
	return intField(first, "attempt_index") == intField(second, "attempt_index")
}

func validateStageInventory(inventory pfInventory, record map[string]any, stageID, taskID string) error {
	sampleID := textField(record, "sample_id")
	if inventory.taskRun[taskID] == "" || inventory.taskSample[taskID] != sampleID ||
		inventory.sampleDataDir[sampleID] != textField(record, "data_dir_id") ||
		inventory.sampleProject[sampleID] != textField(record, "project_id") {
		return fmt.Errorf("%s stage crosses frozen Task/sample/project inventory", stageID)
	}
	wantRun, actualRun := inventory.taskRun[taskID], textField(record, "run_id")
	clone := stageID == "git.clone.lock_wait" || stageID == "git.clone.prepare"
	if (clone && actualRun != "") || (!clone && (actualRun != wantRun || textField(record, "operation_id") == "")) {
		return fmt.Errorf("%s stage crosses frozen Run/operation inventory", stageID)
	}
	return nil
}

func diskSampleIdentity(sample pfDiskSample) string {
	return strings.Join([]string{
		sample.SampleID, sample.DataDirID, sample.DaemonOriginID, sample.StageID, sample.StageKeySHA256,
		strconv.FormatInt(sample.AttemptIndex, 10), sample.Boundary,
	}, "\x00")
}

func sameDiskInventory(marker map[string]any, sample pfDiskSample) bool {
	return textField(marker, "sample_id") == sample.SampleID && textField(marker, "data_dir_id") == sample.DataDirID &&
		textField(marker, "daemon_origin_id") == sample.DaemonOriginID && textField(marker, "stage_id") == sample.StageID &&
		textField(marker, "stage_key_sha256") == sample.StageKeySHA256 && textField(marker, "boundary") == sample.Boundary &&
		textField(marker, "project_id") == sample.ProjectID && textField(marker, "task_id") == sample.TaskID &&
		textField(marker, "run_id") == sample.RunID && textField(marker, "operation_id") == sample.OperationID &&
		intField(marker, "attempt_index") == sample.AttemptIndex
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

func fileText(path string) string {
	raw, _ := os.ReadFile(path)
	return strings.TrimSpace(string(raw))
}

func cpuModel() string {
	for _, line := range strings.Split(fileText("/proc/cpuinfo"), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func mustStat(path string) (*syscall.Stat_t, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
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

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
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
