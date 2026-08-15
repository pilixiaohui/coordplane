# CoordPlane 轻量需求基线

状态：Draft for owner review
版本：0.2
日期：2026-08-15
（本版并入统一参与者框架修订：v1 正式纳入 participant、可配置 role、项目级权限绑定与人类凭据生命周期，见 core.md/acceptance.md 相应条款）

## 1. 文档权威

本目录定义 CoordPlane 第一版的完整产品需求。经项目 owner 确认后，它应整体替换旧的范围问卷、模块设计和产品化验收合同，不能与旧模型并列作为实现依据。

本目录只有五份权威文档：

1. `README.md`：产品范围、职责边界、术语和非目标。
2. `core.md`：持久对象、状态机、Boss/Agent 入口、调度和通信。
3. `runtime.md`：Docker 隔离、CLI adapter、resume、取消、恢复和清理。
4. `git.md`：Git 代码真相、私有 workspace、task ref 和集成。
5. `acceptance.md`：架构约束、合同测试、真实拓扑和产品闭环验收。

对象和状态只能在 `core.md` 定义；运行事实只能在 `runtime.md` 补充；Git 事实只能在 `git.md` 补充；其他文档不得复制或另起同义模型。文档间出现冲突属于需求缺陷，实施者必须停止并要求修订，不能自行选择一个版本实现。

本文使用以下规范词：

- **必须**：第一版完成条件，不得省略。
- **应当**：默认实现要求，只有记录明确理由后才能偏离。
- **可以**：不影响第一版完成的实现选择。
- **不得**：会破坏产品边界或真相源的禁止项。

## 2. 一句话定位

CoordPlane 是一个个人使用、local-first 的常驻后台服务：老板通过它与一组 CLI Agent 对话和派发任务，多个 Agent 在相互隔离的环境中并发开发同一个 Git 项目，CoordPlane 负责记录、调度、通信、唤醒和机械核验，所有需要理解、规划、审查或解决冲突的工作都交给 CLI Agent。

CoordPlane 的价值不是“启动多个进程”，而是以下闭环在并发、失败和重启后仍成立：

```text
Boss
  -> 对话或显式创建 Task
  -> Daemon 为目标 Agent 准备隔离 Run
  -> CLI Agent 使用原生工具开发并通过 Message 协作
  -> Daemon 捕获真实 Git HEAD 为 task ref
  -> Boss或父/Reviewer Agent审查并接受
  -> Daemon 以 expected-old-SHA CAS 推进 canonical ref
  -> stale 或 conflict 变成新的 integration Task
  -> 进度、结果和下一步回到 Boss 或原 Agent
```

## 3. 两类真相源

系统只有两类权威真相：

| 事实 | 权威来源 | CoordPlane 数据库保存什么 |
| --- | --- | --- |
| 项目目标、任务责任、运行、消息、角色权限、身份凭据和关键状态变化 | CoordPlane SQLite | `Project`、`Participant`、`Task`、`Run`、`Message`、`Event`、`Role`、`ParticipantProjectRole`、`Credential` |
| 文件内容、历史、提交和分支 | Git objects 和 refs | 精确 `base_sha`、`head_sha`、canonical ref 和 task ref 的索引 |

约束：

- SQLite 不保存代码快照、patch 或自建 ChangeSet 作为第二份代码真相。
- Git 不保存任务状态、消息已读状态或运行状态。
- Prompt、CLI transcript、容器文件、PID 文件和内存队列都不是持久真相。
- 数据库缓存的 SHA 与 Git ref 不一致时，以实际 Git ref 为准并进入显式 reconciliation；不得用数据库值静默覆盖 Git。
- CLI Agent 的“已经完成”文字只是消息。只有结构化 Task 转移和实际 Git ref 能改变系统状态。

## 4. 产品参与者和组件

| 名称 | 类型 | 职责 |
| --- | --- | --- |
| Boss | 默认人类 owner participant 的显示别名 | 对话、派发、查看进度、接受、返工、取消、唤醒和最终控制 |
| Participant | 统一参与者身份行（kind：`human` 或 `cli_agent`） | 人类与 CLI Agent 共享同一身份、Task/Message 与权限框架，生命周期按 kind 分支；在项目内能做什么由该项目下的角色绑定决定 |
| Daemon | 常驻进程，不建业务对象 | SQLite、调度、Run supervisor、消息递送、Git ref 管理和恢复 |
| Operator CLI | `coordplane` 命令 | 人类参与者的正式入口；经凭据认证后按项目角色限权执行 |
| coordlink | Agent 环境内的薄客户端 | 只暴露固定 Task、Message、progress 操作并转发给 Daemon |
| Agent | `cli_agent` 类型的 participant | 绑定一个静态注册的 CLI adapter（claude/codex）、运行配置（model/subagent_model/base_url/effort）和提示词（instructions_file/text 二选一） |
| CLI adapter | 静态注册组件 | 生成 provider 启动/恢复命令、解析协议、判断 resume 兼容性；可选支持运行中输入，容器生命周期仍由 Runtime executor 统一拥有 |
| Role | 可配置数据（`roles` 表） | capability 集合；只在项目绑定（`participant_project_role`）内生效，全局能力经 `project_id=global` 作用域 |
| Capability | 静态注册权限点 | 每个可调用 operation 一个权限点；代码事实，不是数据，不提供动态 registry |
| Credential | 人类身份凭据（`credentials` 表） | v1 只签发 `operator_token`，保存 SHA-256 hash；支持轮换与吊销，吊销后该 participant 的 operator 操作立即被拒 |
| Runtime | 第一版为 Docker | 为每次 Run 提供隔离进程、workspace 和 Agent 私有 home |
| Git | 成熟代码协作机制 | commit、branch、merge、冲突检测、对象保存和 ref CAS |

Manager、Developer、Reviewer、Integrator 只是 Agent 指令或显示标签，不是内置角色。内置种子角色只有 owner 与 agent 两个（`role-owner`/`role-agent`），由 v3 迁移 seed 写入，是权限集合而非业务语义；Daemon 不理解角色名，也不根据角色名写业务分支。

## 5. 第一版必须完成的能力

### 5.1 Boss 控制面

- 以一个 CLI 入口注册项目和 Agent。
- 创建和编辑 Agent 配置：静态 adapter 列表（`GET /v1/adapters`）、模型/effort/base_url、提示词（file/text 互斥）；API PUT、operator CLI、前端编辑三面同一字段模型。
- 与指定 Agent 进行持久对话；Daemon 只保存和递送文本，不解释自然语言。
- 显式创建、查看、检出未集成结果、接受、返工、重试、取消和唤醒 Task。
- 查看每个 Agent 当前 Task、Run、最近进度、未读消息、错误和 Git base/head。
- 实时跟随事件和 stdout/stderr 日志。
- 停止失控 Run，并在 Daemon 重启后继续看到真实状态。

### 5.2 Agent 团队协作

- Agent 可查看当前 Task、创建明确指派的子 Task、等待子任务、提交结果和请求返工。
- Agent 可向另一个 Agent 或 Boss 发送绑定到某个 Task 的持久 Message。
- Message 可唤醒 idle Agent；支持时可注入当前活动 Run，不支持或失败时必须通过 resume/new Run 递送。
- Agent 可报告简短进度；进度只形成 Event，不形成独立业务对象。
- Run 退出不能自动把 Task 标记为完成。

### 5.3 并发和隔离

- 至少两个不同 Agent 可同时运行。
- 每个 work/integration Task 拥有私有 Git workspace；每个 Run 使用独立容器。
- 不同 Agent 不共享可写 home、workspace 或 Git metadata。
- 普通 Agent 看不到 Daemon 数据库、Docker socket、其他 workspace 或 canonical repo 的可写 ref。
- 每个 Agent 第一版同时最多一个非终态 Run；全局并发数由简单配置限制。

### 5.4 Git 协作

- 每个代码 Task 固定精确 `base_sha`。
- Agent 在私有 workspace 中直接使用原生 Git 命令并创建 commit。
- Daemon 从实际 workspace HEAD 捕获提交到 daemon-owned task ref，不能信任文字上报或分支名。
- Boss 可把精确 task ref 导出为无 control remote 的普通 checkout；Agent Reviewer 可通过显式 source Task 获得同一固定输入提交，本地 convenience ref即使可移动也不改变保存的source SHA。
- canonical ref 只能由 Daemon 使用 expected-old SHA 进行 CAS 更新。
- stale、非 fast-forward 或冲突交给 integration CLI Agent；Daemon 不合并文件、不判断语义、不解决冲突。
- workspace 删除前，所有需要保留的提交必须已由 Git ref 引用。

### 5.5 失败恢复

- SQLite、Git refs、Run 容器和 CLI 原生 session ID 必须可以在 Daemon 重启后对账。
- 消息先持久化再递送，进程或 adapter 失败不能丢消息。
- 旧 Run 的 token/generation 不能覆盖新 Run 的状态。
- 取消、超时和terminal Run必须最终停止并移除容器、per-Run socket/token等运行资源；清理失败保持可诊断和可重试。
- DB 与 Git 跨系统操作必须使用 durable intent、确定性引用和 reconciliation 收敛，不能声明假成功。

## 6. 明确非目标

第一版明确不建设以下能力，需求、代码、表结构和验收不得为其预留平台化框架：

- 多用户账号、组织、tenant、完整 RBAC 或通用 policy engine（capability 静态注册、role 可配置与项目级绑定是产品内权限实现，不是通用 RBAC/policy 平台）。
- 动态 capability registry、机器 schema discovery、通用 `/call` 路由或由 schema 派生工具。
- Skill registry、skill version store、skill binding 或 progressive disclosure 服务。
- TeamConfig DSL、角色策略语言、通信策略语言、终止验收策略或配置版本服务。
- 通用 tool adapter、MCP server、plugin 平台或多种机器协议；第一版只有 `coordplane`、固定 `coordlink` 和 CLI adapter。
- universal same-turn steering、强行中断模型 turn 或 Safe Boundary 子系统。
- 独立 validation/assessment/acceptance engine、release acceptance 数据库或项目业务判定。
- Artifact/ObjectStore 平台；代码由 Git 保存，日志由文件保存，其他输出由 Task/Message 引用普通路径或外部 URL。
- 对 CLI 每个 tool call 的强制完整审计、规范化行为流或永久 transcript 保存。
- WorkContract、Assignment、Lease、Attempt、Envelope、Thread、MailboxItem、DeliveryAttempt、Evidence 等重叠业务对象。
- ChangeSet、GitOperation、MergeAttempt、ConflictSet、RollbackPoint、durable Git lock 等自建 Git 平台对象。
- 代理 `git status/diff/add/commit/rebase/merge` 的通用 Git API；Agent 在私有 workspace 直接使用 Git。
- 自建 Git hosting、PR 网站、CI 平台、发布平台、远端 push 自动化或跨仓库原子事务。
- 分布式队列、远程 runner 集群、Kubernetes、HA、多 Daemon 并行写同一 data directory。
- 复杂 Dashboard、Autopilot、定时任务、模型成本平台、长期语义记忆或向量数据库；Web UI 只作为轻量 Agent 配置/查看面（有独立前端预算），不做平台化。
- CoordPlane 内置 LLM、阅读自然语言后自动拆任务、自动选择角色、自动验收代码或自动解决冲突。
- 内部 agent↔agent 点对点直连（A2A 或其他任何直连协议）；多 Agent 协作只经 Daemon 的 Task/Message 机制。
- AGNTCY/ANP 等跨组织发现/身份/传输基础设施；第一版是单 Daemon、本地优先，不需要 agent 目录/去中心化身份/加密传输网络。

未来若增加上述能力，必须先修改本需求基线，并作为可删除的独立适配层加入；不得在第一版主流程中预埋半成品接口。

### 6.1 行业标准协议定位（演进合同）

本小节固化第一版边界内对行业标准协议的定位，防止实现漂移。详细设计见 `docs/protocols.md`；对象/状态/Run 事实仍只由 `core.md`/`runtime.md` 定义，本小节不新增任何持久对象或状态。

1. **ACP（Agent Client Protocol，协调者 ↔ CLI agent）是 adapter 层的演进目标，不是第一版合同**。第一版 adapter 仍是静态注册的 provider 私有协议（Claude 与 Codex CLI）；当 ACP 达到 1.0 稳定且目标 agent（Claude Code）提供原生或成熟 bridge 支持后，应当实现 ACP client adapter 替换私有事件解析。`runtime.md` §7.1 的 adapter 接口（BuildStartCommand/BuildResumeCommand/BuildInjectInput/ParseEvent/ResumeCompatible）与 ACP 方法（session/new、session/prompt、session/cancel、session/update、session/request_permission）的映射见 `docs/protocols.md`。**第一版不得**预埋 ACP 半成品接口或按协议名写主循环特判；adapter 注册列表保持静态。
2. **A2A（Agent2Agent，agent↔agent）只作为未来对外互操作出口**。Boss 面未来可把 Project/Task 暴露为 A2A 端点 + 静态 AgentCard（能力/技能/安全声明，`/.well-known/agent-card.json`），用于与其他 agent 平台互派任务；但**内部多 Agent 协作必须保持 Daemon 单一协调，禁止 agent↔agent 点对点直连**（见 §6 非目标）。A2A 出口属于未来能力，必须先修改本需求基线。
3. **AG-UI（agent↔UI）作为 Web 前端事件词汇参考，不是传输合同**。实时展示 Run 进度/工具调用时可参考 AG-UI 事件词汇（RunStart/TextMessage/ToolCall/RunComplete 等）；事件传输与增量格式由前端实现选择，不与 AG-UI 规范固定传输绑定。
4. **MCP（Model Context Protocol）不建设平台**。见 §6 非目标（通用 tool adapter、MCP server、plugin 平台）；agent 在容器内自行连接 MCP server 属 agent 自身行为，CoordPlane 不提供平台化支持。
5. **AGNTCY/ANP 等发现/身份/传输基础设施不进第一版**。单 Daemon、本地优先定位不匹配；仅当未来演进为多 Daemon 或对外可发现 agent 时再评估。

## 7. 设计原则

### 7.1 CoordPlane 没有智力职责

任何需要理解内容的动作都由 Boss 或 CLI Agent显式发起。Daemon 只做以下机械判断：

- 状态转移是否合法。
- 调用者是否与当前 Run scope 一致，并持有操作所属项目/global 作用域下的 capability。
- Git commit/ref/祖先关系是否真实。
- expected version 或 expected SHA 是否仍匹配。
- 进程、容器和 session 是否真实存在。

### 7.2 Build to Delete

- Worker、CLI adapter、Task kind handler、检查和清理步骤必须由独立函数通过静态列表注册。
- 主循环不得按 adapter 名、Agent 角色或项目类型堆积 `if/else` 链。
- 删除一种 adapter、检查或步骤时，应只删除一个注册项和所属实现。
- 新机制替代旧机制时，调用方迁移和旧代码删除必须在同一变更中完成。

静态列表注册是代码可维护性要求，不等于动态 capability registry。

### 7.3 Fail loud

- 任何持久化失败、Git CAS 失败、scope 不匹配或运行事实不明都必须返回非零错误并写入可查询状态。
- 不允许把失败转换为空成功，不允许用 transcript 或 Agent 自报补齐缺失事实。
- 可重试错误保持原对象和稳定错误原因；不可恢复错误进入 `failed` 或等待 Boss 决策。

### 7.4 最小持久化

- 只保存恢复和协作需要的事实。
- `Event` 只记录关键状态变化，不记录每个 shell/tool call。
- stdout/stderr 使用可轮转文件，不进入 SQLite blob。
- Task/Message 表本身承担待调度和待递送语义，不再建设通用 QueueItem 平台。

### 7.5 第一版代码预算

代码预算是范围漂移告警，不是用短代码替代正确性的目标：

- 第一版以两个production one-shot CLI adapter（Claude + Codex）为基线；scripted adapter只属于测试。新增第三个provider adapter必须单独增加预算，不能挤占Core、Runtime或Git恢复逻辑。
- Owner已批准 Agent 可配置 CLI/模型/提示词 v1（D1–D7/E1–E5，2026-08-13）对应的重基线：Budgeted maintained production SLOC目标/软阈值/发布阈值为`24,000 / 24,500 / 25,000`；tests为`25,500 / 26,200 / 27,000`；build/test infrastructure为`250 / 500 / 700`；三类合计为`49,750 / 51,200 / 52,700`（E1 暂定上限，最终以 clean revision 实测锁表）。总量合格不能覆盖production超限。
- 前端(web 面 SPA 与 web e2e)单独配置预算,不与后端核心功能共享:`handwritten_frontend` 目标/软阈值/发布阈值为 `2,000 / 2,500 / 3,000` 物理行(非空非纯注释口径,JS/CSS 沿用同一计数器);前端 Go 服务层(`internal/webserver/*`)亦计入 frontend,不计入 production。后端 production/tests/infra/total 预算不受前端影响。
- 上述envelope替换统一参与者框架重基线的`20,000 / 21,000 / 22,600`、`21,000 / 22,000 / 23,700`和总计`41,250 / 43,400 / 47,000`，只改变预算，不删除或降级任何Core、Runtime、Git、CLI、adapter或真实多Agent边界合同。
- 第一版候选必须在clean revision生成LOC JSON，并完成`acceptance.md`定义的真实双Agent和四Agent可靠性场景。固定硬件性能baseline、reference manifest和长时间soak不属于第一版完成条件；真实live证据能否复用按受影响文件的精确diff判断，不能因纯文档变更重复消耗provider调用。
- LOC低于预算不代表完成；所有状态、隔离、recovery、Git CAS和真实Docker/Git验收仍必须通过。
- 不允许通过压缩语句、超长函数、把逻辑搬进generated code/脚本/test helper或保留第二套隐藏路径规避预算。

详细模块预算、统计口径和治理规则见`acceptance.md`。

## 8. 最小配置

第一版只需要一份普通静态配置，不是 DSL：

```yaml
data_dir: /path/to/coordplane-data
operator_socket: /path/to/coordplane-data/operator.sock
max_parallel_runs: 4

retention:
  completed_workspace: 24h
  terminal_task_ref: 168h
  run_log: 168h

runtime:
  docker_network: coordplane
  run_timeout: 0
  shutdown_grace: 5s
  workspace_root: /path/to/coordplane-data/workspaces
  agent_home_root: /path/to/coordplane-data/agent-homes
  log_root: /path/to/coordplane-data/logs
  default_image: coordplane-agent:latest
  provider_env_allowlist:
    - ANTHROPIC_AUTH_TOKEN
    - ANTHROPIC_BASE_URL
    - ANTHROPIC_MODEL
    - ANTHROPIC_DEFAULT_OPUS_MODEL
    - ANTHROPIC_DEFAULT_SONNET_MODEL
    - ANTHROPIC_DEFAULT_HAIKU_MODEL
    - CLAUDE_CODE_SUBAGENT_MODEL
    - CLAUDE_CODE_EFFORT_LEVEL
    - OPENAI_API_KEY
    - OPENAI_BASE_URL
```

规则：

- 未知字段必须报错，不能静默忽略。
- Boss CLI只连接本机operator Unix socket；Agent容器通过`runtime.md`定义的per-Run Unix socket连接，不要求暴露宿主TCP监听端口。
- 配置只保存 Daemon/runtime 设置。Agent 由 `coordplane agent add/update` 写入 SQLite，不能同时由 YAML 和数据库维护。
- per-agent 配置（adapter/image/model/subagent_model/base_url/effort/instructions_file/instructions_text）由 `coordplane agent add/update`、`PUT /v1/agents/{id}` 与前端编辑表单写入 SQLite，不进入 YAML；`adapter_id` 静态列表来自只读 `GET /v1/adapters`；`instructions_file` 与 `instructions_text` 互斥（恰有其一）；PUT 为全量替换（E5），CLI 与前端发送全量字段。
- Adapter/image等Runtime配置修改只影响新 Run；Run 必须保存本次解析后的 adapter、image 和 instructions hash。Retention是当前GC策略，每次preview/run都用当前值计算既有closed Task/terminal Run，不改写其`closed_at/ended_at`。
- Agent 指令可以描述 Manager/Developer/Integrator 工作方式，但 Daemon 不解析其语义。
- Provider credentials 只通过 runtime 明确 allowlist 注入，不写入配置快照、数据库、事件或日志。
- Claude provider 固定使用 `--bare`，并只透传上述 Claude 配置环境变量；不读取 OAuth/keychain/Boss HOME，不挂载或复制宿主 `~/.claude`。
- Codex provider 按固定 argv 模板启动（`codex exec`/`codex exec resume`，见 `runtime.md` §7.2），容器内固定 `HOME=/home/agent`、`CODEX_HOME=/home/agent/.codex`；只透传 allowlist 凭据，不读取或挂载宿主 `~/.codex`。
- `runtime.run_timeout` 可省略或设为 `0` 以禁用自动 deadline；正 duration 会在新 Run 启动时固化为 `deadline_at`，不追溯修改既有 Run。
- `runtime.shutdown_grace` 可省略（默认 `5s`），显式值必须为正 duration；SIGTERM、stop/cancel/timeout 与重启对账统一使用该 grace。
- Project 通过 Boss 命令注册，不要求写进配置文件。
- `retention`只接受正duration或`0`；`0`表示资源满足`runtime.md`/`git.md`全部GC fence后立即eligible，不表示跳过clean、task ref、pending action或ownership检查。Boss手动安全清理使用`gc preview`后执行`gc run --confirm`。

## 9. 唯一术语

| 术语 | 定义 |
| --- | --- |
| Project | 一个 daemon-owned Git repo 和其 canonical ref 的协调范围 |
| Participant | 统一参与者身份行（kind：`human` 或 `cli_agent`）；人类与 CLI Agent 共享同一身份、Task/Message 与权限框架，生命周期按 kind 分支 |
| Agent | 一个 `cli_agent` 类型的 participant 及其 adapter/runtime 配置 |
| Role | 可配置的 capability 集合（`roles` 表），只在项目绑定（`participant_project_role`）内生效；全局能力经 `project_id=global` 作用域 |
| Capability | 每个可调用 operation 的静态注册权限点；代码事实，不是数据，不提供动态 registry |
| Credential | 人类身份凭据（`credentials` 表）；v1 只签发 `operator_token`（保存 SHA-256 hash），支持轮换与吊销 |
| Task | 明确目标、责任人和结果状态的工作或对话单元 |
| Run | 针对一个 Task 启动的一次真实 CLI 进程执行；resume 也创建新 Run |
| Message | 绑定 Task 的 Boss/Agent 持久消息和唤醒单元 |
| Event | 关键状态变化的 append-only 记录 |
| Workspace | work/integration Task 的私有 Git 目录，是 Run/Task 的资源属性，不是业务对象 |
| canonical ref | Project 当前已集成代码的唯一 Git ref |
| task ref | Daemon 为 Task 捕获并保护提交的 Git ref |
| base SHA | Task 开始时固定的基线 commit |
| head SHA | Daemon 从实际 workspace 捕获的提交 commit |
| CLI native session ID | Codex/Claude 等 CLI 自己的 resume key；它不是 live process 证明 |

除本文件“明确非目标”外，其他需求不得把被删除的旧术语重新定义为持久对象或服务。

## 10. 第一版完成定义

只有同时满足以下条件，才能声明第一版完成：

- 五份需求文档描述同一组对象、状态和命令，不存在第二套真相或旧协议。
- Boss 能与 Agent 对话、派发、查看进度、收取结果、返工、取消和唤醒。
- 人类与 CLI Agent 作为统一 participant 协作：可互相派发 Task、发送 Message、按项目角色执行；人类任务以 `human_confirm` 证据收敛，凭据轮换/吊销立即生效，最后管理员保护生效。
- 两个 CLI Agent 能在隔离容器和私有 workspace 中真实并发。
- 消息在 Run/Daemon 失败和 resume 后不丢失。
- 两个 Agent 的提交都被 task ref 保存，并通过 Git CAS/integration Task 收敛到 canonical ref。
- Daemon 重启不会产生重复 active Run、伪完成、丢提交或错误覆盖 canonical ref。
- 两个真实CLI Agent完成端到端闭环；另有一个fresh四Agent真实场景证明4个source Run并发、Message、task ref、integration和canonical CAS能够在一次Daemon重启后继续收敛。
- Budgeted maintained production/tests/infra/total分别不超过`25,000 / 27,000 / 700 / 52,700`，质量blocker清零；没有为省LOC弱化测试或关键恢复合同。
- `acceptance.md` 的静态约束、合同测试、真实 Docker/Git gate 和真实 CLI gate全部通过。
