package store

import (
	"database/sql"

	"coordplane/internal/core"
)

const projectSelect = `SELECT id,name,source,source_ref,initial_sha,control_repo_path,canonical_ref,canonical_sha,integration_agent_id,status,pending_action,pending_action_id,pending_started_at,last_error,version,created_at,updated_at FROM projects`
const agentSelect = `SELECT id,display_name,adapter_id,image,instructions_file,model,subagent_model,base_url,effort,instructions_text,status,version,created_at,updated_at FROM agents`
const taskSelect = `SELECT id,project_id,kind,parent_task_id,retry_of_task_id,created_by_kind,created_by_id,assignee_agent_id,title,description,priority,status,current_run_id,generation,next_run_at,retry_count,max_retries,budget_seconds,wait_reason,result_summary,failure_reason,base_sha,head_sha,head_run_id,task_ref,accepted_by_kind,accepted_by_id,accepted_at,accepted_integration_agent_id,final_canonical_sha,integration_task_id,source_task_id,source_run_id,source_task_ref,source_head_sha,source_ref_released_at,source_accept_version,observed_canonical_sha,pending_action,pending_action_id,pending_action_version,pending_action_run_id,pending_expected_sha,pending_target_sha,pending_started_at,assignee_participant_id,evidence_type,version,created_at,updated_at,submitted_at,completed_at,closed_at FROM tasks`
const runSelect = `SELECT id,project_id,task_id,agent_id,generation,resumed_from_run_id,adapter_id,image,instructions_hash,config_fingerprint,state,workspace_path,container_id,native_session_id,log_path,token_hash,token_revoked_at,requested_outcome,requested_summary,expected_head,requested_at,stop_requested_at,stop_reason,stop_operation_id,heartbeat_at,exit_code,terminal_reason,last_error,cleanup_state,launch_nonce,launch_operation_id,launch_phase,home_path,container_name,deadline_at,last_observed_at,launch_mode,resume_native_session_id,runtime_error_code,cleanup_operation_id,isolation_spec_version,version,created_at,started_at,ended_at FROM runs`
const messageSelect = `SELECT id,project_id,task_id,related_task_id,sender_kind,sender_id,recipient_kind,recipient_id,recipient_participant_id,reply_to_message_id,system_code,body,wake,state,delivered_run_id,delivery_count,max_deliveries,next_delivery_at,last_delivery_error,idempotency_key,version,created_at,delivered_at,acknowledged_at FROM messages`
const eventSelect = `SELECT id,project_id,entity_type,entity_id,kind,actor_kind,actor_id,run_id,request_id,operation_id,payload_json,created_at FROM events`
const taskSummarySelect = `SELECT id,project_id,kind,parent_task_id,assignee_agent_id,title,priority,status,current_run_id,generation,next_run_at,retry_count,max_retries,budget_seconds,wait_reason,result_summary,failure_reason,base_sha,head_sha,head_run_id,evidence_type,task_ref,accepted_by_kind,accepted_by_id,accepted_integration_agent_id,final_canonical_sha,integration_task_id,source_task_id,source_run_id,source_task_ref,source_head_sha,source_ref_released_at,pending_action,pending_action_id,version,created_at,updated_at,submitted_at,completed_at,closed_at FROM tasks`
const runSummarySelect = `SELECT id,project_id,task_id,agent_id,generation,state,container_id,native_session_id,heartbeat_at,deadline_at,last_observed_at,launch_phase,cleanup_state,terminal_reason,last_error,runtime_error_code,version,created_at,started_at,ended_at FROM runs`
const projectSummarySelect = `SELECT id,substr(name,1,256),substr(canonical_ref,1,256),canonical_sha,integration_agent_id,status,pending_action,substr(last_error,1,256),version,created_at,updated_at FROM projects`
const agentSummarySelect = `SELECT id,substr(display_name,1,256),substr(adapter_id,1,256),substr(image,1,256),status,version,created_at,updated_at FROM agents`

type scanner interface {
	Scan(...any) error
}

func scanProject(row scanner) (core.Project, error) {
	var project core.Project
	err := row.Scan(
		&project.ID, &project.Name, &project.Source, &project.SourceRef, &project.InitialSHA,
		&project.ControlRepoPath, &project.CanonicalRef, &project.CanonicalSHA,
		&project.IntegrationAgentID, &project.Status, &project.PendingAction,
		&project.PendingActionID, &project.PendingStartedAt, &project.LastError,
		&project.Version, &project.CreatedAt, &project.UpdatedAt,
	)
	return project, err
}

func scanAgent(row scanner) (core.Agent, error) {
	var agent core.Agent
	err := row.Scan(
		&agent.ID, &agent.DisplayName, &agent.AdapterID, &agent.Image,
		&agent.InstructionsFile, &agent.Model, &agent.SubagentModel, &agent.BaseURL,
		&agent.Effort, &agent.InstructionsText, &agent.Status, &agent.Version,
		&agent.CreatedAt, &agent.UpdatedAt,
	)
	return agent, err
}

func scanTask(row scanner) (core.Task, error) {
	var task core.Task
	err := row.Scan(
		&task.ID, &task.ProjectID, &task.Kind, &task.ParentTaskID, &task.RetryOfTaskID,
		&task.CreatedByKind, &task.CreatedByID, &task.AssigneeAgentID, &task.Title,
		&task.Description, &task.Priority, &task.Status, &task.CurrentRunID,
		&task.Generation, &task.NextRunAt, &task.RetryCount, &task.MaxRetries,
		&task.BudgetSeconds,
		&task.WaitReason, &task.ResultSummary, &task.FailureReason, &task.BaseSHA,
		&task.HeadSHA, &task.HeadRunID, &task.TaskRef, &task.AcceptedByKind,
		&task.AcceptedByID, &task.AcceptedAt, &task.AcceptedIntegrationAgentID,
		&task.FinalCanonicalSHA, &task.IntegrationTaskID, &task.SourceTaskID,
		&task.SourceRunID, &task.SourceTaskRef, &task.SourceHeadSHA, &task.SourceRefReleasedAt,
		&task.SourceAcceptVersion, &task.ObservedCanonicalSHA, &task.PendingAction,
		&task.PendingActionID, &task.PendingActionVersion, &task.PendingActionRunID,
		&task.PendingExpectedSHA, &task.PendingTargetSHA, &task.PendingStartedAt,
		&task.AssigneeParticipantID, &task.EvidenceType,
		&task.Version, &task.CreatedAt, &task.UpdatedAt, &task.SubmittedAt,
		&task.CompletedAt, &task.ClosedAt,
	)
	task.EvidenceType = effectiveEvidenceType(task)
	return task, err
}

// effectiveEvidenceType preserves the durable evidence grade for new rows and
// derives "captured" for pre-evidence rows that already carry a complete
// cli_agent capture triple (head_sha / head_run_id / task_ref). It never
// reclassifies a human_confirm row or a row with no captured workspace result.
func effectiveEvidenceType(task core.Task) string {
	if task.EvidenceType != "" {
		return task.EvidenceType
	}
	if (task.Kind == core.TaskWork || task.Kind == core.TaskIntegration) &&
		task.AssigneeAgentID != "" && task.HeadSHA != "" &&
		task.HeadRunID != "" && task.TaskRef != "" {
		return string(core.EvidenceCaptured)
	}
	return ""
}

func scanRun(row scanner) (core.Run, error) {
	var run core.Run
	var exitCode sql.NullInt64
	err := row.Scan(
		&run.ID, &run.ProjectID, &run.TaskID, &run.AgentID, &run.Generation,
		&run.ResumedFromRunID, &run.AdapterID, &run.Image, &run.InstructionsHash,
		&run.ConfigFingerprint,
		&run.State, &run.WorkspacePath, &run.ContainerID, &run.NativeSessionID,
		&run.LogPath, &run.TokenHash, &run.TokenRevokedAt, &run.RequestedOutcome,
		&run.RequestedSummary, &run.ExpectedHead, &run.RequestedAt,
		&run.StopRequestedAt, &run.StopReason, &run.StopOperationID, &run.HeartbeatAt,
		&exitCode, &run.TerminalReason, &run.LastError, &run.CleanupState,
		&run.LaunchNonce, &run.LaunchOperationID, &run.LaunchPhase, &run.HomePath,
		&run.ContainerName, &run.DeadlineAt, &run.LastObservedAt, &run.LaunchMode,
		&run.ResumeNativeSessionID, &run.RuntimeErrorCode, &run.CleanupOperationID,
		&run.IsolationSpecVersion, &run.Version, &run.CreatedAt, &run.StartedAt, &run.EndedAt,
	)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	return run, err
}

func scanMessage(row scanner) (core.Message, error) {
	var message core.Message
	var wake int
	err := row.Scan(
		&message.ID, &message.ProjectID, &message.TaskID, &message.RelatedTaskID,
		&message.SenderKind, &message.SenderID, &message.RecipientKind,
		&message.RecipientID, &message.RecipientParticipantID, &message.ReplyToMessageID, &message.SystemCode,
		&message.Body, &wake, &message.State, &message.DeliveredRunID,
		&message.DeliveryCount, &message.MaxDeliveries, &message.NextDeliveryAt,
		&message.LastDeliveryError, &message.IdempotencyKey, &message.Version,
		&message.CreatedAt, &message.DeliveredAt, &message.AcknowledgedAt,
	)
	message.Wake = wake == 1
	return message, err
}

func scanEvent(row scanner) (core.Event, error) {
	var event core.Event
	err := row.Scan(
		&event.ID, &event.ProjectID, &event.EntityType, &event.EntityID, &event.Kind,
		&event.ActorKind, &event.ActorID, &event.RunID, &event.RequestID,
		&event.OperationID, &event.PayloadJSON, &event.CreatedAt,
	)
	return event, err
}

func scanTaskSummary(row scanner) (core.TaskSummary, error) {
	var task core.Task
	err := row.Scan(
		&task.ID, &task.ProjectID, &task.Kind, &task.ParentTaskID,
		&task.AssigneeAgentID, &task.Title, &task.Priority, &task.Status,
		&task.CurrentRunID, &task.Generation, &task.NextRunAt, &task.RetryCount,
		&task.MaxRetries, &task.BudgetSeconds, &task.WaitReason, &task.ResultSummary, &task.FailureReason,
		&task.BaseSHA, &task.HeadSHA, &task.HeadRunID, &task.EvidenceType, &task.TaskRef, &task.AcceptedByKind,
		&task.AcceptedByID, &task.AcceptedIntegrationAgentID, &task.FinalCanonicalSHA,
		&task.IntegrationTaskID, &task.SourceTaskID, &task.SourceRunID,
		&task.SourceTaskRef, &task.SourceHeadSHA, &task.SourceRefReleasedAt, &task.PendingAction,
		&task.PendingActionID, &task.Version, &task.CreatedAt, &task.UpdatedAt,
		&task.SubmittedAt, &task.CompletedAt, &task.ClosedAt,
	)
	return taskSummary(task), err
}

func scanRunSummary(row scanner) (core.RunSummary, error) {
	var run core.Run
	err := row.Scan(
		&run.ID, &run.ProjectID, &run.TaskID, &run.AgentID, &run.Generation,
		&run.State, &run.ContainerID, &run.NativeSessionID, &run.HeartbeatAt,
		&run.DeadlineAt, &run.LastObservedAt, &run.LaunchPhase, &run.CleanupState,
		&run.TerminalReason, &run.LastError, &run.RuntimeErrorCode, &run.Version,
		&run.CreatedAt, &run.StartedAt, &run.EndedAt,
	)
	return runSummary(run), err
}

func scanProjectSummary(row scanner) (core.ProjectSummary, error) {
	var summary core.ProjectSummary
	err := row.Scan(
		&summary.ID, &summary.Name, &summary.CanonicalRef, &summary.CanonicalSHA,
		&summary.IntegrationAgentID, &summary.Status, &summary.PendingAction,
		&summary.LastError, &summary.Version, &summary.CreatedAt, &summary.UpdatedAt,
	)
	return summary, err
}

func scanAgentSummary(row scanner) (core.AgentSummary, error) {
	var summary core.AgentSummary
	err := row.Scan(
		&summary.ID, &summary.DisplayName, &summary.AdapterID, &summary.Image,
		&summary.Status, &summary.Version, &summary.CreatedAt, &summary.UpdatedAt,
	)
	return summary, err
}

func collectProjects(rows *sql.Rows) ([]core.Project, error) {
	return collectRows(rows, scanProject)
}

func collectAgents(rows *sql.Rows) ([]core.Agent, error) {
	return collectRows(rows, scanAgent)
}

func collectTasks(rows *sql.Rows) ([]core.Task, error) {
	return collectRows(rows, scanTask)
}

func collectRuns(rows *sql.Rows) ([]core.Run, error) {
	return collectRows(rows, scanRun)
}

func collectMessages(rows *sql.Rows) ([]core.Message, error) {
	return collectRows(rows, scanMessage)
}

func collectEvents(rows *sql.Rows) ([]core.Event, error) {
	return collectRows(rows, scanEvent)
}

func collectTaskSummaries(rows *sql.Rows) ([]core.TaskSummary, error) {
	return collectRows(rows, scanTaskSummary)
}

func collectRunSummaries(rows *sql.Rows) ([]core.RunSummary, error) {
	return collectRows(rows, scanRunSummary)
}

func collectProjectSummaries(rows *sql.Rows) ([]core.ProjectSummary, error) {
	return collectRows(rows, scanProjectSummary)
}

func collectAgentSummaries(rows *sql.Rows) ([]core.AgentSummary, error) {
	return collectRows(rows, scanAgentSummary)
}

func collectRows[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var items []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
