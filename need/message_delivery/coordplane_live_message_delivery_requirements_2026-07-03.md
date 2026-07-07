# CoordPlane 实时消息递送与 Same-Turn Steering 需求

本文是 CoordPlane message delivery 模块的独立需求说明。它定义后台如何在 CLI Agent 会话仍在运行时，通过 turn steer / same-turn steering 注入授权的 AgentCommunicationEnvelope 摘要或轻量新消息信号，提示 Agent 主动调用 CoordPlane 接口读取、回复并继续当前工作。

## 1. 背景和目标

多 Agent 协作不能只依赖“任务结束后再 resume”。真实开发中，其他 Agent、operator 或后台协议检查可能在当前 Agent 还在思考、运行工具或处理任务时产生新反馈。如果这些反馈只能等当前会话完全结束后再追加，会出现：

- Agent 继续基于过期信息工作。
- 子任务结果不能及时反馈给发布者。
- 验证失败、冲突、权限拒绝等信息被推迟到错误路径之外。
- Backend 被迫在 Agent 不知情时推进流程，重新退回“后台替 Agent 决策”的旧设计。

CoordPlane 的目标是：

- Backend 保存完整消息和 mailbox 真相。
- 活跃 CLI 会话优先使用 same-turn steering 收到授权 envelope 摘要或“有新消息”的轻量 signal。
- signal 默认不携带大正文、artifact 内容或未授权详情；已授权接收者可收到 sender、kind、summary、mailbox id 和短正文片段。
- Agent 在同一会话中主动读取、回复、派发任务、提交证据或 resolve mailbox。
- 如果当前会话不活跃或 CLI adapter 不支持 steer，则 mailbox 保持 durable pending，并走普通 resume / start。

## 2. 非目标

Message delivery 模块不负责：

- 判断消息内容的业务语义。
- 自动替 Agent 回复、派发任务、完成合同或验收项目。
- 把未授权正文、长正文、artifact 内容或完整验证详情直接塞入 turn steer payload。
- 在容器文件、prompt 临时文本或 runner 内存中保存协作真相。
- 要求所有 CLI 都提供完全相同的底层 steer API。

## 3. 术语

| 名称 | 定义 |
| --- | --- |
| MailboxItem | Backend 数据库中的 durable 待处理事项，包含接收者、原因、引用、状态和处理要求 |
| AgentCommunicationEnvelope | Agent 间 message、task、followup、result、repair、validation、budget_attention 的统一协议信封 |
| DeliverySignal | 注入到 CLI Agent 活跃 turn 的通知，可包含授权 envelope 摘要、短正文片段和读取方式 |
| DeliveryAttempt | Backend 对一次递送尝试的持久记录 |
| ActiveTurnRoute | 当前 Agent 活跃 CLI 会话和 turn 的可递送路由 |
| SteerAdapter | Runner 内部的 CLI-specific 注入适配器，例如 Codex `turn.steer` |
| Safe Boundary | CLI 可安全接收追加输入的边界，例如当前工具调用结束后、模型下一轮请求前 |

## 4. 核心不变量

- AgentCommunicationEnvelope 和 MailboxItem 是消息真相源；DeliverySignal 只是递送提示或安全摘要。
- signal 不得包含未授权内容、长 message body、artifact 内容或完整 validation detail。
- 对已授权接收者，signal 可以包含 envelope id、kind、sender、summary、mailbox id 和短正文片段，降低 Agent 第一次处理的歧义。
- Agent 必须能通过 `mailbox.list` / `mailbox.get` / `communication.read` / `contract.context` 等接口读取完整授权内容。
- 同一 MailboxItem 可以有多次 DeliveryAttempt，但只能按幂等规则 resolve 一次。
- Steer 成功只代表“通知已被 CLI 接受”，不代表 Agent 已处理。
- Agent 是否处理完成由 `mailbox.resolve` 及其引用的 follow-up action 证明。
- 无 active turn、turn mismatch、adapter 不支持 steer 时，MailboxItem 不能丢失。
- Backend 不因为 signal 已发送就自动推进合同状态。
- `trigger_turn=true` 只表示应唤醒或启动目标 Agent turn；`trigger_turn=false` 表示进入 mailbox 等待处理，不强制唤醒。

## 5. DeliverySignal 设计

### 5.1 文本 signal

当 CLI 只支持文本追加输入时，Runner 注入类似文本。文本可包含授权 envelope 摘要，但不能包含未授权详情或大正文：

```text
CoordPlane has new mailbox updates for this agent.
Envelope: kind=<kind>, sender=<sender>, mailbox_id=<mailbox_id>, summary=<summary>.
Follow the coordplane-service skill workflow: read the relevant item through coordlink if more context is needed, then reply, create follow-up work, submit evidence, or resolve the mailbox through CoordPlane before continuing current work.
```

中文团队可配置本地化版本，但语义必须保持：

```text
CoordPlane 后台有新的 mailbox 更新。
Envelope: kind=<kind>, sender=<sender>, mailbox_id=<mailbox_id>, summary=<summary>。
请按 coordplane-service skill 工作流处理：如需更多上下文，通过 coordlink 读取相关 item，然后根据内容回复、派发后续任务、提交证据或 resolve mailbox，再继续当前工作。
```

### 5.2 结构化 signal

如果 CLI adapter 支持结构化追加上下文，可传入结构化 payload：

```json
{
  "type": "coordplane.mailbox_signal",
  "reason": "new_mailbox_items",
  "envelopes": [
    {
      "envelope_id": "env_123",
      "mailbox_id": "mbox_123",
      "kind": "result",
      "sender": "developer_a",
      "summary": "child contract completed with report evidence",
      "short_body": "optional authorized short text"
    }
  ],
  "mailbox_count": 2,
  "cursor": "mbox_cur_123",
  "required_action": "call mailbox.list/get or communication.read",
  "trace_id": "trace_456"
}
```

结构化 payload 仍不能包含未授权正文、长正文、artifact 内容或完整验证详情。`cursor` 只能用于读取当前 Agent scope 内的 mailbox 列表，不是绕过鉴权的直接数据。

### 5.3 合并和去重

同一 Agent 的多个 pending mailbox 可以合并成一个 signal。合并规则：

- 已有未确认 signal 时，不重复注入相同文本。
- 新 signal 可只更新 `mailbox_count` 或 `cursor`。
- Agent 读取 mailbox 时必须以 backend 当前状态为准，而不是以 signal 中的数量或摘要为准。

### 5.4 trigger_turn 语义

AgentCommunicationEnvelope 可声明 `trigger_turn`：

| 值 | 语义 |
| --- | --- |
| false | 普通消息、结果反馈或可等待通知；进入 mailbox，不强制启动目标 Agent turn |
| true | 新任务、follow-up task、repair_required 或需要及时处理的反馈；idle 时应触发 start/resume，active 时按 same-turn steer 尝试注入 |

`trigger_turn` 不代表合同完成、mailbox resolved 或业务验收通过。它只影响递送和唤醒策略。

## 6. ActiveTurnRoute 和 Adapter 能力

Backend / Runner 必须为每个 active attempt 维护可审计路由：

| 字段 | 要求 |
| --- | --- |
| agent_id | 当前 Agent 身份 |
| attempt_id | 当前 attempt |
| runtime_id | Docker 或 external runtime |
| cli_backend | codex、claude、opencode 等 |
| native_session_id | CLI 原生 session id 或等价 resume key |
| active_turn_id | CLI 当前 turn id；若 CLI 不暴露，可为空 |
| supports_same_turn_steer | 是否支持活跃 turn 注入 |
| supports_expected_turn_guard | 是否支持 expected turn id 防误投 |
| supports_interrupt | 是否支持中断当前执行 |
| supports_resume | 是否支持会话结束后恢复 |

Adapter 能力用注册表声明，不能在主流程写 CLI 名称 if/else 链。

```text
codex  -> supports_same_turn_steer=true, supports_expected_turn_guard=true
claude -> 按实际 CLI 能力声明；不确定时先声明 false 并走 durable resume
external-test -> 可用 fake adapter 捕获 steer payload，用于协议测试
```

## 7. 标准递送流程

### 7.1 活跃 turn 优先 steer

```text
1. Backend 创建 MailboxItem。
2. MailboxItem 引用一个 AgentCommunicationEnvelope。
3. Delivery service 查询目标 Agent 的 ActiveTurnRoute。
4. 如果存在 active turn 且 adapter 支持 same-turn steer：
   - 创建 DeliveryAttempt(status=queued)。
   - 构造 DeliverySignal。
   - 调用 SteerAdapter.Steer(route, signal, expected_turn_id)。
   - adapter 接受后标记 DeliveryAttempt(status=accepted)。
5. CLI 在安全边界把 signal 加入同一会话上下文。
6. Agent 根据 signal 摘要判断下一步；如需完整上下文，调用 mailbox.list / mailbox.get / communication.read 读取。
7. Agent 根据内容调用 message.send / contract.add / report.submit / contract.complete 等能力。
8. Agent 调用 mailbox.resolve，并引用 follow_up_action。
```

### 7.2 不活跃或不可 steer 时 durable resume

```text
1. Backend 创建 MailboxItem。
2. Delivery service 查不到 active turn，或 adapter 不支持 steer。
3. MailboxItem 保持 pending。
4. Runner 根据 mailbox 的 session_route_id 恢复或启动 CLI Agent。
5. 新会话启动 prompt 可包含 pending envelope 摘要，仍要求 Agent 调用 mailbox / communication capability 读取完整授权内容。
```

这不是旧的“任务结束后后台自动推进”，而是同一个 durable mailbox 机制的降级递送路径。

### 7.3 Steer 失败

```text
1. SteerAdapter 返回 turn_mismatch、not_active、temporarily_unavailable 或 rejected。
2. DeliveryAttempt 记录失败原因。
3. 对 turn_mismatch 可刷新 ActiveTurnRoute 后重试一次。
4. 仍失败时，MailboxItem 保持 pending。
5. Runner 走 resume/start 或等待下一次 active route。
```

Steer 失败不能吞掉消息，不能把 mailbox 标为 resolved。

## 8. Safe Boundary 规则

Same-turn steering 不是强行打断任意执行。

默认安全边界：

- 当前模型响应结束后、下一次模型请求前。
- 当前工具调用返回后。
- 当前 shell 命令结束后。
- CLI 原生支持的 pending input drain 点。

可选 interrupt：

- `session.interrupt` 是单独能力，不等于普通 message delivery。
- 只有 operator 或明确策略允许时才能中断长时间工具执行。
- 中断必须产生独立审计事件。

默认 message delivery 只使用 steer signal，不杀进程、不取消工具调用。

## 9. Agent-Facing 接口

Agent 面向的稳定入口是 skills 指引下的 schema-derived tool adapter 或 `coordlink` capability 调用。具体工具协议不是消息递送的 canonical protocol。

必须提供：

- `mailbox.list`：列出当前 Agent scope 内 pending/claimed mailbox。
- `mailbox.get`：读取某个 mailbox 的完整授权内容和关联 envelope。
- `mailbox.resolve`：引用 follow-up action 标记处理完成。
- `communication.read`：读取当前授权 scope 内的 envelope。
- `message.send`：回复或发送说明。
- `contract.add`：派发可追责任务。
- `contract.context`：读取当前合同上下文。

可提供：

- `mailbox.watch`：长轮询 mailbox 变化。
- `session.current`：读取当前 attempt/session 的公开状态。

不得暴露给普通 Agent：

- `session.steer`
- `session.interrupt`
- `session.pin`
- raw active turn id 修改接口

这些是 Runner/backend 内部能力。普通 Agent 只能收到 signal 并调用 mailbox / communication capability。

## 10. Backend 内部接口

Delivery service 建议使用以下内部接口：

```text
CreateMailbox(ctx, item) -> MailboxItem
NotifyMailbox(ctx, mailbox_id) -> DeliveryResult
FindActiveTurnRoute(ctx, agent_id) -> ActiveTurnRoute?
Steer(ctx, route, signal) -> SteerResult
RecordDeliveryAttempt(ctx, attempt) -> DeliveryAttempt
```

接口边界：

- Coordination 模块只负责创建 mailbox 和引用。
- Delivery 模块只负责递送 signal 和记录尝试。
- Runner adapter 只负责把 signal 投递到 CLI。
- Mailbox resolve 仍由 Agent-facing mailbox service 处理。

## 11. 权限和隔离

- DeliverySignal 只发给 mailbox 的 recipient Agent 当前 session。
- DeliverySignal 中的摘要/短正文必须按 recipient Agent 重新鉴权和裁剪。
- `mailbox.get` 必须重新鉴权，不能因为收到了 signal 就放宽权限。
- Agent A 不能通过 cursor 或 mailbox id 读取 Agent B 的内容。
- External runtime 普通 Agent 与 Docker runtime 使用同一权限模型。
- Operator/debug 注入必须有明确身份和审计事件。

## 12. 可观测性

Backend 必须记录：

- MailboxItem 创建事件。
- DeliveryAttempt queued / accepted / failed / fallback。
- signal payload 类型和摘要，但不记录未授权正文、secret 或 artifact 内容。
- target agent、attempt、runtime、cli_backend。
- adapter 返回的错误码。
- Agent 后续是否调用 mailbox.list/get/resolve。

推荐指标：

- pending mailbox 数量。
- steer accepted 率。
- steer 到 mailbox.get 延迟。
- mailbox.get 到 resolve 延迟。
- fallback resume 次数。
- ignored signal 超时次数。

## 13. 错误处理

| 场景 | 行为 |
| --- | --- |
| 无 active turn | mailbox 保持 pending，走 resume/start |
| expected turn mismatch | 刷新 route 后重试一次，仍失败则 fallback |
| adapter 不支持 steer | 不报硬错误，直接 fallback |
| adapter 接受但 Agent 不读取 | mailbox 仍 pending，delivery monitor 可重发 coalesced signal 或告警 |
| Agent 读取后无权限 | 返回 rejected，记录 scope 错误 |
| mailbox.resolve 无 follow-up action | rejected，提示需要 message/contract/evidence/complete 引用 |
| backend 重启 | 从数据库恢复 pending mailbox 和 delivery attempts，不依赖内存 |
| trigger_turn=false 且目标 idle | mailbox 保持 pending，不强制启动 turn；可由 wait/watch 或后续 resume 处理 |
| trigger_turn=true 且目标 idle | 创建 runner/resume 工作项，启动或恢复目标会话 |

## 14. 测试边界

### 14.1 协议不变量测试

- 创建 MailboxItem 后，不会直接改变合同状态。
- DeliverySignal 不包含未授权正文、长正文、完整 contract objective、validation details 或 artifact content。
- DeliverySignal 可包含已授权 envelope id、kind、sender、summary 和短正文片段。
- DeliveryAttempt accepted 不会把 mailbox 标记为 resolved。
- mailbox.resolve 必须引用 durable follow-up action。

### 14.2 Same-turn steer 测试

- 有 active turn 且 adapter 支持 steer 时，Delivery service 调用 SteerAdapter。
- fake external adapter 能捕获 signal payload。
- signal 提示 Agent 根据摘要处理，必要时调用 mailbox.list/get 或 communication.read。
- expected turn mismatch 时只重试一次。

### 14.3 Fallback 测试

- 无 active turn 时，mailbox 保持 pending，创建 resume request。
- adapter 不支持 steer 时，不丢 mailbox。
- steer 失败后，DeliveryAttempt 记录 failed，mailbox 仍可通过 resume 处理。

### 14.4 Scope 测试

- Agent A 收到 signal 后不能读取 Agent B mailbox。
- cursor 不能扩大权限。
- operator 手动注入和普通 Agent signal 有不同审计身份。

### 14.5 Build-to-delete 测试

- CLI adapter 能力通过注册表声明。
- 删除某个 adapter 只移除注册项，不改 Delivery service 主流程。
- 新增一种 signal renderer 只增加 renderer 注册项，不改 mailbox 状态机。

## 15. 开发落地建议

Go 项目中建议按以下包拆分：

```text
internal/delivery/mailbox
internal/delivery/signal
internal/delivery/attempts
internal/delivery/routes
internal/delivery/service
internal/runtime/cliadapter/steer
internal/adapters/tools/mailbox
internal/adapters/http/mailbox
```

注册式接口建议：

```go
type SteerAdapter interface {
    Backend() string
    Capabilities() SteerCapabilities
    Steer(ctx context.Context, route ActiveTurnRoute, signal DeliverySignal) (SteerResult, error)
}

type SignalRenderer interface {
    Kind() string
    Render(ctx context.Context, itemSummary MailboxSummary) (DeliverySignal, error)
}
```

Delivery service 只依赖接口和能力，不判断具体 CLI 名称。

## 16. 设计结论

CoordPlane 的实时通信机制不是第二套任务系统，也不是把消息硬塞进 prompt。它是：

```text
durable mailbox truth
  -> lightweight same-turn signal
  -> Agent 主动读取 backend
  -> Agent 主动处理和 resolve
  -> fallback resume 保证不丢消息
```

这样既能让运行中的 Agent 尽早看到新反馈，又保持 Backend 的单一真相源和 Agent 主动决策模型。
