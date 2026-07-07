package coordlinkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"coordplane/internal/capability"
)

type EnvFunc func(string) string

type commonConfig struct {
	BackendURL  string
	AgentID     string
	RuntimeID   string
	WorkspaceID string
	Token       string
	TenantID    string
}

func Run(ctx context.Context, args []string, getenv EnvFunc, stdin io.Reader, stdout, stderr io.Writer) int {
	if getenv == nil {
		getenv = os.Getenv
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "capability":
		return runCapability(ctx, args[1:], getenv, stdout, stderr)
	case "call":
		return runCall(ctx, args[1:], getenv, stdin, stdout, stderr)
	case "skill":
		return runSkill(ctx, args[1:], getenv, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "coordlink: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func runCapability(ctx context.Context, args []string, getenv EnvFunc, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "coordlink capability: missing subcommand")
		return 2
	}
	switch args[0] {
	case "list":
		cfg, ok := parseCommon("coordlink capability list", args[1:], getenv, stderr)
		if !ok {
			return 2
		}
		if err := cfg.validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		endpoint := strings.TrimRight(cfg.BackendURL, "/") + "/capabilities?agent_id=" + url.QueryEscape(cfg.AgentID)
		return do(ctx, http.MethodGet, endpoint, nil, cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "coordlink capability: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runCall(ctx context.Context, args []string, getenv EnvFunc, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "coordlink call: missing capability name")
		return 2
	}
	capabilityName := args[0]
	fs := flag.NewFlagSet("coordlink call", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := commonFromEnv(getenv)
	addCommonFlags(fs, &cfg)
	inputValue := fs.String("input", "{}", "JSON input object; use '-' to read stdin")
	inputFile := fs.String("input-file", "", "file containing JSON input object")
	scopeValue := fs.String("scope", "", "JSON scope object")
	leaseID := fs.String("lease-id", getenv("COORDPLANE_LEASE_ID"), "lease id to include in scope when --scope is omitted")
	traceID := fs.String("trace-id", getenv("COORDPLANE_TRACE_ID"), "trace id")
	idempotencyKey := fs.String("idempotency-key", "", "idempotency key")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	input, err := rawInput(*inputValue, *inputFile, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	scope, err := rawScope(*scopeValue, *leaseID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	call := capability.Call{
		CapabilityName: capabilityName,
		TraceID:        *traceID,
		IdempotencyKey: *idempotencyKey,
		Subject: capability.Subject{
			TenantID:    cfg.TenantID,
			Kind:        "agent",
			ID:          cfg.AgentID,
			AgentID:     cfg.AgentID,
			RuntimeID:   cfg.RuntimeID,
			WorkspaceID: cfg.WorkspaceID,
		},
		Scope: scope,
		Input: input,
	}
	body, err := json.Marshal(call)
	if err != nil {
		fmt.Fprintf(stderr, "coordlink call: marshal request: %v\n", err)
		return 2
	}
	endpoint := strings.TrimRight(cfg.BackendURL, "/") + "/call"
	return do(ctx, http.MethodPost, endpoint, body, cfg, stdout, stderr)
}

func runSkill(ctx context.Context, args []string, getenv EnvFunc, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "coordlink skill: missing subcommand")
		return 2
	}
	switch args[0] {
	case "list":
		cfg, ok := parseCommon("coordlink skill list", args[1:], getenv, stderr)
		if !ok {
			return 2
		}
		if err := cfg.validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		endpoint := strings.TrimRight(cfg.BackendURL, "/") + "/skills?agent_id=" + url.QueryEscape(cfg.AgentID)
		return do(ctx, http.MethodGet, endpoint, nil, cfg, stdout, stderr)
	case "read":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "coordlink skill read: missing skill name")
			return 2
		}
		name := args[1]
		cfg, ok := parseCommon("coordlink skill read", args[2:], getenv, stderr)
		if !ok {
			return 2
		}
		if err := cfg.validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		endpoint := strings.TrimRight(cfg.BackendURL, "/") + "/skills/" + url.PathEscape(name) + "?agent_id=" + url.QueryEscape(cfg.AgentID)
		return do(ctx, http.MethodGet, endpoint, nil, cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "coordlink skill: unknown subcommand %q\n", args[0])
		return 2
	}
}

func parseCommon(name string, args []string, getenv EnvFunc, stderr io.Writer) (commonConfig, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := commonFromEnv(getenv)
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return commonConfig{}, false
	}
	return cfg, true
}

func commonFromEnv(getenv EnvFunc) commonConfig {
	cfg := commonConfig{
		BackendURL:  getenv("COORDPLANE_BACKEND_URL"),
		AgentID:     getenv("COORDPLANE_AGENT_ID"),
		RuntimeID:   getenv("COORDPLANE_RUNTIME_ID"),
		WorkspaceID: getenv("COORDPLANE_WORKSPACE_ID"),
		Token:       getenv("COORDPLANE_TOKEN"),
		TenantID:    getenv("COORDPLANE_TENANT_ID"),
	}
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	return cfg
}

func addCommonFlags(fs *flag.FlagSet, cfg *commonConfig) {
	fs.StringVar(&cfg.BackendURL, "backend", cfg.BackendURL, "CoordPlane backend URL")
	fs.StringVar(&cfg.AgentID, "agent", cfg.AgentID, "agent id")
	fs.StringVar(&cfg.RuntimeID, "runtime", cfg.RuntimeID, "runtime id")
	fs.StringVar(&cfg.WorkspaceID, "workspace", cfg.WorkspaceID, "workspace id")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "agent token")
	fs.StringVar(&cfg.TenantID, "tenant", cfg.TenantID, "tenant id")
}

func (c commonConfig) validate() error {
	if c.BackendURL == "" {
		return fmt.Errorf("coordlink: backend URL is required via --backend or COORDPLANE_BACKEND_URL")
	}
	if c.AgentID == "" {
		return fmt.Errorf("coordlink: agent id is required via --agent or COORDPLANE_AGENT_ID")
	}
	return nil
}

func rawInput(inputValue, inputFile string, stdin io.Reader) (json.RawMessage, error) {
	var raw []byte
	var err error
	switch {
	case inputFile != "":
		raw, err = os.ReadFile(inputFile)
	case inputValue == "-":
		raw, err = io.ReadAll(stdin)
	default:
		raw = []byte(inputValue)
	}
	if err != nil {
		return nil, fmt.Errorf("coordlink call: read input: %w", err)
	}
	return rawObject(raw, "input")
}

func rawScope(scopeValue, leaseID string) (json.RawMessage, error) {
	if strings.TrimSpace(scopeValue) != "" {
		return rawObject([]byte(scopeValue), "scope")
	}
	if leaseID != "" {
		raw, err := json.Marshal(map[string]string{"lease_id": leaseID})
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	}
	return json.RawMessage(`{}`), nil
}

func rawObject(raw []byte, field string) (json.RawMessage, error) {
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte(`{}`)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("coordlink call: %s must be a JSON object: %w", field, err)
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(normalized), nil
}

func do(ctx context.Context, method, endpoint string, body []byte, cfg commonConfig, stdout, stderr io.Writer) int {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		fmt.Fprintf(stderr, "coordlink: create request: %v\n", err)
		return 1
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CoordPlane-Agent-ID", cfg.AgentID)
	if cfg.RuntimeID != "" {
		req.Header.Set("X-CoordPlane-Runtime-ID", cfg.RuntimeID)
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "coordlink: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "coordlink: read response: %v\n", err)
		return 1
	}
	if len(raw) > 0 {
		_, _ = stdout.Write(raw)
		if raw[len(raw)-1] != '\n' {
			_, _ = stdout.Write([]byte("\n"))
		}
	}
	var envelope struct {
		Status capability.Status `json:"status"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Status == "" {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return 0
		}
		return 1
	}
	switch envelope.Status {
	case capability.StatusAccepted:
		return 0
	case capability.StatusRejected:
		return 2
	default:
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  coordlink capability list [--backend URL] [--agent AGENT]")
	fmt.Fprintln(w, "  coordlink call CAPABILITY [--input JSON|-] [--input-file PATH] [--scope JSON] [--lease-id ID]")
	fmt.Fprintln(w, "  coordlink skill list [--backend URL] [--agent AGENT]")
	fmt.Fprintln(w, "  coordlink skill read NAME [--backend URL] [--agent AGENT]")
}
