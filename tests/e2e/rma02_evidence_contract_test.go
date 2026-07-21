//go:build e2e

package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type rma02Evidence struct {
	SchemaVersion      int                      `json:"schema_version"`
	ScenarioID         string                   `json:"scenario_id"`
	ScenarioExecutions int                      `json:"scenario_executions"`
	Result             string                   `json:"result"`
	Revision           string                   `json:"revision"`
	SourceClean        bool                     `json:"source_clean"`
	ImageDigest        string                   `json:"image_digest"`
	Environment        rma02EnvironmentEvidence `json:"environment"`
	Commands           []rma02CommandEvidence   `json:"commands"`
	StartedAt          string                   `json:"started_at"`
	EndedAt            string                   `json:"ended_at"`
	ProjectID          string                   `json:"project_id"`
	InitialSHA         string                   `json:"initial_sha"`
	Sources            []rma02SourceEvidence    `json:"sources"`
	Overlap            rma02OverlapEvidence     `json:"overlap"`
	Message            rma02MessageEvidence     `json:"message"`
	Restart            rma02RestartEvidence     `json:"restart"`
	DirectCASTaskID    string                   `json:"direct_cas_task_id"`
	DirectCASCount     int                      `json:"direct_cas_count"`
	Integrations       []rma02IntegrationProof  `json:"integrations"`
	Final              rma02FinalEvidence       `json:"final"`
	Cleanup            rma02CleanupEvidence     `json:"cleanup"`
}

type rma02EnvironmentEvidence struct {
	Go     string `json:"go"`
	Git    string `json:"git"`
	Docker string `json:"docker"`
	Claude string `json:"claude"`
}

type rma02CommandEvidence struct {
	Name     string `json:"name"`
	ExitCode int    `json:"exit_code"`
}

type rma02SourceEvidence struct {
	Role                string `json:"role"`
	TaskID              string `json:"task_id"`
	AgentID             string `json:"agent_id"`
	BaseSHA             string `json:"base_sha"`
	RunID               string `json:"run_id"`
	ContainerID         string `json:"container_id"`
	Generation          int64  `json:"generation"`
	LaunchNonce         string `json:"launch_nonce"`
	LiveFrom            string `json:"live_from"`
	LiveUntil           string `json:"live_until"`
	ProgressMarker      string `json:"progress_marker"`
	CoordlinkOperations int    `json:"coordlink_operations"`
	TaskCurrentObserved bool   `json:"task_current_observed"`
	TaskCurrentTaskID   string `json:"task_current_task_id"`
	TaskCurrentRunID    string `json:"task_current_run_id"`
	FixtureMarker       string `json:"fixture_marker"`
	FixtureEventCount   int    `json:"fixture_event_count"`
	FixtureExitCode     int    `json:"fixture_exit_code"`
	CommitSHA           string `json:"commit_sha"`
	HeadSHA             string `json:"head_sha"`
	HeadRunID           string `json:"head_run_id"`
	TaskRef             string `json:"task_ref"`
	SubmitEventCount    int    `json:"submit_event_count"`
}

type rma02OverlapEvidence struct {
	ObservedAt          string   `json:"observed_at"`
	ActiveRunIDs        []string `json:"active_run_ids"`
	RunningContainerIDs []string `json:"running_container_ids"`
}

type rma02MessageEvidence struct {
	ID                   string `json:"id"`
	SenderRunID          string `json:"sender_run_id"`
	DeliveryTaskID       string `json:"delivery_task_id"`
	RecipientAgentID     string `json:"recipient_agent_id"`
	AcknowledgerRunID    string `json:"acknowledger_run_id"`
	State                string `json:"state"`
	CreatedEventCount    int    `json:"created_event_count"`
	AckEventCount        int    `json:"ack_event_count"`
	DurableBeforeRestart bool   `json:"durable_before_restart"`
}

type rma02RunFence struct {
	TaskID      string `json:"task_id"`
	AgentID     string `json:"agent_id"`
	RunID       string `json:"run_id"`
	ContainerID string `json:"container_id"`
	Generation  int64  `json:"generation"`
	LaunchNonce string `json:"launch_nonce"`
}

type rma02RestartEvidence struct {
	Count                int             `json:"count"`
	LiveRunsBefore       int             `json:"live_runs_before"`
	Before               []rma02RunFence `json:"before"`
	After                []rma02RunFence `json:"after"`
	ListenerRestored     bool            `json:"listener_restored"`
	MessageStable        bool            `json:"message_stable"`
	PendingActionsStable bool            `json:"pending_actions_stable"`
	GitFactsStable       bool            `json:"git_facts_stable"`
	ReadyObservedAt      string          `json:"ready_observed_at"`
	FirstContinueAt      string          `json:"first_continue_at"`
	ReadyBeforeContinue  bool            `json:"ready_before_continue"`
	MutationsBeforeReady int             `json:"mutations_before_ready"`
}

type rma02IntegrationProof struct {
	TaskID            string `json:"task_id"`
	SourceTaskID      string `json:"source_task_id"`
	SourceRunID       string `json:"source_run_id"`
	SourceTaskRef     string `json:"source_task_ref"`
	SourceHeadSHA     string `json:"source_head_sha"`
	ObservedCanonical string `json:"observed_canonical_sha"`
	HeadSHA           string `json:"head_sha"`
	SourceAncestor    bool   `json:"source_ancestor"`
	CanonicalAncestor bool   `json:"canonical_ancestor"`
	NestedIntegration bool   `json:"nested_integration"`
	SubmitEventCount  int    `json:"submit_event_count"`
}

type rma02FinalEvidence struct {
	SQLiteCanonical         string   `json:"sqlite_canonical"`
	BossCanonical           string   `json:"boss_canonical"`
	ActualCanonical         string   `json:"actual_canonical"`
	SourceAncestors         []string `json:"source_ancestors"`
	TaskRefsVerified        int      `json:"task_refs_verified"`
	FixtureExitCode         int      `json:"fixture_exit_code"`
	FSCKExitCode            int      `json:"fsck_exit_code"`
	FinalRestartCount       int      `json:"final_restart_count"`
	TasksQueried            int      `json:"tasks_queried"`
	RunsQueried             int      `json:"runs_queried"`
	MessagesQueried         int      `json:"messages_queried"`
	StateStableAfterRestart bool     `json:"state_stable_after_restart"`
}

type rma02CleanupEvidence struct {
	GCRan                 bool `json:"gc_ran"`
	LiveRuns              int  `json:"live_runs"`
	OwnedContainers       int  `json:"owned_containers"`
	BlockedCleanup        int  `json:"blocked_cleanup"`
	PendingGitActions     int  `json:"pending_git_actions"`
	UnknownControlEntries int  `json:"unknown_control_entries"`
	WorkspaceResidue      int  `json:"workspace_residue"`
	HandoffResidue        int  `json:"handoff_residue"`
	LogResidue            int  `json:"log_residue"`
	TaskRefResidue        int  `json:"task_ref_residue"`
	AgentHomeResidue      int  `json:"agent_home_residue"`
}

func validateRMA02Evidence(e rma02Evidence, forbidden ...string) error {
	if e.SchemaVersion != 1 || e.ScenarioID != "RMA-02" || e.ScenarioExecutions != 1 || e.Result != "PASS_REAL_MULTI_AGENT_LOCAL" {
		return fmt.Errorf("invalid gate identity or result")
	}
	if e.ProjectID == "" || !e.SourceClean || !validObjectID(e.InitialSHA) || !validObjectID(e.Revision) || !validRMA02ImageDigest(e.ImageDigest) {
		return fmt.Errorf("missing revision, image, Project, or C0 evidence")
	}
	if e.Environment.Go == "" || e.Environment.Git == "" || e.Environment.Docker == "" || e.Environment.Claude == "" || !validRMA02Commands(e.Commands) {
		return fmt.Errorf("environment or command evidence is incomplete")
	}
	started, startErr := time.Parse(time.RFC3339Nano, e.StartedAt)
	ended, endErr := time.Parse(time.RFC3339Nano, e.EndedAt)
	observed, overlapErr := time.Parse(time.RFC3339Nano, e.Overlap.ObservedAt)
	if startErr != nil || endErr != nil || overlapErr != nil || ended.Before(started) {
		return fmt.Errorf("invalid gate timestamps")
	}
	if err := validateRMA02Sources(e, observed); err != nil {
		return err
	}
	if e.Message.ID == "" || e.Message.SenderRunID != sourceByRole(e.Sources, "A").RunID ||
		e.Message.DeliveryTaskID != sourceByRole(e.Sources, "B").TaskID || e.Message.RecipientAgentID != sourceByRole(e.Sources, "B").AgentID ||
		e.Message.AcknowledgerRunID != sourceByRole(e.Sources, "B").RunID || e.Message.State != "acknowledged" ||
		e.Message.CreatedEventCount != 1 || e.Message.AckEventCount != 1 || !e.Message.DurableBeforeRestart {
		return fmt.Errorf("direct Message acknowledgement is not durable and unique")
	}
	if err := validateRMA02Restart(e.Restart, started, ended); err != nil {
		return err
	}
	if e.DirectCASCount != 1 || !containsSourceTask(e.Sources, e.DirectCASTaskID) || len(e.Integrations) != 3 {
		return fmt.Errorf("acceptance did not produce exactly one direct CAS and three integrations")
	}
	if err := validateRMA02Integrations(e); err != nil {
		return err
	}
	if err := validateRMA02Final(e); err != nil {
		return err
	}
	if !e.Cleanup.GCRan || e.Cleanup.LiveRuns != 0 || e.Cleanup.OwnedContainers != 0 || e.Cleanup.BlockedCleanup != 0 || e.Cleanup.PendingGitActions != 0 || e.Cleanup.UnknownControlEntries != 0 || e.Cleanup.WorkspaceResidue != 0 || e.Cleanup.HandoffResidue != 0 || e.Cleanup.LogResidue != 0 || e.Cleanup.TaskRefResidue != 0 || e.Cleanup.AgentHomeResidue != 0 {
		return fmt.Errorf("runtime or Git cleanup did not converge")
	}
	return rejectRMA02Secrets(e, forbidden)
}

func validateRMA02Sources(e rma02Evidence, observed time.Time) error {
	if len(e.Sources) != 4 || len(e.Overlap.ActiveRunIDs) != 4 || len(e.Overlap.RunningContainerIDs) != 4 {
		return fmt.Errorf("source or overlap cardinality is not four")
	}
	roles, tasks, agents, runs, containers, heads := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, source := range e.Sources {
		from, fromErr := time.Parse(time.RFC3339Nano, source.LiveFrom)
		until, untilErr := time.Parse(time.RFC3339Nano, source.LiveUntil)
		if fromErr != nil || untilErr != nil || observed.Before(from) || observed.After(until) {
			return fmt.Errorf("source %s was not live at overlap witness", source.Role)
		}
		if source.BaseSHA != e.InitialSHA || source.ProgressMarker != "RMA02-READY-"+source.Role || source.CoordlinkOperations < 2 ||
			!source.TaskCurrentObserved || source.TaskCurrentTaskID != source.TaskID || source.TaskCurrentRunID != source.RunID ||
			source.FixtureMarker != "source-"+source.Role || source.FixtureEventCount != 1 || source.FixtureExitCode != 0 ||
			!validObjectID(source.CommitSHA) || source.CommitSHA != source.HeadSHA || source.HeadRunID != source.RunID || source.TaskRef == "" || source.SubmitEventCount != 1 ||
			roles[source.Role] || tasks[source.TaskID] || agents[source.AgentID] || runs[source.RunID] || containers[source.ContainerID] || heads[source.HeadSHA] {
			return fmt.Errorf("source identity or submission proof is incomplete or duplicated")
		}
		roles[source.Role], tasks[source.TaskID], agents[source.AgentID], runs[source.RunID], containers[source.ContainerID], heads[source.HeadSHA] = true, true, true, true, true, true
		if !contains(e.Overlap.ActiveRunIDs, source.RunID) || !contains(e.Overlap.RunningContainerIDs, source.ContainerID) {
			return fmt.Errorf("overlap witness omitted source %s", source.Role)
		}
	}
	for _, role := range []string{"A", "B", "C", "D"} {
		if !roles[role] {
			return fmt.Errorf("source roles are not exactly A-D")
		}
	}
	return nil
}

func validateRMA02Restart(restart rma02RestartEvidence, started, ended time.Time) error {
	ready, readyErr := time.Parse(time.RFC3339Nano, restart.ReadyObservedAt)
	continued, continueErr := time.Parse(time.RFC3339Nano, restart.FirstContinueAt)
	if restart.Count != 1 || restart.LiveRunsBefore < 2 || len(restart.Before) != 4 || len(restart.After) != 4 ||
		!restart.ListenerRestored || !restart.MessageStable || !restart.PendingActionsStable || !restart.GitFactsStable ||
		readyErr != nil || continueErr != nil || ready.Before(started) || continued.Before(ready) || continued.After(ended) ||
		!restart.ReadyBeforeContinue || restart.MutationsBeforeReady != 0 {
		return fmt.Errorf("mid-run restart proof is incomplete")
	}
	after := make(map[string]rma02RunFence, len(restart.After))
	for _, fence := range restart.After {
		if _, duplicate := after[fence.RunID]; duplicate {
			return fmt.Errorf("duplicate active Run after restart")
		}
		after[fence.RunID] = fence
	}
	for _, before := range restart.Before {
		if got, ok := after[before.RunID]; !ok || got != before {
			return fmt.Errorf("Run ownership fence drifted across restart")
		}
	}
	return nil
}

func validateRMA02Integrations(e rma02Evidence) error {
	direct := sourceByRole(e.Sources, "A")
	if direct.TaskID == "" || e.DirectCASTaskID != direct.TaskID {
		return fmt.Errorf("direct CAS is not owned by source A")
	}
	reservedTasks, heads := map[string]bool{}, map[string]bool{}
	for _, source := range e.Sources {
		reservedTasks[source.TaskID] = true
		heads[source.HeadSHA] = true
	}
	expectedCanonical := direct.HeadSHA
	for index, role := range []string{"B", "C", "D"} {
		integration := e.Integrations[index]
		source := sourceByRole(e.Sources, role)
		if integration.TaskID == "" || reservedTasks[integration.TaskID] || integration.SourceTaskID != source.TaskID ||
			integration.SourceRunID != source.RunID || integration.SourceTaskRef != source.TaskRef || integration.SourceHeadSHA != source.HeadSHA ||
			integration.ObservedCanonical != expectedCanonical || !validObjectID(integration.HeadSHA) || heads[integration.HeadSHA] ||
			!integration.SourceAncestor || !integration.CanonicalAncestor || integration.NestedIntegration || integration.SubmitEventCount != 1 {
			return fmt.Errorf("integration identity or lineage proof is invalid")
		}
		reservedTasks[integration.TaskID] = true
		heads[integration.HeadSHA] = true
		expectedCanonical = integration.HeadSHA
	}
	if e.Final.ActualCanonical != expectedCanonical {
		return fmt.Errorf("final canonical is not the last integration head")
	}
	return nil
}

func validateRMA02Final(e rma02Evidence) error {
	final := e.Final
	if !validObjectID(final.SQLiteCanonical) || final.SQLiteCanonical != final.BossCanonical || final.BossCanonical != final.ActualCanonical ||
		final.FixtureExitCode != 0 || final.FSCKExitCode != 0 || final.FinalRestartCount != 1 || !final.StateStableAfterRestart ||
		final.TaskRefsVerified != 7 || final.TasksQueried != 7 || final.RunsQueried != 7 || final.MessagesQueried < 5 || len(final.SourceAncestors) != 4 {
		return fmt.Errorf("final canonical, query, fixture, fsck, or restart proof is incomplete")
	}
	for _, source := range e.Sources {
		if !contains(final.SourceAncestors, source.HeadSHA) {
			return fmt.Errorf("final canonical omits source lineage")
		}
	}
	return nil
}

func rejectRMA02Secrets(e rma02Evidence, forbidden []string) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	var values []string
	collectStrings(document, &values)
	for _, secret := range forbidden {
		for _, candidate := range append([]string{secret, strings.TrimSpace(secret)}, strings.Fields(secret)...) {
			if candidate == "" {
				continue
			}
			digest := sha256.Sum256([]byte(candidate))
			variants := []string{candidate, hex.EncodeToString(digest[:]), strings.ToUpper(hex.EncodeToString(digest[:]))}
			for _, value := range values {
				decoded := value
				if unquoted, decodeErr := strconv.Unquote(`"` + strings.ReplaceAll(value, `"`, `\"`) + `"`); decodeErr == nil {
					decoded = unquoted
				}
				for _, variant := range variants {
					if strings.Contains(value, variant) || strings.Contains(decoded, variant) {
						return fmt.Errorf("evidence contains forbidden credential material")
					}
				}
			}
		}
	}
	return nil
}

func collectStrings(value any, result *[]string) {
	switch typed := value.(type) {
	case string:
		*result = append(*result, typed)
	case []any:
		for _, item := range typed {
			collectStrings(item, result)
		}
	case map[string]any:
		for _, item := range typed {
			collectStrings(item, result)
		}
	}
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validRMA02ImageDigest(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && validObjectID(strings.TrimPrefix(value, "sha256:"))
}

func validRMA02Commands(commands []rma02CommandEvidence) bool {
	want := map[string]bool{
		"source-fixtures": true, "source-submits": true, "daemon-sigkill-restart": true,
		"accept-cascade": true, "final-fixture": true, "git-fsck": true,
		"final-restart-query": true, "gc-preview": true, "gc-run": true,
	}
	for _, command := range commands {
		if !want[command.Name] || command.ExitCode != 0 {
			return false
		}
		delete(want, command.Name)
	}
	return len(want) == 0
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSourceTask(sources []rma02SourceEvidence, taskID string) bool {
	for _, source := range sources {
		if source.TaskID == taskID {
			return true
		}
	}
	return false
}

func sourceByRole(sources []rma02SourceEvidence, role string) rma02SourceEvidence {
	for _, source := range sources {
		if source.Role == role {
			return source
		}
	}
	return rma02SourceEvidence{}
}
