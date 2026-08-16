# CoordPlane v1 需求基线

状态：候选冻结基线，待需求审批人复核精确 revision
版本：1.0-rc3
日期：2026-08-16

## 1. 文档权威

本目录包含五份规范与一份原话审计记录。五份规范共同构成 CoordPlane v1 的唯一可执行需求事实源：

1. `README.md`：产品定位、范围、术语和全局不变量。
2. `core.md`：持久对象、状态机、权限、任务、Conversation 与 Message。
3. `runtime.md`：Docker Run、CLI adapter、隔离、行为日志和恢复。
4. `git.md`：私有 workspace、task ref、接受、集成、CAS 和 Git 恢复。
5. `acceptance.md`：可执行验收合同、分层 gate 和代码预算。

另外，`user-requirements-verbatim.md` 是 append-only 用户需求原话与变更溯源记录，不直接定义产品行为，不能代替上述五份规范。从 UR-0008 起，用户任何新的产品、验收、开发流程或配置需求必须先追加原话记录，再归一化到规范、issue 和测试。更正或撤回也追加新记录，不回改旧原话；凭据值按该文档安全例外处理。

五份规范和该原话记录必须在同一个 Git revision 上冻结，收据记录原话文档 blob SHA 和已处理的最新 `UR-NNNN`。发生冲突时不得选择性引用；先修改本需求基线并重新审批，再修改测试或实现。测试把本基线转成可执行断言，CI只重复执行这些断言，不另行定义产品需求。运行结果、当前缺陷和候选 gate 收据属于 issue/构建产物，不写入规范正文。

`acceptance.md` 指定的小说系统需求目录是外部真实开发负载，不是第六份 CoordPlane 产品需求。它用于证明通用协作机制能支撑真实项目；小说领域规则、角色职责和验收含义不得硬编码进 CoordPlane Core。

## 2. 一句话定位

CoordPlane 是本机运行的多参与者后台协调服务。它接收 Participant 创建的 Task 和 Message，按配置唤醒 Docker 内的 CLI Agent并传递上下文，管理并发 Git workspace、结果引用和 canonical 集成，保存可查询的状态与完整可观测行为日志。

CoordPlane 不理解参与者的业务职责，不按自然语言拆解工作，不判断代码是否正确，也不替参与者决定谁应实现、审查或接受结果。职责、权限、模型、提示词和协作方式均由使用者配置。

标准闭环是：

```text
Participant 创建 Task / Conversation / Message
  -> 明确 assignee 或 Message recipients
  -> cli_agent 由 Scheduler 创建 Docker Run；human 在宿主机操作
  -> Participant 通过 Conversation 交流，通过私有 Git workspace 并发工作
  -> 结果捕获为不可变 task ref
  -> 有权限的 Participant 显式 accept/rework/cancel
  -> Git expected-old CAS 或显式 integration Task 推进 canonical
  -> 状态、消息、Git SHA、Run 和行为日志可从 CLI/Web UI 查询
```

## 3. 权威事实

| 事实 | 权威来源 |
| --- | --- |
| Project、Participant、Role、Task、Conversation、Message、Run 和关键状态 | CoordPlane SQLite |
| 项目代码、提交 lineage、未集成结果和 canonical | 实际 Git object/ref |
| 容器、进程、退出码和资源是否存在 | 实际 Docker/OS 观察 |
| CLI Agent 可观测行为 | 每个 Run 的追加式、已脱敏行为日志及其完整性索引 |

约束：

- 数据库缓存 SHA 与实际 ref 不一致时，以实际 ref 为准并显式 reconcile，禁止用缓存覆盖 Git。
- CLI 文本声称完成不是状态事实；只有结构化 Task 转移和实际 Git 结果能推进状态。
- Event 是关键状态变化的审计事实，不替代当前对象，也不承载大体积行为流。
- 行为日志不作为 Task 成功或 Git 集成授权，但必须足以重建 CoordPlane 可观测到的 Agent 行为顺序。
- Prompt、容器文件、PID 文件、进程内队列和 Web 前端缓存都不是持久真相。

## 4. 参与者与组件

| 名称 | 定义 |
| --- | --- |
| Participant | 唯一业务身份；`kind=human|cli_agent`。Task、Conversation、Message、权限和 Git 合同统一使用 Participant ID |
| Human | 在宿主机通过 Operator CLI 或本机 Web UI 使用服务的 Participant；不产生 Docker Run |
| CLI Agent | 由静态 CLI adapter 配置、在 Docker Run 中执行的 Participant |
| Role | 可配置 capability 集合；名称无内置业务含义 |
| Capability | 每个 Service operation 对应的静态权限点；通过 Role 绑定到 Participant |
| Credential | Human 的本机认证凭据；Agent 使用每个 Run 独立的短期 token |
| Project | 一个协作范围和一个 daemon-owned Git canonical repository |
| Task | 可派发、可提交、可接受的工作责任；不承载对话生命周期 |
| Conversation | Project 内一对一或群组对话及成员关系 |
| Message | 属于一个 Conversation、可选关联一个 Task、显式指定一个或多个接收者的不可变消息 |
| MessageRecipient | Message 对每个接收者独立的未读、递送、确认和重试状态 |
| Run | 一次真实 CLI Agent 进程；可由 Task 或无 Task 的 Conversation Message 触发 |
| Event | 小型、追加式关键状态变化 |
| Behavior Log | Run 的详细可观测行为流；与 Event 分层保存 |

`human` 与 `cli_agent` 只在认证和执行介质上不同：Human 在宿主机工作且无 Run；CLI Agent 在 Docker 内工作并由 Scheduler 唤醒。角色名、职责和业务流程不得按 `kind` 硬编码。理论上任何 Agent Task 都可派给 Human，实际是否这样使用只由配置和派发决定。

系统不得设置 Human、CLI Agent、Task 或 Conversation 的产品级数量上限。`max_parallel_runs` 只限制同时执行的 Docker Run，不限制已配置 Participant 数量。

## 5. v1 必须完成

### 5.1 本机控制面

- 单 Daemon、file-backed SQLite、每个 Project 一个 daemon-owned Git repo。
- Human 通过本机 Unix socket 使用 Operator CLI；Web 服务只监听 loopback，必须认证并执行相同 capability 检查。
- CLI Agent 只通过每个 Run 的私有 Unix socket 和 token 使用服务。
- 不提供远程登录、远程 runner、多 Daemon 共同写同一 data directory 或公网监听。
- 空库必须通过显式、仅一次的本机 bootstrap 创建首个 Participant、Credential 和可配置的全局管理 Role。完成 bootstrap 后，该 Participant 不具有特殊业务身份；任意 Participant 均可按配置取得管理 capability，并保留最后管理者保护。

### 5.2 统一 Participant 与权限

- `participants` 是身份和 CLI Agent 配置的单一权威表；不得保留 `agents` 镜像形成双真相。
- Participant 支持创建、读取、更新、暂停和归档。`paused` 不接收新 Task 指派或新 Conversation 成员关系，已有工作和按权限可读历史保留；CLI Agent 另外不启动新 Run，当前 Run不被静默杀死。
- 每个 operation 在一个静态注册表中声明 capability、输入和允许的 transport；删除 operation 只需移除注册项和实现。
- Operator CLI、Web API 和 coordlink 调用同一 Service operation，不复制状态机，不按角色名分支。
- 传输或运行环境可以使某些参数实际不可用，例如容器不能提供宿主机 repository 路径；这不是职责限制，也不能绕过 capability。

### 5.3 Task 与并发执行

- work/review/integration 等业务含义不进入 Task 状态机；Task kind 只保留确有不同机械收尾规则的最小集合。
- Task 必须指派具体 Participant。Human 与 CLI Agent 共用创建、claim、wait、submit、fail、accept、rework、retry 和 cancel 语义。
- CLI Agent Task 由 Scheduler 创建 Docker Run；Human Task 由 Human 显式 claim，不创建 Run。
- 每个代码 Task 都有私有 workspace。CLI Agent workspace 挂载进自己的容器；Human workspace 以受控宿主机路径提供。
- 同一 CLI Agent 同时最多一个 starting/active Run；多个 Agent 可按 `max_parallel_runs` 并发执行。
- Daemon 不读取 Task 文本决定顺序、角色、验收或冲突处理。

### 5.4 Conversation、Message 与未读

- Conversation 是正式持久对象，支持一对一和群组，成员可读取该 Conversation 的完整历史。
- Message 必须属于一个 Conversation，可选关联一个同 Project Task，并显式指定一个或多个 Conversation 成员为接收者。
- 每个接收者独立维护未读、delivered、acknowledged、cancelled 和重试状态；一个接收者的操作不得改变其他接收者状态。
- 只有明确接收者能产生未读项。Conversation 非接收成员可以读取历史，但不会因此被唤醒。
- `wake=true` 只唤醒明确指定且为 CLI Agent 的接收者；Human 永不产生 Run。
- `wake=false` 不启动 Run，但未读消息必须持久保留。CLI Agent 下次在同 Project 启动任何 Run 时，bootstrap 必须携带未读总数、高水位、有界 ID 样本和 inbox cursor；Agent 通过分页 inbox 读取并确认全部未读，不得为了列出所有 ID 使 bootstrap 无界增长。
- 无 Task 的 wake Message 可以创建 Conversation Run；该 Run 不挂载项目代码 workspace。
- Message 先持久化再递送，采用逐接收者 at-least-once 语义。Inject 是优化，失败不得丢消息。

### 5.5 Git 协作

- 每个代码 Task 创建时从 actual canonical 固定 `base_sha`，结果从实际 workspace HEAD 捕获到不可变 task ref。
- Human 与 CLI Agent 使用相同的 `expected_head`、clean/in-progress 检查、task ref、accept、rework 和 canonical expected-old CAS。
- Human 仅省略 Docker Run；不能用 Human 身份绕过 workspace、capture 或 Git fence。
- 结果是否正确由有权限的 Participant 显式判断。Daemon只执行 Git 机械核验和 CAS。
- canonical 已移动时创建显式 integration Task，指派配置的 integration Participant；该 Participant 可以是 Human 或 CLI Agent。
- Agent 之间通过各自私有 workspace、commit/ref 和 integration Task 并发协作，不共享可写 Git metadata。

### 5.6 行为日志

每个 Run 必须保存两层追加式日志：

1. 已脱敏原始流：尽可能保留 provider/CLI 输出帧、stdout/stderr 和未知帧。
2. 规范化行为流：为查询和 Web UI 统一记录序号、时间、来源、类型、参数摘要、结果摘要、退出码、耗时、关联 Participant/Project/Task/Conversation/Message/Run 及原始流 offset/hash。

在 provider 或 OS 实际暴露的范围内，日志必须覆盖：

- CLI stdout/stderr、provider 结构化事件和 session 信息。
- tool 调用、参数、结果、错误、退出码和耗时。
- shell 命令及其可观测输出。
- coordlink 请求/响应、Task/Message/progress/权限操作。
- Run、Docker、resume、inject、cancel、timeout、cleanup 和 reconcile 生命周期。
- Git 命令观察、操作前后 HEAD/status/task ref/canonical SHA。
- 未知帧、解析失败、丢失区间、截断和脱敏事件。

不要求也不得声称保存 provider 隐藏 chain-of-thought、加密推理、provider 内部状态或 CoordPlane 无法观察的宿主行为。凭据、token 和明确敏感内容必须在落盘前脱敏；脱敏本身留下类型和位置记录，但不保存原值。

默认行为日志保留期为 7 天。Project 可覆盖该期限；每个 Run 唯一的 Behavior Log index 可显式标记 `long_term`，标记期间禁止自动删除。取消标记后，立即使用当前 Project 策略和 Run 原始 `ended_at` 重新计算，可能马上进入 GC eligible。Run 不保存第二份可变 `long_term` 值。Git 继续保存代码真相，日志引用 SHA，不复制完整代码快照。

### 5.7 Web UI

Web UI 是 v1 必交付的本机协调界面，至少提供：

- Project、Participant、Role、CLI Agent 配置和权限范围内的管理。
- Task 树、assignee、状态、Run、等待/失败原因及 create/claim/submit/accept/rework/retry/cancel/wake 操作。
- 一对一/群组 Conversation、显式 recipients、未读状态和 Message 操作。
- Run 行为日志实时跟随、历史筛选、详情、导出和 `long_term` 标记管理。
- base/head/task ref/canonical SHA、capture 和 integration 状态的只读展示。

Web UI 不提供代码编辑器、浏览器终端、任意宿主文件访问或 raw Git ref 修改。所有 mutation 使用与 CLI 相同的 Service operation、CAS、幂等键和权限检查。Web 必须对会话、CSRF、Origin/Host、CORS、CSP、Cookie、HTML/日志输出编码及 SSE/WebSocket 重新授权建立明确的本机安全边界；loopback 不是免认证或免浏览器攻击防护的理由。

## 6. 明确非目标

- 远程账号登录、组织/tenant、跨主机 runner、分布式队列、Kubernetes 或 HA。
- 通用 policy DSL、动态 capability registry、角色职责语言或自动团队编排。
- CoordPlane 内置 LLM、自动拆任务、自动选人、自动验收代码或自动解决冲突。
- 自建 Git hosting、PR/CI/发布平台、自动远端 push 或跨仓库原子事务。
- 代理通用 `git status/diff/add/commit/rebase/merge` API；参与者在私有 workspace 使用标准 Git。
- Artifact/ObjectStore、长期语义记忆、向量数据库、模型成本平台或复杂运营 Dashboard。
- Agent 之间绕过 CoordPlane 的内部点对点协议；内部交流通过 Conversation/Message。
- 保存不可观察的模型内部推理，或以日志替代结构化状态和 Git 事实。
- 通用 MCP/plugin/skill 平台、A2A 对外互操作或远程发现协议。

Web UI、完整可观测行为日志、可配置权限和群组 Conversation 是 v1 正式范围，不得再列为平台化非目标。

## 7. 设计原则

### 7.1 机械协调

Daemon只做持久化、权限判断、调度、隔离、消息递送、Git事实核验、状态恢复、日志采集和 GC。所有需要理解业务内容的动作由有权限的 Participant 显式发起。

### 7.2 Build to Delete

新增 worker、Task handler、adapter、operation、Event renderer、日志 parser/normalizer、GC step 和验收场景必须是独立函数并通过静态列表注册。删除一项只移除注册项和实现，不修改共用执行器，不在循环中按名称或角色增加 if/else 链。

### 7.3 Add-Remove Balance

新机制替代旧机制时，同一变更删除旧表、旧字段、旧入口、旧 fixture 和保护旧语义的测试。不得长期保留 `Boss`、`agents` 镜像、conversation Task、Human 专用 Task FSM 或旧 Message 单接收状态作为兼容路径。

### 7.4 Fail loud

不能证明安全时停止推进并保留诊断事实。外部命令 exit 0 不是充分证据；SQLite、Git、Docker 和行为日志索引必须读取实际结果。失败不能转换为空成功，也不能用 Agent 自报或低层测试替代失败的高层 gate。

### 7.5 最小持久化

只为需要独立生命周期、并发控制、权限或查询的事实建对象。Conversation 和逐接收者状态因群聊、未读和唤醒需要独立持久化；日志大正文留在文件，SQLite只保存索引、hash、offset、保留状态和关键投影。

### 7.6 真实项目循环

每个拟进入验收的 CoordPlane 候选 SHA 先通过 L1-L4，再使用同一 SHA/image 驱动指定小说系统需求的一轮真实多 Agent 开发。该轮必须有唯一小说系统不变量、固定输入 canonical/需求 manifest、真实 Task/Conversation/Message/Run/Git 证据和日志审计收据。

L5 只负责暴露组合和现场问题，不作为盲目调试循环。任何 CoordPlane 产品 finding 先分类并归约成最低可重复的 L1-L4 红测试，修复后先跑低层 gate，再用新 candidate 回放原失败边界并开始下一轮真实开发。同一修复连续两轮不过同一 gate 时停止补丁，回到需求、契约、实现、provider/环境或负载任务规格归因。

## 8. 最小配置

```yaml
data_dir: /path/to/coordplane-data
operator_socket: /path/to/coordplane-data/operator.sock
web_listen: 127.0.0.1:8090
max_parallel_runs: 4

retention:
  completed_workspace: 24h
  terminal_task_ref: 168h
  behavior_log: 168h

runtime:
  docker_network: coordplane
  run_timeout: 0
  shutdown_grace: 5s
  workspace_root: /path/to/coordplane-data/workspaces
  participant_home_root: /path/to/coordplane-data/participant-homes
  log_root: /path/to/coordplane-data/logs
  default_image: coordplane-agent:latest
  provider_secret_files: {}
```

规则：

- 未知字段、非法 duration 和非 loopback `web_listen` 必须拒绝启动。
- `0` retention 表示满足全部 fence 后立即 eligible，不表示跳过 active、pending、Git、ownership 或 `long_term` 检查。
- Project 的 retention override 写入 SQLite并即时用于下一次 preview/GC；不改写历史时间。
- CLI Agent 配置写入 Participant，不在 YAML 和数据库维护两份列表。
- provider secret 通过受控只读 secret file 进入容器进程，不把 secret 值写入 Docker Config.Env、argv、SQLite、Event、日志或 Web API。

## 9. 唯一术语

| 术语 | 定义 |
| --- | --- |
| canonical | Project 当前已接受代码的实际 Git ref |
| task ref | 某次 Task 结果的 daemon-owned 不可变 Git ref |
| private workspace | 单个代码 Task 独占的可写 Git workspace |
| Participant | Human 或 CLI Agent 的统一身份 |
| Conversation | Project 内成员可读的一对一或群组对话 |
| recipient | Message 明确指定且拥有独立未读/递送状态的 Participant |
| Run | 一次不可复活的真实 CLI Agent 进程 |
| Conversation Run | 由无 Task wake Message 触发且不挂载代码 workspace 的 Run |
| Event | 关键状态变化的小型追加事实 |
| Behavior Log | 一个 Run 的详细、已脱敏、可校验行为流 |
| long_term | 保存在Run唯一Behavior Log index上、禁止该行为日志自动GC的显式标记 |
| reference workload round | 验收驱动器基于固定外部需求 manifest 和输入 canonical 执行的一轮真实多 Agent 开发；不是 CoordPlane 持久对象 |
| accept | Participant 对结果的显式业务决定 |
| integrate | Participant 在 integration Task 中使用 Git 收敛 stale 结果 |

不得重新引入 Boss、human task、conversation Task、agents mirror、Validation、ConflictSet 或通用 policy engine 作为第二套概念。

## 10. v1 完成定义

必须在同一候选 SHA 上同时满足：

- 五份规范一致并记录冻结 SHA；同revision的原话记录已追加所有已接收新需求，收据含其blob SHA和最新记录ID。
- SQLite migration、状态机、权限和公开 API 契约通过。
- Human 与 CLI Agent 的统一 Task/Git 流程通过独立契约测试。
- 群组 Conversation、逐接收者未读、定向 wake、重启恢复和未读 bootstrap 通过。
- 真实 Docker 隔离、CLI adapter、resume、cancel、timeout、reconcile 和 cleanup 通过。
- 行为日志完整性、脱敏、查询、导出、7 天默认/Project override/`long_term` GC 通过。
- deterministic 双 Agent 和真实多 Agent Git/Message 收敛场景通过。
- 指定小说系统需求的 reference workload round 在同一 candidate SHA 上通过，并有需求 manifest、输入/输出 canonical、协作证据、日志完整性审计和 finding 处置收据。
- 本机 Web UI 的核心工作流和权限负例通过浏览器验收。
- `acceptance.md` 的静态、单元、契约、race、真实边界和 LOC gate 全部通过。
- 验证、审查和验收证据全部指向同一候选 SHA；失败的高层 gate 不得由低层绿色结果替代。
