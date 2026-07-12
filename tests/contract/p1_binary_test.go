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
	directory  string
	coordplane string
	coordlink  string
}

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "coordplane-p1-contract-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(directory)
	root := repositoryRoot()
	testBinaries.directory = directory
	testBinaries.coordplane = filepath.Join(directory, "coordplane")
	testBinaries.coordlink = filepath.Join(directory, "coordlink")
	for command, output := range map[string]string{"./cmd/coordplane": testBinaries.coordplane, "./cmd/coordlink": testBinaries.coordlink} {
		build := exec.Command("go", "build", "-buildvcs=false", "-o", output, command)
		build.Dir = root
		if raw, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", command, err, raw)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
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
	if !status.DaemonReady || len(status.Snapshot.Projects) != 1 || len(status.Snapshot.Messages) != 2 {
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
	var taskView core.TaskView
	decodeJSON(t, taskRaw, &taskView)
	if taskView.Task.ID != first.Task.ID || taskView.PendingMessageCount != 2 || strings.TrimSpace(taskView.ActualCanonicalSHA) != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("task show projection = %#v", taskView)
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
	messagesRaw := runBinaryJSON(t, testBinaries.coordplane,
		"message", "list", "--socket", socket, "--task", first.Task.ID, "--output", "json")
	var messages []core.Message
	decodeJSON(t, messagesRaw, &messages)
	if len(messages) != 2 || messages[0].Body != "first" || messages[1].Body != "second" {
		t.Fatalf("messages after restart = %#v", messages)
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
	repairedRaw := runBinaryJSON(t, testBinaries.coordplane,
		"project", "repair", project.ID, "--socket", socket,
		"--request-id", "binary-project-repair", "--output", "json")
	var repaired core.Project
	decodeJSON(t, repairedRaw, &repaired)
	if repaired.Status != core.ProjectActive || repaired.InitialSHA != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("repaired project = %#v", repaired)
	}
	if got := strings.TrimSpace(git(t, controlRepo, "rev-parse", "refs/heads/main^{commit}")); got != strings.TrimSpace(sourceRefBefore) {
		t.Fatalf("repaired canonical = %s, want immutable initial %s", got, sourceRefBefore)
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

func TestCT03CoordlinkBinaryRejectsStaleRunWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	gitFacts := &contractGit{sha: strings.Repeat("a", 40), root: filepath.Join(root, "repos")}
	service, err := core.NewService(database, gitFacts, core.ServiceOptions{MaxParallelRuns: 2})
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
	if _, err := service.Chat(ctx, core.ChatInput{ProjectID: project.ID, AgentID: agent.ID, Body: "work", Wake: true, RequestID: "chat"}); err != nil {
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
		{"progress", "--summary", "stale", "--request-id", "stale-progress", "--output", "json"},
		{"message", "send", "--to-boss", "--body", "stale", "--request-id", "stale-message", "--output", "json"},
		{"task", "submit", "--summary", "stale", "--expected-head", gitFacts.sha, "--request-id", "stale-submit", "--output", "json"},
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

type daemonProcess struct {
	command *exec.Cmd
	output  *lockedBuffer
}

func startDaemon(t *testing.T, configPath, socket string) *daemonProcess {
	t.Helper()
	output := &lockedBuffer{}
	command := exec.Command(testBinaries.coordplane, "serve", "--config", configPath)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &daemonProcess{command: command, output: output}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := exec.Command(testBinaries.coordplane, "status", "--socket", socket, "--output", "json")
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
