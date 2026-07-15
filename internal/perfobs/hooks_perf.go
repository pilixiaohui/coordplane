//go:build perf

package perfobs

import (
	"bufio"
	"context"
	"crypto/rand"
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
	offset, attempt int64
	fields          Fields
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
	observer.stages, observer.attempts = map[string]stageStart{}, map[string]int64{}
	observer.received, observer.clients = map[string]receivedPoint{}, loadClientSignatures(file)
	observer.samplerDone = make(chan struct{})
	observer.Unlock()
	emitResource()
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
	stageKey := id + "\x00" + key
	observer.Lock()
	if observer.file == nil {
		observer.Unlock()
		return
	}
	_, duplicate := observer.stages[stageKey]
	attempt := observer.attempts[stageKey]
	observer.attempts[stageKey]++
	observer.stages[stageKey] = stageStart{time.Since(observer.start).Nanoseconds(), attempt, fields}
	observer.Unlock()
	if duplicate {
		emitInvalid("overlapping stage attempt", stageKey)
	}
	emitResource()
}

func EndStage(id, key, result string) {
	stageKey := id + "\x00" + key
	observer.Lock()
	started, ok := observer.stages[stageKey]
	delete(observer.stages, stageKey)
	observer.Unlock()
	if !ok {
		return
	}
	record := baseRecord("stage", started.fields)
	record["stage_id"], record["attempt_index"] = id, started.attempt
	record["start_offset_ns"], record["duration_ns"], record["result"] = started.offset, offset()-started.offset, result
	emit(record)
	emitResource()
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

func loadClientSignatures(file *os.File) map[string]string {
	clients := map[string]string{}
	_, _ = file.Seek(0, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var client clientRecord
		if json.Unmarshal(scanner.Bytes(), &client) == nil && client.RecordType == "client" && client.RequestID != nil && client.SampleID != nil {
			clients[clientKey(*client.SampleID, *client.RequestID, client.Operation)] = fmt.Sprintf("%d\x00%s", client.DurationNS, client.Result)
		}
	}
	_, _ = file.Seek(0, 2)
	return clients
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
