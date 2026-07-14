package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"coordplane/internal/adapter"
	"coordplane/internal/core"
	containerruntime "coordplane/internal/runtime"
)

func validateRuntimeContainerIdentity() error {
	return validateRuntimeContainerUID(os.Geteuid())
}

func validateRuntimeContainerUID(daemonUID int) error {
	if daemonUID == runtimeContainerUID {
		return errors.New("runtime container UID must differ from the daemon service UID")
	}
	return nil
}

func (c *runtimeController) validateAdoptedContainer(
	ctx context.Context,
	run core.Run,
	state containerruntime.LiveState,
) error {
	task, err := c.service.Task(ctx, run.TaskID)
	if err != nil {
		return err
	}
	controlPath := filepath.Join(c.controlRoot, run.ID)
	if err := validateAdoptionBootstrap(c.controlRoot, controlPath, run); err != nil {
		return err
	}
	entry, ok := c.adapters.Lookup(run.AdapterID)
	if !ok {
		return errors.New("runtime adapter is unavailable during container adoption")
	}
	launch := adapter.LaunchSpec{
		BootstrapPath: adapter.ContainerBootstrapPath, Conversation: task.Task.Kind == core.TaskConversation,
		ContainerHome: "/home/agent", ContainerWork: containerWorkingDirectory(task.Task.Kind),
	}
	var command adapter.CommandSpec
	switch run.LaunchMode {
	case "start":
		command, err = entry.BuildStartCommand(launch)
	case "resume":
		command, err = entry.BuildResumeCommand(adapter.ResumeSpec{
			LaunchSpec: launch, NativeSessionID: run.ResumeNativeSessionID,
		})
	default:
		err = errors.New("runtime launch mode is invalid during container adoption")
	}
	if err != nil {
		return err
	}
	spec, err := c.containerSpec(
		run,
		task.Task.Kind,
		command,
		controlPath,
	)
	if err != nil {
		return err
	}
	return containerruntime.ValidateAdoption(spec, state)
}

func validateAdoptionBootstrap(controlRoot, controlPath string, run core.Run) error {
	if err := validateRunControlIdentity(controlRoot, run); err != nil {
		return err
	}
	if err := validateOwnedRunControlFile(filepath.Join(controlPath, "bootstrap")); err != nil {
		return controlOwnershipError(errors.New("run control bootstrap is missing or invalid"))
	}
	return nil
}
