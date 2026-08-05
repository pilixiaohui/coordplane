package core

import (
	"context"
	"strings"
)

func (s *Service) CreateChildTask(ctx context.Context, input CreateChildTaskInput) (Task, error) {
	request, err := s.normalizeWorkTask(input.AssigneeAgentID, input.AssigneeParticipantID, input.Title, input.Description,
		input.SourceTaskID, input.Priority, input.MaxRetries, input.AckMessageIDs, input.RequestID)
	if err != nil {
		return Task{}, err
	}
	request.token = input.Token
	hash, err := inputFingerprint(struct {
		AgentID, Title, Description, SourceTaskID, AckIDs string
		Priority, MaxRetries                              int
	}{request.agentID, request.title, request.description, request.sourceTaskID,
		strings.Join(request.ackIDs, "\x00"), request.priority, request.maxRetries})
	if err != nil {
		return Task{}, err
	}
	request.dedupe = requestDedupe{operation: "task.create_child", requestID: request.requestID, inputHash: hash}
	return s.createWorkTask(ctx, request)
}
