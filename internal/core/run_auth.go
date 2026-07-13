package core

import "context"

// AuthorizeRunScope verifies that a bearer token belongs to the Run exposed by
// one per-Run listener. Starting Runs are intentionally accepted here so the
// requested operation can return RUN_STARTING from the normal Core path.
func (s *Service) AuthorizeRunScope(ctx context.Context, token string, expected RunScope) error {
	if expected.ProjectID == "" || expected.AgentID == "" || expected.TaskID == "" ||
		expected.RunID == "" || expected.Generation <= 0 {
		return NewError(CodeInvalidArgument, "complete expected run scope is required", false)
	}
	return s.repository.Transact(ctx, func(tx Transaction) error {
		run, err := scopedRun(tx, token)
		if err != nil {
			return err
		}
		actual := RunScope{
			ProjectID:  run.ProjectID,
			AgentID:    run.AgentID,
			TaskID:     run.TaskID,
			RunID:      run.ID,
			Generation: run.Generation,
		}
		if actual != expected {
			return NewError(CodeScopeDenied, "run token does not belong to this control socket", false)
		}
		task, err := tx.Task(run.TaskID)
		if err != nil {
			return err
		}
		if run.TokenRevokedAt != "" || IsRunTerminal(run.State) ||
			task.ProjectID != run.ProjectID || task.CurrentRunID != run.ID ||
			task.Generation != run.Generation || task.AssigneeAgentID != run.AgentID {
			return Conflict(CodeStaleRun, "run token is stale", string(run.State), run.Version)
		}
		return nil
	})
}
