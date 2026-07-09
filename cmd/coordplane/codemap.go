package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"coordplane/internal/codemap"
)

func runCodemap(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("codemap subcommand is required")
	}
	switch args[0] {
	case "index":
		return runCodemapIndex(args[1:], stdout, stderr)
	case "validate":
		return runCodemapValidate(args[1:], stdout, stderr)
	case "check":
		return runCodemapCheck(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown codemap subcommand %q", args[0])
	}
}

func runCodemapIndex(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("codemap index", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var root string
	var out string
	var projectID string
	var resourceID string
	var changedFiles string
	var strict bool
	fs.StringVar(&root, "root", ".", "repository root to index")
	fs.StringVar(&out, "out", "", "write snapshot JSON to file instead of stdout")
	fs.StringVar(&projectID, "project-id", "", "project id to stamp into the snapshot")
	fs.StringVar(&resourceID, "resource-id", "", "resource id to stamp into the snapshot")
	fs.StringVar(&changedFiles, "changed-files", "", "comma-separated repo-relative changed files for future incremental refresh semantics")
	fs.BoolVar(&strict, "strict", false, "fail when generated snapshot validation reports errors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, err := codemap.Index(commandContext(), codemap.IndexOptions{
		Root:         root,
		ProjectID:    projectID,
		ResourceID:   resourceID,
		ChangedFiles: splitCSV(changedFiles),
	})
	if err != nil {
		return err
	}
	validation := codemap.ValidateSnapshot(snapshot)
	if strict && len(validation) > 0 {
		return fmt.Errorf("generated codemap snapshot failed validation: %s", validation[0].Message)
	}
	raw, err := codemap.MarshalSnapshot(snapshot)
	if err != nil {
		return err
	}
	if out == "" {
		_, err = stdout.Write(raw)
		return err
	}
	return writeCodemapFile(out, raw)
}

func runCodemapValidate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("codemap validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var snapshotPath string
	fs.StringVar(&snapshotPath, "snapshot", "", "snapshot JSON file to validate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(snapshotPath) == "" {
		return errors.New("--snapshot is required")
	}
	snapshot, err := readCodemapSnapshot(snapshotPath)
	if err != nil {
		return err
	}
	diagnostics := codemap.ValidateSnapshot(snapshot)
	summary := map[string]any{
		"ok":               len(diagnostics) == 0,
		"schema_version":   snapshot.SchemaVersion,
		"snapshot_id":      snapshot.SnapshotID,
		"status":           snapshot.Status,
		"diagnostic_count": len(diagnostics),
	}
	if len(diagnostics) > 0 {
		summary["diagnostics"] = diagnostics
	}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return err
	}
	if len(diagnostics) > 0 {
		return errors.New("codemap snapshot validation failed")
	}
	return nil
}

func runCodemapCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("codemap check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var root string
	var snapshotPath string
	var projectID string
	var resourceID string
	fs.StringVar(&root, "root", ".", "repository root to index")
	fs.StringVar(&snapshotPath, "snapshot", ".coordplane/codemap/latest.json", "existing snapshot JSON file to compare")
	fs.StringVar(&projectID, "project-id", "", "project id to use when regenerating stamped snapshots")
	fs.StringVar(&resourceID, "resource-id", "", "resource id to use when regenerating stamped snapshots")
	if err := fs.Parse(args); err != nil {
		return err
	}
	existing, err := readCodemapSnapshot(snapshotPath)
	if err != nil {
		return err
	}
	if validation := codemap.ValidateSnapshot(existing); len(validation) > 0 {
		return fmt.Errorf("existing codemap snapshot failed validation: %s", validation[0].Message)
	}
	if strings.TrimSpace(projectID) == "" {
		projectID = existing.ProjectID
	}
	if strings.TrimSpace(resourceID) == "" {
		resourceID = existing.ResourceID
	}
	current, err := codemap.Index(commandContext(), codemap.IndexOptions{
		Root:         root,
		ProjectID:    projectID,
		ResourceID:   resourceID,
		ChangedFiles: existing.GeneratedFrom.ChangedFiles,
	})
	if err != nil {
		return err
	}
	if validation := codemap.ValidateSnapshot(current); len(validation) > 0 {
		return fmt.Errorf("current codemap snapshot failed validation: %s", validation[0].Message)
	}
	existingJSON, err := codemap.MarshalSnapshot(existing)
	if err != nil {
		return err
	}
	currentJSON, err := codemap.MarshalSnapshot(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(existingJSON, currentJSON) {
		return fmt.Errorf("codemap snapshot drift detected: regenerate %s with coordplane codemap index --root %s --out %s", snapshotPath, root, snapshotPath)
	}
	_, err = fmt.Fprintf(stdout, "{\"ok\":true,\"snapshot_id\":%q}\n", current.SnapshotID)
	return err
}

func readCodemapSnapshot(path string) (codemap.Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return codemap.Snapshot{}, fmt.Errorf("read codemap snapshot: %w", err)
	}
	snapshot, err := codemap.DecodeSnapshot(raw)
	if err != nil {
		return codemap.Snapshot{}, fmt.Errorf("decode codemap snapshot: %w", err)
	}
	return snapshot, nil
}

func writeCodemapFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func commandContext() context.Context {
	return context.Background()
}
