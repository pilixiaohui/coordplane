package teamconfig

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"coordplane/internal/events"
	"coordplane/internal/store"

	"gopkg.in/yaml.v3"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Config struct {
	TeamID          string                    `json:"team_id" yaml:"team_id"`
	Version         int                       `json:"version" yaml:"version"`
	Agents          []AgentConfig             `json:"agents" yaml:"agents"`
	Communication   CommunicationConfig       `json:"communication,omitempty" yaml:"communication"`
	RuntimeProfiles map[string]RuntimeProfile `json:"runtime_profiles,omitempty" yaml:"runtime_profiles"`
	Termination     TerminationConfig         `json:"termination,omitempty" yaml:"termination"`
}

type AgentConfig struct {
	ID             string   `json:"id" yaml:"id"`
	RolePromptRef  string   `json:"role_prompt_ref,omitempty" yaml:"role_prompt_ref"`
	RolePrompt     string   `json:"role_prompt,omitempty" yaml:"role_prompt"`
	RuntimeProfile string   `json:"runtime_profile" yaml:"runtime_profile"`
	CLIBackend     string   `json:"cli_backend" yaml:"cli_backend"`
	Skills         []string `json:"skills" yaml:"skills"`
	Capabilities   []string `json:"capabilities" yaml:"capabilities"`
}

type RuntimeProfile struct {
	Kind          string `json:"kind" yaml:"kind"`
	Image         string `json:"image,omitempty" yaml:"image"`
	WorkspaceMode string `json:"workspace_mode,omitempty" yaml:"workspace_mode"`
}

type TerminationConfig struct {
	TerminalContractType string `json:"terminal_contract_type,omitempty" yaml:"terminal_contract_type"`
	AcceptedByCapability string `json:"accepted_by_capability,omitempty" yaml:"accepted_by_capability"`
}

type CommunicationConfig struct {
	AllowDirectMessage    bool            `json:"allow_direct_message" yaml:"allow_direct_message"`
	AllowFollowupTask     bool            `json:"allow_followup_task" yaml:"allow_followup_task"`
	TaskRequiresContract  bool            `json:"task_requires_contract" yaml:"task_requires_contract"`
	DefaultTriggerTurn    map[string]bool `json:"default_trigger_turn,omitempty" yaml:"default_trigger_turn"`
	SignalSummaryMaxBytes int             `json:"signal_summary_max_bytes,omitempty" yaml:"signal_summary_max_bytes"`
	SignalBodyMaxBytes    int             `json:"signal_body_max_bytes,omitempty" yaml:"signal_body_max_bytes"`
}

const (
	DefaultSignalSummaryMaxBytes = 160
	DefaultSignalBodyMaxBytes    = 240
)

func DefaultCommunicationConfig() CommunicationConfig {
	return CommunicationConfig{
		AllowDirectMessage:    true,
		AllowFollowupTask:     true,
		TaskRequiresContract:  true,
		DefaultTriggerTurn:    defaultTriggerTurnMap(),
		SignalSummaryMaxBytes: DefaultSignalSummaryMaxBytes,
		SignalBodyMaxBytes:    DefaultSignalBodyMaxBytes,
	}
}

func (c CommunicationConfig) TriggerTurn(kind string) bool {
	normalized := normalizeCommunication(c)
	value, ok := normalized.DefaultTriggerTurn[kind]
	if !ok {
		return true
	}
	return value
}

func (c CommunicationConfig) Normalized() CommunicationConfig {
	return normalizeCommunication(c)
}

func (c CommunicationConfig) SignalSummaryLimit() int {
	return normalizeCommunication(c).SignalSummaryMaxBytes
}

func (c CommunicationConfig) SignalBodyLimit() int {
	return normalizeCommunication(c).SignalBodyMaxBytes
}

func ParseYAML(raw []byte) (Config, error) {
	if err := validateNoSensitiveMarkers("raw_yaml", string(raw)); err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse TeamConfig YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("parse TeamConfig YAML: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("parse TeamConfig YAML: %w", err)
	}
	cfg = normalizeConfig(cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.TeamID == "" {
		return errors.New("teamconfig: team_id is required")
	}
	if c.Version <= 0 {
		return errors.New("teamconfig: version must be positive")
	}
	seen := make(map[string]bool, len(c.Agents))
	for _, agent := range c.Agents {
		if agent.ID == "" {
			return errors.New("teamconfig: agent id is required")
		}
		if seen[agent.ID] {
			return fmt.Errorf("teamconfig: duplicate agent %q", agent.ID)
		}
		seen[agent.ID] = true
		if agent.RuntimeProfile == "" {
			return fmt.Errorf("teamconfig: agent %q runtime_profile is required", agent.ID)
		}
		if agent.CLIBackend == "" {
			return fmt.Errorf("teamconfig: agent %q cli_backend is required", agent.ID)
		}
		if err := validateSafeRolePrompt(agent.ID, agent.RolePrompt); err != nil {
			return err
		}
		if err := validateRolePromptRef(agent.ID, agent.RolePromptRef); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) Agent(agentID string) (AgentConfig, bool) {
	for _, agent := range c.Agents {
		if agent.ID == agentID {
			return cloneAgent(agent), true
		}
	}
	return AgentConfig{}, false
}

type Repository struct {
	db *sql.DB
}

func NewRepository(s *store.Store) *Repository {
	return &Repository{db: s.DB()}
}

func (r *Repository) SaveYAML(ctx context.Context, raw []byte) (Config, error) {
	cfg, err := ParseYAML(raw)
	if err != nil {
		return Config{}, err
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return Config{}, fmt.Errorf("marshal TeamConfig JSON: %w", err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Config{}, fmt.Errorf("begin TeamConfig save: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	now := formatTime(time.Now())

	if _, err := tx.ExecContext(ctx, `DELETE FROM team_config_agents WHERE team_id = ? AND version = ?`, cfg.TeamID, cfg.Version); err != nil {
		return Config{}, fmt.Errorf("replace TeamConfig agents: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO team_config_versions (team_id, version, active, raw_yaml, config_json, created_at)
VALUES (?, ?, 1, ?, ?, ?)
ON CONFLICT(team_id, version) DO UPDATE SET
  active = 1,
  raw_yaml = excluded.raw_yaml,
  config_json = excluded.config_json`,
		cfg.TeamID, cfg.Version, string(raw), string(configJSON), now,
	); err != nil {
		return Config{}, fmt.Errorf("insert TeamConfig version: %w", err)
	}
	for _, agent := range cfg.Agents {
		skillsJSON, err := json.Marshal(agent.Skills)
		if err != nil {
			return Config{}, fmt.Errorf("marshal agent %s skills: %w", agent.ID, err)
		}
		capsJSON, err := json.Marshal(agent.Capabilities)
		if err != nil {
			return Config{}, fmt.Errorf("marshal agent %s capabilities: %w", agent.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO team_config_agents (
  team_id, version, agent_id, role_prompt_ref, role_prompt, runtime_profile,
  cli_backend, skills_json, capabilities_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cfg.TeamID, cfg.Version, agent.ID, agent.RolePromptRef, agent.RolePrompt,
			agent.RuntimeProfile, agent.CLIBackend, string(skillsJSON), string(capsJSON), now,
		); err != nil {
			return Config{}, fmt.Errorf("insert TeamConfig agent %s: %w", agent.ID, err)
		}
	}
	if _, err := store.AppendEventTx(ctx, tx, events.Event{
		TenantID:      "default",
		SubjectKind:   "operator",
		SubjectID:     "teamconfig",
		Type:          "team_config.loaded",
		AggregateType: "team_config",
		AggregateID:   fmt.Sprintf("%s:%d", cfg.TeamID, cfg.Version),
		PayloadJSON:   configJSON,
	}); err != nil {
		return Config{}, err
	}
	if err := tx.Commit(); err != nil {
		return Config{}, fmt.Errorf("commit TeamConfig save: %w", err)
	}
	return cloneConfig(cfg), nil
}

func (r *Repository) Load(ctx context.Context, teamID string, version int) (Config, error) {
	query := `SELECT config_json FROM team_config_versions WHERE team_id = ? AND version = ?`
	args := []any{teamID, version}
	if version == 0 {
		query = `SELECT config_json FROM team_config_versions WHERE team_id = ? AND active = 1 ORDER BY version DESC LIMIT 1`
		args = []any{teamID}
	}
	var raw string
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		return Config{}, fmt.Errorf("load TeamConfig: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("decode TeamConfig JSON: %w", err)
	}
	return cloneConfig(cfg), nil
}

func cloneConfig(cfg Config) Config {
	cloned := cfg
	cloned.Communication = normalizeCommunication(cfg.Communication)
	cloned.Agents = make([]AgentConfig, len(cfg.Agents))
	for i, agent := range cfg.Agents {
		cloned.Agents[i] = cloneAgent(agent)
	}
	if cfg.RuntimeProfiles != nil {
		cloned.RuntimeProfiles = make(map[string]RuntimeProfile, len(cfg.RuntimeProfiles))
		for key, value := range cfg.RuntimeProfiles {
			cloned.RuntimeProfiles[key] = value
		}
	}
	return cloned
}

func normalizeConfig(cfg Config) Config {
	cfg.Communication = normalizeCommunication(cfg.Communication)
	return cfg
}

func normalizeCommunication(cfg CommunicationConfig) CommunicationConfig {
	defaults := DefaultCommunicationConfig()
	normalized := cfg
	if len(normalized.DefaultTriggerTurn) == 0 {
		normalized.DefaultTriggerTurn = defaults.DefaultTriggerTurn
	} else {
		merged := defaultTriggerTurnMap()
		for key, value := range normalized.DefaultTriggerTurn {
			merged[key] = value
		}
		normalized.DefaultTriggerTurn = merged
	}
	if normalized.SignalSummaryMaxBytes <= 0 {
		normalized.SignalSummaryMaxBytes = defaults.SignalSummaryMaxBytes
	}
	if normalized.SignalBodyMaxBytes <= 0 {
		normalized.SignalBodyMaxBytes = defaults.SignalBodyMaxBytes
	}
	if !normalized.AllowDirectMessage && !normalized.AllowFollowupTask && !normalized.TaskRequiresContract {
		normalized.AllowDirectMessage = defaults.AllowDirectMessage
		normalized.AllowFollowupTask = defaults.AllowFollowupTask
		normalized.TaskRequiresContract = defaults.TaskRequiresContract
	}
	return normalized
}

func defaultTriggerTurnMap() map[string]bool {
	return map[string]bool{
		"message":          true,
		"task":             true,
		"result":           true,
		"repair":           true,
		"followup":         true,
		"budget_attention": true,
	}
}

func cloneAgent(agent AgentConfig) AgentConfig {
	cloned := agent
	cloned.Skills = append([]string(nil), agent.Skills...)
	cloned.Capabilities = append([]string(nil), agent.Capabilities...)
	return cloned
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func validateSafeRolePrompt(agentID, prompt string) error {
	if err := validateNoSensitiveMarkers("agent "+agentID+" role_prompt", prompt); err != nil {
		return err
	}
	return nil
}

func validateRolePromptRef(agentID, ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "/") || strings.Contains(ref, "..") {
		return fmt.Errorf("teamconfig: agent %q role_prompt_ref must be a relative safe reference", agentID)
	}
	return validateNoSensitiveMarkers("agent "+agentID+" role_prompt_ref", ref)
}

func validateNoSensitiveMarkers(field, value string) error {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"secret=",
		"token=",
		"password=",
		"api_key",
		"apikey",
		"private_key",
		"/home/",
		"/tmp/",
		"/var/lib/",
		"runtime root",
		"db path",
		"database path",
		"input_schema",
		"output_schema",
		"rejected_schema",
		"available skills:",
		"read with:",
	} {
		if strings.Contains(lower, marker) {
			return fmt.Errorf("teamconfig: %s contains disallowed sensitive or prompt-forging marker %q", field, marker)
		}
	}
	return nil
}
