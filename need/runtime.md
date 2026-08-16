# CoordPlane Runtime、隔离与行为日志需求

状态：候选冻结基线，待需求审批人复核精确 revision
版本：1.0-rc3
日期：2026-08-16
依赖：`README.md`、`core.md`

## 1. 目标和边界

Runtime 负责：

- 为 CLI Agent Participant 创建、监控、恢复和清理 Docker Run。
- 将 Task、Conversation、Message、未读摘要、权限和运行配置安全传入 CLI。
- 为 Task Run挂载私有 Git workspace，为 Conversation Run明确不挂载项目代码。
- 支持 fresh Start、兼容 Resume、可选 Inject、取消、超时和 Daemon重启接管。
- 从进程启动前开始记录追加式、已脱敏、可校验的完整可观测行为日志。

Runtime 不为 Human创建 Run，不决定 Task是否正确，不执行 Git accept/integration，不把 transcript当状态真相，也不声称观察 provider隐藏推理或容器外不可见行为。

## 2. 受信边界

### 2.1 受信组件

- Daemon/Core、Runtime executor、静态注册的 CLI adapter。
- daemon-owned SQLite、control repo、workspace root、Participant home root、log root和per-Run control目录。
- 只读行为日志查询/导出 helper；它不能推进 Task、Message或Git状态。

不受信输入包括 Task/Message正文、Participant名称、模型输出、CLI stdout/stderr、provider事件、workspace内容、Git ref输入、容器内文件和网络响应。

### 2.2 隔离单位

- 每个 CLI Agent Participant一个持久 private home。
- 每个 `workspace_mode=git` Task一个私有 workspace；Human在宿主机使用，CLI Agent仅将自己当前Task workspace挂入容器。
- 每个 Run一个容器、control目录、Unix socket、token、bootstrap和行为日志目录。
- 同一 CLI Agent最多一个 starting/active Run；Human数量和CLI Agent配置数量不受该限制。

## 3. 目录布局

所有路径由服务端稳定 ID生成，调用者不得提供运行资源的宿主绝对路径：

```text
data/
  coordplane.db
  control-repos/<project-id>.git/
  workspaces/<project-id>/<task-id>/
  participant-homes/<participant-id>/
  run-control/<run-id>/
    owner.json
    bootstrap.json
    api.sock
    token
    secrets/
  logs/<run-id>/
    raw.redacted.jsonl
    behavior.jsonl
    stdout.log
    stderr.log
    manifest.json
```

规则：

- ID必须先校验为固定格式，再拼接到配置 root；禁止接受 `..`、symlink逃逸和调用者绝对路径。
- 目录创建使用不可覆盖语义、最小权限和 ownership marker；冲突存在但owner不匹配时 fail closed。
- 清理前重新解析 symlink、检查 root containment、marker、Run/Task版本和实际资源。
- 日志文件只允许 Daemon写；容器不能回写、截断或替换日志。

## 4. Docker 拓扑

### 4.1 Task Run挂载

| 容器路径 | 内容 | 模式 |
| --- | --- | --- |
| `/workspace` | 当前 git Task私有 workspace；`workspace_mode=none` 时不存在 | `rw` |
| `/home/agent` | 当前 CLI Agent private home | `rw` |
| `/run/coordplane` | 本Run control目录、socket、bootstrap和secret file | 最小必要子路径；token/bootstrap/secret只读，socket可连接 |

Conversation Run只挂载 home和control目录，以 `/home/agent` 为工作目录，不挂载任何 Project workspace或control repo。Message可选关联Task不改变这一规则；需要读写代码必须显式领取 git Task。

### 4.2 禁止挂载

- SQLite、control repo、其他Task workspace、其他Participant home、其他Run control/log。
- Docker socket、宿主 `/`、宿主真实 home、SSH agent、云凭据目录或整个 CLI配置目录。
- Operator socket、Human Credential或其他Run token。

### 4.3 容器安全

- 默认非 root、无 privileged、无额外 capabilities、`no-new-privileges`、固定workdir。
- 不发布宿主端口；Agent只通过per-Run Unix socket访问CoordPlane。
- image、adapter argv和mount均由服务端配置生成，Task/Message文本不得控制 executable、mount、network或secret名。
- 容器可访问的资源不因 Participant Role扩大；Role只授权Service operation，不授权宿主文件系统。

### 4.4 网络

- 默认使用显式 Docker network；允许 `none/bridge` 等固定配置，不接受每Task任意网络参数。
- Provider网络是否可用是环境事实；Runtime记录结果但不自动降级为宿主执行。
- Web服务只监听loopback，不从容器网络暴露。

## 5. Workspace、Home 和 Bootstrap

### 5.1 Workspace

- git Task workspace必须在Run创建前由Git模块完成并核验base/source HEAD。
- Task Run开始前重新核验 Task `workspace_state=ready`、目录marker、workspace identity、HEAD、Git中间态和Task绑定；不匹配时Run不得 active，Task workspace转 `blocked` 并保留可行动错误。
- 同一Task retry复用原workspace，除非Git/GC合同证明可安全重建。
- CLI退出后先证明workspace writer停止，再允许capture/read-only检查。
- Human workspace由Git模块以宿主机路径提供，不经过Runtime；Runtime不得为Human伪造Run以复用执行代码。

### 5.2 Participant Home

- CLI Agent home跨Run保留，用于provider原生session/cache；不同Participant绝不共享可写home。
- Resume只使用同一Participant、Project、`session_scope_kind/id`、adapter且config fingerprint兼容的session；Task scope还必须匹配同一workspace identity。
- archived Participant的home只有在无recoverable Task/Message/Run且显式GC后可删除。

### 5.3 Bootstrap

bootstrap是有界结构化JSON，至少包含：

- Project、Participant、Run、runtime generation、adapter配置hash和权限摘要。
- Task Run的Task、parent/children摘要、base/source SHA、workspace容器路径和task generation。
- Conversation Run的Conversation和trigger Message ID，明确 `task_id=null`、`workspace=null`。
- 本次携带的每条 Message的ID、recipient行ID、sender、Conversation、可选Task、wake和受大小限制正文。
- 同Project全部未确认MessageRecipient总数、生成时的稳定高水位、有界ID样本和不透明inbox cursor。Agent必须能通过coordlink稳定分页到高水位；不得把全部未读ID塞进bootstrap。
- coordlink固定命令帮助、socket路径和token文件路径；不得包含operator凭据。

未知字段/超限输入、缺少Task workspace或Conversation上下文、instructions不可读、secret不可用均在创建容器前fail closed。正文和instructions全文不进入Event或错误。

## 6. Run运行字段

除`core.md`字段外至少保存：

| 字段 | 要求 |
| --- | --- |
| `runtime_ref/container_id/launch_nonce` | 可重建的确定性运行引用和ownership |
| `launch_phase` | `prepared/create_issued/container_observed/start_issued/process_observed` |
| `pid/native_session_id` | 可空；session不是live证明 |
| `heartbeat_at/last_observed_at` | 最近真实观察 |
| `started_at/ended_at/exit_code/error_code` | 生命周期事实 |
| `stop_requested_at/reason/operation_id` | 可空 durable stop intent |
| `cleanup_state/operation_id/error` | `not_needed/pending/removed/blocked` |
| `behavior_log_index_id` | 一对一引用唯一Behavior Log index；Run不复制path/sequence/hash/count/retention字段 |

Docker side effect唯一owner是Runtime executor。adapter只能生成命令、输入和解析观察，不能直接更新Task、Message、Git或Docker生命周期。

## 7. CLI Adapter

### 7.1 接口与注册

每个adapter独立实现并静态注册：

```text
BuildStartCommand(StartInput) -> CommandSpec
BuildResumeCommand(ResumeInput) -> CommandSpec
BuildInjectInput(MessageInput) -> bytes        # 可选
ParseEvent(raw frame) -> []ObservedBehavior
ResumeCompatible(previous, current) -> bool
RedactionRules() -> []Rule
Metadata() -> Descriptor
```

Descriptor至少包含 `Name/ExecutionModel/SupportsResume/SupportsInject/AllowedEfforts/ObservableEventKinds`。`GET /v1/adapters`只返回这些安全元数据，不返回executable、argv模板、宿主路径、secret或redaction pattern。

新增/删除adapter只修改注册列表、实现、fixture和conformance测试。主executor不得按adapter名称、模型或角色写if/else特判。

### 7.2 约束

- adapter生成结构化argv，禁止用 `sh -c` 拼接Task/Message/Participant输入。
- Start/Resume均为one-shot CLI进程；一个Run内只启动一个provider CLI。
- adapter必须逐帧报告已消费字节、解析结果和未知帧；不能静默丢弃不可解析输出。
- provider暴露tool call、shell、file、network或token usage事件时，adapter必须规范化；未暴露的行为在Descriptor中明确缺口，不能伪造。
- Parse失败保留已脱敏原始帧并生成 `unknown_frame/parse_error` 行为记录，不得使Task自动成功。
- adapter不持有Docker handle，不写SQLite，不判断Task完成；只返回观察事实。

## 8. Run启动流程

必须按以下顺序，且各阶段可由 durable字段恢复：

1. Core在事务中确认Project/Participant/Task或MessageRecipient条件，递增Participant runtime generation，必要时递增Task generation，创建starting Run并固定配置。
2. 对Task Run回读已由Git worker收敛为`ready`的workspace operation/fingerprint，或确认Conversation Run无workspace，再创建control/log目录和ownership marker。Runtime不使自己成为第二workspace preparer。
3. 打开stdout/stderr/raw/behavior日志写端并写manifest起始记录，确保进程首字节可捕获。
4. 生成token、bootstrap和per-Run secret files，启动Unix listener。
5. 写`create_issued`和launch nonce后创建确定性名称容器；核验labels/image/mount/network/nonce。
6. Attach stdout/stderr从offset 0采集后才允许Start；写`start_issued`。
7. 观察真实CLI进程和container running后写`process_observed`，Run active；Task Run同步Task running。
8. 持续Wait、日志采集和Inspect，直到获得可信terminal事实并完成Task/recipient收尾。

create/start超时不得猜测失败；先按确定性名称Inspect并reconcile。快速one-shot也必须捕获首帧、session和exit事实。

## 9. Resume、Inject 和消息唤醒

### 9.1 Resume

- Resume总是新Run，terminal Run永不复活。
- 只选择同Participant、Project、`session_scope_kind/id`、adapter按`ended_at,id`最新且有session的terminal Run；不得跨Task/Conversation、跨Project或静默跳回更旧session。
- config fingerprint、adapter或provider要求不兼容时fresh Start。Task scope另外要求Task ID、workspace identity和固定base/source与候选session一致；任一不一致必须fresh，不能用提示词覆盖上下文污染。
- Resume返回session-not-found时，本Run以`RESUME_UNAVAILABLE`终结；未ackrecipient回pending，之后另一个generation更高的fresh Run可重试，不能在同一Run启动第二CLI。

### 9.2 Inject与未读

- Message/recipient先durable，再尝试Inject。
- Inject前Inspect确认当前container/process/Run generation仍live；adapter accepted只表示输入被CLI接受，不表示recipient已读。
- 实际正文进入输入后recipient可标delivered；必须由Agent显式ack才acknowledged。
- Inject失败、unsupported或turn不兼容保持pending。Agent busy时不得创建第二Run。
- 每个新Run都包含同Project未读count/high-watermark/有界ID样本/cursor；`wake=false`消息因此在下一次Run可发现并可分页读取。
- `wake=true`只作用于明确CLI Agent recipient。无适合Task Run时创建Conversation Run，不挂workspace。
- 多个recipient消息可合入一个Run，但每个MessageRecipient ID独立记录、递送和ack，禁止按session粗粒度确认。

## 10. Progress、Heartbeat 与行为日志

### 10.1 两类记录

- Event：小型关键状态变化，与业务事务原子；不保存大正文。
- Behavior Log：Run内详细可观测行为，追加文件为主体，SQLite只存索引/hash/retention。行为日志不推进Task/Message/Git状态。

Supervisor heartbeat只表示最近确认container/process live。Agent `progress` 是Event并同时进入行为流；它不延长deadline、不完成Task。

### 10.2 原始流

`raw.redacted.jsonl` 必须从进程首字节开始，按单调sequence记录：

- stdout/stderr chunk及原始stream offset。
- provider结构化frame、session/usage/result/error。
- adapter未知frame和parse error。
- Runtime生成的truncation、redaction、gap和stream-close标记。

原始流是在落盘边界脱敏后的最接近原始表示，不得保存未脱敏副本。二进制/非法UTF-8使用有界base64或摘要并记录encoding。单记录和总文件上限必须产生显式truncation记录，不能静默截断。

### 10.3 规范化行为流

`behavior.jsonl` 每行至少包含：

```text
sequence, observed_at, source, kind, phase,
project_id, participant_id, run_id,
task_id?, conversation_id?, message_id?, recipient_id?,
tool_name?, arguments_redacted?, result_redacted?,
exit_code?, duration_ms?, git_before?, git_after?,
raw_offset?, raw_hash?, previous_hash, record_hash
```

在可观测范围内必须注册并规范化：

- provider turn/session、文本输出、tool call/result、usage和错误。
- shell命令、stdout/stderr、exit code、duration。
- 文件/Git工具调用以及前后HEAD/status/ref观察。
- coordlink请求/响应、权限允许/拒绝、Task/Conversation/Message/progress操作。
- Docker create/start/inspect/wait/stop/remove，resume/inject/cancel/timeout/reconcile/cleanup。
- parser未知、日志gap、截断、redaction和hash checkpoint。

normalizer按behavior kind通过静态列表注册。无法观察的provider内部思维、加密字段、系统调用或容器外Human行为不在承诺范围；不得用推测补行。

### 10.4 完整性、查询与恢复

- 每行使用sequence和hash chain；manifest保存文件hash、首尾sequence、adapter observability descriptor、截断/gap/redaction计数和terminal状态。
- SQLite index更新可以落后文件，但不能领先。Daemon重启从最后已核验offset扫描、验证hash并补索引；hash冲突使Run日志状态`corrupt`并fail loud，不覆盖文件。
- stdout/stderr普通视图可由专用文件或原始流投影，但必须与sequence/offset可交叉定位。
- Web/CLI支持按Project/Participant/Task/Conversation/Message/Run、kind、时间和错误筛选，实时follow、分页详情和导出manifest+日志。查询和follow均读唯一Behavior Log index的快照/version，不用Run副本判断retention。
- 日志访问严格按`log.read_own|log.read_project`判定。一个Run即使携带多个Conversation消息也不按Conversation成员资格拆分授权；成员只能经Message查询读其可见历史。导出要求相同read scope和`log.export`，并再次经过相同redaction检查。

### 10.5 脱敏

- provider secret、Run/operator token、Credential、URL userinfo、instructions全文和已登记敏感值在写入任何文件/SQLite/Event/错误前脱敏。
- structured field allowlist是第一层，递归value sanitizer是第二层，最终字节scanner是第三层。
- 脱敏记录保留字段路径、规则ID、原长度和不可逆摘要，不保留原值。
- 用户明确要求日志全面不能覆盖凭据安全；发现无法安全脱敏的frame时保存元数据/hash并标记content omitted。

## 11. 取消和超时

### 11.1 取消顺序

1. Core持久化Task cancel或Run stop intent、递增相应generation并撤销token。
2. Runtime使用独立cleanup context发送graceful signal，超时后kill完整容器。
3. Wait获得真实退出事实后写Run terminal；迟到正常exit不能覆盖更早cancel intent。
4. 停listener并删除container/socket/token/bootstrap/secret/control资源；行为日志先写terminal/manifest再关闭。

Task cancel取消责任；Run stop只停止本次进程。没有requested outcome的Task Run stop后Task按retry policy queued/failed；Conversation Run stop不创建或修改Task。

### 11.2 超时

- `run_timeout=0` 表示无自动deadline，不表示无cancel能力。
- timeout先持久化intent和撤销token，再停止container；真实退出后Run timed_out。
- Task Run已有更早requested outcome时仍按wait/submit/fail收尾；否则Task重试或failed。
- Conversation Run timeout使未ackrecipient回pending/backoff，不影响关联Task状态。

## 12. Daemon重启与对账

### 12.1 前置

启动持有data-dir锁后，按Project pending Git、Run/container、listener/control、MessageRecipient和behavior log index顺序对账。完成前不开放mutation和调度。

### 12.2 Run矩阵

| 持久状态 | 实际事实 | 行为 |
| --- | --- | --- |
| starting，无container ID | 确定性名称存在matching labels/nonce | 核验后保存ID，继续phase；不重复create |
| starting | matching created | 从offset 0 Attach日志后最多Start一次 |
| starting/active | matching running且CLI可证live | 接管Wait/log/listener并继续同一Run |
| starting/active | container exited | 读取exit/log后terminal并收尾Task/recipients |
| active | container absent或进程无法证明 | interrupted；不得凭session保持active |
| terminal | container仍存在 | cleanup pending并幂等stop/remove |
| 任意 | labels/nonce/generation不匹配 | 不接管、不删除，cleanup blocked并报告 |

对账不能为同一Participant创建第二active Run。日志先恢复写入/验证，再恢复业务递送，避免接管期间行为无记录。

## 13. Cleanup、Retention 和 GC

### 13.1 Run资源

- `cleanup_state`覆盖container和per-Run control资源；全部absent才removed，任何不确定为blocked。
- Docker NotFound只完成container子步骤；仍需关闭listener并删除socket/token/bootstrap/secret/control。
- 旧container可能运行时禁止该Participant下一Run。
- cleanup不删除行为日志；日志走独立retention。

### 13.2 Workspace与Home

- workspace删除条件见`git.md`：Task closed、无writer/pending/source引用、结果已保留、ownership确定且达到retention。
- dirty/untracked/中间态或唯一未捕获commit不得自动删除；危险discard要求单Task expected fingerprint。
- CLI Agent home默认保留；Participant archived、无recoverable Task/Message/Run且显式GC后才可删除。

### 13.3 行为日志保留

- 全局默认 `behavior_log=168h`。Project `behavior_log_retention` 非空时覆盖。
- age从Run不可变`ended_at`计算；`age == retention`开始eligible。
- 唯一Behavior Log index的 `long_term=true` 时无论期限均禁止自动删除。设置/取消必须要求 `log.retain`、目标Run所需read scope、index expected version、request ID并写Event；Run行不发生第二次retention mutation。
- 取消long_term不重置ended_at；立即按当前Project override或全局默认计算，可能马上eligible。
- 删除前要求Run terminal、日志writer关闭、manifest/hash已核验、无进行中的导出/查询租约、无long_term和retention已到。
- 删除日志文件后保留Run终态、exit code、manifest摘要、首尾sequence、最终hash、截断/gap/redaction计数和删除Event。
- `gc preview`列出effective policy、ended_at、eligible时间、long_term和阻塞原因；`gc run --confirm`执行相同注册步骤并重新检查。

## 14. Provider Secret

- secret来源是Daemon配置引用的宿主只读secret file；配置只保存路径/键名，不保存值。
- 创建容器前将必要值写入per-Run mode 0400 secret file。Docker Config.Env只包含非秘密配置或容器内secret文件路径，不包含secret值。
- 受控entrypoint在容器进程内读取secret file并为provider进程设置所需环境；secret不得出现在argv、Docker labels、`docker inspect` Config.Env、bootstrap、SQLite、Event、日志或Agent-facing error。
- per-agent base_url/model/effort不是secret，但仍经过安全校验和日志sanitizer。
- v1不建设Vault/SecretProvider平台；缺失或权限错误明确失败并允许配置修复后retry。

## 15. Runtime 不变量

1. Human无Run；CLI Agent只在Docker内执行，不能降级为宿主进程。
2. 同一CLI Agent最多一个starting/active Run，Task Run与Conversation Run共用该限制。
3. Conversation Run无代码workspace；Task关联Message不隐式授权代码访问。
4. 每个Run启动前日志writer已就绪，快速进程也不能丢首帧/exit。
5. 每个Run bootstrap都用count/high-watermark/有界样本/cursor告知同Project未读；只有显式recipient+wake才单独触发Run。
6. Resume创建新Run；terminal Run永不复活，旧token/generation失效，且session不跨Project、Task/Conversation scope或workspace identity。
7. 行为日志覆盖全部实际可观测行为，未知/丢失/截断/脱敏显式记录，不伪造隐藏推理。
8. Event与行为流分层；日志不能推进Task/Message/Git状态。
9. 每Run唯一Behavior Log index是retention/long_term真相；默认7天、Project override和取消后按原ended_at重算语义唯一。
10. secret值不进入Docker inspect、argv、SQLite、Event、普通/行为日志或Web响应。
11. active、ownership不明或未完成manifest的资源不得GC。
12. adapter、normalizer、worker和GC step通过列表注册，替换路径同一变更删除。
13. Conversation成员资格不授权行为日志；混合Conversation Run日志只按own/project敏感capability整体读取。
