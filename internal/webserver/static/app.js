"use strict";
// CoordPlane web frontend — talks to the daemon operator surface (/v1/*).
// Auth: X-Coordplane-Credential from sessionStorage; SCOPE_DENIED -> login.
// 接口契约: 所有响应为 Envelope{ok, data, error}, 业务数据在 data.data 内
// (见 internal/transport/protocol.go); 无 json tag 的 DTO(Role/Participant/
// AddCredentialInput)在网络上使用大写字段名。
const KEY = "coordplane_credential";
let view = "dash";
let detailTask = null;
let detailAgent = null;
let detailProject = null;
let projFilter = "";
let taskProjFilter = "";
let showCancelled = false;
let projects = [];
let agents = [];
let tasks = [];
let runs = [];
let participants = [];
let roles = [];
let participantsCache = [];
let messagesCache = [];
let eventsCache = [];
let credsCache = null;
let gcPreview = null;
let editMode = "";
let messagesError = "";

const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const pill = st => `<span class="pill st-${esc(st)}">${esc(st)}</span>`;
const short = s => s ? s.slice(0, 8) : "";
const agentName = id => {
  const a = agents.find(x => x.id === id);
  if (a) return esc(a.display_name);
  const p = participants.find(x => x.ID === id);
  return p ? esc(p.DisplayName) + ' <span class="muted">(human)</span>' : esc(id || "—");
};
const projName = id => { const p = projects.find(x => x.id === id); return p ? esc(p.name) : ""; };
const evBadge = t => !["submitted", "completed"].includes(t.status)
  ? ""
  : (t.evidence_type === "human_confirm"
    ? '<span class="pill ev-human">人工确认</span>'
    : (t.evidence_type === "captured" ? `<span class="pill ev-captured">captured ${short(t.head_sha)}</span>` : ""));
const kindBadge = k => k === "human"
  ? '<span class="pill" style="background:#1d2740;color:#8bb8ff">human</span>'
  : '<span class="pill st-active">cli_agent</span>';
const messagesErrorHTML = () => messagesError
  ? `<div id="messages-error" class="banner">消息加载失败：${esc(messagesError)}</div>`
  : '<div id="messages-error"></div>';
const messagesInboxHTML = () => `<table id="inbox-table"><tr><th>来自</th><th>内容</th><th>时间</th><th></th></tr>
${messagesCache.map(m => `<tr><td>${esc(m.sender_id || "")}</td><td>${esc(m.body)} ${m.state === "pending" || m.state === "delivered" ? '<span class="pill st-running">new</span>' : ""}</td><td class="muted">${esc(m.created_at)}</td>
<td>${m.state !== "acknowledged" ? `<button class="btn" onclick="ackMsg('${m.id}')">ack</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="4" class="muted">无</td></tr>'}
</table>`;

// api() 解包 Envelope: 成功返回 data.data, 失败抛出 {code, message}。
async function api(method, path, body) {
  const headers = { "Accept": "application/json" };
  const secret = sessionStorage.getItem(KEY);
  if (secret) headers["X-Coordplane-Credential"] = secret;
  if (body) headers["Content-Type"] = "application/json";
  const resp = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  const text = await resp.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch (e) { data = { raw: text }; } }
  if (!resp.ok || (data && data.ok === false)) {
    const code = data && data.error ? data.error.code : "HTTP " + resp.status;
    const message = data && data.error ? data.error.message : text;
    const err = new Error(code + ": " + message);
    err.code = code;
    err.message = message;
    throw err;
  }
  return data ? data.data : null;
}
const rid = () => "web-" + Math.random().toString(36).slice(2, 10);
const leaveEdit = targetView => { editMode = ""; view = targetView; render(); };

function toast(msg, isErr) {
  const t = $("toast");
  t.textContent = msg;
  t.style.background = isErr ? "#3d1a1c" : "#122a3f";
  t.style.borderColor = isErr ? "var(--red)" : "var(--accent)";
  t.classList.add("show");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.remove("show"), 3000);
}

async function loadData() {
  const [p, a, t, r, parts] = await Promise.all([
    api("GET", "/v1/projects?limit=100"),
    api("GET", "/v1/agents?limit=100"),
    api("GET", "/v1/tasks?limit=100"),
    api("GET", "/v1/runs?limit=100"),
    api("GET", "/v1/participants"),
  ]);
  projects = p.items || [];
  agents = a.items || [];
  tasks = t.items || [];
  runs = r.items || [];
  participants = parts || [];
}

// ---------- 登录 / 凭据 ----------
async function boot() {
  try { await loadData(); render(); }
  catch (e) {
    if (e.code === "SCOPE_DENIED") showLogin("");
    else { toast("无法连接 daemon: " + e.message, true); }
  }
}
function showLogin(errMsg) {
  view = "login";
  $("main").innerHTML = `
    <div class="login">
      <h1>Coord<span style="color:var(--accent)">Plane</span> 登录</h1>
      <label class="muted" style="font-size:12px">凭据 secret(仅存于本标签页 sessionStorage)</label>
      <input id="login-secret" type="password" style="width:100%;margin-top:6px" placeholder="≥16 字符">
      <div class="err">${esc(errMsg || "")}</div>
      <div style="margin-top:10px;display:flex;gap:8px">
        <button class="btn primary" onclick="login(false)">登录</button>
        <button class="btn" onclick="login(true)">首次配置(创建凭据)</button>
      </div>
    </div>`;
}
async function login(bootstrap) {
  const secret = $("login-secret").value.trim();
  if (secret.length < 16) { $(".login .err").textContent = "secret 至少 16 字符"; return; }
  try {
    if (bootstrap) {
      // AddCredentialInput 无 json tag -> 网络字段为大写; 字段: ParticipantID, Kind, Secret, RequestID
      await api("POST", "/v1/credentials", { ParticipantID: "participant-owner", Kind: "operator_token", Secret: secret, RequestID: rid() });
    }
    sessionStorage.setItem(KEY, secret);
    await loadData();
    render();
  } catch (e) {
    if (e.code === "SCOPE_DENIED") { $(".login .err").textContent = bootstrap ? "凭据已存在,请用登录" : "凭据无效或已吊销"; }
    else { $(".login .err").textContent = "失败: " + e.message; }
  }
}

// ---------- 渲染 ----------
function render() {
  document.querySelectorAll("#nav a").forEach(a => a.classList.toggle("on", a.dataset.v === view));
  const m = $("main");
  if (view === "login") return;
  if (editMode) return;
  if (view === "task") { m.innerHTML = renderTaskDetail(); return; }
  if (view === "agent") { m.innerHTML = renderAgentDetail(); return; }
  if (view === "project") { m.innerHTML = renderProjectDetail(); return; }
  if (view === "events") {
    if (aguiLive) {
      const hist = m.querySelector("#events-history");
      if (hist) hist.innerHTML = eventsHistoryHTML();
      return;
    }
    m.innerHTML = views.events();
    aguiLive = true;
    startAguiStream();
    return;
  }
  aguiLive = false;
  m.innerHTML = views[view]();
}

// AG-UI 实时事件流:fetch + ReadableStream 消费 SSE(/v1/events/stream),
// 事件词汇为 run_start / text_message / tool_call / run_complete。
// EventSource 不支持自定义凭据头,故用 fetch 流式读取。
let aguiStreamToken = 0;
let aguiLastId = 0;
let aguiFilter = "";
let aguiLive = false;
let aguiPaused = false;

const AGUI_LABEL = { run_start: "启动", text_message: "消息", tool_call: "调用", run_complete: "结束" };
const AGUI_CLS = { run_start: "agui-run", text_message: "agui-msg", tool_call: "agui-tool", run_complete: "agui-end" };

async function startAguiStream() {
  const token = ++aguiStreamToken;
  const box = $("agui-stream");
  const status = $("agui-status");
  if (!box) return;
  const hadRows = box.querySelectorAll(".agui-row").length > 0;
  if (!hadRows) box.innerHTML = '<div class="muted agui-placeholder">连接中…</div>';
  const connect = async (attempt) => {
    if (aguiPaused || aguiStreamToken !== token) return;
    const url = "/v1/events/stream?after=" + aguiLastId + (aguiFilter ? "&types=" + encodeURIComponent(aguiFilter) : "");
    try {
      const resp = await fetch(url, {
        headers: { "X-Coordplane-Credential": sessionStorage.getItem(KEY) || "" },
      });
      if (!resp.ok || !resp.body) {
        status.textContent = "流不可用(" + resp.status + ")";
        scheduleReconnect(token, attempt);
        return;
      }
      status.textContent = "已连接" + (aguiLastId > 0 ? " · 续传" : "");
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          const dline = block.split("\n").find(l => l.startsWith("data: "));
          if (!dline) continue;
          try {
            const ev = JSON.parse(dline.slice(6));
            if (aguiStreamToken !== token || aguiPaused) return;
            if (typeof ev.id === "number" && ev.id > aguiLastId) aguiLastId = ev.id;
            const row = document.createElement("div");
            row.className = "agui-row " + (AGUI_CLS[ev.type] || "");
            const label = AGUI_LABEL[ev.type] || ev.type;
            row.innerHTML = `<span class="pill agui-badge">${esc(label)}</span> <span class="mono">${esc(ev.run_id || ev.task_id || ev.message_id || "")}</span><span class="muted"> ${esc(ev.summary ? "— " + ev.summary : "")}</span>`;
            const ph = box.querySelector(".agui-placeholder");
            if (ph) ph.remove();
            box.prepend(row);
            while (box.childElementCount > 120) box.removeChild(box.lastChild);
          } catch (e) { /* 忽略坏块 */ }
        }
      }
    } catch (e) {
      if (aguiStreamToken !== token) return;
      status.textContent = "已断开,重连中…";
      scheduleReconnect(token, attempt);
    }
  };
  await connect(0);
}

function scheduleReconnect(token, attempt) {
  if (aguiPaused || aguiStreamToken !== token) return;
  setTimeout(() => { if (aguiStreamToken === token && !aguiPaused) startAguiStream(); }, 3000);
}

function aguiSetFilter(v) { aguiFilter = v; aguiStreamToken++; aguiLastId = 0; startAguiStream(); }
function aguiTogglePause() {
  aguiPaused = !aguiPaused;
  const btn = $("agui-pause");
  if (btn) btn.textContent = aguiPaused ? "继续" : "暂停";
  if (!aguiPaused) { aguiStreamToken++; startAguiStream(); }
}
function aguiClear() { const box = $("agui-stream"); if (box) box.innerHTML = ""; }
function eventSummary(e) {
  let txt = "";
  try {
    const p = JSON.parse(e.payload_json || "{}");
    txt = p.task_title || p.title || p.message || p.note || p.error || p.reason || "";
    if (!txt) txt = [p.launch_mode && "mode:" + p.launch_mode, p.phase, p.task_id, p.run_id, p.agent_id, p.project_id].filter(Boolean).join(" · ");
  } catch (_) { txt = ""; }
  return esc(txt.slice(0, 60));
}
function eventsHistoryHTML() {
  return `<table><tr><th>时间</th><th>事件</th><th>实体</th><th>actor</th><th>备注</th></tr>
  ${eventsCache.map(e => `<tr><td class="mono">${esc(e.created_at)}</td><td>${esc(e.kind)}</td><td class="mono">${esc(e.entity_id)}</td><td>${esc(e.actor_kind)}:${esc(e.actor_id)}</td><td class="muted">${eventSummary(e)}</td></tr>`).join("") || '<tr><td colspan="5" class="muted">无</td></tr>'}
  </table>`;
}

const views = {
  dash() {
    const act = tasks.filter(t => t.status === "running");
    return `
    <h1>Dashboard</h1>
    <div class="cards">
      <div class="card"><div class="num">${projects.length}</div><div class="lbl">Projects</div></div>
      <div class="card"><div class="num">${agents.length}</div><div class="lbl">Agents</div></div>
      <div class="card"><div class="num">${act.length}</div><div class="lbl">Running tasks</div></div>
      <div class="card"><div class="num">${tasks.filter(t => t.status === "waiting").length}</div><div class="lbl">Waiting(需处理)</div></div>
    </div>
    <h2>Agent 进度</h2>
    <table><tr><th>Agent</th><th>状态</th><th>当前任务</th><th>进度</th><th></th></tr>
    ${agents.map(a => { const t = tasks.find(x => x.assignee_agent_id === a.id && ["queued", "running", "finishing", "waiting"].includes(x.status));
      return `<tr><td><b>${esc(a.display_name)}</b></td><td>${pill(a.status)}</td>
      <td>${t ? esc(t.title) + " " + pill(t.status) : '<span class="muted">空闲</span>'}</td>
      <td><span class="progress"><i style="width:${t ? (t.status === "running" ? 60 : t.status === "waiting" ? 40 : 20) : 0}%"></i></span></td>
      <td><button class="btn" onclick="openAgent('${a.id}')">详情</button></td></tr>`; }).join("")}
    </table>
    <h2>项目 <button class="btn" style="float:right" onclick="newProjectForm()">+ 新建项目</button></h2>
    <table>
    <th>名称</th><th>状态</th><th>任务</th><th>canonical</th><th>集成 Agent</th><th>错误</th><th></th>
    ${projects.map(p => { const pt = tasks.filter(t => t.project_id === p.id);
      return `<tr><td><a href="#" onclick="openProject('${p.id}');return false" style="color:var(--accent)">${esc(p.name)}</a></td><td>${pill(p.status)}</td>
      <td>${pt.length} 个任务 <span class="muted">(${pt.filter(t => t.status === "running").length} 运行 / ${pt.filter(t => t.status === "failed").length} 失败)</span></td>
      <td class="mono">${short(p.canonical_sha)}</td><td>${agentName(p.integration_agent_id)}</td><td class="muted">${esc(p.last_error || "")}</td>
      <td>${p.status === "active" ? `<button class="btn danger" onclick="archiveProject('${p.id}')">归档</button>` : p.status === "error" ? `<button class="btn" onclick="repairProject('${p.id}')">Repair</button>` : p.status === "archived" ? `<button class="btn danger" onclick="deleteProject('${p.id}')">删除</button>` : ""}</td></tr>`; }).join("")}
    </table>`;
  },
  projects() {
    const filtered = projects.filter(p => !projFilter || p.status === projFilter);
    return `
    <h1>项目 <button class="btn primary" style="float:right" onclick="newProjectForm()">+ 新建项目</button></h1>
    <div style="margin-bottom:10px">
      <select id="proj-filter" onchange="projFilter=this.value;render()">
        <option value="">全部状态</option>
        ${["active", "creating", "error", "archived"].map(st => `<option value="${st}">${st}</option>`).join("")}
      </select>
    </div>
    <table><tr><th>名称</th><th>状态</th><th>任务</th><th>canonical</th><th>集成 Agent</th><th>错误</th><th>操作</th></tr>
    ${filtered.map(p => `<tr><td><a href="#" onclick="openProject('${p.id}');return false" style="color:var(--accent)"><b>${esc(p.name)}</b></a></td>
    <td>${pill(p.status)}</td><td>${tasks.filter(t => t.project_id === p.id).length} 个任务</td><td class="mono">${short(p.canonical_sha)}</td><td>${agentName(p.integration_agent_id)}</td><td class="muted">${esc(p.last_error || "")}</td>
    <td>${p.status === "active" ? `<button class="btn danger" onclick="archiveProject('${p.id}')">归档</button>` : ""}
    ${p.status === "error" ? `<button class="btn" onclick="repairProject('${p.id}')">Repair</button>` : ""}
    ${p.status === "archived" ? `<button class="btn danger" onclick="deleteProject('${p.id}')">删除</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="7" class="muted">无项目,点右上角新建</td></tr>'}
    </table>`;
  },
  agents() {
    return `
    <h1>Agents</h1>
    <div style="margin-bottom:10px"><button class="btn primary" onclick="newAgentForm()">+ 新建 Agent</button></div>
    <table><tr><th>Agent</th><th>状态</th><th>adapter</th><th>当前任务</th><th>操作</th></tr>
    ${agents.map(a => { const t = tasks.find(x => x.assignee_agent_id === a.id && ["queued", "running", "finishing", "waiting"].includes(x.status));
      return `<tr><td><b>${esc(a.display_name)}</b></td><td>${pill(a.status)}</td><td class="mono">${esc(a.adapter_id)}</td>
      <td>${t ? esc(t.title) : '<span class="muted">空闲</span>'}</td>
      <td><button class="btn" onclick="openAgent('${a.id}')">进度/配置</button>
      ${a.status === "active" ? `<button class="btn" onclick="agentAction('${a.id}','pause')">暂停</button>` : `<button class="btn" onclick="agentAction('${a.id}','resume')">恢复</button>`}
      <button class="btn danger" onclick="agentAction('${a.id}','archive')">归档</button></td></tr>`; }).join("")}
    </table>`;
  },
  tasks() {
    const cols = showCancelled ? ["queued", "running", "waiting", "submitted", "completed", "failed", "cancelled"] : ["queued", "running", "waiting", "submitted", "completed", "failed"];
    const shown = tasks.filter(t => !taskProjFilter || t.project_id === taskProjFilter);
    const activeProjs = projects.filter(p => p.status !== "archived");
    const cancelledN = shown.filter(t => t.status === "cancelled").length;
    return `
    <h1>Tasks <button class="btn primary" style="float:right" onclick="newTaskForm()">+ 创建任务</button></h1>
    <div style="margin-bottom:10px;display:flex;gap:8px;align-items:center">
      <span class="muted">按项目筛选:</span>
      <select id="task-proj-filter" onchange="taskProjFilter=this.value;render()">
        <option value="">全部项目</option>
        ${activeProjs.map(p => `<option value="${p.id}" ${taskProjFilter === p.id ? "selected" : ""}>${esc(p.name)}</option>`).join("")}
      </select>
      <span class="muted" id="task-filter-info">${taskProjFilter ? `当前: ${projName(taskProjFilter)} · ${shown.length} 个任务` : `共 ${tasks.length} 个任务`}</span>
      <label style="margin-left:auto" title="取消的任务会保留审计记录,默认隐藏"><input type="checkbox" ${showCancelled ? "checked" : ""} onchange="showCancelled=this.checked;render()"> 显示已取消 (${cancelledN})</label>
    </div>
    <div class="board">
      ${cols.map(st => `<div class="col"><h3>${st}</h3>${shown.filter(t => t.status === st).map(tc).join("") || '<div class="muted">—</div>'}</div>`).join("")}
    </div>`;
  },
  runs() {
    return `<h1>Runs</h1>
    <table><tr><th>Run</th><th>Task</th><th>Agent</th><th>Gen</th><th>状态</th><th>cleanup</th><th>开始</th><th></th></tr>
    ${runs.map(r => `<tr><td class="mono">${esc(r.id)}</td><td class="mono">${esc(r.task_id)}</td><td>${agentName(r.agent_id)}</td><td>${r.generation}</td><td>${pill(r.state)}</td><td class="muted">${esc(r.cleanup_state || "")}</td><td class="muted">${esc(r.started_at || "")}</td>
    <td>${r.state === "active" || r.state === "starting" ? `<button class="btn danger" onclick="runStop('${r.id}')">Stop</button>` : ""}</td></tr>`).join("")}
    </table>`;
  },
  messages() {
    const activeAgents = agents.filter(a => a.status === "active");
    return `<h1>Messages(收件箱)</h1>
    ${messagesErrorHTML()}
    ${messagesInboxHTML()}
    <h2>发送消息(chat)</h2>
    <div class="form-grid">
      <label>项目</label><select id="chat-proj" onchange="beginChatEdit()">${projects.map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join("")}</select>
      <label>Agent</label><select id="chat-agent" onchange="beginChatEdit()">${activeAgents.map(a => `<option value="${a.id}">${esc(a.display_name)}</option>`).join("")}</select>
      <label>内容</label><textarea id="chat-body" rows="2" oninput="beginChatEdit()"></textarea>
      <button class="btn primary" onclick="sendChat()">发送</button>
    </div>`;
  },
  roles() {
    return `<h1>Roles &amp; Participants <button class="btn primary" style="float:right" onclick="newRoleForm()">+ 新建角色</button></h1>
    <table><tr><th>角色</th><th>capabilities</th><th>操作</th></tr>
    ${roles.map(r => `<tr><td><b>${esc(r.Name)}</b> <span class="muted">${esc(r.Description || "")}</span></td><td class="muted">${(r.Capabilities || []).map(esc).join(", ")}</td>
    <td><button class="btn" onclick="editRoleForm('${r.ID}')">编辑</button><button class="btn danger" onclick="deleteRole('${r.ID}')">删除</button></td></tr>`).join("") || '<tr><td colspan="3" class="muted">—</td></tr>'}
    </table>
    <h2>Participants <button class="btn" style="float:right" onclick="newBindForm()">+ 绑定角色</button></h2>
    <table><tr><th>ID</th><th>kind</th><th>状态</th><th>凭据</th></tr>
    ${participantsCache.map(p =>
      `<tr><td class="mono">${esc(p.ID)}</td><td>${kindBadge(p.Kind)}</td><td>${pill(p.Status)}</td><td class="mono muted">${esc(p.CredentialID || "")}</td></tr>`
    ).join("") || '<tr><td colspan="4" class="muted">无</td></tr>'}
    </table>`;
  },
  creds() {
    return `<h1>Credentials</h1>
    <div class="detail">
      <div class="row"><span class="k">状态</span><span class="pill ${credsCache && credsCache.status === "active" ? "st-active" : "st-cancelled"}">${credsCache ? esc(credsCache.status) : "—"}</span></div>
      <div class="row"><span class="k">kind</span>${credsCache ? esc(credsCache.kind) : "—"}</div>
      <div class="row"><span class="k">创建</span>${credsCache ? esc(credsCache.created_at) : "—"}</div>
      <div class="actions">
        <button class="btn" onclick="rotateCred()">Rotate</button>
        <button class="btn danger" onclick="revokeCred()">Revoke</button>
      </div>
    </div>`;
  },
  events() {
    return `<h1>Events</h1>
    <h2 style="margin-top:18px">实时流 <span class="muted">(AG-UI 词汇: run_start / text_message / tool_call / run_complete)</span></h2>
    <div class="agui-tools">
      <select id="agui-filter" onchange="aguiSetFilter(this.value)">
        <option value="">全部类型</option>
        <option value="run_start">run_start</option>
        <option value="text_message">text_message</option>
        <option value="tool_call">tool_call</option>
        <option value="run_complete">run_complete</option>
      </select>
      <button class="btn" id="agui-pause" onclick="aguiTogglePause()">暂停</button>
      <button class="btn" onclick="aguiClear()">清空</button>
      <span class="muted" id="agui-status">连接中…</span>
    </div>
    <div id="agui-stream" class="agui-stream"><div class="muted">连接中…</div></div>
    <h2 style="margin-top:18px">历史</h2>
    <div id="events-history">${eventsHistoryHTML()}</div>`;
  },
  gc() {
    const ws = gcPreview ? gcPreview.workspaces : [];
    const notEl = ws.filter(w => w.exists && !w.eligible).length;
    return `<h1>GC 维护</h1>
    <div style="margin-bottom:10px"><button class="btn" onclick="refreshGCPreview()">生成预览</button> <button class="btn danger" onclick="runGC()">执行 GC(需确认)</button></div>
    ${gcPreview ? `<div class="muted" style="margin-bottom:10px">生成时间: ${esc(gcPreview.generated_at)}</div>
    ${notEl > 0 ? `<div class="muted" style="margin-bottom:8px">⚠ 可回收标记为 ✗ 的资源仍在 24h 保留期内或属于已归档项目,到期后会自动变为可回收。</div>` : ""}
    <h2>工作区 (${gcPreview.workspaces.length})</h2>
    <table><tr><th>任务</th><th>版本</th><th>存在</th><th>fingerprint</th><th>actual_head</th><th>可回收</th><th>原因</th><th></th></tr>
    ${gcPreview.workspaces.map(w => `<tr><td class="mono">${esc(w.task_id)}</td><td>${w.task_version}</td><td>${w.exists ? "✓" : "—"}</td><td class="mono">${short(w.fingerprint)}</td><td class="mono">${short(w.actual_head)}</td><td>${w.eligible ? "✓" : "✗"}</td><td class="muted">${(w.reasons || []).map(esc).join(", ")}</td>
    <td>${w.exists ? `<button class="btn" onclick="discardWorkspace('${w.task_id}','${w.fingerprint}')">丢弃</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="8" class="muted">无</td></tr>'}
    </table>
    <h2>Task Ref (${gcPreview.task_refs.length})</h2>
    <table><tr><th>任务</th><th>Run</th><th>actual_sha</th><th>存在</th><th>可回收</th><th>原因</th><th></th></tr>
    ${gcPreview.task_refs.map(r => `<tr><td class="mono">${esc(r.task_id)}</td><td class="mono">${esc(r.run_id)}</td><td class="mono">${short(r.actual_sha)}</td><td>${r.exists ? "✓" : "—"}</td><td>${r.eligible ? "✓" : "✗"}</td><td class="muted">${(r.reasons || []).map(esc).join(", ")}</td>
    <td>${r.exists ? `<button class="btn" onclick="discardTaskRef('${r.task_id}','${r.run_id}','${r.actual_sha}')">丢弃</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="7" class="muted">无</td></tr>'}
    </table>
    <h2>Agent Homes (${gcPreview.agent_homes.length})</h2>
    <table><tr><th>Agent</th><th>存在</th><th>可回收</th><th>原因</th></tr>
    ${gcPreview.agent_homes.map(h => `<tr><td class="mono">${esc(h.agent_id)}</td><td>${h.exists ? "✓" : "—"}</td><td>${h.eligible ? "✓" : "✗"}</td><td class="muted">${(h.reasons || []).map(esc).join(", ")}</td></tr>`).join("") || '<tr><td colspan="4" class="muted">无</td></tr>'}
    </table>` : '<div class="muted">点击"生成预览"查看可回收资源。</div>'}
    `;
  },
};

function tc(t) {
  const proj = t.project_id ? `<span class="pill" style="background:#1d2740;color:#8bb8ff">${projName(t.project_id) || esc(t.project_id)}</span> ` : "";
  return `<div class="tcard" onclick="openTask('${t.id}')"><div class="title">${esc(t.title)}</div><div class="meta">${proj}${agentName(t.assignee_agent_id || t.assignee_participant_id)} · ${pill(t.status)} ${evBadge(t)}</div></div>`;
}

function beginChatEdit() { editMode = "chat"; }

function refreshMessagesUI() {
  if (view !== "messages") return;
  const error = $("messages-error");
  if (error) error.outerHTML = messagesErrorHTML();
  const table = $("inbox-table");
  if (table) table.outerHTML = messagesInboxHTML();
}

async function ackMsg(id) {
  try {
    await api("POST", "/v1/messages/" + encodeURIComponent(id) + "/ack", { request_id: rid() });
    toast("已 ack");
  } catch (e) { toast(e.message, true); }
  await refreshMessages();
  render();
}

async function refreshMessages() {
  try {
    const d = await api("GET", "/v1/messages?recipient_kind=boss&limit=20");
    messagesCache.length = 0;
    messagesCache.push(...(d.items || []));
    messagesError = "";
  } catch (e) {
    messagesError = e.message || String(e);
    console.error("刷新 Messages 失败:", e);
  }
  if (editMode === "chat") refreshMessagesUI();
}
async function sendChat() {
  const bodyEl = $("chat-body");
  if (!bodyEl) return;
  const body = bodyEl.value.trim();
  if (!body) { toast("消息内容不能为空", true); return; }
  try {
    await api("POST", "/v1/chat", {
      project_id: $("chat-proj").value,
      agent_id: $("chat-agent").value,
      body,
      request_id: rid(),
    });
    toast("已发送");
    bodyEl.value = "";
    editMode = "";
    await refreshMessages();
    render();
  } catch (e) { toast(e.message, true); }
}
async function refreshRoles() { try { roles = await api("GET", "/v1/roles") || []; } catch (e) { /* 忽略 */ } }
async function refreshParticipants() { try { participantsCache = await api("GET", "/v1/participants") || []; } catch (e) { /* 忽略 */ } }
async function refreshEvents() { try { const d = await api("GET", "/v1/events?limit=100"); eventsCache.length = 0; eventsCache.push(...(d.items || [])); } catch (e) { /* 忽略 */ } }
async function refreshCreds() {
  try {
    const parts = await api("GET", "/v1/participants");
    const me = (parts || []).find(p => p.ID === "participant-owner");
    credsCache = me && me.CredentialID ? { status: "active", kind: "operator_token", created_at: "" } : null;
  } catch (e) { /* 忽略 */ }
}

// ---------- 任务详情与控制 ----------
async function openTask(id) { view = "task"; detailTask = id; render();
  try { const d = await api("GET", "/v1/tasks/" + encodeURIComponent(id)); window._td = d; render(); } catch (e) { toast(e.message, true); } }
function renderTaskDetail() {
  const d = window._td, t = d ? d.task : null;
  if (!t) return `<h1>Loading…</h1>`;
  const isHuman = t.assignee_agent_id === "" && t.assignee_participant_id;
  const run = d.current_run;
  return `
  <h1>Task <span class="mono">${esc(t.id)}</span></h1>
  <div class="detail">
    <div class="row"><span class="k">标题</span><b>${esc(t.title)}</b> ${pill(t.status)} ${evBadge(t)}</div>
    <div class="row"><span class="k">项目</span>${t.project_id ? `<a href="#" onclick="openProject('${esc(t.project_id)}');return false" style="color:var(--accent)">${projName(t.project_id) || esc(t.project_id)}</a>` : '<span class="muted">—</span>'}</div>
    <div class="row"><span class="k">Kind</span>${esc(t.kind || "work")}</div>
    <div class="row"><span class="k">Assignee</span>${agentName(t.assignee_agent_id || t.assignee_participant_id)}</div>
    <div class="row"><span class="k">Priority</span>${t.priority}</div>
    <div class="row"><span class="k">重试</span>${t.retry_count}/${t.max_retries}${t.budget_seconds ? ` · 预算 ${t.budget_seconds}s` : ""}</div>
    <div class="row"><span class="k">Generation</span>${t.generation}${t.next_run_at ? " · next_run_at " + esc(t.next_run_at) : ""}</div>
    <div class="row"><span class="k">WaitReason</span><span class="muted">${esc(t.wait_reason || "")}</span></div>
    <div class="row"><span class="k">Base / Head</span><span class="mono">${short(t.base_sha)} / ${t.evidence_type === "human_confirm" ? "(人工确认,无捕获)" : short(t.head_sha)}</span></div>
    <div class="row"><span class="k">Task ref</span><span class="mono">${esc(t.task_ref || "")}</span></div>
    <div class="row"><span class="k">结果</span><span class="muted">${esc(t.result_summary || "")}</span></div>
    <div class="row"><span class="k">失败原因</span><span class="muted">${esc(t.failure_reason || "")}</span></div>
    ${run ? `<div class="row"><span class="k">当前 Run</span><span class="mono">${esc(run.id)}</span> ${pill(run.state)}</div>` : ""}
    <div class="actions">
      ${t.status === "waiting" && isHuman ? `<button class="btn primary" onclick="completeTask('${t.id}')">Complete(输入 summary)</button>` : ""}
      ${t.status === "submitted" ? `<button class="btn primary" onclick="acceptTask('${t.id}')">Accept</button>` : ""}
      ${["queued", "running", "waiting", "submitted", "failed"].includes(t.status) ? `<button class="btn" onclick="taskAction('${t.id}','rework')">Rework</button>` : ""}
      ${["queued", "running", "waiting"].includes(t.status) ? `<button class="btn danger" onclick="taskAction('${t.id}','cancel')">Cancel</button>` : ""}
      ${t.status === "failed" ? `<button class="btn" onclick="taskAction('${t.id}','retry')">Retry</button>` : ""}
      ${t.status === "waiting" ? `<button class="btn" onclick="taskAction('${t.id}','wake')">Wake</button>` : ""}
      ${t.status === "waiting" ? `<button class="btn" onclick="closeConversation('${t.id}')">Close 会话</button>` : ""}
      ${["completed", "cancelled"].includes(t.status) ? `<button class="btn danger" onclick="deleteTask('${t.id}')">Delete</button>` : ""}
    </div>
  </div>
  <div style="margin-top:10px"><button class="btn" onclick="view='tasks';render();refreshAll()">← 返回看板</button></div>`;
}
async function completeTask(id) {
  const summary = prompt("Result summary(human_confirm 证据):", "reviewed");
  if (summary === null) return;
  try { await api("POST", "/v1/tasks/" + encodeURIComponent(id) + "/complete", { summary, request_id: rid() }); toast("已完成(evidence=human_confirm)"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function acceptTask(id) {
  const agentId = agents.length ? agents[0].id : "";
  try { await api("POST", "/v1/tasks/" + encodeURIComponent(id) + "/accept", { integration_agent_id: agentId, request_id: rid() }); toast("已 Accept"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function taskAction(id, action) {
  try { await api("POST", `/v1/tasks/${encodeURIComponent(id)}/${action}`, { request_id: rid() }); toast(action + " 已提交"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function closeConversation(id) {
  try { await api("POST", `/v1/tasks/${encodeURIComponent(id)}/close`, { request_id: rid() }); toast("会话已关闭"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function deleteTask(id) {
  if (!confirm("永久删除该任务及其所有 Run/消息/事件记录？此操作不可恢复。")) return;
  const reason = (prompt("删除原因(可选，写入审计事件):", "") || "").trim();
  try { await api("POST", `/v1/tasks/${encodeURIComponent(id)}/delete`, { reason, request_id: rid() }); toast("任务已永久删除"); } catch (e) { toast(e.message, true); }
  view = "tasks"; render(); refreshAll();
}
async function runStop(id) {
  try { await api("POST", "/v1/runs/" + encodeURIComponent(id) + "/stop", { request_id: rid() }); toast("Stop 已请求"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function agentAction(id, action) {
  try { await api("POST", `/v1/agents/${encodeURIComponent(id)}/${action}`, { request_id: rid() }); toast(action + " 已提交"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
// ---------- 项目 ----------
function newProjectForm() {
  editMode = "project";
  $("main").innerHTML = `
  <h1>新建项目</h1>
  <div class="form-grid">
    <label>名称</label><input id="p-name" placeholder="项目名(唯一)">
    <label>Source(git 地址)</label><input id="p-source" placeholder="https://github.com/.../.git">
    <label>SourceRef</label><input id="p-source-ref" value="main">
    <label>集成 Agent(可空)</label><select id="p-integration">${agents.filter(a => a.status === "active").map(a => `<option value="${a.id}">${esc(a.display_name)}</option>`).join("")}<option value="">(无)</option></select>
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createProject()">创建</button><button class="btn" onclick="leaveEdit('projects')">取消</button></div>
  </div>`;
}
async function createProject() {
  const body = { name: $("p-name").value.trim(), source: $("p-source").value.trim(), source_ref: $("p-source-ref").value.trim(), request_id: rid() };
  const integ = $("p-integration").value;
  if (integ) body.integration_agent_id = integ;
  try {
    await api("POST", "/v1/projects", body);
    toast("项目已创建");
    editMode = "";
    view = "projects"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
async function repairProject(id) {
  try { await api("POST", "/v1/projects/" + encodeURIComponent(id) + "/repair", { request_id: rid() }); toast("Repair 已提交"); } catch (e) { toast(e.message, true); }
  if (view === "project") openProject(id); else refreshAll();
}
async function deleteProject(id) {
  if (!confirm("永久删除该已归档项目及其所有任务/Run/消息/事件记录？此操作不可恢复。")) return;
  const reason = (prompt("删除原因(可选，写入审计事件):", "") || "").trim();
  try { await api("POST", `/v1/projects/${encodeURIComponent(id)}/delete`, { reason, request_id: rid() }); toast("项目已永久删除"); } catch (e) { toast(e.message, true); }
  view = "projects"; render(); refreshAll();
}
async function openProject(id) {
  view = "project"; detailProject = id; render();
  try { const d = await api("GET", "/v1/projects/" + encodeURIComponent(id)); window._pd = d; render(); }
  catch (e) { toast(e.message, true); }
}
function viewBoard(pid) { taskProjFilter = pid; view = "tasks"; render(); }
function renderProjectDetail() {
  const d = window._pd;
  if (!d || !d.id) return `<h1>Loading…</h1>`;
  const myTasks = tasks.filter(t => t.project_id === d.id);
  return `
  <h1>项目 <span class="mono">${esc(d.name)}</span></h1>
  <div class="detail">
    <div class="row"><span class="k">状态</span>${pill(d.status)}${d.pending_action ? ` · <span class="muted">动作: ${esc(d.pending_action)}</span>` : ""}</div>
    <div class="row"><span class="k">Source</span><span class="mono">${esc(d.source)}</span></div>
    <div class="row"><span class="k">SourceRef</span><span class="mono">${esc(d.source_ref)}</span></div>
    <div class="row"><span class="k">Canonical ref</span><span class="mono">${esc(d.canonical_ref || "—")}</span></div>
    <div class="row"><span class="k">Canonical sha</span><span class="mono">${esc(d.canonical_sha || "—")}</span></div>
    <div class="row"><span class="k">实际 sha</span>${d.actual_canonical_sha ? `<span class="mono">${esc(d.actual_canonical_sha)}</span>` : `<span class="muted">${esc(d.actual_canonical_error || "解析中…")}</span>`}</div>
    <div class="row"><span class="k">初始 sha</span><span class="mono">${esc(d.initial_sha || "—")}</span></div>
    <div class="row"><span class="k">集成 Agent</span>${agentName(d.integration_agent_id)}</div>
    ${d.last_error ? `<div class="row"><span class="k">错误</span><span style="color:var(--red)">${esc(d.last_error)}</span></div>` : ""}
    <div class="row"><span class="k">创建 / 更新</span><span class="muted">${esc(d.created_at)} / ${esc(d.updated_at)}</span></div>
    <div class="row"><span class="k">Version</span><span class="mono">${d.version}</span></div>
    <div class="actions">
      <button class="btn primary" onclick="viewBoard('${d.id}')">查看项目看板</button>
      ${d.status === "active" ? `<button class="btn danger" onclick="archiveProject('${d.id}')">归档</button>` : ""}
      ${d.status === "error" ? `<button class="btn" onclick="repairProject('${d.id}')">Repair</button>` : ""}
      ${d.status === "archived" ? `<button class="btn danger" onclick="deleteProject('${d.id}')">删除</button>` : ""}
    </div>
  </div>
  <h2>项目任务 (${myTasks.length}) <span class="muted">(已取消 ${myTasks.filter(t => t.status === "cancelled").length})</span></h2>
  <table><tr><th>任务</th><th>状态</th><th>Assignee</th><th>证据</th><th>更新</th></tr>
  ${myTasks.filter(t => t.status !== "cancelled").map(t => `<tr><td><a href="#" onclick="openTask('${t.id}');return false" style="color:var(--accent)">${esc(t.title)}</a></td><td>${pill(t.status)}</td><td>${agentName(t.assignee_agent_id || t.assignee_participant_id)}</td><td>${evBadge(t)}</td><td class="muted">${esc(t.updated_at)}</td></tr>`).join("") || '<tr><td colspan="5" class="muted">无</td></tr>'}
  ${myTasks.some(t => t.status === "cancelled") ? `<tr><td colspan="5" class="muted">…另有 ${myTasks.filter(t => t.status === "cancelled").length} 个已取消任务被折叠</td></tr>` : ""}
  </table>
  <div style="margin-top:10px"><button class="btn" onclick="view='projects';render()">← 返回项目</button></div>`;
}
// ---------- Agent ----------
function newAgentForm() {
  editMode = "agent";
  $("main").innerHTML = `
  <h1>新建 Agent</h1>
  <div class="form-grid">
    <label>显示名</label><input id="a-name" placeholder="Agent Name">
    <label>adapter</label><input id="a-adapter" value="claude">
    <label>image</label><input id="a-image" value="node:22-bookworm">
    <label>instructions 文件(绝对路径)</label><input id="a-instr" value="/instructions/agent.md">
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createAgent()">创建</button><button class="btn" onclick="leaveEdit('agents')">取消</button></div>
  </div>`;
}
async function createAgent() {
  try {
    await api("POST", "/v1/agents", { display_name: $("a-name").value.trim(), adapter_id: $("a-adapter").value.trim(), image: $("a-image").value.trim(), instructions_file: $("a-instr").value.trim(), request_id: rid() });
    toast("Agent 已创建");
    editMode = "";
    view = "agents"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
// ---------- 任务 ----------
function newTaskForm() {
  editMode = "task";
  const agentOpts = agents.filter(a => a.status === "active").map(a => `<option value="agent:${a.id}">${esc(a.display_name)}</option>`).join("");
  const humanOpts = participants.filter(p => p.Kind === "human").map(p => `<option value="human:${p.ID}">${esc(p.DisplayName)} (human)</option>`).join("");
  $("main").innerHTML = `
  <h1>创建任务</h1>
  <div class="form-grid">
    <label>标题</label><input id="t-title" placeholder="任务标题">
    <label>项目</label><select id="t-proj">${projects.filter(p => p.status !== "archived").map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join("")}</select>
    <label>Assignee</label><select id="t-assignee">${agentOpts + humanOpts}</select>
    <label>Priority</label><input id="t-prio" type="number" value="0">
    <label>MaxRetries</label><input id="t-retries" type="number" value="0" min="0">
    <label>预算(秒,可空)</label><input id="t-budget" type="number" placeholder="如 600">
    <label>描述</label><textarea id="t-desc" rows="2"></textarea>
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createTask()">创建</button><button class="btn" onclick="leaveEdit('tasks')">取消</button></div>
  </div>`;
}
async function createTask() {
  const sel = $("t-assignee").value;
  const body = { project_id: $("t-proj").value, title: $("t-title").value.trim(), description: $("t-desc").value.trim(), priority: +$("t-prio").value || 0, kind: "work", max_retries: +$("t-retries").value || 0, request_id: rid() };
  const budget = +$("t-budget").value;
  if (budget > 0) body.budget_seconds = budget;
  if (sel.startsWith("agent:")) body.assignee_agent_id = sel.slice(6);
  else body.assignee_participant_id = sel.slice(6);
  try {
    await api("POST", "/v1/tasks", body);
    toast("任务已创建");
    editMode = "";
    view = "tasks"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
// ---------- 角色 / 绑定 ----------
function newRoleForm() {
  editMode = "role";
  $("main").innerHTML = `
  <h1>新建角色</h1>
  <div class="form-grid">
    <label>名称</label><input id="r-name" placeholder="role-name">
    <label>描述</label><input id="r-desc">
    <label>Capabilities(逗号分隔)</label><input id="r-caps" placeholder="task.create, task.assign, ...">
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createRole()">创建</button><button class="btn" onclick="leaveEdit('roles')">取消</button></div>
  </div>`;
}
async function createRole() {
  const caps = $("r-caps").value.split(",").map(s => s.trim()).filter(Boolean);
  try {
    await api("POST", "/v1/roles", { Name: $("r-name").value.trim(), Description: $("r-desc").value.trim(), Capabilities: caps, RequestID: rid() });
    toast("角色已创建");
    editMode = "";
    view = "roles"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
function editRoleForm(id) {
  editMode = "role-edit";
  const r = roles.find(x => x.ID === id);
  $("main").innerHTML = `
  <h1>编辑角色 <span class="mono">${esc(id)}</span></h1>
  <div class="form-grid">
    <label>名称</label><input id="r-name" value="${esc(r.Name)}">
    <label>描述</label><input id="r-desc" value="${esc(r.Description || "")}">
    <label>Capabilities(逗号分隔)</label><input id="r-caps" value="${(r.Capabilities || []).join(", ")}">
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="updateRole('${id}')">保存</button><button class="btn" onclick="leaveEdit('roles')">取消</button></div>
  </div>`;
}
async function updateRole(id) {
  const caps = $("r-caps").value.split(",").map(s => s.trim()).filter(Boolean);
  try {
    await api("PUT", "/v1/roles/" + encodeURIComponent(id), { Name: $("r-name").value.trim(), Description: $("r-desc").value.trim(), Capabilities: caps, RequestID: rid() });
    toast("角色已更新");
    editMode = "";
    view = "roles"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
async function deleteRole(id) {
  if (!confirm("确认删除角色 " + id + " ?")) return;
  try { await api("DELETE", "/v1/roles/" + encodeURIComponent(id), { request_id: rid() }); toast("角色已删除"); } catch (e) { toast(e.message, true); }
  view = "roles"; refreshAll();
}
function newBindForm() {
  editMode = "bind";
  const pOpts = participantsCache.map(p => `<option value="${p.ID}">${esc(p.ID)} (${esc(p.Kind)})</option>`).join("");
  const rOpts = roles.map(r => `<option value="${r.ID}">${esc(r.Name)}</option>`).join("");
  $("main").innerHTML = `
  <h1>绑定角色</h1>
  <div class="form-grid">
    <label>Participant</label><select id="b-participant">${pOpts}</select>
    <label>项目作用域(留空=global)</label><select id="b-project"><option value="global">global</option>${projects.filter(p => p.status !== "archived").map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join("")}</select>
    <label>Role</label><select id="b-role">${rOpts}</select>
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="bindRole()">绑定</button><button class="btn" onclick="leaveEdit('roles')">取消</button></div>
  </div>`;
}
async function bindRole() {
  const body = { ParticipantID: $("b-participant").value, ProjectID: $("b-project").value, RoleID: $("b-role").value, RequestID: rid() };
  try {
    await api("POST", "/v1/participants/" + encodeURIComponent(body.ParticipantID) + "/roles", body);
    toast("已绑定");
    editMode = "";
    view = "roles"; refreshAll();
  } catch (e) { toast(e.message, true); }
}
async function unbindRole(participantID, projectID, roleID) {
  if (!confirm("解绑角色?")) return;
  try { await api("DELETE", "/v1/participants/" + encodeURIComponent(participantID) + "/roles", { ParticipantID: participantID, ProjectID: projectID, RoleID: roleID, RequestID: rid() }); toast("已解绑"); } catch (e) { toast(e.message, true); }
  view = "roles"; refreshAll();
}
// ---------- GC ----------
async function refreshGCPreview() {
  try { gcPreview = await api("GET", "/v1/gc/preview"); render(); } catch (e) { toast(e.message, true); }
}
async function runGC() {
  if (!confirm("确认执行 GC?不可逆。")) return;
  try { const d = await api("POST", "/v1/gc/run", { confirm: true, request_id: rid() }); toast("GC 完成: completed=" + (d ? d.completed : "?")); } catch (e) { toast(e.message, true); }
  refreshGCPreview();
}
async function discardWorkspace(taskID, fingerprint) {
  if (!confirm("丢弃工作区 " + taskID + " ?")) return;
  try { await api("POST", "/v1/gc/discard-workspace", { task_id: taskID, expected_fingerprint: fingerprint, request_id: rid() }); toast("已丢弃工作区"); } catch (e) { toast(e.message, true); }
  refreshGCPreview();
}
async function discardTaskRef(taskID, runID, sha) {
  if (!confirm("丢弃 task ref " + taskID + "/" + runID + " ?")) return;
  try { await api("POST", "/v1/gc/discard-task-ref", { task_id: taskID, run_id: runID, expected_sha: sha, request_id: rid() }); toast("已丢弃 task ref"); } catch (e) { toast(e.message, true); }
  refreshGCPreview();
}
// ---------- 凭据 ----------
async function rotateCred() {
  const secret = prompt("新 secret(≥16 字符):", "");
  if (!secret || secret.length < 16) { toast("secret 至少 16 字符", true); return; }
  try { await api("POST", "/v1/credentials/rotate", { ParticipantID: "participant-owner", Kind: "operator_token", Secret: secret, RequestID: rid() }); sessionStorage.setItem(KEY, secret); toast("凭据已轮换"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function revokeCred() { try { await api("POST", "/v1/credentials/" + encodeURIComponent("participant-owner") + "/revoke", { request_id: rid() }); } catch (e) { /* fence 已拒 */ } sessionStorage.removeItem(KEY); showLogin("凭据已吊销,请轮换后重新登录"); }

async function openAgent(id) { view = "agent"; detailAgent = id; render(); }
function renderAgentDetail() {
  const a = agents.find(x => x.id === detailAgent);
  if (!a) return `<h1>Loading…</h1>`;
  const myTasks = tasks.filter(t => t.assignee_agent_id === a.id);
  const cur = myTasks.find(t => ["queued", "running", "finishing", "waiting"].includes(t.status));
  return `
  <h1>Agent <span class="mono">${esc(a.id)}</span></h1>
  <div class="detail">
    <div class="row"><span class="k">显示名</span><b>${esc(a.display_name)}</b></div>
    <div class="row"><span class="k">状态</span>${pill(a.status)}</div>
    <div class="row"><span class="k">adapter / image</span><span class="mono">${esc(a.adapter_id)} / ${esc(a.image)}</span></div>
    <div class="row"><span class="k">instructions</span><span class="mono">${esc(a.instructions_file)}</span></div>
    <div class="row"><span class="k">创建</span><span class="muted">${esc(a.created_at)}</span></div>
    <div class="actions">
      ${a.status === "active" ? `<button class="btn" onclick="agentAction('${a.id}','pause')">暂停</button>` : `<button class="btn" onclick="agentAction('${a.id}','resume')">恢复</button>`}
      <button class="btn danger" onclick="agentAction('${a.id}','archive')">归档</button>
    </div>
  </div>
  <h2>当前进度</h2>
  <div class="detail">
    <div class="row"><span class="k">当前任务</span>${cur ? esc(cur.title) + " " + pill(cur.status) : '<span class="muted">空闲</span>'}</div>
    <div class="row"><span class="k">进度</span><span class="progress"><i style="width:${cur ? (cur.status === "running" ? 60 : cur.status === "waiting" ? 40 : 20) : 0}%"></i></span></div>
    <div class="row"><span class="k">活跃 Run</span>${runs.find(r => r.agent_id === a.id && (r.state === "active" || r.state === "starting"))?.id || "无"}</div>
  </div>
  <h2>任务(状态 + 证据)</h2>
  <table><tr><th>任务</th><th>状态</th><th>证据</th><th>更新</th></tr>
  ${myTasks.map(t => `<tr><td>${esc(t.title)}</td><td>${pill(t.status)}</td><td>${evBadge(t)}</td><td class="muted">${esc(t.updated_at)}</td></tr>`).join("") || '<tr><td colspan="4" class="muted">无</td></tr>'}
  </table>
  <div style="margin-top:10px"><button class="btn" onclick="view='agents';render()">← 返回</button></div>`;
}

async function refreshAll() {
  try {
    await loadData();
    await Promise.all([refreshMessages(), refreshRoles(), refreshParticipants(), refreshEvents(), refreshCreds()]);
  } catch (e) { if (e.code === "SCOPE_DENIED") { sessionStorage.removeItem(KEY); showLogin("凭据已失效"); } else { toast(e.message, true); } }
  if (!editMode) render();
}
$("nav").addEventListener("click", e => { const a = e.target.closest("a"); if (!a) return; editMode = ""; view = a.dataset.v; render(); if (view === "events") refreshAll(); });
boot();
setInterval(() => { if (view !== "login") refreshAll(); }, 5000);
