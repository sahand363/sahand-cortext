const $ = id => document.getElementById(id);
let currentSession = null, toastTimer, deleteConfirmTimer;
let sessionsCache = [];

let providersList = [];
let providerMap = {};
let selectedProvider = "";
let activeProvider = "";
let activeModel = "";
let freeOnlyFilter = true;
let editingCustomProviderId = null;
let deleteArmed = false;

function applyTheme(theme, persist = true) {
  const t = theme === "light" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", t);
  const meta = $("theme-color-meta");
  if (meta) meta.setAttribute("content", t === "light" ? "#ffffff" : "#0b0d12");

  const icon = t === "light" ? "☾" : "☀";
  const label = t === "light" ? "Switch to Dark Mode" : "Switch to GitHub Light";
  if ($("theme-toggle-btn")) {
    $("theme-toggle-btn").textContent = icon;
    $("theme-toggle-btn").title = label;
  }
  if ($("topbar-theme-btn")) {
    $("topbar-theme-btn").textContent = icon;
    $("topbar-theme-btn").title = label;
  }
  if ($("settings-theme-select")) {
    $("settings-theme-select").value = t;
  }
  if (persist) {
    try { localStorage.setItem("cortext_theme", t); } catch {}
  }
}

function toggleTheme() {
  const cur = document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
  applyTheme(cur === "light" ? "dark" : "light", true);
}

applyTheme(document.documentElement.getAttribute("data-theme") || "dark", false);

if ($("theme-toggle-btn")) $("theme-toggle-btn").onclick = toggleTheme;
if ($("topbar-theme-btn")) $("topbar-theme-btn").onclick = toggleTheme;
if ($("settings-theme-select")) {
  $("settings-theme-select").addEventListener("change", e => applyTheme(e.target.value, true));
}

function setMobileSidebarOpen(open) {
  $("sidebar")?.classList.toggle("open", open);
  $("sidebar-backdrop")?.classList.toggle("open", open);
}

if ($("mobile-menu")) $("mobile-menu").onclick = () => setMobileSidebarOpen(true);
if ($("mobile-close")) $("mobile-close").onclick = () => setMobileSidebarOpen(false);
if ($("sidebar-backdrop")) $("sidebar-backdrop").onclick = () => setMobileSidebarOpen(false);
document.addEventListener("keydown", e => {
  if (e.key === "Escape") setMobileSidebarOpen(false);
});

let orchParticipantsDraft = [
  { role: "coordinator", name: "Coordinator / Architect", provider: "gemini", model: "gemini-2.0-flash" },
  { role: "worker", name: "Worker / Developer", provider: "gemini", model: "gemini-2.0-flash" },
  { role: "auditor", name: "Quality Assurance / Auditor", provider: "gemini", model: "gemini-2.0-flash" }
];
let orchJobsCache = [];
let activeOrchJobId = localStorage.getItem("cortext_active_orch_job") || null;
let orchPollTimer = null;
let orchActiveSubView = "events"; // Default to Live Progress Timeline
let orchSubmitting = false;
let selectedDeliverableFileIdx = 0;

const contribPalette = [
  "#38bdf8",
  "#facc15",
  "#4ade80",
  "#a855f7",
  "#fb7185",
  "#fb923c",
  "#2dd4bf"
];

function getProviderMeta(id) {
  return providerMap[id] || {
    id: id || "unknown",
    name: id || "Unknown Provider",
    short_name: (id || "AI").toUpperCase(),
    icon: "⚡",
    description: "",
    key_label: "API Key",
    key_placeholder: "Paste API key…",
    built_in: false,
    requires_api_key: true,
    supports_free_badge: false,
    configured: false,
    models: []
  };
}

function hydrateProvidersFromRegistry(list) {
  if (!Array.isArray(list)) return;
  const prevModels = {};
  Object.keys(providerMap).forEach(k => {
    prevModels[k] = providerMap[k].models || [];
  });
  providersList = list.map(p => ({
    ...p,
    models: prevModels[p.id] || []
  }));
  providerMap = {};
  providersList.forEach(p => {
    providerMap[p.id] = p;
  });

  if (!selectedProvider || !providerMap[selectedProvider]) {
    selectedProvider = providersList[0]?.id || "";
  }
  renderProviderTabs();
}

function renderProviderTabs() {
  const containers = [
    { el: $("models-provider-tabs"), allowAdd: false },
    { el: $("settings-provider-tabs"), allowAdd: true }
  ];

  containers.forEach(({ el, allowAdd }) => {
    if (!el) return;
    el.innerHTML = "";
    providersList.forEach(p => {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "provider-tab" + (!editingCustomProviderId && p.id === selectedProvider ? " active" : "");
      btn.dataset.provider = p.id;

      const iconSpan = document.createElement("span");
      iconSpan.textContent = p.icon || "⚡";

      const nameNode = document.createTextNode(" " + p.name + " ");

      const dot = document.createElement("i");
      dot.className = "p-dot" + (p.configured ? " on" : "");

      btn.append(iconSpan, nameNode, dot);
      btn.onclick = () => {
        closeCustomProviderForm();
        switchProviderTab(p.id);
      };
      el.appendChild(btn);
    });

    if (allowAdd) {
      const addBtn = document.createElement("button");
      addBtn.type = "button";
      addBtn.className = "provider-tab" + (editingCustomProviderId === "__new__" ? " active" : "");
      addBtn.textContent = "＋ Add OpenAI-Compatible Provider";
      addBtn.onclick = () => openCustomProviderForm(null);
      el.appendChild(addBtn);
    }
  });
}

function openCustomProviderForm(existingMeta) {
  editingCustomProviderId = existingMeta ? existingMeta.id : "__new__";
  renderProviderTabs();
  $("existing-provider-config-wrap").classList.add("hidden");
  $("custom-provider-form").classList.remove("hidden");

  if (existingMeta) {
    $("custom-provider-form-title").textContent = `Edit Provider · ${existingMeta.name}`;
    if ($("cp-adapter-type")) {
      $("cp-adapter-type").value = existingMeta.adapter_type || "openai_compat";
      $("cp-adapter-type").disabled = true;
    }
    $("cp-name").value = existingMeta.name || "";
    $("cp-id").value = existingMeta.id || "";
    $("cp-id").disabled = true;
    $("cp-base-url").value = existingMeta.base_url || "";
    $("cp-auth-type").value = existingMeta.auth_type || "bearer";
    $("cp-auth-header").value = existingMeta.auth_header || "";
    $("cp-api-key").value = "";
    $("cp-manual-models").value = (existingMeta.manual_models || []).join(", ");
    $("cp-enable-discovery").checked = existingMeta.models_path !== "";
  } else {
    $("custom-provider-form-title").textContent = "Add Custom API Provider";
    if ($("cp-adapter-type")) {
      $("cp-adapter-type").value = "openai_compat";
      $("cp-adapter-type").disabled = false;
    }
    syncCustomProviderSpecDefaults();
    $("cp-name").value = "";
    $("cp-id").value = "";
    $("cp-id").disabled = false;
    $("cp-base-url").value = "";
    $("cp-auth-type").value = "bearer";
    $("cp-auth-header").value = "";
    $("cp-api-key").value = "";
    $("cp-manual-models").value = "";
    $("cp-enable-discovery").checked = true;
  }
  syncCustomProviderAuthFields();
}

function closeCustomProviderForm() {
  editingCustomProviderId = null;
  if ($("custom-provider-form")) $("custom-provider-form").classList.add("hidden");
  if ($("existing-provider-config-wrap")) $("existing-provider-config-wrap").classList.remove("hidden");
  renderProviderTabs();
}

function syncCustomProviderAuthFields() {
  const auth = $("cp-auth-type")?.value || "bearer";
  if ($("cp-auth-header-wrap")) $("cp-auth-header-wrap").classList.toggle("hidden", auth !== "header");
  if ($("cp-api-key-wrap")) $("cp-api-key-wrap").classList.toggle("hidden", auth === "none");
}

if ($("cp-auth-type")) {
  $("cp-auth-type").addEventListener("change", syncCustomProviderAuthFields);
}

function syncCustomProviderSpecDefaults() {
  const spec = $("cp-adapter-type")?.value || "openai_compat";
  const isAnt = spec === "anthropic_compat";
  if ($("cp-base-url") && !$("cp-base-url").dataset.touched) {
    $("cp-base-url").placeholder = isAnt
      ? "https://api.anthropic.com/v1 (or Anthropic-compatible proxy)"
      : "https://api.openai.com/v1 (or Groq, Ollama, LM Studio…)";
  }
  if (editingCustomProviderId === "__new__" && $("cp-auth-type")) {
    $("cp-auth-type").value = isAnt ? "header" : "bearer";
    if ($("cp-auth-header") && isAnt && !$("cp-auth-header").value) {
      $("cp-auth-header").value = "x-api-key";
    }
    syncCustomProviderAuthFields();
  }
}

if ($("cp-adapter-type")) {
  $("cp-adapter-type").addEventListener("change", syncCustomProviderSpecDefaults);
}
if ($("cp-name")) {
  $("cp-name").addEventListener("input", () => {
    if (editingCustomProviderId === "__new__" && !$("cp-id").dataset.touched) {
      $("cp-id").value = $("cp-name").value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 36);
    }
  });
}
if ($("cp-id")) {
  $("cp-id").addEventListener("input", () => { $("cp-id").dataset.touched = "1"; });
}
if ($("cancel-custom-provider-btn")) {
  $("cancel-custom-provider-btn").onclick = () => {
    closeCustomProviderForm();
    if (selectedProvider) switchProviderTab(selectedProvider);
  };
}

if ($("custom-provider-form")) {
  $("custom-provider-form").addEventListener("submit", async e => {
    e.preventDefault();
    const btn = $("save-custom-provider-btn");
    btn.disabled = true;
    btn.textContent = "Saving & Discovering Models…";

    const manualModels = ($("cp-manual-models").value || "")
      .split(",")
      .map(s => s.trim())
      .filter(Boolean);

    try {
      const res = await api("/api/providers", {
        method: "POST",
        body: JSON.stringify({
          adapter_type: $("cp-adapter-type").value,
          id: $("cp-id").value.trim().toLowerCase(),
          name: $("cp-name").value.trim(),
          base_url: $("cp-base-url").value.trim(),
          auth_type: $("cp-auth-type").value,
          auth_header: $("cp-auth-header").value.trim(),
          api_key: $("cp-api-key").value.trim(),
          disable_discovery: !$("cp-enable-discovery").checked,
          manual_models: manualModels
        })
      });

      hydrateProvidersFromRegistry(res.providers || []);
      const newId = res.provider.id;
      if (providerMap[newId]) {
        providerMap[newId].models = res.models || [];
      }
      if (res.active_provider && res.active_model) {
        setActiveSelectionUI(res.active_provider, res.active_model);
      }
      closeCustomProviderForm();
      switchProviderTab(newId);
      syncModelDropdown();
      syncAuditorModelDropdown();
      ensureDefaultOrchParticipants();
      renderOrchParticipantsBuilder();
      $("setup-banner").classList.add("hidden");
      toast(`✓ Registered ${res.provider.name} (${(res.models || []).length} models)`);
    } catch (err) {
      toast("Provider error: " + err.message);
    } finally {
      btn.disabled = false;
      btn.innerHTML = 'Save & Register Provider <span>↗</span>';
    }
  });
}

if ($("edit-custom-provider-btn")) {
  $("edit-custom-provider-btn").onclick = () => {
    const meta = getProviderMeta(selectedProvider);
    if (!meta.built_in) openCustomProviderForm(meta);
  };
}

if ($("delete-custom-provider-btn")) {
  $("delete-custom-provider-btn").onclick = async () => {
    const meta = getProviderMeta(selectedProvider);
    if (meta.built_in) return;
    try {
      const res = await api(`/api/providers/${encodeURIComponent(meta.id)}`, { method: "DELETE" });
      hydrateProvidersFromRegistry(res.providers || []);
      if (res.active_provider && res.active_model) {
        setActiveSelectionUI(res.active_provider, res.active_model);
      }
      if (selectedProvider) switchProviderTab(selectedProvider);
      syncModelDropdown();
      syncAuditorModelDropdown();
      renderOrchParticipantsBuilder();
      toast(`🗑 Removed ${meta.name}`);
    } catch (err) {
      toast("Delete failed: " + err.message);
    }
  };
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
  if (Array.isArray(s.providers_list)) {
    hydrateProvidersFromRegistry(s.providers_list);
  } else if (s.providers) {
    Object.keys(s.providers).forEach(pid => {
      if (providerMap[pid]) providerMap[pid].configured = !!s.providers[pid];
    });
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
  const info = getProviderMeta(provider);
  if ($("active-provider-label")) $("active-provider-label").textContent = info.name;
  if ($("active-model-label")) $("active-model-label").textContent = modelId;
  if ($("header-provider-name")) $("header-provider-name").textContent = info.short_name;

  const sel = $("model");
  const composite = `${provider}::${modelId}`;
  if (sel) {
    if (![...sel.options].some(o => o.value === composite)) {
      const opt = document.createElement("option");
      opt.value = composite;
      opt.textContent = `✓ [${info.short_name}] ${modelId}`;
      sel.prepend(opt);
    }
    sel.value = composite;
  }
}

function updateProviderBadges() {
  const anyConn = providersList.some(p => p.configured);
  $("header-provider-state").textContent = anyConn ? "CONNECTED" : "SETUP";
  $("header-provider-state").classList.toggle("connected", anyConn);
  renderProviderTabs();

  const cur = getProviderMeta(selectedProvider);
  const curConn = !!cur.configured;
  $("models-provider-status").textContent = curConn ? "CONNECTED" : "NEEDS SETUP";
  $("models-provider-status").classList.toggle("connected", curConn);
  $("settings-provider-state").textContent = curConn ? "Connected" : "Not connected";
  $("settings-provider-state").classList.toggle("connected", curConn);
}

function switchProviderTab(provider) {
  if (!provider && providersList.length > 0) provider = providersList[0].id;
  selectedProvider = provider;
  const meta = getProviderMeta(provider);

  $("settings-provider-title").textContent = `${meta.name} Connection`;
  $("settings-provider-desc").textContent = meta.description || `Configure ${meta.name} (${meta.base_url || ""})`;

  const needsKey = meta.requires_api_key !== false && meta.auth_type !== "none";
  $("api-key-input-section").classList.toggle("hidden", !needsKey);
  $("api-key-label").textContent = meta.key_label || "API Key";
  $("api-key").placeholder = meta.key_placeholder || "Paste API key…";
  $("api-key").value = "";
  $("save-key-btn").textContent = needsKey ? "Save Key & Load Models ↗" : "Connect & Load Models ↗";

  $("edit-custom-provider-btn")?.classList.toggle("hidden", !!meta.built_in);
  $("delete-custom-provider-btn")?.classList.toggle("hidden", !!meta.built_in);

  const specBadge = $("settings-provider-spec-badge");
  if (specBadge) {
    if (meta.built_in) {
      specBadge.classList.add("hidden");
    } else {
      specBadge.classList.remove("hidden");
      specBadge.textContent = meta.adapter_type === "anthropic_compat" ? "Anthropic Spec" : "OpenAI Spec";
    }
  }

  $("settings-explorer-title").textContent = `Available Models · ${meta.name}`;
  const hasHelp = Boolean(meta.help_url);
  $("provider-help-row").classList.toggle("hidden", !hasHelp);
  if (hasHelp) {
    $("provider-help-text").textContent = meta.help_text || "Manage API keys:";
    $("provider-help-link").href = meta.help_url;
    $("provider-help-link").textContent = meta.help_button || "Open Provider Console ↗";
  }

  if ($("custom-model-id")) {
    $("custom-model-id").placeholder = meta.default_model
      ? `Enter ${meta.name} model ID (e.g. ${meta.default_model})`
      : `Enter custom ${meta.name} model ID`;
  }

  $("models-feature-logo").textContent = meta.icon || "⚡";
  $("models-feature-title").textContent = meta.name;
  $("models-feature-desc").textContent = meta.description || "";

  const showFreeToggle = !!meta.supports_free_badge;
  $("settings-free-filter-wrap").classList.toggle("hidden", !showFreeToggle);
  $("models-free-filter-wrap").classList.toggle("hidden", !showFreeToggle);
  $("settings-free-only").checked = freeOnlyFilter;
  $("models-free-only").checked = freeOnlyFilter;

  updateProviderBadges();

  if (meta.configured && (!meta.models || !meta.models.length)) {
    loadProviderModels(provider, false);
  } else {
    renderModelGrids();
  }
}

$("settings-free-only").addEventListener("change", e => {
  freeOnlyFilter = e.target.checked;
  $("models-free-only").checked = freeOnlyFilter;
  renderModelGrids();
});

$("models-free-only").addEventListener("change", e => {
  freeOnlyFilter = e.target.checked;
  $("settings-free-only").checked = freeOnlyFilter;
  renderModelGrids();
});

async function loadAllConfiguredModels() {
  for (const p of providersList) {
    if (p.configured) {
      await loadProviderModels(p.id, false);
    }
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  ensureDefaultOrchParticipants();
}

async function loadProviderModels(provider, showToastMsg = false) {
  if (!provider) return;
  try {
    const res = await api("/api/models?provider=" + encodeURIComponent(provider));
    if (providerMap[provider]) {
      providerMap[provider].configured = !!res.configured;
      providerMap[provider].models = res.models || [];
    }
    if (res.active_provider && res.active_model) {
      setActiveSelectionUI(res.active_provider, res.active_model);
    }
    updateProviderBadges();
    syncModelDropdown();
    syncAuditorModelDropdown();
    renderModelGrids();
    if (showToastMsg) {
      toast(`Loaded ${(providerMap[provider]?.models || []).length} models from ${getProviderMeta(provider).name}`);
    }
  } catch (err) {
    if (showToastMsg) toast(`Failed to load ${getProviderMeta(provider).name} models: ` + err.message);
  }
}

function buildUnifiedModelOptions(sel) {
  if (!sel) return;
  const prevVal = sel.value;
  sel.innerHTML = "";

  const workingGroup = document.createElement("optgroup");
  workingGroup.label = "★ Verified & Enabled Working Models";
  let hasWorking = false;

  providersList.forEach(p => {
    const short = p.short_name || p.id.toUpperCase();
    (p.models || []).filter(m => m.status === "working" && m.enabled !== false).forEach(m => {
      hasWorking = true;
      const opt = document.createElement("option");
      opt.value = `${p.id}::${m.id}`;
      opt.textContent = `✓ [${short}] ${m.display_name} (${m.latency_ms} ms)`;
      workingGroup.appendChild(opt);
    });
  });
  if (hasWorking) sel.appendChild(workingGroup);

  providersList.forEach(p => {
    const list = (p.models || []).filter(m => m.enabled !== false);
    if (!list.length) return;
    const grp = document.createElement("optgroup");
    grp.label = p.name;
    const filtered = p.supports_free_badge ? list.filter((m, idx) => m.status === "working" || m.is_free || idx < 25) : list;
    filtered.forEach(m => {
      const opt = document.createElement("option");
      opt.value = `${p.id}::${m.id}`;
      const prefix = m.status === "working" ? "✓ " : m.status === "failed" ? "✗ " : "";
      const freeTag = (p.supports_free_badge && m.is_free) ? " [FREE]" : "";
      opt.textContent = `${prefix}[${p.short_name}] ${m.display_name}${freeTag} (${m.id})`;
      grp.appendChild(opt);
    });
    sel.appendChild(grp);
  });

  if (prevVal && [...sel.options].some(o => o.value === prevVal)) {
    sel.value = prevVal;
  }
}

function getDisplayedModels(provider) {
  const meta = getProviderMeta(provider);
  const all = meta.models || [];
  if (meta.supports_free_badge && freeOnlyFilter) {
    return all.filter(m => m.is_free || m.status === "working");
  }
  return all;
}

function getConfiguredProvidersList() {
  return providersList.filter(p => p.configured).map(p => p.id);
}

function getDefaultModelForProvider(p) {
  const meta = getProviderMeta(p);
  const models = (meta.models || []).filter(m => m.enabled !== false);
  const working = models.find(m => m.status === "working");
  if (working) return working.id;
  if (meta.default_model) return meta.default_model;
  if (meta.supports_free_badge) {
    const free = models.find(m => m.is_free);
    if (free) return free.id;
  }
  if (models.length > 0) return models[0].id;
  return "";
}

function ensureDefaultOrchParticipants() {
  const configured = getConfiguredProvidersList();
  const fallbackProv = configured[0] || activeProvider || providersList[0]?.id || "";
  const fallbackMod = getDefaultModelForProvider(fallbackProv) || activeModel || "";

  orchParticipantsDraft.forEach(p => {
    if ((!p.provider || !providerMap[p.provider]?.configured) && configured.length > 0) {
      p.provider = fallbackProv;
      p.model = fallbackMod;
    } else if (!providerMap[p.provider]) {
      p.provider = fallbackProv;
      p.model = fallbackMod;
    } else if (!p.model) {
      p.model = getDefaultModelForProvider(p.provider) || fallbackMod;
    }
  });
}

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

async function copyTextToClipboard(text) {
  if (!text) return false;
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {}
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-9999px";
  ta.style.left = "-9999px";
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  ta.setSelectionRange(0, text.length);
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {}
  document.body.removeChild(ta);
  return ok;
}

function stripRevOptsTag(text) {
  const s = String(text || "").trim();
  if (s.startsWith("[REV_OPTS:")) {
    const endIdx = s.indexOf("]");
    if (endIdx !== -1) {
      return s.slice(endIdx + 1).trim();
    }
  }
  return s;
}

/* ============================================================================
   MULTI-FILE PROJECT PARSER & PURE-JS ZIP ARCHIVE GENERATOR
   ============================================================================ */

function detectCodeLanguageAndFilename(code, hintLang, index) {
  const trimmed = (code || "").trim();
  const firstLines = trimmed.slice(0, 300);
  const h = (hintLang || "").toLowerCase().trim();

  if (h.includes(".") && /^[a-z0-9_./-]+$/i.test(h)) {
    const ext = h.split(".").pop().toLowerCase();
    return {
      filename: h.replace(/^\/+/, ""),
      ext,
      isHtml: ext === "html" || ext === "htm"
    };
  }

  if (h === "go" || h === "golang" || /^package\s+[a-zA-Z0-9_]+/m.test(firstLines)) {
    return { filename: index === 0 ? "main.go" : `file_${index + 1}.go`, ext: "go", isHtml: false };
  }
  if (firstLines.startsWith("module ") && firstLines.includes("\ngo ")) {
    return { filename: "go.mod", ext: "mod", isHtml: false };
  }
  if (h === "sh" || h === "bash" || h === "shell" || firstLines.startsWith("#!/usr/bin/env bash") || firstLines.startsWith("#!/bin/bash") || firstLines.startsWith("#!/bin/sh")) {
    return { filename: index === 0 ? "install.sh" : `script_${index + 1}.sh`, ext: "sh", isHtml: false };
  }
  if (h === "py" || h === "python" || firstLines.startsWith("#!/usr/bin/env python") || /^(import\s+\w+|from\s+\w+\s+import\s+|def\s+\w+\s*\()/m.test(firstLines)) {
    return { filename: index === 0 ? "main.py" : `module_${index + 1}.py`, ext: "py", isHtml: false };
  }
  if (h === "html" || /^<!doctype\s+html/i.test(firstLines) || /^<html[\s>]/i.test(firstLines)) {
    return { filename: index === 0 ? "index.html" : `page_${index + 1}.html`, ext: "html", isHtml: true };
  }
  if (h === "css") return { filename: index === 0 ? "style.css" : `style_${index + 1}.css`, ext: "css", isHtml: false };
  if (h === "js" || h === "javascript") return { filename: index === 0 ? "app.js" : `script_${index + 1}.js`, ext: "js", isHtml: false };
  if (h === "ts" || h === "typescript") return { filename: index === 0 ? "index.ts" : `module_${index + 1}.ts`, ext: "ts", isHtml: false };
  if (h === "json" || (firstLines.startsWith("{") && trimmed.endsWith("}"))) {
    return { filename: index === 0 ? "config.json" : `data_${index + 1}.json`, ext: "json", isHtml: false };
  }
  if (h === "yaml" || h === "yml") return { filename: index === 0 ? "config.yml" : `file_${index + 1}.yml`, ext: "yml", isHtml: false };
  if (h === "sql") return { filename: index === 0 ? "schema.sql" : `query_${index + 1}.sql`, ext: "sql", isHtml: false };
  if (h === "md" || h === "markdown") return { filename: index === 0 ? "README.md" : `doc_${index + 1}.md`, ext: "md", isHtml: false };

  return { filename: index === 0 ? "deliverable.txt" : `part_${index + 1}.txt`, ext: "txt", isHtml: false };
}

function stripOuterCodeFence(text) {
  const t = (text || "").trim();
  const m = t.match(/^```[a-zA-Z0-9_./-]*\s*\n([\s\S]*?)\n```\s*$/);
  return m ? m[1].trim() : t;
}

function unwrapEnvelopeIfPresent(raw) {
  let s = stripOuterCodeFence(raw);
  if (!s.startsWith("{")) return { text: s, extraFiles: [] };

  try {
    const parsed = JSON.parse(s);
    if (parsed && typeof parsed === "object") {
      if (Array.isArray(parsed.files)) {
        return { text: parsed.message || "", extraFiles: parsed.files };
      }
      if (parsed.files && typeof parsed.files === "object") {
        const list = Object.entries(parsed.files).map(([k, v]) => ({ filename: k, content: String(v) }));
        return { text: parsed.message || "", extraFiles: list };
      }
      if (typeof parsed.message === "string" && parsed.message.trim()) {
        return { text: parsed.message.trim(), extraFiles: [] };
      }
      if (typeof parsed.content === "string" && parsed.content.trim()) {
        return { text: parsed.content.trim(), extraFiles: [] };
      }
    }
  } catch {}

  const msgMatch = s.match(/"message"\s*:\s*"([\s\S]*)"\s*,\s*"completion_summary"/i) ||
                   s.match(/"message"\s*:\s*"([\s\S]*)"\s*\}/i);
  if (msgMatch && msgMatch[1]) {
    let extracted = msgMatch[1];
    extracted = extracted
      .replace(/\\n/g, "\n")
      .replace(/\\r/g, "\r")
      .replace(/\\t/g, "\t")
      .replace(/\\"/g, '"')
      .replace(/\\\\/g, "\\");
    return { text: extracted.trim(), extraFiles: [] };
  }

  if (s.includes("\\n=== FILE:") || s.includes("\\n```") || s.includes("\\npackage main")) {
    s = s
      .replace(/\\n/g, "\n")
      .replace(/\\r/g, "\r")
      .replace(/\\t/g, "\t")
      .replace(/\\"/g, '"')
      .replace(/\\\\/g, "\\");
  }

  return { text: s, extraFiles: [] };
}

function extractProjectFiles(rawText, jobId) {
  if (!rawText) return { files: [], summary: "" };

  let body = rawText;
  let summary = "";
  const sumMarker = "\n---\n### Coordinator Completion Summary\n";
  const sumIdx = body.indexOf(sumMarker);
  if (sumIdx !== -1) {
    summary = body.slice(sumIdx + sumMarker.length).trim();
    body = body.slice(0, sumIdx).trim();
  }

  const unwrapped = unwrapEnvelopeIfPresent(body);
  body = unwrapped.text;

  const files = [];
  const usedNames = new Set();

  function addUniqueFile(filename, content, ext, isHtml) {
    let cleanName = (filename || "file.txt")
      .trim()
      .replace(/^[/\\]+/, "")
      .replace(/\.\./g, "")
      .replace(/^[`*'"]+|[`*'"]+$/g, "");
    if (!cleanName) cleanName = "file.txt";
    if (usedNames.has(cleanName)) {
      const dot = cleanName.lastIndexOf(".");
      if (dot !== -1) {
        cleanName = `${cleanName.slice(0, dot)}_${files.length + 1}${cleanName.slice(dot)}`;
      } else {
        cleanName = `${cleanName}_${files.length + 1}`;
      }
    }
    usedNames.add(cleanName);
    const finalExt = ext || (cleanName.includes(".") ? cleanName.split(".").pop().toLowerCase() : "txt");
    files.push({
      filename: cleanName,
      content: String(content || "").trim() + "\n",
      ext: finalExt,
      isHtml: isHtml || finalExt === "html" || finalExt === "htm"
    });
  }

  if (unwrapped.extraFiles && unwrapped.extraFiles.length > 0) {
    unwrapped.extraFiles.forEach((ef, i) => {
      const fname = ef.filename || ef.name || ef.path || `file_${i + 1}.txt`;
      const code = ef.content || ef.code || "";
      if (code) {
        const info = detectCodeLanguageAndFilename(code, fname, i);
        addUniqueFile(fname, code, info.ext, info.isHtml);
      }
    });
  }

  if (files.length === 0) {
    const markerRe = /(?:^|\n)===\s*FILE:\s*([^\n=]+?)\s*===[ \t]*\n/gi;
    const matches = [...body.matchAll(markerRe)];
    if (matches.length > 0) {
      for (let i = 0; i < matches.length; i++) {
        const fname = matches[i][1].trim();
        const start = matches[i].index + matches[i][0].length;
        const end = i + 1 < matches.length ? matches[i + 1].index : body.length;
        const chunk = stripOuterCodeFence(body.slice(start, end));
        if (chunk) {
          const info = detectCodeLanguageAndFilename(chunk, fname, i);
          addUniqueFile(fname, chunk, info.ext, info.isHtml);
        }
      }
    }
  }

  if (files.length === 0) {
    const fenceRe = /```([a-zA-Z0-9_./-]*)[ \t]*\n([\s\S]*?)```/g;
    const fenceMatches = [...body.matchAll(fenceRe)];
    if (fenceMatches.length > 0) {
      fenceMatches.forEach((m, idx) => {
        const hint = (m[1] || "").trim();
        const code = (m[2] || "").trim();
        if (!code) return;

        const before = body.slice(Math.max(0, m.index - 200), m.index);
        const nameMatch = before.match(/(?:###|\*\*|`|File:|filename:)\s*(?:\d+\.\s*)?`?([a-zA-Z0-9_./-]+\.[a-zA-Z0-9]{1,6})`?[:\s*]*$/i);
        const fileHint = nameMatch ? nameMatch[1] : hint;

        const info = detectCodeLanguageAndFilename(code, fileHint, idx);
        addUniqueFile(info.filename, code, info.ext, info.isHtml);
      });

      const withoutFences = body.replace(fenceRe, "").trim();
      if (withoutFences.length > 80 && !usedNames.has("README.md")) {
        addUniqueFile("README.md", body, "md", false);
      }
    }
  }

  if (files.length === 0) {
    const info = detectCodeLanguageAndFilename(body, "", 0);
    addUniqueFile(info.filename, body, info.ext, info.isHtml);
  }

  const hasGo = files.some(f => f.ext === "go");
  const hasInstallSh = files.some(f => f.filename.endsWith(".sh"));
  if (hasGo && !hasInstallSh && files.length === 1) {
    const goFile = files.find(f => f.ext === "go").filename;
    const runScript = `#!/usr/bin/env bash
set -e
echo "Starting ${goFile}..."
if ! command -v go >/dev/null 2>&1; then
  echo "Installing Go..."
  sudo apt-get update && sudo apt-get install -y golang-go
fi
go run ${goFile}
`;
    addUniqueFile("run.sh", runScript, "sh", false);
  }

  return { files, summary };
}

function downloadSingleFileObj(fileObj) {
  if (!fileObj) return;
  const blob = new Blob([fileObj.content], { type: "application/octet-stream" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = fileObj.filename.split("/").pop();
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 1500);
  toast(`⬇ Downloaded ${fileObj.filename}`);
}

const crc32Table = (() => {
  const table = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let k = 0; k < 8; k++) {
      c = (c & 1) ? (0xEDB88320 ^ (c >>> 1)) : (c >>> 1);
    }
    table[i] = c >>> 0;
  }
  return table;
})();

function crc32Bytes(uint8Arr) {
  let crc = 0xFFFFFFFF;
  for (let i = 0; i < uint8Arr.length; i++) {
    crc = crc32Table[(crc ^ uint8Arr[i]) & 0xFF] ^ (crc >>> 8);
  }
  return (crc ^ 0xFFFFFFFF) >>> 0;
}

function buildZipBlob(files) {
  const encoder = new TextEncoder();
  const localChunks = [];
  const centralChunks = [];
  let offset = 0;

  files.forEach(f => {
    const nameBytes = encoder.encode(f.filename);
    const dataBytes = encoder.encode(f.content);
    const crc = crc32Bytes(dataBytes);
    const size = dataBytes.length;

    const localHeader = new Uint8Array(30 + nameBytes.length);
    const lv = new DataView(localHeader.buffer);
    lv.setUint32(0, 0x04034b50, true);
    lv.setUint16(4, 20, true);
    lv.setUint16(6, 0x0800, true);
    lv.setUint16(8, 0, true);
    lv.setUint16(10, 0, true);
    lv.setUint16(12, 0, true);
    lv.setUint32(14, crc, true);
    lv.setUint32(18, size, true);
    lv.setUint32(22, size, true);
    lv.setUint16(26, nameBytes.length, true);
    lv.setUint16(28, 0, true);
    localHeader.set(nameBytes, 30);

    localChunks.push(localHeader, dataBytes);

    const centralHeader = new Uint8Array(46 + nameBytes.length);
    const cv = new DataView(centralHeader.buffer);
    cv.setUint32(0, 0x02014b50, true);
    cv.setUint16(4, 20, true);
    cv.setUint16(6, 20, true);
    cv.setUint16(8, 0x0800, true);
    cv.setUint16(10, 0, true);
    cv.setUint16(12, 0, true);
    cv.setUint16(14, 0, true);
    cv.setUint32(16, crc, true);
    cv.setUint32(20, size, true);
    cv.setUint32(24, size, true);
    cv.setUint16(28, nameBytes.length, true);
    cv.setUint16(30, 0, true);
    cv.setUint16(32, 0, true);
    cv.setUint16(34, 0, true);
    cv.setUint16(36, 0, true);
    cv.setUint32(38, 0, true);
    cv.setUint32(42, offset, true);
    centralHeader.set(nameBytes, 46);

    centralChunks.push(centralHeader);
    offset += localHeader.length + size;
  });

  const centralSize = centralChunks.reduce((acc, c) => acc + c.length, 0);
  const eocd = new Uint8Array(22);
  const ev = new DataView(eocd.buffer);
  ev.setUint32(0, 0x06054b50, true);
  ev.setUint16(4, 0, true);
  ev.setUint16(6, 0, true);
  ev.setUint16(8, files.length, true);
  ev.setUint16(10, files.length, true);
  ev.setUint32(12, centralSize, true);
  ev.setUint32(16, offset, true);
  ev.setUint16(20, 0, true);

  return new Blob([...localChunks, ...centralChunks, eocd], { type: "application/zip" });
}

function downloadProjectZip(files, jobId) {
  if (!files || !files.length) return;
  const zipBlob = buildZipBlob(files);
  const url = URL.createObjectURL(zipBlob);
  const a = document.createElement("a");
  const cleanId = (jobId || "project").replace(/[^a-zA-Z0-9_-]/g, "");
  a.href = url;
  a.download = `${cleanId}-project.zip`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 1500);
  toast(`📦 Downloaded ${cleanId}-project.zip (${files.length} files)`);
}

function showLogin() {
  $("login-screen").classList.remove("hidden");
  $("app").classList.add("hidden");
}

function showApp() {
  $("login-screen").classList.add("hidden");
  $("app").classList.remove("hidden");
}

document.querySelectorAll(".provider-tab").forEach(btn => {
  btn.addEventListener("click", () => switchProviderTab(btn.dataset.provider));
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
  setMobileSidebarOpen(false);

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

function syncModelDropdown() {
  const sel = $("model");
  if (!sel) return;
  buildUnifiedModelOptions(sel);

  const targetVal = `${activeProvider}::${activeModel}`;
  if ([...sel.options].some(o => o.value === targetVal)) {
    sel.value = targetVal;
  } else if (sel.options.length > 0) {
    const [p, m] = sel.options[0].value.split("::");
    setActiveSelectionUI(p, m);
  }
}

function syncAuditorModelDropdown() {
  const sel = $("orch-auditor-select");
  if (!sel) return;
  buildUnifiedModelOptions(sel);
  if (!sel.value && sel.options.length > 0) {
    const targetVal = `${activeProvider}::${activeModel}`;
    if ([...sel.options].some(o => o.value === targetVal)) {
      sel.value = targetVal;
    } else {
      sel.value = sel.options[0].value;
    }
  }
}

async function toggleModelEnabledState(provider, modelId, enabled) {
  const list = providerMap[provider]?.models || [];
  const item = list.find(x => x.id === modelId);
  if (item) {
    item.enabled = enabled;
  }
  try {
    await api("/api/models/toggle", {
      method: "POST",
      body: JSON.stringify({ provider, model: modelId, enabled })
    });
    toast(enabled
      ? `✓ Enabled ${modelId} for Chat, Orchestration & Failovers`
      : `⏸ Disabled ${modelId} (excluded from pickers & failovers)`);
  } catch (err) {
    toast("Could not update model state: " + err.message);
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  renderOrchParticipantsBuilder();
}

async function batchToggleShownModels(enabled) {
  const provider = selectedProvider;
  const shown = getDisplayedModels(provider);
  if (!shown.length) {
    toast(`No models loaded for ${getProviderMeta(provider).name} yet.`);
    return;
  }
  const ids = shown.map(m => m.id);
  shown.forEach(m => { m.enabled = enabled; });

  try {
    await api("/api/models/toggle", {
      method: "POST",
      body: JSON.stringify({ provider, models: ids, enabled })
    });
    toast(enabled
      ? `✓ Checked all ${ids.length} shown models on ${getProviderMeta(provider).name}`
      : `☐ Unchecked all ${ids.length} shown models on ${getProviderMeta(provider).name}`);
  } catch (err) {
    toast("Batch toggle failed: " + err.message);
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  renderOrchParticipantsBuilder();
}

function renderModelGrids() {
  const provider = selectedProvider;
  const meta = getProviderMeta(provider);
  const allModels = providerMap[provider]?.models || [];
  const shownModels = getDisplayedModels(provider);

  const workingCount = allModels.filter(m => m.status === "working" && m.enabled !== false).length;
  const disabledCount = allModels.filter(m => m.enabled === false).length;
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
      const freeNote = provider === "openrouter" ? ` · <b>${freeCount}</b> free` : "";
      const disNote = disabledCount > 0 ? ` · <b>${disabledCount}</b> unchecked/disabled` : "";
      summary.innerHTML = `<b>${meta.name}</b> returned <b>${allModels.length}</b> models${freeNote} (showing <b>${shownModels.length}</b>) · <span class="text-ok"><b>${workingCount}</b> active working</span> · <span class="text-err"><b>${failedCount}</b> failed</span>${disNote}. <b>Click card to test, or uncheck "Enabled" to bench a model:</b>`;
    }

    shownModels.forEach(m => {
      const card = document.createElement("div");
      const isCurrent = (provider === activeProvider && m.id === activeModel);
      const isEnabled = m.enabled !== false;
      card.className = `api-model-card status-${m.status || "untested"}${isCurrent ? " is-active" : ""}${!isEnabled ? " is-disabled" : ""}`;
      card.style.cursor = "pointer";

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

      const enableLabel = document.createElement("label");
      enableLabel.className = "model-enable-toggle";
      enableLabel.title = "Check to allow this model in Chat, Orchestration & Automatic Failovers. Uncheck to disable it.";
      enableLabel.onclick = e => e.stopPropagation();

      const enableChk = document.createElement("input");
      enableChk.type = "checkbox";
      enableChk.checked = isEnabled;
      enableChk.onchange = e => {
        e.stopPropagation();
        toggleModelEnabledState(provider, m.id, enableChk.checked);
      };
      const enableTxt = document.createElement("span");
      enableTxt.textContent = isEnabled ? "Enabled" : "Off";
      enableLabel.append(enableChk, enableTxt);
      badgeWrap.appendChild(enableLabel);

      if (isCurrent && isEnabled) {
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
      if (!isEnabled) {
        metaRow.textContent = "Disabled by you — hidden from Chat, Orchestration & Failover pools.";
      } else if (m.status === "failed" && m.last_error) {
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
  const list = providerMap[provider]?.models || [];
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
      if (setActive && (!item || item.enabled !== false)) {
        setActiveSelectionUI(provider, modelId);
      }
      toast(`✓ [${getProviderMeta(provider).short}] ${modelId} works! (${res.latency_ms} ms)`);
    } else {
      toast(`✗ ${modelId} failed on ${getProviderMeta(provider).short}`);
    }
  } catch (err) {
    if (item) {
      item.status = "failed";
      item.last_error = err.message;
    }
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  renderOrchParticipantsBuilder();
}

async function testShownModels() {
  const provider = selectedProvider;
  const shown = getDisplayedModels(provider).filter(m => m.enabled !== false);
  if (!shown.length) {
    toast(`No enabled models shown on ${getProviderMeta(provider).name}. Check the models you want to test first.`);
    return;
  }
  toast(`Testing ${shown.length} enabled models on ${getProviderMeta(provider).name}…`);
  let foundWorking = false;
  for (const m of shown) {
    const activate = !foundWorking;
    await testSingleModel(provider, m.id, activate);
    const updated = (providerMap[provider].models || []).find(x => x.id === m.id);
    if (updated && updated.status === "working" && !foundWorking) {
      foundWorking = true;
    }
  }
  const workingCount = (providerMap[provider].models || []).filter(m => m.status === "working" && m.enabled !== false).length;
  toast(`Done! ${workingCount} enabled working models found on ${getProviderMeta(provider).name}.`);
}

$("api-key-form").addEventListener("submit", async e => {
  e.preventDefault();
  const provider = selectedProvider;
  const meta = getProviderMeta(provider);
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
    providerMap[provider].configured = true;
    providerMap[provider].models = res.models || [];
    if (res.active_provider && res.active_model) {
      setActiveSelectionUI(res.active_provider, res.active_model);
    }
    updateProviderBadges();
    syncModelDropdown();
    syncAuditorModelDropdown();
    renderModelGrids();
    ensureDefaultOrchParticipants();
    $("setup-banner").classList.add("hidden");

    fb.className = "key-feedback ok";
    fb.innerHTML = `✓ <b>${meta.name} API key saved (${res.latency_ms} ms)!</b> Loaded <b>${providerMap[provider].models.length}</b> models below.`;
    toast(`${meta.name} connected! (${providerMap[provider].models.length} models)`);
  } catch (err) {
    fb.className = "key-feedback err";
    fb.textContent = "";
    const errBold = document.createElement("b");
    errBold.textContent = `✗ ${meta.name} error:`;
    fb.append(errBold, document.createTextNode(" " + err.message));
  } finally {
    btn.disabled = false;
    btn.innerHTML = 'Save Key & Load Models <span>↗</span>';
  }
});

if ($("custom-model-form")) {
  $("custom-model-form").addEventListener("submit", async e => {
    e.preventDefault();
    const provider = selectedProvider;
    const input = $("custom-model-id");
    const model = input.value.trim();
    if (!model) return;
    const btn = $("add-custom-model-btn");
    btn.disabled = true;
    btn.textContent = "Testing…";
    try {
      const res = await api("/api/models/custom", {
        method: "POST",
        body: JSON.stringify({ provider, model })
      });
      input.value = "";
      providerMap[provider].models = res.models || [];
      if (res.ok && res.active_provider && res.active_model) {
        setActiveSelectionUI(res.active_provider, res.active_model);
        toast(`✓ [${getProviderMeta(provider).short}] ${model} verified working (${res.latency_ms} ms)!`);
      } else {
        toast(`Added ${model}, test returned: ${res.error || "failed"}`);
      }
      syncModelDropdown();
      syncAuditorModelDropdown();
      renderModelGrids();
      renderOrchParticipantsBuilder();
    } catch (err) {
      toast(err.message);
    } finally {
      btn.disabled = false;
      btn.textContent = "＋ Add & Test";
    }
  });
}

$("reload-models-btn").onclick = () => loadProviderModels(selectedProvider, true);
$("refresh-models-page").onclick = () => loadProviderModels(selectedProvider, true);
$("test-all-models-btn").onclick = () => testShownModels();
$("test-all-models-page").onclick = () => testShownModels();
if ($("check-all-models-btn")) $("check-all-models-btn").onclick = () => batchToggleShownModels(true);
if ($("uncheck-all-models-btn")) $("uncheck-all-models-btn").onclick = () => batchToggleShownModels(false);
if ($("check-all-models-page")) $("check-all-models-page").onclick = () => batchToggleShownModels(true);
if ($("uncheck-all-models-page")) $("uncheck-all-models-page").onclick = () => batchToggleShownModels(false);

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

async function initOrchestrationPage() {
  ensureDefaultOrchParticipants();
  syncAuditorModelDropdown();
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

    const topBar = document.createElement("div");
    topBar.className = "orch-part-top";

    const stepBadge = document.createElement("span");
    stepBadge.className = "orch-step-num";
    stepBadge.textContent = `#${idx + 1}`;

    const roleSelect = document.createElement("select");
    roleSelect.className = "orch-role-select";
    [
      { value: "coordinator", label: "Coordinator / Architect (Plans & Approves)" },
      { value: "worker", label: "Worker / Developer (Builds & Revises)" },
      { value: "auditor", label: "Quality Assurance / Auditor (Tests & Audits)" }
    ].forEach(r => {
      const opt = document.createElement("option");
      opt.value = r.value;
      opt.textContent = r.label;
      if (part.role === r.value) opt.selected = true;
      roleSelect.appendChild(opt);
    });
    roleSelect.onchange = () => {
      part.role = roleSelect.value;
      if (roleSelect.value === "coordinator") {
        part.name = "Coordinator / Architect";
      } else if (roleSelect.value === "auditor") {
        part.name = "Quality Assurance / Auditor";
      } else {
        part.name = "Worker / Developer";
      }
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

    const gridRow = document.createElement("div");
    gridRow.className = "orch-part-grid";

    const provWrap = document.createElement("label");
    provWrap.innerHTML = `<span>Provider</span>`;
    const provSelect = document.createElement("select");
    providersList.forEach(provObj => {
      const opt = document.createElement("option");
      opt.value = provObj.id;
      opt.textContent = `${provObj.name}${provObj.configured ? " (Connected)" : " (Needs Setup)"}`;
      if (part.provider === provObj.id) opt.selected = true;
      provSelect.appendChild(opt);
    });

    const modWrap = document.createElement("label");
    modWrap.innerHTML = `<span>Model</span>`;
    const modSelect = document.createElement("select");

    function populateModelsForPart() {
      modSelect.innerHTML = "";
      const p = part.provider;
      const list = (providerMap[p]?.models || []).filter(m => m.enabled !== false);
      if (!list.length) {
        const fallbackOpt = document.createElement("option");
        fallbackOpt.value = part.model || "";
        fallbackOpt.textContent = part.model ? `${part.model} (Configure or enable models in Settings)` : "No enabled models — check Models & providers";
        modSelect.appendChild(fallbackOpt);
        return;
      }

      const working = list.filter(m => m.status === "working");
      const others = p === "openrouter"
        ? list.filter((m, i) => m.status !== "working" && (m.is_free || i < 40))
        : list.filter(m => m.status !== "working");

      if (working.length) {
        const grpW = document.createElement("optgroup");
        grpW.label = "★ Verified & Enabled Working";
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
        grpO.label = "Enabled Models";
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

document.querySelectorAll(".orch-view-tab").forEach(btn => {
  btn.onclick = () => {
    orchActiveSubView = btn.dataset.orchview;
    document.querySelectorAll(".orch-view-tab").forEach(b => b.classList.toggle("active", b === btn));
    $("orch-messages-view").classList.toggle("hidden", orchActiveSubView !== "messages");
    $("orch-events-view").classList.toggle("hidden", orchActiveSubView !== "events");
  };
});

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
    if (!providerMap[p.provider]?.configured) {
      errBanner.textContent = `Participant #${i + 1} uses ${getProviderMeta(p.provider).name || p.provider}, which does not have a saved API key yet. Configure it in Settings first.`;
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
  const maxCalls = parseInt($("orch-max-calls").value, 10) || 15;
  const autoRetry = $("orch-opt-autoretry") ? $("orch-opt-autoretry").checked : true;
  const switchParticipant = $("orch-opt-switch") ? $("orch-opt-switch").checked : true;
  const allowGlobalFallback = $("orch-opt-global-fallback") ? $("orch-opt-global-fallback").checked : true;

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
        auto_retry: autoRetry,
        switch_participant: switchParticipant,
        allow_global_fallback: allowGlobalFallback,
        participants: orchParticipantsDraft.map((p, idx) => ({
          role: p.role,
          name: p.role === "coordinator"
            ? "Coordinator / Architect"
            : p.role === "auditor"
              ? "Quality Assurance / Auditor"
              : "Worker / Developer",
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
    meta.textContent = `R${j.current_round || 0}/${j.max_rounds || 5} · ${j.total_calls || 0} msgs`;
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
  if (activeOrchJobId !== jobId) {
    selectedDeliverableFileIdx = 0;
  }
  activeOrchJobId = jobId;
  localStorage.setItem("cortext_active_orch_job", jobId);
  $("orch-builder-panel").classList.add("hidden");
  $("orch-monitor-panel").classList.remove("hidden");
  if ($("orch-preview-wrap")) $("orch-preview-wrap").classList.add("hidden");
  syncAuditorModelDropdown();
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

    const idx = orchJobsCache.findIndex(x => x.id === jobId);
    if (idx !== -1) {
      orchJobsCache[idx] = { ...orchJobsCache[idx], ...details };
      renderOrchJobsList();
    }

    if (details.status === "queued" || details.status === "running" || details.status === "waiting" || details.status === "pausing") {
      orchPollTimer = setTimeout(() => fetchAndRenderOrchJob(jobId), 1500);
    }
  } catch (err) {
    toast("Could not load orchestration job: " + err.message);
  }
}

function parseEventJSON(ev) {
  const raw = ev && (ev.payload || ev.payload_json);
  if (!raw) return {};
  if (typeof raw === "object") return raw;
  try {
    return JSON.parse(raw) || {};
  } catch {
    return {};
  }
}

function renderAIContributionBar(participants, messages, events) {
  const card = $("orch-contrib-card");
  const bar = $("orch-contrib-bar");
  const legend = $("orch-contrib-legend");
  if (!card || !bar || !legend) return;

  const failMap = {};
  events.forEach(ev => {
    if (ev.event_type === "participant_failed" || ev.event_type === "participant_fallback") {
      const p = parseEventJSON(ev);
      const prov = p.provider || p.from_provider || "";
      const mod = p.model || p.from_model || "";
      if (!prov && !mod) return;
      if (ev.event_type === "participant_fallback" && events.some(x => x.event_type === "participant_failed")) {
        return;
      }
      const key = `${prov}::${mod}`;
      if (!failMap[key]) failMap[key] = { count: 0, lastError: "", role: p.role || "worker", provider: prov, model: mod };
      failMap[key].count++;
      if (p.error) failMap[key].lastError = p.error;
    }
  });

  const modelStats = new Map();

  participants.forEach((p, idx) => {
    const key = `${p.provider}::${p.model}::${p.role}`;
    if (!modelStats.has(key)) {
      modelStats.set(key, {
        key,
        provider: p.provider,
        model: p.model,
        role: p.role,
        chars: 0,
        msgs: 0,
        color: contribPalette[idx % contribPalette.length]
      });
    }
  });

  messages.forEach(m => {
    if (m.sender_role === "user") return;
    const key = `${m.provider}::${m.model}::${m.sender_role}`;
    if (!modelStats.has(key)) {
      modelStats.set(key, {
        key,
        provider: m.provider,
        model: m.model,
        role: m.sender_role,
        chars: 0,
        msgs: 0,
        color: contribPalette[modelStats.size % contribPalette.length]
      });
    }
    const st = modelStats.get(key);
    st.chars += (m.content || "").length;
    st.msgs += 1;
  });

  const entries = [...modelStats.values()];
  const totalChars = entries.reduce((sum, e) => sum + e.chars, 0);

  if (entries.length === 0 && Object.keys(failMap).length === 0) {
    card.classList.add("hidden");
    return;
  }

  card.classList.remove("hidden");
  bar.innerHTML = "";
  legend.innerHTML = "";

  const totalKb = (totalChars / 1024).toFixed(1);
  if ($("orch-contrib-total-label")) {
    $("orch-contrib-total-label").textContent = totalChars > 0 ? `${totalKb} KB TOTAL AI OUTPUT` : "WAITING FOR OUTPUT";
  }

  entries.sort((a, b) => b.chars - a.chars);

  entries.forEach(e => {
    const pct = totalChars > 0 ? ((e.chars / totalChars) * 100) : 0;
    const pctStr = pct.toFixed(1) + "%";

    if (pct > 0) {
      const seg = document.createElement("div");
      seg.className = "orch-contrib-seg";
      seg.style.width = `${Math.max(2, pct)}%`;
      seg.style.background = e.color;
      seg.title = `${e.model} (${e.role}): ${pctStr} (${e.msgs} turns)`;
      bar.appendChild(seg);
    }

    const item = document.createElement("div");
    item.className = "orch-contrib-item";

    const dot = document.createElement("span");
    dot.className = "orch-contrib-dot";
    dot.style.background = e.chars > 0 ? e.color : "#ef4444";

    const roleShort = e.role === "coordinator" ? "Coord" : e.role === "auditor" ? "Auditor" : "Worker";
    const provShort = getProviderMeta(e.provider).short_name || e.provider;
    const label = document.createElement("span");
    label.innerHTML = `<b>${e.model}</b> <small style="color:#94a3b8">(${provShort} · ${roleShort})</small> <span style="color:#cbd5e1;font-weight:700">${pctStr}</span>`;

    item.append(dot, label);

    const failInfo = failMap[`${e.provider}::${e.model}`];
    if (failInfo && failInfo.count > 0) {
      const fp = document.createElement("span");
      fp.className = "orch-fail-pill";
      const shortErr = failInfo.lastError ? `: ${failInfo.lastError.slice(0, 48)}` : "";
      fp.textContent = `✗ Failed ${failInfo.count}x${shortErr}`;
      fp.title = failInfo.lastError || "Model invocation failed";
      item.appendChild(fp);
    }

    legend.appendChild(item);
  });
}

async function swapParticipantModelMidJob(jobId, participants, slotIdx, newCompositeValue) {
  if (!newCompositeValue || !newCompositeValue.includes("::")) return;
  const [newProv, newMod] = newCompositeValue.split("::");
  const updated = participants.map((p, i) => {
    if (i === slotIdx) {
      return { ...p, provider: newProv, model: newMod, sort_order: i };
    }
    return { ...p, sort_order: i };
  });

  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/participants`, {
      method: "POST",
      body: JSON.stringify({ participants: updated })
    });
    toast(`✓ Updated Participant #${slotIdx + 1} to ${getProviderMeta(newProv).short_name || newProv} · ${newMod}`);
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Could not swap participant model: " + err.message);
  }
}

function renderOrchJobDetails(d) {
  const status = (d.status || "queued").toLowerCase();
  const stage = (d.current_stage || status).toLowerCase();
  const messages = d.messages || [];
  const events = d.events || [];

  const statusEl = $("orch-detail-status");
  statusEl.className = `orch-status-badge status-${status}`;
  statusEl.textContent = status.toUpperCase();

  $("orch-detail-stage").textContent = `STAGE: ${stage.toUpperCase()}`;
  $("orch-detail-rounds").textContent = `Round ${d.current_round || 0} / ${d.max_rounds || 5}`;
  $("orch-detail-calls").textContent = `Messages ${messages.length} / ${d.max_calls || 15}`;
  $("orch-detail-task").textContent = d.task || "";
  const taskEl = $("orch-detail-task");
  const taskToggleBtn = $("orch-task-toggle-btn");
  const taskText = d.task || "";
  taskEl.textContent = taskText;
  if (taskToggleBtn) {
    const isLong = taskText.length > 240 || taskText.split("\n").length > 3;
    taskToggleBtn.classList.toggle("hidden", !isLong);
    taskEl.classList.add("is-clamped");
    taskToggleBtn.textContent = "Show full prompt ▾";
    taskToggleBtn.onclick = () => {
      const clamped = taskEl.classList.toggle("is-clamped");
      taskToggleBtn.textContent = clamped ? "Show full prompt ▾" : "Collapse prompt ▴";
    };
  }
  $("orch-detail-sub").textContent = `Job ID: ${d.id} · Created ${formatShortTimestamp(d.created_at)}${d.retry_count ? ` · Retries: ${d.retry_count}/${d.max_retries}` : ""}`;

  const coordMsgs = messages.filter(m => m.sender_role === "coordinator").length;
  const workerMsgs = messages.filter(m => m.sender_role === "worker").length;
  const auditorMsgs = messages.filter(m => m.sender_role === "auditor").length;
  const userRevs = messages.filter(m => m.sender_role === "user").length;
  const revisionsReq = messages.filter(m => (m.sender_role === "coordinator" || m.sender_role === "user" || m.sender_role === "auditor") && m.action === "revise").length;
  const failoversCount = events.filter(e => e.event_type === "participant_fallback" || e.event_type === "retry_scheduled" || e.event_type === "participant_failed").length;
  const totalLatencyMs = messages.reduce((sum, m) => sum + (m.latency_ms || 0), 0);

  if ($("orch-stat-msgs")) {
    $("orch-stat-msgs").textContent = String(messages.length);
    const partsSub = [`${coordMsgs} Coord`, `${workerMsgs} Worker`];
    if (auditorMsgs) partsSub.push(`${auditorMsgs} Audit`);
    if (userRevs) partsSub.push(`${userRevs} User`);
    $("orch-stat-msgs-sub").textContent = partsSub.join(" · ");
    $("orch-stat-revisions").textContent = String(revisionsReq);
    $("orch-stat-revisions-sub").textContent = `Rounds active: ${d.current_round || 0} of ${d.max_rounds || 5}`;
    $("orch-stat-failovers").textContent = String(failoversCount);
    $("orch-stat-failovers-sub").textContent = failoversCount > 0 ? "Failovers & model errors" : "No model failures";
    $("orch-stat-latency").textContent = `${(totalLatencyMs / 1000).toFixed(1)} s`;
    $("orch-stat-latency-sub").textContent = `${totalLatencyMs.toLocaleString()} ms total API time`;
  }

  const isPausing = status === "pausing";
  const isActive = status === "queued" || status === "running" || status === "waiting" || isPausing;
  const isPaused = status === "paused";
  const canRetryOrPlay = status === "failed" || status === "cancelled" || status === "waiting" || isPaused;

  const cancelBtn = $("orch-cancel-btn");
  cancelBtn.classList.toggle("hidden", !(isActive || isPaused));
  cancelBtn.disabled = false;
  cancelBtn.textContent = "⏹ Stop Now";
  cancelBtn.onclick = () => cancelActiveOrchJob(d.id);

  const pauseBtn = $("orch-pause-btn");
  if (pauseBtn) {
    pauseBtn.classList.toggle("hidden", !(status === "running" || isPausing));
    pauseBtn.disabled = isPausing;
    pauseBtn.textContent = isPausing ? "⏳ Pausing after current AI finishes…" : "⏸ Pause After Current Turn";
    pauseBtn.onclick = () => pauseActiveOrchJob(d.id);
  }

  const retryBtn = $("orch-retry-btn");
  if (retryBtn) {
    retryBtn.classList.toggle("hidden", !canRetryOrPlay);
    retryBtn.disabled = false;
    retryBtn.textContent = isPaused ? "▶ Play / Continue Task" : "↻ Retry / Resume Task";
    retryBtn.onclick = () => retryOrchJob(d.id);
  }

  const activeBanner = $("orch-active-banner");
  const participants = d.participants || [];
  const workersList = participants.filter(p => p.role === "worker");
  const curRound = Math.max(1, d.current_round || 1);

  const latestInvokeEvt = [...events].reverse().find(e => e.event_type === "model_invoked");
  const invokeMeta = parseEventJSON(latestInvokeEvt);

  const coordPart = participants.find(p => p.role === "coordinator") || participants[0];
  const scheduledWorker = workersList.length > 0 ? workersList[(curRound - 1) % workersList.length] : (participants[1] || coordPart);
  const auditorPart = participants.find(p => p.role === "auditor") || coordPart;

  if (isActive || isPaused) {
    activeBanner.classList.remove("hidden");
    const activeProvName = invokeMeta.provider ? (getProviderMeta(invokeMeta.provider).name || invokeMeta.provider) : "";
    const activeModelLabel = invokeMeta.model ? `${activeProvName} · ${invokeMeta.model}` : "";

    if (isPausing) {
      $("orch-active-headline").textContent = "⏳ Finishing current AI turn before pausing…";
      $("orch-active-subline").textContent = "Waiting for the active model to complete its response so no tokens are wasted. Play will unlock automatically.";
    } else if (isPaused) {
      $("orch-active-headline").textContent = "⏸ Collaboration Paused — Ready to Swap Models or Press Play";
      $("orch-active-subline").textContent = "You can change any upcoming AI model in the participant cards below (except the one that just answered), then click ▶ Play / Continue Task.";
    } else if (status === "queued") {
      $("orch-active-headline").textContent = "Task queued — waiting for background worker…";
      $("orch-active-subline").textContent = "A worker will claim this job automatically.";
    } else if (status === "waiting") {
      $("orch-active-headline").textContent = "Waiting to retry after temporary provider error…";
      $("orch-active-subline").textContent = d.error_message || "Scheduled for automatic retry.";
    } else if (stage === "planning") {
      $("orch-active-headline").textContent = `Coordinator (${activeModelLabel || (coordPart ? `${getProviderMeta(coordPart.provider).name || coordPart.provider} · ${coordPart.model}` : "Architect")}) is creating the implementation plan…`;
      $("orch-active-subline").textContent = "Analyzing task and preparing instructions for Worker.";
    } else if (stage === "working") {
      $("orch-active-headline").textContent = `Worker (${activeModelLabel || (scheduledWorker ? `${getProviderMeta(scheduledWorker.provider).name || scheduledWorker.provider} · ${scheduledWorker.model}` : "Developer")}) is executing round ${curRound}…`;
      $("orch-active-subline").textContent = "Producing deliverable based on Coordinator, QA Auditor, and User instructions.";
    } else if (stage === "auditing") {
      $("orch-active-headline").textContent = `QA Auditor (${activeModelLabel || (auditorPart ? `${getProviderMeta(auditorPart.provider).name || auditorPart.provider} · ${auditorPart.model}` : "Auditor")}) is inspecting round ${curRound}…`;
      $("orch-active-subline").textContent = "Testing Worker deliverable for runtime bugs, missing files, and edge cases before Coordinator review.";
    } else if (stage === "reviewing") {
      $("orch-active-headline").textContent = `Coordinator (${activeModelLabel || (coordPart ? `${getProviderMeta(coordPart.provider).name || coordPart.provider} · ${coordPart.model}` : "Architect")}) is reviewing round ${curRound}…`;
      $("orch-active-subline").textContent = "Evaluating Worker deliverable and QA Auditor report for final approval.";
    } else {
      $("orch-active-headline").textContent = "Collaboration in progress…";
      $("orch-active-subline").textContent = `Stage: ${stage}`;
    }
  } else {
    activeBanner.classList.add("hidden");
  }

  const errEl = $("orch-detail-error");
  if (d.error_message && (status === "failed" || status === "waiting")) {
    errEl.textContent = `Error: ${d.error_message}`;
    errEl.classList.remove("hidden");
  } else {
    errEl.classList.add("hidden");
  }

  const slotFailures = {};
  events.forEach(ev => {
    if (ev.event_type === "participant_failed") {
      const p = parseEventJSON(ev);
      const key = p.sort_order !== undefined && p.sort_order >= 0 ? `slot_${p.sort_order}` : `${p.provider}::${p.model}`;
      slotFailures[key] = (slotFailures[key] || 0) + 1;
    }
  });

  // Identify the participant that answered the very last AI turn (locked from swapping)
  const lastAIMsg = [...messages].reverse().find(m => m.sender_role !== "user");

  const rosterEl = $("orch-detail-roster");
  rosterEl.innerHTML = "";
  participants.forEach((p, i) => {
    const pill = document.createElement("div");

    let isWorkingNow = false;
    if (isActive && status === "running") {
      if (invokeMeta && invokeMeta.sort_order !== undefined && invokeMeta.sort_order >= 0) {
        isWorkingNow = (invokeMeta.sort_order === i);
      } else if (invokeMeta && invokeMeta.provider && invokeMeta.model) {
        isWorkingNow = (p.provider === invokeMeta.provider && p.model === invokeMeta.model && (
          ((stage === "planning" || stage === "reviewing") && p.role === "coordinator") ||
          (stage === "working" && p.role === "worker") ||
          (stage === "auditing" && p.role === "auditor")
        ));
      } else {
        if ((stage === "planning" || stage === "reviewing") && p.role === "coordinator") {
          isWorkingNow = (p === coordPart);
        } else if (stage === "working" && p.role === "worker") {
          isWorkingNow = (p === scheduledWorker);
        } else if (stage === "auditing" && p.role === "auditor") {
          isWorkingNow = (p === auditorPart);
        }
      }
    }

    const answeredLastTurn = !!(lastAIMsg && lastAIMsg.sender_role === p.role && lastAIMsg.provider === p.provider && lastAIMsg.model === p.model);

    pill.className = `orch-roster-pill${isWorkingNow ? " working-now" : ""}`;
    let roleLabel = "Worker / Developer";
    if (p.role === "coordinator") roleLabel = "Coordinator / Architect";
    if (p.role === "auditor") roleLabel = "Quality Assurance / Auditor";
    const provName = getProviderMeta(p.provider).name || p.provider;
    const modelMsgs = messages.filter(m => m.provider === p.provider && m.model === p.model && m.sender_role === p.role).length;
    const failCount = slotFailures[`slot_${i}`] || slotFailures[`${p.provider}::${p.model}`] || 0;

    const stateTag = isWorkingNow
      ? ` · <span style="color:#4ade80;font-weight:700">⚡ WORKING NOW</span>`
      : failCount > 0
        ? ` · <span style="color:#f87171;font-weight:700">✗ ${failCount} fail</span>`
        : "";

    const headerDiv = document.createElement("div");
    const roleSpan = document.createElement("span");
    roleSpan.className = "roster-role";
    roleSpan.innerHTML = `#${i + 1} ${roleLabel} · ${modelMsgs} msg${modelMsgs === 1 ? "" : "s"}${stateTag}`;
    const provBold = document.createElement("b");
    provBold.textContent = `${provName} · ${p.model}`;
    headerDiv.append(roleSpan, provBold);
    pill.appendChild(headerDiv);

    // Mid-Job Model Switcher (locks the model that answered the last turn)
    if (answeredLastTurn) {
      const lockNote = document.createElement("span");
      lockNote.className = "roster-lock-note";
      lockNote.textContent = "🔒 Answered last turn (locked)";
      pill.appendChild(lockNote);
    } else if (status !== "completed") {
      const swapRow = document.createElement("div");
      swapRow.className = "roster-swap-row";
      const swapSel = document.createElement("select");
      swapSel.className = "roster-swap-select";
      swapSel.title = "Change this participant's AI model for upcoming turns";
      buildUnifiedModelOptions(swapSel);
      const curComp = `${p.provider}::${p.model}`;
      if (![...swapSel.options].some(o => o.value === curComp)) {
        const curOpt = document.createElement("option");
        curOpt.value = curComp;
        curOpt.textContent = `${provName} · ${p.model}`;
        swapSel.prepend(curOpt);
      }
      swapSel.value = curComp;
      swapSel.onchange = () => swapParticipantModelMidJob(d.id, participants, i, swapSel.value);
      swapRow.appendChild(swapSel);
      pill.appendChild(swapRow);
    }

    rosterEl.appendChild(pill);
  });

  renderAIContributionBar(participants, messages, events);

  // Final Result Card
  const finalCard = $("orch-final-card");
  if (status === "completed" && d.execution_result) {
    finalCard.classList.remove("hidden");
    syncAuditorModelDropdown();

    const parsedPkg = extractProjectFiles(d.execution_result, d.id);
    const files = parsedPkg.files;
    if (selectedDeliverableFileIdx >= files.length) {
      selectedDeliverableFileIdx = 0;
    }
    const activeFile = files[selectedDeliverableFileIdx] || { filename: "result.txt", content: d.execution_result, ext: "txt", isHtml: false };

    if ($("orch-files-badge")) {
      $("orch-files-badge").textContent = `${files.length} FILE${files.length === 1 ? "" : "S"}`;
    }

    const runAuditBtn = $("orch-run-audit-btn");
    if (runAuditBtn) {
      runAuditBtn.onclick = () => runIndependentAudit(d.id);
    }

    const latestAuditMsg = [...messages].reverse().find(m => m.sender_role === "auditor");
    const auditReportEl = $("orch-audit-report");
    if (auditReportEl) {
      if (latestAuditMsg) {
        auditReportEl.classList.remove("hidden");
        const verdictBadge = $("orch-audit-verdict-badge");
        const isApproved = latestAuditMsg.action === "complete";
        verdictBadge.className = `orch-status-badge ${isApproved ? "status-completed" : "status-waiting"}`;
        verdictBadge.textContent = isApproved ? "✓ APPROVED BY AUDITOR" : "⚠ ISSUES FOUND & FIXED";

        const provShort = getProviderMeta(latestAuditMsg.provider).name || latestAuditMsg.provider;
        $("orch-audit-model-title").textContent = `${provShort} · ${latestAuditMsg.model} (${latestAuditMsg.latency_ms || 0} ms)`;
        $("orch-audit-summary").textContent = latestAuditMsg.completion_summary || "See full auditor report below.";
        $("orch-audit-body").textContent = latestAuditMsg.content || "";

        const applyAuditBtn = $("orch-apply-audit-btn");
        if (applyAuditBtn) {
          const hasCodeInAudit = latestAuditMsg.action === "revise" || latestAuditMsg.content.includes("=== FILE:") || latestAuditMsg.content.includes("```");
          applyAuditBtn.classList.toggle("hidden", !hasCodeInAudit);
          applyAuditBtn.onclick = () => applyAuditorFixes(d.id, latestAuditMsg.id);
        }

        const sendToWorkerBtn = $("orch-send-audit-worker-btn");
        if (sendToWorkerBtn) {
          sendToWorkerBtn.onclick = () => {
            const summaryText = latestAuditMsg.completion_summary || latestAuditMsg.content;
            if ($("orch-revision-input")) {
              $("orch-revision-input").value = `Please fix all issues identified by the Independent Auditor (${latestAuditMsg.model}):\n${summaryText}`;
              $("orch-revision-input").focus();
              toast("Auditor findings copied into the revision box below — click Send Revision to AI Team");
            }
          };
        }
      } else {
        auditReportEl.classList.add("hidden");
      }
    }

    const explorerEl = $("orch-files-explorer");
    const tabsEl = $("orch-files-tabs");
    if (explorerEl && tabsEl) {
      explorerEl.classList.remove("hidden");
      tabsEl.innerHTML = "";
      files.forEach((f, idx) => {
        const tabWrap = document.createElement("div");
        tabWrap.className = `orch-file-tab${idx === selectedDeliverableFileIdx ? " active" : ""}`;

        const selectBtn = document.createElement("button");
        selectBtn.type = "button";
        selectBtn.className = "orch-file-select";
        const sizeKb = Math.max(0.1, (new Blob([f.content]).size / 1024)).toFixed(1);
        const fileBold = document.createElement("b");
        fileBold.textContent = `📄 ${f.filename}`;
        const sizeSmall = document.createElement("small");
        sizeSmall.textContent = `${sizeKb} KB`;
        selectBtn.append(fileBold, document.createTextNode(" "), sizeSmall);
        selectBtn.onclick = () => {
          selectedDeliverableFileIdx = idx;
          renderOrchJobDetails(d);
        };

        const dlIcon = document.createElement("button");
        dlIcon.type = "button";
        dlIcon.className = "orch-file-dl";
        dlIcon.title = `Download ${f.filename}`;
        dlIcon.textContent = "⬇";
        dlIcon.onclick = e => {
          e.stopPropagation();
          downloadSingleFileObj(f);
        };

        tabWrap.append(selectBtn, dlIcon);
        tabsEl.appendChild(tabWrap);
      });
    }

    $("orch-final-body").textContent = activeFile.content;

    const sumNote = $("orch-final-summary-note");
    if (sumNote) {
      if (parsedPkg.summary) {
        sumNote.classList.remove("hidden");
        sumNote.innerHTML = `<b>Coordinator Summary:</b> `;
        const s = document.createElement("span");
        s.textContent = parsedPkg.summary;
        sumNote.appendChild(s);
      } else {
        sumNote.classList.add("hidden");
      }
    }

    const zipBtn = $("orch-download-zip-btn");
    if (zipBtn) {
      zipBtn.textContent = `📦 Download Project (.zip · ${files.length} file${files.length === 1 ? "" : "s"})`;
      zipBtn.onclick = () => downloadProjectZip(files, d.id);
    }

    const dlBtn = $("orch-download-result-btn");
    if (dlBtn) {
      dlBtn.textContent = `⬇ Download ${activeFile.filename}`;
      dlBtn.onclick = () => downloadSingleFileObj(activeFile);
    }

    const copyBtn = $("orch-copy-result-btn");
    if (copyBtn) {
      copyBtn.textContent = `📋 Copy ${activeFile.filename}`;
      copyBtn.onclick = async () => {
        const ok = await copyTextToClipboard(activeFile.content);
        toast(ok ? `✓ Copied ${activeFile.filename} to clipboard!` : "Could not copy automatically");
      };
    }

    const htmlFile = activeFile.isHtml ? activeFile : files.find(f => f.isHtml);
    const prevBtn = $("orch-preview-toggle-btn");
    const prevWrap = $("orch-preview-wrap");
    const prevFrame = $("orch-preview-frame");
    if (prevBtn && prevWrap && prevFrame) {
      prevBtn.classList.toggle("hidden", !htmlFile);
      prevBtn.onclick = () => {
        if (!htmlFile) return;
        const isHidden = prevWrap.classList.contains("hidden");
        if (isHidden) {
          prevFrame.srcdoc = htmlFile.content;
          prevWrap.classList.remove("hidden");
          prevBtn.textContent = "👁 Hide HTML Preview";
        } else {
          prevWrap.classList.add("hidden");
          prevBtn.textContent = "👁 Live HTML Preview";
        }
      };
      if ($("orch-preview-close-btn")) {
        $("orch-preview-close-btn").onclick = () => {
          prevWrap.classList.add("hidden");
          prevBtn.textContent = "👁 Live HTML Preview";
        };
      }
    }
  } else {
    finalCard.classList.add("hidden");
  }

  const revCard = $("orch-revision-card");
  if (revCard) {
    revCard.classList.toggle("hidden", isActive);
  }

  // Render Collapsible Collaboration Transcript (Collapsed by Default)
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
    messages.forEach((m, idx) => {
      const detailsEl = document.createElement("details");
      detailsEl.className = `orch-msg-collapsible role-${m.sender_role}`;
      // Collapsed by default so large code turns don't clutter the screen

      const summaryEl = document.createElement("summary");

      const left = document.createElement("div");
      left.className = "orch-msg-sender";
      let roleTitle = "WORKER / DEVELOPER";
      if (m.sender_role === "coordinator") roleTitle = "COORDINATOR / ARCHITECT";
      if (m.sender_role === "auditor") roleTitle = "QUALITY ASSURANCE / AUDITOR";
      if (m.sender_role === "user") roleTitle = "WORKSPACE OWNER (USER REVISION)";

      const provShort = m.sender_role === "user"
        ? "HUMAN FEEDBACK"
        : (getProviderMeta(m.provider).short_name || (m.provider || "").toUpperCase());
      const modelLabel = m.sender_role === "user" ? "Interactive Revision" : `${provShort} · ${m.model}`;
      left.innerHTML = `<span class="orch-role-tag ${m.sender_role}">#${idx + 1} · ${roleTitle}</span> <b>${modelLabel}</b>`;

      const displayContent = m.sender_role === "user" ? stripRevOptsTag(m.content) : m.content;
      const previewSnippet = (m.completion_summary || displayContent || "").replace(/\s+/g, " ").trim().slice(0, 95);

      const previewSpan = document.createElement("span");
      previewSpan.className = "orch-msg-preview";
      previewSpan.textContent = previewSnippet ? `— ${previewSnippet}… (click to expand)` : "(click to expand)";

      const right = document.createElement("div");
      right.className = "orch-msg-meta";
      right.innerHTML = `<span>Round ${m.round}</span> · <span class="action-tag action-${m.action}">${(m.action || "continue").toUpperCase()}</span>${m.latency_ms ? ` · <span>${(m.latency_ms / 1000).toFixed(1)}s</span>` : ""}`;

      summaryEl.append(left, previewSpan, right);

      const innerCard = document.createElement("div");
      innerCard.style.padding = "12px 14px";

      const toolsBar = document.createElement("div");
      toolsBar.style.display = "flex";
      toolsBar.style.gap = "8px";
      toolsBar.style.marginBottom = "10px";

      const copyTurnBtn = document.createElement("button");
      copyTurnBtn.type = "button";
      copyTurnBtn.className = "mini-turn-btn";
      copyTurnBtn.textContent = "📋 Copy Turn";
      copyTurnBtn.onclick = async () => {
        const ok = await copyTextToClipboard(displayContent);
        toast(ok ? `✓ Turn #${idx + 1} copied` : "Copy failed");
      };

      const dlTurnBtn = document.createElement("button");
      dlTurnBtn.type = "button";
      dlTurnBtn.className = "mini-turn-btn";
      dlTurnBtn.textContent = "⬇ Save Turn";
      dlTurnBtn.onclick = () => {
        const turnFiles = extractProjectFiles(displayContent, `${d.id}-turn-${idx + 1}`).files;
        if (turnFiles.length > 1) {
          downloadProjectZip(turnFiles, `${d.id}-turn-${idx + 1}`);
        } else {
          downloadSingleFileObj(turnFiles[0]);
        }
      };

      toolsBar.append(copyTurnBtn, dlTurnBtn);

      const body = document.createElement("div");
      body.className = "orch-msg-body";
      body.setAttribute("dir", "auto");
      body.textContent = displayContent;

      innerCard.append(toolsBar, body);

      if (m.completion_summary) {
        const sum = document.createElement("div");
        sum.className = "orch-msg-summary";
        sum.setAttribute("dir", "auto");
        sum.innerHTML = `<b>Summary:</b> `;
        const span = document.createElement("span");
        span.textContent = m.completion_summary;
        sum.appendChild(span);
        innerCard.appendChild(sum);
      }

      detailsEl.append(summaryEl, innerCard);
      msgContainer.appendChild(detailsEl);
    });
  }

  // Render Line-by-Line Plain-English Live Progress Timeline
  $("orch-evt-count").textContent = String(events.length);
  const evtContainer = $("orch-events-view");
  evtContainer.innerHTML = "";

  events.forEach((ev, idx) => {
    const line = document.createElement("div");
    line.className = `orch-step-line evt-${ev.event_type}`;

    const time = document.createElement("span");
    time.className = "orch-step-time";
    time.textContent = `#${String(idx + 1).padStart(2, "0")} · ${formatShortTimestamp(ev.created_at)}`;

    const badge = document.createElement("span");
    const isFail = ev.event_type === "participant_failed" || ev.event_type === "participant_fallback" || ev.event_type === "job_failed";
    const isOk = ev.event_type === "plan_generated" || ev.event_type === "worker_response_received" || ev.event_type === "audit_completed" || ev.event_type === "task_completed";
    badge.className = `orch-step-badge${isFail ? " is-fail" : isOk ? " is-ok" : ""}`;
    badge.textContent = formatEventTypeLabel(ev.event_type, ev.phase);

    const text = document.createElement("div");
    text.className = "orch-step-text";
    text.setAttribute("dir", "auto");
    text.textContent = buildDetailedTimelineNarrative(ev);

    line.append(time, badge, text);
    evtContainer.appendChild(line);
  });
}

function buildDetailedTimelineNarrative(ev) {
  const p = parseEventJSON(ev);
  const provName = p.provider ? (getProviderMeta(p.provider).name || p.provider) : "";
  const fromProv = p.from_provider ? (getProviderMeta(p.from_provider).name || p.from_provider) : "";
  const toProv = p.to_provider ? (getProviderMeta(p.to_provider).name || p.to_provider) : "";
  const roundPrefix = p.round ? `[Round ${p.round}] ` : "";

  switch (ev.event_type) {
    case "coordinator_started":
      return `${roundPrefix}Coordinator #${(p.sort_order ?? 0) + 1} (${provName} · ${p.model}) started analyzing your task and writing the master architectural plan…`;
    case "plan_generated":
      return `${roundPrefix}Coordinator (${provName} · ${p.model}) finished the implementation plan in ${((p.latency_ms || 0) / 1000).toFixed(1)}s — passing instructions to Worker.`;
    case "worker_started":
      return `${roundPrefix}Worker #${(p.sort_order ?? 1) + 1} (${provName} · ${p.model}) started building/revising the project files…`;
    case "worker_response_received":
      return `${roundPrefix}Worker #${(p.sort_order ?? 1) + 1} (${provName} · ${p.model}) finished generating the project files in ${((p.latency_ms || 0) / 1000).toFixed(1)}s — passing deliverable for inspection.`;
    case "participant_failed":
      return `${roundPrefix}✗ ${(p.role || "Model").toUpperCase()} (${provName} · ${p.model}) failed with error: ${p.error || ev.message}`;
    case "participant_fallback":
      return `${roundPrefix}⚡ Handing over job: ${fromProv} · ${p.from_model} failed (${p.error || "error"}) → passing turn to backup model ${toProv} · ${p.to_model}…`;
    case "audit_started":
      return `${roundPrefix}QA Auditor (${provName} · ${p.model}) is inspecting the code for runtime bugs, missing files, and broken logic…`;
    case "audit_completed":
      return `${roundPrefix}QA Auditor (${provName} · ${p.model}) finished inspection in ${((p.latency_ms || 0) / 1000).toFixed(1)}s (Verdict: ${(p.action || "complete").toUpperCase()})${p.completion_summary ? ` — "${p.completion_summary}"` : ""}.`;
    case "review_started":
      return `${roundPrefix}Coordinator (${provName} · ${p.model}) is reviewing the Worker's deliverable and QA Auditor report…`;
    case "review_decision":
      return `${roundPrefix}Coordinator (${provName} · ${p.model}) completed review in ${((p.latency_ms || 0) / 1000).toFixed(1)}s — Decision: ${(p.action || "").toUpperCase()}${p.completion_summary ? ` ("${p.completion_summary}")` : ""}.`;
    case "revision_requested":
      return `${roundPrefix}Coordinator requested code revisions — passing task back to the next Worker in your roster.`;
    case "job_paused":
      return `⏸ Task paused by user. You can swap any upcoming AI model in the roster above and click ▶ Play / Continue Task.`;
    default:
      return stripRevOptsTag(ev.message);
  }
}

async function runIndependentAudit(jobId) {
  const sel = $("orch-auditor-select");
  const btn = $("orch-run-audit-btn");
  if (!sel || !sel.value) {
    toast("Please select an Auditor model first.");
    return;
  }
  const [provider, model] = sel.value.split("::");
  btn.disabled = true;
  btn.textContent = `🔍 Auditing with ${model}…`;
  toast(`Running independent audit with ${model}…`);

  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/audit`, {
      method: "POST",
      body: JSON.stringify({ provider, model })
    });
    toast("✓ Independent Second-Opinion Audit completed!");
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Audit failed: " + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "🔍 Audit Project Now";
  }
}

async function applyAuditorFixes(jobId, messageId) {
  const btn = $("orch-apply-audit-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Applying Fixes…";
  }
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/apply-audit`, {
      method: "POST",
      body: JSON.stringify({ message_id: messageId })
    });
    selectedDeliverableFileIdx = 0;
    toast("✓ Auditor's corrected files applied to project!");
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Could not apply auditor fixes: " + err.message);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = "✓ Apply Auditor's Corrected Files";
    }
  }
}

if ($("orch-revision-form")) {
  $("orch-revision-form").addEventListener("submit", async e => {
    e.preventDefault();
    if (!activeOrchJobId) return;
    const input = $("orch-revision-input");
    const instruction = input.value.trim();
    if (!instruction) return;

    const includeFullFiles = $("orch-rev-full-files") ? $("orch-rev-full-files").checked : true;
    const includeSummary = $("orch-rev-summary") ? $("orch-rev-summary").checked : true;

    const btn = $("orch-revision-submit");
    btn.disabled = true;
    btn.textContent = "Sending to AI Team…";

    try {
      await api(`/api/orchestration/jobs/${encodeURIComponent(activeOrchJobId)}/iterate`, {
        method: "POST",
        body: JSON.stringify({
          instruction,
          extra_rounds: 3,
          extra_calls: 8,
          include_full_files: includeFullFiles,
          include_summary: includeSummary
        })
      });
      input.value = "";
      toast("✓ Revision sent! AI team is updating your existing project…");
      await loadOrchJobsList();
      await fetchAndRenderOrchJob(activeOrchJobId);
    } catch (err) {
      toast("Revision error: " + err.message);
    } finally {
      btn.disabled = false;
      btn.innerHTML = 'Send Revision to AI Team <span>↗</span>';
    }
  });
}

async function pauseActiveOrchJob(jobId) {
  const btn = $("orch-pause-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Pausing after current turn…";
  }
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/pause`, { method: "POST" });
    toast("⏸ Pause requested — job will pause as soon as the current AI finishes its turn.");
    await loadOrchJobsList();
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Pause failed: " + err.message);
    if (btn) {
      btn.disabled = false;
      btn.textContent = "⏸ Pause After Current Turn";
    }
  }
}

async function retryOrchJob(jobId) {
  const btn = $("orch-retry-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Resuming…";
  }
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/retry`, { method: "POST" });
    toast("▶ Task resumed! Continuing from last saved step…");
    await loadOrchJobsList();
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Resume failed: " + err.message);
    if (btn) {
      btn.disabled = false;
      btn.textContent = "▶ Play / Resume Task";
    }
  }
}

async function cancelActiveOrchJob(jobId) {
  const btn = $("orch-cancel-btn");
  btn.disabled = true;
  btn.textContent = "Stopping…";
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/cancel`, { method: "POST" });
    toast("⏹ Task stopped immediately");
    await loadOrchJobsList();
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Stop failed: " + err.message);
    btn.disabled = false;
    btn.textContent = "⏹ Stop Now";
  }
}

function formatEventTypeLabel(evtType, phase) {
  const map = {
    job_created: "Queued",
    job_claimed: "Claimed",
    job_started: "Started",
    coordinator_started: "Planning",
    plan_generated: "Plan Ready",
    worker_started: "Worker Active",
    worker_response_received: "Draft Complete",
    review_started: "Reviewing",
    review_decision: "Decision",
    revision_requested: "Revision",
    revision_completed: "Revised",
    participant_failed: "Model Failed",
    participant_fallback: "Failover Switch",
    participants_updated: "Team Updated",
    user_revision_requested: "User Revision",
    audit_started: "Auditing",
    audit_completed: "Audit Verdict",
    audit_applied: "Fixes Applied",
    model_invoked: "API Call",
    model_responded: "API Reply",
    job_paused: "Paused",
    retry_scheduled: "Resumed",
    worker_recovery: "Recovered",
    task_completed: "Completed",
    job_completed: "Finished",
    job_failed: "Failed",
    job_cancelled: "Stopped"
  };
  return map[evtType] || `${evtType}`;
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
