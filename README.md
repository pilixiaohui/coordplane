# CoordPlane 独立需求文档索引

本文档集定义一个新的轻量级多 Agent 协作后台服务：`CoordPlane`。

这些文档应能被单独复制到一个新仓库中，作为开发依据使用。每份文档都应包含足够的术语、边界、接口和验收标准。

在 CoordPlane 项目仓库中，`need/` 是唯一需求基线。架构设计、实现拆分、测试计划、release gate 和产品化验收必须以 `need/` 下的完整文档树为准；issue 评论、临时运行产物、历史总结或单个 README 摘要只能作为补充上下文，不能替代 `need/` 中的需求与验收合同。

## 1. 产品定位

`CoordPlane` 是一个常驻后台服务，用于让多个 CLI Agent 在隔离运行环境中协作。第一版核心是统一 Agent 通信、可追责任务、durable mailbox、会话恢复和后台真相源；受控 Git 系统是建立在同一通信和会话协议上的能力组，用于支持并发代码开发、提交、合并、冲突修复和回滚。

它解决的问题是：

- 多个 Agent 如何通过统一通信信封领取任务、提交结果、相互反馈。
- 一个 Agent 如何把新任务派发给另一个 Agent。
- 子任务完成后如何在活跃会话中 same-turn steer 原发布者，或在不活跃时恢复原会话判断下一步。
- Docker 容器内的 Agent 如何通过稳定接口访问后台服务。
- Agent 会话启动前如何准备环境、启动中如何持久化 resume 路由、结束后如何收尾和恢复。
- 多个 Agent 如何在私有工作区中并发修改代码，并通过后台服务能力完成同步、提交、合并和回滚。
- 后台如何保存协作真相、会话路由、消息和证据，避免依赖临时文件或 Agent 自己的记忆。

`CoordPlane` 不是一个业务开发框架，不内置项目语义，不硬编码团队角色，也不替 Agent 做业务判断。

## 2. 核心组件

| 组件 | 职责 |
| --- | --- |
| CoordPlane backend | 常驻后台服务，保存数据库真相，提供 capability API、skills、任务、消息、邮箱、证据、递送、Git 操作和鉴权接口 |
| coordlink | 运行在 Agent 环境内的本地客户端，把本地命令或 schema-derived tool adapter 调用转发到 backend |
| AgentCommunicationEnvelope | Agent 间通信、任务、结果、修复反馈和预算提醒的统一协议信封 |
| TeamConfig | 定义通用团队成员、角色提示词、skills、capability policy、runtime/CLI 和终止验收策略 |
| Skills | Agent-facing 工作流说明层，按需告诉 Agent 何时、为何、如何调用 capability |
| Runner | 启动、停止、恢复 CLI Agent 会话，管理 Docker 或 external runtime |
| CLI Agent | Codex、Claude Code、OpenCode 等具体 Agent CLI，负责思考、执行、调用 CoordPlane capability |
| Runtime | Agent 所在环境，可以是 Docker 容器，也可以是外部调试环境 |
| Testing Acceptance | 定义 CoordPlane 的全局测试分层、验收口径、场景矩阵和 release health gate |

## 3. 当前目标范围

第一阶段先按单用户后台服务实现核心通信闭环，再覆盖 Agent 实时递送、会话恢复和受控代码管理。单用户只表示默认 tenant/user/policy，不表示临时脚本；store、queue、policy、capability、skill、adapter 和 object store 都必须按长期接口设计。代码管理不是后台自动替 Agent 合并，而是 Agent 在自己的会话内通过 CoordPlane 接口主动发起同步、提交、合并、冲突解决和回滚。

必须实现：

- 单用户 backend MVP：默认 tenant/user、canonical store、event log、DB queue、policy interface、capability registry、skill registry、inspect API。
- TeamConfig：定义 agents、roles、skills、capability policy、runtime/CLI、终止验收和验证策略。
- 统一通信信封：表达 message、task、followup、result、repair、validation、budget_attention 等 Agent 间通信语义。
- 任务合同：表达谁要完成什么工作。
- 分配、租约、尝试：表达谁正在处理、当前处理权和一次 CLI 会话。
- 对话线程和消息：表达 Agent 之间的信息交换。
- 邮箱项：表达需要通知哪个 Agent、关联哪个原会话，以及 Agent 需要主动读取的 durable 待处理事项。
- 报告、产物、验证结果：表达任务完成证据。
- Docker runtime：每个 Agent 独立容器运行。
- external runtime：用于本机调试和协议测试，语义必须等价于 Docker runtime。
- Skills-first Agent 接口：Agent 通过 skills 理解何时、为何、如何调用 CoordPlane capability。
- coordlink 本地客户端：让容器内 Agent 稳定访问 backend capability API。
- 实时消息递送：backend 创建 mailbox 后，活跃 CLI 会话优先通过 turn steer / same-turn steering 收到授权 envelope 摘要或轻量 signal，再由 Agent 主动读取和处理完整上下文。
- 会话生命周期：会话前准备、会话中 pin/resume/progress、会话后终态收敛和异常恢复。
- 受控 Git 操作：作为独立能力组接入同一 backend truth；Agent 通过接口完成 workspace sync、diff、commit、merge、conflict resolve 和 rollback。

暂不作为核心能力：

- 多用户账号系统和完整 RBAC。
- 分布式队列、远程 runner 集群和 Kubernetes 部署。
- 复杂 dashboard UI、完整 secret vault 和长期语义记忆。
- 把 MCP 或任何单一工具协议作为核心协议前提；工具入口必须从 capability registry 派生。
- 项目业务语义检查。
- 自动替 Agent 决策下一步任务。
- Agent 之间直连通信。
- 多套互不一致的 HTTP/tool/CLI 状态逻辑。

如果后续增加远端 PR 平台集成、复杂发布流水线或跨仓库事务，也必须作为独立能力接入同一 backend truth，不能改变本阶段的任务、消息和受控 Git 操作协议。

## 4. Codemap 开发使用方式

`coordplane codemap` 用于在开发时生成可查询的工程知识索引快照。当前验收口径只承认 `status: "ready"` 的原子 ready snapshot；`status: "partial"` 或包含 error diagnostic 的 artifact 只能用于排障，不能作为 CI、评审或需求验收结果。

开发时推荐把快照写到固定路径：

```bash
export PROJECT_ID=coordplane-dev
export RESOURCE_ID=coordplane-repo
go run ./cmd/coordplane codemap index \
  --root . \
  --project-id "$PROJECT_ID" \
  --resource-id "$RESOURCE_ID" \
  --strict \
  --out .coordplane/codemap/latest.json
```

参数说明：

- `--root` 指向要索引的仓库根目录，通常是当前仓库 `.`。
- `--project-id` 和 `--resource-id` 是 project/resource stamp，会写入 snapshot，并参与稳定 `snapshot_id` 计算；同一个验收对象应使用同一组 stamp，避免把不同项目或资源的索引混用。上面的 `coordplane-dev` / `coordplane-repo` 只是本地示例值，验收时应替换为真实 project/resource 身份。
- `--strict` 会在生成结果不是可提升的 ready snapshot 时失败；失败时不会把 partial snapshot 写成 `latest.json`。
- 不带 `--strict` 的 `index` 可能写出 diagnostic-only 的 partial artifact，便于查看缺失 Go module、语法错误或收集器错误，但该文件不能替代 ready snapshot。

生成后先验证快照结构和可提升状态：

```bash
go run ./cmd/coordplane codemap validate \
  --snapshot .coordplane/codemap/latest.json
```

`validate` 成功时输出包含 `"ok": true`、`schema_version`、`snapshot_id`、`status` 和 `diagnostic_count`；如果快照是 partial、building、schema 不匹配、stable id 漂移、包含 error diagnostic 或泄露不允许的路径/敏感标记，命令会返回非零退出码。

提交或验收前用 `check` 对比当前仓库与已保存的 ready snapshot：

```bash
go run ./cmd/coordplane codemap check \
  --root . \
  --snapshot .coordplane/codemap/latest.json
```

`check` 会先验证已有 snapshot，再重新索引当前 `--root` 并比较规范化 JSON；发现源码、文档、配置、测试或 stamp 导致的 drift 时返回非零退出码，并提示重新运行 `codemap index`。如果未显式传入 `--project-id` / `--resource-id`，`check` 会复用已有 snapshot 中的 stamp；需要确认特定项目/资源身份时也可以显式传入：

```bash
go run ./cmd/coordplane codemap check \
  --root . \
  --project-id "$PROJECT_ID" \
  --resource-id "$RESOURCE_ID" \
  --snapshot .coordplane/codemap/latest.json
```

推荐验证命令：

```bash
go test ./cmd/coordplane ./internal/codemap
export PROJECT_ID=coordplane-dev
export RESOURCE_ID=coordplane-repo
go run ./cmd/coordplane codemap index --root . --project-id "$PROJECT_ID" --resource-id "$RESOURCE_ID" --strict --out .coordplane/codemap/latest.json
go run ./cmd/coordplane codemap validate --snapshot .coordplane/codemap/latest.json
go run ./cmd/coordplane codemap check --root . --snapshot .coordplane/codemap/latest.json
```

验收时以 `validate` / `check` 均成功的 ready snapshot 为准；partial artifact、临时 stdout、历史评论或未带正确 project/resource stamp 的快照都不能作为最终验收证据。

## 5. 文档目录

- `backend/`
  - 单用户 CoordPlane 后台 MVP、canonical store、queue、policy、capability registry、skill registry、TeamConfig、object store、inspect 和后续扩展接口。
- `coordination/`
  - 任务、分配、租约、尝试、对话、邮箱、同会话处理入口和证据协议。
- `runtime/`
  - Docker 隔离、external runtime、runner、coordlink、本地客户端和后台连接协议。
- `skills/`
  - skills-first Agent 工作流、skill registry、progressive disclosure、skill binding 和 capability 关系。
- `team_config/`
  - 通用团队配置、角色提示词、capability policy、runtime/CLI 绑定和终止验收策略。
- `session_lifecycle/`
  - Agent 会话预处理、启动顺序、session pin、active guard、后处理、异常恢复和测试边界。
- `message_delivery/`
  - Mailbox、DeliverySignal、DeliveryAttempt、ActiveTurnRoute、same-turn steer、fallback resume 和递送测试边界。
- `code_management/`
  - 多 Agent 并发代码开发、后台受控 Git 操作、合并、冲突解决、回滚和审计协议。
- `testing_acceptance/`
  - 全局测试验收口径、测试分层、场景矩阵、CI gate 和 release health check。

## 6. 全局设计原则

- Backend 是唯一真相源：任务、消息、邮箱、证据、会话路由都必须落到 backend 数据库。
- 单用户不是临时实现：MVP 固定 `tenant_id=default`，但所有 service handler 仍必须经过 policy、store、queue、capability 和 adapter 接口。
- Agent 主动调用 capability：完成、派发、等待、反馈都必须由 Agent 显式调用接口。
- Skills-first：skills 是 Agent-facing 工作流和使用说明层；backend capability API 是机器接口；schema-derived tool adapter 是 Agent 工具入口，MCP 只能是其中一种兼容实现。
- 统一通信信封优先：message、task、followup、result、repair、validation 等都落到 AgentCommunicationEnvelope；WorkContract 是其中可追责任务层。
- 实时通知不绕过鉴权：same-turn steering 可以携带已授权 envelope 摘要或短正文，但完整正文、artifact 和验证详情必须可通过 backend API 读取，不能泄露未授权内容。
- Backend 不做业务判断：它只验证协议、权限、证据完整性和状态机不变量。
- 接口 role-agnostic：capability 不硬编码 assistant、developer、reviewer 等角色名。
- Adapter 不复制业务逻辑：HTTP、coordlink CLI、schema-derived tool adapter 只能调用同一组 backend capability handler。
- Docker 与 external runtime 协议等价：不同 runtime 只影响启动方式，不影响服务语义。
- 会话状态必须真实：workspace/toolchain/coordlink 未准备好前不能把 attempt 标记为 running。
- 失败必须反馈给 Agent：缺证据、无权限、状态不符都要返回可修复的 rejected response。
- Git 操作必须在会话内反馈：同步、提交、合并、冲突和回滚结果必须返回给当前 Agent 会话，不允许等会话结束后后台静默处理。
- 可删除式设计：新增 capability、检查、runtime、adapter 必须通过注册表接入，删除时不改主流程。

## 7. 推荐阅读顺序

1. `backend/coordplane_single_user_backend_mvp_requirements_2026-07-03.md`
2. `skills/coordplane_skills_first_agent_workflow_requirements_2026-07-03.md`
3. `team_config/coordplane_team_config_requirements_2026-07-03.md`
4. `coordination/coordplane_task_and_conversation_requirements_2026-07-03.md`
5. `runtime/coordplane_local_client_coordlink_2026-07-03.md`
6. `message_delivery/coordplane_live_message_delivery_requirements_2026-07-03.md`
7. `session_lifecycle/coordplane_agent_session_lifecycle_requirements_2026-07-03.md`
8. `code_management/coordplane_code_management_requirements_2026-07-03.md`
9. `testing_acceptance/coordplane_test_acceptance_requirements_2026-07-04.md`

读完这些文档，应能开始设计 Go 后端的数据模型、service handler、capability registry、skill registry、coordlink 客户端、Docker runner、受控 Git service、测试验收 gate，以及 schema-derived tool adapter。
