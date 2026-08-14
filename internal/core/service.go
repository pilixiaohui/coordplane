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
	"unicode/utf8"

	"coordplane/internal/ids"
)

type ServiceOptions struct {
	Now                         func() time.Time
	NewID                       func(string) (string, error)
	MaxParallelRuns             int
	AdapterIDs                  []string
	Adapters                    []AdapterDescriptor
	CompletedWorkspaceRetention time.Duration
	TerminalTaskRefRetention    time.Duration
	AgentHomes                  AgentHomeGC
}

type Service struct {
	repository                  Repository
	projectGit                  ProjectGit
	now                         func() time.Time
	newID                       func(string) (string, error)
	maxRuns                     int
	adapters                    map[string]AdapterDescriptor
	completedWorkspaceRetention time.Duration
	terminalTaskRefRetention    time.Duration
	agentHomes                  AgentHomeGC

	readyMu       sync.RWMutex
	ready         bool
	readyReason   string
	runtimeStatus *RuntimeStatus
	gcMu          sync.Mutex
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
	if options.CompletedWorkspaceRetention < 0 || options.TerminalTaskRefRetention < 0 {
		return nil, NewError(CodeInvalidArgument, "GC retention durations cannot be negative", false)
	}
	adapters := make(map[string]AdapterDescriptor, len(options.AdapterIDs))
	for _, adapterID := range options.AdapterIDs {
		if adapterID == "" || strings.TrimSpace(adapterID) != adapterID {
			return nil, NewError(CodeInvalidArgument, "adapter IDs must be non-empty and canonical", false)
		}
		if _, duplicate := adapters[adapterID]; duplicate {
			return nil, NewError(CodeInvalidArgument, "adapter IDs must be unique", false)
		}
		adapters[adapterID] = AdapterDescriptor{ID: adapterID}
	}
	if len(adapters) == 0 {
		// AdapterIDs is retained as the compact legacy constructor; when only
		// descriptors are supplied they become the registry themselves.
		for _, descriptor := range options.Adapters {
			adapterID := strings.TrimSpace(descriptor.ID)
			if adapterID == "" || adapterID != descriptor.ID {
				return nil, NewError(CodeInvalidArgument, "adapter descriptors must have a canonical ID", false)
			}
			normalized, err := normalizeAdapterDescriptor(descriptor)
			if err != nil {
				return nil, err
			}
			if _, duplicate := adapters[adapterID]; duplicate {
				return nil, NewError(CodeInvalidArgument, "adapter IDs must be unique", false)
			}
			adapters[adapterID] = normalized
		}
	}
	if len(adapters) == 0 {
		return nil, NewError(CodeInvalidArgument, "at least one adapter ID is required", false)
	}
	for _, descriptor := range options.Adapters {
		adapterID := strings.TrimSpace(descriptor.ID)
		if adapterID == "" || adapterID != descriptor.ID {
			return nil, NewError(CodeInvalidArgument, "adapter descriptors must have a canonical ID", false)
		}
		if _, registered := adapters[adapterID]; !registered {
			return nil, NewError(CodeInvalidArgument, "adapter descriptor is not in the adapter ID registry", false)
		}
		normalized, err := normalizeAdapterDescriptor(descriptor)
		if err != nil {
			return nil, err
		}
		adapters[adapterID] = normalized
	}
	return &Service{
		repository:                  repository,
		projectGit:                  projectGit,
		now:                         options.Now,
		newID:                       options.NewID,
		maxRuns:                     options.MaxParallelRuns,
		adapters:                    adapters,
		completedWorkspaceRetention: options.CompletedWorkspaceRetention,
		terminalTaskRefRetention:    options.TerminalTaskRefRetention,
		agentHomes:                  options.AgentHomes,
	}, nil
}

func normalizeAdapterDescriptor(descriptor AdapterDescriptor) (AdapterDescriptor, error) {
	seen := make(map[string]struct{}, len(descriptor.AllowedEfforts))
	efforts := make([]string, 0, len(descriptor.AllowedEfforts))
	for _, effort := range descriptor.AllowedEfforts {
		if effort == "" || strings.TrimSpace(effort) != effort {
			return AdapterDescriptor{}, NewError(CodeInvalidArgument, "adapter allowed efforts must be non-empty and canonical", false)
		}
		if _, duplicate := seen[effort]; duplicate {
			return AdapterDescriptor{}, NewError(CodeInvalidArgument, "adapter allowed efforts must be unique", false)
		}
		seen[effort] = struct{}{}
		efforts = append(efforts, effort)
	}
	descriptor.AllowedEfforts = efforts
	return descriptor, nil
}

func (s *Service) SetReady(ready bool, reason string) {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	s.ready = ready
	s.readyReason = strings.TrimSpace(reason)
}

// RequireReady fences mutations until startup recovery has reconciled the
// durable ledger with the actual runtime and Git state.
func (s *Service) RequireReady() error {
	s.readyMu.RLock()
	ready := s.ready
	reason, _ := boundedStatusText(s.readyReason, 1024)
	s.readyMu.RUnlock()
	if ready {
		return nil
	}
	message := "daemon is not ready"
	if reason != "" {
		message += ": " + reason
	}
	return NewError(CodeRuntimeUnavailable, message, true)
}

func (s *Service) SetRuntimeStatus(status RuntimeStatus) {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	copy := status
	s.runtimeStatus = &copy
}

func (s *Service) Status(ctx context.Context, projectID string) (Status, error) {
	projection, err := s.repository.StatusProjection(ctx, strings.TrimSpace(projectID))
	if err != nil {
		return Status{}, err
	}
	status := Status{Snapshot: projection.Snapshot, Tasks: projection.Tasks, PendingBossMessages: projection.PendingBossMessages, SummaryTruncated: projection.Truncated}
	s.readyMu.RLock()
	status.DaemonReady = s.ready
	if s.runtimeStatus != nil {
		copy := *s.runtimeStatus
		status.Runtime = &copy
	}
	var truncated bool
	status.Reason, truncated = boundedStatusText(s.readyReason, 1024)
	status.SummaryTruncated = status.SummaryTruncated || truncated
	s.readyMu.RUnlock()
	actualByProject := make(map[string]GitState, len(status.Snapshot.Projects))
	for index := range status.Snapshot.Projects {
		project := &status.Snapshot.Projects[index]
		gitState := GitState{ProjectID: project.ID, CanonicalRef: project.CanonicalRef}
		gitState.ActualSHA, err = s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
		if err != nil {
			gitState.Error = err.Error()
		}
		gitState.ActualSHA, _ = boundedStatusText(gitState.ActualSHA, 256)
		gitState.Error, truncated = boundedStatusText(gitState.Error, 1024)
		status.SummaryTruncated = status.SummaryTruncated || truncated
		status.ActualRefs = append(status.ActualRefs, gitState)
		actualByProject[project.ID] = gitState
		for value, limit := range map[*string]int{
			&project.Name: 256, &project.Source: 2048, &project.SourceRef: 1024,
			&project.ControlRepoPath: 2048, &project.CanonicalRef: 1024, &project.LastError: 1024,
		} {
			*value, truncated = boundedStatusText(*value, limit)
			status.SummaryTruncated = status.SummaryTruncated || truncated
		}
	}
	for index := range status.Snapshot.Agents {
		agent := &status.Snapshot.Agents[index]
		for value, limit := range map[*string]int{
			&agent.DisplayName: 256, &agent.AdapterID: 256, &agent.Image: 1024, &agent.InstructionsFile: 2048,
		} {
			bounded, truncated := boundedStatusText(*value, limit)
			*value = bounded
			status.SummaryTruncated = status.SummaryTruncated || truncated
		}
	}
	for index := range status.Tasks {
		task := &status.Tasks[index]
		status.SummaryTruncated = status.SummaryTruncated || task.Task.TitleTruncated || task.Task.TextTruncated ||
			(task.CurrentRun != nil && task.CurrentRun.TextTruncated)
		gitState := actualByProject[task.Task.ProjectID]
		task.ActualCanonicalSHA = gitState.ActualSHA
		task.ActualCanonicalError = gitState.Error
		task.Stale = task.Task.BaseSHA != "" && gitState.ActualSHA != "" && task.Task.BaseSHA != gitState.ActualSHA
	}
	return status, nil
}

func (s *Service) Project(ctx context.Context, id string) (ProjectDetail, error) {
	project, err := s.repository.Project(ctx, strings.TrimSpace(id))
	if err != nil {
		return ProjectDetail{}, err
	}
	detail := ProjectDetail{Project: project}
	detail.ActualCanonicalSHA, err = s.projectGit.Resolve(ctx, project.ControlRepoPath, project.CanonicalRef)
	if err != nil {
		detail.ActualCanonicalSHA = ""
		detail.ActualCanonicalError, _ = boundedStatusText(err.Error(), 1024)
	} else {
		detail.ActualCanonicalSHA, _ = boundedStatusText(detail.ActualCanonicalSHA, 256)
	}
	return detail, nil
}

func (s *Service) Agent(ctx context.Context, id string) (Agent, error) {
	return s.repository.Agent(ctx, strings.TrimSpace(id))
}

func (s *Service) ListProjects(ctx context.Context, filter ProjectFilter) (ProjectPage, error) {
	return s.repository.Projects(ctx, filter)
}

func (s *Service) ListAgents(ctx context.Context, filter AgentFilter) (AgentPage, error) {
	return s.repository.Agents(ctx, filter)
}

func (s *Service) Task(ctx context.Context, id string) (TaskDetail, error) {
	projection, err := s.repository.TaskProjection(ctx, strings.TrimSpace(id))
	if err != nil {
		return TaskDetail{}, err
	}
	actual, resolveErr := s.projectGit.Resolve(ctx, projection.Project.ControlRepoPath, projection.Project.CanonicalRef)
	if resolveErr != nil {
		projection.Task.ActualCanonicalError, _ = boundedStatusText(resolveErr.Error(), 1024)
	} else {
		projection.Task.ActualCanonicalSHA, _ = boundedStatusText(actual, 256)
	}
	projection.Task.Stale = projection.Task.Task.BaseSHA != "" && projection.Task.ActualCanonicalSHA != "" && projection.Task.Task.BaseSHA != projection.Task.ActualCanonicalSHA
	return projection.Task, nil
}

func (s *Service) Run(ctx context.Context, id string) (Run, error) {
	return s.repository.Run(ctx, strings.TrimSpace(id))
}

func (s *Service) Snapshot(ctx context.Context, projectID string) (Snapshot, error) {
	return s.repository.Snapshot(ctx, strings.TrimSpace(projectID))
}

func (s *Service) ListTasks(ctx context.Context, filter TaskFilter) (TaskPage, error) {
	return s.repository.Tasks(ctx, filter)
}

func (s *Service) ListRuns(ctx context.Context, filter RunFilter) (RunPage, error) {
	return s.repository.Runs(ctx, filter)
}

func (s *Service) ListMessages(ctx context.Context, filter MessageFilter) (MessagePage, error) {
	return s.repository.Messages(ctx, filter)
}

func (s *Service) ListEvents(ctx context.Context, filter EventFilter) (EventPage, error) {
	limit, err := NormalizeEventPageLimit(filter.Limit)
	if err != nil {
		return EventPage{}, err
	}
	filter.Limit = limit
	return s.repository.EventsPage(ctx, filter)
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
		if len(value) > 256 {
			return "", NewError(CodeInvalidArgument, "request_id must not exceed 256 bytes", false)
		}
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
	maximum := 0
	switch name {
	case "body":
		maximum = MaximumMessageBodyBytes
	case "summary":
		maximum = MaximumProgressSummaryBytes
	case "name", "display_name":
		maximum = 256
	case "title":
		maximum = 512
	case "adapter_id":
		maximum = 256
	case "image", "source_ref":
		maximum = 1024
	case "source", "instructions_file":
		maximum = 2048
	}
	if maximum > 0 && len(value) > maximum {
		return "", NewError(CodeInvalidArgument, fmt.Sprintf("%s must not exceed %d bytes", name, maximum), false)
	}
	return value, nil
}

func boundedStatusText(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func boundedDurableText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	const suffix = "...[truncated]"
	value = value[:limit-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func optionalTextWithin(name, value string, maximum int) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		return "", NewError(CodeInvalidArgument, fmt.Sprintf("%s must not exceed %d bytes", name, maximum), false)
	}
	return value, nil
}
