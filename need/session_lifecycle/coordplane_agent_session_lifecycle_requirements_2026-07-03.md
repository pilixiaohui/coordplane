# CoordPlane Agent 会话预处理与后处理功能需求

本文是 CoordPlane Agent session lifecycle 模块的独立需求说明。它定义一个 CLI Agent 会话启动前、运行中、结束后分别由 CoordPlane backend、runner、runtime 和 coordlink 承担哪些职责。

## 1. 背景和目标

CoordPlane 把每次 CLI Agent 运行视为一个受控执行窗口。这个窗口不能只是“启动进程然后等退出”，因为多 Agent 协作需要：

- 准备正确的隔离 workspace。
- 在任务真正可执行后才标记 running。
- 在会话运行中尽早保存 CLI native session id 和 workdir。
- 让 Agent 在同一会话内收到工具调用结果、冲突、拒绝和 mailbox 更新信号。
- 会话异常退出后能恢复或重试。
- 清理资源时不能误删正在使用的 workspace、volume 或 repo cache。

目标模型：

```text
会话前：准备真实可运行环境，不替 Agent 决策
会话中：Agent 主动调用 capability，CoordPlane 受控执行并即时反馈
会话后：记录事实、释放或保留资源、创建协议要求的后续事项
```

## 2. 术语

| 名称 | 定义 |
| --- | --- |
| Session lifecycle | 一次 CLI Agent 会话从准备到终态收敛的完整生命周期 |
| Attempt | 一次真实 CLI Agent 运行记录 |
| Prepare lease | 会话准备阶段持有的短期租约，用于防止长时间准备被误判为失联 |
| Active guard | 标记 workspace/env root 正在被使用，防止 GC 或 cleanup 误删 |
| Session route | resume 同一 CLI 会话所需的持久路由 |
| session.pin | 会话中尽早保存 session route、workdir、runtime、attempt 的操作 |
| Terminal report | 会话结束时上报 completed、waiting、failed、interrupted 等终态 |
| DeliverySignal | message delivery 模块注入到活跃 turn 的轻量 mailbox 更新信号 |
| AgentCommunicationEnvelope | Agent 间消息、任务、结果、修复和预算提醒的统一通信对象 |

## 3. 非目标

Session lifecycle 模块不负责：

- 判断具体业务任务是否正确。
- 自动替 Agent 创建下一任务。
- 自动替 Agent 合并代码或解决冲突。
- 在 Agent 不知情的情况下修改 canonical branch。
- 把所有上下文一次性塞进 prompt。

它负责的是执行窗口的真实性、一致性、可恢复性和资源边界。

## 4. 会话状态模型

Session route 绑定 Agent 的 CLI 会话和可恢复执行上下文，不绑定某个一次性合同进程。一个 Agent 在同一 CLI 会话中可以处理当前合同、子任务反馈、修复请求、后续派发和重新打开的旧合同。

合同可以 completed、waiting、reopened 或再次进入可处理状态，只要 TeamConfig policy 和 lease scope 允许，都必须能 resume 到同一 session route。合同完成不代表会话历史作废，也不代表后续反馈不能唤醒同一 Agent。

Attempt 状态必须比普通任务状态更细：

```text
created
-> preparing
-> ready_to_launch
-> running
-> waiting
-> completed
-> failed
-> interrupted
-> expired
```

含义：

- `created`：backend 已创建 attempt，但 runner 尚未准备环境。
- `preparing`：runner 正在准备 workspace、toolchain、coordlink、CLI 配置。
- `ready_to_launch`：环境已准备好，尚未启动 CLI Agent。
- `running`：CLI Agent 已启动，且 workspace 已真实存在。
- `waiting`：Agent 显式等待子任务、反馈或外部输入，session route 应保留。
- `completed`：Agent 已完成并通过协议终态上报。
- `failed`：运行失败，可能可重试。
- `interrupted`：被取消、runtime 退出或 operator 中断。
- `expired`：lease 或 attempt 超时，等待 recovery 处理。

禁止状态跳跃：

```text
created -> running
assigned -> running
preparing -> completed
```

workspace、coordlink、token、session route candidate 未准备好前，不能把 attempt 标记为 `running`。

## 5. 会话前预处理

### 5.1 Backend 职责

Backend 在会话启动前必须：

- 根据 assignment、mailbox、AgentCommunicationEnvelope 和 contract feedback 选择目标 Agent。
- 创建 Attempt。
- 创建或续期 Lease。
- 创建 Prepare lease。
- 生成短期 Agent token。
- 确定 runtime kind：Docker 或 external。
- 确定是否新会话还是 resume 原 session。
- 保存 session route candidate。
- 返回最小启动上下文引用，而不是全量团队状态。

Backend 不应该：

- 自动完成合同。
- 自动合并代码。
- 自动决定下一任务交给谁。
- 在没有 Agent 调用的情况下推进业务状态。

### 5.2 Runner 职责

Runner 在会话启动前必须：

- 创建或恢复 runtime。
- 准备私有 workspace。
- 准备持久 home。
- 安装或确认 `coordlink` 可用。
- 配置 coordlink、已授权 skills 和 schema-derived tool adapter。
- 注入 backend URL、token、agent id、runtime id、workspace id、trace id。
- 执行 toolchain ready 检查。
- 创建 active guard，防止 cleanup/GC 删除当前 workspace。
- 确认 CLI backend adapter 可启动。

Runner 不应该：

- 代替 Agent 调用 `contract.complete`。
- 代替 Agent 调用 `git.merge_apply`。
- 根据业务内容自动派发子任务。

### 5.3 Runtime 职责

Docker runtime 必须保证：

- 每个 Agent 独立容器。
- 每个 Agent 私有 workspace。
- 每个 Agent 持久 home。
- 容器内只能通过 `coordlink` 或 schema-derived tool adapter 访问 CoordPlane capability。

External runtime 必须保证：

- 使用同一 backend protocol。
- 普通 Agent 权限不高于 Docker runtime。
- debug/operator 权限必须显式标识。

## 6. 准备阶段不变量

- `running` 只能在 workspace 已落盘、coordlink 已可用、CLI 即将启动或已启动后设置。
- 长时间准备必须刷新 prepare lease。
- 准备失败必须进入 `failed` 或 `preparing_failed` 等可审计状态。
- 准备失败不能留下 active lease 阻塞队列。
- 准备失败不能留下可被误认为 running 的 attempt。
- active guard 必须覆盖 fresh prepare 和 resume prior workdir 两类路径。

## 7. 会话中处理

### 7.1 session.pin

CLI Agent 启动后，一旦 runner 获得以下任意信息，必须尽快调用 `session.pin`：

- CLI native session id。
- workdir。
- runtime id。
- attempt id。
- resume route。

`session.pin` 的目的：

- runner 崩溃后不丢 resume 指针。
- 容器重建后可恢复同一 CLI session。
- mailbox feedback 能找到原会话。
- message delivery 能找到 active turn 或 fallback resume route。
- 后续 GitOperation 能绑定正确 workspace。

不能只在会话结束时保存 session id。

### 7.2 Progress 和 heartbeat

会话中必须周期性上报：

- attempt heartbeat。
- runner heartbeat。
- runtime heartbeat。
- 可选 progress summary。
- 当前工具调用或 GitOperation 状态。

Heartbeat 不是业务完成信号，只表示执行窗口仍活着。

### 7.3 Agent 工具调用

Agent 在会话中根据 skills 指引，通过 `coordlink` 主动调用 capability：

- `contract.context`
- `contract.add`
- `contract.wait`
- `contract.complete`
- `message.send`
- `report.submit`
- `artifact.upload`
- `validation.assessment`
- `workspace.status`
- `workspace.sync`
- `git.status`
- `git.commit`
- `git.merge_preview`
- `git.merge_apply`
- `git.conflicts`
- `git.resolve`
- `git.rollback`

Backend 必须把 accepted、rejected、conflict、failed 等结果即时返回给当前会话。

### 7.4 会话内反馈和实时消息递送

如果后台发现：

- 缺少证据。
- 没有权限。
- workspace 基线过旧。
- Git 冲突。
- 合同状态不允许完成。
- 子任务完成反馈到达。

应优先通过当前工具调用返回结构化 response。异步反馈必须先创建 MailboxItem，再交给 message delivery 模块递送。

活跃 CLI turn 的异步反馈流程：

```text
Backend 创建 AgentCommunicationEnvelope 和 MailboxItem
message delivery 构造轻量 DeliverySignal
Runner/CLI adapter 在安全插入点执行 same-turn steer
Agent 收到 signal 后调用 tool adapter 或 coordlink mailbox/communication capability 读取完整内容
Agent 处理后调用 message.send / contract.add / evidence 接口 / mailbox.resolve
```

不活跃、不可 steer 或 steer 失败时：

```text
MailboxItem 保持 pending
Runner 根据 session route fallback resume 或启动会话
启动 prompt 只提示有 pending mailbox
Agent 仍通过 mailbox / communication capability 读取完整内容
```

安全插入点：

- 当前工具调用结束后。
- 当前 shell 命令结束后。
- 当前 assistant turn 结束后。

不能等整个合同完成后才让 Agent 知道错误。

DeliverySignal 是实时性优化，不是正确性前提。正确性来自 durable AgentCommunicationEnvelope、MailboxItem、session route 和 fallback resume。若 CLI final-answer 或 turn 关闭边界已经错过安全插入点，delivery service 应保留 pending mailbox 并触发下一轮 resume。

DeliverySignal 可以包含已授权 envelope 摘要或很短正文，帮助 Agent 判断是否需要立刻读取；不能包含未授权内容、长正文、完整合同正文、完整验证详情或 artifact 内容。

resume 去重要求：

- 同一 session route 可以被多个不同 mailbox 唤醒。
- 重复处理同一个 mailbox 的 resume request 可以幂等跳过。
- 不同 mailbox 不得因为该 route 曾经发生过 `session.resumed` 而被跳过。
- 如果实现采用 coalescing，resume prompt 或 signal payload 必须列出被合并的 mailbox id 集合，并记录审计事件。

active turn 真实性要求：

- `session_routes.state=active` 只能表示存在可递送或可恢复的 route，不能单独证明 CLI 进程当前仍在运行。
- 对于同步 `--print` / one-shot command CLI，CLI 子进程退出后不得继续作为 same-turn steer 目标；后续异步反馈必须走 fallback resume。
- Runner/adapter 必须记录 CLI session start/resume/exit 事实，delivery service 判断 same-turn steer 时必须结合 adapter capability 和 live turn 状态。

## 8. 会话后处理

会话结束后，CoordPlane 必须做事实收敛和资源处理。

### 8.1 Terminal report

Runner 必须提交 terminal report：

| 状态 | 含义 |
| --- | --- |
| completed | Agent 明确完成当前会话，并且必要协议操作已成功 |
| waiting | Agent 调用了 `contract.wait` 或等待 mailbox/human/input |
| failed | CLI 或工具执行失败 |
| interrupted | 被取消、runtime 退出或 operator 中断 |
| expired | 超时 |

Terminal report 必须具备重试和幂等：

- 使用 idempotency key。
- 网络失败可重试。
- 重复 terminal report 不应产生重复副作用。

### 8.2 Lease 处理

会话后根据状态处理 lease：

- `completed`：释放 lease。
- `waiting`：assignment 进入 waiting，保留 session route。
- `failed`：释放或转入 retry queue。
- `interrupted`：释放或保留给 recovery。
- `expired`：由 recoverer 判定。

### 8.3 Session route 保留

以下情况必须保留 session route：

- contract.wait。
- 子任务完成后要反馈给发布者。
- validation fail 后要恢复原负责 Agent。
- Git conflict 需要同一会话继续处理。
- 会话中途失败但 CLI native session 已存在。

### 8.4 协议副作用

会话后可以做的机械协议副作用：

- `contract.complete` 已成功时，合同进入 satisfied。
- 子合同完成时，创建 `kind=result` 的 AgentCommunicationEnvelope 和 child_completed mailbox item。
- validation fail 时，创建 `kind=repair` 的 AgentCommunicationEnvelope 和 repair_required mailbox item。
- mailbox 创建后，通知 message delivery 模块按 `trigger_turn` 尝试 same-turn steer；失败时保留 pending 并 fallback resume。
- `git.merge_apply` 已成功时，记录 canonical ref 更新。
- 短期 token 撤销。
- usage/transcript/evidence refs 持久化。

会话后不能做：

- 业务语义判断。
- 自动派发下一任务。
- 自动合并未由 Agent 请求的代码。
- 自动把失败任务标记为成功。
- 删除仍可能 resume 的 workspace/home。

## 9. 清理和 GC

清理必须基于 active guard 和 durable state。

允许清理：

- 已 terminal 且不可 resume 的临时文件。
- 可再生成的 build cache。
- 已过期且没有 active lease/operation 的 workspace。
- 已撤销 token。

禁止清理：

- running/preparing/waiting attempt 的 workspace。
- 有 active GitOperation 的 workspace。
- 仍需 resume 的 CLI home。
- 未持久化 transcript/session route 前的临时状态。

如果 cleanup 与 active guard 状态冲突，必须 fail closed：不删。

## 10. 异常恢复

### 10.1 Runner 崩溃

Recoverer 必须：

- 找到 preparing/running attempt。
- 检查 session.pin 是否存在。
- 检查 runtime 是否仍在线。
- 检查 workspace 是否存在。
- 根据状态标记 failed、retryable 或 resume_required。

### 10.2 容器销毁

如果容器销毁：

- 持久 home 必须保留。
- Backend session route 必须保留。
- Runner 可重建容器。
- Resume 后 Agent 应看到原会话历史和 pending mailbox signal；完整 feedback 仍通过 mailbox / communication capability 读取。

### 10.3 Terminal report 丢失

如果 CLI 已退出但 terminal report 未到达：

- Recoverer 不能直接假设成功。
- 只能根据可验证 evidence、contract.complete 记录、GitOperation 状态判断。
- 无法证明成功时标记 failed/retryable，并反馈给 Agent 或 operator。

### 10.4 GitOperation 中断

如果 GitOperation 运行中断：

- 检查 before_ref / after_ref。
- 检查 lock。
- 检查 worktree merge/rebase 状态。
- 能安全 abort 则 abort。
- 不能确定则标记 manual_attention，不能静默修改 canonical branch。

## 11. 生命周期接口

建议提供以下生命周期接口：

`session.current`

- 返回当前 attempt、lease、workspace、session route 摘要。

`session.pin`

- Runner/CLI adapter 调用，持久化 native session id、workdir、runtime route。

`session.progress`

- 上报会话进度和 heartbeat。

`session.wait`

- Agent 显式进入等待状态；通常由 `contract.wait` 调用封装。

`session.finish`

- Runner 提交 terminal report。

`session.inspect`

- Operator/debug 读取 session route、attempt 状态、transcript ref。

普通 Agent 不应能伪造其他 Agent 的 session id、workdir 或 attempt id。
普通 Agent-facing capability discovery 不应暴露 `session.pin`、`session.steer`、`session.interrupt` 或 active turn id 修改能力；这些接口属于 runner/backend 内部生命周期控制面。

## 12. 与其他模块的关系

### 12.1 Coordination

Coordination 决定 assignment、lease、mailbox、contract 状态。Session lifecycle 负责把这些状态落实到可恢复的 CLI 会话。

### 12.2 Runtime

Runtime 提供 Docker/external 环境。Session lifecycle 定义何时准备、启动、恢复、停止 runtime。

### 12.3 Message Delivery

Message delivery 负责把 MailboxItem 递送为 active turn 的轻量 signal，或在不可 steer 时触发 fallback resume。Session lifecycle 负责提供 session route、active turn route、attempt 状态和持久 home。

### 12.4 Code Management

Code management 的 GitOperation 必须绑定当前 session/lease/workspace。Session lifecycle 负责保证 Git 工具运行时 workspace 是真实、当前、未被 GC 的。

### 12.5 coordlink 和 skills

coordlink 是会话中的 Agent-facing capability 入口。Skills 是 Agent-facing 工作流说明。Session lifecycle 负责在启动前配置好 coordlink、已授权 skills 和 identity/scope。

## 13. 不变量

- workspace 未准备好前不能 running。
- CLI native session id 一旦可得必须尽快 session.pin。
- prepare 阶段必须有 prepare lease 或等价 TTL 机制。
- active guard 必须覆盖 preparing/running/waiting/git-operation。
- terminal report 必须幂等。
- 会话后不能做业务决策。
- 会话后可以做协议机械副作用。
- 容器销毁不能导致 session route 丢失。
- DeliverySignal 可以携带已授权 envelope 摘要或很短正文，但不能携带未授权、长正文或完整业务详情；Agent 必须通过 tool adapter 或 coordlink capability 读取权威内容。
- cleanup 不能删除可 resume 会话所需材料。
- Agent-facing Git/contract/artifact 操作必须绑定当前 lease scope。

## 14. 最小验收测试

### 14.1 启动顺序测试

- workspace 未落盘时 attempt 不得进入 running。
- coordlink 配置失败时 attempt 进入 failed，不启动 CLI。
- toolchain.ready 失败时返回 ENV_NOT_READY。

### 14.2 Prepare lease 测试

- 长时间 prepare 会刷新 prepare lease。
- prepare lease 过期后 assignment 可恢复。
- prepare 失败不会留下 active lease 阻塞队列。

### 14.3 session.pin 测试

- CLI native session id 出现后立即持久化。
- runner 崩溃后 recoverer 可用 pinned session route 恢复。
- terminal report 失败不丢 session route。

### 14.4 会话中反馈测试

- `contract.complete` 缺证据时在当前会话返回 rejected。
- `git.merge_apply` 冲突时在当前会话返回 CONFLICTS_FOUND。
- mailbox feedback 到达且 active turn 可 steer 时，runner/adapter 收到 DeliverySignal。
- mailbox feedback 到达但不可 steer 时，保留 pending mailbox 并 fallback resume 原 session route。
- same-turn steer 失败不影响 durable mailbox 正确性；下一轮 resume 仍能读取同一 envelope。
- 同一 route 第一次 mailbox 已 resume 后，第二个不同 mailbox 到达时仍能唤醒或 coalesced 到下一次 resume。
- one-shot command CLI 退出后不再被 same-turn steer；新反馈通过 fallback resume 进入同一逻辑会话。

### 14.5 后处理测试

- completed 释放 lease。
- waiting 保留 session route。
- failed 标记 retryable 或 failed，不伪装成功。
- token 在 terminal 后撤销。

### 14.6 清理测试

- active workspace 不会被 GC 删除。
- waiting/resumable session 的 home 不会被删除。
- 已 terminal 且不可 resume 的临时 workspace 可清理。

### 14.7 异常恢复测试

- runner 崩溃后 running attempt 被 recoverer 处理。
- 容器销毁后可重建并 resume。
- GitOperation running 时崩溃能 abort、retry 或 manual_attention。

## 15. 开发落地建议

Go 项目建议拆分：

```text
internal/session/attempts
internal/session/routes
internal/session/prepare
internal/session/pin
internal/session/progress
internal/session/terminal
internal/session/recovery
internal/session/gcguard
internal/session/service
internal/runtime/runner
internal/runtime/cliadapter
```

生命周期步骤应通过注册表组织：

```text
prepareSteps = [
  acquirePrepareLease,
  prepareWorkspace,
  prepareHome,
  configureCoordlink,
  checkToolchain,
  createActiveGuard,
  markReadyToLaunch,
]

postSteps = [
  persistTerminalReport,
  updateLeaseState,
  createProtocolMailbox,
  persistTranscriptRefs,
  revokeShortToken,
  releaseActiveGuard,
]
```

删除或替换某个步骤时，应只修改注册列表，不改主执行器。

## 16. 设计结论

CoordPlane 的 Agent 会话生命周期应遵循：

```text
会话前：真实准备，不虚报 running
会话中：尽早 pin，工具调用即时反馈
会话后：事实收敛，不替 Agent 决策
异常时：可恢复、可审计、可重试
```

这个设计避免把复杂业务推进藏到后台，同时保证多 Agent Docker 环境中的 session、workspace、GitOperation 和 mailbox 都有可靠的恢复路径。
