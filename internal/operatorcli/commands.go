package operatorcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"coordplane/internal/core"
)

func runChat(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("chat", getenv, stderr)
	input := core.ChatInput{Wake: true}
	var acknowledged stringListFlag
	flags.StringVar(&input.ProjectID, "project", "", "project ID")
	flags.StringVar(&input.AgentID, "agent", "", "recipient agent ID")
	flags.StringVar(&input.Body, "body", "", "message body")
	flags.StringVar(&input.RelatedTask, "related-task", "", "related task ID")
	flags.StringVar(&input.ReplyTo, "reply-to", "", "message being replied to")
	flags.BoolVar(&input.Wake, "wake", true, "ensure the conversation is queued")
	flags.Var(&acknowledged, "ack-message", "message ID to acknowledge atomically; repeat for multiple messages")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	if err := parseNoPositionals(flags, args); err != nil {
		return err
	}
	input.AckMessageIDs = append([]string(nil), acknowledged...)
	var result core.ChatResult
	if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/chat", input, &result); err != nil {
		return err
	}
	return render(stdout, cfg.output, result)
}

func runMessage(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("message subcommand is required")
	}
	switch args[0] {
	case "send":
		flags, cfg := clientFlags("message send", getenv, stderr)
		var input core.BossMessageInput
		var acknowledged stringListFlag
		flags.StringVar(&input.ProjectID, "project", "", "project ID")
		flags.StringVar(&input.AgentID, "agent", "", "recipient Agent ID")
		flags.StringVar(&input.TaskID, "task", "", "delivery Task ID")
		flags.StringVar(&input.RelatedTaskID, "related-task", "", "related Task ID")
		flags.StringVar(&input.Body, "body", "", "message body")
		flags.BoolVar(&input.Wake, "wake", false, "ensure the recipient obtains a Run")
		flags.StringVar(&input.ReplyTo, "reply-to", "", "message being replied to")
		flags.Var(&acknowledged, "ack-message", "message ID to acknowledge atomically; repeat for multiple messages")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input.AckMessageIDs = append([]string(nil), acknowledged...)
		var message core.Message
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/messages", input, &message); err != nil {
			return err
		}
		return render(stdout, cfg.output, message)
	case "list":
		flags, cfg := clientFlags("message list", getenv, stderr)
		var filter core.MessageFilter
		flags.StringVar(&filter.ProjectID, "project", "", "project ID")
		flags.StringVar(&filter.TaskID, "task", "", "task ID")
		flags.StringVar(&filter.RecipientKind, "recipient-kind", "", "recipient kind")
		flags.StringVar(&filter.RecipientID, "recipient-id", "", "recipient ID")
		flags.StringVar(&filter.Cursor, "cursor", "", "opaque next-page cursor")
		flags.IntVar(&filter.Limit, "limit", 0, "maximum messages (default and maximum 20)")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if _, err := core.NormalizeMessagePageLimit(filter.Limit); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "project_id", filter.ProjectID)
		addQuery(query, "task_id", filter.TaskID)
		addQuery(query, "recipient_kind", filter.RecipientKind)
		addQuery(query, "recipient_id", filter.RecipientID)
		addQuery(query, "cursor", filter.Cursor)
		if filter.Limit > 0 {
			query.Set("limit", fmt.Sprint(filter.Limit))
		}
		var messages core.MessagePage
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/messages", query), nil, &messages); err != nil {
			return err
		}
		return render(stdout, cfg.output, messages)
	case "read":
		flags, cfg := clientFlags("message read", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var message core.Message
		path := "/v1/messages/" + url.PathEscape(id) + "/read"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &message); err != nil {
			return err
		}
		return render(stdout, cfg.output, message)
	case "ack":
		flags, cfg := clientFlags("message ack", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var message core.Message
		path := "/v1/messages/" + url.PathEscape(id) + "/ack"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &message); err != nil {
			return err
		}
		return render(stdout, cfg.output, message)
	case "retry":
		flags, cfg := clientFlags("message retry", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var message core.Message
		path := "/v1/messages/" + url.PathEscape(id) + "/retry"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &message); err != nil {
			return err
		}
		return render(stdout, cfg.output, message)
	default:
		return fmt.Errorf("unknown message subcommand %q", args[0])
	}
}

func runTask(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("task subcommand is required")
	}
	switch args[0] {
	case "create":
		flags, cfg := clientFlags("task create", getenv, stderr)
		var input core.CreateTaskInput
		kind := flags.String("kind", string(core.TaskWork), "task kind")
		var acknowledged stringListFlag
		flags.StringVar(&input.ProjectID, "project", "", "project ID")
		flags.StringVar(&input.AssigneeAgentID, "agent", "", "assignee agent ID")
		flags.StringVar(&input.Title, "title", "", "task title")
		flags.StringVar(&input.Description, "description", "", "task description")
		flags.IntVar(&input.Priority, "priority", 0, "task priority")
		flags.IntVar(&input.MaxRetries, "max-retries", 0, "runtime retry limit")
		flags.Var(&acknowledged, "ack-message", "message ID to acknowledge atomically; repeat for multiple messages")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input.Kind = core.TaskKind(strings.TrimSpace(*kind))
		input.AckMessageIDs = append([]string(nil), acknowledged...)
		var task core.Task
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/tasks", input, &task); err != nil {
			return err
		}
		return render(stdout, cfg.output, task)
	case "list":
		flags, cfg := clientFlags("task list", getenv, stderr)
		var filter core.TaskFilter
		flags.StringVar(&filter.ProjectID, "project", "", "filter by project ID")
		flags.StringVar(&filter.Cursor, "cursor", "", "opaque next-page cursor")
		flags.IntVar(&filter.Limit, "limit", 0, "maximum tasks (default 100, maximum 500)")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if _, err := core.NormalizePageLimit(filter.Limit); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "project_id", filter.ProjectID)
		addQuery(query, "cursor", filter.Cursor)
		if filter.Limit > 0 {
			query.Set("limit", fmt.Sprint(filter.Limit))
		}
		var page core.TaskPage
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/tasks", query), nil, &page); err != nil {
			return err
		}
		return render(stdout, cfg.output, page)
	case "show":
		flags, cfg := clientFlags("task show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var task core.TaskDetail
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/tasks/"+url.PathEscape(id), nil, &task); err != nil {
			return err
		}
		return render(stdout, cfg.output, task)
	case "close":
		flags, cfg := clientFlags("task close", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var task core.Task
		path := "/v1/tasks/" + url.PathEscape(id) + "/close"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &task); err != nil {
			return err
		}
		return render(stdout, cfg.output, task)
	case "wake", "retry", "cancel", "rework":
		return runTaskAction(ctx, args[0], args[1:], getenv, stdout, stderr, clients)
	case "accept":
		return runTaskAccept(ctx, args[1:], getenv, stdout, stderr, clients)
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

func runTaskAccept(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("task accept", getenv, stderr)
	var input core.AcceptInput
	var acknowledged stringListFlag
	flags.StringVar(&input.IntegrationAgentID, "integration-agent", "", "integration agent ID")
	flags.Var(&acknowledged, "ack-message", "message ID to acknowledge atomically; repeat for multiple messages")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}
	input.AckMessageIDs = append([]string(nil), acknowledged...)
	var task core.Task
	path := "/v1/tasks/" + url.PathEscape(id) + "/accept"
	if err := request(ctx, *cfg, clients, http.MethodPost, path, input, &task); err != nil {
		return err
	}
	return render(stdout, cfg.output, task)
}

func runTaskAction(ctx context.Context, action string, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("task "+action, getenv, stderr)
	var input core.TaskActionInput
	var acknowledged stringListFlag
	flags.StringVar(&input.Reason, "reason", "", "action reason")
	flags.Var(&acknowledged, "ack-message", "message ID to acknowledge atomically; repeat for multiple messages")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}
	input.AckMessageIDs = append([]string(nil), acknowledged...)
	var task core.Task
	path := "/v1/tasks/" + url.PathEscape(id) + "/" + action
	if err := request(ctx, *cfg, clients, http.MethodPost, path, input, &task); err != nil {
		return err
	}
	return render(stdout, cfg.output, task)
}

func runRun(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("run subcommand is required")
	}
	switch args[0] {
	case "list":
		flags, cfg := clientFlags("run list", getenv, stderr)
		var filter core.RunFilter
		flags.StringVar(&filter.ProjectID, "project", "", "filter by project ID")
		flags.StringVar(&filter.TaskID, "task", "", "filter by task ID")
		flags.StringVar(&filter.AgentID, "agent", "", "filter by agent ID")
		flags.StringVar(&filter.Cursor, "cursor", "", "opaque next-page cursor")
		flags.IntVar(&filter.Limit, "limit", 0, "maximum runs (default 100, maximum 500)")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if _, err := core.NormalizePageLimit(filter.Limit); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "project_id", filter.ProjectID)
		addQuery(query, "task_id", filter.TaskID)
		addQuery(query, "agent_id", filter.AgentID)
		addQuery(query, "cursor", filter.Cursor)
		if filter.Limit > 0 {
			query.Set("limit", fmt.Sprint(filter.Limit))
		}
		var page core.RunPage
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/runs", query), nil, &page); err != nil {
			return err
		}
		return render(stdout, cfg.output, page)
	case "show":
		flags, cfg := clientFlags("run show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var run core.Run
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &run); err != nil {
			return err
		}
		return render(stdout, cfg.output, run)
	case "stop":
		flags, cfg := clientFlags("run stop", getenv, stderr)
		var input core.RunStopInput
		flags.StringVar(&input.Reason, "reason", "", "stop reason")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var run core.Run
		path := "/v1/runs/" + url.PathEscape(id) + "/stop"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, input, &run); err != nil {
			return err
		}
		return render(stdout, cfg.output, run)
	default:
		return fmt.Errorf("unknown run subcommand %q", args[0])
	}
}

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("message ID must not be blank")
	}
	*values = append(*values, value)
	return nil
}

func runEvents(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 || args[0] != "tail" {
		return errors.New("events subcommand must be tail")
	}
	flags, cfg := clientFlags("events tail", getenv, stderr)
	var filter core.EventFilter
	flags.StringVar(&filter.ProjectID, "project", "", "project ID")
	flags.StringVar(&filter.EntityType, "entity-type", "", "entity type")
	flags.StringVar(&filter.EntityID, "entity-id", "", "entity ID")
	flags.StringVar(&filter.RunID, "run-id", "", "run ID")
	flags.StringVar(&filter.Cursor, "cursor", "", "opaque next-page cursor")
	flags.IntVar(&filter.Limit, "limit", 0, "maximum events (default 20, maximum 100)")
	if err := parseNoPositionals(flags, args[1:]); err != nil {
		return err
	}
	if _, err := core.NormalizeEventPageLimit(filter.Limit); err != nil {
		return err
	}
	query := url.Values{}
	addQuery(query, "project_id", filter.ProjectID)
	addQuery(query, "entity_type", filter.EntityType)
	addQuery(query, "entity_id", filter.EntityID)
	addQuery(query, "run_id", filter.RunID)
	addQuery(query, "cursor", filter.Cursor)
	if filter.Limit > 0 {
		query.Set("limit", fmt.Sprint(filter.Limit))
	}
	var page core.EventPage
	if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/events", query), nil, &page); err != nil {
		return err
	}
	return render(stdout, cfg.output, page)
}
