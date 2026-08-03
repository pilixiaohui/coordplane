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

func runRole(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("role subcommand is required")
	}
	switch args[0] {
	case "create":
		flags, cfg := clientFlags("role create", getenv, stderr)
		var input core.RoleInput
		var capabilities stringListFlag
		flags.StringVar(&input.Name, "name", "", "role name")
		flags.StringVar(&input.Description, "description", "", "role description")
		flags.Var(&capabilities, "capability", "capability granted by the role; repeat for multiple capabilities")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input.Capabilities = append([]string(nil), capabilities...)
		var role core.Role
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/roles", input, &role); err != nil {
			return err
		}
		return render(stdout, cfg.output, role)
	case "update":
		flags, cfg := clientFlags("role update", getenv, stderr)
		var input core.RoleUpdateInput
		var capabilities stringListFlag
		flags.StringVar(&input.Name, "name", "", "new role name")
		flags.StringVar(&input.Description, "description", "", "new role description")
		flags.Var(&capabilities, "capability", "capability granted by the role; repeat for multiple capabilities")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		input.RoleID = id
		input.Capabilities = append([]string(nil), capabilities...)
		var role core.Role
		if err := request(ctx, *cfg, clients, http.MethodPut, "/v1/roles/"+url.PathEscape(id), input, &role); err != nil {
			return err
		}
		return render(stdout, cfg.output, role)
	case "delete":
		flags, cfg := clientFlags("role delete", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		if err := request(ctx, *cfg, clients, http.MethodDelete, "/v1/roles/"+url.PathEscape(id), actionRequest{RequestID: *requestID}, nil); err != nil {
			return err
		}
		return render(stdout, cfg.output, struct {
			Deleted string `json:"deleted"`
		}{id})
	case "list":
		flags, cfg := clientFlags("role list", getenv, stderr)
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		var roles []core.Role
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/roles", nil, &roles); err != nil {
			return err
		}
		return render(stdout, cfg.output, roles)
	case "show":
		flags, cfg := clientFlags("role show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var role core.Role
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/roles/"+url.PathEscape(id), nil, &role); err != nil {
			return err
		}
		return render(stdout, cfg.output, role)
	default:
		return fmt.Errorf("unknown role subcommand %q", args[0])
	}
}

func runParticipant(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("participant subcommand is required")
	}
	switch args[0] {
	case "list":
		flags, cfg := clientFlags("participant list", getenv, stderr)
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		var participants []core.Participant
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/participants", nil, &participants); err != nil {
			return err
		}
		return render(stdout, cfg.output, participants)
	case "show":
		flags, cfg := clientFlags("participant show", getenv, stderr)
		id, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var participant core.Participant
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/participants/"+url.PathEscape(id), nil, &participant); err != nil {
			return err
		}
		return render(stdout, cfg.output, participant)
	case "bind":
		flags, cfg := clientFlags("participant bind", getenv, stderr)
		var input core.BindRoleInput
		flags.StringVar(&input.ProjectID, "project", "", "project scope; use \"global\" for management capabilities")
		flags.StringVar(&input.RoleID, "role", "", "role ID")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		participantID, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		input.ParticipantID = participantID
		var binding core.ParticipantRoleBinding
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/participants/"+url.PathEscape(participantID)+"/roles", input, &binding); err != nil {
			return err
		}
		return render(stdout, cfg.output, binding)
	case "unbind":
		flags, cfg := clientFlags("participant unbind", getenv, stderr)
		var input core.BindRoleInput
		flags.StringVar(&input.ProjectID, "project", "", "project scope; use \"global\" for management capabilities")
		flags.StringVar(&input.RoleID, "role", "", "role ID")
		flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
		participantID, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		input.ParticipantID = participantID
		if err := request(ctx, *cfg, clients, http.MethodDelete, "/v1/participants/"+url.PathEscape(participantID)+"/roles", input, nil); err != nil {
			return err
		}
		return render(stdout, cfg.output, struct {
			Unbound string `json:"unbound"`
		}{participantID})
	default:
		return fmt.Errorf("unknown participant subcommand %q", args[0])
	}
}
