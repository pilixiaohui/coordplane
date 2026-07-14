package operatorcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"coordplane/internal/buildinfo"
	"coordplane/internal/core"
	"coordplane/internal/daemon"
	"coordplane/internal/transport"
)

const socketEnvironment = "COORDPLANE_OPERATOR_SOCKET"

type environment func(string) string

type jsonClient interface {
	JSON(context.Context, string, string, any, any) error
	CloseIdleConnections()
}

type clientFactory func(string) (jsonClient, error)
type daemonRunner func(context.Context, string) error

type clientConfig struct {
	socket string
	output string
}

type actionRequest struct {
	RequestID string `json:"request_id"`
}

func Run(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer) int {
	return run(ctx, args, getenv, stdout, stderr, func(socket string) (jsonClient, error) {
		return transport.NewUnixClient(socket)
	}, daemon.Run)
}

func run(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory, serve daemonRunner) int {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	var err error
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) != 1 {
			err = errors.New("help does not accept arguments")
		} else {
			printUsage(stdout)
			return 0
		}
	case "version":
		err = runVersion(args[1:], stdout)
	case "serve":
		err = runServe(ctx, args[1:], stderr, serve)
	case "status":
		err = runStatus(ctx, args[1:], getenv, stdout, stderr, clients)
	case "project":
		err = runProject(ctx, args[1:], getenv, stdout, stderr, clients)
	case "agent":
		err = runAgent(ctx, args[1:], getenv, stdout, stderr, clients)
	case "chat":
		err = runChat(ctx, args[1:], getenv, stdout, stderr, clients)
	case "message":
		err = runMessage(ctx, args[1:], getenv, stdout, stderr, clients)
	case "task":
		err = runTask(ctx, args[1:], getenv, stdout, stderr, clients)
	case "run":
		err = runRun(ctx, args[1:], getenv, stdout, stderr, clients)
	case "events":
		err = runEvents(ctx, args[1:], getenv, stdout, stderr, clients)
	case "gc":
		err = runGC(ctx, args[1:], getenv, stdout, stderr, clients)
	default:
		err = fmt.Errorf("unknown coordplane command %q", args[0])
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func runVersion(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("version does not accept arguments")
	}
	return json.NewEncoder(stdout).Encode(buildinfo.Current())
}

func runServe(ctx context.Context, args []string, stderr io.Writer, serve daemonRunner) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "configuration YAML path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve does not accept positional arguments")
	}
	if strings.TrimSpace(*configPath) == "" {
		return errors.New("--config is required")
	}
	return serve(ctx, strings.TrimSpace(*configPath))
}

func runStatus(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("status", getenv, stderr)
	projectID := flags.String("project", "", "filter by project ID")
	if err := parseNoPositionals(flags, args); err != nil {
		return err
	}
	status, err := fetchStatus(ctx, *cfg, clients, strings.TrimSpace(*projectID))
	if err != nil {
		return err
	}
	return render(stdout, cfg.output, status)
}

func clientFlags(name string, getenv environment, stderr io.Writer) (*flag.FlagSet, *clientConfig) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfg := &clientConfig{socket: strings.TrimSpace(getenv(socketEnvironment)), output: "human"}
	flags.StringVar(&cfg.socket, "socket", cfg.socket, "operator Unix socket")
	flags.StringVar(&cfg.output, "output", cfg.output, "human or json")
	return flags, cfg
}

func parseNoPositionals(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return nil
}

func parseID(flags *flag.FlagSet, args []string) (string, error) {
	leading := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		leading, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if leading != "" {
		if flags.NArg() != 0 {
			return "", errors.New("unexpected positional arguments")
		}
		if leading = strings.TrimSpace(leading); leading == "" {
			return "", errors.New("ID is required")
		}
		return leading, nil
	}
	if flags.NArg() != 1 {
		return "", errors.New("exactly one ID is required")
	}
	id := strings.TrimSpace(flags.Arg(0))
	if id == "" {
		return "", errors.New("ID is required")
	}
	return id, nil
}

func request(ctx context.Context, cfg clientConfig, clients clientFactory, method, path string, input, output any) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	client, err := clients(strings.TrimSpace(cfg.socket))
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return client.JSON(ctx, method, path, input, output)
}

func fetchStatus(ctx context.Context, cfg clientConfig, clients clientFactory, projectID string) (core.Status, error) {
	query := url.Values{}
	addQuery(query, "project_id", projectID)
	var status core.Status
	err := request(ctx, cfg, clients, http.MethodGet, withQuery("/v1/status", query), nil, &status)
	return status, err
}

func (c clientConfig) validate() error {
	if strings.TrimSpace(c.socket) == "" {
		return fmt.Errorf("--socket or %s is required", socketEnvironment)
	}
	switch strings.ToLower(strings.TrimSpace(c.output)) {
	case "human", "json":
		return nil
	default:
		return errors.New("--output must be human or json")
	}
}

func addQuery(values url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		values.Set(key, value)
	}
}

func withQuery(path string, values url.Values) string {
	if encoded := values.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage:")
	fmt.Fprintln(writer, "  coordplane version")
	fmt.Fprintln(writer, "  coordplane serve --config FILE")
	fmt.Fprintln(writer, "  coordplane status [--project ID] [--socket PATH] [--output human|json]")
	fmt.Fprintln(writer, "  coordplane project add|list|show|repair|archive ...")
	fmt.Fprintln(writer, "  coordplane agent add|list|show|pause|resume|archive ...")
	fmt.Fprintln(writer, "  coordplane chat --project ID --agent ID --body TEXT [--request-id ID]")
	fmt.Fprintln(writer, "  coordplane message send|list|read|ack|retry ...")
	fmt.Fprintln(writer, "  coordplane task create|list|show|checkout|wake|retry|cancel|accept|rework|close ...")
	fmt.Fprintln(writer, "  coordplane run list|show|stop ...")
	fmt.Fprintln(writer, "  coordplane events tail ...")
	fmt.Fprintln(writer, "  coordplane gc preview|run|discard-workspace|discard-task-ref ...")
}
