package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type ExecutionModel string

const (
	ExecutionOneShot ExecutionModel = "one_shot"
	ExecutionLive    ExecutionModel = "live"
)

var ErrInjectUnsupported = errors.New("adapter: runtime input injection is not supported")

type Metadata struct {
	ExecutionModel ExecutionModel
	SupportsResume bool
	SupportsInject bool
}

type CommandSpec struct {
	Executable string
	Args       []string
	Env        map[string]string
}

type LaunchSpec struct {
	BootstrapPath string
	Conversation  bool
	ContainerHome string
	ContainerWork string
}

type ResumeSpec struct {
	LaunchSpec
	NativeSessionID string
}

type MessageInput struct {
	MessageIDs []string
	Body       string
}

type RuntimeInput struct {
	Bytes []byte
}

type SessionContext struct {
	AdapterID   string
	AgentID     string
	TaskID      string
	WorkspaceID string
}

type EventKind string

const (
	EventProtocol          EventKind = "protocol"
	EventSessionStarted    EventKind = "session_started"
	EventProviderError     EventKind = "provider_error"
	EventResumeUnavailable EventKind = "resume_unavailable"
)

type Event struct {
	Kind            EventKind
	NativeSessionID string
	Message         string
	Raw             json.RawMessage
}

type CLI interface {
	Name() string
	Metadata() Metadata
	BuildStartCommand(LaunchSpec) (CommandSpec, error)
	BuildResumeCommand(ResumeSpec) (CommandSpec, error)
	BuildInjectInput(MessageInput) (RuntimeInput, error)
	ParseEvent([]byte) (Event, error)
	ResumeCompatible(previous, next SessionContext) bool
}

// Registry is an immutable view over the compile-time production adapter list.
// It intentionally exposes no registration method.
type Registry struct {
	entries map[string]CLI
	names   []string
}

var productionAdapters = []CLI{
	Claude{},
}

func Production() Registry {
	registry, err := newRegistry(productionAdapters)
	if err != nil {
		panic(err)
	}
	return registry
}

func newRegistry(entries []CLI) (Registry, error) {
	registry := Registry{entries: make(map[string]CLI, len(entries))}
	for _, entry := range entries {
		if entry == nil {
			return Registry{}, errors.New("adapter: nil registry entry")
		}
		name := strings.TrimSpace(entry.Name())
		if name == "" || name != entry.Name() {
			return Registry{}, errors.New("adapter: registry names must be non-empty and canonical")
		}
		if _, exists := registry.entries[name]; exists {
			return Registry{}, fmt.Errorf("adapter: duplicate registry name %q", name)
		}
		metadata := entry.Metadata()
		switch metadata.ExecutionModel {
		case ExecutionOneShot:
		case ExecutionLive:
			if !metadata.SupportsInject {
				return Registry{}, fmt.Errorf("adapter: live adapter %q must support injection", name)
			}
		default:
			return Registry{}, fmt.Errorf("adapter: %q has invalid execution model %q", name, metadata.ExecutionModel)
		}
		registry.entries[name] = entry
		registry.names = append(registry.names, name)
	}
	sort.Strings(registry.names)
	return registry, nil
}

func (r Registry) Lookup(name string) (CLI, bool) {
	entry, ok := r.entries[strings.TrimSpace(name)]
	return entry, ok
}

func (r Registry) Names() []string {
	return append([]string(nil), r.names...)
}
