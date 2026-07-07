package releasehealth

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type DenylistScanResult struct {
	Passed       bool     `json:"passed"`
	FilesScanned int      `json:"files_scanned"`
	Markers      int      `json:"markers"`
	Violations   []string `json:"violations,omitempty"`
}

func ScanFilesForDenylist(paths []string, markers []string) (DenylistScanResult, error) {
	result := DenylistScanResult{Markers: len(markers)}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return result, fmt.Errorf("scan artifact %s: %w", path, err)
		}
		result.FilesScanned++
		for _, marker := range markers {
			if marker == "" {
				continue
			}
			if bytes.Contains(raw, []byte(marker)) {
				result.Violations = append(result.Violations, filepath.Base(path)+":"+marker)
			}
		}
	}
	result.Passed = len(result.Violations) == 0
	return result, nil
}

func artifactPaths(dir string, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}
