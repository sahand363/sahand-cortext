const $ = id => document.getElementById(id);
let currentSession = null, toastTimer, deleteConfirmTimer;
let sessionsCache = [];

let selectedProvider = "gemini";
let activeProvider = "gemini";
let activeModel = "gemini-2.0-flash";
let freeOnlyOpenRouter = true;
let deleteArmed = false;

// Orchestration State (Defaults to 3-Role Pipeline: Coordinator -> Worker -> QA Auditor)
let orchParticipantsDraft = [
  { role: "coordinator", name: "Coordinator / Architect", provider: "gemini", model: "gemini-2.0-flash" },
  { role: "worker", name: "Worker / Developer", provider: "gemini", model: "gemini-2.0-flash" },
  { role: "auditor", name: "Quality Assurance / Auditor", provider: "gemini", model: "gemini-2.0-flash" }
];
let orchJobsCache = [];
let activeOrchJobId = localStorage.getItem("cortext_active_orch_job") || null;
let orchPollTimer = null;
let orchActiveSubView = "messages";
let orchSubmitting = false;
let selectedDeliverableFileIdx = 0;

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
  aiprime: {
    name: "AIPrime",
    short: "AIPRIME",
    icon: "◈",
    placeholder: "Paste your AIPrime API key (sk-...)",
    label: "AIPrime API Key (https://api.aiprime.shop/v1)",
    desc: "Connect AIPrime to use GLM 5.3 Flash (glm-5.3-flash) and any other models available on your AIPrime wallet.",
    helpText: "Manage your AIPrime keys and models:",
    helpUrl: "https://aiprime.shop",
    helpBtn: "Open AIPrime Panel ↗"
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

const allProvidersList = ["gemini", "openrouter", "aiprime", "codecraft"];

let providerState = {
  gemini: { configured: false, models: [] },
  openrouter: { configured: false, models: [] },
  aiprime: { configured: false, models: [] },
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

// Universal Copy Helper: works on both HTTP (IP address) and HTTPS
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
    providerState.aiprime.configured = !!s.providers.aiprime;
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
  const anyConn = allProvidersList.some(p => providerState[p].configured);
  $("header-provider-state").textContent = anyConn ? "CONNECTED" : "SETUP";
  $("header-provider-state").classList.toggle("connected", anyConn);

  allProvidersList.forEach(p => {
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

  if ($("custom-model-id")) {
    $("custom-model-id").placeholder = provider === "aiprime"
      ? "Enter AIPrime model ID (e.g. glm-5.3-flash)"
      : `Enter custom ${meta.name} model ID`;
  }

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
  for (const p of allProvidersList) {
    if (providerState[p].configured) {
      await loadProviderModels(p, false);
    }
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
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
    syncAuditorModelDropdown();
    renderModelGrids();
    if (showToastMsg) {
      toast(`Loaded ${providerState[provider].models.length} models from ${providerCatalog[provider].name}`);
    }
  } catch (err) {
    if (showToastMsg) toast(`Failed to load ${providerCatalog[provider].name} models: ` + err.message);
  }
}

function buildUnifiedModelOptions(sel) {
  if (!sel) return;
  const prevVal = sel.value;
  sel.innerHTML = "";

  const workingGroup = document.createElement("optgroup");
  workingGroup.label = "★ Verified Working Models";
  let hasWorking = false;

  allProvidersList.forEach(p => {
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

  allProvidersList.forEach(p => {
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

  if (prevVal && [...sel.options].some(o => o.value === prevVal)) {
    sel.value = prevVal;
  }
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
  syncAuditorModelDropdown();
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
    syncAuditorModelDropdown();
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

// Add & Test Custom Model ID
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
      providerState[provider].models = res.models || [];
      if (res.ok && res.active_provider && res.active_model) {
        setActiveSelectionUI(res.active_provider, res.active_model);
        toast(`✓ [${providerCatalog[provider].short}] ${model} verified working (${res.latency_ms} ms)!`);
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
  return allProvidersList.filter(p => providerState[p].configured);
}

function getDefaultModelForProvider(p) {
  const models = providerState[p]?.models || [];
  const working = models.find(m => m.status === "working");
  if (working) return working.id;
  if (p === "aiprime") return "glm-5.3-flash";
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
    allProvidersList.forEach(p => {
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
  const messages = d.messages || [];
  const events = d.events || [];

  const statusEl = $("orch-detail-status");
  statusEl.className = `orch-status-badge status-${status}`;
  statusEl.textContent = status.toUpperCase();

  $("orch-detail-stage").textContent = `STAGE: ${stage.toUpperCase()}`;
  $("orch-detail-rounds").textContent = `Round ${d.current_round || 0} / ${d.max_rounds || 5}`;
  $("orch-detail-calls").textContent = `Messages ${messages.length} / ${d.max_calls || 15}`;
  $("orch-detail-task").textContent = d.task || "";
  $("orch-detail-sub").textContent = `Job ID: ${d.id} · Created ${formatShortTimestamp(d.created_at)}${d.retry_count ? ` · Retries: ${d.retry_count}/${d.max_retries}` : ""}`;

  // Calculate Collaboration Exchange Stats
  const coordMsgs = messages.filter(m => m.sender_role === "coordinator").length;
  const workerMsgs = messages.filter(m => m.sender_role === "worker").length;
  const auditorMsgs = messages.filter(m => m.sender_role === "auditor").length;
  const userRevs = messages.filter(m => m.sender_role === "user").length;
  const revisionsReq = messages.filter(m => (m.sender_role === "coordinator" || m.sender_role === "user" || m.sender_role === "auditor") && m.action === "revise").length;
  const failoversCount = events.filter(e => e.event_type === "participant_fallback" || e.event_type === "retry_scheduled").length;
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
    $("orch-stat-failovers-sub").textContent = failoversCount > 0 ? "Automatic model switches" : "No model failures";
    $("orch-stat-latency").textContent = `${(totalLatencyMs / 1000).toFixed(1)} s`;
    $("orch-stat-latency-sub").textContent = `${totalLatencyMs.toLocaleString()} ms total API time`;
  }

  const isActive = status === "queued" || status === "running" || status === "waiting";
  const canRetry = status === "failed" || status === "cancelled" || status === "waiting";

  const cancelBtn = $("orch-cancel-btn");
  cancelBtn.classList.toggle("hidden", !isActive);
  cancelBtn.disabled = false;
  cancelBtn.textContent = "⏹ Stop Task";
  cancelBtn.onclick = () => cancelActiveOrchJob(d.id);

  const retryBtn = $("orch-retry-btn");
  if (retryBtn) {
    retryBtn.classList.toggle("hidden", !canRetry);
    retryBtn.disabled = false;
    retryBtn.textContent = "↻ Retry / Resume Task";
    retryBtn.onclick = () => retryOrchJob(d.id);
  }

  const activeBanner = $("orch-active-banner");
  const participants = d.participants || [];
  const coordPart = participants.find(p => p.role === "coordinator") || participants[0];
  const workerPart = participants.find(p => p.role === "worker") || participants[1] || coordPart;
  const auditorPart = participants.find(p => p.role === "auditor") || coordPart;

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
      $("orch-active-subline").textContent = "Producing deliverable based on Coordinator, QA Auditor, and User instructions.";
    } else if (stage === "auditing") {
      $("orch-active-headline").textContent = `QA Auditor (${auditorPart ? `${providerCatalog[auditorPart.provider]?.name || auditorPart.provider} · ${auditorPart.model}` : "Auditor"}) is inspecting round ${d.current_round || 1}…`;
      $("orch-active-subline").textContent = "Testing Worker deliverable for runtime bugs, missing files, and edge cases before Coordinator review.";
    } else if (stage === "reviewing") {
      $("orch-active-headline").textContent = `Coordinator (${coordPart ? `${providerCatalog[coordPart.provider]?.name || coordPart.provider} · ${coordPart.model}` : "Architect"}) is reviewing round ${d.current_round || 1}…`;
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

  const rosterEl = $("orch-detail-roster");
  rosterEl.innerHTML = "";
  participants.forEach((p, i) => {
    const pill = document.createElement("div");
    const isWorkingNow = isActive && (
      ((stage === "planning" || stage === "reviewing") && p.role === "coordinator") ||
      (stage === "working" && p.role === "worker") ||
      (stage === "auditing" && p.role === "auditor")
    );
    pill.className = `orch-roster-pill${isWorkingNow ? " working-now" : ""}`;
    let roleLabel = "Worker / Developer";
    if (p.role === "coordinator") roleLabel = "Coordinator / Architect";
    if (p.role === "auditor") roleLabel = "Quality Assurance / Auditor";
    const provName = providerCatalog[p.provider]?.name || p.provider;
    const modelMsgs = messages.filter(m => m.provider === p.provider && m.model === p.model && m.sender_role === p.role).length;
    pill.innerHTML = `<span class="roster-role">#${i + 1} ${roleLabel} · ${modelMsgs} msg${modelMsgs === 1 ? "" : "s"}</span><b>${provName} · ${p.model}</b>`;
    rosterEl.appendChild(pill);
  });

  // Final Result Card + Second-Opinion Auditor + Multi-File Explorer + ZIP Download
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

        const provShort = providerCatalog[latestAuditMsg.provider]?.name || latestAuditMsg.provider;
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
        selectBtn.innerHTML = `<b>📄 ${f.filename}</b> <small>${sizeKb} KB</small>`;
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

  // Show Human-in-the-Loop Revision Box whenever job is completed, failed, or cancelled
  const revCard = $("orch-revision-card");
  if (revCard) {
    revCard.classList.toggle("hidden", isActive);
  }

  // Render Collaboration Messages Transcript
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
      const card = document.createElement("article");
      card.className = `orch-msg-card role-${m.sender_role}`;

      const head = document.createElement("div");
      head.className = "orch-msg-head";

      const left = document.createElement("div");
      left.className = "orch-msg-sender";
      let roleTitle = "WORKER / DEVELOPER";
      if (m.sender_role === "coordinator") roleTitle = "COORDINATOR / ARCHITECT";
      if (m.sender_role === "auditor") roleTitle = "QUALITY ASSURANCE / AUDITOR";
      if (m.sender_role === "user") roleTitle = "WORKSPACE OWNER (USER REVISION)";

      const provShort = m.sender_role === "user"
        ? "HUMAN FEEDBACK"
        : (providerCatalog[m.provider]?.short || (m.provider || "").toUpperCase());
      const modelLabel = m.sender_role === "user" ? "Interactive Revision" : `${provShort} · ${m.model}`;
      left.innerHTML = `<span class="orch-role-tag ${m.sender_role}">#${idx + 1} · ${roleTitle}</span> <b>${modelLabel}</b>`;

      const right = document.createElement("div");
      right.className = "orch-msg-meta";
      right.innerHTML = `<span>Round ${m.round}</span> · <span class="action-tag action-${m.action}">Action: ${(m.action || "continue").toUpperCase()}</span>${m.latency_ms ? ` · <span>${m.latency_ms} ms</span>` : ""}`;

      const copyTurnBtn = document.createElement("button");
      copyTurnBtn.type = "button";
      copyTurnBtn.className = "mini-turn-btn";
      copyTurnBtn.textContent = "📋 Copy";
      copyTurnBtn.onclick = async () => {
        const ok = await copyTextToClipboard(m.content);
        toast(ok ? `✓ Turn #${idx + 1} copied` : "Copy failed");
      };

      const dlTurnBtn = document.createElement("button");
      dlTurnBtn.type = "button";
      dlTurnBtn.className = "mini-turn-btn";
      dlTurnBtn.textContent = "⬇ Save";
      dlTurnBtn.onclick = () => {
        const turnFiles = extractProjectFiles(m.content, `${d.id}-turn-${idx + 1}`).files;
        if (turnFiles.length > 1) {
          downloadProjectZip(turnFiles, `${d.id}-turn-${idx + 1}`);
        } else {
          downloadSingleFileObj(turnFiles[0]);
        }
      };

      right.append(copyTurnBtn, dlTurnBtn);
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

// Submit Human-in-the-Loop Revision Request
if ($("orch-revision-form")) {
  $("orch-revision-form").addEventListener("submit", async e => {
    e.preventDefault();
    if (!activeOrchJobId) return;
    const input = $("orch-revision-input");
    const instruction = input.value.trim();
    if (!instruction) return;

    const btn = $("orch-revision-submit");
    btn.disabled = true;
    btn.textContent = "Sending to AI Team…";

    try {
      await api(`/api/orchestration/jobs/${encodeURIComponent(activeOrchJobId)}/iterate`, {
        method: "POST",
        body: JSON.stringify({
          instruction,
          extra_rounds: 3,
          extra_calls: 8
        })
      });
      input.value = "";
      toast("✓ Revision sent! AI team is updating your project…");
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

async function retryOrchJob(jobId) {
  const btn = $("orch-retry-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Resuming…";
  }
  try {
    await api(`/api/orchestration/jobs/${encodeURIComponent(jobId)}/retry`, { method: "POST" });
    toast("↻ Task re-queued! Resuming from last saved step…");
    await loadOrchJobsList();
    await fetchAndRenderOrchJob(jobId);
  } catch (err) {
    toast("Retry failed: " + err.message);
    if (btn) {
      btn.disabled = false;
      btn.textContent = "↻ Retry / Resume Task";
    }
  }
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
    participant_fallback: "Automatic Model Failover",
    user_revision_requested: "User Revision Submitted",
    audit_started: "Second-Opinion Audit Started",
    audit_completed: "Second-Opinion Audit Completed",
    audit_applied: "Auditor Fixes Applied",
    model_invoked: "Model Request Sent",
    model_responded: "Model Response Received",
    retry_scheduled: "Retry / Resume Scheduled",
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
