// chat.js — direct-chat view logic.
// Depends on the shared globals declared by control.js (loaded first):
// $, t, post, token, headers. Static labels use data-i18n like the console.

(function () {
  let tools = [];
  let state = null;
  let initialized = false;
  let sse = null;
  let busy = false;
  let activeBody = null;   // assistant body element of the streaming turn
  let activeWaiting = null; // three-dot indicator of the streaming turn
  // raw markdown of the streaming turn; rendered into activeBody (throttled)
  let activeRaw = "";
  let renderTimer = null;
  // started_at of the run bound to the streaming turn; only the result with
  // this exact key may finish the turn.
  let currentRunStartedAt = null;
  // template of the empty-state hero, kept so a fresh conversation can
  // restore it after the transcript is cleared
  let heroTemplate = null;
  // last workdir auto-filled from a picked session, so 新会话 clears the
  // auto-fill without touching a manually typed path
  let autoFilledWorkdir = null;

  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function transcript() { return $("chat-transcript"); }

  function hideHero() {
    const hero = $("chat-hero");
    if (hero) hero.remove();
  }

  function nearBottom() {
    const t = transcript();
    return t.scrollHeight - t.scrollTop - t.clientHeight < 80;
  }

  function scrollToEnd(force) {
    const t = transcript();
    if (force || nearBottom()) t.scrollTop = t.scrollHeight;
  }

  function addUserMessage(text) {
    hideHero();
    transcript().appendChild(el("div", "msg-user", text));
    scrollToEnd(true);
  }

  // Resolve a still-open turn before a new one replaces it: paint its result
  // when it already arrived, otherwise just drop the waiting indicator.
  function resolveStaleTurn() {
    if (!activeBody) {
      currentRunStartedAt = null;
      return;
    }
    const result = state && state.last_result;
    if (currentRunStartedAt !== null && result && result.started_at === currentRunStartedAt) {
      currentRunStartedAt = null;
      finishTurn(result);
      return;
    }
    if (activeWaiting) {
      activeWaiting.remove();
      activeWaiting = null;
    }
    currentRunStartedAt = null;
  }

  function startAssistantTurn() {
    hideHero();
    resolveStaleTurn();
    const wrap = el("div", "msg-assistant");
    activeBody = el("div", "msg-assistant-body");
    activeRaw = "";
    activeWaiting = el("div", "msg-waiting");
    activeWaiting.append(el("span"), el("span"), el("span"));
    wrap.append(activeBody, activeWaiting);
    transcript().appendChild(wrap);
    scrollToEnd(true);
  }

  // ---------- markdown rendering ----------

  // Assistant replies are markdown; rendering uses the locally vendored
  // marked + DOMPurify and falls back to plain text when either is missing.
  function markdownAvailable() {
    return typeof marked !== "undefined" && typeof DOMPurify !== "undefined";
  }

  function renderMarkdown(elm, text) {
    if (!markdownAvailable()) {
      elm.textContent = text;
      elm.classList.add("plain");
      return;
    }
    elm.classList.remove("plain");
    elm.innerHTML = DOMPurify.sanitize(marked.parse(text));
  }

  function scheduleActiveRender() {
    if (renderTimer) return;
    renderTimer = setTimeout(() => {
      renderTimer = null;
      if (activeBody) renderMarkdown(activeBody, activeRaw);
      scrollToEnd(false);
    }, 80);
  }

  function appendLine(line) {
    if (!activeBody) startAssistantTurn();
    if (activeWaiting) { activeWaiting.remove(); activeWaiting = null; }
    // Tools print leading blank lines before the actual reply; they would
    // render as vertical whitespace at the top of the bubble.
    if (line.trim() === "" && activeRaw.trim() === "") return;
    activeRaw += line + "\n";
    scheduleActiveRender();
  }

  function finishTurn(result) {
    if (activeWaiting) { activeWaiting.remove(); activeWaiting = null; }
    if (renderTimer) { clearTimeout(renderTimer); renderTimer = null; }
    const wrap = activeBody ? activeBody.parentElement : null;
    if (wrap && result) {
      renderMarkdown(activeBody, activeRaw.replace(/\s+$/, ""));
      const meta = el("div", "msg-meta");
      const statusWord =
        result.status === "completed" ? t("chat-meta-done") :
        result.status === "stopped" ? t("chat-meta-stopped") : t("chat-meta-failed");
      const statusCls =
        result.status === "completed" ? "meta-status-done" :
        result.status === "stopped" ? "meta-status-stopped" : "meta-status-failed";
      meta.appendChild(el("span", statusCls, "● " + statusWord));
      meta.appendChild(el("span", null, t("chat-exit-code") + " " + result.exit_code));
      if (result.total_tokens > 0) {
        meta.appendChild(el("span", null, result.input_tokens + " / " + result.output_tokens + " tok"));
      }
      meta.appendChild(el("span", null, (result.duration_ms / 1000).toFixed(1) + "s"));
      if (result.session_id) {
        meta.appendChild(el("span", null, t("chat-session-label") + " " + result.session_id.slice(0, 8)));
      }
      wrap.appendChild(meta);
      if (result.status === "failed" && result.error) {
        wrap.appendChild(el("div", "msg-error", result.error));
      }
      scrollToEnd(false);
    }
    activeBody = null;
    activeRaw = "";
  }

  function addNotice(text) {
    transcript().appendChild(el("div", "chat-notice", text));
    scrollToEnd(true);
  }

  function setBusy(v) {
    busy = v;
    $("chat-send").classList.toggle("busy", v);
    $("chat-send-label").textContent = v ? t("chat-stop") : t("chat-send");
    $("chat-provider").disabled = v;
    $("chat-session").disabled = v;
    $("chat-workdir").disabled = v;
    $("chat-new-session").disabled = v;
  }

  // ---------- tools ----------

  function selectedTool() { return tools.find((x) => x.name === $("chat-provider").value); }

  function renderToolStatus(kind, text) {
    const box = $("chat-tool-status");
    box.className = "chat-tool-status" + (kind ? " " + kind : "");
    box.textContent = text;
  }

  function renderSelectedToolStatus() {
    const tool = selectedTool();
    if (!tool) { renderToolStatus("", ""); return; }
    if (tool.installed) renderToolStatus("ok", "✓ " + t("chat-installed"));
    else renderToolStatus("missing", "✗ " + t("chat-missing"));
  }

  async function loadTools(checking) {
    if (checking) renderToolStatus("checking", t("chat-checking"));
    try {
      const res = await fetch("/api/local/chat/tools", { headers });
      if (!res.ok) return;
      const data = await res.json();
      tools = data.tools || [];
      const sel = $("chat-provider");
      const prev = localStorage.getItem("teammate_chat_provider") ||
        (state && state.session && state.session.provider) || "atomcode";
      sel.innerHTML = "";
      tools.forEach((tool) => {
        const opt = document.createElement("option");
        opt.value = tool.name;
        opt.textContent = (tool.installed ? "✓ " : "✗ ") + tool.name;
        sel.appendChild(opt);
      });
      if (tools.some((x) => x.name === prev)) sel.value = prev;
      renderSelectedToolStatus();
    } catch { /* keep last known tool list */ }
  }

  // ---------- tool sessions ----------

  // session id to resume on the next send (one-shot); continuation afterwards
  // follows the manager's session memory.
  let pendingResumeId = "";
  let sessionsCache = [];

  async function loadSessions() {
    const sel = $("chat-session");
    const provider = $("chat-provider").value;
    if (!provider) return;
    try {
      const res = await fetch("/api/local/chat/sessions?provider=" + encodeURIComponent(provider), { headers });
      if (!res.ok) return;
      const data = await res.json();
      sessionsCache = data.sessions || [];
      sel.innerHTML = "";
      const head = document.createElement("option");
      head.value = "";
      head.textContent = t("chat-new-session");
      sel.appendChild(head);
      if (!data.supported) {
        head.textContent = t("chat-sessions-unsupported");
        sel.disabled = true;
        pendingResumeId = "";
        return;
      }
      sel.disabled = false;
      sessionsCache.forEach((s) => {
        const opt = document.createElement("option");
        opt.value = s.id;
        const label = s.title && s.title.length > 24 ? s.title.slice(0, 24) + "…" : (s.title || s.id.slice(0, 8));
        const when = s.updated_at ? fmtAgo(s.updated_at) : "";
        const turns = s.turns > 0 ? " · " + s.turns + " " + t("chat-turns") : "";
        opt.textContent = label + (when ? " · " + when : "") + turns;
        opt.title = (s.title || "") + "\n" + (s.work_dir || "") + "\n" + s.id;
        sel.appendChild(opt);
      });
      sel.value = "";
      sel.title = "";
      pendingResumeId = "";
    } catch { /* keep last known list */ }
  }

  function restoreHero() {
    if (!heroTemplate) return;
    if (transcript().querySelector(".chat-hero")) return;
    transcript().appendChild(heroTemplate.cloneNode(true));
  }

  // Replace the transcript with a loaded session's history, ready to be
  // continued: messages render as read-only bubbles under a divider.
  async function loadSessionHistory(sessionId) {
    const provider = $("chat-provider").value;
    let messages = null;
    try {
      const res = await fetch("/api/local/chat/session?provider=" + encodeURIComponent(provider) +
        "&id=" + encodeURIComponent(sessionId), { headers });
      if (res.ok) {
        const data = await res.json();
        if (data.supported) messages = data.messages || [];
      }
    } catch { /* history stays unavailable; resume still works */ }

    const box = transcript();
    box.innerHTML = "";
    if (!messages || messages.length === 0) {
      addNotice(t("chat-history-empty"));
      restoreHero();
      return;
    }
    const divider = el("div", "chat-notice", t("chat-history-loaded") + " · " + messages.length);
    box.appendChild(divider);
    messages.forEach((m) => {
      if (m.role === "user") {
        const bubble = el("div", "msg-user msg-history", m.text);
        box.appendChild(bubble);
      } else {
        const wrap = el("div", "msg-assistant msg-history");
        const body = el("div", "msg-assistant-body");
        renderMarkdown(body, m.text.replace(/^\s+|\s+$/g, ""));
        wrap.appendChild(body);
        box.appendChild(wrap);
      }
    });
    scrollToEnd(true);
  }

  // ---------- state ----------

  async function refreshState() {
    try {
      const res = await fetch("/api/local/chat/state", { headers });
      if (!res.ok) return;
      state = await res.json();
      applyState();
    } catch { /* transient network error — next poll retries */ }
  }

  function applyState() {
    const active = state && state.active;
    if (!!active !== busy) setBusy(!!active);
    if (active && !activeBody) {
      startAssistantTurn();
      currentRunStartedAt = active.started_at;
    }

    const result = state && state.last_result;
    if (result && activeBody && currentRunStartedAt !== null && result.started_at === currentRunStartedAt) {
      currentRunStartedAt = null;
      finishTurn(result);
      loadSessions();
    }
  }

  // ---------- streaming ----------

  // Stream only lines emitted after page load: the transcript is not
  // replayed because prior turns lack their user prompts server-side.
  async function connectSSE() {
    let since = 0;
    try {
      const res = await fetch("/api/local/chat/logs/recent", { headers });
      if (res.ok) {
        const data = await res.json();
        (data.lines || []).forEach((l) => { if (l.seq > since) since = l.seq; });
      }
    } catch { /* fall back to full replay */ }
    sse = new EventSource("/api/local/chat/logs/stream?since=" + since + "&token=" + encodeURIComponent(token));
    sse.addEventListener("chat.output_line", (e) => {
      try { appendLine(JSON.parse(e.data).line); } catch { /* malformed event */ }
    });
  }

  // ---------- actions ----------

  async function send() {
    const input = $("chat-input");
    const text = input.value.trim();
    if (!text || busy) return;
    input.value = "";
    autosize();
    addUserMessage(text);
    const r = await post("/api/local/chat/send", {
      provider: $("chat-provider").value,
      prompt: text,
      workdir: $("chat-workdir").value.trim(),
      session_id: pendingResumeId,
    });
    if (!r.ok) {
      addNotice(t("chat-send-failed") + (r.text || r.status));
      return;
    }
    // The picked session resumes exactly this run; later turns continue via
    // the manager's session memory.
    pendingResumeId = "";
    $("chat-session").value = "";
    setBusy(true);
    startAssistantTurn();
    if (r.json && r.json.active) currentRunStartedAt = r.json.active.started_at;
    refreshState();
  }

  async function stop() {
    await post("/api/local/chat/stop");
    refreshState();
  }

  // Fresh conversation: drop the resume memory, the picked session, the
  // session's auto-filled workdir, and the transcript itself.
  async function newSession() {
    await post("/api/local/chat/reset");
    await refreshState();
    pendingResumeId = "";
    $("chat-session").value = "";
    const workdir = $("chat-workdir");
    if (autoFilledWorkdir !== null && workdir.value === autoFilledWorkdir) {
      workdir.value = "";
    }
    autoFilledWorkdir = null;
    transcript().innerHTML = "";
    restoreHero();
    scrollToEnd(true);
  }

  // ---------- composer ----------

  function autosize() {
    const ta = $("chat-input");
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, 160) + "px";
  }

  function initOnce() {
    if (initialized) return;
    initialized = true;

    const hero = $("chat-hero");
    if (hero) heroTemplate = hero.cloneNode(true);
    if (markdownAvailable()) marked.setOptions({ gfm: true, breaks: false });

    $("chat-send").onclick = () => (busy ? stop() : send());
    $("chat-check").onclick = () => loadTools(true);
    $("chat-new-session").onclick = newSession;
    $("chat-provider").onchange = () => {
      localStorage.setItem("teammate_chat_provider", $("chat-provider").value);
      renderSelectedToolStatus();
      loadSessions();
    };
    $("chat-session").onchange = () => {
      const sel = $("chat-session");
      const picked = sessionsCache.find((x) => x.id === sel.value);
      if (!picked) {
        // The 新会话 entry in the dropdown behaves like the button.
        newSession();
        return;
      }
      pendingResumeId = picked.id;
      sel.title = (picked.title || picked.id) + "\n" + (picked.work_dir || "") + "\n" + picked.id;
      // Run in the session's own directory so resume lands in the right cwd.
      autoFilledWorkdir = picked.work_dir || null;
      if (picked.work_dir) $("chat-workdir").value = picked.work_dir;
      loadSessionHistory(picked.id);
    };

    const ta = $("chat-input");
    ta.addEventListener("input", autosize);
    ta.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" || e.shiftKey || e.isComposing || e.repeat) return;
      e.preventDefault();
      send();
    });

    connectSSE();
    // Sessions are listed for the selected provider, so they must load only
    // after the provider dropdown has been populated.
    loadTools().then(() => loadSessions());
    refreshState();
    setInterval(() => { if (!document.hidden) refreshState(); }, 2000);
  }

  // Hook used by control.js for view switching and language changes.
  window.__chatView = {
    onShow() {
      initOnce();
      scrollToEnd(true);
    },
    onLangChange() {
      if (busy) $("chat-send-label").textContent = t("chat-stop");
      renderSelectedToolStatus();
    },
  };

  // Saved view may already be "chat" before this script loads.
  if (!$("view-chat").hidden) initOnce();
})();
