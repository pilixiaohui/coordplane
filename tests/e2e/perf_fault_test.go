//go:build e2e

package e2e_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coordplane/internal/core"
)

type pfFaultRow struct {
	SampleID       string         `json:"sample_id"`
	Kind           string         `json:"kind"`
	Index          int            `json:"index"`
	FreshDataDirID string         `json:"fresh_data_dir_id"`
	RecoveryNS     int64          `json:"t_recover_ns"`
	RunIDsBefore   []string       `json:"run_ids_before"`
	RunIDsAfter    []string       `json:"run_ids_after"`
	PreRestart     string         `json:"pre_restart_fact"`
	FinalSHA       string         `json:"final_sha"`
	GitFSCK        string         `json:"git_fsck"`
	Cleanup        string         `json:"cleanup"`
	DurableUnacked int            `json:"durable_unacknowledged_messages"`
	Counts         pfObjectCounts `json:"durable_counts"`
	Result         string         `json:"result"`
}

func runPFFaultTable(
	t *testing.T,
	ctx context.Context,
	binary, image, source, initial, root string,
	profile pfProfile,
) ([]pfFaultRow, []map[string]any, []pfObjectCounts, []pfDiskSample) {
	t.Helper()
	var rows []pfFaultRow
	var records []map[string]any
	var counts []pfObjectCounts
	var disks []pfDiskSample
	for _, test := range []struct {
		kind  string
		count int
	}{{"live", profile.liveFaults}, {"capture", profile.captureFaults}, {"cas", profile.casFaults}} {
		for index := 1; index <= test.count; index++ {
			id := fmt.Sprintf("fault-%s-%02d", test.kind, index)
			faultRoot := filepath.Join(root, id)
			var row pfFaultRow
			var batch *pfBatch
			switch test.kind {
			case "live":
				row, batch = runPFLiveFault(t, ctx, binary, image, source, initial, faultRoot, id, index)
			case "capture":
				row, batch = runPFPendingFault(t, ctx, binary, image, source, initial, faultRoot, id, "capture", index)
			case "cas":
				row, batch = runPFPendingFault(t, ctx, binary, image, source, initial, faultRoot, id, "cas", index)
			}
			batch.close()
			disks = append(disks, batch.diskFacts()...)
			row.Counts = batch.objectCounts()
			row.Cleanup = "absent"
			row.Result = "PASS"
			rows = append(rows, row)
			counts = append(counts, row.Counts)
			records = append(records, readObserverRecords(t, batch.observer)...)
		}
	}
	return rows, records, counts, disks
}

func runPFLiveFault(
	t *testing.T,
	ctx context.Context,
	binary, image, source, initial, root, id string,
	index int,
) (pfFaultRow, *pfBatch) {
	t.Helper()
	batch := newPFBatch(t, ctx, binary, image, source, initial, root, id, 4)
	tasks := make([]core.Task, len(pfRoles))
	base := projectDetail(t, ctx, binary, batch.socket, batch.project.ID).ActualCanonicalSHA
	for position, role := range pfRoles {
		tasks[position] = createPFFaultTask(t, batch, id, role, base)
	}
	runs := make([]core.Run, len(tasks))
	for position, task := range tasks {
		runs[position] = batch.waitRun(task.ID, pfRoles[position]+" live fault READY")
		sendBossMessage(t, ctx, binary, batch.socket, batch.project.ID, batch.agents[position].ID, task.ID, "PF01-HOLD", id+"-hold-"+pfRoles[position])
	}
	row := newPFFaultRow(id, "live", index, batch)
	for _, run := range runs {
		row.RunIDsBefore = append(row.RunIDsBefore, run.ID)
	}
	database, err := sql.Open("sqlite", filepath.Join(batch.dataDir, "coordplane.db"))
	requireNoError(t, err)
	if err := database.QueryRow(`SELECT count(*) FROM messages WHERE project_id=? AND body='PF01-HOLD' AND state<>'acknowledged'`, batch.project.ID).Scan(&row.DurableUnacked); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil || row.DurableUnacked != 4 {
		t.Fatalf("%s durable unacknowledged control Messages = %d, want 4: %v", id, row.DurableUnacked, err)
	}
	row.RecoveryNS = batch.restartAfterKill(time.Second, func() {
		for _, run := range runs {
			if !inspectContainer(t, ctx, run.ContainerID).State.Running {
				t.Fatalf("%s container %s exited after daemon SIGKILL", id, run.ContainerID)
			}
		}
	})
	for position, task := range tasks {
		detail := taskDetail(t, ctx, binary, batch.socket, task.ID)
		if detail.CurrentRun == nil || detail.CurrentRun.ID != runs[position].ID || detail.CurrentRun.ContainerID != runs[position].ContainerID {
			t.Fatalf("%s replacement Run = %#v, want adopted %#v", id, detail.CurrentRun, runs[position])
		}
		if !inspectContainer(t, ctx, detail.CurrentRun.ContainerID).State.Running {
			t.Fatalf("%s adopted container %s is not running", id, detail.CurrentRun.ContainerID)
		}
		row.RunIDsAfter = append(row.RunIDsAfter, detail.CurrentRun.ID)
	}
	goBody := fmt.Sprintf("P5-GO wave=%s d_agent=%s d_task=%s", id, batch.agents[3].ID, tasks[3].ID)
	batch.sendGO(tasks, goBody, id)
	tasks = finishPFFaultTasks(t, batch, tasks, pfRoles[:])
	row.PreRestart = "four live Runs and four unacknowledged durable Messages"
	finishPFFaultBatch(t, batch, tasks, &row)
	return row, batch
}

func runPFPendingFault(t *testing.T, ctx context.Context, binary, image, source, initial, root, id, kind string, index int) (pfFaultRow, *pfBatch) {
	t.Helper()
	ready := filepath.Join(root, kind+".ready.fault")
	environment := []string{"COORDPLANE_CONTRACT_CAPTURE_PHASE=task_ref_written", "COORDPLANE_CONTRACT_CAPTURE_PHASE_READY=" + ready}
	if kind == "cas" {
		environment = []string{"COORDPLANE_PERF_FAULT=git.advance.after_update_ref", "COORDPLANE_PERF_FAULT_READY=" + ready}
	}
	batch := newPFBatchWithEnv(t, ctx, binary, image, source, initial, root, id, 1, environment)
	base := projectDetail(t, ctx, binary, batch.socket, batch.project.ID).ActualCanonicalSHA
	task := createPFFaultTask(t, batch, id, "A", base)
	run := batch.waitRun(task.ID, kind+" fault READY")
	sendPFFaultGO(t, batch, task, "A")
	var acceptDone chan error
	status, pending := "finishing", "capture"
	if kind == "cas" {
		task = batch.waitTask(task.ID, "CAS source submitted", capturedTask)
		acceptDone = make(chan error, 1)
		go func() {
			_, err := commandOutput(ctx, "", binary, "task", "accept", task.ID, "--socket", batch.socket, "--integration-agent", batch.agents[0].ID, "--request-id", id+"-accept", "--output", "json")
			acceptDone <- err
		}()
		status, pending = "submitted", "advance"
	}
	waitForPFPath(t, ctx, ready)
	runID := run.ID
	if task.HeadRunID != "" {
		runID = task.HeadRunID
	}
	row := newPFFaultRow(id, kind, index, batch)
	row.RunIDsBefore = []string{runID}
	row.PreRestart = assertPFFaultBoundary(t, batch, task, runID, status, pending)
	row.RecoveryNS = batch.restartAfterKill(0, nil)
	if acceptDone != nil {
		select {
		case <-acceptDone:
		case <-time.After(10 * time.Second):
			t.Fatal("accept CLI did not exit after daemon SIGKILL")
		}
	} else {
		task = batch.waitTask(task.ID, "capture recovered", capturedTask)
		runJSON[core.Task](t, ctx, binary, "task", "accept", task.ID, "--socket", batch.socket, "--integration-agent", batch.agents[0].ID, "--request-id", id+"-accept", "--output", "json")
	}
	task = batch.waitTask(task.ID, kind+" recovered", func(task core.Task) bool { return task.Status == core.TaskCompleted })
	row.RunIDsAfter = []string{task.HeadRunID}
	finishPFFaultBatch(t, batch, []core.Task{task}, &row)
	return row, batch
}

func createPFFaultTask(t *testing.T, batch *pfBatch, id, role, base string) core.Task {
	t.Helper()
	index := int(role[0] - 'A')
	task := runJSON[core.Task](t, batch.ctx, batch.binary, "task", "create", "--socket", batch.socket,
		"--project", batch.project.ID, "--agent", batch.agents[index].ID, "--title", id+" "+role,
		"--description", "p5_role="+role+";pf01=true;progress_count=2", "--request-id", id+"-task-"+role, "--output", "json")
	if task.BaseSHA != base {
		t.Fatalf("%s fault Task base = %s, want %s", id, task.BaseSHA, base)
	}
	return task
}

func sendPFFaultGO(t *testing.T, batch *pfBatch, task core.Task, role string) {
	t.Helper()
	index := int(role[0] - 'A')
	body := fmt.Sprintf("P5-GO wave=%s d_agent=%s d_task=%s", batch.id, batch.agents[index].ID, task.ID)
	sendBossMessage(t, batch.ctx, batch.binary, batch.socket, batch.project.ID, batch.agents[index].ID, task.ID, body, batch.id+"-go-"+role)
}

func finishPFFaultTasks(t *testing.T, batch *pfBatch, tasks []core.Task, roles []string) []core.Task {
	t.Helper()
	for position := range tasks {
		tasks[position] = batch.waitTask(tasks[position].ID, roles[position]+" fault submitted", capturedTask)
	}
	for position := range tasks {
		runJSON[core.Task](t, batch.ctx, batch.binary, "task", "accept", tasks[position].ID, "--socket", batch.socket,
			"--integration-agent", batch.agents[0].ID, "--request-id", batch.id+"-accept-"+roles[position], "--output", "json")
		tasks[position] = batch.waitTask(tasks[position].ID, roles[position]+" fault integrated", func(task core.Task) bool { return task.Status == core.TaskCompleted })
	}
	return tasks
}

func finishPFFaultBatch(t *testing.T, batch *pfBatch, tasks []core.Task, row *pfFaultRow) {
	t.Helper()
	control := filepath.Join(batch.dataDir, "repos", batch.project.ID+".git")
	row.FinalSHA = projectDetail(t, batch.ctx, batch.binary, batch.socket, batch.project.ID).ActualCanonicalSHA
	var workspaceIDs []string
	for _, task := range tasks {
		gitDirSucceeds(t, batch.ctx, control, "merge-base", "--is-ancestor", task.HeadSHA, row.FinalSHA)
		workspaceIDs = append(workspaceIDs, task.ID)
		if task.IntegrationTaskID != "" {
			workspaceIDs = append(workspaceIDs, task.IntegrationTaskID)
		}
	}
	gitDirSucceeds(t, batch.ctx, control, "fsck", "--full", "--strict")
	row.GitFSCK = "pass"
	waitForNoProjectContainers(t, batch.ctx, batch.project.ID)
	runJSON[core.GCPreview](t, batch.ctx, batch.binary, "gc", "preview", "--socket", batch.socket, "--output", "json")
	batch.recordDisk("gc_preview")
	runJSON[core.GCRunResult](t, batch.ctx, batch.binary, "gc", "run", "--socket", batch.socket, "--confirm", "--request-id", batch.id+"-gc", "--output", "json")
	waitForWorkspacesRemoved(t, batch.ctx, batch.dataDir, batch.project.ID, workspaceIDs...)
	batch.recordDisk("gc_complete")
}

func (b *pfBatch) restartAfterKill(delay time.Duration, afterKill func()) int64 {
	b.t.Helper()
	if err := b.daemon.Kill(); err != nil {
		b.t.Fatal(err)
	}
	if afterKill != nil {
		afterKill()
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	_ = os.Remove(b.socket)
	started := time.Now()
	b.daemon = startPerfDaemon(b.t, b.binary, b.image, b.root, filepath.Join(b.root, "coordplane.yaml"), b.socket, b.id, []string{
		"COORDPLANE_PERF_OBSERVER_OUTPUT=" + b.observer,
		"COORDPLANE_PERF_SAMPLE_ID=" + b.id,
		"COORDPLANE_PERF_DATA_DIR=" + b.dataDir,
		"GOMAXPROCS=3",
		"GOMEMLIMIT=384MiB",
	})
	waitForReady(b.t, b.ctx, b.binary, b.socket, b.id+" replacement ready")
	return time.Since(started).Nanoseconds()
}

func assertPFFaultBoundary(t *testing.T, batch *pfBatch, task core.Task, runID, status, pending string) string {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(batch.dataDir, "coordplane.db"))
	requireNoError(t, err)
	defer database.Close()
	var actualStatus, actualPending, head string
	requireNoError(t, database.QueryRow("SELECT status,pending_action,head_sha FROM tasks WHERE id=?", task.ID).Scan(&actualStatus, &actualPending, &head))
	if actualStatus != status || actualPending != pending {
		t.Fatalf("fault boundary Task = %s/%s, want %s/%s", actualStatus, actualPending, status, pending)
	}
	control := filepath.Join(batch.dataDir, "repos", batch.project.ID+".git")
	if pending == "capture" {
		ref := "refs/coordplane/tasks/" + task.ID + "/runs/" + runID
		actual := gitDir(t, batch.ctx, control, "rev-parse", ref+"^{commit}")
		if head != "" {
			t.Fatalf("capture fault wrote DB head before restart: %s", head)
		}
		return "task_ref=" + actual + ";db=finishing/capture;head_absent=true"
	}
	actual := gitDir(t, batch.ctx, control, "rev-parse", batch.project.CanonicalRef+"^{commit}")
	if actual != task.HeadSHA || head != task.HeadSHA {
		t.Fatalf("CAS fault facts canonical=%s db_head=%s want=%s", actual, head, task.HeadSHA)
	}
	return "canonical=" + actual + ";db=submitted/advance"
}

func newPFFaultRow(id, kind string, index int, batch *pfBatch) pfFaultRow {
	return pfFaultRow{SampleID: id, Kind: kind, Index: index, FreshDataDirID: pfDataDirID(batch.dataDir)}
}

func waitForPFPath(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	eventually(t, ctx, 90*time.Second, filepath.Base(path), func() (bool, bool, string) {
		_, err := os.Stat(path)
		return err == nil, err == nil, fmt.Sprint(err)
	})
}
