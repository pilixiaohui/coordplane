# CoordPlane 任务机制与 Agent 对话机制需求

本文是 CoordPlane coordination 模块的独立需求说明。它定义多 Agent 协作中的统一通信信封、任务、对话、反馈、邮箱、同会话处理入口和证据协议，可直接作为新 Go 项目的开发依据。活跃会话中的 turn steer / same-turn steering 递送由 message delivery 模块定义。

## 1. 背景和目标

多个 CLI Agent 协作时，常见失败来自三类混淆：

- 把“聊天消息”当成正式任务，导致无人负责、无法验收、无法追责。
- 把“任务完成”当成后台自动推进，导致 Agent 没在自己的会话中看到错误反馈。
- 把“运行状态”放在 prompt、文件夹或容器内临时文件里，容器销毁后无法恢复。

CoordPlane 的目标是提供一个轻量、稳定、可审计的后台协作服务：

- Agent 通过统一 AgentCommunicationEnvelope 传递消息、任务、结果、修复反馈和预算提醒。
- Agent 通过 capability 显式创建任务、领取任务、提交证据和完成任务；需要可追责交付时必须产生 WorkContract。
- Agent 通过消息和 mailbox 互相反馈，但任务交付必须落到任务合同。
- 子任务完成后，后台创建 mailbox 并触发递送；活跃会话优先 same-turn steer 原发布者，不活跃时恢复原 CLI 会话继续判断。
- 后台只维护协议状态、权限、证据完整性和会话路由，不替 Agent 做业务决策。
- 所有协作真相都持久化到 backend 数据库。

## 2. 术语

| 名称 | 定义 |
| --- | --- |
| Backend | CoordPlane 常驻服务端，提供 API、保存数据库真相、发出递送事件 |
| Agent | 一个可被调度的智能体身份，例如规划者、开发者、验证者；身份不等于具体进程 |
| CLI Agent | 具体运行的命令行智能体程序，例如 Codex CLI、Claude Code、OpenCode |
| Runtime | CLI Agent 所在环境，可以是 Docker 容器或 external 本机环境 |
| Runner | 管理 runtime 和 CLI Agent 生命周期的组件 |
| Session | CLI Agent 的可恢复会话，由具体 CLI 提供原生 session id 或等价 resume 路由 |
| AgentCommunicationEnvelope | Agent 间通信、任务、后续任务、结果、修复反馈、验证反馈和预算提醒的统一协议信封 |
| WorkContract | 可追责任务合同，描述要完成的工作、完成证据和验收要求 |
| Assignment | 把一个合同分配给某个 Agent 或角色后的待处理队列项 |
| Lease | 某个 Agent 当前处理 assignment 的临时独占权 |
| Attempt | 一次真实 CLI Agent 运行记录 |
| Thread | 一条对话线，可挂在团队、合同、证据或反馈上 |
| Message | Thread 中的一条消息 |
| MailboxItem | 等待某个 Agent 处理的通知或反馈，也是 same-turn steer 或 fallback resume 的 durable 入口 |
| Evidence | 报告、产物、验证结果等任务完成依据 |

## 3. 非目标

CoordPlane coordination 模块不负责：

- 判断具体项目是否写对、测试是否充分、业务语义是否合理。
- 硬编码某个团队角色的职责。
- 直接读写 Agent 工作区文件作为协议真相。
- 自动替 Agent 发布下一任务、自动选择下一个角色、自动完成验收。
- 在 coordination 模块内实现 Git 分支、合并、提交等代码版本管理能力；这些能力属于独立的 code management 模块，coordination 只引用其证据和状态。
- 让 Agent 之间绕过 backend 直连。

## 4. 总体模型

CoordPlane 必须统一递送入口，同时分层管理两类语义：

- AgentCommunicationEnvelope：谁向谁传递什么类型的信息，是否应触发 turn，是否要求合同承接。
- WorkContract：谁要完成什么、证据是什么、什么时候关闭、失败怎么恢复。
- MailboxItem：哪个 Agent 需要处理哪条 envelope，以及如何递送到原会话。

边界规则：

```text
message.send      = 创建 kind=message 的 AgentCommunicationEnvelope，不改变任务完成状态
contract.add      = 创建 kind=task 的 AgentCommunicationEnvelope + WorkContract + Assignment
followup/repair   = 创建 kind=followup/repair/result 等 envelope，并按需关联合同
contract.complete = 请求关闭任务，backend 检查证据和状态
contract.wait     = 当前任务等待子任务或外部反馈，不是完成
mailbox           = 通知某个 Agent 的 durable 待处理事项；递送方式由 message delivery 决定
evidence          = 证明任务完成或验证结果的材料
```

Backend 不通过自然语言猜测一段普通正文是否“像任务”。只有当调用者显式声明 `kind=task`、`intent=task_request` 或 `requires_contract=true` 时，backend 才要求它通过 `contract.add` 创建 WorkContract。普通 `message.send` 不会被 backend 当作正式任务，也不会改变合同状态。

## 5. 数据对象需求

### 5.1 AgentCommunicationEnvelope

AgentCommunicationEnvelope 是所有 Agent 间通信的统一入口。字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成，Agent 不允许提供 |
| kind | message、task、followup、result、repair、validation、budget_attention |
| sender_agent_id | 发送者 Agent |
| recipient_agent_id / recipient_role | 接收者 Agent 或角色 |
| related_contract_id | 相关合同，可为空 |
| related_thread_id / message_id | 相关对话引用，可为空 |
| summary | 可安全显示的摘要 |
| body_ref / inline_body | 正文引用或短正文 |
| references | 合同、证据、artifact、changeset、validation 等引用 |
| trigger_turn | 是否应唤醒目标 Agent 开始或继续 turn |
| requires_contract | 是否必须有关联 WorkContract |
| created_at | 时间戳 |

约束：

- `kind=task` 或 `requires_contract=true` 必须关联 WorkContract。
- `kind=message` 不改变合同状态。
- `kind=result`、`kind=repair`、`kind=validation` 可创建 mailbox 给原发布者或负责 Agent。
- `trigger_turn` 只影响递送/唤醒，不代表任务完成、mailbox resolved 或合同状态变化。
- Envelope 是 communication truth 的统一记录；MailboxItem 是待处理入口；DeliverySignal 只是递送提示。

### 5.2 WorkContract

WorkContract 表达一项可追责工作。字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成，Agent 不允许提供 |
| title | 简短任务名 |
| objective | 任务目标 |
| issuer_agent_id | 发布者 Agent |
| issuer_contract_id | 发布者当前合同，可为空 |
| target | 目标 Agent、目标角色或可领取策略 |
| inputs | 输入引用，例如需求文档、上游报告、消息、证据 |
| allowed_capabilities | 该任务允许使用的能力，例如 report、artifact、validation |
| completion_requirements | 完成所需证据类型和数量 |
| acceptance_policy | 谁或什么条件能最终接受结果 |
| status | open、satisfied、void、reopened |
| budget_accounting | 是否消耗合同预算；普通 contract.add 消耗 |
| communication_envelope_id | 创建该合同的 task envelope |
| created_at / updated_at | 时间戳 |

约束：

- Agent 只能提供任务语义字段，不能提供 id、状态、租约、attempt。
- 后台必须把合同状态和当前执行占用分离。
- 已满足合同可以因为后续验证失败被重新打开。

### 5.3 Assignment

Assignment 表达合同当前如何进入 Agent 待处理队列。字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| contract_id | 关联 WorkContract |
| assignee_agent_id / assignee_role | 目标 Agent 或角色 |
| state | queued、claimed、waiting、returned、expired、cancelled |
| priority | 队列优先级 |
| reason | 创建原因，例如 new_contract、feedback、repair |
| session_route_id | 应恢复的会话路由，可为空 |

约束：

- 只有 `assignment.next` 或 `assignment.watch` 能领取工作。
- 同一 assignment 同一时间只能有一个有效 lease。

### 5.4 Lease

Lease 表达当前处理权。字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| assignment_id | 关联 Assignment |
| agent_id | 当前处理 Agent |
| runtime_id | 当前 runtime |
| session_route_id | 当前会话路由 |
| expires_at | 过期时间 |
| state | active、renewed、released、expired |

约束：

- 所有有副作用接口必须带有效 lease scope，或由 operator 权限显式调用。
- 租约过期后，backend 可以重新入队 assignment。

### 5.5 Attempt

Attempt 表达一次真实 CLI Agent 运行。字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| lease_id | 关联 Lease |
| cli_backend | codex、claude、opencode 等 |
| runtime_kind | docker 或 external |
| session_native_id | CLI 原生 session id |
| start_reason | new_assignment、resume_feedback、manual_debug |
| status | started、active、ended、failed |
| transcript_ref | CLI transcript 存储引用 |
| started_at / ended_at | 时间戳 |

约束：

- Attempt 不等于任务完成。
- 同一合同可有多次 Attempt。
- Transcript 是审计资料，不是唯一状态真相。

### 5.6 Thread、Message、MailboxItem

Thread 字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| scope | team、contract、evidence、feedback 等 |
| subject | 主题 |
| created_by | 创建者 |

Message 字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| thread_id | 关联 Thread |
| sender_agent_id | 发送者 |
| body | 消息正文 |
| references | 合同、报告、产物、验证结果等引用 |
| intent | note、question、feedback、repair、task_request |

MailboxItem 字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| recipient_agent_id / recipient_role | 接收者 |
| reason | question、child_completed、repair_required、budget_attention |
| thread_id / message_id | 关联消息 |
| contract_id | 相关合同 |
| session_route_id | 应递送到的原会话路由；活跃时用于 same-turn steer，不活跃时用于 fallback resume |
| state | pending、claimed、resolved、cancelled |

约束：

- MailboxItem 是待处理事项入口，不是任务本身。
- MailboxItem 必须引用 AgentCommunicationEnvelope 或其派生对象，不能只保存 transient 文本。
- Coordination 只创建 MailboxItem；signal 注入、重试、fallback 和 DeliveryAttempt 由 message delivery 模块负责。
- MailboxItem resolve 必须基于 durable follow-up action，例如回复、创建合同、提交证据或完成合同。

### 5.7 Evidence

Evidence 用于支撑完成或验证。第一阶段建议三类：

| 类型 | 作用 |
| --- | --- |
| report | 结构化完成报告、调查结论、修复说明 |
| artifact | 需要共享给其他 Agent 的文件或产物引用 |
| validation | 验证结论、失败原因、复查建议 |

Evidence 字段建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| kind | report、artifact、validation |
| contract_id | 关联合同 |
| produced_by | 产生 Agent |
| content_ref / inline_content | 内容引用或小文本 |
| summary | 摘要 |
| verdict | validation 可用，例如 pass、fail、needs_work |
| created_at | 时间戳 |

约束：

- `report.submit`、`artifact.upload`、`validation.assessment` 只提交证据，不自动完成合同。
- `contract.complete` 根据 completion_requirements 检查证据是否足够。
- 证据不足必须返回 rejected response，让 Agent 在当前会话继续补交。

## 6. 状态机

### 6.1 合同状态

```text
open -> satisfied
open -> void
satisfied -> reopened
reopened -> satisfied
reopened -> void
```

含义：

- `open`：工作仍需处理。
- `satisfied`：完成请求通过，合同已满足。
- `void`：合同作废，不再处理。
- `reopened`：已满足合同因新反馈、验证失败或 operator 操作重新打开。

### 6.2 Assignment 状态

```text
queued -> claimed
claimed -> waiting
claimed -> returned
claimed -> expired
waiting -> queued
returned -> queued
```

含义：

- `queued`：可领取。
- `claimed`：已有有效租约。
- `waiting`：当前 Agent 显式等待子任务或外部反馈。
- `returned`：Agent 交回但合同未满足。
- `expired`：租约过期。

### 6.3 Mailbox 状态

```text
pending -> claimed -> resolved
pending -> cancelled
claimed -> pending
```

含义：

- `pending`：等待递送、恢复或领取。
- `claimed`：某个 Agent 正在处理。
- `resolved`：已经产生 durable follow-up action。
- `cancelled`：因合同作废等原因取消。

## 7. 标准流程

### 7.1 创建并完成普通任务

```text
Agent 调用 contract.add
Backend 创建 kind=task 的 AgentCommunicationEnvelope、WorkContract 和 Assignment
目标 Agent 调用 assignment.next/watch
Backend 创建 Lease 和 Attempt route
Runner 启动或恢复 CLI Agent
Agent 调用 report.submit / artifact.upload / validation.assessment
Agent 调用 contract.complete
Backend 检查证据和状态
  通过：合同 -> satisfied，租约释放
  不通过：返回 rejected，合同保持 open，同一 lease 可继续修
```

### 7.2 发布子任务并等待

```text
发布者在当前合同中调用 contract.add 创建子合同
Backend 创建 kind=task envelope、子合同和 assignment
发布者调用 contract.wait，说明等待哪个子合同或反馈
Backend 将发布者 assignment 标记为 waiting，并保存 session route
子合同被目标 Agent 完成
Backend 创建 kind=result envelope 和 child_completed mailbox item，指向原发布者和原 session
Backend 通知 message delivery 模块递送 mailbox signal
活跃会话中发布者收到 same-turn signal；不活跃时 Runner fallback resume 原 session
发布者调用 mailbox.list/get 读取反馈，决定继续派发、提交证据或完成原合同
```

`contract.wait` 不能关闭合同，也不能释放后续责任。发布者仍负责验收子任务结果。

### 7.3 验证失败后的修复

```text
验证 Agent 调用 validation.assessment，verdict=fail
Backend 保存 validation evidence
Backend 创建 kind=repair envelope 和 repair_required mailbox item 给原负责 Agent
必要时将已 satisfied 合同改为 reopened
Backend 通知 message delivery 模块递送 mailbox signal
活跃会话中原负责 Agent 收到 same-turn signal；不活跃时 Runner fallback resume 原 session
Agent 调用 mailbox.list/get 读取失败原因，在同一合同内补交证据或修复
```

### 7.4 预算耗尽提醒

合同预算只统计新建合同。反馈、修复、same-turn signal、fallback resume、消息不消耗预算。

如果预算耗尽且最后一个合同不是 TeamConfig 定义的终止验收合同，backend 不应静默成功。它应创建 `budget_attention` mailbox item 给根发布者，提示其决定是否派发终止验收、收缩范围或请求人工增加预算。
该提醒也应记录为 kind=budget_attention 的 AgentCommunicationEnvelope。

“终止验收合同”的定义来自 TeamConfig，不由 backend 硬编码角色名。

## 8. Agent-Facing 接口

所有接口都应作为 backend capability 暴露，也可通过 HTTP、coordlink CLI 或 schema-derived tool adapter 调试。Skills 负责说明 Agent 何时、为何、如何使用这些 capability。不同 adapter 必须调用同一 backend handler。

### 8.1 Assignment 接口

`assignment.next`

- 用途：领取当前 Agent 可处理的下一项 assignment 或 mailbox。
- 输入：可选 capability filter、timeout。
- 输出：assignment、contract 摘要、lease、session route、需要读取的 context resource。
- 失败：无任务返回 `idle`，无权限返回 `unauthorized`。

`assignment.watch`

- 用途：长轮询或流式等待新 assignment/mailbox。
- 输出语义与 `assignment.next` 一致。

### 8.2 Contract 接口

`contract.current`

- 用途：读取当前 lease scope 内合同。
- 不允许返回全量团队合同列表。

`contract.context`

- 用途：读取当前合同所需上下文，包括 objective、inputs、相关 thread、证据摘要、允许能力、完成要求。
- 必须按 scope 裁剪。

`contract.add`

- 用途：创建新 WorkContract。
- Agent 可提供：title、objective、target、inputs、allowed_capabilities、completion_requirements、acceptance_policy。
- Agent 不可提供：id、status、lease、attempt、created_at。
- 成功：返回 backend 生成的 contract_id、assignment_id。
- 失败：返回缺失字段、预算限制、权限不足或目标不可用。

`contract.wait`

- 用途：当前合同进入等待，保留同会话递送路由。
- 输入：等待原因、等待对象引用、期望反馈类型。
- 成功：当前 assignment -> waiting。

`contract.complete`

- 用途：请求完成当前合同。
- 输入：完成摘要、evidence ids、可选给发布者的反馈消息。
- 成功：合同 -> satisfied。
- 失败：返回 rejected，列出缺少哪些证据或状态不符。

### 8.3 Message 和 Mailbox 接口

`message.send`

- 用途：发送普通说明、提问或反馈。
- 成功时创建 `kind=message` 的 AgentCommunicationEnvelope，并按接收者创建 mailbox。
- 如果调用方显式声明 `intent=task_request`、`kind=task` 或 `requires_contract=true`，backend 应返回 rejected，提示使用 `contract.add`；backend 不基于普通文本内容做自然语言任务判断。

`thread.list` / `thread.get`

- 用途：读取当前 scope 允许看到的对话线。

`mailbox.list` / `mailbox.get`

- 用途：读取当前 Agent 的 pending/claimed mailbox item。
- DeliverySignal 只提示有新 mailbox，完整正文必须通过这两个接口读取。
- 不允许读取其他 Agent 的 mailbox。

`mailbox.resolve`

- 用途：标记 mailbox item 已处理。
- 必须引用 follow_up_action，例如 message id、contract id、evidence id 或 contract.complete result。

### 8.4 Evidence 接口

`report.submit`

- 用途：提交文本或结构化报告。

`artifact.upload`

- 用途：上传需要共享给其他 Agent 的非代码文件或产物。

`validation.assessment`

- 用途：提交验证结论。
- 输入必须包含 verdict、reason、checked_refs。
- Backend 不硬编码某个角色才能验证；权限由 TeamConfig policy 决定。

## 9. Rejected Response 规范

所有可修复失败都应返回结构化 rejected response，而不是让 Agent 猜。

示例：

```json
{
  "ok": false,
  "status": "rejected",
  "error_code": "MISSING_REQUIRED_EVIDENCE",
  "message": "contract.complete requires one report evidence before this contract can be satisfied",
  "canonical_ids": {
    "contract_id": "ctr_123",
    "lease_id": "lease_456"
  },
  "missing": [
    {
      "kind": "report",
      "action": "report.submit"
    }
  ],
  "allowed_next_actions": [
    "report.submit",
    "contract.context",
    "message.send"
  ],
  "retryable": true
}
```

必须包含：

- 稳定错误码。
- 人类可读原因。
- 相关 canonical id。
- 缺少什么。
- 下一步可调用哪些能力。
- 是否可重试。

## 10. 权限和隔离

- Agent 只能读取自己的 assignment、lease、mailbox，以及当前合同授权的上下文。
- Agent 不能通过猜 id 读取其他 Agent 的合同详情、消息或证据。
- Backend 依据 token、lease、assignment、team policy 裁剪响应。
- Operator/debug 可以有更高权限，但必须显式标识，不能伪装成普通 Agent。
- Docker Agent 之间不能共享文件系统通信，所有协作通过 backend。

## 11. 不变量

- Agent 不生成 backend id。
- 所有 Agent 间消息、任务、结果、修复反馈和预算提醒必须落到 AgentCommunicationEnvelope。
- 合同状态、assignment 状态、lease 状态、attempt 状态必须分离。
- `message.send` 不改变合同完成状态。
- `trigger_turn` 只控制递送唤醒，不改变合同状态。
- `report.submit`、`artifact.upload`、`validation.assessment` 不自动完成合同。
- `contract.wait` 不是完成。
- 子合同完成必须产生给原发布者的 mailbox item。
- 原发布者在活跃会话中应收到 same-turn signal；不活跃时必须能 fallback resume 同一 CLI session 继续判断。
- DeliverySignal 成功不等于 MailboxItem resolved。
- 已满足合同可以被重新打开。
- 反馈、对话、修复、same-turn signal、fallback resume 不消耗合同预算。
- Backend 不能替 Agent 自动发布下一任务或做业务验收。
- Prompt、容器文件、报告文件都不是协作真相源。

## 12. 最小验收测试

### 12.1 协议测试

- `contract.add` 成功时生成 task envelope、contract、assignment，并拒绝 Agent supplied id。
- `assignment.next` 创建 lease，重复领取不会产生两个有效 lease。
- `contract.complete` 缺报告时 rejected，合同仍 open，lease 仍可继续。
- 补交 report 后再次 `contract.complete` 成功。
- `message.send` 显式 task_request / requires_contract 被拒绝并提示使用 `contract.add`。
- `message.send` 普通文本不会因内容“看起来像任务”被 backend 自然语言拦截。

### 12.2 子任务和递送交接测试

- 发布者创建子合同后调用 `contract.wait`，父合同不关闭。
- 子合同完成后创建 child_completed mailbox item。
- 子合同完成后创建 kind=result envelope。
- mailbox item 包含原发布者、原合同、原 session route 和证据引用。
- coordination 测试只断言已创建可递送 mailbox，不直接断言具体 CLI steer 实现。
- message delivery 测试负责断言 active turn 下 same-turn signal，inactive 下 fallback resume。

### 12.3 修复反馈测试

- validation fail 会创建 repair_required mailbox item。
- validation fail 会创建 kind=repair envelope。
- 如果原合同已 satisfied，会变为 reopened。
- 原负责 Agent 领取修复反馈后，看到 validation evidence 和允许下一步 action。

### 12.4 权限测试

- Agent A 不能读取 Agent B 的 mailbox。
- Agent A 不能伪造 lease id 完成 Agent B 的合同。
- 无 active lease 时，`contract.current` 不返回全量团队状态。

### 12.5 预算测试

- 只有 `contract.add` 消耗预算。
- 子合同反馈、mailbox、same-turn signal、fallback resume、补交 evidence 不消耗预算。
- 预算耗尽且缺少终止验收时，backend 创建 budget_attention mailbox，而不是静默成功。

## 13. 开发落地建议

Go 项目中建议按以下包拆分：

```text
internal/coordination/contracts
internal/coordination/assignments
internal/coordination/mailbox
internal/coordination/messages
internal/coordination/evidence
internal/coordination/policy
internal/coordination/service
internal/adapters/tools
internal/adapters/http
internal/adapters/cli
```

每个接口 handler 应返回同一组 typed response。HTTP、coordlink CLI、schema-derived tool adapter 只负责序列化和鉴权上下文注入，不实现状态机。

## 14. 设计结论

CoordPlane coordination 的核心不是“大自动状态机”，而是五个独立但可组合的机制：

- AgentCommunicationEnvelope 表达所有 Agent 间通信、任务、结果和修复反馈的统一协议入口。
- WorkContract 表达可追责交付。
- Assignment/Lease/Attempt 表达执行权和运行记录。
- Thread/Message 表达信息交流。
- MailboxItem 表达待处理事项和同会话递送入口。
- Evidence 表达完成和验证依据。

这个模型能让 Agent 自主决策，同时让后台服务保持可审计、可恢复、可并发控制。
