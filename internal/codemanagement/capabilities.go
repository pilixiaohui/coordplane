package codemanagement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"coordplane/internal/capability"
)

func RegisterCapabilities(registry *capability.Registry, service *Service) error {
	if registry == nil {
		return errors.New("controlled Git capabilities: registry is nil")
	}
	if service == nil {
		return errors.New("controlled Git capabilities: service is nil")
	}
	for _, spec := range []struct {
		def     capability.Definition
		handler capability.HandlerFunc
	}{
		{definition("workspace.prepare", capability.SideEffectWrite, "agent_repo", true), service.handleWorkspacePrepare},
		{definition("workspace.status", capability.SideEffectRead, "agent_workspace", false), service.handleWorkspaceStatus},
		{definition("workspace.sync", capability.SideEffectWrite, "agent_workspace", true), service.handleWorkspaceSync},
		{definition("git.status", capability.SideEffectRead, "agent_workspace", false), service.handleGitStatus},
		{definition("git.diff", capability.SideEffectRead, "agent_workspace", false), service.handleGitDiff},
		{definition("git.log", capability.SideEffectRead, "agent_workspace", false), service.handleGitLog},
		{definition("git.commit", capability.SideEffectWrite, "agent_workspace", true), service.handleGitCommit},
		{definition("changeset.submit", capability.SideEffectWrite, "agent_workspace", true), service.handleChangeSetSubmit},
		{definition("changeset.abandon", capability.SideEffectWrite, "agent_workspace", true), service.handleChangeSetAbandon},
		{definition("git.merge_preview", capability.SideEffectWrite, "agent_workspace", true), service.handleMergePreview},
		{definition("git.merge_apply", capability.SideEffectWrite, "agent_workspace", true), service.handleMergeApply},
		{definition("git.conflicts", capability.SideEffectRead, "agent_workspace", false), service.handleConflicts},
		{definition("git.resolve", capability.SideEffectWrite, "agent_workspace", true), service.handleResolveMerge},
		{definition("git.abort", capability.SideEffectWrite, "agent_workspace", true), service.handleAbortMerge},
		{definition("git.rollback", capability.SideEffectWrite, "agent_workspace", true), service.handleRollback},
		{definition("git.recover", capability.SideEffectWrite, "agent_repo", true), service.handleRecoverOperations},
	} {
		if err := registry.Register(spec.def, spec.handler); err != nil {
			return err
		}
	}
	return nil
}

func definition(name string, sideEffect capability.SideEffect, scope string, idempotency bool) capability.Definition {
	return capability.Definition{
		Name:           name,
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		RejectedSchema: json.RawMessage(`{"type":"object"}`),
		SideEffect:     sideEffect,
		RequiredScope:  scope,
		Idempotency:    idempotency,
		SkillRefs:      []string{"controlled-git"},
	}
}

func (s *Service) handleWorkspacePrepare(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input WorkspacePrepareInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("workspace.prepare", err)
	}
	input.AgentID = agentIDFromCall(call)
	if input.RuntimeID == "" {
		input.RuntimeID = call.Subject.RuntimeID
	}
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.WorkspacePrepare(ctx, input))
}

func (s *Service) handleWorkspaceStatus(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input WorkspaceStatusInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("workspace.status", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.WorkspaceStatus(ctx, input))
}

func (s *Service) handleWorkspaceSync(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input WorkspaceSyncInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("workspace.sync", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.WorkspaceSync(ctx, input))
}

func (s *Service) handleGitStatus(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input GitStatusInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.status", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.GitStatus(ctx, input))
}

func (s *Service) handleGitDiff(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input GitDiffInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.diff", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.GitDiff(ctx, input))
}

func (s *Service) handleGitLog(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input GitLogInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.log", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.GitLog(ctx, input))
}

func (s *Service) handleGitCommit(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input GitCommitInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.commit", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.GitCommit(ctx, input))
}

func (s *Service) handleChangeSetSubmit(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input SubmitChangeSetInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("changeset.submit", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.SubmitChangeSet(ctx, input))
}

func (s *Service) handleChangeSetAbandon(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input AbandonChangeSetInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("changeset.abandon", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.AbandonChangeSet(ctx, input))
}

func (s *Service) handleMergePreview(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input MergePreviewInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.merge_preview", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.MergePreview(ctx, input))
}

func (s *Service) handleMergeApply(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input MergeApplyInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.merge_apply", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.MergeApply(ctx, input))
}

func (s *Service) handleConflicts(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input ConflictListInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.conflicts", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.Conflicts(ctx, input))
}

func (s *Service) handleResolveMerge(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input ResolveMergeInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.resolve", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.ResolveMerge(ctx, input))
}

func (s *Service) handleAbortMerge(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input AbortMergeInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.abort", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.AbortMerge(ctx, input))
}

func (s *Service) handleRollback(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input RollbackInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("git.rollback", err)
	}
	input.AgentID = agentIDFromCall(call)
	if call.IdempotencyKey != "" {
		input.IdempotencyKey = call.IdempotencyKey
	}
	return responseToRaw(s.Rollback(ctx, input))
}

func (s *Service) handleRecoverOperations(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input RecoverOperationsInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("git.recover", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.RecoverOperations(ctx, input))
}

func decodeInput(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return json.Unmarshal(raw, target)
}

func decodeInputWithScope(call capability.Call, target any) error {
	merged := make(map[string]any)
	if len(call.Scope) > 0 {
		if err := json.Unmarshal(call.Scope, &merged); err != nil {
			return fmt.Errorf("decode scope: %w", err)
		}
	}
	if len(call.Input) > 0 {
		var input map[string]any
		if err := json.Unmarshal(call.Input, &input); err != nil {
			return err
		}
		for key, value := range input {
			merged[key] = value
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func agentIDFromCall(call capability.Call) string {
	if call.Subject.AgentID != "" {
		return call.Subject.AgentID
	}
	if call.Subject.Kind == "agent" {
		return call.Subject.ID
	}
	return call.Subject.ID
}

func invalidInput(capabilityName string, err error) capability.Response[json.RawMessage] {
	return capability.Rejected[json.RawMessage](
		"INVALID_CAPABILITY_INPUT",
		fmt.Sprintf("%s input is invalid: %v", capabilityName, err),
		capability.WithRepairHint("retry with a JSON object matching the capability input schema"),
		capability.WithAllowedNextActions(capabilityName),
		capability.WithRetryable(false),
	)
}

func responseToRaw[T any](response capability.Response[T]) capability.Response[json.RawMessage] {
	raw := capability.Response[json.RawMessage]{
		OK:                 response.OK,
		Status:             response.Status,
		ErrorCode:          response.ErrorCode,
		Message:            response.Message,
		RepairHint:         response.RepairHint,
		CanonicalIDs:       cloneStringMap(response.CanonicalIDs),
		Missing:            append([]capability.MissingRequirement(nil), response.Missing...),
		AllowedNextActions: append([]string(nil), response.AllowedNextActions...),
		Retryable:          response.Retryable,
	}
	if response.Data != nil {
		data, err := json.Marshal(response.Data)
		if err != nil {
			return capability.Error[json.RawMessage]("CAPABILITY_RESPONSE_ENCODE_FAILED", err.Error(), false)
		}
		rawData := json.RawMessage(data)
		raw.Data = &rawData
	}
	return raw
}
