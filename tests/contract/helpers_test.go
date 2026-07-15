package contract_test

import (
	"context"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func activateContractRuntimeRun(t *testing.T, ctx context.Context, service *core.Service, claim core.Claim, prefix string) core.Run {
	t.Helper()
	root := t.TempDir()
	workspace := ""
	if claim.Task.Kind != core.TaskConversation {
		workspace = filepath.Join(root, "workspace")
	}
	prepared, err := service.BeginRunLaunch(ctx, core.RunLaunchInput{
		RunID: claim.Run.ID, Generation: claim.Run.Generation, LaunchNonce: prefix + "-nonce",
		WorkspacePath: workspace, HomePath: filepath.Join(root, "home"),
		LogPath: filepath.Join(root, "run.log"), InstructionsHash: prefix + "-instructions",
		LaunchMode: "start", CleanupOperationID: prefix + "-cleanup", RequestID: prefix + "-prepare",
	})
	requireNoError(t, err)
	fact := core.RunRuntimeFactInput{
		RunID: prepared.ID, Generation: prepared.Generation, LaunchNonce: prepared.LaunchNonce,
		LaunchOperationID: prepared.LaunchOperationID, ContainerID: prefix + "-container",
		RequestID: prefix + "-created",
	}
	created, err := service.RecordContainerCreated(ctx, fact)
	requireNoError(t, err)
	fact.ContainerID = created.ContainerID
	fact.RequestID = prefix + "-start"
	started, err := service.RecordRunStartIssued(ctx, fact)
	requireNoError(t, err)
	fact.ContainerID = started.ContainerID
	fact.RequestID = prefix + "-active"
	active, err := service.ObserveProcessAndActivateRun(ctx, fact)
	requireNoError(t, err)
	return active
}

func interruptContractRuntimeRun(t *testing.T, ctx context.Context, service *core.Service, run core.Run, reason, requestID string) core.Run {
	t.Helper()
	result, err := service.RecordRuntimeRunTerminal(ctx, core.RunTerminalInput{
		RunID: run.ID, Generation: run.Generation, LaunchNonce: run.LaunchNonce,
		LaunchOperationID: run.LaunchOperationID, ContainerID: run.ContainerID,
		State: core.RunInterrupted, TerminalReason: reason, RequestID: requestID,
	})
	requireNoError(t, err)
	cleaned, err := service.RecordRunCleanup(ctx, core.RunCleanupInput{
		RunRuntimeFactInput: core.RunRuntimeFactInput{
			RunID: result.Run.ID, Generation: result.Run.Generation, LaunchNonce: result.Run.LaunchNonce,
			LaunchOperationID: result.Run.LaunchOperationID, ContainerID: result.Run.ContainerID,
			RequestID: requestID + "-cleanup",
		},
		CleanupOperationID: result.Run.CleanupOperationID,
		State:              core.CleanupRemoved,
	})
	requireNoError(t, err)
	return cleaned
}
