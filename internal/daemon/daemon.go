package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"coordplane/internal/transport"
	"coordplane/internal/webserver"
)

type Daemon struct {
	components *components
	server     *transport.UnixServer
	webAddr    string
	webHandler http.Handler

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
	var webAddr string
	var webHandler http.Handler
	if webAddr = strings.TrimSpace(components.config.WebAddr); webAddr != "" {
		webHandler, err = webserver.Handler(transport.NewOperatorHandler(components.service))
		if err != nil {
			return nil, errors.Join(err, components.Close())
		}
	}
	healthy, reason := components.runtime.Healthy()
	components.service.SetReady(healthy, reason)
	return &Daemon{components: components, server: server, webAddr: webAddr, webHandler: webHandler}, nil
}

func Run(ctx context.Context, configPath string) error {
	daemon, err := Open(ctx, configPath)
	if err != nil {
		return err
	}
	defer daemon.Close()
	return daemon.Serve(ctx)
}

// webResultOrNil reads the web server result when a web server is running;
// without web_addr the channel is never written.
func webResultOrNil(webHandler http.Handler, webResult chan error) error {
	if webHandler == nil {
		return nil
	}
	return <-webResult
}

func (d *Daemon) Serve(ctx context.Context) error {
	if d == nil || d.server == nil || d.components == nil {
		return errors.New("coordplane daemon is not initialized")
	}
	webCtx, webCancel := context.WithCancel(ctx)
	defer webCancel()
	serveResult := make(chan error, 1)
	webResult := make(chan error, 1)
	d.components.runtime.Start(ctx)
	go func() {
		serveResult <- d.server.Serve()
	}()
	if d.webHandler != nil {
		go func() {
			webResult <- webserver.Serve(webCtx, d.webAddr, d.webHandler)
		}()
	}
	select {
	case err := <-serveResult:
		webCancel()
		d.components.service.SetReady(false, "operator socket stopped")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.components.runtime.shutdownGrace()+runtimeShutdownOverhead)
		defer cancel()
		return errors.Join(err, d.components.runtime.Shutdown(shutdownCtx), webResultOrNil(d.webHandler, webResult))
	case err := <-webResult:
		d.components.service.SetReady(false, "web server stopped")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.components.runtime.shutdownGrace()+runtimeShutdownOverhead)
		defer cancel()
		return errors.Join(err, d.components.runtime.Shutdown(shutdownCtx), d.server.Shutdown(shutdownCtx), <-serveResult)
	case <-ctx.Done():
		webCancel()
		d.components.service.SetReady(false, "daemon shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.components.runtime.shutdownGrace()+runtimeShutdownOverhead)
		defer cancel()
		runtimeErr := d.components.runtime.Shutdown(shutdownCtx)
		shutdownErr := d.server.Shutdown(shutdownCtx)
		serveErr := <-serveResult
		webErr := webResultOrNil(d.webHandler, webResult)
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.Join(runtimeErr, shutdownErr, serveErr, webErr)
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
