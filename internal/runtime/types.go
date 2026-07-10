package runtime

import (
	"context"
	"time"
)

const (
	ContainerWorkspacePath = "/workspace/project"
	ContainerHomePath      = "/home/agent"
	ContainerCoordlinkPath = "/usr/local/bin/coordlink"
)

type AssignmentSession struct {
	AttemptID     string            `json:"attempt_id"`
	Route         SessionRoute      `json:"route"`
	LeaseID       string            `json:"lease_id"`
	Env           map[string]string `json:"-"`
	ContainerName string            `json:"-"`
}

type SessionRoute struct {
	ID              string `json:"id"`
	AgentID         string `json:"agent_id"`
	RuntimeID       string `json:"runtime_id"`
	CLIBackend      string `json:"cli_backend"`
	SessionNativeID string `json:"session_native_id"`
	Workdir         string `json:"workdir"`
	HomeDir         string `json:"home_dir"`
	AttemptID       string `json:"attempt_id"`
	LeaseID         string `json:"lease_id"`
	AssignmentID    string `json:"assignment_id"`
	State           string `json:"state"`
}

type Attempt struct {
	ID              string     `json:"id"`
	LeaseID         string     `json:"lease_id"`
	CLIBackend      string     `json:"cli_backend"`
	RuntimeKind     string     `json:"runtime_kind"`
	SessionNativeID string     `json:"session_native_id,omitempty"`
	StartReason     string     `json:"start_reason"`
	Status          string     `json:"status"`
	TranscriptRef   string     `json:"transcript_ref,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

type RuntimeBackend interface {
	Name() string
	Kind() string
	IsReady() bool
	Prepare(ctx context.Context, req PrepareRequest) (PreparedRuntime, error)
}

type PrepareRequest struct {
	AgentID        string
	AttemptID      string
	AssignmentID   string
	LeaseID        string
	ContractID     string
	TeamID         string
	RuntimeProfile string
	CLIBackend     string
	BackendURL     string
	WorkspaceName  string
}

type EnvironmentInput struct {
	BackendURL    string
	AgentID       string
	RuntimeID     string
	AttemptID     string
	AssignmentID  string
	LeaseID       string
	Workspace     string
	CLIBackend    string
	TeamID        string
	WorkspaceName string
}

type RuntimeInstance struct {
	ID             string          `json:"id"`
	RuntimeID      string          `json:"runtime_id"`
	RuntimeKind    string          `json:"runtime_kind"`
	RuntimeProfile string          `json:"runtime_profile"`
	AgentID        string          `json:"agent_id"`
	AttemptID      string          `json:"attempt_id"`
	LeaseID        string          `json:"lease_id"`
	ContainerID    string          `json:"container_id,omitempty"`
	ContainerName  string          `json:"container_name,omitempty"`
	Image          string          `json:"image,omitempty"`
	Network        string          `json:"network,omitempty"`
	State          string          `json:"state"`
	WorkspacePath  string          `json:"workspace_path"`
	HomePath       string          `json:"home_path"`
	Checks         map[string]bool `json:"checks,omitempty"`
	EnvKeys        []string        `json:"env_keys,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type CLISession struct {
	ID              string     `json:"id"`
	AttemptID       string     `json:"attempt_id"`
	RuntimeID       string     `json:"runtime_id"`
	AgentID         string     `json:"agent_id"`
	CLIBackend      string     `json:"cli_backend"`
	ProfileName     string     `json:"profile_name"`
	SessionNativeID string     `json:"session_native_id"`
	ContainerID     string     `json:"container_id,omitempty"`
	ContainerName   string     `json:"container_name,omitempty"`
	ProcessRef      string     `json:"process_ref,omitempty"`
	State           string     `json:"state"`
	StartReason     string     `json:"start_reason"`
	ResumeOf        string     `json:"resume_of,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	TranscriptRef   string     `json:"transcript_ref,omitempty"`
	Command         []string   `json:"command,omitempty"`
	EnvKeys         []string   `json:"env_keys,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type StartRequest struct {
	AgentID         string
	AttemptID       string
	AssignmentID    string
	LeaseID         string
	ContractID      string
	SessionNativeID string
	RuntimeID       string
	CLIBackend      string
	Workspace       string
	HomeDir         string
	Env             map[string]string
	BootstrapPrompt string
}

type StartResult struct {
	SessionNativeID string
	TranscriptRef   string
}

type SteerPayload struct {
	RouteID         string `json:"route_id"`
	AgentID         string `json:"agent_id"`
	SessionNativeID string `json:"session_native_id"`
	MailboxID       string `json:"mailbox_id"`
	Reason          string `json:"reason"`
}

type ResumeRequest struct {
	Route      SessionRoute
	Reason     string
	MailboxIDs []string
	Env        map[string]string
}

type ResumeRouteInput struct {
	RouteID    string
	Reason     string
	MailboxIDs []string
}

type ResumeResult struct {
	AttemptID  string            `json:"attempt_id"`
	RouteID    string            `json:"route_id"`
	State      string            `json:"state"`
	MailboxIDs []string          `json:"mailbox_ids,omitempty"`
	Env        map[string]string `json:"-"`
}

type ResumeQueueResult struct {
	QueueItemID string            `json:"queue_item_id,omitempty"`
	MailboxID   string            `json:"mailbox_id,omitempty"`
	RouteID     string            `json:"route_id,omitempty"`
	State       string            `json:"state"`
	Idle        bool              `json:"idle"`
	Env         map[string]string `json:"-"`
}

type SteerRequest struct {
	Route   SessionRoute
	Payload SteerPayload
}

type TerminalReport struct {
	AttemptID         string `json:"attempt_id"`
	Status            string `json:"status"`
	Summary           string `json:"summary,omitempty"`
	TranscriptRef     string `json:"transcript_ref,omitempty"`
	TranscriptContent string `json:"transcript_content,omitempty"`
}

type PinInput struct {
	AttemptID       string
	LeaseID         string
	AssignmentID    string
	AgentID         string
	RuntimeID       string
	CLIBackend      string
	SessionNativeID string
	Workdir         string
	HomeDir         string
}

type PrepareLeaseInput struct {
	LeaseID   string
	AttemptID string
	AgentID   string
	Owner     string
	TTL       time.Duration
	Now       time.Time
}

type PrepareLease struct {
	ID        string    `json:"id"`
	LeaseID   string    `json:"lease_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	AgentID   string    `json:"agent_id"`
	Owner     string    `json:"owner"`
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CleanupTarget struct {
	ResourceKind string
	ResourceRef  string
}

type CLIAdapter interface {
	Start(ctx context.Context, req StartRequest) (StartResult, error)
	Steer(ctx context.Context, req SteerRequest) error
	Finish(ctx context.Context, report TerminalReport) error
}

type CLIAdapterCapabilities struct {
	SupportsSameTurnSteer bool `json:"supports_same_turn_steer"`
	ReturnsOnProcessExit  bool `json:"returns_on_process_exit"`
}

type CLIAdapterCapabilityProvider interface {
	Capabilities() CLIAdapterCapabilities
}

type CLIAdapterBackendCapabilityProvider interface {
	CapabilitiesForBackend(backend string) (CLIAdapterCapabilities, bool)
}

func AdapterCapabilitiesForBackend(adapter CLIAdapter, backend string) (CLIAdapterCapabilities, bool) {
	if adapter == nil {
		return CLIAdapterCapabilities{}, false
	}
	if provider, ok := adapter.(CLIAdapterBackendCapabilityProvider); ok {
		return provider.CapabilitiesForBackend(backend)
	}
	if provider, ok := adapter.(CLIAdapterCapabilityProvider); ok {
		return provider.Capabilities(), true
	}
	return CLIAdapterCapabilities{}, false
}
