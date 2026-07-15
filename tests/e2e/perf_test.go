//go:build e2e

package e2e_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

type pfSample struct {
	ID                 string      `json:"sample_id"`
	BatchID            string      `json:"batch_id"`
	Parallelism        int         `json:"parallelism"`
	Warmup             bool        `json:"warmup"`
	Soak               bool        `json:"soak"`
	WaveNS             int64       `json:"t_wave_ns"`
	WorkNS             int64       `json:"t_work_ns"`
	ProgressOperations int         `json:"progress_operations"`
	PeerMessages       int         `json:"peer_messages"`
	IntegrationTasks   int         `json:"integration_tasks"`
	FinalSHA           string      `json:"final_sha"`
	TaskHeads          []string    `json:"task_heads"`
	TaskIDs            []string    `json:"task_ids"`
	RunIDs             []string    `json:"run_ids"`
	IntegrationTaskIDs []string    `json:"integration_task_ids,omitempty"`
	IntegrationRunIDs  []string    `json:"integration_run_ids,omitempty"`
	QueueNS            []int64     `json:"t_queue_ns"`
	FanoutNS           int64       `json:"t_fanout4_ns"`
	DirectCASNS        int64       `json:"t_cas_ns"`
	IntegrationNS      []int64     `json:"t_integration_ns"`
	Integrations3NS    int64       `json:"t_integrations3_ns"`
	StatusNS           []int64     `json:"status_ns,omitempty"`
	DiskBytes          int64       `json:"data_dir_bytes"`
	StableRSSBytes     int64       `json:"stable_rss_bytes,omitempty"`
	StableGoroutines   int64       `json:"stable_goroutines,omitempty"`
	StableFDs          int64       `json:"stable_open_fds,omitempty"`
	ExternalRSSBytes   int64       `json:"external_rss_bytes,omitempty"`
	ExternalFDs        int64       `json:"external_open_fds,omitempty"`
	RunFacts           []pfRunFact `json:"run_cleanup_facts"`
}

type pfRunFact struct {
	TaskID                 string `json:"task_id"`
	RunID                  string `json:"run_id"`
	Role                   string `json:"role"`
	TerminalState          string `json:"terminal_state"`
	TerminalObservedUnixNS int64  `json:"terminal_observed_unix_ns"`
	ContainerAbsentNS      int64  `json:"t_container_absent_ns"`
	CleanupNS              int64  `json:"t_cleanup_ns"`
	ContainerAbsent        bool   `json:"container_absent"`
	ResourcesAbsent        bool   `json:"resources_absent"`
	ContainerID            string `json:"container_id"`
}

type pfObjectCounts struct {
	SampleID         string `json:"sample_id"`
	ProjectID        string `json:"project_id"`
	Tasks            int    `json:"tasks"`
	Runs             int    `json:"runs"`
	Messages         int    `json:"messages"`
	Events           int    `json:"events"`
	PeerMessages     int    `json:"peer_messages"`
	PeerAcknowledged int    `json:"peer_acknowledged"`
	PeerAckEvent     int    `json:"peer_ack_events"`
	OpenRuns         int    `json:"open_runs"`
	OwnedResidue     int    `json:"owned_residue"`
}

type pfReport struct {
	SchemaVersion int                `json:"schema_version"`
	Scenario      string             `json:"scenario"`
	Profile       string             `json:"profile"`
	Result        string             `json:"result"`
	Reason        string             `json:"reason,omitempty"`
	Revision      string             `json:"revision"`
	Environment   map[string]string  `json:"environment"`
	Fixture       map[string]any     `json:"fixture"`
	Samples       []pfSample         `json:"samples"`
	Statistics    map[string]int64   `json:"nearest_rank_ns"`
	Thresholds    map[string]int64   `json:"thresholds_ns"`
	SerialRatio   float64            `json:"median_t4_over_median_t1"`
	Observer      []map[string]any   `json:"observer_raw_records"`
	ObjectCounts  []pfObjectCounts   `json:"durable_object_counts"`
	Faults        []pfFaultRow       `json:"fault_recovery_raw_table"`
	RawMetrics    map[string][]int64 `json:"raw_metrics_ns"`
	Resources     map[string]int64   `json:"resource_facts"`
	Baseline      map[string]string  `json:"baseline"`
	Idle          []pfIdleSample     `json:"idle_resource_samples"`
	Disk          []pfDiskSample     `json:"disk_samples"`
}

const pfStatisticsVersion = "pf01-nearest-rank-v2"

type pfProfile struct {
	concurrentBatches, concurrentWaves, soakWaves int
	serialBatches, serialWaves                    int
	liveFaults, captureFaults, casFaults          int
	timeout                                       time.Duration
}

type pfBatch struct {
	t                     *testing.T
	ctx                   context.Context
	binary, image, id     string
	root, dataDir, socket string
	daemon                *daemonProcess
	agents                []core.Agent
	project               core.Project
	initialSHA, observer  string
	parallel              int
	trackSoak             bool
	diskMu                sync.Mutex
	diskStop, diskDone    chan struct{}
	disk                  []pfDiskSample
	terminalMu            sync.Mutex
	terminals             map[string]pfTerminalObservation
}

type pfTerminalObservation struct {
	state       core.RunState
	containerID string
	observedNS  int64
}

type pfDiskSample struct {
	SampleID       string `json:"sample_id"`
	Boundary       string `json:"boundary"`
	ObservedUnixNS int64  `json:"observed_unix_ns"`
	Bytes          int64  `json:"bytes"`
	Error          string `json:"error,omitempty"`
}

var pfRoles = [...]string{"A", "B", "C", "D"}

func TestPFDirectorySampleTreatsRemovedTreeAsAbsent(t *testing.T) {
	if size := directoryBytesExcept(t, filepath.Join(t.TempDir(), "removed"), ""); size != 0 {
		t.Fatalf("removed tree size = %d, want 0", size)
	}
}

func TestPFOwnedResidueRejectsUnknownDirectChildren(t *testing.T) {
	dataDir := t.TempDir()
	projectID := "project-1"
	for _, path := range []string{
		filepath.Join(dataDir, "workspaces", projectID),
		filepath.Join(dataDir, "workspaces", ".partial"),
		filepath.Join(dataDir, "workspaces", ".partial", projectID),
		filepath.Join(dataDir, "workspaces", ".empty-git-template"),
		filepath.Join(dataDir, "handoff", projectID),
		filepath.Join(dataDir, "handoff", projectID, "empty-task"),
		filepath.Join(dataDir, "handoff", "quarantine"),
	} {
		requireNoError(t, os.MkdirAll(path, 0o700))
	}
	if residue := ownedResidue(t, dataDir, projectID); len(residue) != 0 {
		t.Fatalf("empty structural roots counted as residue: %v", residue)
	}
	for _, path := range []string{
		filepath.Join(dataDir, "workspaces", "unknown"),
		filepath.Join(dataDir, "handoff", "unknown"),
	} {
		requireNoError(t, os.MkdirAll(path, 0o700))
	}
	if residue := ownedResidue(t, dataDir, projectID); len(residue) != 2 {
		t.Fatalf("unknown direct children escaped residue scan: %v", residue)
	}
}

func TestPF01FourAgentPerformance(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	requireExecutable(t, "E2E_COORDLINK_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	output := strings.TrimSpace(os.Getenv("PF01_OUTPUT"))
	profile := strings.TrimSpace(os.Getenv("PF01_PROFILE"))
	if image == "" || output == "" || (profile != "smoke" && profile != "release") {
		t.Fatal("E2E_RUNTIME_IMAGE, PF01_OUTPUT, and PF01_PROFILE=smoke|release are required")
	}
	settings := pfProfile{
		concurrentBatches: 1, concurrentWaves: 3,
		serialBatches: 1, serialWaves: 1, liveFaults: 1, captureFaults: 1, casFaults: 1,
		timeout: 25 * time.Minute,
	}
	if profile == "release" {
		settings = pfProfile{
			concurrentBatches: 4, concurrentWaves: 5, soakWaves: 15,
			serialBatches: 2, serialWaves: 5, liveFaults: 5, captureFaults: 3, casFaults: 3,
			timeout: 2 * time.Hour,
		}
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e-pf01", settings.timeout)
	requireNoError(t, err)
	defer func() { _ = release() }()

	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	root := t.TempDir()
	report := pfReport{
		SchemaVersion: 2, Scenario: "PF-01", Profile: profile, Revision: commandText(ctx, "git", "rev-parse", "HEAD"),
		Environment: perfEnvironment(ctx, image),
		Thresholds:  map[string]int64{"t_wave_p50": int64(60 * time.Second), "t_wave_p90": int64(90 * time.Second), "t_wave_max": int64(180 * time.Second)},
	}
	defer writePerfReport(t, output, &report)
	if profile == "release" {
		if reason := validateReferenceEnvironment(ctx, root, image, report.Revision, report.Environment); reason != "" {
			report.Result, report.Reason = "INVALID_ENVIRONMENT", reason
			return
		}
	}
	source, initial, manifest := createPerfRepository(t, ctx, root)
	report.Fixture = manifest
	if reason := invalidPerfFixture(manifest); reason != "" {
		report.Result = "INVALID_ENVIRONMENT"
		report.Reason = reason
		return
	}
	for batchIndex := 0; batchIndex < settings.concurrentBatches; batchIndex++ {
		batchID := fmt.Sprintf("concurrent-%02d", batchIndex+1)
		batch := newPFBatch(t, ctx, coordplane, image, source, initial, filepath.Join(root, batchID), batchID, 4)
		batch.trackSoak = profile == "release" && batchIndex == 0
		if profile == "release" {
			report.Idle = append(report.Idle, measurePFIdle(t, batch))
		}
		report.Samples = append(report.Samples, batch.runWave("warmup", true, false))
		for wave := 0; wave < settings.concurrentWaves; wave++ {
			report.Samples = append(report.Samples, batch.runWave(fmt.Sprintf("wave-%02d", wave+1), false, false))
		}
		if batchIndex == 0 {
			for wave := 0; wave < settings.soakWaves; wave++ {
				report.Samples = append(report.Samples, batch.runWave(fmt.Sprintf("soak-%02d", wave+1), false, true))
			}
		}
		batch.close()
		report.Disk = append(report.Disk, batch.diskFacts()...)
		report.Observer = append(report.Observer, readObserverRecords(t, batch.observer)...)
		report.ObjectCounts = append(report.ObjectCounts, batch.objectCounts())
	}
	faults, records, counts, disks := runPFFaultTable(t, ctx, coordplane, image, source, initial, root, settings)
	report.Faults = faults
	report.Observer = append(report.Observer, records...)
	report.ObjectCounts = append(report.ObjectCounts, counts...)
	report.Disk = append(report.Disk, disks...)
	for batchIndex := 0; batchIndex < settings.serialBatches; batchIndex++ {
		batchID := fmt.Sprintf("serial-%02d", batchIndex+1)
		batch := newPFBatch(t, ctx, coordplane, image, source, initial, filepath.Join(root, batchID), batchID, 1)
		if profile == "release" {
			report.Samples = append(report.Samples, batch.runWave("warmup", true, false))
		}
		for wave := 0; wave < settings.serialWaves; wave++ {
			report.Samples = append(report.Samples, batch.runWave(fmt.Sprintf("wave-%02d", wave+1), false, false))
		}
		batch.close()
		report.Disk = append(report.Disk, batch.diskFacts()...)
		report.Observer = append(report.Observer, readObserverRecords(t, batch.observer)...)
		report.ObjectCounts = append(report.ObjectCounts, batch.objectCounts())
	}
	var scored, concurrentWork, serialWork []int64
	for _, sample := range report.Samples {
		if !sample.Warmup && !sample.Soak && sample.Parallelism == 4 {
			scored = append(scored, sample.WaveNS)
			concurrentWork = append(concurrentWork, sample.WorkNS)
		}
		if !sample.Warmup && sample.Parallelism == 1 {
			serialWork = append(serialWork, sample.WorkNS)
		}
	}
	report.Statistics = map[string]int64{"t_wave_p50": nearestRank(scored, 50), "t_wave_p90": nearestRank(scored, 90), "t_wave_max": nearestRank(scored, 100)}
	if serialMedian := nearestRank(serialWork, 50); serialMedian > 0 {
		report.SerialRatio = float64(nearestRank(concurrentWork, 50)) / float64(serialMedian)
	}
	if err := validateObserver(&report); err != nil {
		report.Result, report.Reason = "FAIL", err.Error()
		return
	}
	if profile == "release" {
		if err := validateReleaseThresholds(&report); err != nil {
			report.Result, report.Reason = "FAIL", err.Error()
			return
		}
	}
	if profile == "smoke" {
		report.Result = "PASS"
		return
	}
	report.Result = releasePFResult(&report)
	if report.Result != "PASS" {
		report.Reason = "release thresholds or approved baseline did not pass"
	}
}

func newPFBatch(
	t *testing.T,
	ctx context.Context,
	binary, image, source, initial, root, id string,
	parallel int,
) *pfBatch {
	return newPFBatchWithEnv(t, ctx, binary, image, source, initial, root, id, parallel, nil)
}

func newPFBatchWithEnv(
	t *testing.T,
	ctx context.Context,
	binary, image, source, initial, root, id string,
	parallel int,
	extraEnvironment []string,
) *pfBatch {
	t.Helper()
	requireNoError(t, os.MkdirAll(root, 0o700))
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	writeFile(t, instructions, []byte("Execute the deterministic PF-01 bootstrap contract.\n"), 0o600)
	config := writePerfConfig(t, root, dataDir, socket, image, parallel)
	observer := filepath.Join(root, "observer.jsonl")
	environment := append([]string{
		"COORDPLANE_PERF_OBSERVER_OUTPUT=" + observer,
		"COORDPLANE_PERF_SAMPLE_ID=" + id,
		"COORDPLANE_PERF_DATA_DIR=" + dataDir,
		"GOMAXPROCS=3",
		"GOMEMLIMIT=384MiB",
	}, extraEnvironment...)
	daemon := startDaemonWithEnv(t, binary, config, socket, environment)
	registerLiveHomeCleanup(t, image, dataDir)
	waitForReady(t, ctx, binary, socket, id+" startup")
	batch := &pfBatch{
		t: t, ctx: ctx, binary: binary, image: image, id: id,
		root: root, dataDir: dataDir, socket: socket, daemon: daemon,
		initialSHA: initial, parallel: parallel, observer: observer,
		diskStop: make(chan struct{}), diskDone: make(chan struct{}),
		terminals: make(map[string]pfTerminalObservation),
	}
	go batch.sampleDisk()
	batch.agents = make([]core.Agent, len(pfRoles))
	for index, role := range pfRoles {
		batch.agents[index] = pfJSON[core.Agent](batch, "agent", "add", "--socket", socket, "--display-name", "PF01 "+role, "--adapter", "codex", "--image", image, "--instructions-file", instructions, "--request-id", id+"-agent-"+role, "--output", "json")
	}
	batch.project = pfJSON[core.Project](batch, "project", "add", "--socket", socket, "--name", "PF01 "+id, "--repo", source, "--ref", "refs/heads/main", "--integration-agent", batch.agents[0].ID, "--request-id", id+"-project", "--output", "json")
	if batch.project.InitialSHA != initial || batch.project.CanonicalSHA != initial {
		t.Fatalf("PF-01 batch %s started at %s/%s, want %s", id, batch.project.InitialSHA, batch.project.CanonicalSHA, initial)
	}
	return batch
}

func (b *pfBatch) close() {
	b.t.Helper()
	close(b.diskStop)
	<-b.diskDone
	if err := b.daemon.Stop(); err != nil {
		b.t.Fatalf("stop PF-01 batch %s: %v\n%s", b.id, err, readLog(b.daemon.logPath))
	}
}

func (b *pfBatch) runWave(name string, warmup, soak bool) pfSample {
	b.t.Helper()
	waveID := b.id + "-" + name
	base := projectDetail(b.t, b.ctx, b.binary, b.socket, b.project.ID).ActualCanonicalSHA
	b.recordDisk("wave_start")
	tasks := make([]core.Task, len(pfRoles))
	createStarted := make([]time.Time, len(pfRoles))
	for index, role := range pfRoles {
		createStarted[index] = time.Now()
		tasks[index] = pfJSON[core.Task](b, "task", "create", "--socket", b.socket, "--project", b.project.ID, "--agent", b.agents[index].ID, "--title", "PF01 "+waveID+" "+role, "--description", "p5_role="+role+";pf01=true;progress_count=50", "--request-id", waveID+"-task-"+role, "--output", "json")
		if tasks[index].BaseSHA != base {
			b.t.Fatalf("PF01 %s task base = %s want %s", waveID, tasks[index].BaseSHA, base)
		}
	}
	goBody := fmt.Sprintf("P5-GO wave=%s d_agent=%s d_task=%s", waveID, b.agents[3].ID, tasks[3].ID)
	var workStart time.Time
	var statusNS []int64
	queueNS := make([]int64, len(tasks))
	var fanoutNS int64
	if b.parallel == 4 {
		for index, task := range tasks {
			b.waitRunRow(task.ID)
			queueNS[index] = time.Since(createStarted[index]).Nanoseconds()
		}
		for _, task := range tasks {
			b.waitRun(task.ID, "PF01 READY")
		}
		fanoutNS = time.Since(createStarted[len(createStarted)-1]).Nanoseconds()
		time.Sleep(time.Second)
		if soakStatusHold(name) {
			statusNS = samplePFStatus(b.t, b.ctx, b.binary, b.socket)
		}
		workStart = b.sendGO(tasks, goBody, waveID)
	} else {
		for index, role := range pfRoles {
			b.waitRunRow(tasks[index].ID)
			queueNS[index] = time.Since(createStarted[index]).Nanoseconds()
			b.waitRun(tasks[index].ID, role+" serial READY")
			started := time.Now()
			if workStart.IsZero() {
				workStart = started
			}
			sendBossMessage(b.t, b.ctx, b.binary, b.socket, b.project.ID, b.agents[index].ID, tasks[index].ID, goBody, waveID+"-go-"+role)
			tasks[index] = b.waitTask(tasks[index].ID, role+" serial submit", capturedTask)
		}
	}
	for index := range tasks {
		tasks[index] = b.waitTask(tasks[index].ID, pfRoles[index]+" submitted", capturedTask)
	}
	b.recordDisk("capture_submitted")
	workDuration := time.Since(workStart)
	var directCASNS, integrations3NS int64
	var integrationNS []int64
	if b.parallel == 4 {
		var integrationsStarted time.Time
		for index := range tasks {
			acceptStarted := time.Now()
			accepted := pfJSON[core.Task](b, "task", "accept", tasks[index].ID, "--socket", b.socket, "--integration-agent", b.agents[0].ID, "--request-id", waveID+"-accept-"+pfRoles[index], "--output", "json")
			responseObserved := time.Now()
			if index == 1 {
				integrationsStarted = acceptStarted
			}
			if index == 0 {
				b.observeTerminal(accepted.HeadRunID)
				if accepted.Status == core.TaskCompleted {
					tasks[index] = accepted
				} else {
					tasks[index] = b.waitTask(tasks[index].ID, pfRoles[index]+" integrated", func(task core.Task) bool {
						return task.Status == core.TaskCompleted
					})
				}
				directCASNS = time.Since(acceptStarted).Nanoseconds()
			} else {
				completed := b.waitIntegrationPair(accepted, responseObserved, pfRoles[index])
				tasks[index] = completed.source
				integrationNS = append(integrationNS, completed.finished.Sub(completed.integrationFirst).Nanoseconds())
				if index == len(tasks)-1 {
					integrations3NS = completed.sourceObserved.Sub(integrationsStarted).Nanoseconds()
				}
			}
		}
		b.recordDisk("integration_completed")
	} else {
		for index := range tasks {
			tasks[index] = pfJSON[core.Task](b, "task", "cancel", tasks[index].ID, "--socket", b.socket, "--reason", "PF-01 serial comparison complete", "--request-id", waveID+"-cancel-"+pfRoles[index], "--output", "json")
		}
	}
	final := projectDetail(b.t, b.ctx, b.binary, b.socket, b.project.ID).ActualCanonicalSHA
	waveDuration := time.Since(createStarted[0])
	control := filepath.Join(b.dataDir, "repos", b.project.ID+".git")
	if b.parallel == 4 {
		for _, task := range tasks {
			gitDirSucceeds(b.t, b.ctx, control, "merge-base", "--is-ancestor", task.HeadSHA, final)
		}
	} else if final != base {
		b.t.Fatalf("serial comparison moved canonical from %s to %s", base, final)
	}
	gitDirSucceeds(b.t, b.ctx, control, "fsck", "--full", "--strict")
	diskBytes := directoryBytesExcept(b.t, b.dataDir, "")
	heads := make([]string, 4)
	taskIDs := make([]string, 4)
	runIDs := make([]string, 4)
	var integrationTaskIDs, integrationRunIDs []string
	for index := range tasks {
		heads[index] = tasks[index].HeadSHA
		taskIDs[index] = tasks[index].ID
		runIDs[index] = tasks[index].HeadRunID
		if tasks[index].IntegrationTaskID != "" {
			integration := taskDetail(b.t, b.ctx, b.binary, b.socket, tasks[index].IntegrationTaskID).Task
			integrationTaskIDs = append(integrationTaskIDs, integration.ID)
			integrationRunIDs = append(integrationRunIDs, integration.HeadRunID)
		}
	}
	runFacts := b.terminalRunFacts(taskIDs, runIDs, integrationTaskIDs, integrationRunIDs)
	b.waitForRunContainersAbsent(runFacts)
	pfJSON[core.GCPreview](b, "gc", "preview", "--socket", b.socket, "--output", "json")
	b.recordDisk("gc_preview")
	pfJSON[core.GCRunResult](b, "gc", "run", "--socket", b.socket, "--confirm", "--request-id", waveID+"-gc", "--output", "json")
	waitForWorkspacesRemoved(b.t, b.ctx, b.dataDir, b.project.ID, append(taskIDs, integrationTaskIDs...)...)
	b.recordDisk("gc_complete")
	b.finishRunFacts(runFacts)
	var stableRSS, stableGoroutines, stableFDs, externalRSS, externalFDs int64
	if b.trackSoak && !warmup {
		time.Sleep(5 * time.Second)
		stableRSS, stableGoroutines, stableFDs = medianPFResource(b.t, b.observer)
		externalRSS, _ = readPFProcess(b.t, b.daemon.command.Process.Pid)
		externalFDs = pfProcessFDs(b.t, b.daemon.command.Process.Pid)
	}
	integrationTasks := 0
	if b.parallel == 4 {
		integrationTasks = 3
	}
	return pfSample{
		ID: waveID, BatchID: b.id, Parallelism: b.parallel, Warmup: warmup, Soak: soak,
		WaveNS: waveDuration.Nanoseconds(), WorkNS: workDuration.Nanoseconds(),
		ProgressOperations: 200, PeerMessages: 10, IntegrationTasks: integrationTasks,
		FinalSHA: final, TaskHeads: heads, TaskIDs: taskIDs, RunIDs: runIDs,
		IntegrationTaskIDs: integrationTaskIDs, IntegrationRunIDs: integrationRunIDs,
		QueueNS: queueNS, FanoutNS: fanoutNS, DirectCASNS: directCASNS,
		IntegrationNS: integrationNS, Integrations3NS: integrations3NS, StatusNS: statusNS,
		DiskBytes:      diskBytes,
		StableRSSBytes: stableRSS, StableGoroutines: stableGoroutines, StableFDs: stableFDs,
		ExternalRSSBytes: externalRSS, ExternalFDs: externalFDs, RunFacts: runFacts,
	}
}

func (b *pfBatch) waitRunRow(taskID string) core.Run {
	b.t.Helper()
	return eventually(b.t, b.ctx, 30*time.Second, "PF-01 Run row", func() (core.Run, bool, string) {
		detail, err := commandJSON[core.TaskDetail](b.ctx, b.binary, "task", "show", taskID, "--socket", b.socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		if detail.CurrentRun == nil {
			return core.Run{}, false, string(detail.Task.Status)
		}
		return *detail.CurrentRun, true, ""
	})
}

func soakStatusHold(name string) bool {
	var index int
	_, _ = fmt.Sscanf(name, "soak-%02d", &index)
	return index > 10
}

func samplePFStatus(t *testing.T, ctx context.Context, binary, socket string) []int64 {
	t.Helper()
	values := make([]int64, 200)
	for index := range values {
		started := time.Now()
		status := runJSON[core.Status](t, ctx, binary, "status", "--socket", socket, "--output", "json")
		if !status.DaemonReady {
			t.Fatal("status sample observed daemon_ready=false")
		}
		values[index] = time.Since(started).Nanoseconds()
	}
	return values
}

func medianPFResource(t *testing.T, path string) (int64, int64, int64) {
	t.Helper()
	var rss, goroutines, fds []int64
	var latest int64
	records := readObserverRecords(t, path)
	for _, record := range records {
		if record["record_type"] == "resource" && int64(record["mono_offset_ns"].(float64)) > latest {
			latest = int64(record["mono_offset_ns"].(float64))
		}
	}
	for _, record := range records {
		if record["record_type"] == "resource" && int64(record["mono_offset_ns"].(float64)) >= latest-int64(5*time.Second) {
			rss = append(rss, int64(record["rss_bytes"].(float64)))
			goroutines = append(goroutines, int64(record["goroutines"].(float64)))
			fds = append(fds, int64(record["open_fds"].(float64)))
		}
	}
	if len(rss) == 0 {
		t.Fatal("observer has no resource record")
	}
	return nearestRank(rss, 50), nearestRank(goroutines, 50), nearestRank(fds, 50)
}

func pfProcessFDs(t *testing.T, pid int) int64 {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	requireNoError(t, err)
	return int64(len(entries))
}

func (b *pfBatch) terminalRunFacts(sourceTasks, sourceRuns, integrationTasks, integrationRuns []string) []pfRunFact {
	b.t.Helper()
	facts := make([]pfRunFact, 0, len(sourceRuns)+len(integrationRuns))
	add := func(role string, tasks, runs []string) {
		if len(tasks) != len(runs) {
			b.t.Fatalf("%s Task/Run identity count differs: %d/%d", role, len(tasks), len(runs))
		}
		for index, runID := range runs {
			run := pfJSON[core.Run](b, "run", "show", runID, "--socket", b.socket, "--output", "json")
			observation, ok := b.terminalObservation(runID)
			if run.TaskID != tasks[index] || !core.IsRunTerminal(run.State) || run.EndedAt == "" || !ok || observation.state != run.State {
				b.t.Fatalf("%s terminal Run fact is incomplete: %#v", role, run)
			}
			facts = append(facts, pfRunFact{TaskID: tasks[index], RunID: runID, Role: role,
				TerminalState: string(run.State), TerminalObservedUnixNS: observation.observedNS, ContainerID: observation.containerID})
		}
	}
	add("source", sourceTasks, sourceRuns)
	add("integration", integrationTasks, integrationRuns)
	return facts
}

func (b *pfBatch) waitForRunContainersAbsent(facts []pfRunFact) {
	b.t.Helper()
	eventually(b.t, b.ctx, 20*time.Second, "each PF-01 Run container absent", func() (bool, bool, string) {
		all := true
		for index := range facts {
			fact := &facts[index]
			if fact.ContainerAbsent {
				continue
			}
			if _, err := commandOutput(b.ctx, "", "docker", "inspect", fact.ContainerID); err == nil {
				all = false
				continue
			}
			listed, err := commandOutput(b.ctx, "", "docker", "ps", "-aq", "--no-trunc", "--filter", "id="+fact.ContainerID)
			if err != nil || strings.TrimSpace(string(listed)) != "" {
				all = false
				continue
			}
			fact.ContainerAbsent = true
			fact.ContainerAbsentNS = time.Now().UnixNano() - fact.TerminalObservedUnixNS
		}
		return all, all, "containers remain"
	})
}

func (b *pfBatch) finishRunFacts(facts []pfRunFact) {
	b.t.Helper()
	for index := range facts {
		fact := &facts[index]
		paths := []string{
			filepath.Join(b.dataDir, "workspaces", b.project.ID, fact.TaskID),
			filepath.Join(b.dataDir, "handoff", b.project.ID, fact.TaskID, fact.RunID),
			filepath.Join(b.dataDir, "run-control", fact.RunID), filepath.Join(b.dataDir, "logs", fact.RunID),
		}
		eventually(b.t, b.ctx, 20*time.Second, "all Run-owned resources absent", func() (bool, bool, string) {
			for _, path := range paths {
				if _, err := os.Lstat(path); err == nil {
					return false, false, path
				} else if !errors.Is(err, os.ErrNotExist) {
					return false, false, err.Error()
				}
			}
			return true, true, ""
		})
		fact.ResourcesAbsent = true
		fact.CleanupNS = time.Now().UnixNano() - fact.TerminalObservedUnixNS
	}
}

func (b *pfBatch) objectCounts() pfObjectCounts {
	b.t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(b.dataDir, "coordplane.db"))
	if err != nil {
		b.t.Fatal(err)
	}
	defer database.Close()
	counts := pfObjectCounts{SampleID: b.id, ProjectID: b.project.ID}
	query := `WITH selected(id) AS (VALUES (?)) SELECT
		(SELECT count(*) FROM tasks WHERE project_id=selected.id),
		(SELECT count(*) FROM runs WHERE project_id=selected.id),
		(SELECT count(*) FROM messages WHERE project_id=selected.id),
		(SELECT count(*) FROM events WHERE project_id=selected.id),
		(SELECT count(*) FROM messages WHERE project_id=selected.id AND body LIKE 'PF01-PEER %'),
		(SELECT count(*) FROM messages WHERE project_id=selected.id AND body LIKE 'PF01-PEER %' AND state='acknowledged'),
		(SELECT count(*) FROM events WHERE project_id=selected.id AND kind='message.acknowledged' AND entity_id IN (SELECT id FROM messages WHERE body LIKE 'PF01-PEER %')),
		(SELECT count(*) FROM runs WHERE project_id=selected.id AND state IN ('starting','active')) FROM selected`
	if err := database.QueryRow(query, b.project.ID).Scan(
		&counts.Tasks, &counts.Runs, &counts.Messages, &counts.Events,
		&counts.PeerMessages, &counts.PeerAcknowledged, &counts.PeerAckEvent, &counts.OpenRuns,
	); err != nil {
		b.t.Fatal(err)
	}
	residue := ownedResidue(b.t, b.dataDir, b.project.ID)
	counts.OwnedResidue = len(residue)
	if counts.OpenRuns != 0 || counts.OwnedResidue != 0 || counts.PeerMessages != counts.PeerAcknowledged || counts.PeerMessages != counts.PeerAckEvent {
		b.t.Fatalf("%s durable counts or cleanup differ: %#v residue=%v", b.id, counts, residue)
	}
	return counts
}

func readObserverRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	requireNoError(t, err)
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode PF-01 observer record: %v", err)
		}
		records = append(records, record)
	}
	requireNoError(t, scanner.Err())
	return records
}

func ownedResidue(t *testing.T, dataDir, projectID string) []string {
	t.Helper()
	var residue []string
	allowedDirect := map[string]map[string]bool{
		"workspaces": {projectID: true, ".partial": true, ".empty-git-template": true},
		"handoff":    {projectID: true, "quarantine": true},
	}
	for _, root := range []string{"workspaces", "handoff", "run-control", "logs"} {
		base := filepath.Join(dataDir, root)
		_ = filepath.Walk(base, func(path string, _ os.FileInfo, err error) error {
			relative := filepath.ToSlash(strings.TrimPrefix(path, base+string(filepath.Separator)))
			parts := strings.Split(relative, "/")
			structural := len(parts) == 1 && allowedDirect[root][relative]
			structural = structural || root == "workspaces" && len(parts) == 2 && parts[0] == ".partial" && parts[1] == projectID
			structural = structural || root == "handoff" && len(parts) == 2 && parts[0] == projectID
			if err == nil && path != base && !structural {
				residue = append(residue, filepath.ToSlash(strings.TrimPrefix(path, dataDir+string(filepath.Separator))))
			}
			return nil
		})
	}
	return residue
}

type pfIntegrationCompletion struct {
	source           core.Task
	sourceObserved   time.Time
	integrationFirst time.Time
	finished         time.Time
}

func (b *pfBatch) waitIntegrationPair(source core.Task, responseObserved time.Time, role string) pfIntegrationCompletion {
	b.t.Helper()
	result := pfIntegrationCompletion{source: source}
	if source.Status == core.TaskCompleted {
		result.sourceObserved = responseObserved
		b.observeTerminal(source.HeadRunID)
	}
	integrationID := source.IntegrationTaskID
	if integrationID != "" {
		result.integrationFirst = responseObserved
	}
	var integrationObserved time.Time
	return eventually(b.t, b.ctx, 3*time.Minute, role+" source/integration completed", func() (pfIntegrationCompletion, bool, string) {
		sourceDetail, sourceErr := commandJSON[core.TaskDetail](b.ctx, b.binary, "task", "show", source.ID, "--socket", b.socket, "--output", "json")
		if sourceErr != nil {
			return result, false, sourceErr.Error()
		}
		sourceNow := time.Now()
		b.observeTerminal(sourceDetail.Task.HeadRunID)
		if result.sourceObserved.IsZero() && sourceDetail.Task.Status == core.TaskCompleted {
			result.source, result.sourceObserved = sourceDetail.Task, sourceNow
		}
		if integrationID == "" && sourceDetail.Task.IntegrationTaskID != "" {
			integrationID = sourceDetail.Task.IntegrationTaskID
			result.integrationFirst = sourceNow
		}
		if integrationID == "" {
			return result, false, fmt.Sprintf("source=%s integration=not_observed", sourceDetail.Task.Status)
		}
		integration, integrationErr := commandJSON[core.TaskDetail](b.ctx, b.binary, "task", "show", integrationID, "--socket", b.socket, "--output", "json")
		if integrationErr != nil {
			return result, false, integrationErr.Error()
		}
		b.observeTerminal(integration.Task.HeadRunID)
		if integrationObserved.IsZero() && integration.Task.Status == core.TaskCompleted {
			integrationObserved = time.Now()
		}
		if result.sourceObserved.IsZero() || integrationObserved.IsZero() {
			return result, false, fmt.Sprintf("source=%s integration=%s", sourceDetail.Task.Status, integration.Task.Status)
		}
		result.finished = result.sourceObserved
		if integrationObserved.After(result.finished) {
			result.finished = integrationObserved
		}
		return result, true, ""
	})
}

func capturedTask(task core.Task) bool {
	return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.TaskRef != ""
}

func (b *pfBatch) waitTask(taskID, reason string, predicate func(core.Task) bool) core.Task {
	b.t.Helper()
	return eventually(b.t, b.ctx, 3*time.Minute, reason, func() (core.Task, bool, string) {
		detail, err := commandJSON[core.TaskDetail](b.ctx, b.binary, "task", "show", taskID, "--socket", b.socket, "--output", "json")
		if err != nil {
			return core.Task{}, false, err.Error()
		}
		b.observeTerminal(detail.Task.HeadRunID)
		return detail.Task, predicate(detail.Task), fmt.Sprintf(
			"status=%s pending=%s integration=%s failure=%s",
			detail.Task.Status, detail.Task.PendingAction, detail.Task.IntegrationTaskID, detail.Task.FailureReason,
		)
	})
}

func (b *pfBatch) observeTerminal(runID string) {
	if runID == "" {
		return
	}
	if _, ok := b.terminalObservation(runID); ok {
		return
	}
	run, err := commandJSON[core.Run](b.ctx, b.binary, "run", "show", runID, "--socket", b.socket, "--output", "json")
	if err != nil || !core.IsRunTerminal(run.State) {
		return
	}
	b.terminalMu.Lock()
	if _, exists := b.terminals[run.ID]; !exists {
		b.terminals[run.ID] = pfTerminalObservation{state: run.State, containerID: run.ContainerID, observedNS: time.Now().UnixNano()}
	}
	b.terminalMu.Unlock()
}

func (b *pfBatch) terminalObservation(runID string) (pfTerminalObservation, bool) {
	b.terminalMu.Lock()
	defer b.terminalMu.Unlock()
	observation, ok := b.terminals[runID]
	return observation, ok
}

func (b *pfBatch) waitRun(taskID, reason string) core.Run {
	b.t.Helper()
	run := eventually(b.t, b.ctx, 60*time.Second, reason, func() (core.Run, bool, string) {
		detail, err := commandJSON[core.TaskDetail](b.ctx, b.binary, "task", "show", taskID, "--socket", b.socket, "--output", "json")
		if err != nil {
			return core.Run{}, false, err.Error()
		}
		ready := detail.CurrentRun != nil && detail.CurrentRun.State == core.RunActive &&
			detail.CurrentRun.ContainerID != "" && detail.LatestProgress != nil &&
			strings.Contains(detail.LatestProgress.PayloadJSON, "P5-READY")
		if !ready {
			return core.Run{}, false, fmt.Sprintf("task=%s run=%v progress=%v", detail.Task.Status, detail.CurrentRun != nil, detail.LatestProgress != nil)
		}
		return *detail.CurrentRun, true, ""
	})
	if inspect := inspectContainer(b.t, b.ctx, run.ContainerID); !inspect.State.Running {
		b.t.Fatalf("PF-01 READY Run %s is not live in Docker", run.ID)
	}
	return run
}

func (b *pfBatch) sendGO(tasks []core.Task, body, waveID string) time.Time {
	b.t.Helper()
	type result struct {
		started time.Time
		err     error
	}
	barrier := make(chan struct{})
	results := make(chan result, len(tasks))
	for index := range tasks {
		go func(index int) {
			<-barrier
			started := time.Now()
			_, err := commandJSON[core.Message](b.ctx, b.binary, "message", "send", "--socket", b.socket,
				"--project", b.project.ID, "--agent", b.agents[index].ID, "--task", tasks[index].ID,
				"--body", body, "--request-id", waveID+"-go-"+string(rune('A'+index)), "--output", "json")
			results <- result{started: started, err: err}
		}(index)
	}
	close(barrier)
	var earliest time.Time
	for range tasks {
		result := <-results
		if result.err != nil {
			b.t.Fatalf("send PF-01 GO: %v", result.err)
		}
		if earliest.IsZero() || result.started.Before(earliest) {
			earliest = result.started
		}
	}
	return earliest
}

func pfJSON[T any](batch *pfBatch, args ...string) T {
	return runJSON[T](batch.t, batch.ctx, batch.binary, args...)
}

func writePerfConfig(t *testing.T, root, dataDir, socket, image string, parallel int) string {
	t.Helper()
	path := filepath.Join(root, "coordplane.yaml")
	content := fmt.Sprintf("data_dir: %s\noperator_socket: %s\nmax_parallel_runs: %d\nretention:\n  completed_workspace: 0\n  terminal_task_ref: 24h\n  run_log: 0\nruntime:\n  docker_network: none\n  workspace_root: %s\n  agent_home_root: %s\n  log_root: %s\n  default_image: %s\n  provider_env_allowlist: []\n  run_timeout: 3m\n  shutdown_grace: 3s\ngit:\n  capture_helper_image: %s\n  capture_timeout: 30s\n  maximum_bundle_bytes: 67108864\n  maximum_objects: 250000\n  maximum_handoff_bytes: 268435456\n", dataDir, socket, parallel, filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "agent-homes"), filepath.Join(dataDir, "logs"), image, image)
	writeFile(t, path, []byte(content), 0o600)
	return path
}

func createPerfRepository(t *testing.T, ctx context.Context, root string) (string, string, map[string]any) {
	t.Helper()
	repo := filepath.Join(root, "fixture")
	requireNoError(t, os.MkdirAll(filepath.Join(repo, "bench"), 0o700))
	requireNoError(t, os.MkdirAll(filepath.Join(repo, "data"), 0o700))
	run(t, ctx, "git", "init", "-q", "-b", "main", repo)
	runIn(t, ctx, repo, "git", "config", "user.name", "PF01 Fixture")
	runIn(t, ctx, repo, "git", "config", "user.email", "pf01@coordplane.local")
	writeFile(t, filepath.Join(repo, "fixture-test.sh"), []byte("#!/bin/sh\nset -eu\nexec /usr/local/bin/pf01-fixturecheck\n"), 0o755)
	for _, role := range []string{"a", "b", "c", "d"} {
		writeFile(t, filepath.Join(repo, "bench", role+".txt"), []byte("base\n"), 0o600)
	}
	for index := 0; index < 2048; index++ {
		path := filepath.Join(repo, "data", fmt.Sprintf("file-%04d.bin", index))
		writeFile(t, path, perfFixtureBytes(index, 0), 0o600)
	}
	commitPerfFixture(t, ctx, repo, 0)
	for commit := 1; commit < 32; commit++ {
		for item := 0; item < 64; item++ {
			index := (commit-1)*64 + item
			path := filepath.Join(repo, "data", fmt.Sprintf("file-%04d.bin", index))
			writeFile(t, path, perfFixtureBytes(index, commit), 0o600)
		}
		commitPerfFixture(t, ctx, repo, commit)
	}
	runIn(t, ctx, repo, "git", "gc", "--quiet")
	initial := git(t, ctx, repo, "rev-parse", "HEAD")
	generator := sha256.Sum256([]byte("pf01-fixture-v1:sha256(seed,index,version):2048x8192:32"))
	return repo, initial, map[string]any{
		"data_files": 2048, "commits": 32, "bytes_per_file": 8192,
		"initial_sha": initial, "tree_sha": git(t, ctx, repo, "rev-parse", "HEAD^{tree}"),
		"generator_sha256": hex.EncodeToString(generator[:]),
		"checkout_bytes":   directoryBytesExcept(t, repo, filepath.Join(repo, ".git")),
		"pack_bytes":       matchingFileBytes(t, filepath.Join(repo, ".git", "objects", "pack"), ".pack"),
	}
}

func perfFixtureBytes(index, version int) []byte {
	result := make([]byte, 0, 8192)
	for block := 0; len(result) < cap(result); block++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("coordplane-pf01-v1:%04d:%02d:%04d", index, version, block)))
		result = append(result, digest[:]...)
	}
	return result
}

func commitPerfFixture(t *testing.T, ctx context.Context, repo string, index int) {
	t.Helper()
	runIn(t, ctx, repo, "git", "add", ".")
	date := fmt.Sprintf("2026-01-%02dT00:00:00Z", index%28+1)
	command := exec.CommandContext(ctx, "git", "-c", "commit.gpgsign=false", "commit", "-q", "--date", date, "-m", fmt.Sprintf("fixture %02d", index))
	command.Dir = repo
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date, "TZ=UTC")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("commit deterministic fixture %02d: %v; output=%s", index, err, raw)
	}
}

func directoryBytesExcept(t *testing.T, root, excluded string) int64 {
	t.Helper()
	var total int64
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if path == excluded {
			return filepath.SkipDir
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

func (b *pfBatch) sampleDisk() {
	defer close(b.diskDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	b.recordDisk("daemon_ready")
	for {
		select {
		case <-ticker.C:
			b.recordDisk("interval")
		case <-b.diskStop:
			return
		}
	}
}

func (b *pfBatch) recordDisk(boundary string) {
	sample := pfDiskSample{SampleID: b.id, Boundary: boundary, ObservedUnixNS: time.Now().UnixNano()}
	sample.Bytes, sample.Error = directoryBytes(b.dataDir)
	b.diskMu.Lock()
	b.disk = append(b.disk, sample)
	b.diskMu.Unlock()
}

func directoryBytes(root string) (int64, string) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err.Error()
	}
	return total, ""
}

func (b *pfBatch) diskFacts() []pfDiskSample {
	b.diskMu.Lock()
	defer b.diskMu.Unlock()
	return append([]pfDiskSample(nil), b.disk...)
}

func matchingFileBytes(t *testing.T, root, suffix string) int64 {
	t.Helper()
	entries, err := os.ReadDir(root)
	requireNoError(t, err)
	var total int64
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), suffix) {
			info, err := entry.Info()
			requireNoError(t, err)
			total += info.Size()
		}
	}
	return total
}

func invalidPerfFixture(manifest map[string]any) string {
	const (
		initialSHA = "1b01f5928c31c13a1430311a2947793779987d6b"
		treeSHA    = "d4f9fb06a92d63dde5e43e1222e2ee3136b24b9b"
		generator  = "62b2c1cb163183e7fcb551c1efb9ca2af9711f6e0f65abfe5ec53a40005a9a4e"
	)
	if manifest["initial_sha"] != initialSHA || manifest["tree_sha"] != treeSHA ||
		manifest["generator_sha256"] != generator || manifest["data_files"] != 2048 ||
		manifest["commits"] != 32 || manifest["bytes_per_file"] != 8192 {
		return "PF-01 fixture identity does not match the frozen manifest"
	}
	checkout, checkoutOK := manifest["checkout_bytes"].(int64)
	pack, packOK := manifest["pack_bytes"].(int64)
	if !checkoutOK || checkout < 16<<20 || checkout > 24<<20 ||
		!packOK || pack < 24<<20 || pack > 40<<20 {
		return "PF-01 fixture checkout or pack size is outside the frozen range"
	}
	return ""
}

func nearestRank(values []int64, percentile int) int64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if len(sorted) == 0 {
		return 0
	}
	index := (percentile*len(sorted)+99)/100 - 1
	return sorted[index]
}
func commandText(ctx context.Context, command string, args ...string) string {
	raw, err := commandOutput(ctx, "", command, args...)
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(raw))
}
func perfEnvironment(ctx context.Context, image string) map[string]string {
	return map[string]string{
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"kernel":                commandText(ctx, "uname", "-sr"),
		"docker":                commandText(ctx, "docker", "version", "--format", "{{.Server.Version}}"),
		"docker_storage_driver": commandText(ctx, "docker", "info", "--format", "{{.Driver}}"),
		"image_digest":          commandText(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image),
		"git":                   commandText(ctx, "git", "--version"),
		"runner_id":             strings.TrimSpace(os.Getenv("PF01_RUNNER_ID")),
		"statistics_version":    pfStatisticsVersion,
	}
}
func writePerfReport(t *testing.T, path string, report *pfReport) {
	t.Helper()
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Error(err)
		return
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Error(err)
	}
}
