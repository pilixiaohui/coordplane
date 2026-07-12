package daemon

import (
	"context"

	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
)

type projectGitAdapter struct {
	initializer *gitrepo.Initializer
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
