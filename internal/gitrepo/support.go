package gitrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const markerFilename = "coordplane-project.json"

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (i *Initializer) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, i.gitPath, args...)
	cmd.Env = gitEnvironment()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &gitCommandError{args: append([]string(nil), args...), stderr: boundedText(stderr.String()), err: err}
	}
	return stdout.String(), nil
}

type gitCommandError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitCommandError) Error() string {
	if e.stderr == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitCommandError) Unwrap() error { return e.err }

func gitEnvironment() []string {
	// Git has many environment overrides for repository discovery, object
	// storage, and config injection. Start from a fixed environment rather than
	// trying to maintain a denylist of variables that must not cross the trust
	// boundary.
	env := make([]string, 0, 10)
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file",
		"LC_ALL=C",
	)
}

type repositoryMarker struct {
	Version      int    `json:"version"`
	ProjectID    string `json:"project_id"`
	OperationID  string `json:"operation_id"`
	SourcePath   string `json:"source_path"`
	SourceRef    string `json:"source_ref"`
	InitialSHA   string `json:"initial_sha"`
	CanonicalRef string `json:"canonical_ref"`
}

func writeMarker(path string, marker repositoryMarker) error {
	raw, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("gitrepo: encode ownership marker: %w", err)
	}
	parent := filepath.Dir(path)
	tmp, err := os.CreateTemp(parent, ".coordplane-marker-")
	if err != nil {
		return fmt.Errorf("gitrepo: create ownership marker: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gitrepo: chmod ownership marker: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gitrepo: write ownership marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gitrepo: sync ownership marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gitrepo: close ownership marker: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("gitrepo: publish ownership marker: %w", err)
	}
	return nil
}

func readMarker(path string) (repositoryMarker, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return repositoryMarker{}, fmt.Errorf("gitrepo: stat ownership marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return repositoryMarker{}, errors.New("gitrepo: ownership marker must be a direct regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return repositoryMarker{}, fmt.Errorf("gitrepo: read ownership marker: %w", err)
	}
	var marker repositoryMarker
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return repositoryMarker{}, fmt.Errorf("gitrepo: decode ownership marker: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return repositoryMarker{}, errors.New("gitrepo: ownership marker contains trailing content")
	}
	if marker.Version != 1 {
		return repositoryMarker{}, errors.New("gitrepo: unsupported ownership marker version")
	}
	return marker, nil
}

func canonicalExistingDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return canonical, nil
}

func (i *Initializer) validateFinalPath(path string) error {
	if i == nil {
		return errors.New("gitrepo: nil initializer")
	}
	if err := i.validateRoot(); err != nil {
		return err
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("gitrepo: control repository path must be canonical and absolute")
	}
	if filepath.Dir(path) != i.root {
		return errors.New("gitrepo: control repository path is outside the repository root")
	}
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".git") {
		return errors.New("gitrepo: control repository path is not a deterministic final path")
	}
	projectID := strings.TrimSuffix(base, ".git")
	if err := validateID("project", projectID); err != nil {
		return err
	}
	if path != filepath.Join(i.root, projectID+".git") {
		return errors.New("gitrepo: control repository path is not deterministic")
	}
	return validateDirectDirectory(path, "control repository")
}

func (i *Initializer) validateRoot() error {
	if i == nil {
		return errors.New("gitrepo: nil initializer")
	}
	return validateDirectDirectory(i.root, "repository root")
}

func (i *Initializer) ensureDirectSubdirectories(target string) error {
	if err := i.validateRoot(); err != nil {
		return err
	}
	relative, err := filepath.Rel(i.root, target)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("gitrepo: repository path escapes the repository root")
	}
	current := i.root
	if relative == "." {
		return nil
	}
	for _, element := range strings.Split(relative, string(filepath.Separator)) {
		if element == "" || element == "." || element == ".." {
			return errors.New("gitrepo: repository path contains an invalid component")
		}
		parent := current
		current = filepath.Join(current, element)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("gitrepo: create repository directory: %w", err)
		}
		if err := validateDirectDirectory(current, "repository directory"); err != nil {
			return err
		}
		if err := syncDirectory(parent); err != nil {
			return fmt.Errorf("gitrepo: sync repository directory parent: %w", err)
		}
	}
	return nil
}

func (i *Initializer) validatePartialPath(path string) error {
	if err := i.validateRoot(); err != nil {
		return err
	}
	relative, err := filepath.Rel(i.root, path)
	if err != nil || filepath.IsAbs(relative) {
		return errors.New("gitrepo: partial repository path is outside the repository root")
	}
	elements := strings.Split(relative, string(filepath.Separator))
	if len(elements) != 3 || elements[0] != ".partial" || !strings.HasSuffix(elements[2], ".git") {
		return errors.New("gitrepo: partial repository path is not deterministic")
	}
	if err := validateID("project", elements[1]); err != nil {
		return err
	}
	if err := validateID("operation", strings.TrimSuffix(elements[2], ".git")); err != nil {
		return err
	}
	for index := 1; index <= len(elements); index++ {
		current := filepath.Join(append([]string{i.root}, elements[:index]...)...)
		if err := validateDirectDirectory(current, "partial repository path"); err != nil {
			return err
		}
	}
	return nil
}

func validateDirectDirectory(path, kind string) error {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("gitrepo: stat %s: %w", kind, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("gitrepo: %s must not be a symlink", kind)
	}
	if !info.IsDir() {
		return fmt.Errorf("gitrepo: %s is not a directory", kind)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("gitrepo: resolve %s: %w", kind, err)
	}
	if filepath.Clean(resolved) != path {
		return fmt.Errorf("gitrepo: %s resolves outside its direct path", kind)
	}
	return nil
}

func requireEmptyDirectory(path, kind string) error {
	if err := validateDirectDirectory(path, kind); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("gitrepo: inspect %s: %w", kind, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("gitrepo: %s must be empty", kind)
	}
	return nil
}

func validateID(kind, value string) error {
	if !safeIDPattern.MatchString(value) || value == "." || value == ".." {
		return fmt.Errorf("gitrepo: invalid %s ID", kind)
	}
	return nil
}

func validateObjectID(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return errors.New("object ID must contain 40 or 64 hexadecimal characters")
	}
	for _, char := range sha {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return errors.New("object ID must be lowercase hexadecimal")
		}
	}
	return nil
}

func zeroObjectID(sha string) string { return strings.Repeat("0", len(sha)) }

func initializationRef(project Project) string {
	return "refs/coordplane/init/" + project.ID + "/" + project.OperationID
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("gitrepo: stat %s: %w", path, err)
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func boundedText(value string) string {
	value = strings.TrimSpace(value)
	const limit = 4096
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
