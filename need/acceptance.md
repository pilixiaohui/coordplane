# CoordPlane 验收需求

状态：Draft for owner review
依赖：`README.md`、`core.md`、`runtime.md`、`git.md`

## 1. 目标和非目标

本文件定义第一版完成所需的测试和真实验收。它是一份普通工程测试合同，不是产品内的 validation/acceptance engine。

验收必须证明：

- 六类协调事实能持久化、并发更新和恢复。
- Boss/Agent 使用正式 CLI入口完成任务和通信。
- 两个 CLI Agent 在真实隔离环境中并发。
- Message 在 Inject、Run退出和 Daemon重启后不丢。
- Git HEAD由控制器机械读取，commit由 task ref保护。
- canonical ref 的并发更新不丢失，stale/conflict交给 integration CLI Agent。
- 取消、超时、崩溃和 GC不产生伪完成、孤儿运行或代码丢失。

本文件不要求：

- 在产品数据库中保存验收 verdict、predicate、evidence bundle或 release acceptance。
- 自建 artifact/object store。
- 判断 Agent代码的业务质量。
- 保存每个 tool call或完整模型行为流。
- 用一个超大 live gate代替低层回归测试。

测试命令、退出码、标准测试报告、真实 SQLite/Git/Docker状态和必要日志就是验收依据。

## 2. 测试设计规则

每个重要测试必须保护一个命名不变量，并写清：

```text
Invariant
Risk layer
Production entrypoint/boundary
Positive state assertion
Forbidden side effect
Fault or misuse case
Mocks allowed
Mocks forbidden
Verification command
```

要求：

- 使用能覆盖风险的最低真实边界。
- 测 public CLI/API，而不是只调用内部 helper后声称入口正确。
- 状态测试断言 SQLite行和 Event；Git测试断言 actual refs/objects；Runtime测试断言 actual Docker mounts/processes。
- 每个失败测试同时断言“错误副作用没有发生”。
- 并发、取消、goroutine和共享 cache改动必须运行 race gate。
- 真实故障先归约到可重复的低层回归测试，再重跑完整 E2E。
- 禁止用固定 sleep证明 scheduler/reconciler已运行；等待具体 Event、状态或 Docker/Git事实，并带明确 deadline。
- 删除旧机制时，同一变更删除旧 fixture和正向兼容测试；只保留防止旧路径回归的负向 guard。

SQLite startup no-side-effect signatures have one explicit sidecar rule: the main
database and `-wal` bytes must remain identical. The `-shm` path, type, mode, and
size are durable directory evidence, but its WAL-index and lock bytes are
process-local SQLite state and are excluded from byte equality because a
read-only preflight may acquire and release those slots. Tests must not treat
that permitted index churn as a migration or startup side effect.

## 3. 测试层级

| 层级 | 必测内容 | 必须使用的真实边界 |
| --- | --- | --- |
| Static guard | 对象/表 allowlist、删除的入口和包、静态注册、文档一致性 | 源码和 schema扫描 |
| Pure logic | FSM、排序、GC predicate、错误分类、ref名和状态投影 | 表驱动纯函数 |
| Adapter conformance | 本版选定的production adapter（Codex或Claude）协议事件、session ID、exit、resume和声明能力 | 真实协议 frame；外部 provider可 fake |
| Public contract | `coordplane`、`coordlink`、scope、幂等、错误和 durable副作用 | 正式 CLI + 本地 Daemon/API |
| Storage/state | claim、generation fencing、Message redelivery、migration、restart | file-backed真实 SQLite |
| Git component | private clone、bundle/import、task ref、CAS、stale、GC | 真实临时 Git repo和git进程 |
| Runtime integration | mount、process、token、cancel、resume、cleanup | 真实 Docker daemon |
| Deterministic E2E | Boss到两个Agent到Git收敛 | 真实Daemon/SQLite/Git/Docker + scripted CLI |
| Performance | 四Agent饱和、控制面延迟、Docker/Git阶段、恢复和资源泄漏 | 真实Daemon/SQLite/Git/Docker +固定fixture/scripted CLI |
| Real CLI E2E | 最终 provider接线和真实Agent并发 | 真实Daemon/Docker/Codex或Claude |

不得 mock 被验收的边界：

- 验 SQLite claim不能用内存 map。
- 验 Git CAS不能用字符串变量。
- 验 Docker mount/kill不能 mock `docker run`。
- 验正式 CLI命令不能只调 operation helper。
- 验 Message durability不能只检查 notifier被调用。

## 4. 架构静态约束

### 4.1 文档和术语

- 权威需求目录只能包含五份文档，根索引必须只链接这五份。
- 对象/状态定义只能出现在 `core.md`；runtime/git文档只能扩展相应字段和外部事实。
- same-turn/Inject只能描述为可选优化，任何正确性断言必须有 durable Message + resume/new Run路径。
- SQLite不能被描述为代码真相，Git不能被描述为任务/消息真相。

### 4.2 Schema allowlist

产品业务表只允许：

```text
projects
agents
tasks
runs
messages
events
```

允许少量纯基础设施表，例如 `schema_migrations` 和 request dedupe，但它们不得有独立业务状态机或公开 API。新增业务表必须先修改本需求。

### 4.3 删除旧入口

静态 guard 必须阻止以下路径重新成为生产入口：

- 通用 `/call`、`/capabilities`、`/skills`。
- `coordlink capability ...`、`coordlink skill ...`、`coordlink call NAME`。
- 动态 capability/skill registry、TeamConfig parser/policy服务。
- Validation/assessment/release acceptance/object store服务。
- 通用 QueueItem/DeliveryAttempt/Mailbox状态机。
- Backend Agent-facing `git.status/diff/commit/rebase/merge/resolve/rollback` wrapper。
- ChangeSet/GitOperation/MergeAttempt/ConflictSet/RollbackPoint业务表或服务。
- 以 transcript/Agent文本写 Task completed的代码路径。

旧术语可以出现在 migration删除说明和负向测试数据中，但不得有生产 writer、handler或正向 fixture。

### 4.4 Build to Delete

静态/合同测试必须证明：

- Workers来自一个静态注册列表。
- CLI adapters来自一个静态注册列表。
- Runtime prepare/cleanup步骤来自列表。
- Task kind hook来自列表。
- 删除一个 adapter/步骤/检查不需要修改主循环或添加名称特判。

## 5. 命名不变量

| ID | 不变量 |
| --- | --- |
| INV-01 | SQLite只保存协调事实，Git refs/objects只保存代码事实，prompt/log不是真相 |
| INV-02 | 业务对象只有 Project、Agent、Task、Run、Message、Event |
| INV-03 | Task和Run分离；outcome先进入finishing，Run terminal/capture后才改变结果；旧Run受generation fence阻止 |
| INV-04 | 同一Agent和同一Task最多一个starting/active Run，claim不重复 |
| INV-05 | Message先持久化、至少一次递送、pending/delivered可原子ack、有限重投；Inject失败不丢消息 |
| INV-06 | 每个Docker Run只看到自己的workspace/home/token，不能写control repo |
| INV-07 | live process和resumable native session分离；resume创建新Run |
| INV-08 | cancel/timeout/restart最终收敛Run和container，不伪完成Task |
| INV-09 | 代码结果来自实际clean workspace HEAD，并先由不可变task ref保护 |
| INV-10 | canonical只用fast-forward expected-old CAS推进，竞争更新不丢失 |
| INV-11 | stale/non-FF/conflict由integration CLI Agent处理，Daemon不做智能Git决策 |
| INV-12 | Capture/CAS/Run cleanup由业务行窄pending字段和operation ID恢复；closed资源GC只用可重算fence，Event不充当隐藏Operation |
| INV-13 | active、dirty、可恢复或未捕获的workspace/ref不能被GC |
| INV-14 | Boss能从正式入口看到责任、真实Run、消息、base/head、错误和最终SHA |
| INV-15 | 第一版没有动态registry、TeamConfig DSL、验收引擎、artifact平台或per-tool审计 |
| INV-16 | 四Agent负载下控制面不成为瓶颈，Daemon崩溃后在有界时间恢复且无重复/泄漏 |
| INV-17 | 第一版production维护面保持在已声明SLOC预算内，超预算不能靠隐藏/压缩路径规避 |

## 6. Core 合同场景

### CT-01 Migration 和重启

Boundary：真实 file-backed SQLite + Daemon启动入口。
覆盖：INV-01、INV-02、INV-12。

必须断言：

- 空库 migration成功并只产生允许的表。
- 二次 migration幂等。
- 创建 Project/Agent/Task/Message后重启，行、version和Event保持。
- migration中断不会产生部分可用 schema或开始调度。
- 数据库损坏/不可写时 Daemon拒绝 ready。

禁止副作用：创建旧业务表、使用内存状态补齐缺失行。

### CT-02 Claim 竞争

Boundary：真实 SQLite事务 + 两个并发 scheduler worker。
覆盖：INV-03、INV-04。

必须断言：

- 对同一 queued Task并发 claim，恰好一个 Run创建成功。
- 同一 Agent有两个 queued Task时，恰好一个获得 current Run。
- generation只递增一次，Task.current_run_id与Run一致。
- 失败 claim无 Run/Event/容器副作用。
- 排序为 priority DESC、created/id ASC。

并发测试必须在 `-race` 下重复运行。

### CT-03 Run fencing

Boundary：正式 coordlink写入口 + 真实 SQLite。
覆盖：INV-03。

流程：创建 Run-1，终结并创建 generation更高的 Run-2，再用Run-1 token调用 progress/message/submit。

必须断言：所有请求返回 `STALE_RUN`，Task/Message/head/version/Event数量均不改变；Run-2正常请求成功。

### CT-04 Exit 不是完成

Boundary：Runner supervisor + scripted adapter。
覆盖：INV-03、INV-08。

- CLI exit 0但未调用 wait/submit/fail：Run exited，Task记录 `NO_TASK_OUTCOME`并按retry policy requeue/failed。
- CLI输出“done/completed”不能改变结果。
- 非零退出不能提交/完成Task。
- Agent调用wait/submit/fail后，Task先finishing、Run保存requested outcome、token立即失效；Run仍live时不得提前waiting/submitted/failed。
- Submit Run terminal后由静止workspace capture成功，Task才submitted；exit处理不得重复转移。
- starting one-shot在active事务前快速退出时Run可直接exited，Task不被卡在queued+current Run。
- finishing/pending capture期间Boss accept/rework/cancel必须返回`ACTION_IN_PROGRESS`；`run stop`可以促使进程收敛，但不得丢失已保存outcome。

### CT-05 父子任务

Boundary：coordlink `task create/wait/submit/accept/rework`。
覆盖：INV-03、INV-05、INV-14。

- Agent显式创建指定 assignee的child Task，然后parent waiting。
- Child submitted产生绑定parent的system Message并唤醒parent。
- Boss可把child精确task ref导出checkout；parent/Reviewer可创建带source Task的work Task并在private workspace读取同一head，不需要control repo权限。
- Backend不自动接受child、不自动完成parent、不自动创建下一任务。
- Parent Agent accept/rework使用当前Run scope；重复操作幂等或稳定冲突。

### CT-06 Message at-least-once

Boundary：coordplane/coordlink Message入口 + notifier + SQLite。
覆盖：INV-05、INV-07。

- Message row/Event提交后才调用adapter。
- Inject unsupported/failed时保持pending。
- Resume Run输入包含全部pending Message ID；建立输入后delivered。
- Run在ack前terminal，Message回pending并产生redelivered Event。
- pending或delivered Message都可由接收者ack；Inject先发生、delivered事务稍后提交的竞态不导致ack失败。
- 同一Message可重复递送但只ack一次；两个不同Message不能被route级去重吞掉。
- 自动redelivery达到上限后保持可查询但停止auto wake；显式retry才能继续，不能无限创建Run。
- Delivery Task进入submitted/failed/completed/cancelled时，未处理Agent Message在同一事务转到recipient conversation/Boss或cancel，不等待不存在的下一Run且不永久阻止GC。
- 创建Message时若显式delivery Task已经submitted/failed/closed，必须在同一事务改投recipient conversation并保留related Task，或稳定拒绝；不得先插入一个永远无法递送的pending行。
- 首次direct Message显式wake=false时，conversation Task与Message原子创建且Task初始waiting；Scheduler不得启动Run，直到显式wake或后续wake Message。
- failed conversation不能作为direct Message复用/重路由目标；调用稳定失败或消息转Boss/cancel，不能留下pending。Child terminal遇Project error、closed parent或archived parent Agent时仍成功终结，并把结果Message交给Boss。
- acknowledged不自动改变Task状态。
- `task create/message send或reply/wait/submit/fail/accept/rework`携带`ack_message_ids`时，动作和ack同事务；在“动作后、响应前”kill client不会重复业务副作用。
- 在wait已写finishing、旧Run尚未terminal时创建wake Message：不得立刻创建第二Run或形成waiting+active；旧Run terminal事务恰好一次转queued，之后才创建新Run。wake=false在同一窗口不得自行queue。

### CT-07 Boss chat

Boundary：正式 `coordplane chat` 命令。
覆盖：INV-05、INV-14。

- 首次chat创建conversation Task，后续chat复用。
- conversation上的submit/accept/rework以及work/integration上的close返回`INVALID_STATE`且零副作用。
- Boss文本只产生Message，不直接创建work Task或改Git。
- Agent回复可见并ack，Agent wait后下一条Boss消息创建新Run；兼容时Resume，不兼容/unsupported时fresh Start，绝不复活旧Run。
- Daemon重启后conversation和消息顺序保持。

### CT-08 Accept 与撤销竞态

Boundary：正式`task accept/rework/cancel` +真实SQLite +可阻塞Git executor。
覆盖：INV-03、INV-10、INV-12。

- Accept获胜时必须原子写accepted字段和`pending_action=advance`；并发rework/cancel返回`ACTION_IN_PROGRESS`，不能撤销授权后仍推进canonical。
- Accept必须把显式/Project默认解析出的active integration Agent精确写入source Task；重启或Project默认变化后仍使用该ID。pending advance期间该Agent archive失败；之后pause只使新integration Task等待，不得静默改派。
- Rework/cancel先获胜时，accept返回`INVALID_STATE`且零Git side effect。
- Advance terminal事务再次匹配operation ID、pending version、target head和accepted授权。
- Stale创建integration Task后，source的rework/cancel/第二accept继续返回`ACTION_IN_PROGRESS`；integration capture和最终CAS都必须匹配source accepted version/link/head/ref。
- 显式cancel无pending action的integration Task必须原子释放source link/accepted授权；与integration submit/CAS并发时只能一方获胜。
- 相同request ID重放返回同一结果，不创建第二个integration Task或第二次CAS。

### CT-09 Project/Agent 生命周期

Boundary：正式project/agent命令 +真实SQLite。
覆盖：INV-04、INV-12、INV-14。

- Project creating/error/active/archived状态和Agent active/paused/archived转移符合Core FSM。
- Project error重启后保持fail-closed，不调度、不集成；只有`project repair`核验成功才能active。
- 有starting/active Run、Project/Task pending action或open Task时Project archive失败且零副作用。
- 有starting/active Run或open assigned Task时Agent archive失败；pause不静默kill当前Run。
- Agent被open source Task固定为accepted integration Agent时archive失败，直到source直接完成或linked integration完成/取消并释放授权。
- 没有accepted引用时Agent archive可原子清除Project默认integrator；后续accept必须选择另一个active Agent，不能复用archived默认值。
- archived Project/Agent拒绝新Task和Agent-directed Message且不产生孤立pending行；Project error中的错误结果仍能由Boss读取。

## 7. Runtime 场景

### RT-01 真实隔离

Boundary：真实Docker。
覆盖：INV-06。

同时启动Agent A/B，断言：

- container ID、workspace、`.git`、home和token文件不同。
- per-Run API socket/control目录不同，A不能连接B socket，也看不到operator socket。
- A不能读取/修改B workspace/home/token。
- 两者都看不到DB、Docker socket、control repo或Boss HOME。
- 容器使用非root、无额外capabilities、无published port。
- Agent修改自己private clone的`main`/refs不能改变actual canonical。

禁止用fake Docker替代。

### RT-02 Active truth

Boundary：真实Docker + one-shot scripted adapter。
覆盖：INV-03、INV-07。

- Workspace/home/coordlink/CLI未准备完时Run不是active、Task不是running。
- `prepareWorkspace`在任何Run到达`start_issued`前失败且workspace absent时，修复环境后的failed retry可按不可变base/source首次物化；若历史Run已到`start_issued`，外部删除workspace后retry必须fail loud且不得新建container/clone来掩盖可能丢失的Agent修改。
- `launch_phase`按intent/created/start_issued/process_observed单调持久化；Docker Start与active事务间崩溃能仅凭Run行和Docker事实判定failed或保守interrupted。
- Docker start前已attach日志或可从offset 0 replay；快速one-shot的session/exit事件不丢。
- Process启动已观察、Supervisor持有Wait且尚未观察terminal后才写active/running；启动窗口的coordlink自动重试`RUN_STARTING`。
- native session ID在process退出前写入。
- Process退出后Run立即terminal；即使session可resume也不能Inject。
- 新Message创建新Resume Run而不是复活旧Run。
- Daemon重启后使用durable RuntimeRef Attach同一container、重放日志并继续Wait，不依赖旧内存handle。
- Daemon重启在同一bind-mounted control目录重建per-Run API socket，既有coordlink经有界重试重新连接；错误Run token仍被scope fence拒绝。
- Adapter只构造provider Inject输入，真实process I/O由Executor执行；任一路径都不能绕过live RuntimeRef/nonce检查。

### RT-03 Resume fallback

Boundary：adapter conformance +真实Run状态。
覆盖：INV-05、INV-07。

- Resume success使用相同Task workspace/home、新Run/token，并引用旧Run。
- 静态unsupported/incompatible直接创建launch_mode=start的Run，不先启动Resume进程。
- session-not-found使Resume Run terminal并写`RESUME_UNAVAILABLE`；Message回pending，随后另一个generation更高的fresh Start Run包含durable Task/Message。
- fresh Start不宣称恢复原模型上下文。
- 不支持resume的adapter从一开始走Start，不进入无限retry。
- Resume只选择同Task/Agent最新兼容terminal Run，不把旧adapter session交给新adapter，也不静默跳回更旧历史。

### RT-04 Cancel 和 timeout

Boundary：正式Boss命令 +真实Docker长进程。
覆盖：INV-08、INV-13。

- Task cancel intent先持久化、generation前进并使旧token失效；Task不再自动调度。
- Graceful stop超时后kill完整容器/process tree。
- 已取消context不妨碍独立cleanup context remove容器。
- Run分别收敛cancelled/timed_out；Task不completed。
- Dirty workspace不被reset/clean/delete。
- `run stop`只终结本Run为interrupted；没有requested outcome时Task按retry policy恢复，不等同task cancel。
- `run stop`发生在wait/submit/fail outcome已经durable之后时，Run仍按该outcome完成finishing收尾，不能把它降级成普通interrupted并丢失结果。
- timeout发生在requested outcome之后时同样保留wait/submit/fail收尾；timeout先发生时迟到outcome被撤销token拒绝，Task只走runtime retry/failed。

### RT-05 Daemon crash/reconcile

Boundary：真实SQLite +真实Docker，进程级fault injection。
覆盖：INV-08、INV-12。

分别在以下点kill Daemon：

1. Run intent已写、container未create。
2. Container已create、Run尚未active。
3. Run active且CLI仍运行。
4. CLI已exit、DB尚未终结Run。
5. Run terminal、container尚未remove。

重启后必须使用Run ID/generation/launch nonce/launch phase收敛，不重复启动、不猜成功、不误删nonce不匹配容器。另覆盖create成功但container ID尚未落库、matching Docker state=created、container ID已落库但NotFound；依次证明按确定性name/nonce采用并落ID、只start一次、按phase终结旧Run且不为同一Run create第二个CLI。容器必须`AutoRemove=false/RestartPolicy=no`，terminal事实落库后才remove。

每个崩溃点还要分别带requested outcome和stop/cancel/timeout intent重跑：重启必须优先幂等Stop并继续对应outcome，不得仅因container仍running就恢复普通业务执行。

Docker API不可达场景必须进入degraded并停止新调度，不能把所有Run标lost或cleanup removed。Container NotFound但per-Run socket/token/control目录仍存在时cleanup保持pending/blocked；只有所有Run-owned runtime资源absent才removed。

SIGTERM测试必须证明Daemon先停止claim，grace到期后写`daemon_shutdown` stop intent并终结starting/active Run；已有outcome继续对应收尾，第一版不留下故意detach的无人监控container。无Run row的orphan container只能quarantine/manual，不能凭label猜nonce后删除。

## 8. Git 场景

### GT-00 Project 注册恢复

Boundary：正式`project add/repair` +真实本地Git +SQLite fault injection。
覆盖：INV-01、INV-12。

分别在creating行提交、临时bare导入、canonical ref创建、最终rename、Project active提交前kill Daemon。intent后移动source branch，重启仍必须使用Project行固定的initial SHA/operation ID确定性继续或转error；不得留下active但无repo、无Project行却被自动采用的repo，且source working tree/ref/config零修改。Project error重启后持续停止调度/集成。

### GT-01 Private clone

Boundary：真实临时bare repo +真实Docker workspace。
覆盖：INV-06、INV-09。

必须断言：

- clone使用精确base SHA。
- `.git`私有，无alternates、shared object/common git dir。
- control repo不在mount内，origin不可写且不泄露host path。
- A/B从同base创建不同workspace/branch，commit互不影响。
- `task checkout`导出的HEAD精确等于未集成Task.head_sha且无control remote；带source Task的review work clone通过受限bundle得到同一commit的convenience ref，篡改该ref不改变保存的source SHA。
- linked worktree实现不能通过此测试。

### GT-02 真实HEAD和clean gate

Boundary：正式 `coordlink task submit` +真实Git。
覆盖：INV-09。

- Agent上报错误expected head时，第一事务仍只durable写finishing/outcome；Run terminal后的helper发现mismatch并requeue/failed，且不得创建task ref、写head或导入可达control ref。
- Submit请求先使Task finishing、Run outcome/token冻结；Run仍live时不得capture或submitted。
- Run terminal、workspace无writer后，受信只读helper发现dirty/untracked或Git中间态时requeue/failed并保留workspace。
- Helper和controller bare都读取actual HEAD并校验base ancestor，关闭replace/grafts。
- 通过后创建不可变task/run ref，最终operation/version/generation fence通过后才把Task转submitted。
- 修改private branch名不能欺骗captured SHA。
- Agent篡改workspace source ref、`refs/replace/*`或grafts不能欺骗controller lineage。

### GT-03 Capture crash matrix

Boundary：真实Git bundle/import + SQLite fault injection。
覆盖：INV-09、INV-12。

分别在以下点kill Daemon：

1. Task finishing/pending capture已写，Run尚未terminal。
2. Run terminal，handoff未ready。
3. handoff ready，尚未import。
4. object已import，task ref尚未create。
5. task ref已create，DB head/status尚未写。
6. DB submitted已写，临时handoff尚未clean。

重启后必须幂等完成或保持可恢复错误；不得生成两个业务提交、移动错误ref、丢唯一commit或把partial文件当成功。

另需在task ref创建后模拟Task cancel/retry/new generation或pending action ID/version不匹配，断言旧ref可保留诊断但绝不能补写Task submitted。Corrupt、oversized和超对象数bundle必须fail loud、Project/Task按错误分类收敛且control repo仍`git fsck`通过。

### GT-04 Canonical CAS竞态

Boundary：真实bare repo +多个并发`git update-ref`调用。
覆盖：INV-10。

创建同一current的两个不同descendant head，同时请求accept：

- 恰好一个expected-old CAS直接成功。
- 另一个得到actual stale并保留task ref。
- canonical始终指向有效commit，赢家不被覆盖。
- 没有依赖进程内业务lock的正确性。
- `git fsck`通过。

额外覆盖：

- intended head已经是actual canonical祖先时直接确认already integrated，不创建integration Task。
- `update-ref`成功、DB未完成时kill Daemon，再让canonical前进到head后代；恢复必须识别已包含并补齐completed/final SHA。
- accept与rework/cancel竞态遵守CT-08，旧授权不能推进ref。

该测试必须并发重复并在race gate运行相关Go代码。

### GT-05 Integration Task

Boundary：真实Git + scripted integration CLI。
覆盖：INV-10、INV-11。

- Source Task先被Boss/parent显式accept。
- non-FF后只创建一个去重integration Task，Daemon不运行merge/rebase/cherry-pick。
- Source Task链接integration后保持accepted锁定；source version/link/head/ref任一不匹配都使integration capture/final CAS fail loud且canonical不变。
- Integration workspace基于actual canonical并包含source input ref。
- CLI生成同时以canonical/source head为祖先的clean head。
- Agent篡改private convenience source ref或replace ref时，Daemon仍以Task保存的source SHA在controller bare复核并拒绝伪lineage。
- Daemon捕获其task ref并fast-forward CAS。
- 成功后source和integration Task均completed，canonical包含两者lineage。
- Cancel integration时原子释放source授权；cancel与submit/CAS barrier竞态只能产生“已取消且canonical不变”或“已集成且双方completed”之一。

### GT-06 冲突和再次stale

Boundary：真实Git同行冲突 +可恢复integration Run。
覆盖：INV-11、INV-12。

- 两个head修改同一行时，integration CLI的普通Git产生真实冲突。
- canonical不变，source task ref和dirty/conflict workspace保留。
- 冲突通过progress/Message可见，不产生ConflictSet业务对象。
- CLI Agent解决、测试、commit后才能重新submit。
- 若期间canonical再次前进，CAS失败，integration Task rework/queued并收到new SHA；不得覆盖赢家。
- 再次stale必须requeue同一个integration Task，不创建嵌套integration；source accepted link在最终成功或显式cancel前保持。
- Rework Run启动前必须把new canonical object通过受限bundle导入private workspace；不得挂载control repo或只更新一个不可解析的SHA文本。

### GT-07 Git GC

Boundary：真实Git ref/reachability + workspace filesystem。
覆盖：INV-13。

- Active/waiting/recoverable/dirty workspace不删。
- 未完成Task.pending action或任何work/integration source引用的task ref不删；已有terminal配对的历史Event不永久阻止GC。
- 用barrier并发执行`task create --source-task`/`task checkout`与task-ref GC，断言共同per-Project维护锁使“新引用提交”和“expected-old删除+prune”只能串行；不能创建指向刚被删ref的Task。
- 未集成唯一commit在显式discard前不被prune。
- Cleanup与capture/resume并发时cleanup退出。
- 删除ref后再次检查引用，维护锁内`git gc`，最终`git fsck`通过。
- Config/public contract拒绝负duration和非法retention，接受`0`；使用可控时钟证明`age < retention`不删、`age == retention`开始eligible，且起点分别是Task `closed_at`和Run `ended_at`。
- 修改retention配置并重启Daemon后不改写历史时间；下一次`gc preview`/`gc run --confirm`对既有closed Task/terminal Run使用当前配置。`0`仍须满足全部clean/ref/pending/ownership fence，不能直接删除active或不确定资源。
- 在workspace删除和expected-old task-ref删除后分别注入崩溃；重启重算predicate并把actual absent视为幂等完成，不新增GC pending/operation业务对象，也不依赖terminal Event授权。
- `gc run --confirm`不能删除dirty workspace或未进canonical的唯一ref。两个discard命令缺Task、expected identity或request ID时拒绝；preview后workspace/ref变化使expected CAS失败且零副作用，重复同request或actual已absent幂等成功。
- 同一Task准备两个不同Run ref及两个指向同SHA的ref；`discard-task-ref`缺Run ID、Run不属于Task或expected SHA错误时零副作用，合法命令只删除指定Task+Run ref，其他ref保持。
- Failed Task的discard命令必须拒绝且零副作用；原地retry按RT-02复用workspace或只在从未`start_issued`时完成首次物化。Boss先cancel后才能discard，后续工作必须创建`retry_of_task_id`新Task；曾到`start_issued`的workspace意外absent时retry要fail loud，不能伪装成已授权重建。
- `task create --retry-of`只接受同Project completed/cancelled Task并保存lineage；open/cross-Project目标拒绝。新Task base来自当前actual canonical，除非Boss另给合法`--source-task`，不得从retry关系猜旧ref。
- 用合法integration取消流程产生两个不同`closed_at`：integration先cancel、source后cancel。在`integration.closed_at + retention`已到但`source.closed_at + retention`未到时ref仍保留，达到两者较晚边界后才eligible。

## 9. Deterministic 双 Agent E2E

该 gate使用真实Daemon、file-backed SQLite、真实Git、真实Docker和确定性的scripted CLI。它用于日常回归，不依赖模型随机性。

### 9.1 Fixture

- 初始化本地Git Project，canonical=`C0`。
- 配置Agent A、Agent B，并发度至少2。
- Scripted CLI必须通过正式coordlink入口操作，不能直接写DB/control repo。
- A/B分别修改不同fixture文件并commit。

### 9.2 流程

1. Boss创建两个work Task，明确指派A/B；两者base_sha都为C0。
2. Scheduler同时创建两个Docker Run。
3. 使用Docker live事实证明两个container/process存在重叠时间窗口。
4. A向B发送direct Message，delivery使用B的conversation Task、related Task使用A当前Task；scripted adapter可Inject到B当前work Run，失败时后续conversation Resume仍必须读取并ack。
5. A/B分别progress、运行fixture test、commit并调用task submit；两者先进入finishing并终结Run。
6. Daemon从静止actual workspace捕获两个不同task refs/heads，最终fence后两Task才submitted。
7. Boss先用`task checkout`读取并验证A的精确未集成head，再接受A，canonical以CAS从C0前进到CA。
8. 接受B时直接CAS失败为stale，只创建一个integration Task。
9. Integration CLI在新private workspace合并CA和B source head，运行测试并commit。
10. Daemon捕获integration head，以expected-old CAS推进canonical。
11. Boss从status/task/events看到两个结果、所有Run、Message和最终SHA。
12. 重启Daemon后再次查询；执行cleanup preview/run并检查保留条件。

### 9.3 必须断言

- 两个Agent真实并发，不是先后运行的时间戳模拟。
- Workspace/home/token/container完全不同且互不可读写。
- Message durable，ack前Run退出会再次递送。
- Run exit不替代Task submit/accept。
- 两个原始task ref在workspace清理后仍存在。
- 第一个CAS成功，第二个不覆盖第一个。
- Integration最终head来自integration Run的task ref；Daemon Git executor静态/运行记录只允许import、ancestry、fsck和update-ref，不包含merge/rebase/cherry-pick。
- 最终canonical包含C0、A head、B head的祖先lineage，fixture test和`git fsck`通过。
- 无remote push、PR、artifact或validation service参与。

## 10. 真实 CLI E2E

### 10.1 Adapter smoke

每个第一版production adapter至少通过一次真实Docker smoke：

- Start真实CLI并获得live process。
- 在退出前记录native session ID（provider支持时）。
- CLI通过coordlink读取当前Task、发progress/Message并wait。
- 新Message触发Resume新Run并复用同一Task workspace/Agent home。
- 取消能停止真实CLI容器。

### 10.2 两个真实 Agent并发闭环

产品完成前必须至少用一个production adapter的两个真实实例完成第9节等价闭环；不强制Codex和Claude混用。

要求：

- 两个真实CLI Agent在两个Docker容器中有可证明的live重叠。
- 两者从同一C0修改不同文件、使用coordlink通信、commit和submit。
- 一个直接CAS，另一个由真实integration CLI Agent处理stale并提交merge结果。
- 最终canonical、task refs、SQLite状态和Boss查询一致。

为降低模型随机性，同一行冲突解决放在GT-06确定性测试，不要求真实LLM gate每次制造并解决冲突。

真实CLI gate需要provider凭据或网络时可以在普通CI标记SKIP，但任何SKIP结果只能声明“自动测试通过，live未验收”，不能声明产品完成。

## 11. 第一版性能场景

### 11.1 PF-01：四 Agent 并发开发、收敛和恢复

第一版只设置一个正式性能场景`PF-01`。它测量CoordPlane控制面、file-SQLite、Docker和Git路径，不把provider网络排队或模型生成时间算成CoordPlane性能。

Boundary：正式`coordplane`/`coordlink`、真实Daemon、file-backed SQLite、真实Git、真实Docker和确定性scripted one-shot CLI。除模型provider外不得mock被测边界。Scripted CLI必须在真实容器中使用正式socket、token、private clone和原生Git。

PF-01只补充性能和soak证据，不替代第3至10节的最低真实边界测试。场景内任何重复Run、消息丢失、错误task ref/CAS、伪完成、Git损坏或cleanup不收敛都直接FAIL，耗时再短也不能抵消。

### 11.2 固定环境和fixture

正式阈值只适用于项目登记的原生Linux reference runner：

- 测试job使用cgroup/cpuset固定为8 logical CPU、16 GiB RAM和10 GiB磁盘额度；Daemon及其Git helper合计最多4 CPU/512 MiB，4个Agent container各为`1 CPU / 512 MiB / 256 PIDs`。
- 数据目录、Docker `overlay2`和临时目录位于同一本地ext4/xfs SSD/NVMe；不得使用Docker Desktop、网络盘或远程Docker daemon。
- 没有其他重负载job/container；报告记录runner ID、CPU型号、内存、filesystem/mount、kernel、Docker storage driver以及Docker/Git/Go版本。
- Agent image预先build/pull并固定digest；冷启动Daemon和image pull单列报告，不进入warm-cache发布分布。
- Agent不注入provider凭据，使用`network=none`；coordlink只走per-Run Unix socket，fixture测试不访问网络。
- Perf配置把`retention.completed_workspace`和`retention.run_log`设为`0`。每波只调用正式`gc preview`和`gc run --confirm`；所有clean、ref、pending action和ownership fence仍必须满足，不能由benchmark直接删除产品目录。

其他主机只能输出观察结果，不能声明reference性能PASS。环境额度、fixture/hash、image digest或storage driver不符时结果为`INVALID_ENVIRONMENT`；它不是PASS。

Git fixture由固定seed程序化生成：

- 2,048个确定性文件、32个线性commit，checkout约16-24 MiB、pack约24-40 MiB。
- 固定author/time、生成器hash、manifest和初始`C0`；体积或hash不匹配时场景`INVALID_ENVIRONMENT`。
- 每波四个work Task共享同一actual canonical base，分别只修改`bench/a.txt`至`bench/d.txt`，形成真实分叉但不制造文本冲突。
- 项目只使用标准库并运行固定、离线测试；测试退出0是scripted Agent提交前置，不是Daemon智能验收。

### 11.3 样本拓扑和正常负载

配置Agent A/B/C/D、Project默认integration Agent=A和`max_parallel_runs=4`。Scripted work模式用正式coordlink报告READY并等待GO；integration模式只使用Task固定source ref和原生Git。每个Run在收到GO前保持live，socket短暂不可用时以`50-500 ms`退避重连最多30秒，恢复前不请求outcome。

Release样本由4个相互独立的fresh data directory/Daemon批次组成；每批先做1个不计分warm-up，再做5个相关计分波次，共20个计分波次。第一批随后额外运行15波，连同其5个计分波次形成同一Daemon连续20波soak；额外15波不进入release延迟分位数。每波内容唯一，不能复用旧Task、workspace或commit。

每个并发波次固定为：

1. 从同一canonical SHA以相同priority和A/B/C/D固定顺序创建4个明确指派的work Task；该顺序也保证串行对照中D最后运行。
2. 等待4个Run均写出durable READY progress且由Docker事实证明process live；barrier保持1秒后，Harness通过正式Boss Message入口并发向四个Task各发送一条`GO`。GO正文使用固定benchmark schema，包含`wave_id`和4个work Task ID，因此A/B/C能机械取得D Task ID且D能区分GO和peer Message。这4条控制Message同样必须durable/ack，但不计入步骤4的200条peer Message延迟样本。该hold单列，不计`T_work4`。
3. 四个Agent各执行50次`coordlink progress`，每个请求使用唯一request ID，共200次并发mutation。
4. A/B/C分别向步骤1创建的D work Task发送`4/3/3`条Message。D每50 ms轮询inbox，在收到10条后逐ID ack；ack请求可因超时重试，但每个Message只能发生一次durable `acknowledged`状态转移和一次对应Event。
5. 四者修改各自文件、执行同一固定测试、commit并`task submit`；等待Run terminal、静止workspace capture和4个不可变task refs。
6. Boss先accept A，显式/默认固定integration Agent=A并直接fast-forward CAS。随后依次accept B/C/D，每次等上一个integration Task完成；每波必须恰好创建3个且没有嵌套integration Task。
7. Scripted integration Agent用原生Git合并固定source、运行同一测试、commit和submit；Daemon只做capture、ancestry和expected-old CAS。
8. 断言最终canonical包含四个source head，SQLite投影一致，并运行`git fsck`和项目测试。
9. 等待全部Run cleanup，先确认test observer已收齐client record，再执行`gc preview`和`gc run --confirm`；断言本波eligible workspace和Run log实际absent。下一波前不得有starting/active Run、owned container、blocked cleanup、未知control目录或未解释pending action。

串行对照使用相同fixture、消息、200次progress、Git修改、测试和submit负载，但配置`max_parallel_runs=1`；每个Run达到READY+live后Harness立即通过同一Boss Message入口发送该Task的GO，D最后消费已持久化的10条Message，不等待“四Run同时READY”。在2个fresh Daemon批次各做1个warm-up和5个计分波次，共10个样本。`T1`从第一条串行GO request write到第四个source submitted；`T4`从4条并发GO中最早的request write到第四个source submitted。取得T1后，Boss显式cancel四个submitted Task，等待cleanup并走正式GC，使下一波不积累submitted workspace；task ref仍服从Git保留合同。`median(T4)/median(T1)`只作为并发收益诊断，不作为发布Gate，因为它同时受磁盘、per-Project Git锁和Docker并发度影响。

`scripts/perf-v1.sh --profile smoke`可只跑1个warm-up、3个并发波次、1个串行对照和每类1个故障样本，用于开发反馈；它只能证明场景可执行。正式结论只能来自`--profile release`的完整样本。

### 11.4 故障恢复段

故障样本使用独立fresh data directory，与正常延迟分布分开报告，不能混入或删除：

1. 5次live crash：4个Run均READY且有未ack Message时SIGKILL Daemon，1秒后重启。Scripted进程按11.3节持续存活/重连；Daemon必须接管原4个Run/container、重建socket并在ready前完成runtime/Git reconciliation，不能创建第5个Run；消息最终ack且工作继续收敛。
2. 3次capture crash：test-only failpoint在task ref已创建、SQLite submitted事务前退出。重启只能依据Task pending action和actual ref补齐。
3. 3次CAS crash：test-only failpoint在`update-ref`成功、SQLite completed事务前退出。重启识别actual canonical已包含head，不重复CAS或创建integration Task。

Failpoint只存在于测试构建，不得形成production fallback。每个故障样本仍须完成最终lineage、`git fsck`、消息、cleanup和对象计数断言。

### 11.5 计时和统计口径

外部harness使用自己的单调时钟。Boss CLI计时从写请求前开始，到收到已durable响应或正式查询/Git/Docker事实结束；状态轮询间隔20 ms且开销计入结果。Event UTC时间只验证顺序，不用于延迟统计。所有公式固定写为`end - start`，任何负duration直接使报告INVALID，不能按0截断。

Scripted CLI对每个coordlink请求使用本进程单调时钟，并把`schema_version/record_type=client/request_id/task_id/run_id/operation/duration_ns/result`作为带固定前缀的单行JSON写stdout。Test-build observer在Runtime正常写Run log的同时，只校验并tee该固定schema到harness output；其他stdout不解析、不复制，production hook为no-op。Harness在GC前要求每个progress request恰好一个client record。Client record只计算同一进程内的`R_progress`，绝不在不同Agent或Harness时钟之间相减。

同一harness output除上述client tee外，Daemon test-build observer自产生三种普通JSONL record：

- `stage`：`schema_version/daemon_origin_id/sample_id/request_id/operation_id/attempt_index/project_id/task_id/run_id/stage_id/start_offset_ns/duration_ns/result`。
- `point`：`schema_version/daemon_origin_id/sample_id/request_id/operation_id/project_id/task_id/run_id/message_id/point_id/mono_offset_ns/result`。
- `resource`：每100 ms及stage边界输出`daemon_origin_id/sample_id/mono_offset_ns/rss_bytes/goroutines/open_fds`；RSS/FD与外部`/proc`交叉检查，goroutine来自`runtime.NumGoroutine`。

不适用ID明确为null。每个Daemon进程启动时生成只用于perf输出的唯一`daemon_origin_id`；Offset相对该进程单调起点，只允许同origin相减，跨重启只使用外部`T_recover`。Production build对应hook/sampler为no-op，不提供产品metrics端点；observer不得改变控制流、写SQLite/Event、记录Message正文/路径/secret或复制Git/Runtime实现。Daemon不识别wave/benchmark Task；Harness保存`task_id/run_id -> batch/wave`映射并用stage边界给resource sample分窗。正确性仍只来自正式状态和外部事实，observer只是耗时/资源证据。

Point的`mono_offset_ns`在`point_id`所指时刻捕获，不等于JSONL写出时间。`api.progress.received`在完整request frame读入后、auth/scope/排队前捕获并暂存；只有关联事务durable后才与`core.progress.committed`一起输出，失败请求另以result记录且不混入成功延迟。其他`*_commit` point在对应SQLite commit返回成功后立即捕获/输出。

Point ID固定为`api.progress.received`、`core.progress.committed`、`core.message.created_commit`、`core.message.acknowledged_commit`、`core.outcome.accepted_commit`和`git.capture.submitted_commit`。每个request/message/run只能产生合同允许的一组point；缺失、重复、跨`daemon_origin_id`或ID无法join时结果INVALID。

Stage边界固定如下；每次调用恰好一条terminal record，重试沿用业务operation ID并递增`attempt_index`，失败样本保留：

| Stage ID | start | end/聚合 |
| --- | --- | --- |
| `git.clone.lock_wait` | 请求per-Project维护锁前 | 取得锁；只诊断，不并入clone work |
| `git.clone.prepare` | 已取得锁、即将执行clone准备 | private clone完成，HEAD/base回读和ownership marker核验完成；不含lock wait/SQLite finalize |
| `runtime.container.create_start` | Docker Create请求前 | Start后Inspect证明process live且wait/log attach已安装；不含clone和active事务 |
| `git.capture.freeze` | outcome accepted commit后 | writer已停止、Run terminal且workspace静止 |
| `git.capture.handoff` | 开始生成bundle/pack | `.ready`原子发布并回读成功 |
| `git.capture.lock_wait` | 请求per-Project维护锁前 | 取得锁 |
| `git.capture.import` | 打开`.ready`前 | 对象导入且captured head可解析 |
| `git.capture.fsck` | controller bare完整性检查前 | fsck/commit类型/ancestry核验完成 |
| `git.capture.ref` | expected task-ref update前 | update和read-back等于captured head；不含submitted事务 |
| `git.advance.lock_wait` | 请求per-Project维护锁前 | 取得锁 |
| `git.advance.ancestry` | 解析actual canonical/task ref前 | current/head关系完成分类 |
| `git.advance.update_ref` | expected-old `update-ref`前 | update后read-back完成；不含completed/stale事务 |
| `runtime.cleanup` | Run terminal且cleanup intent可执行 | container/control资源全部absent或写blocked结果 |

每波clone work指标是4个source Task `git.clone.prepare` duration之和；等待由`git.clone.lock_wait`和外部`T_fanout4`单列。Docker逐Run统计；capture/advance子阶段分别统计，不把并发duration相加伪造成wall time。

必须分别报告：

```text
R_progress   = client durable response read - same-client request write
T_progress_burst = max(core.progress.committed) - min(api.progress.received)
T_message    = core.message.acknowledged_commit - core.message.created_commit
T_queue      = Run row first observable - eligible Task create request write
T_fanout4    = fourth process observed - last of four Task create request writes
T_capture    = git.capture.submitted_commit - core.outcome.accepted_commit
T_cas        = completed/stale first observable - task accept request write
T_work4      = fourth source Task submitted - earliest GO request write
T_integration = both source and integration completed first observable - integration Task first observable
T_integrations3 = D source completed first observable - B task accept request write
T_wave       = final canonical externally verified - first Task create request write
T_container_absent = container absent first observed - Run terminal first observable
T_cleanup    = all Run-owned resources absent - Run terminal first observable
T_recover    = first `daemon_ready=true` status response - replacement Daemon exec
```

Harness轮询正式`status --output json`的顶层`daemon_ready`。`true`沿用Core合同：Project ref、pending Git action、Run ownership、原live Run attach/wait/log和per-Run listener均已对账且mutation已开放；因此不另设可在ready之后完成的“reattach”宽限。Provider阶段只在真实CLI smoke中单列；真实Codex/Claude总耗时只设15分钟watchdog，不设性能SLA。

短操作合并20个计分波次的原始样本：progress共4,000个、peer Message共200个；每个Agent的50次progress串行发送、同一发送方的peer Message一次最多一个in-flight，四个Agent之间并发。每波吞吐使用同一Daemon origin的`200/T_progress_burst`。`status --output json`在soak达到至少70 Task/1,000 Event后，于最后5波四Run READY+live但尚未发GO时单线程连续采样200次，共1,000个；它测大账本+4 live Run查询，不声称与mutation burst重叠。该额外hold不进入20个release波次分布。所有分位数使用nearest-rank：升序样本的`ceil(p*N)`项，不插值。阶段/波次的20样本只报告p50/p90/max；故障恢复的11个样本逐个判定。失败和timeout作为FAIL保留，不能删除或当离群值。

Gate样本集合固定：`T_queue`取20个release波次的80个source work Task，`T_fanout4`每波一个共20个；Docker create/start取80个source work Run，60个integration Run另列诊断；`T_capture/T_container_absent/T_cleanup`取80个source和60个integration共140个Run；direct `T_cas`只取每波A的20次直接advance，stale检测和integration advance另列；`T_integration`取B/C/D的60个integration Task，`T_integrations3/T_wave`各取20个release波次。Integration当前状态可直接越过submitted，但`submitted_at + head_sha + task_ref`必须永久可查询，`T_capture`以submitted commit point为终点。

RSS/goroutine/FD每100 ms采样；harness每1秒并在handoff/import/capture/GC stage边界以固定`du -sb`等价口径采样fresh data directory。Idle窗口在fresh Daemon ready、无可运行Task且稳定30秒后开始，持续60秒；每批idle RSS取窗口median、idle CPU取窗口CPU time，四批均须通过。Load RSS取20个release波次全部样本的全局max；磁盘取每个fresh data directory的采样max且每个目录都须通过。Soak每波cleanup/GC后再稳定5秒取RSS/goroutine/FD中位数，以前5波和后5波中位数及20点最小二乘slope判定泄漏。

### 11.6 第一版冻结阈值

| 指标 | 第一版reference PASS条件 |
| --- | --- |
| 4,000次progress | 每波`200 / T_progress_burst`的median `>=100 ops/s`、min `>=50 ops/s`；`R_progress` p95 `<=100 ms`、p99 `<=250 ms`；零外显`SQLITE_BUSY`、丢失或重复副作用 |
| 200条Message | `T_message` p95 `<=1 s`、max `<=2 s`；每个ID恰好一次durable acknowledged transition/Event |
| 1,000次大账本+4 live Run status | p95 `<=200 ms`、p99 `<=500 ms`，输出始终可解析且投影一致 |
| `T_queue` | p90 `<=500 ms`、max `<=2 s`；只统计Agent和slot在Task创建时均可用的样本 |
| Git private clone work | 每波4个`git.clone.prepare` duration之和p90 `<=8 s`、max `<=12 s`；lock wait单列 |
| Docker create/start stage | 单Run p90 `<=3 s`、max `<=5 s`；`T_fanout4` p90 `<=12 s`、max `<=20 s` |
| `T_capture` | p90 `<=5 s`且max `<=10 s`；handoff/import/fsck/ref子阶段分别报告 |
| `T_cas` | direct ancestry/CAS p90 `<=1 s`、max `<=2 s`；lock wait和update-ref分别报告 |
| Integration | `T_integration` p90 `<=15 s`、max `<=30 s`；`T_integrations3` p90 `<=35 s`、max `<=60 s` |
| `T_wave` | p50 `<=60 s`、p90 `<=90 s`、max `<=180 s` |
| Runtime cleanup | `T_container_absent` p90 `<=5 s`；`T_cleanup` max `<=10 s` |
| `T_recover` | 5个live crash和6个pending action crash每次`<=8 s`；ready时已完成required reconciliation/reattach |
| Daemon内存 | idle RSS `<=128 MiB`；四Run load peak RSS `<=384 MiB`，不含Agent/containerd/Docker daemon |
| Daemon idle CPU | 60秒窗口CPU time `<=1.2 s`（单核2%），不得busy poll |
| 20波soak泄漏 | goroutine后5波相对前5波`<=16`、FD `<=8`、RSS `<=64 MiB`；RSS slope `<=2 MiB/波` |
| 峰值磁盘 | 每个fresh data directory相对创建后峰值`<=1.5 GiB`，无未知workspace/handoff/control目录 |

Reference绝对阈值是发布硬Gate，任何一项失败都不能被baseline抵消。另设独立同机回归Gate，只比较人工批准、不可自动滚动的固定baseline revision：它必须绑定runner/environment fingerprint、fixture manifest、image digest、实现revision和本节统计版本；失败/INVALID结果不能更新baseline。延迟/阶段的同名Gate统计值超过`max(1.25*baseline, baseline+5 ms)`，或progress吞吐低于其80%，回归Gate失败；RSS、CPU time、磁盘分别超过`max(1.25*baseline, baseline+8 MiB/0.1 s/64 MiB)`时失败。Goroutine/FD增量和RSS slope只使用绝对Gate，不做可能以0/负数为分母的相对比较。首个通过绝对Gate的owner批准结果冻结为baseline，该次记`BASELINE_BOOTSTRAP`；后续发布必须同时通过绝对和回归Gate。非reference主机只给warning/趋势，不建立“环境无关”结论。

性能报告写普通JSON/文本，至少包含环境指纹、fixture manifest、image digest、实现/baseline revision、每个原始样本、nearest-rank结果、阶段duration、对象计数、并发诊断比值和PASS/FAIL/INVALID_ENVIRONMENT原因；不为此建设产品内metrics、artifact或acceptance数据库。

## 12. 第一版代码行数预算

### 12.1 预算基线

预算以一个production one-shot adapter、一个scripted test adapter、Docker-only runtime、local-Git-only（每Project一个repo）和两个薄CLI为基线。产品仍可管理多个Project，PF-01只测一个Project。不包含TUI/Web、host/external runtime、remote Git、Inject必选实现、第二provider adapter或平台化预留。

SLOC是规划和架构漂移信号，不是质量分数。低于预算仍必须满足全部不变量；超过预算先检查重复边界和范围偏移，不能删除错误处理、recovery或测试来换取数字。

Owner已批准保留上述完整产品范围和本节非空、非纯注释物理行统计口径，并用以下envelope透明替换旧预算；本次重基线不构成范围删除、统计排除或测试豁免：

| bucket | 旧目标/软阈值/发布或审查阈值 | 批准的目标/软阈值/发布或审查阈值 | 逐项增量 |
| --- | ---: | ---: | ---: |
| Production | `10,500 / 12,550 / 14,650` | `18,500 / 19,500 / 20,500` | `+8,000 / +6,950 / +5,850` |
| Tests | `12,300 / 15,450 / 19,000` | `20,000 / 21,500 / 22,500` | `+7,700 / +6,050 / +3,500` |
| Build/test infra | `250 / 400 / 600` | `250 / 400 / 600` | `0 / 0 / 0` |
| Budgeted total | `23,050 / 28,400 / 34,250` | `38,750 / 41,400 / 43,600` | `+15,700 / +13,000 / +9,350` |

Production和Tests新增空间先以透明、未分配的重基线reserve列入下表，保留原模块/测试边界审查值以持续暴露重复和owner漂移。PF-01完整落地后按12.6节在同一revision报告实际模块分布；reserve不是忽略模块超限或弱化合同的授权。

### 12.2 Budgeted maintained production预算

| 所有边界 | 目标 | 软阈值 | 模块审查阈值 |
| --- | ---: | ---: | ---: |
| `cmd`：coordplane/coordlink参数解析和渲染 | 700 | 900 | 1,100 |
| `transport`：operator/per-Run socket、JSON、scope/token middleware | 650 | 850 | 1,000 |
| `core`：六对象、FSM、operations、scheduler/notifier/status | 2,300 | 2,700 | 3,100 |
| `store`：SQLite transaction/CAS/dedupe/migration | 1,500 | 1,800 | 2,100 |
| `runtime`：Docker、launch、supervisor、resume、stop/cleanup/reconcile/log | 2,200 | 2,500 | 2,900 |
| `git`：Project、private clone、capture、task ref、CAS、integration/GC | 1,900 | 2,250 | 2,600 |
| `adapter`：静态接口/registry和一个production adapter | 550 | 700 | 850 |
| `daemon/config/shared`：wiring、file lock、worker registry、config、clock/ID/error/redaction | 700 | 850 | 1,000 |
| Owner批准的未分配重基线reserve | 8,000 | 6,950 | 5,850 |
| **Production合计** | **18,500** | **19,500** | **20,500** |

模块超过软阈值可以在production总软阈值内小幅调剂，但必须在变更说明中列出净增量、所属不变量和删除/合并计划。任一模块超过模块审查阈值时必须复核owner边界和重复实现，不能靠把语义搬到其他目录消除命中；只要production总量仍`<=20,500`、12.6节同revision最终批准已完成且复核未发现范围漂移，模块命中本身不阻断发布。Production总量严格`>20,500`时阻断第一版发布，直到删除旧/重复路径，或owner先显式修改需求和预算。

第二个production adapter不在基线内。确需第一版加入时，规划增量为adapter production目标/软/审查`+450/+550/+650` SLOC、adapter tests`+500/+650/+800` SLOC；完整预算将变为Production `18,950/20,050/21,150`、Tests `20,500/22,150/23,300`、budgeted总计`39,700/42,600/45,050`。必须先把完整表和README改成新基线，再实现并说明为什么一个adapter不能完成产品验收；只有更新后的完整表可作为`loc-budget`输入。

### 12.3 测试和基础设施预算

| 测试边界 | 目标 | 软阈值 | 重复性审查阈值 |
| --- | ---: | ---: | ---: |
| Static guard + pure/FSM | 1,000 | 1,250 | 1,500 |
| Core/store/public CLI contract | 3,500 | 4,200 | 5,000 |
| Adapter conformance | 800 | 1,000 | 1,200 |
| 真实Git component/fault matrix | 2,500 | 3,200 | 4,000 |
| 真实Docker runtime/fault matrix | 2,200 | 2,800 | 3,500 |
| Deterministic + real CLI E2E harness | 1,500 | 1,900 | 2,300 |
| PF-01 harness、fixture generator、phase observer和报告 | 800 | 1,100 | 1,500 |
| Owner批准的未分配重基线reserve | 7,700 | 6,050 | 3,500 |
| **Tests合计** | **20,000** | **21,500** | **22,500** |

测试超过审查阈值只触发重复fixture/helper和边界重叠审查，不得仅因LOC删除测试。测试能否删除只由对应不变量已删除或已由更低、更真实边界完整替代决定。

PF-01必须复用既有public CLI、Docker/Git fixture helper、静态stage wrapper和状态断言；它只新增负载编排、采样和报告，不得复制Core/Runtime/Git实现到benchmark harness。

Budgeted build/test infrastructure（shell、Dockerfile、Makefile、手写YAML及语义型generated输入/输出）目标250、软阈值400、审查阈值600 SLOC。Budgeted maintained surface目标/软/审查为`38,750/41,400/43,600`；该总数包含语义型generated SLOC，不含机械generated输出和静态fixture，不能覆盖production超限，也不是减少测试的理由。

### 12.4 统一统计口径

实现仓库必须提供一个固定`scripts/loc-budget.sh --output json`，按gofmt后“非空、非纯注释物理行”统计maintained SLOC，并同时输出每模块和本次Git diff：

- 所有第一方、非generated生产源都按owner边界1:1计入production，包括`.go`、migration/queries SQL、`.proto`/OpenAPI、状态或命令template、embedded JSON/YAML规则和runtime shell；不能把语义搬到非Go文件逃预算。Owner path manifest未识别的第一方语义源默认计入`first_party_source_total`并使`--check`失败，直到明确归入production/test/infra，不能默认排除。
- `*_test.go`、test helper和test-only生成输入一律计tests；只在test/perf build tag编译且可由production build证明不可达的普通Go源（phase observer、failpoint、scripted adapter等）也计`budgeted_tests`，production binary内保留的no-op hook/wiring计`budgeted_production`。不能把production实现藏进helper或build tag。Migration SQL计入store，建议包含在store内的目标/软/审查子预算为250/400/550。
- Generated输出只有同时具备标准`Code generated ... DO NOT EDIT.`标记、`generated-manifest.json`中的output allowlist、owner、mechanical kind、generator版本/命令、全部输入和hash，并且CI从clean tree重新生成后`git diff --exit-code`为空，才不计主预算；无法机械复现、未登记或path未allowlist时默认按handwritten计入。
- Generated SLOC必须单列；1,500触发warning，超过3,000触发审查。Generator、schema、template和其他生成输入本身1:1计入所属production或test。
- Allowlist只接受机械展开的wire/DB binding/mock等输出。包含FSM、SQL业务决策、权限、recovery或provider特判的generated控制流即使可复现仍按1:1回算owner模块；该语义分类由manifest和静态审查锁定，`loc-budget.sh`不得假装能从文本可靠推断。
- Vendor、第三方源码、`go.sum`、文档和纯静态protocol/golden数据不计SLOC；禁止提交vendor隐藏规模。
- Fixture静态数据按体积治理：256 KiB warning、超过1 MiB审查。可执行fixture、generator、embedded shell/SQL按test/infra或所属源文件计算；Git repo必须测试时程序化生成，不提交`.git`或大二进制。

LOC JSON先输出互斥原子桶`handwritten_production/tests/infra`、`generated_semantic_production/tests/infra`和`generated_mechanical_excluded`，再按以下固定公式输出派生值：

```text
budgeted_production = handwritten_production + generated_semantic_production
budgeted_tests      = handwritten_tests + generated_semantic_tests
budgeted_infra      = handwritten_infra + generated_semantic_infra
budgeted_total      = budgeted_production + budgeted_tests + budgeted_infra
generated_total     = generated_semantic_production + generated_semantic_tests
                    + generated_semantic_infra + generated_mechanical_excluded
first_party_source_total = 所有上述源码物理行集合去重后的总数
```

三张预算表和所有Gate只使用`budgeted_*`；`generated_total`是有意重叠的可见性指标，不再参与`budgeted_total`加总。JSON另输出`fixture_bytes`，避免用预算排除项低报实际维护体积。所有阈值统一按严格`>`触发，等于阈值仍在该档内。

### 12.5 超预算治理

超预算时按以下顺序收敛：

1. 删除已被替代的旧路径、重复DTO/renderer/store wrapper和第二套CLI/transport逻辑。
2. 删除第一版非目标或可选项：Inject实现、第二adapter、TUI/fancy formatter、host runtime、remote/平台化预留。
3. 将真正新增的产品能力移出第一版，先修改需求再实现。
4. 只有无法通过上述方式收敛时，由owner基于实际模块报告调整预算；调整必须同时更新本节和baseline，不能在代码中加ignore名单。

无论是否超预算，以下内容不得因LOC删减：version/generation/token/nonce fence、pending action/operation ID、durable Message ack/redelivery、finishing两阶段submit、actual HEAD capture、expected-old CAS、Runtime attach/stop/cleanup/reconcile、Project fail-closed及对应SQLite/Git/Docker crash/race测试。

禁止通过一行多个语句、超长函数、大switch、删除有价值错误上下文或把逻辑搬进脚本/generated/test helper降LOC。Production单文件500/800 SLOC、单函数80/140 SLOC分别触发warning/阻断审查；拆分仍必须按静态注册和owner边界，不得制造无意义薄文件。

### 12.6 PF-01落地后的同revision最终批准

上述envelope已获准用于继续实现和审查，但第一版完成仍要求owner对PF-01落地后的精确revision作最终批准：

1. PF-01 client/point/stage/resource observer、5+3+3 fault raw table、smoke/release profile、环境preflight、全部阈值和fail-closed报告完成后，冻结一个clean implementation revision `R`。
2. 在不修改源码、测试、脚本、manifest或需求的情况下，对同一`R`运行`scripts/perf-v1.sh --profile release --output perf-v1.json`和`scripts/loc-budget.sh --check --output loc-budget.json`。两份JSON必须记录并匹配`R`；worktree dirty、revision缺失/不一致或任一报告来自其他revision时结果无效。
3. LOC最终报告必须保留全部原子桶、`budgeted_production/tests/infra/total`、每模块实际值、generated/fixture可见性、Git diff以及unknown path、file/function和gofmt质量blocker；不得只报告四个合计数。PF-01最终报告必须保留环境/fixture/image/baseline指纹、全部raw样本、nearest-rank、11个故障行、对象计数、资源/cleanup事实和PASS/FAIL/INVALID原因。
4. 只有PF-01 reference绝对Gate通过、LOC四个发布值分别`<=20,500/22,500/600/43,600`、质量blocker清零且owner明确记录对revision `R`和首个`BASELINE_BOOTSTRAP`的批准，预算Gate才完成。报告生成后任何tracked或untracked maintained source变化都必须生成新revision并重跑两份报告；失败或INVALID报告不能获批或更新baseline。
5. 最终批准只确认同一revision的实际分布，没有改变12.4统计口径或12.5禁止规避条款。若要扩大envelope、排除源码或删减产品/测试合同，必须在实现前再次显式修改五份need；不得在脚本、manifest或批准评论中暗改。

## 13. 验证命令和Gate

实现仓库必须提供等价命令；推荐：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go test -tags=docker ./... -count=1
scripts/e2e-deterministic.sh
scripts/perf-v1.sh --profile release --output perf-v1.json
scripts/loc-budget.sh --check --output loc-budget.json
scripts/e2e-real-cli.sh
```

Gate顺序：

1. Static guard。
2. Pure/adapter/public contract。
3. Storage和Git component。
4. Full Go和race。
5. Docker integration。
6. Deterministic双Agent E2E。
7. 同一clean revision的PF-01性能、LOC预算gate和12.6节owner最终批准。
8. Real CLI E2E。

E2E失败后不得直接反复改E2E脚本或prompt。必须先分类到Core、Runtime、Git、环境/provider或Task spec，并增加最低层可复现测试。

所有外部gate输出明确的 `PASS`、`FAIL` 或 `SKIP(reason)`。脚本exit 0但内部SKIP不能伪装PASS。

## 14. 完成判定

只有全部满足才能声明第一版完成：

- Static guard确认旧对象、旧入口和平台化服务没有生产实现。
- Core/Runtime/Git全部命名不变量有最低真实边界测试。
- 全量Go、race和vet通过。
- 真实SQLite migration/restart通过。
- 真实Git capture/CAS/crash/GC测试通过且`git fsck`成功。
- 真实Docker隔离、取消、resume、reconcile和cleanup通过。
- Deterministic双Agent E2E通过。
- PF-01 reference性能场景通过，原始样本和环境信息完整；没有用provider时间或删除失败样本美化结果；PF-01和LOC报告来自同一clean revision。
- `budgeted_production/tests/infra/total <= 20,500/22,500/600/43,600`，质量blocker清零，且owner已按12.6节最终批准该revision和首个固定baseline；测试未因LOC被弱化。
- 两个真实CLI Agent E2E通过且未SKIP。
- 验收结束后没有starting/active Run、owned orphan container、未解释capture/CAS intent或误删的task ref。
- Boss能从正式命令读取最终Task、Run、Message、base/head和canonical SHA。
- Boss和Reviewer能从正式入口实际读取、检出和测试精确未集成task head，而不获得control repo写权限。
- 验收没有依赖remote Git平台、动态registry、TeamConfig、validation engine、artifact平台或per-tool-call审计。

若真实CLI或Docker gate未运行，报告必须明确剩余runtime风险；不得用更多mock测试替代。
