package core

import (
	"context"
	"path/filepath"
	"strings"
)

func (s *Service) CheckoutTask(ctx context.Context, input TaskCheckoutInput) (GitCheckoutFact, error) {
	taskID, err := requireText("task_id", input.TaskID)
	if err != nil {
		return GitCheckoutFact{}, err
	}
	destination := strings.TrimSpace(input.Destination)
	if destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return GitCheckoutFact{}, NewError(CodeInvalidArgument, "destination must be canonical and absolute", false)
	}
	task, err := s.repository.Task(ctx, taskID)
	if err != nil {
		return GitCheckoutFact{}, err
	}
	if task.Status != TaskSubmitted && task.Status != TaskCompleted {
		return GitCheckoutFact{}, Conflict(CodeInvalidState, "task must be submitted or completed", string(task.Status), task.Version)
	}
	if task.TaskRef == "" || task.HeadSHA == "" {
		return GitCheckoutFact{}, NewError(CodeGitInvariantViolation, "task has no captured Git result", false)
	}
	project, err := s.repository.Project(ctx, task.ProjectID)
	if err != nil {
		return GitCheckoutFact{}, err
	}
	executor, ok := s.projectGit.(TaskGit)
	if !ok {
		return GitCheckoutFact{}, NewError(CodeGitInvariantViolation, "task Git executor is not configured", false)
	}
	return executor.Checkout(ctx, GitCheckoutIntent{
		GitTaskRefIntent: GitTaskRefIntent{
			ProjectID: task.ProjectID, ControlRepo: project.ControlRepoPath,
			TaskRef: task.TaskRef, ExpectedSHA: task.HeadSHA,
		},
		Destination: destination,
	})
}
