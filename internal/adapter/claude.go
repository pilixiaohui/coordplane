package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const claudeAdapterName = "claude"

const ContainerBootstrapPath = "/run/coordplane/bootstrap"

type Claude struct{}

func (Claude) Name() string { return claudeAdapterName }

func (Claude) Metadata() Metadata {
	return Metadata{ExecutionModel: ExecutionOneShot, SupportsResume: true}
}

func (Claude) BuildStartCommand(spec LaunchSpec) (CommandSpec, error) {
	if err := validateLaunch(spec); err != nil {
		return CommandSpec{}, err
	}
	args := []string{
		"-p", "--bare", "--verbose", "--output-format", "stream-json",
		"--dangerously-skip-permissions", "--", bootstrapReferencePrompt(),
	}
	return CommandSpec{
		Executable: "claude",
		Args:       args,
		Env:        map[string]string{"HOME": spec.ContainerHome},
	}, nil
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
	return CommandSpec{
		Executable: "claude",
		Args:       args,
		Env:        map[string]string{"HOME": spec.ContainerHome},
	}, nil
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
	}
	return event, nil
}

type claudeEnvelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   *bool           `json:"is_error"`
	Result    string          `json:"result"`
	Message   string          `json:"message"`
	Error     json.RawMessage `json:"error"`
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
	for _, message := range []string{payload.Message, payload.Result} {
		if strings.TrimSpace(message) != "" {
			return message
		}
	}
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload.Error, &detail) == nil && strings.TrimSpace(detail.Message) != "" {
		return detail.Message
	}
	var message string
	if json.Unmarshal(payload.Error, &message) == nil && strings.TrimSpace(message) != "" {
		return message
	}
	return "Claude provider error"
}
