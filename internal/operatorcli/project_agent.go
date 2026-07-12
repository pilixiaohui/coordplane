package operatorcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"coordplane/internal/core"
)

func runProject(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("project subcommand is required")
	}
	switch args[0] {
	case "add":
		flags, cfg := clientFlags("project add", getenv, stderr)
		var input core.AddProjectInput
		flags.StringVar(&input.Name, "name", "", "project name")
		flags.StringVar(&input.Source, "repo", "", "local source repository")
		flags.StringVar(&input.SourceRef, "ref", "", "full source branch ref")
		flags.StringVar(&input.IntegrationAgentID, "integration-agent", "", "default integration agent")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		var project core.Project
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/projects", input, &project); err != nil {
			return err
		}
		return render(stdout, cfg.output, project)
	case "list":
		flags, cfg := clientFlags("project list", getenv, stderr)
		cursor := flags.String("cursor", "", "opaque next-page cursor")
		limit := flags.Int("limit", 0, "maximum projects (default and maximum 100)")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if _, err := core.NormalizeCompactPageLimit(*limit); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "cursor", *cursor)
		if *limit > 0 {
			query.Set("limit", fmt.Sprint(*limit))
		}
		var page core.ProjectPage
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/projects", query), nil, &page); err != nil {
			return err
		}
		return render(stdout, cfg.output, page)
	case "show":
		flags, cfg := clientFlags("project show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var project core.ProjectDetail
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/projects/"+url.PathEscape(id), nil, &project); err != nil {
			return err
		}
		return render(stdout, cfg.output, project)
	case "repair", "archive":
		return runProjectAction(ctx, args[0], args[1:], getenv, stdout, stderr, clients)
	default:
		return fmt.Errorf("unknown project subcommand %q", args[0])
	}
}

func runProjectAction(ctx context.Context, action string, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("project "+action, getenv, stderr)
	requestID := flags.String("request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}
	var project core.Project
	path := "/v1/projects/" + url.PathEscape(id) + "/" + action
	if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &project); err != nil {
		return err
	}
	return render(stdout, cfg.output, project)
}

func runAgent(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("agent subcommand is required")
	}
	switch args[0] {
	case "add":
		flags, cfg := clientFlags("agent add", getenv, stderr)
		var input core.AddAgentInput
		flags.StringVar(&input.ID, "id", "", "stable agent ID")
		flags.StringVar(&input.DisplayName, "display-name", "", "agent display name")
		flags.StringVar(&input.AdapterID, "adapter", "", "CLI adapter ID")
		flags.StringVar(&input.Image, "image", "", "runtime image")
		flags.StringVar(&input.InstructionsFile, "instructions-file", "", "instructions file")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		var agent core.Agent
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/agents", input, &agent); err != nil {
			return err
		}
		return render(stdout, cfg.output, agent)
	case "list":
		flags, cfg := clientFlags("agent list", getenv, stderr)
		cursor := flags.String("cursor", "", "opaque next-page cursor")
		limit := flags.Int("limit", 0, "maximum agents (default and maximum 100)")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if _, err := core.NormalizeCompactPageLimit(*limit); err != nil {
			return err
		}
		query := url.Values{}
		addQuery(query, "cursor", *cursor)
		if *limit > 0 {
			query.Set("limit", fmt.Sprint(*limit))
		}
		var page core.AgentPage
		if err := request(ctx, *cfg, clients, http.MethodGet, withQuery("/v1/agents", query), nil, &page); err != nil {
			return err
		}
		return render(stdout, cfg.output, page)
	case "show":
		flags, cfg := clientFlags("agent show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var agent core.Agent
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/agents/"+url.PathEscape(id), nil, &agent); err != nil {
			return err
		}
		return render(stdout, cfg.output, agent)
	case "pause", "resume", "archive":
		return runAgentAction(ctx, args[0], args[1:], getenv, stdout, stderr, clients)
	default:
		return fmt.Errorf("unknown agent subcommand %q", args[0])
	}
}

func runAgentAction(ctx context.Context, action string, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("agent "+action, getenv, stderr)
	requestID := flags.String("request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}
	var agent core.Agent
	path := "/v1/agents/" + url.PathEscape(id) + "/" + action
	if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &agent); err != nil {
		return err
	}
	return render(stdout, cfg.output, agent)
}
