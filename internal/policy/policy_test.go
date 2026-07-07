package policy_test

import (
	"context"
	"encoding/json"
	"testing"

	"coordplane/internal/capability"
	"coordplane/internal/policy"
	"coordplane/internal/teamconfig"
)

func TestDiscoverCapabilitiesIsTrimmedByTeamConfigPolicy(t *testing.T) {
	registry := capability.NewRegistry()
	for _, name := range []string{"contract.current", "contract.add", "session.steer"} {
		if err := registry.Register(capability.Definition{
			Name:          name,
			SideEffect:    capability.SideEffectRead,
			RequiredScope: "agent_lease",
		}, func(context.Context, capability.Call) capability.Response[json.RawMessage] {
			return capability.AcceptedRaw(json.RawMessage(`{"ok":true}`))
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	cfg := teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "builder",
				RolePrompt:     "Renamed role prompt must not affect authorization.",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   []string{"contract.current"},
			},
		},
	}
	p := policy.New(cfg)

	discovered := p.DiscoverCapabilities("builder", registry)
	if len(discovered) != 1 || discovered[0].Name != "contract.current" {
		t.Fatalf("discovered capabilities = %+v, want only contract.current", discovered)
	}
	if decision := p.CanUseCapability("builder", "contract.current"); !decision.Allowed {
		t.Fatalf("contract.current decision = %+v, want allowed", decision)
	}
	if decision := p.CanUseCapability("builder", "contract.add"); decision.Allowed || decision.RepairHint == "" {
		t.Fatalf("contract.add decision = %+v, want denied with repair hint", decision)
	}
	if p.CanUseCapability("builder", "session.steer").Allowed {
		t.Fatal("session.steer should not be allowed when TeamConfig does not list it")
	}
}

func TestDispatcherRejectsUnauthorizedCapabilityBeforeRegistryHandler(t *testing.T) {
	handled := false
	registry := capability.NewRegistry()
	if err := registry.Register(capability.Definition{
		Name:          "object.read",
		SideEffect:    capability.SideEffectRead,
		RequiredScope: "agent_object",
	}, func(context.Context, capability.Call) capability.Response[json.RawMessage] {
		handled = true
		return capability.AcceptedRaw(json.RawMessage(`{"content":"leaked"}`))
	}); err != nil {
		t.Fatalf("register object.read: %v", err)
	}
	dispatcher := policy.NewDispatcher(teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "builder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   []string{"contract.current"},
			},
		},
	}, registry)

	response := dispatcher.Handle(context.Background(), capability.Call{
		CapabilityName: "object.read",
		Subject: capability.Subject{
			Kind:    "agent",
			ID:      "builder",
			AgentID: "builder",
		},
		Input: json.RawMessage(`{"object_ref":"obj_sha256_valid"}`),
	})
	if handled {
		t.Fatal("unauthorized capability reached registry handler")
	}
	if response.Status != capability.StatusRejected || response.ErrorCode != "UNAUTHORIZED_CAPABILITY_CALL" {
		t.Fatalf("response = %+v, want UNAUTHORIZED_CAPABILITY_CALL rejected", response)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("validate response: %v", err)
	}
}
