package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQuarantineUnknownPreservesOnlyRegisteredRepositoryPaths(t *testing.T) {
	initializer := newTestInitializer(t)
	registered := RegisteredPath{ProjectID: "project-known", PendingOperationID: "operation-known"}
	paths, err := initializer.Paths(registered.ProjectID, registered.PendingOperationID)
	requireNoError(t, err)
	requireNoError(t, os.MkdirAll(paths.Final, 0o700))
	requireNoError(t, os.MkdirAll(paths.Partial, 0o700))
	unknown := []string{
		filepath.Join(initializer.root, "orphan.git"),
		filepath.Join(initializer.root, "orphan.file"),
		filepath.Join(initializer.root, ".partial", "project-orphan", "operation.git"),
		filepath.Join(initializer.root, ".partial", registered.ProjectID, "operation-old.git"),
	}
	for _, path := range unknown {
		requireNoError(t, os.MkdirAll(path, 0o700))
	}
	outside := filepath.Join(t.TempDir(), "outside")
	requireNoError(t, os.WriteFile(outside, []byte("unchanged\n"), 0o600))
	unknownLink := filepath.Join(initializer.root, "orphan-link.git")
	requireNoError(t, os.Symlink(outside, unknownLink))

	got, err := initializer.QuarantineUnknown([]RegisteredPath{registered})
	requireNoError(t, err)
	want := []string{
		".partial/project-known/operation-old.git",
		".partial/project-orphan",
		"orphan-link.git",
		"orphan.file",
		"orphan.git",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quarantined = %v, want %v", got, want)
	}
	for _, path := range append(unknown, unknownLink) {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unknown path %q remains active: %v", path, err)
		}
	}
	for _, path := range []string{paths.Final, paths.Partial} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("registered path %q was not preserved: %v", path, err)
		}
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "unchanged\n" {
		t.Fatalf("quarantined symlink target changed: raw=%q err=%v", raw, err)
	}
	entries, err := os.ReadDir(filepath.Join(initializer.root, ".quarantine"))
	if err != nil || len(entries) != len(want) {
		t.Fatalf("quarantine entries=%d err=%v, want %d", len(entries), err, len(want))
	}

	again, err := initializer.QuarantineUnknown([]RegisteredPath{registered})
	if err != nil || len(again) != 0 {
		t.Fatalf("second quarantine = %v err=%v", again, err)
	}
}
