package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
	"coordplane/internal/transport"
)

var testBinaries struct {
	directory          string
	coordplane         string
	coordplaneContract string
	coordlink          string
}

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "coordplane-p1-contract-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root := repositoryRoot()
	testBinaries.directory = directory
	testBinaries.coordplane = filepath.Join(directory, "coordplane")
	testBinaries.coordplaneContract = filepath.Join(directory, "coordplane-contract")
	testBinaries.coordlink = filepath.Join(directory, "coordlink")
	builds := []struct {
		command string
		output  string
		tags    string
	}{
		{command: "./cmd/coordplane", output: testBinaries.coordplane},
		{command: "./cmd/coordplane", output: testBinaries.coordplaneContract, tags: "contract"},
		{command: "./cmd/coordlink", output: testBinaries.coordlink, tags: "contract"},
	}
	code := 0
	for _, target := range builds {
		args := []string{"build", "-buildvcs=false"}
		if target.tags != "" {
			args = append(args, "-tags="+target.tags)
		}
		args = append(args, "-o", target.output, target.command)
		build := exec.Command("go", args...)
		build.Dir = root
		if raw, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", target.command, err, raw)
			code = 1
			break
		}
	}
	if code == 0 {
		code = m.Run()
	}
	if err := os.RemoveAll(directory); err != nil {
		fmt.Fprintf(os.Stderr, "remove contract binaries: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestGT00DaemonBinaryRecoversEveryInitializationPhase(t *testing.T) {
	phases := []string{
		"intent_committed",
		"partial_prepared",
		"bare_initialized",
		"objects_imported",
		"canonical_written",
		"integrity_verified",
		"promoted",
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "cp-gt00-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			dataDir := filepath.Join(root, "data")
			socket := filepath.Join(dataDir, "operator.sock")
			configPath := writeConfig(t, root, dataDir, socket, "")
			source := createRepository(t, root)
			initial := strings.TrimSpace(git(t, source, "rev-parse", "refs/heads/main"))
			readyPath := filepath.Join(root, "git-phase-ready")
			daemon := startDaemonBinaryWithEnv(t, testBinaries.coordplaneContract, configPath, socket, []string{
				"COORDPLANE_CONTRACT_GIT_PHASE=" + phase,
				"COORDPLANE_CONTRACT_GIT_PHASE_READY=" + readyPath,
			})

			var addOutput lockedBuffer
			add := exec.Command(testBinaries.coordplaneContract,
				"project", "add", "--socket", socket, "--name", "phase-"+phase,
				"--repo", source, "--ref", "refs/heads/main",
				"--request-id", "phase-"+phase, "--output", "json")
			add.Stdout = &addOutput
			add.Stderr = &addOutput
			if err := add.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, readyPath)
			killDaemon(t, daemon)
			if err := add.Wait(); err == nil {
				t.Fatalf("project add survived daemon kill: %s", addOutput.String())
			}

			if err := os.WriteFile(filepath.Join(source, "advanced.txt"), []byte(phase+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, source, "add", "advanced.txt")
			git(t, source, "commit", "-m", "advance source after "+phase)
			advanced := strings.TrimSpace(git(t, source, "rev-parse", "refs/heads/main"))
			if advanced == initial {
				t.Fatal("source ref did not advance after initialization intent")
			}
			sourceStatus := git(t, source, "status", "--porcelain=v1", "--untracked-files=all")
			sourceConfig, err := os.ReadFile(filepath.Join(source, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}

			daemon = startDaemon(t, configPath, socket)
			t.Cleanup(func() { stopDaemon(t, daemon, socket) })
			projectsRaw := runBinaryJSON(t, testBinaries.coordplane,
				"project", "list", "--socket", socket, "--output", "json")
			var projects core.ProjectPage
			decodeJSON(t, projectsRaw, &projects)
			if len(projects.Items) != 1 {
				t.Fatalf("recovered projects = %#v", projects.Items)
			}
			projectRaw := runBinaryJSON(t, testBinaries.coordplane,
				"project", "show", projects.Items[0].ID, "--socket", socket, "--output", "json")
			var project core.ProjectDetail
			decodeJSON(t, projectRaw, &project)
			if project.Status != core.ProjectActive || project.PendingAction != "" ||
				project.InitialSHA != initial || project.CanonicalSHA != initial ||
				project.ActualCanonicalSHA != initial {
				t.Fatalf("recovered project = %#v, want immutable initial %s", project, initial)
			}
			controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
			if got := strings.TrimSpace(git(t, controlRepo, "rev-parse", "refs/heads/main^{commit}")); got != initial {
				t.Fatalf("recovered canonical = %s, want %s", got, initial)
			}
			git(t, controlRepo, "fsck", "--full", "--strict")
			if got := strings.TrimSpace(git(t, source, "rev-parse", "refs/heads/main")); got != advanced {
				t.Fatalf("recovery rewrote source ref: got %s want %s", got, advanced)
			}
			if got := git(t, source, "status", "--porcelain=v1", "--untracked-files=all"); got != sourceStatus {
				t.Fatalf("recovery changed source worktree: before=%q after=%q", sourceStatus, got)
			}
			afterConfig, err := os.ReadFile(filepath.Join(source, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterConfig, sourceConfig) {
				t.Fatal("recovery changed source Git config")
			}

			eventsRaw := runBinaryJSON(t, testBinaries.coordplane,
				"events", "tail", "--socket", socket, "--project", project.ID, "--output", "json")
			var eventPage core.EventPage
			decodeJSON(t, eventsRaw, &eventPage)
			events := eventPage.Items
			if len(events) != 2 || events[0].Kind != "project.creating" || events[1].Kind != "project.active" ||
				events[0].OperationID == "" || events[0].OperationID != events[1].OperationID ||
				events[0].ID >= events[1].ID {
				t.Fatalf("recovered project events = %#v", events)
			}
		})
	}
}

func TestGT00ProductionBinaryHasNoContractFaultControl(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "")
	source := createRepository(t, root)
	readyPath := filepath.Join(root, "must-not-exist")
	daemon := startDaemonBinaryWithEnv(t, testBinaries.coordplane, configPath, socket, []string{
		"COORDPLANE_CONTRACT_GIT_PHASE=partial_prepared",
		"COORDPLANE_CONTRACT_GIT_PHASE_READY=" + readyPath,
	})
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, testBinaries.coordplane,
		"project", "add", "--socket", socket, "--name", "production-project",
		"--repo", source, "--ref", "refs/heads/main",
		"--request-id", "production-project", "--output", "json")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("production project add used contract fault control: %v\n%s", err, raw)
	}
	var project core.Project
	decodeJSON(t, raw, &project)
	if project.Status != core.ProjectActive {
		t.Fatalf("production project = %#v", project)
	}
	if _, err := os.Stat(readyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("production binary published contract phase marker: %v", err)
	}
}

func TestP1OperatorBinaryUnixCutoverAndRestart(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "")
	source := createRepository(t, root)
	sourceRefBefore := git(t, source, "rev-parse", "refs/heads/main")
	sourceStatusBefore := git(t, source, "status", "--porcelain=v1", "--untracked-files=all")
	sourceConfigBefore, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := startDaemon(t, configPath, socket)

	agentRaw := runBinaryJSON(t, testBinaries.coordplane,
		"agent", "add", "--socket", socket, "--display-name", "Developer",
		"--adapter", "one-shot", "--image", "agent:latest",
		"--instructions-file", filepath.Join(root, "agent.md"),
		"--request-id", "binary-agent", "--output", "json")
	var agent core.Agent
	decodeJSON(t, agentRaw, &agent)
	projectRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "add", "--socket", socket, "--name", "binary-project",
		"--repo", source, "--ref", "refs/heads/main", "--integration-agent", agent.ID,
		"--request-id", "binary-project", "--output", "json")
	var project core.Project
	decodeJSON(t, projectRaw, &project)
	if project.Status != core.ProjectActive || project.InitialSHA != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("project = %#v", project)
	}
	firstRaw := runBinaryJSON(t, testBinaries.coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", "first", "--request-id", "chat-first", "--output", "json")
	secondRaw := runBinaryJSON(t, testBinaries.coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", "second", "--request-id", "chat-second", "--output", "json")
	var first, second core.ChatResult
	decodeJSON(t, firstRaw, &first)
	decodeJSON(t, secondRaw, &second)
	if first.Task.ID != second.Task.ID || first.Message.ID == second.Message.ID {
		t.Fatalf("chat results = %#v / %#v", first, second)
	}
	statusRaw := runBinaryJSON(t, testBinaries.coordplane, "status", "--socket", socket, "--project", project.ID, "--output", "json")
	var status core.Status
	decodeJSON(t, statusRaw, &status)
	if !status.DaemonReady || len(status.Snapshot.Projects) != 1 || len(status.Snapshot.Agents) != 1 ||
		len(status.Snapshot.Tasks) != 0 || len(status.Snapshot.Runs) != 0 || len(status.Snapshot.Messages) != 0 {
		t.Fatalf("status = %#v", status)
	}
	if len(status.ActualRefs) != 1 || strings.TrimSpace(status.ActualRefs[0].ActualSHA) != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("status actual Git facts = %#v", status.ActualRefs)
	}
	if len(status.Tasks) != 1 || status.Tasks[0].Task.ID != first.Task.ID || status.Tasks[0].PendingMessageCount != 2 {
		t.Fatalf("status conversation projection = %#v", status.Tasks)
	}
	taskRaw := runBinaryJSON(t, testBinaries.coordplane,
		"task", "show", first.Task.ID, "--socket", socket, "--output", "json")
	var taskDetail core.TaskDetail
	decodeJSON(t, taskRaw, &taskDetail)
	if taskDetail.Task.ID != first.Task.ID || taskDetail.PendingMessageCount != 2 || strings.TrimSpace(taskDetail.ActualCanonicalSHA) != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("task show projection = %#v", taskDetail)
	}
	messagesBeforeRaw := runBinaryJSON(t, testBinaries.coordplane,
		"message", "list", "--socket", socket, "--task", first.Task.ID, "--output", "json")
	var messagesBefore core.MessagePage
	decodeJSON(t, messagesBeforeRaw, &messagesBefore)
	eventsBeforeRaw := runBinaryJSON(t, testBinaries.coordplane,
		"events", "tail", "--socket", socket, "--output", "json")
	var eventsBefore core.EventPage
	decodeJSON(t, eventsBeforeRaw, &eventsBefore)
	beforeRestart, err := json.Marshal(struct {
		Status   core.Status      `json:"status"`
		Task     core.TaskDetail  `json:"task"`
		Messages core.MessagePage `json:"messages"`
		Events   core.EventPage   `json:"events"`
	}{status, taskDetail, messagesBefore, eventsBefore})
	if err != nil {
		t.Fatal(err)
	}

	secondDaemon := exec.Command(testBinaries.coordplane, "serve", "--config", configPath)
	secondOutput, err := secondDaemon.CombinedOutput()
	if err == nil || !bytes.Contains(secondOutput, []byte("already locked")) {
		t.Fatalf("second daemon err=%v output=%s", err, secondOutput)
	}
	if raw, err := exec.Command(testBinaries.coordplane, "task", "run", "--backend-url", "http://unused").CombinedOutput(); err == nil || !bytes.Contains(raw, []byte("unknown task subcommand")) {
		t.Fatalf("legacy CLI err=%v output=%s", err, raw)
	}

	stopDaemon(t, daemon, socket)
	daemon = startDaemon(t, configPath, socket)
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })
	statusAfterRaw := runBinaryJSON(t, testBinaries.coordplane, "status", "--socket", socket, "--project", project.ID, "--output", "json")
	var statusAfter core.Status
	decodeJSON(t, statusAfterRaw, &statusAfter)
	taskAfterRaw := runBinaryJSON(t, testBinaries.coordplane,
		"task", "show", first.Task.ID, "--socket", socket, "--output", "json")
	var taskAfter core.TaskDetail
	decodeJSON(t, taskAfterRaw, &taskAfter)
	messagesRaw := runBinaryJSON(t, testBinaries.coordplane,
		"message", "list", "--socket", socket, "--task", first.Task.ID, "--output", "json")
	var messages core.MessagePage
	decodeJSON(t, messagesRaw, &messages)
	if len(messages.Items) != 2 || messages.Items[0].Body != "first" || messages.Items[1].Body != "second" {
		t.Fatalf("messages after restart = %#v", messages)
	}
	eventsAfterRaw := runBinaryJSON(t, testBinaries.coordplane,
		"events", "tail", "--socket", socket, "--output", "json")
	var eventsAfter core.EventPage
	decodeJSON(t, eventsAfterRaw, &eventsAfter)
	afterRestart, err := json.Marshal(struct {
		Status   core.Status      `json:"status"`
		Task     core.TaskDetail  `json:"task"`
		Messages core.MessagePage `json:"messages"`
		Events   core.EventPage   `json:"events"`
	}{statusAfter, taskAfter, messages, eventsAfter})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRestart, beforeRestart) {
		t.Fatalf("restart changed row versions or Event IDs/order\nbefore=%s\nafter=%s", beforeRestart, afterRestart)
	}
	if got := git(t, source, "rev-parse", "refs/heads/main"); got != sourceRefBefore {
		t.Fatalf("source ref changed: before=%s after=%s", sourceRefBefore, got)
	}
	if got := git(t, source, "status", "--porcelain=v1", "--untracked-files=all"); got != sourceStatusBefore {
		t.Fatalf("source status changed: before=%q after=%q", sourceStatusBefore, got)
	}
	sourceConfigAfter, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceConfigBefore, sourceConfigAfter) {
		t.Fatal("project registration modified source Git config")
	}

	stopDaemon(t, daemon, socket)
	controlRepo := filepath.Join(dataDir, "repos", project.ID+".git")
	if err := os.WriteFile(filepath.Join(source, "canonical-advanced.txt"), []byte("advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, source, "add", "canonical-advanced.txt")
	git(t, source, "commit", "-m", "advance canonical before loss")
	advancedSHA := strings.TrimSpace(git(t, source, "rev-parse", "refs/heads/main"))
	git(t, controlRepo, "fetch", "--no-tags", source, advancedSHA)
	git(t, controlRepo, "update-ref", "refs/heads/main", advancedSHA, strings.TrimSpace(sourceRefBefore))
	missingRepo := filepath.Join(root, "missing-control.git")
	if err := os.Rename(controlRepo, missingRepo); err != nil {
		t.Fatal(err)
	}
	daemon = startDaemon(t, configPath, socket)
	failedRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "show", project.ID, "--socket", socket, "--output", "json")
	var failedView struct {
		core.Project
		ActualCanonicalError string `json:"actual_canonical_error"`
	}
	decodeJSON(t, failedRaw, &failedView)
	if failedView.Status != core.ProjectError || failedView.LastError == "" || failedView.ActualCanonicalError == "" {
		t.Fatalf("project did not fail closed after control repo loss: %#v", failedView)
	}
	repair := exec.Command(testBinaries.coordplane,
		"project", "repair", project.ID, "--socket", socket,
		"--request-id", "binary-project-repair-missing", "--output", "json")
	repairRaw, repairErr := repair.CombinedOutput()
	if repairErr == nil || !bytes.Contains(repairRaw, []byte(core.CodeGitInvariantViolation)) {
		t.Fatalf("missing active repo repair err=%v output=%s", repairErr, repairRaw)
	}
	if _, err := os.Stat(controlRepo); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair recreated a formerly active control repo: %v", err)
	}
	stillFailedRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "show", project.ID, "--socket", socket, "--output", "json")
	decodeJSON(t, stillFailedRaw, &failedView)
	if failedView.Status != core.ProjectError || failedView.CanonicalSHA != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("failed repair changed project truth: %#v", failedView)
	}

	if err := os.Rename(missingRepo, controlRepo); err != nil {
		t.Fatal(err)
	}
	repairedRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "repair", project.ID, "--socket", socket,
		"--request-id", "binary-project-repair-restored", "--output", "json")
	var repaired core.Project
	decodeJSON(t, repairedRaw, &repaired)
	if repaired.Status != core.ProjectActive || repaired.InitialSHA != strings.TrimSpace(sourceRefBefore) || repaired.CanonicalSHA != advancedSHA {
		t.Fatalf("repaired project = %#v", repaired)
	}
	if got := strings.TrimSpace(git(t, controlRepo, "rev-parse", "refs/heads/main^{commit}")); got != advancedSHA {
		t.Fatalf("repaired canonical = %s, want restored actual %s", got, advancedSHA)
	}
}

func TestP1ServeRejectsUnknownConfigBeforeCreatingDatabase(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "unknown_field: true\n")
	command := exec.Command(testBinaries.coordplane, "serve", "--config", configPath)
	raw, err := command.CombinedOutput()
	if err == nil || !bytes.Contains(raw, []byte("field unknown_field not found")) {
		t.Fatalf("serve err=%v output=%s", err, raw)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "coordplane.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config created database: %v", err)
	}
}

func TestP1BinaryReadSurfacesStayBoundedPastTwoMiBLedger(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "cp-bounded-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := writeConfig(t, root, dataDir, socket, "")
	source := createRepository(t, root)
	daemon := startDaemon(t, configPath, socket)
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })

	agentRaw := runBinaryJSON(t, testBinaries.coordplane,
		"agent", "add", "--socket", socket, "--display-name", "Bounded Agent",
		"--adapter", "one-shot", "--image", "agent:latest", "--instructions-file", filepath.Join(root, "agent.md"),
		"--request-id", "bounded-agent", "--output", "json")
	var agent core.Agent
	decodeJSON(t, agentRaw, &agent)
	projectRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "add", "--socket", socket, "--name", "bounded-project",
		"--repo", source, "--ref", "refs/heads/main", "--request-id", "bounded-project", "--output", "json")
	var project core.Project
	decodeJSON(t, projectRaw, &project)

	body := strings.Repeat("x", core.MaximumMessageBodyBytes)
	messageCount := 34
	var taskID string
	for index := 0; index < messageCount; index++ {
		raw := runBinaryJSON(t, testBinaries.coordplane,
			"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
			"--body", body, "--request-id", fmt.Sprintf("bounded-chat-%02d", index), "--output", "json")
		if len(raw) >= 2<<20 {
			t.Fatalf("chat response %d is unbounded: %d bytes", index, len(raw))
		}
		var result core.ChatResult
		decodeJSON(t, raw, &result)
		taskID = result.Task.ID
	}
	controlBody := strings.Repeat("\x01", core.MaximumMessageBodyBytes)
	controlRaw := runBinaryJSON(t, testBinaries.coordplane,
		"chat", "--socket", socket, "--project", project.ID, "--agent", agent.ID,
		"--body", controlBody, "--request-id", "bounded-chat-control", "--output", "json")
	if len(controlRaw) >= 2<<20 {
		t.Fatalf("control-character chat response is unbounded: %d bytes", len(controlRaw))
	}
	messageCount++
	if messageCount*core.MaximumMessageBodyBytes <= 2<<20 {
		t.Fatal("test fixture did not exceed the old 2 MiB ledger threshold")
	}

	stopDaemon(t, daemon, socket)
	database, err := store.Open(context.Background(), filepath.Join(dataDir, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := core.NewService(database, &contractGit{sha: project.InitialSHA, root: filepath.Join(dataDir, "repos")}, core.ServiceOptions{MaxParallelRuns: 1})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	claim, ok, err := service.ClaimNext(context.Background(), project.ID)
	if err != nil || !ok || claim.Task.ID != taskID {
		_ = database.Close()
		t.Fatalf("seed bounded run: claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if _, err := service.ActivateRun(context.Background(), claim.Run.ID, "bounded-run-active"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	daemon = startDaemon(t, configPath, socket)

	statusRaw := runBinaryJSON(t, testBinaries.coordplane,
		"status", "--socket", socket, "--project", project.ID, "--output", "json")
	if len(statusRaw) >= 2<<20 {
		t.Fatalf("status response is unbounded: %d bytes", len(statusRaw))
	}
	var status core.Status
	decodeJSON(t, statusRaw, &status)
	if len(status.Tasks) != 1 || status.Tasks[0].Task.ID != taskID || status.Tasks[0].PendingMessageCount != messageCount {
		t.Fatalf("bounded status lost current ledger counts: %#v", status.Tasks)
	}
	if status.Tasks[0].CurrentRun == nil || status.Tasks[0].CurrentRun.ID != claim.Run.ID {
		t.Fatalf("bounded status lost current Run: %#v", status.Tasks[0])
	}

	projectListRaw := runBinaryJSON(t, testBinaries.coordplane, "project", "list", "--socket", socket, "--output", "json")
	var projects core.ProjectPage
	decodeJSON(t, projectListRaw, &projects)
	if len(projectListRaw) >= 2<<20 || len(projects.Items) != 1 || projects.Items[0].ID != project.ID {
		t.Fatalf("project list response=%d page=%#v", len(projectListRaw), projects)
	}
	projectShowRaw := runBinaryJSON(t, testBinaries.coordplane, "project", "show", project.ID, "--socket", socket, "--output", "json")
	var projectDetail core.ProjectDetail
	decodeJSON(t, projectShowRaw, &projectDetail)
	if len(projectShowRaw) >= 2<<20 || projectDetail.ID != project.ID || projectDetail.ActualCanonicalSHA == "" {
		t.Fatalf("project show response=%d detail=%#v", len(projectShowRaw), projectDetail)
	}

	agentListRaw := runBinaryJSON(t, testBinaries.coordplane, "agent", "list", "--socket", socket, "--output", "json")
	var agents core.AgentPage
	decodeJSON(t, agentListRaw, &agents)
	if len(agentListRaw) >= 2<<20 || len(agents.Items) != 1 || agents.Items[0].ID != agent.ID {
		t.Fatalf("agent list response=%d page=%#v", len(agentListRaw), agents)
	}
	agentShowRaw := runBinaryJSON(t, testBinaries.coordplane, "agent", "show", agent.ID, "--socket", socket, "--output", "json")
	var shownAgent core.Agent
	decodeJSON(t, agentShowRaw, &shownAgent)
	if len(agentShowRaw) >= 2<<20 || shownAgent.ID != agent.ID {
		t.Fatalf("agent show response=%d agent=%#v", len(agentShowRaw), shownAgent)
	}

	taskListRaw := runBinaryJSON(t, testBinaries.coordplane, "task", "list", "--project", project.ID, "--socket", socket, "--output", "json")
	var tasks core.TaskPage
	decodeJSON(t, taskListRaw, &tasks)
	if len(taskListRaw) >= 2<<20 || len(tasks.Items) != 1 || tasks.Items[0].ID != taskID {
		t.Fatalf("task list response=%d page=%#v", len(taskListRaw), tasks)
	}
	taskShowRaw := runBinaryJSON(t, testBinaries.coordplane, "task", "show", taskID, "--socket", socket, "--output", "json")
	var taskDetail core.TaskDetail
	decodeJSON(t, taskShowRaw, &taskDetail)
	if len(taskShowRaw) >= 2<<20 || taskDetail.Task.ID != taskID || taskDetail.PendingMessageCount != messageCount {
		t.Fatalf("task show response=%d detail=%#v", len(taskShowRaw), taskDetail)
	}

	runListRaw := runBinaryJSON(t, testBinaries.coordplane, "run", "list", "--project", project.ID, "--socket", socket, "--output", "json")
	var runs core.RunPage
	decodeJSON(t, runListRaw, &runs)
	if len(runListRaw) >= 2<<20 || len(runs.Items) != 1 || runs.Items[0].ID != claim.Run.ID {
		t.Fatalf("run list response=%d page=%#v", len(runListRaw), runs)
	}
	runShowRaw := runBinaryJSON(t, testBinaries.coordplane, "run", "show", claim.Run.ID, "--socket", socket, "--output", "json")
	var shownRun core.Run
	decodeJSON(t, runShowRaw, &shownRun)
	if len(runShowRaw) >= 2<<20 || shownRun.ID != claim.Run.ID || shownRun.State != core.RunActive {
		t.Fatalf("run show response=%d run=%#v", len(runShowRaw), shownRun)
	}

	cursor := ""
	seenMessages := 0
	seenBytes := 0
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		args := []string{"message", "list", "--socket", socket, "--task", taskID, "--output", "json"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		raw := runBinaryJSON(t, testBinaries.coordplane, args...)
		if len(raw) >= 2<<20 {
			t.Fatalf("message page %d is unbounded: %d bytes", pageNumber, len(raw))
		}
		var page core.MessagePage
		decodeJSON(t, raw, &page)
		if len(page.Items) == 0 {
			t.Fatalf("message page %d made no cursor progress", pageNumber)
		}
		for _, message := range page.Items {
			seenMessages++
			seenBytes += len(message.Body)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" || seenMessages != messageCount || seenBytes <= 2<<20 {
		t.Fatalf("message pagination cursor=%q count=%d bytes=%d", cursor, seenMessages, seenBytes)
	}

	eventCursor := ""
	seenEventIDs := make(map[int64]bool)
	var previousOldest int64
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		args := []string{"events", "tail", "--socket", socket, "--project", project.ID, "--output", "json"}
		if eventCursor != "" {
			args = append(args, "--cursor", eventCursor)
		}
		raw := runBinaryJSON(t, testBinaries.coordplane, args...)
		if len(raw) >= 2<<20 {
			t.Fatalf("event page %d is unbounded: %d bytes", pageNumber, len(raw))
		}
		var page core.EventPage
		decodeJSON(t, raw, &page)
		if len(page.Items) == 0 || len(page.Items) > core.EventPageLimit {
			t.Fatalf("event page %d made invalid progress: %#v", pageNumber, page)
		}
		for index, event := range page.Items {
			if index > 0 && page.Items[index-1].ID >= event.ID {
				t.Fatalf("event page %d is not ascending: %#v", pageNumber, page.Items)
			}
			if seenEventIDs[event.ID] {
				t.Fatalf("event ID %d appeared in more than one binary page", event.ID)
			}
			seenEventIDs[event.ID] = true
		}
		if newest := page.Items[len(page.Items)-1].ID; previousOldest > 0 && newest >= previousOldest {
			t.Fatalf("event page %d did not move backward: newest=%d previous_oldest=%d", pageNumber, newest, previousOldest)
		}
		previousOldest = page.Items[0].ID
		eventCursor = page.NextCursor
		if eventCursor == "" {
			break
		}
	}
	if eventCursor != "" || len(seenEventIDs) <= core.EventPageLimit {
		t.Fatalf("event traversal cursor=%q count=%d", eventCursor, len(seenEventIDs))
	}

	stopDaemon(t, daemon, socket)
	database, err = store.Open(context.Background(), filepath.Join(dataDir, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	durableEvents, err := database.Events(context.Background(), core.EventFilter{ProjectID: project.ID})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if len(seenEventIDs) != len(durableEvents) {
		t.Fatalf("binary event traversal returned %d of %d durable events", len(seenEventIDs), len(durableEvents))
	}
	for _, event := range durableEvents {
		if !seenEventIDs[event.ID] {
			t.Fatalf("binary event traversal omitted durable ID %d", event.ID)
		}
	}
}

func TestCT03CoordlinkBinaryRejectsStaleRunWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	gitFacts := &contractGit{sha: strings.Repeat("a", 40), root: filepath.Join(root, "repos")}
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	service, err := core.NewService(database, gitFacts, core.ServiceOptions{
		MaxParallelRuns: 2,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := service.AddAgent(ctx, core.AddAgentInput{DisplayName: "Agent", AdapterID: "one-shot", Image: "agent:latest", InstructionsFile: "/instructions", RequestID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.AddProject(ctx, core.AddProjectInput{Name: "Project", Source: "/source", SourceRef: "refs/heads/main", RequestID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := service.Chat(ctx, core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "work", Wake: true, RequestID: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	run1, ok, err := service.ClaimNext(ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("run1 claim ok=%v err=%v", ok, err)
	}
	if _, err := service.ActivateRun(ctx, run1.Run.ID, "activate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.InterruptRun(ctx, run1.Run.ID, "rotate", "interrupt"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	run2, ok, err := service.ClaimNext(ctx, project.ID)
	if err != nil || !ok {
		t.Fatalf("run2 claim ok=%v err=%v", ok, err)
	}
	if _, err := service.ActivateRun(ctx, run2.Run.ID, "activate-2"); err != nil {
		t.Fatal(err)
	}
	controlRoot, err := os.MkdirTemp("/tmp", "coordplane-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlRoot) })
	socket := filepath.Join(controlRoot, "api.sock")
	server, err := transport.NewUnixServer(controlRoot, socket, transport.NewRunHandler(service))
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	defer func() {
		_ = server.Close()
		<-serveDone
	}()
	before := durableSignature(t, database, project.ID)
	staleCalls := [][]string{
		{"task", "current", "--output", "json"},
		{"task", "show", run1.Task.ID, "--output", "json"},
		{"task", "create", "--agent", agent.ID, "--title", "stale child", "--request-id", "stale-create", "--output", "json"},
		{"task", "wait", "--reason", "stale wait", "--request-id", "stale-wait", "--output", "json"},
		{"task", "submit", "--summary", "stale submit", "--expected-head", gitFacts.sha, "--request-id", "stale-submit", "--output", "json"},
		{"task", "fail", "--reason", "stale fail", "--request-id", "stale-fail", "--output", "json"},
		{"task", "accept", run1.Task.ID, "--request-id", "stale-accept", "--output", "json"},
		{"task", "rework", run1.Task.ID, "--request-id", "stale-rework", "--output", "json"},
		{"inbox", "list", "--output", "json"},
		{"inbox", "read", chat.Message.ID, "--output", "json"},
		{"inbox", "ack", "--ack-message", chat.Message.ID, "--request-id", "stale-ack", "--output", "json"},
		{"progress", "--summary", "stale", "--request-id", "stale-progress", "--output", "json"},
		{"message", "send", "--to-boss", "--body", "stale", "--request-id", "stale-message", "--output", "json"},
	}
	for _, args := range staleCalls {
		raw, err := runCoordlink(socket, run1.Token, args...)
		if err == nil || !bytes.Contains(raw, []byte(core.CodeStaleRun)) {
			t.Fatalf("coordlink %v err=%v output=%s", args, err, raw)
		}
		if after := durableSignature(t, database, project.ID); after != before {
			t.Fatalf("stale coordlink changed durable state\nbefore=%s\nafter=%s", before, after)
		}
	}
	raw, err := runCoordlink(socket, run2.Token, "progress", "--summary", "current", "--request-id", "current-progress", "--output", "json")
	if err != nil || !bytes.Contains(raw, []byte(`"kind":"task.progress"`)) {
		t.Fatalf("current coordlink err=%v output=%s", err, raw)
	}
}

func TestP2CoordlinkBinaryPersistsOutcomeIntentBeforeTerminalFact(t *testing.T) {
	tests := []struct {
		name        string
		args        func(string) []string
		wantSummary string
		wantCapture bool
	}{
		{
			name: "wait",
			args: func(string) []string {
				return []string{"task", "wait", "--reason", "awaiting review", "--request-id", "binary-wait", "--output", "json"}
			},
			wantSummary: "awaiting review",
		},
		{
			name: "submit",
			args: func(head string) []string {
				return []string{"task", "submit", "--summary", "ready", "--expected-head", head, "--request-id", "binary-submit", "--output", "json"}
			},
			wantSummary: "ready",
			wantCapture: true,
		},
		{
			name: "fail",
			args: func(string) []string {
				return []string{"task", "fail", "--reason", "tests failed", "--request-id", "binary-fail", "--output", "json"}
			},
			wantSummary: "tests failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			gitFacts := &contractGit{sha: strings.Repeat("a", 40), root: filepath.Join(root, "repos")}
			service, err := core.NewService(database, gitFacts, core.ServiceOptions{MaxParallelRuns: 1})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := service.AddAgent(ctx, core.AddAgentInput{
				DisplayName: "Agent", AdapterID: "one-shot", Image: "agent:latest",
				InstructionsFile: "/instructions", RequestID: "agent-" + test.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			project, err := service.AddProject(ctx, core.AddProjectInput{
				Name: "Project " + test.name, Source: "/source", SourceRef: "refs/heads/main",
				RequestID: "project-" + test.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			task, err := service.CreateTask(ctx, core.CreateTaskInput{
				ProjectID: project.ID, Kind: core.TaskWork, AssigneeAgentID: agent.ID,
				Title: "work " + test.name, RequestID: "task-" + test.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := service.ClaimNext(ctx, project.ID)
			if err != nil || !ok || claim.Task.ID != task.ID {
				t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
			}
			if _, err := service.ActivateRun(ctx, claim.Run.ID, "activate-"+test.name); err != nil {
				t.Fatal(err)
			}

			controlRoot, err := os.MkdirTemp("/tmp", "coordplane-run-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(controlRoot) })
			socket := filepath.Join(controlRoot, "api.sock")
			server, err := transport.NewUnixServer(controlRoot, socket, transport.NewRunHandler(service))
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve() }()
			defer func() {
				_ = server.Close()
				<-serveDone
			}()

			args := test.args(gitFacts.sha)
			raw, err := runCoordlink(socket, claim.Token, args...)
			if err != nil {
				t.Fatalf("coordlink %v err=%v output=%s", args, err, raw)
			}
			snapshot, err := database.Snapshot(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotTask := contractTaskWithID(t, snapshot, task.ID)
			gotRun := contractRunWithID(t, snapshot, claim.Run.ID)
			if gotTask.Status != core.TaskFinishing || gotTask.CurrentRunID != gotRun.ID ||
				gotRun.State != core.RunActive || gotRun.RequestedOutcome != test.name ||
				gotRun.RequestedSummary != test.wantSummary || gotRun.TokenRevokedAt == "" {
				t.Fatalf("durable outcome intent task=%#v run=%#v", gotTask, gotRun)
			}
			if (gotTask.PendingAction == "capture") != test.wantCapture || gotTask.Status == core.TaskSubmitted ||
				gotTask.Status == core.TaskWaiting || gotTask.Status == core.TaskFailed {
				t.Fatalf("outcome committed a premature terminal Task state: %#v", gotTask)
			}
			if test.wantCapture && (gotTask.PendingActionID == "" || gotTask.PendingActionRunID != gotRun.ID ||
				gotTask.PendingExpectedSHA != gitFacts.sha || gotTask.PendingActionVersion != gotTask.Version) {
				t.Fatalf("submit capture intent = %#v", gotTask)
			}
			beforeReplay := durableSignature(t, database, project.ID)
			replayed, err := runCoordlink(socket, claim.Token, args...)
			if err != nil {
				t.Fatalf("idempotent outcome replay err=%v output=%s", err, replayed)
			}
			if afterReplay := durableSignature(t, database, project.ID); afterReplay != beforeReplay {
				t.Fatalf("outcome replay changed durable state\nbefore=%s\nafter=%s", beforeReplay, afterReplay)
			}
		})
	}
}

func contractTaskWithID(t *testing.T, snapshot core.Snapshot, id string) core.Task {
	t.Helper()
	for _, task := range snapshot.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("Task %q not found in snapshot", id)
	return core.Task{}
}

func contractRunWithID(t *testing.T, snapshot core.Snapshot, id string) core.Run {
	t.Helper()
	for _, run := range snapshot.Runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("Run %q not found in snapshot", id)
	return core.Run{}
}

type daemonProcess struct {
	command *exec.Cmd
	output  *lockedBuffer
}

func startDaemon(t *testing.T, configPath, socket string) *daemonProcess {
	return startDaemonBinaryWithEnv(t, testBinaries.coordplane, configPath, socket, nil)
}

func startDaemonBinaryWithEnv(t *testing.T, binary, configPath, socket string, extraEnv []string) *daemonProcess {
	t.Helper()
	output := &lockedBuffer{}
	command := exec.Command(binary, "serve", "--config", configPath)
	command.Env = append(os.Environ(), extraEnv...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &daemonProcess{command: command, output: output}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := exec.Command(binary, "status", "--socket", socket, "--output", "json")
		if raw, err := status.Output(); err == nil && bytes.Contains(raw, []byte(`"daemon_ready":true`)) {
			return process
		}
		if processExited(command.Process) {
			_, _ = command.Process.Wait()
			t.Fatalf("daemon exited before ready: %s", output.String())
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
			t.Fatalf("daemon readiness timeout: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func killDaemon(t *testing.T, process *daemonProcess) {
	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil {
		t.Fatal("daemon process is not running")
	}
	if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	if err := process.command.Wait(); err == nil {
		t.Fatal("SIGKILLed daemon exited successfully")
	}
	process.command = nil
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopDaemon(t *testing.T, process *daemonProcess, socket string) {
	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil {
		return
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon shutdown: %v\n%s", err, process.output.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		t.Fatal("daemon shutdown timeout")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operator socket survived daemon stop: %v", err)
	}
	process.command = nil
}

func runBinaryJSON(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	command := exec.Command(binary, args...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", binary, args, err, raw)
	}
	return raw
}

func runCoordlink(socket, token string, args ...string) ([]byte, error) {
	command := exec.Command(testBinaries.coordlink, args...)
	command.Env = append(os.Environ(), "COORDPLANE_RUN_SOCKET="+socket, "COORDPLANE_RUN_TOKEN="+token)
	return command.CombinedOutput()
}

func durableSignature(t *testing.T, database *store.Store, projectID string) string {
	t.Helper()
	snapshot, err := database.Snapshot(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := database.Events(context.Background(), core.EventFilter{ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Snapshot core.Snapshot `json:"snapshot"`
		Events   []core.Event  `json:"events"`
	}{snapshot, events})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeConfig(t *testing.T, root, dataDir, socket, suffix string) string {
	t.Helper()
	path := filepath.Join(root, "coordplane.yaml")
	raw := fmt.Sprintf(`data_dir: %s
operator_socket: %s
max_parallel_runs: 4
retention:
  completed_workspace: 24h
  terminal_task_ref: 168h
  run_log: 168h
runtime:
  docker_network: coordplane
  workspace_root: %s
  agent_home_root: %s
  log_root: %s
  default_image: agent:latest
  provider_env_allowlist: []
%s`, dataDir, socket, filepath.Join(dataDir, "workspaces"), filepath.Join(dataDir, "agent-homes"), filepath.Join(dataDir, "logs"), suffix)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createRepository(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "source")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-b", "main")
	git(t, path, "config", "user.email", "contract@coordplane.local")
	git(t, path, "config", "user.name", "CoordPlane Contract")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, path, "add", "README.md")
	git(t, path, "commit", "-m", "initial")
	return path
}

func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
	return string(raw)
}

func decodeJSON(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, raw)
	}
}

func repositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func processExited(process *os.Process) bool {
	if process == nil {
		return true
	}
	err := process.Signal(syscall.Signal(0))
	return errors.Is(err, os.ErrProcessDone)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(raw)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type contractGit struct {
	sha  string
	root string
}

func (g *contractGit) Preflight(context.Context, string, string) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{Source: "/source", SourceRef: "refs/heads/main", InitialSHA: g.sha, CanonicalRef: "refs/heads/main", CanonicalSHA: g.sha}, nil
}

func (g *contractGit) ControlPath(id string) string { return filepath.Join(g.root, id+".git") }
func (g *contractGit) Initialize(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}
func (g *contractGit) Verify(context.Context, core.ProjectGitIntent) (core.ProjectGitFact, error) {
	return core.ProjectGitFact{CanonicalSHA: g.sha}, nil
}
func (g *contractGit) Exists(string) bool                                      { return true }
func (g *contractGit) Resolve(context.Context, string, string) (string, error) { return g.sha, nil }
