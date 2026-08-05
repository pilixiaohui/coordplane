//go:build e2e && linux && amd64

package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestRMA02ArtifactReaderResistsSynchronizedCheckReadReplacement(t *testing.T) {
	if os.Getenv("RMA02_ARTIFACT_RACE_CHILD") == "1" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		requireRMA02FixtureMarker(t, os.Getenv("RMA02_ARTIFACT_RACE_DATA"), "agent-A", "task-A", "source-A")
		return
	}

	dataDir := writeRMA02ArtifactFixture(t)
	currentPath := filepath.Join(rma02ArtifactRoot(dataDir, "agent-A"), "task-A", "run-A", "task-current.json")
	canaryPath := filepath.Join(t.TempDir(), "rma02-artifact-secret-canary")
	requireNoError(t, os.WriteFile(canaryPath, []byte("rma02-artifact-secret-canary\n"), 0o600))
	replacement := currentPath + ".replacement"
	requireNoError(t, os.Symlink(canaryPath, replacement))

	outputPath := filepath.Join(t.TempDir(), "child-output")
	outputFile, err := os.Create(outputPath)
	requireNoError(t, err)
	command := exec.Command(os.Args[0], "-test.run=^TestRMA02ArtifactReaderResistsSynchronizedCheckReadReplacement$")
	command.Env = append(os.Environ(), "GOMAXPROCS=1", "RMA02_ARTIFACT_RACE_CHILD=1", "RMA02_ARTIFACT_RACE_DATA="+dataDir)
	command.Stdout, command.Stderr = outputFile, outputFile
	command.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	requireNoError(t, command.Start())

	replaced, status, traceErr := traceRMA02ArtifactCheckReadRace(command.Process.Pid, currentPath, replacement)
	_ = outputFile.Close()
	output, readErr := os.ReadFile(outputPath)
	requireNoError(t, readErr)
	if traceErr != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		t.Fatalf("trace artifact reader: %v output=%s", traceErr, output)
	}
	if !replaced {
		t.Fatal("artifact path was not replaced at the synchronized check/read boundary")
	}
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("descriptor-pinned reader failed after path replacement: status=%v output=%s", status, output)
	}
	if strings.Contains(string(output), "rma02-artifact-secret-canary") {
		t.Fatalf("artifact reader leaked replacement canary: %s", output)
	}
}

func traceRMA02ArtifactCheckReadRace(pid int, currentPath, replacement string) (bool, syscall.WaitStatus, error) {
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
		return false, status, err
	}
	if !status.Stopped() {
		return false, status, fmt.Errorf("child did not stop after exec: %v", status)
	}
	if err := syscall.PtraceSetOptions(pid, syscall.PTRACE_O_TRACESYSGOOD); err != nil {
		return false, status, err
	}

	const syscallTrap = syscall.Signal(int(syscall.SIGTRAP) | 0x80)
	atEntry, pendingOpen, replaceOnExit := true, false, false
	targetFD, signal, replaced := int64(-1), 0, false
	for {
		if err := syscall.PtraceSyscall(pid, signal); err != nil {
			return replaced, status, err
		}
		signal = 0
		if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
			return replaced, status, err
		}
		if status.Exited() || status.Signaled() {
			return replaced, status, nil
		}
		if !status.Stopped() {
			continue
		}
		stopSignal := status.StopSignal()
		if stopSignal != syscallTrap {
			if stopSignal != syscall.SIGTRAP && stopSignal != syscall.SIGSTOP {
				signal = int(stopSignal)
			}
			continue
		}

		var registers syscall.PtraceRegs
		if err := syscall.PtraceGetRegs(pid, &registers); err != nil {
			return replaced, status, err
		}
		if atEntry {
			pendingOpen, replaceOnExit = false, false
			switch registers.Orig_rax {
			case syscall.SYS_OPENAT:
				path, _ := readRMA02PtraceString(pid, uintptr(registers.Rsi))
				pendingOpen = filepath.Base(path) == filepath.Base(currentPath)
			case syscall.SYS_NEWFSTATAT:
				path, _ := readRMA02PtraceString(pid, uintptr(registers.Rsi))
				replaceOnExit = !replaced && filepath.Base(path) == filepath.Base(currentPath)
			case syscall.SYS_FSTAT:
				replaceOnExit = !replaced && int64(registers.Rdi) == targetFD
			}
		} else {
			if pendingOpen && int64(registers.Rax) >= 0 {
				targetFD = int64(registers.Rax)
			}
			if replaceOnExit && !replaced {
				if err := os.Rename(replacement, currentPath); err != nil {
					return false, status, err
				}
				replaced = true
			}
		}
		atEntry = !atEntry
	}
}

func readRMA02PtraceString(pid int, address uintptr) (string, error) {
	result := make([]byte, 0, 256)
	for len(result) < 4096 {
		word := make([]byte, 8)
		count, err := syscall.PtracePeekData(pid, address+uintptr(len(result)), word)
		if err != nil {
			return "", err
		}
		word = word[:count]
		if end := bytes.IndexByte(word, 0); end >= 0 {
			return string(append(result, word[:end]...)), nil
		}
		result = append(result, word...)
	}
	return "", fmt.Errorf("ptrace path exceeded limit")
}
