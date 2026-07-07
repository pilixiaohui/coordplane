package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"coordplane/internal/capability"
)

const defaultLeaseSeconds = 3600

type leaseInput struct {
	LeaseID string `json:"lease_id"`
}

type mailboxInput struct {
	MailboxID string `json:"mailbox_id"`
}

type assignmentNextCallInput struct {
	LeaseSeconds int64 `json:"lease_seconds,omitempty"`
}

// RegisterCapabilities exposes the coordination state machine through the
// shared capability registry used by HTTP and coordlink adapters.
func RegisterCapabilities(registry *capability.Registry, service *Service) error {
	if registry == nil {
		return errors.New("coordination capabilities: registry is nil")
	}
	if service == nil {
		return errors.New("coordination capabilities: service is nil")
	}
	for _, spec := range []struct {
		def     capability.Definition
		handler capability.HandlerFunc
	}{
		{definition("assignment.next", capability.SideEffectWrite, "agent", true), service.handleAssignmentNext},
		{definition("assignment.watch", capability.SideEffectRead, "agent", false), service.handleAssignmentWatch},
		{definition("contract.add", capability.SideEffectWrite, "agent_or_lease", true), service.handleContractAdd},
		{definition("contract.current", capability.SideEffectRead, "agent_lease", false), service.handleContractCurrent},
		{definition("contract.context", capability.SideEffectRead, "agent_lease", false), service.handleContractContext},
		{definition("contract.wait", capability.SideEffectWrite, "agent_lease", true), service.handleContractWait},
		{definition("contract.complete", capability.SideEffectWrite, "agent_lease", true), service.handleContractComplete},
		{definition("message.send", capability.SideEffectWrite, "agent_lease", true), service.handleMessageSend},
		{definition("communication.read", capability.SideEffectRead, "agent", false), service.handleCommunicationRead},
		{definition("mailbox.list", capability.SideEffectRead, "agent", false), service.handleMailboxList},
		{definition("mailbox.get", capability.SideEffectRead, "agent", false), service.handleMailboxGet},
		{definition("mailbox.resolve", capability.SideEffectWrite, "agent", true), service.handleMailboxResolve},
		{definition("report.submit", capability.SideEffectWrite, "agent_lease", true), service.handleReportSubmit},
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
		SkillRefs:      skillRefsForCapability(name),
	}
}

func skillRefsForCapability(name string) []string {
	switch name {
	case "contract.add", "contract.wait":
		return []string{"contract-delegation"}
	case "mailbox.list", "mailbox.get", "communication.read", "message.send":
		return []string{"coordplane-service", "contract-delegation"}
	default:
		return []string{"coordplane-service"}
	}
}

func (s *Service) handleAssignmentNext(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input assignmentNextCallInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("assignment.next", err)
	}
	leaseFor := time.Duration(defaultLeaseSeconds) * time.Second
	if input.LeaseSeconds > 0 {
		leaseFor = time.Duration(input.LeaseSeconds) * time.Second
	}
	result, err := s.AssignmentNext(ctx, AssignmentNextInput{
		AgentID:  agentIDFromCall(call),
		LeaseFor: leaseFor,
	})
	if err != nil {
		return capability.Error[json.RawMessage]("ASSIGNMENT_NEXT_FAILED", err.Error(), false)
	}
	return acceptedJSON("assignment.next", result)
}

func (s *Service) handleAssignmentWatch(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	result, err := s.AssignmentWatch(ctx, agentIDFromCall(call))
	if err != nil {
		return capability.Error[json.RawMessage]("ASSIGNMENT_WATCH_FAILED", err.Error(), false)
	}
	return acceptedJSON("assignment.watch", result)
}

func (s *Service) handleContractAdd(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input AddContractInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("contract.add", err)
	}
	input.IssuerAgentID = agentIDFromCall(call)
	result, err := s.AddContract(ctx, input)
	if err != nil {
		var rejected addContractRejectedErr
		if errors.As(err, &rejected) {
			return responseToRaw(rejected.response)
		}
		return capability.Error[json.RawMessage]("CONTRACT_ADD_FAILED", err.Error(), false)
	}
	return acceptedJSON("contract.add", result)
}

func (s *Service) handleContractCurrent(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input leaseInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("contract.current", err)
	}
	result, err := s.CurrentContract(ctx, input.LeaseID, agentIDFromCall(call))
	if err != nil {
		return capability.Error[json.RawMessage]("CONTRACT_CURRENT_FAILED", err.Error(), false)
	}
	return acceptedJSON("contract.current", result)
}

func (s *Service) handleContractContext(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input leaseInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("contract.context", err)
	}
	result, err := s.ContractContext(ctx, input.LeaseID, agentIDFromCall(call))
	if err != nil {
		return capability.Error[json.RawMessage]("CONTRACT_CONTEXT_FAILED", err.Error(), false)
	}
	return acceptedJSON("contract.context", result)
}

func (s *Service) handleContractWait(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input WaitContractInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("contract.wait", err)
	}
	input.AgentID = agentIDFromCall(call)
	result, err := s.WaitContract(ctx, input)
	if err != nil {
		return capability.Error[json.RawMessage]("CONTRACT_WAIT_FAILED", err.Error(), false)
	}
	return acceptedJSON("contract.wait", result)
}

func (s *Service) handleContractComplete(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input CompleteContractInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("contract.complete", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.CompleteContract(ctx, input))
}

func (s *Service) handleMessageSend(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input SendMessageInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("message.send", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.SendMessage(ctx, input))
}

func (s *Service) handleCommunicationRead(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input CommunicationReadInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("communication.read", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.ReadCommunication(ctx, input))
}

func (s *Service) handleMailboxList(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	result, err := s.MailboxList(ctx, agentIDFromCall(call))
	if err != nil {
		return capability.Error[json.RawMessage]("MAILBOX_LIST_FAILED", err.Error(), false)
	}
	return acceptedJSON("mailbox.list", result)
}

func (s *Service) handleMailboxGet(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input mailboxInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("mailbox.get", err)
	}
	result, err := s.MailboxGet(ctx, agentIDFromCall(call), input.MailboxID)
	if err != nil {
		return capability.Error[json.RawMessage]("MAILBOX_GET_FAILED", err.Error(), false)
	}
	return acceptedJSON("mailbox.get", result)
}

func (s *Service) handleMailboxResolve(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input ResolveMailboxInput
	if err := decodeInput(call.Input, &input); err != nil {
		return invalidInput("mailbox.resolve", err)
	}
	input.AgentID = agentIDFromCall(call)
	return responseToRaw(s.MailboxResolve(ctx, input))
}

func (s *Service) handleReportSubmit(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	var input SubmitReportInput
	if err := decodeInputWithScope(call, &input); err != nil {
		return invalidInput("report.submit", err)
	}
	input.AgentID = agentIDFromCall(call)
	result, err := s.SubmitReport(ctx, input)
	if err != nil {
		return capability.Error[json.RawMessage]("REPORT_SUBMIT_FAILED", err.Error(), false)
	}
	return acceptedJSON("report.submit", result)
}

func decodeInput(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return nil
}

func decodeInputWithScope(call capability.Call, target any) error {
	var merged map[string]any
	if len(call.Scope) > 0 {
		if err := json.Unmarshal(call.Scope, &merged); err != nil {
			return fmt.Errorf("decode scope: %w", err)
		}
	}
	if merged == nil {
		merged = make(map[string]any)
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
	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(mergedBytes, target)
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

func acceptedJSON(capabilityName string, data any) capability.Response[json.RawMessage] {
	resp, err := capability.AcceptedJSON(data)
	if err != nil {
		return capability.Error[json.RawMessage](
			"CAPABILITY_RESPONSE_ENCODE_FAILED",
			fmt.Sprintf("%s response could not be encoded: %v", capabilityName, err),
			false,
		)
	}
	return resp
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

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
