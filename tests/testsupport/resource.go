package testsupport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DockerResource = "docker"

// AcquireSerialResource coordinates host resources shared by parallel Go test
// package processes. The kernel releases the lock if a test process exits.
func AcquireSerialResource(resource, owner string, timeout time.Duration) (func() error, error) {
	if resource == "" || strings.ContainsAny(resource, `/\\`) {
		return nil, fmt.Errorf("invalid test resource %q", resource)
	}
	path := serialResourcePath(resource)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open test resource lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock test resource %s: %w", resource, err)
		}
		if time.Now().After(deadline) {
			holder, readErr := os.ReadFile(path)
			_ = file.Close()
			if readErr != nil {
				return nil, fmt.Errorf("wait %s for test resource %s and read holder: %w", timeout, resource, readErr)
			}
			return nil, fmt.Errorf("wait %s for test resource %s held by %q", timeout, resource, strings.TrimSpace(string(holder)))
		}
		time.Sleep(25 * time.Millisecond)
	}
	holder := fmt.Sprintf("pid=%d owner=%s acquired=%s\n", os.Getpid(), owner, time.Now().UTC().Format(time.RFC3339Nano))
	if err := file.Truncate(0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("truncate test resource lock %s: %w", resource, err)
	}
	if _, err := file.WriteAt([]byte(holder), 0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("record test resource owner %s: %w", resource, err)
	}
	var releaseErr error
	var once sync.Once
	release := func() error {
		once.Do(func() {
			releaseErr = errors.Join(
				syscall.Flock(int(file.Fd()), syscall.LOCK_UN),
				file.Close(),
			)
		})
		return releaseErr
	}
	return release, nil
}

func serialResourcePath(resource string) string {
	return filepath.Join(os.TempDir(), "coordplane-test-resource-"+resource+".lock")
}
