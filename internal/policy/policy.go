package policy

import (
	"context"
	"encoding/json"

	"coordplane/internal/capability"
	"coordplane/internal/teamconfig"
)

type Decision struct {
	Allowed    bool
	Reason     string
	RepairHint string
}

type Policy struct {
	cfg teamconfig.Config
}

func New(cfg teamconfig.Config) *Policy {
	return &Policy{cfg: cfg}
}

func (p *Policy) AgentDeclared(agentID string) Decision {
	if agentID == "" {
		return Decision{
			Allowed:    false,
			Reason:     "agent identity is required for capability discovery",
			RepairHint: "provide the agent identity from the authenticated coordlink subject",
		}
	}
	if _, ok := p.cfg.Agent(agentID); !ok {
		return Decision{
			Allowed:    false,
			Reason:     "agent is not declared in TeamConfig",
			RepairHint: "load a TeamConfig version that declares this agent",
		}
	}
	return Decision{Allowed: true}
}

func (p *Policy) CanUseCapability(agentID, capabilityName string) Decision {
	if declared := p.AgentDeclared(agentID); !declared.Allowed {
		return declared
	}
	agent, _ := p.cfg.Agent(agentID)
	if capabilityName == "" {
		return Decision{
			Allowed:    false,
			Reason:     "capability name is required",
			RepairHint: "retry with a capability name from scoped capability discovery",
		}
	}
	for _, allowed := range agent.Capabilities {
		if allowed == capabilityName {
			return Decision{Allowed: true}
		}
	}
	return Decision{
		Allowed:    false,
		Reason:     "capability is not allowed by TeamConfig for this agent",
		RepairHint: "update TeamConfig capabilities for the agent or call an allowed capability",
	}
}

func (p *Policy) DiscoverCapabilities(agentID string, registry *capability.Registry) []capability.Definition {
	if registry == nil {
		return nil
	}
	if declared := p.AgentDeclared(agentID); !declared.Allowed {
		return nil
	}
	agent, _ := p.cfg.Agent(agentID)
	allowed := make(map[string]bool, len(agent.Capabilities))
	for _, name := range agent.Capabilities {
		allowed[name] = true
	}
	var out []capability.Definition
	for _, definition := range registry.List() {
		if allowed[definition.Name] {
			out = append(out, definition)
		}
	}
	return out
}

type Dispatcher struct {
	policy   *Policy
	registry *capability.Registry
}

func NewDispatcher(cfg teamconfig.Config, registry *capability.Registry) *Dispatcher {
	return &Dispatcher{policy: New(cfg), registry: registry}
}

func (d *Dispatcher) Handle(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	agentID := agentIDFromSubject(call.Subject)
	if decision := d.policy.CanUseCapability(agentID, call.CapabilityName); !decision.Allowed {
		return capability.Rejected[json.RawMessage](
			"UNAUTHORIZED_CAPABILITY_CALL",
			decision.Reason,
			capability.WithRepairHint(decision.RepairHint),
			capability.WithAllowedNextActions("capability.list"),
			capability.WithRetryable(false),
		)
	}
	return d.registry.Handle(ctx, call)
}

func (d *Dispatcher) ListForSubject(ctx context.Context, subject capability.Subject) capability.Response[json.RawMessage] {
	agentID := agentIDFromSubject(subject)
	if decision := d.policy.AgentDeclared(agentID); !decision.Allowed {
		return capability.Rejected[json.RawMessage](
			"UNAUTHORIZED_CAPABILITY_DISCOVERY",
			decision.Reason,
			capability.WithRepairHint(decision.RepairHint),
			capability.WithRetryable(false),
		)
	}
	response, err := capability.AcceptedJSON(d.policy.DiscoverCapabilities(agentID, d.registry))
	if err != nil {
		return capability.Error[json.RawMessage]("CAPABILITY_LIST_ENCODE_FAILED", err.Error(), false)
	}
	return response
}

func agentIDFromSubject(subject capability.Subject) string {
	if subject.AgentID != "" {
		return subject.AgentID
	}
	if subject.Kind == "agent" {
		return subject.ID
	}
	return ""
}
