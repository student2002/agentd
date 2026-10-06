// control.js — frontend logic for the agentd local control console.
//
// One daemon serves multiple workspace connections, each materializing its
// own instances. An agent instance is addressed by (connection, name): the
// topbar connection selector scopes the console and the agent selector picks
// the instance every per-instance endpoint (snapshot / control / logs)
// targets. The instances card groups the rows by connection — instances are
// created in the workspace web UI and materialized here on delivery.

// ---------- i18n ----------

const i18n = {
  en: {
    "title": "Teammate Agent Console",
    "subtitle": "Local console — operations affect only this agentd instance; the server stays unaware (node remains in_progress).",
    "runtime": "Runtime",
    "agent": "Agent",
    "execution": "Execution",
    "tool": "Tool",
    "current-node": "Current Node",
    "no-node": "No node running",
    "task": "Task",
    "node": "Node",
    "node-id": "Node ID",
    "node-actions": "Node actions",
    "tool-label": "Tool",
    "started": "Started",
    "workdir": "Work dir",
    "claiming": "Claiming",
    "pause": "Pause",
    "resume": "Resume",
    "paused": "Claiming paused",
    "claiming-on": "Claiming active",
    "soft-interrupt": "Take over",
    "handback": "Hand back",
    "complete": "Complete",
    "intervene": "Send",
    "input-placeholder": "Message to append to the session, Enter to send…",
    "intervene-hint": "Sends one message into the current session and runs a single turn.",
    "logs": "Live Logs",
    "follow": "Follow",
    "clear": "Clear",
    "live": "Live",
    "reconnecting": "Reconnecting…",
    "connecting": "Connecting…",
    "waiting-logs": "Waiting for logs…",
    "connection-failed": "Connection failed: ",
    "failed-to-get-snapshot": "Snapshot error: ",
    "execution-failed": "Execution failed: ",
    "gate-title": "Local token required",
    "gate-desc": "Enter local.local_token from ~/.teammate/config.yaml, or open the page with ?token=…",
    "token-placeholder": "lt_local_…",
    "gate-save": "Unlock",
    "gate-invalid": "Token rejected by agentd, please check and retry.",
    "heartbeat": "Heartbeat",
    "sse-on": "SSE connected",
    "sse-off": "SSE disconnected",
    "never": "never",
    "idle": "idle",
    "ago-now": "just now",
    "ago-s": "{n}s ago",
    "ago-m": "{n}m ago",
    "ago-h": "{n}h ago",
    "view-console": "Console",
    "view-chat": "Direct Chat",
    "instances": "Instances",
    "agent-source-hint": "One row per (connection, instance). Instances are created in the workspace web UI and materialized here on delivery.",
    "server-card": "Server & Workspaces",
    "server-url": "Server URL",
    "server-url-ph": "http://127.0.0.1:8080",
    "connection-add-label": "Add workspace connection",
    "connection-name-ph": "alias (e.g. team-a)",
    "connection-token-ph": "td_… — daemon token from that workspace's web UI",
    "connection-add": "Add",
    "connection-hint": "Each connection registers with its own token; instances created in that workspace's web UI are materialized here on delivery.",
    "connection-remove": "Remove",
    "no-connections": "No workspace connections configured. Paste a td_ token above to connect this machine to a workspace.",
    "confirm-remove-connection": "Remove workspace connection",
    "conn-registered": "registered",
    "conn-unregistered": "not registered",
    "conn-sse-on": "SSE",
    "conn-agents-count": "{n} instance(s)",
    "conn-workspace-pending": "pending registration",
    "connection-saved": "Connection added — registering…",
    "workspace-all": "All workspaces",
    "workspace-focus": "Scope the console to this workspace",
    "ws-no-node": "No node running in this workspace",
    "ws-conn-offline": "workspace offline",
    "ws-conn-registering": "registering…",
    "ws-not-materialized": "no instances delivered",
    "no-agents": "No instances materialized yet — create them in a workspace web UI; they appear here once delivered.",
    "select-agent-first": "Select an agent instance first.",
    "agent-op-failed": "Operation failed: ",
    "status-pending": "pending",
    "status-online": "online",
    "status-busy": "busy",
    "status-paused": "paused",
    "identity-tooltip": "Identity: ",
    "persona-tooltip": "Memory key (shared across workspaces on this machine): ",
    "persona-missing": "no persona",
    "memory": "Memory",
    "memory-title": "Instance memory",
    "memory-sub": "One unified memory per instance, shared by every workspace this instance works in. Edits apply immediately.",
    "memory-save": "Save",
    "memory-clear": "Clear",
    "memory-close": "Close",
    "memory-confirm-clear": "Delete the entire memory of this instance",
    "memory-saved": "Saved.",
    "memory-empty": "(empty)",
    "chat-tool": "Tool",
    "chat-check": "Check",
    "chat-checking": "Checking…",
    "chat-installed": "installed",
    "chat-missing": "not installed",
    "chat-workdir": "Work dir",
    "chat-workdir-ph": "optional — defaults to a local scratch dir",
    "chat-new-session": "New session",
    "chat-session-reset": "New session started",
    "chat-hero-title": "Chat directly with a local coding tool",
    "chat-hero-sub": "Runs entirely on this machine — no task node, no server involved.",
    "chat-input-ph": "Message… (Enter to send, Shift+Enter for newline)",
    "chat-send": "Send",
    "chat-stop": "Stop",
    "chat-send-failed": "Send failed: ",
    "chat-meta-done": "done",
    "chat-meta-stopped": "stopped",
    "chat-meta-failed": "failed",
    "chat-exit-code": "exit code",
    "chat-session-label": "session",
    "chat-session": "Session",
    "chat-turns": "turns",
    "chat-sessions-unsupported": "Session list not supported for this tool",
    "chat-history-loaded": "Loaded session history",
    "chat-history-empty": "No readable history for this session",
  },
  zh: {
    "title": "Teammate 代理控制台",
    "subtitle": "本地控制台 — 操作仅作用于本机 agentd，服务器保持无感（节点保持 in_progress）。",
    "runtime": "运行时",
    "agent": "代理",
    "execution": "执行",
    "tool": "工具",
    "current-node": "当前节点",
    "no-node": "无运行中的节点",
    "task": "任务",
    "node": "节点",
    "node-id": "节点 ID",
    "node-actions": "节点操作",
    "tool-label": "工具",
    "started": "开始时间",
    "workdir": "工作目录",
    "claiming": "认领控制",
    "pause": "暂停",
    "resume": "恢复",
    "paused": "已暂停认领",
    "claiming-on": "认领进行中",
    "soft-interrupt": "接管",
    "handback": "交还",
    "complete": "人工完成",
    "intervene": "发送",
    "input-placeholder": "向当前会话追加一条消息，回车发送…",
    "intervene-hint": "向当前会话发送一条消息并执行一轮。",
    "logs": "实时日志",
    "follow": "跟随",
    "clear": "清空",
    "live": "已连接",
    "reconnecting": "重连中…",
    "connecting": "连接中…",
    "waiting-logs": "等待日志…",
    "connection-failed": "连接失败：",
    "failed-to-get-snapshot": "快照获取失败：",
    "execution-failed": "执行失败：",
    "gate-title": "需要本地令牌",
    "gate-desc": "请输入 ~/.teammate/config.yaml 中的 local.local_token，或在地址栏使用 ?token=… 打开。",
    "token-placeholder": "lt_local_…",
    "gate-save": "解锁",
    "gate-invalid": "令牌校验未通过，请检查后重试。",
    "heartbeat": "心跳",
    "sse-on": "SSE 已连接",
    "sse-off": "SSE 未连接",
    "never": "从未",
    "idle": "空闲",
    "ago-now": "刚刚",
    "ago-s": "{n} 秒前",
    "ago-m": "{n} 分钟前",
    "ago-h": "{n} 小时前",
    "view-console": "控制台",
    "view-chat": "直接对话",
    "instances": "实例",
    "agent-source-hint": "每行对应一个（连接, 实例）。实例由空间 web UI 创建，下发后自动物化到对应连接。",
    "server-card": "服务与空间",
    "server-url": "服务地址",
    "server-url-ph": "http://127.0.0.1:8080",
    "connection-add-label": "添加空间连接",
    "connection-name-ph": "别名（如 team-a）",
    "connection-token-ph": "td_… — 该空间 web UI 签发的守护令牌",
    "connection-add": "添加",
    "connection-hint": "每条连接使用各自的令牌注册；该空间 web UI 创建的实例经下发自动物化到本机。",
    "connection-remove": "删除",
    "no-connections": "尚未配置任何空间连接。粘贴 td_ 令牌即可把本机接入一个空间。",
    "confirm-remove-connection": "删除空间连接",
    "conn-registered": "已注册",
    "conn-unregistered": "未注册",
    "conn-sse-on": "SSE",
    "conn-agents-count": "{n} 个实例",
    "conn-workspace-pending": "待注册",
    "connection-saved": "连接已添加 — 正在注册…",
    "workspace-all": "全部空间",
    "workspace-focus": "将控制台聚焦到该空间",
    "ws-no-node": "该空间无运行中的节点",
    "ws-conn-offline": "空间离线",
    "ws-conn-registering": "注册中…",
    "ws-not-materialized": "尚无下发实例",
    "no-agents": "尚未物化任何实例 — 在空间 web UI 创建，下发后显示在此。",
    "select-agent-first": "请先选择一个代理实例。",
    "agent-op-failed": "操作失败：",
    "status-pending": "待绑定",
    "status-online": "在线",
    "status-busy": "忙碌",
    "status-paused": "已暂停",
    "identity-tooltip": "身份：",
    "persona-tooltip": "记忆标识（本机上跨空间共用）：",
    "persona-missing": "无记忆标识",
    "memory": "记忆",
    "memory-title": "实例记忆",
    "memory-sub": "每个实例一份统一记忆，在该实例工作的所有空间间共享。编辑立即生效。",
    "memory-save": "保存",
    "memory-clear": "清空",
    "memory-close": "关闭",
    "memory-confirm-clear": "删除该实例的全部记忆",
    "memory-saved": "已保存。",
    "memory-empty": "（空）",
    "chat-tool": "工具",
    "chat-check": "检测",
    "chat-checking": "检测中…",
    "chat-installed": "已安装",
    "chat-missing": "未安装",
    "chat-workdir": "工作目录",
    "chat-workdir-ph": "可选 — 留空使用本地临时目录",
    "chat-new-session": "新会话",
    "chat-session-reset": "已开启新会话",
    "chat-hero-title": "直接与本地编码工具对话",
    "chat-hero-sub": "全程在本机执行 — 不经过任务节点，服务器无感。",
    "chat-input-ph": "输入消息…（Enter 发送，Shift+Enter 换行）",
    "chat-send": "发送",
    "chat-stop": "停止",
    "chat-send-failed": "发送失败：",
    "chat-meta-done": "完成",
    "chat-meta-stopped": "已停止",
    "chat-meta-failed": "失败",
    "chat-exit-code": "退出码",
    "chat-session-label": "会话",
    "chat-session": "会话",
    "chat-turns": "轮",
    "chat-sessions-unsupported": "该工具暂不支持会话列表",
    "chat-history-loaded": "已加载会话历史",
    "chat-history-empty": "该会话没有可读的历史记录",
  },
};

let currentLang = localStorage.getItem("teammate_lang") || "en";
let lastSnapshot = null;
let logsLineCount = 0;
const LOGS_MAX_LINES = 3000;

// Agent instance selection: an instance is addressed by (connection, name).
// Every per-instance endpoint is rooted at
// /api/local/connections/{conn}/agents/{name}. Switching re-targets the
// snapshot poll and the log stream.
let agentsList = [];
let currentAgentConn = "";
let currentAgentName = "";
let logsES = null;

// Workspace scope: "" shows the aggregated (all-connections) view; a
// connection name scopes the console cards and the agent selector to that
// workspace.
let currentWorkspace = "";

function t(key) { return (i18n[currentLang] && i18n[currentLang][key]) || key; }

function fmtAgo(iso) {
  if (!iso) return t("never");
  const at = new Date(iso);
  // Zero-value times (e.g. "0001-01-01T00:00:00Z") mean "never happened".
  if (!at.getTime() || at.getFullYear() < 2000) return t("never");
  const ms = Date.now() - at.getTime();
  if (ms < 0 || isNaN(ms)) return t("never");
  const s = Math.floor(ms / 1000);
  if (s < 5) return t("ago-now");
  if (s < 60) return t("ago-s").replace("{n}", s);
  const m = Math.floor(s / 60);
  if (m < 60) return t("ago-m").replace("{n}", m);
  return t("ago-h").replace("{n}", Math.floor(m / 60));
}

function applyLang() {
  document.getElementById("lang-switch").textContent = currentLang === "en" ? "中文" : "English";
  document.documentElement.lang = currentLang === "en" ? "en" : "zh-CN";
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    const key = el.getAttribute("data-i18n");
    const val = i18n[currentLang][key];
    if (val) el.textContent = val;
  });
  document.querySelectorAll("[data-placeholder-i18n]").forEach((el) => {
    const key = el.getAttribute("data-placeholder-i18n");
    const val = i18n[currentLang][key];
    if (val) el.placeholder = val;
  });
  localStorage.setItem("teammate_lang", currentLang);
  renderAgentOptions();
  renderAgentsList();
  renderConnections();
  renderWorkspaceOptions();
  if (lastSnapshot) renderSnapshot(lastSnapshot);
  if (window.__chatView) window.__chatView.onLangChange();
}

// ---------- view switching ----------

function applyView(view) {
  $("view-console-btn").classList.toggle("active", view === "console");
  $("view-chat-btn").classList.toggle("active", view === "chat");
  $("view-console").hidden = view === "chat";
  $("view-chat").hidden = view !== "chat";
  localStorage.setItem("teammate_view", view);
  if (view === "chat" && window.__chatView) window.__chatView.onShow();
}

// ---------- token ----------

let token = new URLSearchParams(location.search).get("token") || localStorage.getItem("teammate_local_token") || "";
if (token) localStorage.setItem("teammate_local_token", token);
let headers = buildHeaders();

function buildHeaders() {
  return { "Authorization": "Bearer " + token, "Content-Type": "application/json" };
}

function showGate() {
  document.getElementById("gate").hidden = false;
  document.getElementById("gate-token").focus();
}

async function gateSave() {
  const input = document.getElementById("gate-token");
  const err = document.getElementById("gate-error");
  const val = input.value.trim();
  if (!val) return;
  token = val;
  headers = buildHeaders();
  try {
    const res = await fetch("/api/local/agents", { headers });
    if (!res.ok) { err.hidden = false; err.textContent = t("gate-invalid"); return; }
    localStorage.setItem("teammate_local_token", token);
    document.getElementById("gate").hidden = true;
    refreshAgents();
    refreshConnections();
  } catch {
    err.hidden = false;
    err.textContent = t("gate-invalid");
  }
}

// ---------- api ----------

const $ = (id) => document.getElementById(id);

async function post(path, body) {
  const res = await fetch(path, { method: "POST", headers, body: body ? JSON.stringify(body) : "{}" });
  const text = await res.text();
  try { return { ok: res.ok, status: res.status, json: JSON.parse(text) }; }
  catch { return { ok: res.ok, status: res.status, text }; }
}

async function del(path) {
  const res = await fetch(path, { method: "DELETE", headers });
  const text = await res.text();
  try { return { ok: res.ok, status: res.status, json: JSON.parse(text) }; }
  catch { return { ok: res.ok, status: res.status, text }; }
}

async function put(path, body) {
  const res = await fetch(path, { method: "PUT", headers, body: body ? JSON.stringify(body) : "{}" });
  const text = await res.text();
  try { return { ok: res.ok, status: res.status, json: JSON.parse(text) }; }
  catch { return { ok: res.ok, status: res.status, text }; }
}

function agentBase() {
  return "/api/local/connections/" + encodeURIComponent(currentAgentConn) +
    "/agents/" + encodeURIComponent(currentAgentName);
}

function currentTask() { return lastSnapshot && lastSnapshot.execution_session && lastSnapshot.execution_session.task_id; }
function currentNode() { return lastSnapshot && lastSnapshot.execution_session && lastSnapshot.execution_session.node_id; }

// ---------- rendering ----------

function setDot(id, cls) {
  const el = $(id);
  el.className = "dot" + (cls ? " " + cls : "");
}

function renderSnapshot(snap) {
  const runtime = snap.runtime || {};
  const agent = snap.agent || {};
  const es = snap.execution_session || {};
  const tool = snap.tool || {};
  const cfg = snap.config || {};
  const wsConn = currentWorkspace ? connections.find((c) => c.name === currentWorkspace) : null;
  const esInScope = !wsConn || !cfg.workspace_id || (es.workspace_id && es.workspace_id === wsConn.workspace_id);

  const identityLabel = [currentAgentConn, currentAgentName].filter(Boolean).join(" / ");
  $("brand-agent").textContent = identityLabel || "agentd";
  $("chip-instance").textContent = currentAgentName || "";
  $("chip-instance").title = cfg.agent_id
    ? t("identity-tooltip") + shortID(cfg.workspace_id) + "→" + shortID(cfg.agent_id)
    : (identityLabel || "");
  $("chip-provider").textContent = tool.provider || cfg.provider || "";

  // runtime card — a workspace is selected: reflect that connection's health
  // instead of the instance's own view
  if (wsConn) {
    $("val-runtime").textContent = wsConn.registered ? "online" : "offline";
    setDot("dot-runtime", wsConn.registered && wsConn.sse_connected ? "ok" : wsConn.registered ? "warn" : "err");
    $("sub-runtime").textContent = wsConn.registered
      ? (wsConn.sse_connected ? t("sse-on") : t("sse-off"))
      : t("ws-conn-registering");
  } else {
    $("val-runtime").textContent = runtime.status || "—";
    setDot("dot-runtime", runtime.status === "online" ? "ok" : (snap.last_error && snap.last_error.message ? "err" : "warn"));
    $("sub-runtime").textContent =
      (runtime.sse_connected ? t("sse-on") : t("sse-off")) + " · " +
      t("heartbeat") + " " + fmtAgo(runtime.last_heartbeat_at);
  }

  // agent card
  $("val-agent").textContent = agent.status || "—";
  setDot("dot-agent", agent.paused ? "warn" : "ok");
  $("sub-agent").textContent = agent.paused ? t("paused") : t("claiming-on");

  // execution card — filtered to the selected workspace
  const running = esInScope && (es.status === "running" || es.status === "intervening");
  $("val-exec").textContent = esInScope ? (es.status || t("idle")) : t("idle");
  setDot("dot-exec", running && es.status === "running" ? "busy" : running && es.status === "intervening" ? "warn" : "");
  $("sub-exec").textContent = running && es.tool ? es.tool : t("idle");

  // tool card
  $("val-tool").textContent = tool.provider || cfg.provider || "—";
  setDot("dot-tool", tool.last_error ? "err" : tool.status ? "ok" : "");
  $("sub-tool").textContent = tool.last_error || tool.status || "—";

  // node card — filtered to the selected workspace
  const showNode = esInScope && !!es.task_id;
  const pill = $("exec-pill");
  pill.textContent = showNode ? (es.status || t("idle")) : t("idle");
  pill.className = "pill" + (showNode && es.status === "running" ? " running" : showNode && es.status === "intervening" ? " intervening" : "");
  $("node-empty").hidden = showNode;
  if (wsConn && !showNode) {
    $("node-empty").textContent = t("ws-no-node");
  } else {
    $("node-empty").textContent = t("no-node");
  }
  $("node-details").hidden = !showNode;
  if (showNode) {
    $("d-task").textContent = "#" + es.task_id;
    $("d-node").textContent = es.node_name || "—";
    $("d-node-id").textContent = es.node_id || "—";
    $("d-node-id").title = es.node_id || "";
    $("d-tool").textContent = es.tool || "—";
    $("d-started").textContent = fmtAgo(es.started_at);
    $("d-workdir").textContent = es.workdir || "—";
    $("d-workdir").title = es.workdir || "";
  }

  // controls — intervention targets the running node regardless of scope;
  // disable the node actions while the scope hides the running node
  const allowIntervene = running && es.tool_session_id_present;
  $("soft-interrupt").disabled = !running;
  $("intervene").disabled = !allowIntervene;
  $("handback").disabled = !(esInScope && es.status === "intervening");
  $("complete").disabled = !running;
  $("pause").disabled = !!agent.paused;
  $("resume").disabled = !agent.paused;

  setDot("dot-health", agent.paused ? "warn" : "ok");
}

async function refreshSnapshot() {
  if (!currentAgentName) {
    $("sub-runtime").textContent = t("select-agent-first");
    return;
  }
  try {
    const res = await fetch(agentBase() + "/runtime/snapshot", { headers });
    if (res.status === 401) { showGate(); return; }
    if (res.status === 404) { refreshAgents(); return; }
    if (!res.ok) { $("sub-runtime").textContent = t("failed-to-get-snapshot") + res.status; return; }
    const snap = await res.json();
    lastSnapshot = snap;
    renderSnapshot(snap);
  } catch (e) {
    setDot("dot-health", "err");
    $("sub-runtime").textContent = t("connection-failed") + e;
  }
}

// ---------- agent instances ----------

function statusLabel(info) {
  return t("status-" + (info.status || "pending"));
}

function agentKey(conn, name) { return conn + "/" + name; }

function renderAgentOptions() {
  const select = $("agent-select");
  if (!select) return;
  const prevConn = currentAgentConn;
  const prevName = currentAgentName;
  const inScope = currentWorkspace
    ? agentsList.filter((a) => a.connection === currentWorkspace)
    : agentsList;
  select.innerHTML = "";
  for (const info of inScope) {
    const opt = document.createElement("option");
    opt.value = agentKey(info.connection, info.name);
    opt.textContent = currentWorkspace
      ? info.name + " · " + statusLabel(info)
      : info.name + " @ " + info.connection + " · " + statusLabel(info);
    select.appendChild(opt);
  }
  // Keep the selection when it still exists and stays in scope; otherwise
  // fall back to the first scoped instance.
  const keys = inScope.map((a) => agentKey(a.connection, a.name));
  const preferred = keys.find((k) => k === agentKey(prevConn, prevName)) || keys[0] || "";
  const [conn, name] = splitAgentKey(preferred);
  if (conn !== currentAgentConn || name !== currentAgentName) {
    switchAgent(conn, name, false);
  } else {
    select.value = preferred;
  }
}

function splitAgentKey(key) {
  if (!key) return ["", ""];
  const idx = key.indexOf("/");
  if (idx < 0) return ["", key];
  return [key.slice(0, idx), key.slice(idx + 1)];
}

function renderAgentsList() {
  const list = $("agents-list");
  if (!list) return;
  $("agents-count").textContent = String(agentsList.length);
  list.innerHTML = "";
  if (!agentsList.length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = t("no-agents");
    list.appendChild(empty);
    return;
  }
  // Group rows by connection, in the connections list's order (unknown
  // connections append alphabetically).
  const order = connections.map((c) => c.name);
  const groups = new Map(agentsList.map((a) => [a.connection, []]));
  for (const name of [...groups.keys()]) {
    if (!order.includes(name)) order.push(name);
  }
  for (const info of agentsList) {
    groups.get(info.connection).push(info);
  }
  const wsConn = currentWorkspace ? connections.find((c) => c.name === currentWorkspace) : null;
  for (const connName of order) {
    const rows = groups.get(connName) || [];
    if (!rows.length) continue;
    const header = document.createElement("div");
    header.className = "agent-group-head";
    const headName = document.createElement("span");
    headName.className = "agent-group-name";
    headName.textContent = connName;
    const conn = connections.find((c) => c.name === connName);
    const headState = document.createElement("span");
    headState.className = "pill pill-sm" + (conn && conn.registered ? " running" : " attention");
    headState.textContent = conn ? (conn.registered ? t("conn-registered") : t("conn-unregistered")) : "";
    const headCount = document.createElement("span");
    headCount.className = "agent-group-count";
    headCount.textContent = t("conn-agents-count").replace("{n}", rows.length);
    header.append(headName, headState, headCount);
    list.appendChild(header);

    for (const info of rows) {
      const selected = info.connection === currentAgentConn && info.name === currentAgentName;
      const row = document.createElement("div");
      row.className = "agent-row" + (selected ? " selected" : "");
      if (selected) row.style.borderColor = "var(--accent)";

      const name = document.createElement("span");
      name.className = "agent-row-name";
      name.textContent = info.name;
      const provider = document.createElement("span");
      provider.className = "agent-row-provider";
      provider.textContent = info.provider + " · " + statusLabel(info);
      if (info.agent_id) {
        provider.textContent += " · " + shortID(info.agent_id);
        provider.title = info.agent_id;
      }
      if (info.persona_key) {
        provider.textContent += " · mem:" + shortID(info.persona_key);
        provider.title += (provider.title ? " | " : "") + t("persona-tooltip") + info.persona_key;
      }
      const spacer = document.createElement("span");
      spacer.className = "agent-row-spacer";

      // A scoped console greys out rows of other connections.
      if (wsConn && wsConn.name !== info.connection) {
        row.classList.add("disabled");
        row.append(name, provider, spacer);
      } else {
        const memory = document.createElement("button");
        memory.className = "btn btn-ghost btn-sm";
        memory.textContent = t("memory");
        memory.title = t("memory-title");
        memory.onclick = (e) => { e.stopPropagation(); openMemory(info.persona_key || info.name); };
        row.append(name, provider, spacer, memory);
        row.onclick = () => switchAgent(info.connection, info.name);
        row.style.cursor = "pointer";
      }

      list.appendChild(row);
    }
  }
}

async function refreshAgents() {
  try {
    const url = currentWorkspace
      ? "/api/local/agents?connection=" + encodeURIComponent(currentWorkspace)
      : "/api/local/agents";
    const res = await fetch(url, { headers });
    if (res.status === 401) { showGate(); return; }
    if (!res.ok) return;
    const payload = await res.json();
    agentsList = payload.agents || [];
    renderAgentOptions();
    renderAgentsList();
  } catch {
    setDot("dot-health", "err");
  }
}

function switchAgent(conn, name, save) {
  currentAgentConn = conn;
  currentAgentName = name;
  if (save !== false) localStorage.setItem("teammate_agent", conn && name ? agentKey(conn, name) : "");
  const select = $("agent-select");
  if (select) select.value = conn && name ? agentKey(conn, name) : "";
  lastSnapshot = null;
  resetLogs();
  streamLogs();
  refreshSnapshot();
}

// Restore the persisted selection after the first agents fetch.
function restorePersistedAgent() {
  const saved = localStorage.getItem("teammate_agent") || "";
  if (!saved) return;
  const [conn, name] = splitAgentKey(saved);
  if (!conn || !name) return;
  const exists = agentsList.some((a) => a.connection === conn && a.name === name);
  if (exists && !currentAgentName) {
    switchAgent(conn, name, false);
  }
}

// ---------- workspace connections ----------

let connections = [];

function shortID(id) {
  return id && id.length > 8 ? id.slice(0, 8) : (id || "");
}

function renderConnections() {
  const pill = $("server-pill");
  const registered = connections.filter((c) => c.registered).length;
  if (pill) {
    pill.textContent = registered + "/" + connections.length;
    pill.className = "pill" + (registered > 0 ? " running" : connections.length ? " attention" : "");
  }
  const card = $("server-card");
  if (card) card.classList.toggle("attention", connections.length > 0 && registered === 0);
  const urlRow = $("server-url-row");
  if (urlRow) urlRow.textContent = connections.length ? (connections[0].server_url || "—") : "—";

  const list = $("connections-list");
  if (!list) return;
  list.innerHTML = "";
  if (!connections.length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = t("no-connections");
    list.appendChild(empty);
    return;
  }
  for (const conn of connections) {
    const row = document.createElement("div");
    row.className = "agent-row connection-row" + (conn.name === currentWorkspace ? " selected" : "");

    const name = document.createElement("span");
    name.className = "agent-row-name";
    name.textContent = conn.name;

    const state = document.createElement("span");
    state.className = "pill pill-sm" + (conn.registered ? " running" : " attention");
    state.textContent = conn.registered ? t("conn-registered") : t("conn-unregistered");

    const sse = document.createElement("span");
    sse.className = "pill pill-sm" + (conn.sse_connected ? " running" : "");
    sse.textContent = t("conn-sse-on");
    if (!conn.sse_connected) sse.style.opacity = "0.4";

    const workspace = document.createElement("span");
    workspace.className = "agent-row-provider mono";
    workspace.textContent = conn.workspace_id ? shortID(conn.workspace_id) : t("conn-workspace-pending");
    workspace.title = conn.workspace_id || "";

    const agents = document.createElement("span");
    agents.className = "agent-row-provider";
    agents.textContent = (conn.agents && conn.agents.length)
      ? t("conn-agents-count").replace("{n}", conn.agents.length)
      : t("ws-not-materialized");
    agents.title = (conn.agents || []).join(", ");

    const token = document.createElement("span");
    token.className = "agent-row-provider mono";
    token.textContent = conn.token_masked || "";
    token.title = conn.token_masked || "";

    const spacer = document.createElement("span");
    spacer.className = "agent-row-spacer";
    row.append(name, state, sse, workspace, token, agents, spacer);

    const focus = document.createElement("button");
    focus.className = "btn btn-sm";
    focus.textContent = currentWorkspace === conn.name ? "★" : "☆";
    focus.title = t("workspace-focus");
    focus.onclick = () => switchWorkspace(currentWorkspace === conn.name ? "" : conn.name);
    row.appendChild(focus);

    const remove = document.createElement("button");
    remove.className = "btn btn-warn btn-sm";
    remove.textContent = t("connection-remove");
    remove.onclick = () => {
      if (!confirm(t("confirm-remove-connection") + ": " + conn.name)) return;
      connectionOp(() => del("/api/local/connections/" + encodeURIComponent(conn.name)));
    };
    row.appendChild(remove);

    list.appendChild(row);
  }
}

// renderWorkspaceOptions fills the topbar workspace scope: "" aggregates all
// connections, one option per connection name otherwise.
function renderWorkspaceOptions() {
  const select = $("workspace-select");
  if (!select) return;
  select.innerHTML = "";
  const all = document.createElement("option");
  all.value = "";
  all.textContent = t("workspace-all");
  select.appendChild(all);
  for (const conn of connections) {
    const opt = document.createElement("option");
    opt.value = conn.name;
    const ws = conn.workspace_id ? shortID(conn.workspace_id) : t("conn-workspace-pending");
    opt.textContent = conn.name + " · " + ws + (conn.sse_connected ? "" : " · " + t("ws-conn-offline"));
    select.appendChild(opt);
  }
  select.value = connections.some((c) => c.name === currentWorkspace) ? currentWorkspace : "";
  if (select.value !== currentWorkspace) {
    currentWorkspace = select.value;
    if (lastSnapshot) renderSnapshot(lastSnapshot);
    renderAgentOptions();
    renderAgentsList();
  }
}

// switchWorkspace scopes the console view to one connection.
function switchWorkspace(name) {
  currentWorkspace = name;
  const select = $("workspace-select");
  if (select) select.value = name;
  renderConnections();
  renderAgentsList();
  refreshAgents();
  if (lastSnapshot) renderSnapshot(lastSnapshot);
}

async function connectionOp(op) {
  const r = await op();
  if (!r.ok) { alert(t("agent-op-failed") + (r.text || r.json?.error || r.status)); }
  await refreshConnections();
}

async function refreshConnections() {
  try {
    const res = await fetch("/api/local/connections", { headers });
    if (res.status === 401) { showGate(); return; }
    if (!res.ok) return;
    const payload = await res.json();
    connections = payload.connections || [];
    renderConnections();
    renderWorkspaceOptions();
  } catch { /* surfaced by the health dot */ }
}

async function addConnection() {
  const tokenVal = $("connection-add-token").value.trim();
  if (!tokenVal) return;
  const r = await post("/api/local/connections", {
    name: $("connection-add-name").value.trim(),
    token: tokenVal,
  });
  if (!r.ok) { alert(t("agent-op-failed") + (r.text || r.json?.error || r.status)); return; }
  connections = r.json.connections || connections;
  renderConnections();
  $("connection-add-name").value = "";
  $("connection-add-token").value = "";
  // Registration runs in the background; refresh the status shortly after.
  setTimeout(refreshConnections, 3000);
}

// ---------- instance memory ----------

let memoryInstance = "";

async function openMemory(instance) {
  memoryInstance = instance;
  const modal = $("memory-modal");
  const content = $("memory-content");
  const err = $("memory-error");
  err.hidden = true;
  $("memory-sub").textContent = t("memory-sub");
  modal.hidden = false;
  content.value = "…";
  content.disabled = true;
  try {
    const res = await fetch("/api/local/memory/" + encodeURIComponent(instance), { headers });
    if (res.status === 401) { showGate(); modal.hidden = true; return; }
    if (!res.ok) throw new Error("HTTP " + res.status);
    const payload = await res.json();
    content.value = payload.content || "";
    if (!content.value) content.value = "";
  } catch (e) {
    err.hidden = false;
    err.textContent = t("agent-op-failed") + e;
  }
  content.disabled = false;
  content.focus();
}

async function saveMemory() {
  if (!memoryInstance) return;
  const r = await put("/api/local/memory/" + encodeURIComponent(memoryInstance), { content: $("memory-content").value });
  const err = $("memory-error");
  if (!r.ok) { err.hidden = false; err.textContent = t("agent-op-failed") + (r.text || r.status); return; }
  err.hidden = false;
  err.textContent = t("memory-saved");
}

async function clearMemory() {
  if (!memoryInstance) return;
  if (!confirm(t("memory-confirm-clear") + ": " + memoryInstance)) return;
  const r = await del("/api/local/memory/" + encodeURIComponent(memoryInstance));
  if (!r.ok) {
    const err = $("memory-error");
    err.hidden = false;
    err.textContent = t("agent-op-failed") + (r.text || r.status);
    return;
  }
  $("memory-content").value = "";
}

function closeMemory() {
  $("memory-modal").hidden = true;
  memoryInstance = "";
}

$("memory-save").onclick = saveMemory;
$("memory-clear").onclick = clearMemory;
$("memory-close").onclick = closeMemory;

// ---------- actions ----------

async function sendIntervene() {
  const msg = $("message").value.trim();
  if (!msg || !currentAgentName) return;
  const r = await post(agentBase() + "/control/intervene", { task_id: currentTask(), node_id: currentNode(), message: msg });
  if (!r.ok) { alert(t("execution-failed") + (r.text || r.status)); }
  $("message").value = "";
  refreshSnapshot();
}

$("pause").onclick = async () => { if (currentAgentName) { await post(agentBase() + "/control/pause"); refreshSnapshot(); } };
$("resume").onclick = async () => { if (currentAgentName) { await post(agentBase() + "/control/resume"); refreshSnapshot(); } };
$("soft-interrupt").onclick = async () => { if (currentAgentName) { await post(agentBase() + "/control/soft-interrupt", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); } };
$("handback").onclick = async () => { if (currentAgentName) { await post(agentBase() + "/control/handback", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); } };
$("complete").onclick = async () => { if (currentAgentName) { await post(agentBase() + "/control/complete", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); } };
$("intervene").onclick = sendIntervene;
$("message").addEventListener("keydown", (e) => { if (e.key === "Enter" && !$("intervene").disabled) sendIntervene(); });
$("lang-switch").onclick = () => { currentLang = currentLang === "en" ? "zh" : "en"; applyLang(); };
$("gate-save").onclick = gateSave;
$("gate-token").addEventListener("keydown", (e) => { if (e.key === "Enter") gateSave(); });
$("view-console-btn").onclick = () => applyView("console");
$("view-chat-btn").onclick = () => applyView("chat");
$("agent-select").onchange = (e) => { const [conn, name] = splitAgentKey(e.target.value); switchAgent(conn, name); };
$("workspace-select").onchange = (e) => switchWorkspace(e.target.value);

// ---------- logs ----------

function appendLog(line) {
  const el = $("logs");
  if (logsLineCount === 0) el.textContent = "";
  el.textContent += line + "\n";
  logsLineCount++;
  if (logsLineCount > LOGS_MAX_LINES) {
    const drop = logsLineCount - Math.floor(LOGS_MAX_LINES / 2);
    el.textContent = el.textContent.split("\n").slice(drop).join("\n");
    logsLineCount = Math.floor(LOGS_MAX_LINES / 2);
  }
  if ($("follow").checked) el.scrollTop = el.scrollHeight;
}

function setStreamState(live) {
  setDot("dot-stream", live ? "ok" : "warn");
  $("stream-text").textContent = live ? t("live") : t("reconnecting");
}

function resetLogs() {
  if (logsES) { logsES.close(); logsES = null; }
  logsLineCount = 0;
  const el = $("logs");
  if (el) el.textContent = t("waiting-logs");
  setStreamState(false);
}

$("clear-logs").onclick = () => { $("logs").textContent = t("waiting-logs"); logsLineCount = 0; };

function streamLogs() {
  if (logsES) { logsES.close(); logsES = null; }
  if (!currentAgentName) return;
  // EventSource cannot set headers; the local token is passed via query.
  const es = new EventSource(agentBase() + "/logs/stream?since=0&token=" + encodeURIComponent(token));
  logsES = es;
  es.onopen = () => setStreamState(true);
  es.onerror = () => setStreamState(false);
  es.addEventListener("execution_session.output_line", (e) => {
    try { appendLog(JSON.parse(e.data).line); } catch {}
  });
}

// ---------- init ----------

applyLang();
applyView(localStorage.getItem("teammate_view") === "chat" ? "chat" : "console");

resetLogs();
refreshAgents().then(() => { restorePersistedAgent(); refreshSnapshot(); streamLogs(); });
refreshConnections();
setInterval(refreshSnapshot, 3000);
setInterval(refreshAgents, 10000);
setInterval(refreshConnections, 10000);
$("connection-add").onclick = addConnection;
$("connection-add-token").addEventListener("keydown", (e) => { if (e.key === "Enter") addConnection(); });
$("connection-add-name").addEventListener("keydown", (e) => { if (e.key === "Enter") addConnection(); });
