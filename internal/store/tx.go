package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"coordplane/internal/core"
)

type unitOfWork struct {
	ctx context.Context
	tx  *sql.Tx
}

func (u *unitOfWork) Dedupe(actor, operation, key string) ([]byte, bool, error) {
	var raw []byte
	err := u.tx.QueryRowContext(u.ctx, `SELECT result_json FROM request_dedupes WHERE actor_scope=? AND operation=? AND idempotency_key=?`, actor, operation, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func (u *unitOfWork) PutDedupe(actor, operation, key string, result []byte, createdAt string) error {
	_, err := u.tx.ExecContext(u.ctx, `INSERT INTO request_dedupes(actor_scope,operation,idempotency_key,result_json,created_at) VALUES(?,?,?,?,?)`, actor, operation, key, result, createdAt)
	return err
}

func (u *unitOfWork) Project(id string) (core.Project, error) {
	project, err := scanProject(u.tx.QueryRowContext(u.ctx, projectSelect+` WHERE id=?`, id))
	return project, mapNotFound("project", id, err)
}

func (u *unitOfWork) ProjectByName(name string) (core.Project, error) {
	project, err := scanProject(u.tx.QueryRowContext(u.ctx, projectSelect+` WHERE name=?`, name))
	return project, mapNotFound("project", name, err)
}

func (u *unitOfWork) InsertProject(project core.Project) error {
	_, err := u.tx.ExecContext(u.ctx, `
INSERT INTO projects(id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,integration_agent_id,status,pending_action,pending_action_id,pending_started_at,last_error,version,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		project.ID, project.Name, project.Source, project.SourceRef, project.InitialSHA,
		project.ControlRepoPath, project.CanonicalRef, project.CanonicalSHA,
		project.IntegrationAgentID, project.Status, project.PendingAction,
		project.PendingActionID, project.PendingStartedAt, project.LastError,
		project.Version, project.CreatedAt, project.UpdatedAt,
	)
	return err
}

func (u *unitOfWork) UpdateProject(project core.Project, expectedVersion int64, expectedStatus core.ProjectStatus) error {
	result, err := u.tx.ExecContext(u.ctx, `
UPDATE projects SET name=?,source=?,source_ref=?,initial_sha=?,control_repo_path=?,canonical_ref=?,canonical_sha=?,integration_agent_id=?,status=?,pending_action=?,pending_action_id=?,pending_started_at=?,last_error=?,version=?,updated_at=?
WHERE id=? AND version=? AND status=?`,
		project.Name, project.Source, project.SourceRef, project.InitialSHA, project.ControlRepoPath,
		project.CanonicalRef, project.CanonicalSHA, project.IntegrationAgentID, project.Status,
		project.PendingAction, project.PendingActionID, project.PendingStartedAt, project.LastError,
		project.Version, project.UpdatedAt, project.ID, expectedVersion, expectedStatus,
	)
	return u.casResult(result, err, "project", project.ID)
}

func (u *unitOfWork) ProjectsByIntegrationAgent(agentID string) ([]core.Project, error) {
	rows, err := u.tx.QueryContext(u.ctx, projectSelect+` WHERE integration_agent_id=? ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	return collectProjects(rows)
}

func (u *unitOfWork) ProjectBlockers(projectID string) (core.LifecycleBlockers, error) {
	var blockers core.LifecycleBlockers
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM runs WHERE project_id=? AND state IN ('starting','active')`, projectID).Scan(&blockers.LiveRuns); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM tasks WHERE project_id=? AND status NOT IN ('completed','cancelled')`, projectID).Scan(&blockers.OpenTasks); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM tasks WHERE project_id=? AND pending_action<>''`, projectID).Scan(&blockers.PendingActions); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM messages WHERE project_id=? AND recipient_kind='agent' AND state IN ('pending','delivered')`, projectID).Scan(&blockers.UnresolvedAgentMessages); err != nil {
		return blockers, err
	}
	return blockers, nil
}

func (u *unitOfWork) Agent(id string) (core.Agent, error) {
	agent, err := scanAgent(u.tx.QueryRowContext(u.ctx, agentSelect+` WHERE id=?`, id))
	return agent, mapNotFound("agent", id, err)
}

func (u *unitOfWork) InsertAgent(agent core.Agent) error {
	_, err := u.tx.ExecContext(u.ctx, `INSERT INTO agents(id,display_name,adapter_id,image,instructions_file,status,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		agent.ID, agent.DisplayName, agent.AdapterID, agent.Image, agent.InstructionsFile,
		agent.Status, agent.Version, agent.CreatedAt, agent.UpdatedAt,
	)
	return err
}

func (u *unitOfWork) UpdateAgent(agent core.Agent, expectedVersion int64, expectedStatus core.AgentStatus) error {
	result, err := u.tx.ExecContext(u.ctx, `UPDATE agents SET display_name=?,adapter_id=?,image=?,instructions_file=?,status=?,version=?,updated_at=? WHERE id=? AND version=? AND status=?`,
		agent.DisplayName, agent.AdapterID, agent.Image, agent.InstructionsFile, agent.Status,
		agent.Version, agent.UpdatedAt, agent.ID, expectedVersion, expectedStatus,
	)
	return u.casResult(result, err, "agent", agent.ID)
}

func (u *unitOfWork) AgentBlockers(agentID string) (core.LifecycleBlockers, error) {
	var blockers core.LifecycleBlockers
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM runs WHERE agent_id=? AND state IN ('starting','active')`, agentID).Scan(&blockers.LiveRuns); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM tasks WHERE assignee_agent_id=? AND status NOT IN ('completed','cancelled')`, agentID).Scan(&blockers.OpenTasks); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM tasks WHERE accepted_integration_agent_id=? AND status NOT IN ('completed','cancelled')`, agentID).Scan(&blockers.AcceptedIntegrationSource); err != nil {
		return blockers, err
	}
	if err := u.tx.QueryRowContext(u.ctx, `SELECT COUNT(*) FROM messages WHERE recipient_kind='agent' AND recipient_id=? AND state IN ('pending','delivered')`, agentID).Scan(&blockers.UnresolvedAgentMessages); err != nil {
		return blockers, err
	}
	return blockers, nil
}

func (u *unitOfWork) Task(id string) (core.Task, error) {
	task, err := scanTask(u.tx.QueryRowContext(u.ctx, taskSelect+` WHERE id=?`, id))
	return task, mapNotFound("task", id, err)
}

func (u *unitOfWork) RunnableTasks(projectID string) ([]core.Task, error) {
	query := taskSelect + ` WHERE status='queued' AND current_run_id=''`
	args := []any{}
	if projectID != "" {
		query += ` AND project_id=?`
		args = append(args, projectID)
	}
	rows, err := u.tx.QueryContext(u.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collectTasks(rows)
}

func (u *unitOfWork) Conversation(projectID, agentID string) (core.Task, error) {
	task, err := scanTask(u.tx.QueryRowContext(u.ctx, taskSelect+` WHERE project_id=? AND assignee_agent_id=? AND kind='conversation' AND status NOT IN ('completed','cancelled')`, projectID, agentID))
	return task, mapNotFound("conversation", projectID+"/"+agentID, err)
}

func (u *unitOfWork) InsertTask(task core.Task) error {
	_, err := u.tx.ExecContext(u.ctx, `
INSERT INTO tasks(id,project_id,kind,parent_task_id,retry_of_task_id,created_by_kind,created_by_id,assignee_agent_id,title,description,priority,status,current_run_id,generation,next_run_at,retry_count,max_retries,wait_reason,result_summary,failure_reason,base_sha,head_sha,head_run_id,task_ref,accepted_by_kind,accepted_by_id,accepted_at,accepted_integration_agent_id,final_canonical_sha,integration_task_id,source_task_id,source_run_id,source_task_ref,source_head_sha,source_ref_released_at,source_accept_version,observed_canonical_sha,pending_action,pending_action_id,pending_action_version,pending_action_run_id,pending_expected_sha,pending_target_sha,pending_started_at,version,created_at,updated_at,submitted_at,completed_at,closed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskValues(task)...,
	)
	return err
}

func (u *unitOfWork) UpdateTask(task core.Task, expectedVersion int64, expectedStatus core.TaskStatus) error {
	result, err := u.tx.ExecContext(u.ctx, `
UPDATE tasks SET status=?,current_run_id=?,generation=?,next_run_at=?,retry_count=?,max_retries=?,wait_reason=?,result_summary=?,failure_reason=?,base_sha=?,head_sha=?,head_run_id=?,task_ref=?,accepted_by_kind=?,accepted_by_id=?,accepted_at=?,accepted_integration_agent_id=?,final_canonical_sha=?,integration_task_id=?,source_task_id=?,source_run_id=?,source_task_ref=?,source_head_sha=?,source_ref_released_at=?,source_accept_version=?,observed_canonical_sha=?,pending_action=?,pending_action_id=?,pending_action_version=?,pending_action_run_id=?,pending_expected_sha=?,pending_target_sha=?,pending_started_at=?,version=?,updated_at=?,submitted_at=?,completed_at=?,closed_at=?
WHERE id=? AND version=? AND status=?`,
		task.Status, task.CurrentRunID, task.Generation, task.NextRunAt, task.RetryCount,
		task.MaxRetries, task.WaitReason, task.ResultSummary, task.FailureReason, task.BaseSHA,
		task.HeadSHA, task.HeadRunID, task.TaskRef, task.AcceptedByKind, task.AcceptedByID,
		task.AcceptedAt, task.AcceptedIntegrationAgentID, task.FinalCanonicalSHA,
		task.IntegrationTaskID, task.SourceTaskID, task.SourceRunID, task.SourceTaskRef,
		task.SourceHeadSHA, task.SourceRefReleasedAt, task.SourceAcceptVersion, task.ObservedCanonicalSHA,
		task.PendingAction, task.PendingActionID, task.PendingActionVersion,
		task.PendingActionRunID, task.PendingExpectedSHA, task.PendingTargetSHA,
		task.PendingStartedAt, task.Version, task.UpdatedAt, task.SubmittedAt,
		task.CompletedAt, task.ClosedAt, task.ID, expectedVersion, expectedStatus,
	)
	return u.casResult(result, err, "task", task.ID)
}

func (u *unitOfWork) Run(id string) (core.Run, error) {
	run, err := scanRun(u.tx.QueryRowContext(u.ctx, runSelect+` WHERE id=?`, id))
	return run, mapNotFound("run", id, err)
}

func (u *unitOfWork) RunByTokenHash(tokenHash string) (core.Run, error) {
	run, err := scanRun(u.tx.QueryRowContext(u.ctx, runSelect+` WHERE token_hash=?`, tokenHash))
	return run, mapNotFound("run", "token", err)
}

func (u *unitOfWork) InsertRun(run core.Run) error {
	_, err := u.tx.ExecContext(u.ctx, `
INSERT INTO runs(id,project_id,task_id,agent_id,generation,resumed_from_run_id,adapter_id,image,instructions_hash,state,workspace_path,container_id,native_session_id,log_path,token_hash,token_revoked_at,requested_outcome,requested_summary,expected_head,requested_at,stop_requested_at,stop_reason,stop_operation_id,heartbeat_at,exit_code,terminal_reason,last_error,cleanup_state,launch_nonce,launch_operation_id,launch_phase,home_path,container_name,deadline_at,last_observed_at,launch_mode,resume_native_session_id,runtime_error_code,cleanup_operation_id,version,created_at,started_at,ended_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runValues(run)...,
	)
	return err
}

func (u *unitOfWork) UpdateRun(run core.Run, expectedVersion int64, expectedState core.RunState) error {
	result, err := u.tx.ExecContext(u.ctx, `
UPDATE runs SET resumed_from_run_id=?,instructions_hash=?,state=?,workspace_path=?,container_id=?,native_session_id=?,log_path=?,token_revoked_at=?,requested_outcome=?,requested_summary=?,expected_head=?,requested_at=?,stop_requested_at=?,stop_reason=?,stop_operation_id=?,heartbeat_at=?,exit_code=?,terminal_reason=?,last_error=?,cleanup_state=?,launch_nonce=?,launch_operation_id=?,launch_phase=?,home_path=?,container_name=?,deadline_at=?,last_observed_at=?,launch_mode=?,resume_native_session_id=?,runtime_error_code=?,cleanup_operation_id=?,version=?,started_at=?,ended_at=?
WHERE id=? AND version=? AND state=?`,
		run.ResumedFromRunID, run.InstructionsHash, run.State, run.WorkspacePath,
		run.ContainerID, run.NativeSessionID, run.LogPath, run.TokenRevokedAt,
		run.RequestedOutcome, run.RequestedSummary, run.ExpectedHead, run.RequestedAt,
		run.StopRequestedAt, run.StopReason, run.StopOperationID, run.HeartbeatAt,
		run.ExitCode, run.TerminalReason, run.LastError, run.CleanupState,
		run.LaunchNonce, run.LaunchOperationID, run.LaunchPhase, run.HomePath,
		run.ContainerName, run.DeadlineAt, run.LastObservedAt, run.LaunchMode,
		run.ResumeNativeSessionID, run.RuntimeErrorCode, run.CleanupOperationID,
		run.Version, run.StartedAt, run.EndedAt,
		run.ID, expectedVersion, expectedState,
	)
	return u.casResult(result, err, "run", run.ID)
}

func (u *unitOfWork) LiveRunCount(projectID, agentID string) (int, error) {
	query := `SELECT COUNT(*) FROM runs WHERE state IN ('starting','active')`
	var args []any
	if projectID != "" {
		query += ` AND project_id=?`
		args = append(args, projectID)
	}
	if agentID != "" {
		query += ` AND agent_id=?`
		args = append(args, agentID)
	}
	var count int
	err := u.tx.QueryRowContext(u.ctx, query, args...).Scan(&count)
	return count, err
}

func (u *unitOfWork) AgentRuntimeOccupancy(agentID string) (int, error) {
	var count int
	err := u.tx.QueryRowContext(u.ctx, `
SELECT COUNT(*) FROM runs
WHERE agent_id=?
  AND (state IN ('starting','active') OR cleanup_state IN ('pending','blocked'))`,
		strings.TrimSpace(agentID),
	).Scan(&count)
	return count, err
}

func (u *unitOfWork) Message(id string) (core.Message, error) {
	message, err := scanMessage(u.tx.QueryRowContext(u.ctx, messageSelect+` WHERE id=?`, id))
	return message, mapNotFound("message", id, err)
}

func (u *unitOfWork) MessagesForTask(taskID string) ([]core.Message, error) {
	rows, err := u.tx.QueryContext(u.ctx, messageSelect+` WHERE task_id=? ORDER BY created_at,id`, taskID)
	if err != nil {
		return nil, err
	}
	return collectMessages(rows)
}

func (u *unitOfWork) MessagesForRun(runID string) ([]core.Message, error) {
	rows, err := u.tx.QueryContext(u.ctx, messageSelect+` WHERE delivered_run_id=? ORDER BY created_at,id`, runID)
	if err != nil {
		return nil, err
	}
	return collectMessages(rows)
}

func (u *unitOfWork) MessagesForRecipient(kind, id string) ([]core.Message, error) {
	rows, err := u.tx.QueryContext(u.ctx, messageSelect+` WHERE recipient_kind=? AND recipient_id=? ORDER BY created_at,id`, kind, id)
	if err != nil {
		return nil, err
	}
	return collectMessages(rows)
}

func (u *unitOfWork) PendingWakeAt(taskID string) (string, bool, error) {
	var next string
	err := u.tx.QueryRowContext(u.ctx, `
		SELECT next_delivery_at FROM messages
		WHERE task_id=? AND recipient_kind='agent' AND wake=1 AND state='pending'
		  AND next_delivery_at<>'' AND (max_deliveries=0 OR delivery_count<max_deliveries)
		ORDER BY next_delivery_at,id LIMIT 1`, taskID).Scan(&next)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return next, err == nil, err
}

func (u *unitOfWork) InsertMessage(message core.Message) error {
	_, err := u.tx.ExecContext(u.ctx, `
INSERT INTO messages(id,project_id,task_id,related_task_id,sender_kind,sender_id,recipient_kind,recipient_id,reply_to_message_id,system_code,body,wake,state,delivered_run_id,delivery_count,max_deliveries,next_delivery_at,last_delivery_error,idempotency_key,version,created_at,delivered_at,acknowledged_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		message.ID, message.ProjectID, message.TaskID, message.RelatedTaskID,
		message.SenderKind, message.SenderID, message.RecipientKind, message.RecipientID,
		message.ReplyToMessageID, message.SystemCode, message.Body, message.Wake,
		message.State, message.DeliveredRunID, message.DeliveryCount, message.MaxDeliveries,
		message.NextDeliveryAt, message.LastDeliveryError, message.IdempotencyKey,
		message.Version, message.CreatedAt, message.DeliveredAt, message.AcknowledgedAt,
	)
	return err
}

func (u *unitOfWork) UpdateMessage(message core.Message, expectedVersion int64, expectedState core.MessageState) error {
	result, err := u.tx.ExecContext(u.ctx, `
	UPDATE messages SET task_id=?,related_task_id=?,recipient_kind=?,recipient_id=?,system_code=?,body=?,wake=?,state=?,delivered_run_id=?,delivery_count=?,max_deliveries=?,next_delivery_at=?,last_delivery_error=?,version=?,delivered_at=?,acknowledged_at=?
	WHERE id=? AND version=? AND state=?`,
		message.TaskID, message.RelatedTaskID, message.RecipientKind, message.RecipientID,
		message.SystemCode, message.Body, message.Wake, message.State, message.DeliveredRunID,
		message.DeliveryCount, message.MaxDeliveries, message.NextDeliveryAt,
		message.LastDeliveryError, message.Version, message.DeliveredAt,
		message.AcknowledgedAt, message.ID, expectedVersion, expectedState,
	)
	return u.casResult(result, err, "message", message.ID)
}

func (u *unitOfWork) AppendEvent(event core.Event) (core.Event, error) {
	if len(event.PayloadJSON) > core.MaximumEventPayloadBytes {
		return core.Event{}, core.NewError(core.CodeInternal, "event payload exceeds 32768 bytes", false)
	}
	result, err := u.tx.ExecContext(u.ctx, `
INSERT INTO events(project_id,entity_type,entity_id,kind,actor_kind,actor_id,run_id,request_id,operation_id,payload_json,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		event.ProjectID, event.EntityType, event.EntityID, event.Kind, event.ActorKind,
		event.ActorID, event.RunID, event.RequestID, event.OperationID, event.PayloadJSON,
		event.CreatedAt,
	)
	if err != nil {
		return core.Event{}, err
	}
	event.ID, err = result.LastInsertId()
	return event, err
}

func (u *unitOfWork) casResult(result sql.Result, err error, entity, id string) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		query, ok := map[string]string{
			"project": `SELECT status,version FROM projects WHERE id=?`,
			"agent":   `SELECT status,version FROM agents WHERE id=?`,
			"task":    `SELECT status,version FROM tasks WHERE id=?`,
			"run":     `SELECT state,version FROM runs WHERE id=?`,
			"message": `SELECT state,version FROM messages WHERE id=?`,
		}[entity]
		if !ok {
			return core.NewError(core.CodeInternal, "unknown CAS entity", false)
		}
		var state string
		var version int64
		if err := u.tx.QueryRowContext(u.ctx, query, id).Scan(&state, &version); err != nil {
			return mapNotFound(entity, id, err)
		}
		return core.Conflict(core.CodeVersionConflict, fmt.Sprintf("%s %q changed", entity, id), state, version)
	}
	return nil
}

func taskValues(task core.Task) []any {
	return []any{
		task.ID, task.ProjectID, task.Kind, task.ParentTaskID, task.RetryOfTaskID,
		task.CreatedByKind, task.CreatedByID, task.AssigneeAgentID, task.Title,
		task.Description, task.Priority, task.Status, task.CurrentRunID, task.Generation,
		task.NextRunAt, task.RetryCount, task.MaxRetries, task.WaitReason,
		task.ResultSummary, task.FailureReason, task.BaseSHA, task.HeadSHA, task.HeadRunID,
		task.TaskRef, task.AcceptedByKind, task.AcceptedByID, task.AcceptedAt,
		task.AcceptedIntegrationAgentID, task.FinalCanonicalSHA, task.IntegrationTaskID,
		task.SourceTaskID, task.SourceRunID, task.SourceTaskRef, task.SourceHeadSHA,
		task.SourceRefReleasedAt,
		task.SourceAcceptVersion, task.ObservedCanonicalSHA, task.PendingAction,
		task.PendingActionID, task.PendingActionVersion, task.PendingActionRunID,
		task.PendingExpectedSHA, task.PendingTargetSHA, task.PendingStartedAt,
		task.Version, task.CreatedAt, task.UpdatedAt, task.SubmittedAt, task.CompletedAt,
		task.ClosedAt,
	}
}

func runValues(run core.Run) []any {
	return []any{
		run.ID, run.ProjectID, run.TaskID, run.AgentID, run.Generation,
		run.ResumedFromRunID, run.AdapterID, run.Image, run.InstructionsHash, run.State,
		run.WorkspacePath, run.ContainerID, run.NativeSessionID, run.LogPath, run.TokenHash,
		run.TokenRevokedAt, run.RequestedOutcome, run.RequestedSummary, run.ExpectedHead,
		run.RequestedAt, run.StopRequestedAt, run.StopReason, run.StopOperationID,
		run.HeartbeatAt, run.ExitCode, run.TerminalReason, run.LastError, run.CleanupState,
		run.LaunchNonce, run.LaunchOperationID, run.LaunchPhase, run.HomePath,
		run.ContainerName, run.DeadlineAt, run.LastObservedAt, run.LaunchMode,
		run.ResumeNativeSessionID, run.RuntimeErrorCode, run.CleanupOperationID,
		run.Version, run.CreatedAt, run.StartedAt, run.EndedAt,
	}
}

var _ core.Transaction = (*unitOfWork)(nil)

func quotePlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
