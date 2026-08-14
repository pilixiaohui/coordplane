//go:build e2e

package e2e_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/tests/testsupport"
)

// Web surface: with web_addr configured the daemon serves the SPA at / and
// the operator API at /v1/* through the same handler, so the credential
// fence behaves exactly like the Unix socket surface.
func TestWebSurfaceServesSPAAndFencesOperatorAPI(t *testing.T) {
	coordplane := requireExecutable(t, "E2E_COORDPLANE_BIN")
	image := strings.TrimSpace(os.Getenv("E2E_RUNTIME_IMAGE"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoError(t, err)
	webAddr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	socket := filepath.Join(dataDir, "operator.sock")
	configPath := testsupport.WriteFile(t, filepath.Join(root, "coordplane.yaml"),
		testsupport.RuntimeConfigYAML(testsupport.RuntimeConfigFixture{DataDir: dataDir, OperatorSocket: socket, MaxParallelRuns: 1, CompletedWorkspace: "0", TerminalTaskRef: "24h", RunLog: "24h", DockerNetwork: "none", DefaultImage: image, Tail: "  run_timeout: 2m\n  shutdown_grace: 3s\nweb_addr: " + webAddr + "\n"}), 0o600)

	daemon := startDaemon(t, coordplane, configPath, socket)
	t.Cleanup(func() { _ = daemon.Stop() })
	waitForReady(t, ctx, coordplane, socket, "web e2e daemon startup")

	base := "http://" + webAddr
	page := httpGet(t, ctx, base+"/")
	if !strings.Contains(page, "CoordPlane") || !strings.Contains(page, "app.js") {
		t.Fatalf("SPA page missing markers: %.300s", page)
	}
	app := httpGet(t, ctx, base+"/app.js")
	if !strings.Contains(app, "/v1/") || !strings.Contains(app, "X-Coordplane-Credential") ||
		!strings.Contains(app, "/v1/adapters") || !strings.Contains(app, "data-agent-action") {
		t.Fatalf("app.js missing API surface: %.300s", app)
	}
	for _, forbidden := range []string{
		`onclick="editAgentForm('${`,
		`onclick="updateAgent('${`,
		`onclick="openAgent('${`,
		`onclick="agentAction('${`,
	} {
		if strings.Contains(app, forbidden) {
			t.Fatalf("app.js still interpolates an agent ID into inline JavaScript: %q", forbidden)
		}
	}
	// Fresh install keeps the bootstrap trust boundary: status without a
	// credential is reachable.
	if got := httpGet(t, ctx, base+"/v1/status"); !strings.Contains(got, "daemon_ready") {
		t.Fatalf("bootstrap status body: %.200s", got)
	}
	// Once a credential exists the web fence closes: no header -> SCOPE_DENIED,
	// correct header -> 200.
	const secret = "web-e2e-secret-0123456789"
	runJSON[core.Credential](t, ctx, coordplane, "credential", "add", "--socket", socket, "--participant", "participant-owner", "--secret", secret, "--request-id", "web-e2e-cred", "--output", "json")
	if got := httpGet(t, ctx, base+"/v1/status"); !strings.Contains(got, "SCOPE_DENIED") {
		t.Fatalf("fenced status body: %.200s", got)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/status", nil)
	requireNoError(t, err)
	req.Header.Set("X-Coordplane-Credential", secret)
	resp, err := http.DefaultClient.Do(req)
	requireNoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	requireNoError(t, err)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "daemon_ready") {
		t.Fatalf("status with credential = %d: %.200s", resp.StatusCode, raw)
	}
	t.Logf("PASS web surface addr=%s (SPA + credential fence)", webAddr)
}

func httpGet(t *testing.T, ctx context.Context, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	requireNoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	requireNoError(t, err)
	return string(raw)
}
