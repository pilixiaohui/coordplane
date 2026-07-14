//go:build contract

package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"coordplane/internal/core"
)

const (
	contractRuntimePhaseEnv = "COORDPLANE_CONTRACT_RUNTIME_PHASE"
	contractRuntimeReadyEnv = "COORDPLANE_CONTRACT_RUNTIME_PHASE_READY"
)

func init() {
	runtimeContractPhaseHook = waitAtRuntimeContractPhase
}

func waitAtRuntimeContractPhase(ctx context.Context, phase runtimeContractPhase, _ core.Run) error {
	want := strings.TrimSpace(os.Getenv(contractRuntimePhaseEnv))
	if want == "" || want != string(phase) {
		return nil
	}
	readyPath := strings.TrimSpace(os.Getenv(contractRuntimeReadyEnv))
	if readyPath == "" {
		return fmt.Errorf("daemon: %s is required for contract phase %s", contractRuntimeReadyEnv, phase)
	}
	if err := os.WriteFile(readyPath, []byte(phase+"\n"), 0o600); err != nil {
		return fmt.Errorf("daemon: publish contract phase %s: %w", phase, err)
	}
	<-ctx.Done()
	return ctx.Err()
}
