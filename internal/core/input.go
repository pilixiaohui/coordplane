package core

type AddProjectInput struct {
	Name               string `json:"name"`
	Source             string `json:"source"`
	SourceRef          string `json:"source_ref"`
	IntegrationAgentID string `json:"integration_agent_id,omitempty"`
	RequestID          string `json:"request_id"`
}

type AddAgentInput struct {
	ID               string `json:"id,omitempty"`
	DisplayName      string `json:"display_name"`
	AdapterID        string `json:"adapter_id"`
	Image            string `json:"image"`
	InstructionsFile string `json:"instructions_file"`
	RequestID        string `json:"request_id"`
}

type ChatInput struct {
	ProjectID   string `json:"project_id"`
	AgentID     string `json:"agent_id"`
	Body        string `json:"body"`
	RelatedTask string `json:"related_task_id,omitempty"`
	Wake        bool   `json:"wake"`
	RequestID   string `json:"request_id"`
	ReplyTo     string `json:"reply_to_message_id,omitempty"`
}

type ChatResult struct {
	Task    Task    `json:"task"`
	Message Message `json:"message"`
}

type CreateTaskInput struct {
	ProjectID       string   `json:"project_id"`
	Kind            TaskKind `json:"kind"`
	AssigneeAgentID string   `json:"assignee_agent_id"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Priority        int      `json:"priority"`
	MaxRetries      int      `json:"max_retries"`
	RequestID       string   `json:"request_id"`
}

type Claim struct {
	Task  Task   `json:"task"`
	Run   Run    `json:"run"`
	Token string `json:"token,omitempty"`
}

type RunScope struct {
	ProjectID  string `json:"project_id"`
	AgentID    string `json:"agent_id"`
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id"`
	Generation int64  `json:"generation"`
}

type ProgressInput struct {
	Token     string `json:"-"`
	Summary   string `json:"summary"`
	RequestID string `json:"request_id"`
}

type AgentMessageInput struct {
	Token     string `json:"-"`
	Body      string `json:"body"`
	RequestID string `json:"request_id"`
	ReplyTo   string `json:"reply_to_message_id,omitempty"`
}

type OutcomeInput struct {
	Token        string `json:"-"`
	Outcome      string `json:"outcome"`
	Summary      string `json:"summary,omitempty"`
	ExpectedHead string `json:"expected_head,omitempty"`
	Reason       string `json:"reason,omitempty"`
	RequestID    string `json:"request_id"`
}
