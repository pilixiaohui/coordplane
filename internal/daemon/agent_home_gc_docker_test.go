//go:build docker

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	containerruntime "coordplane/internal/runtime"
)

// TestAgentHomeGCDockerDeletesContainerWrittenHome is the red core
// regression for COD-66: an Agent home whose contents were written by a
// runtime container as uid 65532 cannot be deleted by the host daemon user,
// so Delete must go through the trusted Docker boundary. Before the fix
// os.RemoveAll fails with permission denied on the 65532-owned 0700
// directories and Delete returns an error (red); after the fix the boundary
// removes the tree as root inside a trusted container and Delete succeeds
// with the home fully converged.
func TestAgentHomeGCDockerDeletesContainerWrittenHome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for the Agent home GC boundary regression: %v", err)
	}
	image := dockerGitHelperImage(t, ctx, t.TempDir())
	root := t.TempDir()
	const agentID = "agent-red-65532"
	home := filepath.Join(root, agentID)
	gid := strconv.Itoa(os.Getgid())
	requireNoError(t, os.Mkdir(home, 0o2770))

	// Write the production Agent home shape from inside a container as
	// uid 65532 (runtime launch shape: 65532 with the daemon group added, a
	// 02770 home): a 0700 65532-owned directory tree (e.g. ~/.claude) that
	// the host daemon user cannot remove.
	write := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "65532:"+gid, "--group-add", gid,
		"-v", home+":/home/agent", "--entrypoint", "sh", image,
		"-c", "mkdir -p /home/agent/.claude && printf x > /home/agent/.claude/settings.json && chmod 700 /home/agent/.claude && printf y > /home/agent/notes.txt")
	if raw, err := write.CombinedOutput(); err != nil {
		t.Fatalf("write 65532-owned Agent home: %v\n%s", err, raw)
	}
	claude, err := os.Lstat(filepath.Join(home, ".claude"))
	requireNoError(t, err)
	if claude.Mode().Perm() != 0o700 {
		t.Fatalf("expected 0700 65532-owned .claude, got %v", claude.Mode().Perm())
	}
	if stat, ok := claude.Sys().(*syscall.Stat_t); !ok || stat.Uid != 65532 {
		t.Fatalf("expected uid 65532 on .claude, got %#v", claude.Sys())
	}

	// On failure (red run) the 65532-owned tree survives and would break
	// t.TempDir cleanup; empty it through the same trusted boundary.
	t.Cleanup(func() {
		if _, err := os.Lstat(home); errors.Is(err, os.ErrNotExist) {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
			"-v", home+":/cleanup", "--entrypoint", "sh", image, "-c", "find /cleanup -mindepth 1 -delete").Run()
	})

	gc, err := newAgentHomeGC(root)
	requireNoError(t, err)
	deleted, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("Delete of 65532-owned Agent home: %v", err)
	}
	if !deleted {
		t.Fatal("Delete returned not deleted")
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Agent home must be fully removed, got %v", err)
	}
}
