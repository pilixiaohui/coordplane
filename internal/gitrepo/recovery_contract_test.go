package gitrepo

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

func TestGT00ProjectIntentAndRealGitPhasesReconcileAfterRestart(t *testing.T) {
	phases := []struct {
		name      string
		phase     Phase
		beforeGit bool
	}{
		{name: "creating intent committed", beforeGit: true},
		{name: string(PhasePartialPrepared), phase: PhasePartialPrepared},
		{name: string(PhaseBareInitialized), phase: PhaseBareInitialized},
		{name: string(PhaseObjectsImported), phase: PhaseObjectsImported},
		{name: string(PhaseCanonicalWritten), phase: PhaseCanonicalWritten},
		{name: string(PhaseIntegrityVerified), phase: PhaseIntegrityVerified},
		{name: string(PhasePromoted), phase: PhasePromoted},
	}
	for _, test := range phases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			source, initial := newSourceRepository(t)
			root := t.TempDir()
			initializer, err := New(filepath.Join(root, "repos"))
			if err != nil {
				t.Fatal(err)
			}
			adapter := &recoveryProjectGit{initializer: initializer, interruptBeforeGit: test.beforeGit}
			if !test.beforeGit {
				fired := false
				initializer.phaseHook = func(_ context.Context, phase Phase, _ phaseFact) error {
					if phase == test.phase && !fired {
						fired = true
						return context.Canceled
					}
					return nil
				}
			}
			databasePath := filepath.Join(root, "coordplane.db")
			database, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			service, err := core.NewService(database, adapter, core.ServiceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			project, err := service.AddProject(ctx, core.AddProjectInput{
				Name: "recovery-" + test.name, Source: source, SourceRef: "refs/heads/main",
				RequestID: "request-" + test.name,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted project add error = %v, want context cancellation", err)
			}
			persisted, err := database.Project(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != core.ProjectCreating || persisted.PendingAction != "initialize" || persisted.InitialSHA != initial {
				t.Fatalf("pending project = %#v", persisted)
			}
			if persisted.CanonicalSHA != "" {
				t.Fatalf("unverified project cached canonical SHA %q before first active transition", persisted.CanonicalSHA)
			}
			advanced := commitFile(t, source, "advanced.txt", test.name+"\n", "move source after intent")
			if advanced == initial {
				t.Fatal("source branch did not advance")
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			initializer.phaseHook = nil
			adapter.interruptBeforeGit = false
			reopened, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restarted, err := core.NewService(reopened, adapter, core.ServiceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.ReconcileProjects(ctx); err != nil {
				t.Fatal(err)
			}
			persisted, err = reopened.Project(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != core.ProjectActive || persisted.InitialSHA != initial || persisted.CanonicalSHA != initial || persisted.PendingAction != "" {
				t.Fatalf("reconciled project = %#v, want original initial SHA %s", persisted, initial)
			}
			actual, err := initializer.Resolve(ctx, persisted.ControlRepoPath, persisted.CanonicalRef)
			if err != nil || actual != initial {
				t.Fatalf("actual canonical = %s err=%v, want %s", actual, err, initial)
			}
			events, err := reopened.Events(ctx, core.EventFilter{ProjectID: project.ID})
			if err != nil {
				t.Fatal(err)
			}
			var creatingOperation, activeOperation string
			for _, event := range events {
				switch event.Kind {
				case "project.creating":
					creatingOperation = event.OperationID
				case "project.active":
					activeOperation = event.OperationID
				}
			}
			if creatingOperation == "" || activeOperation != creatingOperation {
				t.Fatalf("project event operation pair = creating %q active %q", creatingOperation, activeOperation)
			}
		})
	}
}

func TestGT00RepairRetriesOnlyNeverActiveInitializationAtSavedSHA(t *testing.T) {
	ctx := context.Background()
	source, initial := newSourceRepository(t)
	root := t.TempDir()
	initializer, err := New(filepath.Join(root, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected initialization failure")
	fired := false
	initializer.phaseHook = func(_ context.Context, phase Phase, _ phaseFact) error {
		if phase == PhaseObjectsImported && !fired {
			fired = true
			return injected
		}
		return nil
	}
	adapter := &recoveryProjectGit{initializer: initializer}
	database, err := store.Open(ctx, filepath.Join(root, "coordplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := core.NewService(database, adapter, core.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.AddProject(ctx, core.AddProjectInput{
		Name: "repair-never-active", Source: source, SourceRef: "refs/heads/main", RequestID: "add-never-active",
	})
	if !core.IsCode(err, core.CodeGitInvariantViolation) {
		t.Fatalf("project add error = %v, want %s", err, core.CodeGitInvariantViolation)
	}
	persisted, err := database.Project(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != core.ProjectError || persisted.CanonicalSHA != "" || persisted.InitialSHA != initial {
		t.Fatalf("failed never-active project = %#v", persisted)
	}
	advanced := commitFile(t, source, "advanced.txt", "advanced\n", "move source after failed initialization")
	if advanced == initial {
		t.Fatal("source branch did not advance")
	}

	initializer.phaseHook = nil
	repaired, err := service.RepairProject(ctx, project.ID, "repair-never-active")
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != core.ProjectActive || repaired.InitialSHA != initial || repaired.CanonicalSHA != initial {
		t.Fatalf("repaired project = %#v, want saved initial SHA %s", repaired, initial)
	}
	actual, err := initializer.Resolve(ctx, repaired.ControlRepoPath, repaired.CanonicalRef)
	if err != nil || actual != initial {
		t.Fatalf("actual canonical = %q err=%v, want saved initial SHA %s", actual, err, initial)
	}
}

type recoveryProjectGit struct {
	initializer        *Initializer
	interruptBeforeGit bool
}

func (a *recoveryProjectGit) Preflight(ctx context.Context, source, sourceRef string) (core.ProjectGitFact, error) {
	fact, err := a.initializer.Preflight(ctx, source, sourceRef)
	if err != nil {
		return core.ProjectGitFact{}, err
	}
	return core.ProjectGitFact{
		Source: fact.SourcePath, SourceRef: fact.SourceRef, InitialSHA: fact.InitialSHA,
		CanonicalRef: fact.SourceRef, CanonicalSHA: fact.InitialSHA,
	}, nil
}

func (a *recoveryProjectGit) ControlPath(projectID string) string {
	paths, err := a.initializer.Paths(projectID, "control")
	if err != nil {
		return ""
	}
	return paths.Final
}

func (a *recoveryProjectGit) Initialize(ctx context.Context, intent core.ProjectGitIntent) (core.ProjectGitFact, error) {
	if a.interruptBeforeGit {
		return core.ProjectGitFact{}, context.Canceled
	}
	fact, err := a.initializer.Initialize(ctx, projectFromIntent(intent))
	return factFromGit(intent, fact), err
}

func (a *recoveryProjectGit) Verify(ctx context.Context, intent core.ProjectGitIntent) (core.ProjectGitFact, error) {
	fact, err := a.initializer.Verify(ctx, projectFromIntent(intent))
	return factFromGit(intent, fact), err
}

func (a *recoveryProjectGit) Exists(path string) bool { return a.initializer.Exists(path) }

func (a *recoveryProjectGit) Resolve(ctx context.Context, path, ref string) (string, error) {
	return a.initializer.Resolve(ctx, path, ref)
}

func projectFromIntent(intent core.ProjectGitIntent) Project {
	return Project{
		ID: intent.ProjectID, OperationID: intent.OperationID, SourcePath: intent.Source,
		SourceRef: intent.SourceRef, InitialSHA: intent.InitialSHA,
		ControlRepoPath: intent.ControlRepo, CanonicalRef: intent.CanonicalRef,
	}
}

func factFromGit(intent core.ProjectGitIntent, fact Fact) core.ProjectGitFact {
	return core.ProjectGitFact{
		Source: intent.Source, SourceRef: intent.SourceRef, InitialSHA: fact.InitialSHA,
		CanonicalRef: fact.CanonicalRef, CanonicalSHA: fact.CanonicalSHA,
	}
}

var _ core.ProjectGit = (*recoveryProjectGit)(nil)
