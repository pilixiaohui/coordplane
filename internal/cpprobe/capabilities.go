package cpprobe

type CapabilityDecision struct {
	SpecName       string   `json:"spec_name"`
	Implemented    []string `json:"implemented"`
	Decision       string   `json:"decision"`
	AgentFacing    bool     `json:"agent_facing"`
	DockerDeferred bool     `json:"docker_deferred"`
}

var CapabilityDecisions = []CapabilityDecision{
	{
		SpecName:    "mailbox.read",
		Implemented: []string{"mailbox.get"},
		Decision:    "Use the implemented point-read mailbox capability.",
		AgentFacing: true,
	},
	{
		SpecName:    "artifact.upload",
		Implemented: []string{"report.submit", "command.run"},
		Decision:    "Use report.submit for agent-authored summaries and command.run stdout/stderr object refs for command evidence until an active artifact upload capability exists.",
		AgentFacing: true,
	},
	{
		SpecName:    "artifact.download",
		Implemented: []string{"object.inspect", "object.read"},
		Decision:    "Use scoped object inspection and object reads for durable evidence retrieval.",
		AgentFacing: true,
	},
	{
		SpecName:    "inspect.read",
		Implemented: []string{"/inspect"},
		Decision:    "Operator/test-driver inspect remains an HTTP endpoint, not an ordinary agent capability.",
		AgentFacing: false,
	},
	{
		SpecName:    "git.rebase",
		Implemented: []string{"workspace.sync", "git.conflicts", "git.resolve"},
		Decision:    "The minimal skeleton uses workspace.sync and explicit merge/resolve capabilities; a first-class git.rebase capability is deferred.",
		AgentFacing: true,
	},
}

func ForbiddenSpecCapabilityNames() []string {
	return []string{
		"mailbox.read",
		"artifact.upload",
		"artifact.download",
		"inspect.read",
		"git.rebase",
	}
}

func DecisionForCapability(name string) (CapabilityDecision, bool) {
	for _, decision := range CapabilityDecisions {
		if decision.SpecName == name {
			return decision, true
		}
	}
	return CapabilityDecision{}, false
}
