package operatorcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

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
	case "delete":
		return runProjectDelete(ctx, args[1:], getenv, stdout, stderr, clients)
	default:
		return fmt.Errorf("unknown project subcommand %q", args[0])
	}
}

// runProjectDelete permanently removes an archived project. The delete is a
// void operation (no project body is returned), so it posts the delete input
// with reason + request ID and reports success.
func runProjectDelete(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("project delete", getenv, stderr)
	var input core.ProjectDeleteInput
	flags.StringVar(&input.Reason, "reason", "", "deletion reason")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}
	path := "/v1/projects/" + url.PathEscape(id) + "/delete"
	if err := request(ctx, *cfg, clients, http.MethodPost, path, input, nil); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.output), "json") {
		return render(stdout, cfg.output, struct {
			OK bool `json:"ok"`
		}{OK: true})
	}
	_, err = fmt.Fprintf(stdout, "project %s deleted\n", id)
	return err
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
		var config agentConfigFlagValues
		flags.StringVar(&input.ID, "id", "", "stable agent ID")
		bindAgentConfigFlags(flags, &config)
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		agentConfig := agentConfigFromFlagValues(config)
		input.DisplayName = agentConfig.DisplayName
		input.AdapterID = agentConfig.AdapterID
		input.Image = agentConfig.Image
		input.InstructionsFile = agentConfig.InstructionsFile
		input.InstructionsText = agentConfig.InstructionsText
		input.Model = agentConfig.Model
		input.SubagentModel = agentConfig.SubagentModel
		input.BaseURL = agentConfig.BaseURL
		input.Effort = agentConfig.Effort
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
	case "update":
		return runAgentUpdate(ctx, args[1:], getenv, stdout, stderr, clients)
	case "pause", "resume", "archive":
		return runAgentAction(ctx, args[0], args[1:], getenv, stdout, stderr, clients)
	default:
		return fmt.Errorf("unknown agent subcommand %q", args[0])
	}
}

// agentConfigFlagValues is the one CLI flag set shared by agent add and agent
// update. It keeps the operator command surface aligned with the single
// core.AgentConfigInput used by the HTTP and frontend entry points.
type agentConfigFlagValues struct {
	displayName      string
	adapterID        string
	image            string
	instructionsFile string
	instructionsText string
	model            string
	subagentModel    string
	baseURL          string
	effort           string
}

func bindAgentConfigFlags(flags *flag.FlagSet, values *agentConfigFlagValues) {
	flags.StringVar(&values.displayName, "display-name", "", "agent display name")
	flags.StringVar(&values.adapterID, "adapter", "", "CLI adapter ID")
	flags.StringVar(&values.image, "image", "", "runtime image")
	flags.StringVar(&values.instructionsFile, "instructions-file", "", "daemon-host instructions file")
	flags.StringVar(&values.instructionsText, "instructions-text", "", "inline instructions text")
	flags.StringVar(&values.model, "model", "", "provider model override")
	flags.StringVar(&values.subagentModel, "subagent-model", "", "provider subagent model override")
	flags.StringVar(&values.baseURL, "base-url", "", "provider https base URL")
	flags.StringVar(&values.effort, "effort", "", "adapter-allowed reasoning effort")
}

func agentConfigFromFlagValues(values agentConfigFlagValues) core.AgentConfigInput {
	return core.AgentConfigInput{
		DisplayName:      values.displayName,
		AdapterID:        values.adapterID,
		Image:            values.image,
		InstructionsFile: values.instructionsFile,
		InstructionsText: values.instructionsText,
		Model:            values.model,
		SubagentModel:    values.subagentModel,
		BaseURL:          values.baseURL,
		Effort:           values.effort,
	}
}

// runAgentUpdate implements the GET-current + explicit-flag-overlay + PUT
// contract. Omitted flags preserve the current full configuration, while an
// explicitly supplied empty value clears the corresponding nullable field;
// the final result is still validated as a complete AgentConfigInput by the
// daemon. --version defaults to the value read from GET so ordinary edits do
// not have to discover and repeat it.
func runAgentUpdate(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	flags, cfg := clientFlags("agent update", getenv, stderr)
	var config agentConfigFlagValues
	var requestID string
	var version int64
	bindAgentConfigFlags(flags, &config)
	flags.Int64Var(&version, "version", 0, "expected current version; defaults to the GET result")
	flags.StringVar(&requestID, "request-id", "", "idempotency key")
	id, err := parseID(flags, args)
	if err != nil {
		return err
	}

	var current core.Agent
	if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/agents/"+url.PathEscape(id), nil, &current); err != nil {
		return err
	}
	input := core.UpdateAgentInput{
		ID:      id,
		Version: current.Version,
		AgentConfigInput: agentConfigFromFlagValues(agentConfigFlagValues{
			displayName: current.DisplayName, adapterID: current.AdapterID, image: current.Image,
			instructionsFile: current.InstructionsFile, instructionsText: current.InstructionsText,
			model: current.Model, subagentModel: current.SubagentModel,
			baseURL: current.BaseURL, effort: current.Effort,
		}),
		RequestID: requestID,
	}
	flags.Visit(func(flagValue *flag.Flag) {
		switch flagValue.Name {
		case "display-name":
			input.DisplayName = config.displayName
		case "adapter":
			input.AdapterID = config.adapterID
		case "image":
			input.Image = config.image
		case "instructions-file":
			input.InstructionsFile = config.instructionsFile
		case "instructions-text":
			input.InstructionsText = config.instructionsText
		case "model":
			input.Model = config.model
		case "subagent-model":
			input.SubagentModel = config.subagentModel
		case "base-url":
			input.BaseURL = config.baseURL
		case "effort":
			input.Effort = config.effort
		case "version":
			input.Version = version
		case "request-id":
			input.RequestID = requestID
		}
	})

	var updated core.Agent
	if err := request(ctx, *cfg, clients, http.MethodPut, "/v1/agents/"+url.PathEscape(id), input, &updated); err != nil {
		return err
	}
	return render(stdout, cfg.output, updated)
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
