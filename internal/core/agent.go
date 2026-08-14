package core

import (
	"context"
	"strings"
)

func (s *Service) AddAgent(ctx context.Context, input AddAgentInput) (Agent, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityAgentManage, GlobalProjectID); err != nil {
		return Agent{}, err
	}
	requestedID, err := optionalTextWithin("id", input.ID, 256)
	if err != nil {
		return Agent{}, err
	}
	config, err := s.validateAgentConfig(AgentConfigInput{
		DisplayName:      input.DisplayName,
		AdapterID:        input.AdapterID,
		Image:            input.Image,
		InstructionsFile: input.InstructionsFile,
		InstructionsText: input.InstructionsText,
		Model:            input.Model,
		SubagentModel:    input.SubagentModel,
		BaseURL:          input.BaseURL,
		Effort:           input.Effort,
	})
	if err != nil {
		return Agent{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Agent{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ID, RequestID string
		Config        agentConfig
	}{requestedID, requestID, config})
	if err != nil {
		return Agent{}, err
	}
	dedupe := requestDedupe{"boss", "agent.add", requestID, inputHash}
	var agent Agent
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			agent, err = tx.Agent(replay.ID)
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
		agent = agentFromConfig(agentID, config, now)
		if err := tx.InsertAgent(agent); err != nil {
			return err
		}
		// Every CLI agent is also a participant in the unified framework, so
		// role bindings can target it like any other participant. The
		// participant row is an exact config-domain mirror.
		if err := tx.InsertParticipant(participantFromAgent(agent)); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event("", "agent", agent.ID, "agent.created", "boss", "", "", requestID, "", "{}", now)); err != nil {
			return err
		}
		return dedupe.record(tx, agent.ID, "", now)
	})
	return agent, err
}

// UpdateAgent replaces the complete Agent configuration in one transaction.
// It shares AddAgent's field validation and requires the caller to prove the
// current version; an old version conflicts before either mirror row is
// written, so a failed update has zero side effects.
func (s *Service) UpdateAgent(ctx context.Context, input UpdateAgentInput) (Agent, error) {
	if err := s.requireOperatorCapability(ctx, CapabilityAgentManage, GlobalProjectID); err != nil {
		return Agent{}, err
	}
	agentID, err := optionalTextWithin("id", input.ID, 256)
	if err != nil {
		return Agent{}, err
	}
	if agentID == "" {
		return Agent{}, NewError(CodeInvalidArgument, "id is required", false)
	}
	if input.Version < 1 {
		return Agent{}, NewError(CodeInvalidArgument, "version is required", false)
	}
	config, err := s.validateAgentConfig(input.AgentConfigInput)
	if err != nil {
		return Agent{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Agent{}, err
	}
	inputHash, err := inputFingerprint(struct {
		ID, RequestID string
		Version       int64
		Config        agentConfig
	}{agentID, requestID, input.Version, config})
	if err != nil {
		return Agent{}, err
	}
	dedupe := requestDedupe{"boss", "agent.update", requestID, inputHash}
	var agent Agent
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			agent, err = tx.Agent(replay.ID)
			return err
		}
		agent, err = tx.Agent(agentID)
		if err != nil {
			return err
		}
		if agent.Version != input.Version {
			return Conflict(CodeVersionConflict, "agent configuration changed", string(agent.Status), agent.Version)
		}
		changed := agentChangedFieldNames(agent, config)
		now := s.nowText()
		expectedAgentVersion := agent.Version
		applyAgentConfig(&agent, config, now)
		if err := tx.UpdateAgent(agent, expectedAgentVersion, agent.Status); err != nil {
			return err
		}
		participant, err := tx.Participant(agent.ID)
		if err != nil {
			return err
		}
		expectedParticipantVersion := participant.Version
		applyParticipantConfig(&participant, config, agent.Status, now)
		if err := tx.UpdateParticipant(participant, expectedParticipantVersion); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event("", "agent", agent.ID, "agent.updated", "boss", "", "", requestID, "", agentUpdatedEventPayload(agent.Version, changed), now)); err != nil {
			return err
		}
		return dedupe.record(tx, agent.ID, "", now)
	})
	if err != nil {
		return Agent{}, err
	}
	return agent, nil
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
		if err := mirrorAgentParticipantStatus(tx, agent.ID, string(agent.Status), now); err != nil {
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
		if err := mirrorAgentParticipantStatus(tx, agent.ID, string(agent.Status), now); err != nil {
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

func agentFromConfig(id string, config agentConfig, now string) Agent {
	return Agent{
		ID: id, DisplayName: config.DisplayName, AdapterID: config.AdapterID,
		Image: config.Image, InstructionsFile: config.InstructionsFile,
		InstructionsText: config.InstructionsText, Model: config.Model,
		SubagentModel: config.SubagentModel, BaseURL: config.BaseURL,
		Effort: config.Effort, Status: AgentActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

func participantFromAgent(agent Agent) Participant {
	return Participant{
		ID: agent.ID, Kind: ParticipantKindCLIAgent, DisplayName: agent.DisplayName,
		Status: string(agent.Status), AdapterID: agent.AdapterID, Image: agent.Image,
		InstructionsFile: agent.InstructionsFile, InstructionsText: agent.InstructionsText,
		Model: agent.Model, SubagentModel: agent.SubagentModel, BaseURL: agent.BaseURL,
		Effort: agent.Effort, Version: agent.Version, CreatedAt: agent.CreatedAt,
		UpdatedAt: agent.UpdatedAt,
	}
}

func applyAgentConfig(agent *Agent, config agentConfig, now string) {
	agent.DisplayName = config.DisplayName
	agent.AdapterID = config.AdapterID
	agent.Image = config.Image
	agent.InstructionsFile = config.InstructionsFile
	agent.InstructionsText = config.InstructionsText
	agent.Model = config.Model
	agent.SubagentModel = config.SubagentModel
	agent.BaseURL = config.BaseURL
	agent.Effort = config.Effort
	agent.Version++
	agent.UpdatedAt = now
}

func applyParticipantConfig(participant *Participant, config agentConfig, status AgentStatus, now string) {
	participant.DisplayName = config.DisplayName
	participant.Status = string(status)
	participant.AdapterID = config.AdapterID
	participant.Image = config.Image
	participant.InstructionsFile = config.InstructionsFile
	participant.InstructionsText = config.InstructionsText
	participant.Model = config.Model
	participant.SubagentModel = config.SubagentModel
	participant.BaseURL = config.BaseURL
	participant.Effort = config.Effort
	participant.Version++
	participant.UpdatedAt = now
}

func mirrorAgentParticipantStatus(tx Transaction, agentID, status, now string) error {
	participant, err := tx.Participant(agentID)
	if err != nil {
		return err
	}
	expectedVersion := participant.Version
	participant.Status = status
	participant.Version++
	participant.UpdatedAt = now
	return tx.UpdateParticipant(participant, expectedVersion)
}
