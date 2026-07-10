package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"coordplane/internal/backend"
	"coordplane/internal/buildinfo"
	"coordplane/internal/releasehealth"
)

type typedResponse struct {
	OK        bool            `json:"ok"`
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Message   string          `json:"message,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, http.DefaultClient); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, client *http.Client) error {
	if client == nil {
		client = http.DefaultClient
	}
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return nil
	case "version":
		return json.NewEncoder(stdout).Encode(buildinfo.Current())
	case "serve":
		return runServe(args[1:], stderr)
	case "task":
		return runTask(args[1:], stdout, stderr, client)
	case "release-health":
		return runReleaseHealth(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  coordplane version")
	fmt.Fprintln(w, "  coordplane serve --db PATH [--listen ADDR] [--teamconfig PATH]")
	fmt.Fprintln(w, "  coordplane task create --backend-url URL --payload FILE [--operator-token-env ENV]")
	fmt.Fprintln(w, "  coordplane task run --backend-url URL --payload FILE [--wait] [--evidence-out PATH] [--operator-token-env ENV]")
	fmt.Fprintln(w, "  coordplane release-health cp-accept-001 [flags]")
	fmt.Fprintln(w, "  coordplane release-health cp-probe-001 [flags]")
}

func runServe(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg backend.Config
	var claudeEnv string
	fs.StringVar(&cfg.DBPath, "db", "", "sqlite database path")
	fs.StringVar(&cfg.ListenAddr, "listen", "", "listen address")
	fs.StringVar(&cfg.TeamConfigPath, "teamconfig", "", "TeamConfig YAML path")
	fs.StringVar(&cfg.TeamID, "team-id", "", "TeamConfig team id override")
	fs.StringVar(&cfg.BackendURL, "backend-url", "", "public backend URL for runtime env")
	fs.StringVar(&cfg.RuntimeWorkspaceRoot, "runtime-workspace-root", "", "external runtime workspace root")
	fs.StringVar(&cfg.RuntimeHomeRoot, "runtime-home-root", "", "external runtime home root")
	fs.StringVar(&cfg.DockerNetwork, "docker-network", "", "Docker runtime network")
	fs.StringVar(&cfg.CoordlinkPath, "coordlink", "", "coordlink binary path for Docker runtime")
	fs.StringVar(&cfg.ClaudeBinary, "claude-bin", "", "Claude binary path")
	fs.StringVar(&claudeEnv, "claude-env", "", "comma-separated Claude env allowlist")
	fs.StringVar(&cfg.OperatorToken, "operator-token", "", "operator bearer token")
	fs.StringVar(&cfg.OperatorTokenEnv, "operator-token-env", "", "operator token env var")
	fs.StringVar(&cfg.OperatorSubjectID, "operator-subject-id", "", "operator subject id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.ClaudeEnvKeys = splitCSV(claudeEnv)
	return backend.RunServe(context.Background(), cfg)
}

func runTask(args []string, stdout, stderr io.Writer, client *http.Client) error {
	if len(args) == 0 {
		return errors.New("task subcommand is required")
	}
	switch args[0] {
	case "create":
		return runTaskCreate(args[1:], stdout, stderr, client)
	case "run":
		return runTaskRun(args[1:], stdout, stderr, client)
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

type taskFlags struct {
	backendURL              string
	payloadPath             string
	operatorToken           string
	operatorTokenEnv        string
	startIdempotencyKey     string
	executionTimeoutSeconds int
	executionTimeoutMillis  int
	wait                    bool
	evidenceOut             string
	waitTimeoutSeconds      int
	waitTimeoutMillis       int
	pollIntervalMillis      int
}

func commonTaskFlags(name string, stderr io.Writer) (*flag.FlagSet, *taskFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := &taskFlags{}
	fs.StringVar(&cfg.backendURL, "backend-url", "", "CoordPlane backend URL")
	fs.StringVar(&cfg.payloadPath, "payload", "", "operator task payload JSON file")
	fs.StringVar(&cfg.operatorToken, "operator-token", "", "operator bearer token")
	fs.StringVar(&cfg.operatorTokenEnv, "operator-token-env", "COORDPLANE_OPERATOR_TOKEN", "operator token env var")
	return fs, cfg
}

func runTaskCreate(args []string, stdout, stderr io.Writer, client *http.Client) error {
	fs, cfg := commonTaskFlags("task create", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, payload, err := cfg.operatorRequestParts()
	if err != nil {
		return err
	}
	raw, response, err := postOperatorJSON(client, cfg.backendURL, "/operator/tasks", token, payload)
	if err != nil {
		return err
	}
	if response.Status != "accepted" {
		return fmt.Errorf("operator task create rejected: %s", string(raw))
	}
	_, err = stdout.Write(append(raw, '\n'))
	return err
}

func runTaskRun(args []string, stdout, stderr io.Writer, client *http.Client) error {
	fs, cfg := commonTaskFlags("task run", stderr)
	fs.StringVar(&cfg.startIdempotencyKey, "start-idempotency-key", "", "operator task start idempotency key")
	fs.IntVar(&cfg.executionTimeoutSeconds, "execution-timeout-seconds", 0, "explicit operator task execution deadline in seconds")
	fs.IntVar(&cfg.executionTimeoutMillis, "execution-timeout-ms", 0, "explicit operator task execution deadline in milliseconds")
	fs.BoolVar(&cfg.wait, "wait", false, "wait for operator task terminal evidence state")
	fs.StringVar(&cfg.evidenceOut, "evidence-out", "", "write operator evidence bundle JSON to path")
	fs.IntVar(&cfg.waitTimeoutSeconds, "wait-timeout-seconds", 30, "operator task wait timeout in seconds")
	fs.IntVar(&cfg.waitTimeoutMillis, "wait-timeout-ms", 0, "operator task wait timeout in milliseconds")
	fs.IntVar(&cfg.pollIntervalMillis, "poll-interval-ms", 250, "operator task wait poll interval in milliseconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, payload, err := cfg.operatorRequestParts()
	if err != nil {
		return err
	}
	payload, err = taskRunStartablePayload(payload)
	if err != nil {
		return err
	}
	createRaw, createResponse, err := postOperatorJSON(client, cfg.backendURL, "/operator/tasks", token, payload)
	if err != nil {
		return err
	}
	if createResponse.Status != "accepted" {
		return fmt.Errorf("operator task create rejected: %s", string(createRaw))
	}
	var createData struct {
		TaskRunID string `json:"task_run_id"`
	}
	if err := json.Unmarshal(createResponse.Data, &createData); err != nil {
		return fmt.Errorf("decode operator task create response: %w", err)
	}
	if createData.TaskRunID == "" {
		return errors.New("operator task create response missing task_run_id")
	}
	startKey := strings.TrimSpace(cfg.startIdempotencyKey)
	if startKey == "" {
		startKey = "start:" + createData.TaskRunID
	}
	startBody, err := json.Marshal(map[string]any{
		"idempotency_key":           startKey,
		"execution_timeout_seconds": cfg.executionTimeoutSeconds,
		"execution_timeout_millis":  cfg.executionTimeoutMillis,
	})
	if err != nil {
		return err
	}
	startPath := "/operator/tasks/" + url.PathEscape(createData.TaskRunID) + "/start"
	startRaw, startResponse, err := postOperatorJSON(client, cfg.backendURL, startPath, token, startBody)
	if err != nil {
		_ = cfg.writeEvidenceAfterStartFailure(client, token, createData.TaskRunID)
		return err
	}
	if startResponse.Status != "accepted" {
		_ = cfg.writeEvidenceAfterStartFailure(client, token, createData.TaskRunID)
		return fmt.Errorf("operator task start rejected: %s", string(startRaw))
	}
	if cfg.wait {
		waitBody, err := json.Marshal(map[string]int{
			"timeout_seconds":      cfg.waitTimeoutSeconds,
			"timeout_millis":       cfg.waitTimeoutMillis,
			"poll_interval_millis": cfg.pollIntervalMillis,
		})
		if err != nil {
			return err
		}
		waitPath := "/operator/tasks/" + url.PathEscape(createData.TaskRunID) + "/wait"
		waitRaw, waitResponse, err := postOperatorJSON(client, cfg.backendURL, waitPath, token, waitBody)
		if err != nil {
			return err
		}
		if waitResponse.Status != "accepted" {
			return fmt.Errorf("operator task wait rejected: %s", string(waitRaw))
		}
		if strings.TrimSpace(cfg.evidenceOut) != "" {
			if err := writeEvidenceFromWaitResponse(cfg.evidenceOut, waitResponse); err != nil {
				return err
			}
		}
		_, err = stdout.Write(append(waitRaw, '\n'))
		return err
	}
	if strings.TrimSpace(cfg.evidenceOut) != "" {
		evidencePath := "/operator/tasks/" + url.PathEscape(createData.TaskRunID) + "/evidence"
		evidenceRaw, evidenceResponse, err := getOperatorJSON(client, cfg.backendURL, evidencePath, token)
		if err != nil {
			return err
		}
		if evidenceResponse.Status != "accepted" {
			return fmt.Errorf("operator task evidence rejected: %s", string(evidenceRaw))
		}
		if err := writeRawJSONFile(cfg.evidenceOut, evidenceResponse.Data); err != nil {
			return err
		}
	}
	_, err = stdout.Write(append(startRaw, '\n'))
	return err
}

func taskRunStartablePayload(payload []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return nil, errors.New("task run payload must be a JSON object")
	}
	object["require_startable"] = json.RawMessage("true")
	out, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode startable task run payload: %w", err)
	}
	return out, nil
}

func (cfg taskFlags) writeEvidenceAfterStartFailure(client *http.Client, token, taskRunID string) error {
	if !cfg.wait || strings.TrimSpace(cfg.evidenceOut) == "" || strings.TrimSpace(taskRunID) == "" {
		return nil
	}
	evidencePath := "/operator/tasks/" + url.PathEscape(taskRunID) + "/evidence"
	_, evidenceResponse, err := getOperatorJSON(client, cfg.backendURL, evidencePath, token)
	if err != nil {
		return err
	}
	if evidenceResponse.Status != "accepted" {
		return fmt.Errorf("operator task evidence rejected after start failure")
	}
	return writeRawJSONFile(cfg.evidenceOut, evidenceResponse.Data)
}

func (cfg taskFlags) operatorRequestParts() (string, []byte, error) {
	if strings.TrimSpace(cfg.backendURL) == "" {
		return "", nil, errors.New("--backend-url is required")
	}
	if strings.TrimSpace(cfg.payloadPath) == "" {
		return "", nil, errors.New("--payload is required")
	}
	token := strings.TrimSpace(cfg.operatorToken)
	if token == "" && strings.TrimSpace(cfg.operatorTokenEnv) != "" {
		token = strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.operatorTokenEnv)))
	}
	if token == "" {
		return "", nil, errors.New("operator token is required")
	}
	payload, err := os.ReadFile(cfg.payloadPath)
	if err != nil {
		return "", nil, fmt.Errorf("read payload: %w", err)
	}
	if !json.Valid(payload) {
		return "", nil, errors.New("payload must be valid JSON")
	}
	return token, payload, nil
}

func postOperatorJSON(client *http.Client, backendURL, path, token string, body []byte) ([]byte, typedResponse, error) {
	endpoint := strings.TrimRight(backendURL, "/") + path
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, typedResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, typedResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, typedResponse{}, err
	}
	var decoded typedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return raw, typedResponse{}, fmt.Errorf("decode operator response: %w; body=%s", err, string(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, decoded, fmt.Errorf("operator endpoint returned HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return raw, decoded, nil
}

func getOperatorJSON(client *http.Client, backendURL, path, token string) ([]byte, typedResponse, error) {
	endpoint := strings.TrimRight(backendURL, "/") + path
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, typedResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, typedResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, typedResponse{}, err
	}
	var decoded typedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return raw, typedResponse{}, fmt.Errorf("decode operator response: %w; body=%s", err, string(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, decoded, fmt.Errorf("operator endpoint returned HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return raw, decoded, nil
}

func runReleaseHealth(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("release-health scenario is required")
	}
	switch args[0] {
	case releasehealth.ScenarioCPAccept001:
		return runCPAccept001(args[1:], stdout, stderr)
	case releasehealth.ScenarioCPProbe001:
		return runCPProbe001(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown release-health scenario %q", args[0])
	}
}

func runCPAccept001(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release-health cp-accept-001", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg releasehealth.CPAccept001Config
	var teamVersion string
	var claudeEnv string
	var inspectOut string
	fs.StringVar(&cfg.DBPath, "db", "", "sqlite database path")
	fs.StringVar(&cfg.RootContract, "root-contract", "", "root contract id to verify")
	fs.StringVar(&cfg.TeamID, "team-id", "", "team id")
	fs.StringVar(&teamVersion, "team-version", "", "team version")
	fs.StringVar(&cfg.TeamConfig, "teamconfig", "", "TeamConfig YAML path")
	fs.StringVar(&cfg.ListenAddr, "listen", "", "listen address")
	fs.StringVar(&cfg.BackendURL, "backend-url", "", "backend URL")
	fs.StringVar(&cfg.CoordlinkPath, "coordlink", "", "coordlink binary path")
	fs.StringVar(&cfg.DockerNetwork, "docker-network", "", "Docker network")
	fs.StringVar(&cfg.ClaudeBinary, "claude-bin", "", "Claude binary path")
	fs.StringVar(&claudeEnv, "claude-env", "", "comma-separated Claude env allowlist")
	fs.StringVar(&cfg.WorkDir, "workdir", "", "workdir")
	fs.StringVar(&cfg.RunLabel, "run-label", "", "run label")
	fs.StringVar(&cfg.CreatedBy, "created-by", "", "created by")
	fs.StringVar(&inspectOut, "inspect-out", "", "write inspect JSON to path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	version, err := parseOptionalInt(teamVersion, "team-version")
	if err != nil {
		return err
	}
	cfg.TeamVersion = version
	cfg.ClaudeEnvKeys = splitCSV(claudeEnv)
	ctx := commandContext()
	result, runErr := releasehealth.RunCPAccept001(ctx, cfg)
	if inspectOut != "" && result.Inspect != nil {
		if err := writeJSONFile(inspectOut, result.Inspect); err != nil && runErr == nil {
			runErr = err
		}
	}
	if err := json.NewEncoder(stdout).Encode(result.Acceptance); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func runCPProbe001(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release-health cp-probe-001", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg releasehealth.CPProbe001Config
	var teamVersion string
	var dockerTeamVersion string
	var claudeEnv string
	fs.StringVar(&cfg.DBPath, "db", "", "sqlite database path")
	fs.StringVar(&cfg.TeamID, "team-id", "", "team id")
	fs.StringVar(&teamVersion, "team-version", "", "team version")
	fs.StringVar(&cfg.TeamConfig, "teamconfig", "", "TeamConfig YAML path")
	fs.StringVar(&cfg.DockerTeamID, "docker-team-id", "", "Docker team id")
	fs.StringVar(&dockerTeamVersion, "docker-team-version", "", "Docker team version")
	fs.StringVar(&cfg.DockerTeamConfig, "docker-teamconfig", "", "Docker TeamConfig YAML path")
	fs.StringVar(&cfg.ListenAddr, "listen", "", "listen address")
	fs.StringVar(&cfg.BackendURL, "backend-url", "", "backend URL")
	fs.StringVar(&cfg.CoordlinkPath, "coordlink", "", "coordlink binary path")
	fs.StringVar(&cfg.DockerNetwork, "docker-network", "", "Docker network")
	fs.StringVar(&cfg.ClaudeBinary, "claude-bin", "", "Claude binary path")
	fs.StringVar(&claudeEnv, "claude-env", "", "comma-separated Claude env allowlist")
	fs.StringVar(&cfg.WorkDir, "workdir", "", "workdir")
	fs.StringVar(&cfg.ArtifactDir, "artifact-dir", "", "artifact dir")
	fs.StringVar(&cfg.RuntimeWorkspaceRoot, "runtime-workspace-root", "", "runtime workspace root")
	fs.StringVar(&cfg.RuntimeHomeRoot, "runtime-home-root", "", "runtime home root")
	fs.StringVar(&cfg.EnvironmentBlocker, "environment-blocker", "", "environment blocker text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	version, err := parseOptionalInt(teamVersion, "team-version")
	if err != nil {
		return err
	}
	dockerVersion, err := parseOptionalInt(dockerTeamVersion, "docker-team-version")
	if err != nil {
		return err
	}
	cfg.TeamVersion = version
	cfg.DockerTeamVersion = dockerVersion
	cfg.ClaudeEnvKeys = splitCSV(claudeEnv)
	result, runErr := releasehealth.RunCPProbe001(commandContext(), cfg)
	if err := json.NewEncoder(stdout).Encode(result); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func parseOptionalInt(value, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	out, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("--%s must be an integer", name)
	}
	return out, nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeJSONFile(path string, value any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(value); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeEvidenceFromWaitResponse(path string, response typedResponse) error {
	var data struct {
		Evidence json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return fmt.Errorf("decode operator wait evidence: %w", err)
	}
	if len(data.Evidence) == 0 {
		return errors.New("operator task wait response missing evidence")
	}
	return writeRawJSONFile(path, data.Evidence)
}

func writeRawJSONFile(path string, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("operator evidence is not valid JSON")
	}
	return os.WriteFile(path, append(append([]byte(nil), raw...), '\n'), 0o644)
}

func commandContext() context.Context {
	return context.Background()
}
