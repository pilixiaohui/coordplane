package transport

import (
	"context"

	"coordplane/internal/core"
)

// OperatorOperations is the fixed Boss-facing operation surface.
type OperatorOperations interface {
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
	Task(context.Context, string) (core.TaskDetail, error)
	Run(context.Context, string) (core.Run, error)
	CloseConversation(context.Context, string, string) (core.Task, error)
	ListTasks(context.Context, core.TaskFilter) (core.TaskPage, error)
	ListRuns(context.Context, core.RunFilter) (core.RunPage, error)
	ListMessages(context.Context, core.MessageFilter) (core.MessagePage, error)
	AcknowledgeBossMessage(context.Context, string, string) (core.Message, error)
	ListEvents(context.Context, core.EventFilter) ([]core.Event, error)
}

// RunOperations is the fixed Agent-facing operation surface. The transport
// forwards the bearer token; scope and generation checks remain in core.
type RunOperations interface {
	CurrentTask(context.Context, string) (core.Task, error)
	Progress(context.Context, core.ProgressInput) (core.Event, error)
	AgentMessageToBoss(context.Context, core.AgentMessageInput) (core.Message, error)
	RequestOutcome(context.Context, core.OutcomeInput) (core.Task, error)
}

var _ OperatorOperations = (*core.Service)(nil)
var _ RunOperations = (*core.Service)(nil)
