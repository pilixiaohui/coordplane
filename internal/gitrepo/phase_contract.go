//go:build contract

package gitrepo

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const (
	contractPhaseEnv = "COORDPLANE_CONTRACT_GIT_PHASE"
	contractReadyEnv = "COORDPLANE_CONTRACT_GIT_PHASE_READY"
)

func init() {
	contractPhaseHook = waitAtContractPhase
}

func waitAtContractPhase(ctx context.Context, phase Phase, _ phaseFact) error {
	want := strings.TrimSpace(os.Getenv(contractPhaseEnv))
	if want == "" || want != string(phase) {
		return nil
	}
	readyPath := strings.TrimSpace(os.Getenv(contractReadyEnv))
	if readyPath == "" {
		return fmt.Errorf("gitrepo: %s is required for contract phase %s", contractReadyEnv, phase)
	}
	if err := os.WriteFile(readyPath, []byte(phase+"\n"), 0o600); err != nil {
		return fmt.Errorf("gitrepo: publish contract phase %s: %w", phase, err)
	}
	<-ctx.Done()
	return ctx.Err()
}
