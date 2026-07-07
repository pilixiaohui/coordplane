//go:build releasehealth

package releasehealth_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCPAccept001DockerClaudeGate(t *testing.T) {
	if os.Getenv("COORDPLANE_RELEASE_HEALTH") != "1" {
		t.Skip("set COORDPLANE_RELEASE_HEALTH=1 to run the formal Docker/Claude release-health gate")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts", "release-health-cp-accept-001.sh"))
	cmd.Dir = root
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("formal release-health gate failed: %v\n%s", err, output)
	}
}
