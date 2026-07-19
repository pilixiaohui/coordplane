package buildinfo_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coordplane/internal/buildinfo"
	"coordplane/tests/testsupport"
)

func TestMakeBuildProducesTraceableBinariesAndValidManifest(t *testing.T) {
	repoRoot := testsupport.RepositoryRoot()
	outputDir := t.TempDir()
	const (
		wantVersion = "1.2.3-build-gate"
		wantCommit  = "build-gate-commit"
	)
	cmd := exec.Command("make", "build",
		"BUILD_DIR="+outputDir,
		"BUILD_VERSION="+wantVersion,
		"BUILD_COMMIT="+wantCommit,
		"BUILD_DIRTY=false",
	)
	cmd.Dir = repoRoot
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make build: %v\n%s", err, raw)
	}

	binDir := filepath.Join(outputDir, "bin")
	for _, component := range []string{"coordplane", "coordlink"} {
		binaryPath := filepath.Join(binDir, component)
		versionCmd := exec.Command(binaryPath, "version")
		raw, err := versionCmd.Output()
		if err != nil {
			t.Fatalf("%s version: %v", component, err)
		}
		var info buildinfo.Info
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode %s version: %v; output=%s", component, err, raw)
		}
		if info.SchemaVersion != buildinfo.SchemaVersion ||
			info.Component != component ||
			info.Version != wantVersion ||
			info.Commit != wantCommit ||
			info.DirtyState != "clean" ||
			info.DigestStatus != "available" {
			t.Fatalf("%s build info = %+v, want exact injected provenance", component, info)
		}
		binary, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read %s binary: %v", component, err)
		}
		sum := sha256.Sum256(binary)
		if got := hex.EncodeToString(sum[:]); info.ExecutableSHA256 != got {
			t.Fatalf("%s executable SHA-256 = %s, recomputed %s", component, info.ExecutableSHA256, got)
		}
	}

	manifestPath := filepath.Join(binDir, "build-manifest.sha256")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read build manifest: %v", err)
	}
	for _, component := range []string{"coordplane", "coordlink"} {
		if !strings.Contains(string(manifest), fmt.Sprintf("  %s\n", component)) {
			t.Fatalf("build manifest missing %s entry: %s", component, manifest)
		}
	}
	check := exec.Command("sha256sum", "--check", filepath.Base(manifestPath))
	check.Dir = binDir
	if raw, err := check.CombinedOutput(); err != nil {
		t.Fatalf("verify build manifest: %v\n%s", err, raw)
	}
}
