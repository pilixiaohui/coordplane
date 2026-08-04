"use strict";
// CoordPlane web frontend — talks to the daemon operator surface (/v1/*).
// Auth: X-Coordplane-Credential from sessionStorage; SCOPE_DENIED -> login.
const KEY = "coordplane_credential";
let view = "dash";
let detailTask = null;
let detailAgent = null;
let projects = [];
let agents = [];
let tasks = [];
let runs = [];
let participants = [];

const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const pill = (st, cls) => `<span class="pill ${cls ? "st-" + st : "st-" + st}">${esc(st)}</span>`;
const agentName = id => {
  const a = agents.find(x => x.id === id);
  if (a) return esc(a.display_name);
  const p = participants.find(x => x.id === id);
  return p ? esc(p.display_name) + ' <span class="muted">(human)</span>' : esc(id || "—");
};
const evBadge = t => t.evidence_type === "human_confirm"
  ? '<span class="pill ev-human">人工确认</span>'
  : (t.evidence_type === "captured" ? `<span class="pill ev-captured">captured ${short(t.head_sha)}</span>` : "");
const short = s => s ? s.slice(0, 8) : "";

async function api(method, path, body) {
  const headers = { "Accept": "application/json" };
  const secret = sessionStorage.getItem(KEY);
  if (secret) headers["X-Coordplane-Credential"] = secret;
  if (body) headers["Content-Type"] = "application/json";
  const resp = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  const text = await resp.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch (e) { data = { raw: text }; } }
  if (!resp.ok) {
    const code = data && data.error ? data.error.code : "HTTP " + resp.status;
    const err = new Error(code + ": " + (data && data.error ? data.error.message : text));
    err.code = code;
    throw err;
  }
  return data;
}
const rid = () => "web-" + Math.random().toString(36).slice(2, 10);

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
    api("GET", "/v1/projects?limit=200"),
    api("GET", "/v1/agents?limit=200"),
    api("GET", "/v1/tasks?limit=500"),
    api("GET", "/v1/runs?limit=500"),
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
    if (bootstrap) await api("POST", "/v1/credentials", { participant_id: "participant-owner", kind: "operator_token", secret, request_id: rid() });
    else { sessionStorage.setItem(KEY, secret); try { await loadData(); render(); return; } catch (e) { sessionStorage.removeItem(KEY); throw e; } }
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
  if (view === "task") { m.innerHTML = renderTaskDetail(); return; }
  if (view === "agent") { m.innerHTML = renderAgentDetail(); return; }
  m.innerHTML = views[view]();
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
    <h2>项目</h2>
    <table><tr><th>名称</th><th>状态</th><th>canonical</th><th>错误</th></tr>
    ${projects.map(p => `<tr><td>${esc(p.name)}</td><td>${pill(p.status)}</td><td class="mono">${short(p.canonical_sha)}</td><td class="muted">${esc(p.last_error || "")}</td></tr>`).join("")}
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
    const cols = ["queued", "running", "waiting", "submitted", "completed", "failed", "cancelled"];
    return `
    <h1>Tasks <button class="btn primary" style="float:right" onclick="newTaskForm()">+ 创建任务</button></h1>
    <div class="board">
      ${cols.map(st => `<div class="col"><h3>${st}</h3>${tasks.filter(t => t.status === st).map(tc).join("") || '<div class="muted">—</div>'}</div>`).join("")}
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
    return `<h1>Messages(收件箱)</h1>
    <table><tr><th>来自</th><th>内容</th><th>时间</th><th></th></tr>
    ${tasks.length ? "" : ""}
    ${messagesCache.map(m => `<tr><td>${esc(m.sender_id)}</td><td>${esc(m.body)} ${m.state === "pending" || m.state === "delivered" ? '<span class="pill st-running">new</span>' : ""}</td><td class="muted">${esc(m.created_at)}</td>
    <td>${m.state !== "acknowledged" ? `<button class="btn" onclick="ackMsg('${m.id}')">ack</button>` : ""}</td></tr>`).join("") || '<tr><td colspan="4" class="muted">无</td></tr>'}
    </table>
    <h2>发送消息(chat)</h2>
    <div class="form-grid">
      <label>项目</label><select id="chat-proj">${projects.map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join("")}</select>
      <label>Agent</label><select id="chat-agent">${agents.filter(a => a.status === "active").map(a => `<option value="${a.id}">${esc(a.display_name)}</option>`).join("")}</select>
      <label>内容</label><textarea id="chat-body" rows="2"></textarea>
      <button class="btn primary" onclick="sendChat()">发送</button>
    </div>`;
  },
  roles() {
    return `<h1>Roles &amp; Participants</h1>
    <table><tr><th>角色</th><th>capabilities</th><th></th></tr>
    ${rolesCache.map(r => `<tr><td><b>${esc(r.name)}</b></td><td class="muted">${(r.capabilities || []).length} 个权限点</td><td><button class="btn" onclick="toast('role edit 走 CLI/后端(role.manage)')">查看</button></td></tr>`).join("") || '<tr><td colspan="3" class="muted">—</td></tr>'}
    </table>
    <h2>Participants</h2>
    <table><tr><th>ID</th><th>kind</th><th>状态</th></tr>
    ${participants.map(p => `<tr><td class="mono">${esc(p.id)}</td><td>${p.kind === "human" ? '<span class="pill kind-human" style="background:#1d2740;color:#8bb8ff">human</span>' : '<span class="pill st-active">cli_agent</span>'}</td><td>${pill(p.status)}</td></tr>`).join("")}
    </table>`;
  },
  creds() {
    return `<h1>Credentials</h1>
    <div class="detail">
      <div class="row"><span class="k">状态</span><span class="pill ${credsCache && credsCache.status === "active" ? "st-active" : "st-cancelled"}">${credsCache ? esc(credsCache.status) : "—"}</span></div>
      <div class="row"><span class="k">kind</span>${credsCache ? esc(credsCache.kind) : "—"}</div>
      <div class="row"><span class="k">创建</span>${credsCache ? esc(credsCache.created_at) : "—"}</div>
      <div class="actions">
        <button class="btn" onclick="toast('轮换:coordplane credential rotate(前端演示)')">Rotate</button>
        <button class="btn danger" onclick="revokeCred()">Revoke</button>
      </div>
    </div>`;
  },
  events() {
    return `<h1>Events</h1>
    <table><tr><th>时间</th><th>事件</th><th>实体</th><th>actor</th><th>备注</th></tr>
    ${eventsCache.map(e => `<tr><td class="mono">${esc(e.created_at)}</td><td>${esc(e.kind)}</td><td class="mono">${esc(e.entity_id)}</td><td>${esc(e.actor_kind)}:${esc(e.actor_id)}</td><td class="muted">${esc((e.payload_json || "").slice(0, 80))}</td></tr>`).join("") || '<tr><td colspan="5" class="muted">无</td></tr>'}
    </table>`;
  },
};

const messagesCache = [], rolesCache = [], eventsCache = [];
let credsCache = null;

function tc(t) {
  return `<div class="tcard" onclick="openTask('${t.id}')"><div class="title">${esc(t.title)}</div><div class="meta">${agentName(t.assignee_agent_id || t.assignee_participant_id)} · ${pill(t.status)} ${evBadge(t)}</div></div>`;
}

async function refreshMessages() { try { const d = await api("GET", "/v1/messages?recipient_kind=boss&limit=200"); messagesCache.length = 0; messagesCache.push(...(d.items || [])); } catch (e) { /* 忽略 */ } }
async function refreshRoles() { try { rolesCache.length = 0; rolesCache.push(...(await api("GET", "/v1/roles"))); } catch (e) { /* 忽略 */ } }
async function refreshEvents() { try { const d = await api("GET", "/v1/events?limit=200"); eventsCache.length = 0; eventsCache.push(...(d.items || [])); } catch (e) { /* 忽略 */ } }
async function refreshCreds() { try { const parts = await api("GET", "/v1/participants"); const me = (parts || []).find(p => p.id === "participant-owner"); credsCache = me && me.credential_id ? { status: "active", kind: "operator_token", created_at: "" } : null; } catch (e) { /* 忽略 */ } }

// ---------- 任务详情与控制 ----------
async function openTask(id) { view = "task"; detailTask = id; render();
  try { const d = await api("GET", "/v1/tasks/" + encodeURIComponent(id)); window._td = d; render(); } catch (e) { toast(e.message, true); } }
function renderTaskDetail() {
  const d = window._td, t = d ? d.task : null;
  if (!t) return `<h1>Loading…</h1>`;
  const isHuman = t.assignee_agent_id === "" && t.assignee_participant_id;
  return `
  <h1>Task <span class="mono">${esc(t.id)}</span></h1>
  <div class="detail">
    <div class="row"><span class="k">标题</span><b>${esc(t.title)}</b> ${pill(t.status)} ${evBadge(t)}</div>
    <div class="row"><span class="k">项目</span>${esc(t.project_id)}</div>
    <div class="row"><span class="k">Assignee</span>${agentName(t.assignee_agent_id || t.assignee_participant_id)}</div>
    <div class="row"><span class="k">Priority</span>${t.priority}</div>
    <div class="row"><span class="k">Base / Head</span><span class="mono">${short(t.base_sha)} / ${t.evidence_type === "human_confirm" ? "(人工确认,无捕获)" : short(t.head_sha)}</span></div>
    <div class="row"><span class="k">Task ref</span><span class="mono">${esc(t.task_ref || "")}</span></div>
    <div class="row"><span class="k">结果</span><span class="muted">${esc(t.result_summary || "")}</span></div>
    <div class="row"><span class="k">失败原因</span><span class="muted">${esc(t.failure_reason || "")}</span></div>
    <div class="actions">
      ${t.status === "waiting" && isHuman ? `<button class="btn primary" onclick="completeTask('${t.id}')">Complete(输入 summary)</button>` : ""}
      ${t.status === "submitted" ? `<button class="btn primary" onclick="acceptTask('${t.id}')">Accept</button>` : ""}
      ${["queued", "running", "waiting", "submitted", "failed"].includes(t.status) ? `<button class="btn" onclick="taskAction('${t.id}','rework')">Rework</button>` : ""}
      ${["queued", "running", "waiting"].includes(t.status) ? `<button class="btn danger" onclick="taskAction('${t.id}','cancel')">Cancel</button>` : ""}
      ${t.status === "failed" ? `<button class="btn" onclick="taskAction('${t.id}','retry')">Retry</button>` : ""}
      ${t.status === "waiting" ? `<button class="btn" onclick="taskAction('${t.id}','wake')">Wake</button>` : ""}
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
async function runStop(id) {
  try { await api("POST", "/v1/runs/" + encodeURIComponent(id) + "/stop", { request_id: rid() }); toast("Stop 已请求"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
async function agentAction(id, action) {
  try { await api("POST", `/v1/agents/${encodeURIComponent(id)}/${action}`, { request_id: rid() }); toast(action + " 已提交"); } catch (e) { toast(e.message, true); }
  refreshAll();
}
function newAgentForm() {
  $("main").innerHTML = `
  <h1>新建 Agent</h1>
  <div class="form-grid">
    <label>显示名</label><input id="a-name" placeholder="Agent Name">
    <label>adapter</label><input id="a-adapter" value="claude">
    <label>image</label><input id="a-image" value="node:22-bookworm">
    <label>instructions 文件(绝对路径)</label><input id="a-instr" value="/instructions/agent.md">
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createAgent()">创建</button><button class="btn" onclick="view='agents';render()">取消</button></div>
  </div>`;
}
async function createAgent() {
  try { await api("POST", "/v1/agents", { display_name: $("a-name").value.trim(), adapter_id: $("a-adapter").value.trim(), image: $("a-image").value.trim(), instructions_file: $("a-instr").value.trim(), request_id: rid() }); toast("Agent 已创建"); } catch (e) { toast(e.message, true); }
  view = "agents"; refreshAll();
}
function newTaskForm() {
  const agentOpts = agents.filter(a => a.status === "active").map(a => `<option value="agent:${a.id}">${esc(a.display_name)}</option>`).join("");
  const humanOpts = participants.filter(p => p.kind === "human").map(p => `<option value="human:${p.id}">${esc(p.display_name)} (human)</option>`).join("");
  $("main").innerHTML = `
  <h1>创建任务</h1>
  <div class="form-grid">
    <label>标题</label><input id="t-title" placeholder="任务标题">
    <label>项目</label><select id="t-proj">${projects.map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join("")}</select>
    <label>Assignee</label><select id="t-assignee">${agentOpts + humanOpts}</select>
    <label>Priority</label><input id="t-prio" type="number" value="0">
    <label>描述</label><textarea id="t-desc" rows="2"></textarea>
    <div style="grid-column:1/-1;margin-top:8px"><button class="btn primary" onclick="createTask()">创建</button><button class="btn" onclick="view='tasks';render()">取消</button></div>
  </div>`;
}
async function createTask() {
  const sel = $("t-assignee").value;
  const body = { project_id: $("t-proj").value, title: $("t-title").value.trim(), description: $("t-desc").value.trim(), priority: +$("t-prio").value || 0, request_id: rid() };
  if (sel.startsWith("agent:")) body.assignee_agent_id = sel.slice(6);
  else body.assignee_participant_id = sel.slice(6);
  try { await api("POST", "/v1/tasks", body); toast("任务已创建"); } catch (e) { toast(e.message, true); }
  view = "tasks"; refreshAll();
}
async function ackMsg(id) { try { await api("POST", "/v1/messages/" + encodeURIComponent(id) + "/ack", { request_id: rid() }); toast("已 ack"); } catch (e) { toast(e.message, true); } refreshMessages(); render(); }
async function sendChat() {
  try { await api("POST", "/v1/chat", { project_id: $("chat-proj").value, agent_id: $("chat-agent").value, body: $("chat-body").value.trim(), request_id: rid() }); toast("已发送"); $("chat-body").value = ""; } catch (e) { toast(e.message, true); }
}
async function revokeCred() { try { await api("POST", "/v1/credentials/" + "participant-owner" + "/revoke", { request_id: rid() }); } catch (e) { /* fence 已拒 */ } sessionStorage.removeItem(KEY); showLogin("凭据已吊销,请轮换后重新登录"); }

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
    await Promise.all([refreshMessages(), refreshRoles(), refreshEvents(), refreshCreds()]);
  } catch (e) { if (e.code === "SCOPE_DENIED") { sessionStorage.removeItem(KEY); showLogin("凭据已失效"); } else { toast(e.message, true); } }
  render();
}
$("nav").addEventListener("click", e => { const a = e.target.closest("a"); if (!a) return; view = a.dataset.v; render(); });
boot();
setInterval(() => { if (view !== "login") refreshAll(); }, 5000);
