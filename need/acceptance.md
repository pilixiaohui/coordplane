# CoordPlane v1 验收需求

状态：候选冻结基线，待需求审批人复核精确 revision
版本：1.0-rc3
日期：2026-08-16
依赖：`README.md`、`core.md`、`runtime.md`、`git.md`

## 1. 目标和边界

本文件把四份产品需求转换成可执行约束。它不是CoordPlane内部的validation/acceptance业务对象，也不保存发布verdict。

验收必须证明：

- Participant是Human/CLI Agent唯一业务身份，职责完全配置化且所有入口权限一致。
- Human与CLI Agent共用Task和Git结果合同，Human仅省略Docker Run。
- 一对一/群组Conversation、显式多recipient、逐接收者未读和定向wake在并发/重启下不丢失。
- Task Run和Conversation Run均受单Agent、generation、token和真实进程fence约束。
- 子Task结果通过固定Conversation/recipient幂等回传，waiting parent可被定向唤醒并恢复同一Task session scope。
- Git结果来自actual clean HEAD和不可变submission ref，canonical只由显式accept后的expected-old CAS推进。
- workspace准备、integration多轮stale和Resume跨scope拒绝在崩溃/并发下保持唯一可恢复真相。
- CLI Agent全部可观测行为形成可查询、可校验、已脱敏行为日志，并遵守7天默认、Project override和`long_term`。
- 本机Web UI能完成协调和监控工作流，但不能编辑代码、打开宿主终端或修改raw Git ref。
- migration、取消、超时、Daemon崩溃、capture/CAS崩溃和GC不产生伪完成、越权、消息丢失、代码丢失或日志误删。
- 每个拟验收candidate在L1-L4通过后，都使用指定小说系统需求完成一轮真实多Agent代码开发与行为日志审计。

不要求：

- 保存provider隐藏chain-of-thought、加密推理或CoordPlane无法观察的行为。
- 在产品数据库保存验收报告、证据bundle或release状态机。
- 远程登录/runner、远端Git发布、自动业务验收或性能SLA。
- 用一个巨大live gate替代低层契约测试。

候选运行结果、当前缺陷和“已通过”状态必须写入外部收据，不得回填本规范正文。

## 2. 收敛测试规则

每个重要测试必须保护一个命名不变量，并说明：

```text
Invariant
Risk layer
Production entrypoint/boundary
Red behavior against the old design
Positive state assertion
Forbidden side effect
Fault or misuse case
Mocks allowed / forbidden
Verification command
```

要求：

- 使用能覆盖风险的最低真实边界；公开合同使用正式CLI/API/Web入口。
- SQLite状态测试回读真实file-backed数据库和Event；Git测试读取actual ref/object；Runtime测试读取真实Docker/OS事实；日志测试读取实际文件/hash/index。
- 每个失败路径同时断言业务行、Event、ref、container、日志和recipient中不应发生的副作用。
- 并发、取消、worker、日志writer和共享状态改动必须运行race gate。
- live失败先归约成最低可重复测试；低层变绿后才重跑高层场景。
- L5每轮只选一个小说系统不变量/可独立Task slice。小说需求只是负载；修复CoordPlane必须指向通用Participant/Task/Message/Run/Git/log/Web契约，禁止按项目名、小说术语、Participant名或固定Task ID特判。
- 禁止固定sleep断言异步完成；等待具体状态/Event/ref/container/log sequence并设置deadline。
- 新机制替换旧机制时，同一变更删除旧表/字段/入口/fixture/正向测试；保留证明旧路径不可返回的负向guard。
- 高层gate失败不能用低层通过抵消，不同candidate SHA的证据不能拼接。
- “每次修改后重跑”的机械边界是一个已提交、可部署、具有唯一SHA的CoordPlane candidate，不是每次编辑器保存。任何产品修复产生新SHA后，旧candidate的RMA-03不得复用为新candidate收据。

SQLite startup的无副作用断言允许`-shm`内部WAL-index/lock字节变化，但主数据库、`-wal`业务内容、schema和业务行必须保持；测试不得把只读preflight产生的合法SQLite进程态误判为migration。

## 3. 证据层级

| 层 | 目的 | 必须使用的边界 | 失败规则 |
| --- | --- | --- | --- |
| L1 Static/lint | 删除旧模型、注册结构、格式和schema形状 | 源码/SQL/文档扫描、gofmt、vet | 不过即停 |
| L2 Contract | 冻结Participant/Task/Conversation/Message/log/Git不变量 | 真实SQLite、公开Service/CLI、临时Git、adapter transcript | 不过即锁未开 |
| L3 Full/race | 防共享逻辑回归 | `go test ./...`、`go test -race ./...` | 不过即停 |
| L4 Deterministic real boundary | 证明SQLite+Docker+Git+Web组合不变量 | 真实Daemon/Docker/browser，scripted adapter | 不过即停 |
| L5 Real CLI/reference workload | 证明production adapter、多Agent组合闭环和持续真实项目开发 | 真实Claude/Codex CLI、Docker、固定外部需求manifest和项目Git | 新问题先分类，产品finding转最低层红测试 |

Mock只允许位于被测边界之外。禁止用内存map验证SQLite claim、字符串变量验证Git CAS、fake Docker验证mount/kill、handler直调冒充CLI/Web、notifier mock冒充Message durability或构造日志行冒充真实CLI采集。

## 4. 静态和架构约束

### 4.1 文档一致性

- `need/`可执行权威集合精确为`README.md/core.md/runtime.md/git.md/acceptance.md`五份规范，均有相同版本、日期和候选状态。目录另有且只有一份`user-requirements-verbatim.md`溯源记录，它不是第六份产品行为规范。
- 原话记录的`UR-NNNN`单调、唯一且无缺号；从首次冻结后，旧前缀必须字节级不变，新记录只能在`后续追加区`末尾增加。需求规范有变更时，diff/issue/收据必须引用本轮新增`UR-NNNN`。
- 原话记录进入常规secret scanner；凭据仅允许`[敏感凭据未入库]`、脱敏原因和不可逆摘要，不允许可恢复原值。
- 对象/FSM只在`core.md`定义；Runtime/Git只扩展外部字段和边界。
- 文档不得把SQLite称为代码真相、把Git称为消息真相、把行为日志称为Task授权。
- 静态guard禁止生产语义出现 `Boss`、`participant-owner`、`role-owner`、`role-agent`、Human专用Task FSM、`conversation Task`、`evidence_type=human_confirm`和`agents`镜像。
- 旧词仅可出现在migration拒绝错误、负向fixture或“禁止重新引入”的说明中。
- 不允许未在五份规范内定义的产品决策编号、可变“当前工作”、历史PASS或candidate执行结果。`UR-NNNN`只是原话溯源ID，不是产品决策或业务对象。

### 4.2 Schema exact-set

空库migration后的表精确为：

```text
projects
participants
roles
participant_project_roles
credentials
tasks
conversations
conversation_members
messages
message_recipients
runs
events
behavior_log_indexes
schema_migrations
request_dedupes
```

禁止`agents`、conversation task辅助表、DeliveryAttempt业务状态机、Validation、ConflictSet、GitOperation、Artifact或第二套权限表。`conversation_members`、`message_recipients`和`behavior_log_indexes`分别是正式关系/索引事实，不得退化为Message JSON blob或内存状态。

### 4.3 删除旧入口

静态guard必须阻止：

- 通用`/call`、动态capability/skill registry、TeamConfig/职责策略DSL。
- `coordlink call NAME`、raw DB、raw Git ref和Agent-facing通用Git wrapper。
- 通过CLI文本、Event、日志或Human确认字段直接写Task completed。
- 单recipient字段作为群组Message权威，或一个Message聚合state覆盖逐recipient状态。
- Run只允许task_id非空的旧假设，以及用conversation Task唤醒Agent。
- provider secret值进入Docker Config.Env/argv/labels、SQLite、Event、日志或Web响应。
- Web代码编辑器、浏览器终端、任意宿主文件读取和raw ref mutation入口。

### 4.4 Build to Delete和Continuous GC

以下组件必须由独立函数和静态列表注册：

- Service operations及capability descriptor。
- workers、Run source、Task kind handlers。
- CLI adapters、behavior parsers/normalizers/redactors。
- Runtime prepare/cleanup、Git capture/recovery、GC steps。
- acceptance scenarios和Web导航/fixture清单。

删除一个组件只移除注册项和实现。静态质量gate同时检查未使用import/function、生产单函数和单文件阈值；本轮直接影响的超长函数必须拆分，禁止新增TODO债务或保留旧兼容writer。

## 5. 命名不变量

| ID | 不变量 |
| --- | --- |
| INV-01 | SQLite保存协调事实，actual Git保存代码事实，Docker/OS保存运行事实，Behavior Log保存可观测行为；四者不能互相伪造 |
| INV-02 | Participant是Human/CLI Agent唯一身份和Agent配置权威；不存在Boss或agents镜像双真相 |
| INV-03 | kind只改变认证和执行介质；Task、Conversation、Message、Role和Git合同不按kind或职责分叉 |
| INV-04 | Role名称无语义；所有入口通过同一operation registry、scope和capability检查，拒绝零副作用 |
| INV-05 | Task与Run分离；Human无Run，CLI Agent由真实Run claim；两者结果都先submitted再显式accept |
| INV-06 | 同一CLI Agent最多一个starting/active Run；Task Run与Conversation Run共同受Participant generation fence |
| INV-07 | Conversation独立于Task，支持direct/group；Message必须属于Conversation且Task关联可空 |
| INV-08 | Message显式一个或多个recipient，各自独立未读/delivered/ack/cancel/retry；非recipient不被唤醒 |
| INV-09 | wake=false不创建Run但下次同Project Run必须通过count/high-watermark/有界样本/cursor知道并可分页读取未读；wake=true只为明确CLI Agent recipient创建/合入Run |
| INV-10 | Message先durable后递送，Inject失败/Run退出/Daemon重启不丢失，逐recipient至少一次 |
| INV-11 | Conversation Run无Task和代码workspace；Message关联Task不隐式授权代码访问 |
| INV-12 | Resume创建新Run且不跨Participant/Project/Task或Conversation scope/adapter/config/workspace identity；terminal Run不复活，旧token/generation不能写新状态 |
| INV-13 | 每个git Task有唯一私有workspace identity和可恢复`pending/ready/blocked/removed`；Human宿主路径与Agent容器挂载保护同一base/source/ownership |
| INV-14 | Git结果来自actual expected HEAD和clean状态；Human双fingerprint变化时不能捕获 |
| INV-15 | 每个成功submit产生不可变submission ref；Event、summary或日志不能替代ref |
| INV-16 | canonical只由显式accept后的fast-forward expected-old CAS推进；竞争stale不覆盖 |
| INV-17 | integration Participant可为Human或CLI Agent；source/accept version/initial canonical不可变，再次stale只单调更新expected canonical/round并保留已有工作 |
| INV-18 | pending operation和actual外部事实驱动capture/CAS/Run恢复；Event不充当隐藏Operation |
| INV-19 | active、dirty、唯一未捕获、被source引用、ownership不明或long_term资源不得GC |
| INV-20 | 每个Run从首字节记录raw redacted和normalized行为流；未知/gap/truncate/redaction显式可见 |
| INV-21 | 行为日志覆盖provider暴露的tool/shell/coordlink/Run/Git事实，不声称记录隐藏推理，不推进业务状态 |
| INV-22 | 每Run唯一Behavior Log index是retention/long_term真相；默认168h，Project可覆盖，取消long_term后按原ended_at和当前策略重算 |
| INV-23 | secret落盘前脱敏，Docker inspect/argv/SQLite/Event/log/Web均无secret值；日志hash/index可恢复 |
| INV-24 | Web只监听loopback、必须认证、复用Service operation，对CSRF/Origin/Host/CORS/CSP/Cookie/XSS/SSE-WebSocket失效安全，并覆盖Task/Conversation/Run/log/Git状态工作流 |
| INV-25 | Participant/Task/Conversation数量无产品级上限；max_parallel_runs只限制Docker并发 |
| INV-26 | terminal成功状态清除旧failure/wait原因；CLI exit 0、文本完成或低层green不能伪造完成 |
| INV-27 | 子Task结果通过固定coordination Conversation/result recipient和唯一system Message回传，waiting parent的定向wake与通知同事务且重启幂等 |
| INV-28 | 行为日志只按log.read_own/log.read_project授权；Conversation成员资格不授予混合Run日志 |
| INV-29 | 每个验收candidate均以固定需求manifest和输入canonical完成一轮reference workload；L5 finding先归约低层再修复 |

## 6. Core合同场景

### CT-01 Migration、bootstrap和重启

Boundary：真实file-backed SQLite + 正式启动/bootstrap入口。覆盖INV-01/02/18。

必须断言：

- 空库产生4.2 exact-set，无`agents`或默认owner/agent seed。
- 非本机、已有Participant或Daemon已开放mutation时bootstrap拒绝且零副作用。
- 首次bootstrap创建一个普通Human、Credential、可配置管理Role/binding；之后可将管理capability配置给另一Human或CLI Agent。
- 删除/降级最后管理者拒绝；增加第二管理者后可变更首个，证明无特殊身份。
- 二次migration幂等；旧不可判定schema稳定返回`LEGACY_SCHEMA_REBUILD_REQUIRED`，不启动scheduler/Web mutation。
- 创建全部对象后重启，row/version/Event/recipient/log index保持；migration中断不产生半schema。

### CT-02 Participant、Role和transport一致性

Boundary：Service + Operator CLI + coordlink + Web API。覆盖INV-02/03/04/25。

- 创建多个Human/CLI Agent，不存在硬数量上限或按名称分支。
- 同一Role/capability对Human和CLI Agent调用同一operation得到相同授权结果；kind只影响Run/host path物理前置。
- 项目scope、global scope、跨Project、非Conversation member、错误assignee均有负例并断言零行/Event变化。
- CLI、Web、coordlink的成功结果和错误码一致；不得有某入口绕过expected version/idempotency。
- paused不接收新Task指派或Conversation成员关系，已有Task/成员关系/历史保留；Human可继续已有工作，CLI不启动新Run且当前Run不被静默停止。archived不认证/接收，历史仍按权限可读。

### CT-03 统一Task FSM

Boundary：真实SQLite + Service公开入口。覆盖INV-03/05/26。

- 同样的work Task分别指派Human和CLI Agent，初始都queued。
- Human显式claim后running且`current_run_id=null`；Scheduler不能为Human创建Run。
- CLI Agent只有Run真实active后Task才running；claim竞争只创建一个Run。
- Human与Agent均可wait->waiting->wake->queued、fail->failed->retry、submit->submitted、accept->completed。
- 不存在Human `waiting->completed`/`human_confirm`旁路；旧操作稳定拒绝且不写Event。
- completed清旧failure/wait字段；exit 0或Message正文“完成”不能改变Task结果。

### CT-04 群组Conversation和逐recipient未读

Boundary：真实SQLite + CLI/API。覆盖INV-07/08/10。

- direct恰有两成员且无序pair唯一；group至少两成员并支持增加/移除。
- paused/archived Participant不能成为新Conversation成员；暂停时已有成员仍按权限可读历史且不产生新Run。
- 一个group Message显式发给B/C，不发给D：创建一个Message、两个recipient行；B ack不改变C，D无未读且不被wake。
- Task关联为空和非空均可；cross-Project Task、非成员recipient、零recipient和sender自收稳定拒绝且零副作用。
- 成员都可按权限读取历史；离开后不能发新消息或成为recipient。
- recipient在delivered Run退出未ack后回pending；Daemon重启后count/order/retry不变。
- Project/Conversation/Participant archive前未处置recipient必须显式ack/cancel/redirect，不得回退到固定Human。

### CT-05 未读提示和定向wake

Boundary：真实SQLite + scheduler/notifier + scripted adapter。覆盖INV-06/09/10/11。

- wake=false不创建Run；Agent下一次同Project Task Run bootstrap含未读count、高水位、有界Message/recipient ID样本和cursor。创建超过bootstrap上限的未读，断言bootstrap大小不增长为全量，通过稳定分页可无漏无重读到高水位。
- wake=true只为明确CLI Agent recipients启动/合入Run；Human和group非recipient不产生Run。
- 无Task wake创建`task_id=null` Conversation Run且bootstrap含Conversation/Message；容器无workspace mount。
- Agent busy时不创建第二Run；Inject失败保持pending，terminal后Conversation Run再投递。
- 多消息合入仍逐recipient ack，达到max deliveries停止自动wake但保持未读可查询。

### CT-06 Run fencing

Boundary：真实SQLite + per-Run API。覆盖INV-05/06/12。

- Task Run和Conversation Run均递增Participant runtime generation并占用唯一current Run。
- 旧token、错Participant/Project/Run/runtime generation拒绝所有mutation。
- Task outcome另校验task ID/generation；Conversation Run不能提交Task outcome。
- cancel/stop/terminal立即撤销token；迟到ack/outcome不得覆盖新Run，但可按Message idempotency安全返回已ack结果。

### CT-07 接受、rework和取消竞态

Boundary：真实SQLite + Git operation fake仅位于外部CAS边界。覆盖INV-16/17/18。

- 同一submitted Task并发accept/rework/cancel只有一个CAS胜出。
- accept固定接受者和integration Participant；后续Project默认变化不改写。
- source链接open integration后第二accept/rework/cancel返回`ACTION_IN_PROGRESS`。
- 取消integration原子释放source授权；任意失败不留下半link/accepted/pending Event。

### CT-08 Agent配置和adapter descriptor

Boundary：POST/PUT、CLI、Web、coordlink和静态adapter registry。覆盖INV-02/04/23。

- `adapter_id/image/instructions来源/model/subagent_model/base_url/effort`写读更新一致，完整替换语义明确。
- instructions恰一来源、大小限制、安全token、HTTPS URL和AllowedEfforts负例零副作用。
- config变化只影响新Run，fingerprint不同禁止Resume；旧Run保留原hash。
- descriptor不泄露executable/argv/path/secret，adapter增删只改注册项。

### CT-09 子Task结果回传

Boundary：真实SQLite + Service + scheduler/notifier + scripted adapter。覆盖INV-10/12/27。

- A的parent Task在running时通过正式operation为B创建child，固定coordination Conversation和result recipient=A，然后parent wait。
- B首次submitted时与唯一`child_result_ready` Message/recipient同事务持久；parent仍waiting时同事务转queued，A可在新Run中收到该Message并Resume同parent scope。
- 在child terminal事务前后和parent wake后注入Daemon crash，重启后仍恰有一条Message/recipient，不丢wake、不重复Run，不跨scope Resume。
- 缺Conversation/result recipient、跨Project、非成员或paused recipient的child创建拒绝且零副作用；不存在subscriber/inbox fallback或固定Human路由。
- open child存在时归档coordination Conversation、移除route成员、归档result recipient或将parent改派使route失效均拒绝。child创建后recipient被paused时结果Message仍唯一durable且parent可queued，但不创建新Run；恢复active后可继续。

## 7. Runtime与日志场景

### RT-01 真实隔离

Boundary：真实Docker。覆盖INV-06/11/13/23。

- 同时启动A/B Task Run，container/workspace/.git/home/token/socket/log目录全部不同。
- A不能读写B资源、DB、Docker socket、control repo、operator socket或宿主home。
- Conversation Run无`/workspace`；Task Run只挂当前Task workspace。
- non-root、无额外capability、无published port；Agent修改private refs不能改变canonical。

### RT-02 Active truth和快速退出

Boundary：真实Docker + one-shot scripted adapter。覆盖INV-05/06/12/20/26。

- create前Run starting；container和CLI进程可证live后才active/Task running。
- start前日志writer/Attach已就绪；立即输出并exit的首字节、session、exit均存在。
- session存在但进程消失时Run interrupted，不保持active。
- exit 0无structured outcome时Task只requeue/failed，不submitted/completed。

### RT-03 Resume、Inject、cancel和timeout

Boundary：真实Docker + adapter transcript。覆盖INV-09/10/12/18。

- Resume兼容时创建新Run并引用旧Run；fingerprint变化/未知时fresh。
- 同Participant的其他Project、其他Task、Conversation scope、不同workspace identity或较旧session均不可被Resume；选择结果只能fresh或精确同scope最新session。
- session-not-found终结当前Run，recipient回pending，后续fresh Run；一个Run不启动第二CLI。
- Inject前Inspect live；accepted只delivered不ack；unsupported/failure保持pending。
- cancel/timeout先durable intent和token撤销，再真实stop/kill/remove；Task与Conversation Run收尾各自正确。
- stop/cancel竞态、迟到exit和迟到outcome不伪完成。

### RT-04 Daemon crash/reconcile

Boundary：真实SQLite + Docker，在create/start/active/terminal/cleanup阶段kill Daemon。覆盖INV-06/10/12/18/20。

- matching container按launch phase接管且不重复create/start；nonce/label/generation不匹配不接管不删除。
- 日志hash/offset先恢复，再开放消息递送和mutation；接管期间输出无静默gap。
- delivered未ack recipient恢复pending；同Participant不出现第二active Run。
- terminal container/control资源最终removed；Docker不可达保持blocked并可重试。

### RT-05 行为日志完整性

Boundary：真实scripted CLI进程 + adapter +文件+SQLite index。覆盖INV-20/21/22。

一个Run必须实际产生并验证：stdout、stderr、provider frame、tool call/result、shell command/output/exit/duration、coordlink请求/响应、权限拒绝、Task/Message操作、Docker生命周期、Git前后SHA、unknown frame和redaction记录。

- raw与normalized sequence/offset/hash可交叉定位，manifest首尾/hash/计数匹配。
- adapter不支持的observability在descriptor声明，不能伪造；未知frame保留redacted raw并规范化parse_error。
- 强制单记录/总量上限产生truncation行；模拟writer中断产生显式gap或可证明无gap。
- SQLite index不得领先文件；落后时重启补齐。篡改hash使日志`corrupt`且不覆盖原文件。
- Event数量不随stdout chunk/tool call线性膨胀，证明两层分离。
- 同一Run携带两个Conversation消息时，Conversation member但非Run owner且无`log.read_project`者不能读/follow/export日志；Run owner的`log.read_own`和敏感Project级访问分别有正负例。

### RT-06 Retention和secret

Boundary：可控时钟 +真实文件/SQLite/Docker inspect。覆盖INV-19/22/23。

- 无override使用168h；Project override即时用于历史Run，`age < retention`不删、`age ==` eligible。
- 每Run只有一个Behavior Log index；Run行无可独立更新的long_term/retention副本。index long_term阻止过期日志GC；无权限设置拒绝。取消后不改ended_at，按当前策略可能立即eligible。
- active writer、未核验manifest、进行中export/reader lease、corrupt待处置日志不得删。
- 删除后保留Run终态、manifest摘要/hash/计数和Event。
- 已知secret在SQLite、Event、raw/normalized/stdout/stderr、错误、Web响应、argv、labels和raw `docker inspect` Config.Env中均不存在；entrypoint仍能从secret file启动provider。

### RT-07 Production adapter conformance

Claude与Codex分别使用真实协议frame/golden transcript验证Start/Resume、session、usage、tool/shell事件、unknown/error、exit、redaction和ObservableEventKinds。fixture必须来自真实协议形状但移除secret/private正文；不能用同一个手写parser fake同时冒充两个adapter。

## 8. Git场景

### GT-00 Project注册恢复

Boundary：真实临时Git repo + file-backed SQLite。覆盖INV-01/18。

- dirty source不被修改；branch移动后initial SHA仍固定。
- 在intent、repo create、object import、ref create、terminal事务各阶段故障，重启只得到一个正确control repo或明确Project error。
- actual canonical与cache不一致时不reset actual；repair核验后才active。
- 在workspace prepare intent、目录创建、marker、object checkout和ready CAS各阶段崩溃；重启只能得到原identity的ready workspace或blocked，不重读移动canonical、不创建第二目录、不在ready前claim。

### GT-01 Human/Agent workspace等价

Boundary：真实Git workspace + Operator CLI + Docker。覆盖INV-03/13。

- 相同base的Human/Agent git Task拥有不同workspace和`.git`，初始HEAD一致。
- Human只从授权宿主入口获得自己的路径；Agent只看到`/workspace`且不知道宿主路径。
- 两者都不能写control repo/canonical；标准Git提交后走同一submit/capture代码路径。
- `workspace_mode=none`不创建workspace；integration必须git。

### GT-02 Agent capture

Boundary：真实Git + terminal Docker Run。覆盖INV-14/15/18。

- dirty/untracked、中间态、expected mismatch、oversize/corrupt bundle稳定失败且不建submission ref。
- writer停止、clean HEAD匹配后quarantine/fsck/create-only ref成功，Task submitted并记录run/submission/ref/head。
- hook/global config/alternates/replace refs/危险环境不能影响受信helper。

### GT-03 Human稳定快照

Boundary：真实宿主workspace + 并发writer + Service submit。覆盖INV-14/15。

- F1/F2稳定时Human提交成功，`head_run_id=null`但submission ID/ref存在。
- 在HEAD读取、bundle生成、第二次status各窗口并发修改/commit，返回`WORKSPACE_CHANGED`或`GIT_HEAD_MISMATCH`，Task回running、canonical不变、无授权ref。
- 快照后新commit不被静默纳入旧submission，workspace保留可再次提交。
- Human不能使用旧complete/evidence入口绕过capture。

### GT-04 Capture crash matrix

Boundary：真实Git + failpoints +重启。覆盖INV-15/18。

- pending无ref只在Task/version/operation/head匹配时重试。
- ref已有且DB未写只在相同fence下补submitted；Task cancel/rework/generation变化后旧ref不推进状态。
- ref SHA冲突使Project error且不覆盖/删除。
- Agent与Human capture均覆盖；`git fsck`始终通过。

### GT-05 Canonical CAS竞争

Boundary：真实Git `update-ref expected-old` +真实SQLite。覆盖INV-16/18。

- 两个相同base结果并发accept，最多一个直接推进；另一个保留submission并stale。
- CAS成功/SQLite提交前crash，重启核验actual target后幂等完成。
- actual为第三SHA时不reset，创建integration；非fast-forward不由Daemon merge。

### GT-06 Integration Participant等价

Boundary：真实Git +Human和CLI Agent两种assignee。覆盖INV-03/17。

- stale source分别指派Human/Agent integration，输入字段、workspace、lineage、capture和CAS断言相同。
- result必须同时包含source head和当轮integration expected canonical ancestor；squash/cherry-pick丢lineage拒绝。
- 冲突保留在private workspace并通过Task/Message可见；不创建ConflictSet。
- source/accept version/initial canonical创建后不变。连续两次stale都机械requeue同一integration Task，每次expected canonical更新为actual且round恰好+1；不嵌套Task、不换人、不丢source/已提交修复。
- integration完成原子完成source并写同一final canonical；取消原子释放source授权。

### GT-07 Git GC

Boundary：可控时钟 +真实Git/workspace。覆盖INV-13/15/19。

- open/running/finishing、dirty、中间态、source引用、pending、唯一未捕获commit和ownership不明均阻止删除。
- completed/cancelled、clean、已由canonical包含且达到期限后才自动删workspace/ref。
- 未集成唯一submission只有单目标expected-SHA discard可删；wrong Task/submission/SHA零副作用。
- ref删除后再次reachability检查再prune；与并发source Task创建互斥，`git fsck`通过。

## 9. Web UI验收

Boundary：真实Daemon +真实浏览器自动化，至少桌面1440x900和移动390x844。覆盖INV-04/08/20/22/24。

### WT-01 本机和认证

- 服务只绑定配置的loopback地址；配置非loopback拒绝启动。
- 未认证、吊销Credential、缺capability、跨Project/Conversation访问均拒绝且不泄露摘要。
- Web mutation与CLI使用相同operation、version、idempotency和Event；刷新/重启后状态一致。
- 会话cookie为HttpOnly/SameSite=Strict（TLS时Secure），token不进localStorage/DOM/URL/日志；登出或Credential吊销使旧会话失效。
- 每个mutation拒绝缺失/错误CSRF、错Origin、错Host、simple cross-origin form与预检CORS，且零业务副作用；安全同源请求正常通过。
- CSP禁止非预期源、inline script和`eval`；Task/Message/日志内的HTML/script/URL payload显示为文本且不执行。SSE/WebSocket使用错Origin/无scope不能建立，权限吊销后在有界时间内断开并不再泄露新事件。

### WT-02 协调工作流

- 查看Project/Participant/Role/Agent配置，创建并派发Task，执行claim/submit/accept/rework/retry/cancel/wake。
- 查看Task树、assignee、Run真实状态、原因、base/head/submission ref/canonical和integration链接。
- 不存在代码编辑器、终端、任意文件路径输入或raw ref修改控件/route。

### WT-03 Conversation和未读

- 创建direct/group、管理成员、显式选择多个recipients、分别显示未读/delivered/ack/exhausted。
- 非recipient不出现未读badge；一个recipient ack后其他状态不变化。
- Message可选关联Task，历史按稳定顺序显示；定向wake只影响选中CLI Agent。

### WT-04 行为日志

- 实时follow和历史分页按Project/Participant/Task/Conversation/Message/Run/kind/error筛选。
- tool/shell/coordlink/Docker/Git/unknown/truncate/redaction行可查看完整已脱敏详情和sequence关联。
- 导出包含manifest/hash并无secret；有权限用户可设置/取消long_term并立即看到effective retention/eligible时间。

### WT-05 可用性

- browser console无未处理错误，网络请求无意外4xx/5xx。
- 桌面/移动均无文字溢出、控件遮挡、不可达操作或横向破版；动态日志不会改变工具栏/状态布局。
- 真实截图和关键页面像素检查证明非空、正确渲染；浏览器E2E必须实际点击公开界面，不能只测HTTP handler。

## 10. Deterministic组合E2E

使用真实Daemon、file-backed SQLite、真实Git、真实Docker、scripted adapter和浏览器，不调用外部provider。

### 10.1 Fixture

- Human H，CLI Agent A/B/C；配置Role证明名称不决定职责。
- 一个Project，canonical C0；一个group Conversation包含H/A/B/C。
- `max_parallel_runs >= 2`，adapter可控输出、message/tool/shell/Git和故障点。

### 10.2 流程

1. H创建两个相同base的git Task指派A/B，两Run真实重叠。
2. A在group向B/C发Message但不发H；B wake=true、C wake=false。H是成员但无未读。
3. B active时Inject失败，recipient保持pending；B后续Run bootstrap知道未读并ack。C不因消息启动Run，但下一次Task Run知道未读。
4. A/B各自提交不同commit，行为日志包含tool/shell/coordlink/Git和Docker事实。
5. H从Web/CLI检查submission refs，accept A直接CAS到CA；accept B变stale并创建integration Task。
6. integration分别用一次CLI Agent fixture完成并使canonical包含A/B lineage。
7. 创建一个Human git Task，H在宿主workspace提交，模拟一次并发变化失败后稳定重提并走相同accept/CAS。
8. 发送无Task wake Message给A，创建无workspace Conversation Run并ack。
9. 在至少一个Run live和一个recipient pending时kill/restart Daemon，验证接管、未读和日志hash恢复。
10. Web执行Conversation/Task/log查询、导出和long_term切换；最后cleanup/GC preview。

### 10.3 断言

- Human/Agent Task FSM和Git结果结构除Run字段外一致；不存在旧Human完成旁路。
- recipient状态精确，非recipient未读/wake为零，重启后不丢不重复业务行。
- 同CLI Agent无重叠Run，Conversation Run无workspace。
- 每个提交有不可变submission ref；canonical lineage、SQLite投影和`git fsck`一致。
- 日志sequence/hash/manifest/index一致，全部要求行为种类可查且无secret。
- Web、CLI和coordlink读到同一状态；拒绝操作零副作用。
- 结束后无starting/active Run、owned orphan container、未解释pending action或误删ref/log。

## 11. 真实CLI E2E

真实provider调用不得自动连续重试。每次运行需要明确授权，结果分类为产品、provider/环境或Task specification；只有产品问题先落最低层回归并修复。

### RMA-01 Production adapter smoke

Claude和Codex各至少一次真实Task Run：读取Task/未读摘要，发progress/Message，执行至少一个可观测tool或shell操作，提交结构化outcome并terminal。验证session、resume/fresh判定、行为日志、redaction和cleanup。任一adapter SKIP不算v1 PASS。

### RMA-02 四Agent恢复与收敛

只定义一个fresh四CLI Agent场景：

1. 四个git source Task从同一canonical创建，四个Run在时间线上真实重叠。
2. 使用group Conversation发送至少一条多recipient Message和一条只定向单Agent的Message，验证非recipient不wake。
3. 至少两个source Run仍live且已有durable行为/Message时受控重启一次Daemon；原Run接管或按正式合同终结，不重复active。
4. 恢复后完成四个source Task；一个允许直接CAS，其余通过真实integration Task收敛，不能Daemon后台merge。
5. 至少一次Message通过下一Run未读bootstrap而不是Inject完成ack；至少一次无Task Conversation Run。
6. 最终canonical包含所有accepted source lineage，SQLite/ref/日志/Web投影一致。
7. 完全停止并重启后复查，再执行正式cleanup/GC；无orphan、pending不明或日志完整性错误。

### RMA-03 小说系统持续真实开发

RMA-03是每个拟验收CoordPlane candidate都必须重新执行的reference workload，不是只在发布前跑一次的demo。它的当前本机需求源为：

```text
/home/zxh/multica_workspaces/用于检验coordplane的小说系统软件测试需求文档
```

该目录当前是需求文档集而不是Git repository。验收驱动器不得将它直接注册为CoordPlane Project，也不得修改原目录。

#### 11.3.1 输入冻结与开发代码库

1. 每轮开始前，验收驱动器只读枚举需求目录下全部regular files，拒绝symlink/device/socket，对排序后的relative path、size和SHA-256生成`requirements_manifest.json`和总hash。结束时重算，任一变化使该轮`INVALID_INPUT_CHANGED`，不能拼接证据。
2. 小说系统代码使用单独、持续的Git负载代码库。首轮的`workload_base_sha`为空项目，后续轮为上一PASS轮的accepted workload canonical。每轮都在隔离seed repo中从base开始，用当前manifest精确替换驱动器拥有的`requirements/`快照；若tree变化，使用固定author/message/时间规则生成可重现requirements snapshot commit。该结果才是本轮`workload_input_sha`，因此Agent在workspace所见文档与收据manifest始终一致。
3. 每轮从固定`workload_input_sha`创建全新的CoordPlane data dir、Project、workspace和Run，不复用上轮SQLite、container、provider session或工作树证据。只有该轮整体PASS后，才能把`workload_output_sha`发布为下轮base；FAIL/INVALID轮不移动持续负载代码库基线，下次从原base和同一requirements manifest可重建input。
4. 需求目录路径只是验收驱动器参数和收据字段，不得出现在CoordPlane产品调度、handler、prompt特判或schema中。

#### 11.3.2 每轮唯一开发锁

每轮必须从需求manifest和当前负载canonical选一个尚未实现或最近失败的小说系统不变量，收据固定其requirement citation、范围/非范围、可证伪验收项、项目自有验证命令和预期产物。输入在轮内不得增加第二个不变量；新需求放入下轮backlog。

验收配置至少有四个真实CLI Agent Participant，职责只由Role/instructions/Task配置，并执行：

1. A领取parent Task，经coordlink为B/C创建至少两个不共享可写文件的child git Task，固定coordination group Conversation/result recipient，发送显式recipient Message后wait。
2. B/C的Docker Run时间线真实重叠，只在各自private workspace使用标准Git，通过Message询问/回答，执行项目测试并产生不可变submission ref。
3. 第一个child结果使A的waiting parent被幂等唤醒；A创建新Run且Resume精确parent session scope，经分页inbox读取所有未读，不跨Task/Conversation污染。
4. D通过普通git review/test Task独立复核输出，不使用日志或Agent自报代替代码/测试证据。有权Participant通过正式accept推进canonical，至少一个并发结果经显式integration Task收敛。
5. 在至少一个Run active、parent waiting或recipient pending时受控重启Daemon，继续同一Run或按契约终结/新Run Resume，不重复Task、Message、submission或active Run。
6. 最终在actual workload canonical上执行该轮锁定的项目契约测试和相关全量测试。只有它们PASS、输出canonical包含全部accepted lineage且独立复核通过，才认为小说系统本轮交付完成。

#### 11.3.3 行为日志审计

本轮必须从实际Docker/OS wait、provider raw frame、coordlink请求结果、SQLite Event/业务行和Git ref/object构建外部观察集，再与每Run Behavior Log对账，不用日志自证完整。至少审计：

- Run从create/attach前到terminal/cleanup的时序，session/resume/inject决策和Daemon重启接管。
- provider已暴露的text/tool/shell/file/Git/usage/error，coordlink Task/Conversation/Message/progress/权限允许与拒绝。
- 每条行为与Project/Participant/Task/Conversation/Message/recipient/Run、raw offset/hash和Git before/after的可追溯关联。
- unknown/gap/truncation/redaction数量、manifest/hash chain/index和Web/CLI筛选/导出一致性，以及secret/XSS负例。

任一已观察行为无日志或无gap/truncation说明、日志凭空生成行为、关联错Task/Conversation/Run、日志推进业务状态或未授权读取，均是CoordPlane finding。隐藏chain-of-thought和CoordPlane不可观察的宿主Human行为不在对账分母。

#### 11.3.4 Finding和下一轮

RMA-03失败先写收据并只选一类：`F1_REQUIREMENT`、`F2_CONTRACT_GAP`、`F3_PRODUCT_IMPLEMENTATION`、`F4_PROVIDER_ENVIRONMENT`、`F5_SECURITY_RELIABILITY`、`F6_WORKLOAD_TASK`或`F7_OBSERVATION`。收据必须含复现、期望/实际、影响、对应通用不变量和证据ID；不得用“Agent说失败”代替。

- F1先改需求基线并重新审批；不直接补产品代码。
- F2/F3/F5停止当前candidate，先在最低真实边界增加能在旧行为上变红的契约测试，每轮只修一个不变量；L1-L4通过后以新candidate回放原失败边界并重新完整执行RMA-03。
- F4报告`INVALID_ENVIRONMENT(reason)`，保留收据且不宣称产品PASS；F6在同一小说系统Task/rework流中修复，不因业务代码问题修改CoordPlane；F7保持open直到证据可归类。
- 同一CoordPlane修复连续两轮未过同一gate时停止叠加补丁，回到不变量owner和失败分类。

RMA-03只在本轮小说系统Task交付、CoordPlane组合状态收敛、日志对账通过且无未处置F2/F3/F5 finding时PASS。每个新CoordPlane candidate都需要新收据，不得用前一candidate的真实负载结果豁免。

### 11.4 收据

每个真实场景记录candidate SHA、image digest、adapter/CLI版本、开始结束时间、Task/Run/Message/recipient ID、最终SHA、日志manifest/hash、重启前后事实和脱敏后的失败分类。RMA-03另外记录需求目录、前后requirements manifest hash、workload input/output SHA、本轮唯一不变量/引用、项目测试命令、行为日志对账统计、finding和是否发布下轮负载基线。公开收据不保存Credential/token/secret、Message私密正文、instructions全文、provider隐藏thinking/signature或未脱敏response。

RMA-01、RMA-02和RMA-03名称与含义唯一；不得在其他章节用RMA-02指代双Agent场景，或用RMA-03指代deterministic fixture。

## 12. 代码预算

预算是架构漂移和重复实现警报，不授权删减fencing、recovery、日志完整性、Web权限或测试。唯一数值事实源是本节，其他文档只能引用。

当前候选需求审批前必须在精确实现基线SHA上产生可复算LOC报告，同时列出为删除旧Boss/agents/conversation Task/Human旁路预计减少的代码、为当前候选新契约必须增加的代码/测试和模块归属。若尚未实现新范围前已超下表发布阈值，当前候选不得冻结；只能选择带可验证删除量的Add-Remove计划保持阈值，或由需求审批人显式修改本表。禁止预计未来会删除就先声称当前可行，也禁止为达数值删除有效契约测试。

| bucket | 目标 | 软阈值 | 发布阈值 |
| --- | ---: | ---: | ---: |
| Budgeted production | 24,000 | 24,500 | 25,000 |
| Budgeted tests | 25,500 | 26,200 | 27,000 |
| Build/test infrastructure | 250 | 500 | 700 |
| 后端合计 | 49,750 | 51,200 | 52,700 |
| Handwritten frontend（SPA、webserver、browser e2e） | 2,000 | 2,500 | 3,000 |

统计规则：

- 非空、非纯注释物理行；第一方非generated语义源按其真实bucket 1:1计入。
- `*_test.go`、test helper、scripted adapter/failpoint和仅测试build tag源码计tests；运行脚本、Dockerfile和测试工具计infra。
- generated只有标准标记、manifest allowlist、owner、generator命令/版本、输入/hash齐全且clean重生成无diff才排除；包含业务FSM/权限/recovery/provider特判仍回算所属bucket。
- fixture正文可单列可见但不能藏实现；unknown path使`--check`失败。
- production单文件500行warning/800阻断，单函数80行warning/140阻断；禁止压缩语句、大switch、删除错误上下文或搬到脚本/generated逃预算。
- 超软阈值必须列净增量、所属不变量和删除/合并计划；超发布阈值阻断，调整预算必须先显式修改本需求，不得在脚本/report暗改。

候选LOC JSON必须记录candidate SHA、全部bucket/模块、unknown/generated/fixture、Git diff、gofmt、file/function blocker。当前工作树数值和历史候选结果不属于需求正文。

## 13. Gate和验证命令

实现仓库必须提供以下或严格等价入口：

```bash
gofmt -l <go-files>
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go test -tags=docker ./... -count=1
go test ./internal/store -run '^TestCT01' -count=1
go test ./internal/core -run '^TestCT0[1-9]' -count=1
go test ./tests/e2e -run '^TestWeb' -count=1
scripts/e2e-deterministic.sh
scripts/loc-budget.sh --check --output loc-budget.json
scripts/e2e-real-cli.sh
scripts/e2e-real-multi-agent.sh
scripts/e2e-reference-workload.sh \
  --requirements-dir '/home/zxh/multica_workspaces/用于检验coordplane的小说系统软件测试需求文档' \
  --workload-repo '/home/zxh/multica_workspaces/coordplane_reference_workloads/novel-system.git'
```

Gate顺序：L1 static/vet -> L2 narrow contracts -> L3 full/race -> L4 Docker/deterministic/Web/LOC -> L5 RMA-01/RMA-02/RMA-03。脚本必须输出`PASS/FAIL/INVALID_INPUT_CHANGED(reason)/INVALID_ENVIRONMENT(reason)/SKIP(reason)`；exit 0但SKIP/INVALID不能伪装PASS。RMA-03每个拟验收candidate必须新跑，无diff豁免。

每轮实现交付声明本地candidate commit SHA。测试审核、代码审查、浏览器验收、L4和L5证据必须基于同一SHA；不同SHA必须重跑受影响层。RMA-01/RMA-02只可在精确diff证明未影响production/runtime/adapter/fixture/image时复用；RMA-03对每个新candidate都必须生成新收据，不适用此豁免。

## 14. 完成判定

只有以下全部成立才可冻结v1：

- 五份规范与原话记录同revision，不存在旧模型冲突或未归一化的新需求；冻结SHA、原话记录blob SHA和已处理的最新`UR-NNNN`写入独立只读release receipt。
- L1-L4全部PASS，full/race/vet、SQLite、Git、Docker、行为日志和真实浏览器测试无SKIP。
- RMA-01两个production adapter、唯一RMA-02四Agent场景和RMA-03小说系统reference workload全部PASS且指向同candidate。
- Human/CLI Agent统一Task/Git、群组逐recipient未读、Conversation Run、Web和日志retention均有独立复核。
- 本节预算已经精确基线/删增分析后审批，且`production/tests/infra/total <= 25,000/27,000/700/52,700`，frontend `<=3,000`，质量blocker为零。
- `git fsck`通过；无starting/active Run、owned orphan container、未解释capture/CAS intent、丢失submission ref、未处置recipient或错误删除的long_term日志。
- 最终收据列出谁验证了什么、命令、candidate SHA、问题处置、发布状态和剩余风险。

任一真实CLI、四Agent、小说reference workload、Docker或浏览器gate未运行，只能报告已证明的较窄范围，不能声明v1完成。连续两轮同一修复未过同一gate时停止补丁并回到需求/契约/实现/provider或环境/负载Task规格归因。
