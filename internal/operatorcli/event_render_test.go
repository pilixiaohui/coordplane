package operatorcli

import (
	"bytes"
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
	const want = "41\ttask.created\ttask:task-1\n42\trun.started\trun:run-1\nnext_cursor=event-cursor\n"
	if output.String() != want {
		t.Fatalf("event page render\n got: %q\nwant: %q", output.String(), want)
	}
}
