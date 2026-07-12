package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/transport"
)

type Daemon struct {
	components *components
	server     *transport.UnixServer

	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, configPath string) (*Daemon, error) {
	components, err := buildComponents(ctx, configPath)
	if err != nil {
		return nil, err
	}
	server, err := transport.NewUnixServer(
		components.config.DataDir,
		components.config.OperatorSocket,
		transport.NewOperatorHandler(components.service),
	)
	if err != nil {
		return nil, errors.Join(err, components.Close())
	}
	components.service.SetReady(true, "")
	return &Daemon{components: components, server: server}, nil
}

func Run(ctx context.Context, configPath string) error {
	daemon, err := Open(ctx, configPath)
	if err != nil {
		return err
	}
	defer daemon.Close()
	return daemon.Serve(ctx)
}

func (d *Daemon) Serve(ctx context.Context) error {
	if d == nil || d.server == nil || d.components == nil {
		return errors.New("coordplane daemon is not initialized")
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- d.server.Serve()
	}()
	select {
	case err := <-serveResult:
		d.components.service.SetReady(false, "operator socket stopped")
		return err
	case <-ctx.Done():
		d.components.service.SetReady(false, "daemon shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := d.server.Shutdown(shutdownCtx)
		serveErr := <-serveResult
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.Join(shutdownErr, serveErr)
		}
		return errors.Join(ctx.Err(), shutdownErr, serveErr)
	}
}

func (d *Daemon) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		var serverErr, componentErr error
		if d.server != nil {
			serverErr = d.server.Close()
		}
		if d.components != nil {
			componentErr = d.components.Close()
		}
		d.closeErr = errors.Join(serverErr, componentErr)
	})
	return d.closeErr
}

// NewRunServer composes the fixed Agent-facing socket from the daemon's
// canonical Core instance. Runtime owns serving and closing the per-Run
// listener because its lifetime is tied to the corresponding container.
func (d *Daemon) NewRunServer(controlRoot, socketPath string) (*transport.UnixServer, error) {
	if d == nil || d.components == nil || d.components.service == nil {
		return nil, errors.New("coordplane daemon is not initialized")
	}
	return transport.NewUnixServer(controlRoot, socketPath, transport.NewRunHandler(d.components.service))
}

// RecordRunTerminal is the narrow runtime-to-Core terminal fact boundary. It
// is deliberately absent from both operator and Agent transports.
func (d *Daemon) RecordRunTerminal(ctx context.Context, input core.TerminalRunInput) (core.Run, error) {
	if d == nil || d.components == nil || d.components.service == nil {
		return core.Run{}, errors.New("coordplane daemon is not initialized")
	}
	return d.components.service.RecordRunTerminal(ctx, input)
}

func (d *Daemon) String() string {
	if d == nil || d.components == nil {
		return "coordplane daemon"
	}
	return fmt.Sprintf("coordplane daemon (%s)", d.components.config.DataDir)
}
