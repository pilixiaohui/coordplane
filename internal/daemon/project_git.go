package daemon

import (
	"context"
	"fmt"

	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
)

type projectGitAdapter struct {
	initializer *gitrepo.Initializer
	workspaces  *gitrepo.WorkspaceManager
}

func (a projectGitAdapter) Preflight(ctx context.Context, source, sourceRef string) (core.ProjectGitFact, error) {
	fact, err := a.initializer.Preflight(ctx, source, sourceRef)
	if err != nil {
		return core.ProjectGitFact{}, err
	}
	return core.ProjectGitFact{
		Source: fact.SourcePath, SourceRef: fact.SourceRef, InitialSHA: fact.InitialSHA,
		CanonicalRef: fact.SourceRef, CanonicalSHA: fact.InitialSHA,
	}, nil
}

func (a projectGitAdapter) ControlPath(projectID string) string {
	paths, err := a.initializer.Paths(projectID, "control")
	if err != nil {
		return ""
	}
	return paths.Final
}

func (a projectGitAdapter) Initialize(ctx context.Context, intent core.ProjectGitIntent) (core.ProjectGitFact, error) {
	fact, err := a.initializer.Initialize(ctx, gitProject(intent))
	return coreGitFact(intent, fact), err
}

func (a projectGitAdapter) Verify(ctx context.Context, intent core.ProjectGitIntent) (core.ProjectGitFact, error) {
	fact, err := a.initializer.Verify(ctx, gitProject(intent))
	return coreGitFact(intent, fact), err
}

func (a projectGitAdapter) Exists(path string) bool {
	return a.initializer.Exists(path)
}

func (a projectGitAdapter) Resolve(ctx context.Context, path, ref string) (string, error) {
	return a.initializer.Resolve(ctx, path, ref)
}

func (a projectGitAdapter) Capture(ctx context.Context, intent core.GitCaptureIntent) (core.GitCaptureFact, error) {
	if a.workspaces == nil {
		return core.GitCaptureFact{}, fmt.Errorf("workspace manager is not configured")
	}
	wantPath, err := a.workspaces.Path(intent.ProjectID, intent.TaskID)
	if err != nil {
		return core.GitCaptureFact{}, err
	}
	if intent.WorkspacePath != wantPath {
		return core.GitCaptureFact{}, fmt.Errorf("workspace path does not match task identity")
	}
	workspace := gitrepo.WorkspaceSpec{
		ProjectID: intent.ProjectID, TaskID: intent.TaskID, BaseSHA: intent.BaseSHA,
	}
	if intent.Source != nil {
		workspace.Source = &gitrepo.WorkspaceSource{
			TaskID: intent.Source.TaskID, RunID: intent.Source.RunID,
			TaskRef: intent.Source.TaskRef, HeadSHA: intent.Source.HeadSHA,
		}
	}
	fact, err := a.workspaces.Capture(ctx, gitrepo.CaptureSpec{
		Workspace: workspace, RunID: intent.RunID, ExpectedHead: intent.ExpectedHead,
		ControlRepoPath: intent.ControlRepo, OperationID: intent.OperationID,
	})
	return core.GitCaptureFact{HeadSHA: fact.HeadSHA, TaskRef: fact.TaskRef}, err
}

func (a projectGitAdapter) CleanupCapture(ctx context.Context, intent core.GitCaptureIntent) error {
	if a.workspaces == nil {
		return fmt.Errorf("workspace manager is not configured")
	}
	return a.workspaces.CleanupCapture(ctx, intent.ProjectID, intent.TaskID, intent.RunID)
}

func (a projectGitAdapter) Advance(ctx context.Context, intent core.GitAdvanceIntent) (core.GitAdvanceFact, error) {
	fact, err := a.initializer.Advance(ctx, gitrepo.AdvanceSpec{
		ProjectID: intent.ProjectID, TaskID: intent.TaskID, RunID: intent.RunID,
		OperationID: intent.OperationID, ControlRepoPath: intent.ControlRepo,
		CanonicalRef: intent.CanonicalRef, TaskRef: intent.TaskRef,
		ExpectedOldSHA: intent.ExpectedOldSHA, TargetSHA: intent.TargetSHA,
	})
	return core.GitAdvanceFact{Outcome: core.GitAdvanceOutcome(fact.Outcome), ActualSHA: fact.ActualSHA}, err
}

func (a projectGitAdapter) ResolveTaskRef(ctx context.Context, intent core.GitTaskRefIntent) (string, error) {
	return a.initializer.ResolveTaskRef(ctx, intent.ProjectID, intent.ControlRepo, intent.TaskRef, intent.ExpectedSHA)
}

func (a projectGitAdapter) UseTaskRef(ctx context.Context, intent core.GitTaskRefIntent, use func(string) error) error {
	return a.initializer.UseTaskRef(ctx, intent.ProjectID, intent.ControlRepo, intent.TaskRef, intent.ExpectedSHA, use)
}

func (a projectGitAdapter) Checkout(ctx context.Context, intent core.GitCheckoutIntent) (core.GitCheckoutFact, error) {
	fact, err := a.initializer.Checkout(ctx, gitrepo.CheckoutSpec{
		ProjectID: intent.ProjectID, ControlRepoPath: intent.ControlRepo,
		TaskRef: intent.TaskRef, ExpectedHead: intent.ExpectedSHA,
		Destination: intent.Destination,
	})
	return core.GitCheckoutFact{Destination: fact.Destination, HeadSHA: fact.HeadSHA}, err
}

func (a projectGitAdapter) WorkspaceState(ctx context.Context, intent core.GitWorkspaceStateIntent) (core.GitWorkspaceStateFact, error) {
	spec := workspaceSpec(intent.GitDeleteWorkspaceIntent)
	fact, err := a.workspaces.State(ctx, spec, intent.ExpectedHead, intent.TaskVersion)
	return core.GitWorkspaceStateFact{
		Exists: fact.Exists, Fingerprint: fact.Fingerprint, HeadSHA: fact.HeadSHA, Clean: fact.Clean,
	}, err
}

func (a projectGitAdapter) DiscardWorkspace(ctx context.Context, intent core.GitDiscardWorkspaceIntent, authorize func() (bool, error)) (bool, error) {
	spec := workspaceSpec(intent.GitDeleteWorkspaceIntent)
	return a.workspaces.Discard(ctx, spec, intent.ExpectedHead, intent.TaskVersion, intent.ExpectedFingerprint, authorize)
}

func (a projectGitAdapter) TaskRefState(ctx context.Context, intent core.GitDeleteRefIntent) (core.GitTaskRefStateFact, error) {
	fact, err := a.initializer.TaskRefState(ctx, gitrepo.DeleteTaskRefSpec{
		ProjectID: intent.ProjectID, ControlRepoPath: intent.ControlRepo,
		CanonicalRef: intent.CanonicalRef, TaskRef: intent.TaskRef, ExpectedHead: intent.ExpectedSHA,
	})
	return core.GitTaskRefStateFact{Exists: fact.Exists, ActualSHA: fact.ActualSHA, Included: fact.Included}, err
}

func (a projectGitAdapter) DeleteTaskRefAndPrune(ctx context.Context, intent core.GitDeleteRefIntent, authorize func() (bool, error)) (bool, error) {
	return a.initializer.DeleteTaskRefAndPrune(ctx, gitrepo.DeleteTaskRefSpec{
		ProjectID: intent.ProjectID, ControlRepoPath: intent.ControlRepo,
		CanonicalRef: intent.CanonicalRef, TaskRef: intent.TaskRef,
		ExpectedHead: intent.ExpectedSHA, AllowDiscard: intent.AllowDiscard,
	}, authorize)
}

func (a projectGitAdapter) DeleteWorkspace(ctx context.Context, intent core.GitDeleteWorkspaceIntent, authorize func() (bool, error)) (bool, error) {
	if a.workspaces == nil {
		return false, fmt.Errorf("workspace manager is not configured")
	}
	spec := workspaceSpec(intent)
	return a.workspaces.Delete(ctx, spec, intent.ExpectedHead, authorize)
}

func workspaceSpec(intent core.GitDeleteWorkspaceIntent) gitrepo.WorkspaceSpec {
	spec := gitrepo.WorkspaceSpec{ProjectID: intent.ProjectID, TaskID: intent.TaskID, BaseSHA: intent.BaseSHA}
	if intent.Source != nil {
		spec.Source = &gitrepo.WorkspaceSource{
			TaskID: intent.Source.TaskID, RunID: intent.Source.RunID,
			TaskRef: intent.Source.TaskRef, HeadSHA: intent.Source.HeadSHA,
		}
	}
	return spec
}

func gitProject(intent core.ProjectGitIntent) gitrepo.Project {
	return gitrepo.Project{
		ID: intent.ProjectID, OperationID: intent.OperationID, SourcePath: intent.Source,
		SourceRef: intent.SourceRef, InitialSHA: intent.InitialSHA,
		ControlRepoPath: intent.ControlRepo, CanonicalRef: intent.CanonicalRef,
	}
}

func coreGitFact(intent core.ProjectGitIntent, fact gitrepo.Fact) core.ProjectGitFact {
	return core.ProjectGitFact{
		Source: intent.Source, SourceRef: intent.SourceRef, InitialSHA: fact.InitialSHA,
		CanonicalRef: fact.CanonicalRef, CanonicalSHA: fact.CanonicalSHA,
	}
}

var _ core.ProjectGit = projectGitAdapter{}
var _ core.TaskGit = projectGitAdapter{}
