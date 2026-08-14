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

// writeContainerHome writes the production Agent home shape from inside a
// container as uid 65532 (runtime launch shape: 65532 with the daemon group
// added, a 02770 home): a 0700 65532-owned directory tree (e.g. ~/.claude)
// that the host daemon user cannot remove.
func writeContainerHome(t *testing.T, ctx context.Context, image, home string) {
	t.Helper()
	gid := strconv.Itoa(os.Getgid())
	requireNoError(t, os.Mkdir(home, 0o2770))
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
		hostUID := -1
		if ok {
			hostUID = int(stat.Uid)
		}
		// Docker daemons with UID remapping (e.g. userns-remap) map the
		// container's 65532 to a different host uid. The test premise is
		// environment-specific, so skip with the observed mapping as evidence
		// instead of failing the product regression.
		_ = exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
			"-v", home+":/cleanup", "--entrypoint", "sh", image, "-c", "find /cleanup -mindepth 1 -delete").Run()
		t.Skipf("SKIP(UID remap): container UID 65532 maps to host uid %d", hostUID)
	}
}

// TestAgentHomeGCDockerDeletesContainerWrittenHome is the red core
// regression for COD-66: an Agent home whose contents were written by a
// runtime container as uid 65532 cannot be deleted by the host daemon user,
// so Delete must escalate to the trusted Docker boundary. Before the fix
// os.RemoveAll fails with permission denied on the 65532-owned 0700
// directories and Delete returns an error (red, the live #12 failure
// shape); after the fix the boundary removes the tree as root inside a
// trusted container and Delete succeeds with the home fully converged.
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
	writeContainerHome(t, ctx, image, home)

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

	gc, err := newAgentHomeGC(root, image)
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

// TestAgentHomeGCDockerBoundaryUnavailableFailClosed is the fail-closed
// regression for COD-66: when the content requires the trusted Docker
// boundary and the boundary cannot run (here the cleanup image reference is
// rejected before any daemon interaction), Delete must return an error and
// leave the home in place — the GC segment keeps its
// RUNTIME_INVARIANT_VIOLATION failure instead of silently reporting the
// deletion as done.
func TestAgentHomeGCDockerBoundaryUnavailableFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for the Agent home GC boundary regression: %v", err)
	}
	image := dockerGitHelperImage(t, ctx, t.TempDir())
	root := t.TempDir()
	const agentID = "agent-red-failclosed"
	home := filepath.Join(root, agentID)
	writeContainerHome(t, ctx, image, home)

	// The 65532-owned tree survives by design here; empty it through the
	// boundary so t.TempDir cleanup converges.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
			"-v", home+":/cleanup", "--entrypoint", "sh", image, "-c", "find /cleanup -mindepth 1 -delete").Run()
	})

	gc, err := newAgentHomeGC(root, "bad:ref:format")
	requireNoError(t, err)
	if _, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil }); err == nil {
		t.Fatal("Delete with an unavailable trusted boundary must fail closed")
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatalf("Agent home must be left in place when the boundary fails, got %v", err)
	}
}

// TestAgentHomeGCDockerDeletesHomeUnder65532OwnedRoot pins the second
// dockerCleanupBoundary branch (COD-67 review hygiene): when the emptied
// Agent home root itself cannot be removed by the host because its parent
// directory is 65532-owned without host write permission, RemoveTree must
// escalate to mounting the parent and removing the root from inside the
// container. Reachability in production is low (AgentHomeRoot is
// daemon-owned), so this is a defensive closure; before COD-67 the branch
// had no automated coverage and was only exercised by manual runs.
func TestAgentHomeGCDockerDeletesHomeUnder65532OwnedRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	requireNoError(t, err)
	if err := executor.Ping(ctx); err != nil {
		t.Fatalf("real Docker is required for the Agent home GC boundary regression: %v", err)
	}
	image := dockerGitHelperImage(t, ctx, t.TempDir())
	root := filepath.Join(t.TempDir(), "root")
	requireNoError(t, os.Mkdir(root, 0o700))
	const agentID = "agent-red-parent-65532"
	home := filepath.Join(root, agentID)
	writeContainerHome(t, ctx, image, home)

	// Make the Agent home root's parent 65532-owned without host write
	// permission: the host can still traverse it (so State and the docker
	// bind mounts work) but can no longer unlink the emptied home root,
	// which forces the second dockerCleanupBoundary branch.
	chown := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
		"-v", root+":/agent-root", "--entrypoint", "sh", image,
		"-c", "chown 65532:65532 /agent-root && chmod 755 /agent-root")
	if raw, err := chown.CombinedOutput(); err != nil {
		t.Fatalf("chown Agent home root to 65532: %v\n%s", err, raw)
	}
	rootStat, err := os.Lstat(root)
	requireNoError(t, err)
	if stat, ok := rootStat.Sys().(*syscall.Stat_t); !ok || stat.Uid != 65532 {
		t.Fatalf("expected uid 65532 on Agent home root, got %#v", rootStat.Sys())
	}
	if rootStat.Mode().Perm() != 0o755 {
		t.Fatalf("expected 0755 Agent home root, got %v", rootStat.Mode().Perm())
	}

	// On failure the 65532-owned tree survives and would break t.TempDir
	// cleanup; empty it through the same trusted boundary.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
			"-v", root+":/cleanup", "--entrypoint", "sh", image, "-c", "find /cleanup -mindepth 1 -delete").Run()
	})

	gc, err := newAgentHomeGC(root, image)
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
