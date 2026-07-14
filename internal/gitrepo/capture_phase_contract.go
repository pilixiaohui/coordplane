//go:build contract

package gitrepo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	contractCapturePhaseEnv = "COORDPLANE_CONTRACT_CAPTURE_PHASE"
	contractCaptureReadyEnv = "COORDPLANE_CONTRACT_CAPTURE_PHASE_READY"
	contractTaskRefUseEnv   = "COORDPLANE_CONTRACT_TASK_REF_USE"
	contractTaskRefReadyEnv = "COORDPLANE_CONTRACT_TASK_REF_READY"
	contractTaskRefGoEnv    = "COORDPLANE_CONTRACT_TASK_REF_RELEASE"
)

func init() {
	contractCapturePhaseHook = waitAtCaptureContractPhase
	contractTaskRefUseHook = waitAtTaskRefUse
}

func waitAtTaskRefUse(ctx context.Context, _ string, ref string) error {
	if want := strings.TrimSpace(os.Getenv(contractTaskRefUseEnv)); want == "" || want != ref {
		return nil
	}
	ready, release := strings.TrimSpace(os.Getenv(contractTaskRefReadyEnv)), strings.TrimSpace(os.Getenv(contractTaskRefGoEnv))
	if ready == "" || release == "" {
		return fmt.Errorf("gitrepo: task-ref barrier paths are required")
	}
	if err := os.WriteFile(ready, []byte(ref+"\n"), 0o600); err != nil {
		return err
	}
	for {
		if _, err := os.Stat(release); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func waitAtCaptureContractPhase(ctx context.Context, phase capturePhase, _ CaptureSpec) error {
	if want := strings.TrimSpace(os.Getenv(contractCapturePhaseEnv)); want == "" || want != string(phase) {
		return nil
	}
	ready := strings.TrimSpace(os.Getenv(contractCaptureReadyEnv))
	if ready == "" {
		return fmt.Errorf("gitrepo: %s is required for capture phase %s", contractCaptureReadyEnv, phase)
	}
	if err := os.WriteFile(ready, []byte(phase+"\n"), 0o600); err != nil {
		return fmt.Errorf("gitrepo: publish capture phase %s: %w", phase, err)
	}
	<-ctx.Done()
	return ctx.Err()
}
