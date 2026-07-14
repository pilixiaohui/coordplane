//go:build contract

package core

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const contractCaptureFinalizedReadyEnv = "COORDPLANE_CONTRACT_CAPTURE_FINALIZED_READY"

func init() {
	contractCaptureFinalizedHook = waitAfterCaptureFinalized
}

func waitAfterCaptureFinalized(ctx context.Context, _ GitCaptureIntent) error {
	ready := strings.TrimSpace(os.Getenv(contractCaptureFinalizedReadyEnv))
	if ready == "" {
		return nil
	}
	if err := os.WriteFile(ready, []byte("capture finalized\n"), 0o600); err != nil {
		return fmt.Errorf("publish finalized capture phase: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}
