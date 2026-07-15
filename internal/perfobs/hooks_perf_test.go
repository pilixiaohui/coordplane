//go:build perf

package perfobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestObserverCommitSchemaAndRestartContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	t.Setenv(outputEnvironment, path)
	t.Setenv(sampleEnvironment, "sample-contract")
	fields := Fields{RequestID: "request-1", OperationID: "operation-1", ProjectID: "project-1", TaskID: "task-1", RunID: "run-1"}
	startObserver(t)
	Received("api.progress.received", fields, "received")
	if raw, _ := os.ReadFile(path); bytes.Contains(raw, []byte("api.progress.received")) {
		t.Fatal("received point escaped before durable commit")
	}
	FailedReceived("api.progress.received", fields)
	Received("api.progress.received", fields, "received")
	Point("core.progress.committed", fields, "success")
	StartStage("runtime.cleanup", fields.RunID, fields)
	EndStage("runtime.cleanup", fields.RunID, "success")
	validClient := ClientPrefix + `{"schema_version":1,"record_type":"client","request_id":"request-1","task_id":null,"run_id":null,"operation":"post progress","duration_ns":2,"result":"success"}`
	ClientLine([]byte(validClient), fields)
	ClientLine([]byte(ClientPrefix+`{"schema_version":1,"record_type":"client","request_id":"negative","task_id":null,"run_id":null,"operation":"progress","duration_ns":-1,"result":"success"}`), Fields{})
	ClientLine([]byte(ClientPrefix+`{"schema_version":1,"record_type":"client","request_id":"unknown","task_id":null,"run_id":null,"operation":"progress","duration_ns":1,"result":"success","extra":true}`), Fields{})
	stopObserver(t)
	for index := 0; index < 2; index++ {
		startObserver(t)
		ClientLine([]byte(validClient), fields)
		stopObserver(t)
	}

	counts, results := map[string]int{}, map[string]int{}
	for _, record := range observerRecords(t, path) {
		if record["schema_version"] != float64(1) {
			t.Fatalf("invalid record schema: %#v", record)
		}
		kind, _ := record["record_type"].(string)
		counts[kind]++
		if record["point_id"] == "api.progress.received" {
			result, _ := record["result"].(string)
			results[result]++
		}
	}
	for _, kind := range []string{"point", "stage", "resource", "client"} {
		if counts[kind] == 0 {
			t.Fatalf("missing %s record: %v", kind, counts)
		}
	}
	if counts["client"] != 1 || counts["invalid"] != 2 || results["error"] != 1 || results["received"] != 1 {
		t.Fatalf("observer terminal counts=%v results=%v", counts, results)
	}
}

func startObserver(t *testing.T) {
	t.Helper()
	if err := Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func stopObserver(t *testing.T) {
	t.Helper()
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
}

func observerRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}
