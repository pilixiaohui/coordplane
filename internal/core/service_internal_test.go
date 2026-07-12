package core

import (
	"reflect"
	"testing"
	"time"
)

func TestServiceTimestampsRemainLexicallySortable(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC),
		time.Date(2026, 7, 12, 1, 2, 3, 1, time.UTC),
		time.Date(2026, 7, 12, 1, 2, 3, 100_000_000, time.UTC),
	}
	formatted := make([]string, 0, len(instants))
	for _, instant := range instants {
		service := &Service{now: func() time.Time { return instant }}
		formatted = append(formatted, service.nowText())
	}
	for index := 1; index < len(formatted); index++ {
		if len(formatted[index]) != len(formatted[0]) || formatted[index-1] >= formatted[index] {
			t.Fatalf("timestamps are not fixed-width sortable: %q", formatted)
		}
	}
}

func TestRunnableTasksUseCanonicalPriorityCreatedAndIDOrder(t *testing.T) {
	tasks := []Task{
		{ID: "tsk-later", Priority: 10, CreatedAt: "2026-07-12T00:00:01.000000000Z"},
		{ID: "tsk-z", Priority: 10, CreatedAt: "2026-07-12T00:00:00.000000000Z"},
		{ID: "tsk-a", Priority: 10, CreatedAt: "2026-07-12T00:00:00.000000000Z"},
		{ID: "tsk-high", Priority: 11, CreatedAt: "2026-07-12T00:00:02.000000000Z"},
	}

	sortRunnable(tasks)
	got := make([]string, 0, len(tasks))
	for _, task := range tasks {
		got = append(got, task.ID)
	}
	want := []string{"tsk-high", "tsk-a", "tsk-z", "tsk-later"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runnable order = %v, want %v", got, want)
	}
}
