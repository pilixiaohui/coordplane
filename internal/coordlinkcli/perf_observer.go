//go:build perf

package coordlinkcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/perfobs"
)

type perfObservedClient struct {
	next   jsonClient
	output io.Writer
	mu     sync.Mutex
}

func withPerfObserver(client jsonClient, output io.Writer) jsonClient {
	return &perfObservedClient{next: client, output: output}
}

func (c *perfObservedClient) JSON(ctx context.Context, method, path string, input, output any) error {
	started := time.Now()
	err := c.next.JSON(ctx, method, path, input, output)
	record := map[string]any{
		"schema_version": 1, "record_type": "client", "request_id": requestID(input),
		"task_id": nil, "run_id": nil, "operation": operationName(method, path),
		"duration_ns": time.Since(started).Nanoseconds(), "result": "success",
	}
	if err != nil {
		record["result"] = "error"
	}
	switch value := output.(type) {
	case *core.Event:
		record["task_id"], record["run_id"] = nullable(value.EntityID), nullable(value.RunID)
	case *core.Message:
		record["task_id"] = nullable(value.TaskID)
	case *core.OutcomeResult:
		record["task_id"], record["run_id"] = nullable(value.Task.ID), nullable(value.Run.ID)
	}
	raw, marshalErr := json.Marshal(record)
	if marshalErr == nil {
		c.mu.Lock()
		_, _ = fmt.Fprintf(c.output, "%s%s\n", perfobs.ClientPrefix, raw)
		c.mu.Unlock()
	}
	return err
}

func (c *perfObservedClient) CloseIdleConnections() { c.next.CloseIdleConnections() }

func requestID(input any) any {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	value, _ := fields["request_id"].(string)
	return nullable(value)
}

func operationName(method, path string) string {
	return strings.ToLower(method) + " " + strings.TrimPrefix(path, "/v1/")
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
