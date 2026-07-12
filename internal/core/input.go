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
	ProjectID     string   `json:"project_id"`
	AgentID       string   `json:"agent_id"`
	Body          string   `json:"body"`
	RelatedTask   string   `json:"related_task_id,omitempty"`
	Wake          bool     `json:"wake"`
	AckMessageIDs []string `json:"ack_message_ids,omitempty"`
	RequestID     string   `json:"request_id"`
	ReplyTo       string   `json:"reply_to_message_id,omitempty"`
}

type ChatResult struct {
	Task    Task    `json:"task"`
	Message Message `json:"message"`
}

type BossMessageInput struct {
	ProjectID     string   `json:"project_id"`
	AgentID       string   `json:"agent_id"`
	TaskID        string   `json:"task_id,omitempty"`
	RelatedTaskID string   `json:"related_task_id,omitempty"`
	Body          string   `json:"body"`
	Wake          bool     `json:"wake"`
	ReplyTo       string   `json:"reply_to_message_id,omitempty"`
	AckMessageIDs []string `json:"ack_message_ids,omitempty"`
	RequestID     string   `json:"request_id"`
}

type CreateTaskInput struct {
	ProjectID       string   `json:"project_id"`
	Kind            TaskKind `json:"kind"`
	AssigneeAgentID string   `json:"assignee_agent_id"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Priority        int      `json:"priority"`
	MaxRetries      int      `json:"max_retries"`
	AckMessageIDs   []string `json:"ack_message_ids,omitempty"`
	RequestID       string   `json:"request_id"`
}

type CreateChildTaskInput struct {
	Token           string   `json:"-"`
	AssigneeAgentID string   `json:"assignee_agent_id"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Priority        int      `json:"priority"`
	MaxRetries      int      `json:"max_retries"`
	AckMessageIDs   []string `json:"ack_message_ids,omitempty"`
	RequestID       string   `json:"request_id"`
}

type Claim struct {
	Task  Task   `json:"task"`
	Run   Run    `json:"run"`
	Token string `json:"token,omitempty"`
}

type CurrentTaskResult struct {
	Task               Task `json:"task"`
	Run                Run  `json:"run"`
	UnreadMessageCount int  `json:"unread_message_count"`
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

type SendMessageInput struct {
	Token         string   `json:"-"`
	RecipientKind string   `json:"recipient_kind"`
	RecipientID   string   `json:"recipient_id,omitempty"`
	TaskID        string   `json:"task_id,omitempty"`
	RelatedTaskID string   `json:"related_task_id,omitempty"`
	Body          string   `json:"body"`
	Wake          bool     `json:"wake"`
	ReplyTo       string   `json:"reply_to_message_id,omitempty"`
	AckMessageIDs []string `json:"ack_message_ids,omitempty"`
	RequestID     string   `json:"request_id"`
}

type AcknowledgeMessagesInput struct {
	Token      string   `json:"-"`
	MessageIDs []string `json:"message_ids"`
	RequestID  string   `json:"request_id"`
}

type Outcome string

const (
	OutcomeWait   Outcome = "wait"
	OutcomeSubmit Outcome = "submit"
	OutcomeFail   Outcome = "fail"
)

type OutcomeInput struct {
	Token         string   `json:"-"`
	Outcome       Outcome  `json:"outcome"`
	Reason        string   `json:"reason,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	ExpectedHead  string   `json:"expected_head,omitempty"`
	AckMessageIDs []string `json:"ack_message_ids,omitempty"`
	RequestID     string   `json:"request_id"`
}

type OutcomeResult struct {
	Task         Task      `json:"task"`
	Run          Run       `json:"run"`
	Acknowledged []Message `json:"acknowledged,omitempty"`
}

type MessageDeliveryInput struct {
	RunID       string   `json:"run_id"`
	MessageIDs  []string `json:"message_ids"`
	RequestID   string   `json:"request_id"`
	OperationID string   `json:"operation_id,omitempty"`
}

type RunTerminalInput struct {
	RunID            string   `json:"run_id"`
	State            RunState `json:"state"`
	ExitCode         *int     `json:"exit_code,omitempty"`
	TerminalReason   string   `json:"terminal_reason,omitempty"`
	LastError        string   `json:"last_error,omitempty"`
	RuntimeErrorCode string   `json:"runtime_error_code,omitempty"`
	NativeSessionID  string   `json:"native_session_id,omitempty"`
	RequestID        string   `json:"request_id"`
	OperationID      string   `json:"operation_id,omitempty"`
}

type RunTerminalResult struct {
	Run         Run       `json:"run"`
	Task        Task      `json:"task"`
	Redelivered []Message `json:"redelivered,omitempty"`
}

type TaskActionInput struct {
	Token         string   `json:"-"`
	TaskID        string   `json:"task_id"`
	Reason        string   `json:"reason,omitempty"`
	AckMessageIDs []string `json:"ack_message_ids,omitempty"`
	RequestID     string   `json:"request_id"`
}

type RunStopInput struct {
	RunID       string `json:"run_id"`
	Reason      string `json:"reason,omitempty"`
	RequestID   string `json:"request_id"`
	OperationID string `json:"operation_id,omitempty"`
}

type AcceptInput struct {
	Token              string   `json:"-"`
	TaskID             string   `json:"task_id"`
	IntegrationAgentID string   `json:"integration_agent_id,omitempty"`
	AckMessageIDs      []string `json:"ack_message_ids,omitempty"`
	RequestID          string   `json:"request_id"`
}
