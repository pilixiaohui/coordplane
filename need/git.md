# CoordPlane Git 协作需求

状态：候选冻结基线，待需求审批人复核精确 revision
版本：1.0-rc3
日期：2026-08-16
依赖：`README.md`、`core.md`、`runtime.md`

## 1. 目标和边界

Git模块负责：

- 从本机已有repository注册每个Project的daemon-owned control repo和canonical ref。
- 为每个 `workspace_mode=git` Task创建私有workspace并固定base/source输入。
- 让Human在宿主机、CLI Agent在Docker内使用同一标准Git工作模型。
- 从actual committed HEAD捕获不可变submission ref，使用expected-old CAS推进canonical。
- canonical stale时创建显式integration Task，支持崩溃恢复、冲突保留和安全GC。

Git模块不判断代码正确，不代理通用Git命令，不自动发布远端，不按Participant kind或职责改变accept/integration规则。

## 2. 代码真相和仓库布局

每个Project只有一个daemon-owned bare control repo：

```text
control-repos/<project-id>.git/
  refs/heads/<canonical>
  refs/coordplane/tasks/<task-id>/submissions/<submission-id>
  refs/coordplane/runs/<run-id>                         # 可选恢复/诊断ref
```

每个git Task只有一个私有workspace：

```text
workspaces/<project-id>/<task-id>/
```

权威顺序：

1. 实际Git object和ref。
2. SQLite中固定base/head/submission/task ref和pending intent。
3. Event与行为日志只作审计，不授权ref变化。

约束：

- `canonical_sha`是缓存；actual canonical ref不一致时先reconcile，禁止缓存覆盖actual。
- task submission ref创建后不可移动。rework后的新结果使用新submission ID/ref，不覆盖旧ref。
- Human与CLI Agent提交都必须有stable `submission_id`。Agent成功submit可使用capture operation ID并另记`head_run_id`；Human `head_run_id=null`。
- Project、Task、submission ID先校验固定格式，再由服务端构造完整ref；调用者不能提供raw ref。

## 3. Project注册和导出

### 3.1 注册

`project add` 只接受本机Git repository路径和完整branch ref：

1. 只读preflight解析source ref为精确commit `initial_sha`，拒绝ambiguous/revision表达式、SHA-256 object format、未初始化repo和非法ref。
2. SQLite事务创建Project `creating`、固定source/ref/initial SHA和initialize operation。
3. 在临时目录从source复制/拉取必要object，创建bare control repo和canonical ref；不得修改source。
4. 核验actual ref、`git fsck`和ownership后，SQLite CAS清pending并active。

同名/同source并发注册必须通过Project identity和CAS收敛为一个结果。崩溃后reconciler根据pending和actual repo继续或error，不重新读取已移动的source ref作为新initial SHA。

### 3.2 导出

- `project checkout PROJECT --dest PATH` 从actual canonical创建普通checkout，确认HEAD后移除control remote。
- `task checkout TASK --dest PATH` 从Task保存的精确submission ref/head导出，确认HEAD并移除control remote。
- 目标必须不存在或为空，禁止覆盖已有Human工作目录。
- 导出用于只读审查或外部工作，不是Task私有workspace，也不能直接submit回CoordPlane。
- 远端GitHub/GitLab发布由Participant使用标准Git在CoordPlane之外执行，不是v1完成条件。

## 4. 私有workspace

### 4.1 创建和ownership

- 每个git Task一个workspace，owner marker固定Project、Task、assignee Participant、base/source SHA和创建operation。
- 普通work Task从actual canonical的固定`base_sha`创建；带source Task时另外导入固定source ref/head。
- integration Task首轮从不可变`integration_initial_canonical_sha`创建；后续轮从当轮`integration_expected_canonical_sha`继续，并导入source submission convenience ref。
- Task创建事务只固定输入并写`workspace_state=pending`、稳定prepare operation ID和workspace identity。Git workspace worker是唯一preparer，核验目录、marker、HEAD、source和ownership后CAS为`ready`；任一不确定为`blocked`。
- workspace `.git` 与其他Task完全隔离；不能共享index、worktree metadata、rebase/merge状态或锁文件。
- workspace不能拥有更新control repo canonical/task refs的remote凭据。对象回传由Daemon受信helper完成。

### 4.2 Human与CLI Agent访问

- Human Task的workspace `ready`后，授权Human可从专用Operator CLI/Web host operation获得服务端生成的宿主路径；只有该Task assignee或同时具备`git.workspace.read`的Human可获得。路径不进入coordlink、Agent bootstrap、通用status/Event/日志/错误。
- CLI Agent只在自己的Task Run中看到 `/workspace`，API不得暴露宿主绝对路径。
- 两类Participant都在workspace直接使用标准Git进行status/diff/add/commit/rebase/merge。
- CoordPlane不记录Human在宿主机的每个shell行为；只记录经Service进行的claim/submit和capture时Git前后事实。CLI Agent在Run内的可观测Git/tool行为按`runtime.md`进入行为日志。
- Human与CLI Agent都不能用kind绕过clean、expected-head、source、capture或CAS检查。

### 4.3 生命周期

- Task retry复用原workspace；rework保留旧submission ref并在同workspace继续，提交新submission。复用前必须重新核验workspace identity/marker；需要恢复准备时沿用原operation ID，不创建第二目录。
- assignee变化前必须证明无Run、无Human active claim、无pending capture和workspace ownership可转移，并写Event；否则拒绝。
- active/running/finishing、dirty、Git中间态、唯一未捕获commit或ownership不明的workspace不得自动删除。

## 5. Task基线和stale可见性

- git Task创建事务前在Project维护锁内读取actual canonical，事务保存不可变`base_sha`。integration Task另外保存不可变initial canonical和每轮可CAS更新的expected canonical。
- workspace HEAD初始必须等于base，或integration Task当轮expected canonical所要求的已核验准备结果；`workspace_state=ready` 前Task不能claim/Run active。
- canonical后续移动不自动rebase/restart已有Task。参与者通过Message/status看到base与current canonical差异。
- 需要最新基线时，由有权限Participant显式rework/retry或创建新Task；Daemon不根据stale自动丢弃工作。
- source Task输入在新Task创建事务复制`source_task_ref/source_head_sha`，后续source状态或ref GC前置必须尊重该持久引用。

## 6. Submit与结果捕获

### 6.1 共同前置条件

git Task submit必须提供`expected_head`并满足：

- Project active，Task running，caller是assignee且有`task.submit`。
- HEAD解析为commit并等于expected head；commit从Task固定base/source可达，禁止无关替换历史。
- index/worktree clean，无untracked文件，无merge/rebase/cherry-pick/bisect等中间态，无锁文件。
- commit/object/path数量、bundle大小和Git命令时限在静态安全上限内。
- Task无pending action或open integration授权。

CLI Agent必须先在同一事务写requested submit、撤销Run token并进入finishing；真实Run terminal、container writer停止后才能capture。Human没有Run：submit事务直接进入finishing并创建稳定capture operation/submission ID。

### 6.2 Human并发快照

Daemon无法假设宿主Human进程已停止写workspace，因此Human capture必须使用稳定快照协议：

1. submit事务固定`expected_head`、submission ID、Task version和capture intent。
2. helper获取per-Task维护锁，读取HEAD、clean/in-progress状态及workspace fingerprint F1。
3. 只按不可变expected commit生成有界bundle/pack，不跟随之后移动的workspace HEAD。
4. 再读取HEAD、clean/in-progress状态和fingerprint F2。
5. F1/F2不一致、HEAD移动或workspace变脏时，隔离临时对象，清pending并回到running，返回`WORKSPACE_CHANGED`；不得捕获新HEAD或声称成功。
6. F1/F2稳定时才导入quarantine并继续共同capture。

Human在快照后继续产生的新commit不属于本submission，必须保留workspace并通过新rework/submit显式处理。

### 6.3 受信capture

受信helper不得信任workspace配置、hook、alternates、replace refs或环境：

- 使用固定Git executable、最小环境、禁用hooks/global/system config、清理危险`GIT_*`变量。
- 按expected commit创建bundle/pack到quarantine，校验header/object count/size、commit类型和reachability。
- 将对象导入control repo quarantine，执行fsck/connectivity后才进入主object store。
- 以 `git update-ref <submission-ref> <head> <zero>` 或等价create-only CAS创建不可变ref。
- 回读ref和commit后，SQLite CAS确认Task仍finishing、pending operation/version/head/submission匹配，写`head_sha/head_submission_id/task_ref`；Agent另写`head_run_id`，Human保持空；清pending并submitted。

任何校验失败不得创建/移动canonical。可修复dirty/changed/expected mismatch让Task回running（Human）或queued/backoff（CLI Agent）；对象损坏、ref冲突或control repo invariant失败使Project error并保留诊断。

### 6.4 Capture崩溃恢复

reconciler按pending和actual submission ref处理：

- pending存在、ref不存在：只有Task仍finishing且operation/version/head匹配时重试capture；否则清pending并保留workspace。
- ref存在、SQLite head为空：只有相同最终fence全部成立才补写submitted；Task已cancel/rework/generation变化时ref只保留诊断/GC，绝不能推进状态。
- ref存在且SHA不等于pending target：Project error，不覆盖、不删除。
- SQLite已submitted且ref匹配：幂等完成。

Event或日志不能补齐缺少的pending授权。崩溃点测试必须覆盖intent前后、object import前后、ref创建前后和SQLite terminal事务前后。

## 7. 显式接受和canonical CAS

### 7.1 接受责任

- submitted Task只能由具备`task.accept`和`git.accept`的Participant显式accept；creator、Human或Agent身份不产生隐式权力。
- accept只表达Participant已作业务判断，Daemon不审查代码。review是普通Task，可引用source submission。
- git Task accept必须从命令或Project默认值解析一个active integration Participant，并固定到`accepted_integration_participant_id`；Human和CLI Agent均可。缺失返回`INTEGRATION_PARTICIPANT_REQUIRED`且零副作用。
- accept事务固定actual canonical expected SHA、target head、Task/ref/version、接受者和integration Participant，写advance intent和Event。

### 7.2 Fast-forward expected-old CAS

Daemon在Project维护锁内：

1. 回读actual canonical和Task submission ref，核验SHA、object和lineage。
2. 若actual canonical等于Task base/expected且target可fast-forward，执行`git update-ref canonical target expected-old`。
3. 回读actual canonical；只有等于target才可提交Task completed/final SHA。
4. 若expected-old失败，读取actual。如果actual已等于target且pending匹配则幂等完成；否则标记`CANONICAL_STALE`并进入integration流程。

禁止merge、reset、force、cherry-pick或用数据库缓存覆盖actual canonical。完成事务再次校验pending ID/version/target、Task accepted事实和固定integration Participant。

### 7.3 Advance崩溃恢复

- pending存在且canonical仍expected：重试同一个expected-old CAS。
- canonical已target：核验后补写completed。
- canonical为第三个SHA：不得回退；转stale并创建/reuse唯一integration Task。
- target/ref/object损坏或pending授权不匹配：Project error。

## 8. Integration Task

### 8.1 创建

stale事务原子创建或复用同Project/source submission唯一open integration Task，固定：

- assignee=`accepted_integration_participant_id`，可以是Human或CLI Agent。
- `workspace_mode=git`、source Task/run/submission/ref/head。
- source Task/run/submission/ref/head和source当前version `source_accept_version`，之后均不可改。
- 创建时actual canonical同时写为不可变`integration_initial_canonical_sha`、首轮`integration_expected_canonical_sha`和`integration_round=1`，并在source写`integration_task_id`。
- workspace按第4节先进入pending，准备成功后才ready。source链路、initial canonical和已产生的integration commits在重试/后续stale轮不得改写或丢弃。

assignee archived/paused或无Project访问时不得静默改派；保持source submitted并报告阻塞，由有权限Participant显式取消原授权后重新accept。

### 8.2 执行

- integration workspace首轮从initial canonical创建并导入source convenience ref；后续轮以当轮expected canonical为新集成输入，但复用同一workspace identity并保留已提交冲突修复。
- Participant使用标准Git merge/rebase/cherry-pick或冲突修复，但最终history必须包含source head为ancestor；不支持squash后声称包含。
- Human显式claim并在宿主workspace工作；CLI Agent由Docker Run执行。提交/capture完全复用第6节。
- 冲突留在私有workspace，通过Task/Conversation/Message和行为日志可见，不创建ConflictSet业务对象。

### 8.3 Integration submit与再次stale

integration结果必须同时满足：

- source head是result head ancestor。
- 当轮integration expected canonical是result head ancestor。
- workspace clean且capture成功。

accept后以当轮`integration_expected_canonical_sha`为expected-old推进actual canonical。若再次stale，静态integration handler在同一事务中机械执行submitted -> queued，把actual canonical CAS写为新`integration_expected_canonical_sha`，`integration_round += 1`，将workspace准备置pending并发送幂等Message。不变initial canonical/source字段，不得创建嵌套integration Task、自动更换assignee或静默丢失source head/已提交冲突修复。

integration完成后同一事务完成source Task，双方保存相同`final_canonical_sha`并清链接pending。取消integration必须原子取消integration、清source授权/link，使source回submitted可重新accept/rework/cancel。

## 9. 并发和维护锁

- Project ref mutation使用per-Project维护锁；Task prepare/capture和Human快照使用per-Task锁。
- 锁只协调进程内/单Daemon操作，不作为durable授权；崩溃恢复依赖SQLite pending和expected-old ref。
- GC最后DB检查到expected-old ref删除、source ref核验到新Task事务提交必须受同一Project锁，避免删除刚成为source的ref。
- 并发accept最多一个expected-old CAS成功；失败者读取actual并进入stale，不得覆盖winner。
- 不同Task workspace无共享可写文件，可真实并行；并行上限是Runtime配置，不是Participant数量上限。

## 10. 回退和修复

- 未集成错误结果：rework/cancel，submission ref按retention保留，不影响canonical。
- 已集成错误：创建普通git Task，由Participant执行`git revert`或修复，再走相同capture/accept/CAS。
- Daemon不提供隐式rollback/reset/history rewrite。
- 危险远端或历史重写属于CoordPlane外的显式Human Git操作，不是v1自动能力。

## 11. Git Recovery

启动在调度前核验：

- control repo存在、ownership正确、canonical ref合法且object可读。
- SQLite cache与actual ref一致或进入可解释reconcile。
- Task base/source/head/submission ref和integration lineage一致。
- workspace marker、HEAD、status和Git中间态与Task状态兼容。

恢复规则：

- `workspace_state=pending`时，根据稳定operation ID/identity检查预期目录；absent则继续create，matching且完整则补ready，partial或ownership冲突则blocked，不使用新目录规避。
- `ready`但目录absent、identity/marker不匹配或HEAD违反Task状态时转blocked并停止claim；只有可证明writer从未启动且依据不变base/source可安全重建时，显式repair才能沿用原identity重试。
- workspace HEAD新于已捕获submission时保留并通知assignee，不静默捕获或删除。
- Task submitted/completed但ref缺失或SHA不匹配使Project error。
- Run terminal但workspace dirty/中间态时Task回queued/failed并保留workspace；Human running workspace不由reconciler猜测完成。
- orphan ref/object只在证明无Task/source/pending引用且达到retention后GC。

## 12. Git GC

### 12.1 Workspace

自动删除必须同时满足：

- Task completed/cancelled且达到`retention.completed_workspace`。
- 无active Human claim、starting/active/recoverable Run、pending action或open integration/source引用。
- 需要保留的HEAD已有不可变submission ref或可证明从未产生代码结果。
- workspace clean、无中间态、ownership/path/symlink检查通过。

dirty/untracked/唯一未捕获commit只能由具备`git.discard`的Participant使用单Task命令，携带preview得到的expected fingerprint和request ID显式放弃；不能覆盖active/pending/ownership/source fence。

### 12.2 Submission ref

自动删除必须同时满足：

- source及其integration Task均completed/cancelled且不可恢复。
- 无open Task、source_task_ref或pending action引用。
- commit已由canonical包含。
- 从相关Task较晚`closed_at`起达到`retention.terminal_task_ref`。

未进canonical的唯一submission ref只能通过`gc discard-task-ref --task T --submission S --expected-sha H --request-id R`显式放弃。删除使用完整服务端ref和expected-old SHA；absent视为幂等完成。ref删除与`git gc`分开，prune前再次检查reachability和引用。

行为日志GC遵循`runtime.md`，不得因删除Git ref连带删除日志或反之；日志只引用SHA。

## 13. Git安全

- 固定Git executable和最小环境；禁用hooks、global/system config、protocol扩展和调用者alias。
- source/workspace视为不可信；alternates、replace refs、symlink、submodule和object format必须显式校验或拒绝。
- bundle/object count、size、path length、命令duration和磁盘余量有上限，失败fail loud。
- control repo、raw ref和宿主私有路径不暴露给Agent或未授权Web/API响应。
- 每个外部Git动作记录operation ID、before/after SHA和结果Event；CLI Agent内可观测Git命令另进入行为日志。

## 14. Remote和多仓库非目标

v1不实现自动fetch/push、Git smart HTTP/SSH、remote credential、PR/CI状态、merge queue、跨仓库Task、submodule凭据或远端发布。Project注册仅接受本机repo，canonical仅在daemon-owned control repo。

## 15. Git不变量

1. actual Git ref/object是代码真相，SQLite/Event/日志不能覆盖。
2. Human与CLI Agent使用同一git Task、private workspace、expected-head、capture、submission ref、accept和CAS。
3. Human capture使用expected commit和双fingerprint稳定快照，不假设宿主writer停止。
4. 每次成功submit有不可变submission ID/ref；rework不移动旧ref。
5. canonical只由显式accept后的expected-old CAS推进；stale不覆盖并进入显式integration Task。
6. integration Participant可为Human或CLI Agent，身份不改变Git合同。
7. accepted source和open integration的授权/link不可被竞争mutation绕过。
8. crash恢复只依据pending intent、actual ref和版本fence；Event/日志不授权补写。
9. active、dirty、唯一未捕获结果、source引用或ownership不明的workspace/ref不得自动GC。
10. helper、handler、recovery和GC step通过静态列表注册，替换旧run-based-only capture时同变更删除旧路径。
11. workspace只有一个稳定identity和prepare operation；`pending/ready/blocked/removed`与实际目录可恢复对账，未ready不得启动writer。
12. integration source/accept version/initial canonical不可变；每次stale只单调更新expected canonical和round，并保留同一Task/workspace内已有工作。
