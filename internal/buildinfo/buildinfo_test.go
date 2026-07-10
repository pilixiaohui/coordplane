package buildinfo

import (
	"encoding/hex"
	"testing"
)

func TestCurrentReportsTraceableExecutable(t *testing.T) {
	info := Current()
	if info.SchemaVersion != "coordplane.build.v1" {
		t.Fatalf("schema version = %q, want coordplane.build.v1", info.SchemaVersion)
	}
	if info.Component == "" || info.Version == "" || info.Commit == "" {
		t.Fatalf("build identity = %+v, want non-empty component/version/commit", info)
	}
	switch info.DirtyState {
	case "clean", "dirty", "unknown":
	default:
		t.Fatalf("dirty state = %q, want clean, dirty, or unknown", info.DirtyState)
	}
	if info.DigestStatus != "available" {
		t.Fatalf("digest status = %q, want available", info.DigestStatus)
	}
	if len(info.ExecutableSHA256) != 64 {
		t.Fatalf("executable SHA-256 = %q, want 64 hex characters", info.ExecutableSHA256)
	}
	if _, err := hex.DecodeString(info.ExecutableSHA256); err != nil {
		t.Fatalf("executable SHA-256 = %q: %v", info.ExecutableSHA256, err)
	}
}
