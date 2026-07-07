# CoordPlane TeamConfig 需求

本文是 CoordPlane TeamConfig 模块的独立需求说明。它定义一个多 Agent 团队如何被配置，并确保团队角色、skills、runtime 和 capability policy 与具体项目需求解耦。

## 1. 目标

TeamConfig 是单用户 MVP 的团队配置入口。它回答：

- 有哪些 Agent。
- 每个 Agent 的通用角色提示词是什么。
- 每个 Agent 使用哪些 skills。
- 每个 Agent 可以调用哪些 capability。
- 每个 Agent 运行在 Docker 还是 external runtime。
- 使用哪个 CLI backend，例如 codex 或 claude。
- 什么样的合同算终止验收合同。

TeamConfig 不回答具体项目要做什么。项目需求、验收标准和资料应进入 root contract、project context 或 Agent 维护的项目记忆，不应写进通用团队角色。

## 2. 非目标

- 不硬编码 assistant、developer、reviewer 等角色名的特殊行为。
- 不包含项目特定检查，例如“必须有 15 章”或“必须检查某个文件名”。
- 不保存实时任务状态、mailbox、lease 或 attempt。
- 不保存 secret 明文。
- 不替 Agent 自动决策下一步任务。

## 3. 最小配置结构

建议配置结构：

```yaml
team_id: default-go-team
version: 1
agents:
  - id: coordinator
    role_prompt_ref: roles/coordinator.md
    runtime_profile: docker-default
    cli_backend: codex
    skills:
      - coordplane-service
      - contract-delegation
    capabilities:
      - contract.current
      - contract.context
      - contract.add
      - contract.wait
      - contract.complete
      - communication.read
      - mailbox.list
      - mailbox.get
      - mailbox.resolve
      - message.send
  - id: builder
    role_prompt_ref: roles/builder.md
    runtime_profile: docker-default
    cli_backend: claude
    skills:
      - coordplane-service
      - controlled-git
      - artifact-sharing
    capabilities:
      - contract.current
      - contract.context
      - communication.read
      - contract.complete
      - command.run
      - git.status
      - git.diff
      - git.commit
      - artifact.upload
  - id: validator
    role_prompt_ref: roles/validator.md
    runtime_profile: docker-default
    cli_backend: codex
    skills:
      - coordplane-service
      - validation-review
    capabilities:
      - contract.current
      - contract.context
      - communication.read
      - validation.assessment
      - contract.complete
      - artifact.download

communication:
  allow_direct_message: true
  allow_followup_task: true
  task_requires_contract: true
  default_trigger_turn:
    message: false
    followup: true
    task: true
    result: false
    repair: true
    budget_attention: true

runtime_profiles:
  docker-default:
    kind: docker
    image: coordplane-agent:latest
    workspace_mode: private_clone
  external-debug:
    kind: external
    workspace_mode: host_path

termination:
  terminal_contract_type: final_acceptance
  accepted_by_capability: validation.assessment
```

字段名可以在实现中调整，但语义边界必须保留。

## 4. 角色提示词

角色提示词是团队通用能力说明，不是项目说明。

应该包含：

- 该角色负责的通用职责。
- 常用 skills。
- 交付风格和协作边界。
- 何时派发任务、何时等待反馈、何时请求验证。

不应包含：

- 具体项目需求。
- 特定仓库文件名。
- 特定业务验收条目。
- 后台 DB、runtime root、secret 或其他 Agent 私有路径。

## 5. Project Context 入口

用户提供的需求文档、产品说明、项目书和验收目标应进入项目上下文，而不是 TeamConfig。

第一阶段可以提供：

- root contract context。
- project memory document。
- artifact/document upload。
- coordinator 维护的 project brief。

TeamConfig 只绑定“谁有资格维护/读取这些上下文”的 capability policy。

## 6. Capability Policy

Policy 应从 TeamConfig 派生默认授权。

要求：

- Agent 只能 discovery 自己授权的 capability。
- Runner/internal capability 不暴露给普通 Agent。
- 验证、终止、代码操作权限都由配置声明，不由 backend 硬编码角色名。
- Docker 和 external runtime 使用同一 policy 语义。
- Policy response 必须说明拒绝原因和可修正方式。

## 7. Communication Policy

通信策略从 TeamConfig 派生，不由 backend 硬编码角色或自然语言含义。

必须能表达：

- 是否允许普通 direct message。
- 是否允许 follow-up task。
- 普通消息是否必须转为合同。
- 不同 envelope kind 的默认 `trigger_turn`。
- 哪些 Agent、role 或 contract type 可以向哪些目标发送 message、task、result、repair。

要求：

- `message.send` 的默认行为由 communication policy 决定；backend 不根据“这段话听起来像任务”来拒绝或强制改为合同。
- `contract.add` 是创建可追责工作合同的唯一入口；当调用方明确 `kind=task`、`intent=task_request` 或 `requires_contract=true` 时，才要求使用该入口。
- `trigger_turn=true` 只表示需要唤醒、resume 或 same-turn steer，不代表完成、验收或自动派发下一任务。
- Docker runtime 和 external runtime 使用同一 communication policy。
- policy 拒绝必须返回可修复 rejected response，不得静默丢消息。

## 8. Skill Binding

Skill 绑定规则：

- Agent 启动时只能看到绑定给自己的 skill。
- Contract 可以追加临时 skill requirement，但必须经过 policy。
- 禁用 skill 后，不应影响 capability handler，只影响 discovery 和 prompt composition。
- Attempt 必须记录启动时 resolved skill versions。

## 9. Runtime 和 CLI

TeamConfig 只声明 runtime profile 和 CLI backend，不直接拼接启动命令。

Runner 负责：

- 解析 runtime profile。
- 注入 `coordlink` 配置。
- 注入授权 token。
- 启动或恢复 CLI backend。
- 记录 session route。

新增 runtime 或 CLI backend 应只添加注册项和 adapter，不改 TeamConfig 主解析流程。

## 10. 终止验收

终止验收是团队配置，不是 backend 固定语义。

TeamConfig 至少应能声明：

- 哪类 contract 是 terminal acceptance。
- 哪个 capability 可以提交验收结果，例如 `validation.assessment`。
- 合同预算耗尽且最后一个合同不是终止验收合同时，通知哪个发布者或 coordinator 决策下一步。

Backend 只验证“是否满足 TeamConfig 声明的终止条件”，不判断项目语义是否正确。

## 11. 版本和迁移

- TeamConfig 必须有 version。
- 新 attempt 使用当前 active version。
- 已运行 attempt 记录启动时 version，resume 时默认沿用原 version，除非 operator 显式迁移。
- 配置变更必须产生审计事件。

## 12. 测试边界

必须覆盖：

- 加载 TeamConfig 后进入 canonical store。
- capability discovery 来自 TeamConfig policy，不来自硬编码角色名。
- Agent bootstrap prompt 组合 role prompt + skill 摘要 + assignment context，不包含项目全量状态。
- Docker runtime 和 external runtime 使用同一授权结果。
- 禁用某 skill 后该 Agent 不再 discovery 它。
- communication policy 控制 trigger_turn 默认值和 direct message / follow-up task 权限。
- backend 不使用自然语言检测来判断消息是否必须变成合同。
- 终止验收规则来自 TeamConfig，不来自 backend 角色判断。
- role prompt 不包含项目特定业务语义。
- secret 只通过 SecretProvider 注入 runtime，不进入 TeamConfig 明文。

## 13. 设计结论

TeamConfig 是通用团队能力配置，Project Context 是具体项目需求入口。CoordPlane backend 依据 TeamConfig 裁剪 prompt、skills、capability、communication policy 和 runtime，不硬编码角色语义，也不把项目需求写进团队角色。
