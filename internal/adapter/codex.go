package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const codexAdapterName = "codex"

type Codex struct{}

func (Codex) Name() string { return codexAdapterName }

func (Codex) Metadata() Metadata {
	return Metadata{ExecutionModel: ExecutionOneShot, SupportsResume: true}
}

func (Codex) BuildStartCommand(spec LaunchSpec) (CommandSpec, error) {
	if err := validateLaunch(spec); err != nil {
		return CommandSpec{}, err
	}
	args := []string{
		"exec", "--json", "--color", "never",
		"--dangerously-bypass-approvals-and-sandbox",
	}
	if spec.Conversation {
		args = append(args, "--skip-git-repo-check")
	}
	args = append(args, "--", spec.Bootstrap)
	return CommandSpec{
		Executable: "codex",
		Args:       args,
		Env: map[string]string{
			"HOME":       spec.ContainerHome,
			"CODEX_HOME": spec.ContainerHome,
		},
	}, nil
}

func (Codex) BuildResumeCommand(spec ResumeSpec) (CommandSpec, error) {
	if err := validateLaunch(spec.LaunchSpec); err != nil {
		return CommandSpec{}, err
	}
	sessionID := strings.TrimSpace(spec.NativeSessionID)
	if sessionID == "" {
		return CommandSpec{}, errors.New("adapter: native session ID is required for resume")
	}
	args := []string{
		"exec", "resume", "--json",
		"--dangerously-bypass-approvals-and-sandbox",
	}
	if spec.Conversation {
		args = append(args, "--skip-git-repo-check")
	}
	args = append(args, sessionID, "--", spec.Bootstrap)
	return CommandSpec{
		Executable: "codex",
		Args:       args,
		Env: map[string]string{
			"HOME":       spec.ContainerHome,
			"CODEX_HOME": spec.ContainerHome,
		},
	}, nil
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

func (Codex) ParseEvent(frame []byte) (Event, error) {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return Event{}, errors.New("adapter: empty Codex protocol frame")
	}
	var payload map[string]any
	if err := json.Unmarshal(frame, &payload); err != nil {
		return Event{}, errors.New("adapter: invalid Codex JSON event")
	}
	eventType, _ := payload["type"].(string)
	event := Event{Kind: EventProtocol, Raw: append(json.RawMessage(nil), frame...)}
	switch eventType {
	case "thread.started":
		event.Kind = EventSessionStarted
		event.NativeSessionID, _ = payload["thread_id"].(string)
		if strings.TrimSpace(event.NativeSessionID) == "" {
			return Event{}, errors.New("adapter: Codex thread.started omitted thread_id")
		}
	case "error", "turn.failed":
		event.Message = protocolMessage(payload)
		lower := strings.ToLower(event.Message)
		if strings.Contains(lower, "session") &&
			(strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "unknown")) {
			event.Kind = EventResumeUnavailable
		} else {
			event.Kind = EventProviderError
		}
	}
	return event, nil
}

func validateLaunch(spec LaunchSpec) error {
	if strings.TrimSpace(spec.Bootstrap) == "" {
		return errors.New("adapter: bootstrap prompt is required")
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

func protocolMessage(payload map[string]any) string {
	for _, key := range []string{"message", "error"} {
		switch value := payload[key].(type) {
		case string:
			return value
		case map[string]any:
			if message, ok := value["message"].(string); ok {
				return message
			}
		}
	}
	return "Codex provider error"
}
