# CoordPlane 行业标准协议设计（ACP / A2A / AG-UI / MCP）

状态：Design doc（非权威需求；权威边界见 `need/README.md` §6.1、`need/runtime.md` §7.4、`need/core.md` §14）
日期：2026-08-05
依据：COD-68 协议调研（2026-08 时点文献 + 架构对照）

## 1. 决策摘要

| 协议 | 层次 | 决策 | 说明 |
| --- | --- | --- | --- |
| ACP | 客户端↔agent CLI | **演进目标**（adapter 层平移） | 1.0 稳定 + Claude Code 原生/bridge 成熟后实现 ACP client adapter |
| A2A | agent↔agent | **仅对外出口**（未来能力） | Boss 面 AgentCard + tasks/send；内部禁止直连 |
| AG-UI | agent↔UI | **前端参考** | Run 进度流事件词汇映射 |
| MCP | 模型↔工具 | **不建设平台** | agent 容器内自连属 agent 行为 |
| AGNTCY/ANP | 基础设施 | **不进第一版** | 单 Daemon 定位不匹配，长期观望 |

## 2. ACP：adapter 层演进设计

### 2.1 现状与动机

第一版 adapter（`internal/adapter/`）解析 Claude Code 私有 JSONL 事件流（`session_id`/`type`/`init`/`assistant`/`tool_use`），是单一 provider 绑定。ACP 提供标准化的 session/prompt/update 接口，采用后可一套代码支持任何 ACP agent（Gemini CLI、Copilot CLI 已原生支持 ACP）。

### 2.2 接口映射（正式合同见 need/runtime.md §7.4）

| adapter 接口 | ACP 方法/事件 | 迁移点 |
| --- | --- | --- |
| BuildStartCommand | session/new | 启动 ACP server（agent CLI 包装）+ 新建 session |
| bootstrap 输入 | session/prompt | 一次性投影作为首轮 prompt |
| BuildResumeCommand | session/load | native session ID 恢复 |
| BuildInjectInput | session/prompt（进行中会话） | 仍受 Run token/generation 校验 |
| ParseEvent | session/update 事件流 | 私有 JSONL → 标准化事件 |
| ResumeCompatible | 协议版本 + agent 能力声明 | 兼容性判断来源 |

### 2.3 采用前置条件（全部满足才可实现）

1. ACP 发布 1.0（当前 v0.10.8 pre-1.0，协议可能变动，实现时钉版本）。
2. Claude Code 原生支持 ACP，或经审核的 bridge（如 Zed 的 `@agentclientprotocol/claude-agent-acp`）在生产环境稳定。
3. ACP conformance 测试纳入本仓库验收：session 生命周期（new/prompt/update/cancel）、流式事件完整性、resume、运行中输入。
4. provider 计费/凭据路径与现有 `provider_env_allowlist` 兼容（注意 Anthropic 对 ACP/Agent SDK/`claude -p` 的独立 Agent SDK credit pool 计费）。

### 2.4 实现约束

- 新增 `internal/adapter/acp.go` 静态注册，主循环零改动（Build to Delete 合同）。
- 保留 one-shot 执行模型、三类事实分离（live / resumable / acknowledged）、resume 即新 Run、Docker 生命周期由 Executor 统一拥有。
- 删除私有 adapter 时：同一变更删除其 fixture 与正向测试，保留防止旧路径回归的负向 guard。

## 3. A2A：对外互操作出口设计（未来能力）

### 3.1 边界（need/core.md §14）

- 对外 `tasks/send`、`tasks/get`、`tasks/cancel` 全部映射到 CoordPlane 的 Task/Message 语义，不创建第二套任务对象或状态机。
- 对外身份/凭据独立于内部 Run token/generation fence。
- 对外委派进来的任务与内部 Task 同表同规则（scope、幂等、Event 同事务）。

### 3.2 状态映射

| A2A Task 状态 | CoordPlane Task 状态 |
| --- | --- |
| SUBMITTED | queued |
| WORKING | running |
| INPUT_REQUIRED | waiting |
| COMPLETED | completed |
| FAILED | failed |
| CANCELED | cancelled |
| REJECTED | （领域拒绝 → 422 语义，不产生 Task 终态） |

### 3.3 AgentCard（静态能力声明）

- 路径：`/.well-known/agent-card.json`（RFC 8615），仅当 A2A 出口启用时由 Daemon 静态生成。
- 内容：`name/description/url`（A2A 端点）、`protocolVersion`、`skills[]`（只声明静态能力，如 `task.dispatch`、`task.query`，不声明动态技能）、`securitySchemes`（Bearer 或 mTLS，独立凭据）。
- 不用于内部动态路由；内部仍是 daemon 静态协调。

### 3.4 明确禁止

- 容器网络内不允许 agent↔agent 直连（A2A 或其他）；A2A 只存在于 Daemon 的 Boss 面出口。
- 不允许外部 A2A 请求绕过 operator 鉴权直接操作内部对象。

## 4. AG-UI：前端事件流映射（参考）

Web 前端（`web_addr` SPA）实时展示 Run 进度时，事件词汇映射：

| AG-UI 事件 | CoordPlane 数据源 | 说明 |
| --- | --- | --- |
| RunStart | run.created / run.active | Run 启动 |
| TextMessageStart/Content/End | Message created/delivered | 对话消息流 |
| ToolCallStart/Result | run 日志（stdout 轮转文件） | 工具调用展示（受 redaction 边界约束） |
| RunComplete / Error | run.exited / task.failed | Run 终态 |

事件传输与增量格式由前端实现选择，不与 AG-UI 规范固定绑定：后端 `events tail` 是事实源，前端适配层负责投影；若前端需要增量，可参考 JSON Patch 编码 `STATE_DELTA.delta` 作为可选增量格式，不是对 AG-UI 传输合同的强制兼容，也不改变后端事件模型。

## 5. 明确不采用

| 协议/能力 | 理由 | 需求依据 |
| --- | --- | --- |
| MCP server 平台 | 非目标：通用 tool adapter/MCP server/plugin 平台 | need/README.md §6 |
| 内部 A2A 直连 | 破坏 Daemon 单一事实源/审计/隔离 | need/README.md §6、core.md §14、INV-19 |
| AGNTCY（ADS/Identity/SLIM） | 单 Daemon、本地优先；跨组织基础设施不匹配 | need/README.md §6 |
| ANP（DID/去中心化） | 同上 | need/README.md §6 |

## 6. 协议跟踪项

| 跟踪项 | 当前状态（2026-08） | 触发动作 |
| --- | --- | --- |
| ACP 1.0 | v0.10.8 pre-1.0 | 发布后评估 §2.3 前置条件 |
| Claude Code 原生 ACP | 非原生（Zed bridge） | 原生/稳定 bridge 后评估 |
| A2A 1.x | v1.0（LF，150+ 组织） | 用户确认对外互操作需求后走需求修订 |
| MCP 大版本 | 2026-07-28 无状态化 | 仅生态参照，无动作 |
| AG-UI | 发展中 | 前端实时流事件词汇参考；传输与增量格式由前端实现选择，不绑定 AG-UI 传输 |

## 7. 与现有文档的引用关系

- 权威定位：`need/README.md` §6.1（协议演进合同）
- Runtime 合同：`need/runtime.md` §7.4（ACP 演进合同）
- 未来能力边界：`need/core.md` §14（A2A 出口映射、内部直连禁令）
- 验收约束：`need/acceptance.md` INV-18/INV-19
