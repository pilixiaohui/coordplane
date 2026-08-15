package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const codexAdapterName = "codex"

const codexContainerCodexHome = "/home/agent/.codex"

// codexAllowedEfforts is the union of the static reasoning-effort values
// shipped by the local Codex CLI 0.146.0 model catalog.
var codexAllowedEfforts = []string{"low", "medium", "high", "xhigh", "max", "ultra"}

// codexResumeUnavailableMarkers is fixed from the Codex 0.146.0 binary
// ("Session not found for thread_id:") and its thread/session counterparts.
var codexResumeUnavailableMarkers = []string{"session not found", "thread not found"}

// codexPrivateToolCallArgumentFields are provider-internal fields that must
// never survive inside tool_call arguments. The same names are rejected at
// every nesting level, including inside arrays.
var codexPrivateToolCallArgumentFields = map[string]bool{
	"signature":         true,
	"encrypted_content": true,
	"usage":             true,
}

// Verification status: testdata/codex-0.146.0-partial-golden.jsonl is a real
// local CLI capture of thread.started, turn.started, top-level error, and an
// error item.completed. The success-side agent_message/tool_call frame shapes
// and the exact resume-not-found JSONL envelope remain UNVERIFIED end-to-end:
// they are implemented from the local 0.146.0 binary's embedded schema and
// must be finalized against a complete golden transcript before live
// acceptance.

// Codex implements the Codex CLI 0.146.0 exec adapter. The event schema is
// deliberately a conservative whitelist; see ParseEvent for what is admitted
// and what is discarded.
type Codex struct{}

func (Codex) Name() string { return codexAdapterName }

func (Codex) Metadata() Metadata {
	return Metadata{
		Name: codexAdapterName, ExecutionModel: ExecutionOneShot, SupportsResume: true,
		AllowedEfforts: append([]string(nil), codexAllowedEfforts...),
	}
}

func (Codex) BuildStartCommand(spec LaunchSpec) (CommandSpec, error) {
	if err := validateLaunch(spec); err != nil {
		return CommandSpec{}, err
	}
	args := []string{
		"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config", "-C", spec.ContainerWork,
	}
	args = append(args, codexProviderArgs(spec.Provider, true)...)
	args = append(args, "--", bootstrapReferencePrompt())
	return CommandSpec{Executable: "codex", Args: args, Env: codexEnvironment(spec.ContainerHome)}, nil
}

func (Codex) BuildResumeCommand(spec ResumeSpec) (CommandSpec, error) {
	if err := validateLaunch(spec.LaunchSpec); err != nil {
		return CommandSpec{}, err
	}
	sessionID := strings.TrimSpace(spec.NativeSessionID)
	if sessionID == "" {
		return CommandSpec{}, errors.New("adapter: native session ID is required for Codex resume")
	}
	args := []string{
		"exec", "resume", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
		"--ignore-user-config",
	}
	// Resume reuses the persisted model binding but re-applies every non-model
	// configuration override from the original launch.
	args = append(args, codexProviderArgs(spec.LaunchSpec.Provider, false)...)
	args = append(args, sessionID, "--", bootstrapReferencePrompt())
	return CommandSpec{Executable: "codex", Args: args, Env: codexEnvironment(spec.LaunchSpec.ContainerHome)}, nil
}

func (Codex) BuildInjectInput(MessageInput) (RuntimeInput, error) {
	return RuntimeInput{}, ErrInjectUnsupported
}

func (Codex) ResumeCompatible(previous, next SessionContext) bool {
	return previous.AdapterID == codexAdapterName && next.AdapterID == codexAdapterName &&
		previous.AgentID != "" && previous.AgentID == next.AgentID &&
		previous.TaskID != "" && previous.TaskID == next.TaskID &&
		previous.WorkspaceID == next.WorkspaceID
}

// ParseEvent parses one stdout line as exactly one JSON object. Unknown
// top-level event types and private item types are ignored without leaking
// their fields. A malformed frame, missing type, or malformed whitelisted
// payload is rejected so the runtime monitor's fail-closed path takes over.
func (Codex) ParseEvent(frame []byte) (Event, error) {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return Event{}, errors.New("adapter: empty Codex protocol frame")
	}
	var envelope codexEnvelope
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return Event{}, errors.New("adapter: invalid Codex JSON event")
	}
	if strings.TrimSpace(envelope.Type) == "" {
		return Event{}, errors.New("adapter: Codex event omitted type")
	}
	switch envelope.Type {
	case "thread.started":
		sessionID := strings.TrimSpace(envelope.ThreadID)
		if sessionID == "" {
			return Event{}, errors.New("adapter: Codex thread.started omitted thread_id")
		}
		raw, err := json.Marshal(map[string]any{"type": envelope.Type, "thread_id": sessionID})
		if err != nil {
			return Event{}, err
		}
		return Event{Kind: EventSessionStarted, NativeSessionID: sessionID, Raw: raw}, nil
	case "turn.started", "turn.completed", "thread.completed":
		raw, err := json.Marshal(map[string]any{"type": envelope.Type})
		if err != nil {
			return Event{}, err
		}
		return Event{Kind: EventProtocol, Raw: raw}, nil
	case "item.started", "item.completed":
		return codexItemEvent(envelope.Type, envelope.Item)
	case "error":
		return codexErrorEvent(map[string]any{"type": envelope.Type, "message": envelope.Message}, envelope.Message)
	default:
		// Syntactically valid unknown events are deliberately ignored.
		return Event{}, nil
	}
}

type codexEnvelope struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Message  string          `json:"message"`
	Item     json.RawMessage `json:"item"`
}

type codexItem struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Message      string          `json:"message"`
	Content      json.RawMessage `json:"content"`
	Name         string          `json:"name"`
	ToolCallID   string          `json:"tool_call_id"`
	Arguments    json.RawMessage `json:"arguments"`
	ToolCall     json.RawMessage `json:"tool_call"`
	FunctionCall json.RawMessage `json:"function_call"`
}

func codexItemEvent(kind string, rawItem json.RawMessage) (Event, error) {
	if len(bytes.TrimSpace(rawItem)) == 0 {
		return Event{}, errors.New("adapter: Codex item event omitted item")
	}
	var item codexItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return Event{}, errors.New("adapter: invalid Codex item")
	}
	if strings.TrimSpace(item.Type) == "" {
		return Event{}, errors.New("adapter: Codex item omitted type")
	}
	if item.Type == "error" {
		return codexErrorEvent(map[string]any{
			"type": kind,
			"item": map[string]any{"id": item.ID, "type": item.Type, "message": item.Message},
		}, item.Message)
	}
	var visible map[string]any
	var err error
	switch item.Type {
	case "agent_message":
		visible, err = codexVisibleAgentMessage(item)
	case "tool_call", "function_call", "mcp_tool_call", "dynamic_tool_call", "custom_tool_call", "collab_agent_tool_call":
		visible, err = codexVisibleToolCall(item)
	default:
		// Private and unknown item types (reasoning, command execution,
		// file changes, encrypted content, and so on) never enter Event.Raw.
		return Event{}, nil
	}
	if err != nil || visible == nil {
		return Event{}, err
	}
	raw, err := json.Marshal(map[string]any{"type": kind, "item": visible})
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: EventProtocol, Raw: raw}, nil
}

func codexVisibleAgentMessage(item codexItem) (map[string]any, error) {
	role := strings.TrimSpace(item.Role)
	if role == "" {
		return nil, errors.New("adapter: Codex agent_message item omitted role")
	}
	content := make([]any, 0)
	rawContent := bytes.TrimSpace(item.Content)
	if len(rawContent) > 0 && !bytes.Equal(rawContent, []byte("null")) {
		var blocks []json.RawMessage
		if err := json.Unmarshal(rawContent, &blocks); err != nil {
			return nil, errors.New("adapter: invalid Codex agent_message content")
		}
		for _, rawBlock := range blocks {
			var block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(rawBlock, &block); err != nil {
				return nil, errors.New("adapter: invalid Codex agent_message content block")
			}
			if block.Type == "output_text" {
				if strings.TrimSpace(block.Text) != "" {
					content = append(content, map[string]any{"type": "output_text", "text": block.Text})
				}
			}
			// Every non-output_text block is private or unsupported and is
			// dropped with its fields.
		}
	}
	if len(content) == 0 {
		return nil, nil
	}
	return map[string]any{"type": "agent_message", "id": item.ID, "role": role, "content": content}, nil
}

func codexVisibleToolCall(item codexItem) (map[string]any, error) {
	call := item
	for _, rawCall := range [][]byte{item.ToolCall, item.FunctionCall} {
		rawCall = bytes.TrimSpace(rawCall)
		if len(rawCall) == 0 {
			continue
		}
		if err := json.Unmarshal(rawCall, &call); err != nil {
			return nil, errors.New("adapter: invalid Codex tool_call payload")
		}
	}
	name := strings.TrimSpace(call.Name)
	if name == "" {
		// Tool-call start deltas can arrive before their stable name; they are
		// ignored rather than treated as protocol failure.
		return nil, nil
	}
	id := strings.TrimSpace(call.ToolCallID)
	if id == "" {
		id = strings.TrimSpace(call.ID)
	}
	projected := map[string]any{"type": "tool_call", "id": id, "name": name}
	rawArguments := bytes.TrimSpace(call.Arguments)
	if len(rawArguments) > 0 && !bytes.Equal(rawArguments, []byte("null")) {
		arguments, err := codexVisibleToolCallArguments(rawArguments)
		if err != nil {
			return nil, err
		}
		projected["arguments"] = arguments
	}
	return projected, nil
}

func codexVisibleToolCallArguments(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil || arguments == nil {
		return nil, errors.New("adapter: invalid Codex tool_call arguments")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("adapter: invalid Codex tool_call arguments")
	}
	return codexSanitizeToolCallArguments(arguments), nil
}

// codexSanitizeToolCallArguments keeps normal tool inputs but recursively
// removes provider-private fields from every object and array.
func codexSanitizeToolCallArguments(value any) any {
	switch typed := value.(type) {
	case []any:
		clean := make([]any, len(typed))
		for index, item := range typed {
			clean[index] = codexSanitizeToolCallArguments(item)
		}
		return clean
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, item := range typed {
			if codexPrivateToolCallArgumentFields[key] {
				continue
			}
			clean[key] = codexSanitizeToolCallArguments(item)
		}
		return clean
	default:
		return value
	}
}

func codexErrorEvent(projection map[string]any, message string) (Event, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return Event{}, errors.New("adapter: Codex error event omitted message")
	}
	kind := EventProviderError
	lower := strings.ToLower(message)
	for _, marker := range codexResumeUnavailableMarkers {
		if strings.Contains(lower, marker) {
			kind = EventResumeUnavailable
			break
		}
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: kind, Message: message, Raw: raw}, nil
}

func codexProviderArgs(provider ProviderConfig, includeModel bool) []string {
	var args []string
	if includeModel {
		if model := strings.TrimSpace(provider.Model); model != "" {
			args = append(args, "-m", model)
		}
	}
	for _, override := range []struct {
		key, value string
	}{
		{"default_subagent_model", provider.SubagentModel},
		{"model_reasoning_effort", provider.Effort},
	} {
		if value := strings.TrimSpace(override.value); value != "" {
			args = append(args, "-c", override.key+"="+value)
		}
	}
	if baseURL := strings.TrimSpace(provider.BaseURL); baseURL != "" {
		// Codex 0.146.0 validates a model_providers.<name> entry's name field,
		// so the fixed provider name must be supplied together with base_url.
		args = append(args,
			"-c", "model_providers.codex.name=codex",
			"-c", "model_providers.codex.base_url="+baseURL,
		)
	}
	return args
}

func codexEnvironment(containerHome string) map[string]string {
	return map[string]string{
		"HOME":       containerHome,
		"CODEX_HOME": codexContainerCodexHome,
	}
}
