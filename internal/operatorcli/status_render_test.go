package operatorcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"coordplane/internal/core"
)

func TestHumanRenderIsReadableFormOfCanonicalResult(t *testing.T) {
	status := core.Status{
		DaemonReady: true, SummaryTruncated: true,
		Runtime: &core.RuntimeStatus{WorkspaceQuotaReason: "not enabled: bind mount", TmpfsLimitBytes: 64 << 20},
		Tasks:   []core.TaskView{{Task: core.TaskSummary{ID: "task-1", Title: "bounded", TitleTruncated: true}}},
	}
	var output bytes.Buffer
	if err := render(&output, "human", status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\n  \"daemon_ready\": true") || !strings.Contains(output.String(), `"summary_truncated": true`) {
		t.Fatalf("human output is not readable or omits truncation: %s", output.String())
	}
	var decoded core.Status
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || !decoded.DaemonReady || !decoded.SummaryTruncated {
		t.Fatalf("human output does not preserve the public result: %#v err=%v", decoded, err)
	}
}

func TestJSONRenderRemainsCanonical(t *testing.T) {
	value := core.TaskPage{Items: []core.TaskSummary{{ID: "task-1"}}, NextCursor: "cursor"}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	var output bytes.Buffer
	if err := render(&output, "json", value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("JSON render changed: got %s want %s", output.Bytes(), want)
	}
}
