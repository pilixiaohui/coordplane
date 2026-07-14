package operatorcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"coordplane/internal/core"
)

func runGC(
	ctx context.Context,
	args []string,
	getenv environment,
	stdout, stderr io.Writer,
	clients clientFactory,
) error {
	if len(args) == 0 {
		return errors.New("gc subcommand is required")
	}
	switch args[0] {
	case "preview":
		flags, cfg := clientFlags("gc preview", getenv, stderr)
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		var result core.GCPreview
		if err := request(ctx, *cfg, clients, http.MethodGet, "/v1/gc/preview", nil, &result); err != nil {
			return err
		}
		return render(stdout, cfg.output, result)
	case "run":
		flags, cfg := clientFlags("gc run", getenv, stderr)
		confirm := flags.Bool("confirm", false, "confirm safe derived GC")
		requestID := flags.String("request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		if !*confirm {
			return errors.New("gc run requires --confirm")
		}
		input := core.GCRunInput{Confirm: true, RequestID: strings.TrimSpace(*requestID)}
		var result core.GCRunResult
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/gc/run", input, &result); err != nil {
			return err
		}
		return render(stdout, cfg.output, result)
	case "discard-workspace":
		flags, cfg := clientFlags("gc discard-workspace", getenv, stderr)
		taskID := flags.String("task", "", "terminal task ID")
		fingerprint := flags.String("expected-fingerprint", "", "preview workspace fingerprint")
		requestID := flags.String("request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input := core.GCDiscardWorkspaceInput{
			TaskID: strings.TrimSpace(*taskID), ExpectedFingerprint: strings.TrimSpace(*fingerprint),
			RequestID: strings.TrimSpace(*requestID),
		}
		if input.TaskID == "" || input.ExpectedFingerprint == "" || input.RequestID == "" {
			return errors.New("discard-workspace requires --task, --expected-fingerprint, and --request-id")
		}
		var result core.GCDiscardResult
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/gc/discard-workspace", input, &result); err != nil {
			return err
		}
		return render(stdout, cfg.output, result)
	case "discard-task-ref":
		flags, cfg := clientFlags("gc discard-task-ref", getenv, stderr)
		taskID := flags.String("task", "", "terminal task ID")
		runID := flags.String("run", "", "captured Run ID")
		expectedSHA := flags.String("expected-sha", "", "expected task-ref SHA")
		requestID := flags.String("request-id", "", "idempotency key")
		if err := parseNoPositionals(flags, args[1:]); err != nil {
			return err
		}
		input := core.GCDiscardTaskRefInput{
			TaskID: strings.TrimSpace(*taskID), RunID: strings.TrimSpace(*runID),
			ExpectedSHA: strings.TrimSpace(*expectedSHA), RequestID: strings.TrimSpace(*requestID),
		}
		if input.TaskID == "" || input.RunID == "" || input.ExpectedSHA == "" || input.RequestID == "" {
			return errors.New("discard-task-ref requires --task, --run, --expected-sha, and --request-id")
		}
		var result core.GCDiscardResult
		if err := request(ctx, *cfg, clients, http.MethodPost, "/v1/gc/discard-task-ref", input, &result); err != nil {
			return err
		}
		return render(stdout, cfg.output, result)
	default:
		return fmt.Errorf("unknown gc subcommand %q", args[0])
	}
}
