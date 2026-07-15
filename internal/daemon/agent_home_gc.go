package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coordplane/internal/core"
)

type agentHomeGC struct {
	root string
}

func newAgentHomeGC(root string) (*agentHomeGC, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("Agent home root must be absolute")
	}
	return &agentHomeGC{root: root}, nil
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
	if err := os.RemoveAll(path); err != nil {
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
