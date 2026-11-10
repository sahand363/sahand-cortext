const $ = id => document.getElementById(id);
let currentSession = null, toastTimer, deleteConfirmTimer;
let sessionsCache = [];

let selectedProvider = "gemini"; // currently viewed tab in Settings / Models
let activeProvider = "gemini";   // active provider used for Ordinary Chat
let activeModel = "gemini-2.0-flash";
let freeOnlyOpenRouter = true;
let deleteArmed = false;

// Orchestration State
let orchParticipantsDraft = [
  { role: "coordinator", name: "Coordinator / Architect", provider: "gemini", model: "gemini-2.0-flash" },
  { role: "worker", name: "Worker / Developer", provider: "gemini", model: "gemini-2.0-flash" }
];
let orchJobsCache = [];
let activeOrchJobId = localStorage.getItem("cortext_active_orch_job") || null;
let orchPollTimer = null;
let orchActiveSubView = "messages"; // "messages" or "events"
let orchSubmitting = false;

const providerCatalog = {
  gemini: {
    name: "Google Gemini",
    short: "GEMINI",
    icon: "✳",
    placeholder: "Paste your Gemini API key (AIzaSy...)",
    label: "Google AI Studio API Key",
    desc: "Save your Google AI Studio key to list all Gemini models and test which ones respond.",
    helpText: "Get a Gemini API key from Google AI Studio:",
    helpUrl: "https://aistudio.google.com/app/apikey",
    helpBtn: "Open Google AI Studio ↗"
  },
  openrouter: {
    name: "OpenRouter",
    short: "OPENROUTER",
    icon: "⇄",
    placeholder: "Paste your OpenRouter API key (sk-or-v1-...)",
    label: "OpenRouter API Key",
    desc: "Connect OpenRouter to access free (:free) and open-weight models. Free models are filtered at the top automatically.",
    helpText: "Get a free OpenRouter API key:",
    helpUrl: "https://openrouter.ai/keys",
    helpBtn: "Open OpenRouter Keys ↗"
  },
  codecraft: {
    name: "CodeCraft API",
    short: "CODECRAFT",
    icon: "⚡",
    placeholder: "Paste your CodeCraft API key (cc_...)",
    label: "CodeCraft API Key (https://codecraftapi.com/v1)",
    desc: "Connect CodeCraft API to test which of their hosted models respond on your plan.",
    helpText: "Get a CodeCraft API key:",
    helpUrl: "https://codecraftapi.com/docs/getting-started",
    helpBtn: "Open CodeCraft Docs ↗"
  }
};

let providerState = {
  gemini: { configured: false, models: [] },
  openrouter: { configured: false, models: [] },
  codecraft: { configured: false, models: [] }
};

const pageNames = {
  chat: "Chat",
  models: "Models & providers",
  orchestration: "AI orchestration",
  memory: "Topic memory",
  sessions: "Sessions",
  usage: "Usage & analytics",
  settings: "Settings"
};

async function api(url, opts = {}) {
  const r = await fetch(url, {
    ...opts,
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) }
  });
  let d = {};
  try { d = await r.json(); } catch {}
  if (r.status === 401 && url !== "/api/login") {
    stopOrchPolling();
    showLogin();
    throw new Error(d.error || "Session expired. Please sign in again.");
  }
  if (!r.ok) throw new Error(d.error || "Request failed");
  return d;
}

function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 3400);
}

function showLogin() {
  $("login-screen").classList.remove("hidden");
  $("app").classList.add("hidden");
}

function showApp() {
  $("login-screen").classList.add("hidden");
  $("app").classList.remove("hidden");
}

async function checkAuth() {
  try {
    const s = await api("/api/status");
    showApp();
    applyStatus(s);
    await loadSessions();
    await loadAllConfiguredModels();
    await loadOrchJobsList();
    if (!s.any_configured) {
      $("setup-banner").classList.remove("hidden");
      showSetupNudge();
    }
    const savedPage = localStorage.getItem("cortext_active_page");
    if (savedPage && pageNames[savedPage]) {
      switchPage(savedPage);
    }
  } catch {
    showLogin();
  }
}

function applyStatus(s) {
  if (s.providers) {
    providerState.gemini.configured = !!s.providers.gemini;
    providerState.openrouter.configured = !!s.providers.openrouter;
    providerState.codecraft.configured = !!s.providers.codecraft;
  }
  if (s.active_provider && s.active_model) {
    setActiveSelectionUI(s.active_provider, s.active_model);
  }
  updateProviderBadges();
  if (s.any_configured) {
    $("setup-banner").classList.add("hidden");
  }
}

function setActiveSelectionUI(provider, modelId) {
  if (!provider || !modelId) return;
  activeProvider = provider;
  activeModel = modelId;
  const info = providerCatalog[provider] || providerCatalog.gemini;
  if ($("active-provider-label")) $("active-provider-label").textContent = info.name;
  if ($("active-model-label")) $("active-model-label").textContent = modelId;
  if ($("header-provider-name")) $("header-provider-name").textContent = info.short;

  const sel = $("model");
  const composite = `${provider}::${modelId}`;
  if (sel) {
    if (![...sel.options].some(o => o.value === composite)) {
      const opt = document.createElement("option");
      opt.value = composite;
      opt.textContent = `✓ [${info.short}] ${modelId}`;
      sel.prepend(opt);
    }
    sel.value = composite;
  }
}

function updateProviderBadges() {
  const anyConn = providerState.gemini.configured || providerState.openrouter.configured || providerState.codecraft.configured;
  $("header-provider-state").textContent = anyConn ? "CONNECTED" : "SETUP";
  $("header-provider-state").classList.toggle("connected", anyConn);

  ["gemini", "openrouter", "codecraft"].forEach(p => {
    const conn = providerState[p].configured;
    const d1 = $("dot-settings-" + p);
    const d2 = $("dot-models-" + p);
    if (d1) d1.classList.toggle("on", conn);
    if (d2) d2.classList.toggle("on", conn);
  });

  const curConn = providerState[selectedProvider].configured;
  $("models-provider-status").textContent = curConn ? "CONNECTED" : "NEEDS SETUP";
  $("models-provider-status").classList.toggle("connected", curConn);
  $("settings-provider-state").textContent = curConn ? "Connected" : "Not connected";
  $("settings-provider-state").classList.toggle("connected", curConn);
}

function switchProviderTab(provider) {
  selectedProvider = provider;
  const meta = providerCatalog[provider];

  document.querySelectorAll(".provider-tab").forEach(b => {
    b.classList.toggle("active", b.dataset.provider === provider);
  });

  $("settings-provider-title").textContent = `${meta.name} Connection`;
  $("settings-provider-desc").textContent = meta.desc;
  $("api-key-label").textContent = meta.label;
  $("api-key").placeholder = meta.placeholder;
  $("api-key").value = "";
  $("settings-explorer-title").textContent = `Available Models · ${meta.name}`;
  $("provider-help-text").textContent = meta.helpText;
  $("provider-help-link").href = meta.helpUrl;
  $("provider-help-link").textContent = meta.helpBtn;

  $("models-feature-logo").textContent = meta.icon;
  $("models-feature-title").textContent = meta.name;
  $("models-feature-desc").textContent = meta.desc;

  const showFreeToggle = provider === "openrouter";
  $("settings-free-filter-wrap").classList.toggle("hidden", !showFreeToggle);
  $("models-free-filter-wrap").classList.toggle("hidden", !showFreeToggle);
  $("settings-free-only").checked = freeOnlyOpenRouter;
  $("models-free-only").checked = freeOnlyOpenRouter;

  updateProviderBadges();

  if (providerState[provider].configured && !providerState[provider].models.length) {
    loadProviderModels(provider, false);
  } else {
    renderModelGrids();
  }
}

document.querySelectorAll(".provider-tab").forEach(btn => {
  btn.addEventListener("click", () => switchProviderTab(btn.dataset.provider));
});

$("settings-free-only").addEventListener("change", e => {
  freeOnlyOpenRouter = e.target.checked;
  $("models-free-only").checked = freeOnlyOpenRouter;
  renderModelGrids();
});

$("models-free-only").addEventListener("change", e => {
  freeOnlyOpenRouter = e.target.checked;
  $("settings-free-only").checked = freeOnlyOpenRouter;
  renderModelGrids();
});

function showSetupNudge() {
  if (!currentSession) $("chat-empty").classList.remove("hidden");
}

$("login-form").addEventListener("submit", async e => {
  e.preventDefault();
  $("login-error").textContent = "";
  try {
    await api("/api/login", {
      method: "POST",
      body: JSON.stringify({ username: $("username").value, password: $("password").value })
    });
    await checkAuth();
  } catch (err) {
    $("login-error").textContent = err.message;
  }
});

$("logout").onclick = async () => {
  stopOrchPolling();
  try { await api("/api/logout", { method: "POST" }); } finally { showLogin(); }
};

function updateCurrentSessionHeader() {
  const cur = sessionsCache.find(s => s.id === currentSession);
  resetDeleteButton();
  closeRenameBar();
  if (!cur) return;
  $("conversation-title").textContent = cur.title;
  $("pinned-indicator").classList.toggle("hidden", !cur.pinned);
  $("pin-session-label").textContent = cur.pinned ? "Unpin" : "Pin";
  $("pin-session-btn").classList.toggle("active-tool", !!cur.pinned);
}

async function loadSessions() {
  const sessions = await api("/api/sessions");
  sessionsCache = sessions || [];
  $("session-list").innerHTML = "";
  $("sessions-grid").innerHTML = "";
  $("chat-count").textContent = String(sessionsCache.length).padStart(2, "0");

  sessionsCache.forEach(s => {
    const row = document.createElement("div");
    row.className = "session-row" + (s.id === currentSession ? " active" : "");

    const openBtn = document.createElement("button");
    openBtn.type = "button";
    openBtn.className = "session-item";
    openBtn.setAttribute("dir", "auto");
    openBtn.textContent = (s.pinned ? "📌 " : "") + s.title;
    openBtn.onclick = () => {
      switchPage("chat");
      openSession(s.id);
    };

    const actions = document.createElement("div");
    actions.className = "session-row-actions";

    const pinBtn = document.createElement("button");
    pinBtn.type = "button";
    pinBtn.className = "mini-act" + (s.pinned ? " is-pinned" : "");
    pinBtn.title = s.pinned ? "Unpin" : "Pin to top";
    pinBtn.textContent = "📌";
    pinBtn.onclick = e => {
      e.stopPropagation();
      togglePinSession(s.id, !s.pinned);
    };

    const renBtn = document.createElement("button");
    renBtn.type = "button";
    renBtn.className = "mini-act";
    renBtn.title = "Rename";
    renBtn.textContent = "✎";
    renBtn.onclick = async e => {
      e.stopPropagation();
      if (currentSession !== s.id) {
        switchPage("chat");
        await openSession(s.id, false);
      }
      openRenameBar();
    };

    const delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "mini-act danger";
    delBtn.title = "Delete conversation";
    delBtn.textContent = "×";
    delBtn.onclick = e => {
      e.stopPropagation();
      deleteSessionById(s.id);
    };

    actions.append(pinBtn, renBtn, delBtn);
    row.append(openBtn, actions);
    $("session-list").appendChild(row);

    const card = document.createElement("div");
    card.className = "session-card" + (s.pinned ? " is-pinned" : "");

    const cardMain = document.createElement("div");
    cardMain.className = "session-card-main";
    cardMain.onclick = () => { switchPage("chat"); openSession(s.id); };

    const title = document.createElement("b");
    title.setAttribute("dir", "auto");
    title.textContent = (s.pinned ? "📌 " : "") + s.title;
    const meta = document.createElement("span");
    meta.textContent = "Open conversation →";
    cardMain.append(title, meta);

    const cardTools = document.createElement("div");
    cardTools.className = "session-card-tools";

    const cPin = document.createElement("button");
    cPin.type = "button";
    cPin.className = "tool-btn small";
    cPin.textContent = s.pinned ? "📌 Unpin" : "📌 Pin";
    cPin.onclick = () => togglePinSession(s.id, !s.pinned);

    const cRen = document.createElement("button");
    cRen.type = "button";
    cRen.className = "tool-btn small";
    cRen.textContent = "✎ Rename";
    cRen.onclick = async () => {
      switchPage("chat");
      await openSession(s.id, false);
      openRenameBar();
    };

    const cDel = document.createElement("button");
    cDel.type = "button";
    cDel.className = "tool-btn small danger";
    cDel.textContent = "🗑 Delete";
    cDel.onclick = () => deleteSessionById(s.id);

    cardTools.append(cPin, cRen, cDel);
    card.append(cardMain, cardTools);
    $("sessions-grid").appendChild(card);
  });

  if (currentSession && !sessionsCache.some(s => s.id === currentSession)) {
    currentSession = null;
  }

  if (!currentSession && sessionsCache.length) {
    await openSession(sessionsCache[0].id, false);
  } else if (!sessionsCache.length) {
    $("chat-active").classList.add("hidden");
    $("chat-empty").classList.remove("hidden");
  } else {
    updateCurrentSessionHeader();
  }
}

async function newSession() {
  const s = await api("/api/sessions", { method: "POST" });
  currentSession = s.id;
  $("chat-empty").classList.add("hidden");
  $("chat-active").classList.remove("hidden");
  $("conversation-title").textContent = "New conversation";
  $("messages").innerHTML = "";
  await loadSessions();
  $("message-input").focus();
}

async function openSession(id, refresh = true) {
  currentSession = id;
  const msgs = await api("/api/messages?session_id=" + encodeURIComponent(id));
  $("chat-empty").classList.add("hidden");
  $("chat-active").classList.remove("hidden");
  $("messages").innerHTML = "";
  msgs.forEach(addMessage);
  if (refresh) await loadSessions();
  updateCurrentSessionHeader();
  $("messages").scrollTop = $("messages").scrollHeight;
}

async function togglePinSession(id, pinned) {
  try {
    await api("/api/sessions", {
      method: "PATCH",
      body: JSON.stringify({ id, pinned })
    });
    toast(pinned ? "📌 Conversation pinned to top" : "Conversation unpinned");
    await loadSessions();
  } catch (err) {
    toast("Could not update pin: " + err.message);
  }
}

function openRenameBar() {
  if (!currentSession) return;
  const cur = sessionsCache.find(s => s.id === currentSession);
  $("title-display-row").classList.add("hidden");
  $("rename-form").classList.remove("hidden");
  $("rename-input").value = cur ? cur.title : $("conversation-title").textContent;
  $("rename-input").focus();
  $("rename-input").select();
}

function closeRenameBar() {
  $("rename-form").classList.add("hidden");
  $("title-display-row").classList.remove("hidden");
}

$("rename-session-btn").onclick = () => openRenameBar();
$("conversation-title").onclick = () => openRenameBar();
$("rename-cancel").onclick = () => closeRenameBar();

$("rename-form").addEventListener("submit", async e => {
  e.preventDefault();
  if (!currentSession) return;
  const newTitle = $("rename-input").value.trim();
  if (!newTitle) return;
  try {
    await api("/api/sessions", {
      method: "PATCH",
      body: JSON.stringify({ id: currentSession, title: newTitle })
    });
    closeRenameBar();
    toast("✓ Conversation renamed");
    await loadSessions();
  } catch (err) {
    toast("Rename failed: " + err.message);
  }
});

$("pin-session-btn").onclick = () => {
  if (!currentSession) return;
  const cur = sessionsCache.find(s => s.id === currentSession);
  togglePinSession(currentSession, !(cur && cur.pinned));
};

function resetDeleteButton() {
  deleteArmed = false;
  clearTimeout(deleteConfirmTimer);
  if ($("delete-session-label")) $("delete-session-label").textContent = "Delete";
  if ($("delete-session-btn")) $("delete-session-btn").classList.remove("armed");
}

$("delete-session-btn").onclick = async () => {
  if (!currentSession) return;
  if (!deleteArmed) {
    deleteArmed = true;
    $("delete-session-label").textContent = "Confirm?";
    $("delete-session-btn").classList.add("armed");
    clearTimeout(deleteConfirmTimer);
    deleteConfirmTimer = setTimeout(() => resetDeleteButton(), 3500);
    return;
  }
  await deleteSessionById(currentSession);
};

async function deleteSessionById(id) {
  try {
    await api("/api/sessions?id=" + encodeURIComponent(id), { method: "DELETE" });
    resetDeleteButton();
    if (currentSession === id) currentSession = null;
    toast("🗑 Conversation deleted");
    await loadSessions();
  } catch (err) {
    toast("Delete failed: " + err.message);
  }
}

function addMessage(m) {
  const row = document.createElement("div");
  row.className = "message " + m.role;
  const av = document.createElement("div");
  av.className = "message-avatar";
  av.textContent = m.role === "user" ? "S" : "✳";
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  bubble.setAttribute("dir", "auto");
  bubble.textContent = m.content;
  row.append(av, bubble);
  $("messages").appendChild(row);
}

$("new-chat").onclick = $("hero-start").onclick = $("clear-view").onclick = $("sessions-new").onclick = () => newSession();
$("refresh-sessions").onclick = () => loadSessions();

$("model").addEventListener("change", () => {
  const val = $("model").value;
  if (val.includes("::")) {
    const [p, m] = val.split("::");
    setActiveSelectionUI(p, m);
  }
  renderModelGrids();
});

$("chat-form").addEventListener("submit", async e => {
  e.preventDefault();
  const input = $("message-input"), message = input.value.trim();
  if (!message) return;
  if (!currentSession) await newSession();
  input.value = "";
  addMessage({ role: "user", content: message });
  addMessage({ role: "assistant", content: "Thinking…" });
  const send = $("chat-form").querySelector(".send-button");
  send.disabled = true;
  try {
    const selVal = $("model").value;
    let provider = activeProvider, model = activeModel;
    if (selVal.includes("::")) {
      [provider, model] = selVal.split("::");
    }
    const out = await api("/api/messages", {
      method: "POST",
      body: JSON.stringify({ session_id: currentSession, message, provider, model })
    });
    $("messages").lastElementChild.remove();
    addMessage({ role: "assistant", content: out.reply });
    setActiveSelectionUI(out.provider || provider, out.model || model);
    toast(`Response received · ${out.latency_ms} ms (${out.model})`);
  } catch (err) {
    $("messages").lastElementChild.remove();
    addMessage({ role: "assistant", content: "Unable to complete request: " + err.message });
  } finally {
    send.disabled = false;
    await loadSessions();
    $("messages").scrollTop = $("messages").scrollHeight;
  }
});

$("message-input").addEventListener("keydown", e => {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    $("chat-form").requestSubmit();
  }
});

function switchPage(name) {
  localStorage.setItem("cortext_active_page", name);
  document.querySelectorAll(".page").forEach(p => p.classList.add("hidden"));
  $("page-" + name).classList.remove("hidden");
  document.querySelectorAll(".nav-link").forEach(b => b.classList.toggle("active", b.dataset.page === name));
  $("breadcrumb-page").textContent = pageNames[name];
  $("sidebar").classList.remove("open");

  if (name === "sessions") loadSessions();
  if (name === "settings" || name === "models") {
    refreshStatus();
    switchProviderTab(selectedProvider);
  }
  if (name === "orchestration") {
    initOrchestrationPage();
  } else {
    stopOrchPolling();
  }
}

document.querySelectorAll(".nav-link").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.page)));
document.querySelectorAll("[data-goto]").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.goto)));
$("setup-open").onclick = () => switchPage("settings");

async function refreshStatus() {
  try {
    const s = await api("/api/status");
    applyStatus(s);
  } catch {}
}

async function loadAllConfiguredModels() {
  for (const p of ["gemini", "openrouter", "codecraft"]) {
    if (providerState[p].configured) {
      await loadProviderModels(p, false);
    }
  }
  syncModelDropdown();
  renderModelGrids();
  ensureDefaultOrchParticipants();
}

async function loadProviderModels(provider, showToastMsg = false) {
  try {
    const res = await api("/api/models?provider=" + encodeURIComponent(provider));
    providerState[provider].configured = !!res.configured;
    providerState[provider].models = res.models || [];
    if (res.active_provider && res.active_model) {
      setActiveSelectionUI(res.active_provider, res.active_model);
    }
    updateProviderBadges();
    syncModelDropdown();
    renderModelGrids();
    if (showToastMsg) {
      toast(`Loaded ${providerState[provider].models.length} models from ${providerCatalog[provider].name}`);
    }
  } catch (err) {
    if (showToastMsg) toast(`Failed to load ${providerCatalog[provider].name} models: ` + err.message);
  }
}

function syncModelDropdown() {
  const sel = $("model");
  if (!sel) return;
  sel.innerHTML = "";

  const workingGroup = document.createElement("optgroup");
  workingGroup.label = "★ Verified Working Models";
  let hasWorking = false;

  ["gemini", "openrouter", "codecraft"].forEach(p => {
    const short = providerCatalog[p].short;
    (providerState[p].models || []).filter(m => m.status === "working").forEach(m => {
      hasWorking = true;
      const opt = document.createElement("option");
      opt.value = `${p}::${m.id}`;
      opt.textContent = `✓ [${short}] ${m.display_name} (${m.latency_ms} ms)`;
      workingGroup.appendChild(opt);
    });
  });
  if (hasWorking) sel.appendChild(workingGroup);

  ["gemini", "openrouter", "codecraft"].forEach(p => {
    const list = providerState[p].models || [];
    if (!list.length) return;
    const grp = document.createElement("optgroup");
    grp.label = providerCatalog[p].name;
    const filtered = p === "openrouter" ? list.filter((m, idx) => m.status === "working" || m.is_free || idx < 25) : list;
    filtered.forEach(m => {
      const opt = document.createElement("option");
      opt.value = `${p}::${m.id}`;
      const prefix = m.status === "working" ? "✓ " : m.status === "failed" ? "✗ " : "";
      const freeTag = (p === "openrouter" && m.is_free) ? " [FREE]" : "";
      opt.textContent = `${prefix}${m.display_name}${freeTag} (${m.id})`;
      grp.appendChild(opt);
    });
    sel.appendChild(grp);
  });

  const targetVal = `${activeProvider}::${activeModel}`;
  if ([...sel.options].some(o => o.value === targetVal)) {
    sel.value = targetVal;
  } else if (sel.options.length > 0) {
    const [p, m] = sel.options[0].value.split("::");
    setActiveSelectionUI(p, m);
  }
}

function getDisplayedModels(provider) {
  const all = providerState[provider]?.models || [];
  if (provider === "openrouter" && freeOnlyOpenRouter) {
    return all.filter(m => m.is_free || m.status === "working");
  }
  return all;
}

function renderModelGrids() {
  const provider = selectedProvider;
  const meta = providerCatalog[provider];
  const allModels = providerState[provider]?.models || [];
  const shownModels = getDisplayedModels(provider);

  const workingCount = allModels.filter(m => m.status === "working").length;
  const failedCount = allModels.filter(m => m.status === "failed").length;
  const freeCount = allModels.filter(m => m.is_free).length;

  const targets = [
    { grid: $("settings-models-grid"), summary: $("settings-models-summary") },
    { grid: $("models-page-grid"), summary: $("models-page-summary") }
  ];

  targets.forEach(({ grid, summary }) => {
    if (!grid) return;
    grid.innerHTML = "";
    if (!allModels.length) {
      if (summary) {
        summary.innerHTML = `No models loaded for <b>${meta.name}</b> yet. Paste your ${meta.name} API key in Settings and click <b>Save Key & Load Models</b>.`;
      }
      return;
    }

    if (summary) {
      const freeNote = provider === "openrouter" ? ` · <b>${freeCount}</b> free models` : "";
      summary.innerHTML = `<b>${meta.name}</b> returned <b>${allModels.length}</b> models${freeNote} (showing <b>${shownModels.length}</b>) · <span class="text-ok"><b>${workingCount}</b> verified working</span> · <span class="text-err"><b>${failedCount}</b> failed</span>. <b>Click any card to test live:</b>`;
    }

    shownModels.forEach(m => {
      const card = document.createElement("button");
      card.type = "button";
      const isCurrent = (provider === activeProvider && m.id === activeModel);
      card.className = `api-model-card status-${m.status || "untested"}${isCurrent ? " is-active" : ""}`;

      const top = document.createElement("div");
      top.className = "api-model-top";

      const titleWrap = document.createElement("div");
      titleWrap.className = "api-model-title";
      const nameEl = document.createElement("b");
      nameEl.textContent = m.display_name || m.id;
      nameEl.title = m.display_name || m.id;
      const codeEl = document.createElement("small");
      codeEl.textContent = m.id;
      codeEl.title = m.id;
      titleWrap.append(nameEl, codeEl);

      const badgeWrap = document.createElement("div");
      badgeWrap.className = "api-model-badges";
      if (isCurrent) {
        const act = document.createElement("span");
        act.className = "model-badge active-badge";
        act.textContent = "★ ACTIVE";
        badgeWrap.appendChild(act);
      }
      if (m.is_free && provider === "openrouter") {
        const freeBadge = document.createElement("span");
        freeBadge.className = "model-badge free-badge";
        freeBadge.textContent = "FREE";
        badgeWrap.appendChild(freeBadge);
      }

      const statusBadge = document.createElement("span");
      statusBadge.className = `model-badge badge-${m.status || "untested"}`;
      if (m.status === "working") {
        statusBadge.textContent = `✓ ${m.latency_ms} ms`;
      } else if (m.status === "testing") {
        statusBadge.textContent = "⟳ TESTING…";
      } else if (m.status === "failed") {
        statusBadge.textContent = "✗ FAILED";
      } else {
        statusBadge.textContent = "TEST";
      }
      badgeWrap.appendChild(statusBadge);
      top.append(titleWrap, badgeWrap);

      const metaRow = document.createElement("div");
      metaRow.className = "api-model-meta";
      if (m.status === "failed" && m.last_error) {
        metaRow.textContent = m.last_error;
        metaRow.classList.add("err-text");
      } else if (m.status === "working") {
        metaRow.textContent = `Verified working on ${meta.name} (${m.latency_ms} ms) · ${m.price_info || "Ready for chat"}`;
        metaRow.classList.add("ok-text");
      } else {
        const ctx = m.context_len ? `Context: ${m.context_len.toLocaleString()} tokens` : "";
        const price = m.price_info ? `${m.price_info}` : "";
        const cleanDesc = (m.description || "").replace(/\s+/g, " ").trim();
        const infoLine = [price, ctx, cleanDesc].filter(Boolean).join(" · ");
        metaRow.textContent = infoLine || "Click to send a test prompt and verify if this model works.";
      }

      card.append(top, metaRow);
      card.onclick = () => testSingleModel(provider, m.id, true);
      grid.appendChild(card);
    });
  });
}

async function testSingleModel(provider, modelId, setActive = true) {
  const list = providerState[provider]?.models || [];
  const item = list.find(x => x.id === modelId);
  if (item) {
    item.status = "testing";
    renderModelGrids();
  }
  try {
    const res = await api("/api/models/test", {
      method: "POST",
      body: JSON.stringify({ provider, model: modelId, set_active: setActive })
    });
    if (item) {
      item.status = res.ok ? "working" : "failed";
      item.latency_ms = res.latency_ms || 0;
      item.last_error = res.error || "";
    }
    if (res.ok) {
      if (setActive) setActiveSelectionUI(provider, modelId);
      toast(`✓ [${providerCatalog[provider].short}] ${modelId} works! (${res.latency_ms} ms)`);
    } else {
      toast(`✗ ${modelId} failed on ${providerCatalog[provider].short}`);
    }
  } catch (err) {
    if (item) {
      item.status = "failed";
      item.last_error = err.message;
    }
  }
  syncModelDropdown();
  renderModelGrids();
  renderOrchParticipantsBuilder();
}

async function testShownModels() {
  const provider = selectedProvider;
  const shown = getDisplayedModels(provider);
  if (!shown.length) {
    toast(`Save your ${providerCatalog[provider].name} API key first.`);
    return;
  }
  toast(`Testing ${shown.length} models on ${providerCatalog[provider].name}…`);
  let foundWorking = false;
  for (const m of shown) {
    const activate = !foundWorking;
    await testSingleModel(provider, m.id, activate);
    const updated = (providerState[provider].models || []).find(x => x.id === m.id);
    if (updated && updated.status === "working" && !foundWorking) {
      foundWorking = true;
    }
  }
  const workingCount = (providerState[provider].models || []).filter(m => m.status === "working").length;
  toast(`Done! ${workingCount} working models found on ${providerCatalog[provider].name}.`);
}

$("api-key-form").addEventListener("submit", async e => {
  e.preventDefault();
  const provider = selectedProvider;
  const meta = providerCatalog[provider];
  const key = $("api-key").value.trim();
  const fb = $("key-feedback");
  const btn = $("save-key-btn");
  btn.disabled = true;
  btn.textContent = `Connecting to ${meta.name}…`;
  fb.className = "key-feedback";
  fb.textContent = `Sending request to ${meta.name} to verify key and retrieve available models…`;

  try {
    const res = await api("/api/setup/provider", {
      method: "POST",
      body: JSON.stringify({ provider, api_key: key })
    });
    $("api-key").value = "";
    providerState[provider].configured = true;
    providerState[provider].models = res.models || [];
    if (res.active_provider && res.active_model) {
      setActiveSelectionUI(res.active_provider, res.active_model);
    }
    updateProviderBadges();
    syncModelDropdown();
    renderModelGrids();
    ensureDefaultOrchParticipants();
    $("setup-banner").classList.add("hidden");

    fb.className = "key-feedback ok";
    fb.innerHTML = `✓ <b>${meta.name} API key saved (${res.latency_ms} ms)!</b> Loaded <b>${providerState[provider].models.length}</b> models below. <b>Click any model card below to test if it responds!</b>`;
    toast(`${meta.name} connected! (${providerState[provider].models.length} models)`);
  } catch (err) {
    fb.className = "key-feedback err";
    fb.innerHTML = `✗ <b>${meta.name} error:</b> ${err.message}`;
  } finally {
    btn.disabled = false;
    btn.innerHTML = 'Save Key & Load Models <span>↗</span>';
  }
});

$("reload-models-btn").onclick = () => loadProviderModels(selectedProvider, true);
$("refresh-models-page").onclick = () => loadProviderModels(selectedProvider, true);
$("test-all-models-btn").onclick = () => testShownModels();
$("test-all-models-page").onclick = () => testShownModels();

$("toggle-key").onclick = () => {
  const i = $("api-key");
  i.type = i.type === "password" ? "text" : "password";
  $("toggle-key").textContent = i.type === "password" ? "Show" : "Hide";
};

document.querySelectorAll(".settings-tab").forEach(b => b.onclick = () => {
  document.querySelectorAll(".settings-tab").forEach(x => x.classList.toggle("active", x === b));
  document.querySelectorAll(".settings-panel").forEach(x => x.classList.add("hidden"));
  $(b.dataset.tab).classList.remove("hidden");
});

/* ============================================================================
   PHASE 3.3 — AI ORCHESTRATION FRONTEND INTEGRATION
   ============================================================================ */

function getConfiguredProvidersList() {
  return ["gemini", "openrouter", "codecraft"].filter(p => providerState[p].configured);
}

function getDefaultModelForProvider(p) {
  const models = providerState[p]?.models || [];
  const working = models.find(m => m.status === "working");
  if (working) return working.id;
  if (p === "openrouter") {
    const free = models.find(m => m.is_free);
    if (free) return free.id;
  }
  if (models.length > 0) return models[0].id;
  return p === "gemini" ? "gemini-2.0-flash" : "";
}

function ensureDefaultOrchParticipants() {
  const configured = getConfiguredProvidersList();
  const fallbackProv = configured[0] || activeProvider || "gemini";
  const fallbackMod = getDefaultModelForProvider(fallbackProv) || activeModel || "gemini-2.0-flash";

  orchParticipantsDraft.forEach(p => {
    if (!providerState[p.provider]?.configured && configured.length > 0) {
      p.provider = fallbackProv;
      p.model = fallbackMod;
    } else if (!p.model) {
      p.model = getDefaultModelForProvider(p.provider) || fallbackMod;
    }
  });
}

async function initOrchestrationPage() {
  ensureDefaultOrchParticipants();
  renderOrchParticipantsBuilder();
  await loadOrchJobsList();

  if (activeOrchJobId) {
    await openOrchJobMonitor(activeOrchJobId);
  } else {
    showOrchBuilder();
  }
}

function showOrchBuilder() {
  stopOrchPolling();
  activeOrchJobId = null;
  localStorage.removeItem("cortext_active_orch_job");
  $("orch-monitor-panel").classList.add("hidden");
  $("orch-builder-panel").classList.remove("hidden");
  renderOrchParticipantsBuilder();
  highlightActiveOrchJobCard();
}

function renderOrchParticipantsBuilder() {
  const container = $("orch-participants-list");
  if (!container) return;
  container.innerHTML = "";

  orchParticipantsDraft.forEach((part, idx) => {
    const row = document.createElement("div");
    row.className = "orch-participant-card";

    // Order badge & Role selector
    const topBar = document.createElement("div");
    topBar.className = "orch-part-top";

    const stepBadge = document.createElement("span");
    stepBadge.className = "orch-step-num";
    stepBadge.textContent = `#${idx + 1}`;

    const roleSelect = document.createElement("select");
    roleSelect.className = "orch-role-select";
    [
      { value: "coordinator", label: "Coordinator / Architect" },
      { value: "worker", label: "Worker / Developer" }
    ].forEach(r => {
      const opt = document.createElement("option");
      opt.value = r.value;
      opt.textContent = r.label;
      if (part.role === r.value) opt.selected = true;
      roleSelect.appendChild(opt);
    });
    roleSelect.onchange = () => {
      part.role = roleSelect.value;
      part.name = roleSelect.value === "coordinator" ? "Coordinator / Architect" : "Worker / Developer";
    };

    const orderBtns = document.createElement("div");
    orderBtns.className = "orch-part-controls";

    const upBtn = document.createElement("button");
    upBtn.type = "button";
    upBtn.className = "mini-act";
    upBtn.title = "Move up";
    upBtn.textContent = "↑";
    upBtn.disabled = idx === 0;
    upBtn.onclick = () => {
      if (idx > 0) {
        const tmp = orchParticipantsDraft[idx - 1];
        orchParticipantsDraft[idx - 1] = orchParticipantsDraft[idx];
        orchParticipantsDraft[idx] = tmp;
        renderOrchParticipantsBuilder();
      }
    };

    const downBtn = document.createElement("button");
    downBtn.type = "button";
    downBtn.className = "mini-act";
    downBtn.title = "Move down";
    downBtn.textContent = "↓";
    downBtn.disabled = idx === orchParticipantsDraft.length - 1;
    downBtn.onclick = () => {
      if (idx < orchParticipantsDraft.length - 1) {
        const tmp = orchParticipantsDraft[idx + 1];
        orchParticipantsDraft[idx + 1] = orchParticipantsDraft[idx];
        orchParticipantsDraft[idx] = tmp;
        renderOrchParticipantsBuilder();
      }
    };

    orderBtns.append(upBtn, downBtn);

    if (orchParticipantsDraft.length > 2) {
      const rmBtn = document.createElement("button");
      rmBtn.type = "button";
      rmBtn.className = "mini-act danger";
      rmBtn.title = "Remove participant";
      rmBtn.textContent = "×";
      rmBtn.onclick = () => {
        orchParticipantsDraft.splice(idx, 1);
        renderOrchParticipantsBuilder();
      };
      orderBtns.appendChild(rmBtn);
    }

    topBar.append(stepBadge, roleSelect, orderBtns);

    // Provider & Model selectors
    const gridRow = document.createElement("div");
    gridRow.className = "orch-part-grid";

    const provWrap = document.createElement("label");
    provWrap.innerHTML = `<span>Provider</span>`;
    const provSelect = document.createElement("select");
    ["gemini", "openrouter", "codecraft"].forEach(p => {
      const opt = document.createElement("option");
      opt.value = p;
      const isCfg = providerState[p].configured;
      opt.textContent = `${providerCatalog[p].name}${isCfg ? " (Connected)" : " (Needs API Key)"}`;
      if (part.provider === p) opt.selected = true;
      provSelect.appendChild(opt);
    });

    const modWrap = document.createElement("label");
    modWrap.innerHTML = `<span>Model</span>`;
    const modSelect = document.createElement("select");

    function populateModelsForPart() {
      modSelect.innerHTML = "";
      const p = part.provider;
      const list = providerState[p]?.models || [];
      if (!list.length) {
        const fallbackOpt = document.createElement("option");
        fallbackOpt.value = part.model || "";
        fallbackOpt.textContent = part.model ? `${part.model} (Configure key in Settings)` : "No models loaded — configure provider";
        modSelect.appendChild(fallbackOpt);
        return;
      }

      const working = list.filter(m => m.status === "working");
      const others = p === "openrouter"
        ? list.filter((m, i) => m.status !== "working" && (m.is_free || i < 40))
        : list.filter(m => m.status !== "working");

      if (working.length) {
        const grpW = document.createElement("optgroup");
        grpW.label = "★ Verified Working";
        working.forEach(m => {
          const o = document.createElement("option");
          o.value = m.id;
          o.textContent = `✓ ${m.display_name} (${m.id})`;
          grpW.appendChild(o);
        });
        modSelect.appendChild(grpW);
      }

      if (others.length) {
        const grpO = document.createElement("optgroup");
        grpO.label = "Available Models";
        others.forEach(m => {
          const o = document.createElement("option");
          o.value = m.id;
          const freeTag = (p === "openrouter" && m.is_free) ? " [FREE]" : "";
          o.textContent = `${m.display_name}${freeTag} (${m.id})`;
          grpO.appendChild(o);
        });
        modSelect.appendChild(grpO);
      }

      if ([...modSelect.options].some(o => o.value === part.model)) {
        modSelect.value = part.model;
      } else if (modSelect.options.length > 0) {
        part.model = modSelect.options[0].value;
        modSelect.value = part.model;
      }
    }

    provSelect.onchange = () => {
      part.provider = provSelect.value;
      part.model = getDefaultModelForProvider(part.provider);
      populateModelsForPart();
    };
    modSelect.onchange = () => {
      part.model = modSelect.value;
    };

    populateModelsForPart();
    provWrap.appendChild(provSelect);
    modWrap.appendChild(modSelect);
    gridRow.append(provWrap, modWrap);

    row.append(topBar, gridRow);
    container.appendChild(row);
  });
}

$("orch-add-participant").onclick = () => {
  const configured = getConfiguredProvidersList();
  const defProv = configured[0] || activeProvider || "gemini";
  const defMod = getDefaultModelForProvider(defProv) || activeModel || "gemini-2.0-flash";
  orchParticipantsDraft.push({
    role: "worker",
    name: "Worker / Developer",
    provider: defProv,
    model: defMod
  });
  renderOrchParticipantsBuilder();
};

$("orch-new-task-btn").onclick = () => showOrchBuilder();
$("orch-back-builder-btn").onclick = () => showOrchBuilder();
$("orch-refresh-btn").onclick = async () => {
  await loadOrchJobsList();
  if (activeOrchJobId) await fetchAndRenderOrchJob(activeOrchJobId);
  toast("Orchestration state refreshed");
};

// Switch between Collaboration Transcript and Live Progress Timeline
document.querySelectorAll(".orch-view-tab").forEach(btn => {
  btn.onclick = () => {
    orchActiveSubView = btn.dataset.orchview;
    document.querySelectorAll(".orch-view-tab").forEach(b => b.classList.toggle("active", b === btn));
    $("orch-messages-view").classList.toggle("hidden", orchActiveSubView !== "messages");
    $("orch-events-view").classList.toggle("hidden", orchActiveSubView !== "events");
  };
});

// Submit New Collaboration Job
$("orch-create-form").addEventListener("submit", async e => {
  e.preventDefault();
  if (orchSubmitting) return;

  const errBanner = $("orch-form-error");
  errBanner.classList.add("hidden");
  errBanner.textContent = "";

  const task = $("orch-task-input").value.trim();
  if (!task) {
    errBanner.textContent = "Please enter a task or objective for the models to collaborate on.";
    errBanner.classList.remove("hidden");
    return;
  }

  if (orchParticipantsDraft.length < 2) {
    errBanner.textContent = "At least two participants are required for collaboration.";
    errBanner.classList.remove("hidden");
    return;
  }

  const hasCoord = orchParticipantsDraft.some(p => p.role === "coordinator");
  const hasWorker = orchParticipantsDraft.some(p => p.role === "worker");
  if (!hasCoord || !hasWorker) {
    errBanner.textContent = "Please assign at least one Coordinator / Architect and one Worker / Developer.";
    errBanner.classList.remove("hidden");
    return;
  }

  for (let i = 0; i < orchParticipantsDraft.length; i++) {
    const p = orchParticipantsDraft[i];
    if (!providerState[p.provider]?.configured) {
      errBanner.textContent = `Participant #${i + 1} uses ${providerCatalog[p.provider]?.name || p.provider}, which does not have a saved API key yet. Configure it in Settings first.`;
      errBanner.classList.remove("hidden");
      return;
    }
    if (!p.model || !p.model.trim()) {
      errBanner.textContent = `Please select a valid model for Participant #${i + 1}.`;
      errBanner.classList.remove("hidden");
      return;
    }
  }

  const maxRounds = parseInt($("orch-max-rounds").value, 10) || 5;
  const maxCalls = parseInt($("orch-max-calls").value, 10) || 12;

  orchSubmitting = true;
  const submitBtn = $("orch-submit-btn");
  submitBtn.disabled = true;
  submitBtn.textContent = "Starting Collaboration…";

  try {
    const createdJob = await api("/api/orchestration/jobs", {
      method: "POST",
      body: JSON.stringify({
        task,
        max_rounds: maxRounds,
        max_calls: maxCalls,
        participants: orchParticipantsDraft.map((p, idx) => ({
          role: p.role,
          name: p.role === "coordinator" ? "Coordinator / Architect" : "Worker / Developer",
          provider: p.provider,
          model: p.model,
          sort_order: idx
        }))
      })
    });

    $("orch-task-input").value = "";
    toast("Collaboration task started in background");
    await loadOrchJobsList();
    await openOrchJobMonitor(createdJob.id);
  } catch (err) {
    errBanner.textContent = err.message;
    errBanner.classList.remove("hidden");
  } finally {
    orchSubmitting = false;
    submitBtn.disabled = false;
    submitBtn.innerHTML = 'Start Collaboration <span>↗</span>';
  }
});

async function loadOrchJobsList() {
  try {
    const jobs = await api("/api/orchestration/jobs?limit=40");
    orchJobsCache = jobs || [];
    $("orch-count").textContent = String(orchJobsCache.length).padStart(2, "0");
    if ($("orch-jobs-total")) $("orch-jobs-total").textContent = `${orchJobsCache.length} TASKS`;
    renderOrchJobsList();
  } catch {}
}

function renderOrchJobsList() {
  const listEl = $("orch-jobs-list");
  if (!listEl) return;
  listEl.innerHTML = "";

  if (!orchJobsCache.length) {
    const empty = document.createElement("div");
    empty.className = "orch-empty-note";
    empty.textContent = "No collaboration tasks yet. Configure your team and start your first multi-model task.";
    listEl.appendChild(empty);
    return;
  }

  orchJobsCache.forEach(j => {
    const item = document.createElement("button");
    item.type = "button";
    item.className = `orch-job-item${j.id === activeOrchJobId ? " active" : ""}`;
    item.dataset.jobId = j.id;

    const top = document.createElement("div");
    top.className = "orch-job-item-top";

    const badge = document.createElement("span");
    badge.className = `orch-status-pill status-${j.status}`;
    badge.textContent = (j.status || "queued").toUpperCase();

    const meta = document.createElement("small");
    meta.textContent = `R${j.current_round || 0}/${j.max_rounds || 5} · ${j.total_calls || 0} calls`;
    top.append(badge, meta);

    const title = document.createElement("b");
    title.setAttribute("dir", "auto");
    title.textContent = j.task;

    const timeEl = document.createElement("span");
    timeEl.className = "orch-job-time";
    timeEl.textContent = formatShortTimestamp(j.created_at);

    item.append(top, title, timeEl);
    item.onclick = () => openOrchJobMonitor(j.id);
    listEl.appendChild(item);
  });
}

function highlightActiveOrchJobCard() {
  document.querySelectorAll(".orch-job-item").forEach(el => {
    el.classList.toggle("active", el.dataset.jobId === activeOrchJobId);
  });
}

async function openOrchJobMonitor(jobId) {
  activeOrchJobId = jobId;
  localStorage.setItem("cortext_active_orch_job", jobId);
  $("orch-builder-panel").classList.add("hidden");
  $("orch-monitor-panel").classList.remove("hidden");
  highlightActiveOrchJobCard();

  await fetchAndRenderOrchJob(jobId);
}

function stopOrchPolling() {
  if (orchPollTimer) {
    clearTimeout(orchPollTimer);
    orchPollTimer = null;
  }
}

async function fetchAndRenderOrchJob(jobId) {
  stopOrchPolling();
  if (!jobId || activeOrchJobId !== jobId) return;

  try {
    const details = await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/details`);
    renderOrchJobDetails(details);

    // Update cached job status in sidebar list
    const idx = orchJobsCache.findIndex(x => x.id === jobId);
    if (idx !== -1) {
      orchJobsCache[idx] = { ...orchJobsCache[idx], ...details };
      renderOrchJobsList();
    }

    // Keep polling while job is active
    if (details.status === "queued" || details.status === "running" || details.status === "waiting") {
      orchPollTimer = setTimeout(() => fetchAndRenderOrchJob(jobId), 1500);
    }
  } catch (err) {
    toast("Could not load orchestration job: " + err.message);
  }
}

function renderOrchJobDetails(d) {
  const status = (d.status || "queued").toLowerCase();
  const stage = (d.current_stage || status).toLowerCase();

  const statusEl = $("orch-detail-status");
  statusEl.className = `orch-status-badge status-${status}`;
  statusEl.textContent = status.toUpperCase();

  $("orch-detail-stage").textContent = `STAGE: ${stage.toUpperCase()}`;
  $("orch-detail-rounds").textContent = `Round ${d.current_round || 0} / ${d.max_rounds || 5}`;
  $("orch-detail-calls").textContent = `Calls ${d.total_calls || 0} / ${d.max_calls || 12}`;
  $("orch-detail-task").textContent = d.task || "";
  $("orch-detail-sub").textContent = `Job ID: ${d.id} · Created ${formatShortTimestamp(d.created_at)}${d.retry_count ? ` · Retries: ${d.retry_count}/${d.max_retries}` : ""}`;

  // Show/hide Cancel button based on active state
  const isActive = status === "queued" || status === "running" || status === "waiting";
  const cancelBtn = $("orch-cancel-btn");
  cancelBtn.classList.toggle("hidden", !isActive);
  cancelBtn.disabled = false;
  cancelBtn.textContent = "⏹ Stop Task";
  cancelBtn.onclick = () => cancelActiveOrchJob(d.id);

  // Active model indicator banner
  const activeBanner = $("orch-active-banner");
  const participants = d.participants || [];
  const coordPart = participants.find(p => p.role === "coordinator") || participants[0];
  const workerPart = participants.find(p => p.role === "worker") || participants[1] || coordPart;

  if (isActive) {
    activeBanner.classList.remove("hidden");
    if (status === "queued") {
      $("orch-active-headline").textContent = "Task queued — waiting for background worker…";
      $("orch-active-subline").textContent = "A worker will claim this job automatically.";
    } else if (status === "waiting") {
      $("orch-active-headline").textContent = "Waiting to retry after temporary provider error…";
      $("orch-active-subline").textContent = d.error_message || "Scheduled for automatic retry.";
    } else if (stage === "planning") {
      $("orch-active-headline").textContent = `Coordinator (${coordPart ? `${providerCatalog[coordPart.provider]?.name || coordPart.provider} · ${coordPart.model}` : "Architect"}) is creating the implementation plan…`;
      $("orch-active-subline").textContent = "Analyzing task and preparing instructions for Worker.";
    } else if (stage === "working") {
      $("orch-active-headline").textContent = `Worker (${workerPart ? `${providerCatalog[workerPart.provider]?.name || workerPart.provider} · ${workerPart.model}` : "Developer"}) is executing round ${d.current_round || 1}…`;
      $("orch-active-subline").textContent = "Producing deliverable based on Coordinator instructions.";
    } else if (stage === "reviewing") {
      $("orch-active-headline").textContent = `Coordinator (${coordPart ? `${providerCatalog[coordPart.provider]?.name || coordPart.provider} · ${coordPart.model}` : "Architect"}) is reviewing round ${d.current_round || 1}…`;
      $("orch-active-subline").textContent = "Evaluating Worker deliverable for completeness and accuracy.";
    } else {
      $("orch-active-headline").textContent = "Collaboration in progress…";
      $("orch-active-subline").textContent = `Stage: ${stage}`;
    }
  } else {
    activeBanner.classList.add("hidden");
  }

  // Error Banner
  const errEl = $("orch-detail-error");
  if (d.error_message && (status === "failed" || status === "waiting")) {
    errEl.textContent = `Error: ${d.error_message}`;
    errEl.classList.remove("hidden");
  } else {
    errEl.classList.add("hidden");
  }

  // Participant Roster Pills
  const rosterEl = $("orch-detail-roster");
  rosterEl.innerHTML = "";
  participants.forEach((p, i) => {
    const pill = document.createElement("div");
    const isWorkingNow = isActive && ((stage === "planning" || stage === "reviewing") ? p.role === "coordinator" : (stage === "working" && p.role === "worker"));
    pill.className = `orch-roster-pill${isWorkingNow ? " working-now" : ""}`;
    const roleLabel = p.role === "coordinator" ? "Coordinator / Architect" : "Worker / Developer";
    const provName = providerCatalog[p.provider]?.name || p.provider;
    pill.innerHTML = `<span class="roster-role">#${i + 1} ${roleLabel}</span><b>${provName} · ${p.model}</b>`;
    rosterEl.appendChild(pill);
  });

  // Final Result Card
  const finalCard = $("orch-final-card");
  if (status === "completed" && d.execution_result) {
    finalCard.classList.remove("hidden");
    $("orch-final-body").textContent = d.execution_result;
    $("orch-copy-result-btn").onclick = () => {
      navigator.clipboard.writeText(d.execution_result).then(() => toast("✓ Final result copied to clipboard"));
    };
  } else {
    finalCard.classList.add("hidden");
  }

  // Render Collaboration Messages Transcript
  const messages = d.messages || [];
  $("orch-msg-count").textContent = String(messages.length);
  const msgContainer = $("orch-messages-view");
  msgContainer.innerHTML = "";

  if (!messages.length) {
    const emptyMsg = document.createElement("div");
    emptyMsg.className = "orch-empty-note";
    emptyMsg.textContent = isActive
      ? "Waiting for the first model response to complete…"
      : "No model responses were recorded for this task.";
    msgContainer.appendChild(emptyMsg);
  } else {
    messages.forEach(m => {
      const card = document.createElement("article");
      card.className = `orch-msg-card role-${m.sender_role}`;

      const head = document.createElement("div");
      head.className = "orch-msg-head";

      const left = document.createElement("div");
      left.className = "orch-msg-sender";
      const roleTitle = m.sender_role === "coordinator" ? "COORDINATOR / ARCHITECT" : "WORKER / DEVELOPER";
      const provShort = providerCatalog[m.provider]?.short || (m.provider || "").toUpperCase();
      left.innerHTML = `<span class="orch-role-tag ${m.sender_role}">${roleTitle}</span> <b>${provShort} · ${m.model}</b>`;

      const right = document.createElement("div");
      right.className = "orch-msg-meta";
      right.innerHTML = `<span>Round ${m.round}</span> · <span class="action-tag action-${m.action}">Action: ${(m.action || "continue").toUpperCase()}</span> · <span>${m.latency_ms || 0} ms</span>`;

      head.append(left, right);

      const body = document.createElement("div");
      body.className = "orch-msg-body";
      body.setAttribute("dir", "auto");
      body.textContent = m.content;

      card.append(head, body);

      if (m.completion_summary) {
        const sum = document.createElement("div");
        sum.className = "orch-msg-summary";
        sum.setAttribute("dir", "auto");
        sum.innerHTML = `<b>Summary:</b> `;
        const span = document.createElement("span");
        span.textContent = m.completion_summary;
        sum.appendChild(span);
        card.appendChild(sum);
      }

      msgContainer.appendChild(card);
    });
  }

  // Render Persisted Event Timeline
  const events = d.events || [];
  $("orch-evt-count").textContent = String(events.length);
  const evtContainer = $("orch-events-view");
  evtContainer.innerHTML = "";

  events.forEach(ev => {
    const row = document.createElement("div");
    row.className = `orch-event-row evt-${ev.event_type}`;

    const dot = document.createElement("span");
    dot.className = "orch-event-dot";

    const main = document.createElement("div");
    main.className = "orch-event-main";
    const topLine = document.createElement("div");
    topLine.className = "orch-event-top";

    const label = document.createElement("b");
    label.textContent = formatEventTypeLabel(ev.event_type, ev.phase);

    const time = document.createElement("small");
    time.textContent = formatShortTimestamp(ev.created_at);

    topLine.append(label, time);

    const msg = document.createElement("div");
    msg.className = "orch-event-msg";
    msg.setAttribute("dir", "auto");
    msg.textContent = ev.message;

    main.append(topLine, msg);
    row.append(dot, main);
    evtContainer.appendChild(row);
  });
}

async function cancelActiveOrchJob(jobId) {
  const btn = $("orch-cancel-btn");
  btn.disabled = true;
  btn.textContent = "Stopping…";
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/cancel`, { method: "POST" });
    toast("⏹ Task cancellation confirmed");
    await loadOrchJobsList();
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Cancel failed: " + err.message);
    btn.disabled = false;
    btn.textContent = "⏹ Stop Task";
  }
}

function formatEventTypeLabel(evtType, phase) {
  const map = {
    job_created: "Job Created",
    job_claimed: "Claimed by Worker",
    job_started: "Execution Started",
    coordinator_started: "Coordinator Planning Started",
    plan_generated: "Implementation Plan Generated",
    worker_started: "Worker Execution Started",
    worker_response_received: "Worker Deliverable Received",
    review_started: "Coordinator Review Started",
    review_decision: "Coordinator Review Decision",
    revision_requested: "Revision Requested",
    revision_completed: "Worker Revision Completed",
    model_invoked: "Model Request Sent",
    model_responded: "Model Response Received",
    retry_scheduled: "Transient Retry Scheduled",
    worker_recovery: "Recovered by Worker",
    task_completed: "Collaboration Completed",
    job_completed: "Job Marked Complete",
    job_failed: "Job Failed",
    job_cancelled: "Job Cancelled"
  };
  return map[evtType] || `${evtType} (${phase || "info"})`;
}

function formatShortTimestamp(ts) {
  if (!ts) return "";
  try {
    const d = new Date(ts);
    if (isNaN(d.getTime())) return String(ts);
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  } catch {
    return String(ts);
  }
}

$("mobile-menu").onclick = () => $("sidebar").classList.add("open");
$("mobile-close").onclick = () => $("sidebar").classList.remove("open");

document.addEventListener("keydown", e => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    switchPage("chat");
    newSession();
  }
  if (e.key === "Escape") {
    $("sidebar").classList.remove("open");
    closeRenameBar();
    resetDeleteButton();
  }
});

switchProviderTab("gemini");
checkAuth();
