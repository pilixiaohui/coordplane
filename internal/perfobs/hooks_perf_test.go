//go:build perf

package perfobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	EndStage("runtime.cleanup", fields.RunID, "success")
	validClient := ClientPrefix + `{"schema_version":1,"record_type":"client","request_id":"request-1","task_id":null,"run_id":null,"operation":"post progress","duration_ns":2,"result":"success"}`
	ClientLine([]byte(validClient), fields)
	ClientLine([]byte(ClientPrefix+`{"schema_version":1,"record_type":"client","request_id":"negative","task_id":null,"run_id":null,"operation":"progress","duration_ns":-1,"result":"success"}`), Fields{})
	ClientLine([]byte(ClientPrefix+`{"schema_version":1,"record_type":"client","request_id":"unknown","task_id":null,"run_id":null,"operation":"progress","duration_ns":1,"result":"success","extra":true}`), Fields{})
	stopObserver(t)
	for index := 0; index < 2; index++ {
		startObserver(t)
		ClientLine([]byte(validClient), fields)
		StartStage("runtime.cleanup", fields.RunID, fields)
		EndStage("runtime.cleanup", fields.RunID, "success")
		stopObserver(t)
	}
	appendObserverRecord(t, path, map[string]any{
		"schema_version": float64(1), "record_type": "stage_start",
		"daemon_origin_id": "dead-origin", "sample_id": "sample-contract",
		"stage_id": "runtime.cleanup", "stage_key_sha256": "crashed-key",
		"attempt_index": float64(0), "start_offset_ns": float64(1),
		"start_unix_ns": float64(1), "run_id": fields.RunID,
	})
	startObserver(t)
	stopObserver(t)
	startObserver(t)
	EndStage("runtime.cleanup", "missing", "success")
	stopObserver(t)

	counts, results := map[string]int{}, map[string]int{}
	var attempts []int64
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
		if kind == "stage" && record["stage_id"] == "runtime.cleanup" {
			attempts = append(attempts, int64(record["attempt_index"].(float64)))
		}
		if kind == "stage_interrupted" && (record["daemon_origin_id"] != "dead-origin" ||
			record["interrupted_by_origin_id"] == "dead-origin" || record["interrupted_by_origin_id"] == "" ||
			record["result"] != "interrupted" || record["duration_ns"] != nil) {
			t.Fatalf("cross-origin interrupted stage = %#v", record)
		}
	}
	for _, kind := range []string{"point", "stage", "resource", "client"} {
		if counts[kind] == 0 {
			t.Fatalf("missing %s record: %v", kind, counts)
		}
	}
	if counts["client"] != 1 || counts["invalid"] != 4 || counts["stage_interrupted"] != 1 ||
		results["error"] != 1 || results["received"] != 1 {
		t.Fatalf("observer terminal counts=%v results=%v", counts, results)
	}
	if fmt.Sprint(attempts) != "[0 1 2]" {
		t.Fatalf("stage attempts across restart = %v, want [0 1 2]", attempts)
	}
}

func appendObserverRecord(t *testing.T, path string, record map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	requirePerfNoError(t, err)
	defer file.Close()
	requirePerfNoError(t, json.NewEncoder(file).Encode(record))
}

func startObserver(t *testing.T) {
	t.Helper()
	requirePerfNoError(t, Start(context.Background()))
}

func stopObserver(t *testing.T) {
	t.Helper()
	requirePerfNoError(t, Stop())
}

func observerRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	requirePerfNoError(t, err)
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		requirePerfNoError(t, json.Unmarshal(scanner.Bytes(), &record))
		records = append(records, record)
	}
	return records
}

func requirePerfNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
