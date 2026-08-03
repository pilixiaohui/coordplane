package transport

import (
	"context"

	"coordplane/internal/core"
)

// Readiness is the process-local recovery fence shared by both HTTP surfaces.
type Readiness interface {
	RequireReady() error
}

// OperatorOperations is the fixed Boss-facing operation surface.
type OperatorOperations interface {
	Readiness
	Status(context.Context, string) (core.Status, error)
	Project(context.Context, string) (core.ProjectDetail, error)
	Agent(context.Context, string) (core.Agent, error)
	ListProjects(context.Context, core.ProjectFilter) (core.ProjectPage, error)
	ListAgents(context.Context, core.AgentFilter) (core.AgentPage, error)
	AddProject(context.Context, core.AddProjectInput) (core.Project, error)
	RepairProject(context.Context, string, string) (core.Project, error)
	ArchiveProject(context.Context, string, string) (core.Project, error)
	AddAgent(context.Context, core.AddAgentInput) (core.Agent, error)
	SetAgentStatus(context.Context, string, core.AgentStatus, string) (core.Agent, error)
	ArchiveAgent(context.Context, string, string) (core.Agent, error)
	Chat(context.Context, core.ChatInput) (core.ChatResult, error)
	CreateTask(context.Context, core.CreateTaskInput) (core.Task, error)
	CompleteTask(context.Context, core.CompleteTaskInput) (core.Task, error)
	Task(context.Context, string) (core.TaskDetail, error)
	CheckoutTask(context.Context, core.TaskCheckoutInput) (core.GitCheckoutFact, error)
	Run(context.Context, string) (core.Run, error)
	CloseConversation(context.Context, string, string) (core.Task, error)
	WakeTask(context.Context, core.TaskActionInput) (core.Task, error)
	RetryTask(context.Context, core.TaskActionInput) (core.Task, error)
	CancelTask(context.Context, core.TaskActionInput) (core.Task, error)
	RequestAccept(context.Context, core.AcceptInput) (core.Task, error)
	ReworkTask(context.Context, core.TaskActionInput) (core.Task, error)
	RequestRunStop(context.Context, core.RunStopInput) (core.Run, error)
	ListTasks(context.Context, core.TaskFilter) (core.TaskPage, error)
	ListRuns(context.Context, core.RunFilter) (core.RunPage, error)
	ListMessages(context.Context, core.MessageFilter) (core.MessagePage, error)
	SendBossMessage(context.Context, core.BossMessageInput) (core.Message, error)
	ReadBossMessage(context.Context, string, string) (core.Message, error)
	AcknowledgeBossMessage(context.Context, string, string) (core.Message, error)
	RetryMessage(context.Context, string, string) (core.Message, error)
	ListEvents(context.Context, core.EventFilter) (core.EventPage, error)
	GCPreview(context.Context) (core.GCPreview, error)
	GCRun(context.Context, core.GCRunInput) (core.GCRunResult, error)
	GCDiscardWorkspace(context.Context, core.GCDiscardWorkspaceInput) (core.GCDiscardResult, error)
	GCDiscardTaskRef(context.Context, core.GCDiscardTaskRefInput) (core.GCDiscardResult, error)
	AuthenticateOperator(context.Context, string) error
	AddCredential(context.Context, core.AddCredentialInput) (core.Credential, error)
	RotateCredential(context.Context, core.AddCredentialInput) (core.Credential, error)
	RevokeCredential(context.Context, string, string) (core.Credential, error)
	Role(context.Context, string) (core.Role, error)
	ListRoles(context.Context) ([]core.Role, error)
	CreateRole(context.Context, core.RoleInput) (core.Role, error)
	UpdateRole(context.Context, core.RoleUpdateInput) (core.Role, error)
	DeleteRole(context.Context, string, string) error
	Participant(context.Context, string) (core.Participant, error)
	ListParticipants(context.Context) ([]core.Participant, error)
	BindParticipantRole(context.Context, core.BindRoleInput) (core.ParticipantRoleBinding, error)
	UnbindParticipantRole(context.Context, core.BindRoleInput) error
}

// RunOperations is the fixed Agent-facing operation surface. The transport
// forwards the bearer token; scope and generation checks remain in core.
type RunOperations interface {
	Readiness
	CurrentTask(context.Context, string) (core.CurrentTaskResult, error)
	TaskForRun(context.Context, string, string) (core.Task, error)
	CreateChildTask(context.Context, core.CreateChildTaskInput) (core.Task, error)
	RequestOutcome(context.Context, core.OutcomeInput) (core.OutcomeResult, error)
	RequestAccept(context.Context, core.AcceptInput) (core.Task, error)
	ReworkTask(context.Context, core.TaskActionInput) (core.Task, error)
	Inbox(context.Context, string) ([]core.Message, error)
	InboxMessage(context.Context, string, string) (core.Message, error)
	AcknowledgeAgentMessages(context.Context, core.AcknowledgeMessagesInput) ([]core.Message, error)
	SendAgentMessage(context.Context, core.SendMessageInput) (core.Message, error)
	Progress(context.Context, core.ProgressInput) (core.Event, error)
}

// ScopedRunOperations adds the read-only listener-scope authorization needed
// before a per-Run handler forwards any request to the regular Core operation.
type ScopedRunOperations interface {
	RunOperations
	AuthorizeRunScope(context.Context, string, core.RunScope) error
}

var _ OperatorOperations = (*core.Service)(nil)
var _ RunOperations = (*core.Service)(nil)
var _ ScopedRunOperations = (*core.Service)(nil)
