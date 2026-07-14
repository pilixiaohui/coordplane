package daemon

import (
	"context"

	"coordplane/internal/core"
)

type runtimeContractPhase string

const (
	runtimePhaseIntentBeforeCreate runtimeContractPhase = "intent_before_create"
	runtimePhaseContainerCreated   runtimeContractPhase = "container_created"
	runtimePhaseProcessObserved    runtimeContractPhase = "process_observed"
	runtimePhaseProcessExited      runtimeContractPhase = "process_exited"
	runtimePhaseTerminalPersisted  runtimeContractPhase = "terminal_persisted"
)

type runtimePhaseHook func(context.Context, runtimeContractPhase, core.Run) error

// runtimeContractPhaseHook is nil in production builds. The contract-tagged
// implementation installs a process-level crash observer without adding a
// product fault-control surface.
var runtimeContractPhaseHook runtimePhaseHook

func afterRuntimeContractPhase(ctx context.Context, phase runtimeContractPhase, run core.Run) error {
	if runtimeContractPhaseHook == nil {
		return nil
	}
	return runtimeContractPhaseHook(ctx, phase, run)
}
