package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"coordplane/internal/core"
)

func (c *runtimeController) cleanupRunLogs(ctx context.Context, before string) error {
	cursor := ""
	for {
		page, err := c.service.ListRuns(ctx, core.RunFilter{
			Cursor: cursor,
			Limit:  core.MaximumCompactPageLimit,
		})
		if err != nil {
			return fmt.Errorf("list Run logs for retention: %w", err)
		}
		for _, summary := range page.Items {
			if core.IsRunLive(summary.State) || summary.EndedAt == "" || summary.EndedAt > before {
				continue
			}
			run, err := c.service.Run(ctx, summary.ID)
			if err != nil {
				return fmt.Errorf("load Run log retention fact: %w", err)
			}
			if run.LogPath == "" {
				continue
			}
			want := filepath.Join(c.config.Runtime.LogRoot, run.ID, "run.log")
			if run.LogPath != want {
				return fmt.Errorf("Run %s log path violates retention ownership", run.ID)
			}
			if err := os.RemoveAll(filepath.Dir(want)); err != nil {
				return fmt.Errorf("remove retained Run log %s: %w", run.ID, err)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}
