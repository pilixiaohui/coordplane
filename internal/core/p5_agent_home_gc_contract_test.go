package core_test

import (
	"context"
	"sync"
	"testing"

	"coordplane/internal/core"
)

func TestGT07ArchivedAgentHomeRequiresExplicitGCAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	agent := h.addAgent(t, "home-gc-agent")
	homes := &fakeAgentHomes{exists: map[string]bool{agent.ID: true}}
	service, err := core.NewService(h.database, h.git, core.ServiceOptions{
		Now: h.clock.Now, NewID: h.ids.New, MaxParallelRuns: 4,
		AdapterIDs: []string{"one-shot"}, AgentHomes: homes,
	})
	if err != nil {
		t.Fatal(err)
	}

	active := agentHomeTarget(t, service, agent.ID)
	if !active.Exists || active.Eligible || !hasReason(active.Reasons, "agent_not_archived") {
		t.Fatalf("active Agent home preview = %#v", active)
	}
	if _, err := service.GCRun(context.Background(), core.GCRunInput{Confirm: true, RequestID: "active-home-gc"}); err != nil {
		t.Fatal(err)
	}
	if homes.deleteCalls != 0 {
		t.Fatalf("active Agent home delete calls = %d", homes.deleteCalls)
	}

	if _, err := service.ArchiveAgent(context.Background(), agent.ID, "archive-home-agent"); err != nil {
		t.Fatal(err)
	}
	if homes.deleteCalls != 0 || !homes.exists[agent.ID] {
		t.Fatal("archive deleted Agent home without explicit GC")
	}
	archived := agentHomeTarget(t, service, agent.ID)
	if !archived.Exists || !archived.Eligible || len(archived.Reasons) != 0 {
		t.Fatalf("archived Agent home preview = %#v", archived)
	}

	input := core.GCRunInput{Confirm: true, RequestID: "archived-home-gc"}
	if result, err := service.GCRun(context.Background(), input); err != nil || !result.Completed {
		t.Fatalf("archived Agent home GC = %#v err=%v", result, err)
	}
	if homes.deleteCalls != 1 || homes.exists[agent.ID] {
		t.Fatalf("archived Agent home state exists=%t calls=%d", homes.exists[agent.ID], homes.deleteCalls)
	}
	if result, err := service.GCRun(context.Background(), input); err != nil || !result.Completed {
		t.Fatalf("archived Agent home GC replay = %#v err=%v", result, err)
	}
	if result, err := service.GCRun(context.Background(), core.GCRunInput{Confirm: true, RequestID: "archived-home-gc-absent"}); err != nil || !result.Completed {
		t.Fatalf("absent Agent home GC = %#v err=%v", result, err)
	}
	if homes.deleteCalls != 1 {
		t.Fatalf("idempotent Agent home delete calls = %d", homes.deleteCalls)
	}
	absent := agentHomeTarget(t, service, agent.ID)
	if absent.Exists || absent.Eligible || !hasReason(absent.Reasons, "absent") {
		t.Fatalf("absent Agent home preview = %#v", absent)
	}
}

func agentHomeTarget(t *testing.T, service *core.Service, agentID string) core.GCAgentHomeTarget {
	t.Helper()
	preview, err := service.GCPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range preview.AgentHomes {
		if target.AgentID == agentID {
			return target
		}
	}
	t.Fatalf("GC preview omitted Agent home %s: %#v", agentID, preview)
	return core.GCAgentHomeTarget{}
}

func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

type fakeAgentHomes struct {
	mu          sync.Mutex
	exists      map[string]bool
	deleteCalls int
}

func (f *fakeAgentHomes) State(_ context.Context, agentID string) (core.AgentHomeStateFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return core.AgentHomeStateFact{Exists: f.exists[agentID]}, nil
}

func (f *fakeAgentHomes) Delete(_ context.Context, agentID string, authorize func() (bool, error)) (bool, error) {
	allowed, err := authorize()
	if err != nil || !allowed {
		return allowed, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists[agentID] {
		return true, nil
	}
	f.deleteCalls++
	delete(f.exists, agentID)
	return true, nil
}
