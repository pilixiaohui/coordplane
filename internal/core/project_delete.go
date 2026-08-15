package core

import (
	"context"
	"strings"
)

// DeleteProject permanently removes an archived project and all of its
// associated records (Tasks, Runs, Messages, Events, participant bindings) in
// a single transaction, then best-effort quarantines the project's on-disk
// control repository. The deleted project keeps exactly one audit Event
// ("project.deleted") so operators can still trace that it existed.
func (s *Service) DeleteProject(ctx context.Context, input ProjectDeleteInput) error {
	projectID, err := requireText("project_id", input.ProjectID)
	if err != nil {
		return err
	}
	input.Reason, err = optionalTextWithin("reason", input.Reason, MaximumOutcomeTextBytes)
	if err != nil {
		return err
	}
	requestID, err := s.requestID(input.RequestID)
	if err != nil {
		return err
	}
	inputHash, err := inputFingerprint(struct{ ProjectID, Reason string }{projectID, input.Reason})
	if err != nil {
		return err
	}
	dedupe := requestDedupe{"boss", "project.delete", requestID, inputHash}
	if err := s.requireOperatorCapability(ctx, CapabilityProjectDelete, projectID); err != nil {
		return err
	}
	err = s.repository.Transact(ctx, func(tx Transaction) error {
		if _, ok, err := dedupe.replay(tx); err != nil {
			return err
		} else if ok {
			// The project is already deleted; replaying the request is a success.
			return nil
		}
		project, err := tx.Project(projectID)
		if err != nil {
			return err
		}
		if project.PendingAction != "" {
			return Conflict(CodeActionInProgress, "project action is in progress", string(project.Status), project.Version)
		}
		if project.Status != ProjectArchived {
			return Conflict(CodeInvalidState, "only an archived project can be deleted", string(project.Status), project.Version)
		}
		blockers, err := tx.ProjectBlockers(project.ID)
		if err != nil {
			return err
		}
		if blockers.LiveRuns+blockers.OpenTasks+blockers.PendingActions+blockers.AcceptedIntegrationSource+blockers.UnresolvedAgentMessages > 0 {
			return NewError(CodeInvalidState, "project has live runs, open tasks, pending actions, accepted integration source, or unresolved Agent messages", false)
		}
		now := s.nowText()
		if err := tx.DeleteMessagesByProject(project.ID); err != nil {
			return err
		}
		runs, err := tx.DeleteRunsByProject(project.ID)
		if err != nil {
			return err
		}
		for _, run := range runs {
			if IsRunLive(run.State) {
				return Conflict(CodeActionInProgress, "project has a live Run", string(run.State), run.Version)
			}
		}
		if err := tx.DeleteTasksByProject(project.ID); err != nil {
			return err
		}
		if err := tx.DeleteEventsByProject(project.ID); err != nil {
			return err
		}
		if err := tx.DeleteProjectBindings(project.ID); err != nil {
			return err
		}
		payload := eventPayload(map[string]any{
			"reason": boundedDurableText(input.Reason, MaximumOutcomeTextBytes), "run_count": len(runs),
		})
		if _, err := tx.AppendEvent(event(project.ID, "project", project.ID, "project.deleted", "boss", "", "", requestID, "", payload, now)); err != nil {
			return err
		}
		if err := tx.DeleteProject(project.ID); err != nil {
			return err
		}
		return dedupe.record(tx, project.ID, "", now)
	})
	if err != nil {
		return err
	}
	s.disposeDeletedProjectResources(ctx, projectID)
	return nil
}

func (s *Service) disposeDeletedProjectResources(ctx context.Context, projectID string) {
	executor, ok := s.projectGit.(ProjectGitDispose)
	if !ok {
		return
	}
	_ = executor.Dispose(ctx, strings.TrimSpace(projectID))
}
