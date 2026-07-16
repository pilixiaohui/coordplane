package contract_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func TestGT07FormalOperatorGCDeletesOnlyArchivedAgentHome(t *testing.T) {
	root := t.TempDir()
	_, dataDir, socket, configPath := contractConfigPaths(t, "", root)
	daemon := startDaemon(t, configPath, socket)
	t.Cleanup(func() { stopDaemon(t, daemon, socket) })

	raw := runBinaryJSON(t, testBinaries.coordplane,
		"agent", "add", "--socket", socket, "--display-name", "Home GC Agent",
		"--adapter", "codex", "--image", "agent:latest", "--instructions-file", filepath.Join(root, "agent.md"),
		"--request-id", "home-gc-agent", "--output", "json")
	var agent core.Agent
	decodeJSON(t, raw, &agent)
	home := filepath.Join(dataDir, "agent-homes", agent.ID)
	requireNoError(t, os.MkdirAll(home, 0o700))
	requireNoError(t, os.WriteFile(filepath.Join(home, "session"), []byte("recoverable\n"), 0o600))

	active := formalAgentHomeTarget(t, socket, agent.ID)
	if !active.Exists || active.Eligible || !hasGCReason(active.Reasons, "agent_not_archived") {
		t.Fatalf("formal active Agent home preview = %#v", active)
	}
	runBinaryJSON(t, testBinaries.coordplane,
		"gc", "run", "--socket", socket, "--confirm", "--request-id", "active-home-gc", "--output", "json")
	if _, err := os.Stat(filepath.Join(home, "session")); err != nil {
		t.Fatalf("active Agent home changed by GC: %v", err)
	}

	runBinaryJSON(t, testBinaries.coordplane,
		"agent", "archive", agent.ID, "--socket", socket, "--request-id", "archive-home-agent", "--output", "json")
	if _, err := os.Stat(filepath.Join(home, "session")); err != nil {
		t.Fatalf("archive deleted Agent home: %v", err)
	}
	archived := formalAgentHomeTarget(t, socket, agent.ID)
	if !archived.Exists || !archived.Eligible || len(archived.Reasons) != 0 {
		t.Fatalf("formal archived Agent home preview = %#v", archived)
	}

	database, err := store.Open(context.Background(), filepath.Join(dataDir, "coordplane.db"))
	requireNoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	args := []string{"gc", "run", "--socket", socket, "--confirm", "--request-id", "archived-home-gc", "--output", "json"}
	first := runBinaryJSON(t, testBinaries.coordplane, args...)
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archived Agent home survived formal GC: %v", err)
	}
	afterFirst := durableSignature(t, database, "")
	replay := runBinaryJSON(t, testBinaries.coordplane, args...)
	if !bytes.Equal(first, replay) || durableSignature(t, database, "") != afterFirst {
		t.Fatal("formal Agent home GC replay changed response or durable state")
	}
	runBinaryJSON(t, testBinaries.coordplane,
		"gc", "run", "--socket", socket, "--confirm", "--request-id", "archived-home-gc-absent", "--output", "json")
	absent := formalAgentHomeTarget(t, socket, agent.ID)
	if absent.Exists || absent.Eligible || !hasGCReason(absent.Reasons, "absent") {
		t.Fatalf("formal absent Agent home preview = %#v", absent)
	}
}

func formalAgentHomeTarget(t *testing.T, socket, agentID string) core.GCAgentHomeTarget {
	t.Helper()
	raw := runBinaryJSON(t, testBinaries.coordplane, "gc", "preview", "--socket", socket, "--output", "json")
	var preview core.GCPreview
	decodeJSON(t, raw, &preview)
	for _, target := range preview.AgentHomes {
		if target.AgentID == agentID {
			return target
		}
	}
	t.Fatalf("formal GC preview omitted Agent home %s: %#v", agentID, preview)
	return core.GCAgentHomeTarget{}
}

func hasGCReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
