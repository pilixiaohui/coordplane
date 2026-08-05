package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrDataDirLocked = errors.New("coordplane daemon: data directory is already locked")

type DataDirLock struct {
	file     *os.File
	close    sync.Once
	closeErr error
}

// AcquireDataDirLock acquires the process-wide data directory lock without
// waiting for an existing owner.
func AcquireDataDirLock(dataDir string) (*DataDirLock, error) {
	if dataDir == "" {
		return nil, errors.New("coordplane daemon: data directory is required")
	}
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("coordplane daemon: data directory must be an absolute path")
	}

	dataDir = filepath.Clean(dataDir)
	if err := ensureDirectDirectoryPath(dataDir); err != nil {
		return nil, fmt.Errorf("coordplane daemon: create data directory: %w", err)
	}
	if err := validateDataDirectory(dataDir, dataDir); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(dataDir, "locks")
	if err := os.Mkdir(lockDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("coordplane daemon: create lock directory: %w", err)
	}
	if err := validateDataDirectory(dataDir, lockDir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(lockDir, "daemon.lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("coordplane daemon: open data directory lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if err := validateLockFile(file, lockPath); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDataDirLocked
		}
		return nil, fmt.Errorf("coordplane daemon: acquire data directory lock: %w", err)
	}
	return &DataDirLock{file: file}, nil
}

func ensureDirectDirectoryPath(path string) error {
	current := filepath.Clean(path)
	var missing []string
	for {
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("path component %s is not a direct directory", current)
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return fmt.Errorf("resolve existing path component %s: %w", current, err)
			}
			if filepath.Clean(resolved) != current {
				return fmt.Errorf("path component %s resolves through a symlink", current)
			}
			for index := len(missing) - 1; index >= 0; index-- {
				current = filepath.Join(current, missing[index])
				if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
					return fmt.Errorf("create path component %s: %w", current, err)
				}
				created, err := os.Lstat(current)
				if err != nil {
					return fmt.Errorf("inspect path component %s: %w", current, err)
				}
				if created.Mode()&os.ModeSymlink != 0 || !created.IsDir() {
					return fmt.Errorf("path component %s is not a direct directory", current)
				}
			}
			return nil
		case errors.Is(err, os.ErrNotExist):
			parent := filepath.Dir(current)
			if parent == current {
				return fmt.Errorf("path %s has no existing directory ancestor", path)
			}
			missing = append(missing, filepath.Base(current))
			current = parent
		default:
			return fmt.Errorf("inspect path component %s: %w", current, err)
		}
	}
}

func validateLockFile(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("coordplane daemon: inspect opened data directory lock: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coordplane daemon: inspect data directory lock path: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("coordplane daemon: data directory lock must be a direct regular file")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("coordplane daemon: data directory lock is not owned by the daemon user")
	}
	if opened.Mode().Perm()&0o022 != 0 {
		return errors.New("coordplane daemon: data directory lock must not be group/other writable")
	}
	return nil
}

func (l *DataDirLock) Close() error {
	if l == nil {
		return nil
	}
	l.close.Do(func() {
		if l.file == nil {
			return
		}
		unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		closeErr := l.file.Close()
		l.closeErr = errors.Join(unlockErr, closeErr)
	})
	return l.closeErr
}
