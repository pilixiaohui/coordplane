package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const claudeAdapterName = "claude"

const ContainerBootstrapPath = "/run/coordplane/bootstrap"

var claudeAllowedEfforts = []string{"low", "medium", "high"}

type Claude struct{}

func (Claude) Name() string { return claudeAdapterName }

func (Claude) Metadata() Metadata {
	return Metadata{
		Name: claudeAdapterName, ExecutionModel: ExecutionOneShot, SupportsResume: true,
		AllowedEfforts: append([]string(nil), claudeAllowedEfforts...),
	}
}

func (Claude) BuildStartCommand(spec LaunchSpec) (CommandSpec, error) {
	if err := validateLaunch(spec); err != nil {
		return CommandSpec{}, err
	}
	args := []string{
		"-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--", bootstrapReferencePrompt(),
	}
	return CommandSpec{Executable: "claude", Args: args, Env: claudeEnvironment(spec)}, nil
}

func (Claude) BuildResumeCommand(spec ResumeSpec) (CommandSpec, error) {
	if err := validateLaunch(spec.LaunchSpec); err != nil {
		return CommandSpec{}, err
	}
	sessionID := strings.TrimSpace(spec.NativeSessionID)
	if sessionID == "" {
		return CommandSpec{}, errors.New("adapter: native session ID is required for resume")
	}
	args := []string{
		"-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--resume", sessionID,
		"--", bootstrapReferencePrompt(),
	}
	return CommandSpec{Executable: "claude", Args: args, Env: claudeEnvironment(spec.LaunchSpec)}, nil
}

func (Claude) BuildInjectInput(MessageInput) (RuntimeInput, error) {
	return RuntimeInput{}, ErrInjectUnsupported
}

func (Claude) ResumeCompatible(previous, next SessionContext) bool {
	return previous.AdapterID == claudeAdapterName && next.AdapterID == claudeAdapterName &&
		previous.AgentID != "" && previous.AgentID == next.AgentID &&
		previous.TaskID != "" && previous.TaskID == next.TaskID &&
		previous.WorkspaceID == next.WorkspaceID
}

func claudeEnvironment(spec LaunchSpec) map[string]string {
	env := map[string]string{"HOME": spec.ContainerHome}
	if value := strings.TrimSpace(spec.Provider.Model); value != "" {
		env["ANTHROPIC_MODEL"] = value
	}
	if value := strings.TrimSpace(spec.Provider.SubagentModel); value != "" {
		env["CLAUDE_CODE_SUBAGENT_MODEL"] = value
	}
	if value := strings.TrimSpace(spec.Provider.BaseURL); value != "" {
		env["ANTHROPIC_BASE_URL"] = value
	}
	if value := strings.TrimSpace(spec.Provider.Effort); value != "" {
		env["CLAUDE_CODE_EFFORT_LEVEL"] = value
	}
	return env
}

func (Claude) ParseEvent(frame []byte) (Event, error) {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return Event{}, errors.New("adapter: empty Claude protocol frame")
	}
	var payload claudeEnvelope
	if err := json.Unmarshal(frame, &payload); err != nil {
		return Event{}, errors.New("adapter: invalid Claude JSON event")
	}
	event := Event{Kind: EventProtocol, Raw: append(json.RawMessage(nil), frame...)}
	switch payload.Type {
	case "system":
		if payload.Subtype != "init" {
			return event, nil
		}
		event.Kind = EventSessionStarted
		event.NativeSessionID = payload.SessionID
		if strings.TrimSpace(event.NativeSessionID) == "" {
			return Event{}, errors.New("adapter: Claude system init omitted session_id")
		}
	case "result":
		if payload.Subtype == "success" {
			if payload.IsError == nil {
				return Event{}, errors.New("adapter: Claude success result omitted is_error")
			}
			if !*payload.IsError {
				return event, nil
			}
		}
		event.Kind = EventProviderError
		event.Message = claudeProtocolMessage(payload)
	case "error":
		event.Kind = EventProviderError
		event.Message = claudeProtocolMessage(payload)
	case "assistant":
		return parseClaudeAssistantEvent(event, payload.Message)
	}
	return event, nil
}

type claudeEnvelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   *bool           `json:"is_error"`
	Result    string          `json:"result"`
	Message   json.RawMessage `json:"message"`
	Error     json.RawMessage `json:"error"`
}

type claudeAssistantMessage struct {
	Type, Role string
	Content    []json.RawMessage
}

type claudeTextBlock struct{ Type, Text string }
type claudeThinkingBlock struct{ Type, Thinking, Signature string }

type claudeToolUseBlock struct {
	Type, ID, Name string
	Input          json.RawMessage
}

func parseClaudeAssistantEvent(event Event, rawMessage json.RawMessage) (Event, error) {
	rawMessage = bytes.TrimSpace(rawMessage)
	var legacy string
	if len(rawMessage) > 0 && rawMessage[0] == '"' && json.Unmarshal(rawMessage, &legacy) == nil {
		return event, nil
	}
	var message claudeAssistantMessage
	if err := json.Unmarshal(rawMessage, &message); err != nil || message.Type != "message" || message.Role != "assistant" || message.Content == nil {
		return Event{}, errors.New("adapter: unsupported Claude assistant message")
	}
	content := make([]any, 0, len(message.Content))
	for _, rawBlock := range message.Content {
		block, keep, err := parseClaudeContentBlock(rawBlock)
		if err != nil {
			return Event{}, err
		}
		if keep {
			content = append(content, block)
		}
	}
	event.Raw = nil
	if len(content) > 0 {
		event.Raw, _ = json.Marshal(map[string]any{
			"type": "assistant", "message": map[string]any{"type": "message", "role": "assistant", "content": content},
		})
	}
	return event, nil
}

func parseClaudeContentBlock(raw []byte) (any, bool, error) {
	var typed struct{ Type string }
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, false, errors.New("adapter: invalid Claude assistant content block")
	}
	switch typed.Type {
	case "thinking":
		// Thinking is private reasoning: always discarded, never enters events.
		// Real streaming paths omit the signature and may emit hollow or
		// signature-only blocks, and blocks may carry extra fields (id); all of
		// these are dropped with the block, so only structural validity matters.
		var block claudeThinkingBlock
		if json.Unmarshal(raw, &block) != nil {
			return nil, false, errors.New("adapter: invalid Claude assistant content block")
		}
		return nil, false, nil
	case "text":
		// Real text blocks carry extra fields (id, citations); the event
		// projection below is a whitelist, so extra fields never leak in.
		var block claudeTextBlock
		if json.Unmarshal(raw, &block) != nil {
			return nil, false, errors.New("adapter: invalid Claude assistant content block")
		}
		// Empty text blocks appear in real streaming frames; dropping them is
		// lossless (no visible content) and must not fail the whole run.
		if strings.TrimSpace(block.Text) == "" {
			return nil, false, nil
		}
		return map[string]any{"type": "text", "text": block.Text}, true, nil
	case "tool_use":
		var block claudeToolUseBlock
		// Unknown fields (e.g. streaming "partial") are tolerated, but the
		// input safety validation stays strict: id/name must be non-blank and
		// input must be a non-null JSON object.
		if json.Unmarshal(raw, &block) != nil || strings.TrimSpace(block.ID) == "" || strings.TrimSpace(block.Name) == "" || json.Unmarshal(block.Input, &struct{}{}) != nil || bytes.Equal(bytes.TrimSpace(block.Input), []byte("null")) {
			return nil, false, errors.New("adapter: invalid Claude tool_use block")
		}
		return map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": block.Input}, true, nil
	default:
		return nil, false, errors.New("adapter: unsupported Claude assistant content block")
	}
}

func validateLaunch(spec LaunchSpec) error {
	if spec.BootstrapPath != ContainerBootstrapPath {
		return errors.New("adapter: bootstrap path must be the fixed run-control file")
	}
	if spec.ContainerHome != "/home/agent" {
		return errors.New("adapter: container home must be /home/agent")
	}
	if spec.Conversation {
		if spec.ContainerWork != "/home/agent" {
			return errors.New("adapter: conversation work directory must be /home/agent")
		}
	} else if spec.ContainerWork != "/workspace/project" {
		return errors.New("adapter: code work directory must be /workspace/project")
	}
	return nil
}

func bootstrapReferencePrompt() string {
	return "Read and follow the complete CoordPlane run bootstrap at " + ContainerBootstrapPath + " before acting."
}

func claudeProtocolMessage(payload claudeEnvelope) string {
	var message string
	if json.Unmarshal(payload.Message, &message) == nil && strings.TrimSpace(message) != "" {
		return message
	}
	if strings.TrimSpace(payload.Result) != "" {
		return payload.Result
	}
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload.Error, &detail) == nil && strings.TrimSpace(detail.Message) != "" {
		return detail.Message
	}
	var errorMessage string
	if json.Unmarshal(payload.Error, &errorMessage) == nil && strings.TrimSpace(errorMessage) != "" {
		return errorMessage
	}
	return "Claude provider error"
}
