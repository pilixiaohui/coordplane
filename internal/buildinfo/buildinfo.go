package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

const SchemaVersion = "coordplane.build.v1"

var (
	component = "unknown"
	version   = "devel"
	commit    = "unknown"
	dirty     = "unknown"

	currentOnce sync.Once
	currentInfo Info
)

type Info struct {
	SchemaVersion    string `json:"schema_version"`
	Component        string `json:"component"`
	Version          string `json:"version"`
	Commit           string `json:"commit"`
	DirtyState       string `json:"dirty_state"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
	DigestStatus     string `json:"digest_status"`
}

func Current() Info {
	currentOnce.Do(func() {
		currentInfo = resolveMetadata()
		if digest, ok := executableSHA256(); ok {
			currentInfo.ExecutableSHA256 = digest
			currentInfo.DigestStatus = "available"
		} else {
			currentInfo.DigestStatus = "unavailable"
		}
	})
	return currentInfo
}

func resolveMetadata() Info {
	info := Info{
		SchemaVersion: SchemaVersion,
		Component:     nonEmpty(component, "unknown"),
		Version:       nonEmpty(version, "devel"),
		Commit:        nonEmpty(commit, "unknown"),
		DirtyState:    normalizeDirtyState(dirty),
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if info.Version == "devel" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "unknown" && strings.TrimSpace(setting.Value) != "" {
				info.Commit = strings.TrimSpace(setting.Value)
			}
		case "vcs.modified":
			if info.DirtyState == "unknown" {
				info.DirtyState = normalizeDirtyState(setting.Value)
			}
		}
	}
	return info
}

func executableSHA256() (string, bool) {
	path, err := os.Executable()
	if err != nil {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", false
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

func normalizeDirtyState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "dirty", "modified":
		return "dirty"
	case "0", "false", "clean", "unmodified":
		return "clean"
	default:
		return "unknown"
	}
}

func nonEmpty(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
