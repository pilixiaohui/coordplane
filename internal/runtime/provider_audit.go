package runtime

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"coordplane/internal/ids"
)

const (
	providerPermissionDeniedCode = "PROVIDER_PERMISSION_DENIED"
	coordlinkPolicyRejectedCode  = "COORDLINK_PROVIDER_POLICY_REJECTED"
	providerAuditParseFailedCode = "PROVIDER_AUDIT_PARSE_FAILED"
)

type providerToolOutcome struct {
	SourceStage    string
	OutcomeKind    string
	ToolUseID      string
	CapabilityName string
	Status         string
	ErrorCode      string
	Ordinal        int
}

type providerAuditResult struct {
	Rejected bool
	Stage    string
}

func providerAuditRejection(result providerAuditResult) error {
	if !result.Rejected {
		return nil
	}
	switch result.Stage {
	case "coordlink_local_policy":
		return NewRuntimeApprovalPolicyUnavailable("provider tool was rejected by coordlink local policy")
	default:
		return NewRuntimeApprovalPolicyUnavailable("provider permission policy rejected a runtime tool invocation")
	}
}

type providerStreamRecord struct {
	Type              string                     `json:"type"`
	PermissionDenials []providerPermissionDenial `json:"permission_denials"`
	Message           providerStreamMessage      `json:"message"`
}

type providerPermissionDenial struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

type providerStreamMessage struct {
	Content []providerContentBlock `json:"content"`
}

type providerContentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

func (a *CommandCLIAdapter) projectProviderToolOutcomes(ctx context.Context, sessionID string, instance RuntimeInstance, result ContainerExecResult, transcriptRef string) (providerAuditResult, error) {
	if a.profile.Backend != "claude" {
		return providerAuditResult{}, nil
	}
	if _, configured := a.runtimeCommandPolicy(instance); !configured {
		return providerAuditResult{}, nil
	}
	outcomes, err := parseProviderToolOutcomes(result.Stdout)
	if err != nil {
		if persistErr := a.markProviderAuditFailed(ctx, sessionID, providerAuditParseFailedCode); persistErr != nil {
			return providerAuditResult{}, errors.Join(err, persistErr)
		}
		return providerAuditResult{}, err
	}
	transcriptSHA := strings.TrimPrefix(transcriptRef, "obj_sha256_")
	if transcriptRef == "" || transcriptSHA == "" || transcriptSHA == transcriptRef {
		if persistErr := a.markProviderAuditFailed(ctx, sessionID, providerAuditParseFailedCode); persistErr != nil {
			return providerAuditResult{}, persistErr
		}
		return providerAuditResult{}, errors.New("provider audit transcript identity is unavailable")
	}
	audit := providerAuditResult{}
	err = withTx(ctx, a.db, func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		for _, outcome := range outcomes {
			outcomeID, err := ids.New("pto")
			if err != nil {
				return err
			}
			toolUseID := safeProviderToolUseID(outcome.ToolUseID)
			if toolUseID == "" {
				toolUseID = fmt.Sprintf("ordinal:%d", outcome.Ordinal)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO provider_tool_outcomes (
  id, tenant_id, cli_session_id, attempt_id, lease_id, runtime_id,
  source_stage, outcome_kind, tool_use_id, capability_name, status,
  error_code, ordinal, transcript_ref, transcript_sha256, created_at
) VALUES (?, 'default', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cli_session_id, tool_use_id, outcome_kind) DO UPDATE SET
  source_stage = excluded.source_stage,
  capability_name = excluded.capability_name,
  status = excluded.status,
  error_code = excluded.error_code,
  ordinal = excluded.ordinal,
  transcript_ref = excluded.transcript_ref,
  transcript_sha256 = excluded.transcript_sha256`,
				outcomeID, sessionID, instance.AttemptID, instance.LeaseID, instance.RuntimeID,
				outcome.SourceStage, outcome.OutcomeKind, toolUseID, outcome.CapabilityName,
				outcome.Status, outcome.ErrorCode, outcome.Ordinal, transcriptRef, transcriptSHA, now,
			); err != nil {
				return fmt.Errorf("insert provider tool outcome: %w", err)
			}
			if outcome.Status == "rejected" {
				audit.Rejected = true
				if audit.Stage == "" {
					audit.Stage = outcome.SourceStage
				}
			}
		}
		_, err := tx.ExecContext(ctx, `
UPDATE cli_sessions
SET provider_audit_state = 'complete', provider_audit_error_code = '', updated_at = ?
WHERE id = ?`, now, sessionID)
		return err
	})
	return audit, err
}

func safeProviderToolUseID(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '.', char == '_', char == '-':
		default:
			return ""
		}
	}
	return value
}

func (a *CommandCLIAdapter) markProviderAuditFailed(ctx context.Context, sessionID, code string) error {
	_, err := a.db.ExecContext(ctx, `
UPDATE cli_sessions
SET provider_audit_state = 'failed', provider_audit_error_code = ?, updated_at = ?
WHERE id = ?`, code, formatTime(time.Now()), sessionID)
	return err
}

func parseProviderToolOutcomes(raw []byte) ([]providerToolOutcome, error) {
	toolCapabilities := make(map[string]string)
	var outcomes []providerToolOutcome
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 2*1024*1024)
	ordinal := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record providerStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, errors.New("provider stream audit contains invalid JSON")
		}
		for _, block := range record.Message.Content {
			switch block.Type {
			case "tool_use":
				var input struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(block.Input, &input) == nil && block.ID != "" {
					toolCapabilities[block.ID] = safeProviderCapability(input.Command)
				}
			case "tool_result":
				code := providerMachineErrorCode(block.Content)
				if !block.IsError || code != coordlinkPolicyRejectedCode {
					continue
				}
				outcomes = append(outcomes, providerToolOutcome{
					SourceStage:    "coordlink_local_policy",
					OutcomeKind:    "tool_result",
					ToolUseID:      block.ToolUseID,
					CapabilityName: toolCapabilities[block.ToolUseID],
					Status:         "rejected",
					ErrorCode:      code,
					Ordinal:        ordinal,
				})
				ordinal++
			}
		}
		for _, denial := range record.PermissionDenials {
			if denial.ToolName != "" && denial.ToolName != "Bash" {
				continue
			}
			outcomes = append(outcomes, providerToolOutcome{
				SourceStage:    "provider_permission",
				OutcomeKind:    "permission_denial",
				ToolUseID:      denial.ToolUseID,
				CapabilityName: safeProviderCapability(denial.ToolInput.Command),
				Status:         "rejected",
				ErrorCode:      providerPermissionDeniedCode,
				Ordinal:        ordinal,
			})
			ordinal++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan provider stream audit: %w", err)
	}
	return outcomes, nil
}

func safeProviderCapability(command string) string {
	fields := strings.Fields(command)
	for index := 0; index+2 < len(fields); index++ {
		if fields[index] != ContainerCoordlinkPath || fields[index+1] != "call" {
			continue
		}
		name := fields[index+2]
		if safeProviderCapabilityName(name) {
			return name
		}
	}
	return ""
}

func providerMachineErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return providerMachineErrorCode(json.RawMessage(text))
	}
	var object struct {
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(raw, &object) == nil && object.ErrorCode == coordlinkPolicyRejectedCode {
		return object.ErrorCode
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, block := range blocks {
			if code := providerMachineErrorCode(json.RawMessage(block.Text)); code != "" {
				return code
			}
		}
	}
	return ""
}
