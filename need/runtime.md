# CoordPlane Runtime 与隔离需求

状态：Draft for owner review
依赖：`README.md`、`core.md`

## 1. 目标和边界

Runtime 负责把一个 Core Run 变成真实、可观察、可停止、可恢复的 CLI 进程执行。第一版产品路径只要求 Docker。

Runtime 必须：

- 为每个 Run 创建独立临时容器。
- 为每个 work/integration Task 保持私有 workspace，为每个 Agent 保持私有 CLI home。
- 启动、恢复、停止和检查 Codex/Claude 等 CLI。
- 尽早记录 CLI native session ID。
- 证明 live process 与 resumable session 是两种不同事实。
- 递送 pending Message，支持可选 Inject，并在失败时 fallback 到新 Run。
- 在取消、超时、Daemon 崩溃和 Docker异常后收敛 Run 与资源。

Runtime 不决定 Task 是否正确、不执行 Git 集成、不提供 external runtime 等价合同、不建设 tool adapter/skill 平台，也不把容器或 transcript 作为状态真相。

## 2. 受信边界

### 2.1 受信组件

- CoordPlane Daemon 是受信宿主进程。
- 只有 Daemon 可以访问 Docker daemon、SQLite、control repo 和 runtime root。
- CLI Agent、项目源码、项目 Git config/hooks 和 Agent 生成的文件都视为不可信输入。

### 2.2 隔离单位

- 每个 Run 一个临时容器。
- 每个 work/integration Task 一个私有 workspace；同一 Task 的后续 Run 可以复用。
- 每个 Agent 一个私有持久 home；不同 Agent 永不共享。
- 同一 Agent 第一版最多一个 starting/active Run，因此不会并发写同一 home 或 session cache。
- 多 Agent 并发必须来自不同 Agent、不同容器、不同 workspace 和不同 home。

容器只是可删除执行壳。Task workspace 和 Agent home 是可恢复资源，但都不是 Core 状态真相。

## 3. 目录布局

在 `data_dir` 下建议使用以下固定布局：

```text
data_dir/
  coordplane.db
  repos/<project-id>.git
  workspaces/<project-id>/<task-id>/
  agent-homes/<agent-id>/
  logs/<run-id>/
  run-control/<run-id>/
  handoff/<run-id>/
  locks/daemon.lock
```

要求：

- 所有路径必须在启动时 canonicalize，并校验位于配置 root 内。
- 目录必须由 Daemon 服务用户拥有；agent-homes 和 token 临时目录权限至少为 `0700`。
- workspace、home、log、run-control和handoff路径必须由服务端ID生成，调用者不能提供宿主绝对路径。
- 清理前必须重新解析 symlink、检查 root containment 和 ownership marker。
- Agent-facing response 只显示容器路径、对象 ID 或 redacted path，不显示宿主绝对路径。
- `handoff` 只用于 `git.md` 的短生命周期bundle/pack传输，导入task ref后立即清理；它不是Artifact/ObjectStore。

## 4. Docker 拓扑

### 4.1 必须挂载

一个 Run 容器最多需要：

| 容器路径 | 来源 | 模式 |
| --- | --- | --- |
| `/workspace/project` | 当前 work/integration Task 私有 workspace | `rw` |
| `/home/agent` | 当前 Agent 私有 home | `rw` |
| `/usr/local/bin/coordlink` | 受信版本的 coordlink | `ro` |
| `/run/coordplane` | 本Run私有control目录：token、bootstrap、`api.sock` | bind `rw`，但宿主owner/mode使Agent只有文件读取和socket连接权限 |

conversation Run 不挂载项目 workspace，以 `/home/agent` 作为工作目录，只处理 Message 和 Task协调。需要读取或修改项目代码时，Agent 必须显式创建 work/review Task，不能在对话 Run 偷偷产生交付代码。

每个Run的control目录由Daemon拥有，目录建议`0750`、token/bootstrap建议`0440`、Unix socket建议`0660`，容器Agent只属于受限group且不能创建、替换或删除目录项。目录bind mount必须让Daemon崩溃后在同一路径重建`api.sock`时对既有容器可见；不得把整个run-control root挂入容器。

### 4.2 禁止挂载

容器不得挂载：

- Docker socket。
- SQLite 数据库或整个 `data_dir`。
- Project control bare repo。
- 其他 Task workspace。
- 其他 Agent home。
- Boss 的真实 `$HOME`、`~/.codex`、`~/.claude` 或整个配置目录。
- 未经 allowlist 的宿主路径、SSH agent socket、Git credential store。

### 4.3 容器安全基线

- 使用非root且不同于宿主Daemon service UID的容器UID/GID；补充group只用于读取本Run文件和连接本Run socket。
- 启用 `no-new-privileges`，drop Linux capabilities。
- 设置PID、内存、CPU和最大运行时限。支持磁盘quota的runtime profile应设置workspace/临时空间上限；宿主不支持时必须明确报告未启用，不能伪称已隔离容量。
- 不发布入站端口。
- 容器主进程必须是 CLI 或一个正确转发 signal/exit code 的极薄 init wrapper。
- Docker label 至少包含 `coordplane.managed=true`、project/task/agent/run ID、generation 和 launch nonce。
- 容器名必须由 Run ID确定；重复 create 需要幂等识别，不能生成无归属随机容器。

### 4.4 网络

- coordlink只通过本Run的`/run/coordplane/api.sock`访问Daemon，不通过宿主TCP；socket路径和Run token必须同时匹配scope。
- Agent网络只用于模型provider和项目明确允许的外部服务。
- 默认 profile 不允许访问 Docker socket或宿主文件服务。
- Provider/Git 凭据没有明确注入时，网络可达也不得获得凭据。
- 第一版不要求通用网络 policy engine；允许的网络配置是少量静态 runtime profile。

## 5. Workspace、Home 和 Bootstrap

### 5.1 Workspace

- Workspace 由 `git.md` 准备，并在 Run active 前证明存在、归属正确、HEAD/base 正确。
- work/integration Task 在同一 Task 的后续 Run 中复用同一 workspace，容器内路径始终为 `/workspace/project`。
- Runtime 不静默 checkout、reset、clean、rebase 或 merge workspace。
- Workspace dirty、存在 Git operation 中间态或有未捕获提交时，不得自动删除。
- Runtime 不允许一个 Run 改用其他 Task workspace，即使 Agent 和 Project 相同。

### 5.2 Agent Home

- Home 只包含该 Agent 的 CLI 配置、provider 认证缓存、native session 数据和用户级 cache。
- 不得两个 Run 同时挂载同一 Agent home。
- Start 和 Resume 都使用相同 Agent home，但使用新的 Run token。
- Home 的 session 数据只是 CLI provider 的恢复材料，不替代 `native_session_id` 和 Run 记录。
- Agent archived 后，只有不存在 active/recoverable Task 和 pending Message，且 Boss 显式执行 GC 时才可删除 home。

### 5.3 Bootstrap

每次 Run 的 bootstrap 必须由 Core 当前事实生成，至少包含：

- Project、Agent、Task 和 Run ID。
- Task title/description、parent 和 kind。
- 代码 Task 的 base SHA、当前 captured head/task ref（如有）。
- 可选source Task的固定source ID/head和容器内convenience ref；明确该ref只供checkout/diff，不能替代保存的SHA。
- work/integration Task 的 workspace 容器路径；conversation 不提供项目路径。
- pending Message ID、发送者和受大小限制的正文。
- 固定 coordlink 命令说明。
- 明确要求 Agent 用原生 Git开发，并显式调用 `task wait/submit/fail`。
- 明确说明 CLI exit 不等于 Task 完成。

Bootstrap 是一次性投影，不是持久状态。不得把全部团队任务、其他 Agent 私有状态、secret 或宿主路径塞入 prompt。不得覆盖项目自己的 `AGENTS.md`/`CLAUDE.md`；CoordPlane 指令使用独立只读文件或启动 prompt。

## 6. Run 扩展字段

除 `core.md` 定义的字段外，Run 必须保存以下 runtime 字段；它们仍属于 Run，不形成新对象：

| 字段 | 要求 |
| --- | --- |
| `launch_nonce` | 容器 create 前生成并持久化，用于 ownership 对账 |
| `launch_operation_id` | create/start生命周期的稳定operation ID；Run terminal前不变 |
| `launch_phase` | 单调`intent -> created -> start_issued -> process_observed`，每步外部动作前后持久化 |
| `home_path` | Daemon 内部 Agent home 路径 |
| `container_name` | 由 Run ID确定 |
| `deadline_at` | 可空；超时边界 |
| `last_observed_at` | 最近一次 Docker/进程真实观察时间 |
| `launch_mode` | `start`或`resume`；一个Run只能选择一次 |
| `resume_native_session_id` | Resume 输入；Start 时为空 |
| `runtime_error_code` | 稳定 runtime 错误 |
| `cleanup_operation_id` | cleanup pending/blocked重试使用的稳定operation ID；removed后可保留审计 |

`launch_nonce`、Run ID 和 generation 必须同时匹配，Daemon 才能接管、停止或删除容器。

## 7. CLI Adapter

### 7.1 Docker executor 与 CLI adapter

Docker side effect只有一个 owner：Runtime executor。它使用可由Run字段重建的durable runtime ref，不依赖旧进程内handle：

```text
Create(ctx, ContainerSpec) -> RuntimeRef
Attach(ctx, RuntimeRef) -> LiveHandle
Start(ctx, RuntimeRef) -> LiveHandle
Inject(ctx, RuntimeRef, RuntimeInput) -> InjectResult
Inspect(ctx, RuntimeRef) -> LiveState
Wait(ctx, RuntimeRef) -> ExitFact
Logs(ctx, RuntimeRef, from_start=true) -> Stream
Stop(ctx, RuntimeRef, grace_period) -> StopResult
Remove(ctx, RuntimeRef) -> RemoveResult
```

`RuntimeRef`至少由container ID/name、Run ID、generation和launch nonce组成。Daemon重启后必须能用它Attach/Wait/Logs/Stop，不依赖旧内存channel。

CLI adapter只拥有provider命令和协议解析，使用编译期静态注册列表：

```text
Name() -> string
BuildStartCommand(LaunchSpec) -> CommandSpec
BuildResumeCommand(ResumeSpec) -> CommandSpec
BuildInjectInput(MessageInput) -> RuntimeInput          # 可选
ParseEvent(frame) -> AdapterEvent
ResumeCompatible(previous, next) -> bool
```

Adapter 注册项声明：

```text
execution_model: one_shot | live
supports_resume: true | false
supports_inject: true | false
```

这只是 adapter 本身的静态元数据，不是动态 capability registry。

### 7.2 Executor/Adapter 约束

- Adapter生成结构化argv；不得用`sh -c`拼接Boss/Agent输入。
- Executor统一create/start/attach/inject/inspect/wait/stop/remove容器和进程I/O；Adapter不得形成第二套Docker生命周期。
- Adapter 只报告运行事实，不直接更新 Task、Message 或 Git。
- native session ID 一旦从 provider 事件流获得，必须立即写 Run 和 `run.session_recorded` Event，不能等 CLI 退出。
- Executor Stop必须幂等并终止完整进程树；已退出/不存在视为幂等成功。
- Logs必须能从容器输出起点replay，Daemon重启后不能因旧stream channel丢失session/exit前输出。
- Inject由Adapter只构造provider输入、Executor执行实际process I/O；两者都必须校验静态支持，Executor还必须校验Run ID、generation、launch nonce和实时process状态。
- Inject accepted 只表示 CLI 接受输入，不表示 Message 已读或已处理。
- 新增或删除 adapter 只添加/删除实现和一个注册项，Runner 主流程不得判断 `if adapter == codex`。

### 7.3 One-shot 与 live

必须分开三类事实：

```text
CLI process/container 当前存活   <- Docker inspect/Wait
CLI session 将来可能 resume      <- native session ID + Agent home
Message 已被处理                 <- Message acknowledged
```

- One-shot CLI 只在 OS process/container 实际运行期间是 live。
- CLI 退出后即使 native session 可 resume，也不能作为 Inject 目标。
- `native_session_id`、旧 heartbeat、Task running 或 route 字段都不能单独证明 live。
- 新 Message 到达已退出 one-shot session 时，必须创建新 Resume Run。
- 第一版production adapter应使用one-shot执行模型。未来注册live adapter时，必须同时支持Inject或明确可由Executor关闭当前turn；`live && !supports_inject`且不会自行退出的组合必须在注册时拒绝。

## 8. Run 启动流程

Core claim 事务创建 state=starting 的 Run 后，Runtime 按注册步骤执行：

```text
prepareWorkspace
prepareAgentHome
writeRunToken
writeBootstrap
openRunAPISocket
createContainer
attachStreams
startCLI
verifyLive
```

每一步是独立函数并在静态列表中注册。要求：

1. 外部动作前把launch nonce、launch operation ID和container intent写入Run，并写同operation ID的Event；Run字段/state是恢复权威，不能只靠Event恢复。
2. 任一步失败都终结 Run，撤销 token，按 Core retry policy 处理 Task。
3. workspace、home、coordlink、token、bootstrap、per-Run API socket和CLI binary未全部准备好前，Run不得active。
4. Create前写`launch_phase=intent`；Container create后立即保存container ID并写`created`。Start前先写`start_issued`，观察到matching process running后写`process_observed`。Phase只能前进，不能靠Event推导。
5. Container设置`AutoRemove=false`、`RestartPolicy=no`，terminal事实持久化后才能remove。
6. 在Docker start前建立attach，或保证Logs从offset 0完整replay；快速one-shot不能丢stdout/native session/exit。
7. CLI start后，Daemon观察到进程已启动、持有Wait监控且尚未观察到terminal，再以事务写Run active与Task running。active是有界观察事实，不是SQLite与进程的瞬时原子承诺。
8. CLI可能在active事务前调用coordlink；coordlink必须对socket短暂重建和当前generation的`RUN_STARTING`做有deadline的自动重试，不能把准备/Daemon重启窗口当业务失败。
9. Process在active事务前退出时，Run从starting直接转exited并按Core矩阵处理。
10. stdout/stderr从启动起写入Run log，必须有大小轮转和截断标记。
11. Supervisor持续等待真实退出，不使用固定sleep推断状态。

Agent成功调用wait/submit/fail时，Core在同一事务把Task转finishing、保存`Run.requested_outcome`并撤销token。Runtime在命令响应已经送达后，先CAS写入确定性的`stop_operation_id`和Event，再请求CLI graceful stop，超时再强制停止；Run terminal后才由Core应用wait/fail，submit则进入`git.md`的静止workspace capture。

## 9. Resume 和消息唤醒

### 9.1 Resume

- Resume 总是创建新 Run，旧 Run保持 terminal。
- 新 Run 记录 `resumed_from_run_id` 和 `resume_native_session_id`；work/integration 使用相同 Task workspace，所有 kind 使用相同 Agent home、新 token 和新 container。
- Daemon只选择同Agent/Task中按`ended_at,id`排序的最新terminal Run作为resume来源；不得静默跳回更旧session。Boss显式指定旧Run属于未来能力。
- 只有相同Agent、相同Task、相同workspace语义、相同adapter ID且`ResumeCompatible`通过时可resume；不能把Codex session交给Claude或把旧Task session恢复到新Task branch。
- Adapter静态不支持resume或兼容检查失败时，新Run从一开始使用launch_mode=start，不先创建失败resume进程。
- 实际Resume CLI返回session-not-found时，该Resume Run以failed/exited + `RESUME_UNAVAILABLE`终结，Message回pending；Scheduler随后创建另一个generation更高的fresh Start Run并写`run.resume_fallback` Event。一个Run内不得启动第二个CLI进程。
- Fresh fallback 必须标记 `RESUME_UNAVAILABLE` 和来源 Run，不能伪称恢复了模型上下文。
- transient provider/runtime 错误按 Task retry/backoff 处理，不能无限重试。

### 9.2 Inject

只有全部条件满足时才可尝试；Notifier先让Adapter构造provider输入，再调用Executor Inject：

- Run DB state=active。
- matching container/process 此刻真实存活。
- Run ID、generation 和 launch nonce 一致。
- Adapter 静态声明supports_inject并实现`BuildInjectInput`，Executor支持对应输入transport。
- Message delivery Task就是该Run的Task；或者Message delivery Task是同一Agent的conversation Task且输入明确标注delivery/related Task ID。

Inject 失败、turn mismatch 或 adapter unsupported 时，Message 保持 pending。不得为了同一 Agent 启动第二个并发 Run；当前 Run terminal 后再 Resume。多个 pending Message 可以合入一个 Run 输入，但必须列出每个 Message ID，不能按 session route 粗粒度去重。

## 10. Progress、Heartbeat 和日志

- Supervisor heartbeat 只表示最近确认容器/进程 live，写入 Run `heartbeat_at/last_observed_at`。
- Agent `coordlink progress` 产生小型 Event，用于 Boss 看进度；不延长 deadline，不改变 Task状态，也不作为完成证据。
- CLI stdout/stderr 写普通轮转文件；日志可跟随，但不解析成 Task状态。
- 第一版不要求保存 chain-of-thought、每个 tool call、provider 原始完整事件或永久 transcript。
- Redaction 必须在日志写入边界处理已知 token/credential；事件和错误不得包含 secret、prompt 全文或宿主路径。

## 11. 取消和超时

### 11.1 取消顺序

`task cancel` 的顺序：

1. Core事务把Task转cancelled、递增Task generation、撤销token；存在Run时同时写稳定`stop_operation_id`和Event。Task finishing/pending action时返回`ACTION_IN_PROGRESS`，Boss可先用`run stop`促使收敛。
2. Runtime 使用独立 cleanup context 调用 Stop；不能复用已取消的 Run context。
3. 先发送 graceful signal，超过 grace period 后强制 kill 完整容器。
4. 真实退出后 Run 转 cancelled，`cleanup_state=pending`。
5. Container remove、listener关闭以及socket/token/bootstrap/control目录删除全部完成后，才收敛到`cleanup_state=removed`。

如果真实进程已先退出并由 CAS 写入 terminal，终态事实获胜；如果 cancel intent 先提交，迟到的正常 exit不能把 Task恢复或完成。

`run stop` 是不同操作：Core只在Run写`stop_requested_at/reason/stop_operation_id`并撤销当前token，Task暂不cancel。没有requested outcome时，真实退出后Run转interrupted，Task按retry policy回queued或failed；已有requested outcome时继续finishing收尾。Stop/cleanup ownership使用Run自身generation/launch nonce，不使用取消后变化的Task generation。

### 11.2 超时

- `deadline_at` 到期先持久化timeout stop operation，再执行与取消相同的停止流程，Run terminal记录timed_out。
- 若requested outcome已在timeout/stop intent之前durable，Task仍按wait/submit/fail finishing流程收尾；timeout只决定Run终因和是否强制kill。没有requested outcome时，Task才按runtime retry policy requeue或failed。迟到outcome因token已撤销必须拒绝。
- Stop 不能执行 `git reset/clean`，不能删除 workspace 的未提交修改。

## 12. Daemon 重启和周期对账

### 12.1 单实例

- Daemon 必须持有 `data_dir` OS file lock。
- 第一版不允许多个 Daemon 管理同一 SQLite/runtime root，不设计分布式 lease。

### 12.2 对账前置

- 启动时先读取非终态 Run，再按 Run ID/generation/launch nonce 查询 Docker。
- 对仍可能live的Run，Daemon必须在恢复业务监控前按Run行重建同一control目录的`api.sock` listener；listener本身不授权请求，token/generation仍必须校验。
- Reconciliation 完成前不得 claim 新 Task。
- 同一 reconciliation 周期运行，修复运行中漂移。
- Docker API 不可达时进入 degraded：保持原 Run状态、停止新调度、显示错误；不得推断 container lost 或 cleanup成功。

### 12.3 对账矩阵

对账必须先处理durable终止意图：任何starting/active Run只要已有requested outcome、stop/cancel/timeout/shutdown intent或deadline已到，Attach/Inspect后就必须幂等Stop并继续对应terminal/outcome流程，不能按普通live Run恢复业务执行。

| SQLite 事实 | Docker 事实 | 必须行为 |
| --- | --- | --- |
| starting且container ID为空 | 容器不存在 | 依据launch intent幂等继续create，或失败/requeue |
| starting且container ID为空 | 确定性名称下存在matching labels/nonce容器 | 校验ownership后CAS保存container ID，再按其created/running/exited事实继续；不重复create |
| starting且container ID已落库 | Docker明确NotFound | launch phase为intent/created则Run failed；start_issued/process_observed则保守interrupted。随后按outcome/retry规则收敛，不得为同一Run重新create第二个CLI |
| starting | matching 容器created | 重新Attach日志并只Start一次；不得重复create |
| starting | matching 容器running | 接管、附加wait/log；无终止intent且真实CLI live后active |
| active | matching 容器running | 无终止intent时重新监控同一Run，不创建新Run |
| starting/active | matching 容器 exited | 保存真实 exit code，按 Core terminal 规则收敛 |
| active | Docker 明确 NotFound | Run interrupted，Task requeue/failed；不得成功 |
| terminal | matching 容器存在 | 幂等 stop/remove，结果不改变 Run outcome |
| 任意 | labels/nonce/generation 不匹配 | 不接管、不删除，cleanup blocked 并通知 Boss |
| 无 Run | managed container 存在 | 隔离/quarantine并提示人工处理；没有Run中的expected nonce就不得自动删除 |

如果同一 Agent 存在多个可能 live 的 owned container，必须停止调度该 Agent并报告 invariant violation，不能任选一个继续。

### 12.4 优雅关闭

- 收到SIGTERM后立即停止新claim，等待配置的shutdown grace。
- Grace内已自然结束的Run正常收敛。
- Grace到期后，为每个starting/active Run持久化`daemon_shutdown` stop intent和stop operation ID并由Executor Stop；Run最终interrupted或按已保存outcome收尾，Task按对应Core规则恢复。
- 第一版不允许优雅关闭后故意留下无人监控的运行容器。

## 13. Cleanup 和 GC

### 13.1 Run 容器

- `cleanup_state`覆盖Container和per-Run control目录的整体清理：尚未创建任何可清理资源时`not_needed`；control目录/listener/token或container任一创建后即`pending`并写确定性cleanup operation ID；只有listener已关闭且container、socket、token、bootstrap和control目录全部absent时才`removed`；任一步失败或ownership不明时`blocked`。人工/周期重试先`blocked -> pending`，沿用同一operation ID。
- exited、failed、interrupted、cancelled、timed_out Run 都必须最终 stop/remove 容器。
- Docker NotFound只表示container子步骤幂等完成；仍需删除control资源才能写overall removed。Docker unreachable且Run曾有container时不可猜container已删除。
- cleanup 失败写 `cleanup_state=blocked` 和稳定错误，周期重试；不能修改原 Run终态。
- 旧容器仍可能运行时，禁止该 Agent 的下一 Run。

### 13.2 Workspace

自动删除必须同时满足：

- 没有 starting/active Run。
- Task completed/cancelled。Failed Task因Boss仍可retry，既不自动删除也不允许discard；Boss必须选择保留workspace原地retry，或先cancel再按closed Task规则处理。
- 任何需要保留的 commit 已由 task ref 捕获；或确定从未产生代码结果。
- Workspace clean，且不存在 merge/rebase/cherry-pick 中间态。
- 没有 integration Task、pending action或可恢复Run依赖。
- 从Task不可变`closed_at`起已达到`retention.completed_workspace`。
- ownership marker、路径 root 和 symlink检查全部通过。

任一事实不确定、workspace dirty、存在未跟踪文件或 task ref 未捕获时，一律不自动删除。Boss只能在`gc preview`读取当前fingerprint后，用`gc discard-workspace --task T --expected-fingerprint F --request-id R`主动放弃dirty/untracked内容；命令语义和不可覆盖fence见`core.md`。

Workspace/log删除属于`core.md`定义的幂等派生GC：周期worker和Boss `gc run --confirm`复用同一静态步骤列表，每次删除前重读Task/Run、当前retention和ownership marker；崩溃后从外部actual absence继续，不增加Workspace或GC业务对象。

Cancelled Task的workspace被显式discard后不可原地retry；需要继续时创建`retry_of_task_id`新Task并从新Task固定base准备private clone。Failed Task retry时：workspace存在则复用；workspace absent且该Task所有历史Run都从未到达`launch_phase=start_issued`时，证明Agent writer从未启动，可按Task不可变base/source字段重新执行首次prepare；任一历史Run已到`start_issued/process_observed`时，absent必须fail loud并通知Boss。不得把曾可写workspace的丢失伪装成授权discard，也不得从移动ref重建。

### 13.3 Home 和日志

- Agent home 默认长期保留，直到 Agent archived、无可恢复 Task/Message 且 Boss 显式 GC。
- 日志按大小轮转；Run terminal后达到`retention.run_log`才可删除。删除日志不删除 Run终态、exit code和关键 Event。
- 不建设 artifact/archive 平台。需要长期保存的项目输出应进入 Git 或由 Task/Message 引用外部位置。

## 14. Provider secret

- Provider token/key 只从 Daemon 配置的环境变量或只读 secret file allowlist注入。
- Secret不得出现在 argv、SQLite、Event、bootstrap、普通日志、Agent-facing error 或 `docker inspect` 可读 label。
- 若 provider CLI 必须把认证缓存写入 home，该 home 仅属于该 Agent且权限受限。
- 第一版不建设 SecretProvider/Vault；缺少 secret 是明确 runtime error，由 Boss修正配置后 retry。

## 15. Runtime 不变量

- 正式隔离路径只有 Docker；host/external 仅可作为不计入产品验收的开发工具。
- 每个 Run 一个容器，每个 work/integration Task一个 workspace，每个 Agent一个 home；conversation没有项目workspace。
- 同一 Agent最多一个 starting/active Run。
- Run active必须基于matching process/container的最近启动观察和持续Wait监控；它不是跨SQLite/Docker瞬时一致承诺。Inject前实时Inspect，terminal观察后有界收敛；native session ID不是live证明。
- Resume 创建新 Run，terminal Run永不复活。
- Message 先 durable；Inject 只是可选优化，失败时 resume/new Run。
- Container不得获得 DB、Docker socket、control repo、其他 workspace/home 或 operator token。
- 每个Container只获得自己的per-Run API socket/control目录，不得看到operator socket或其他Run socket。
- workspace 未准备好前 Run不得 active。
- 取消/超时先持久化 intent，再幂等 stop/remove，旧 token立即失效。
- Docker不可达时 fail loud，不猜 lost或removed。
- active、可恢复、dirty 或未捕获提交的 workspace不得 GC。
- Runtime不判断 Task完成、不执行代码验收、不代理 Git集成。
