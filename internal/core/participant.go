package core

// ParticipantKind distinguishes human identities from CLI agent identities
// inside the unified participant framework. Both are participants: the same
// Task/Message/role framework applies; capability differences come from roles.
type ParticipantKind string

const (
	ParticipantKindHuman    ParticipantKind = "human"
	ParticipantKindCLIAgent ParticipantKind = "cli_agent"
)

const (
	// DefaultHumanParticipantID is the deterministic identity seeded by the
	// v3 migration for the first human owner. Operator CLI requests are
	// attributed to this participant until credential binding lands.
	DefaultHumanParticipantID = "participant-owner"
	// DefaultOwnerRoleID is the seeded owner role carrying every capability.
	DefaultOwnerRoleID = "role-owner"
	// DefaultAgentRoleID is the seeded default CLI agent role.
	DefaultAgentRoleID = "role-agent"
	// GlobalProjectID is the reserved scope for non-project (management)
	// capabilities such as role.manage and role.bind.
	GlobalProjectID = "global"
)

// Participant is a unified identity row: a human or a CLI agent. CLI agents
// keep their runtime fields (adapter/image/instructions); humans use
// credential binding instead.
type Participant struct {
	ID               string
	Kind             ParticipantKind
	DisplayName      string
	Status           string
	CredentialID     string
	AdapterID        string
	Image            string
	InstructionsFile string
	InstructionsText string
	Model            string
	SubagentModel    string
	BaseURL          string
	Effort           string
	Version          int64
	CreatedAt        string
	UpdatedAt        string
}

// ParticipantRoleBinding is one role bound to a participant inside a project
// scope (or the reserved global scope).
type ParticipantRoleBinding struct {
	ParticipantID string
	ProjectID     string
	RoleID        string
	RoleName      string
	Capabilities  []Capability
	Version       int64
	CreatedAt     string
	UpdatedAt     string
}

// BindRoleInput binds or unbinds one role for one participant in one project
// scope.
type BindRoleInput struct {
	ParticipantID string
	ProjectID     string
	RoleID        string
	RequestID     string
}

// ParticipantCapabilities resolves the effective capability set of a
// participant in a project scope: the union of every bound role whose scope
// is the project itself or the reserved global scope.
func ParticipantCapabilities(bindings []ParticipantRoleBinding, projectID string) []Capability {
	seen := make(map[Capability]struct{})
	var capabilities []Capability
	for _, binding := range bindings {
		if binding.ProjectID != projectID && binding.ProjectID != GlobalProjectID {
			continue
		}
		for _, capability := range binding.Capabilities {
			if _, duplicate := seen[capability]; duplicate {
				continue
			}
			seen[capability] = struct{}{}
			capabilities = append(capabilities, capability)
		}
	}
	return capabilities
}
