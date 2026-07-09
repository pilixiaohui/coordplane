package codemap

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func readFileDigest(path string) ([]byte, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return raw, DigestBytes(raw), nil
}

func relPath(root, fullPath string) (string, error) {
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return "", err
	}
	return cleanRelPath(rel), nil
}

func walkRelativeFiles(root string, keep func(string) bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := relPath(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel != "." && skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if keep(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func skipDir(rel string) bool {
	base := filepath.Base(rel)
	if base == "vendor" {
		return true
	}
	if strings.HasPrefix(base, ".coordplane-release-health") {
		return true
	}
	switch base {
	case ".git", ".multica", ".agents", ".codex", ".agent_context":
		return true
	default:
		return false
	}
}

func cleanChangedFiles(files []string) []string {
	out := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		file = cleanRelPath(file)
		if file == "." || file == "" || seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}
