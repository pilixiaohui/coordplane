package coordlinkcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"coordplane/internal/buildinfo"
	"coordplane/internal/core"
	"coordplane/internal/transport"
)

const (
	socketEnvironment = "COORDPLANE_RUN_SOCKET"
	tokenEnvironment  = "COORDPLANE_RUN_TOKEN"
)

type environment func(string) string

func Run(ctx context.Context, args []string, getenv environment, stdin io.Reader, stdout, stderr io.Writer) int {
	_ = stdin
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	if args[0] == "version" {
		if err := json.NewEncoder(stdout).Encode(buildinfo.Current()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return 0
	}
	if args[0] != "task" && args[0] != "progress" && args[0] != "message" {
		fmt.Fprintf(stderr, "unknown coordlink command %q\n", args[0])
		return 1
	}
	if err := validateSocketCommand(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client, err := scopedClient(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()

	var runErr error
	switch args[0] {
	case "task":
		runErr = runTask(ctx, client, args[1:], stdout, stderr)
	case "progress":
		runErr = runProgress(ctx, client, args[1:], stdout, stderr)
	case "message":
		runErr = runMessage(ctx, client, args[1:], stdout, stderr)
	default:
		runErr = fmt.Errorf("unknown coordlink command %q", args[0])
	}
	if runErr != nil {
		fmt.Fprintln(stderr, runErr)
		return 1
	}
	return 0
}

func runTask(ctx context.Context, client *transport.Client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("task subcommand is required")
	}
	switch args[0] {
	case "current":
		flags, output := outputFlags("task current", stderr)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var task core.Task
		if err := client.JSON(ctx, http.MethodGet, "/v1/task/current", nil, &task); err != nil {
			return err
		}
		return render(stdout, *output, task)
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

func validateSocketCommand(args []string) error {
	if args[0] != "task" {
		return nil
	}
	if len(args) == 1 {
		return errors.New("task subcommand is required")
	}
	if args[1] != "current" {
		return fmt.Errorf("unknown task subcommand %q", args[1])
	}
	return nil
}

func runProgress(ctx context.Context, client *transport.Client, args []string, stdout, stderr io.Writer) error {
	flags, output := outputFlags("progress", stderr)
	var input core.ProgressInput
	flags.StringVar(&input.Summary, "summary", "", "short progress summary")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var event core.Event
	if err := client.JSON(ctx, http.MethodPost, "/v1/progress", input, &event); err != nil {
		return err
	}
	return render(stdout, *output, event)
}

func runMessage(ctx context.Context, client *transport.Client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "send" {
		return errors.New("message subcommand must be send")
	}
	flags, output := outputFlags("message send", stderr)
	var input core.AgentMessageInput
	var toBoss bool
	flags.BoolVar(&toBoss, "to-boss", false, "send to Boss")
	flags.StringVar(&input.Body, "body", "", "message body")
	flags.StringVar(&input.ReplyTo, "reply-to", "", "message being replied to")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if !toBoss {
		return errors.New("P1 message send requires --to-boss")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var message core.Message
	if err := client.JSON(ctx, http.MethodPost, "/v1/message", input, &message); err != nil {
		return err
	}
	return render(stdout, *output, message)
}

func scopedClient(getenv environment) (*transport.Client, error) {
	socket := strings.TrimSpace(getenv(socketEnvironment))
	if socket == "" {
		return nil, fmt.Errorf("%s is required", socketEnvironment)
	}
	token := strings.TrimSpace(getenv(tokenEnvironment))
	if token == "" {
		return nil, fmt.Errorf("%s is required", tokenEnvironment)
	}
	return transport.NewUnixClient(socket, transport.WithBearerToken(token))
}

func outputFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "human", "human or json")
	return flags, output
}

func render(writer io.Writer, mode string, value any) error {
	if err := validateOutput(mode); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "json":
		return json.NewEncoder(writer).Encode(value)
	case "human", "":
		switch typed := value.(type) {
		case core.Task:
			_, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", typed.ID, typed.Status, typed.Title)
			return err
		case core.Message:
			_, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", typed.ID, typed.State, typed.Body)
			return err
		case core.Event:
			_, err := fmt.Fprintf(writer, "%d\t%s\t%s\n", typed.ID, typed.Kind, typed.EntityID)
			return err
		default:
			return json.NewEncoder(writer).Encode(value)
		}
	}
	return nil
}

func validateOutput(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "human", "json":
		return nil
	default:
		return errors.New("--output must be human or json")
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage:")
	fmt.Fprintln(writer, "  coordlink version")
	fmt.Fprintln(writer, "  coordlink task current [--output human|json]")
	fmt.Fprintln(writer, "  coordlink message send --to-boss --body TEXT [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink progress --summary TEXT [--request-id ID]")
}
