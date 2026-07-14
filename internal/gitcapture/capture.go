package gitcapture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const (
	PartialName        = "capture.partial"
	ReadyName          = "capture.ready"
	InspectPartialName = "inspect.partial"
	InspectReadyName   = "inspect.ready"
	BundleName         = "result.bundle"
	FactsName          = "facts.json"
)

var (
	objectIDPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	refPattern      = regexp.MustCompile(`^refs/[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

type Request struct {
	Workspace          string
	Handoff            string
	ExpectedHead       string
	BaseSHA            string
	SourceSHA          string
	MaximumBundleBytes int64
	MaximumObjects     int
}

type Fact struct {
	HeadSHA      string `json:"head_sha"`
	StatusDigest string `json:"status_digest"`
	ObjectCount  int    `json:"object_count"`
	BundleBytes  int64  `json:"bundle_bytes"`
	Clean        bool   `json:"clean"`
	Unfinished   bool   `json:"unfinished_operation"`
}

type InspectRequest struct {
	Workspace      string
	Handoff        string
	MaximumObjects int
}

func Capture(ctx context.Context, request Request) (Fact, error) {
	if err := validateRequest(request); err != nil {
		return Fact{}, err
	}
	if fact, ok, err := readyFact(request); ok || err != nil {
		return fact, err
	}
	partial := filepath.Join(request.Handoff, PartialName)
	ready := filepath.Join(request.Handoff, ReadyName)
	if err := os.RemoveAll(partial); err != nil {
		return Fact{}, fmt.Errorf("capture helper: remove stale partial handoff: %w", err)
	}
	if err := os.Mkdir(partial, 0o770); err != nil {
		return Fact{}, fmt.Errorf("capture helper: create partial handoff: %w", err)
	}
	if err := os.Chmod(partial, 0o770); err != nil {
		return Fact{}, fmt.Errorf("capture helper: set partial handoff access: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(partial)
		}
	}()

	gitDir, err := directGitDirectory(request.Workspace)
	if err != nil {
		return Fact{}, err
	}
	head, err := resolveHead(gitDir)
	if err != nil {
		return Fact{}, err
	}
	if head != request.ExpectedHead {
		return Fact{}, errors.New("capture helper: actual workspace HEAD does not match expected head")
	}
	trustedRoot, err := os.MkdirTemp("", "coordplane-git-capture-")
	if err != nil {
		return Fact{}, fmt.Errorf("capture helper: create trusted Git view: %w", err)
	}
	defer os.RemoveAll(trustedRoot)
	trustedGitDir, env, err := prepareTrustedView(ctx, trustedRoot, request.Workspace, gitDir, head)
	if err != nil {
		return Fact{}, err
	}
	if err := rejectUnfinishedOperation(gitDir); err != nil {
		return Fact{}, err
	}
	status, err := runGit(ctx, env, "--git-dir="+trustedGitDir, "--work-tree="+request.Workspace,
		"status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Fact{}, fmt.Errorf("capture helper: inspect clean workspace: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return Fact{}, errors.New("capture helper: workspace must be clean before capture")
	}
	for name, ancestor := range map[string]string{"base": request.BaseSHA, "source": request.SourceSHA} {
		if ancestor == "" {
			continue
		}
		included, err := isAncestor(ctx, env, trustedGitDir, ancestor, head)
		if err != nil {
			return Fact{}, fmt.Errorf("capture helper: verify %s ancestry: %w", name, err)
		}
		if !included {
			return Fact{}, fmt.Errorf("capture helper: workspace head does not contain immutable %s", name)
		}
	}
	count, err := countObjects(ctx, env, trustedGitDir, request.MaximumObjects)
	if err != nil {
		return Fact{}, err
	}
	bundlePath := filepath.Join(partial, BundleName)
	bundleBytes, err := writeBundle(ctx, env, trustedGitDir, bundlePath, request.MaximumBundleBytes)
	if err != nil {
		return Fact{}, err
	}
	digest := sha256.Sum256([]byte(status))
	fact := Fact{
		HeadSHA: head, StatusDigest: hex.EncodeToString(digest[:]),
		ObjectCount: count, BundleBytes: bundleBytes, Clean: true,
	}
	if err := writeFacts(filepath.Join(partial, FactsName), fact); err != nil {
		return Fact{}, err
	}
	if err := syncDirectory(partial); err != nil {
		return Fact{}, err
	}
	if err := os.Rename(partial, ready); err != nil {
		if replay, ok, replayErr := readyFact(request); ok || replayErr != nil {
			return replay, replayErr
		}
		return Fact{}, fmt.Errorf("capture helper: publish ready handoff: %w", err)
	}
	if err := syncDirectory(request.Handoff); err != nil {
		return Fact{}, err
	}
	published = true
	return fact, nil
}

func Inspect(ctx context.Context, request InspectRequest) (Fact, error) {
	if err := validateInspectRequest(request); err != nil {
		return Fact{}, err
	}
	if fact, ok, err := readyInspectFact(request); ok || err != nil {
		return fact, err
	}
	partial := filepath.Join(request.Handoff, InspectPartialName)
	ready := filepath.Join(request.Handoff, InspectReadyName)
	if err := os.RemoveAll(partial); err != nil {
		return Fact{}, fmt.Errorf("capture helper: remove stale inspect partial: %w", err)
	}
	if err := os.Mkdir(partial, 0o770); err != nil {
		return Fact{}, fmt.Errorf("capture helper: create inspect partial: %w", err)
	}
	if err := os.Chmod(partial, 0o770); err != nil {
		return Fact{}, fmt.Errorf("capture helper: set inspect partial access: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(partial)
		}
	}()
	gitDir, err := directGitDirectory(request.Workspace)
	if err != nil {
		return Fact{}, err
	}
	head, err := resolveHead(gitDir)
	if err != nil {
		return Fact{}, err
	}
	trustedRoot, err := os.MkdirTemp("", "coordplane-git-inspect-")
	if err != nil {
		return Fact{}, fmt.Errorf("capture helper: create trusted inspect view: %w", err)
	}
	defer os.RemoveAll(trustedRoot)
	trustedGitDir, env, err := prepareTrustedView(ctx, trustedRoot, request.Workspace, gitDir, head)
	if err != nil {
		return Fact{}, err
	}
	unfinished, err := hasUnfinishedOperation(gitDir)
	if err != nil {
		return Fact{}, err
	}
	status, err := runGit(ctx, env, "--git-dir="+trustedGitDir, "--work-tree="+request.Workspace,
		"status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Fact{}, fmt.Errorf("capture helper: inspect workspace status: %w", err)
	}
	count, err := countObjects(ctx, env, trustedGitDir, request.MaximumObjects)
	if err != nil {
		return Fact{}, err
	}
	digest := sha256.Sum256([]byte(status))
	fact := Fact{
		HeadSHA: head, StatusDigest: hex.EncodeToString(digest[:]), ObjectCount: count,
		Clean: status == "", Unfinished: unfinished,
	}
	if err := writeFacts(filepath.Join(partial, FactsName), fact); err != nil {
		return Fact{}, err
	}
	if err := syncDirectory(partial); err != nil {
		return Fact{}, err
	}
	if err := os.Rename(partial, ready); err != nil {
		if replay, ok, replayErr := readyInspectFact(request); ok || replayErr != nil {
			return replay, replayErr
		}
		return Fact{}, fmt.Errorf("capture helper: publish inspect ready: %w", err)
	}
	if err := syncDirectory(request.Handoff); err != nil {
		return Fact{}, err
	}
	published = true
	return fact, nil
}

func validateRequest(request Request) error {
	for name, value := range map[string]string{
		"workspace": request.Workspace, "handoff": request.Handoff,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("capture helper: %s must be canonical and absolute", name)
		}
		info, err := os.Lstat(value)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("capture helper: %s must be a direct directory", name)
		}
	}
	for name, value := range map[string]string{
		"expected head": request.ExpectedHead, "base": request.BaseSHA,
	} {
		if !objectIDPattern.MatchString(value) {
			return fmt.Errorf("capture helper: %s must be an object ID", name)
		}
	}
	if request.SourceSHA != "" && !objectIDPattern.MatchString(request.SourceSHA) {
		return errors.New("capture helper: source must be an object ID")
	}
	if request.MaximumBundleBytes <= 0 || request.MaximumObjects <= 0 {
		return errors.New("capture helper: positive bundle byte and object limits are required")
	}
	return nil
}

func validateInspectRequest(request InspectRequest) error {
	for name, value := range map[string]string{"workspace": request.Workspace, "handoff": request.Handoff} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("capture helper: %s must be canonical and absolute", name)
		}
		info, err := os.Lstat(value)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("capture helper: %s must be a direct directory", name)
		}
	}
	if request.MaximumObjects <= 0 {
		return errors.New("capture helper: positive object limit is required")
	}
	return nil
}

func directGitDirectory(workspace string) (string, error) {
	gitDir := filepath.Join(workspace, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("capture helper: workspace Git directory must be direct")
	}
	objects := filepath.Join(gitDir, "objects")
	info, err = os.Lstat(objects)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("capture helper: workspace object directory must be direct")
	}
	if _, err := os.Lstat(filepath.Join(objects, "info", "alternates")); err == nil {
		return "", errors.New("capture helper: object alternates are forbidden")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("capture helper: inspect object alternates")
	}
	return gitDir, nil
}

func resolveHead(gitDir string) (string, error) {
	raw, err := readSmallRegular(filepath.Join(gitDir, "HEAD"), 4096)
	if err != nil {
		return "", fmt.Errorf("capture helper: read HEAD: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	if objectIDPattern.MatchString(value) {
		return value, nil
	}
	ref, ok := strings.CutPrefix(value, "ref: ")
	if !ok || !validRef(ref) {
		return "", errors.New("capture helper: HEAD is not a valid commit reference")
	}
	loose, err := readSmallRegular(filepath.Join(gitDir, filepath.FromSlash(ref)), 4096)
	if err == nil {
		value = strings.TrimSpace(string(loose))
		if objectIDPattern.MatchString(value) {
			return value, nil
		}
		return "", errors.New("capture helper: loose HEAD reference is invalid")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("capture helper: read HEAD reference: %w", err)
	}
	packed, err := readSmallRegular(filepath.Join(gitDir, "packed-refs"), 8<<20)
	if err != nil {
		return "", fmt.Errorf("capture helper: resolve packed HEAD reference: %w", err)
	}
	for _, line := range strings.Split(string(packed), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref && objectIDPattern.MatchString(fields[0]) {
			return fields[0], nil
		}
	}
	return "", errors.New("capture helper: HEAD reference is missing")
}

func validRef(ref string) bool {
	return refPattern.MatchString(ref) && !strings.Contains(ref, "..") &&
		!strings.Contains(ref, "//") && !strings.HasSuffix(ref, ".lock")
}

func prepareTrustedView(ctx context.Context, root, workspace, sourceGitDir, head string) (string, []string, error) {
	if _, err := runGit(ctx, fixedEnvironment(), "init", "-q", root); err != nil {
		return "", nil, fmt.Errorf("capture helper: initialize trusted Git view: %w", err)
	}
	gitDir := filepath.Join(root, ".git")
	config := []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tfilemode = true\n\tfsmonitor = false\n[protocol]\n\tallow = never\n")
	if err := os.WriteFile(filepath.Join(gitDir, "config"), config, 0o600); err != nil {
		return "", nil, fmt.Errorf("capture helper: write trusted config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head+"\n"), 0o600); err != nil {
		return "", nil, fmt.Errorf("capture helper: write trusted HEAD: %w", err)
	}
	indexSource := filepath.Join(sourceGitDir, "index")
	index, err := readSmallRegular(indexSource, 64<<20)
	if err != nil {
		return "", nil, fmt.Errorf("capture helper: read workspace index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index"), index, 0o600); err != nil {
		return "", nil, fmt.Errorf("capture helper: copy workspace index: %w", err)
	}
	env := append(fixedEnvironment(),
		"GIT_OBJECT_DIRECTORY="+filepath.Join(sourceGitDir, "objects"),
		"GIT_WORK_TREE="+workspace,
	)
	return gitDir, env, nil
}

func fixedEnvironment() []string {
	env := []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	return env
}

func rejectUnfinishedOperation(gitDir string) error {
	unfinished, err := hasUnfinishedOperation(gitDir)
	if err != nil {
		return err
	}
	if unfinished {
		return errors.New("capture helper: workspace has an unfinished Git operation")
	}
	return nil
}

func hasUnfinishedOperation(gitDir string) (bool, error) {
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-apply", "rebase-merge"} {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("capture helper: inspect unfinished Git operation")
		}
	}
	return false, nil
}

func isAncestor(ctx context.Context, env []string, gitDir, ancestor, head string) (bool, error) {
	_, err := runGit(ctx, env, "--git-dir="+gitDir, "merge-base", "--is-ancestor", ancestor, head)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func countObjects(ctx context.Context, env []string, gitDir string, maximum int) (int, error) {
	command := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "rev-list", "--objects", "HEAD")
	command.Env = env
	stdout, err := command.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	count := 0
	for scanner.Scan() {
		count++
		if count > maximum {
			_ = command.Process.Kill()
			_ = command.Wait()
			return 0, errors.New("capture helper: object limit exceeded")
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if scanErr != nil {
		return 0, fmt.Errorf("capture helper: count objects: %w", scanErr)
	}
	if waitErr != nil {
		return 0, fmt.Errorf("capture helper: count objects: %w: %s", waitErr, stderr.String())
	}
	return count, nil
}

func writeBundle(ctx context.Context, env []string, gitDir, path string, maximum int64) (int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o660)
	if err != nil {
		return 0, fmt.Errorf("capture helper: create bundle: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	command := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "bundle", "create", "-", "HEAD")
	command.Env = env
	stdout, err := command.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(stdout, maximum+1))
	if copyErr != nil || written > maximum {
		_ = command.Process.Kill()
		_ = command.Wait()
		if copyErr != nil {
			return 0, fmt.Errorf("capture helper: write bundle: %w", copyErr)
		}
		return 0, errors.New("capture helper: bundle byte limit exceeded")
	}
	if err := command.Wait(); err != nil {
		return 0, fmt.Errorf("capture helper: create bundle: %w: %s", err, stderr.String())
	}
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("capture helper: sync bundle: %w", err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("capture helper: close bundle: %w", err)
	}
	closed = true
	return written, nil
}

func writeFacts(path string, fact Fact) error {
	raw, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o660)
	if err != nil {
		return fmt.Errorf("capture helper: create facts: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("capture helper: write facts: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("capture helper: sync facts: %w", err)
	}
	return file.Close()
}

func readyFact(request Request) (Fact, bool, error) {
	ready := filepath.Join(request.Handoff, ReadyName)
	info, err := os.Lstat(ready)
	if errors.Is(err, os.ErrNotExist) {
		return Fact{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Fact{}, true, errors.New("capture helper: ready handoff is not a direct directory")
	}
	raw, err := readSmallRegular(filepath.Join(ready, FactsName), 64<<10)
	if err != nil {
		return Fact{}, true, fmt.Errorf("capture helper: read ready facts: %w", err)
	}
	var fact Fact
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return Fact{}, true, fmt.Errorf("capture helper: decode ready facts: %w", err)
	}
	if fact.HeadSHA != request.ExpectedHead || fact.ObjectCount <= 0 || fact.ObjectCount > request.MaximumObjects ||
		fact.BundleBytes <= 0 || fact.BundleBytes > request.MaximumBundleBytes || !fact.Clean || fact.Unfinished ||
		fact.StatusDigest != emptyStatusDigest() {
		return Fact{}, true, errors.New("capture helper: ready facts do not match request limits")
	}
	bundle, err := os.Lstat(filepath.Join(ready, BundleName))
	if err != nil || bundle.Mode()&os.ModeSymlink != 0 || !bundle.Mode().IsRegular() || bundle.Size() != fact.BundleBytes {
		return Fact{}, true, errors.New("capture helper: ready bundle does not match facts")
	}
	return fact, true, nil
}

func readyInspectFact(request InspectRequest) (Fact, bool, error) {
	ready := filepath.Join(request.Handoff, InspectReadyName)
	info, err := os.Lstat(ready)
	if errors.Is(err, os.ErrNotExist) {
		return Fact{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Fact{}, true, errors.New("capture helper: inspect ready is not a direct directory")
	}
	raw, err := readSmallRegular(filepath.Join(ready, FactsName), 64<<10)
	if err != nil {
		return Fact{}, true, fmt.Errorf("capture helper: read inspect facts: %w", err)
	}
	var fact Fact
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return Fact{}, true, fmt.Errorf("capture helper: decode inspect facts: %w", err)
	}
	if !objectIDPattern.MatchString(fact.HeadSHA) || len(fact.StatusDigest) != sha256.Size*2 ||
		fact.ObjectCount <= 0 || fact.ObjectCount > request.MaximumObjects || fact.BundleBytes != 0 {
		return Fact{}, true, errors.New("capture helper: inspect facts violate identity or object limit")
	}
	if _, err := hex.DecodeString(fact.StatusDigest); err != nil || fact.Clean != (fact.StatusDigest == emptyStatusDigest()) {
		return Fact{}, true, errors.New("capture helper: inspect facts contain an invalid status digest")
	}
	return fact, true, nil
}

func emptyStatusDigest() string {
	digest := sha256.Sum256(nil)
	return hex.EncodeToString(digest[:])
}

func readSmallRegular(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("file is not a bounded direct regular file")
	}
	return os.ReadFile(path)
}

func runGit(ctx context.Context, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = env
	var stdout, stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(raw []byte) (int, error) {
	written := len(raw)
	remaining := (1 << 20) - b.Len()
	if remaining > 0 {
		if remaining < len(raw) {
			raw = raw[:remaining]
		}
		_, _ = b.Buffer.Write(raw)
	}
	return written, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("capture helper: open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("capture helper: sync directory: %w", err)
	}
	return nil
}
