//go:build perf

package perfobs

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	outputEnvironment = "COORDPLANE_PERF_OBSERVER_OUTPUT"
	sampleEnvironment = "COORDPLANE_PERF_SAMPLE_ID"
	faultEnvironment  = "COORDPLANE_PERF_FAULT"
	readyEnvironment  = "COORDPLANE_PERF_FAULT_READY"
)

type stageStart struct {
	offset, attempt, unixNS int64
	fields                  Fields
}

type receivedPoint struct {
	offset int64
	fields Fields
	result string
}

type clientRecord struct {
	SchemaVersion int     `json:"schema_version"`
	RecordType    string  `json:"record_type"`
	SampleID      *string `json:"sample_id,omitempty"`
	RequestID     *string `json:"request_id"`
	TaskID        *string `json:"task_id"`
	RunID         *string `json:"run_id"`
	Operation     string  `json:"operation"`
	DurationNS    int64   `json:"duration_ns"`
	Result        string  `json:"result"`
}

var observer struct {
	sync.Mutex
	file           *os.File
	start          time.Time
	origin, sample string
	stages         map[string]stageStart
	attempts       map[string]int64
	received       map[string]receivedPoint
	clients        map[string]string
	cancel         context.CancelFunc
	samplerDone    chan struct{}
}

func Start(ctx context.Context) error {
	path := strings.TrimSpace(os.Getenv(outputEnvironment))
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("perf observer output must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare perf observer output: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open perf observer output: %w", err)
	}
	origin, err := randomID()
	if err != nil {
		_ = file.Close()
		return err
	}
	observer.Lock()
	if observer.file != nil {
		observer.Unlock()
		_ = file.Close()
		return fmt.Errorf("perf observer already started")
	}
	sampleCtx, cancel := context.WithCancel(ctx)
	observer.file, observer.start, observer.origin = file, time.Now(), origin
	observer.sample, observer.cancel = strings.TrimSpace(os.Getenv(sampleEnvironment)), cancel
	observer.received = map[string]receivedPoint{}
	observer.clients, observer.attempts, observer.stages = loadObserverState(file)
	interrupted := observer.stages
	observer.stages = map[string]stageStart{}
	observer.samplerDone = make(chan struct{})
	observer.Unlock()
	emitResource()
	for stageKey, started := range interrupted {
		emitStage(stageKey, started, "interrupted")
	}
	go sampleResources(sampleCtx, observer.samplerDone)
	return nil
}

func Stop() error {
	observer.Lock()
	cancel, done := observer.cancel, observer.samplerDone
	observer.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	observer.Lock()
	openStages, pending := mapKeys(observer.stages), mapKeys(observer.received)
	observer.Unlock()
	for _, key := range openStages {
		emitInvalid("unterminated stage", key)
	}
	for _, key := range pending {
		emitInvalid("uncommitted received point", key)
	}
	observer.Lock()
	file := observer.file
	observer.file, observer.cancel, observer.samplerDone = nil, nil, nil
	observer.stages, observer.attempts, observer.received, observer.clients = nil, nil, nil, nil
	observer.Unlock()
	return file.Close()
}

func Received(id string, fields Fields, result string) {
	if fields.RequestID == "" {
		return
	}
	key := id + "\x00" + fields.RequestID
	observer.Lock()
	if observer.file == nil {
		observer.Unlock()
		return
	}
	_, duplicate := observer.received[key]
	observer.received[key] = receivedPoint{time.Since(observer.start).Nanoseconds(), fields, result}
	observer.Unlock()
	if duplicate {
		emitInvalid("duplicate received point", key)
	}
}

func FailedReceived(id string, fields Fields) {
	if received, ok := takeReceived(id, fields.RequestID); ok {
		emitPoint(id, received.fields, received.offset, "error")
	}
}

func Point(id string, fields Fields, result string) {
	if id == "core.progress.committed" {
		if received, ok := takeReceived("api.progress.received", fields.RequestID); ok {
			emitPoint("api.progress.received", fields, received.offset, received.result)
		}
	}
	emitPoint(id, fields, offset(), result)
}

func takeReceived(id, requestID string) (receivedPoint, bool) {
	if requestID == "" {
		return receivedPoint{}, false
	}
	key := id + "\x00" + requestID
	observer.Lock()
	received, ok := observer.received[key]
	delete(observer.received, key)
	observer.Unlock()
	return received, ok
}

func emitPoint(id string, fields Fields, at int64, result string) {
	record := baseRecord("point", fields)
	record["point_id"], record["mono_offset_ns"], record["result"] = id, at, result
	emit(record)
}

func StartStage(id, key string, fields Fields) {
	if key == "" {
		return
	}
	stageKey := observedStageKey(id, key)
	observer.Lock()
	if observer.file == nil {
		observer.Unlock()
		return
	}
	_, duplicate := observer.stages[stageKey]
	attempt := observer.attempts[stageKey]
	observer.attempts[stageKey]++
	started := stageStart{time.Since(observer.start).Nanoseconds(), attempt, time.Now().UnixNano(), fields}
	observer.stages[stageKey] = started
	observer.Unlock()
	if duplicate {
		emitInvalid("overlapping stage attempt", stageKey)
	}
	record := baseRecord("stage_start", fields)
	record["stage_id"], record["stage_key_sha256"], record["attempt_index"] = id, strings.Split(stageKey, "\x00")[1], attempt
	record["start_offset_ns"], record["start_unix_ns"] = started.offset, started.unixNS
	emit(record)
	emitResource()
}

func EndStage(id, key, result string) {
	stageKey := observedStageKey(id, key)
	observer.Lock()
	started, ok := observer.stages[stageKey]
	completed := observer.attempts[stageKey] > 0
	delete(observer.stages, stageKey)
	observer.Unlock()
	if !ok {
		if !completed {
			emitInvalid("stage end without start", stageKey)
		}
		return
	}
	emitStage(stageKey, started, result)
}

func emitStage(stageKey string, started stageStart, result string) {
	identity := strings.SplitN(stageKey, "\x00", 2)
	record := baseRecord("stage", started.fields)
	record["stage_id"], record["stage_key_sha256"], record["attempt_index"] = identity[0], identity[1], started.attempt
	duration := time.Now().UnixNano() - started.unixNS
	record["start_offset_ns"], record["start_unix_ns"], record["duration_ns"], record["result"] = started.offset, started.unixNS, duration, result
	emit(record)
	emitResource()
}

func EndOpenStages(key, result string, ids ...string) {
	for _, id := range ids {
		stageKey := observedStageKey(id, key)
		observer.Lock()
		_, open := observer.stages[stageKey]
		observer.Unlock()
		if open {
			EndStage(id, key, result)
		}
	}
}

func RuntimeLimit(fields Fields, memoryBytes, nanoCPUs, pids int64) {
	record := baseRecord("runtime_limit", fields)
	record["memory_bytes"], record["nano_cpus"], record["pids_limit"] = memoryBytes, nanoCPUs, pids
	emit(record)
}

func ClientLine(line []byte, fields Fields) {
	text := string(line)
	if !strings.HasPrefix(text, ClientPrefix) {
		return
	}
	var client clientRecord
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(text, ClientPrefix)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&client) != nil {
		emitInvalid("invalid client JSON", "")
		return
	}
	if client.SchemaVersion != 1 || client.RecordType != "client" || (client.RequestID != nil && *client.RequestID == "") ||
		client.Operation == "" || client.DurationNS < 0 || (client.Result != "success" && client.Result != "error") {
		emitInvalid("invalid client schema", "")
		return
	}
	record := baseRecord("client", fields)
	record["request_id"], record["task_id"], record["run_id"] = client.RequestID, client.TaskID, client.RunID
	record["operation"], record["duration_ns"], record["result"] = client.Operation, client.DurationNS, client.Result
	if fields.TaskID != "" {
		record["task_id"] = fields.TaskID
	}
	if fields.RunID != "" {
		record["run_id"] = fields.RunID
	}
	if client.RequestID != nil && seenClient(*client.RequestID, client.Operation, client.DurationNS, client.Result) {
		return
	}
	emit(record)
}

func seenClient(request, operation string, duration int64, result string) bool {
	signature := fmt.Sprintf("%d\x00%s", duration, result)
	observer.Lock()
	key := clientKey(observer.sample, request, operation)
	previous, exists := observer.clients[key]
	if !exists {
		observer.clients[key] = signature
	}
	observer.Unlock()
	if exists && previous != signature {
		emitInvalid("conflicting client replay", key)
	}
	return exists
}

func Fault(ctx context.Context, id string) error {
	if strings.TrimSpace(os.Getenv(faultEnvironment)) != id {
		return nil
	}
	ready := strings.TrimSpace(os.Getenv(readyEnvironment))
	if ready == "" {
		return fmt.Errorf("%s is required for perf fault %s", readyEnvironment, id)
	}
	if err := os.WriteFile(ready, []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func baseRecord(kind string, fields Fields) map[string]any {
	observer.Lock()
	origin, sample := observer.origin, observer.sample
	observer.Unlock()
	return map[string]any{
		"schema_version": 1, "record_type": kind, "daemon_origin_id": origin, "sample_id": nullable(sample),
		"request_id": nullable(fields.RequestID), "operation_id": nullable(fields.OperationID),
		"project_id": nullable(fields.ProjectID), "task_id": nullable(fields.TaskID),
		"run_id": nullable(fields.RunID), "message_id": nullable(fields.MessageID),
	}
}

func emitResource() {
	fds, _ := os.ReadDir("/proc/self/fd")
	record := baseRecord("resource", Fields{})
	record["mono_offset_ns"], record["rss_bytes"] = offset(), residentBytes()
	record["goroutines"], record["open_fds"] = runtime.NumGoroutine(), len(fds)
	emit(record)
}

func sampleResources(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			emitResource()
		case <-ctx.Done():
			return
		}
	}
}

func emitInvalid(reason, key string) {
	record := baseRecord("invalid", Fields{})
	record["reason"] = reason
	if key != "" {
		record["key"] = key
	}
	emit(record)
}

func loadObserverState(file *os.File) (map[string]string, map[string]int64, map[string]stageStart) {
	clients := map[string]string{}
	attempts := map[string]int64{}
	stages := map[string]stageStart{}
	_, _ = file.Seek(0, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record["record_type"] == "client" {
			request, requestOK := record["request_id"].(string)
			sample, sampleOK := record["sample_id"].(string)
			operation, operationOK := record["operation"].(string)
			duration, durationOK := record["duration_ns"].(float64)
			result, resultOK := record["result"].(string)
			if requestOK && sampleOK && operationOK && durationOK && resultOK {
				clients[clientKey(sample, request, operation)] = fmt.Sprintf("%d\x00%s", int64(duration), result)
			}
		}
		if record["record_type"] == "stage_start" || record["record_type"] == "stage" {
			id, idOK := record["stage_id"].(string)
			key, keyOK := record["stage_key_sha256"].(string)
			attempt, attemptOK := record["attempt_index"].(float64)
			stageKey := id + "\x00" + key
			if idOK && keyOK && attemptOK {
				attempts[stageKey] = max(attempts[stageKey], int64(attempt)+1)
				if record["record_type"] == "stage" {
					delete(stages, stageKey)
				} else {
					stages[stageKey] = stageStart{intRecord(record, "start_offset_ns"), int64(attempt), intRecord(record, "start_unix_ns"), fieldsRecord(record)}
				}
			}
		}
	}
	_, _ = file.Seek(0, 2)
	return clients, attempts, stages
}

func intRecord(record map[string]any, key string) int64 {
	value, _ := record[key].(float64)
	return int64(value)
}

func fieldsRecord(record map[string]any) Fields {
	text := func(key string) string { value, _ := record[key].(string); return value }
	return Fields{ProjectID: text("project_id"), TaskID: text("task_id"), RunID: text("run_id"),
		MessageID: text("message_id"), OperationID: text("operation_id"), RequestID: text("request_id")}
}

func observedStageKey(id, key string) string {
	sum := sha256.Sum256([]byte(key))
	return id + "\x00" + hex.EncodeToString(sum[:])
}

func residentBytes() int64 {
	raw, _ := os.ReadFile("/proc/self/statm")
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(fields[1], 10, 64)
	return pages * int64(os.Getpagesize())
}

func emit(record map[string]any) {
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	observer.Lock()
	defer observer.Unlock()
	if observer.file != nil {
		_, _ = observer.file.Write(append(raw, '\n'))
	}
}

func offset() int64 {
	observer.Lock()
	defer observer.Unlock()
	return time.Since(observer.start).Nanoseconds()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func clientKey(sample, request, operation string) string {
	return sample + "\x00" + request + "\x00" + operation
}

func randomID() (string, error) {
	var raw [16]byte
	_, err := rand.Read(raw[:])
	return hex.EncodeToString(raw[:]), err
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
