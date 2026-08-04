//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

// PR-D3: the full credential lifecycle against the real daemon and operator
// CLI. Once a credential exists, every operator request must present it in
// COORDPLANE_CREDENTIAL; rotation rejects the old secret on the next request;
// revocation rejects everything and survives a daemon restart.
func TestPRD3CredentialRotationAndRevocationLifecycle(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"), testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 1, CompletedWorkspace: "0", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image, Tail: "  run_timeout: 2m\n  shutdown_grace: 3s\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "prd3 daemon startup")

	const first = "prd3-first-secret-0123456789"
	const second = "prd3-second-secret-0123456789"
	runJSON[core.Credential](t, ctx, coordplane, "credential", "add", "--socket", socket, "--participant", "participant-owner", "--secret", first, "--request-id", "prd3-add", "--output", "json")
	credentialCLI(t, ctx, coordplane, socket, "", "project", "list").expect(false)
	credentialCLI(t, ctx, coordplane, socket, "wrong-secret-0123456789", "project", "list").expect(false)
	credentialCLI(t, ctx, coordplane, socket, first, "project", "list").expect(true)

	runJSON[core.Credential](t, ctx, coordplane, "credential", "rotate", "--socket", socket, "--participant", "participant-owner", "--secret", second, "--request-id", "prd3-rotate", "--output", "json")
	credentialCLI(t, ctx, coordplane, socket, first, "project", "list").expect(false)
	credentialCLI(t, ctx, coordplane, socket, second, "project", "list").expect(true)

	runJSON[core.Credential](t, ctx, coordplane, "credential", "revoke", "participant-owner", "--socket", socket, "--request-id", "prd3-revoke", "--output", "json")
	credentialCLI(t, ctx, coordplane, socket, second, "project", "list").expect(false)

	if err := daemon.Stop(); err != nil {
		t.Fatalf("stop daemon before restart: %v\n%s", err, readLog(daemon.logPath))
	}
	daemon = startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	// the revoked state persists across a daemon restart: even the correct
	// secret is rejected once the socket is back
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	credentialCLI(t, ctx, coordplane, socket, second, "project", "list").expect(false)
}

// credentialCLI runs one operator command with COORDPLANE_CREDENTIAL set.
func credentialCLI(t *testing.T, ctx context.Context, binary, socket, credential string, args ...string) *credentialCommand {
	t.Helper()
	t.Setenv("COORDPLANE_CREDENTIAL", credential)
	raw, err := commandOutput(ctx, "", binary, append(args, "--socket", socket)...)
	return &credentialCommand{t: t, raw: raw, err: err}
}

type credentialCommand struct {
	t   *testing.T
	raw []byte
	err error
}

func (c *credentialCommand) expect(ok bool) {
	c.t.Helper()
	if ok && c.err != nil {
		c.t.Fatalf("credential-gated command failed: %v\n%s", c.err, c.raw)
	}
	if !ok && c.err == nil {
		c.t.Fatalf("credential-gated command unexpectedly succeeded: %s", c.raw)
	}
}
