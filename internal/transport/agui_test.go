package transport

import (
	"testing"

	"coordplane/internal/core"
)

func TestAguiEventPayload(t *testing.T) {
	cases := []struct {
		name   string
		event  core.Event
		wantOK bool
		want   string
	}{
		{
			name:   "run created projects to run_start",
			event:  core.Event{Kind: "run.created", RunID: "run_1", EntityType: "task", EntityID: "tsk_1", ActorID: "dev-a", CreatedAt: "t0"},
			wantOK: true, want: "run_start",
		},
		{
			name:   "run active projects to run_start",
			event:  core.Event{Kind: "run.active", RunID: "run_1", CreatedAt: "t0"},
			wantOK: true, want: "run_start",
		},
		{
			name:   "message created projects to text_message",
			event:  core.Event{Kind: "message.created", EntityID: "msg_1", EntityType: "message", RunID: "run_1", CreatedAt: "t0"},
			wantOK: true, want: "text_message",
		},
		{
			name:   "task progress projects to tool_call with summary",
			event:  core.Event{Kind: "task.progress", EntityID: "tsk_1", EntityType: "task", PayloadJSON: `{"summary":"完成 Phase 1"}`, CreatedAt: "t0"},
			wantOK: true, want: "tool_call",
		},
		{
			name:   "run exited projects to run_complete",
			event:  core.Event{Kind: "run.exited", RunID: "run_1", CreatedAt: "t0"},
			wantOK: true, want: "run_complete",
		},
		{
			name:   "task failed projects to run_complete",
			event:  core.Event{Kind: "task.failed", RunID: "run_1", CreatedAt: "t0"},
			wantOK: true, want: "run_complete",
		},
		{
			name:   "unrelated event is skipped",
			event:  core.Event{Kind: "task.created", CreatedAt: "t0"},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, ok := aguiEventPayload(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got := payload["type"]; got != tc.want {
				t.Fatalf("type = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAguiSummaryOf(t *testing.T) {
	event := core.Event{Kind: "task.progress", PayloadJSON: `{"summary":"完成 M3 领域核心","other":1}`}
	if got := summaryOf(event); got != "完成 M3 领域核心" {
		t.Fatalf("summary = %q", got)
	}
	empty := core.Event{Kind: "task.progress", PayloadJSON: `{}`}
	if got := summaryOf(empty); got != "" {
		t.Fatalf("empty summary = %q", got)
	}
}
