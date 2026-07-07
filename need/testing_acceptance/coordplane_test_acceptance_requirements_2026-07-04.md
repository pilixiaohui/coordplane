# CoordPlane 测试验收需求

本文是 CoordPlane 第一阶段测试验收的独立需求说明。它不替代各模块文档中的最小测试边界，而是定义全局验收口径：哪些不变量必须被测试保护，哪些场景必须覆盖，哪些测试只能作为发布健康检查。

## 1. 目标

CoordPlane 的测试必须让实现收敛，而不是只证明某次演示跑通。

测试体系必须做到：

- 每个关键测试保护一个命名不变量。
- 尽量在最低可行真实边界复现问题。
- 公共接口必须走真实 public entrypoint，例如 HTTP、coordlink CLI、runner adapter、DB queue 或 Git repo。
- 必须验证 durable state、事件、返回值和禁止副作用。
- fake adapter 只能替代外部 CLI 智能体，不得替代正在测试的 CoordPlane 边界。
- 真实 CLI / Docker / 多 Agent live gate 是发布健康检查，不是第一调试入口。

## 2. 非目标

- 不用测试判断具体项目业务语义是否正确。
- 不把“真实 CLI 一次成功”当成所有协议正确。
- 不为了覆盖率复制大量同质 happy path。
- 不写死某个角色名、任务编号、随机模型输出或一次性项目文本。
- 不通过 mock 掉 DB、queue、Docker、Git 或 public service 来声称这些边界已被验证。

## 3. 测试分层

第一阶段按以下层级组织测试：

| 层级 | 用途 | 推荐边界 |
| --- | --- | --- |
| Static guard | 架构防退化、禁用旧路径、schema 单一来源 | 源码扫描、注册表检查、配置解析 |
| Pure logic | 状态计算、scope 裁剪、schema 校验、排序、幂等 key | 纯函数或小 service |
| Adapter conformance | HTTP、coordlink、schema-derived tool adapter、CLI backend adapter | realistic protocol frame / local fake server |
| Public service contract | capability 调用、权限、rejected response、事件副作用 | HTTP/coordlink public endpoint |
| State machine / storage | DB transaction、queue、lease、retry、recovery、migration | 真实 SQLite 或测试数据库 |
| Runtime boundary | Docker、external runtime、workspace、env、token、coordlink 安装 | 真实容器或真实 subprocess |
| Release health check | 最小多 Agent 流程、真实 CLI resume、same-turn signal | 少量稳定 live gate |

原则：能在低层证明的不变量，不上升到 live gate 才发现。

## 4. 测试场景格式

每个非平凡测试场景必须至少写清：

```text
Invariant: 被保护的不变量
Layer: 测试层级
Boundary: 真实入口或真实边界
Setup: 最小环境和数据
Steps: 行为步骤
Assertions: 必须成立的状态、事件、响应
Forbidden side effects: 明确不能发生什么
Mocks/Fakes: 允许 fake 什么，不允许 fake 什么
```

场景可以用表驱动注册。新增场景应添加到场景列表，不应在主测试执行器中增加 if/else 特判。

## 5. 必须覆盖的全局不变量

| ID | 不变量 |
| --- | --- |
| INV-01 | Backend canonical store 是唯一真相源，prompt、transcript、容器文件不能成为状态真相源 |
| INV-02 | HTTP、coordlink CLI、schema-derived tool adapter 调用同一 capability handler |
| INV-03 | Agent-facing surface 是 skills + schema-derived tool adapter / coordlink capability，工具 schema 来自 capability registry |
| INV-04 | TeamConfig 决定 skills、capability policy、runtime/CLI 和终止验收规则，backend 不硬编码角色语义 |
| INV-05 | 普通 Agent 无法越权读取其他 Agent mailbox、workspace、token、session 或全局 inspect |
| INV-06 | 失败必须以 rejected response 或 mailbox 反馈给 Agent，不能静默吞错 |
| INV-07 | 子任务完成会反馈给发布者原会话；backend 不替发布者自动做业务决策 |
| INV-08 | active turn 优先 same-turn signal，inactive 时 fallback resume；signal 可带授权摘要但不携带未授权或完整业务详情 |
| INV-09 | workspace、toolchain、coordlink 未准备好前，attempt 不能标记 running |
| INV-10 | Docker runtime 与 external runtime 在服务协议语义上等价 |
| INV-11 | Git/code operation 必须在 Agent 会话中返回可修复反馈，不在会话结束后后台静默合并 |
| INV-12 | retry、duplicate call、crash recovery 不产生重复合同、重复 mailbox、重复 commit 或 orphan event |
| INV-13 | AgentCommunicationEnvelope 是 Agent 间 message、task、result、repair、budget attention 的统一通信对象；backend 不用自然语言猜测任务语义 |

## 6. 测试场景矩阵

### TA-01 Capability 单一入口

Invariant: `INV-02`

Layer: Public service contract + adapter conformance

Boundary: HTTP endpoint、`coordlink call`、schema-derived tool adapter wrapper

Setup: 注册一个读 capability 和一个写 capability，使用同一 subject/scope/input。

Steps:

1. 分别通过 HTTP、coordlink 和 tool adapter 调用同一 capability。
2. 对写 capability 重复调用一次相同 idempotency key。

Assertions:

- 三个入口返回同一 typed response 或同一 rejected response。
- 写 capability 只产生一次 durable side effect。
- event log 中 capability name、subject、trace id、scope 一致。

Forbidden side effects:

- adapter 自己实现状态机。
- coordlink 返回与 HTTP 不同的成功语义。
- tool adapter 暴露 policy 不允许的 capability。
- tool adapter 维护手写第二 schema。

### TA-02 Skills 渐进披露

Invariant: `INV-03`

Layer: Agent-facing canary + public service contract

Boundary: Agent bootstrap prompt、`coordlink skill list/read`

Setup: TeamConfig 绑定 `coordplane-service` 给 agent A，不绑定 `controlled-git`；agent B 绑定两者。

Steps:

1. 生成 agent A 和 agent B 的 bootstrap prompt。
2. 分别调用 `skill.list` 和 `skill.read`。

Assertions:

- prompt 只包含 skill 摘要和读取方式，不内联全量 capability schema。
- agent A 无法 discovery 或读取未绑定 skill。
- skill 文档不包含 DB path、runtime root、secret、其他 Agent 私有状态。

Forbidden side effects:

- prompt 直接写入完整团队任务列表。
- skill 文档成为状态真相源。

### TA-03 TeamConfig Policy Scope

Invariant: `INV-04`、`INV-05`

Layer: Public service contract + pure logic

Boundary: capability discovery、policy authorize、prompt composer

Setup: 两个 Agent，角色名任意；TeamConfig 授权不同 capability。

Steps:

1. agent A 查询 capability discovery。
2. agent A 调用未授权 capability。
3. 修改角色名但保持 policy 不变，重复查询。

Assertions:

- discovery 只返回 policy 授权 capability。
- 未授权调用返回可修复 rejected response。
- 改角色名不改变授权结果。

Forbidden side effects:

- backend 根据硬编码角色名判断权限。
- rejected response 泄露其他 Agent 的私有信息。

### TA-04 Contract 派发、等待和反馈

Invariant: `INV-06`、`INV-07`、`INV-13`

Layer: State machine / storage + public service contract

Boundary: `contract.add`、`contract.wait`、`contract.complete`、mailbox queue

Setup: root contract 由 coordinator 持有；coordinator 派发子 contract 给 builder。

Steps:

1. coordinator 调用 `contract.add` 创建子任务。
2. coordinator 调用 `contract.wait` 进入等待子任务状态。
3. builder 提交 evidence 并 `contract.complete`。
4. backend 创建给 coordinator 的 mailbox item。

Assertions:

- 子 contract、assignment、lease、attempt、evidence、mailbox 都落入 canonical store。
- `contract.add` 创建 `kind=task` 的 AgentCommunicationEnvelope。
- 子任务完成反馈创建 `kind=result` 的 AgentCommunicationEnvelope。
- coordinator 原 contract 不被 backend 自动完成。
- mailbox 指向原发布者会话 route。

Forbidden side effects:

- 子任务完成后 backend 自动派发下一任务。
- 发布者完成状态导致无法 resume。
- mailbox 只有 transient log，没有 durable row。

### TA-04B Message 和 Task 边界

Invariant: `INV-13`

Layer: Public service contract + state machine / storage

Boundary: `message.send`、`contract.add`、`communication.read`

Setup: 两个 Agent，发送方有 direct message 和 contract.add 权限。

Steps:

1. 发送方调用 `message.send` 发送普通协作信息。
2. 发送方调用 `contract.add` 派发可追责工作。
3. 发送方发送一段自然语言看起来像任务、但没有 `kind=task`、`intent=task_request` 或 `requires_contract=true` 的普通消息。

Assertions:

- `message.send` 创建 `kind=message` envelope，不创建 WorkContract。
- `contract.add` 创建 `kind=task` envelope、WorkContract 和 Assignment。
- `communication.read` 只能读取授权 envelope。
- 第三步不会被 backend 用自然语言检测强制拒绝或改写为合同。

Forbidden side effects:

- backend 根据“这段话看起来像任务”进行 NLP 拦截。
- 普通消息偷偷创建合同。
- task envelope 没有对应 WorkContract。

### TA-05 Rejected Response 可修复

Invariant: `INV-06`

Layer: Public service contract

Boundary: `contract.complete`、`artifact.upload`、`validation.assessment`

Setup: Agent 尝试完成合同但缺少 required evidence。

Steps:

1. 调用 completion capability，故意缺少 evidence ref。
2. 按 rejected response 中的修正提示补充 evidence。
3. 再次调用 completion capability。

Assertions:

- 第一次返回 rejected，包含 error_code、message、repair_hint、retryable。
- 第一次不改变合同终态。
- 第二次成功后 durable state 和 event 完整。

Forbidden side effects:

- 把失败转换成空成功。
- 后台后置检查才发现缺 evidence。
- Agent 看不到修正路径。

### TA-06 Mailbox Delivery same-turn / fallback

Invariant: `INV-08`

Layer: Adapter conformance + state machine

Boundary: Delivery service、fake CLI steer adapter、runner queue

Setup: 一个 active turn route，一个 inactive session route。

Steps:

1. 给 active route 创建 mailbox。
2. 给 inactive route 创建 mailbox。
3. fake steer adapter 对 active route 成功，对 inactive route 不可用。

Assertions:

- active route 创建 DeliveryAttempt 并发送轻量 signal。
- signal 可以包含授权 envelope 摘要或短正文，但不包含未授权内容、长正文或完整业务详情。
- inactive route 进入 fallback resume queue。
- mailbox 不因 steer 失败被标记 resolved。

Forbidden side effects:

- signal 携带完整合同、完整消息正文、完整验证详情或未授权内容。
- steer 失败后丢消息。
- delivery service 直接修改合同业务状态。

### TA-07 Runtime 准备和隔离

Invariant: `INV-09`、`INV-10`

Layer: Runtime boundary

Boundary: Docker runner、external runner、coordlink version/status

Setup: 一个 Docker runtime profile，一个 external runtime profile，使用同一 TeamConfig policy。

Steps:

1. 启动 Docker runtime，检查 workspace、coordlink、token、backend URL。
2. 启动 external runtime，调用同一 capability。
3. 模拟 toolchain 未准备好。

Assertions:

- Docker 容器中无 DB path、runtime root、其他 Agent token。
- Docker 与 external 对同一 capability 返回同一结构化语义。
- toolchain 未准备好时 attempt 不能 running。

Forbidden side effects:

- 用 fake docker 代替真实 Docker 隔离测试。
- 容器通过文件读取调度真相。
- external debug 权限静默扩大到普通 Agent。

### TA-08 Session Pin 和容器重建 Resume

Invariant: `INV-08`、`INV-09`

Layer: Runtime boundary + state machine

Boundary: `session.pin`、SessionRoute store、runner resume adapter

Setup: CLI backend fake 能报告 native session id；Docker home volume 可持久化。

Steps:

1. attempt running 后调用 `session.pin`。
2. 销毁 runtime 但保留持久 home。
3. 创建 pending mailbox。
4. runner 按 SessionRoute resume。

Assertions:

- SessionRoute 包含足够 resume 信息。
- resume 后同一 session route 被使用。
- Agent 只收到 pending mailbox signal，完整内容仍需 capability 读取。

Forbidden side effects:

- session route 只存在容器临时文件里。
- 容器销毁后需要新会话从零开始。

### TA-09 Queue / Retry / Crash Recovery

Invariant: `INV-12`

Layer: State machine / storage

Boundary: DB queue、lease、worker recovery

Setup: assignment queue、delivery queue、runner queue 各一个 item。

Steps:

1. worker claim item 后模拟 crash。
2. lease 过期后 recovery worker 重新排队。
3. 重复执行同一 idempotency key。

Assertions:

- 同一时间只有一个 worker lease。
- retry 产生 backoff 和 attempt_count。
- 成功后 durable object 不重复。
- 超过 retry limit 进入 dead，但 canonical object 保留。

Forbidden side effects:

- crash 后 item 永久 leased。
- retry 生成重复 contract/mailbox/event。

### TA-10 Object Store / Artifact

Invariant: `INV-01`、`INV-05`

Layer: Public service contract + storage

Boundary: `artifact.upload`、`artifact.download`、ObjectStore

Setup: agent A 上传 artifact，agent B 只有部分授权。

Steps:

1. agent A 上传 artifact 并引用到 evidence。
2. agent B 尝试下载未授权 artifact。
3. 授权后再次下载。

Assertions:

- artifact metadata 和 blob ref 都持久化。
- 未授权下载 rejected，且不泄露真实 host path。
- 授权后内容 hash 与上传一致。

Forbidden side effects:

- artifact 只存在 agent workspace。
- download 返回 host absolute path 给普通 Agent。

### TA-11 Controlled Git 会话内反馈

Invariant: `INV-11`、`INV-12`

Layer: Runtime boundary + real Git repo

Boundary: `workspace.sync`、`git.commit`、`git.merge_apply`、`git.rollback`

Setup: 真实临时 Git repo，两个 Agent workspace，各自有提交。

Steps:

1. Agent A 提交变更。
2. Agent B 基于旧 base 提交冲突变更。
3. Agent B 调用 merge preview/apply。
4. 触发冲突并要求 Agent 在会话内 resolve 或 abort。

Assertions:

- Git operation 返回结构化 conflict result。
- 冲突状态、涉及文件、下一步 capability 写入 durable state。
- abort/rollback 后 repo 回到可解释状态。

Forbidden side effects:

- 会话结束后后台静默 merge。
- 冲突只写到本地文件，不反馈给 Agent。
- rollback 删除无关提交或无关工作区文件。

### TA-12 Inspect 和可观测性

Invariant: `INV-01`、`INV-05`

Layer: Public service contract

Boundary: inspect API、operator CLI

Setup: 有 contracts、attempts、mailbox、delivery attempts、failed queue item。

Steps:

1. operator 调用 inspect。
2. 普通 Agent 尝试调用全局 inspect。

Assertions:

- operator 能看到可诊断状态：queue、lease、attempt、mailbox、last_error、trace id。
- 普通 Agent 被拒绝或只看到自身 public scope。
- inspect 不暴露 secret、token、host path。

Forbidden side effects:

- inspect 读取 transcript 文件作为状态真相。
- 普通 Agent 通过 inspect 获取全局团队状态。

### TA-13 End-to-end Fake CLI Protocol Gate

Invariant: `INV-02` 到 `INV-12` 的组合健康检查

Layer: Release health check

Boundary: backend + real DB + fake CLI adapter + coordlink

Setup: 三个 Agent：coordinator、builder、validator；所有 Agent 使用 fake CLI adapter，所有操作通过 public capability。

Steps:

1. operator 创建 root contract。
2. coordinator 领取任务并派发 builder 子任务。
3. builder 提交 evidence 并完成。
4. coordinator 收到 mailbox 后派发 validator 终止验收合同。
5. validator 提交 `validation.assessment`。
6. coordinator 完成 root contract。

Assertions:

- 全程无 backend 自动业务决策。
- 所有状态可由 canonical store 重建。
- 每一步都通过 capability 调用产生 durable event。
- 合同预算只在发布新合同计数，反馈和 resume 不计入预算。

Forbidden side effects:

- fake CLI 直接改 DB。
- 用固定文本模拟项目语义通过。

### TA-14 Real CLI / Docker Smoke Gate

Invariant: runtime wiring health

Layer: Release health check

Boundary: real Docker container + one real CLI backend + coordlink + backend

Setup: 一个最小 assignment，只要求 CLI 调用 `contract.current`、`mailbox.list` 和提交一条 harmless report。

Steps:

1. runner 启动 Docker runtime。
2. CLI session 读取当前合同。
3. CLI 调用 coordlink 提交 report。
4. runner 结束 attempt。

Assertions:

- 容器内 CLI 可用。
- coordlink 可连接 backend。
- attempt lifecycle 正确收尾。
- transcript ref 已保存但不是状态真相源。

Forbidden side effects:

- 把该 smoke gate 当作业务验收。
- 为了跑通 smoke gate 绕过 policy 或 canonical store。

## 7. CI Gate 建议

第一阶段建议拆成以下 gate：

| Gate | 内容 | 触发 |
| --- | --- | --- |
| `unit` | pure logic、schema、policy、prompt composer | 每次提交 |
| `contract` | HTTP/coordlink capability、rejected response、scope | 每次提交 |
| `storage` | SQLite migration、queue、lease、retry、recovery | 每次提交 |
| `adapter` | fake CLI、coordlink、schema-derived tool adapter conformance | 每次提交 |
| `runtime` | Docker/external runtime、coordlink install、isolation | 合并前或带 runtime 标签 |
| `release-health` | TA-13 fake CLI flow、TA-14 real CLI smoke | 发布前或手动 |

实现时可以用 Go build tags 或环境变量区分 runtime/live gate，但不能让跳过 runtime gate 的结果伪装成完整验收通过。

## 8. 测试代码组织建议

推荐 Go 包结构：

```text
internal/testing/scenario
internal/testing/assertions
internal/testing/fakes/cliadapter
internal/testing/fixtures/teamconfig
tests/contract
tests/storage
tests/runtime
tests/releasehealth
```

要求：

- 场景用注册表组织，例如 `[]Scenario{...}`。
- 公共断言放到 helper 中，例如 `AssertNoOrphanEvents`、`AssertRejectedIsRepairable`、`AssertScopedVisibility`。
- 不为每个模块复制同一套 setup。
- 删除某个 capability 或 adapter 时，应能删除对应 scenario 注册项，而不是改主执行器。
- fixture 只表达协议，不承载具体项目业务语义。

## 9. 验收完成定义

一个功能可以声明完成，至少满足：

1. 对应需求文档中的不变量有测试覆盖。
2. public entrypoint 被覆盖，不能只测内部 helper。
3. 失败路径、权限拒绝、重复调用或 crash recovery 被覆盖，除非该功能无此风险。
4. 测试断言 durable state 和禁止副作用。
5. 旧设计或错误路径有静态 guard、负向测试或删除证明。
6. 相关 narrow test、模块 suite 和必要 gate 已通过。

真实 live gate 失败时，不直接在 live gate 上反复修补。必须先归约为一个低层失败场景，补回归测试，再修实现。

## 10. 设计结论

CoordPlane 的测试验收应围绕协议不变量、public capability、DB truth、scope、runtime 和会话内反馈建立。fake CLI flow 用来证明后台信息流转，真实 CLI/Docker gate 只证明运行接线健康。项目业务语义不进入 CoordPlane 核心测试。
