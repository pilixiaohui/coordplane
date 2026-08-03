package core

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is a static permission point for one callable operation. The
// capability set is code fact: adding an operation requires adding a registry
// entry here. Capabilities are never data; only role→capability mappings are
// configurable data.
type Capability string

const (
	CapabilityTaskCreate        Capability = "task.create"
	CapabilityTaskAssign        Capability = "task.assign"
	CapabilityTaskAccept        Capability = "task.accept"
	CapabilityTaskRework        Capability = "task.rework"
	CapabilityTaskCancel        Capability = "task.cancel"
	CapabilityTaskComplete      Capability = "task.complete"
	CapabilityTaskWake          Capability = "task.wake"
	CapabilityTaskRetry         Capability = "task.retry"
	CapabilityMessageSend       Capability = "message.send"
	CapabilityMessageRead       Capability = "message.read"
	CapabilityRunStop           Capability = "run.stop"
	CapabilityRunView           Capability = "run.view"
	CapabilityProjectView       Capability = "project.view"
	CapabilityProjectRepair     Capability = "project.repair"
	CapabilityProjectArchive    Capability = "project.archive"
	CapabilityGCPreview         Capability = "gc.preview"
	CapabilityGCRun             Capability = "gc.run"
	CapabilityGCDiscard         Capability = "gc.discard"
	CapabilityAgentManage       Capability = "agent.manage"
	CapabilityParticipantManage Capability = "participant.manage"
	CapabilityRoleManage        Capability = "role.manage"
	CapabilityRoleBind          Capability = "role.bind"
)

// capabilityRegistry is the canonical static capability list. Keep it sorted;
// AllCapabilities returns a copy so callers cannot mutate the registry.
var capabilityRegistry = []Capability{
	CapabilityAgentManage,
	CapabilityGCDiscard,
	CapabilityGCPreview,
	CapabilityGCRun,
	CapabilityMessageRead,
	CapabilityMessageSend,
	CapabilityParticipantManage,
	CapabilityProjectArchive,
	CapabilityProjectRepair,
	CapabilityProjectView,
	CapabilityRoleBind,
	CapabilityRoleManage,
	CapabilityRunStop,
	CapabilityRunView,
	CapabilityTaskAccept,
	CapabilityTaskAssign,
	CapabilityTaskCancel,
	CapabilityTaskComplete,
	CapabilityTaskCreate,
	CapabilityTaskRetry,
	CapabilityTaskRework,
	CapabilityTaskWake,
}

// agentDefaultCapabilities is the minimal capability set granted to CLI agents
// by the seeded default agent role. It mirrors the fixed coordlink surface:
// agents act inside their own Task/Run scope only.
var agentDefaultCapabilities = []Capability{
	CapabilityMessageRead,
	CapabilityMessageSend,
	CapabilityTaskAccept,
	CapabilityTaskCreate,
	CapabilityTaskRework,
	CapabilityTaskWake,
}

// AllCapabilities returns a copy of the static registry.
func AllCapabilities() []Capability {
	return append([]Capability(nil), capabilityRegistry...)
}

// AgentDefaultCapabilities returns the default CLI agent capability set.
func AgentDefaultCapabilities() []Capability {
	return append([]Capability(nil), agentDefaultCapabilities...)
}

// CapabilityNames renders capabilities as stable sorted strings.
func CapabilityNames(capabilities []Capability) []string {
	names := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		names = append(names, string(capability))
	}
	sort.Strings(names)
	return names
}

// ParseCapabilities validates raw names against the static registry and
// returns the canonical capability set. Unknown names are rejected so a
// configurable role can never reference a capability the binary does not know.
func ParseCapabilities(names []string) ([]Capability, error) {
	seen := make(map[Capability]struct{}, len(names))
	parsed := make([]Capability, 0, len(names))
	for _, name := range names {
		capability := Capability(strings.TrimSpace(name))
		if capability == "" {
			continue
		}
		if !knownCapability(capability) {
			return nil, NewError(CodeInvalidArgument, fmt.Sprintf("unknown capability %q", name), false)
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		parsed = append(parsed, capability)
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i] < parsed[j] })
	return parsed, nil
}

// HasCapability reports whether the capability set contains want.
func HasCapability(capabilities []Capability, want Capability) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

func knownCapability(capability Capability) bool {
	for _, registered := range capabilityRegistry {
		if registered == capability {
			return true
		}
	}
	return false
}
