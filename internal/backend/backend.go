package backend

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"coordplane/internal/adapters/httpapi"
	"coordplane/internal/capability"
	"coordplane/internal/claudeenv"
	"coordplane/internal/codemanagement"
	"coordplane/internal/commandrun"
	"coordplane/internal/coordination"
	"coordplane/internal/delivery"
	"coordplane/internal/ids"
	"coordplane/internal/objects"
	operator "coordplane/internal/operator"
	"coordplane/internal/policy"
	"coordplane/internal/queue"
	"coordplane/internal/releaseacceptance"
	cpruntime "coordplane/internal/runtime"
	"coordplane/internal/secrets"
	"coordplane/internal/sessionauth"
	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
	"coordplane/internal/validation"

	_ "modernc.org/sqlite"
)

const defaultTeamID = "default-go-team"

type Config struct {
	DBPath               string
	ListenAddr           string
	TeamConfigPath       string
	TeamID               string
	BackendURL           string
	RuntimeWorkspaceRoot string
	RuntimeHomeRoot      string
	DockerNetwork        string
	CoordlinkPath        string
	ClaudeBinary         string
	ClaudeEnvKeys        []string
	ClaudeStartArgs      []string
	ClaudeResumeArgs     []string
	ClaudeTimeout        time.Duration
	OperatorToken        string
	OperatorTokenEnv     string
	OperatorSubjectID    string
}

type Backend struct {
	DB                       *sql.DB
	Store                    *store.Store
	Queue                    *queue.Queue
	Capabilities             *capability.Registry
	Skills                   *skills.Registry
	TeamConfig               teamconfig.Config
	TeamConfigLoaded         bool
	Dispatcher               *policy.Dispatcher
	Coordination             *coordination.Service
	CommandRun               *commandrun.Service
	Validation               *validation.Service
	ReleaseAcceptance        *releaseacceptance.Service
	CodeManagement           *codemanagement.Service
	Delivery                 *delivery.Service
	OperatorTasks            *operator.Service
	Runner                   *cpruntime.Runner
	Runtime                  cpruntime.RuntimeBackend
	RuntimeRegistry          []RuntimeEntry
	RuntimeInstances         []cpruntime.RuntimeInstance
	CLISessions              []cpruntime.CLISession
	CommandRuns              []commandrun.Run
	ValidationAssessments    []validation.Assessment
	ReleaseAcceptanceSummary releaseacceptance.Summary
	CLIAdapterRegistry       []CLIAdapterEntry
	Handler                  http.Handler
	Migrations               []string
	StartedAt                time.Time
	dbPath                   string
	listenAddr               string
	authenticator            *sessionauth.Authenticator
	operatorToken            string
	operatorSubjectID        string
}

type RuntimeEntry struct {
	ID            string `json:"id"`
	Profile       string `json:"profile,omitempty"`
	Kind          string `json:"kind"`
	Ready         bool   `json:"ready"`
	WorkspaceRoot string `json:"workspace_root"`
	HomeRoot      string `json:"home_root"`
}

type CLIAdapterEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Ready bool   `json:"ready"`
}

type Inspect struct {
	Status                string                      `json:"status"`
	DBPath                string                      `json:"db_path"`
	ListenAddr            string                      `json:"listen_addr"`
	StartedAt             time.Time                   `json:"started_at"`
	Migrations            []string                    `json:"migrations"`
	Counts                map[string]int64            `json:"counts"`
	Capabilities          []string                    `json:"capabilities"`
	BuiltinSkills         []string                    `json:"builtin_skills"`
	TeamConfigLoaded      bool                        `json:"team_config_loaded"`
	TeamID                string                      `json:"team_id,omitempty"`
	TeamVersion           int                         `json:"team_version,omitempty"`
	QueueInitialized      bool                        `json:"queue_initialized"`
	RunnerInitialized     bool                        `json:"runner_initialized"`
	RuntimeRegistry       []RuntimeEntry              `json:"runtime_registry"`
	RuntimeInstances      []cpruntime.RuntimeInstance `json:"runtime_instances,omitempty"`
	CLISessions           []cpruntime.CLISession      `json:"cli_sessions"`
	CommandRuns           []commandrun.Run            `json:"command_runs"`
	ValidationAssessments []validation.Assessment     `json:"validation_assessments"`
	ReleaseAcceptance     releaseacceptance.Summary   `json:"release_acceptance"`
	CLIAdapterRegistry    []CLIAdapterEntry           `json:"cli_adapter_registry"`
	CapabilityRegistry    bool                        `json:"capability_registry_initialized"`
	SkillRegistry         bool                        `json:"skill_registry_initialized"`
	SQLiteFileBacked      bool                        `json:"sqlite_file_backed"`
	AcceptanceGateState   string                      `json:"acceptance_gate_state"`
}

type auditedDispatcher struct {
	db    *sql.DB
	inner *policy.Dispatcher
}

func newAuditedDispatcher(db *sql.DB, inner *policy.Dispatcher) *auditedDispatcher {
	return &auditedDispatcher{db: db, inner: inner}
}

func (d *auditedDispatcher) Handle(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	response := d.inner.Handle(ctx, call)
	if err := d.record(ctx, call, response); err != nil {
		return capability.Error[json.RawMessage]("CAPABILITY_CALL_AUDIT_FAILED", err.Error(), true)
	}
	return response
}

func (d *auditedDispatcher) ListForSubject(ctx context.Context, subject capability.Subject) capability.Response[json.RawMessage] {
	response := d.inner.ListForSubject(ctx, subject)
	call := capability.Call{
		CapabilityName: "capability.list",
		Subject:        subject,
		Scope:          json.RawMessage(`{}`),
	}
	if err := d.record(ctx, call, response); err != nil {
		return capability.Error[json.RawMessage]("CAPABILITY_CALL_AUDIT_FAILED", err.Error(), true)
	}
	return response
}

func (d *auditedDispatcher) record(ctx context.Context, call capability.Call, response capability.Response[json.RawMessage]) error {
	if d == nil || d.db == nil {
		return errors.New("audit dispatcher has no database")
	}
	id, err := ids.New("capcall")
	if err != nil {
		return err
	}
	traceID := call.TraceID
	if traceID == "" {
		traceID = id
	}
	subjectKind := call.Subject.Kind
	if subjectKind == "" {
		subjectKind = "agent"
	}
	subjectID := firstNonEmpty(call.Subject.ID, call.Subject.AgentID)
	scope := string(call.Scope)
	if strings.TrimSpace(scope) == "" {
		scope = "{}"
	}
	var scoped struct {
		LeaseID string `json:"lease_id"`
	}
	_ = json.Unmarshal(call.Scope, &scoped)
	attemptID := ""
	if scoped.LeaseID != "" {
		_ = d.db.QueryRowContext(ctx, `
SELECT id FROM attempts WHERE lease_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, scoped.LeaseID).Scan(&attemptID)
	}
	var retryable any
	if response.Retryable != nil {
		retryable = 0
		if *response.Retryable {
			retryable = 1
		}
	}
	_, err = d.db.ExecContext(ctx, `
INSERT INTO capability_calls (
  id, tenant_id, trace_id, capability_name, subject_kind, subject_id,
  scope_json, status, error_code, retryable, attempt_id, lease_id, runtime_id,
  idempotency_key, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, traceID, call.CapabilityName, subjectKind, subjectID, scope,
		string(response.Status), response.ErrorCode, retryable, attemptID,
		scoped.LeaseID, call.Subject.RuntimeID, call.IdempotencyKey,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func Open(ctx context.Context, cfg Config) (*Backend, error) {
	cfg = cfg.withDefaults()
	if cfg.DBPath == "" {
		return nil, errors.New("coordplane serve: --db is required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db parent directory: %w", err)
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = db.Close()
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite db: %w", err)
	}

	st := store.New(db)
	migrationResult, err := st.Migrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	allMigrations, err := listMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(allMigrations) == 0 {
		allMigrations = migrationResult.Applied
	}

	skillRegistry := skills.NewRegistry(st)
	if err := skillRegistry.RegisterBuiltins(ctx); err != nil {
		return nil, fmt.Errorf("register built-in skills: %w", err)
	}
	teamRepo := teamconfig.NewRepository(st)
	teamCfg, teamLoaded, err := loadTeamConfig(ctx, teamRepo, cfg)
	if err != nil {
		return nil, err
	}

	coordSvc := coordination.NewServiceWithTeam(st, teamCfg.TeamID, teamCfg.Version)
	codeSvc := codemanagement.NewService(st)
	commandSvc, err := commandrun.NewService(commandrun.Config{Store: st})
	if err != nil {
		return nil, fmt.Errorf("initialize command.run service: %w", err)
	}
	validationSvc, err := validation.NewService(validation.Config{Store: st})
	if err != nil {
		return nil, fmt.Errorf("initialize validation.assessment service: %w", err)
	}
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, coordSvc); err != nil {
		return nil, err
	}
	if err := commandrun.RegisterCapabilities(registry, commandSvc); err != nil {
		return nil, err
	}
	if err := validation.RegisterCapabilities(registry, validationSvc); err != nil {
		return nil, err
	}
	if err := objects.RegisterCapabilities(registry, coordSvc.ObjectStore()); err != nil {
		return nil, err
	}
	if err := codemanagement.RegisterCapabilities(registry, codeSvc); err != nil {
		return nil, err
	}
	releaseSvc, err := releaseacceptance.NewService(releaseacceptance.Config{
		Store:        st,
		DBPath:       cfg.DBPath,
		Capabilities: capabilityNames(registry),
	})
	if err != nil {
		return nil, fmt.Errorf("initialize release acceptance service: %w", err)
	}
	dispatcher := policy.NewDispatcher(teamCfg, registry)
	auditedDispatcher := newAuditedDispatcher(db, dispatcher)
	runtimeAdapter, cliAdapterRegistry := buildCLIAdapters(st, db, cfg, teamCfg)
	externalRuntime := cpruntime.ExternalRuntime{
		ID:            "external-local",
		WorkspaceRoot: cfg.RuntimeWorkspaceRoot,
		HomeRoot:      cfg.RuntimeHomeRoot,
		Ready:         true,
	}
	runtimeBackends, runtimeRegistry := buildRuntimeBackends(db, cfg, teamCfg, externalRuntime)
	runner, err := cpruntime.NewRunner(cpruntime.RunnerConfig{
		Store:           st,
		Coordination:    coordSvc,
		TeamConfig:      teamCfg,
		Skills:          skillRegistry,
		Runtime:         externalRuntime,
		RuntimeBackends: runtimeBackends,
		Adapter:         runtimeAdapter,
		BackendURL:      cfg.BackendURL,
		WorkspaceName:   "coordplane-serve",
	})
	if err != nil {
		return nil, fmt.Errorf("initialize runner: %w", err)
	}
	deliverySvc, err := delivery.NewServiceWithCommunication(st, runner, teamCfg.Communication)
	if err != nil {
		return nil, fmt.Errorf("initialize delivery service: %w", err)
	}
	operatorSvc, err := operator.NewService(operator.Config{
		Store:            st,
		TeamConfig:       teamCfg,
		TeamConfigLoaded: teamLoaded,
		Runner:           runner,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize operator task service: %w", err)
	}

	authenticator := sessionauth.NewAll(db)
	backend := &Backend{
		DB:                 db,
		Store:              st,
		Queue:              queue.New(db),
		Capabilities:       registry,
		Skills:             skillRegistry,
		TeamConfig:         teamCfg,
		TeamConfigLoaded:   teamLoaded,
		Dispatcher:         dispatcher,
		Coordination:       coordSvc,
		CommandRun:         commandSvc,
		Validation:         validationSvc,
		ReleaseAcceptance:  releaseSvc,
		CodeManagement:     codeSvc,
		Delivery:           deliverySvc,
		OperatorTasks:      operatorSvc,
		Runner:             runner,
		Runtime:            externalRuntime,
		RuntimeRegistry:    runtimeRegistry,
		CLIAdapterRegistry: cliAdapterRegistry,
		Migrations:         allMigrations,
		StartedAt:          time.Now().UTC(),
		dbPath:             cfg.DBPath,
		listenAddr:         cfg.ListenAddr,
		authenticator:      authenticator,
		operatorToken:      cfg.OperatorToken,
		operatorSubjectID:  cfg.OperatorSubjectID,
	}
	backend.Handler = backend.routes(httpapi.NewWithAuthenticator(auditedDispatcher, authenticator))
	closeOnError = false
	return backend, nil
}

func RunServe(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	backend, err := Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = backend.Close()
	}()
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	server := &http.Server{Handler: backend.Handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (b *Backend) Close() error {
	if b == nil || b.DB == nil {
		return nil
	}
	return b.DB.Close()
}

func (b *Backend) Inspect(ctx context.Context) (Inspect, error) {
	counts, err := tableCounts(ctx, b.DB, []string{
		"schema_migrations",
		"events",
		"queue_items",
		"capability_calls",
		"team_config_versions",
		"skill_packages",
		"agent_communication_envelopes",
		"mailbox_items",
		"work_contracts",
		"assignments",
		"leases",
		"attempts",
		"session_routes",
		"runtime_instances",
		"cli_sessions",
		"command_runs",
		"validation_assessments",
		"release_acceptances",
		"contract_team_scopes",
		"delivery_attempts",
		"git_operations",
		"operator_task_runs",
	})
	if err != nil {
		return Inspect{}, err
	}
	skillNames, err := builtinSkillNames(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	runtimeInstances, err := cpruntime.ListRuntimeInstances(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	cliSessions, err := cpruntime.ListCLISessions(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	if cliSessions == nil {
		cliSessions = []cpruntime.CLISession{}
	}
	commandRuns, err := commandrun.ListRuns(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	if commandRuns == nil {
		commandRuns = []commandrun.Run{}
	}
	validationAssessments, err := validation.ListAssessments(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	if validationAssessments == nil {
		validationAssessments = []validation.Assessment{}
	}
	releaseSummary, err := releaseacceptance.LatestSummary(ctx, b.DB)
	if err != nil {
		return Inspect{}, err
	}
	return Inspect{
		Status:                "ok",
		DBPath:                b.dbPath,
		ListenAddr:            b.listenAddr,
		StartedAt:             b.StartedAt,
		Migrations:            append([]string(nil), b.Migrations...),
		Counts:                counts,
		Capabilities:          capabilityNames(b.Capabilities),
		BuiltinSkills:         skillNames,
		TeamConfigLoaded:      b.TeamConfigLoaded,
		TeamID:                b.TeamConfig.TeamID,
		TeamVersion:           b.TeamConfig.Version,
		QueueInitialized:      b.Queue != nil,
		RunnerInitialized:     b.Runner != nil,
		RuntimeRegistry:       append([]RuntimeEntry(nil), b.RuntimeRegistry...),
		RuntimeInstances:      runtimeInstances,
		CLISessions:           cliSessions,
		CommandRuns:           commandRuns,
		ValidationAssessments: validationAssessments,
		ReleaseAcceptance:     releaseSummary,
		CLIAdapterRegistry:    append([]CLIAdapterEntry(nil), b.CLIAdapterRegistry...),
		CapabilityRegistry:    b.Capabilities != nil,
		SkillRegistry:         b.Skills != nil,
		SQLiteFileBacked:      b.dbPath != "" && b.dbPath != ":memory:",
		AcceptanceGateState:   "step_passed_only",
	}, nil
}

func (b *Backend) EvaluateReleaseAcceptance(ctx context.Context, in releaseacceptance.EvaluateInput) (releaseacceptance.Acceptance, error) {
	if b == nil || b.ReleaseAcceptance == nil {
		return releaseacceptance.Acceptance{}, errors.New("release acceptance service is not initialized")
	}
	if in.TeamID == "" {
		in.TeamID = b.TeamConfig.TeamID
	}
	if in.TeamVersion == 0 {
		in.TeamVersion = b.TeamConfig.Version
	}
	return b.ReleaseAcceptance.Evaluate(ctx, in)
}

func (b *Backend) Ready(ctx context.Context) (Inspect, error) {
	if err := b.DB.PingContext(ctx); err != nil {
		return Inspect{}, err
	}
	inspect, err := b.Inspect(ctx)
	if err != nil {
		return Inspect{}, err
	}
	if len(inspect.Migrations) == 0 {
		return Inspect{}, errors.New("no applied migrations")
	}
	if len(inspect.Capabilities) == 0 {
		return Inspect{}, errors.New("capability registry is empty")
	}
	if len(inspect.BuiltinSkills) == 0 {
		return Inspect{}, errors.New("skill registry is empty")
	}
	return inspect, nil
}

func (b *Backend) routes(capabilityHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"service":    "coordplane",
			"started_at": b.StartedAt,
		})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		inspect, err := b.Ready(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready",
				"error":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, inspect)
	})
	mux.HandleFunc("/inspect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		inspect, err := b.Inspect(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, inspect)
	})
	mux.HandleFunc("/operator/tasks", b.handleOperatorTasks)
	mux.HandleFunc("/operator/tasks/", b.handleOperatorTaskSubresource)
	mux.HandleFunc("/skills", b.handleSkillList)
	mux.HandleFunc("/skills/", b.handleSkillRead)
	mux.Handle("/call", capabilityHandler)
	mux.Handle("/capabilities", capabilityHandler)
	return mux
}

func (b *Backend) handleOperatorTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subject, ok := b.authenticateOperator(w, r)
	if !ok {
		return
	}
	if b == nil || b.OperatorTasks == nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_SERVICE_UNAVAILABLE", "operator task service is not initialized", true))
		return
	}
	var input operator.CreateTaskInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&input); err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("INVALID_OPERATOR_TASK_REQUEST", "request body must be a JSON operator task create payload", false))
		return
	}
	result, err := b.OperatorTasks.CreateTask(r.Context(), subject, input)
	if err != nil {
		var rejected operator.RejectedError
		if errors.As(err, &rejected) {
			writeTypedResponse(w, capability.Rejected[json.RawMessage](
				rejected.Code,
				rejected.Message,
				capability.WithRepairHint("retry with a valid operator task create payload and loaded TeamConfig"),
				capability.WithAllowedNextActions("operator.task.create"),
				capability.WithRetryable(false),
			))
			return
		}
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_CREATE_FAILED", err.Error(), false))
		return
	}
	response, err := capability.AcceptedJSON(result)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_RESPONSE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func (b *Backend) handleOperatorTaskSubresource(w http.ResponseWriter, r *http.Request) {
	taskRunID, action, ok := parseOperatorTaskSubresource(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	subject, ok := b.authenticateOperator(w, r)
	if !ok {
		return
	}
	if b == nil || b.OperatorTasks == nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_SERVICE_UNAVAILABLE", "operator task service is not initialized", true))
		return
	}
	switch action {
	case "start":
		b.handleOperatorTaskStart(w, r, subject, taskRunID)
	case "wait":
		b.handleOperatorTaskWait(w, r, subject, taskRunID)
	case "evidence":
		b.handleOperatorTaskEvidence(w, r, subject, taskRunID)
	default:
		http.NotFound(w, r)
	}
}

func (b *Backend) handleOperatorTaskStart(w http.ResponseWriter, r *http.Request, subject operator.Subject, taskRunID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input operator.StartTaskInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		writeTypedResponse(w, capability.Error[json.RawMessage]("INVALID_OPERATOR_TASK_START_REQUEST", "request body must be a JSON operator task start payload", false))
		return
	}
	result, err := b.OperatorTasks.StartTask(r.Context(), subject, taskRunID, input)
	if err != nil {
		var rejected operator.RejectedError
		if errors.As(err, &rejected) {
			writeTypedResponse(w, capability.Rejected[json.RawMessage](
				rejected.Code,
				rejected.Message,
				capability.WithRepairHint("retry with a valid operator task run and operator token"),
				capability.WithAllowedNextActions("operator.task.start"),
				capability.WithRetryable(false),
			))
			return
		}
		if reason, ok := cpruntime.ErrorTerminalReason(err); ok {
			switch reason {
			case cpruntime.TerminalReasonRuntimeExecTimeout:
				writeTypedResponse(w, capability.Rejected[json.RawMessage](
					reason,
					err.Error(),
					capability.WithRepairHint("inspect operator task evidence, then retry start only after the provider timeout cause is resolved"),
					capability.WithAllowedNextActions("operator.task.evidence", "operator.task.wait", "operator.task.start"),
					capability.WithRetryable(true),
				))
				return
			case cpruntime.TerminalReasonAgentExitedWithoutAction:
				writeTypedResponse(w, capability.Rejected[json.RawMessage](
					reason,
					err.Error(),
					capability.WithRepairHint("inspect operator task evidence, then retry with a provider turn that calls contract.complete or contract.wait"),
					capability.WithAllowedNextActions("operator.task.evidence", "operator.task.wait", "operator.task.start"),
					capability.WithRetryable(true),
				))
				return
			}
		}
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_START_FAILED", err.Error(), false))
		return
	}
	response, err := capability.AcceptedJSON(result)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_RESPONSE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func (b *Backend) handleOperatorTaskWait(w http.ResponseWriter, r *http.Request, subject operator.Subject, taskRunID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input operator.WaitTaskInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		writeTypedResponse(w, capability.Error[json.RawMessage]("INVALID_OPERATOR_TASK_WAIT_REQUEST", "request body must be a JSON operator task wait payload", false))
		return
	}
	result, err := b.OperatorTasks.WaitTask(r.Context(), subject, taskRunID, input)
	if err != nil {
		var rejected operator.RejectedError
		if errors.As(err, &rejected) {
			writeTypedResponse(w, capability.Rejected[json.RawMessage](
				rejected.Code,
				rejected.Message,
				capability.WithRepairHint("retry with a valid operator task run and operator token"),
				capability.WithAllowedNextActions("operator.task.wait", "operator.task.evidence"),
				capability.WithRetryable(false),
			))
			return
		}
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_WAIT_FAILED", err.Error(), false))
		return
	}
	response, err := capability.AcceptedJSON(result)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_RESPONSE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func (b *Backend) handleOperatorTaskEvidence(w http.ResponseWriter, r *http.Request, subject operator.Subject, taskRunID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := b.OperatorTasks.Evidence(r.Context(), subject, taskRunID)
	if err != nil {
		var rejected operator.RejectedError
		if errors.As(err, &rejected) {
			writeTypedResponse(w, capability.Rejected[json.RawMessage](
				rejected.Code,
				rejected.Message,
				capability.WithRepairHint("retry with a valid operator task run and operator token"),
				capability.WithAllowedNextActions("operator.task.evidence"),
				capability.WithRetryable(false),
			))
			return
		}
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_EVIDENCE_FAILED", err.Error(), false))
		return
	}
	response, err := capability.AcceptedJSON(result)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("OPERATOR_TASK_RESPONSE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func parseOperatorTaskSubresource(path string) (taskRunID string, action string, ok bool) {
	rest := strings.TrimPrefix(path, "/operator/tasks/")
	if rest == path || rest == "" {
		return "", "", false
	}
	taskRunID, action, ok = strings.Cut(rest, "/")
	if !ok || taskRunID == "" || action == "" || strings.Contains(action, "/") {
		return "", "", false
	}
	return taskRunID, action, true
}

func (b *Backend) authenticateOperator(w http.ResponseWriter, r *http.Request) (operator.Subject, bool) {
	if b == nil || strings.TrimSpace(b.operatorToken) == "" {
		writeOperatorAuthRejected(w, http.StatusUnauthorized, "OPERATOR_AUTH_REQUIRED", "operator bearer token is required")
		return operator.Subject{}, false
	}
	token, ok := operatorBearerToken(r)
	if !ok {
		writeOperatorAuthRejected(w, http.StatusUnauthorized, "OPERATOR_AUTH_REQUIRED", "operator bearer token is required")
		return operator.Subject{}, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(b.operatorToken)) != 1 {
		writeOperatorAuthRejected(w, http.StatusForbidden, "OPERATOR_AUTH_REJECTED", "authorization token is not an operator token")
		return operator.Subject{}, false
	}
	subjectID := strings.TrimSpace(b.operatorSubjectID)
	if subjectID == "" {
		subjectID = "operator"
	}
	return operator.Subject{Kind: "operator", ID: subjectID}, true
}

func operatorBearerToken(r *http.Request) (string, bool) {
	header := ""
	if r != nil {
		header = strings.TrimSpace(r.Header.Get("Authorization"))
	}
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}

func writeOperatorAuthRejected(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, capability.Rejected[json.RawMessage](
		code,
		message,
		capability.WithRepairHint("retry with the configured operator token"),
		capability.WithAllowedNextActions("operator.task.create"),
		capability.WithRetryable(false),
	))
}

func (b *Backend) handleSkillList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subject, ok := b.authenticateDiscoverySubject(w, r)
	if !ok {
		return
	}
	agentID := subject.AgentID
	summaries, err := b.Skills.ListForAgent(r.Context(), b.TeamConfig, agentID)
	if err != nil {
		writeTypedResponse(w, capability.Rejected[json.RawMessage](
			"SKILL_LIST_REJECTED",
			err.Error(),
			capability.WithRepairHint("load TeamConfig for this agent or request skills bound to the current agent"),
			capability.WithAllowedNextActions("skill.list"),
			capability.WithRetryable(false),
		))
		return
	}
	response, err := capability.AcceptedJSON(summaries)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("SKILL_LIST_ENCODE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func (b *Backend) handleSkillRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/skills/")
	if name == "" {
		writeTypedResponse(w, capability.Rejected[json.RawMessage](
			"SKILL_NAME_REQUIRED",
			"skill name is required",
			capability.WithRepairHint("retry with /skills/{name}"),
			capability.WithAllowedNextActions("skill.list"),
			capability.WithRetryable(false),
		))
		return
	}
	subject, ok := b.authenticateDiscoverySubject(w, r)
	if !ok {
		return
	}
	agentID := subject.AgentID
	skill, err := b.Skills.ReadForAgent(r.Context(), b.TeamConfig, agentID, name)
	if err != nil {
		writeTypedResponse(w, capability.Rejected[json.RawMessage](
			"SKILL_READ_REJECTED",
			err.Error(),
			capability.WithRepairHint("read only skills bound to the current agent in TeamConfig"),
			capability.WithAllowedNextActions("skill.list"),
			capability.WithRetryable(false),
		))
		return
	}
	response, err := capability.AcceptedJSON(skill)
	if err != nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("SKILL_READ_ENCODE_FAILED", err.Error(), false))
		return
	}
	writeTypedResponse(w, response)
}

func (b *Backend) authenticateDiscoverySubject(w http.ResponseWriter, r *http.Request) (capability.Subject, bool) {
	if b == nil || b.authenticator == nil {
		writeTypedResponse(w, capability.Error[json.RawMessage]("AUTH_STORE_UNAVAILABLE", "runtime token store is unavailable", true))
		return capability.Subject{}, false
	}
	subject, response := b.authenticator.AuthenticateSubject(r.Context(), r, subjectFromRequest(r))
	if response.Status != "" {
		writeTypedResponse(w, response)
		return capability.Subject{}, false
	}
	return subject, true
}

func buildCLIAdapters(st *store.Store, db *sql.DB, cfg Config, teamCfg teamconfig.Config) (cpruntime.CLIAdapter, []CLIAdapterEntry) {
	fake := cpruntime.NewFakeCLIAdapter()
	registrations := []cpruntime.CLIAdapterRegistration{{
		Name:    "fake",
		Kind:    "fake",
		Ready:   true,
		Adapter: fake,
	}}
	entries := []CLIAdapterEntry{{
		Name:  "fake",
		Kind:  "fake",
		Ready: true,
	}}
	claudeReady := cfg.ClaudeBinary != ""
	if claudeReady {
		claude, err := cpruntime.NewClaudeCommandCLIAdapter(cpruntime.CommandCLIAdapterConfig{
			Store: st,
			Profile: cpruntime.CommandCLIProfile{
				Name:                   "claude",
				Backend:                "claude",
				Binary:                 cfg.ClaudeBinary,
				StartArgs:              cfg.ClaudeStartArgs,
				ResumeArgs:             cfg.ClaudeResumeArgs,
				Timeout:                cfg.ClaudeTimeout,
				RuntimeCommandPolicies: runtimeCommandPolicies(teamCfg),
				AgentCapabilities:      teamConfigAgentCapabilities(teamCfg),
			},
		})
		if err == nil {
			registrations = append(registrations, cpruntime.CLIAdapterRegistration{
				Name:    "claude",
				Kind:    "command",
				Ready:   true,
				Adapter: claude,
			})
		} else {
			claudeReady = false
		}
	}
	entries = append(entries, CLIAdapterEntry{
		Name:  "claude",
		Kind:  "command",
		Ready: claudeReady,
	})
	return cpruntime.NewCLIAdapterRegistry(db, registrations), entries
}

func teamConfigAgentCapabilities(teamCfg teamconfig.Config) map[string][]string {
	if len(teamCfg.Agents) == 0 {
		return nil
	}
	out := make(map[string][]string, len(teamCfg.Agents))
	for _, agent := range teamCfg.Agents {
		out[agent.ID] = append([]string(nil), agent.Capabilities...)
	}
	return out
}

func runtimeCommandPolicies(teamCfg teamconfig.Config) map[string]cpruntime.RuntimeCommandPolicy {
	if len(teamCfg.RuntimeProfiles) == 0 {
		return nil
	}
	out := make(map[string]cpruntime.RuntimeCommandPolicy, len(teamCfg.RuntimeProfiles))
	for name, profile := range teamCfg.RuntimeProfiles {
		policy := profile.CommandPolicy
		if profile.Kind != "docker" && !policy.NonInteractiveApproval && len(policy.AllowCoordlinkCapabilities) == 0 {
			continue
		}
		out[name] = cpruntime.RuntimeCommandPolicy{
			NonInteractiveApproval:     policy.NonInteractiveApproval,
			AllowCoordlinkCapabilities: append([]string(nil), policy.AllowCoordlinkCapabilities...),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (cfg Config) withDefaults() Config {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.OperatorTokenEnv == "" {
		cfg.OperatorTokenEnv = "COORDPLANE_OPERATOR_TOKEN"
	}
	if cfg.OperatorToken == "" && cfg.OperatorTokenEnv != "" {
		cfg.OperatorToken = strings.TrimSpace(os.Getenv(cfg.OperatorTokenEnv))
	}
	if cfg.OperatorSubjectID == "" {
		cfg.OperatorSubjectID = "operator"
	}
	if cfg.TeamID == "" {
		cfg.TeamID = defaultTeamID
	}
	if cfg.DBPath != "" {
		base := filepath.Join(filepath.Dir(cfg.DBPath), "runtime")
		if cfg.RuntimeWorkspaceRoot == "" {
			cfg.RuntimeWorkspaceRoot = filepath.Join(base, "workspaces")
		}
		if cfg.RuntimeHomeRoot == "" {
			cfg.RuntimeHomeRoot = filepath.Join(base, "home")
		}
	}
	if cfg.BackendURL == "" {
		cfg.BackendURL = "http://" + listenAddrForURL(cfg.ListenAddr)
	}
	return cfg
}

func buildRuntimeBackends(db *sql.DB, cfg Config, teamCfg teamconfig.Config, external cpruntime.ExternalRuntime) (map[string]cpruntime.RuntimeBackend, []RuntimeEntry) {
	backends := make(map[string]cpruntime.RuntimeBackend)
	var entries []RuntimeEntry
	secretProvider := secrets.NewEnvProvider(effectiveClaudeEnvKeys(cfg.ClaudeEnvKeys))
	for profileName, profile := range teamCfg.RuntimeProfiles {
		switch profile.Kind {
		case "external", "":
			runtime := cpruntime.ExternalRuntime{
				ID:            profileName,
				WorkspaceRoot: cfg.RuntimeWorkspaceRoot,
				HomeRoot:      cfg.RuntimeHomeRoot,
				Ready:         true,
			}
			backends[profileName] = runtime
			entries = append(entries, RuntimeEntry{
				ID:            runtime.ID,
				Profile:       profileName,
				Kind:          "external",
				Ready:         runtime.Ready,
				WorkspaceRoot: runtime.WorkspaceRoot,
				HomeRoot:      runtime.HomeRoot,
			})
		case "docker":
			dockerRoot := filepath.Join(filepath.Clean(filepath.Dir(cfg.DBPath))+"-docker-runtime", profileName)
			ready := profile.Image != "" && cfg.CoordlinkPath != ""
			runtime := cpruntime.NewDockerRuntime(cpruntime.DockerRuntimeConfig{
				DB:             db,
				ProfileName:    profileName,
				TeamID:         teamCfg.TeamID,
				Image:          profile.Image,
				Network:        cfg.DockerNetwork,
				RuntimeRoot:    dockerRoot,
				CoordlinkPath:  cfg.CoordlinkPath,
				DBPath:         cfg.DBPath,
				ClaudeBinary:   cfg.ClaudeBinary,
				Ready:          ready,
				SecretProvider: secretProvider,
			})
			backends[profileName] = runtime
			entries = append(entries, RuntimeEntry{
				ID:            profileName,
				Profile:       profileName,
				Kind:          "docker",
				Ready:         ready,
				WorkspaceRoot: cpruntime.ContainerWorkspacePath,
				HomeRoot:      cpruntime.ContainerHomePath,
			})
		}
	}
	if len(entries) == 0 {
		backends[external.ID] = external
		entries = append(entries, RuntimeEntry{
			ID:            external.ID,
			Profile:       external.ID,
			Kind:          "external",
			Ready:         external.Ready,
			WorkspaceRoot: external.WorkspaceRoot,
			HomeRoot:      external.HomeRoot,
		})
	}
	return backends, entries
}

func effectiveClaudeEnvKeys(keys []string) []string {
	if len(keys) > 0 {
		return append([]string(nil), keys...)
	}
	return append([]string(nil), claudeenv.RuntimeKeys...)
}

func loadTeamConfig(ctx context.Context, repo *teamconfig.Repository, cfg Config) (teamconfig.Config, bool, error) {
	if cfg.TeamConfigPath != "" {
		raw, err := os.ReadFile(cfg.TeamConfigPath)
		if err != nil {
			return teamconfig.Config{}, false, fmt.Errorf("read TeamConfig: %w", err)
		}
		loaded, err := repo.SaveYAML(ctx, raw)
		if err != nil {
			return teamconfig.Config{}, false, fmt.Errorf("load TeamConfig: %w", err)
		}
		return loaded, true, nil
	}
	loaded, err := repo.Load(ctx, cfg.TeamID, 0)
	if err == nil {
		return loaded, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return teamconfig.Config{TeamID: cfg.TeamID, Version: 0}, false, nil
	}
	return teamconfig.Config{}, false, err
}

func listMigrations(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		out = append(out, version)
	}
	return out, rows.Err()
}

func tableCounts(ctx context.Context, db *sql.DB, tables []string) (map[string]int64, error) {
	out := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = count
	}
	return out, nil
}

func builtinSkillNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT name
FROM skill_packages
WHERE enabled = 1
ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list built-in skills: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func capabilityNames(registry *capability.Registry) []string {
	definitions := registry.List()
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	sort.Strings(out)
	return out
}

func listenAddrForURL(listenAddr string) string {
	if strings.HasPrefix(listenAddr, ":") {
		return "127.0.0.1" + listenAddr
	}
	return listenAddr
}

func subjectFromRequest(r *http.Request) capability.Subject {
	agentID := requestValue(r, "agent_id", "X-CoordPlane-Agent-ID")
	runtimeID := requestValue(r, "runtime_id", "X-CoordPlane-Runtime-ID")
	workspaceID := requestValue(r, "workspace_id", "X-CoordPlane-Workspace-ID")
	return capability.Subject{
		Kind:        "agent",
		ID:          agentID,
		AgentID:     agentID,
		RuntimeID:   runtimeID,
		WorkspaceID: workspaceID,
	}
}

func requestValue(r *http.Request, queryKey, headerKey string) string {
	if r == nil {
		return ""
	}
	value := r.URL.Query().Get(queryKey)
	if value == "" {
		value = r.Header.Get(headerKey)
	}
	return value
}

func writeTypedResponse(w http.ResponseWriter, response capability.Response[json.RawMessage]) {
	status := http.StatusInternalServerError
	switch response.Status {
	case capability.StatusAccepted:
		status = http.StatusOK
	case capability.StatusRejected:
		status = http.StatusBadRequest
	}
	writeJSON(w, status, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
