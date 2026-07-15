package core

type ProjectStatus string

const (
	ProjectCreating ProjectStatus = "creating"
	ProjectActive   ProjectStatus = "active"
	ProjectError    ProjectStatus = "error"
	ProjectArchived ProjectStatus = "archived"
)

type AgentStatus string

const (
	AgentActive   AgentStatus = "active"
	AgentPaused   AgentStatus = "paused"
	AgentArchived AgentStatus = "archived"
)

type TaskKind string

const (
	TaskConversation TaskKind = "conversation"
	TaskWork         TaskKind = "work"
	TaskIntegration  TaskKind = "integration"
)

type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskFinishing TaskStatus = "finishing"
	TaskWaiting   TaskStatus = "waiting"
	TaskSubmitted TaskStatus = "submitted"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

type RunState string

const (
	RunStarting    RunState = "starting"
	RunActive      RunState = "active"
	RunExited      RunState = "exited"
	RunFailed      RunState = "failed"
	RunInterrupted RunState = "interrupted"
	RunCancelled   RunState = "cancelled"
	RunTimedOut    RunState = "timed_out"
)

type MessageState string

const (
	MessagePending      MessageState = "pending"
	MessageDelivered    MessageState = "delivered"
	MessageAcknowledged MessageState = "acknowledged"
	MessageCancelled    MessageState = "cancelled"
)

type Project struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Source             string        `json:"source"`
	SourceRef          string        `json:"source_ref"`
	InitialSHA         string        `json:"initial_sha"`
	ControlRepoPath    string        `json:"-"`
	CanonicalRef       string        `json:"canonical_ref"`
	CanonicalSHA       string        `json:"canonical_sha"`
	IntegrationAgentID string        `json:"integration_agent_id,omitempty"`
	Status             ProjectStatus `json:"status"`
	PendingAction      string        `json:"pending_action,omitempty"`
	PendingActionID    string        `json:"pending_action_id,omitempty"`
	PendingStartedAt   string        `json:"pending_started_at,omitempty"`
	LastError          string        `json:"last_error,omitempty"`
	Version            int64         `json:"version"`
	CreatedAt          string        `json:"created_at"`
	UpdatedAt          string        `json:"updated_at"`
}

type Agent struct {
	ID               string      `json:"id"`
	DisplayName      string      `json:"display_name"`
	AdapterID        string      `json:"adapter_id"`
	Image            string      `json:"image"`
	InstructionsFile string      `json:"instructions_file"`
	Status           AgentStatus `json:"status"`
	Version          int64       `json:"version"`
	CreatedAt        string      `json:"created_at"`
	UpdatedAt        string      `json:"updated_at"`
}

type Task struct {
	ID                         string     `json:"id"`
	ProjectID                  string     `json:"project_id"`
	Kind                       TaskKind   `json:"kind"`
	ParentTaskID               string     `json:"parent_task_id,omitempty"`
	RetryOfTaskID              string     `json:"retry_of_task_id,omitempty"`
	CreatedByKind              string     `json:"created_by_kind"`
	CreatedByID                string     `json:"created_by_id,omitempty"`
	AssigneeAgentID            string     `json:"assignee_agent_id"`
	Title                      string     `json:"title"`
	Description                string     `json:"description"`
	Priority                   int        `json:"priority"`
	Status                     TaskStatus `json:"status"`
	CurrentRunID               string     `json:"current_run_id,omitempty"`
	Generation                 int64      `json:"generation"`
	NextRunAt                  string     `json:"next_run_at"`
	RetryCount                 int        `json:"retry_count"`
	MaxRetries                 int        `json:"max_retries"`
	WaitReason                 string     `json:"wait_reason,omitempty"`
	ResultSummary              string     `json:"result_summary,omitempty"`
	FailureReason              string     `json:"failure_reason,omitempty"`
	BaseSHA                    string     `json:"base_sha,omitempty"`
	HeadSHA                    string     `json:"head_sha,omitempty"`
	HeadRunID                  string     `json:"head_run_id,omitempty"`
	TaskRef                    string     `json:"task_ref,omitempty"`
	AcceptedByKind             string     `json:"accepted_by_kind,omitempty"`
	AcceptedByID               string     `json:"accepted_by_id,omitempty"`
	AcceptedAt                 string     `json:"accepted_at,omitempty"`
	AcceptedIntegrationAgentID string     `json:"accepted_integration_agent_id,omitempty"`
	FinalCanonicalSHA          string     `json:"final_canonical_sha,omitempty"`
	IntegrationTaskID          string     `json:"integration_task_id,omitempty"`
	SourceTaskID               string     `json:"source_task_id,omitempty"`
	SourceRunID                string     `json:"source_run_id,omitempty"`
	SourceTaskRef              string     `json:"source_task_ref,omitempty"`
	SourceHeadSHA              string     `json:"source_head_sha,omitempty"`
	SourceRefReleasedAt        string     `json:"source_ref_released_at,omitempty"`
	SourceAcceptVersion        int64      `json:"source_accept_version,omitempty"`
	ObservedCanonicalSHA       string     `json:"observed_canonical_sha,omitempty"`
	PendingAction              string     `json:"pending_action,omitempty"`
	PendingActionID            string     `json:"pending_action_id,omitempty"`
	PendingActionVersion       int64      `json:"pending_action_version,omitempty"`
	PendingActionRunID         string     `json:"pending_action_run_id,omitempty"`
	PendingExpectedSHA         string     `json:"pending_expected_sha,omitempty"`
	PendingTargetSHA           string     `json:"pending_target_sha,omitempty"`
	PendingStartedAt           string     `json:"pending_started_at,omitempty"`
	Version                    int64      `json:"version"`
	CreatedAt                  string     `json:"created_at"`
	UpdatedAt                  string     `json:"updated_at"`
	SubmittedAt                string     `json:"submitted_at,omitempty"`
	CompletedAt                string     `json:"completed_at,omitempty"`
	ClosedAt                   string     `json:"closed_at,omitempty"`
}

type Run struct {
	ID                    string   `json:"id"`
	ProjectID             string   `json:"project_id"`
	TaskID                string   `json:"task_id"`
	AgentID               string   `json:"agent_id"`
	Generation            int64    `json:"generation"`
	ResumedFromRunID      string   `json:"resumed_from_run_id,omitempty"`
	AdapterID             string   `json:"adapter_id"`
	Image                 string   `json:"image"`
	InstructionsHash      string   `json:"instructions_hash"`
	State                 RunState `json:"state"`
	WorkspacePath         string   `json:"-"`
	ContainerID           string   `json:"container_id,omitempty"`
	NativeSessionID       string   `json:"native_session_id,omitempty"`
	LogPath               string   `json:"-"`
	TokenHash             string   `json:"-"`
	TokenRevokedAt        string   `json:"token_revoked_at,omitempty"`
	RequestedOutcome      string   `json:"requested_outcome,omitempty"`
	RequestedSummary      string   `json:"requested_summary,omitempty"`
	ExpectedHead          string   `json:"expected_head,omitempty"`
	RequestedAt           string   `json:"requested_at,omitempty"`
	StopRequestedAt       string   `json:"stop_requested_at,omitempty"`
	StopReason            string   `json:"stop_reason,omitempty"`
	StopOperationID       string   `json:"stop_operation_id,omitempty"`
	HeartbeatAt           string   `json:"heartbeat_at,omitempty"`
	ExitCode              *int     `json:"exit_code,omitempty"`
	TerminalReason        string   `json:"terminal_reason,omitempty"`
	LastError             string   `json:"last_error,omitempty"`
	CleanupState          string   `json:"cleanup_state"`
	LaunchNonce           string   `json:"launch_nonce"`
	LaunchOperationID     string   `json:"launch_operation_id"`
	LaunchPhase           string   `json:"launch_phase"`
	HomePath              string   `json:"-"`
	ContainerName         string   `json:"container_name"`
	DeadlineAt            string   `json:"deadline_at,omitempty"`
	LastObservedAt        string   `json:"last_observed_at,omitempty"`
	LaunchMode            string   `json:"launch_mode"`
	ResumeNativeSessionID string   `json:"resume_native_session_id,omitempty"`
	RuntimeErrorCode      string   `json:"runtime_error_code,omitempty"`
	CleanupOperationID    string   `json:"cleanup_operation_id,omitempty"`
	IsolationSpecVersion  int64    `json:"isolation_spec_version"`
	Version               int64    `json:"version"`
	CreatedAt             string   `json:"created_at"`
	StartedAt             string   `json:"started_at,omitempty"`
	EndedAt               string   `json:"ended_at,omitempty"`
}

const (
	RunIsolationSpecV1      int64 = 1
	RunIsolationSpecCurrent int64 = 2
)

type Message struct {
	ID                string       `json:"id"`
	ProjectID         string       `json:"project_id"`
	TaskID            string       `json:"task_id"`
	RelatedTaskID     string       `json:"related_task_id,omitempty"`
	SenderKind        string       `json:"sender_kind"`
	SenderID          string       `json:"sender_id,omitempty"`
	RecipientKind     string       `json:"recipient_kind"`
	RecipientID       string       `json:"recipient_id,omitempty"`
	ReplyToMessageID  string       `json:"reply_to_message_id,omitempty"`
	SystemCode        string       `json:"system_code,omitempty"`
	Body              string       `json:"body"`
	Wake              bool         `json:"wake"`
	State             MessageState `json:"state"`
	DeliveredRunID    string       `json:"delivered_run_id,omitempty"`
	DeliveryCount     int          `json:"delivery_count"`
	MaxDeliveries     int          `json:"max_deliveries"`
	NextDeliveryAt    string       `json:"next_delivery_at"`
	LastDeliveryError string       `json:"last_delivery_error,omitempty"`
	IdempotencyKey    string       `json:"idempotency_key"`
	Version           int64        `json:"version"`
	CreatedAt         string       `json:"created_at"`
	DeliveredAt       string       `json:"delivered_at,omitempty"`
	AcknowledgedAt    string       `json:"acknowledged_at,omitempty"`
}

type Event struct {
	ID          int64  `json:"id"`
	ProjectID   string `json:"project_id,omitempty"`
	EntityType  string `json:"entity_type"`
	EntityID    string `json:"entity_id"`
	Kind        string `json:"kind"`
	ActorKind   string `json:"actor_kind"`
	ActorID     string `json:"actor_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	PayloadJSON string `json:"payload_json"`
	CreatedAt   string `json:"created_at"`
}

type Snapshot struct {
	Projects []Project `json:"projects"`
	Agents   []Agent   `json:"agents"`
	Tasks    []Task    `json:"tasks"`
	Runs     []Run     `json:"runs"`
	Messages []Message `json:"messages"`
	Events   []Event   `json:"events,omitempty"`
}

// StatusProjection is the bounded SQLite view used to build Status. It keeps
// current operational state separate from the paginated history surfaces.
type StatusProjection struct {
	Snapshot  Snapshot   `json:"snapshot"`
	Tasks     []TaskView `json:"tasks,omitempty"`
	Truncated bool       `json:"-"`
}

type TaskProjection struct {
	Project Project    `json:"project"`
	Task    TaskDetail `json:"task"`
}

type TaskPage struct {
	Items      []TaskSummary `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type RunPage struct {
	Items      []RunSummary `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type ProjectPage struct {
	Items      []ProjectSummary `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type ProjectDetail struct {
	Project
	ActualCanonicalSHA   string `json:"actual_canonical_sha,omitempty"`
	ActualCanonicalError string `json:"actual_canonical_error,omitempty"`
}

type AgentPage struct {
	Items      []AgentSummary `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type MessagePage struct {
	Items      []Message `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type Status struct {
	DaemonReady      bool           `json:"daemon_ready"`
	Reason           string         `json:"reason,omitempty"`
	Runtime          *RuntimeStatus `json:"runtime,omitempty"`
	SummaryTruncated bool           `json:"summary_truncated,omitempty"`
	Snapshot         Snapshot       `json:"snapshot"`
	ActualRefs       []GitState     `json:"actual_refs,omitempty"`
	Tasks            []TaskView     `json:"tasks,omitempty"`
}

type RuntimeStatus struct {
	WorkspaceQuotaEnabled bool   `json:"workspace_quota_enabled"`
	WorkspaceQuotaReason  string `json:"workspace_quota_reason"`
	TmpfsLimitBytes       int64  `json:"tmpfs_limit_bytes"`
}

// TaskView is a read-only status projection. It joins the six persisted
// objects and actual Git state without becoming another durable object.
type TaskView struct {
	Task                  TaskSummary `json:"task"`
	CurrentRun            *RunSummary `json:"current_run,omitempty"`
	LatestProgress        *Event      `json:"latest_progress,omitempty"`
	PendingMessageCount   int         `json:"pending_message_count"`
	DeliveredMessageCount int         `json:"delivered_message_count"`
	ActualCanonicalSHA    string      `json:"actual_canonical_sha,omitempty"`
	ActualCanonicalError  string      `json:"actual_canonical_error,omitempty"`
	Stale                 bool        `json:"stale"`
	Derived               bool        `json:"derived"`
}

// TaskDetail is the bounded single-Task read. It keeps the complete Task while
// joining at most one current Run and one latest progress Event.
type TaskDetail struct {
	Task                  Task   `json:"task"`
	CurrentRun            *Run   `json:"current_run,omitempty"`
	LatestProgress        *Event `json:"latest_progress,omitempty"`
	PendingMessageCount   int    `json:"pending_message_count"`
	DeliveredMessageCount int    `json:"delivered_message_count"`
	ActualCanonicalSHA    string `json:"actual_canonical_sha,omitempty"`
	ActualCanonicalError  string `json:"actual_canonical_error,omitempty"`
	Stale                 bool   `json:"stale"`
	Derived               bool   `json:"derived"`
}

// TaskSummary deliberately omits descriptions and terminal history fields;
// the exact Task remains available from the paginated task read surface.
type TaskSummary struct {
	ID                         string     `json:"id"`
	ProjectID                  string     `json:"project_id"`
	Kind                       TaskKind   `json:"kind"`
	ParentTaskID               string     `json:"parent_task_id,omitempty"`
	AssigneeAgentID            string     `json:"assignee_agent_id"`
	Title                      string     `json:"title"`
	TitleTruncated             bool       `json:"title_truncated,omitempty"`
	Priority                   int        `json:"priority"`
	Status                     TaskStatus `json:"status"`
	CurrentRunID               string     `json:"current_run_id,omitempty"`
	Generation                 int64      `json:"generation"`
	NextRunAt                  string     `json:"next_run_at"`
	RetryCount                 int        `json:"retry_count"`
	MaxRetries                 int        `json:"max_retries"`
	WaitReason                 string     `json:"wait_reason,omitempty"`
	ResultSummary              string     `json:"result_summary,omitempty"`
	FailureReason              string     `json:"failure_reason,omitempty"`
	TextTruncated              bool       `json:"text_truncated,omitempty"`
	BaseSHA                    string     `json:"base_sha,omitempty"`
	HeadSHA                    string     `json:"head_sha,omitempty"`
	TaskRef                    string     `json:"task_ref,omitempty"`
	AcceptedByKind             string     `json:"accepted_by_kind,omitempty"`
	AcceptedByID               string     `json:"accepted_by_id,omitempty"`
	AcceptedIntegrationAgentID string     `json:"accepted_integration_agent_id,omitempty"`
	FinalCanonicalSHA          string     `json:"final_canonical_sha,omitempty"`
	IntegrationTaskID          string     `json:"integration_task_id,omitempty"`
	SourceTaskID               string     `json:"source_task_id,omitempty"`
	SourceRunID                string     `json:"source_run_id,omitempty"`
	SourceTaskRef              string     `json:"source_task_ref,omitempty"`
	SourceHeadSHA              string     `json:"source_head_sha,omitempty"`
	SourceRefReleasedAt        string     `json:"source_ref_released_at,omitempty"`
	PendingAction              string     `json:"pending_action,omitempty"`
	PendingActionID            string     `json:"pending_action_id,omitempty"`
	Version                    int64      `json:"version"`
	CreatedAt                  string     `json:"created_at"`
	UpdatedAt                  string     `json:"updated_at"`
	SubmittedAt                string     `json:"submitted_at,omitempty"`
	CompletedAt                string     `json:"completed_at,omitempty"`
	ClosedAt                   string     `json:"closed_at,omitempty"`
}

// RunSummary contains only current coordination facts. Full immutable Run
// history is served through the run page and show endpoints.
type RunSummary struct {
	ID                   string   `json:"id"`
	ProjectID            string   `json:"project_id"`
	TaskID               string   `json:"task_id"`
	AgentID              string   `json:"agent_id"`
	Generation           int64    `json:"generation"`
	State                RunState `json:"state"`
	ContainerIDPresent   bool     `json:"container_id_present"`
	NativeSessionPresent bool     `json:"native_session_present"`
	HeartbeatAt          string   `json:"heartbeat_at,omitempty"`
	DeadlineAt           string   `json:"deadline_at,omitempty"`
	LastObservedAt       string   `json:"last_observed_at,omitempty"`
	LaunchPhase          string   `json:"launch_phase"`
	CleanupState         string   `json:"cleanup_state"`
	TerminalReason       string   `json:"terminal_reason,omitempty"`
	LastError            string   `json:"last_error,omitempty"`
	RuntimeErrorCode     string   `json:"runtime_error_code,omitempty"`
	TextTruncated        bool     `json:"text_truncated,omitempty"`
	Version              int64    `json:"version"`
	CreatedAt            string   `json:"created_at"`
	StartedAt            string   `json:"started_at,omitempty"`
	EndedAt              string   `json:"ended_at,omitempty"`
}

type ProjectSummary struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	CanonicalRef       string        `json:"canonical_ref"`
	CanonicalSHA       string        `json:"canonical_sha"`
	IntegrationAgentID string        `json:"integration_agent_id,omitempty"`
	Status             ProjectStatus `json:"status"`
	PendingAction      string        `json:"pending_action,omitempty"`
	LastError          string        `json:"last_error,omitempty"`
	Version            int64         `json:"version"`
	CreatedAt          string        `json:"created_at"`
	UpdatedAt          string        `json:"updated_at"`
}

type AgentSummary struct {
	ID          string      `json:"id"`
	DisplayName string      `json:"display_name"`
	AdapterID   string      `json:"adapter_id"`
	Image       string      `json:"image"`
	Status      AgentStatus `json:"status"`
	Version     int64       `json:"version"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
}

type GitState struct {
	ProjectID    string `json:"project_id"`
	CanonicalRef string `json:"canonical_ref"`
	ActualSHA    string `json:"actual_sha,omitempty"`
	Error        string `json:"error,omitempty"`
}

type GCPreview struct {
	GeneratedAt string              `json:"generated_at"`
	Workspaces  []GCWorkspaceTarget `json:"workspaces"`
	TaskRefs    []GCTaskRefTarget   `json:"task_refs"`
	AgentHomes  []GCAgentHomeTarget `json:"agent_homes"`
}

type GCAgentHomeTarget struct {
	AgentID  string   `json:"agent_id"`
	Exists   bool     `json:"exists"`
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

type GCWorkspaceTarget struct {
	TaskID      string   `json:"task_id"`
	TaskVersion int64    `json:"task_version"`
	Exists      bool     `json:"exists"`
	Fingerprint string   `json:"fingerprint"`
	ActualHead  string   `json:"actual_head,omitempty"`
	Eligible    bool     `json:"eligible"`
	Reasons     []string `json:"reasons,omitempty"`
}

type GCTaskRefTarget struct {
	TaskID    string   `json:"task_id"`
	RunID     string   `json:"run_id"`
	ActualSHA string   `json:"actual_sha,omitempty"`
	Exists    bool     `json:"exists"`
	Eligible  bool     `json:"eligible"`
	Reasons   []string `json:"reasons,omitempty"`
}

type GCRunResult struct {
	Completed bool `json:"completed"`
}

type GCDiscardResult struct {
	TaskID    string `json:"task_id"`
	RunID     string `json:"run_id,omitempty"`
	Discarded bool   `json:"discarded"`
}

func IsTaskClosed(status TaskStatus) bool {
	return status == TaskCompleted || status == TaskCancelled
}

func IsTaskOpen(status TaskStatus) bool {
	return !IsTaskClosed(status)
}

func IsRunLive(state RunState) bool {
	return state == RunStarting || state == RunActive
}

func IsRunTerminal(state RunState) bool {
	return !IsRunLive(state)
}
