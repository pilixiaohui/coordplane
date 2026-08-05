package gitcapture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/tests/testsupport"
)

func TestCaptureIgnoresRepositoryConfigAndPublishesReadyAtomically(t *testing.T) {
	workspace, head := newCaptureRepository(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	command := filepath.Join(t.TempDir(), "fsmonitor")
	testsupport.RequireNoError(t, os.WriteFile(command, []byte("#!/bin/sh\n: > "+marker+"\n"), 0o700))
	testsupport.Git(t, workspace, "config", "core.fsmonitor", command)

	handoff := t.TempDir()
	fact, err := Capture(context.Background(), Request{
		Workspace: workspace, Handoff: handoff, ExpectedHead: head,
		BaseSHA: head, MaximumBundleBytes: 8 << 20, MaximumObjects: 100,
	})
	testsupport.RequireNoError(t, err)
	if fact.HeadSHA != head || fact.ObjectCount == 0 {
		t.Fatalf("capture fact = %#v", fact)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository fsmonitor executed in helper: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handoff, ReadyName, BundleName)); err != nil {
		t.Fatalf("ready bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handoff, PartialName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial handoff survived successful capture: %v", err)
	}
}

func TestCaptureLimitsLeaveNoReadyHandoff(t *testing.T) {
	workspace, head := newCaptureRepository(t)
	for _, test := range []struct {
		name       string
		maxBytes   int64
		maxObjects int
		want       string
	}{
		{name: "bytes", maxBytes: 1, maxObjects: 100, want: "byte limit"},
		{name: "objects", maxBytes: 8 << 20, maxObjects: 1, want: "object limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handoff := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := Capture(ctx, Request{
				Workspace: workspace, Handoff: handoff, ExpectedHead: head,
				BaseSHA: head, MaximumBundleBytes: test.maxBytes, MaximumObjects: test.maxObjects,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Capture() error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(filepath.Join(handoff, ReadyName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed capture published ready handoff: %v", statErr)
			}
		})
	}
}

func TestInspectIgnoresRepositoryConfigAndReportsDirtyAndUnfinishedState(t *testing.T) {
	workspace, head := newCaptureRepository(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	command := filepath.Join(t.TempDir(), "fsmonitor")
	testsupport.RequireNoError(t, os.WriteFile(command, []byte("#!/bin/sh\n: > "+marker+"\n"), 0o700))
	testsupport.Git(t, workspace, "config", "core.fsmonitor", command)

	inspect := func() Fact {
		t.Helper()
		fact, err := Inspect(context.Background(), InspectRequest{
			Workspace: workspace, Handoff: t.TempDir(), MaximumObjects: 100,
		})
		testsupport.RequireNoError(t, err)
		return fact
	}
	clean := inspect()
	if clean.HeadSHA != head || !clean.Clean || clean.Unfinished || clean.StatusDigest != emptyStatusDigest() {
		t.Fatalf("clean inspect fact = %#v", clean)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository fsmonitor executed during inspect: %v", err)
	}

	testsupport.RequireNoError(t, os.WriteFile(filepath.Join(workspace, "dirty.txt"), []byte("dirty\n"), 0o600))
	dirty := inspect()
	if dirty.Clean || dirty.StatusDigest == clean.StatusDigest {
		t.Fatalf("dirty inspect fact = %#v, clean = %#v", dirty, clean)
	}
	testsupport.RequireNoError(t, os.WriteFile(filepath.Join(workspace, ".git", "MERGE_HEAD"), []byte(head+"\n"), 0o600))
	unfinished := inspect()
	if !unfinished.Unfinished {
		t.Fatalf("unfinished inspect fact = %#v", unfinished)
	}
}

func newCaptureRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	testsupport.Git(t, root, "init", "-q")
	testsupport.Git(t, root, "config", "user.name", "Capture Test")
	testsupport.Git(t, root, "config", "user.email", "capture@example.invalid")
	testsupport.RequireNoError(t, os.WriteFile(filepath.Join(root, "result.txt"), []byte("result\n"), 0o600))
	testsupport.Git(t, root, "add", "result.txt")
	testsupport.Git(t, root, "commit", "-q", "-m", "result")
	return root, testsupport.Git(t, root, "rev-parse", "HEAD^{commit}")
}
