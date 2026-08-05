package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const unixSocketPathBytes = 108

// ListenUnix opens a Unix socket inside an existing caller-owned data
// directory. The caller must hold the data-directory lock before calling it.
func ListenUnix(dataDir, socketPath string) (net.Listener, error) {
	return ListenUnixWithMode(dataDir, socketPath, 0o600)
}

// ListenUnixWithMode opens a Unix socket with an explicit final permission
// mode. The caller remains responsible for the socket directory's group.
func ListenUnixWithMode(dataDir, socketPath string, mode os.FileMode) (net.Listener, error) {
	if mode&^os.ModePerm != 0 || mode.Perm() == 0 {
		return nil, errors.New("transport: Unix socket mode must contain only permission bits")
	}
	_, socketPath, err := ownedSocketPath(dataDir, socketPath)
	if err != nil {
		return nil, err
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}
	listenPath, releasePath, err := usableUnixSocketPath(socketPath)
	if err != nil {
		return nil, err
	}
	defer releasePath()
	listener, err := net.Listen("unix", listenPath)
	if err != nil {
		return nil, fmt.Errorf("transport: listen on Unix socket: %w", err)
	}
	cleanup := func(cause error) (net.Listener, error) {
		closeErr := listener.Close()
		removeErr := os.Remove(socketPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return nil, errors.Join(cause, closeErr, removeErr)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		return cleanup(errors.New("transport: Unix listener has an unexpected implementation"))
	}
	unixListener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, mode.Perm()); err != nil {
		return cleanup(fmt.Errorf("transport: chmod Unix socket: %w", err))
	}
	identity, err := os.Lstat(socketPath)
	if err != nil {
		return cleanup(fmt.Errorf("transport: stat Unix socket: %w", err))
	}
	return &removingListener{Listener: listener, path: socketPath, identity: identity}, nil
}

// UnixServer owns an HTTP server and its removable Unix listener.
type UnixServer struct {
	server   *http.Server
	listener net.Listener
}

func NewUnixServer(dataDir, socketPath string, handler http.Handler) (*UnixServer, error) {
	return NewUnixServerWithMode(dataDir, socketPath, 0o600, handler)
}

// NewUnixServerWithMode preserves the same ownership and stale-socket checks
// as NewUnixServer while allowing the per-Run 0660 socket contract.
func NewUnixServerWithMode(dataDir, socketPath string, mode os.FileMode, handler http.Handler) (*UnixServer, error) {
	if handler == nil {
		return nil, errors.New("transport: Unix server handler is required")
	}
	listener, err := ListenUnixWithMode(dataDir, socketPath, mode)
	if err != nil {
		return nil, err
	}
	return &UnixServer{
		server:   &http.Server{Handler: handler},
		listener: listener,
	}, nil
}

// Serve blocks until the server is closed or serving fails.
func (s *UnixServer) Serve() error {
	if s == nil || s.server == nil || s.listener == nil {
		return errors.New("transport: Unix server is not initialized")
	}
	err := s.server.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *UnixServer) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *UnixServer) Close() error {
	if s == nil {
		return nil
	}
	var serverErr, listenerErr error
	if s.server != nil {
		serverErr = s.server.Close()
	}
	if s.listener != nil {
		listenerErr = s.listener.Close()
	}
	return errors.Join(serverErr, listenerErr)
}

type removingListener struct {
	net.Listener
	path     string
	identity os.FileInfo
	once     sync.Once
	err      error
}

func (l *removingListener) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		closeErr := l.Listener.Close()
		current, statErr := os.Lstat(l.path)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			statErr = nil
		case statErr != nil:
			statErr = fmt.Errorf("transport: stat Unix socket during close: %w", statErr)
		case !os.SameFile(l.identity, current):
			statErr = errors.New("transport: Unix socket path changed ownership before close")
		case current.Mode()&os.ModeSocket == 0:
			statErr = errors.New("transport: Unix socket path is no longer a socket")
		default:
			statErr = os.Remove(l.path)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				statErr = fmt.Errorf("transport: remove Unix socket: %w", statErr)
			}
		}
		l.err = errors.Join(closeErr, statErr)
	})
	return l.err
}

func ownedSocketPath(dataDir, socketPath string) (string, string, error) {
	if !filepath.IsAbs(dataDir) || !filepath.IsAbs(socketPath) {
		return "", "", errors.New("transport: data directory and Unix socket path must be absolute")
	}
	dataDir = filepath.Clean(dataDir)
	info, err := os.Stat(dataDir)
	if err != nil {
		return "", "", fmt.Errorf("transport: stat data directory: %w", err)
	}
	if !info.IsDir() {
		return "", "", errors.New("transport: data directory is not a directory")
	}
	if err := requireCurrentOwner(info, "data directory"); err != nil {
		return "", "", err
	}
	resolvedDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", "", fmt.Errorf("transport: resolve data directory: %w", err)
	}
	parent := filepath.Dir(filepath.Clean(socketPath))
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return "", "", fmt.Errorf("transport: stat Unix socket directory: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", "", errors.New("transport: Unix socket parent is not a directory")
	}
	if err := requireCurrentOwner(parentInfo, "Unix socket directory"); err != nil {
		return "", "", err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", fmt.Errorf("transport: resolve Unix socket directory: %w", err)
	}
	relative, err := filepath.Rel(resolvedDataDir, resolvedParent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("transport: Unix socket must be inside the data directory")
	}
	return resolvedDataDir, filepath.Join(resolvedParent, filepath.Base(socketPath)), nil
}

func requireCurrentOwner(info os.FileInfo, kind string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("transport: cannot verify %s ownership", kind)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("transport: %s is not owned by the current process user", kind)
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("transport: stat existing Unix socket: %w", err)
	case info.Mode()&os.ModeSocket == 0:
		return errors.New("transport: refusing to remove a non-socket path")
	}
	dialer := net.Dialer{Timeout: 100 * time.Millisecond}
	dialPath, releasePath, aliasErr := usableUnixSocketPath(path)
	if aliasErr != nil {
		return aliasErr
	}
	connection, dialErr := dialer.Dial("unix", dialPath)
	releasePath()
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("transport: Unix socket is already active")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("transport: cannot prove existing Unix socket is stale: %w", dialErr)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("transport: remove stale Unix socket: %w", err)
	}
	return nil
}

func usableUnixSocketPath(path string) (string, func(), error) {
	if len(path) < unixSocketPathBytes {
		return path, func() {}, nil
	}
	parent := filepath.Dir(path)
	fd, err := syscall.Open(parent, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", func() {}, errors.New("transport: open long Unix socket parent")
	}
	directory := os.NewFile(uintptr(fd), "unix-socket-parent")
	if directory == nil {
		_ = syscall.Close(fd)
		return "", func() {}, errors.New("transport: open long Unix socket parent")
	}
	release := func() { _ = directory.Close() }
	alias := filepath.Join("/proc/self/fd", fmt.Sprintf("%d", fd), filepath.Base(path))
	if len(alias) >= unixSocketPathBytes {
		release()
		return "", func() {}, errors.New("transport: Unix socket basename is too long")
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		release()
		return "", func() {}, errors.New("transport: long Unix socket paths require /proc/self/fd")
	}
	return alias, release, nil
}
