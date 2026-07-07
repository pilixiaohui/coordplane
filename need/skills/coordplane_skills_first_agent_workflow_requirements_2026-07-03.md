# CoordPlane Skills-First Agent 工作流需求

本文是 CoordPlane skills 模块的独立需求说明。它定义 Agent 如何通过渐进式披露的 skills 理解 CoordPlane 工作流，并通过 schema-derived tool adapter 或 `coordlink` 调用 backend capability。

## 1. 目标

CoordPlane 不应把所有接口说明、全部 capability schema、团队状态和项目上下文一次性塞进 Agent prompt。Agent 需要的是：

- 启动时知道自己有哪些 skills。
- 需要某类操作时能读取对应 skill。
- skill 告诉 Agent 何时调用 capability、调用后会发生什么、失败时如何修正。
- 具体机器输入输出由 backend capability registry、schema-derived tool adapter 和 rejected response 保证。

Skills 是 Agent-facing 工作流层；capability API 是机器接口；TeamConfig 决定哪些 Agent 可以看到哪些 skills。

## 2. 非目标

- Skill 不保存任务状态、运行状态或对话历史。
- Skill 不作为第二套接口 schema。
- Skill 不内置项目业务语义。
- Skill 不决定权限；权限由 capability policy 决定。
- Skill 不暴露 secret、数据库路径、runner 内部接口或其他 Agent 私有状态。

## 3. 核心对象

| 对象 | 含义 |
| --- | --- |
| SkillPackage | 一个可注册的 skill，例如 `coordplane-service` |
| SkillVersion | skill 内容版本，运行中的 attempt 必须记录使用版本 |
| SkillBinding | TeamConfig 中把 skill 绑定到 agent、role 或 contract type 的规则 |
| CapabilityRef | skill 中引用的 capability 名，例如 `contract.add` |
| SkillDocument | Agent 可读取的正文，说明工作流、注意事项和错误修正 |
| AgentCommunicationEnvelope | Agent 间消息、任务、结果、修复和预算提醒的统一通信对象 |
| ToolAdapterSchema | 从 backend capability registry 派生的 Agent 工具 schema |

## 4. Skill 内容结构

每个 skill 应包含以下稳定章节：

1. 适用场景：Agent 什么时候应该读取该 skill。
2. 允许做什么：该 skill 关联哪些 capability 类别。
3. 不应该做什么：常见误用和边界。
4. 推荐工作流：按目标组织的步骤，不是硬编码唯一顺序。
5. 失败反馈处理：收到 rejected response 时如何修正。
6. 最小调用示例：优先展示工具调用语义，再展示 `coordlink call <capability>` fallback 形状，但不复制完整 schema。
7. 证据要求：完成或提交时需要哪些通用证据。

## 5. 内置 skills

第一阶段内置以下 skills，但必须通过注册表启用，不能在主流程硬编码：

| Skill | 用途 |
| --- | --- |
| `coordplane-service` | 读取当前任务、communication envelope、mailbox、thread，提交完成、回复和 follow-up action |
| `contract-delegation` | 派发可追责任务、等待子任务、处理子任务反馈 |
| `controlled-git` | 通过 CoordPlane 执行 sync、diff、commit、merge、resolve、rollback |
| `artifact-sharing` | 上传、下载和引用非代码交付物 |
| `validation-review` | 提交验证评估、说明证据和失败原因 |
| `runtime-troubleshooting` | 处理 backend 不可达、权限拒绝、workspace 未准备好等运行问题 |

删除某个内置 skill 应只删除注册项或 TeamConfig 绑定，不应修改 capability handler 或 runner 主流程。

## 6. Agent Bootstrap

Agent 启动 prompt 只应包含：

- 当前 Agent 身份、角色提示词摘要和当前 assignment 摘要。
- 可用 skill 列表和读取方式。
- `coordplane-service` 作为最小入口 skill。
- 明确要求：需要上下文时先读取对应 skill 或调用 capability，不要猜测后台状态。
- 明确说明：若 CLI 暴露了 CoordPlane 工具，优先使用工具；没有工具时使用 `coordlink call` fallback。

Agent 启动 prompt 不应包含：

- 全部 capability schema。
- 全团队任务列表。
- 其他 Agent 私有 mailbox。
- secret、DB path、runtime root、host path。
- adapter schema 或 adapter-specific 调用细节。

## 7. Progressive Disclosure

Skill 读取必须支持渐进式披露：

- `coordlink skill list` 返回当前 scope 可用 skill 摘要。
- `coordlink skill read <name>` 返回当前授权版本正文。
- skill 可以引用另一个 skill，但引用应短且明确。
- Agent 不需要读取无关 skill 才能完成当前任务。

Backend 返回 skill 时必须按 token、agent、team、contract 和 TeamConfig version 裁剪。

## 8. Capability 关系

Skill 可以引用 capability 名，但不复制完整机器 schema。

示例：

```text
读取当前任务：调用 contract.current；fallback 为 coordlink call contract.current
读取上下文：调用 contract.context；fallback 为 coordlink call contract.context
读取通信：调用 communication.read；fallback 为 coordlink call communication.read
提交完成：调用 contract.complete；fallback 为 coordlink call contract.complete --input complete.json
派发任务：调用 contract.add；fallback 为 coordlink call contract.add --input add.json
发送普通消息：调用 message.send；fallback 为 coordlink call message.send --input message.json
读取 mailbox：调用 mailbox.list/mailbox.get；fallback 为 coordlink call mailbox.list
```

要求：

- capability schema 来自 backend capability registry。
- tool adapter schema 来自同一 capability registry，不能由 skill 文档复制或手写。
- rejected response 必须足够明确，让 Agent 在同一会话中修正。
- Skill 中不得要求 Agent 直接读后台文件。

## 9. Communication Envelope 工作流说明

`coordplane-service` skill 必须解释统一通信对象，但不复制完整机器 schema。

稳定语义：

- `message.send` 创建 `kind=message` 的 `AgentCommunicationEnvelope`，不改变合同状态。
- `contract.add` 创建 `kind=task` 的 `AgentCommunicationEnvelope`，并创建 WorkContract、Assignment 和授权 mailbox。
- 子合同完成后，backend 创建 `kind=result` 的 envelope 和 mailbox，发布者在原会话中读取后自行决策下一步。
- validation 或协议失败时，backend 创建 `kind=repair` 的 envelope；Agent 应读取 rejected response 或 mailbox 后修正再提交。
- 合同预算耗尽但仍需决策时，backend 创建 `kind=budget_attention` 的 envelope，并按 TeamConfig policy 唤醒对应 Agent。
- `trigger_turn=true` 只表示需要启动、resume 或 same-turn steer；它不会完成任务，也不会替 Agent 自动派发任务。
- DeliverySignal 中的摘要只是辅助提醒；权威完整上下文必须通过 `mailbox.get`、`communication.read` 或 `contract.context` 读取。

## 10. 配置和版本

- SkillPackage 必须有稳定 name。
- 每次内容变更产生 version。
- TeamConfig 绑定具体 skill name，可选择 pin version 或使用 latest policy。
- Attempt 启动时记录 resolved skill versions。
- Inspect API 可查看某次 attempt 使用了哪些 skill version，但不能暴露 secret。

## 11. 测试边界

必须覆盖：

- Agent bootstrap prompt 只列出 skill 摘要，不内联全量 capability schema。
- `skill.list/read` 按 TeamConfig 和 policy 裁剪。
- 删除 skill 绑定只影响 discovery，不影响 capability handler。
- Skill 文档不包含 secret、DB path、runtime root、host path。
- tool adapter 与 skill 系统解耦；禁用 adapter 不影响 `coordlink skill read`。
- tool adapter schema 从 capability registry 派生，不由 skill 文档维护第二 schema。
- `coordplane-service` 正确说明 message、task、result、repair、budget attention envelope 的差异。
- Rejected response 中的修正提示不依赖项目特定语义。

## 12. 设计结论

CoordPlane 的 Agent-facing 层应以 skills 为核心。Agent 通过 skills 学习工作流，通过 schema-derived tool adapter 或 `coordlink` 调用 capability，通过 backend rejected response 在同一会话中修正错误。skills 不承担机器 schema，工具 schema 来自 backend capability registry。
