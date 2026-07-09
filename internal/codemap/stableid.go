package codemap

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

func StableNodeID(kind NodeKind, relPath, qualifiedName string) string {
	return stableID("node", string(kind), cleanRelPath(relPath), strings.TrimSpace(qualifiedName))
}

func StableEdgeID(kind EdgeKind, fromID, toID string, evidence []Evidence) string {
	parts := []string{"edge", string(kind), fromID, toID}
	for _, ev := range evidence {
		parts = append(parts, cleanRelPath(ev.Path), ev.Collector)
	}
	return stableID(parts...)
}

func StableSnapshotID(schemaVersion, rootDigest, modulePath, inputDigest, mode string, changedFiles []string, projectID, resourceID string, collectors []CollectorVersion) string {
	parts := []string{
		"snapshot",
		schemaVersion,
		rootDigest,
		modulePath,
		inputDigest,
		mode,
		strings.Join(cleanChangedFiles(changedFiles), ","),
		strings.TrimSpace(projectID),
		strings.TrimSpace(resourceID),
	}
	for _, collector := range collectors {
		parts = append(parts, collector.Name, collector.Version)
	}
	return stableID(parts...)
}

func DigestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func MarkdownAnchor(heading string) string {
	heading = strings.TrimSpace(strings.ToLower(heading))
	var out strings.Builder
	dash := false
	for _, r := range heading {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
			dash = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '/':
			if !dash && out.Len() > 0 {
				out.WriteByte('-')
				dash = true
			}
		}
	}
	anchor := strings.Trim(out.String(), "-")
	if anchor == "" {
		return "section"
	}
	return anchor
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "codemap:" + parts[0] + ":" + hex.EncodeToString(sum[:])[:24]
}

func cleanRelPath(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || value == "." {
		return "."
	}
	value = path.Clean(value)
	if value == "." {
		return "."
	}
	return strings.TrimPrefix(value, "./")
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	var digits [20]byte
	i := len(digits)
	n := value
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if value == 0 {
		return "0"
	}
	return string(digits[i:])
}
