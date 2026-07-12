package gitrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// RegisteredPath identifies the repository paths currently authorized by a
// persisted Project row. PendingOperationID is set only for an initialize
// intent whose partial repository may be resumed.
type RegisteredPath struct {
	ProjectID          string
	PendingOperationID string
}

// QuarantineUnknown moves repository-root entries that have no persisted
// Project owner out of the active namespace. It never adopts or deletes them.
func (i *Initializer) QuarantineUnknown(registrations []RegisteredPath) ([]string, error) {
	if i == nil {
		return nil, errors.New("gitrepo: nil initializer")
	}
	if err := i.validateRoot(); err != nil {
		return nil, err
	}
	knownFinal := make(map[string]struct{}, len(registrations))
	knownProjects := make(map[string]struct{}, len(registrations))
	knownPartial := make(map[string]struct{}, len(registrations))
	for _, registration := range registrations {
		if err := validateID("project", registration.ProjectID); err != nil {
			return nil, err
		}
		knownFinal[registration.ProjectID+".git"] = struct{}{}
		knownProjects[registration.ProjectID] = struct{}{}
		if registration.PendingOperationID == "" {
			continue
		}
		paths, err := i.Paths(registration.ProjectID, registration.PendingOperationID)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(filepath.Join(i.root, ".partial"), paths.Partial)
		if err != nil {
			return nil, fmt.Errorf("gitrepo: identify registered partial repository: %w", err)
		}
		knownPartial[relative] = struct{}{}
	}

	quarantineRoot := filepath.Join(i.root, ".quarantine")
	if err := i.ensureDirectSubdirectories(quarantineRoot); err != nil {
		return nil, fmt.Errorf("gitrepo: prepare quarantine: %w", err)
	}
	var quarantined []string
	entries, err := os.ReadDir(i.root)
	if err != nil {
		return nil, fmt.Errorf("gitrepo: scan repository root: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		switch name {
		case ".partial":
			if err := validateDirectDirectory(filepath.Join(i.root, name), "partial repository root"); err != nil {
				return nil, err
			}
		case ".quarantine":
			if err := validateDirectDirectory(quarantineRoot, "repository quarantine"); err != nil {
				return nil, err
			}
		case ".empty-git-template":
			if err := requireEmptyDirectory(filepath.Join(i.root, name), "empty Git template"); err != nil {
				return nil, err
			}
		default:
			if _, ok := knownFinal[name]; ok {
				continue
			}
			if err := i.quarantinePath(filepath.Join(i.root, name), name, quarantineRoot); err != nil {
				return nil, err
			}
			quarantined = append(quarantined, name)
		}
	}

	partialRoot := filepath.Join(i.root, ".partial")
	partialProjects, err := os.ReadDir(partialRoot)
	if errors.Is(err, os.ErrNotExist) {
		sort.Strings(quarantined)
		return quarantined, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitrepo: scan partial repository root: %w", err)
	}
	for _, projectEntry := range partialProjects {
		projectName := projectEntry.Name()
		projectPath := filepath.Join(partialRoot, projectName)
		if _, ok := knownProjects[projectName]; !ok || projectEntry.Type()&os.ModeSymlink != 0 || !projectEntry.IsDir() {
			relative := filepath.Join(".partial", projectName)
			if err := i.quarantinePath(projectPath, relative, quarantineRoot); err != nil {
				return nil, err
			}
			quarantined = append(quarantined, filepath.ToSlash(relative))
			continue
		}
		if err := validateDirectDirectory(projectPath, "partial project directory"); err != nil {
			return nil, err
		}
		operations, err := os.ReadDir(projectPath)
		if err != nil {
			return nil, fmt.Errorf("gitrepo: scan partial project directory: %w", err)
		}
		for _, operationEntry := range operations {
			relative := filepath.Join(projectName, operationEntry.Name())
			if _, ok := knownPartial[relative]; ok {
				continue
			}
			path := filepath.Join(projectPath, operationEntry.Name())
			quarantineName := filepath.Join(".partial", relative)
			if err := i.quarantinePath(path, quarantineName, quarantineRoot); err != nil {
				return nil, err
			}
			quarantined = append(quarantined, filepath.ToSlash(quarantineName))
		}
	}
	sort.Strings(quarantined)
	return quarantined, nil
}

func (i *Initializer) quarantinePath(source, relative, quarantineRoot string) error {
	sum := sha256.Sum256([]byte(filepath.ToSlash(relative)))
	base := hex.EncodeToString(sum[:])
	var target string
	for suffix := 0; ; suffix++ {
		name := base
		if suffix > 0 {
			name = fmt.Sprintf("%s-%d", base, suffix)
		}
		target = filepath.Join(quarantineRoot, name)
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return fmt.Errorf("gitrepo: inspect quarantine target: %w", err)
		}
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("gitrepo: quarantine unowned repository path %q: %w", relative, err)
	}
	if err := syncDirectory(filepath.Dir(source)); err != nil {
		return fmt.Errorf("gitrepo: sync source after quarantine: %w", err)
	}
	if err := syncDirectory(quarantineRoot); err != nil {
		return fmt.Errorf("gitrepo: sync quarantine: %w", err)
	}
	return nil
}
