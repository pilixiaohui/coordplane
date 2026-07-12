package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"coordplane/internal/ids"
)

type ServiceOptions struct {
	Now             func() time.Time
	NewID           func(string) (string, error)
	MaxParallelRuns int
}

type Service struct {
	repository Repository
	projectGit ProjectGit
	now        func() time.Time
	newID      func(string) (string, error)
	maxRuns    int

	readyMu     sync.RWMutex
	ready       bool
	readyReason string
}

func NewService(repository Repository, projectGit ProjectGit, options ServiceOptions) (*Service, error) {
	if repository == nil {
		return nil, NewError(CodeInvalidArgument, "repository is required", false)
	}
	if projectGit == nil {
		return nil, NewError(CodeInvalidArgument, "project Git controller is required", false)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewID == nil {
		options.NewID = ids.New
	}
	if options.MaxParallelRuns <= 0 {
		options.MaxParallelRuns = 1
	}
	return &Service{
		repository: repository,
		projectGit: projectGit,
		now:        options.Now,
		newID:      options.NewID,
		maxRuns:    options.MaxParallelRuns,
	}, nil
}

func (s *Service) SetReady(ready bool, reason string) {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	s.ready = ready
	s.readyReason = strings.TrimSpace(reason)
}

func (s *Service) Status(ctx context.Context, projectID string) (Status, error) {
	snapshot, err := s.repository.Snapshot(ctx, strings.TrimSpace(projectID))
	if err != nil {
		return Status{}, err
	}
	status := Status{Snapshot: snapshot}
	s.readyMu.RLock()
	status.DaemonReady = s.ready
	status.Reason = s.readyReason
	s.readyMu.RUnlock()
	actualByProject := make(map[string]GitState, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		gitState := GitState{ProjectID: project.ID, CanonicalRef: project.CanonicalRef}
		gitState.ActualSHA, err = s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
		if err != nil {
			gitState.Error = err.Error()
		}
		status.ActualRefs = append(status.ActualRefs, gitState)
		actualByProject[project.ID] = gitState
	}
	runsByID := make(map[string]Run, len(snapshot.Runs))
	for _, run := range snapshot.Runs {
		runsByID[run.ID] = run
	}
	messageCounts := make(map[string][2]int, len(snapshot.Tasks))
	for _, message := range snapshot.Messages {
		counts := messageCounts[message.TaskID]
		switch message.State {
		case MessagePending:
			counts[0]++
		case MessageDelivered:
			counts[1]++
		}
		messageCounts[message.TaskID] = counts
	}
	for _, task := range snapshot.Tasks {
		counts := messageCounts[task.ID]
		gitState := actualByProject[task.ProjectID]
		view := TaskView{
			Task: task, PendingMessageCount: counts[0], DeliveredMessageCount: counts[1],
			ActualCanonicalSHA: gitState.ActualSHA, ActualCanonicalError: gitState.Error,
			Stale:   task.BaseSHA != "" && gitState.ActualSHA != "" && task.BaseSHA != gitState.ActualSHA,
			Derived: true,
		}
		if current, ok := runsByID[task.CurrentRunID]; ok {
			currentCopy := current
			view.CurrentRun = &currentCopy
		}
		progress, progressErr := s.repository.Events(ctx, EventFilter{
			ProjectID: task.ProjectID, EntityType: "task", EntityID: task.ID, Kind: "task.progress", Limit: 1,
		})
		if progressErr != nil {
			return Status{}, progressErr
		}
		if len(progress) == 1 {
			progressCopy := progress[0]
			view.LatestProgress = &progressCopy
		}
		status.Tasks = append(status.Tasks, view)
	}
	return status, nil
}

func (s *Service) Project(ctx context.Context, id string) (Project, error) {
	return s.repository.Project(ctx, strings.TrimSpace(id))
}

func (s *Service) Agent(ctx context.Context, id string) (Agent, error) {
	return s.repository.Agent(ctx, strings.TrimSpace(id))
}

func (s *Service) Snapshot(ctx context.Context, projectID string) (Snapshot, error) {
	return s.repository.Snapshot(ctx, strings.TrimSpace(projectID))
}

func (s *Service) ListMessages(ctx context.Context, filter MessageFilter) ([]Message, error) {
	return s.repository.Messages(ctx, filter)
}

func (s *Service) ListEvents(ctx context.Context, filter EventFilter) ([]Event, error) {
	return s.repository.Events(ctx, filter)
}

func (s *Service) nowText() string {
	return s.now().UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (s *Service) requiredID(prefix string) (string, error) {
	id, err := s.newID(prefix)
	if err != nil {
		return "", WrapError(CodeInternal, "generate identifier", false, err)
	}
	if strings.TrimSpace(id) == "" {
		return "", NewError(CodeInternal, "identifier generator returned an empty value", false)
	}
	return id, nil
}

func (s *Service) requestID(value string) (string, error) {
	if value = strings.TrimSpace(value); value != "" {
		return value, nil
	}
	return s.requiredID("req")
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type dedupeResult struct {
	ID        string `json:"id"`
	RelatedID string `json:"related_id,omitempty"`
	InputHash string `json:"input_hash"`
}

func inputFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", WrapError(CodeInternal, "encode idempotency input", false, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func encodeDedupe(id, relatedID, inputHash string) ([]byte, error) {
	return json.Marshal(dedupeResult{ID: id, RelatedID: relatedID, InputHash: inputHash})
}

func decodeDedupe(raw []byte, inputHash string) (dedupeResult, error) {
	var result dedupeResult
	if err := json.Unmarshal(raw, &result); err != nil || result.ID == "" || result.InputHash == "" {
		return dedupeResult{}, NewError(CodeInternal, "invalid request dedupe record", false)
	}
	if result.InputHash != inputHash {
		return dedupeResult{}, NewError(CodeVersionConflict, "idempotency key was already used with different input", false)
	}
	return result, nil
}

func event(projectID, entityType, entityID, kind, actorKind, actorID, runID, requestID, operationID, payload, now string) Event {
	if strings.TrimSpace(payload) == "" {
		payload = "{}"
	}
	return Event{
		ProjectID: projectID, EntityType: entityType, EntityID: entityID, Kind: kind,
		ActorKind: actorKind, ActorID: actorID, RunID: runID, RequestID: requestID,
		OperationID: operationID, PayloadJSON: payload, CreatedAt: now,
	}
}

func eventPayload(fields map[string]any) string {
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(fmt.Sprintf("core: encode event payload: %v", err))
	}
	return string(raw)
}

func sortRunnable(tasks []Task) {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority > tasks[j].Priority
		}
		if tasks[i].CreatedAt != tasks[j].CreatedAt {
			return tasks[i].CreatedAt < tasks[j].CreatedAt
		}
		return tasks[i].ID < tasks[j].ID
	})
}

func requireText(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", NewError(CodeInvalidArgument, fmt.Sprintf("%s is required", name), false)
	}
	return value, nil
}
