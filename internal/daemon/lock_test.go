package daemon_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"coordplane/internal/daemon"
)

func TestDataDirLockRejectsConcurrentOwnerAndReleasesOnClose(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	first, err := daemon.AcquireDataDirLock(dataDir)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := daemon.AcquireDataDirLock(dataDir); !errors.Is(err, daemon.ErrDataDirLocked) {
		t.Fatalf("acquire competing lock error = %v, want ErrDataDirLocked", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "locks", "daemon.lock")); err != nil {
		t.Fatalf("stat lock file: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock again: %v", err)
	}
	second, err := daemon.AcquireDataDirLock(dataDir)
	if err != nil {
		t.Fatalf("reacquire released lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second lock: %v", err)
	}
}

func TestDataDirLockRejectsRelativePathWithoutCreatingIt(t *testing.T) {
	workingDir := t.TempDir()
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDir) })

	if _, err := daemon.AcquireDataDirLock("relative-data"); err == nil {
		t.Fatal("AcquireDataDirLock() accepted a relative data directory")
	}
	if _, err := os.Stat(filepath.Join(workingDir, "relative-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative data directory stat error = %v, want not exist", err)
	}
}

func TestDataDirLockRejectsSymlinksWithoutTouchingTheirTargets(t *testing.T) {
	t.Run("intermediate data directory", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
			t.Fatal(err)
		}
		dataDir := filepath.Join(root, "redirect", "nested", "data")
		if lock, err := daemon.AcquireDataDirLock(dataDir); err == nil {
			_ = lock.Close()
			t.Fatal("AcquireDataDirLock() accepted an intermediate symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "nested")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("intermediate symlink target was modified: %v", err)
		}
	})

	t.Run("lock directory", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		outside := filepath.Join(root, "outside")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dataDir, "locks")); err != nil {
			t.Fatal(err)
		}

		if lock, err := daemon.AcquireDataDirLock(dataDir); err == nil {
			_ = lock.Close()
			t.Fatal("AcquireDataDirLock() accepted a symlinked lock directory")
		}
		if _, err := os.Stat(filepath.Join(outside, "daemon.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink target contains daemon.lock: %v", err)
		}
	})

	t.Run("lock file", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		lockDir := filepath.Join(dataDir, "locks")
		if err := os.MkdirAll(lockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "outside.lock")
		const sentinel = "do not touch\n"
		if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(lockDir, "daemon.lock")); err != nil {
			t.Fatal(err)
		}

		if lock, err := daemon.AcquireDataDirLock(dataDir); err == nil {
			_ = lock.Close()
			t.Fatal("AcquireDataDirLock() accepted a symlinked lock file")
		}
		raw, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != sentinel {
			t.Fatalf("symlink target changed to %q", raw)
		}
	})
}
