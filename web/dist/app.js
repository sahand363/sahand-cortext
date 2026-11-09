const $ = id => document.getElementById(id);
let currentSession = null, toastTimer;

let selectedProvider = "gemini"; // currently viewed tab in Settings / Models
let activeProvider = "gemini";   // active provider used for Chat
let activeModel = "gemini-2.0-flash";
let freeOnlyOpenRouter = true;   // default to showing :free models first on OpenRouter

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
    desc: "Connect CodeCraft API to test which of their 33 hosted models respond on your free starter plan.",
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
    if (!s.any_configured) {
      $("setup-banner").classList.remove("hidden");
      showSetupNudge();
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

  // Show the ":free only" checkbox on OpenRouter
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
  try { await api("/api/logout", { method: "POST" }); } finally { showLogin(); }
};

async function loadSessions() {
  const sessions = await api("/api/sessions");
  $("session-list").innerHTML = "";
  $("sessions-grid").innerHTML = "";
  $("chat-count").textContent = String(sessions.length).padStart(2, "0");
  sessions.forEach(s => {
    const b = document.createElement("button");
    b.className = "session-item" + (s.id === currentSession ? " active" : "");
    b.textContent = s.title;
    b.onclick = () => openSession(s.id);
    $("session-list").appendChild(b);

    const card = document.createElement("button");
    card.className = "session-card";
    const title = document.createElement("b");
    title.textContent = s.title;
    const meta = document.createElement("span");
    meta.textContent = "Open conversation →";
    card.append(title, meta);
    card.onclick = () => { switchPage("chat"); openSession(s.id); };
    $("sessions-grid").appendChild(card);
  });
  if (!currentSession && sessions.length) await openSession(sessions[0].id, false);
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
  const sessionBtn = [...$("session-list").children].find(x => x.classList.contains("active"));
  $("conversation-title").textContent = sessionBtn?.textContent || "Conversation";
  if (refresh) await loadSessions();
  $("messages").scrollTop = $("messages").scrollHeight;
}

function addMessage(m) {
  const row = document.createElement("div");
  row.className = "message " + m.role;
  const av = document.createElement("div");
  av.className = "message-avatar";
  av.textContent = m.role === "user" ? "S" : "✳";
  const bubble = document.createElement("div");
  bubble.className = "bubble";
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

  // Group 1: All Verified Working Models across all providers
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

  // Group per provider
  ["gemini", "openrouter", "codecraft"].forEach(p => {
    const list = providerState[p].models || [];
    if (!list.length) return;
    const grp = document.createElement("optgroup");
    grp.label = providerCatalog[p].name;
    // For OpenRouter, avoid adding 300+ untested paid models to dropdown; show working + free + top 25
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
      const nameEl = document.createElement("b");
      nameEl.textContent = m.display_name || m.id;
      const codeEl = document.createElement("small");
      codeEl.textContent = m.id;
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
        statusBadge.textContent = `✓ WORKING · ${m.latency_ms} ms`;
      } else if (m.status === "testing") {
        statusBadge.textContent = "⟳ TESTING…";
      } else if (m.status === "failed") {
        statusBadge.textContent = "✗ FAILED";
      } else {
        statusBadge.textContent = "CLICK TO TEST";
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
        const infoLine = [price, ctx, m.description].filter(Boolean).join(" · ");
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

// Save API Key & Load Models for Selected Provider (Does not close window!)
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

$("mobile-menu").onclick = () => $("sidebar").classList.add("open");
$("mobile-close").onclick = () => $("sidebar").classList.remove("open");

document.addEventListener("keydown", e => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    newSession();
  }
  if (e.key === "Escape") $("sidebar").classList.remove("open");
});

switchProviderTab("gemini");
checkAuth();
