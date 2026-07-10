package store

const coreSchemaSQL = `
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  role TEXT NOT NULL,
  runtime_kind TEXT NOT NULL,
  cli_backend TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS work_contracts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  title TEXT NOT NULL,
  objective TEXT NOT NULL,
  issuer_agent_id TEXT,
  issuer_contract_id TEXT,
  target_kind TEXT NOT NULL,
  target_id TEXT NOT NULL,
  status TEXT NOT NULL,
  completion_requirements_json TEXT NOT NULL DEFAULT '{}',
  acceptance_policy_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assignments (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  contract_id TEXT NOT NULL,
  assignee_agent_id TEXT,
  assignee_role TEXT,
  state TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL,
  session_route_id TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id)
);

CREATE TABLE IF NOT EXISTS leases (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  assignment_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  runtime_id TEXT,
  session_route_id TEXT,
  state TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(assignment_id) REFERENCES assignments(id)
);

CREATE TABLE IF NOT EXISTS attempts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  lease_id TEXT NOT NULL,
  cli_backend TEXT NOT NULL,
  runtime_kind TEXT NOT NULL,
  session_native_id TEXT,
  start_reason TEXT NOT NULL,
  status TEXT NOT NULL,
  transcript_ref TEXT,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  FOREIGN KEY(lease_id) REFERENCES leases(id)
);

CREATE TABLE IF NOT EXISTS session_routes (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  agent_id TEXT NOT NULL,
  runtime_id TEXT,
  cli_backend TEXT NOT NULL,
  session_native_id TEXT NOT NULL,
  route_json TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  scope TEXT NOT NULL,
  subject TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  thread_id TEXT NOT NULL,
  sender_agent_id TEXT NOT NULL,
  body TEXT NOT NULL,
  references_json TEXT NOT NULL DEFAULT '[]',
  intent TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(thread_id) REFERENCES threads(id)
);

CREATE TABLE IF NOT EXISTS mailbox_items (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  recipient_agent_id TEXT,
  recipient_role TEXT,
  reason TEXT NOT NULL,
  thread_id TEXT,
  message_id TEXT,
  contract_id TEXT,
  session_route_id TEXT,
  state TEXT NOT NULL,
  followup_ref TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS evidence (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  kind TEXT NOT NULL,
  contract_id TEXT NOT NULL,
  produced_by TEXT NOT NULL,
  content_ref TEXT,
  inline_content TEXT,
  summary TEXT NOT NULL,
  verdict TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id)
);

CREATE TABLE IF NOT EXISTS delivery_attempts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  mailbox_item_id TEXT NOT NULL,
  route_id TEXT,
  signal_json TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL,
  last_error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(mailbox_item_id) REFERENCES mailbox_items(id)
);

CREATE TABLE IF NOT EXISTS capability_calls (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  trace_id TEXT NOT NULL,
  capability_name TEXT NOT NULL,
  subject_kind TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  scope_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  idempotency_key TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS artifacts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  owner_agent_id TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  checksum TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  content_type TEXT NOT NULL,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transcripts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  attempt_id TEXT NOT NULL,
  object_ref TEXT NOT NULL,
  checksum TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(attempt_id) REFERENCES attempts(id)
);

CREATE TABLE IF NOT EXISTS queue_items (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  queue_name TEXT NOT NULL,
  kind TEXT NOT NULL,
  payload_ref TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('queued', 'leased', 'done', 'failed', 'dead')),
  lease_owner TEXT,
  lease_expires_at TEXT,
  attempt_count INTEGER NOT NULL DEFAULT 0,
  next_run_at TEXT NOT NULL,
  last_error TEXT,
  idempotency_key TEXT,
  priority INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS queue_items_idempotency_idx
  ON queue_items(queue_name, idempotency_key)
  WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE INDEX IF NOT EXISTS queue_items_claim_idx
  ON queue_items(queue_name, state, next_run_at, priority);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  trace_id TEXT,
  subject_kind TEXT,
  subject_id TEXT,
  agent_id TEXT,
  runtime_id TEXT,
  capability_name TEXT,
  event_type TEXT NOT NULL,
  aggregate_type TEXT NOT NULL,
  aggregate_id TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  occurred_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS events_trace_idx ON events(trace_id, occurred_at);
CREATE INDEX IF NOT EXISTS events_aggregate_idx ON events(aggregate_type, aggregate_id, occurred_at);
`

const teamConfigSkillSchemaSQL = `
CREATE TABLE IF NOT EXISTS team_config_versions (
  team_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  raw_yaml TEXT NOT NULL,
  config_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(team_id, version)
);

CREATE TABLE IF NOT EXISTS team_config_agents (
  team_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  agent_id TEXT NOT NULL,
  role_prompt_ref TEXT,
  role_prompt TEXT NOT NULL DEFAULT '',
  runtime_profile TEXT NOT NULL,
  cli_backend TEXT NOT NULL,
  skills_json TEXT NOT NULL DEFAULT '[]',
  capabilities_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  PRIMARY KEY(team_id, version, agent_id),
  FOREIGN KEY(team_id, version) REFERENCES team_config_versions(team_id, version)
);

CREATE TABLE IF NOT EXISTS skill_packages (
  name TEXT NOT NULL,
  version INTEGER NOT NULL,
  summary TEXT NOT NULL,
  content TEXT NOT NULL,
  capability_refs_json TEXT NOT NULL DEFAULT '[]',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  PRIMARY KEY(name, version)
);

CREATE INDEX IF NOT EXISTS team_config_agents_lookup_idx
  ON team_config_agents(team_id, version, agent_id);
CREATE INDEX IF NOT EXISTS skill_packages_name_idx
  ON skill_packages(name, enabled, version);
`

const sessionLifecycleSchemaSQL = `
CREATE TABLE IF NOT EXISTS prepare_leases (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  lease_id TEXT NOT NULL,
  attempt_id TEXT,
  agent_id TEXT NOT NULL,
  owner TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active', 'released', 'expired')),
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(lease_id) REFERENCES leases(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS prepare_leases_active_lease_idx
  ON prepare_leases(lease_id)
  WHERE state = 'active';

CREATE TABLE IF NOT EXISTS active_guards (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  resource_kind TEXT NOT NULL,
  resource_ref TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  session_route_id TEXT,
  state TEXT NOT NULL CHECK (state IN ('active', 'released')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(attempt_id) REFERENCES attempts(id),
  FOREIGN KEY(lease_id) REFERENCES leases(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS active_guards_resource_idx
  ON active_guards(resource_kind, resource_ref)
  WHERE state = 'active';

CREATE INDEX IF NOT EXISTS active_guards_attempt_idx
  ON active_guards(attempt_id, state);
`

const objectStoreSchemaSQL = `
CREATE TABLE IF NOT EXISTS object_blobs (
  object_ref TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  owner_agent_id TEXT,
  checksum TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  content_type TEXT NOT NULL,
  content BLOB NOT NULL,
  created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS object_blobs_checksum_idx
  ON object_blobs(checksum);

CREATE INDEX IF NOT EXISTS object_blobs_owner_idx
  ON object_blobs(owner_agent_id, created_at);
`

const controlledGitSchemaSQL = `
CREATE TABLE IF NOT EXISTS git_repositories (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  source_path TEXT NOT NULL,
  canonical_branch TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'error')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS git_repositories_source_idx
  ON git_repositories(source_path, canonical_branch);

CREATE TABLE IF NOT EXISTS git_workspaces (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  repo_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  runtime_id TEXT,
  contract_id TEXT,
  path TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  head_ref TEXT NOT NULL,
  dirty INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL CHECK (state IN ('preparing', 'ready', 'locked', 'error', 'archived')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(repo_id) REFERENCES git_repositories(id)
);

CREATE INDEX IF NOT EXISTS git_workspaces_agent_idx
  ON git_workspaces(agent_id, repo_id, state);

CREATE TABLE IF NOT EXISTS git_operations (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  operation_type TEXT NOT NULL,
  actor_agent_id TEXT NOT NULL,
  workspace_id TEXT,
  repo_id TEXT NOT NULL,
  idempotency_key TEXT,
  before_ref TEXT NOT NULL DEFAULT '',
  after_ref TEXT NOT NULL DEFAULT '',
  stdout TEXT NOT NULL DEFAULT '',
  stderr TEXT NOT NULL DEFAULT '',
  exit_code INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'rejected', 'failed')),
  feedback_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  completed_at TEXT,
  FOREIGN KEY(workspace_id) REFERENCES git_workspaces(id),
  FOREIGN KEY(repo_id) REFERENCES git_repositories(id)
);

CREATE INDEX IF NOT EXISTS git_operations_workspace_idx
  ON git_operations(workspace_id, created_at);

CREATE TABLE IF NOT EXISTS git_locks (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('workspace', 'repo')),
  scope_id TEXT NOT NULL,
  operation_id TEXT NOT NULL,
  owner_agent_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active', 'released')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  FOREIGN KEY(operation_id) REFERENCES git_operations(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS git_locks_active_scope_idx
  ON git_locks(scope_kind, scope_id)
  WHERE state = 'active';

CREATE INDEX IF NOT EXISTS git_locks_operation_idx
  ON git_locks(operation_id, state);

CREATE TABLE IF NOT EXISTS changesets (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  workspace_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  contract_id TEXT,
  base_ref TEXT NOT NULL,
  head_ref TEXT NOT NULL,
  commit_ids_json TEXT NOT NULL DEFAULT '[]',
  summary TEXT NOT NULL,
  evidence_refs_json TEXT NOT NULL DEFAULT '[]',
  state TEXT NOT NULL CHECK (state IN ('draft', 'submitted', 'abandoned')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES git_workspaces(id),
  FOREIGN KEY(repo_id) REFERENCES git_repositories(id)
);

CREATE INDEX IF NOT EXISTS changesets_workspace_idx
  ON changesets(workspace_id, state, created_at);
`

const controlledGitV2SchemaSQL = `
CREATE TABLE IF NOT EXISTS git_merge_attempts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  changeset_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  target_ref TEXT NOT NULL,
  integration_path TEXT NOT NULL DEFAULT '',
  base_before TEXT NOT NULL,
  result_ref TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('clean', 'conflicted', 'resolved', 'applied', 'aborted', 'failed')),
  conflict_set_id TEXT,
  operation_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(changeset_id) REFERENCES changesets(id),
  FOREIGN KEY(repo_id) REFERENCES git_repositories(id),
  FOREIGN KEY(workspace_id) REFERENCES git_workspaces(id),
  FOREIGN KEY(operation_id) REFERENCES git_operations(id)
);

CREATE INDEX IF NOT EXISTS git_merge_attempts_changeset_idx
  ON git_merge_attempts(changeset_id, state, created_at);

CREATE TABLE IF NOT EXISTS git_conflict_sets (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  merge_attempt_id TEXT NOT NULL,
  files_json TEXT NOT NULL DEFAULT '[]',
  summary TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('open', 'resolved', 'abandoned')),
  resolved_by TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(merge_attempt_id) REFERENCES git_merge_attempts(id)
);

CREATE INDEX IF NOT EXISTS git_conflict_sets_attempt_idx
  ON git_conflict_sets(merge_attempt_id, state);

CREATE TABLE IF NOT EXISTS git_rollback_points (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  operation_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  workspace_id TEXT,
  target_ref TEXT NOT NULL,
  before_ref TEXT NOT NULL,
  after_ref TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('available', 'used', 'expired')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(operation_id) REFERENCES git_operations(id),
  FOREIGN KEY(repo_id) REFERENCES git_repositories(id),
  FOREIGN KEY(workspace_id) REFERENCES git_workspaces(id)
);

CREATE INDEX IF NOT EXISTS git_rollback_points_operation_idx
  ON git_rollback_points(operation_id, state);
`

const controlledGitOperationEvidenceSchemaSQL = `
ALTER TABLE git_repositories ADD COLUMN alias TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS git_repositories_alias_idx
  ON git_repositories(alias, canonical_branch)
  WHERE alias <> '';

ALTER TABLE git_operations ADD COLUMN runtime_id TEXT NOT NULL DEFAULT '';
ALTER TABLE git_operations ADD COLUMN execution_location TEXT NOT NULL DEFAULT 'backend_control_plane';
`

const controlledGitOperationSubjectKindSchemaSQL = `
ALTER TABLE git_operations ADD COLUMN subject_kind TEXT NOT NULL DEFAULT 'agent_runtime';
`

const runtimeEvidenceSchemaSQL = `
CREATE TABLE IF NOT EXISTS runtime_instances (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  runtime_id TEXT NOT NULL UNIQUE,
  runtime_profile TEXT NOT NULL,
  runtime_kind TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  container_name TEXT NOT NULL DEFAULT '',
  image TEXT NOT NULL DEFAULT '',
  network TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('preparing', 'ready', 'failed', 'stopped')),
  workspace_path TEXT NOT NULL,
  home_path TEXT NOT NULL,
  host_workspace_ref TEXT NOT NULL DEFAULT '',
  host_home_ref TEXT NOT NULL DEFAULT '',
  coordlink_path TEXT NOT NULL DEFAULT '',
  checks_json TEXT NOT NULL DEFAULT '{}',
  env_keys_json TEXT NOT NULL DEFAULT '[]',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(attempt_id) REFERENCES attempts(id),
  FOREIGN KEY(lease_id) REFERENCES leases(id)
);

CREATE INDEX IF NOT EXISTS runtime_instances_attempt_idx
  ON runtime_instances(attempt_id, state);

CREATE INDEX IF NOT EXISTS runtime_instances_agent_idx
  ON runtime_instances(agent_id, runtime_kind, state);
`

const cliSessionSchemaSQL = `
CREATE TABLE IF NOT EXISTS cli_sessions (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  attempt_id TEXT NOT NULL,
  runtime_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  cli_backend TEXT NOT NULL,
  profile_name TEXT NOT NULL,
  session_native_id TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  container_name TEXT NOT NULL DEFAULT '',
  process_ref TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('starting', 'running', 'resumed', 'exited', 'failed', 'finished')),
  start_reason TEXT NOT NULL,
  resume_of TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  last_error TEXT NOT NULL DEFAULT '',
  transcript_ref TEXT NOT NULL DEFAULT '',
  command_json TEXT NOT NULL DEFAULT '[]',
  env_keys_json TEXT NOT NULL DEFAULT '[]',
  started_at TEXT NOT NULL,
  ended_at TEXT,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(attempt_id) REFERENCES attempts(id)
);

CREATE INDEX IF NOT EXISTS cli_sessions_attempt_idx
  ON cli_sessions(attempt_id, state, started_at);

CREATE INDEX IF NOT EXISTS cli_sessions_native_idx
  ON cli_sessions(cli_backend, session_native_id);
`

const commandRunSchemaSQL = `
CREATE TABLE IF NOT EXISTS command_runs (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  agent_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  assignment_id TEXT NOT NULL,
  contract_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  session_route_id TEXT NOT NULL,
  runtime_id TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  container_name TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL,
  argv_json TEXT NOT NULL DEFAULT '[]',
  env_keys_json TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'timed_out')),
  exit_code INTEGER,
  stdout_ref TEXT NOT NULL DEFAULT '',
  stderr_ref TEXT NOT NULL DEFAULT '',
  stdout_bytes INTEGER NOT NULL DEFAULT 0,
  stderr_bytes INTEGER NOT NULL DEFAULT 0,
  stdout_truncated INTEGER NOT NULL DEFAULT 0,
  stderr_truncated INTEGER NOT NULL DEFAULT 0,
  timeout_seconds INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  evidence_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(lease_id) REFERENCES leases(id),
  FOREIGN KEY(attempt_id) REFERENCES attempts(id),
  FOREIGN KEY(session_route_id) REFERENCES session_routes(id),
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS command_runs_idempotency_idx
  ON command_runs(agent_id, attempt_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE INDEX IF NOT EXISTS command_runs_attempt_idx
  ON command_runs(attempt_id, created_at);

CREATE INDEX IF NOT EXISTS command_runs_contract_idx
  ON command_runs(contract_id, created_at);
`

const runtimeTokenSchemaSQL = `
CREATE TABLE IF NOT EXISTS runtime_tokens (
  token_hash TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  agent_id TEXT NOT NULL,
  runtime_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active', 'revoked')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(attempt_id) REFERENCES attempts(id),
  FOREIGN KEY(lease_id) REFERENCES leases(id)
);

CREATE INDEX IF NOT EXISTS runtime_tokens_attempt_idx
  ON runtime_tokens(attempt_id, state, updated_at);

CREATE INDEX IF NOT EXISTS runtime_tokens_runtime_idx
  ON runtime_tokens(runtime_id, state, updated_at);
`

const validationAssessmentSchemaSQL = `
CREATE TABLE IF NOT EXISTS validation_assessments (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  verifier_agent_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  assignment_id TEXT NOT NULL,
  contract_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  session_route_id TEXT NOT NULL,
  runtime_id TEXT NOT NULL,
  assessed_contract_id TEXT NOT NULL,
  verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'fail', 'blocked')),
  reason TEXT NOT NULL,
  summary TEXT NOT NULL,
  checked_refs_json TEXT NOT NULL DEFAULT '[]',
  ref_snapshot_json TEXT NOT NULL DEFAULT '[]',
  evidence_id TEXT NOT NULL,
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(lease_id) REFERENCES leases(id),
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id),
  FOREIGN KEY(attempt_id) REFERENCES attempts(id),
  FOREIGN KEY(session_route_id) REFERENCES session_routes(id),
  FOREIGN KEY(evidence_id) REFERENCES evidence(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS validation_assessments_idempotency_idx
  ON validation_assessments(verifier_agent_id, attempt_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE INDEX IF NOT EXISTS validation_assessments_contract_idx
  ON validation_assessments(contract_id, created_at);

CREATE INDEX IF NOT EXISTS validation_assessments_assessed_contract_idx
  ON validation_assessments(assessed_contract_id, created_at);
`

const releaseAcceptanceSchemaSQL = `
CREATE TABLE IF NOT EXISTS release_acceptances (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  root_contract_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  team_version INTEGER NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'blocked')),
  run_label TEXT NOT NULL DEFAULT '',
  predicate_results_json TEXT NOT NULL DEFAULT '[]',
  evidence_refs_json TEXT NOT NULL DEFAULT '[]',
  inspect_summary_json TEXT NOT NULL DEFAULT '{}',
  event_cursor_json TEXT NOT NULL DEFAULT '{}',
  failure_summary TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS release_acceptances_run_idx
  ON release_acceptances(root_contract_id, team_id, team_version, run_label);

CREATE INDEX IF NOT EXISTS release_acceptances_root_idx
  ON release_acceptances(root_contract_id, created_at);
`

const contractTeamScopeSchemaSQL = `
CREATE TABLE IF NOT EXISTS contract_team_scopes (
  contract_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  team_id TEXT NOT NULL,
  team_version INTEGER NOT NULL,
  source TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id),
  FOREIGN KEY(team_id, team_version) REFERENCES team_config_versions(team_id, version)
);

CREATE INDEX IF NOT EXISTS contract_team_scopes_team_idx
  ON contract_team_scopes(team_id, team_version);
`

const agentCommunicationEnvelopeSchemaSQL = `
CREATE TABLE IF NOT EXISTS agent_communication_envelopes (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  kind TEXT NOT NULL CHECK (kind IN ('message', 'task', 'result', 'repair', 'budget_attention', 'followup')),
  sender_agent_id TEXT NOT NULL,
  recipient_agent_id TEXT,
  recipient_role TEXT,
  thread_id TEXT,
  message_id TEXT,
  contract_id TEXT,
  parent_envelope_id TEXT,
  summary TEXT NOT NULL DEFAULT '',
  body_inline TEXT,
  body_ref TEXT,
  trigger_turn INTEGER NOT NULL DEFAULT 1,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  FOREIGN KEY(thread_id) REFERENCES threads(id),
  FOREIGN KEY(message_id) REFERENCES messages(id),
  FOREIGN KEY(contract_id) REFERENCES work_contracts(id),
  FOREIGN KEY(parent_envelope_id) REFERENCES agent_communication_envelopes(id)
);

CREATE INDEX IF NOT EXISTS agent_communication_envelopes_recipient_idx
  ON agent_communication_envelopes(recipient_agent_id, kind, created_at);

CREATE INDEX IF NOT EXISTS agent_communication_envelopes_contract_idx
  ON agent_communication_envelopes(contract_id, kind, created_at);

ALTER TABLE mailbox_items ADD COLUMN envelope_id TEXT;

ALTER TABLE mailbox_items ADD COLUMN trigger_turn INTEGER NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS mailbox_items_envelope_idx
  ON mailbox_items(envelope_id);
`

const operatorTaskRunsSchemaSQL = `
CREATE TABLE IF NOT EXISTS operator_task_runs (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT 'default',
  idempotency_key TEXT NOT NULL UNIQUE,
  run_label TEXT NOT NULL DEFAULT '',
  operator_subject_kind TEXT NOT NULL,
  operator_subject_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  team_version INTEGER NOT NULL,
  root_contract_id TEXT NOT NULL,
  root_assignment_id TEXT NOT NULL,
  root_envelope_id TEXT NOT NULL,
  root_mailbox_id TEXT NOT NULL,
  request_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  FOREIGN KEY(root_contract_id) REFERENCES work_contracts(id),
  FOREIGN KEY(root_assignment_id) REFERENCES assignments(id),
  FOREIGN KEY(root_envelope_id) REFERENCES agent_communication_envelopes(id),
  FOREIGN KEY(root_mailbox_id) REFERENCES mailbox_items(id)
);

CREATE INDEX IF NOT EXISTS operator_task_runs_root_idx
  ON operator_task_runs(root_contract_id);
`

const capabilityAuditOutcomeSchemaSQL = `
ALTER TABLE capability_calls ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE capability_calls ADD COLUMN retryable INTEGER;
ALTER TABLE capability_calls ADD COLUMN attempt_id TEXT;
ALTER TABLE capability_calls ADD COLUMN lease_id TEXT;
ALTER TABLE capability_calls ADD COLUMN runtime_id TEXT;

CREATE INDEX IF NOT EXISTS capability_calls_runtime_scope_idx
  ON capability_calls(lease_id, attempt_id, runtime_id, created_at);
`

const managedRuntimeCleanupSchemaSQL = `
ALTER TABLE runtime_instances ADD COLUMN cleanup_state TEXT NOT NULL DEFAULT 'not_requested'
  CHECK (cleanup_state IN ('not_requested', 'pending', 'in_progress', 'removed', 'failed'));
ALTER TABLE runtime_instances ADD COLUMN cleanup_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_instances ADD COLUMN cleanup_error TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_instances ADD COLUMN cleanup_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_instances ADD COLUMN cleanup_lease_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_instances ADD COLUMN cleanup_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runtime_instances ADD COLUMN removed_at TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS runtime_instances_cleanup_idx
  ON runtime_instances(runtime_kind, cleanup_state, cleanup_lease_expires_at);
`
