# CoordPlane v1 rc3 Add-Remove 准入计划

状态：已按 UR-0010 授权执行
基线代码 SHA：`2dee6e7bccaa712f59b0a7e410f734077125ad0b`
需求候选：`need/` 五份 `1.0-rc3` 规范 + `user-requirements-verbatim.md`
日期：2026-08-16

## 1. 目的和审批边界

本计划满足 `need/acceptance.md` 的代码预算准入要求，不修改任何预算阈值，也不以删除有效契约测试换取额度。UR-0010 授权执行开发准入步骤 1-5，因此批准的是本计划的硬上限、先删后加顺序和逐候选复算机制；每个后续产品 candidate 仍需独立测试审核和代码审查。

当前基线已经超过 tests 发布阈值，且 production/total 只剩极小余量。正式功能开发前必须先迁移旧合同、删除旧入口并回到软阈值；不得以预计未来删除为由先增加代码。

## 2. 可复算基线

复算命令：

```bash
LOC_BASE_REF=a69bc4220da94edbe77e96f134edf8d6659efe06 \
  scripts/loc-budget.sh --check --output loc-budget.json
```

`2dee6e7` 的原子 bucket：

| bucket | 当前 | 目标 | 软阈值 | 发布阈值 | 到软阈值必须净减少 |
| --- | ---: | ---: | ---: | ---: | ---: |
| production | 24,974 | 24,000 | 24,500 | 25,000 | 474 |
| tests | 27,107 | 25,500 | 26,200 | 27,000 | 907 |
| infra | 603 | 250 | 500 | 700 | 103 |
| backend total | 52,684 | 49,750 | 51,200 | 52,700 | 1,484 |
| frontend | 1,173 | 2,000 | 2,500 | 3,000 | 0 |

本次需求文档合同守卫删除了旧 README/provider 字符串耦合，并增加精确文档集合与 `UR-NNNN` 连续性检查，static/FSM tests 净增 16 行。该 requirement revision 的工作树复算为 `production=24,974/tests=27,123/infra=603/total=52,700/frontend=1,173`；total 不超过发布阈值，但 tests 仍超发布阈值 123 行。

最初的产品 cutover candidate 必须从该 requirement revision 同时净减少 production 474、tests 923、infra 103、backend total 1,500 行，达到 `production <= 24,500`、`tests <= 26,200`、`infra <= 500`、`total <= 51,200`。四项必须共同满足，不能只删除一个 bucket 后把额度转移到另一个 bucket。

## 3. 模块上限

回到软阈值后的模块上限如下。一个模块需要超过上限时，必须在同一 candidate 从同 bucket 的明确旧 owner 删除等量代码，并在 issue 中记录调整；bucket 和总上限不变。

### Production

| module | 基线 | cutover 上限 |
| --- | ---: | ---: |
| adapter | 671 | 700 |
| cmd | 1,948 | 1,800 |
| core | 8,368 | 8,000 |
| daemon/config/shared | 1,167 | 1,100 |
| git | 4,216 | 4,100 |
| runtime | 4,307 | 4,300 |
| store | 3,113 | 3,000 |
| transport | 1,184 | 1,500 |
| 合计 | 24,974 | 24,500 |

### Tests

| module | 基线 | cutover 上限 |
| --- | ---: | ---: |
| adapter conformance | 339 | 350 |
| core/store/CLI | 12,246 | 11,600 |
| Docker runtime | 4,709 | 4,500 |
| E2E | 5,393 | 5,400 |
| Git component | 3,239 | 3,200 |
| static/FSM | 1,181 | 1,150 |
| 合计 | 27,107 | 26,200 |

Infra cutover 上限为 500；frontend cutover 上限为 2,500。新增 reference workload 入口必须是薄 wrapper，先合并现有 live gate 的重复环境检查、收据输出和退出分类，不能在当前 603 行上直接叠加大脚本。

## 4. 删除来源与新合同 owner

下列是必须迁移后删除的旧路径库存，不是允许整批盲删的清单。删除前必须用 `rg` 证明调用方已迁移，并由新合同测试保护相同或更强的不变量。

| 旧路径或合同 | 物理行库存 | 新 owner / 迁移目标 |
| --- | ---: | --- |
| `internal/core/agent.go` | 318 | `Participant` 单一身份和静态 agent 配置 operation |
| `internal/core/boss_message.go` | 61 | `Conversation/Message/MessageRecipient` operation |
| `internal/core/agent_message.go` | 282 | 显式多 recipient 消息合同 |
| `internal/core/task_complete.go` | 118 | 统一 submit -> accept FSM；删除 `human_confirm` |
| `internal/core/message.go` | 218 | 通用 Participant 消息入口 |
| `internal/operatorcli/project_agent.go` | 309 | Participant/Role/Credential CLI |
| 上述完整旧生产文件库存 | 1,306 | 足以覆盖 production 首个 474 行净删除下限，但只有迁移完成的文件可删除 |
| 四个 P2 Boss/Human 旧合同测试 | 671 | CT-02/03/04/05/06/09 表驱动合同 |
| agent 配置与旧 RMA 测试库存 | 1,951 | Participant adapter 合同和唯一 RMA-02/RMA-03 场景；先迁移断言再删除重复 fixture |

Store、transport、coordlink 和 operator CLI 中仍存在 `boss`、`recipient_kind`、`sender_kind`、`actor_kind`、`agents` 和 `human_confirm` 混合路径。这些文件不能整文件计作预期删除量；每个收敛锁只能在迁移调用方后以 candidate 实际 diff 计入净删除。

新合同必须增加或改写的 owner 范围：

| 合同 | Production owner | Test owner |
| --- | --- | --- |
| Participant/Role/Credential/exact schema | core/store/transport/cmd | core/store/CLI + static/FSM |
| Conversation/MessageRecipient/unread/child result | core/store/transport | core/store/CLI + deterministic E2E |
| Run fencing/Docker/resume/log index | runtime/daemon/adapter | Docker runtime + adapter conformance |
| Human capture/task ref/integration CAS | git/core | Git component + deterministic E2E |
| Web coordination/log/security | web frontend | browser E2E，计入 frontend bucket |
| RMA-03 reference workload | thin infra wrapper | E2E receipt and workload tests |

## 5. 每个 candidate 的机械准入

每个实现 issue 必须记录 `before/add/remove/after` 四组 bucket 数值和模块归属。验证顺序固定为：

1. `rg` 确认被替换函数、字段、路由和 fixture 无调用方。
2. 删除旧实现和保护旧语义的测试。
3. 增加最低真实边界的红测与最小实现。
4. 运行目标契约测试、`go test ./...` 和适用的 race/Docker gate。
5. 在 clean candidate SHA 运行 `scripts/loc-budget.sh --check`。

在首次达到全部 soft threshold 前，candidate 不得净增加任何超软 bucket；达到后仍不得超过 `need/acceptance.md` 的发布阈值。若有效新合同无法在阈值内实现，只能先提交新的 `UR-NNNN`、修改需求预算并重新审批，不能修改脚本阈值或压缩代码逃避统计。
