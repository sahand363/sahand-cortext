const $ = id => document.getElementById(id);
let currentSession = null, toastTimer;
let cachedModels = [];
let activeModel = "gemini-2.0-flash";

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
  toastTimer = setTimeout(() => t.classList.remove("show"), 3200);
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
    if (s.active_model) setActiveModelUI(s.active_model);
    updateProvider(s.gemini_configured);
    await loadSessions();
    if (s.gemini_configured) {
      await loadModels(false);
    } else {
      $("setup-banner").classList.remove("hidden");
      showSetupNudge();
    }
  } catch {
    showLogin();
  }
}

function setActiveModelUI(modelId) {
  if (!modelId) return;
  activeModel = modelId;
  if ($("active-model-label")) $("active-model-label").textContent = modelId;
  const sel = $("model");
  if (sel) {
    if (![...sel.options].some(o => o.value === modelId)) {
      const opt = document.createElement("option");
      opt.value = modelId;
      opt.textContent = `✓ ${modelId} (Active)`;
      sel.prepend(opt);
    }
    sel.value = modelId;
  }
}

function updateProvider(connected) {
  $("header-provider-state").textContent = connected ? "CONNECTED" : "SETUP";
  $("header-provider-state").classList.toggle("connected", connected);
  $("models-gemini-status").textContent = connected ? "CONNECTED" : "NEEDS SETUP";
  $("models-gemini-status").classList.toggle("connected", connected);
  $("settings-provider-state").textContent = connected ? "Connected" : "Not connected";
  $("settings-provider-state").classList.toggle("connected", connected);
}

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
  setActiveModelUI($("model").value);
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
    const out = await api("/api/messages", {
      method: "POST",
      body: JSON.stringify({ session_id: currentSession, message, model: $("model").value })
    });
    $("messages").lastElementChild.remove();
    addMessage({ role: "assistant", content: out.reply });
    setActiveModelUI(out.model);
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
    loadModels(false);
  }
}

document.querySelectorAll(".nav-link").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.page)));
document.querySelectorAll("[data-goto]").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.goto)));
$("setup-open").onclick = () => switchPage("settings");

async function refreshStatus() {
  try {
    const s = await api("/api/status");
    updateProvider(s.gemini_configured);
    if (s.active_model) setActiveModelUI(s.active_model);
    if (s.gemini_configured) $("setup-banner").classList.add("hidden");
  } catch {}
}

// Populate Chat dropdown & interactive Model Cards in both Settings and Models pages
function syncModelDropdown() {
  const sel = $("model");
  if (!sel || !cachedModels.length) return;
  sel.innerHTML = "";
  cachedModels.forEach(m => {
    const opt = document.createElement("option");
    opt.value = m.id;
    const prefix = m.status === "working" ? "✓ " : m.status === "failed" ? "✗ " : "";
    opt.textContent = `${prefix}${m.display_name} (${m.id})`;
    sel.appendChild(opt);
  });
  if ([...sel.options].some(o => o.value === activeModel)) {
    sel.value = activeModel;
  } else if (cachedModels.length) {
    const firstWorking = cachedModels.find(m => m.status === "working") || cachedModels[0];
    setActiveModelUI(firstWorking.id);
  }
}

function renderModelGrids() {
  const targets = [
    { grid: $("settings-models-grid"), summary: $("settings-models-summary") },
    { grid: $("models-page-grid"), summary: $("models-page-summary") }
  ];

  const workingCount = cachedModels.filter(m => m.status === "working").length;
  const failedCount = cachedModels.filter(m => m.status === "failed").length;

  targets.forEach(({ grid, summary }) => {
    if (!grid) return;
    grid.innerHTML = "";
    if (!cachedModels.length) {
      if (summary) summary.textContent = "No models loaded yet. Save your Gemini API key in Settings to list all versions from Google.";
      return;
    }
    if (summary) {
      summary.innerHTML = `Google returned <b>${cachedModels.length}</b> models for your API key · <span class="text-ok"><b>${workingCount}</b> verified working</span> · <span class="text-err"><b>${failedCount}</b> restricted/failed</span>. <b>Click any model below to test it live & set it as active:</b>`;
    }

    cachedModels.forEach(m => {
      const card = document.createElement("button");
      card.type = "button";
      const isCurrent = m.id === activeModel;
      card.className = `api-model-card status-${m.status || "untested"}${isCurrent ? " is-active" : ""}`;
      card.dataset.modelId = m.id;

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

      const meta = document.createElement("div");
      meta.className = "api-model-meta";
      if (m.status === "failed" && m.last_error) {
        meta.textContent = m.last_error;
        meta.classList.add("err-text");
      } else if (m.status === "working") {
        meta.textContent = `Verified working with your API key (${m.latency_ms} ms). Click to re-test or select for chat.`;
        meta.classList.add("ok-text");
      } else {
        meta.textContent = m.description || `Input limit: ${(m.input_limit || 0).toLocaleString()} tokens · Click to test live response`;
      }

      card.append(top, meta);
      card.onclick = () => testSingleModel(m.id, true);
      grid.appendChild(card);
    });
  });
}

async function loadModels(showToastMsg = false) {
  try {
    const res = await api("/api/models");
    if (res.active_model) setActiveModelUI(res.active_model);
    cachedModels = res.models || [];
    syncModelDropdown();
    renderModelGrids();
    if (showToastMsg) toast(`Loaded ${cachedModels.length} models from Google API`);
  } catch (err) {
    if (showToastMsg) toast("Failed to load models: " + err.message);
  }
}

async function testSingleModel(modelId, setActive = true) {
  const item = cachedModels.find(x => x.id === modelId);
  if (item) {
    item.status = "testing";
    renderModelGrids();
  }
  try {
    const res = await api("/api/models/test", {
      method: "POST",
      body: JSON.stringify({ model: modelId, set_active: setActive })
    });
    if (item) {
      item.status = res.ok ? "working" : "failed";
      item.latency_ms = res.latency_ms || 0;
      item.last_error = res.error || "";
    }
    if (res.ok) {
      if (setActive) setActiveModelUI(modelId);
      toast(`✓ ${modelId} works! (${res.latency_ms} ms) — Set as active model`);
    } else {
      toast(`✗ ${modelId} rejected by Google`);
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

async function testAllModels() {
  if (!cachedModels.length) {
    await loadModels(false);
    if (!cachedModels.length) {
      toast("Save your Gemini API key first.");
      return;
    }
  }
  toast(`Testing all ${cachedModels.length} models sequentially…`);
  let foundFirstWorking = false;
  for (const m of cachedModels) {
    const shouldActivate = !foundFirstWorking;
    await testSingleModel(m.id, shouldActivate);
    const updated = cachedModels.find(x => x.id === m.id);
    if (updated && updated.status === "working" && !foundFirstWorking) {
      foundFirstWorking = true;
    }
  }
  const workingCount = cachedModels.filter(m => m.status === "working").length;
  toast(`Finished testing! ${workingCount} of ${cachedModels.length} models work with your key.`);
}

// Save API Key & Load Models (Does not close the page!)
$("api-key-form").addEventListener("submit", async e => {
  e.preventDefault();
  const key = $("api-key").value.trim();
  const fb = $("key-feedback");
  const btn = $("save-key-btn");
  btn.disabled = true;
  btn.textContent = "Saving & querying Google…";
  fb.className = "key-feedback";
  fb.textContent = "Connecting to Google API to save key and retrieve all available model versions…";
  try {
    const res = await api("/api/setup/gemini", {
      method: "POST",
      body: JSON.stringify({ api_key: key })
    });
    $("api-key").value = "";
    cachedModels = res.models || [];
    if (res.active_model) setActiveModelUI(res.active_model);
    syncModelDropdown();
    renderModelGrids();
    updateProvider(true);
    $("setup-banner").classList.add("hidden");
    fb.className = "key-feedback ok";
    fb.innerHTML = `✓ <b>API Key saved and verified (${res.latency_ms} ms)!</b> Google returned <b>${cachedModels.length}</b> model versions below. <b>Click any model card below to test if it responds!</b>`;
    toast(`Key saved! Found ${cachedModels.length} models.`);
  } catch (err) {
    fb.className = "key-feedback err";
    fb.innerHTML = `✗ <b>Could not verify API key:</b> ${err.message}`;
  } finally {
    btn.disabled = false;
    btn.innerHTML = 'Save Key & Load Models <span>↗</span>';
  }
});

$("reload-models-btn").onclick = () => loadModels(true);
$("refresh-models-page").onclick = () => loadModels(true);
$("test-all-models-btn").onclick = () => testAllModels();
$("test-all-models-page").onclick = () => testAllModels();

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

checkAuth();
