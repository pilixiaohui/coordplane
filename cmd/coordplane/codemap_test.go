package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodemapCLIIndexValidateCheckAndDetectsDrift(t *testing.T) {
	root := writeCLICodemapFixture(t)
	snapshotPath := filepath.Join(t.TempDir(), "latest.json")
	var stdout, stderr bytes.Buffer
	if err := runCodemap([]string{"index", "--root", root, "--project-id", "project-1", "--resource-id", "resource-1", "--out", snapshotPath}, &stdout, &stderr); err != nil {
		t.Fatalf("codemap index error = %v; stderr=%s", err, stderr.String())
	}
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"schema_version": "coordplane.codemap.snapshot.v1"`)) {
		t.Fatalf("snapshot = %s, want codemap schema", string(raw))
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"validate", "--snapshot", snapshotPath}, &stdout, &stderr); err != nil {
		t.Fatalf("codemap validate error = %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("validate stdout = %s, want ok true", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"check", "--root", root, "--snapshot", snapshotPath}, &stdout, &stderr); err != nil {
		t.Fatalf("codemap check should infer existing project/resource stamp: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("check stdout = %s, want ok true", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"check", "--root", root, "--project-id", "project-1", "--resource-id", "resource-1", "--snapshot", snapshotPath}, &stdout, &stderr); err != nil {
		t.Fatalf("codemap check with explicit project/resource stamp error = %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("check stdout = %s, want ok true", stdout.String())
	}

	appendFile(t, filepath.Join(root, "internal", "widget", "widget.go"), "\nfunc Drift() string { return \"changed\" }\n")
	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"check", "--root", root, "--snapshot", snapshotPath}, &stdout, &stderr); err == nil {
		t.Fatalf("codemap check succeeded after source drift; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestCodemapCLIStrictValidateAndCheckRejectPartialSnapshots(t *testing.T) {
	emptyRoot := t.TempDir()
	snapshotPath := filepath.Join(t.TempDir(), "partial.json")
	var stdout, stderr bytes.Buffer
	if err := runCodemap([]string{"index", "--root", emptyRoot, "--strict", "--out", snapshotPath}, &stdout, &stderr); err == nil {
		t.Fatalf("codemap index --strict accepted partial snapshot; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("strict index wrote a partial snapshot file; stat err=%v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"index", "--root", emptyRoot, "--out", snapshotPath}, &stdout, &stderr); err != nil {
		t.Fatalf("non-strict index should write diagnostic partial snapshot: %v; stderr=%s", err, stderr.String())
	}
	rawPartial, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read non-strict partial snapshot: %v", err)
	}
	for _, forbidden := range []string{
		emptyRoot,
		"/tmp/",
		"/home/",
		"github_pat_",
		"ghp_",
		"-----BEGIN PRIVATE KEY-----",
	} {
		if strings.Contains(string(rawPartial), forbidden) {
			t.Fatalf("non-strict partial snapshot leaked %q: %s", forbidden, string(rawPartial))
		}
	}
	if !strings.Contains(string(rawPartial), `"status": "partial"`) || !strings.Contains(string(rawPartial), `"CODMAP_GO_MODULE_UNAVAILABLE"`) {
		t.Fatalf("partial snapshot = %s, want diagnostic artifact without host path leakage", string(rawPartial))
	}
	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"validate", "--snapshot", snapshotPath}, &stdout, &stderr); err == nil {
		t.Fatalf("codemap validate accepted partial/error snapshot; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":false`) {
		t.Fatalf("validate stdout = %s, want ok false", stdout.String())
	}

	goodRoot := writeCLICodemapFixture(t)
	goodSnapshot := filepath.Join(t.TempDir(), "ready.json")
	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"index", "--root", goodRoot, "--out", goodSnapshot}, &stdout, &stderr); err != nil {
		t.Fatalf("write ready snapshot: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := runCodemap([]string{"check", "--root", emptyRoot, "--snapshot", goodSnapshot}, &stdout, &stderr); err == nil {
		t.Fatalf("codemap check accepted current partial snapshot; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestCodemapCLIValidateRejectsMalformedSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
	}{
		{name: "invalid-json", body: `{"schema_version":`},
		{name: "unknown-field", body: `{"schema_version":"coordplane.codemap.snapshot.v1","unknown":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write malformed snapshot: %v", err)
			}
			var stdout, stderr bytes.Buffer
			if err := runCodemap([]string{"validate", "--snapshot", path}, &stdout, &stderr); err == nil {
				t.Fatalf("validate accepted malformed snapshot %s; stdout=%s stderr=%s", tc.name, stdout.String(), stderr.String())
			}
		})
	}
}

func writeCLICodemapFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeCLIFile(t, root, "go.mod", "module example.com/codemapclifixture\n\ngo 1.22\n")
	writeCLIFile(t, root, "need/README.md", "# Needs\n")
	writeCLIFile(t, root, "need/验收合同.md", "# Acceptance\n\n## CLI gate\n")
	writeCLIFile(t, root, "cmd/app/main.go", `package main

import "example.com/codemapclifixture/internal/widget"

func main() {
	_ = widget.NewWidget()
}
`)
	writeCLIFile(t, root, "internal/widget/widget.go", `package widget

type Widget struct{}

func NewWidget() Widget {
	return Widget{}
}
`)
	writeCLIFile(t, root, "internal/widget/widget_test.go", `package widget

import "testing"

func TestWidget(t *testing.T) {
	_ = NewWidget()
}
`)
	writeCLIFile(t, root, "team_config/fixtures/team.yaml", `team_id: codemap-cli
version: 1
agents:
  - id: dev
    runtime_profile: local
    cli_backend: codex
runtime_profiles:
  local:
    kind: local
`)
	writeCLIFile(t, root, "Makefile", "test:\n\tgo test ./...\n")
	writeCLIFile(t, root, "scripts/release-health-cli.sh", "#!/usr/bin/env bash\n")
	return root
}

func writeCLIFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture file %s: %v", rel, err)
	}
}

func appendFile(t *testing.T, path, body string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open file for append: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(body); err != nil {
		t.Fatalf("append file: %v", err)
	}
}
