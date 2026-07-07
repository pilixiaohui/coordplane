# CoordPlane 代码管理功能需求

本文是 CoordPlane code management 模块的独立需求说明。它定义多 Agent Docker 环境下的代码同步、提交、合并、冲突解决、回滚和审计协议。

## 1. 背景和目标

CoordPlane 需要支持多个 CLI Agent 并发开发同一个项目。每个 Agent 都在自己的隔离 runtime 中读写代码，但代码最终需要进入同一条受控主线。

目标不是让后台机械替 Agent 合并，也不是让 Agent 绕过后台直接 `git push`。正确模型是：

```text
Agent 在自己的会话内判断下一步
Agent 按 controlled-git skill 指引调用 coordlink Git capability
CoordPlane backend 执行受控 Git 操作或调度 runner 执行
Backend 记录操作、锁、输入、输出、before/after ref
Backend 把成功、冲突或失败结果立即返回给当前 Agent 会话
Agent 在同一会话中继续修复、重试、回滚或反馈
```

CoordPlane 必须信任 Agent 的判断能力，但所有代码同步、提交、合并、回滚都要被后台跟踪、限制和审计。

代码管理是建立在同一 AgentCommunicationEnvelope、session route、capability registry 和 coordlink/tool adapter 协议之上的独立能力组。第一阶段可以先完成通信、mailbox、session 和基础 contract 闭环；受控 Git 能力不应阻塞最小通信 MVP，但实现后必须复用同一协议边界。

## 2. 非目标

第一版不做：

- 自动替 Agent 决策要改什么代码。
- 后台在 Agent 会话结束后静默合并。
- 让多个 Agent 共享同一个可写源码目录。
- 让 Agent 直接向 canonical branch `git push`。
- 硬编码项目语义、测试命令或业务验收标准。
- 完整替代 GitHub/GitLab 的 PR UI。

可以后续扩展：

- 远端 PR 创建和状态同步。
- 多仓库事务。
- 大规模 merge queue。
- 发布流水线和部署 gate。

## 3. 术语

| 名称 | 定义 |
| --- | --- |
| Canonical repo | CoordPlane 管理的项目代码真相仓库 |
| Canonical branch | 受控主线，例如 `main` 或团队配置指定分支 |
| Agent workspace | 某个 Agent 的私有工作区，位于 Docker 容器或 external runtime 中 |
| Workspace base | Agent workspace 创建或同步时基于的 canonical commit |
| ChangeSet | Agent 提交给后台跟踪的一组代码变更 |
| GitOperation | 一次后台受控 Git 操作记录 |
| MergeAttempt | 一次把 ChangeSet 合入目标 ref 的尝试 |
| ConflictSet | 一次合并冲突的文件、片段和解决状态 |
| RollbackPoint | 可回退点，记录某个操作前后的 ref 和工作区状态 |
| Integration ref | 后台用于试合并和验证的临时 ref |

## 4. 总体模型

每个 Agent 拥有独立工作区：

```text
canonical repo
  -> workspace.prepare
  -> agent private workspace
  -> agent edits code
  -> git.commit / changeset.submit
  -> git.merge_preview
  -> git.merge_apply
  -> canonical branch updated
```

Agent 可以在容器内读写自己的 workspace，但代码管理的关键副作用必须通过 backend 接口：

- 创建工作区。
- 同步到最新主线。
- 生成 diff。
- 创建 commit。
- 登记 ChangeSet。
- 尝试合并。
- 应用合并。
- 查询冲突。
- 标记冲突已解决。
- 回滚。

## 5. 后台职责边界

Backend 负责：

- 保存 Repo、Workspace、ChangeSet、GitOperation、MergeAttempt、ConflictSet、RollbackPoint。
- 鉴权和 scope 裁剪。
- 管理 workspace lock、repo integration lock 和 ref lease。
- 执行或调度 runner 执行 Git 命令。
- 记录每次 Git 操作的 before_ref、after_ref、stdout、stderr、exit_code。
- 把冲突、失败和可修复下一步返回给 Agent。

Agent 负责：

- 理解任务目标。
- 修改代码。
- 判断何时提交、同步、合并、回滚。
- 读取冲突信息并修复冲突。
- 在会话中根据 backend 反馈继续处理。

Runner 负责：

- 在正确 workspace 中执行受控 Git 命令。
- 保证命令运行环境和 Agent CLI 所在 workspace 一致。
- 不替 Agent 做业务决策。

coordlink 负责：

- 暴露 local command adapter 和可选协议 adapter。
- 传递 Agent identity、lease scope、workspace id、trace id。
- 原样返回 backend response。

## 6. 数据对象

### 6.1 Repo

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| remote_url | 仓库地址 |
| canonical_branch | 受控主线 |
| default_base_ref | 默认基线 |
| status | active、disabled、error |
| policy | 合并、回滚、验证策略 |

### 6.2 Workspace

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| repo_id | 关联 Repo |
| agent_id | 所属 Agent |
| runtime_id | 所属 runtime |
| contract_id | 当前任务合同 |
| path | 逻辑路径，真实路径只给 runner |
| base_ref | 创建或同步时的 commit |
| head_ref | 当前 workspace HEAD |
| dirty | 是否有未提交变更 |
| state | preparing、ready、locked、error、archived |

### 6.3 ChangeSet

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| workspace_id | 来源 workspace |
| contract_id | 关联任务 |
| base_ref | 变更基线 |
| head_ref | 变更 HEAD |
| commit_ids | 包含的提交 |
| summary | Agent 提交的摘要 |
| evidence_refs | 测试、报告或验证证据 |
| state | draft、submitted、merged、abandoned、reverted |

### 6.4 GitOperation

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| operation_type | status、sync、diff、commit、rebase、merge_preview、merge_apply、resolve、rollback |
| actor_agent_id | 发起 Agent |
| workspace_id | 操作 workspace |
| repo_id | 操作 repo |
| idempotency_key | 防重复提交 |
| before_ref | 操作前 ref |
| after_ref | 操作后 ref |
| stdout_ref / stderr_ref | 日志引用 |
| exit_code | Git 命令退出码 |
| state | pending、running、succeeded、rejected、failed、rolled_back |
| created_at / completed_at | 时间戳 |

### 6.5 MergeAttempt

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| changeset_id | 目标 ChangeSet |
| target_ref | 合并目标 |
| integration_ref | 临时合并 ref |
| base_before | 合并前目标 commit |
| result_ref | 合并结果 commit，可为空 |
| state | clean、conflicted、failed、applied、aborted |
| conflict_set_id | 有冲突时关联 ConflictSet |

### 6.6 ConflictSet

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| merge_attempt_id | 关联 MergeAttempt |
| files | 冲突文件列表 |
| conflict_summary | 面向 Agent 的摘要 |
| state | open、resolved、abandoned |
| resolved_by | 解决冲突的 Agent |

### 6.7 RollbackPoint

| 字段 | 要求 |
| --- | --- |
| id | backend 生成 |
| operation_id | 关联 GitOperation |
| scope | workspace、changeset、integration、published |
| before_ref | 回滚前 ref |
| after_ref | 回滚后 ref |
| rollback_strategy | reset、abort、revert |
| state | available、used、expired |

## 7. Agent-facing 接口

所有接口应作为 backend capability 暴露，也可通过 `coordlink` local command 或 schema-derived tool adapter 调试。HTTP、coordlink CLI、tool adapter 必须调用同一 backend handler。controlled-git skill 负责说明 Agent 如何使用这些 capability。

### 7.1 Workspace 接口

`workspace.prepare`

- 准备或恢复 Agent 私有 workspace。
- 返回 workspace id、base ref、逻辑路径和当前状态。

`workspace.status`

- 返回当前 workspace 的 base/head、dirty 状态、是否落后 canonical branch。

`workspace.sync`

- Agent 主动请求同步到最新 canonical branch。
- 如果 workspace 有未提交变更，必须 rejected 并提示先 `git.diff`、`git.commit` 或 `git.rollback`。

### 7.2 Git 读取接口

`git.status`

- 返回受控 `git status --porcelain` 语义。

`git.diff`

- 返回当前 workspace diff 或指定 ChangeSet diff。

`git.log`

- 返回当前 workspace 相关提交，按 scope 裁剪。

### 7.3 Git 写入接口

`git.commit`

- 在当前 workspace 创建 commit。
- Agent 提供 commit message 和 path scope。
- Backend/runner 执行 add/commit，记录 before/after ref。
- 禁止默认全量 add；必须显式 paths 或由 policy 允许。

`git.rebase`

- 将 workspace 变更 rebase 到指定 target ref。
- 冲突时返回 ConflictSet。

`changeset.submit`

- 把当前 commit 范围登记为 ChangeSet。
- 不自动合入 canonical branch。

`changeset.abandon`

- 放弃未合并 ChangeSet，保留审计记录。

### 7.4 合并接口

`git.merge_preview`

- 在 integration ref 上尝试合并 ChangeSet。
- 不更新 canonical branch。
- 返回 clean、conflicted 或 failed。

`git.merge_apply`

- 将已 clean 的 MergeAttempt 应用到 canonical branch。
- 必须持有 repo integration lock。
- 必须使用 expected old sha，防止覆盖其他 Agent 的新提交。

`git.conflicts`

- 返回当前 ConflictSet 的文件列表、冲突片段位置、建议下一步。

`git.resolve`

- Agent 修完冲突后调用。
- Backend 检查冲突标记是否清除，并继续 rebase/merge。

### 7.5 回滚接口

`git.rollback`

- 按 operation id、changeset id 或 rollback point 回退。
- 已发布远端的变更不得 rewrite history，必须生成 revert ChangeSet。

`git.abort`

- 中止当前 rebase、merge 或 cherry-pick。

## 8. 标准流程

### 8.1 并发开发

```text
Agent A workspace.prepare(base=s10)
Agent B workspace.prepare(base=s10)
Agent A 修改代码并 git.commit -> cA
Agent B 修改代码并 git.commit -> cB
Agent A changeset.submit
Agent A git.merge_preview -> clean
Agent A git.merge_apply -> canonical=s11
Agent B workspace.status -> stale_base
Agent B workspace.sync 或 git.rebase(target=s11)
Agent B 解决可能冲突
Agent B merge_apply -> canonical=s12
```

### 8.2 冲突解决

```text
Agent 调用 git.merge_apply
Backend 返回 CONFLICTS_FOUND
Agent 调用 git.conflicts
Agent 修改冲突文件
Agent 调用 git.resolve
Backend 验证冲突清除
Agent 再次 git.merge_preview
Agent 调用 git.merge_apply
```

所有步骤都发生在同一个 Agent 会话内。Backend 不能等 Agent 会话结束后静默合并。

### 8.3 回滚

```text
Agent 调用 git.rollback(operation_id=op123)
Backend 判断 rollback scope
  workspace 未发布：reset 到 rollback point
  changeset 未合并：abandon
  integration 未发布：reset integration ref
  已发布：生成 revert changeset
Backend 返回结果
Agent 决定继续修复或反馈
```

## 9. 锁和并发控制

必须至少有三类锁：

- Workspace lock：同一 workspace 同时只能有一个 Git 写操作。
- Repo integration lock：更新 canonical branch 时串行。
- Ref lease：更新目标 ref 时必须带 expected old sha。

锁必须有 TTL 和 recoverer：

- 进程崩溃后锁可恢复。
- operation 卡住后可标记 failed 或 retryable。
- 不允许 DB 状态和 Git ref 状态长期不一致。

## 10. 失败和反馈

所有 Git 失败必须返回结构化 response 给当前 Agent。

冲突示例：

```json
{
  "ok": false,
  "status": "rejected",
  "error_code": "CONFLICTS_FOUND",
  "message": "merge produced conflicts in 2 files",
  "canonical_ids": {
    "merge_attempt_id": "merge_123",
    "conflict_set_id": "conflict_456",
    "workspace_id": "ws_789"
  },
  "conflicts": [
    {"path": "src/auth.go"},
    {"path": "tests/auth_test.go"}
  ],
  "allowed_next_actions": [
    "git.conflicts",
    "git.resolve",
    "git.abort",
    "git.rollback"
  ],
  "retryable": true
}
```

落后基线示例：

```json
{
  "ok": false,
  "status": "rejected",
  "error_code": "STALE_BASE",
  "message": "workspace is based on s10 but canonical branch is s12",
  "allowed_next_actions": [
    "workspace.sync",
    "git.rebase",
    "git.diff"
  ],
  "retryable": true
}
```

## 11. 回滚策略

### 11.1 Workspace 回滚

适用于未提交或未合并变更。

- 可使用 `git reset --hard` 回到 rollback point。
- 必须先记录当前 dirty diff，避免不可审计丢失。
- 返回被丢弃的 diff ref。

### 11.2 ChangeSet 回滚

适用于已提交但未合并的 ChangeSet。

- 标记 ChangeSet 为 abandoned。
- 不删除提交记录。
- 可保留 workspace 供 Agent 继续修改。

### 11.3 Integration 回滚

适用于合并到 integration ref 但未更新 canonical branch。

- 可以 reset integration ref。
- 释放 integration lock。

### 11.4 Published 回滚

适用于已经进入 canonical branch 或远端的提交。

- 禁止默认 rewrite history。
- 生成 revert commit 或 revert ChangeSet。
- Agent 必须能在会话内看到 revert diff 并决定是否提交。

## 12. 从 Multica 可借鉴的能力边界

Multica 可借鉴：

- daemon 领取任务并启动 CLI Agent。
- 每个任务独立 workdir。
- repo bare cache。
- git worktree 创建。
- per-repo mutex，避免 fetch/worktree 并发破坏 Git 内部状态。
- session_id / work_dir 持久化。
- workdir 准备完成后再标记 running。
- 使用真实 Git repo 写回归测试。

Multica 不能直接满足本需求：

- 它没有完整的 Agent-facing Git transaction API。
- 它的 repo checkout 主要是 daemon 本地能力，不是 backend 受控合并系统。
- 它没有把每次 commit/rebase/merge/rollback 建模为可恢复的 GitOperation。
- 它不负责让 Agent 在同一会话内根据 merge/conflict response 自行修复。

CoordPlane 应借鉴 Multica 的 runtime 和 worktree 工程经验，但必须新增受控 Git service。

## 13. 不变量

- Agent 不直接 push canonical branch。
- Agent 不共享可写源码目录。
- 所有 Git 写操作必须产生 GitOperation。
- 所有 Git 写操作必须记录 before_ref 和 after_ref。
- `git.merge_apply` 必须持有 repo integration lock。
- 更新 canonical ref 必须带 expected old sha。
- 冲突必须反馈给当前 Agent 会话。
- 回滚不能删除审计记录。
- 已发布变更默认用 revert，不 rewrite history。
- Backend 不替 Agent 判断业务代码是否正确。
- Runner 不替 Agent 自动合并下一步。

## 14. 最小验收测试

### 14.1 Workspace 测试

- 两个 Agent 准备同一 repo 时得到不同私有 workspace。
- Agent A 无法访问 Agent B workspace。
- workspace.status 能识别 dirty、clean、stale_base。

### 14.2 Commit 测试

- `git.commit` 必须要求显式 paths 或 policy 允许。
- commit 成功记录 GitOperation before/after ref。
- commit 失败保留可读 stderr。

### 14.3 Merge 测试

- 两个无冲突 ChangeSet 串行 merge_apply 后 canonical branch 前进两次。
- merge_apply 使用 expected old sha；目标 ref 被别人更新时返回 STALE_TARGET。
- merge_preview 不更新 canonical branch。

### 14.4 Conflict 测试

- 两个 Agent 修改同一行时，后合并者收到 CONFLICTS_FOUND。
- git.conflicts 返回冲突文件。
- Agent 修复后 git.resolve 能推进 merge。

### 14.5 Rollback 测试

- 未合并 ChangeSet abandon 后不影响 canonical branch。
- integration ref 回滚释放 integration lock。
- 已发布提交 rollback 生成 revert ChangeSet，不 rewrite history。

### 14.6 Crash Recovery 测试

- GitOperation running 时进程崩溃，recoverer 能根据 lock 和 ref 状态恢复。
- DB 成功但 Git ref 未更新时，状态变为 failed/retryable。
- Git ref 更新但 DB 未提交时，recoverer 能发现并补齐或告警。

## 15. 开发落地建议

Go 项目建议拆分：

```text
internal/codemanagement/repos
internal/codemanagement/workspaces
internal/codemanagement/operations
internal/codemanagement/changesets
internal/codemanagement/merge
internal/codemanagement/conflicts
internal/codemanagement/rollback
internal/codemanagement/gitexec
internal/codemanagement/service
internal/adapters/tools/git
internal/coordlink/gitclient
```

Git 命令执行必须集中在 `gitexec`，业务 handler 不能到处 shell out。

新增 Git capability 必须通过注册表接入：

```text
registeredGitCapabilities = [
  workspace.prepare,
  workspace.status,
  workspace.sync,
  git.status,
  git.diff,
  git.log,
  git.commit,
  git.rebase,
  changeset.submit,
  changeset.abandon,
  git.merge_preview,
  git.merge_apply,
  git.conflicts,
  git.resolve,
  git.abort,
  git.rollback,
]
```

删除某个 capability 时，应只需从注册表移除，不应修改主执行器。

## 16. 设计结论

CoordPlane 的代码管理功能应是一个完整的受控 Git 系统：

```text
Agent 负责判断
CoordPlane 负责受控执行和审计
Runner 负责在正确 workspace 中执行命令
coordlink 负责把结果送回 Agent 会话
```

这样既保留 Agent 在会话内处理复杂冲突和决策的能力，又避免裸 Git 操作造成不可审计、不可恢复、不可并发控制的代码状态。
