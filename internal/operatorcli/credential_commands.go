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

func runCredential(ctx context.Context, args []string, getenv environment, stdout, stderr io.Writer, clients clientFactory) error {
	if len(args) == 0 {
		return errors.New("credential subcommand is required")
	}
	switch args[0] {
	case "add":
		flags, cfg := clientFlags("credential add", getenv, stderr)
		input, err := credentialInput(flags, cfg, args[1:], "add")
		if err != nil {
			return err
		}
		var credential core.Credential
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/credentials", input, &credential); err != nil {
			return err
		}
		return render(stdout, cfg.output, credential)
	case "rotate":
		flags, cfg := clientFlags("credential rotate", getenv, stderr)
		input, err := credentialInput(flags, cfg, args[1:], "rotate")
		if err != nil {
			return err
		}
		var credential core.Credential
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/credentials/rotate", input, &credential); err != nil {
			return err
		}
		return render(stdout, cfg.output, credential)
	case "revoke":
		flags, cfg := clientFlags("credential revoke", getenv, stderr)
		requestID := flags.String("request-id", "", "idempotency key")
		participantID, err := parseID(flags, args[1:])
		if err != nil {
			return err
		}
		var credential core.Credential
		path := "/v1/credentials/" + url.PathEscape(participantID) + "/revoke"
		if err := request(ctx, *cfg, clients, http.MethodPost, path, actionRequest{RequestID: *requestID}, &credential); err != nil {
			return err
		}
		return render(stdout, cfg.output, credential)
	default:
		return fmt.Errorf("unknown credential subcommand %q", args[0])
	}
}

func credentialInput(flags *flag.FlagSet, cfg *clientConfig, args []string, name string) (core.AddCredentialInput, error) {
	var input core.AddCredentialInput
	kind := string(core.CredentialKindOperatorToken)
	flags.StringVar(&input.ParticipantID, "participant", "", "participant ID (default: the human owner)")
	flags.StringVar(&kind, "kind", string(core.CredentialKindOperatorToken), "credential kind")
	flags.StringVar(&input.Secret, "secret", "", "credential secret (kept client-side; only its hash is stored)")
	flags.StringVar(&input.RequestID, "request-id", "", "idempotency key")
	if err := parseNoPositionals(flags, args); err != nil {
		return input, err
	}
	if strings.TrimSpace(input.ParticipantID) == "" {
		input.ParticipantID = core.DefaultHumanParticipantID
	}
	input.Kind = core.CredentialKind(strings.TrimSpace(kind))
	return input, nil
}
