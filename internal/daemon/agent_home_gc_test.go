package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingBoundary records the paths handed to the cleanup boundary and
// optionally fails, so the non-Docker tests can exercise Delete's gating and
// fail-closed semantics without a real container. It cannot emulate the
// boundary's root powers through plain host syscalls, so tests that must
// reach it use a host-undeletable 000-mode subdirectory.
type recordingBoundary struct {
	calls []string
	err   error
	real  bool
}

func (b *recordingBoundary) RemoveTree(ctx context.Context, path string) error {
	b.calls = append(b.calls, path)
	if b.real {
		// Emulate the boundary clearing host-undeletable content as root:
		// the 000-mode subdirectory is owned by this test user, so a chmod
		// unblocks the removal the host daemon could not perform.
		if err := os.Chmod(filepath.Join(path, "locked"), 0o700); err != nil {
			return err
		}
		return os.RemoveAll(path)
	}
	return b.err
}

// hostUndeletableHome builds an Agent home whose contents the host daemon
// user cannot remove (a 000-mode subdirectory holding a file), mirroring the
// permission shape of a 65532-owned home without needing root.
func hostUndeletableHome(t *testing.T, root, agentID string) string {
	t.Helper()
	home := filepath.Join(root, agentID)
	requireNoError(t, os.Mkdir(home, 0o700))
	locked := filepath.Join(home, "locked")
	requireNoError(t, os.Mkdir(locked, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(locked, "marker"), []byte("x"), 0o600))
	requireNoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	return home
}

func TestNewAgentHomeGCValidatesRootAndCleanupImage(t *testing.T) {
	if _, err := newAgentHomeGC("relative/root", "image"); err == nil {
		t.Fatal("relative Agent home root must be rejected")
	}
	if _, err := newAgentHomeGC("/tmp/root", "  "); err == nil {
		t.Fatal("empty cleanup image must be rejected")
	}
	if _, err := newAgentHomeGC("/tmp/root", "image"); err != nil {
		t.Fatalf("valid construction failed: %v", err)
	}
}

func TestAgentHomeGCDeleteHostDeletesWithoutBoundary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const agentID = "agent-host-owned"
	home := filepath.Join(root, agentID)
	requireNoError(t, os.Mkdir(home, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(home, "marker"), []byte("x"), 0o600))
	boundary := &recordingBoundary{err: errors.New("boundary must not run")}
	gc := &agentHomeGC{root: root, boundary: boundary}

	// Host-deletable content is removed by the host daemon user without the
	// boundary, so hosts without a usable boundary keep today's semantics.
	deleted, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("Delete of host-owned home: %v", err)
	}
	if !deleted {
		t.Fatal("Delete returned not deleted")
	}
	if len(boundary.calls) != 0 {
		t.Fatalf("boundary must not run for host-deletable content, calls = %v", boundary.calls)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home must be removed, got %v", err)
	}
}

func TestAgentHomeGCDeleteEscalatesUndeletableContentToBoundary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const agentID = "agent-boundary-success"
	home := hostUndeletableHome(t, root, agentID)
	boundary := &recordingBoundary{real: true}
	gc := &agentHomeGC{root: root, boundary: boundary}

	deleted, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("Delete through boundary: %v", err)
	}
	if !deleted {
		t.Fatal("Delete returned not deleted")
	}
	if len(boundary.calls) != 1 || boundary.calls[0] != home {
		t.Fatalf("boundary calls = %v, want [%s]", boundary.calls, home)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home must be removed, got %v", err)
	}
}

func TestAgentHomeGCDeleteAuthorizeRejectedDoesNotDelete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const agentID = "agent-authorize-rejected"
	home := filepath.Join(root, agentID)
	requireNoError(t, os.Mkdir(home, 0o700))
	boundary := &recordingBoundary{}
	gc := &agentHomeGC{root: root, boundary: boundary}

	deleted, err := gc.Delete(ctx, agentID, func() (bool, error) { return false, nil })
	if err != nil || deleted {
		t.Fatalf("Delete with rejected authorize = (%v, %v), want (false, nil)", deleted, err)
	}
	if len(boundary.calls) != 0 {
		t.Fatalf("boundary must not run when authorize rejects, calls = %v", boundary.calls)
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatalf("home must be untouched, got %v", err)
	}

	want := errors.New("authorize failed")
	if _, err := gc.Delete(ctx, agentID, func() (bool, error) { return false, want }); !errors.Is(err, want) {
		t.Fatalf("authorize error = %v, want %v", err, want)
	}
	if len(boundary.calls) != 0 {
		t.Fatalf("boundary must not run when authorize errors, calls = %v", boundary.calls)
	}
}

func TestAgentHomeGCDeleteMissingStateIsNoOp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	boundary := &recordingBoundary{}
	gc := &agentHomeGC{root: root, boundary: boundary}

	// Pre-existing contract: an absent home is a no-op — no error and the
	// cleanup boundary must never run (the bool reports the end state).
	_, err := gc.Delete(ctx, "agent-absent", func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("Delete of absent home must not error, got %v", err)
	}
	if len(boundary.calls) != 0 {
		t.Fatalf("boundary must not run for absent state, calls = %v", boundary.calls)
	}
}

func TestAgentHomeGCDeleteSymlinkGuardPreserved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const agentID = "agent-symlink"
	target := filepath.Join(root, "target")
	requireNoError(t, os.Mkdir(target, 0o700))
	requireNoError(t, os.Symlink(target, filepath.Join(root, agentID)))
	boundary := &recordingBoundary{}
	gc := &agentHomeGC{root: root, boundary: boundary}

	if _, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil }); err == nil ||
		!strings.Contains(err.Error(), "not a direct directory") {
		t.Fatalf("symlink home must be rejected, got %v", err)
	}
	if len(boundary.calls) != 0 {
		t.Fatalf("boundary must not run for symlink homes, calls = %v", boundary.calls)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("symlink target must be untouched, got %v", err)
	}
}

func TestAgentHomeGCDeleteBoundaryFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const agentID = "agent-boundary-failure"
	hostUndeletableHome(t, root, agentID)
	want := errors.New("trusted boundary unavailable")
	boundary := &recordingBoundary{err: want}
	gc := &agentHomeGC{root: root, boundary: boundary}

	if _, err := gc.Delete(ctx, agentID, func() (bool, error) { return true, nil }); !errors.Is(err, want) {
		t.Fatalf("boundary failure = %v, want %v", err, want)
	}
}

func TestAgentHomeGCDeleteUnconfiguredBoundaryFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	hostUndeletableHome(t, root, "agent-no-boundary")
	gc := &agentHomeGC{root: root}

	if _, err := gc.Delete(ctx, "agent-no-boundary", func() (bool, error) { return true, nil }); err == nil {
		t.Fatal("Delete without a configured boundary must fail closed")
	}
}
