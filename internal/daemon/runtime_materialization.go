package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
)

const (
	runtimeBootstrapMessageBodyLimit  = 4 << 10
	runtimeBootstrapMessageTotalLimit = 64 << 10
)

func (c *runtimeController) acquireRunOperation(runID string) *runOperation {
	if runID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.monitors[runID] != nil {
		return nil
	}
	if c.runOperations == nil {
		c.runOperations = make(map[string]*runOperation)
	}
	if _, exists := c.runOperations[runID]; exists {
		return nil
	}
	operation := &runOperation{}
	c.runOperations[runID] = operation
	return operation
}

func (c *runtimeController) releaseRunOperation(runID string, operation *runOperation) {
	c.mu.Lock()
	if c.runOperations[runID] == operation {
		delete(c.runOperations, runID)
	}
	c.mu.Unlock()
}

func gitWorkspaceSpec(task core.Task) (gitrepo.WorkspaceSpec, error) {
	spec := gitrepo.WorkspaceSpec{ProjectID: task.ProjectID, TaskID: task.ID, BaseSHA: task.BaseSHA}
	if task.SourceTaskID == "" && task.SourceRunID == "" && task.SourceTaskRef == "" && task.SourceHeadSHA == "" {
		return spec, nil
	}
	if task.SourceTaskID == "" || task.SourceRunID == "" || task.SourceTaskRef == "" || task.SourceHeadSHA == "" {
		return gitrepo.WorkspaceSpec{}, errors.New("source task workspace inputs are incomplete")
	}
	spec.Source = &gitrepo.WorkspaceSource{
		TaskID: task.SourceTaskID, RunID: task.SourceRunID,
		TaskRef: task.SourceTaskRef, HeadSHA: task.SourceHeadSHA,
	}
	return spec, nil
}

func buildBootstrap(
	launch core.RunLaunchContext,
	run core.Run,
	instructions, workspacePath string,
	workspaceSpec gitrepo.WorkspaceSpec,
) string {
	var body strings.Builder
	body.WriteString(instructions)
	body.WriteString("\n\nCoordPlane Run context\n")
	fmt.Fprintf(&body, "Project: %s\nAgent: %s\nTask: %s\nRun: %s\nGeneration: %d\n", launch.Project.ID, launch.Agent.ID, launch.Task.ID, run.ID, run.Generation)
	fmt.Fprintf(&body, "Kind: %s\nTitle: %s\nDescription: %s\nParent task: %s\n", launch.Task.Kind, launch.Task.Title, launch.Task.Description, launch.Task.ParentTaskID)
	if launch.Task.Kind != core.TaskConversation {
		fmt.Fprintf(&body, "Workspace: /workspace/project\nBase SHA: %s\n", launch.Task.BaseSHA)
		if launch.Task.HeadSHA != "" {
			fmt.Fprintf(&body, "Captured head: %s\nTask ref: %s\n", launch.Task.HeadSHA, launch.Task.TaskRef)
		}
		if workspaceSpec.Source != nil {
			fmt.Fprintf(&body, "Source task: %s\nSource head: %s\nSource convenience ref: %s\n", launch.Task.SourceTaskID, launch.Task.SourceHeadSHA, workspaceSpec.Source.ConvenienceRef())
			body.WriteString("The convenience ref may move; the source SHA above is authoritative.\n")
		}
		_ = workspacePath
	} else {
		body.WriteString("This is a conversation Run: the project workspace is NOT mounted (no /workspace/project) and no project code is available.\n")
		body.WriteString("Do not attempt to read, modify, build, or commit project code here, and do not run git commands expecting the project repository.\n")
		body.WriteString("To do code work, create or request a work/review Task (coordlink task create, or a message to the Boss).\n")
	}
	if len(launch.Messages) > 0 {
		body.WriteString("Pending messages:\n")
		remainingBodyBytes := runtimeBootstrapMessageTotalLimit
		for _, message := range launch.Messages {
			relatedTaskID := message.RelatedTaskID
			if relatedTaskID == "" {
				relatedTaskID = "none"
			}
			messageBody, truncated := boundedBootstrapMessageBody(message.Body, remainingBodyBytes)
			remainingBodyBytes -= len(messageBody)
			if truncated {
				if messageBody == "" {
					messageBody = "[body omitted: aggregate limit]"
				} else {
					messageBody += " [body truncated]"
				}
			}
			fmt.Fprintf(
				&body,
				"- [%s] delivery_task=%s related_task=%s from %s/%s: %s\n",
				message.ID, message.TaskID, relatedTaskID, message.SenderKind, message.SenderID, messageBody,
			)
		}
	}
	if launch.Task.Kind == core.TaskConversation {
		body.WriteString("Reply to the pending messages above. Use coordlink task wait when you are done for now; CLI exit or text such as done does not complete the Task.\n")
	} else {
		body.WriteString("Use native Git commands in the private workspace. Use coordlink task wait, submit, or fail for an explicit outcome. CLI exit or text such as done does not complete the Task.\n")
	}
	return body.String()
}

func boundedBootstrapMessageBody(value string, remaining int) (string, bool) {
	limit := min(runtimeBootstrapMessageBodyLimit, remaining)
	if limit < 0 {
		limit = 0
	}
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func containerWorkingDirectory(kind core.TaskKind) string {
	if kind == core.TaskConversation {
		return "/home/agent"
	}
	return "/workspace/project"
}

func runtimeRef(run core.Run) containerruntime.RuntimeRef {
	return containerruntime.RuntimeRef{
		ContainerID: run.ContainerID, ContainerName: run.ContainerName,
		ProjectID: run.ProjectID, TaskID: run.TaskID, AgentID: run.AgentID,
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
	}
}

func runtimeFactInput(run core.Run, ref containerruntime.RuntimeRef, phase string) core.RunRuntimeFactInput {
	return core.RunRuntimeFactInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: ref.ContainerID,
		RequestID: runtimeRequest(run, phase),
	}
}

func runtimeTerminalInput(run core.Run, input core.RunTerminalInput) core.RunTerminalInput {
	input.RunID = run.ID
	input.Generation = run.Generation
	input.LaunchNonce = run.LaunchNonce
	input.LaunchOperationID = run.LaunchOperationID
	input.ContainerID = run.ContainerID
	return input
}

func runtimeRequest(run core.Run, phase string) string {
	return "runtime:" + run.ID + ":" + run.LaunchOperationID + ":" + phase
}

func randomRuntimeID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw), nil
}

func readInstructions(path string) (string, string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", "", errors.New("Agent instructions file must be absolute")
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", "", fmt.Errorf("read Agent instructions: %w", err)
	}
	if len(raw) > 1<<20 {
		return "", "", errors.New("Agent instructions exceed 1 MiB")
	}
	sum := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(sum[:]), nil
}

func ensureRuntimeDirectory(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("runtime directory path must be canonical and absolute")
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("runtime directory is not a direct directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("runtime directory is not owned by the daemon user")
	}
	return nil
}

func writeRuntimeFile(path string, raw []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".runtime-file-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func canonicalExecutable(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return "", errors.New("coordlink executable path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve coordlink executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("coordlink executable is missing or not executable")
	}
	return resolved, nil
}

func resolveCoordlinkExecutable() string {
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), "coordlink")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		return candidate
	}
	return filepath.Join(string(filepath.Separator), "usr", "local", "bin", "coordlink")
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
