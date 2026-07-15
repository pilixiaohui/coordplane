//go:build e2e

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

type pfSample struct {
	ID                 string   `json:"sample_id"`
	BatchID            string   `json:"batch_id"`
	Parallelism        int      `json:"parallelism"`
	Warmup             bool     `json:"warmup"`
	Soak               bool     `json:"soak"`
	WaveNS             int64    `json:"t_wave_ns"`
	WorkNS             int64    `json:"t_work_ns"`
	CleanupNS          int64    `json:"t_cleanup_ns"`
	ProgressOperations int      `json:"progress_operations"`
	PeerMessages       int      `json:"peer_messages"`
	IntegrationTasks   int      `json:"integration_tasks"`
	FinalSHA           string   `json:"final_sha"`
	TaskHeads          []string `json:"task_heads"`
}

type pfReport struct {
	SchemaVersion int               `json:"schema_version"`
	Scenario      string            `json:"scenario"`
	Profile       string            `json:"profile"`
	Result        string            `json:"result"`
	Reason        string            `json:"reason,omitempty"`
	Revision      string            `json:"revision"`
	Environment   map[string]string `json:"environment"`
	Fixture       map[string]any    `json:"fixture"`
	Samples       []pfSample        `json:"samples"`
	Statistics    map[string]int64  `json:"nearest_rank_ns"`
	Thresholds    map[string]int64  `json:"thresholds_ns"`
	SerialRatio   float64           `json:"median_t4_over_median_t1"`
}

type pfProfile struct {
	concurrentBatches int
	concurrentWaves   int
	soakWaves         int
	serialBatches     int
	serialWaves       int
	timeout           time.Duration
}

type pfBatch struct {
	t          *testing.T
	ctx        context.Context
	binary     string
	image      string
	id         string
	root       string
	dataDir    string
	socket     string
	daemon     *daemonProcess
	agents     []core.Agent
	project    core.Project
	initialSHA string
	parallel   int
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
		serialBatches: 1, serialWaves: 1, timeout: 25 * time.Minute,
	}
	if profile == "release" {
		settings = pfProfile{
			concurrentBatches: 4, concurrentWaves: 5, soakWaves: 15,
			serialBatches: 2, serialWaves: 5, timeout: 2 * time.Hour,
		}
	}
	release, err := testsupport.AcquireSerialResource(testsupport.DockerResource, "tests/e2e-pf01", settings.timeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	root := t.TempDir()
	source, initial, manifest := createPerfRepository(t, ctx, root)
	report := pfReport{
		SchemaVersion: 1, Scenario: "PF-01", Profile: profile, Revision: commandText(ctx, "git", "rev-parse", "HEAD"),
		Environment: perfEnvironment(ctx, image), Fixture: manifest,
		Thresholds: map[string]int64{"t_wave_p50": int64(60 * time.Second), "t_wave_p90": int64(90 * time.Second), "t_wave_max": int64(180 * time.Second)},
	}
	defer writePerfReport(t, output, &report)
	if reason := invalidPerfFixture(manifest); reason != "" {
		report.Result = "INVALID_ENVIRONMENT"
		report.Reason = reason
		return
	}
	for batchIndex := 0; batchIndex < settings.concurrentBatches; batchIndex++ {
		batchID := fmt.Sprintf("concurrent-%02d", batchIndex+1)
		batch := newPFBatch(t, ctx, coordplane, image, source, initial, filepath.Join(root, batchID), batchID, 4)
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
	}
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
	if os.Getenv("PF01_REFERENCE_RUNNER") != "1" {
		report.Result = "INVALID_ENVIRONMENT"
		report.Reason = "PF01_REFERENCE_RUNNER=1 and registered cgroup/storage fingerprint are required for a release PASS"
		return
	}
	report.Result = "INVALID_ENVIRONMENT"
	report.Reason = "PF-01 client/point/stage/resource records and the 5/3/3 crash table are not complete"
}

func newPFBatch(
	t *testing.T,
	ctx context.Context,
	binary, image, source, initial, root, id string,
	parallel int,
) *pfBatch {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	instructions := filepath.Join(root, "instructions.md")
	writeFile(t, instructions, []byte("Execute the deterministic PF-01 bootstrap contract.\n"), 0o600)
	config := writePerfConfig(t, root, dataDir, socket, image, parallel)
	daemon := startDaemon(t, binary, config, socket)
	registerLiveHomeCleanup(t, image, dataDir)
	waitForReady(t, ctx, binary, socket, id+" startup")
	roles := []string{"A", "B", "C", "D"}
	agents := make([]core.Agent, 4)
	for index, role := range roles {
		agents[index] = runJSON[core.Agent](t, ctx, binary, "agent", "add", "--socket", socket, "--display-name", "PF01 "+role, "--adapter", "codex", "--image", image, "--instructions-file", instructions, "--request-id", id+"-agent-"+role, "--output", "json")
	}
	project := runJSON[core.Project](t, ctx, binary, "project", "add", "--socket", socket, "--name", "PF01 "+id, "--repo", source, "--ref", "refs/heads/main", "--integration-agent", agents[0].ID, "--request-id", id+"-project", "--output", "json")
	if project.InitialSHA != initial || project.CanonicalSHA != initial {
		t.Fatalf("PF-01 batch %s started at %s/%s, want %s", id, project.InitialSHA, project.CanonicalSHA, initial)
	}
	return &pfBatch{
		t: t, ctx: ctx, binary: binary, image: image, id: id,
		root: root, dataDir: dataDir, socket: socket, daemon: daemon,
		agents: agents, project: project, initialSHA: initial, parallel: parallel,
	}
}

func (b *pfBatch) close() {
	b.t.Helper()
	if err := b.daemon.Stop(); err != nil {
		b.t.Fatalf("stop PF-01 batch %s: %v\n%s", b.id, err, readLog(b.daemon.logPath))
	}
}

func (b *pfBatch) runWave(name string, warmup, soak bool) pfSample {
	b.t.Helper()
	roles := []string{"A", "B", "C", "D"}
	waveID := b.id + "-" + name
	waveStart := time.Now()
	base := projectDetail(b.t, b.ctx, b.binary, b.socket, b.project.ID).ActualCanonicalSHA
	tasks := make([]core.Task, len(roles))
	for index, role := range roles {
		tasks[index] = runJSON[core.Task](b.t, b.ctx, b.binary, "task", "create", "--socket", b.socket, "--project", b.project.ID, "--agent", b.agents[index].ID, "--title", "PF01 "+waveID+" "+role, "--description", "p5_role="+role+";pf01=true;progress_count=50", "--request-id", waveID+"-task-"+role, "--output", "json")
		if tasks[index].BaseSHA != base {
			b.t.Fatalf("PF01 %s task base = %s want %s", waveID, tasks[index].BaseSHA, base)
		}
	}
	goBody := fmt.Sprintf("P5-GO wave=%s d_agent=%s d_task=%s", waveID, b.agents[3].ID, tasks[3].ID)
	var workStart time.Time
	if b.parallelism() == 4 {
		waitForTaskRuns(b.t, b.ctx, b.binary, b.socket, tasks)
		time.Sleep(time.Second)
		workStart = sendPFGo(b.t, b.ctx, b.binary, b.socket, b.project.ID, b.agents, tasks, goBody, waveID)
	} else {
		for index, role := range roles {
			waitForPFRun(b.t, b.ctx, b.binary, b.socket, tasks[index].ID, role+" serial READY")
			started := time.Now()
			if workStart.IsZero() {
				workStart = started
			}
			sendBossMessage(b.t, b.ctx, b.binary, b.socket, b.project.ID, b.agents[index].ID, tasks[index].ID, goBody, waveID+"-go-"+role)
			tasks[index] = waitForPFTask(b.t, b.ctx, b.binary, b.socket, tasks[index].ID, role+" serial submit", capturedTask)
		}
	}
	for index := range tasks {
		tasks[index] = waitForPFTask(b.t, b.ctx, b.binary, b.socket, tasks[index].ID, roles[index]+" submitted", capturedTask)
	}
	workDuration := time.Since(workStart)
	if b.parallelism() == 4 {
		for index := range tasks {
			runJSON[core.Task](b.t, b.ctx, b.binary, "task", "accept", tasks[index].ID, "--socket", b.socket, "--integration-agent", b.agents[0].ID, "--request-id", waveID+"-accept-"+roles[index], "--output", "json")
			tasks[index] = waitForPFTask(b.t, b.ctx, b.binary, b.socket, tasks[index].ID, roles[index]+" integrated", func(task core.Task) bool { return task.Status == core.TaskCompleted })
		}
	} else {
		for index := range tasks {
			tasks[index] = runJSON[core.Task](b.t, b.ctx, b.binary, "task", "cancel", tasks[index].ID, "--socket", b.socket, "--reason", "PF-01 serial comparison complete", "--request-id", waveID+"-cancel-"+roles[index], "--output", "json")
		}
	}
	final := projectDetail(b.t, b.ctx, b.binary, b.socket, b.project.ID).ActualCanonicalSHA
	control := filepath.Join(b.dataDir, "repos", b.project.ID+".git")
	if b.parallelism() == 4 {
		for _, task := range tasks {
			gitDirSucceeds(b.t, b.ctx, control, "merge-base", "--is-ancestor", task.HeadSHA, final)
		}
	} else if final != base {
		b.t.Fatalf("serial comparison moved canonical from %s to %s", base, final)
	}
	gitDirSucceeds(b.t, b.ctx, control, "fsck", "--full", "--strict")
	waveDuration := time.Since(waveStart)
	cleanupStart := time.Now()
	waitForNoProjectContainers(b.t, b.ctx, b.project.ID)
	runJSON[core.GCPreview](b.t, b.ctx, b.binary, "gc", "preview", "--socket", b.socket, "--output", "json")
	runJSON[core.GCRunResult](b.t, b.ctx, b.binary, "gc", "run", "--socket", b.socket, "--confirm", "--request-id", waveID+"-gc", "--output", "json")
	waitForWorkspacesRemoved(b.t, b.ctx, b.dataDir, b.project.ID, tasks[0].ID, tasks[1].ID, tasks[2].ID, tasks[3].ID)
	cleanupDuration := time.Since(cleanupStart)
	heads := make([]string, 4)
	for index := range tasks {
		heads[index] = tasks[index].HeadSHA
	}
	integrationTasks := 0
	if b.parallelism() == 4 {
		integrationTasks = 3
	}
	return pfSample{
		ID: waveID, BatchID: b.id, Parallelism: b.parallelism(), Warmup: warmup, Soak: soak,
		WaveNS: waveDuration.Nanoseconds(), WorkNS: workDuration.Nanoseconds(), CleanupNS: cleanupDuration.Nanoseconds(),
		ProgressOperations: 200, PeerMessages: 10, IntegrationTasks: integrationTasks,
		FinalSHA: final, TaskHeads: heads,
	}
}

func (b *pfBatch) parallelism() int {
	return b.parallel
}

func capturedTask(task core.Task) bool {
	return task.Status == core.TaskSubmitted && task.HeadSHA != "" && task.TaskRef != ""
}

func waitForPFTask(
	t *testing.T,
	ctx context.Context,
	binary, socket, taskID, reason string,
	predicate func(core.Task) bool,
) core.Task {
	t.Helper()
	return eventually(t, ctx, 3*time.Minute, reason, func() (core.Task, bool, string) {
		detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
		if err != nil {
			return core.Task{}, false, err.Error()
		}
		return detail.Task, predicate(detail.Task), fmt.Sprintf(
			"status=%s pending=%s integration=%s failure=%s",
			detail.Task.Status, detail.Task.PendingAction, detail.Task.IntegrationTaskID, detail.Task.FailureReason,
		)
	})
}

func waitForTaskRuns(t *testing.T, ctx context.Context, binary, socket string, tasks []core.Task) {
	for _, task := range tasks {
		waitForPFRun(t, ctx, binary, socket, task.ID, "PF01 READY")
	}
}

func waitForPFRun(t *testing.T, ctx context.Context, binary, socket, taskID, reason string) core.Run {
	t.Helper()
	run := eventually(t, ctx, 60*time.Second, reason, func() (core.Run, bool, string) {
		detail, err := commandJSON[core.TaskDetail](ctx, binary, "task", "show", taskID, "--socket", socket, "--output", "json")
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
	if inspect := inspectContainer(t, ctx, run.ContainerID); !inspect.State.Running {
		t.Fatalf("PF-01 READY Run %s is not live in Docker", run.ID)
	}
	return run
}

func sendPFGo(
	t *testing.T,
	ctx context.Context,
	binary, socket, projectID string,
	agents []core.Agent,
	tasks []core.Task,
	body, waveID string,
) time.Time {
	t.Helper()
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
			_, err := commandJSON[core.Message](ctx, binary, "message", "send", "--socket", socket,
				"--project", projectID, "--agent", agents[index].ID, "--task", tasks[index].ID,
				"--body", body, "--request-id", waveID+"-go-"+string(rune('A'+index)), "--output", "json")
			results <- result{started: started, err: err}
		}(index)
	}
	close(barrier)
	var earliest time.Time
	for range tasks {
		result := <-results
		if result.err != nil {
			t.Fatalf("send PF-01 GO: %v", result.err)
		}
		if earliest.IsZero() || result.started.Before(earliest) {
			earliest = result.started
		}
	}
	return earliest
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
	if err := os.MkdirAll(filepath.Join(repo, "bench"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
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
		if path == excluded {
			return filepath.SkipDir
		}
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

func matchingFileBytes(t *testing.T, root, suffix string) int64 {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), suffix) {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
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
