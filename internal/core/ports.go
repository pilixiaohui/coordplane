package core

import "context"

type Repository interface {
	Transact(context.Context, func(Transaction) error) error
	Project(context.Context, string) (Project, error)
	ProjectsByStatus(context.Context, ...ProjectStatus) ([]Project, error)
	Agent(context.Context, string) (Agent, error)
	Projects(context.Context, ProjectFilter) (ProjectPage, error)
	Agents(context.Context, AgentFilter) (AgentPage, error)
	Task(context.Context, string) (Task, error)
	PendingGitTasks(context.Context) ([]Task, error)
	TaskRefCandidates(context.Context, string) ([]Task, error)
	TaskRefEligible(context.Context, string, string, string) (bool, error)
	WorkspaceCandidates(context.Context, string) ([]Task, error)
	WorkspaceEligible(context.Context, string, string) (bool, error)
	Run(context.Context, string) (Run, error)
	LiveRuns(context.Context) ([]Run, error)
	RunsNeedingCleanup(context.Context) ([]Run, error)
	LatestTerminalRun(context.Context, string, string) (Run, error)
	TaskHasStartedRun(context.Context, string) (bool, error)
	Snapshot(context.Context, string) (Snapshot, error)
	StatusProjection(context.Context, string) (StatusProjection, error)
	TaskProjection(context.Context, string) (TaskProjection, error)
	Role(context.Context, string) (Role, error)
	Roles(context.Context) ([]Role, error)
	Participant(context.Context, string) (Participant, error)
	Participants(context.Context) ([]Participant, error)
	ParticipantRoles(context.Context, string) ([]ParticipantRoleBinding, error)
	Tasks(context.Context, TaskFilter) (TaskPage, error)
	Runs(context.Context, RunFilter) (RunPage, error)
	Messages(context.Context, MessageFilter) (MessagePage, error)
	EventsPage(context.Context, EventFilter) (EventPage, error)
	Events(context.Context, EventFilter) ([]Event, error)
}

type Transaction interface {
	Dedupe(string, string, string) ([]byte, bool, error)
	PutDedupe(string, string, string, []byte, string) error

	Project(string) (Project, error)
	ProjectByName(string) (Project, error)
	InsertProject(Project) error
	UpdateProject(Project, int64, ProjectStatus) error
	ProjectsByIntegrationAgent(string) ([]Project, error)
	ProjectBlockers(string) (LifecycleBlockers, error)

	Agent(string) (Agent, error)
	InsertAgent(Agent) error
	UpdateAgent(Agent, int64, AgentStatus) error
	AgentBlockers(string) (LifecycleBlockers, error)

	Task(string) (Task, error)
	RunnableTasks(string) ([]Task, error)
	Conversation(string, string) (Task, error)
	InsertTask(Task) error
	UpdateTask(Task, int64, TaskStatus) error

	Run(string) (Run, error)
	RunByTokenHash(string) (Run, error)
	InsertRun(Run) error
	UpdateRun(Run, int64, RunState) error
	LiveRunCount(string, string) (int, error)
	AgentRuntimeOccupancy(string) (int, error)

	Message(string) (Message, error)
	MessagesForTask(string) ([]Message, error)
	MessagesForRun(string) ([]Message, error)
	MessagesForRecipient(string, string) ([]Message, error)
	PendingWakeAt(string) (string, bool, error)
	InsertMessage(Message) error
	UpdateMessage(Message, int64, MessageState) error

	Role(string) (Role, error)
	Roles() ([]Role, error)
	InsertRole(Role) error
	UpdateRole(Role, int64) error
	DeleteRole(string) error
	RoleBindingCount(string) (int, error)

	Participant(string) (Participant, error)
	Participants() ([]Participant, error)
	InsertParticipant(Participant) error
	ParticipantRoles(string) ([]ParticipantRoleBinding, error)
	InsertParticipantRole(ParticipantRoleBinding) error
	DeleteParticipantRole(string, string, string) error
	Credential(string) (Credential, error)
	Credentials(string) ([]Credential, error)
	InsertCredential(Credential) error
	UpdateCredential(Credential, CredentialStatus) error
	SetParticipantCredential(string, string) error

	AppendEvent(Event) (Event, error)
}

type LifecycleBlockers struct {
	LiveRuns                  int
	OpenTasks                 int
	PendingActions            int
	AcceptedIntegrationSource int
	UnresolvedAgentMessages   int
}

type MessageFilter struct {
	ProjectID     string
	TaskID        string
	RecipientKind string
	RecipientID   string
	Cursor        string
	Limit         int
}

type ProjectFilter struct {
	Cursor string
	Limit  int
}

type AgentFilter struct {
	Cursor string
	Limit  int
}

type TaskFilter struct {
	ProjectID string
	Cursor    string
	Limit     int
}

type RunFilter struct {
	ProjectID string
	TaskID    string
	AgentID   string
	Cursor    string
	Limit     int
}

type EventFilter struct {
	ProjectID  string
	EntityType string
	EntityID   string
	RunID      string
	Kind       string
	Cursor     string
	Limit      int
}

type ProjectGit interface {
	Preflight(context.Context, string, string) (ProjectGitFact, error)
	ControlPath(string) string
	Initialize(context.Context, ProjectGitIntent) (ProjectGitFact, error)
	Verify(context.Context, ProjectGitIntent) (ProjectGitFact, error)
	Exists(string) bool
	Resolve(context.Context, string, string) (string, error)
}

type ProjectGitIntent struct {
	ProjectID      string
	Source         string
	SourceRef      string
	InitialSHA     string
	ControlRepo    string
	CanonicalRef   string
	OperationID    string
	ExpectedStatus ProjectStatus
}

type ProjectGitFact struct {
	Source       string
	SourceRef    string
	InitialSHA   string
	CanonicalRef string
	CanonicalSHA string
}

// AgentHomeGC owns the narrow filesystem boundary for archived Agent homes.
// Core supplies the final durable authorization immediately before deletion.
type AgentHomeGC interface {
	State(context.Context, string) (AgentHomeStateFact, error)
	Delete(context.Context, string, func() (bool, error)) (bool, error)
}

type AgentHomeStateFact struct {
	Exists bool `json:"exists"`
}

// TaskGit is the trusted Git boundary used by the daemon-owned intent
// reconciler. It is separate from ProjectGit so project registration fakes do
// not need to implement result mutation behavior.
type TaskGit interface {
	Capture(context.Context, GitCaptureIntent) (GitCaptureFact, error)
	CleanupCapture(context.Context, GitCaptureIntent) error
	Advance(context.Context, GitAdvanceIntent) (GitAdvanceFact, error)
	ResolveTaskRef(context.Context, GitTaskRefIntent) (string, error)
	UseTaskRef(context.Context, GitTaskRefIntent, func(string) error) error
	Checkout(context.Context, GitCheckoutIntent) (GitCheckoutFact, error)
	WorkspaceState(context.Context, GitWorkspaceStateIntent) (GitWorkspaceStateFact, error)
	DiscardWorkspace(context.Context, GitDiscardWorkspaceIntent, func() (bool, error)) (bool, error)
	TaskRefState(context.Context, GitDeleteRefIntent) (GitTaskRefStateFact, error)
	DeleteTaskRefAndPrune(context.Context, GitDeleteRefIntent, func() (bool, error)) (bool, error)
	DeleteWorkspace(context.Context, GitDeleteWorkspaceIntent, func() (bool, error)) (bool, error)
}

type GitSource struct {
	TaskID  string
	RunID   string
	TaskRef string
	HeadSHA string
}

type GitCaptureIntent struct {
	ProjectID     string
	TaskID        string
	RunID         string
	WorkspacePath string
	ControlRepo   string
	BaseSHA       string
	ExpectedHead  string
	Source        *GitSource
	OperationID   string
}

type GitCaptureFact struct {
	HeadSHA string
	TaskRef string
}

type GitAdvanceIntent struct {
	ProjectID      string
	TaskID         string
	RunID          string
	OperationID    string
	ControlRepo    string
	CanonicalRef   string
	TaskRef        string
	ExpectedOldSHA string
	TargetSHA      string
}

type GitAdvanceOutcome string

const (
	GitAdvanceUpdated  GitAdvanceOutcome = "updated"
	GitAdvanceIncluded GitAdvanceOutcome = "included"
	GitAdvanceStale    GitAdvanceOutcome = "stale"
)

type GitAdvanceFact struct {
	Outcome   GitAdvanceOutcome
	ActualSHA string
}

type GitTaskRefIntent struct {
	ProjectID   string
	ControlRepo string
	TaskRef     string
	ExpectedSHA string
}

type GitCheckoutIntent struct {
	GitTaskRefIntent
	Destination string
}

type GitCheckoutFact struct {
	Destination string `json:"destination"`
	HeadSHA     string `json:"head_sha"`
}

type GitDeleteRefIntent struct {
	ProjectID    string
	ControlRepo  string
	CanonicalRef string
	TaskRef      string
	ExpectedSHA  string
	AllowDiscard bool
}

type GitDeleteWorkspaceIntent struct {
	ProjectID    string
	TaskID       string
	BaseSHA      string
	ExpectedHead string
	Source       *GitSource
}

type GitWorkspaceStateIntent struct {
	GitDeleteWorkspaceIntent
	TaskVersion int64
}

type GitWorkspaceStateFact struct {
	Exists      bool   `json:"exists"`
	Fingerprint string `json:"fingerprint"`
	HeadSHA     string `json:"head_sha,omitempty"`
	Clean       bool   `json:"clean"`
}

type GitDiscardWorkspaceIntent struct {
	GitWorkspaceStateIntent
	ExpectedFingerprint string
}

type GitTaskRefStateFact struct {
	Exists    bool   `json:"exists"`
	ActualSHA string `json:"actual_sha,omitempty"`
	Included  bool   `json:"included_in_canonical"`
}
