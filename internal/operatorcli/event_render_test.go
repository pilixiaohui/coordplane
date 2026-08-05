package operatorcli

import (
	"bytes"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestEventPageHumanRenderIncludesContinuationCursor(t *testing.T) {
	page := core.EventPage{
		Items: []core.Event{
			{ID: 41, Kind: "task.created", EntityType: "task", EntityID: "task-1"},
			{ID: 42, Kind: "run.started", EntityType: "run", EntityID: "run-1"},
		},
		NextCursor: "event-cursor",
	}
	var output bytes.Buffer
	if err := render(&output, "human", page); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind": "task.created"`, `"kind": "run.started"`, `"next_cursor": "event-cursor"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human event page omitted %s: %s", want, output.String())
		}
	}
}
