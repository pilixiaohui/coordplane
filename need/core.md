# CoordPlane Core 需求

状态：Draft for owner review
依赖：`README.md`

## 1. 目标和边界

Core 是单用户 Daemon 的协调内核，负责：

- 保存 Project、Agent、Task、Run、Message 和 Event。
- 为 Boss 提供固定的命令和状态视图。
- 为 CLI Agent 提供固定的 `coordlink` 操作。
- 调度 queued Task，限制并发，创建 Run。
- 递送 Message，唤醒 waiting/idle Agent。
- 在进程、Daemon 或 adapter 失败后恢复可执行状态。

Core 不执行项目业务判断，不解析聊天文本，不保存代码，不代理 Git 开发命令，也不建设动态能力、策略、skill、验收或 artifact 平台。

Owner批准的预算重基线不改变本文件的对象、状态、命令或不变量。全库production/tests/infra/total envelope固定为`20,000/21,000/22,000`、`21,000/22,000/23,300`、`250/400/600`和`41,250/43,400/45,900`；统计仍按`acceptance.md`的非空、非纯注释物理行口径。Core模块实际SLOC、质量blocker和diff必须进入clean候选revision的LOC JSON，并通过真实多Agent可靠性场景验证状态收敛；重基线reserve不得授权删除本文件合同。

## 2. 持久化总则

### 2.1 SQLite

- 第一版必须使用 file-backed SQLite，启用 WAL、foreign keys 和 busy timeout。
- Schema migration 必须带单调版本号，并在 Daemon 开始调度前完成。
- 同一 `data_dir` 同时只允许一个写 Daemon；第二个实例必须立即失败，不能共同消费 Task。
- 数据库损坏、migration 失败或 data directory 不可写时，Daemon 必须拒绝启动。
- 所有时间使用 UTC，持久化精度和排序规则必须统一。

### 2.2 事务和事件

- 每个业务状态变化和对应 Event 必须在同一 SQLite 事务内提交。
- Event 是 append-only 事实记录，但不替代当前业务行。
- 查询视图必须从六类对象和实际 Git/runtime 事实投影，不能从日志文本推断。
- 跨 SQLite/Docker/Git 且会推进业务状态或创建运行资源的操作，必须先在拥有该动作的 Project/Task/Run 行写窄的 pending/intention字段，并同时写带相同`operation_id`的Event；再执行外部动作，最后以同一`operation_id`清除pending字段并写完成/失败Event。
- 唯一例外是GC删除已由closed/terminal业务事实和当前retention完全推导出的workspace、log或Git ref：它不推进业务状态、不新增对象，无需增加GC pending字段。每次执行/重试都必须重新读取业务行并检查全部fence，按ownership marker或expected-old SHA删除；目标已absent视为幂等成功，Event不得成为删除授权。Unintegrated ref和dirty workspace仍必须由Boss显式discard。
- Reconciler以业务行的 pending字段和外部事实恢复；Event单独存在不得授权重放。不得把 Event扩张成通用Operation/Outbox状态机。
- 不得仅因外部命令 exit 0 就写成功；必须读取目标进程、容器或 Git ref 确认预期事实。

### 2.3 版本和 fencing

- Project、Agent、Task、Run 和 Message 行必须有整数 `version`；每次变更递增。
- 状态转移使用 `WHERE id=? AND version=? AND state=?` 或等价 CAS，受影响行数不是 1 时返回 `VERSION_CONFLICT`。
- Task 必须有单调 `generation`。每次创建新 Run 时递增，并写入 Run 和 runtime token。
- Agent 发起的每个写操作必须同时匹配 `agent_id + task_id + run_id + generation`。
- 旧 Run、旧 token 或旧 generation 的写入必须返回 `STALE_RUN`，且不产生任何业务副作用。

## 3. 唯一持久对象

下面六类是产品业务对象的完整集合。实现可以有 migration、request dedupe 等内部辅助表，但不得把辅助表暴露成第七类业务状态机。

### 3.1 Project

Project 表示一个协作项目和一个 daemon-owned Git repo。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 稳定 ID |
| `name` | Boss 可读名称，同一 Daemon 唯一 |
| `source` | 注册时使用的本地 Git repository路径，仅作初始化来源 |
| `source_ref` | 注册时规范化的完整本地 branch ref，例如 `refs/heads/main` |
| `initial_sha` | `project add` 第一个事务固定的 source commit；后续 source ref 移动不得改变它 |
| `control_repo_path` | Daemon 私有 bare repo；Agent 响应不得暴露宿主绝对路径 |
| `canonical_ref` | 完整 ref，例如 `refs/heads/main` |
| `canonical_sha` | 最近一次已核验的缓存；实际 ref 才是权威 |
| `integration_agent_id` | 可空；stale/冲突时接收 integration Task 的 Agent |
| `status` | `creating`、`active`、`error` 或 `archived` |
| `pending_action` | 可空；仅 `initialize` 或 `verify` |
| `pending_action_id/pending_started_at` | 当前注册/修复外部动作及开始时间，可空 |
| `last_error` | error时的稳定错误码和摘要 |
| `version` | CAS 版本 |
| `created_at/updated_at` | 时间 |

规则：

- `project add` 的只读 preflight 先把完整 source branch ref解析为精确commit；第一个事务必须创建status=creating的Project，固定source/ref/initial SHA和pending operation并写Event，再从source建立control repo。核验成功后清除pending并转active，失败转error。
- 注册不得修改 source repository 的 branch、index 或 working tree。
- creating/error Project不调度Task、不运行Git集成；Boss通过`project repair`重试确定性注册/核验后才可转active。
- `project repair`必须以事务执行error -> creating，写`pending_action=initialize|verify`和新operation ID；不得改变initial SHA，不得把actual canonical reset到缓存值。只有外部repo/ref核验完成后才清除pending并active。
- creating/error/archived Project不接受新Task或Agent-directed Message；archived Project不再创建Run，历史、Boss查询和Git refs保留。
- Project archive要求无starting/active Run、无Project/Task pending action、所有Task均已closed且Agent Message已ack/cancel或转交Boss；否则必须先stop/cancel并等待收敛。
- Project 删除第一版不提供级联物理删除；清理由显式 GC 完成。

### 3.2 Agent

Agent 表示一个持久 CLI 员工身份，不表示常驻进程。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 配置和命令使用的稳定 ID |
| `display_name` | 可读名称 |
| `adapter_id` | 静态注册的 CLI adapter 名 |
| `image` | Docker image 名或 digest |
| `instructions_file` | Boss 管理的指令文件路径 |
| `status` | `active`、`paused` 或 `archived` |
| `version` | CAS 版本 |
| `created_at/updated_at` | 时间 |

规则：

- Manager、Developer、Reviewer、Integrator 只写在 display/instructions 中，不进入状态机。
- 一个 Agent 第一版同时最多一个 `starting` 或 `active` Run。
- paused Agent 不领取新 Task，但当前 Run 不被静默杀死；Boss 可显式 stop。
- Agent archive要求没有starting/active Run、不存在open assigned Task，也没有open source Task把它固定为`accepted_integration_agent_id`；需要继续的工作由Boss另建引用原Task的新Task。其conversation Task必须closed，未处理Message必须ack、cancel或转交Boss；被cancel的消息仍保留为可查询历史。
- archived Agent 不接受新Task/Message、不领取任务，历史 Task、Run 和 Message 保留。
- Agent archive事务必须清除引用它的Project默认`integration_agent_id`并写Project Event；已接受Task保存的`accepted_integration_agent_id`不随默认值变化，且按上一条阻止archive直到收敛。
- Agent 通过 Boss `agent add/update` 写入 SQLite；Daemon YAML 不保存第二份 Agent 列表。
- Agent 字段修改只影响新 Run；已创建 Run 保存解析后的 adapter、image 和 instructions hash，不受后续修改影响。

### 3.3 Task

Task 是唯一任务、责任和对话工作单元。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 稳定 ID |
| `project_id` | 所属 Project |
| `kind` | `conversation`、`work` 或 `integration` |
| `parent_task_id` | 可空；显式父子关系 |
| `retry_of_task_id` | 可空；Boss 对 completed/cancelled Task 重做时创建新 Task |
| `created_by_kind/id` | `boss`、`agent` 或 `system` 及其 ID |
| `assignee_agent_id` | 明确目标 Agent；不可只写角色名 |
| `title` | 简短标题 |
| `description` | 原始目标和约束；Daemon 不解释语义 |
| `priority` | 整数；高值优先，同值按创建顺序 |
| `status` | `queued`、`running`、`finishing`、`waiting`、`submitted`、`completed`、`failed`、`cancelled` |
| `current_run_id` | 可空；用于 starting/active 或 finishing收尾 Run 的唯一占用 |
| `generation` | 每次创建新 Run 时递增 |
| `next_run_at` | 失败退避后的最早调度时间 |
| `retry_count/max_retries` | runtime 失败自动重试边界 |
| `wait_reason` | waiting 时的可读原因 |
| `result_summary` | Agent 提交的简短结果，不是代码真相 |
| `failure_reason` | 稳定错误码和摘要 |
| `base_sha` | 代码 Task 的不可变起始 commit；conversation 可空 |
| `head_sha` | Daemon 捕获的实际提交；提交前为空 |
| `head_run_id` | 产生当前head/task ref的Run，可空 |
| `task_ref` | Daemon-owned Git ref；捕获前为空 |
| `accepted_by_kind/id` | Boss或父Task Agent显式接受者，可空 |
| `accepted_at` | 接受时间，可空 |
| `accepted_integration_agent_id` | accept时解析并固定的integration Agent；未接受时为空 |
| `final_canonical_sha` | 成功集成后实际canonical SHA，可空 |
| `integration_task_id` | stale后创建的integration Task，可空 |
| `source_task_id/source_run_id` | work Task可选、integration Task必填的固定source，可空 |
| `source_task_ref/source_head_sha` | 创建时复制的精确Git输入；后续source Task变化不得改写，可空 |
| `source_accept_version` | integration Task链接后source Task的精确version；其他work Task为空 |
| `observed_canonical_sha` | integration Task创建时看到的canonical，可空 |
| `pending_action` | 可空；仅`capture`或`advance` |
| `pending_action_id` | 外部动作的稳定operation ID，可空 |
| `pending_action_version` | intent提交后的Task version，可空 |
| `pending_action_run_id` | capture/advance关联Run，可空 |
| `pending_expected_sha/pending_target_sha` | Git外部动作的精确SHA，可空 |
| `pending_started_at` | 外部动作开始时间，可空 |
| `version` | CAS 版本 |
| `created_at/updated_at/submitted_at/completed_at/closed_at` | 相应时间；`closed_at`只在首次进入completed/cancelled时设置且不可改写 |

Task kind 规则：

- `conversation`：Boss 与一个 Agent 的长期对话入口。Agent 回复后通常进入 waiting，新 Boss Message 将其重新 queued；由 Boss 显式 close 为 completed。
- conversation只允许Message/progress/wait/fail和Boss close，不允许submit/accept/rework；work/integration不允许close。非法kind操作返回`INVALID_STATE`且零副作用。
- conversation的delivery-capable状态只有`queued/running/finishing/waiting`。failed conversation虽仍可retry/cancel，但不能接收新Message或被用作重路由目标。
- 同一Project/assignee同时最多一个open conversation Task；Boss chat和无显式delivery Task的direct Message只复用delivery-capable conversation。若唯一open conversation已failed，调用稳定失败，Boss必须retry/cancel后再继续。
- `work`：普通实现、调研、审查或修复任务。可通过`source_task_*`字段固定一个已capture结果作为输入，Daemon不区分其业务语义。
- `integration`：使用上述窄字段固定 source task/run/ref/head 和 observed canonical SHA；由 CLI Agent 使用原生 Git 收敛代码。
- 不提供通用`context_json` DSL。kind-specific机器事实必须是本文列出的窄字段；新增字段先修改需求。
- 已被接受的integration Task在canonical再次stale时，kind handler可机械执行submitted -> queued并发送new SHA Message；这是静态注册的Git收敛规则，不授权普通work Task自动rework。
- `completed`和`cancelled`是Task closed状态；`failed`仍可显式retry/cancel，因此是open但不可自动调度状态。`submitted`同样open，只等待accept/rework/cancel。
- source Task一旦链接一个尚未closed的integration Task，其accepted授权由`accepted_* + integration_task_id`共同固定；source上的rework/cancel/第二次accept必须返回`ACTION_IN_PROGRESS`。取消该integration Task时必须按`git.md`原子释放这份授权。
- Task kind 的准备/完成钩子必须通过静态 handler 列表注册，主调度循环不得按角色或项目写特判。

### 3.4 Run

Run 表示一次真实 CLI 进程执行。恢复同一个 CLI native session 也必须创建新 Run；terminal Run 永不复活。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 稳定 ID |
| `project_id/task_id/agent_id` | 固定归属 |
| `generation` | 创建时 Task generation |
| `resumed_from_run_id` | 可空；resume 来源 |
| `adapter_id` | 本 Run 固定 adapter |
| `image` | 本 Run 固定 image 或 digest |
| `instructions_hash` | 本 Run 使用的指令内容 hash |
| `state` | `starting`、`active`、`exited`、`failed`、`interrupted`、`cancelled`、`timed_out` |
| `workspace_path` | Daemon 内部 work/integration workspace 路径；conversation 可空 |
| `container_id` | 创建容器后立即保存，可空 |
| `native_session_id` | CLI 提供后立即保存，可空 |
| `log_path` | stdout/stderr 轮转日志位置 |
| `token_hash` | Run scope token 的 hash；不得保存明文 |
| `token_revoked_at` | Task outcome、run stop、cancel 或 terminal 后立即填写 |
| `requested_outcome` | 可空；`wait`、`submit` 或 `fail` |
| `requested_summary/expected_head` | outcome输入；submit时由后续capture核验 |
| `requested_at` | outcome 请求时间，可空 |
| `stop_requested_at/stop_reason` | Boss `run stop` 的窄runtime intent，可空 |
| `stop_operation_id` | stop/cancel/timeout/shutdown外部停止动作的稳定operation ID，可空 |
| `heartbeat_at` | Supervisor 最近确认 live 的时间 |
| `exit_code` | 进程退出码，可空 |
| `terminal_reason/last_error` | 稳定原因和摘要 |
| `cleanup_state` | `not_needed`、`pending`、`removed`、`blocked` |
| `version` | CAS 版本 |
| `created_at/started_at/ended_at` | 相应时间 |

规则：

- `native_session_id` 只证明未来可能 resume，不证明进程或 turn 当前存活。
- stdout/stderr 日志不是 Run 状态真相。
- Run exit 0 不自动修改 Task 为 submitted/completed。
- `exited` 表示 CLI 进程已得到可信 exit fact，可能在Run写active之前快速退出；exit code可以为0或非0。
- `failed` 只表示 workspace/container/CLI 准备或启动阶段失败，Run 从未 active。
- `interrupted` 表示Run预期已有或可能已有进程，但在starting/active阶段失去它且无法取得可信exit fact。
- Agent 成功调用 wait/submit/fail 后，Run 保存 requested outcome、立即撤销 token，并由 Runtime 收敛真实进程。
- `run stop` 只写stop intent并停止本次Run，不取消Task；没有requested outcome时Run最终interrupted，Task按retry policy requeue/failed。
- Run terminal 后 token 必须保持失效，所有迟到写入被 token/generation fence 拒绝。
- terminal Run 对象永久保留；大日志按大小轮转，达到`retention.run_log`后可删除。

### 3.5 Message

Message 是唯一对话、收件和唤醒对象，不另建 Thread、Mailbox 或 DeliveryAttempt。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 稳定 ID |
| `project_id/task_id` | 每条消息必须绑定接收方要处理的 delivery Task |
| `related_task_id` | 可空；消息讨论的 source/child Task，可与 delivery Task不同 |
| `sender_kind/id` | `boss`、`agent` 或 `system` |
| `recipient_kind/id` | `boss` 或具体 `agent_id`；不得只写角色 |
| `reply_to_message_id` | 可空 |
| `system_code` | 系统消息的稳定短码；普通消息为空 |
| `body` | UTF-8 正文；大文件不内联 |
| `wake` | 是否要求确保接收方获得 Run |
| `state` | `pending`、`delivered`、`acknowledged` 或 `cancelled` |
| `delivered_run_id` | 可空；实际承载消息的 Run |
| `delivery_count/max_deliveries` | 自动wake/redelivery上限 |
| `next_delivery_at` | 自动重投的最早时间 |
| `last_delivery_error` | 可空；最近递送错误 |
| `idempotency_key` | 同一发送者 scope 内去重 |
| `version` | CAS 版本 |
| `created_at/delivered_at/acknowledged_at` | 相应时间 |

规则：

- Message 必须先在 SQLite 提交，才可尝试 Inject、resume 或新 Run。
- Agent-directed Message要求Project active且recipient Agent不是archived；否则稳定拒绝且不插入Message。Recipient为Boss的既有Task结果/错误仍可在Project error/archive前置收尾事务中产生和读取。
- 指定 `task_id` 时，该 Task必须由接收 Agent负责，或接收者为Boss。未指定目标Task的direct Agent Message必须创建/复用接收Agent在该Project的conversation Task作为delivery Task。
- 给Agent显式指定的Task若在同一事务已是submitted/failed/closed，不得把新Message留在该Task；Daemon必须改用recipient的delivery-capable conversation Task，并把原指定Task保存为related Task，或返回稳定`INVALID_STATE`。状态检查和Message插入必须原子。
- `delivered` 只表示消息已进入一个真实 Run 的输入，不表示 Agent 已处理。
- 接收者可将pending或delivered Message显式ack；这消除外部Inject/Run输入先发生而delivered事务稍后提交的竞态。
- 承载消息的 Run terminal 而消息未 ack 时，Message 必须恢复 pending 并可再次递送；因此语义是 at-least-once。
- 每次自动redelivery递增delivery_count并设置backoff。达到max_deliveries后保持pending但停止自动wake，在Boss状态中显示；只有显式`task wake/message retry`才重新启用，不能形成付费Run紧循环。
- Delivery Task进入不能再接收Run的`submitted/failed/completed/cancelled`，或Project/Agent将archive时，不得留下只绑定该Task的pending/delivered Agent Message。状态转移事务必须把它重路由到recipient的delivery-capable conversation Task并以原delivery Task作为related Task；不存在此类conversation时带明确disposition转cancelled或转交Boss。failed conversation不得被当作重路由目标。
- Recipient 为 Boss 时，正式 `message read/chat` 可将其标记 delivered 且 `delivered_run_id` 为空；显示成功或显式 ack 后进入 acknowledged。
- 同一 Task/recipient 按 `(created_at, id)` 稳定排序；不承诺跨 Task 全局顺序。
- 大正文应存于项目可访问文件或外部 URL，并在 body 中引用；Core 不提供 blob 上传。

### 3.6 Event

Event 是关键变化的 append-only 记录。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id` | 单调可排序 ID |
| `project_id` | 可空；Project内事件必填，全局Agent/Daemon事件使用daemon scope |
| `entity_type/entity_id` | Project、Agent、Task、Run 或 Message |
| `kind` | 稳定事件名 |
| `actor_kind/id` | boss、agent、daemon 或 system |
| `run_id` | Agent/Run 产生时填写，可空 |
| `request_id` | 幂等和追踪用，可空 |
| `operation_id` | 外部动作intent/terminal配对使用；此类Event必填，普通观察可空 |
| `payload_json` | 小型结构化差异、错误码或精确 SHA；不得放 secret/大日志 |
| `created_at` | UTC 时间 |

Boss/Agent mutation产生的Event必须有request ID；Daemon外部动作的requested/terminal Event必须有同一个operation ID。纯观察/heartbeat可以没有operation ID，但不得被reconciler当作动作授权。

至少需要以下事件族：

```text
project.creating / project.active / project.error / project.archived
agent.created / agent.paused / agent.archived
task.created / task.claimed / task.running / task.finishing / task.waiting
task.submit_requested / task.submitted / task.requeued
task.completed / task.failed / task.cancelled
run.created / run.active / run.session_recorded
run.outcome_requested / run.stop_requested / run.resume_fallback
run.exited / run.failed / run.interrupted / run.cancelled / run.timed_out
message.created / message.delivered / message.acknowledged / message.redelivered / message.cancelled / message.delivery_exhausted
message.rerouted
git.task_ref_capture_requested / git.task_ref_captured
git.canonical_advance_requested / git.canonical_advanced / git.canonical_stale
runtime.reconciled / runtime.cleanup_removed / runtime.cleanup_blocked
```

新增 Event renderer/check 必须列表注册。不得把每个 CLI tool call、stdout chunk 或模型 token 写成 Event。

## 4. Canonical 状态机

### 4.1 Project 和 Agent 状态机

```text
Project: creating -> active | error
         error    -> creating | archived
         active   -> error | archived

Agent:   active <-> paused
         active|paused -> archived
```

- Project error是持久fail-closed状态；只有`project repair`重新进入creating并完成Git核验后才能active。
- Project/Agent archive前置条件见对象规则；archived不原地恢复，需新建对象或未来显式需求变更。

### 4.2 Task 状态机

```text
create ordinary/wake Task ----------------------> queued
create conversation + only wake=false Message --> waiting
queued + Run becomes active -------------------> running
running + agent requests wait/submit/fail ------> finishing
finishing + Run terminal + wait ----------------> waiting | queued
finishing + Run terminal + valid capture -------> submitted
finishing + Run terminal + fail ----------------> failed
waiting + explicit wake/wake Message/child result -> queued
submitted work + accept + canonical包含/FF CAS -> completed
submitted work + accept + stale ---------------> submitted + linked integration
submitted integration + canonical再次stale ----> queued
submitted + explicit rework --------------------> queued
queued|running|waiting + unrecoverable failure -> failed
failed + explicit retry ------------------------> queued
queued|running|waiting|submitted|failed
  + explicit cancel ----------------------------> cancelled
conversation waiting + Boss close --------------> completed
```

约束：

- Task 创建后默认queued。唯一例外是首次direct Agent Message显式`wake=false`且没有open conversation Task：Message与conversation Task必须在同一事务创建，Task初始waiting，不得启动Run。
- Scheduler 创建 `starting` Run 时，Task 仍保持 queued，但设置 `current_run_id`；Boss 视图可投影为 `preparing`，不得持久化第二套 Task 状态。
- 只有 runtime 证明容器和 CLI 进程 live 后，Run 才 active，Task 才 running。
- waiting 表示责任仍属于同一 Task/Agent，只是当前没有 active Run。
- finishing 表示Agent outcome已持久化、token已撤销，Runtime正在结束Run或Git正在capture；它不是第二套任务语义，且不允许accept/rework/cancel等竞争mutation。
- submitted 表示 Agent 已提交可审查结果；它不是 accepted、integrated 或 completed。
- completed 必须由 Boss、Task 创建者/父 Task 当前 Agent显式接受，或由 Git 模块确认已成功集成后产生。
- Agent 的 wait/submit/fail 必须在同一 SQLite事务把Task从running转finishing、保存`Run.requested_outcome`、撤销token并写Event；Runtime随后幂等结束该Run，只有terminal/capture事实成立后才转waiting/submitted/failed。
- Submit capture可修复失败时，finishing清除pending/current Run后转queued并带backoff/Message；超过retry上限转failed。Invariant失败使Task failed、Project error。
- failed 可由 Boss/创建者显式 retry；retry 必须递增 generation、清除旧 `current_run_id`，保留失败 Event。
- completed/cancelled 不原地重开；需要继续时创建带 `retry_of_task_id` 的新 Task。
- 不定义 `blocked`、`needs_input`、`repair` 状态。等待人或 Agent 的情况使用 waiting + Message。
- `pending_action` 非空或Task finishing时，除status、reconciler和`run stop`外的竞争mutation必须返回`ACTION_IN_PROGRESS`。尤其accept-vs-rework/cancel不能并发授权同一旧结果。
- source Task链接尚未closed的integration Task时适用同一`ACTION_IN_PROGRESS`规则；不能在integration运行期间撤销source后仍让旧结果推进canonical。

### 4.3 Run 状态机

```text
starting -> active
starting -> exited | failed | interrupted | cancelled | timed_out
active   -> exited | interrupted | cancelled | timed_out
```

- 所有 terminal Run 状态不可返回 active。
- resume 创建新 Run，并通过 `resumed_from_run_id` 和 native session ID 关联旧 Run。
- Daemon crash 后发现原容器仍真实运行时，可以重新监控同一 active Run；这不是 Run 状态复活。
- 发现 active Run 的容器/进程不存在时，必须转 interrupted，不能仅凭 native session ID 保持 active。

### 4.4 Run terminal 对 Task 的影响

| Run 结果 | Task 当前状态 | 必须行为 |
| --- | --- | --- |
| starting时CLI快速exit | queued | 保存exit；清除current Run并按retry policy requeue/failed |
| requested outcome=wait | finishing | 清除current Run；有pending wake则queued，否则waiting |
| requested outcome=submit | finishing | 在静止workspace执行capture；成功submitted，失败requeue/failed |
| requested outcome=fail | finishing | 清除current Run并转failed |
| timeout/stop发生但已有更早requested outcome | finishing | Run保留真实终因；Task仍按wait/submit/fail规则收尾 |
| Run 正常 exit，但 Task 仍 running | running | 记录 `NO_TASK_OUTCOME`；未超 retry 时 requeue，超限 failed |
| Run error/interrupted | running 或 queued | 未超 retry 时带 backoff requeue，超限 failed |
| Boss cancel | FSM允许且无pending/integration授权的open Task | Task cancelled；存在Run时stop/cleanup异步但必须收敛 |
| timeout且无requested outcome | running | Run timed_out；Task按 retry policy requeue 或 failed |
| starting阶段timeout | queued | Run timed_out；清除current Run，Task按retry policy requeue或failed |

### 4.5 Message 状态机

```text
pending -> delivered -> acknowledged
pending ----------------> acknowledged
delivered -> pending   # delivered Run terminal 且未 ack
pending|delivered -----> cancelled
```

- Inject accepted、resume prompt 建立或新 Run prompt 建立后，才能标 delivered；接收者直接从inbox读取pending时可原子ack。
- adapter 不支持 Inject、Inject 失败或目标 Agent busy 时保持 pending。
- acknowledged 不自动完成 Task。

## 5. Task 创建、父子任务和等待

### 5.1 创建

- Boss 和当前 active Run 的 Agent都可以创建 Task。
- 创建Task要求Project active、assignee不是archived；paused assignee可以接收queued Task但在resume前不调度。
- Agent 创建 Task 必须在同一 Project，必须给出具体 `assignee_agent_id`，并自动记录当前 Task 为 parent，除非显式创建同级 Task 且 scope 允许。
- Boss或有权读取source Task的Agent可以用`--source-task ID`创建work Task。Daemon只接受已有task ref的submitted/completed source，并在创建事务复制精确source task/run/ref/head；后续不得跟随可变状态重解析。
- Boss使用`task create --retry-of T`时，T必须是同一Project内completed/cancelled Task；新Task记录lineage但仍按普通创建规则固定当前actual canonical为base。Open/cross-Project目标稳定拒绝且零副作用；需要使用旧代码结果时必须另显式`--source-task`，Daemon不从retry关系猜代码输入。
- Daemon 不根据自然语言把 Message 自动转换成 Task。
- 代码 Task 创建时必须从实际 canonical ref 读取并固定 `base_sha`；调用方可提供 expected base 进行一致性校验，但不能提供不存在的 SHA。
- 创建和 `task.created` Event 必须同事务。

### 5.2 子任务反馈

- 子 Task submitted、completed、failed 或 cancelled 时，Daemon必须通知parent当前责任方：Project active且parent Agent未archived时按Message规则投递/重路由；parent已closed、Agent已archived、Project非active或不存在delivery-capable conversation时，必须在同一事务创建recipient=Boss的结果Message并关联parent/child。目标不可递送是确定性Boss fallback，不是业务错误；SQLite事务本身失败仍整体fail loud。
- system Message 必须包含 child task ID、状态、result summary 以及存在时的 head SHA/task ref。
- parent Task waiting 且Message `wake=true`时转queued；parent处于running且已有live Run时按消息递送规则处理。
- Backend 不因子 Task 完成自动接受结果、自动完成 parent 或自动创建下一业务 Task。

### 5.3 等待

- Agent 调用 `task wait` 必须提供可读 reason，可选列出等待的 child/message ID。
- wait 将 Task转finishing，设置Run.requested_outcome=wait并撤销token，再请求当前Run正常结束；只有Run terminal后才转waiting，若此时已有pending wake则直接queued。
- waiting Task只有显式 wake、目标 wake Message或子 Task反馈才回 queued。

## 6. 调度和并发

### 6.1 Scheduler 选择

Scheduler 只选择满足全部条件的 Task：

- Project active。
- Task status queued。
- `current_run_id` 为空。
- `next_run_at <= now`。
- Assignee Agent active 且没有 starting/active Run。
- 全局 active/starting Run 数小于 `max_parallel_runs`。

排序固定为 `priority DESC, created_at ASC, id ASC`。Daemon 不读取 description 判断顺序，不提供通用依赖 DAG 或 stage barrier；Agent 通过何时创建子 Task 和何时 wait 表达工作顺序。

### 6.2 Claim 事务

一次 claim 必须在单个 SQLite 事务中：

1. 再次检查 Task/Agent/并发条件。
2. Task generation 加一。
3. 创建 state=starting 的 Run。
4. Task.current_run_id 指向 Run，但 Task status 暂保持 queued。
5. 写 `task.claimed` 和 `run.created` Event。

事务提交后才允许准备 Docker/workspace。准备失败必须终结 Run、清除 current_run_id，并按 retry policy requeue/failed。

### 6.3 Worker 组织

Daemon worker 至少包括 scheduler、message notifier、run supervisor、reconciler 和 GC。每个 worker 是独立实现，通过静态列表注册：

```text
workers = [scheduler, notifier, supervisor, reconciler, gc]
```

删除一个 worker 应只移除注册项和实现，不得修改共用主循环。Worker 必须支持 context cancellation，退出时不得泄漏 goroutine。

周期GC worker只执行由closed/terminal事实、当前retention和全部fence确定为安全的自动清理。`gc preview`只读列出当前可删/不可删目标、原因和CAS输入：workspace返回由Task/version、ownership marker、actual HEAD和porcelain status digest组成的fingerprint；每个task/run ref分别返回Task ID、Run ID和actual SHA，ref全名仍由服务端构造且不接受用户输入。`gc run --confirm`立即调用同一组已注册安全步骤并在执行前重算全部predicate，不信任旧preview输出。

危险放弃不复用`gc run`的宽泛flag，只允许两个单目标命令：`gc discard-workspace --task T --expected-fingerprint F --request-id R`和`gc discard-task-ref --task T --run U --expected-sha S --request-id R`。两者要求Task已completed/cancelled；task-ref命令还要求Run U属于Task T并由服务端构造唯一ref。命令重算identity/CAS和所有不可覆盖fence；expected不符零副作用，目标已absent幂等成功。显式discard只能覆盖dirty或“commit未进canonical”这一条保留原因，不能覆盖open Task、active Run、pending action、ownership不明、source/integration引用或Project error。Failed Task必须先retry保留原workspace，或cancel后再discard；cancelled Task要继续工作只能创建`retry_of_task_id`新Task。所有调用使用request dedupe；Event只记录结果，不授权重放。

## 7. Message 递送和唤醒

### 7.1 创建顺序

发送 Message 的事务必须先完成：

1. 校验 sender scope、recipient 和 Task。
2. 插入 Message state=pending。
3. 写 `message.created` Event。
4. 若目标 Task waiting 且 wake=true，以同事务转 queued；若Task finishing，只保存Message，等待Run terminal事务决定queued/waiting。

事务提交后 notifier 才能尝试递送。

Agent发送direct Message但未给delivery Task时，Daemon必须在同一Project创建/复用接收Agent的conversation Task；发送方当前Task写入 `related_task_id`。首次创建且wake=false时conversation按Task FSM直接waiting；wake=true时queued。该机械路由只选择消息投递上下文，不解释正文。

### 7.2 递送顺序

Notifier 按以下固定顺序处理：

1. 若接收 Agent 在目标 Task 上有真实 active Run，且 adapter 支持 Inject，则尝试 Inject。若delivery Task是该Agent的conversation Task，可选Inject到同一Agent当前其他Task的active Run，但输入必须同时标明delivery/related Task ID。
2. Inject 成功后 Message delivered 并绑定该 Run。
3. Inject 不支持或失败时保持 pending；只有Message.wake=true且Task waiting时才转queued；若wake=false则等待显式读取/唤醒或现有Run。Task finishing时等待旧Run收敛；Task running但没有真实Run时由reconciler先修正Task。
4. Scheduler 创建 resume/new Run，启动 prompt 包含全部 pending Message ID 和受大小限制的正文。
5. Run 输入建立成功后，相关 Message delivered。
6. Agent 读取并显式 ack；未 ack 且 Run terminal 时回 pending。

same-turn Inject 只是第 1 步的可选优化。系统正确性不得依赖它。

当Task outcome使delivery Task变成submitted/failed/closed时，未ack消息必须在同一事务按Message规则重路由或cancel；不能等待一个永远不会再创建的Run。

所有会根据Message采取副作用的mutation命令必须接受重复的`ack_message_ids`。Task创建、Message回复、wait/submit/fail、accept/rework与这些ack在同一SQLite事务提交，避免“先ack后动作丢失”或“动作成功但未ack导致重复执行”。Standalone ack只用于接收者明确确认无需其他副作用的消息。

### 7.3 Boss 对话

- `coordplane chat --agent A` 必须创建或复用 Boss 与 A 的delivery-capable conversation Task；存在failed conversation时稳定失败，等待Boss retry/cancel。
- Boss 输入形成绑定该 Task 的 Message，默认 `wake=true`。
- Agent 回复形成 recipient=boss 的 Message；Boss chat 按顺序显示并 ack。
- Agent 回复后可调用 `task wait`，下一条 Boss Message 再 resume/new Run。
- Boss 在 chat 中的自然语言不得直接改 Task、Project 或 Git 状态；结构化动作必须调用对应命令，或由 CLI Agent显式调用 coordlink。

## 8. 固定命令面

所有命令必须支持清晰的人类输出；查询和 mutation result 必须支持 `--output json`。失败使用非零退出码。Operator CLI、coordlink 和内部 HTTP transport 必须调用同一 operation 函数，不能复制状态逻辑。

### 8.1 Boss 的 `coordplane`

| 命令 | 需求 |
| --- | --- |
| `serve --config FILE` | 启动单 Daemon，migration/reconcile 成功后才 ready |
| `status [--project ID]` | 顶层返回`daemon_ready`/未ready或degraded原因，并汇总 Project、Task、Agent、Run、pending Message、Git SHA 和最近错误 |
| `project add --name N --repo LOCAL_PATH --ref refs/heads/BRANCH [--integration-agent A]` | 从本地repo的精确branch/initial SHA创建daemon-owned control repo，不修改source |
| `project list/show/update/repair/archive` | 查询、修改默认integration Agent、修复error Project或停止新调度 |
| `project checkout ID --dest PATH` | 从 canonical ref 创建 Boss 可用的新 checkout；目标非空时失败 |
| `agent add/update/list/show/pause/resume/archive` | 管理简单 Agent身份；不提供 role/policy DSL |
| `chat --project P --agent A [--related-task T]` | 始终投递到该Agent conversation Task；可把另一个Task只作为讨论关联 |
| `message send/list/read/ack/retry` | 非交互消息入口；给出接收者，Task可显式指定或使用接收Agent conversation Task；retry重新启用耗尽的自动递送 |
| `task create/list/show` | 创建和查询明确 Task；work Task可用`--source-task ID`固定待审查输入，Boss可用`--retry-of ID`引用同Project closed Task |
| `task checkout ID --dest PATH` | 从已capture的精确task ref导出普通checkout，移除control remote；用于Boss审查未集成结果 |
| `task accept [--integration-agent A] / rework/retry/cancel/wake/close` | 显式状态动作；代码accept必须有可用integration Agent，Git行为见`git.md` |
| `run list/show/logs/stop` | 查询真实 Run、跟随日志、请求停止 |
| `events tail` | 按 project/task/agent/run 过滤关键 Event |
| `gc preview / gc run --confirm` | preview返回原因和workspace fingerprint/ref SHA；run只清理满足`runtime.md`/`git.md`自动安全条件的资源 |
| `gc discard-workspace --task T ... / discard-task-ref --task T --run U ...` | 单workspace或单run-ref危险放弃；必须携带preview得到的expected identity和request ID，且不能越过active/pending/ownership/source引用fence |

### 8.2 Agent 的 `coordlink`

| 命令 | 需求 |
| --- | --- |
| `task current` | 返回 token 绑定的 Task、Run、base/head 和 unread Message 数 |
| `task show ID` | 只读取当前、parent、child、自己创建或自己负责的 Task |
| `task create --agent A [--source-task ID] ...` | 创建显式子 Task；source必须在当前scope可读且已有task ref |
| `task wait --reason TEXT` | 当前Task进入finishing；Run terminal后才waiting或因pending wake直接queued |
| `task submit --summary TEXT --expected-head SHA` | 代码Task记录submit outcome并进入finishing；Run terminal后核验expected head并capture |
| `task fail --reason TEXT` | 记录fail outcome并进入finishing；Run terminal后Task才failed |
| `task accept ID [--integration-agent A]` / `task rework ID` | 只允许Task创建者或parent当前Agent处理子Task结果；代码accept固定本次integrator |
| `inbox list/read/ack` | 当前 Task/Agent scope 的 Message 操作 |
| `message send [--task T] --to-agent A|--to-boss ...` | 发送持久 Message；direct Agent消息未给Task时使用其conversation Task，可要求wake |
| `progress --summary TEXT` | 写一个 progress Event；不得改变 Task状态 |

coordlink 不得提供 `capability list`、`skill list/read`、通用 `call NAME`、任意 HTTP path 或 raw DB/Git ref 操作。

`task cancel` 与 `run stop` 必须严格区分：

- `task cancel` 取消工作责任、递增Task generation并使当前token失效；当前Run由Runtime最终置cancelled，Task不得自动重启。
- `run stop` 只在Run行写`stop_requested_at/reason`并结束本次进程；没有requested outcome时Run置interrupted，Task按retry policy回queued或failed。
- Runtime接管/cleanup始终比较不可变的Run generation和launch nonce，不能拿已递增的Task generation否定旧容器ownership。

## 9. 身份和 scope

第一版不是多用户 RBAC，但必须防止 Run 冒充其他 Agent/Task：

- Boss 默认通过本机 Unix socket 文件权限或显式 operator token 访问全部 Project；不得把 operator token 注入 Agent 容器。
- Agent transport只接受`runtime.md`的per-Run Unix socket；socket归属用于缩小可达面，但不能替代Run token、generation和scope校验。
- 每个 Run 创建独立随机 token，只在启动时注入容器；数据库只保存 hash。
- Token 固定绑定 `project_id + agent_id + task_id + run_id + generation`，请求体中的 ID 只能做一致性校验，不能覆盖绑定值。
- Run outcome请求、run stop、Run terminal、Task cancel或generation前进时token立即失效。
- Agent 可读取当前 Task、parent/children、自己创建/负责的 Task 和相应 Message；不得读取其他 Agent workspace、home、token 或私有日志。
- Agent 创建子 Task 可指定同一 Project 中 active Agent；不能创建跨 Project Task。
- Scope 拒绝只返回稳定错误和目标对象类型，不泄露对象内容或宿主路径。

## 10. 幂等和错误

### 10.1 幂等

- 创建 Task、发送 Message、submit、accept、cancel 和外部 Git动作必须接受 `request_id`/idempotency key。
- wait/fail/rework和任何携带`ack_message_ids`的mutation同样必须有request ID。
- Task/Message 创建使用 `(actor_scope, operation, idempotency_key)` 唯一约束；重复请求返回原对象。
- 状态转移重复到相同结果时可以返回当前对象；状态或参数冲突时返回 `VERSION_CONFLICT`/`INVALID_STATE`，不能偷偷创建副本。
- 外部动作使用确定性 Run ID、container label 和 Git ref；reconciler 能区分重试和新动作。

### 10.2 稳定错误

至少定义：

```text
NOT_FOUND
INVALID_ARGUMENT
INVALID_STATE
ACTION_IN_PROGRESS
VERSION_CONFLICT
SCOPE_DENIED
STALE_RUN
RUN_STARTING
AGENT_BUSY
RUNTIME_UNAVAILABLE
RESUME_UNAVAILABLE
GIT_DIRTY
GIT_STALE
INTEGRATION_AGENT_REQUIRED
GIT_INVARIANT_VIOLATION
INTERNAL
```

结构化错误必须包含 `code`、`message`、`retryable`，状态冲突时包含当前 `state/version`。不要求 repair hint、allowed actions 或通用 rejected-response schema。

## 11. Boss 状态视图

`status` 和 `task show` 至少返回：

- `status --output json`顶层返回`daemon_ready`；启动reconciliation完成前为false，完成且允许mutation后为true，degraded时同时返回稳定原因。
- Task id/kind/status/version/assignee/parent。
- 当前 Run id/state/container live truth/native session 是否存在。
- 最近 progress Event 和时间。
- pending/delivered Message 数。
- base SHA、captured head SHA、task ref、实际 canonical SHA。
- source task/head、accepted by、固定integration Agent、linked integration Task和pending action（如有）。
- retry count、next run time、last error、cleanup state。
- 对于 derived phase，明确标注 `derived=true`，且能追溯到 Task/Run 字段。

状态视图不得：

- 读取 transcript 猜任务是否完成。
- 因 native session ID 存在而显示 Run active。
- 因 Agent 最近输出“done”而显示 submitted/completed。
- 暴露 control repo、workspace、home、DB 或 token 的宿主绝对路径给 Agent；Boss debug 输出可显式请求 redacted=false。

## 12. 启动、关闭和恢复

Daemon 启动顺序必须为：

1. 独占 data directory。
2. 验证配置和目录权限。
3. 打开 SQLite并完成 migration。
4. 校验 Project control repo 与 actual refs。
5. 执行 runtime/Git reconciliation。
6. 启动静态 worker 列表。
7. 标记 ready 并接受 mutation。

reconcile 未完成前，查询可以只读开放，但不得 claim Task 或启动 Run。

优雅关闭必须先停止新 claim，再让 runtime supervisor 按 `runtime.md` 处理 starting/active Run。崩溃关闭不得在内存中留下唯一状态；下一次启动必须只依赖 SQLite、Docker 和 Git 事实恢复。

## 13. Core 不变量

- 业务持久对象只有 Project、Agent、Task、Run、Message、Event。
- Task 与 Run 分离；一次 Task 可有多个不可复活的 Run。
- 同一 Agent 最多一个 starting/active Run，同一 Task 最多一个 current Run。
- Task running必须指向active Run。Run active表示“启动已观察、Supervisor已持有Wait监控、尚未观察到terminal”；外部进程可在事务后退出，Supervisor/reconciler必须在有界时间收敛，Inject前仍实时Inspect。
- Task finishing必须有current Run或pending capture action，且该Run token已撤销；Scheduler不得claim。
- Source Task链接open integration Task时其accepted授权不可被rework/cancel；integration终结必须原子完成source或释放授权。
- Run exit 0、Agent文字或 transcript 都不能完成 Task。
- 所有 Agent写操作受 token + generation fence 约束。
- Message 先持久化后递送，未 ack 的消息可重复但不能丢。
- Message自动wake有backoff/次数上限；不能再接收Run的delivery Task上的未处理Message必须cancel或显式重路由。
- waiting 是唯一等待状态；等待人类或其他 Agent通过 Message表达。
- Task/Message 表直接承担调度/递送队列，不建设通用 QueueItem。
- 每次状态变化与 Event 同事务；每个外部动作都有 intent 和 reconciliation。
- Daemon 不理解任务正文、角色名、代码或验收语义。
