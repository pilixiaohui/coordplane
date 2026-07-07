# CoordPlane 单用户后台 MVP 需求

本文是 CoordPlane backend 第一阶段的独立需求说明。它把当前开发目标收敛为：先实现单用户后台核心功能闭环，同时通过稳定接口为后续多用户、多 runner、分布式队列、完整 secret vault、UI 和长期记忆留出扩展位置。

## 1. 目标

第一阶段要完成一个可以真实运行的 CoordPlane 后台服务：

- 单用户、单机、单 backend 进程。
- 多个 Agent 可通过 Docker 或 external runtime 接入。
- Agent 通过 skills 理解工作流，并通过 `coordlink` 调用 backend capability。
- Backend 保存 AgentCommunicationEnvelope、任务、消息、邮箱、证据、会话、递送、artifact 和 transcript 的唯一真相。
- Backend 不替 Agent 做业务判断，不自动推进项目语义。
- Runner 可以启动、恢复和 same-turn steer CLI Agent。
- Operator 可以通过 inspect API 查看后台当前状态。

单用户不是临时脚本。单用户只表示默认 `tenant_id`、默认用户和默认 policy；数据模型、队列、事件、capability、skill、session、adapter 和 object store 仍按长期后台服务边界设计。

## 2. 非目标

第一阶段暂不实现：

- 多用户账号系统和完整 RBAC。
- 远程 runner 集群。
- Redis、NATS、Kafka 等分布式队列。
- Kubernetes 部署。
- 复杂 dashboard UI。
- 完整 secret vault、长期语义记忆和向量检索。
- 远端 PR 平台集成。
- 项目业务语义检查。
- Backend 自动替 Agent 发布下一任务或验收具体项目。
- 把 MCP 或任何单一工具协议作为核心协议前提；如果需要 MCP，只能作为 schema-derived tool adapter 的一种兼容实现，调用同一组 capability handler。

这些能力必须保留接口，但不能在 MVP 主流程里写死临时分支或兼容路径。

## 3. 单用户身份模型

第一阶段固定：

```text
tenant_id = "default"
user_id   = "default_user"
workspace_id = "default_workspace"
```

但所有有副作用记录仍必须保存以下字段：

| 字段 | 用途 |
| --- | --- |
| tenant_id | 后续多租户扩展；MVP 固定 default |
| subject_kind | operator、runner、agent、system |
| subject_id | 调用者身份 |
| agent_id | Agent 调用时的具体 Agent 身份 |
| runtime_id | Docker/external runtime 身份 |
| trace_id | 串联一次 capability 调用、递送或 runner 操作 |

权限分层：

- `operator`：外部用户或调试入口，可 inspect、创建根任务、暂停/恢复。
- `runner`：后台运行组件，可启动 CLI、pin session、finish attempt、steer active turn。
- `agent`：容器内或 external CLI Agent，只能按 skills 指引通过 tool adapter 或 `coordlink` 访问授权 capability；MCP 如存在只是 tool adapter 的一种兼容实现。
- `system`：backend 内部机械事件，例如 retry、recovery、queue tick。

普通 Agent 不得调用：

- `session.steer`
- `session.pin`
- `session.finish`
- raw DB / raw event log 写入
- 其他 Agent token 或 mailbox 读取

## 4. Policy 接口

MVP 使用 `SingleUserPolicy`，默认允许同一 `tenant_id=default` 下的合法 scope 操作。

必须定义长期接口：

```text
Authorize(ctx, subject, action, resource) -> Decision
FilterReadable(ctx, subject, resource_set) -> filtered_resource_set
CanUseCapability(ctx, subject, capability_name, scope) -> Decision
```

要求：

- 所有 service handler 必须经过 policy。
- MVP policy 可以简单，但不能绕过 policy 直接访问 store。
- 后续替换成 RBAC 时，handler 主流程不应修改。

## 5. Store 和事件模型

Backend 必须有单一 canonical store。第一阶段建议 SQLite，后续可替换 Postgres。

推荐接口：

```text
Store.Tx(ctx, fn) -> error
Store.AppendEvent(ctx, event) -> event_id
Store.Query(ctx, query) -> rows
Store.Migrate(ctx) -> migration_result
```

必须保存的 canonical objects：

- Agent
- AgentCommunicationEnvelope
- WorkContract
- Assignment
- Lease
- Attempt
- SessionRoute
- Thread
- Message
- MailboxItem
- Evidence
- DeliveryAttempt
- CapabilityCall
- Artifact
- Transcript
- QueueItem
- Event

规则：

- 状态变化必须在 DB 事务中完成。
- Event log 记录事实，不替代业务表。
- Projection / summary / inspect view 必须可由 canonical store 重建。
- Prompt、容器文件、CLI transcript 都不是状态真相源。

## 6. Queue / Lock / Retry 最小模型

MVP 不需要分布式队列，但必须有 DB-backed queue。

推荐队列：

- `assignment_queue`
- `delivery_queue`
- `runner_queue`
- `recovery_queue`

QueueItem 字段：

| 字段 | 用途 |
| --- | --- |
| id | backend 生成 |
| queue_name | assignment、delivery、runner、recovery |
| kind | 具体工作类型 |
| payload_ref | 指向 canonical object |
| state | queued、leased、done、failed、dead |
| lease_owner | 当前 worker |
| attempt_count | 重试次数 |
| next_run_at | 下次可执行时间 |
| last_error | 最近错误摘要 |
| idempotency_key | 去重 |

要求：

- Worker 只能 claim 可运行 queue item。
- 重试必须有 backoff。
- 超过 retry limit 进入 dead 状态，但 canonical object 不能丢失。
- 任何 retry 成功都必须幂等。

## 7. Backend MVP 必须实现的 service

### 7.1 Capability registry

Backend 定义唯一 capability registry。HTTP、coordlink CLI、schema-derived tool adapter 都调用同一 capability handler。任何具体工具协议都不是 canonical protocol，不能成为第二套语义。

Capability 字段：

| 字段 | 要求 |
| --- | --- |
| name | 稳定能力名，例如 `contract.add` |
| input_schema | 机器可校验输入 |
| output_schema | 成功响应结构 |
| rejected_schema | 可修复失败结构 |
| side_effect | none、read、write、external_exec 等 |
| required_scope | agent lease、runner internal、operator 等 |
| idempotency | 是否要求 idempotency key |
| skill_refs | 推荐读取的 skills |

第一阶段 Agent-facing capability 分为核心闭环和扩展能力。核心闭环必须先完成；扩展能力可以在同一 registry 中预留，但不能阻塞通信、会话和 mailbox MVP 通过。

核心闭环必须包含：

```text
assignment:
  assignment.next
  assignment.watch

contract:
  contract.current
  contract.context
  contract.add
  contract.wait
  contract.complete

message/mailbox:
  message.send
  communication.read
  thread.list
  thread.get
  mailbox.list
  mailbox.get
  mailbox.resolve
  mailbox.watch

evidence/artifact:
  report.submit
  artifact.upload
  artifact.download
  validation.assessment

session public:
  session.current
```

受控 Git 能力属于独立 capability group。第一阶段可以在 schema 和 policy 中预留，但只有在进入 code management 阶段时才作为发布 gate：

```text
workspace/git:
  workspace.prepare
  workspace.status
  workspace.sync
  git.status
  git.diff
  git.log
  git.commit
  git.rebase
  changeset.submit
  changeset.abandon
  git.merge_preview
  git.merge_apply
  git.conflicts
  git.resolve
  git.abort
  git.rollback
```

说明：

- `message.send` 创建 `kind=message` 的 AgentCommunicationEnvelope。
- `contract.add` 创建 `kind=task` 的 AgentCommunicationEnvelope、WorkContract 和 Assignment。
- `communication.read` 读取当前 Agent scope 内已授权 envelope；`mailbox.get` 返回 mailbox 及其关联 envelope。
- Backend 不基于自然语言判断普通消息是否“像任务”；只有显式 `kind=task`、`intent=task_request` 或 `requires_contract=true` 才要求走 `contract.add`。
- Git capability 不能成为第一阶段通信闭环的前置条件，但其 handler 仍必须遵守同一 capability registry、policy、rejected response 和 session scope。

第一阶段 internal/operator capability：

```text
runner/session internal:
  session.pin
  session.progress
  session.finish
  session.steer

operator:
  inspect.agents
  inspect.assignments
  inspect.mailbox
  inspect.attempts
  inspect.delivery
  inspect.queue
  inspect.events
  inspect.trace
```

要求：

- 新增 capability 只添加注册项和 handler，不改主执行循环。
- 所有 rejected response 结构一致。
- Capability 不硬编码具体项目语义或团队角色名。
- 普通 Agent 的 capability discovery 不得返回 internal/operator capability。

### 7.2 Skill registry

Skills 是 Agent-facing 工作流和使用说明层。它说明 Agent 什么时候使用 capability、如何读取上下文、如何处理 rejected response、如何提交 durable follow-up action。

Skill 对象建议：

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| name | 稳定 skill 名 |
| description | 简述用途 |
| content | 主说明，等价 SKILL.md |
| files | 附属参考文件 |
| scope | global、team、agent、project、contract |
| enabled | 是否启用 |
| version | skill 版本 |

MVP 必须内置：

- `coordplane-service`：如何读取 contract/mailbox/envelope，如何提交结果，如何处理 rejected。
- `contract-delegation`：如何发布子合同、wait、处理 child_completed。
- `controlled-git`：如何 sync、diff、commit、merge、resolve、rollback。
- `artifact-sharing`：如何上传/下载共享文件。
- `validation-review`：如何提交 validation evidence。
- `runtime-troubleshooting`：如何理解 ENV_NOT_READY、CLI missing、provider auth 等运行时错误。

要求：

- Bootstrap prompt 只提示当前身份、当前 contract/mailbox 和需要读取的 skills。
- 不把所有 capability schema 全量塞进 prompt。
- Agent 可以按需读取 skill 内容；skills 可被 TeamConfig、agent、contract 选择启用。
- 删除某个 skill 只移除注册/绑定，不改 capability handler。

### 7.3 TeamConfig service

TeamConfig 是单用户 MVP 的配置入口。它定义哪些 Agent 存在、绑定哪些 skills、允许哪些 capability、使用什么 runtime/CLI，以及终止验收策略。

最小 schema：

```yaml
team:
  name: default-team

agents:
  - id: planner
    role: planner
    runtime: docker
    cli: codex
    skills:
      - coordplane-service
      - contract-delegation

  - id: developer
    role: developer
    runtime: docker
    cli: claude
    skills:
      - coordplane-service
      - controlled-git
      - artifact-sharing

  - id: verifier
    role: verifier
    runtime: docker
    cli: codex
    skills:
      - coordplane-service
      - validation-review

policies:
  termination:
    final_acceptance_capability: validation.assessment
    final_acceptance_roles:
      - verifier

  validation:
    allowed_capabilities:
      - validation.assessment

  git:
    require_explicit_paths: true
```

要求：

- Backend 不硬编码 planner/developer/verifier 等角色含义。
- Policy 只读取 TeamConfig 中声明的 capability 和 role 约束。
- TeamConfig 修改必须有版本号；新会话使用新版本，已运行 attempt 记录其启动时版本。
- 单用户 MVP 可以使用本地 YAML，但加载后必须进入 canonical store。

### 7.4 SecretProvider MVP

完整 secret vault 暂缓，但 MVP 必须有最小 SecretProvider，避免真实 CLI 凭据泄露。

接口：

```text
GetRuntimeEnv(ctx, agent_id, runtime_id) -> env map
ListSecretMetadata(ctx, agent_id) -> has_secret/key_count/key_names
Redact(ctx, value) -> "***"
```

要求：

- MVP 可从本地 env/config 读取 provider token。
- 真实 token 只能注入目标 runtime 进程环境。
- 真实 token 不进入 prompt、skill、event log、transcript、inspect response。
- Inspect 只能看到 key 名和是否配置，不能看到值。
- Agent 不能通过 capability 读取 SecretProvider 原始值。

### 7.5 Coordination service

实现任务、分配、租约、消息、邮箱和证据的状态机。

必须满足：

- `contract.add` 创建 WorkContract + Assignment。
- `message.send` 创建 AgentCommunicationEnvelope；显式 task intent 必须转为 `contract.add`。
- `assignment.next/watch` 创建 Lease，不重复发放有效 lease。
- `contract.wait` 进入 waiting，不关闭合同。
- `contract.complete` 检查 evidence，不足时 rejected。
- `message.send` 不改变合同完成状态。
- `mailbox.resolve` 必须引用 durable follow-up action。

### 7.6 Delivery service

实现 mailbox -> signal -> agent action 的递送闭环。

必须满足：

- AgentCommunicationEnvelope + MailboxItem 是唯一消息真相。
- DeliverySignal 不包含未授权正文、大正文、artifact 内容或完整验证详情；可包含已授权 envelope 摘要或短正文片段。
- active turn 存在且 adapter 支持时，创建 DeliveryAttempt 并 same-turn steer。
- steer 失败或无 active turn 时，MailboxItem 保持 pending，并进入 fallback resume。
- DeliveryAttempt accepted 不等于 mailbox resolved。

### 7.7 Session service

实现 attempt、session route、pin、finish、recovery 的后台真相。

必须满足：

- workspace/coordlink/toolchain 未准备好前，attempt 不能 running。
- CLI native session id 一旦可得必须 pin。
- Terminal report 幂等。
- 容器销毁后可通过持久 home + session route 恢复。

### 7.8 Runtime / runner service

第一阶段必须至少支持：

- external fake runtime，用于协议和手动模拟测试。
- Docker runtime，用于真实 agent 隔离。
- 一个真实 CLI adapter，例如 Codex 或 Claude Code。

Runner adapter 接口：

```text
Start(ctx, Runtime, Assignment, Lease) -> AttemptRoute
Resume(ctx, SessionRoute, MailboxItem) -> AttemptRoute
Steer(ctx, ActiveTurnRoute, DeliverySignal) -> SteerResult
Stop(ctx, AttemptRoute) -> Result
Inspect(ctx, SessionRoute) -> SessionInfo
```

要求：

- Docker/external 差异不得进入 coordination handler。
- CLI 类型差异不得进入 delivery 主流程。
- Adapter 能力通过注册表声明。

### 7.9 Object store

Artifact 和 transcript 不直接塞进 DB 大字段。MVP 使用本地 object store。

接口：

```text
PutObject(ctx, meta, reader) -> object_ref
GetObject(ctx, object_ref) -> reader
StatObject(ctx, object_ref) -> object_meta
DeleteObject(ctx, object_ref) -> result
```

要求：

- DB 保存 metadata、checksum、size、content_type、owner、引用关系。
- artifact 上传成功后返回 durable artifact id。
- transcript 保存原始 CLI stream 或文件引用。
- Object store 路径不能暴露给普通 Agent 作为协议真相。

### 7.10 Inspect API

必须提供 operator/debug 只读视图。

最小 inspect 能力：

- 查看 agents。
- 查看 pending assignments。
- 查看 pending mailbox。
- 查看 attempts 和 session routes。
- 查看 delivery attempts。
- 查看 queue 状态。
- 查看最近 event log。
- 查看某个 trace_id 的相关事件。

要求：

- Inspect 是 operator/debug 面，不是 Agent 协作协议。
- Inspect 不应成为第二套写入路径。
- Inspect view 可由 canonical store 重建。

## 8. 暂缓但必须保留接口的能力

| 能力 | MVP 做法 | 预留接口 |
| --- | --- | --- |
| 多用户/RBAC | 固定 default tenant/user | `Policy` |
| 分布式队列 | DB queue | `Queue` |
| 远程 runner | 单机 runner | `RunnerRegistry` |
| 完整 Secret vault | 本地 env/config + redaction | `SecretProvider` |
| Object storage | 本地目录 | `ObjectStore` |
| 长期记忆 | 保存 transcript/evidence | `MemoryIndex` |
| UI | inspect HTTP/CLI JSON | `InspectService` |
| 多数据库 | SQLite | `Store` |
| 多 CLI | fake + 一个真实 adapter | `CLIAdapterRegistry` |
| Tool adapter | 可选 Agent 工具入口；MCP 只是其中一种兼容实现 | `CapabilityAdapter` |

## 9. 建议 Go 包结构

```text
cmd/coordplane
cmd/coordlink

internal/store
internal/events
internal/policy
internal/queue
internal/capability
internal/skills
internal/teamconfig
internal/secrets
internal/coordination
internal/delivery
internal/session
internal/runtime
internal/runtime/docker
internal/runtime/external
internal/cliadapter
internal/cliadapter/fake
internal/objectstore
internal/adapters/tools
internal/http
internal/inspect
internal/config
```

边界要求：

- `internal/http`、`internal/adapters/tools`、`coordlink` 只做 adapter。
- 业务状态机只在 service 层。
- store 不调用 adapter。
- runner 不直接改合同状态。
- delivery 不读取文件系统消息正文。
- skills 不保存运行状态，只保存 Agent-facing 工作流说明。
- capability handler 不直接读取 provider secret，只通过 SecretProvider 注入 runtime。

## 10. 开发顺序

推荐按以下顺序实现：

1. Store schema + migration + event log。
2. Typed response / rejected error 规范。
3. Capability registry。
4. Skill registry + 内置 coordplane-service skill。
5. TeamConfig loader + canonical store 持久化。
6. Policy interface + SingleUserPolicy。
7. SecretProvider MVP + redaction。
8. Coordination 最小闭环：assignment、contract、lease、evidence。
9. Message/mailbox。
10. Delivery service + fake steer adapter。
11. Session route + attempt lifecycle。
12. Queue + retry + recovery worker。
13. coordlink CLI adapter + schema-derived tool adapter shell。
14. External runtime conformance。
15. Docker runtime。
16. Object store + artifact/transcript。
17. Inspect API。
18. 真实 Codex 或 Claude Code adapter。

每一步都应有独立不变量测试；不要用真实 L6/L7 作为第一反馈。

## 11. 最小验收测试

### 11.1 Store 和事件测试

- 任一状态变化写 canonical table 和 event。
- backend 重启后可恢复 pending assignment、mailbox、delivery 和 queue。
- projection 删除后可从 canonical store 重建。

### 11.2 单用户 policy 测试

- Agent 不能读取其他 Agent mailbox。
- Agent 不能调用 runner/internal capability。
- Operator 可以 inspect，但普通 Agent 不可以 inspect 全局状态。

### 11.3 Queue 测试

- Queue item 同一时间只能被一个 worker lease。
- worker crash 后 queue item 可重试。
- retry limit 后进入 dead，canonical object 不丢失。

### 11.4 Capability registry 测试

- HTTP、coordlink CLI、schema-derived tool adapter 对同一 capability 调用同一 handler。
- 普通 Agent capability discovery 不包含 `session.steer`、`session.pin`、raw DB 写入工具。
- rejected response 结构一致。

### 11.5 Skill registry / TeamConfig 测试

- TeamConfig 加载后进入 canonical store，运行中的 attempt 记录启动时 TeamConfig 版本。
- Agent bootstrap prompt 只引用需要读取的 skills，不内联全部 capability schema。
- 删除一个 skill 绑定只影响 skill discovery，不影响 capability handler。
- Backend 不硬编码角色名决定验证权限，权限来自 TeamConfig policy。

### 11.6 SecretProvider 测试

- Provider token 能注入目标 runtime env。
- prompt、event log、transcript、inspect response 不包含 token 明文。
- Inspect 只能返回 secret key metadata，不能返回值。

### 11.7 Delivery 测试

- active turn 下 fake adapter 捕获 DeliverySignal。
- DeliverySignal 不包含完整 message body。
- steer accepted 不会 resolve mailbox。
- steer 失败后 mailbox pending 且可 fallback resume。

### 11.8 Runtime 测试

- external runtime 与 Docker runtime 使用同一 backend protocol。
- Docker Agent 只能看到本地 workspace、coordlink 和必要 env。
- Runner 不直接完成合同或发布下一任务。

### 11.9 Object store 测试

- artifact upload 生成 durable artifact id 和 checksum。
- transcript 保存为 object ref。
- 普通 Agent 不能通过 object path 直接越权读取。

### 11.10 Inspect 测试

- inspect 可以按 trace_id 找到 tool call、event、delivery attempt。
- inspect 是只读接口。
- inspect view 不作为 canonical truth。

## 12. 设计结论

CoordPlane MVP 的正确收敛方式是：

```text
single-user defaults
  + backend canonical store
  + unified AgentCommunicationEnvelope
  + DB-backed queues
  + policy interface
  + capability registry
  + skill registry
  + TeamConfig
  + adapter registries
  + SecretProvider redaction
  + inspect/debug surface
```

这样第一阶段足够轻量，可以先把后台核心功能跑通；同时不会把单用户假设写死到主流程里，后续增加多用户、远程 runner、分布式队列或 UI 时只替换接口实现，不重写协作协议。
