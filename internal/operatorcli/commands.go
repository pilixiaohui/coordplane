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
	flags.StringVar(&input.ProjectID, "project", "", "project ID")
	flags.StringVar(&input.AgentID, "agent", "", "recipient agent ID")
	flags.StringVar(&input.Body, "body", "", "message body")
	flags.StringVar(&input.RelatedTask, "related-task", "", "related task ID")
	flags.StringVar(&input.ReplyTo, "reply-to", "", "message being replied to")
	flags.BoolVar(&input.Wake, "wake", true, "ensure the conversation is queued")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	if err := parseNoPositionals(flags, args); err != nil {
		return err
	}
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
	case "list":
		flags, cfg := clientFlags("message list", getenv, stderr)
		var filter core.MessageFilter
		flags.StringVar(&filter.ProjectID, "project", "", "project ID")
		flags.StringVar(&filter.TaskID, "task", "", "task ID")
		flags.StringVar(&filter.RecipientKind, "recipient-kind", "", "recipient kind")
		flags.StringVar(&filter.RecipientID, "recipient-id", "", "recipient ID")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "project_id", filter.ProjectID)
		addQuery(query, "task_id", filter.TaskID)
		addQuery(query, "recipient_kind", filter.RecipientKind)
		addQuery(query, "recipient_id", filter.RecipientID)
		var messages []core.Message
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/messages", query), nil, &messages); err != nil {
			return err
		}
		return render(stdout, cfg.output, messages)
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
		flags.StringVar(&input.ProjectID, "project", "", "project ID")
		flags.StringVar(&input.AssigneeAgentID, "agent", "", "assignee agent ID")
		flags.StringVar(&input.Title, "title", "", "task title")
		flags.StringVar(&input.Description, "description", "", "task description")
		flags.IntVar(&input.Priority, "priority", 0, "task priority")
		flags.IntVar(&input.MaxRetries, "max-retries", 0, "runtime retry limit")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input.Kind = core.TaskKind(strings.TrimSpace(*kind))
		var task core.Task
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/tasks", input, &task); err != nil {
			return err
		}
		return render(stdout, cfg.output, task)
	case "list":
		flags, cfg := clientFlags("task list", getenv, stderr)
		projectID := flags.String("project", "", "filter by project ID")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		status, err := fetchStatus(ctx, *cfg, clients, strings.TrimSpace(*projectID))
		if err != nil {
			return err
		}
		return render(stdout, cfg.output, status.Snapshot.Tasks)
	case "show":
		flags, cfg := clientFlags("task show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		status, err := fetchStatus(ctx, *cfg, clients, "")
		if err != nil {
			return err
		}
		task, ok := findTaskView(status.Tasks, id)
		if !ok {
			return core.NewError(core.CodeNotFound, "task not found", false)
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
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
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
	flags.IntVar(&filter.Limit, "limit", 0, "maximum events")
	if err := parseNoPositionals(flags, args[1:]); err != nil {
		return err
	}
	if filter.Limit < 0 {
		return errors.New("--limit must be a non-negative integer")
	}
	query := url.Values{}
	addQuery(query, "project_id", filter.ProjectID)
	addQuery(query, "entity_type", filter.EntityType)
	addQuery(query, "entity_id", filter.EntityID)
	addQuery(query, "run_id", filter.RunID)
	if filter.Limit > 0 {
		query.Set("limit", fmt.Sprint(filter.Limit))
	}
	var events []core.Event
	if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/events", query), nil, &events); err != nil {
		return err
	}
	return render(stdout, cfg.output, events)
}
