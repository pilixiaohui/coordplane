// Package webserver serves the CoordPlane web frontend and the operator API
// surface on a loopback TCP listener. The operator handler is reused as-is,
// so the credential fence and capability gates behave exactly like the Unix
// socket surface; the static single-page frontend is served same-origin.
package webserver

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"syscall"
	"time"
)

// listenConfig reuses the listener address across restarts (TIME_WAIT
// connections from a previous run must not block the next bind).
var listenConfig = net.ListenConfig{
	Control: func(network, address string, connection syscall.RawConn) error {
		return connection.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})
	},
}

//go:embed static
var staticFiles embed.FS

// Handler composes the operator API surface (credential fence included) with
// the static single-page frontend.
func Handler(operator http.Handler) (http.Handler, error) {
	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", operator)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return mux, nil
}

// Serve runs the composed handler on addr until ctx is cancelled, then shuts
// down gracefully.
func Serve(ctx context.Context, addr string, handler http.Handler) error {
	listener, err := listenConfig.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(server.Shutdown(shutdownCtx), <-done)
	}
}
