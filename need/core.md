# CoordPlane Core 需求

状态：候选冻结基线，待需求审批人复核精确 revision
版本：1.0-rc3
日期：2026-08-16
依赖：`README.md`

## 1. 目标和边界

Core 是本机多参与者 Daemon 的协调内核，负责：

- 保存 Project、Participant、Role、Credential、Task、Conversation、Message、MessageRecipient、Run 和 Event。
- 对 Operator CLI、Web API 和 coordlink 执行统一认证、授权、CAS、幂等和状态转移。
- 调度 CLI Agent Task 和定向 wake Message，限制 Docker Run 并发。
- 在 Daemon、Docker、adapter 或 Git 操作失败后恢复可执行状态。
- 为 Runtime 行为日志保存索引、完整性和 retention 元数据。

Core 不解释 Task/Message 文本，不内置人员职责，不自动验收结果，不保存代码正文，不代理通用 Git 命令，也不把 Event/日志当成状态推进授权。

## 2. 持久化总则

### 2.1 SQLite 与单一真相

- v1 使用 file-backed SQLite，启用 WAL、foreign keys 和 busy timeout。
- migration 带单调版本号，必须在调度、Web 服务 ready 和 mutation 开放前完成。
- 同一 `data_dir` 同时只允许一个写 Daemon；第二个实例立即失败。
- 数据库损坏、migration 失败、data directory 不可写或 schema 不符合 exact-set 时拒绝启动。
- 所有时间使用 UTC，持久化精度和排序规则统一。
- `participants` 是 Human 和 CLI Agent 身份及 Agent 静态配置的单一权威；旧 `agents` 镜像表和双写入口必须在同一迁移删除。
- `Conversation` 是对话权威；旧 `conversation Task` 不保留兼容状态机。
- 预冻结 schema 未对外承诺兼容。无法无歧义转换旧 Boss 枚举、Human 特殊 Task 或 conversation Task 的数据库必须 fail closed 并返回 `LEGACY_SCHEMA_REBUILD_REQUIRED`，不得同时运行新旧语义。

### 2.2 事务、外部动作和 Event

- 每个业务状态变化和对应 Event 在同一 SQLite 事务提交。
- Event 是 append-only 关键事实，不替代当前业务行，不承载 stdout chunk、tool call 大正文或 provider transcript。
- 跨 SQLite/Docker/Git 且推进状态或创建资源的操作，先在拥有动作的对象写窄 pending/intention 字段和稳定 `operation_id`，再执行外部动作，最后以同一 ID提交完成/失败。
- Reconciler 只依据业务行 pending 字段和实际外部事实恢复；Event 单独存在不能授权重放。
- GC 是唯一可从 terminal/closed 状态、当前 retention 和实际 fence 推导的外部删除，不新增通用 Operation 对象；每次重试必须重新检查。
- 外部命令 exit 0 不是成功充分条件，必须读取 SQLite、Git ref、Docker/OS 或日志文件事实确认。

### 2.3 版本、幂等和 fencing

- Project、Participant、Role、Task、Conversation、Message、MessageRecipient 和 Run 有整数 `version`，mutation 使用 expected version/CAS。
- 创建与 mutation 接受 `request_id` 或 idempotency key；重复请求返回原结果，不创建重复对象。
- CLI Agent Participant 有单调 `runtime_generation`，每次创建 Run 递增并写入 Run/token。
- Task Run 同时保存 Task `generation`；Conversation Run 的 `task_id` 可空，但仍受 Participant runtime generation 和 Run ID fence。
- Agent 写操作必须匹配 `participant_id + project_id + run_id + runtime_generation`，Task outcome 还必须匹配 `task_id + task_generation`。
- 旧 Run、旧 token、旧 generation 或已撤销 token 返回 `STALE_RUN`，零业务副作用。

## 3. 持久对象

### 3.1 Project

Project 表示一个协作范围和一个 daemon-owned Git repo。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id/name` | 稳定 ID；同一 Daemon 内 name 唯一 |
| `source/source_ref/initial_sha` | 注册时的宿主机本地 repo、完整 branch ref 和固定初始 commit |
| `control_repo_path/canonical_ref/canonical_sha` | Daemon 私有 bare repo、实际 canonical ref 和核验缓存 |
| `integration_participant_id` | 可空；stale 结果默认指派的 active Participant |
| `behavior_log_retention` | 可空；空表示使用全局 168h，非空为 Project override |
| `status` | `creating/active/error/archived` |
| `pending_action/id/started_at` | 可空；注册或核验外部动作 |
| `last_error/version/timestamps` | 稳定错误、CAS 版本和 UTC 时间 |

规则：

- 注册先从 source actual ref 固定 commit，再以 durable intent 创建 control repo；不得修改 source branch/index/worktree。
- actual canonical ref 是代码权威；缓存不一致时 Project 进入显式 reconcile，禁止缓存覆盖实际 ref。
- creating/error/archived Project不调度、不创建新 Task/Conversation/Message；历史和错误仍按权限可读。
- Project 没有 Participant 数量上限。归档要求无 active Run、pending action、open Task 和未处置 MessageRecipient。
- Project retention override 修改不改写历史 `ended_at`，下一次 preview/GC 使用当前值。

### 3.2 Participant

Participant 是唯一业务身份，`kind` 不承载职责。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id/kind/display_name` | `kind=human|cli_agent`；稳定 ID |
| `status` | `active/paused/archived` |
| `runtime_generation/current_run_id` | CLI Agent 使用；Human 必须为空或 0 |
| `adapter_id/image` | 仅 CLI Agent必填；来自静态 adapter registry |
| `instructions_file/instructions_text` | 仅 CLI Agent；恰一来源，最大 1 MiB |
| `model/subagent_model/base_url/effort` | 仅 CLI Agent可配置；按 adapter descriptor 校验 |
| `version/timestamps` | CAS 版本和 UTC 时间 |

规则：

- Human 和 CLI Agent 共享 Task、Conversation、Message、Role、Git 和查询模型；不设置数量上限。
- `active` 可接收新 Task 指派和 Conversation 成员关系。`paused` 不接收两者；已有 Task、Conversation 成员关系和按权限可读历史保留，Human 可继续已有工作，CLI Agent 不创建新 Run且当前 Run不被静默杀死。`archived` 不再认证、接收指派或 Run，历史保留。
- archive 要求没有 active Run、open assigned Task、未处置 MessageRecipient、尚未回传的open child Task result recipient引用，或被固定为 integration Participant 的 open source Task。
- CLI Agent 配置更新只影响新 Run。Run 固定解析后的 adapter、image、instructions hash 和 config fingerprint。
- `instructions_file` 为 daemon 宿主绝对路径；text/file 全文不得进入 Event、错误、日志或 Web 响应，只记录 hash 和大小。
- `model/subagent_model` 只接受安全 token；`base_url` 只接受无 userinfo/query/fragment 的 `https://` URL；`effort` 来自 adapter `AllowedEfforts`。
- Manager、Developer、Reviewer、Integrator 等只存在于 Role、Task 文本或 instructions 中；Daemon 不解释这些名称。

### 3.3 Role、绑定与 Credential

Role 是 capability 名集合。`participant_project_roles` 将一个 Role 绑定到一个 Participant 的 Project 或 `global` scope。

- capability 名来自静态 operation registry，未知值拒绝；Role 名没有内置含义。
- 同一 Participant 可在不同 Project 获得不同 Role；同一 scope 多个 Role 的 capability 取并集。
- 管理类 operation 只能由 global capability 授权。项目级 Role不能伪造 global scope。
- 被绑定引用的 Role 不可删除；删除或降级最后一个 `participant.manage` 持有者必须拒绝且零副作用。
- 任意 Participant 包括 CLI Agent 均可按配置获得管理 capability；执行仍受 transport、Run token和资源可达性约束。

Credential 仅属于 Human：

- v1 签发 `operator_token`，只保存强 hash，支持创建、轮换和吊销。
- 吊销立即阻止新操作；Web session 每次 mutation 必须确认凭据仍 active。
- Credential 不注入 Agent 容器。Agent 使用每个 Run 独立 token，数据库只保存 hash。
- 空库 `coordplane bootstrap` 只能在本机、无 Participant 且 Daemon未开放 mutation 时执行一次，创建普通 Human Participant、Credential 和包含全局管理 capability 的可配置 Role。完成后所有对象走普通 Role/Participant 规则，不存在 `participant-owner` 或 Boss 特判。

### 3.4 Task

Task 是工作责任，不是 Conversation。

必须字段：

| 字段 | 要求 |
| --- | --- |
| `id/project_id` | 稳定 ID和所属 Project |
| `kind` | `work/integration`；review、测试、文档等都是 work |
| `workspace_mode` | `none/git`；integration 必须为 git |
| `parent_task_id/retry_of_task_id` | 可空 lineage |
| `coordination_conversation_id/result_recipient_participant_id` | root Task为空；有`parent_task_id`的子Task必填并固定结果通知路由 |
| `created_by_participant_id/assignee_participant_id` | 统一 Participant ID |
| `title/description/priority` | Daemon 不解释的任务内容和机械优先级 |
| `status` | `queued/running/finishing/waiting/submitted/completed/failed/cancelled` |
| `current_run_id/generation/next_run_at/retry_count/max_retries` | 执行与重试字段；Human current Run为空 |
| `wait_reason/result_summary/failure_reason` | 可空；terminal 成功不得保留旧 failure_reason |
| `base_sha/head_sha/head_run_id/head_submission_id/task_ref` | git Task 的固定输入和捕获结果；Human 的 `head_run_id` 为空，所有成功捕获都有稳定 submission ID |
| `workspace_state/workspace_operation_id/workspace_identity/workspace_error` | `not_needed/pending/ready/blocked/removed`；identity是Project/Task/owner/base/source/创建operation的不可变指纹 |
| `accepted_by_participant_id/accepted_at` | 显式接受事实 |
| `accepted_integration_participant_id` | accept 时固定；可为 Human 或 CLI Agent |
| `final_canonical_sha/integration_task_id` | 集成结果与链接 |
| `source_task_id/source_run_id/source_task_ref/source_head_sha` | 固定 source 结果 |
| `source_accept_version/integration_initial_canonical_sha` | integration Task创建时固定且不可改 |
| `integration_expected_canonical_sha/integration_round` | integration Task每轮CAS预期值和单调轮次 |
| `pending_action/id/version/expected_sha/target_sha/started_at` | capture/advance durable intent |
| `version/timestamps` | CAS 版本；`closed_at`首次 completed/cancelled 后不可改写 |

规则：

- 创建时必须指定 active Participant；paused/archived assignee 拒绝。新 Conversation 成员也必须 active，不得用成员变更绕过 paused。
- `workspace_mode=git` 创建时从 actual canonical 固定 base SHA并在事务中设置 `workspace_state=pending` 和稳定 operation ID；受信 Git worker核验实际目录/marker/HEAD后才可 `ready`。`workspace_mode=none` 为 `not_needed`。`pending/blocked/removed` Task不得 claim 或创建 Run。
- workspace 准备崩溃后只依据 Task operation ID、workspace identity 和实际 Git/文件事实继续或置 `blocked`；不得通过重新读取已移动 canonical 改写固定基线，不得将未核验目录标记 ready。
- Human 和 CLI Agent 使用同一 FSM。差别仅是 CLI Agent 由 Scheduler/Run claim，Human 通过公开 operation 显式 claim；Human 无 Run。
- 所有 Task 结果先到 submitted，再由具备 `task.accept` 的 Participant 显式 accept/rework/cancel。不存在 Human `waiting -> completed`、`human_confirm` 或创建者隐式权限。
- git Task submit 必须经过 actual HEAD capture；非 git Task submit 保存 summary 后进入 submitted。
- completed/cancelled 是 closed；failed/submitted 仍 open。closed Task不原地重开，继续工作创建 `retry_of_task_id` 新 Task。
- source Task 链接 open integration Task 时，rework/cancel/第二次 accept 返回 `ACTION_IN_PROGRESS`，直到显式取消或完成 integration。
- parent Task存在尚未回传的open child且child result recipient为当前parent assignee时，禁止更换parent assignee；先让child回传或显式cancel child。更换child assignee时新assignee必须仍是固定coordination Conversation成员，不改写result recipient。
- Task kind handler 通过静态列表注册，主流程不得按职责、Participant kind 或项目名特判。

### 3.5 Conversation 与成员

Conversation 是 Project 内正式的一对一或群组对话。

| 字段 | 要求 |
| --- | --- |
| `id/project_id` | 稳定 ID和所属 Project |
| `kind` | `direct/group` |
| `title` | group 必填；direct 可空 |
| `created_by_participant_id` | 创建者 |
| `status` | `active/archived` |
| `version/timestamps` | CAS 和 UTC 时间 |

`conversation_members` 至少保存 `conversation_id/participant_id/state/joined_at/left_at/version`。

- direct 创建时恰有两个不同 active Participant；同一 Project、同一无序 Participant 对只能有一个 active direct Conversation。
- group 创建时至少两个 active 成员；成员数量无产品级上限。
- active 成员能读取从 Conversation 创建起的完整历史。退出/移除后仍可按审计权限读取其在籍期间历史，但不能发送或成为新 Message recipient。
- 成员变更、归档和发送均要求 capability；Conversation 名称或创建者不产生隐式权限。
- Conversation归档、移除成员或成员退出前，必须不存在将该Conversation或成员固定为coordination/result route且尚未回传的open child Task；否则拒绝且零副作用。
- Conversation 不占用 Task，不存在 conversation Task、close Task 或每 Agent 一个对话 Task 的约束。

### 3.6 Message 与逐接收者状态

Message 正文在创建后不可变：

| 字段 | 要求 |
| --- | --- |
| `id/project_id/conversation_id` | Conversation 必填且同 Project |
| `task_id` | 可空；非空必须同 Project |
| `sender_participant_id` | 普通消息必填；daemon 通知为空并带 `system_code` |
| `reply_to_message_id/system_code/system_payload_json/body` | reply 可空；system payload只允许静态code对应的有界schema；正文 UTF-8，大文件只引用路径/URL |
| `idempotency_key/created_at` | sender scope 内去重和稳定排序 |

每个 Message 必须有一个或多个 `message_recipients`：

| 字段 | 要求 |
| --- | --- |
| `message_id/recipient_participant_id` | 唯一接收者行；必须是创建时 active Conversation member且不是 sender |
| `wake_requested` | 是否允许该消息单独触发 CLI Agent Run |
| `state` | `pending/delivered/acknowledged/cancelled` |
| `delivered_run_id` | 可空；Human 读取时为空 |
| `delivery_count/max_deliveries/next_delivery_at/last_error` | 独立重试状态 |
| `version/delivered_at/acknowledged_at` | CAS 与时间 |

规则：

- Message 与全部 recipient 行在一个事务创建，然后才允许 Inject/Run。
- Message 明确 recipients之外的成员不产生未读项、不被唤醒；仍能读取 Conversation 历史。
- 一个 recipient 的 delivered/ack/cancel/retry 不得改变其他 recipient 行。
- Human 通过 CLI/Web 读取其 pending MessageRecipient时可原子 delivered/ack，不创建 Run。
- CLI Agent 的 `wake_requested=true` 可触发递送；false 只等待下次同 Project Run。每次 Run bootstrap 至少提供该 Participant 在 Project 内未读总数、稳定高水位、有界 Message/recipient ID 样本和不透明 inbox cursor；完整未读集合只能通过稳定分页读取，不得塞入 bootstrap。
- 正文进入真实 Run input或 recipient主动读取后才可 delivered；Inject accepted 不是 acknowledged。
- delivered Run terminal 且未 ack 时回 pending，保持 at-least-once。达到 max deliveries 后保持 pending但停止自动 wake，直到显式 retry。
- optional Task只表达讨论关联和输入上下文，不决定 Conversation/recipient 生命周期，不要求 Task assignee等于 recipient。
- Project/Conversation/Participant archive 前必须处理所有未确认 recipient：ack、cancel或由有权限 Participant 显式重定向；不得静默转给 bootstrap Human。

### 3.7 Run

Run 表示一个真实 CLI Agent 进程，terminal 后永不复活。

| 字段 | 要求 |
| --- | --- |
| `id/project_id/participant_id` | 固定归属；Participant 必须为 cli_agent |
| `task_id` | 可空；Task Run 必填，Conversation Run为空 |
| `conversation_id/trigger_message_ids` | Conversation wake 时填写；Message ID列表有界且可追溯 |
| `session_scope_kind/session_scope_id` | `task|conversation` 和对应 Task/Conversation ID；决定唯一 resume scope |
| `workspace_identity` | Task Run必填并与Task/ownership marker不变指纹一致；Conversation Run为空 |
| `runtime_generation/task_generation` | Participant fence；Task Run另有 Task fence |
| `resumed_from_run_id/native_session_id` | 可空 resume lineage |
| `adapter_id/image/config_fingerprint/instructions_hash` | 创建时冻结 |
| `state` | `starting/active/exited/failed/interrupted/cancelled/timed_out` |
| `requested_outcome` | Task Run可空或 `wait/submit/fail`；Conversation Run必须为空 |
| `container/runtime/cleanup/log` 字段 | 详见 `runtime.md` |
| `version/timestamps` | CAS 与生命周期时间 |

- 同一 CLI Agent 同时最多一个 starting/active Run，不论由 Task 还是 Conversation 触发。
- Conversation Run 不创建或挂载项目 workspace，只使用 Participant home和私有 control socket。
- 一个 Run 可携带同 Project 多个 Conversation 的未读消息；实际进入输入的每条 Message/recipient ID持久关联 Run，不按 session 粗粒度去重。其他未读只通过有界摘要和 cursor 发现。
- Task outcome 只允许绑定该 Task 的 Run提交；Conversation Run不能修改 Task outcome，但可执行其 capability 允许的普通 Service operation。

### 3.8 Event 与行为日志索引

Event 必须包含 `id/project_id/entity_type/entity_id/kind/actor_participant_id/run_id/request_id/operation_id/payload_json/created_at`。Daemon 发起时 `actor_participant_id` 为空并标记 `actor_type=daemon`；不得保留 `boss|agent|system` actor 枚举。

至少覆盖 project、participant、role/binding、credential、task、conversation/member、message/recipient、run、git、runtime、log retention 和 bootstrap 事件族。新增 renderer/check 通过静态列表注册。

每个 Run 恰有一行 `behavior_log_indexes`，作为该 Run 日志 manifest、完整性、retention 和 `long_term` 的唯一可变权威。它保存 Run ID、manifest/文件路径标识、first/last sequence、字节数、hash、截断/丢失/脱敏计数、retention 状态、`long_term`、版本和时间。Run只保存该索引ID，查询时通过关联行投影摘要；不复制路径、sequence/hash/count、retention或`long_term`。大日志留在 `runtime.md` 指定文件中。Event 与行为日志必须可用 Run ID和 sequence/offset 交叉定位，但不能互相替代。

## 4. Canonical 状态机

### 4.1 Project 与 Participant

```text
Project:     creating -> active | error
             error    -> creating | archived
             active   -> error | archived

Participant: active <-> paused
             active|paused -> archived
```

archived 不原地恢复。Project error 是持久 fail closed；只有显式 repair 和实际 Git核验后才能 active。

### 4.2 Task

```text
create ----------------------------------------> queued
queued + Human claim --------------------------> running
queued + CLI Run proven active ----------------> running
running + wait --------------------------------> waiting
waiting + explicit wake -----------------------> queued
running + submit ------------------------------> finishing
finishing + non-git finalize ------------------> submitted
finishing + valid Git capture -----------------> submitted
running + fail --------------------------------> failed
submitted + accept non-git --------------------> completed
submitted + accept Git FF/CAS -----------------> completed
submitted + accept Git stale ------------------> submitted + integration Task
submitted + rework ----------------------------> queued
failed + retry --------------------------------> queued
queued|running|waiting|submitted|failed + cancel -> cancelled
```

约束：

- Human 和 CLI Agent 状态集合相同。Human claim/wait/fail直接事务收敛；submit 的 Git外部动作仍使用 finishing/pending intent。
- CLI Agent wait/submit/fail先写 requested outcome并撤销 token，Runtime 终结 Run 后再按真实结果推进。
- `finishing` 或 pending action期间，竞争 accept/rework/cancel 返回 `ACTION_IN_PROGRESS`。
- completed/cancelled时清除旧 `failure_reason/wait_reason/current_run_id`；failed不得伪装 closed。
- running CLI Task没有 structured outcome而进程退出时返回 queued/backoff或 failed，绝不能因 exit 0自动 submitted/completed。

### 4.3 MessageRecipient

```text
pending -> delivered -> acknowledged
pending ----------------> acknowledged
delivered -> pending          # Run terminal且未ack
pending|delivered -> cancelled
```

Message 本身不可变且没有聚合 delivery state。查询可投影 recipient 计数，但不得把部分 acknowledged显示为全体已读。

### 4.4 Run

```text
starting -> active
starting -> exited | failed | interrupted | cancelled | timed_out
active   -> exited | interrupted | cancelled | timed_out
```

resume 创建新 Run并引用旧 Run。Daemon crash后重新 Attach真实仍在运行的容器是接管同一 Run，不是 terminal 状态复活。

## 5. Task 创建、执行和反馈

- 创建要求 Project active、creator具备 `task.create`、assignee active且同 Project可见。
- Agent 在 Run内创建子 Task仍必须显式 assignee，并记录 parent；Human 使用同一 operation。
- `--source-task` 只接受同 Project、已有 task ref 的 submitted/completed Task，创建时复制精确 source ref/head。
- `--retry-of` 只接受同 Project completed/cancelled Task，只保存 lineage；base仍来自当前 actual canonical，除非另给 source Task。
- Human 对 queued Task调用 claim；CLI Agent不得伪造 Human claim，Scheduler不得为 Human创建 Run。
- 每个有parent的子 Task 创建时必须固定 active `coordination_conversation_id` 和该 Conversation 内的 active `result_recipient_participant_id`；parent assignee、child assignee和 result recipient 必须是成员。root Task两字段为空。不提供未定义的 subscriber 或 inbox fallback 通知路径。
- 子 Task 首次进入 `submitted|failed|cancelled` 时，同一事务中以稳定幂等键创建 `system_code=child_result_ready`、`task_id=parent_task_id` 的 Message 和唯一 result recipient。有界 system payload 固定 child Task ID/status/version/submission ID/task ref/head SHA 摘要；Task行和实际 Git ref仍是权威，通知不是第二真相。
- 若 parent 仍为 `waiting`、当前 assignee 就是 result recipient 且没有运行中 Run，同一事务将 parent `waiting -> queued`；CLI Agent recipient 的 recipient行设 `wake_requested=true`，Human 只获得未读。recipient在child创建后被paused时仍持久消息并可queued parent，但不启动新Run，直到恢复active或有权Human显式处置。重试/重启不得重复创建 Message。这形成“A创建B并wait -> B交付 -> A收到定向结果并恢复parent”的唯一路由。
- waiting 只表示责任仍属于 assignee且当前无执行；wake让 Task回 queued。CLI Agent后续由 Scheduler运行，Human需重新 claim。

## 6. 调度、消息递送和并发

### 6.1 Run 选择

Scheduler/Notifier 只为 active CLI Agent创建 Run，并共同遵守 Participant单 Run和全局 `max_parallel_runs`。

Task 候选：Project active、Task queued、assignee为active CLI Agent、无 current Run、backoff到期，且 workspace为`ready|not_needed`，按 `priority DESC, created_at ASC, id ASC`。

Message 候选：Project/Conversation active、recipient pending、wake requested、backoff到期、recipient为active CLI Agent且无 current Run。若同一 recipient 的关联 Task正好是可运行且归其负责的 Task，可将消息合入该 Task Run；否则创建 Conversation Run。不得为投递消息修改无关 Task状态。

Task handlers、Run sources、workers 和排序策略均通过静态列表注册；共用循环不按 kind/name/role写分支链。

### 6.2 Run bootstrap 和未读提示

每个 Run bootstrap 至少包含：

- Participant/Project/Run ID、runtime generation和权限摘要。
- Task Run的完整 Task、base/source信息和workspace路径；Conversation Run明确无 Task/workspace。
- 本次实际携带的每个 Message/recipient ID、sender、Conversation、可选 Task和受大小限制正文。
- 同 Project 全部未确认 recipient 的总数、创建时的稳定高水位、有界 Message/recipient ID样本和不透明 inbox cursor；因数量/大小未携带的项必须可经稳定分页 inbox读取。

bootstrap生成和 Message delivered提交必须可恢复。Run输入已经发生但数据库尚未写 delivered时，recipient 可用 ID直接 ack；重试不得生成第二份 Message。

### 6.3 Inject、退出和 redelivery

- 已有 active Run时可尝试 Inject；支持性和结果由 adapter 报告，Core不假定成功。
- 只有正文实际进入 CLI输入才写 delivered；Inject失败保持 pending。
- Run terminal时，所有由该 Run delivered但未ack的 recipient回 pending并计算独立 backoff。
- wake=false recipient不会单独创建 Run，但 Agent下次任一同 Project Run一定知道其未读存在。
- provider失败不得形成无界付费重试；达到 max deliveries 后停止自动 wake并显式显示 exhausted。

## 7. Operation、权限和 transport

### 7.1 统一 operation registry

每个 mutation/query operation以独立函数注册，descriptor 至少包含名称、所需 capability、scope解析器、输入校验、允许 transport 和是否要求幂等键。CLI、Web 和 coordlink只做编码/展示，调用同一 operation。

主要 capability 至少覆盖：

```text
project.read / project.manage
participant.read / participant.manage
role.read / role.manage / role.bind
task.read / task.create / task.claim / task.submit / task.accept / task.manage
conversation.read / conversation.manage / message.send / message.ack / message.wake
run.read / run.manage / log.read_own / log.read_project / log.export / log.retain
git.read / git.workspace.read / git.accept / git.discard
```

职责不进入 capability 名。失败授权返回 `SCOPE_DENIED` 且零副作用，不泄露目标内容。

### 7.2 认证

- Human 经 operator token认证；Operator CLI使用 Unix socket，Web只监听 loopback并建立短期本机会话。
- CLI Agent 经 per-Run socket和 token认证。socket ownership 缩小可达面，但不替代 token、generation、scope和 capability。
- operation先认证 Participant，再解析 target scope，再合并该 scope Role capabilities，最后执行 object-level membership/assignee检查。
- Agent token可调用其 capability允许且 transport支持的 Project/global operation；不得因 `kind=cli_agent`硬编码职责限制。
- `log.read_own` 只允许 CLI Agent 读取其自身 Run 的已脱敏日志；`log.read_project` 是敏感 Project 级权限，可读取该 Project 其他 Participant Run。Conversation 成员资格单独不授予任何行为日志访问。`log.export/log.retain` 另行检查目标 Run、Project scope 和所需 read capability。
- `workspace_host_path` 只能由服务端生成，并只经认证 Human 使用的 Operator/Web host transport返回给 Task assignee 或同时具备 `git.workspace.read` 的 Human。coordlink、Agent bootstrap、通用状态投影、Event、日志和错误不得暴露该宿主路径；容器也不得提交宿主路径参数。

### 7.3 公开入口

Operator CLI和Web UI必须覆盖 Project、Participant/Role/Credential、Task、Conversation/Message、Run/log、Git accept/integration和GC的正式操作。coordlink必须覆盖运行中 Agent实际可执行的同一结构化 operation，包括 Task、Conversation、Message、查询和其 capability允许的管理操作；不提供 raw DB、raw Git ref或任意 HTTP path。

所有入口支持稳定机器输出；Web mutation使用相同 expected version和idempotency key。自然语言 chat只创建 Message，不直接更改 Task或Git。

## 8. 状态视图和 Web 投影

`status`、Web API和Web UI从相同 query service投影，至少返回：

- Daemon ready/degraded原因和 schema version。
- Project actual/cached canonical SHA、pending Git动作和 retention override。
- Participant kind/status/current Run、配置 fingerprint和有效 Role摘要。
- Task树、assignee、状态、base/head/task ref、integration链接和原因字段。
- Conversation成员、Message recipients逐项未读/递送状态及 exhausted项。
- Run真实状态、resume lineage、container/cleanup事实和行为日志完整性/retention状态。

敏感字段、token、provider secret、instructions全文和未授权 Conversation/日志不得进入投影。宿主私有路径默认不进入投影；只有上节专用 workspace operation 可向授权 Human返回。

Web 会话使用 HttpOnly、SameSite=Strict cookie（TLS时另加 Secure），不将 credential/session token存入 localStorage或输出到页面。所有 mutation 使用非 GET、会话绑定 CSRF token并校验精确 Origin/Host allowlist；CORS默认关闭。响应使用严格 CSP（默认同源，禁止 `unsafe-inline`/`unsafe-eval`）和上下文正确输出编码。SSE/WebSocket 建立时校验认证/Origin/scope，凭据吊销或权限变更后有界时间内终止越权流。

## 9. 稳定错误

至少定义：

```text
NOT_FOUND
INVALID_ARGUMENT
INVALID_STATE
ACTION_IN_PROGRESS
VERSION_CONFLICT
SCOPE_DENIED
STALE_RUN
IDEMPOTENCY_CONFLICT
PROJECT_NOT_ACTIVE
PARTICIPANT_NOT_ACTIVE
CONVERSATION_NOT_ACTIVE
NOT_CONVERSATION_MEMBER
RECIPIENT_REQUIRED
DELIVERY_EXHAUSTED
INTEGRATION_PARTICIPANT_REQUIRED
GIT_HEAD_MISMATCH
GIT_DIRTY
GIT_OPERATION_IN_PROGRESS
WORKSPACE_CHANGED
CANONICAL_STALE
RUNTIME_UNAVAILABLE
LEGACY_SCHEMA_REBUILD_REQUIRED
```

错误必须给调用者可行动的稳定码和安全摘要。秘密、宿主路径、Message私密正文和 provider隐藏字段不能进入错误。

## 10. 启动、关闭和恢复

- 启动顺序：data dir/单实例锁 -> SQLite/migration -> adapter/config校验 -> Git核验 -> pending external reconcile -> Run/container reconcile -> MessageRecipient reconcile -> behavior log index核验 -> scheduler/notifier/GC -> CLI/Web ready。
- reconcile 未完成前所有 mutation返回 `DAEMON_NOT_READY`；只读诊断可返回有限状态。
- 关闭先停止接收 mutation，再停新 claim/wake，等待事务，向 Runtime 请求停止或保留可接管 Run，最后关闭 SQLite/lock。
- Daemon crash不得丢失已提交 Task、Conversation、MessageRecipient、Git intent、Run ownership或行为日志 offset/hash。

## 11. Core 不变量

1. Participant 是 Human/CLI Agent 唯一身份和权限主体，无 `Boss` 或 `agents` 第二真相。
2. `kind` 只改变认证与执行介质，不改变 Task、Conversation、Message、Git或 capability语义。
3. Task不是 Conversation；Message必须属于 Conversation且Task关联可空。
4. Message有显式一个或多个 recipients，每个 recipient 独立未读、递送、ack和重试。
5. 只有明确 CLI Agent recipient且 wake requested 才能触发 Run；所有 Run都提示同 Project未读。
6. Human 与 CLI Agent 的 git Task 使用同一 workspace、capture、task ref、accept和CAS合同。
7. 同一 CLI Agent最多一个 starting/active Run；Conversation Run无 Task也受 Participant generation fence。
8. 角色职责完全配置化；所有入口调用同一 operation和权限检查。
9. 状态变化、外部 intent和Event原子；Event/日志不能授权状态推进。
10. 完整行为流与关键Event分层保存，7天默认、Project override和`long_term`语义唯一。
11. terminal/closed成功对象不得携带旧失败原因；旧 token/Run不得覆盖新状态。
12. 新 handler/worker/operation/parser/check通过列表注册，替换机制同变更删除旧路径。
13. 子 Task结果只通过固定 coordination Conversation、result recipient和幂等system Message回传，可机械唤醒waiting parent。
14. Resume不得跨 Project、Task/Conversation scope、adapter/config或Task workspace identity。
15. 每个Run恰有一个Behavior Log index作retention/long_term真相；Conversation成员资格不隐式授权Run日志。
