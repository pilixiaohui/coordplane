package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound    = errors.New("runtime: container not found")
	ErrUnavailable = errors.New("runtime: Docker unavailable")
	ErrOwnership   = errors.New("runtime: container ownership mismatch")
	ErrExited      = errors.New("runtime: container already exited")
	ErrUnsupported = errors.New("runtime: operation is unsupported")
)

const (
	LabelManaged     = "coordplane.managed"
	LabelContract    = "coordplane.runtime_contract"
	LabelProjectID   = "coordplane.project_id"
	LabelTaskID      = "coordplane.task_id"
	LabelAgentID     = "coordplane.agent_id"
	LabelRunID       = "coordplane.run_id"
	LabelGeneration  = "coordplane.generation"
	LabelLaunchNonce = "coordplane.launch_nonce"
)

type RuntimeRef struct {
	ContainerID   string
	ContainerName string
	ProjectID     string
	TaskID        string
	AgentID       string
	RunID         string
	Generation    int64
	LaunchNonce   string
}

type CommandSpec struct {
	Executable string
	Args       []string
	Env        map[string]string
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type ResourceLimits struct {
	PIDs        int64
	MemoryBytes int64
	NanoCPUs    int64
	TmpfsBytes  int64
}

type ContainerSpec struct {
	Ref              RuntimeRef
	Image            string
	Command          CommandSpec
	SensitiveEnvKeys []string
	WorkingDir       string
	User             string
	GroupAdd         []string
	Network          string
	Mounts           []Mount
	ReadOnlyRoot     bool
	Limits           ResourceLimits
}

type ContainerStatus string

const (
	StatusCreated ContainerStatus = "created"
	StatusRunning ContainerStatus = "running"
	StatusExited  ContainerStatus = "exited"
)

type MountFact struct {
	Type        string
	Source      string
	Destination string
	ReadWrite   bool
	Propagation string
}

// EnvironmentFact records only the name and a one-way value digest. Docker
// inspect exposes environment values, including provider credentials; runtime
// facts must never make those plaintext values available to callers or logs.
type EnvironmentFact struct {
	Name        string
	ValueDigest string
}

type LiveState struct {
	Ref            RuntimeRef
	Image          string
	Entrypoint     []string
	CommandArgs    []string
	Environment    []EnvironmentFact
	WorkingDir     string
	User           string
	GroupAdd       []string
	Network        string
	Status         ContainerStatus
	Running        bool
	PID            int
	ExitCode       *int
	AutoRemove     bool
	RestartPolicy  string
	Privileged     bool
	ReadonlyRootfs bool
	CapAdd         []string
	CapDrop        []string
	SecurityOpt    []string
	PublishedPorts int
	PIDsLimit      int64
	MemoryBytes    int64
	NanoCPUs       int64
	Init           bool
	Tmpfs          map[string]string
	Mounts         []MountFact
}

type ExitFact struct {
	Ref      RuntimeRef
	ExitCode int
}

type StopResult struct {
	AlreadyStopped bool
}

type RemoveResult struct {
	AlreadyAbsent bool
}

type InjectResult struct {
	Accepted bool
}

type Executor interface {
	Ping(context.Context) error
	Create(context.Context, ContainerSpec) (RuntimeRef, error)
	Attach(context.Context, RuntimeRef) (RuntimeRef, error)
	Start(context.Context, RuntimeRef) (RuntimeRef, error)
	Inject(context.Context, RuntimeRef, []byte) (InjectResult, error)
	Inspect(context.Context, RuntimeRef) (LiveState, error)
	Wait(context.Context, RuntimeRef) (ExitFact, error)
	Logs(context.Context, RuntimeRef, bool) (io.ReadCloser, error)
	Stop(context.Context, RuntimeRef, time.Duration) (StopResult, error)
	Remove(context.Context, RuntimeRef) (RemoveResult, error)
	Managed(context.Context) ([]LiveState, error)
}
