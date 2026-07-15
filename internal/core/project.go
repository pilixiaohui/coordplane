package core

import (
	"context"
	"errors"
	"strings"
)

func (s *Service) AddProject(ctx context.Context, input AddProjectInput) (Project, error) {
	name, err := requireText("name", input.Name)
	if err != nil {
		return Project{}, err
	}
	source, err := requireText("source", input.Source)
	if err != nil {
		return Project{}, err
	}
	sourceRef, err := requireText("source_ref", input.SourceRef)
	if err != nil {
		return Project{}, err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return Project{}, err
	}
	inputHash, err := inputFingerprint(struct {
		Name, Source, SourceRef, IntegrationAgentID string
	}{name, source, sourceRef, strings.TrimSpace(input.IntegrationAgentID)})
	if err != nil {
		return Project{}, err
	}
	dedupe := requestDedupe{"boss", "project.add", requestID, inputHash}
	if existing, ok, err := s.dedupedProject(ctx, "project.add", requestID, inputHash); err != nil || ok {
		if err == nil && existing.Status == ProjectError {
			err = replayProjectGitFailure(existing.LastError)
		}
		return existing, err
	}

	fact, err := s.projectGit.Preflight(ctx, source, sourceRef)
	if err != nil {
		return Project{}, WrapError(CodeGitInvariantViolation, "project source preflight failed", false, err)
	}
	projectID, err := s.requiredID("prj")
	if err != nil {
		return Project{}, err
	}
	operationID, err := s.requiredID("op")
	if err != nil {
		return Project{}, err
	}
	now := s.nowText()
	project := Project{
		ID: projectID, Name: name, Source: fact.Source, SourceRef: fact.SourceRef,
		InitialSHA: fact.InitialSHA, ControlRepoPath: s.projectGit.ControlPath(projectID),
		CanonicalRef:       fact.CanonicalRef,
		IntegrationAgentID: strings.TrimSpace(input.IntegrationAgentID), Status: ProjectCreating,
		PendingAction: "initialize", PendingActionID: operationID, PendingStartedAt: now,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	created := false
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if replay, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			project, err = tx.Project(replay.ID)
			return err
		}
		if _, err := tx.ProjectByName(name); err == nil {
			return NewError(CodeInvalidArgument, "project name already exists", false)
		} else if !IsCode(err, CodeNotFound) {
			return err
		}
		if project.IntegrationAgentID != "" {
			agent, err := tx.Agent(project.IntegrationAgentID)
			if err != nil {
				return err
			}
			if agent.Status != AgentActive {
				return Conflict(CodeInvalidState, "integration agent is not active", string(agent.Status), agent.Version)
			}
		}
		if err := tx.InsertProject(project); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(project.ID, "project", project.ID, "project.creating", "boss", "", "", requestID, operationID, eventPayload(map[string]any{"initial_sha": project.InitialSHA}), now)); err != nil {
			return err
		}
		if err := dedupe.record(tx, project.ID, "", now); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil || !created {
		return project, err
	}

	gitFact, gitErr := s.projectGit.Initialize(ctx, projectIntent(project))
	if gitErr != nil && (errors.Is(gitErr, context.Canceled) || errors.Is(gitErr, context.DeadlineExceeded)) {
		return project, gitErr
	}
	return s.finishProjectAction(ctx, project.ID, operationID, requestID, gitFact, gitErr)
}

func (s *Service) RepairProject(ctx context.Context, projectID, requestID string) (Project, error) {
	projectID = strings.TrimSpace(projectID)
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Project{}, err
	}
	inputHash, err := inputFingerprint(struct{ ProjectID string }{projectID})
	if err != nil {
		return Project{}, err
	}
	dedupe := requestDedupe{"boss", "project.repair", requestID, inputHash}
	if existing, ok, err := s.dedupedProject(ctx, "project.repair", requestID, inputHash); err != nil || ok {
		if err == nil && existing.Status == ProjectError {
			err = replayProjectGitFailure(existing.LastError)
		}
		return existing, err
	}
	project, err := s.repository.Project(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.Status != ProjectError {
		return Project{}, Conflict(CodeInvalidState, "only an error project can be repaired", string(project.Status), project.Version)
	}
	operationID, err := s.requiredID("op")
	if err != nil {
		return Project{}, err
	}
	// A non-empty cache proves this project was active before. Never rebuild its
	// missing code truth from the registration SHA.
	action := "verify"
	if project.CanonicalSHA == "" && !s.projectGit.Exists(project.ControlRepoPath) {
		action = "initialize"
	}
	now := s.nowText()
	expectedVersion := project.Version
	project.Status = ProjectCreating
	project.PendingAction = action
	project.PendingActionID = operationID
	project.PendingStartedAt = now
	project.LastError = ""
	project.Version++
	project.UpdatedAt = now
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if err := tx.UpdateProject(project, expectedVersion, ProjectError); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(event(project.ID, "project", project.ID, "project.creating", "boss", "", "", requestID, operationID, eventPayload(map[string]any{"action": action}), now)); err != nil {
			return err
		}
		return dedupe.record(tx, project.ID, "", now)
	})
	if err != nil {
		return Project{}, err
	}
	var gitFact ProjectGitFact
	if action == "verify" {
		gitFact, err = s.projectGit.Verify(ctx, projectIntent(project))
	} else {
		gitFact, err = s.projectGit.Initialize(ctx, projectIntent(project))
	}
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return project, err
	}
	return s.finishProjectAction(ctx, project.ID, operationID, requestID, gitFact, err)
}

func (s *Service) ReconcileProjects(ctx context.Context) error {
	creating, err := s.repository.ProjectsByStatus(ctx, ProjectCreating)
	if err != nil {
		return err
	}
	for _, project := range creating {
		var fact ProjectGitFact
		if project.PendingAction == "verify" {
			fact, err = s.projectGit.Verify(ctx, projectIntent(project))
		} else {
			fact, err = s.projectGit.Initialize(ctx, projectIntent(project))
		}
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return err
		}
		if _, finishErr := s.finishProjectAction(ctx, project.ID, project.PendingActionID, "", fact, err); finishErr != nil && !IsCode(finishErr, CodeGitInvariantViolation) {
			return finishErr
		}
	}
	active, err := s.repository.ProjectsByStatus(ctx, ProjectActive)
	if err != nil {
		return err
	}
	for _, project := range active {
		fact, verifyErr := s.projectGit.Verify(ctx, projectIntent(project))
		if finishErr := s.finishActiveVerification(ctx, project, fact, verifyErr); finishErr != nil {
			return finishErr
		}
	}
	return nil
}

func (s *Service) ArchiveProject(ctx context.Context, projectID, requestID string) (Project, error) {
	requestID, err := s.requestID(requestID)
	if err != nil {
		return Project{}, err
	}
	var project Project
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		project, err = tx.Project(strings.TrimSpace(projectID))
		if err != nil {
			return err
		}
		if project.PendingAction != "" {
			return Conflict(CodeActionInProgress, "project action is in progress", string(project.Status), project.Version)
		}
		if err := ValidateProjectTransition(project.Status, ProjectArchived); err != nil {
			return Conflict(CodeInvalidState, "project cannot be archived", string(project.Status), project.Version)
		}
		blockers, err := tx.ProjectBlockers(project.ID)
		if err != nil {
			return err
		}
		if blockers.LiveRuns+blockers.OpenTasks+blockers.PendingActions+blockers.UnresolvedAgentMessages > 0 {
			return NewError(CodeInvalidState, "project has live runs, open tasks, pending actions, or unresolved Agent messages", false)
		}
		now := s.nowText()
		expectedVersion, expectedStatus := project.Version, project.Status
		project.Status = ProjectArchived
		project.Version++
		project.UpdatedAt = now
		if err := tx.UpdateProject(project, expectedVersion, expectedStatus); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(project.ID, "project", project.ID, "project.archived", "boss", "", "", requestID, "", "{}", now))
		return err
	})
	return project, err
}

func (s *Service) dedupedProject(ctx context.Context, operation, requestID, inputHash string) (Project, bool, error) {
	var project Project
	found := false
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		raw, ok, err := tx.Dedupe("boss", operation, requestID)
		if err != nil || !ok {
			return err
		}
		result, err := decodeDedupe(raw, inputHash)
		if err != nil {
			return err
		}
		project, err = tx.Project(result.ID)
		found = err == nil
		return err
	})
	return project, found, err
}

func (s *Service) finishProjectAction(ctx context.Context, projectID, operationID, requestID string, fact ProjectGitFact, actionErr error) (Project, error) {
	var project Project
	err := s.repository.Transact(ctx, func(tx Transaction) error {
		var err error
		project, err = tx.Project(projectID)
		if err != nil {
			return err
		}
		if project.Status != ProjectCreating || project.PendingActionID != operationID {
			return Conflict(CodeVersionConflict, "project action fence changed", string(project.Status), project.Version)
		}
		now := s.nowText()
		expectedVersion := project.Version
		project.PendingAction = ""
		project.PendingActionID = ""
		project.PendingStartedAt = ""
		project.UpdatedAt = now
		project.Version++
		kind := "project.active"
		payload := "{}"
		if actionErr != nil {
			project.Status = ProjectError
			project.LastError = newProjectGitFailure(actionErr).Error()
			kind = "project.error"
			payload = eventPayload(map[string]any{"error": project.LastError})
		} else {
			project.Status = ProjectActive
			project.CanonicalSHA = fact.CanonicalSHA
			project.LastError = ""
			payload = eventPayload(map[string]any{"canonical_sha": fact.CanonicalSHA})
		}
		if err := tx.UpdateProject(project, expectedVersion, ProjectCreating); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(project.ID, "project", project.ID, kind, "daemon", "", "", requestID, operationID, payload, now))
		return err
	})
	if err != nil {
		return Project{}, err
	}
	if actionErr != nil {
		return project, newProjectGitFailure(actionErr)
	}
	return project, nil
}

func (s *Service) finishActiveVerification(ctx context.Context, project Project, fact ProjectGitFact, verifyErr error) error {
	if verifyErr == nil && project.CanonicalSHA == fact.CanonicalSHA {
		return nil
	}
	return s.repository.Transact(ctx, func(tx Transaction) error {
		current, err := tx.Project(project.ID)
		if err != nil {
			return err
		}
		if current.Status != ProjectActive || current.Version != project.Version {
			return Conflict(CodeVersionConflict, "project changed during verification", string(current.Status), current.Version)
		}
		now := s.nowText()
		current.Version++
		current.UpdatedAt = now
		kind := "project.active"
		payload := "{}"
		if verifyErr != nil {
			current.Status = ProjectError
			current.LastError = newProjectGitFailure(verifyErr).Error()
			kind = "project.error"
			payload = eventPayload(map[string]any{"error": current.LastError})
		} else {
			current.CanonicalSHA = fact.CanonicalSHA
			payload = eventPayload(map[string]any{"canonical_sha": fact.CanonicalSHA, "drift": true})
		}
		if err := tx.UpdateProject(current, project.Version, ProjectActive); err != nil {
			return err
		}
		_, err = tx.AppendEvent(event(current.ID, "project", current.ID, kind, "daemon", "", "", "", "", payload, now))
		return err
	})
}

func newProjectGitFailure(cause error) *Error {
	message := "project Git action failed"
	if cause != nil && strings.TrimSpace(cause.Error()) != "" {
		message += ": " + strings.TrimSpace(cause.Error())
	}
	message = boundedDurableText(message, MaximumOutcomeTextBytes)
	return NewError(CodeGitInvariantViolation, message, false)
}

func replayProjectGitFailure(lastError string) *Error {
	prefix := string(CodeGitInvariantViolation) + ": "
	message := strings.TrimPrefix(strings.TrimSpace(lastError), prefix)
	if message == "" {
		message = "project Git action failed"
	}
	return NewError(CodeGitInvariantViolation, message, false)
}

func projectIntent(project Project) ProjectGitIntent {
	return ProjectGitIntent{
		ProjectID: project.ID, Source: project.Source, SourceRef: project.SourceRef,
		InitialSHA: project.InitialSHA, ControlRepo: project.ControlRepoPath,
		CanonicalRef: project.CanonicalRef, OperationID: project.PendingActionID,
		ExpectedStatus: project.Status,
	}
}
