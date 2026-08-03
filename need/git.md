# CoordPlane Git 协作需求

状态：Draft for owner review
依赖：`README.md`、`core.md`、`runtime.md`

## 1. 目标和边界

Git 模块只负责让多个隔离 Task 的真实 commit 安全收敛到一个 canonical ref。

它必须：

- 用 daemon-owned bare repo 保存项目代码真相。
- 为每个代码 Task准备独立 private clone 和固定 base SHA。
- 从实际 workspace捕获 commit 到 controller-owned task ref。
- 用 Git 原生 ancestry 和 expected-old ref CAS 推进 canonical。
- 把 non-fast-forward、stale 和冲突工作转成普通 integration Task交给 CLI Agent。
- 在崩溃、清理和并发更新后保证 commit 不丢、canonical 不被覆盖。

它不得代理 Agent 日常 Git命令，不建设 ChangeSet/GitOperation/ConflictSet/rollback 平台，不理解 diff，不执行后台 merge/rebase/cherry-pick，也不把 SQLite 当代码真相。

Owner批准的预算重基线不改变本文件的private clone、actual HEAD capture、task ref、expected-old CAS、integration、recovery或GC合同。全库production/tests/infra/total envelope固定为`20,000/21,000/22,400`、`21,000/22,000/23,600`、`250/400/600`和`41,250/43,400/46,600`；统计仍按`acceptance.md`的非空、非纯注释物理行口径。Git模块实际SLOC、质量blocker和diff必须进入clean候选revision的LOC JSON，并在真实多Agent场景中验证所有接受结果的lineage、task ref、integration和canonical CAS；不得用重基线reserve删除真实Git crash/fence证据。

## 2. 代码真相和仓库布局

每个 Project 有一个 Daemon 私有 bare repo：

```text
<data_dir>/repos/<project-id>.git
```

该 bare repo 保存：

```text
<Project.canonical_ref>                         # 例如 refs/heads/main
refs/coordplane/tasks/<task-id>/runs/<run-id>  # 每次捕获的不可变结果
```

规则：

- canonical ref 是 Project 当前已集成代码的唯一真相。
- Task `base_sha/head_sha/task_ref` 是精确引用；数据库不保存 patch或代码副本。
- 每次读取状态、提交结果或集成前都必须重新解析实际 ref，不能信任缓存的 `canonical_sha`、Agent自报或可移动 branch 名。
- Task/run ref 名由服务端 ID生成并通过 `git check-ref-format` 校验；Agent不能提供任意 ref。
- Bare repo只允许 Daemon服务用户访问，绝不以可写方式挂载进 Agent容器。
- Project source只允许本地Git repository并仅用于初始化；第一版不接收远端URL、不自动fetch/push。

## 3. Project 注册和导出

### 3.1 注册

`coordplane project add --repo SOURCE --ref REF` 必须：

1. 校验SOURCE是可读本地Git repository，要求REF规范化为完整`refs/heads/*`并解析为精确commit。
2. SQLite事务创建status=creating的Project，固定Project ID、source/ref、immutable initial SHA、最终control path、`pending_action=initialize`和operation ID，并写Event。
3. 在由Project ID确定的临时目录创建bare repo并导入对象。
4. 创建Project canonical ref指向该commit并运行最小完整性检查。
5. 原子移动到最终control repo路径。
6. SQLite CAS确认同一creating Project/operation/initial SHA，清除pending、转active并写terminal Event。已观察且完成必要隔离/清理的失败必须在事务中清除pending、转error并保留稳定原因；Daemon未能提交terminal事务的崩溃仍保持creating，由reconciler处理。

注册失败不得留下active Project。重启时creating Project只按Project行保存的initial SHA、operation ID和确定性临时/最终路径继续、核验或转error；即使source ref已经移动也不得换成新commit。无Project行的未知repo目录一律quarantine，不自动采用。注册本地non-bare repo时，不得修改其ref、index、working tree、config或hooks。

`project repair`对尚未完成初始化的Project沿用immutable initial SHA重新进入initialize；对已有control repo的error Project只做verify/fsck/ref核验并接受actual canonical作为事实，不得从`canonical_sha`或initial SHA回写、reset或force ref。每次repair使用新的Project pending operation ID，成功/失败都由Project行和同operation Event收敛。

### 3.2 Boss checkout

`coordplane project checkout PROJECT --dest PATH` 从实际 canonical ref 创建一个新的普通 checkout：

- 目标路径必须不存在或为空。
- 命令不得覆盖 Boss已有工作区。
- Checkout通过bundle或导出后移除/改写指向control repo的origin，只消费代码真相，不获得更新control canonical的路径或凭据。
- 第一版向 GitHub/GitLab 发布由 Boss使用标准 Git自行完成，不是 CoordPlane完成条件。

`coordplane task checkout TASK --dest PATH`使用同一导出边界，但目标必须是Task行保存的精确task ref/head。命令只接受已有capture结果的Task，导出后必须确认HEAD等于Task.head_sha并移除control remote；这是第一版中Boss审查未集成代码的正式入口。

## 4. Private clone 隔离

Docker/不可信 CLI Agent必须使用完整 private clone，不得使用共享 common Git dir：

```text
git clone --no-local <control-repo> <task-workspace>
git checkout --detach <base_sha>
git switch -c coordplane/task/<task-id>
```

准备后必须满足：

- HEAD 精确等于 Task.base_sha。
- `.git` 目录完全位于该 Task workspace内。
- `.git/objects/info/alternates` 不存在。
- 不使用 `--shared`、`--reference`、hardlink object store或 linked worktree。
- 删除或改写包含宿主 control repo路径的 `origin`；容器内不能访问该路径。
- 不注入 control repo/remote 的写凭据。
- 默认不继承 host hooks、credential helper、filter或全局 Git config。
- 默认不递归初始化 submodule；项目确需 submodule 时另行明确 source/credential边界。

带`source_task_*`输入的work/integration Task还必须通过受限bundle把保存的精确source commit导入private clone并创建本地convenience ref。该ref可被Agent移动，只用于checkout/diff；Daemon后续判断始终使用Task保存的SHA和controller bare事实。

Linked worktree共享 common Git dir和 refs，不构成不可信 Agent隔离。它只能用于明确 trusted-host 的开发测试，不能作为 Docker产品实现或通过隔离验收。

## 5. Task 基线和 workspace

### 5.1 Base 固定

- 创建代码 Task时，Daemon从实际 canonical ref读取 commit并写 `Task.base_sha`。
- `base_sha` 在 Task生命周期内不可修改。
- 两个并发 Task可以拥有相同 base SHA。
- canonical 后续前进不静默修改 active workspace或 Task.base_sha。

### 5.2 Stale 可见性

- `status/task current` 必须通过actual canonical与Task base/head投影`stale=true/false`和old/new SHA。
- 第一版不要求canonical每次前进后扫描所有Task并主动广播，避免消息风暴和第二套同步队列。
- Daemon不自动checkout/rebase/merge/reset active workspace。Agent可以继续旧base结果，接受时由CAS/integration Task收敛。
- 如果Agent必须基于最新代码重做，由Boss/父Agent显式rework/retry或创建新Task。

### 5.3 Agent 原生 Git

Agent可在 private clone内直接运行：

```text
git status / diff / log
git add / commit
git branch / merge / rebase / cherry-pick
项目测试命令
```

这些命令只改变 private clone。CoordPlane不代理、不审计每一步，也不保证 Agent选择的开发方法正确。最终是否可提交只由机械 capture检查决定。

审查未集成结果时，Boss使用`task checkout`；Agent Reviewer创建带`--source-task ID`的普通work Task，在其private workspace检查固定source convenience ref。Review结果仍用普通Task summary/Message表达，不创建validation对象。

## 6. 结果捕获

### 6.1 Submit 前置条件

代码Task调用`coordlink task submit --expected-head SHA`时，Core第一事务只做以下动作：

1. CAS校验Task=running、current Run、generation和token。
2. Task转finishing，写`pending_action=capture`、稳定operation ID、Run ID/generation、expected head和intent后的Task version。
3. Run写requested_outcome=submit、summary/expected head并撤销token。
4. 写使用同一operation ID的requested Event。

该命令只表示submit请求已durable，不得立即把Task写成submitted。Runtime在响应送达后结束Agent Run；只有Run terminal、workspace无任何writer后，受信capture helper才读取：

```text
git rev-parse --verify HEAD^{commit}
git status --porcelain
是否存在 merge/rebase/cherry-pick 中间态
当前 private branch（仅诊断）
```

capture要求：

- Working tree和 index必须 clean，包括未跟踪文件。
- 不得存在未完成 merge/rebase/cherry-pick。
- HEAD必须是 commit，且 Task.base_sha必须是 HEAD祖先；否则返回稳定 Git错误并保持 Task可恢复。
- 调用方给出的`expected_head`只做一致性校验；实际HEAD由受信helper读取且必须相等。
- HEAD可以等于 base，表示无代码变化的有效结果；result summary仍必须显式说明。

### 6.2 不可信 handoff

Host controller不得在Agent可写workspace上做“检查后继续运行Agent”的TOCTOU capture，也不得直接信任其`.git` config。结果导入使用受限handoff：

1. 确认source Run已terminal、Task仍finishing、pending action ID/version/run/generation完全匹配，且不存在其他容器/进程写该workspace。
2. 在独立受信helper容器中把workspace只读挂载，关闭replace/graft和system/global config，再次确认clean/HEAD并生成只包含目标commit graph的Git bundle/pack。
3. Daemon通过per-Run临时目录取得handoff文件，限制大小、对象数、处理时间和磁盘占用。
4. 使用sanitized Git环境校验bundle完整性并导入control bare repo。
5. 在controller bare中重新验证commit类型、base ancestry和expected head，不信任workspace的replace/graft结果。
6. 创建不可变`refs/coordplane/tasks/<task-id>/runs/<run-id>`并重新读取确认。
7. SQLite最终事务再次校验Task=finishing、pending action ID/version/run ID、Run generation/terminal，并要求resolved task ref、pending expected SHA、helper/controller验证的head三者完全相等；成功后写head/head Run/task ref/result summary，Task转submitted、清除current Run/pending action并写同operation ID的terminal Event。
8. 删除临时handoff；task ref继续保护commit。

Controller Git命令必须使用 argv，不拼 shell；关闭 system/global config和 hooks，路径参数使用 `--`。

### 6.3 Capture 崩溃恢复

Task pending action字段是未完成capture的当前权威，Event只保留同operation ID的历史。Task/run ref由确定性ID命名，因此不需要GitOperation对象：

- pending capture存在、task ref不存在：只有Task仍finishing、pending action/version/run/generation匹配且workspace已静止时才重试；否则清除/失败并保留任何孤立ref供诊断。
- task ref存在、数据库head为空：必须通过与上面相同的最终fence才可补齐submitted；Task已cancel/retry/new generation时只能保留或GC ref，不能授权状态转移。
- task ref与 intent head不同：标记 `GIT_INVARIANT_VIOLATION`，停止该 Project集成，不任选一方覆盖。
- Dirty/中间态/expected-head不匹配等可修复capture失败：清除pending/current Run，Task从finishing转queued并发送Message；超过retry上限转failed。Ref不一致/对象损坏等invariant失败：Project转error、Task转failed。
- handoff `.partial` 不作为有效输入；只有原子改名后的 `.ready` 可以导入。
- 未知 partial/ready文件必须隔离并在确认无 intent引用后清理。

## 7. 显式接受和机械集成

### 7.1 接受责任

- work Task submitted后必须由 Boss或创建它的父 Task当前 Agent显式 `task accept`。
- Review是普通 Task：需要时，Boss/Agent创建一个引用 source task ref/head的 review Task，由 Reviewer CLI Agent判断；Core不提供 validation对象或 gate。
- `accept`只表达智能决策已经由Boss/Agent做出。代码Task必须从命令显式`--integration-agent A`或Project默认值解析出一个active Agent，即使当前看起来可直接fast-forward；否则返回`INTEGRATION_AGENT_REQUIRED`且不写accepted/pending字段，以保证CAS竞态变stale时一定可收敛。
- Accept开始前读取actual canonical和task ref。SQLite事务CAS校验Task=submitted/version、pending action为空、接受者scope和integration Agent仍active，然后把精确Agent ID写入`accepted_integration_agent_id`，同时写accepted by/at、`pending_action=advance`、operation ID、intent后的Task version、expected current/target head，并写同operation ID的Event。后续Project默认值变化不得改写本次选择。
- pending advance存在期间rework/cancel/第二次accept返回`ACTION_IN_PROGRESS`，避免已撤销结果仍推进canonical。

### 7.2 Fast-forward CAS

Daemon唯一允许自动修改 canonical的算法：

```text
current = resolve(Project.canonical_ref)
head    = resolve(Task.task_ref)

if current == head:
    confirm success
else if git merge-base --is-ancestor head current:
    confirm already integrated
else if git merge-base --is-ancestor current head:
    git update-ref <canonical_ref> <head> <current>
    read back <canonical_ref> and require == head
else:
    create/reuse integration Task
```

要求：

- Task pending action字段是当前advance intent权威；Event只保存使用同operation ID的`git.canonical_advance_requested`历史。
- `git update-ref` 必须携带 expected-old SHA；不得无条件更新、force或 reset canonical。
- CAS失败后重新读取actual canonical。若actual等于head或head是actual祖先，结果已包含并视为成功；若actual仍是head祖先，可以有限重试；否则进入stale。
- 完成/转stale的SQLite事务必须再次校验pending action ID/version/target、Task仍submitted+accepted及`accepted_integration_agent_id`未变，随后清除pending字段。任何不匹配都进入Project error，不能用Git成功绕过已变化授权。
- 只有读取actual canonical确认等于或包含head后，Task才可completed、写final canonical SHA和`git.canonical_advanced` Event。
- 正确性依赖 Git ref CAS，不依赖进程内 integration lock。

### 7.3 CAS 崩溃恢复

存在未完成 advance intent时：

| actual canonical | 行为 |
| --- | --- |
| 等于 intended head | 通过pending action fence后补齐DB completed/Event |
| intended head是actual祖先 | 已被后续canonical包含；通过fence后补齐completed/final SHA |
| 仍等于 expected old | 幂等重试 update-ref |
| actual是intended head的祖先 | 使用新expected old有限重试 |
| 其他 | work source写stale并创建/reuse integration Task；integration Task机械requeue自身，不创建嵌套integration |

Reconciler必须以Task pending action为入口，并重新校验operation ID/version/accepted授权和selected integration Agent。历史Event单独存在不重放。数据库不得因intent存在就显示成功，也不得把canonical回写到旧SHA。

## 8. Integration Task

### 8.1 创建

当已接受 source Task不能 fast-forward时，Daemon幂等创建 `kind=integration` 的普通 Task。结构化上下文至少包含：

- source task ID、source run ID。
- source head SHA和不可变 task ref。
- 创建时 actual canonical SHA。
- Project canonical ref。

Assignee必须使用source Task持久化的`accepted_integration_agent_id`，不得在stale或恢复时重新读取Project默认值/Event。相同Project + source task ref同时最多一个open integration Task；stale事务必须再次确认该Agent未archived，原子创建/reuse并指派它、写source Task.integration_task_id、把链接后的source version保存为integration Task.source_accept_version、清除source pending action并保留accepted状态。Agent此时paused可创建queued Task等待resume；若违反archive fence而已archived，Project转error而不是静默改派。

integration Task创建是已接受 source结果的机械后续，不代表 Daemon判断代码正确。Boss若要放弃或改换方案，必须先显式cancel该integration Task；该事务在确认双方都无pending action后，取消integration、清除source的integration link、accepted字段和`accepted_integration_agent_id`，并让source保持submitted可重新accept/rework/cancel。failed integration仍是open且保持source锁定，只能retry或先cancel。

### 8.2 Workspace

- Integration workspace从创建时 actual canonical SHA的private clone开始。
- Daemon把source commit导入private clone的本地convenience ref；Agent拥有private `.git`，所以该ref不是只读或安全边界，只用于方便CLI操作。真正source身份来自Task.source SHA/ref和controller bare。
- CLI Agent使用原生 Git merge并运行项目测试，自己理解和修复文本/语义冲突。
- 冲突只存在于 private workspace、普通 progress/Message和日志，不创建 ConflictSet。
- Daemon不解析冲突片段、不建议修复、不自动 abort/resolve。

### 8.3 Integration submit

Integration head必须机械满足：

- workspace clean且无 Git中间态。
- 创建时 canonical base是 integration head的祖先。
- source task head是 integration head的祖先。

Workspace内检查只作早期反馈。Bundle导入后，Daemon必须在controller bare中关闭replace refs/grafts并重新验证：actual/创建时canonical关系、source head ancestry、commit类型和captured head。Agent移动convenience ref、写`refs/replace/*`或`.git/info/grafts`不能影响结论。

这意味着第一版集成必须保留 source commit lineage；不支持 squash或仅 cherry-pick后声称包含 source结果。Agent可以创建 merge commit并在其后追加冲突修复 commit。

有效integration submit同样先经历finishing、Run terminal和第6节capture。第6节capture最终事务必须同时重新校验source仍submitted、accepted字段/selected integration Agent未变、source version等于`source_accept_version`、source.integration_task_id指向当前Task且source head/ref完全匹配；随后kind handler在同一事务把integration写为submitted+accepted并启动第7节pending advance，不得留下可被cancel/rework抢占的submitted空窗。这不是新的业务验收。CAS成功后的最终SQLite事务还必须再次执行同一source fence，并同时：

- integration Task completed。
- source Task completed。
- 记录 final canonical SHA和对应 Event。
- 给 source Task创建者/parent发送结果 Message。

CAS再次 stale时：

- canonical不变更、不丢 source/integration task ref。
- 静态integration kind handler机械清除本Task pending advance、执行submitted -> queued，并发送new canonical SHA的Message；不创建嵌套integration Task，也不要求新的智能accept。
- 新Run启动前，Daemon从controller bare为actual canonical生成受限bundle并导入private workspace的convenience ref；不挂载control repo。Agent在同一private workspace中合并后重新提交。
- Task.base_sha保持创建时值，历史 Event记录每次 actual canonical；不得静默改写 base。

## 9. 并发和维护锁

- 多个 Agent private clone可完全并发开发和 commit。
- 多个workspace可并发生成handoff；同一control repo的bundle import/ref维护由短期per-Project维护锁串行，不同Project仍可并发。
- canonical并发推进只靠 expected-old CAS；两个竞争更新最多一个成功，其余进入 retry/stale。
- Repo维护操作，例如初始 import、bundle import、`git gc`、clone准备和临时 ref清理，应使用短期 per-Project进程 mutex或文件锁串行。
- `task create --source-task`、`task checkout`和task-ref删除必须使用同一per-Project维护锁：source Task创建在锁内重新解析ref/head并提交引用字段；checkout持锁直到bundle/导出已保护对象；GC持锁在删除前重新查询SQLite source/pending引用并使用expected-old SHA删ref。
- 该共同锁必须覆盖“GC最后一次DB检查 -> expected-old ref删除 -> reachability复查/prune”和“source ref核验 -> 新Task事务提交”两个窗口，保证二者不能交错。新Task一旦提交，其`source_task_ref`就是持久保留条件。
- 锁顺序固定为先per-Project维护锁、再开启短SQLite事务；任何路径不得持有SQLite事务等待该锁。长时间Agent工作、测试和Docker运行绝不持锁。
- 维护锁不是业务对象，不入 SQLite，不承担 canonical正确性；Daemon crash后不能留下“持有者仍有效”的语义。
- 第一版单 Daemon/data root约束仍适用，不设计分布式 Git lease。

## 10. 回退和修复

CoordPlane不提供 RollbackPoint或历史重写：

- 未集成 Task：取消/废弃 Task，task ref按`retention.terminal_task_ref`保留，不影响 canonical。
- 已进入 canonical的错误：创建普通 work/integration Task，CLI Agent执行 `git revert` 或修复并提交，再走相同 capture/accept/CAS。
- Daemon不得对 canonical执行隐式 reset、force update或删除已集成 commit。
- Boss若要执行危险历史重写，属于本产品之外的人工 Git管理行为。

## 11. Git Recovery

Daemon启动时必须在新调度前执行：

1. 对creating Project继续确定性注册或转error；对active/error Project运行最小repo完整性和ref解析。
2. 读取 actual canonical，修正数据库缓存并记录 drift Event。
3. 核对所有open Task的task ref、head和capture intent。
4. 核对未完成 canonical advance intent。
5. 核对 work/integration Task引用的 source ref仍存在，integration的source accepted version/link仍匹配。
6. 核对 workspace HEAD是否存在未捕获 commit/dirty内容。

关键规则：

- Git ref存在而DB投影缺失时，只有Task pending action ID/version/run/generation全部匹配才可补齐；历史Event不授权恢复。
- DB声称submitted/completed但对应ref或canonical事实不成立时，Project转error并在重启后持续fail-closed；不得伪造ref。只有`project repair`重新核验成功才回active。
- Workspace HEAD新于 task ref时保留 workspace并唤醒/通知原 Agent，不静默捕获或集成。
- Active/可恢复 workspace的 source commit不得被 `git gc` prune。

## 12. Git GC

### 12.1 Workspace 删除

除 `runtime.md` 条件外，还必须满足：

- 最终需要保留的 HEAD已经有不可变 task/run ref，或确认Task无代码结果。
- 没有 capture/advance intent依赖workspace。
- 没有 integration Task依赖workspace本地未捕获内容。

### 12.2 Task ref 删除

只有全部满足才可删除 task/run ref：

- Source Task及其存在时的integration Task都处于completed/cancelled且不可恢复。
- 没有open Task、`source_task_ref`或未完成Task.pending_action引用该ref；已经有terminal配对的历史Event不阻止retention GC。
- Commit已由 canonical ref包含，或Boss用`gc discard-task-ref --task T --run U --expected-sha S --request-id R`显式确认放弃该Run的未集成提交。
- 从source和integration Task中较晚的不可变`closed_at`起已达到`retention.terminal_task_ref`；不存在integration Task时只使用source `closed_at`。

删除 ref和运行 `git gc` 必须分开；ref删除后再次检查 reachability和引用，再由维护锁保护 GC。不得为了节省空间丢失尚未集成的唯一 commit。

Task-ref删除属于`core.md`定义的幂等派生GC：周期worker/`gc run --confirm`只能自动删除已由canonical包含且满足全部条件的ref；未集成唯一commit必须由Boss使用上述单Task命令discard。每次在per-Project维护锁内重读Task/source引用和actual ref，以`git update-ref -d <ref> <expected-old>`或等价CAS删除；崩溃后actual absent视为幂等完成，不增加GitOperation或GC业务对象。

## 13. Git 安全

- Agent输入不能指定宿主 repo/cache path、任意 ref、任意 executable或 shell片段。
- 所有服务端 Git命令使用 argv和固定可执行文件；路径参数使用 `--`。
- 禁用/隔离 Agent可控 hooks、credential helper、filters、pager、editor和 upload-pack配置。
- Controller ancestry/fsck命令必须使用`--no-replace-objects`或等价环境，忽略Agent workspace grafts/replace refs，只在control bare中验证Task保存的精确SHA。
- Bundle/pack设置大小、对象数、处理时间和磁盘限额。
- Controller不向 Agent容器注入 source remote写凭据。
- Container不能访问 control bare repo或其 filesystem path。
- Agent-facing错误和状态不得泄露 host path。
- Capture前必须重新检查 Task current Run/generation；旧 Run不能更新新一代Task head。
- Cleanup、capture、resume和GC并发时，active/capture intent优先，cleanup退出。

## 14. Remote 和多仓库非目标

第一版不实现：

- 自动 fetch/push、Git smart HTTP/SSH server。
- GitHub/GitLab PR、webhook、CI状态、branch protection或merge queue。
- Remote credential托管。
- 跨仓库Task和原子集成。
- Submodule自动凭据和递归发布。

这些能力未来只能作为显式 adapter加入，不能改变本地 canonical/task ref和CAS合同。

## 15. Git 不变量

- Git objects/refs是唯一代码真相，SQLite只保存SHA/ref索引。
- Docker Agent使用无 alternates、无shared objects、无可写control remote的private clone。
- Agent可直接用原生Git，但只能改变自己的private clone。
- Task base SHA固定；Daemon从实际HEAD捕获结果，不信任文字或分支名。
- Dirty workspace、Git中间态或base非祖先结果不能提交。
- Submit先进入finishing并终结workspace writer；Commit再由不可变task/run ref保护，最终fence通过后才写Task submitted投影。
- Canonical只通过“head已被包含”确认或fast-forward expected-old `git update-ref`更新。
- Non-fast-forward/stale交给integration CLI Agent，Daemon不merge/rebase/解决冲突。
- Integration head必须包含current canonical和source head的祖先lineage。
- CAS/Daemon崩溃通过intent + actual ref收敛，不建设GitOperation对象。
- Active、可恢复、未捕获或仍被integration引用的commit/workspace/ref不得GC。
- 已集成错误通过普通revert/fix Task修复，不隐式重写canonical历史。
