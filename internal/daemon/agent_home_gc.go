package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"coordplane/internal/core"
)

// agentHomeCleanupBoundary removes a directory tree that the host daemon
// user cannot delete directly: Agent home contents are written by a runtime
// container as uid 65532, and 0700 65532-owned directories block host
// os.RemoveAll. Implementations must fail closed — an error means the
// caller must not silently skip the deletion.
type agentHomeCleanupBoundary interface {
	RemoveTree(ctx context.Context, path string) error
}

type agentHomeGC struct {
	root     string
	boundary agentHomeCleanupBoundary
}

func newAgentHomeGC(root string, cleanupImage string) (*agentHomeGC, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("Agent home root must be absolute")
	}
	cleanupImage = strings.TrimSpace(cleanupImage)
	if cleanupImage == "" {
		return nil, fmt.Errorf("Agent home cleanup image is required")
	}
	return &agentHomeGC{root: root, boundary: dockerCleanupBoundary{image: cleanupImage}}, nil
}

func (g *agentHomeGC) State(ctx context.Context, agentID string) (core.AgentHomeStateFact, error) {
	if err := ctx.Err(); err != nil {
		return core.AgentHomeStateFact{}, err
	}
	path, err := g.path(agentID)
	if err != nil {
		return core.AgentHomeStateFact{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return core.AgentHomeStateFact{}, nil
	}
	if err != nil {
		return core.AgentHomeStateFact{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return core.AgentHomeStateFact{}, fmt.Errorf("Agent home is not a direct directory")
	}
	return core.AgentHomeStateFact{Exists: true}, nil
}

func (g *agentHomeGC) Delete(ctx context.Context, agentID string, authorize func() (bool, error)) (bool, error) {
	state, err := g.State(ctx, agentID)
	if err != nil || !state.Exists {
		return err == nil, err
	}
	allowed, err := authorize()
	if err != nil || !allowed {
		return allowed, err
	}
	path, err := g.path(agentID)
	if err != nil {
		return false, err
	}
	// The host daemon user deletes what it may; content it cannot remove
	// (written by a runtime container as uid 65532) is deleted through the
	// trusted Docker boundary. This escalation keeps hosts without a usable
	// boundary on today's semantics and never reports success while a
	// deletion the host cannot perform was skipped.
	if err := os.RemoveAll(path); err == nil {
		return true, nil
	}
	if g.boundary == nil {
		return false, errors.New("Agent home cleanup boundary is not configured")
	}
	if err := g.boundary.RemoveTree(ctx, path); err != nil {
		return false, err
	}
	return true, nil
}

func (g *agentHomeGC) path(agentID string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || agentID == "." || agentID == ".." || filepath.Base(agentID) != agentID || strings.ContainsAny(agentID, `/\\`) {
		return "", fmt.Errorf("invalid Agent home identity")
	}
	return filepath.Join(g.root, agentID), nil
}

var _ core.AgentHomeGC = (*agentHomeGC)(nil)

// dockerCleanupBoundary deletes a host directory tree from inside a trusted
// container running as root, mirroring the harness boundary the e2e suite
// uses for live Agent home cleanup (registerLiveHomeCleanup). The container
// sees the bind mount even when the host daemon user has no permission on
// the tree, so 65532-owned content written by a runtime container can be
// removed without relaxing host permissions. A failed boundary run is an
// error, never a silent skip.
type dockerCleanupBoundary struct {
	image string
}

func (b dockerCleanupBoundary) RemoveTree(ctx context.Context, path string) error {
	if err := b.run(ctx, path, "find /cleanup -mindepth 1 -delete"); err != nil {
		return err
	}
	// The bind mount point itself stays behind; remove the (now empty) root
	// directory from the host, or through the boundary again when the root
	// itself is not host-removable (e.g. 65532-owned 0700).
	if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return b.run(ctx, filepath.Dir(path), "rm -rf /cleanup/"+filepath.Base(path))
}

func (b dockerCleanupBoundary) run(ctx context.Context, mountSource, command string) error {
	cleanup := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "0:0",
		"-v", mountSource+":/cleanup", "--entrypoint", "sh", b.image, "-c", command)
	raw, err := cleanup.CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete Agent home through trusted Docker boundary: %v: %s", err, boundedOutput(raw))
	}
	return nil
}

func boundedOutput(raw []byte) string {
	output := strings.TrimSpace(string(raw))
	if len(output) > 2048 {
		return output[:2048] + "...[truncated]"
	}
	return output
}
