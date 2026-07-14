package core

import (
	"context"
	"strings"
)

func (s *Service) AddAgent(ctx context.Context, input AddAgentInput) (Agent, error) {
	requestedID, err := optionalTextWithin("id", input.ID, 256)
	if err != nil {
		return Agent{}, err
	}
	displayName, err := requireText("display_name", input.DisplayName)
	if err != nil {
		return Agent{}, err
	}
	adapterID, err := requireText("adapter_id", input.AdapterID)
	if err != nil {
		return Agent{}, err
	}
	if _, registered := s.adapters[adapterID]; !registered {
		return Agent{}, NewError(CodeInvalidArgument, "adapter_id is not registered", false)
	}
	image, err := requireText("image", input.Image)
	if err != nil {
		return Agent{}, err
	}
	instructions, err := requireText("instructions_file", input.InstructionsFile)
	if err != nil {
		return Agent{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Agent{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ID, DisplayName, AdapterID, Image, InstructionsFile string
	}{requestedID, displayName, adapterID, image, instructions})
	if err != nil {
		return Agent{}, err
	}
	var agent Agent
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if raw, ok, err := tx.Dedupe("boss", "agent.add", requestID); err != nil {
			return err
		} else if ok {
			result, err := decodeDedupe(raw, inputHash)
			if err != nil {
				return err
			}
			agent, err = tx.Agent(result.ID)
			return err
		}
		agentID := requestedID
		if agentID == "" {
			var err error
			agentID, err = s.requiredID("agt")
			if err != nil {
				return err
			}
		}
		if _, err := tx.Agent(agentID); err == nil {
			return NewError(CodeInvalidArgument, "agent ID already exists", false)
		} else if !IsCode(err, CodeNotFound) {
			return err
		}
		now := s.nowText()
		agent = Agent{
			ID: agentID, DisplayName: displayName, AdapterID: adapterID, Image: image,
			InstructionsFile: instructions, Status: AgentActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertAgent(agent); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event("", "agent", agent.ID, "agent.created", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
		raw, err := encodeDedupe(agent.ID, "", inputHash)
		if err != nil {
			return err
		}
		return tx.PutDedupe("boss", "agent.add", requestID, raw, now)
	})
	return agent, err
}

func (s *Service) SetAgentStatus(ctx context.Context, agentID string, status AgentStatus, requestID string) (Agent, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Agent{}, err
	}
	var agent Agent
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		agent, err = tx.Agent(strings.TrimSpace(agentID))
		if err != nil {
			return err
		}
		if agent.Status == status {
			return nil
		}
		if status == AgentArchived {
			return NewError(CodeInvalidArgument, "use the archive operation", false)
		}
		if err := ValidateAgentTransition(agent.Status, status); err != nil {
			return Conflict(CodeInvalidState, "agent status transition is not allowed", string(agent.Status), agent.Version)
		}
		now := s.nowText()
		expectedVersion, expectedStatus := agent.Version, agent.Status
		agent.Status = status
		agent.Version++
		agent.UpdatedAt = now
		if err := tx.UpdateAgent(agent, expectedVersion, expectedStatus); err != nil {
			return err
		}
		kind := "agent.paused"
		if status == AgentActive {
			kind = "agent.resumed"
		}
		_, err = tx.AppendEvent(event("", "agent", agent.ID, kind, "boss", "", "", requestID, "", "{}", now))
		return err
	})
	return agent, err
}

func (s *Service) ArchiveAgent(ctx context.Context, agentID, requestID string) (Agent, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Agent{}, err
	}
	var agent Agent
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		agent, err = tx.Agent(strings.TrimSpace(agentID))
		if err != nil {
			return err
		}
		if err := ValidateAgentTransition(agent.Status, AgentArchived); err != nil {
			return Conflict(CodeInvalidState, "agent cannot be archived", string(agent.Status), agent.Version)
		}
		blockers, err := tx.AgentBlockers(agent.ID)
		if err != nil {
			return err
		}
		if blockers.LiveRuns+blockers.OpenTasks+blockers.AcceptedIntegrationSource+blockers.UnresolvedAgentMessages > 0 {
			return NewError(CodeInvalidState, "agent has a live run, open task, unresolved message, or accepted integration responsibility", false)
		}
		now := s.nowText()
		expectedVersion, expectedStatus := agent.Version, agent.Status
		agent.Status = AgentArchived
		agent.Version++
		agent.UpdatedAt = now
		if err := tx.UpdateAgent(agent, expectedVersion, expectedStatus); err != nil {
			return err
		}
		projects, err := tx.ProjectsByIntegrationAgent(agent.ID)
		if err != nil {
			return err
		}
		for _, project := range projects {
			expectedVersion, expectedStatus := project.Version, project.Status
			project.IntegrationAgentID = ""
			project.Version++
			project.UpdatedAt = now
			if err := tx.UpdateProject(project, expectedVersion, expectedStatus); err != nil {
				return err
			}
			payload := eventPayload(map[string]any{"integration_agent_id": "", "version": project.Version})
			if _, err := tx.AppendEvent(event(project.ID, "project", project.ID, "project.updated", "boss", "", "", requestID, "", payload, now)); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(event("", "agent", agent.ID, "agent.archived", "boss", "", "", requestID, "", "{}", now))
		return err
	})
	return agent, err
}
