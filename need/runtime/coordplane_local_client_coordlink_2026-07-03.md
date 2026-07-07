# CoordPlane Runtime 与 coordlink 本地客户端需求

本文是 CoordPlane runtime 模块和 `coordlink` 本地客户端的独立需求说明。它定义如何在 Docker 或 external 环境中运行 CLI Agent，并让 Agent 通过 skills 指引和 `coordlink` 稳定访问 CoordPlane backend capability。

## 1. 背景和目标

CoordPlane 要支持多个 CLI Agent 并发协作。每个 Agent 应在独立环境中工作，不能直接读取后台数据库、其他 Agent 的工作区或调度文件。

目标运行模型：

```text
CLI Agent
  -> skills 指引下的本地命令或 schema-derived tool adapter 调用
  -> coordlink
  -> CoordPlane backend
  -> database / object store / event log
```

Docker runtime 用于真实隔离和并发运行。External runtime 用于本机调试、协议测试和不启动容器时的手动模拟。两者在服务协议层必须等价。

## 2. 术语

| 名称 | 定义 |
| --- | --- |
| CoordPlane backend | 常驻后台服务，提供任务、消息、证据、邮箱和递送接口 |
| coordlink | 安装在 Agent 运行环境中的本地 capability client、skill helper 和 tool adapter bridge |
| Runner | 管理 runtime、容器、CLI Agent 进程和 session resume 的组件 |
| Docker runtime | 每个 Agent 一个独立容器，拥有私有 workspace 和持久 home |
| External runtime | 本机或开发环境中的 Agent 入口，用于调试，语义等价于 Docker runtime |
| CLI backend adapter | Runner 用于启动或恢复具体 CLI Agent 的适配器，例如 codex、claude |
| Capability | Backend 注册的机器接口，例如 `contract.add`、`mailbox.get`、`git.commit` |
| Skill | Agent-facing 工作流说明，告诉 Agent 何时、为何、如何调用 capability |
| AgentCommunicationEnvelope | Agent 之间消息、任务、结果、修复和预算提醒的统一通信对象 |
| Tool adapter | 从 backend capability registry 派生的 CLI 工具入口，例如某些 CLI 的原生 tool、plugin 或 MCP 兼容层 |
| Session route | 恢复同一 CLI 会话所需的 backend 持久记录 |

## 3. 命名

新后台服务名：`CoordPlane`。

容器内本地客户端名：`coordlink`。

选择 `coordlink` 的理由：

- 表达“把 Agent 运行环境连接到 CoordPlane backend”。
- 不暗示它是调度器、数据库或 Agent 本体。
- 名称不绑定 Docker、不绑定 AI、不绑定某个 CLI。
- 可同时服务 Codex、Claude Code、OpenCode 等 CLI Agent。
- 命令名短，适合在容器、tool adapter 配置和调试脚本中长期使用。

## 4. 总体架构

```text
CoordPlane backend
  - API handlers
  - capability schema provider
  - skill registry
  - database truth
  - object store
  - event log
  - delivery signal queue

Runner
  - creates runtime
  - injects coordlink config
  - starts/resumes CLI Agent
  - records attempt and transcript refs

Runtime
  - Docker container 或 external environment
  - contains CLI Agent
  - contains coordlink
  - contains project workspace

coordlink
  - capability command client
  - skill reader/sync helper
  - schema-derived tool adapter bridge
  - local command adapter
  - backend client
```

关键边界：

- Backend 是唯一状态真相源。
- Runner 管生命周期，不做业务判断。
- coordlink 是 Agent-facing adapter，不保存任务真相。
- CLI Agent 负责思考、执行和显式调用 CoordPlane capability。

## 5. Docker runtime 需求

每个 Agent 必须运行在独立 Docker 容器中，除非显式使用 external runtime 调试。

每个容器必须有：

- 私有项目工作区，例如 `/workspace/project`。
- 持久 CLI home，例如 `/home/agent`。
- `coordlink` 可执行文件。
- 已配置好的 CLI Agent。
- 指向 CoordPlane backend 的网络访问能力。
- Agent token 和 runtime identity。

每个容器不得有：

- Backend 数据库路径。
- Host runtime root。
- 其他 Agent 的 workspace。
- 未裁剪的团队全量状态文件。
- 可以绕过 backend 的调度文件或控制 socket。
- 宿主机 canonical repo 真实路径。
- Docker socket 或可间接控制宿主 Docker daemon 的 socket / credential。

Docker 网络要求：

- Backend 应在私有网络中以稳定服务名可访问，例如 `http://coordplane:8080`。
- Agent 容器只需要访问 backend 和必要的项目依赖网络。
- Agent 容器之间不需要互相直连。

持久化要求：

- `/home/agent` 或等价 volume 必须跨容器重建保留。
- CLI 原生 session cache、认证配置和 resume 所需状态保存在持久 home。
- 项目 workspace 可以按策略重新 clone 或缓存，但不能作为调度真相源。

路径边界要求：

- 容器内 Agent 只能看到容器路径或逻辑路径，例如 `/workspace/project`、`workspace_id`、`repo_id`。
- Backend / runner 可以保存宿主机路径，但这些字段属于控制面内部数据，不得出现在普通 Agent-facing response、skill 内容、release artifact 或 redacted inspect 中。
- Docker Agent 不得通过 `repo_path`、`workspace_root` 或类似字段请求 backend 访问任意宿主机路径。
- 如果必须支持 operator 以 host path 注册仓库，该入口必须是 operator/debug capability，并记录审计事件；普通 Agent 后续只能使用注册后的 `repo_id` 或 repo alias。

## 6. External runtime 需求

External runtime 用于开发和协议验证，不启动 Docker 容器，但必须与 Docker runtime 在服务语义上等价。

用途：

- 手动调用 backend 接口模拟 Agent 行为。
- 检查 prompt/context/resource 组装结果。
- 测试 assignment、mailbox、resume、evidence 信息流转。
- 快速复现协议错误。

约束：

- External Agent 也必须通过 token、lease、assignment scope 访问 backend。
- External runtime 不能拥有比 Docker runtime 更强的普通 Agent 权限。
- External runtime 的 `coordlink` 调用结果必须与 Docker runtime 一致。
- 允许 operator/debug 权限存在，但必须显式标识，不能混入 Agent 流程。

## 7. coordlink 定位

`coordlink` 是运行在 Agent 环境内的本地客户端。它不是 backend、不是 scheduler、不是 truth store。

它负责：

- 向 CLI Agent 提供稳定的 capability 调用入口。
- 帮助 Agent 读取已授权 skills。
- 将本地命令或 schema-derived tool adapter 调用转发到 CoordPlane backend。
- 提供本地命令 fallback，便于 CLI Agent 或人工调试。
- 注入 agent identity、runtime identity、lease scope、trace id。
- 原样返回 backend 的成功、拒绝和错误响应。

它不能：

- 保存 WorkContract、Assignment、Lease、Attempt 的 authoritative truth。
- 生成 backend id。
- 判断合同是否完成。
- 自动发布下一任务。
- 读取或写入 backend DB。
- 访问其他 Agent 的 workspace。
- 本地伪造成功。
- 维护一套独立于 backend 的 capability schema 或 skill 内容。

## 8. coordlink 入口能力

### 8.1 Capability command client

用于 CLI Agent 或人工调试直接调用 backend capability。

必须支持：

- `coordlink capability list`
- `coordlink call <capability>`
- `coordlink skill list`
- `coordlink skill read <skill>`

行为：

- capability schema 来自 backend。
- `coordlink capability list`、`coordlink skill list`、`coordlink skill read` 和 `coordlink call` 都必须携带 runtime token。
- `coordlink` 可以发送本地注入的 agent/runtime/lease 字段，但 backend 必须用 token 绑定校验这些字段；字段不一致时返回 rejected。
- skill 内容来自 backend skill registry。
- `coordlink call` 转发到 backend 对应 capability handler。
- `coordlink skill read` 转发到 backend skill handler。
- coordlink 不在本地复制业务 schema 或 skill 内容。

### 8.2 Schema-derived tool adapter bridge

用于把 backend capability 暴露成 CLI Agent 原生可调用工具。长期优先路径是：

- skills 只解释工作流、何时调用和失败后如何修正；
- backend capability registry 提供机器 schema；
- tool adapter 从 capability registry 派生工具 schema，让 CLI Agent 像调用真实工具一样调用 capability；
- `coordlink call` 保留为低级 fallback、调试入口和没有工具协议时的本地命令。

MCP 不是 CoordPlane 的设计前提；如果某个 CLI 需要 MCP，它只能作为 tool adapter 的一种兼容实现。

约束：

- tool adapter schema 必须从 backend capability registry 派生，不能手写第二套 schema。
- tool adapter call 必须转发到同一 backend capability handler。
- tool adapter 不能提供比 coordlink CLI 更多的普通 Agent 权限。
- 删除任一 tool adapter 不应影响 backend capability handler 或 `coordlink call`。

### 8.3 Local command examples

用于不方便通过 tool adapter 调用时的低级 fallback。

示例：

```bash
coordlink call contract.current
coordlink call contract.complete --input complete.json
coordlink skill read coordplane-service
coordlink mailbox list
coordlink version
```

约束：

- local command 与 tool adapter 调用必须共享同一 backend handler。
- 相同 identity、scope、input 下，tool adapter 和 local command 返回同一语义。
- backend rejected 时，本地命令退出非零并输出结构化 rejected response。
- local command 不能提供比 capability policy 更多的普通 Agent 权限。

### 8.4 Environment bootstrap

Runner 在启动 CLI Agent 前必须完成环境配置，Agent 不需要自己找服务地址或拼 token。

必须注入：

| 配置 | 用途 |
| --- | --- |
| COORDPLANE_BACKEND_URL | backend 地址 |
| COORDPLANE_AGENT_ID | 当前 Agent |
| COORDPLANE_RUNTIME_ID | 当前 runtime |
| COORDPLANE_TOKEN | 当前 Agent 访问 token |
| COORDPLANE_WORKSPACE | 逻辑 workspace 路径 |
| COORDPLANE_TRACE_ID | 追踪本次启动或调用 |

可按需注入：

- 当前 assignment id。
- 当前 lease id。
- 当前 attempt id。
- CLI backend 类型。

不得注入：

- backend DB path。
- host runtime root。
- 其他 Agent token。
- 其他 Agent workspace。
- 未裁剪团队全量状态。

## 9. Backend Capability API、Skills 和 Adapter 关系

CoordPlane 的正式机器协议是 backend capability API。Agent-facing 使用说明是 skills。HTTP、coordlink CLI、schema-derived tool adapter 都是 adapter，不是第二套协议。

必须遵守：

- Backend 定义唯一 capability schema 和 skill registry。
- tool adapter 的暴露项从 backend capability schema 派生。
- HTTP adapter 调用同一 service handler。
- coordlink tool adapter bridge 和 coordlink local command 调用同一 backend endpoint 或 handler。
- 新增 capability 时只新增 handler 和 schema 注册，不改主执行循环。
- 新增 skill 时只新增 skill 内容和绑定，不改 capability handler。

推荐 Agent-facing capability：

- `assignment.next`
- `assignment.watch`
- `contract.current`
- `contract.context`
- `contract.add`
- `contract.wait`
- `contract.complete`
- `communication.read`
- `message.send`
- `mailbox.list`
- `mailbox.get`
- `mailbox.resolve`
- `report.submit`
- `artifact.upload`
- `validation.assessment`
- `mailbox.watch`

推荐 skills/resources：

- `skill.coordplane-service`
- `skill.controlled-git`
- `skill.contract-delegation`
- `skill.validation-review`
- `contract.context`
- `mailbox.current`
- `thread.current`
- `evidence.summary`

## 10. CLI backend adapter

CLI backend adapter 是 Runner 内部组件，用于启动和恢复具体 CLI Agent。它与 coordlink 分离。

职责：

- 构造 CLI 启动命令。
- 配置 CLI 使用 coordlink、已授权 skills 和 schema-derived tool adapter。
- 配置工作目录和 home。
- 注入必要环境变量。
- 保存 CLI 原生 session id。
- 支持 resume。
- 在 adapter 能力允许时支持 same-turn steer。
- 捕获 transcript 和退出状态。

不负责：

- 实现任务状态机。
- 替 Agent 调用 `contract.complete`。
- 解析业务结论。
- 读取其他 Agent 信息。
- 不得向普通 Agent 暴露 `session.steer`、active turn id 修改或内部递送控制接口。

建议支持统一接口：

```text
Start(ctx, Runtime, Assignment, Lease) -> AttemptRoute
Resume(ctx, SessionRoute, MailboxItem) -> AttemptRoute
Steer(ctx, ActiveTurnRoute, DeliverySignal) -> SteerResult
Stop(ctx, AttemptRoute) -> Result
Inspect(ctx, SessionRoute) -> SessionInfo
```

`Steer` 是 Runner/backend 内部接口，不是 Agent-facing capability。普通 Agent 只能收到 signal，然后通过 tool adapter 或 `coordlink` 调用 mailbox / communication capability 读取完整内容。

## 11. 实时递送、steer 和 fallback resume

Backend 创建 MailboxItem 后，message delivery 模块应先尝试把轻量 DeliverySignal 递送到目标 Agent 的活跃 CLI turn。Runner 只负责按 CLI adapter 能力执行注入。

活跃 turn 流程：

```text
Backend 创建 mailbox item
Delivery service 找到 ActiveTurnRoute
Runner 调用 CLI backend adapter Steer
CLI Agent 在安全边界收到轻量 signal
Agent 调用 tool adapter 或 coordlink mailbox/communication capability 读取完整内容并处理
```

如果没有 active turn、CLI adapter 不支持 steer、或 steer 失败，才进入 fallback resume：

```text
MailboxItem 保持 pending
Runner 获取 fallback wake event
Runner 找到目标 Agent runtime
Runner 确认持久 home 和 session route
Runner 调用 CLI backend adapter Resume
CLI Agent 在同一 session 中收到 pending mailbox signal
Agent 调用 tool adapter 或 coordlink 读取 context 并处理
```

如果 CLI 支持插入消息，Runner 应选择安全插入点：

- 当前工具调用结束后。
- 当前 shell 命令结束后。
- 当前 assistant turn 结束后。

不能等整个合同完成后才处理反馈。

DeliverySignal 可以包含已授权 envelope 摘要或很短正文，帮助 Agent 判断是否需要立刻处理；不能包含未授权内容、长正文、完整合同详情、完整验证详情或 artifact 内容。权威内容应由 Agent 通过 tool adapter 或 `coordlink call mailbox.list/get`、`coordlink call communication.read` 获取；tool adapter 也只能转发到同一 backend capability。

容器被销毁后的要求：

- Backend 保存 session route。
- Runner 可重建容器。
- 持久 home 保留 CLI session cache。
- Resume 后 Agent 应看到同一会话历史和 pending mailbox signal；完整 feedback 仍通过 mailbox / communication capability 读取。

## 12. 错误响应

coordlink 必须原样返回 backend 的结构化错误。

后端不可达示例：

```json
{
  "ok": false,
  "status": "error",
  "error_code": "COORDPLANE_BACKEND_UNAVAILABLE",
  "message": "CoordPlane backend is not reachable",
  "retryable": true
}
```

权限不足示例：

```json
{
  "ok": false,
  "status": "rejected",
  "error_code": "UNAUTHORIZED_SCOPE",
  "message": "this agent cannot access the requested assignment",
  "retryable": false
}
```

要求：

- 不能吞掉 backend 错误。
- 不能把失败转换成空成功。
- retryable 必须清晰。
- local command 失败时使用非零退出码。

## 13. 安全和隔离

- Agent token 必须是短期或可撤销 token。
- 所有副作用调用必须带 trace id。
- Backend 按 token、lease、assignment 和 team policy 裁剪响应。
- Agent 不能通过修改环境变量伪造其他身份。
- coordlink 不能信任命令行传入的 agent id 覆盖 token 身份。
- Docker 容器默认最小权限运行。
- 容器内只挂载必要目录。
- Transcript 可以保存到 backend 指定位置，但不能作为 Agent 间共享文件通道。

## 14. 最小测试边界

### 14.1 coordlink 安装测试

- Docker image 内存在 `coordlink`。
- `coordlink version` 返回 client version、protocol version、backend compatibility。
- 没有另一个生产组件也叫 `coordlink` 却承担不同职责。

### 14.2 Capability / adapter 测试

- `coordlink capability list` 来自 backend。
- `coordlink skill list/read` 来自 backend。
- `coordlink call contract.current` 在 active lease 下返回当前合同。
- 无 active lease 时不返回全量团队状态。
- 普通 Agent 的 capability discovery 不包含 `session.steer`、`session.interrupt` 或 active turn 修改能力。
- tool adapter 的暴露列表必须由 capability policy 和 capability registry 派生。
- local command、schema tool adapter、可选 MCP 兼容 adapter 在同一 identity/scope/input 下返回同一语义。
- tool adapter schema 必须从 capability registry 派生，不能维护手写第二 schema。

### 14.3 Local command 测试

- `coordlink call contract.current` 和 tool adapter 调用返回同一语义。
- backend rejected 时，local command 非零退出并输出 rejected JSON。
- 后端不可达时返回 `COORDPLANE_BACKEND_UNAVAILABLE`。

### 14.4 Docker 隔离测试

- 容器内没有 DB path、runtime root、其他 Agent token。
- Agent A 容器无法读取 Agent B workspace。
- Agent A 无法通过 coordlink 读取 Agent B mailbox。
- 容器重建后可用持久 home resume 同一 CLI session。

### 14.5 External 等价测试

- External runtime 使用同一 token/scope 调用 backend。
- 同一接口在 external 和 Docker 下返回同一结构化语义。
- External debug 权限必须显式标识。

### 14.6 Runner 测试

- Runner 启动前完成 backend URL、token、workspace、coordlink 配置。
- Runner 在 adapter 支持时能对 active turn 投递 DeliverySignal。
- Runner 能根据 mailbox item resume 原 session route。
- Runner 不会自动完成合同或发布下一任务。
- CLI backend adapter 可替换，替换时不影响 coordlink 协议。
- fake external adapter 可捕获 steer payload，用于不启动真实 CLI 的协议测试。

## 15. 开发落地建议

Go 项目中建议按以下包拆分：

```text
cmd/coordplane
cmd/coordlink
internal/backend/service
internal/backend/capability
internal/backend/skills
internal/backend/http
internal/backend/store
internal/adapters/tools
internal/runtime/runner
internal/runtime/docker
internal/runtime/external
internal/runtime/cliadapter
internal/coordlink/client
internal/coordlink/adapterbridge
internal/coordlink/cmdadapter
```

注册式设计要求：

- Capability 用列表注册。
- Skills 用列表或 store 绑定注册。
- CLI backend adapter 用列表或 map 注册。
- Runtime backend 用列表或 map 注册。
- Validation/check steps 用列表注册。
- 删除一个 adapter 或 tool 时，只移除注册项，不改主循环。

## 16. 设计结论

CoordPlane 的 runtime 层应保持简单：

```text
Backend 保存真相
Runner 管生命周期
Runtime 提供隔离
coordlink 提供本地访问
CLI Agent 负责决策
```

`coordlink` 是必要的，因为容器内 Agent 需要一个稳定、本地、低摩擦的服务入口；但它必须只是 adapter，不能演变成第二个后台服务。
