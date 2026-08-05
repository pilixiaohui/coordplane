package gitrepo

import (
	"context"
	"errors"
	"fmt"
)

// Exists reports whether controlRepoPath is an existing deterministic final
// repository directory owned by this initializer's root. It rejects nested,
// escaped, and symlinked paths even when their targets exist.
func (i *Initializer) Exists(controlRepoPath string) bool {
	return i.validateFinalPath(controlRepoPath) == nil
}

// Resolve returns the actual commit at canonicalRef in a deterministic final
// control repository. It does not read or mutate the ownership marker or any
// ref other than through Git's read-only resolution commands.
func (i *Initializer) Resolve(ctx context.Context, controlRepoPath, canonicalRef string) (string, error) {
	if err := i.validateFinalPath(controlRepoPath); err != nil {
		return "", err
	}
	if err := i.validateBranchRef(ctx, canonicalRef); err != nil {
		return "", fmt.Errorf("gitrepo: invalid canonical ref: %w", err)
	}
	bare, err := i.isBare(ctx, controlRepoPath)
	if err != nil {
		return "", err
	}
	if !bare {
		return "", errors.New("gitrepo: control repository is not bare")
	}
	sha, exists, err := i.resolveRef(ctx, controlRepoPath, canonicalRef)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", errors.New("gitrepo: canonical ref is missing")
	}
	return sha, nil
}
