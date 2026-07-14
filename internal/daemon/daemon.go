package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	healthy, reason := components.runtime.Healthy()
	components.service.SetReady(healthy, reason)
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
	d.components.runtime.Start(ctx)
	go func() {
		serveResult <- d.server.Serve()
	}()
	select {
	case err := <-serveResult:
		d.components.service.SetReady(false, "operator socket stopped")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.components.runtime.shutdownGrace()+runtimeShutdownOverhead)
		defer cancel()
		return errors.Join(err, d.components.runtime.Shutdown(shutdownCtx))
	case <-ctx.Done():
		d.components.service.SetReady(false, "daemon shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.components.runtime.shutdownGrace()+runtimeShutdownOverhead)
		defer cancel()
		runtimeErr := d.components.runtime.Shutdown(shutdownCtx)
		shutdownErr := d.server.Shutdown(shutdownCtx)
		serveErr := <-serveResult
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.Join(runtimeErr, shutdownErr, serveErr)
		}
		return errors.Join(ctx.Err(), runtimeErr, shutdownErr, serveErr)
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

func (d *Daemon) String() string {
	if d == nil || d.components == nil {
		return "coordplane daemon"
	}
	return fmt.Sprintf("coordplane daemon (%s)", d.components.config.DataDir)
}
