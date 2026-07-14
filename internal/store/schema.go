package store

const schemaVersion = 1
const schemaName = "coordplane_v1_six_objects"

const schemaSQL = `
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  source TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  initial_sha TEXT NOT NULL,
  control_repo_path TEXT NOT NULL UNIQUE,
  canonical_ref TEXT NOT NULL,
  canonical_sha TEXT NOT NULL,
  integration_agent_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('creating','active','error','archived')),
  pending_action TEXT NOT NULL DEFAULT '' CHECK (pending_action IN ('','initialize','verify')),
  pending_action_id TEXT NOT NULL DEFAULT '',
  pending_started_at TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE agents (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  adapter_id TEXT NOT NULL,
  image TEXT NOT NULL,
  instructions_file TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('active','paused','archived')),
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  kind TEXT NOT NULL CHECK (kind IN ('conversation','work','integration')),
  parent_task_id TEXT NOT NULL DEFAULT '',
  retry_of_task_id TEXT NOT NULL DEFAULT '',
  created_by_kind TEXT NOT NULL CHECK (created_by_kind IN ('boss','agent','system')),
  created_by_id TEXT NOT NULL DEFAULT '',
  assignee_agent_id TEXT NOT NULL REFERENCES agents(id),
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL CHECK (status IN ('queued','running','finishing','waiting','submitted','completed','failed','cancelled')),
  current_run_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  next_run_at TEXT NOT NULL,
  retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
  max_retries INTEGER NOT NULL DEFAULT 0 CHECK (max_retries >= 0),
  wait_reason TEXT NOT NULL DEFAULT '',
  result_summary TEXT NOT NULL DEFAULT '',
  failure_reason TEXT NOT NULL DEFAULT '',
  base_sha TEXT NOT NULL DEFAULT '',
  head_sha TEXT NOT NULL DEFAULT '',
  head_run_id TEXT NOT NULL DEFAULT '',
  task_ref TEXT NOT NULL DEFAULT '',
  accepted_by_kind TEXT NOT NULL DEFAULT '',
  accepted_by_id TEXT NOT NULL DEFAULT '',
  accepted_at TEXT NOT NULL DEFAULT '',
  accepted_integration_agent_id TEXT NOT NULL DEFAULT '',
  final_canonical_sha TEXT NOT NULL DEFAULT '',
  integration_task_id TEXT NOT NULL DEFAULT '',
  source_task_id TEXT NOT NULL DEFAULT '',
  source_run_id TEXT NOT NULL DEFAULT '',
  source_task_ref TEXT NOT NULL DEFAULT '',
  source_head_sha TEXT NOT NULL DEFAULT '',
  source_accept_version INTEGER NOT NULL DEFAULT 0,
  observed_canonical_sha TEXT NOT NULL DEFAULT '',
  pending_action TEXT NOT NULL DEFAULT '' CHECK (pending_action IN ('','capture','advance')),
  pending_action_id TEXT NOT NULL DEFAULT '',
  pending_action_version INTEGER NOT NULL DEFAULT 0,
  pending_action_run_id TEXT NOT NULL DEFAULT '',
  pending_expected_sha TEXT NOT NULL DEFAULT '',
  pending_target_sha TEXT NOT NULL DEFAULT '',
  pending_started_at TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  submitted_at TEXT NOT NULL DEFAULT '',
  completed_at TEXT NOT NULL DEFAULT '',
  closed_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  agent_id TEXT NOT NULL REFERENCES agents(id),
  generation INTEGER NOT NULL CHECK (generation >= 1),
  resumed_from_run_id TEXT NOT NULL DEFAULT '',
  adapter_id TEXT NOT NULL,
  image TEXT NOT NULL,
  instructions_hash TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('starting','active','exited','failed','interrupted','cancelled','timed_out')),
  workspace_path TEXT NOT NULL DEFAULT '',
  container_id TEXT NOT NULL DEFAULT '',
  native_session_id TEXT NOT NULL DEFAULT '',
  log_path TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL UNIQUE,
  token_revoked_at TEXT NOT NULL DEFAULT '',
  requested_outcome TEXT NOT NULL DEFAULT '' CHECK (requested_outcome IN ('','wait','submit','fail')),
  requested_summary TEXT NOT NULL DEFAULT '',
  expected_head TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL DEFAULT '',
  stop_requested_at TEXT NOT NULL DEFAULT '',
  stop_reason TEXT NOT NULL DEFAULT '',
  stop_operation_id TEXT NOT NULL DEFAULT '',
  heartbeat_at TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  terminal_reason TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  cleanup_state TEXT NOT NULL CHECK (cleanup_state IN ('not_needed','pending','removed','blocked')),
  launch_nonce TEXT NOT NULL DEFAULT '',
  launch_operation_id TEXT NOT NULL DEFAULT '',
  launch_phase TEXT NOT NULL DEFAULT 'intent' CHECK (launch_phase IN ('intent','created','start_issued','process_observed')),
  home_path TEXT NOT NULL DEFAULT '',
  container_name TEXT NOT NULL,
  deadline_at TEXT NOT NULL DEFAULT '',
  last_observed_at TEXT NOT NULL DEFAULT '',
  launch_mode TEXT NOT NULL CHECK (launch_mode IN ('start','resume')),
  resume_native_session_id TEXT NOT NULL DEFAULT '',
  runtime_error_code TEXT NOT NULL DEFAULT '',
  cleanup_operation_id TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  ended_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE messages (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  task_id TEXT NOT NULL REFERENCES tasks(id),
  related_task_id TEXT NOT NULL DEFAULT '',
  sender_kind TEXT NOT NULL CHECK (sender_kind IN ('boss','agent','system')),
  sender_id TEXT NOT NULL DEFAULT '',
  recipient_kind TEXT NOT NULL CHECK (recipient_kind IN ('boss','agent')),
  recipient_id TEXT NOT NULL DEFAULT '',
  reply_to_message_id TEXT NOT NULL DEFAULT '',
  system_code TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  wake INTEGER NOT NULL CHECK (wake IN (0,1)),
  state TEXT NOT NULL CHECK (state IN ('pending','delivered','acknowledged','cancelled')),
  delivered_run_id TEXT NOT NULL DEFAULT '',
  delivery_count INTEGER NOT NULL DEFAULT 0 CHECK (delivery_count >= 0),
  max_deliveries INTEGER NOT NULL DEFAULT 0 CHECK (max_deliveries >= 0),
  next_delivery_at TEXT NOT NULL,
  last_delivery_error TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL,
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  delivered_at TEXT NOT NULL DEFAULT '',
  acknowledged_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL DEFAULT '',
  entity_type TEXT NOT NULL CHECK (entity_type IN ('project','agent','task','run','message','daemon')),
  entity_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  actor_kind TEXT NOT NULL CHECK (actor_kind IN ('boss','agent','daemon','system')),
  actor_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  operation_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json)),
  created_at TEXT NOT NULL
);

CREATE TABLE request_dedupes (
  actor_scope TEXT NOT NULL,
  operation TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  result_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (actor_scope, operation, idempotency_key)
);

CREATE UNIQUE INDEX tasks_one_open_conversation
  ON tasks(project_id, assignee_agent_id)
  WHERE kind = 'conversation' AND status NOT IN ('completed','cancelled');
CREATE UNIQUE INDEX tasks_current_run_unique
  ON tasks(current_run_id) WHERE current_run_id <> '';
CREATE UNIQUE INDEX tasks_one_open_integration_source
  ON tasks(project_id,source_task_ref)
  WHERE kind='integration' AND source_task_ref<>'' AND status NOT IN ('completed','cancelled');
CREATE INDEX tasks_schedule
  ON tasks(status, next_run_at, priority DESC, created_at, id);
CREATE INDEX tasks_assignee_status
  ON tasks(assignee_agent_id, status);
CREATE UNIQUE INDEX runs_one_live_per_agent
  ON runs(agent_id) WHERE state IN ('starting','active');
CREATE UNIQUE INDEX runs_one_live_per_task
  ON runs(task_id) WHERE state IN ('starting','active');
CREATE INDEX runs_task_created ON runs(task_id, created_at, id);
CREATE INDEX messages_delivery ON messages(recipient_kind, recipient_id, state, next_delivery_at);
CREATE INDEX messages_order ON messages(task_id, created_at, id);
CREATE INDEX events_project_order ON events(project_id, id);
CREATE INDEX events_entity_order ON events(entity_type, entity_id, id);
`
