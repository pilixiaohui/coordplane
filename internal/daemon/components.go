package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"coordplane/internal/adapter"
	"coordplane/internal/config"
	"coordplane/internal/core"
	"coordplane/internal/gitrepo"
	containerruntime "coordplane/internal/runtime"
	"coordplane/internal/store"
)

type components struct {
	config  config.Config
	lock    *DataDirLock
	store   *store.Store
	service *core.Service
	runtime *runtimeController

	closeOnce sync.Once
	closeErr  error
}

func buildComponents(ctx context.Context, configPath string) (*components, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	lock, err := AcquireDataDirLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	result := &components{config: cfg, lock: lock}
	fail := func(cause error) (*components, error) {
		return nil, errors.Join(cause, result.Close())
	}
	if err := prepareDataDirectories(cfg); err != nil {
		return fail(err)
	}
	database, err := store.Open(ctx, filepath.Join(cfg.DataDir, "coordplane.db"))
	if err != nil {
		return fail(err)
	}
	result.store = database
	initializer, err := gitrepo.New(filepath.Join(cfg.DataDir, "repos"))
	if err != nil {
		return fail(err)
	}
	projects, err := database.ProjectsByStatus(ctx)
	if err != nil {
		return fail(fmt.Errorf("list registered projects: %w", err))
	}
	registered := make([]gitrepo.RegisteredPath, 0, len(projects))
	for _, project := range projects {
		operationID := ""
		if project.Status == core.ProjectCreating && project.PendingAction == "initialize" {
			operationID = project.PendingActionID
		}
		registered = append(registered, gitrepo.RegisteredPath{
			ProjectID: project.ID, PendingOperationID: operationID,
		})
	}
	quarantined, err := initializer.QuarantineUnknown(registered)
	if err != nil {
		return fail(fmt.Errorf("quarantine unowned repositories: %w", err))
	}
	if len(quarantined) > 0 {
		return fail(fmt.Errorf("quarantined unowned repository paths %q; inspect the quarantine and restart", quarantined))
	}
	executor, err := containerruntime.NewDockerExecutorFromEnvironment()
	if err != nil {
		return fail(err)
	}
	captureHelper, err := newDockerCaptureHelper(
		executor, cfg.Git, filepath.Join(cfg.DataDir, "handoff"), resolveGitCaptureHelperExecutable(),
	)
	if err != nil {
		return fail(err)
	}
	workspaceManager, err := gitrepo.NewWorkspaceManager(initializer, cfg.Runtime.WorkspaceRoot, captureHelper)
	if err != nil {
		return fail(err)
	}
	agentHomes, err := newAgentHomeGC(cfg.Runtime.AgentHomeRoot)
	if err != nil {
		return fail(err)
	}
	finalized, err := database.FinalizedCaptureTasks(ctx)
	if err != nil {
		return fail(fmt.Errorf("list finalized Git captures: %w", err))
	}
	for _, task := range finalized {
		if err := workspaceManager.CleanupCapture(ctx, task.ProjectID, task.ID, task.HeadRunID); err != nil {
			return fail(fmt.Errorf("clean finalized Git capture %s/%s: %w", task.ID, task.HeadRunID, err))
		}
	}
	pending, err := database.PendingGitTasks(ctx)
	if err != nil {
		return fail(fmt.Errorf("list pending Git tasks: %w", err))
	}
	validHandoffs := make([]gitrepo.CaptureHelperRequest, 0, len(pending))
	for _, task := range pending {
		if task.PendingAction != "capture" {
			continue
		}
		run, runErr := database.Run(ctx, task.PendingActionRunID)
		if runErr != nil {
			return fail(fmt.Errorf("load capture Run %s: %w", task.PendingActionRunID, runErr))
		}
		request := gitrepo.CaptureHelperRequest{
			ProjectID: task.ProjectID, TaskID: task.ID, RunID: run.ID,
			Workspace: run.WorkspacePath, ExpectedHead: task.PendingExpectedSHA, BaseSHA: task.BaseSHA,
		}
		if task.SourceTaskID != "" {
			request.SourceSHA = task.SourceHeadSHA
		}
		validHandoffs = append(validHandoffs, request)
	}
	if err := captureHelper.Recover(validHandoffs); err != nil {
		return fail(fmt.Errorf("recover Git capture handoffs: %w", err))
	}
	service, err := core.NewService(database, projectGitAdapter{initializer: initializer, workspaces: workspaceManager}, core.ServiceOptions{
		MaxParallelRuns: cfg.MaxParallelRuns, AdapterIDs: adapter.Production().Names(),
		CompletedWorkspaceRetention: cfg.Retention.CompletedWorkspace,
		TerminalTaskRefRetention:    cfg.Retention.TerminalTaskRef,
		AgentHomes:                  agentHomes,
	})
	if err != nil {
		return fail(err)
	}
	result.service = service
	service.SetReady(false, "startup reconciliation")
	if err := service.ReconcileProjects(ctx); err != nil {
		return fail(fmt.Errorf("reconcile projects: %w", err))
	}
	if err := validateRuntimeContainerIdentity(); err != nil {
		return fail(err)
	}
	result.runtime = newRuntimeController(
		cfg, service, executor, adapter.Production(), workspaceManager, resolveCoordlinkExecutable(),
	)
	service.SetRuntimeStatus(core.RuntimeStatus{
		WorkspaceQuotaEnabled: false,
		WorkspaceQuotaReason:  "not enabled: workspace is a host bind mount without an enforced quota",
		TmpfsLimitBytes:       runtimeTmpfsLimit,
	})
	if err := result.runtime.Reconcile(ctx); err != nil {
		return fail(fmt.Errorf("reconcile runtime: %w", err))
	}
	return result, nil
}

func (c *components) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.service != nil {
			c.service.SetReady(false, "daemon stopped")
		}
		var runtimeErr, databaseErr error
		if c.runtime != nil {
			runtimeErr = c.runtime.Close()
		}
		if c.store != nil {
			databaseErr = c.store.Close()
		}
		c.closeErr = errors.Join(runtimeErr, databaseErr, c.lock.Close())
	})
	return c.closeErr
}

func prepareDataDirectories(cfg config.Config) error {
	directories := []string{
		cfg.DataDir,
		filepath.Dir(cfg.OperatorSocket),
		filepath.Join(cfg.DataDir, "locks"),
		filepath.Join(cfg.DataDir, "repos"),
		cfg.Runtime.WorkspaceRoot,
		cfg.Runtime.AgentHomeRoot,
		cfg.Runtime.LogRoot,
		filepath.Join(cfg.DataDir, "run-control"),
		filepath.Join(cfg.DataDir, "handoff"),
	}
	seen := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		directory = filepath.Clean(directory)
		if _, exists := seen[directory]; exists {
			continue
		}
		seen[directory] = struct{}{}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("prepare data directory %s: %w", directory, err)
		}
		if err := validateDataDirectory(cfg.DataDir, directory); err != nil {
			return err
		}
	}
	probe, err := os.CreateTemp(cfg.DataDir, ".coordplane-write-probe-")
	if err != nil {
		return fmt.Errorf("data_dir is not writable: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close data_dir write probe: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove data_dir write probe: %w", err)
	}
	return nil
}

func validateDataDirectory(dataDir, directory string) error {
	if !filepath.IsAbs(dataDir) || !filepath.IsAbs(directory) {
		return fmt.Errorf("data directory paths must be absolute: %s", directory)
	}
	dataDir = filepath.Clean(dataDir)
	directory = filepath.Clean(directory)

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("data directory must be a direct directory, not a symlink: %s", directory)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve data directory %s: %w", directory, err)
	}
	if resolved != directory {
		return fmt.Errorf("data directory was substituted through a symlink: %s", directory)
	}
	relative, err := filepath.Rel(dataDir, resolved)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("data directory is outside data_dir: %s", directory)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify data directory ownership: %s", directory)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("data directory is not owned by the daemon user: %s", directory)
	}
	permissions := info.Mode().Perm()
	if permissions&0o700 != 0o700 {
		return fmt.Errorf("data directory owner must have rwx permissions: %s", directory)
	}
	if permissions&0o022 != 0 {
		return fmt.Errorf("data directory must not be group/other writable: %s", directory)
	}
	return nil
}
