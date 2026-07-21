package coordlinkcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"coordplane/internal/buildinfo"
	"coordplane/internal/core"
	"coordplane/internal/transport"
)

const (
	socketEnvironment    = "COORDPLANE_RUN_SOCKET"
	tokenFileEnvironment = "COORDPLANE_RUN_TOKEN_FILE"
	maxRunTokenBytes     = 4096
	runRetryMaxAttempts  = 20
	runRetryDelay        = 50 * time.Millisecond
)

type environment func(string) string

type jsonClient interface {
	JSON(context.Context, string, string, any, any) error
	CloseIdleConnections()
}

type retryingClient struct {
	next        jsonClient
	maxAttempts int
	delay       time.Duration
}

func (c *retryingClient) JSON(ctx context.Context, method, path string, input, output any) error {
	if c == nil || c.next == nil {
		return errors.New("coordlink: Unix client is not initialized")
	}
	attempts := c.maxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		err := c.next.JSON(ctx, method, path, input, output)
		if err == nil || !retryableRunRequest(err) || attempt == attempts || ctx.Err() != nil {
			return err
		}
		c.next.CloseIdleConnections()
		if c.delay <= 0 {
			continue
		}
		timer := time.NewTimer(c.delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
	return nil
}

func (c *retryingClient) CloseIdleConnections() {
	if c != nil && c.next != nil {
		c.next.CloseIdleConnections()
	}
}

func retryableRunRequest(err error) bool {
	return core.IsCode(err, core.CodeRunStarting) || core.IsCode(err, core.CodeRuntimeUnavailable) ||
		errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

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
	if args[0] != "task" && args[0] != "inbox" && args[0] != "progress" && args[0] != "message" {
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
	case "inbox":
		runErr = runInbox(ctx, client, args[1:], stdout, stderr)
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

func runTask(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
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
		var result core.CurrentTaskResult
		if err := client.JSON(ctx, http.MethodGet, "/v1/task/current", nil, &result); err != nil {
			return err
		}
		return render(stdout, *output, result)
	case "show":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return errors.New("task show requires a task ID")
		}
		taskID := strings.TrimSpace(args[1])
		flags, output := outputFlags("task show", stderr)
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var task core.Task
		if err := client.JSON(ctx, http.MethodGet, "/v1/task/"+url.PathEscape(taskID), nil, &task); err != nil {
			return err
		}
		return render(stdout, *output, task)
	case "create":
		flags, output := outputFlags("task create", stderr)
		var input core.CreateChildTaskInput
		var ackMessageIDs stringListFlag
		flags.StringVar(&input.AssigneeAgentID, "agent", "", "assignee Agent ID")
		flags.StringVar(&input.Title, "title", "", "short task title")
		flags.StringVar(&input.Description, "description", "", "task description")
		flags.IntVar(&input.Priority, "priority", 0, "task priority")
		flags.IntVar(&input.MaxRetries, "max-retries", 0, "runtime retry limit")
		flags.StringVar(&input.SourceTaskID, "source-task", "", "captured source task ID")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		flags.Var(&ackMessageIDs, "ack-message", "message ID to acknowledge atomically (repeatable)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		input.AckMessageIDs = ackMessageIDs
		var task core.Task
		if err := client.JSON(ctx, http.MethodPost, "/v1/task/create", input, &task); err != nil {
			return err
		}
		return render(stdout, *output, task)
	case "wait":
		return runOutcome(ctx, client, core.OutcomeWait, args[1:], stdout, stderr)
	case "submit":
		return runOutcome(ctx, client, core.OutcomeSubmit, args[1:], stdout, stderr)
	case "fail":
		return runOutcome(ctx, client, core.OutcomeFail, args[1:], stdout, stderr)
	case "accept":
		return runTaskAccept(ctx, client, args[1:], stdout, stderr)
	case "rework":
		return runTaskRework(ctx, client, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

func validateSocketCommand(args []string) error {
	switch args[0] {
	case "task":
		if len(args) == 1 {
			return errors.New("task subcommand is required")
		}
		switch args[1] {
		case "current", "show", "create", "wait", "submit", "fail", "accept", "rework":
			return nil
		default:
			return fmt.Errorf("unknown task subcommand %q", args[1])
		}
	case "inbox":
		if len(args) == 1 {
			return errors.New("inbox subcommand is required")
		}
		if args[1] != "list" && args[1] != "read" && args[1] != "ack" {
			return fmt.Errorf("unknown inbox subcommand %q", args[1])
		}
	}
	return nil
}

func runTaskAccept(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
	flags, output := outputFlags("task accept", stderr)
	var input core.AcceptInput
	var ackMessageIDs stringListFlag
	flags.StringVar(&input.IntegrationAgentID, "integration-agent", "", "integration Agent ID")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	flags.Var(&ackMessageIDs, "ack-message", "message ID to acknowledge atomically (repeatable)")
	taskID, err := parseTaskActionID(flags, args)
	if err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	input.AckMessageIDs = ackMessageIDs
	var task core.Task
	path := "/v1/task/" + url.PathEscape(taskID) + "/accept"
	if err := client.JSON(ctx, http.MethodPost, path, input, &task); err != nil {
		return err
	}
	return render(stdout, *output, task)
}

func runTaskRework(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
	flags, output := outputFlags("task rework", stderr)
	var input core.TaskActionInput
	var ackMessageIDs stringListFlag
	flags.StringVar(&input.Reason, "reason", "", "rework reason")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	flags.Var(&ackMessageIDs, "ack-message", "message ID to acknowledge atomically (repeatable)")
	taskID, err := parseTaskActionID(flags, args)
	if err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	input.AckMessageIDs = ackMessageIDs
	var task core.Task
	path := "/v1/task/" + url.PathEscape(taskID) + "/rework"
	if err := client.JSON(ctx, http.MethodPost, path, input, &task); err != nil {
		return err
	}
	return render(stdout, *output, task)
}

func parseTaskActionID(flags *flag.FlagSet, args []string) (string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || strings.TrimSpace(args[0]) == "" {
		return "", errors.New("task ID is required")
	}
	taskID := strings.TrimSpace(args[0])
	if err := flags.Parse(args[1:]); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected positional arguments")
	}
	return taskID, nil
}

func runOutcome(ctx context.Context, client jsonClient, outcome core.Outcome, args []string, stdout, stderr io.Writer) error {
	flags, output := outputFlags("task "+string(outcome), stderr)
	input := core.OutcomeInput{Outcome: outcome}
	var ackMessageIDs stringListFlag
	switch outcome {
	case core.OutcomeWait, core.OutcomeFail:
		flags.StringVar(&input.Reason, "reason", "", "outcome reason")
	case core.OutcomeSubmit:
		flags.StringVar(&input.Summary, "summary", "", "result summary")
		flags.StringVar(&input.ExpectedHead, "expected-head", "", "expected workspace HEAD")
	default:
		return fmt.Errorf("unsupported task outcome %q", outcome)
	}
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	flags.Var(&ackMessageIDs, "ack-message", "message ID to acknowledge atomically (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	input.AckMessageIDs = ackMessageIDs
	var result core.OutcomeResult
	if err := client.JSON(ctx, http.MethodPost, "/v1/task/outcome", input, &result); err != nil {
		return err
	}
	return render(stdout, *output, result)
}

func runProgress(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
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

func runInbox(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("inbox subcommand is required")
	}
	switch args[0] {
	case "list":
		flags, output := outputFlags("inbox list", stderr)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var messages []core.Message
		if err := client.JSON(ctx, http.MethodGet, "/v1/inbox", nil, &messages); err != nil {
			return err
		}
		return render(stdout, *output, messages)
	case "read":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "-") {
			return errors.New("inbox read requires a message ID")
		}
		messageID := strings.TrimSpace(args[1])
		flags, output := outputFlags("inbox read", stderr)
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var message core.Message
		if err := client.JSON(ctx, http.MethodGet, "/v1/inbox/"+url.PathEscape(messageID), nil, &message); err != nil {
			return err
		}
		return render(stdout, *output, message)
	case "ack":
		flags, output := outputFlags("inbox ack", stderr)
		var input core.AcknowledgeMessagesInput
		var messageIDs stringListFlag
		flags.Var(&messageIDs, "ack-message", "message ID to acknowledge (repeatable)")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		if len(messageIDs) == 0 {
			return errors.New("inbox ack requires at least one --ack-message")
		}
		input.MessageIDs = messageIDs
		var messages []core.Message
		if err := client.JSON(ctx, http.MethodPost, "/v1/inbox/ack", input, &messages); err != nil {
			return err
		}
		return render(stdout, *output, messages)
	default:
		return fmt.Errorf("unknown inbox subcommand %q", args[0])
	}
}

func runMessage(ctx context.Context, client jsonClient, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "send" {
		return errors.New("message subcommand must be send")
	}
	flags, output := outputFlags("message send", stderr)
	var input core.SendMessageInput
	var toBoss bool
	var toAgent string
	var ackMessageIDs stringListFlag
	flags.BoolVar(&toBoss, "to-boss", false, "send to Boss")
	flags.StringVar(&toAgent, "to-agent", "", "recipient Agent ID")
	flags.StringVar(&input.TaskID, "task", "", "delivery Task ID")
	flags.BoolVar(&input.Wake, "wake", false, "ensure the recipient obtains a Run")
	flags.StringVar(&input.Body, "body", "", "message body")
	flags.StringVar(&input.ReplyTo, "reply-to", "", "message being replied to")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	flags.Var(&ackMessageIDs, "ack-message", "message ID to acknowledge atomically (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	toAgent = strings.TrimSpace(toAgent)
	if toBoss == (toAgent != "") {
		return errors.New("exactly one of --to-boss or --to-agent is required")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if toBoss {
		input.RecipientKind = "boss"
	} else {
		input.RecipientKind = "agent"
		input.RecipientID = toAgent
	}
	input.AckMessageIDs = ackMessageIDs
	var message core.Message
	if err := client.JSON(ctx, http.MethodPost, "/v1/message", input, &message); err != nil {
		return err
	}
	return render(stdout, *output, message)
}

func scopedClient(getenv environment) (jsonClient, error) {
	socket := strings.TrimSpace(getenv(socketEnvironment))
	if socket == "" {
		return nil, fmt.Errorf("%s is required", socketEnvironment)
	}
	token, err := runToken(getenv)
	if err != nil {
		return nil, err
	}
	client, err := transport.NewUnixClient(socket, transport.WithBearerToken(token))
	if err != nil {
		return nil, err
	}
	return &retryingClient{next: client, maxAttempts: runRetryMaxAttempts, delay: runRetryDelay}, nil
}

func runToken(getenv environment) (string, error) {
	if path := strings.TrimSpace(getenv(tokenFileEnvironment)); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%s must be an absolute path", tokenFileEnvironment)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", tokenFileEnvironment, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, maxRunTokenBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return "", fmt.Errorf("read %s: %w", tokenFileEnvironment, readErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %s: %w", tokenFileEnvironment, closeErr)
		}
		if len(raw) > maxRunTokenBytes {
			return "", fmt.Errorf("%s exceeds %d bytes", tokenFileEnvironment, maxRunTokenBytes)
		}
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token, nil
		}
		return "", fmt.Errorf("%s is empty", tokenFileEnvironment)
	}
	return "", fmt.Errorf("%s is required", tokenFileEnvironment)
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
		case core.CurrentTaskResult:
			_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d unread\n", typed.Task.ID, typed.Task.Status, typed.Task.Title, typed.Run.ID, typed.UnreadMessageCount)
			return err
		case core.Message:
			_, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", typed.ID, typed.State, typed.Body)
			return err
		case []core.Message:
			for _, message := range typed {
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\n", message.ID, message.State, message.Body); err != nil {
					return err
				}
			}
			return nil
		case core.OutcomeResult:
			_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", typed.Task.ID, typed.Task.Status, typed.Run.ID, typed.Run.State)
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

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("message ID must not be empty")
	}
	*values = append(*values, value)
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
	fmt.Fprintln(writer, "  coordlink task show ID [--output human|json]")
	fmt.Fprintln(writer, "  coordlink task create --agent A --title TEXT [--description TEXT] [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink task wait --reason TEXT [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink task submit --summary TEXT --expected-head SHA [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink task fail --reason TEXT [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink task accept ID [--integration-agent A] [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink task rework ID [--reason TEXT] [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink inbox list [--output human|json]")
	fmt.Fprintln(writer, "  coordlink inbox read ID [--output human|json]")
	fmt.Fprintln(writer, "  coordlink inbox ack --ack-message ID [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink message send (--to-boss | --to-agent A) [--task T] [--wake] --body TEXT [--request-id ID]")
	fmt.Fprintln(writer, "  coordlink progress --summary TEXT [--request-id ID]")
}
