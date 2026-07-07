package cpprobe_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"coordplane/internal/backend"
	"coordplane/internal/cpprobe"
	"coordplane/internal/teamconfig"
)

func TestCPProbeTeamConfigUsesImplementedCapabilityNames(t *testing.T) {
	raw, err := os.ReadFile(cpProbeTeamConfigPath(t))
	if err != nil {
		t.Fatalf("read CP-PROBE TeamConfig fixture: %v", err)
	}
	cfg, err := teamconfig.ParseYAML(raw)
	if err != nil {
		t.Fatalf("parse CP-PROBE TeamConfig fixture: %v", err)
	}
	if cfg.TeamID != cpprobe.TeamID || cfg.Version != cpprobe.TeamVersion {
		t.Fatalf("fixture identity = %s v%d, want %s v%d", cfg.TeamID, cfg.Version, cpprobe.TeamID, cpprobe.TeamVersion)
	}
	for _, agentID := range []string{"coordinator", "developer-a", "developer-b", "verifier"} {
		if _, ok := cfg.Agent(agentID); !ok {
			t.Fatalf("fixture missing agent %s", agentID)
		}
	}
	developerA, _ := cfg.Agent("developer-a")
	developerB, _ := cfg.Agent("developer-b")
	assertNamesEqual(t, developerA.Capabilities, developerB.Capabilities)
	assertNamesEqual(t, developerA.Capabilities, cpProbeDeveloperCapabilities())
	verifier, _ := cfg.Agent("verifier")
	for _, want := range []string{"command.run", "workspace.prepare", "git.log", "validation.assessment"} {
		if !containsName(verifier.Capabilities, want) {
			t.Fatalf("verifier capabilities missing %s: %v", want, verifier.Capabilities)
		}
	}

	allCapabilities := make(map[string]bool)
	for _, agent := range cfg.Agents {
		for _, name := range agent.Capabilities {
			allCapabilities[name] = true
		}
	}
	for _, forbidden := range cpprobe.ForbiddenSpecCapabilityNames() {
		if allCapabilities[forbidden] {
			t.Fatalf("CP-PROBE TeamConfig still uses spec-only capability name %q", forbidden)
		}
	}
}

func TestCPProbeCapabilityDecisionsMapSpecAliasesToRegisteredCapabilities(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := backend.Open(ctx, backend.Config{
		DBPath:         filepath.Join(dir, "coordplane.db"),
		ListenAddr:     "127.0.0.1:0",
		TeamConfigPath: cpProbeTeamConfigPath(t),
	})
	if err != nil {
		t.Fatalf("open backend with CP-PROBE fixture: %v", err)
	}
	defer app.Close()

	registered := make(map[string]bool)
	for _, definition := range app.Capabilities.List() {
		registered[definition.Name] = true
	}
	for _, specName := range cpprobe.ForbiddenSpecCapabilityNames() {
		decision, ok := cpprobe.DecisionForCapability(specName)
		if !ok {
			t.Fatalf("missing capability decision for %s", specName)
		}
		if decision.AgentFacing {
			for _, implemented := range decision.Implemented {
				if !registered[implemented] {
					t.Fatalf("%s maps to unregistered capability %s", specName, implemented)
				}
			}
		}
	}
	inspectDecision, _ := cpprobe.DecisionForCapability("inspect.read")
	if inspectDecision.AgentFacing || len(inspectDecision.Implemented) != 1 || inspectDecision.Implemented[0] != "/inspect" {
		t.Fatalf("inspect.read decision = %+v, want operator-only /inspect", inspectDecision)
	}
}

func cpProbeTeamConfigPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve CP-PROBE fixture path: runtime caller unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "team_config", "fixtures", "cp_probe_001_manual_service.yaml")
}

func cpProbeDeveloperCapabilities() []string {
	return []string{
		"assignment.next",
		"assignment.watch",
		"changeset.abandon",
		"changeset.submit",
		"command.run",
		"communication.read",
		"contract.complete",
		"contract.context",
		"contract.current",
		"git.abort",
		"git.commit",
		"git.conflicts",
		"git.diff",
		"git.log",
		"git.merge_apply",
		"git.merge_preview",
		"git.recover",
		"git.resolve",
		"git.rollback",
		"git.status",
		"object.inspect",
		"object.read",
		"report.submit",
		"workspace.prepare",
		"workspace.status",
		"workspace.sync",
	}
}

func assertNamesEqual(t *testing.T, got, want []string) {
	t.Helper()
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("names = %v, want %v", gotCopy, wantCopy)
	}
}

func containsName(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
