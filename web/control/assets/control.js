// control.js — frontend logic for the agentd local control console.

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
    "token-placeholder": "tm_local_…",
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
    "token-placeholder": "tm_local_…",
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
  },
};

let currentLang = localStorage.getItem("teammate_lang") || "en";
let lastSnapshot = null;
let logsLineCount = 0;
const LOGS_MAX_LINES = 3000;

function t(key) { return (i18n[currentLang] && i18n[currentLang][key]) || key; }

function fmtAgo(iso) {
  if (!iso) return t("never");
  const ms = Date.now() - new Date(iso).getTime();
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
  if (lastSnapshot) renderSnapshot(lastSnapshot);
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
    const res = await fetch("/api/local/runtime/snapshot", { headers });
    if (!res.ok) { err.hidden = false; err.textContent = t("gate-invalid"); return; }
    localStorage.setItem("teammate_local_token", token);
    document.getElementById("gate").hidden = true;
    refreshSnapshot();
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

  $("brand-agent").textContent = [cfg.agent_name, cfg.provider].filter(Boolean).join(" · ") || "agentd";
  if (cfg.profile) $("brand-agent").textContent += " (" + cfg.profile + ")";
  $("chip-instance").textContent = snap.instance_id ? snap.instance_id.slice(0, 8) : "";
  $("chip-instance").title = snap.instance_id || "";

  // runtime card
  $("val-runtime").textContent = runtime.status || "—";
  setDot("dot-runtime", runtime.status === "online" ? "ok" : (snap.last_error && snap.last_error.message ? "err" : "warn"));
  $("sub-runtime").textContent =
    (runtime.sse_connected ? t("sse-on") : t("sse-off")) + " · " +
    t("heartbeat") + " " + fmtAgo(runtime.last_heartbeat_at);

  // agent card
  $("val-agent").textContent = agent.status || "—";
  setDot("dot-agent", agent.paused ? "warn" : "ok");
  $("sub-agent").textContent = agent.paused ? t("paused") : t("claiming-on");

  // execution card
  const running = es.status === "running" || es.status === "intervening";
  $("val-exec").textContent = es.status || t("idle");
  setDot("dot-exec", es.status === "running" ? "busy" : es.status === "intervening" ? "warn" : "");
  $("sub-exec").textContent = es.tool ? es.tool : (running ? "—" : t("idle"));

  // tool card
  $("val-tool").textContent = tool.provider || cfg.provider || "—";
  setDot("dot-tool", tool.last_error ? "err" : tool.status ? "ok" : "");
  $("sub-tool").textContent = tool.last_error || tool.status || "—";

  // node card
  const pill = $("exec-pill");
  pill.textContent = es.status || t("idle");
  pill.className = "pill" + (es.status === "running" ? " running" : es.status === "intervening" ? " intervening" : "");
  $("node-empty").hidden = !!es.status && es.status !== "idle";
  $("node-details").hidden = !es.task_id;
  if (es.task_id) {
    $("d-task").textContent = "#" + es.task_id;
    $("d-node").textContent = es.node_name || "—";
    $("d-node-id").textContent = es.node_id || "—";
    $("d-node-id").title = es.node_id || "";
    $("d-tool").textContent = es.tool || "—";
    $("d-started").textContent = fmtAgo(es.started_at);
    $("d-workdir").textContent = es.workdir || "—";
    $("d-workdir").title = es.workdir || "";
  }

  // controls
  const allowIntervene = running && es.tool_session_id_present;
  $("soft-interrupt").disabled = !running;
  $("intervene").disabled = !allowIntervene;
  $("handback").disabled = es.status !== "intervening";
  $("complete").disabled = !running;
  $("pause").disabled = !!agent.paused;
  $("resume").disabled = !agent.paused;

  setDot("dot-health", agent.paused ? "warn" : "ok");
}

async function refreshSnapshot() {
  try {
    const res = await fetch("/api/local/runtime/snapshot", { headers });
    if (res.status === 401) { showGate(); return; }
    if (!res.ok) { $("sub-runtime").textContent = t("failed-to-get-snapshot") + res.status; return; }
    const snap = await res.json();
    lastSnapshot = snap;
    renderSnapshot(snap);
  } catch (e) {
    setDot("dot-health", "err");
    $("sub-runtime").textContent = t("connection-failed") + e;
  }
}

// ---------- actions ----------

async function sendIntervene() {
  const msg = $("message").value.trim();
  if (!msg) return;
  const r = await post("/api/local/control/intervene", { task_id: currentTask(), node_id: currentNode(), message: msg });
  if (!r.ok) { alert(t("execution-failed") + (r.text || r.status)); }
  $("message").value = "";
  refreshSnapshot();
}

$("pause").onclick = async () => { await post("/api/local/control/pause"); refreshSnapshot(); };
$("resume").onclick = async () => { await post("/api/local/control/resume"); refreshSnapshot(); };
$("soft-interrupt").onclick = async () => { await post("/api/local/control/soft-interrupt", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); };
$("handback").onclick = async () => { await post("/api/local/control/handback", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); };
$("complete").onclick = async () => { await post("/api/local/control/complete", { task_id: currentTask(), node_id: currentNode() }); refreshSnapshot(); };
$("intervene").onclick = sendIntervene;
$("message").addEventListener("keydown", (e) => { if (e.key === "Enter" && !$("intervene").disabled) sendIntervene(); });
$("lang-switch").onclick = () => { currentLang = currentLang === "en" ? "zh" : "en"; applyLang(); };
$("gate-save").onclick = gateSave;
$("gate-token").addEventListener("keydown", (e) => { if (e.key === "Enter") gateSave(); });

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

$("clear-logs").onclick = () => { $("logs").textContent = t("waiting-logs"); logsLineCount = 0; };

function streamLogs() {
  const es = new EventSource("/api/local/logs/stream?since=0");
  es.onopen = () => setStreamState(true);
  es.onerror = () => setStreamState(false);
  es.addEventListener("execution_session.output_line", (e) => {
    try { appendLog(JSON.parse(e.data).line); } catch {}
  });
}

// ---------- init ----------

applyLang();
$("logs").textContent = t("waiting-logs");
setStreamState(false);
refreshSnapshot();
streamLogs();
setInterval(refreshSnapshot, 3000);
