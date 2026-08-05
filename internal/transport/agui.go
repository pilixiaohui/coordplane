package transport

import (
	"strings"

	"coordplane/internal/core"
)

// aguiEventPayload 把 CoordPlane 事件投影为 AG-UI 词汇事件
// (docs/protocols.md §4)。投影是只读适配:不改变后端事件模型,
// 不产生新持久对象。返回 nil 表示该事件不入流。
//
// 词汇映射:
//
//	run.created / run.active            -> run_start
//	message.created / message.delivered -> text_message
//	task.progress                       -> tool_call   (进度投影,近似语义)
//	run.exited|failed|interrupted|timed_out|run.cancelled 与 task.failed -> run_complete
func aguiEventPayload(event core.Event) (map[string]any, bool) {
	at := event.CreatedAt
	switch event.Kind {
	case "run.created", "run.active":
		return map[string]any{
			"id":      event.ID,
			"type":    "run_start",
			"run_id":  event.RunID,
			"task_id": entityIDFor(event, "task"),
			"agent_id": event.ActorID,
			"at":      at,
		}, true
	case "message.created", "message.delivered":
		return map[string]any{
			"id":        event.ID,
			"type":      "text_message",
			"message_id": event.EntityID,
			"task_id":   entityIDFor(event, "task"),
			"run_id":    event.RunID,
			"at":        at,
		}, true
	case "task.progress":
		return map[string]any{
			"id":       event.ID,
			"type":     "tool_call",
			"task_id":  event.EntityID,
			"run_id":   event.RunID,
			"summary":  summaryOf(event),
			"at":       at,
		}, true
	case "run.exited", "run.failed", "run.interrupted", "run.timed_out", "run.cancelled", "task.failed":
		return map[string]any{
			"id":      event.ID,
			"type":    "run_complete",
			"run_id":  event.RunID,
			"task_id": entityIDFor(event, "task"),
			"outcome": event.Kind,
			"at":      at,
		}, true
	default:
		return nil, false
	}
}

// entityIDFor 返回事件实体 ID;当事件本身不是目标类型时回退为空。
func entityIDFor(event core.Event, entityType string) string {
	if strings.TrimSpace(event.EntityType) == entityType {
		return event.EntityID
	}
	return ""
}

// summaryOf 从 task.progress 的 payload 中提取短摘要(截断 200 字符)。
func summaryOf(event core.Event) string {
	const prefix = `"summary":`
	raw := event.PayloadJSON
	if idx := strings.Index(raw, prefix); idx >= 0 {
		rest := raw[idx+len(prefix):]
		rest = strings.TrimSpace(rest)
		if len(rest) > 0 && rest[0] == '"' {
			if end := strings.Index(rest[1:], `"`); end >= 0 {
				s := rest[1 : 1+end]
				if len(s) > 200 {
					return s[:200] + "..."
				}
				return s
			}
		}
	}
	return ""
}
