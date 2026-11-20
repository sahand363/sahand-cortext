const $ = id => document.getElementById(id);
let currentSession = null, toastTimer, deleteConfirmTimer;
let sessionsCache = [];
let botSessionsCache = [];

const DEVICE_STATE_KEYS = {
  page: "cortext_active_page",
  chatView: "cortext_chat_view_mode", // "list" | "active"
  session: "cortext_active_session",
  orchJob: "cortext_active_orch_job",
  orchSubView: "cortext_active_orch_subview",
  settingsTab: "cortext_active_settings_tab",
  selectedProvider: "cortext_selected_provider"
};

let chatViewMode = localStorage.getItem(DEVICE_STATE_KEYS.chatView) || "active";

let providersList = [];
let providerMap = {};
let personasList = [];
let personaMap = {};
let editingPersonaId = null;

let selectedProvider = "";
let activeProvider = "";
let activeModel = "";
let freeOnlyFilter = true;
let editingCustomProviderId = null;
let deleteArmed = false;

let chatModelScope = {
  mode: "all",
  allowed_models: []
};

let orchParticipantsDraft = [
  { role: "coordinator", name: "Coordinator / Architect", provider: "", model: "", persona_id: "" },
  { role: "worker", name: "Worker / Developer", provider: "", model: "", persona_id: "" },
  { role: "auditor", name: "Quality Assurance / Auditor", provider: "", model: "", persona_id: "" }
];
let orchJobsCache = [];
let activeOrchJobId = localStorage.getItem(DEVICE_STATE_KEYS.orchJob) || null;
let orchPollTimer = null;
let orchActiveSubView = localStorage.getItem(DEVICE_STATE_KEYS.orchSubView) || "events";
let orchSubmitting = false;
let selectedDeliverableFileIdx = 0;

// Cache full collaboration message bodies fetched on demand: key = `${jobId}:${msgId}`
const loadedOrchMessagesCache = new Map();

const contribPalette = [
  "#38bdf8",
  "#facc15",
  "#4ade80",
  "#a855f7",
  "#fb7185",
  "#fb923c",
  "#2dd4bf"
];

const pageNames = {
  chat: "Chat",
  orchestration: "AI orchestration",
  skills: "Skills",
  channels: "Channels",
  memory: "Memories",
  mobile: "Mobile App",
  backup: "Backup",
  usage: "Usage & analytics",
  settings: "Settings"
};

/* ============================================================================
   THEME & MOBILE NAVIGATION
   ============================================================================ */

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

/* ============================================================================
   CORE API, CLIPBOARD & UTILITIES
   ============================================================================ */

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
  if (!r.ok) {
    const err = new Error(d.error || "Request failed");
    err.status = r.status;
    throw err;
  }
  return d;
}

function toast(msg) {
  const t = $("toast");
  if (!t) return;
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

// Orchestration downloads are named date_time_three-words.zip, e.g.
// 2026-11-20_1406_telegram-bot-refactor.zip: the job's start time (local)
// plus the first three meaningful words of its task.
const ORCH_NAME_STOP = new Set(("a an the and or but for of to in on at by with from into about as is are was were be been " +
  "make build create write please can could would should you your me my i we our it its this that these those " +
  "using use new simple some any all just also then than so do does need want help me " +
  "برای با از به در که این آن را یک و یا تا هم بساز بنویس کن").split(" "));

function orchTaskWords(task) {
  const words = String(task || "").toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean);
  const out = [];
  for (const w of words) {
    if (ORCH_NAME_STOP.has(w)) continue;
    if (w.length < 3 && !/^\p{N}+$/u.test(w)) continue;
    if (out.includes(w)) continue;
    out.push(w.slice(0, 20));
    if (out.length === 3) break;
  }
  return out.length ? out.join("-") : "orchestration-result";
}

function orchZipName(job, suffix) {
  let d = null;
  if (job && job.created_at) {
    const ts = String(job.created_at);
    d = new Date(ts.includes("T") ? ts : ts.replace(" ", "T") + (/[zZ]|[+-]\d\d:?\d\d$/.test(ts) ? "" : "Z"));
  }
  if (!d || isNaN(d.getTime())) d = new Date();
  const p = n => String(n).padStart(2, "0");
  const stamp = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}_${p(d.getHours())}${p(d.getMinutes())}`;
  return `${stamp}_${orchTaskWords(job && job.task)}${suffix ? "_" + suffix : ""}.zip`;
}

function downloadProjectZip(files, zipName) {
  if (!files || !files.length) return;
  const zipBlob = buildZipBlob(files);
  const url = URL.createObjectURL(zipBlob);
  const a = document.createElement("a");
  const name = /\.zip$/i.test(zipName || "") ? zipName : `${String(zipName || "project").replace(/[^\p{L}\p{N}_-]/gu, "")}.zip`;
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(url), 1500);
  toast(`📦 Downloaded ${name} (${files.length} files)`);
}

/* ============================================================================
   PLUGGABLE CHATBOTS / PERSONAS REGISTRY & VOICE TRAINER
   ============================================================================ */

function hydratePersonas(list) {
  if (!Array.isArray(list)) return;
  personasList = list;
  personaMap = {};
  personasList.forEach(p => {
    personaMap[p.id] = p;
  });
  syncChatPersonaDropdown();
  renderPersonasGrid();
  renderOrchParticipantsBuilder();
}

function getPersonaById(id) {
  return id ? (personaMap[id] || null) : null;
}

function getActiveSessionPersona() {
  const cur = sessionsCache.find(s => s.id === currentSession);
  if (cur && cur.persona_id) {
    return getPersonaById(cur.persona_id);
  }
  const selVal = $("chat-persona-select")?.value || "";
  return getPersonaById(selVal);
}

function syncChatPersonaDropdown() {
  const sel = $("chat-persona-select");
  if (!sel) return;
  const cur = sessionsCache.find(s => s.id === currentSession);
  const targetPersonaId = cur ? (cur.persona_id || "") : (sel.value || "");

  sel.innerHTML = "";
  const defOpt = document.createElement("option");
  defOpt.value = "";
  defOpt.textContent = "Default Assistant";
  sel.appendChild(defOpt);

  personasList.filter(p => p.enabled !== false).forEach(p => {
    const opt = document.createElement("option");
    opt.value = p.id;
    opt.textContent = `${p.icon || "🤖"} ${p.name}`;
    sel.appendChild(opt);
  });

  if ([...sel.options].some(o => o.value === targetPersonaId)) {
    sel.value = targetPersonaId;
  } else {
    sel.value = "";
  }
}

$("chat-persona-select")?.addEventListener("change", async e => {
  const pid = e.target.value || "";
  const bot = getPersonaById(pid);
  if (currentSession) {
    try {
      await api("/api/sessions", {
        method: "PATCH",
        body: JSON.stringify({ id: currentSession, persona_id: pid })
      });
      const sess = sessionsCache.find(s => s.id === currentSession);
      if (sess) sess.persona_id = pid;
    } catch (err) {
      toast("Could not switch chatbot: " + err.message);
    }
  }
  syncModelDropdown();
  if (bot && bot.default_provider && bot.default_model) {
    const comp = `${bot.default_provider}::${bot.default_model}`;
    if ([...$("model").options].some(o => o.value === comp)) {
      setActiveSelectionUI(bot.default_provider, bot.default_model);
    }
  }
  toast(bot ? `Switched to ${bot.icon || "🤖"} ${bot.name}` : "Switched to Default Assistant");
});

function renderPersonasGrid() {
  const grid = $("personas-list-grid");
  if (!grid) return;
  grid.innerHTML = "";

  if (!personasList.length) {
    const empty = document.createElement("div");
    empty.className = "orch-empty-note";
    empty.textContent = "No custom chatbots yet. Click \"＋ New Chatbot / Voice Clone\" to create a specialist assistant or train a bot to talk like you.";
    grid.appendChild(empty);
    return;
  }

  personasList.forEach(p => {
    const card = document.createElement("div");
    card.className = "persona-card";

    const top = document.createElement("div");
    top.className = "persona-card-top";

    const titleArea = document.createElement("div");
    titleArea.className = "persona-card-title";
    const iconSpan = document.createElement("span");
    iconSpan.className = "persona-card-icon";
    iconSpan.textContent = p.icon || "🤖";

    const infoDiv = document.createElement("div");
    const nameB = document.createElement("b");
    nameB.textContent = p.name;
    const descSmall = document.createElement("small");
    descSmall.textContent = p.description || (p.voice_blueprint ? "Trained Voice & Style Persona" : "Custom AI Persona");
    infoDiv.append(nameB, descSmall);
    titleArea.append(iconSpan, infoDiv);

    const badges = document.createElement("div");
    badges.className = "api-model-badges";
    if (p.voice_blueprint) {
      const vBadge = document.createElement("span");
      vBadge.className = "model-badge badge-working";
      vBadge.textContent = "🎙️ VOICE TRAINED";
      badges.appendChild(vBadge);
    }
    if (p.telegram_enabled) {
      const tgBadge = document.createElement("span");
      tgBadge.className = "model-badge free-badge";
      tgBadge.textContent = p.has_telegram_token ? "✈ TG READY" : "✈ TG NO TOKEN";
      badges.appendChild(tgBadge);
    }
    const scopeBadge = document.createElement("span");
    scopeBadge.className = "model-badge badge-untested";
    scopeBadge.textContent = p.model_scope_mode === "selected"
      ? `${(p.allowed_models || []).length} ALLOWED MODELS`
      : "ALL MODELS";
    badges.appendChild(scopeBadge);

    top.append(titleArea, badges);

    const metaLine = document.createElement("div");
    metaLine.className = "api-model-meta";
    const defText = (p.default_provider && p.default_model)
      ? `Default: ${getProviderMeta(p.default_provider).short_name} · ${p.default_model}`
      : "Default: Workspace active model";
    const exCount = (p.few_shot_examples || []).length;
    metaLine.textContent = `${defText}${exCount > 0 ? ` · ${exCount} style anchor example(s)` : ""}`;

    const actions = document.createElement("div");
    actions.className = "session-card-tools";

    const chatBtn = document.createElement("button");
    chatBtn.type = "button";
    chatBtn.className = "btn btn-primary btn-small";
    chatBtn.textContent = "💬 Chat Now";
    chatBtn.onclick = async () => {
      switchPage("chat");
      await newSession(p.id);
    };

    const editBtn = document.createElement("button");
    editBtn.type = "button";
    editBtn.className = "tool-btn small";
    editBtn.textContent = "✎ Edit / Train";
    editBtn.onclick = () => openPersonaEditor(p);

    const delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "tool-btn small danger";
    delBtn.textContent = "🗑 Delete";
    delBtn.onclick = async () => {
      try {
        const res = await api(`/api/personas/${encodeURIComponent(p.id)}`, { method: "DELETE" });
        hydratePersonas(res.personas || []);
        toast(`🗑 Deleted ${p.name}`);
      } catch (err) {
        toast("Delete failed: " + err.message);
      }
    };

    actions.append(chatBtn, editBtn, delBtn);
    card.append(top, metaLine, actions);
    grid.appendChild(card);
  });
}

function renderPersonaAllowedModelsCheckboxes(selectedList) {
  const listEl = $("per-allowed-models-list");
  if (!listEl) return;
  listEl.innerHTML = "";
  const allowedSet = new Set(selectedList || []);

  providersList.forEach(p => {
    if (!p.configured) return;
    const enabledModels = (p.models || []).filter(m => m.enabled !== false);
    if (!enabledModels.length) return;

    const group = document.createElement("div");
    group.className = "chat-scope-group";

    const groupTitle = document.createElement("div");
    groupTitle.className = "chat-scope-group-title";
    groupTitle.textContent = `${p.icon || "⚡"} ${p.name}`;
    group.appendChild(groupTitle);

    enabledModels.forEach(m => {
      const composite = `${p.id}::${m.id}`;
      const row = document.createElement("label");
      row.className = "chat-scope-item";

      const chk = document.createElement("input");
      chk.type = "checkbox";
      chk.value = composite;
      chk.checked = allowedSet.has(composite);

      const nameSpan = document.createElement("span");
      nameSpan.textContent = `${m.display_name || m.id} (${m.id})`;
      row.append(chk, nameSpan);

      if (m.status === "working") {
        const badge = document.createElement("span");
        badge.className = "model-badge badge-working";
        badge.textContent = `✓ ${m.latency_ms} ms`;
        row.appendChild(badge);
      }
      group.appendChild(row);
    });

    listEl.appendChild(group);
  });
}

function openPersonaEditor(existing) {
  editingPersonaId = existing ? existing.id : "__new__";
  $("persona-editor-form")?.classList.remove("hidden");

  buildUnifiedModelOptions($("per-train-model"));
  buildUnifiedModelOptions($("per-default-model"));
  if ($("per-default-model")) {
    const autoOpt = document.createElement("option");
    autoOpt.value = "";
    autoOpt.textContent = "Use Workspace Active Model";
    $("per-default-model").prepend(autoOpt);
  }

  if (existing) {
    $("persona-form-title").textContent = `Edit Chatbot · ${existing.name}`;
    $("per-name").value = existing.name || "";
    $("per-icon").value = existing.icon || "🤖";
    $("per-desc").value = existing.description || "";
    $("per-system-prompt").value = existing.system_prompt || "";
    $("per-voice-blueprint").value = existing.voice_blueprint || "";
    $("per-few-shot").value = JSON.stringify(existing.few_shot_examples || [], null, 2);
    $("per-knowledge").value = existing.knowledge_notes || "";

    const defComp = (existing.default_provider && existing.default_model)
      ? `${existing.default_provider}::${existing.default_model}`
      : "";
    $("per-default-model").value = [...$("per-default-model").options].some(o => o.value === defComp) ? defComp : "";

    $("per-scope-mode").value = existing.model_scope_mode === "selected" ? "selected" : "all";
    $("per-allowed-models-wrap").classList.toggle("hidden", $("per-scope-mode").value !== "selected");
    renderPersonaAllowedModelsCheckboxes(existing.allowed_models || []);

    $("per-tg-enabled").checked = !!existing.telegram_enabled;
    $("per-tg-fields").classList.toggle("hidden", !existing.telegram_enabled);
    $("per-tg-token").value = "";
    $("per-tg-token-status").textContent = existing.has_telegram_token ? "Saved on server" : "Not set";
    $("per-tg-users").value = (existing.telegram_allowed_users || []).join(", ");
  } else {
    $("persona-form-title").textContent = "Create Chatbot / Voice Clone";
    $("per-name").value = "";
    $("per-icon").value = "🤖";
    $("per-desc").value = "";
    $("per-system-prompt").value = "";
    $("per-voice-blueprint").value = "";
    $("per-few-shot").value = "[]";
    $("per-knowledge").value = "";
    $("per-default-model").value = "";
    $("per-scope-mode").value = "all";
    $("per-allowed-models-wrap").classList.add("hidden");
    renderPersonaAllowedModelsCheckboxes([]);
    $("per-tg-enabled").checked = false;
    $("per-tg-fields").classList.add("hidden");
    $("per-tg-token").value = "";
    $("per-tg-token-status").textContent = "Not set";
    $("per-tg-users").value = "";
  }

  $("per-train-log").value = "";
  $("per-train-status").textContent = "";
  $("persona-editor-form")?.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function closePersonaEditor() {
  editingPersonaId = null;
  $("persona-editor-form")?.classList.add("hidden");
}

$("new-persona-btn")?.addEventListener("click", () => openPersonaEditor(null));
$("cancel-persona-btn")?.addEventListener("click", () => closePersonaEditor());

$("per-scope-mode")?.addEventListener("change", e => {
  const isSel = e.target.value === "selected";
  $("per-allowed-models-wrap")?.classList.toggle("hidden", !isSel);
  if (isSel && !$("per-allowed-models-list")?.children.length) {
    renderPersonaAllowedModelsCheckboxes([]);
  }
});

$("per-models-select-all")?.addEventListener("click", () => {
  $("per-allowed-models-list")?.querySelectorAll("input[type='checkbox']").forEach(c => { c.checked = true; });
});
$("per-models-clear-all")?.addEventListener("click", () => {
  $("per-allowed-models-list")?.querySelectorAll("input[type='checkbox']").forEach(c => { c.checked = false; });
});

$("per-tg-enabled")?.addEventListener("change", e => {
  $("per-tg-fields")?.classList.toggle("hidden", !e.target.checked);
});

$("per-train-file")?.addEventListener("change", async e => {
  const file = e.target.files?.[0];
  if (!file) return;
  try {
    const text = await file.text();
    $("per-train-log").value = text.slice(0, 250000);
    toast(`📂 Loaded ${file.name} (${(file.size / 1024).toFixed(1)} KB)`);
  } catch (err) {
    toast("Could not read file: " + err.message);
  }
});

$("per-run-train-btn")?.addEventListener("click", async () => {
  const rawChatLog = $("per-train-log").value.trim();
  if (!rawChatLog) {
    toast("Please paste or upload a chat log first.");
    return;
  }
  const btn = $("per-run-train-btn");
  const statusEl = $("per-train-status");
  btn.disabled = true;
  btn.textContent = "⏳ Analyzing your voice & rhythm…";
  statusEl.textContent = "Extracting your messages and distilling style blueprint…";

  const trainerModel = $("per-train-model")?.value || "";
  const [tProv, tMod] = trainerModel.includes("::") ? trainerModel.split("::") : [activeProvider, activeModel];

  try {
    const res = await api("/api/personas/train", {
      method: "POST",
      body: JSON.stringify({
        target_name: $("per-train-speaker").value.trim() || $("per-name").value.trim() || "Me",
        raw_chat_log: rawChatLog,
        extra_notes: $("per-train-hints").value.trim(),
        provider: tProv,
        model: tMod,
        max_examples: 6
      })
    });
    $("per-voice-blueprint").value = res.voice_blueprint || "";
    $("per-few-shot").value = JSON.stringify(res.few_shot_examples || [], null, 2);
    statusEl.textContent = `✓ Distilled from ${res.matched_replies} matched replies (${res.detected_format})`;
    toast("✨ Voice Blueprint & Style Anchor Examples generated!");
  } catch (err) {
    statusEl.textContent = `✗ ${err.message}`;
    toast("Training failed: " + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "✨ Distill Voice Blueprint & Examples";
  }
});

$("persona-editor-form")?.addEventListener("submit", async e => {
  e.preventDefault();
  const btn = $("save-persona-btn");
  btn.disabled = true;
  btn.textContent = "Saving Chatbot…";

  let fewShotParsed = [];
  try {
    const rawFS = $("per-few-shot").value.trim();
    if (rawFS) {
      const parsed = JSON.parse(rawFS);
      if (Array.isArray(parsed)) fewShotParsed = parsed;
    }
  } catch {
    toast("Invalid JSON in Style Anchor Examples — fix formatting before saving.");
    btn.disabled = false;
    btn.innerHTML = 'Save Chatbot Persona <span>↗</span>';
    return;
  }

  const defComp = $("per-default-model").value || "";
  const [defProv, defMod] = defComp.includes("::") ? defComp.split("::") : ["", ""];

  const allowedModels = [];
  $("per-allowed-models-list")?.querySelectorAll("input[type='checkbox']:checked").forEach(chk => {
    allowedModels.push(chk.value);
  });

  const tgUsers = ($("per-tg-users").value || "")
    .split(",")
    .map(s => s.trim())
    .filter(Boolean);

  try {
    const res = await api("/api/personas", {
      method: "POST",
      body: JSON.stringify({
        id: editingPersonaId && editingPersonaId !== "__new__" ? editingPersonaId : "",
        name: $("per-name").value.trim(),
        icon: $("per-icon").value.trim() || "🤖",
        description: $("per-desc").value.trim(),
        system_prompt: $("per-system-prompt").value.trim(),
        voice_blueprint: $("per-voice-blueprint").value.trim(),
        few_shot_examples: fewShotParsed,
        knowledge_notes: $("per-knowledge").value.trim(),
        default_provider: defProv,
        default_model: defMod,
        model_scope_mode: $("per-scope-mode").value,
        allowed_models: allowedModels,
        telegram_enabled: $("per-tg-enabled").checked,
        telegram_bot_token: $("per-tg-token").value.trim(),
        telegram_allowed_users: tgUsers
      })
    });
    hydratePersonas(res.personas || []);
    closePersonaEditor();
    toast(`✓ Saved chatbot "${res.persona.name}"`);
  } catch (err) {
    toast("Save failed: " + err.message);
  } finally {
    btn.disabled = false;
    btn.innerHTML = 'Save Chatbot Persona <span>↗</span>';
  }
});

/* ============================================================================
   DYNAMIC PROVIDER REGISTRY & CUSTOM SPEC BUILDER
   ============================================================================ */

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
    adapter_type: "openai_compat",
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
      addBtn.textContent = "＋ Add Custom Provider";
      addBtn.onclick = () => openCustomProviderForm(null);
      el.appendChild(addBtn);
    }
  });
}

function syncCustomProviderAuthFields() {
  const auth = $("cp-auth-type")?.value || "bearer";
  if ($("cp-auth-header-wrap")) $("cp-auth-header-wrap").classList.toggle("hidden", auth !== "header");
  if ($("cp-api-key-wrap")) $("cp-api-key-wrap").classList.toggle("hidden", auth === "none");
}

function syncCustomProviderSpecDefaults() {
  const spec = $("cp-adapter-type")?.value || "openai_compat";
  const isAnt = spec === "anthropic_compat";
  if ($("cp-base-url") && !$("cp-base-url").dataset.touched) {
    $("cp-base-url").placeholder = isAnt
      ? "https://api.anthropic.com/v1 (or Anthropic-compatible proxy)"
      : "https://api.openai.com/v1 (or ChatGPT/Groq/Ollama endpoint…)";
  }
  if (editingCustomProviderId === "__new__" && $("cp-auth-type")) {
    $("cp-auth-type").value = isAnt ? "header" : "bearer";
    if ($("cp-auth-header") && isAnt && !$("cp-auth-header").value) {
      $("cp-auth-header").value = "x-api-key";
    }
    syncCustomProviderAuthFields();
  }
}

function openCustomProviderForm(existingMeta) {
  editingCustomProviderId = existingMeta ? existingMeta.id : "__new__";
  renderProviderTabs();
  $("existing-provider-config-wrap")?.classList.add("hidden");
  $("custom-provider-form")?.classList.remove("hidden");

  if (existingMeta) {
    $("custom-provider-form-title").textContent = `Edit Provider · ${existingMeta.name}`;
    $("cp-adapter-type").value = existingMeta.adapter_type || "openai_compat";
    $("cp-adapter-type").disabled = true;
    $("cp-name").value = existingMeta.name || "";
    $("cp-id").value = existingMeta.id || "";
    $("cp-id").disabled = true;
    $("cp-base-url").value = existingMeta.base_url || "";
    $("cp-auth-type").value = existingMeta.auth_type || "bearer";
    $("cp-auth-header").value = existingMeta.auth_header || "";
    $("cp-api-key").value = "";
    $("cp-manual-models").value = (existingMeta.manual_models || []).join(", ");
    $("cp-enable-discovery").checked = existingMeta.models_path !== "";
    syncCustomProviderAuthFields();
  } else {
    $("custom-provider-form-title").textContent = "Add Custom API Provider";
    $("cp-adapter-type").value = "openai_compat";
    $("cp-adapter-type").disabled = false;
    $("cp-name").value = "";
    $("cp-id").value = "";
    $("cp-id").disabled = false;
    delete $("cp-id").dataset.touched;
    $("cp-base-url").value = "";
    delete $("cp-base-url").dataset.touched;
    $("cp-auth-type").value = "bearer";
    $("cp-auth-header").value = "";
    $("cp-api-key").value = "";
    $("cp-manual-models").value = "";
    $("cp-enable-discovery").checked = true;
    syncCustomProviderSpecDefaults();
  }
}

function closeCustomProviderForm() {
  editingCustomProviderId = null;
  $("custom-provider-form")?.classList.add("hidden");
  $("existing-provider-config-wrap")?.classList.remove("hidden");
  renderProviderTabs();
}

if ($("cp-adapter-type")) $("cp-adapter-type").addEventListener("change", syncCustomProviderSpecDefaults);
if ($("cp-auth-type")) $("cp-auth-type").addEventListener("change", syncCustomProviderAuthFields);
if ($("cp-base-url")) $("cp-base-url").addEventListener("input", () => { $("cp-base-url").dataset.touched = "1"; });
if ($("cp-name")) {
  $("cp-name").addEventListener("input", () => {
    if (editingCustomProviderId === "__new__" && !$("cp-id").dataset.touched) {
      $("cp-id").value = $("cp-name").value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 36);
    }
  });
}
if ($("cp-id")) $("cp-id").addEventListener("input", () => { $("cp-id").dataset.touched = "1"; });
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
      renderChatModelScopeSettings();
      ensureDefaultOrchParticipants();
      renderOrchParticipantsBuilder();
      $("setup-banner")?.classList.add("hidden");
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
      renderChatModelScopeSettings();
      renderOrchParticipantsBuilder();
      toast(`🗑 Removed ${meta.name}`);
    } catch (err) {
      toast("Delete failed: " + err.message);
    }
  };
}

/* ============================================================================
   AUTH, STATUS & PER-DEVICE RESUME
   ============================================================================ */

function showLogin() {
  $("login-screen").classList.remove("hidden");
  $("app").classList.add("hidden");
}

function showApp() {
  $("login-screen").classList.add("hidden");
  $("app").classList.remove("hidden");
}

// Show the saved page's shell at once, before any network work, so a tab
// that Chrome discarded and reloads never flashes the first page.
function showPageShell(name) {
  if (!name || !pageNames[name] || !$("page-" + name)) return;
  document.querySelectorAll(".page").forEach(p => p.classList.add("hidden"));
  $("page-" + name).classList.remove("hidden");
  document.querySelectorAll(".nav-link").forEach(b => b.classList.toggle("active", b.dataset.page === name));
  if ($("breadcrumb-page")) $("breadcrumb-page").textContent = pageNames[name];
  if (name === "orchestration" && activeOrchJobId) {
    $("orch-builder-panel")?.classList.add("hidden");
    $("orch-monitor-panel")?.classList.remove("hidden");
  }
}

/* Per-page scroll position, so coming back lands where the user left off. */
const SCROLL_KEY = "cortext_scroll_positions";
let scrollRestoring = true; // held until the boot restore finishes
function scrollKeyFor(page) {
  if (page === "orchestration" && activeOrchJobId) return "orchestration:" + activeOrchJobId;
  return page || "";
}
function readScrollMap() {
  try { return JSON.parse(localStorage.getItem(SCROLL_KEY) || "{}") || {}; } catch { return {}; }
}
function saveScrollPosition() {
  if (scrollRestoring) return;
  const page = localStorage.getItem(DEVICE_STATE_KEYS.page);
  if (!page || page === "chat") return; // chat keeps its own scroll-to-bottom behaviour
  const m = readScrollMap();
  m[scrollKeyFor(page)] = Math.round(window.scrollY || document.documentElement.scrollTop || 0);
  try { localStorage.setItem(SCROLL_KEY, JSON.stringify(m)); } catch {}
}
function restoreScrollPosition(page) {
  const y = (page && page !== "chat") ? readScrollMap()[scrollKeyFor(page)] : 0;
  if (!y) { scrollRestoring = false; return; }
  scrollRestoring = true;
  // Content may still be growing; retry a few times while it lays out.
  let tries = 0;
  const step = () => {
    window.scrollTo(0, y);
    tries++;
    if (Math.abs((window.scrollY || 0) - y) > 4 && tries < 20) {
      setTimeout(step, 100);
    } else {
      scrollRestoring = false;
    }
  };
  requestAnimationFrame(step);
}
let scrollSaveTimer = null;
window.addEventListener("scroll", () => {
  clearTimeout(scrollSaveTimer);
  scrollSaveTimer = setTimeout(saveScrollPosition, 250);
}, { passive: true });
window.addEventListener("pagehide", saveScrollPosition);
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") saveScrollPosition();
});

async function checkAuth() {
  // Read the last page BEFORE any async work: loading data can switch pages,
  // and switchPage saves to localStorage, which would otherwise overwrite
  // the page the user was on with "chat" before the restore below reads it.
  const savedPageAtLoad = localStorage.getItem(DEVICE_STATE_KEYS.page);
  showPageShell(savedPageAtLoad);

  let s;
  try {
    s = await api("/api/status");
  } catch {
    scrollRestoring = false;
    showLogin();
    return;
  }
  showApp();
  applyStatus(s);

  const savedProv = localStorage.getItem(DEVICE_STATE_KEYS.selectedProvider);
  if (savedProv && providerMap[savedProv]) {
    selectedProvider = savedProv;
  }

  // Load in parallel, and never treat a failed list as a sign-out: only a
  // 401 (handled inside api()) means the session is gone.
  await Promise.allSettled([
    loadSessions(),
    loadBotSessions(),
    loadAllConfiguredModels(),
    loadOrchJobsList()
  ]);

  if (!s.any_configured) {
    $("setup-banner")?.classList.remove("hidden");
    showSetupNudge();
  }

  const savedPage = savedPageAtLoad;
  if (savedPage && (pageNames[savedPage] || MOVED_PAGES[savedPage])) {
    await switchPage(savedPage);
  } else {
    await restoreChatLastState();
  }
  restoreScrollPosition(savedPage);
}

function applyStatus(s) {
  if (s.version && $("app-version")) $("app-version").textContent = "v" + s.version;
  if (Array.isArray(s.providers_list)) {
    hydrateProvidersFromRegistry(s.providers_list);
  } else if (s.providers) {
    Object.keys(s.providers).forEach(pid => {
      if (providerMap[pid]) providerMap[pid].configured = !!s.providers[pid];
    });
  }
  if (Array.isArray(s.personas)) {
    hydratePersonas(s.personas);
  }
  if (s.chat_model_scope && typeof s.chat_model_scope === "object") {
    chatModelScope = {
      mode: s.chat_model_scope.mode === "selected" ? "selected" : "all",
      allowed_models: Array.isArray(s.chat_model_scope.allowed_models) ? s.chat_model_scope.allowed_models : []
    };
    renderChatModelScopeSettings();
  }
  if (s.active_provider && s.active_model) {
    setActiveSelectionUI(s.active_provider, s.active_model);
  }
  updateProviderBadges();
  if (s.any_configured) {
    $("setup-banner")?.classList.add("hidden");
  }
}

function setActiveSelectionUI(provider, modelId) {
  if (!provider || !modelId) return;
  activeProvider = provider;
  activeModel = modelId;
  const info = getProviderMeta(provider);
  if ($("header-provider-name")) $("header-provider-name").textContent = info.short_name;

  const sel = $("model");
  const composite = `${provider}::${modelId}`;
  if (sel) {
    if (![...sel.options].some(o => o.value === composite)) {
      if (isModelAllowedInMainChat(provider, modelId) || sel.options.length === 0) {
        const opt = document.createElement("option");
        opt.value = composite;
        opt.textContent = `✓ [${info.short_name}] ${modelId}`;
        sel.prepend(opt);
      }
    }
    if ([...sel.options].some(o => o.value === composite)) {
      sel.value = composite;
    }
  }
}

function updateProviderBadges() {
  const anyConn = providersList.some(p => p.configured);
  const connDot = $("header-conn-dot");
  if (connDot) {
    connDot.classList.toggle("online", anyConn);
    connDot.title = anyConn ? "Connected & Ready" : "Needs API Key Setup";
  }
  renderProviderTabs();

  const cur = getProviderMeta(selectedProvider);
  const curConn = !!cur.configured;
  if ($("models-provider-status")) {
    $("models-provider-status").textContent = curConn ? "CONNECTED" : "NEEDS SETUP";
    $("models-provider-status").classList.toggle("connected", curConn);
  }
  if ($("settings-provider-state")) {
    $("settings-provider-state").textContent = curConn ? "Connected" : "Not connected";
    $("settings-provider-state").classList.toggle("connected", curConn);
  }
}

function switchProviderTab(provider) {
  if (!provider && providersList.length > 0) provider = providersList[0].id;
  selectedProvider = provider;
  if (provider) {
    try { localStorage.setItem(DEVICE_STATE_KEYS.selectedProvider, provider); } catch {}
  }
  const meta = getProviderMeta(provider);

  $("settings-provider-title").textContent = `${meta.name} Connection`;
  $("settings-provider-desc").textContent = meta.description || `Configure ${meta.name} (${meta.base_url || ""})`;

  const specBadge = $("settings-provider-spec-badge");
  if (specBadge) {
    if (meta.built_in) {
      specBadge.classList.add("hidden");
    } else {
      specBadge.classList.remove("hidden");
      specBadge.textContent = meta.adapter_type === "anthropic_compat" ? "Anthropic Spec" : "OpenAI Spec";
    }
  }

  const needsKey = meta.requires_api_key !== false && meta.auth_type !== "none";
  $("api-key-input-section")?.classList.toggle("hidden", !needsKey);
  $("api-key-label").textContent = meta.key_label || "API Key";
  $("api-key").placeholder = meta.key_placeholder || "Paste API key…";
  $("api-key").value = "";
  $("save-key-btn").textContent = needsKey ? "Save Key & Load Models ↗" : "Connect & Load Models ↗";

  $("edit-custom-provider-btn")?.classList.toggle("hidden", !!meta.built_in);
  $("delete-custom-provider-btn")?.classList.toggle("hidden", !!meta.built_in);

  $("settings-explorer-title").textContent = `Available Models · ${meta.name}`;
  const hasHelp = Boolean(meta.help_url);
  $("provider-help-row")?.classList.toggle("hidden", !hasHelp);
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

  if ($("models-feature-logo")) $("models-feature-logo").textContent = meta.icon || "⚡";
  if ($("models-feature-title")) $("models-feature-title").textContent = meta.name;
  if ($("models-feature-desc")) $("models-feature-desc").textContent = meta.description || "";

  const showFreeToggle = !!meta.supports_free_badge;
  $("settings-free-filter-wrap")?.classList.toggle("hidden", !showFreeToggle);
  $("models-free-filter-wrap")?.classList.toggle("hidden", !showFreeToggle);
  if ($("settings-free-only")) $("settings-free-only").checked = freeOnlyFilter;
  if ($("models-free-only")) $("models-free-only").checked = freeOnlyFilter;

  updateProviderBadges();

  if (meta.configured && (!meta.models || !meta.models.length)) {
    loadProviderModels(provider, false);
  } else {
    renderModelGrids();
  }
}

$("settings-free-only")?.addEventListener("change", e => {
  freeOnlyFilter = e.target.checked;
  if ($("models-free-only")) $("models-free-only").checked = freeOnlyFilter;
  renderModelGrids();
});

$("models-free-only")?.addEventListener("change", e => {
  freeOnlyFilter = e.target.checked;
  if ($("settings-free-only")) $("settings-free-only").checked = freeOnlyFilter;
  renderModelGrids();
});

function showSetupNudge() {
  if (!currentSession && chatViewMode !== "list") {
    $("chat-empty")?.classList.remove("hidden");
  }
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

/* ============================================================================
   STATEFUL WEB CHAT LIST, ACTIVE CONVERSATION & BOT SESSIONS
   ============================================================================ */

function setChatViewMode(mode) {
  chatViewMode = mode === "list" ? "list" : "active";
  try { localStorage.setItem(DEVICE_STATE_KEYS.chatView, chatViewMode); } catch {}
}

function showChatListView() {
  setChatViewMode("list");
  $("chat-active")?.classList.add("hidden");
  $("chat-empty")?.classList.add("hidden");
  $("chat-list-view")?.classList.remove("hidden");
  renderCompactChatList();
}

async function restoreChatLastState() {
  const savedMode = localStorage.getItem(DEVICE_STATE_KEYS.chatView) || "active";
  if (savedMode === "list") {
    showChatListView();
    return;
  }

  const savedSessId = localStorage.getItem(DEVICE_STATE_KEYS.session);
  if (savedSessId && sessionsCache.some(s => s.id === savedSessId)) {
    await openSession(savedSessId, false);
    return;
  }

  if (sessionsCache.length > 0) {
    showChatListView();
  } else {
    $("chat-list-view")?.classList.add("hidden");
    $("chat-active")?.classList.add("hidden");
    $("chat-empty")?.classList.remove("hidden");
  }
}

function formatCompactDate(ts) {
  if (!ts) return "";
  try {
    const d = new Date(ts.replace(" ", "T") + (ts.includes("Z") ? "" : "Z"));
    if (isNaN(d.getTime())) return "";
    const now = new Date();
    if (d.toDateString() === now.toDateString()) {
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    return d.toLocaleDateString([], { month: "short", day: "numeric" });
  } catch {
    return "";
  }
}

function renderCompactChatList() {
  const container = $("chat-list-container");
  if (!container) return;
  container.innerHTML = "";

  if (!sessionsCache.length) {
    const empty = document.createElement("div");
    empty.className = "orch-empty-note";
    empty.textContent = "No web chats yet. Click \"＋ New Chat\" above to start a conversation.";
    container.appendChild(empty);
    return;
  }

  // Pinned first, then sorted by most recent updated_at (already ordered by API, enforced client-side too)
  const sorted = [...sessionsCache].sort((a, b) => {
    if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
    return String(b.updated_at || "").localeCompare(String(a.updated_at || ""));
  });

  sorted.forEach(s => {
    const row = document.createElement("div");
    row.className = "compact-chat-row" + (s.pinned ? " is-pinned" : "");

    const main = document.createElement("div");
    main.className = "compact-chat-main";
    main.onclick = () => openSession(s.id, true);

    const titleEl = document.createElement("div");
    titleEl.className = "compact-chat-title";
    titleEl.setAttribute("dir", "auto");
    titleEl.textContent = (s.pinned ? "📌 " : "") + s.title;

    const dateEl = document.createElement("span");
    dateEl.className = "compact-chat-date";
    dateEl.textContent = formatCompactDate(s.updated_at);

    main.append(titleEl, dateEl);

    const actions = document.createElement("div");
    actions.className = "compact-chat-actions";

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
      await openSession(s.id, false);
      openRenameBar();
    };

    const delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "mini-act danger";
    delBtn.title = "Delete";
    delBtn.textContent = "🗑";
    delBtn.onclick = e => {
      e.stopPropagation();
      deleteSessionById(s.id);
    };

    actions.append(pinBtn, renBtn, delBtn);
    row.append(main, actions);
    container.appendChild(row);
  });
}

function updateCurrentSessionHeader() {
  const cur = sessionsCache.find(s => s.id === currentSession);
  resetDeleteButton();
  closeRenameBar();
  syncChatPersonaDropdown();
  syncModelDropdown();
  if (!cur) return;
  $("conversation-title").textContent = cur.title;
  $("pinned-indicator").classList.toggle("hidden", !cur.pinned);
  $("pin-session-label").textContent = cur.pinned ? "Unpin" : "Pin";
  $("pin-session-btn").classList.toggle("active-tool", !!cur.pinned);
}

async function loadSessions() {
  const sessions = await api("/api/sessions");
  sessionsCache = sessions || [];
  if ($("chat-count")) {
    $("chat-count").textContent = String(sessionsCache.length).padStart(2, "0");
  }

  const sideList = $("session-list");
  if (sideList) {
    sideList.innerHTML = "";
    sessionsCache.forEach(s => {
      const row = document.createElement("div");
      row.className = "session-row" + (s.id === currentSession && chatViewMode === "active" ? " active" : "");

      const openBtn = document.createElement("button");
      openBtn.type = "button";
      openBtn.className = "session-item";
      openBtn.setAttribute("dir", "auto");
      openBtn.textContent = (s.pinned ? "📌 " : "") + s.title;
      openBtn.onclick = () => {
        switchPage("chat", false);
        openSession(s.id);
      };
      row.appendChild(openBtn);
      sideList.appendChild(row);
    });
  }

  if (currentSession && !sessionsCache.some(s => s.id === currentSession)) {
    currentSession = null;
    try { localStorage.removeItem(DEVICE_STATE_KEYS.session); } catch {}
  }

  renderCompactChatList();
  if (chatViewMode === "active" && currentSession) {
    updateCurrentSessionHeader();
  }
}


async function loadBotSessions() {
  try {
    const bots = await api("/api/sessions?bots=true");
    botSessionsCache = bots || [];
    if ($("bot-sessions-count")) {
      $("bot-sessions-count").textContent = String(botSessionsCache.length).padStart(2, "0");
    }
    const grid = $("sessions-grid");
    if (!grid) return;
    grid.innerHTML = "";

    if (!botSessionsCache.length) {
      const empty = document.createElement("div");
      empty.className = "orch-empty-note";
      empty.textContent = "🔒 Telegram conversations are private: each one is stored encrypted with its own key and never shown here, not even to you. People can see what a bot remembers about them with /memory and erase it with /forget.";
      grid.appendChild(empty);
      return;
    }

    botSessionsCache.forEach(s => {
      const card = document.createElement("div");
      card.className = "session-card" + (s.pinned ? " is-pinned" : "");

      const cardMain = document.createElement("div");
      cardMain.className = "session-card-main";
      cardMain.onclick = async () => {
        // Preview bot transcript in a toast or modal-free view without hijacking web chat state
        try {
          const msgs = await api("/api/messages?session_id=" + encodeURIComponent(s.id));
          toast(`${s.title}: ${msgs.length} message(s) stored`);
        } catch {}
      };

      const title = document.createElement("b");
      title.setAttribute("dir", "auto");
      title.textContent = (s.pinned ? "📌 " : "") + s.title;

      const meta = document.createElement("span");
      const bot = getPersonaById(s.persona_id);
      const tgChatId = s.id.replace(/^tg-/, "");
      meta.textContent = `${bot ? `${bot.icon || "🤖"} ${bot.name}` : "Telegram Bot"} · Chat ID: ${tgChatId}`;
      cardMain.append(title, meta);

      const cardTools = document.createElement("div");
      cardTools.className = "session-card-tools";

      const cDel = document.createElement("button");
      cDel.type = "button";
      cDel.className = "tool-btn small danger";
      cDel.textContent = "🗑 Clear Bot History";
      cDel.onclick = async () => {
        try {
          await api("/api/sessions?id=" + encodeURIComponent(s.id), { method: "DELETE" });
          toast("🗑 Bot session history cleared");
          await loadBotSessions();
        } catch (err) {
          toast("Clear failed: " + err.message);
        }
      };

      cardTools.appendChild(cDel);
      card.append(cardMain, cardTools);
      grid.appendChild(card);
    });
  } catch {}
}

async function newSession(personaId = "") {
  const s = await api("/api/sessions", {
    method: "POST",
    body: JSON.stringify({ persona_id: personaId })
  });
  currentSession = s.id;
  setChatViewMode("active");
  try { localStorage.setItem(DEVICE_STATE_KEYS.session, s.id); } catch {}
  $("chat-list-view")?.classList.add("hidden");
  $("chat-empty")?.classList.add("hidden");
  $("chat-active")?.classList.remove("hidden");
  $("conversation-title").textContent = s.title || "New conversation";
  $("messages").innerHTML = "";
  await loadSessions();
  const bot = getPersonaById(s.persona_id);
  if (bot && bot.default_provider && bot.default_model) {
    const comp = `${bot.default_provider}::${bot.default_model}`;
    if ([...$("model").options].some(o => o.value === comp)) {
      setActiveSelectionUI(bot.default_provider, bot.default_model);
    }
  }
  $("message-input").focus();
}

async function openSession(id, refresh = true) {
  currentSession = id;
  setChatViewMode("active");
  try { localStorage.setItem(DEVICE_STATE_KEYS.session, id); } catch {}
  const msgs = await api("/api/messages?session_id=" + encodeURIComponent(id));
  $("chat-list-view")?.classList.add("hidden");
  $("chat-empty")?.classList.add("hidden");
  $("chat-active")?.classList.remove("hidden");
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
  $("rename-form")?.classList.add("hidden");
  $("title-display-row")?.classList.remove("hidden");
}

$("back-to-chat-list-btn")?.addEventListener("click", () => {
  showChatListView();
});

$("chat-list-new-btn")?.addEventListener("click", () => newSession());
/* ============================================================================
   SKILLS PAGE
   ============================================================================ */

function escapeHtml(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

let cachedSkillPersonas = null;

async function getSkillPersonas() {
  if (cachedSkillPersonas) return cachedSkillPersonas;
  try {
    const d = await api("/api/personas");
    cachedSkillPersonas = d.personas || [];
  } catch {
    cachedSkillPersonas = [];
  }
  return cachedSkillPersonas;
}

async function loadSkills() {
  const listEl = $("skills-list");
  if (!listEl) return;
  listEl.innerHTML = "<p class=\"muted-note\">Loading skills…</p>";
  try {
    const data = await api("/api/skills");
    const skills = data.skills || [];
    const personas = await getSkillPersonas();
    if (!skills.length) {
      listEl.innerHTML = "<p class=\"muted-note\">No skills saved yet. Teach the bot with /learn &lt;url&gt; in chat or Telegram — or /learn-deep &lt;url&gt; to learn a whole page and its same-site links.</p>";
      return;
    }
    const scopeNames = (scopes) => {
      const list = (scopes || []).map(sc => {
        if (sc === "all") return "Everyone";
        if (sc === "main") return "Main chat";
        const p = personas.find(x => x.id === sc);
        return p ? p.name : sc;
      });
      return list.length ? list.join(", ") : "Nobody";
    };
    listEl.innerHTML = skills.map(s => {
      const scs = s.scopes || [];
      const isGlobal = scs.includes("all");
      const box = (val, label) => `<label style="display:inline-flex;align-items:center;gap:4px;font-size:12.5px;color:var(--muted)"><input type="checkbox" data-scope="${escapeHtml(val)}" ${scs.includes(val) ? "checked" : ""} ${isGlobal && val !== "all" ? "disabled" : ""} onchange="changeSkillScopes('${escapeHtml(s.id)}')"> ${escapeHtml(label)}</label>`;
      const boxes = [box("all", "Everyone"), box("main", "Main chat")]
        .concat(personas.map(p => box(p.id, p.name))).join(" ");
      return `
      <div class="session-card">
        <div><b>${escapeHtml(s.name)}</b><span>${escapeHtml(s.description || "")}</span></div>
        <span>${escapeHtml(s.source_url || "")} · ${escapeHtml(s.storage_mode || "")} · ${s.enabled ? "ENABLED" : "DISABLED"} · Visible to: ${escapeHtml(scopeNames(scs))}</span>
        <div class="session-card-tools" id="skill-scopes-${escapeHtml(s.id)}">
          <span class="muted-note">Visible to:</span>
          ${boxes}
        </div>
        <div class="session-card-tools">
          <button class="btn btn-quiet btn-small" onclick="toggleSkill('${escapeHtml(s.id)}', ${!s.enabled})">${s.enabled ? "Disable" : "Enable"}</button>
          <button class="btn btn-quiet btn-small" onclick="viewSkill('${escapeHtml(s.id)}')">View</button>
          <button class="btn btn-quiet btn-small" onclick="deleteSkill('${escapeHtml(s.id)}')">Delete</button>
        </div>
        <pre id="skill-content-${escapeHtml(s.id)}" class="hidden" style="white-space:pre-wrap;word-break:break-word;font-size:12px;color:var(--muted);max-height:300px;overflow:auto;margin:0">${escapeHtml(s.content || "")}</pre>
      </div>`;
    }).join("");
  } catch (e) {
    listEl.innerHTML = "<p class=\"muted-note\">Could not load skills.</p>";
  }
}

/* ============================================================================
   PERSONAS PAGE (simple manager: list / create / edit / delete)
   The full editor (voice trainer, model allowlists, Telegram) lives in
   Settings → Chatbots & personas. This page only touches name,
   description and system instructions — and when editing it sends the
   persona's FULL stored object back (with the three fields replaced),
   so the voice blueprint, examples and other advanced settings are
   never wiped by a simple rename.
   ============================================================================ */

let editingPersonaPageId = null;
let editingPersonaPageFull = null;

async function loadPersonasPage() {
  const listEl = $("personas-page-list");
  if (!listEl) return;
  listEl.innerHTML = "<p class=\"muted-note\">Loading personas…</p>";
  try {
    const data = await api("/api/personas");
    const list = data.personas || [];
    hydratePersonas(list); // keep the Chat dropdown + Settings grid in sync
    if (!list.length) {
      listEl.innerHTML = "<p class=\"muted-note\">No personas yet. Create one — give it a name and instructions — and it appears in Chat and in the Android app.</p>";
      return;
    }
    listEl.innerHTML = list.map(p => {
      const prompt = (p.system_prompt || "");
      return `
      <div class="session-card">
        <div><b>${escapeHtml(p.icon || "🤖")} ${escapeHtml(p.name)}</b><span>${escapeHtml(p.description || "")}</span></div>
        <span>${escapeHtml(prompt.slice(0, 160))}${prompt.length > 160 ? "…" : ""}</span>
        <div class="session-card-tools">
          <button class="btn btn-quiet btn-small" onclick="openPersonaPageEditor('${escapeHtml(p.id)}')">Edit</button>
          <button class="btn btn-quiet btn-small" onclick="deletePersonaPage('${escapeHtml(p.id)}')">Delete</button>
        </div>
      </div>`;
    }).join("");
  } catch (e) {
    listEl.innerHTML = "<p class=\"muted-note\">Could not load personas.</p>";
  }
}

async function openPersonaPageEditor(id) {
  const form = $("persona-page-form");
  if (!form) return;
  editingPersonaPageId = id || null;
  editingPersonaPageFull = null;
  if (id) {
    try {
      const p = await api("/api/personas/" + encodeURIComponent(id));
      editingPersonaPageFull = p;
      $("persona-page-name").value = p.name || "";
      $("persona-page-desc").value = p.description || "";
      $("persona-page-prompt").value = p.system_prompt || "";
      $("persona-page-form-title").textContent = "Edit Persona · " + (p.name || "");
    } catch (e) {
      toast("Could not load that persona");
      return;
    }
  } else {
    $("persona-page-name").value = "";
    $("persona-page-desc").value = "";
    $("persona-page-prompt").value = "";
    $("persona-page-form-title").textContent = "New Persona";
  }
  form.classList.remove("hidden");
  form.scrollIntoView({ behavior: "smooth", block: "nearest" });
  $("persona-page-name").focus();
}

function closePersonaPageEditor() {
  $("persona-page-form")?.classList.add("hidden");
  editingPersonaPageId = null;
  editingPersonaPageFull = null;
}

async function deletePersonaPage(id) {
  const p = getPersonaById(id);
  if (!confirm(`Delete persona "${p ? p.name : id}"? Chats using it switch back to the Default Assistant.`)) return;
  try {
    const res = await api("/api/personas/" + encodeURIComponent(id), { method: "DELETE" });
    if (res.personas) hydratePersonas(res.personas);
    toast("Persona deleted");
    loadPersonasPage();
  } catch (e) {
    toast("Could not delete persona: " + (e.message || e));
  }
}

$("new-persona-page-btn")?.addEventListener("click", () => openPersonaPageEditor(null));
$("cancel-persona-page-btn")?.addEventListener("click", () => closePersonaPageEditor());
$("refresh-personas-page-btn")?.addEventListener("click", () => loadPersonasPage());
$("persona-page-form")?.addEventListener("submit", async e => {
  e.preventDefault();
  const name = $("persona-page-name").value.trim();
  if (!name) {
    toast("Give the persona a name");
    return;
  }
  const btn = $("save-persona-page-btn");
  btn.disabled = true;
  try {
    // Edit: send the full stored persona back with the three fields
    // replaced, so voice/examples/model settings survive. Create:
    // only the three fields (the rest take their defaults).
    const payload = editingPersonaPageFull
      ? { ...editingPersonaPageFull, name, description: $("persona-page-desc").value.trim(), system_prompt: $("persona-page-prompt").value.trim() }
      : { name, description: $("persona-page-desc").value.trim(), system_prompt: $("persona-page-prompt").value.trim() };
    const res = await api("/api/personas", {
      method: "POST",
      body: JSON.stringify(payload)
    });
    if (res.personas) hydratePersonas(res.personas);
    toast(editingPersonaPageId ? "Persona updated" : "Persona created");
    closePersonaPageEditor();
    loadPersonasPage();
  } catch (err) {
    toast("Could not save persona: " + (err.message || err));
  } finally {
    btn.disabled = false;
  }
});

function skillScopesState(id) {
  const boxEl = $("skill-scopes-" + id);
  if (!boxEl) return { global: false, scopes: [] };
  const everyone = boxEl.querySelector('[data-scope="all"]');
  const global = everyone ? everyone.checked : false;
  const scopes = [];
  boxEl.querySelectorAll('[data-scope]').forEach(cb => {
    if (cb.dataset.scope !== "all" && cb.checked) scopes.push(cb.dataset.scope);
  });
  return { global, scopes };
}

async function changeSkillScopes(id) {
  const st = skillScopesState(id);
  await api("/api/skills/" + encodeURIComponent(id), { method: "PUT", body: JSON.stringify({ global: st.global, scopes: st.scopes }) });
  loadSkills();
}

async function toggleSkill(id, enabled) {
  await api("/api/skills/" + encodeURIComponent(id), { method: "PUT", body: JSON.stringify({ enabled }) });
  loadSkills();
}

async function deleteSkill(id) {
  if (!confirm("Delete this skill?")) return;
  await api("/api/skills/" + encodeURIComponent(id), { method: "DELETE" });
  loadSkills();
}

function viewSkill(id) {
  const el = $("skill-content-" + id);
  if (el) el.classList.toggle("hidden");
}

async function loadMemories() {
  const listEl = $("memories-list");
  if (!listEl) return;
  listEl.innerHTML = "<p class=\"muted-note\">Loading memories…</p>";
  try {
    const data = await api("/api/memories");
    const memories = data.memories || [];
    const personas = await getSkillPersonas();
    const scopeSel = $("new-memory-scope");
    if (scopeSel) {
      const cur = scopeSel.value || "main";
      scopeSel.innerHTML = `<option value="main">Main chat</option><option value="all">Everyone</option>` +
        personas.map(p => `<option value="${escapeHtml(p.id)}">${escapeHtml(p.name)}</option>`).join("");
      scopeSel.value = [...scopeSel.options].some(o => o.value === cur) ? cur : "main";
    }
    if (!memories.length) {
      listEl.innerHTML = "<p class=\"muted-note\">No memories yet. Chat for a while and the AI will remember durable facts automatically — or add one above, or press “Extract now”.</p>";
      return;
    }
    const scopeName = (sc) => {
      if (sc === "all") return "Everyone";
      if (sc === "main") return "Main chat";
      const p = personas.find(x => x.id === sc);
      return p ? p.name : sc;
    };
    listEl.innerHTML = memories.map(m => {
      const opts = [`<option value="main" ${m.scope === "main" ? "selected" : ""}>Main chat</option>`,
        `<option value="all" ${m.scope === "all" ? "selected" : ""}>Everyone</option>`]
        .concat(personas.map(p => `<option value="${escapeHtml(p.id)}" ${m.scope === p.id ? "selected" : ""}>${escapeHtml(p.name)}</option>`)).join("");
      return `
      <div class="session-card">
        <div><b>${escapeHtml(m.text)}</b></div>
        <span>Visible to: ${escapeHtml(scopeName(m.scope))}${m.source_session ? " · from a chat" : ""} · ${escapeHtml(m.updated_at || "")}</span>
        <div class="session-card-tools">
          <span class="muted-note">Visible to:</span>
          <select id="memory-scope-${escapeHtml(m.id)}" onchange="changeMemoryScope('${escapeHtml(m.id)}')">${opts}</select>
          <button class="btn btn-quiet btn-small" onclick="deleteMemory('${escapeHtml(m.id)}')">Delete</button>
        </div>
      </div>`;
    }).join("");
  } catch (e) {
    listEl.innerHTML = "<p class=\"muted-note\">Could not load memories.</p>";
  }
}

async function changeMemoryScope(id) {
  const sel = $("memory-scope-" + id);
  if (!sel) return;
  await api("/api/memories/" + encodeURIComponent(id), { method: "PUT", body: JSON.stringify({ scope: sel.value }) });
  loadMemories();
}

async function deleteMemory(id) {
  if (!confirm("Delete this memory? The AI will no longer remember this fact.")) return;
  await api("/api/memories/" + encodeURIComponent(id), { method: "DELETE" });
  loadMemories();
}

async function addMemory() {
  const input = $("new-memory-text");
  const scopeSel = $("new-memory-scope");
  const text = input ? input.value.trim() : "";
  if (!text) return;
  await api("/api/memories", { method: "POST", body: JSON.stringify({ text, scope: scopeSel ? scopeSel.value : "main" }) });
  if (input) input.value = "";
  loadMemories();
}

async function extractMemories() {
  await api("/api/memories/extract", { method: "POST", body: JSON.stringify({}) });
  toast("Extraction started — memories will appear shortly");
  setTimeout(loadMemories, 8000);
}

$("refresh-memories-btn")?.addEventListener("click", () => {
  loadMemories();
  toast("Memories refreshed");
});

$("extract-memories-btn")?.addEventListener("click", () => {
  extractMemories();
});

$("add-memory-btn")?.addEventListener("click", () => {
  addMemory();
});

$("new-memory-text")?.addEventListener("keydown", (e) => {
  if (e.key === "Enter") addMemory();
});

/* ============================================================================
   MOBILE APP PAGE (hub-owned)
   ============================================================================ */

let mobileStatusCache = null;

// The one-liner installs (or updates) the Go relay binary on the middle
// server. It kills any old relay FIRST — re-running the old one-liner
// used to fail silently with "Address already in use" because the old
// relay still held the port — then downloads the right binary for the
// server's arch from GitHub Releases (latest) and starts it. The relay
// is a plain Go binary now, not a Python script: no python3 needed on
// the middle server, and the same one-liner both installs and updates.
function buildMobileInstaller() {
  const port = ($("mobile-relay-port")?.value || "8080").trim() || "8080";
  const host = window.location.host; // hostname:port of this hub page
  return `pkill -f cortext-relay; sleep 1; arch=$(dpkg --print-architecture 2>/dev/null || uname -m); case "$arch" in amd64|x86_64) asset=cortext-relay;; arm64|aarch64) asset=cortext-relay-arm64;; *) echo "unsupported arch: $arch"; exit 1;; esac; curl -fsSL "https://github.com/sahand363/sahand-cortext/releases/latest/download/$asset" -o /tmp/cortext-relay && chmod +x /tmp/cortext-relay && nohup /tmp/cortext-relay --hub http://${host} --port ${port} > /tmp/cortext-relay.log 2>&1 &`;
}

function timeAgoText(iso) {
  if (!iso) return null;
  const t = new Date(iso).getTime();
  if (isNaN(t)) return null;
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return s + "s ago";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m ago";
  const h = Math.floor(m / 60);
  if (h < 24) return h + "h ago";
  return Math.floor(h / 24) + "d ago";
}

async function loadMobile() {
  try {
    const s = await api("/api/mobile/status");
    mobileStatusCache = s;
    const enabled = !!s.enabled;
    const toggle = $("mobile-enabled-toggle");
    if (toggle) toggle.checked = enabled;
    if ($("mobile-enabled-label")) $("mobile-enabled-label").textContent = enabled ? "On" : "Off";
    const pill = $("mobile-enabled-pill");
    if (pill) {
      pill.textContent = enabled ? "ACTIVATED" : "NOT ACTIVATED";
      pill.classList.toggle("connected", enabled);
    }
    const hubAddr = window.location.hostname + ":" + (s.hub_port || 8787);
    if ($("mobile-hub-address")) $("mobile-hub-address").value = hubAddr;
    if ($("mobile-token")) $("mobile-token").value = s.token || "";
    const relayOn = !!(s.relay_ip && s.relay_ip.trim());
    if ($("mobile-relay-enabled")) $("mobile-relay-enabled").checked = relayOn;
    if ($("mobile-relay-ip")) $("mobile-relay-ip").value = s.relay_ip || "";
    if ($("mobile-relay-port")) $("mobile-relay-port").value = s.relay_port || 8080;
    if ($("mobile-installer")) $("mobile-installer").value = buildMobileInstaller();
    const relaySeen = s.relay_last_seen;
    const relayOk = relaySeen && (Date.now() - new Date(relaySeen).getTime() < 60000);
    if ($("mobile-relay-status")) {
      $("mobile-relay-status").textContent = relayOk
        ? "Connected" + (s.relay_id ? " · " + s.relay_id : "") + " · last seen " + (timeAgoText(relaySeen) || "just now")
        : (relaySeen ? "Not connected · last seen " + (timeAgoText(relaySeen) || "") : "Not connected");
    }
    if ($("mobile-app-status")) {
      $("mobile-app-status").textContent = s.app_last_seen
        ? "Active · last seen " + (timeAgoText(s.app_last_seen) || "just now")
        : "Never connected";
    }
  } catch (e) {
    toast("Could not load Mobile settings: " + (e.message || e));
  }
}

async function saveMobileSettings(regenerate = false) {
  const enabled = $("mobile-enabled-toggle")?.checked || false;
  const relayOn = $("mobile-relay-enabled")?.checked || false;
  const relayIp = relayOn ? ($("mobile-relay-ip")?.value || "").trim() : "";
  const relayPort = parseInt(($("mobile-relay-port")?.value || "8080").trim(), 10) || 8080;
  let s;
  try {
    s = await api("/api/mobile/settings", {
      method: "POST",
      body: JSON.stringify({ enabled, relay_ip: relayIp, relay_port: relayPort, regenerate_token: regenerate })
    });
  } catch (e) {
    // A failed save used to fail silently (no toast, no error) — the
    // toggle/fields looked changed but nothing was stored. Show the
    // error and reload the stored values so the page shows the truth.
    toast("Could not save Mobile settings: " + (e.message || e));
    loadMobile();
    return;
  }
  mobileStatusCache = s;
  toast(regenerate ? "New token generated — update the app with it" : "Mobile settings saved");
  loadMobile();
}

async function regenerateMobileToken() {
  if (!confirm("Regenerate the token? The old token stops working at once — you must update the app (and the relay) with the new one.")) return;
  await saveMobileSettings(true);
}

async function copyMobileField(id, label) {
  const el = $(id);
  if (!el || !el.value) return;
  const ok = await copyTextToClipboard(el.value);
  toast(ok ? label + " copied" : "Copy failed — select and copy manually");
}

$("mobile-enabled-toggle")?.addEventListener("change", () => { saveMobileSettings(false); });
$("mobile-relay-port")?.addEventListener("input", () => {
  const el = $("mobile-installer");
  if (el) el.value = buildMobileInstaller();
});
$("mobile-save-btn")?.addEventListener("click", () => { saveMobileSettings(false); });
$("mobile-copy-hub-btn")?.addEventListener("click", () => { copyMobileField("mobile-hub-address", "Hub address"); });
$("mobile-copy-token-btn")?.addEventListener("click", () => { copyMobileField("mobile-token", "Token"); });
$("mobile-regen-token-btn")?.addEventListener("click", () => { regenerateMobileToken(); });
$("mobile-copy-installer-btn")?.addEventListener("click", () => { copyMobileField("mobile-installer", "Installer command"); });
$("mobile-refresh-btn")?.addEventListener("click", () => { loadMobile(); toast("Status refreshed"); });

// --- Backup page -----------------------------------------------------------
// Export: GET /api/backup/export returns a .zip download. Import: POST
// /api/backup/import with a multipart file field; the hub stages the
// restore and the panel shows the exact restart step (plus the env
// one-liner when the hub could not write /etc/sahand-cortext.env itself).
// Usage & analytics page: totals, per-model and per-day breakdowns
// from GET /api/usage/summary. No cost is shown — the hub has no
// per-model pricing to compute it from.
let usageDays = 30;
async function loadUsage() {
  const daysSel = $("usage-days");
  if (daysSel) usageDays = parseInt(daysSel.value, 10) || 30;
  const tokensEl = $("usage-tokens"), reqEl = $("usage-requests"), latEl = $("usage-latency");
  const reqSub = $("usage-requests-sub"), tokSub = $("usage-tokens-sub");
  const emptyEl = $("usage-empty"), contentEl = $("usage-content");
  const modelsEl = $("usage-models-list"), barsEl = $("usage-bars"), daysEl = $("usage-days-list");
  if (!tokensEl) return;
  const fmt = n => (Number(n) || 0).toLocaleString("en-US");
  tokensEl.textContent = "…"; reqEl.textContent = "…"; latEl.textContent = "…";
  try {
    const data = await api("/api/usage/summary?days=" + usageDays);
    const t = data.totals || {};
    tokensEl.textContent = fmt(t.total_tokens);
    if (tokSub) tokSub.textContent = fmt(t.prompt_tokens) + " prompt · " + fmt(t.completion_tokens) + " completion";
    reqEl.textContent = fmt(t.requests);
    if (reqSub) reqSub.textContent = fmt(t.successful) + " successful · " + fmt(t.failed) + " failed";
    latEl.textContent = (t.requests ? Math.round(t.avg_latency_ms || 0).toLocaleString("en-US") + " ms" : "—");
    const hasData = (t.requests || 0) > 0;
    if (emptyEl) emptyEl.classList.toggle("hidden", hasData);
    if (contentEl) contentEl.classList.toggle("hidden", !hasData);
    const models = data.by_model || [];
    if (modelsEl) {
      if (!models.length) {
        modelsEl.innerHTML = "<p class=\"muted-note\">No models used in this range.</p>";
      } else {
        modelsEl.innerHTML = models.map(m =>
          "<div style=\"display:flex;justify-content:space-between;gap:8px;padding:8px 0;border-bottom:1px solid var(--line)\">" +
          "<span><b>" + escapeHtml(m.provider || "") + "</b> · " + escapeHtml(m.model || "") + "</span>" +
          "<span class=\"muted-note\">" + fmt(m.requests) + " requests · " + fmt(m.total_tokens) + " tokens · " + Math.round(m.avg_latency_ms || 0).toLocaleString("en-US") + " ms avg</span></div>"
        ).join("");
      }
    }
    const days = data.by_day || [];
    if (barsEl) {
      const last = days.slice(-30);
      const max = Math.max(1, ...last.map(d => d.total_tokens || 0));
      barsEl.innerHTML = last.map(d => {
        const h = Math.max(4, Math.round(((d.total_tokens || 0) / max) * 100));
        return "<i style=\"--h:" + h + "%;flex:1\" title=\"" + escapeHtml(d.day) + ": " + fmt(d.requests) + " requests, " + fmt(d.total_tokens) + " tokens\"></i>";
      }).join("");
    }
    if (daysEl) {
      const rows = [...days].reverse();
      daysEl.innerHTML = rows.length ? rows.map(d =>
        "<div style=\"display:flex;justify-content:space-between;gap:8px;padding:6px 0;border-bottom:1px solid var(--line)\">" +
        "<span>" + escapeHtml(d.day) + "</span>" +
        "<span class=\"muted-note\">" + fmt(d.requests) + " requests · " + fmt(d.total_tokens) + " tokens</span></div>"
      ).join("") : "<p class=\"muted-note\">No daily activity in this range.</p>";
    }
  } catch (e) {
    tokensEl.textContent = "—"; reqEl.textContent = "—"; latEl.textContent = "—";
    if (modelsEl) modelsEl.innerHTML = "<p class=\"muted-note\">Could not load usage: " + escapeHtml(e.message || e) + "</p>";
  }
}

async function exportBackup() {
  const status = $("backup-export-status");
  if (status) status.textContent = "Preparing backup…";
  try {
    const r = await fetch("/api/backup/export");
    if (!r.ok) {
      let d = {};
      try { d = await r.json(); } catch {}
      throw new Error(d.error || "Export failed");
    }
    const blob = await r.blob();
    let filename = "cortext-backup-" + new Date().toISOString().slice(0, 10) + ".zip";
    const cd = r.headers.get("Content-Disposition") || "";
    const m = cd.match(/filename="([^"]+)"/);
    if (m) filename = m[1];
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    if (status) status.textContent = "Backup downloaded: " + filename;
    toast("Backup downloaded");
  } catch (e) {
    if (status) status.textContent = "Export failed: " + e.message;
    toast("Export failed: " + e.message);
  }
}

async function importBackup() {
  const fileInput = $("backup-import-file");
  const result = $("backup-import-result");
  const file = fileInput && fileInput.files ? fileInput.files[0] : null;
  if (!file) { toast("Choose a backup .zip file first"); return; }
  if (!$("backup-import-confirm") || !$("backup-import-confirm").checked) {
    toast("Tick the confirmation box first — importing replaces everything on this hub");
    return;
  }
  if (!confirm("Restore this backup? It will REPLACE all chats, personas, skills, memories and settings on this hub.")) return;
  if (result) result.innerHTML = "Uploading and checking the backup…";
  try {
    const fd = new FormData();
    fd.append("file", file);
    const r = await fetch("/api/backup/import", { method: "POST", body: fd });
    let d = {};
    try { d = await r.json(); } catch {}
    if (!r.ok) throw new Error(d.error || "Import failed");
    let html = "<b>Backup restored and staged.</b><br>";
    html += "<span class='muted-note'>The database will become live when the hub restarts. After the restart you will be logged out — sign in with the restored password.</span><br>";
    if (d.env_applied) {
      html += "<span class='muted-note'>The login and encryption secrets were applied.</span><br>";
    } else if (d.env_oneliner) {
      html += "<span class='muted-note'>" + (d.env_note || "The secrets could not be written automatically.") + "</span><br>";
      html += "<label class='muted-note'>Run this on the server to apply the secrets and restart:</label>";
      html += "<div style='display:flex;gap:8px;margin-top:4px'><input id='backup-env-oneliner' class='form-control' readonly style='flex:1;font-family:monospace;font-size:12px'><button class='btn btn-quiet btn-small' id='backup-copy-oneliner-btn'>Copy</button></div>";
    } else if (d.env_note) {
      html += "<span class='muted-note'>" + d.env_note + "</span><br>";
    }
    html += "<label class='muted-note' style='display:block;margin-top:12px'>Restart the hub to finish:</label>";
    html += "<div style='display:flex;gap:8px;margin-top:4px'><input id='backup-restart-cmd' class='form-control' readonly style='flex:1;font-family:monospace' value='sudo systemctl restart sahand-cortext'><button class='btn btn-quiet btn-small' id='backup-copy-restart-btn'>Copy</button></div>";
    if (result) result.innerHTML = html;
    if (d.env_oneliner) {
      const el = $("backup-env-oneliner");
      if (el) el.value = d.env_oneliner;
      $("backup-copy-oneliner-btn")?.addEventListener("click", () => { copyMobileField("backup-env-oneliner", "Command"); });
    }
    $("backup-copy-restart-btn")?.addEventListener("click", () => { copyMobileField("backup-restart-cmd", "Command"); });
    toast("Backup staged — restart the hub to finish");
  } catch (e) {
    if (result) result.innerHTML = "<b>Import failed:</b> " + e.message;
    toast("Import failed: " + e.message);
  }
}

$("backup-export-btn")?.addEventListener("click", () => { exportBackup(); });
$("backup-import-btn")?.addEventListener("click", () => { importBackup(); });
$("usage-days")?.addEventListener("change", () => { loadUsage(); });
$("usage-refresh-btn")?.addEventListener("click", () => { loadUsage(); });

$("refresh-skills-btn")?.addEventListener("click", () => {
  loadSkills();
  toast("Skills refreshed");
});

$("refresh-bot-sessions-btn")?.addEventListener("click", () => {
  loadBotSessions();
  toast("Bot sessions refreshed");
});

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
    const wasCurrent = currentSession === id;
    if (wasCurrent) {
      currentSession = null;
      try { localStorage.removeItem(DEVICE_STATE_KEYS.session); } catch {}
    }
    toast("🗑 Conversation deleted");
    await loadSessions();
    if (wasCurrent) {
      showChatListView();
    }
  } catch (err) {
    toast("Delete failed: " + err.message);
  }
}

function addMessage(m) {
  const row = document.createElement("div");
  row.className = "message " + m.role;
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  bubble.setAttribute("dir", "auto");
  bubble.textContent = m.content;
  row.append(bubble);
  $("messages").appendChild(row);
}

function resizeMessageInput() {
  const el = $("message-input");
  if (!el) return;
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, 150) + "px";
}

$("message-input").addEventListener("input", resizeMessageInput);

$("new-chat").onclick = $("hero-start").onclick = $("clear-view").onclick = () => {
  switchPage("chat", false);
  newSession();
};
if ($("refresh-sessions")) $("refresh-sessions").onclick = () => loadSessions();

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
  const selectedPersonaId = $("chat-persona-select")?.value || "";
  if (!currentSession) await newSession(selectedPersonaId);
  input.value = "";
  resizeMessageInput();
  addMessage({ role: "user", content: message });
  addMessage({ role: "assistant", content: "Thinking…" });
  $("messages").scrollTop = $("messages").scrollHeight;
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
      body: JSON.stringify({
        session_id: currentSession,
        persona_id: selectedPersonaId,
        message,
        provider,
        model
      })
    });
    $("messages").lastElementChild.remove();
    addMessage({ role: "assistant", content: out.reply });
    setActiveSelectionUI(out.provider || provider, out.model || model);
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

/* ============================================================================
   PAGE NAVIGATION, MAIN CHAT MODEL ALLOWLIST & MODELS MANAGEMENT
   ============================================================================ */

function switchSettingsTab(tabId) {
  if (!tabId || !$(tabId)) return;
  document.querySelectorAll(".settings-tab").forEach(x => x.classList.toggle("active", x.dataset.tab === tabId));
  document.querySelectorAll(".settings-panel").forEach(x => x.classList.add("hidden"));
  $(tabId).classList.remove("hidden");
  try { localStorage.setItem(DEVICE_STATE_KEYS.settingsTab, tabId); } catch {}
  if (tabId === "general-settings") {
    renderChatModelScopeSettings();
  }
  if (tabId === "personas-settings") {
    renderPersonasGrid();
  }
  if (tabId === "websearch-settings") {
    loadWebSearchSettings();
  }
}

document.querySelectorAll(".settings-tab").forEach(b => b.onclick = () => switchSettingsTab(b.dataset.tab));

// Pages removed from the sidebar in v0.9.9 live in Settings now. Old links,
// saved "last page" values and bookmarks land on their Settings tab.
const MOVED_PAGES = { models: ["settings", "provider-settings"], personas: ["settings", "personas-settings"], sessions: ["chat", null] };

async function switchPage(name, autoRestoreChat = true) {
  let movedTab = null;
  if (MOVED_PAGES[name]) { movedTab = MOVED_PAGES[name][1]; name = MOVED_PAGES[name][0]; }
  if (!$("page-" + name)) name = "chat";
  if (localStorage.getItem(DEVICE_STATE_KEYS.page) !== name) saveScrollPosition();
  try { localStorage.setItem(DEVICE_STATE_KEYS.page, name); } catch {}
  document.querySelectorAll(".page").forEach(p => p.classList.add("hidden"));
  $("page-" + name).classList.remove("hidden");
  document.querySelectorAll(".nav-link").forEach(b => b.classList.toggle("active", b.dataset.page === name));
  $("breadcrumb-page").textContent = pageNames[name];
  setMobileSidebarOpen(false);

  if (name === "chat" && autoRestoreChat) {
    restoreChatLastState();
  }
  if (name === "chat") renderChatModelScopeSettings();
  if (name === "skills") loadSkills();
  if (name === "channels") loadChannelsPage();
  if (name === "memory") loadMemories();
  if (name === "usage") loadUsage();
  if (name === "mobile") loadMobile();
  if (name === "settings") {
    refreshStatus();
    if (name === "settings") {
      const savedSettingsTab = localStorage.getItem(DEVICE_STATE_KEYS.settingsTab);
      if (movedTab && $(movedTab)) {
        switchSettingsTab(movedTab);
      } else if (savedSettingsTab && $(savedSettingsTab)) {
        switchSettingsTab(savedSettingsTab);
      }
    }
    if (!editingCustomProviderId) switchProviderTab(selectedProvider);
  }
  if (name === "orchestration") {
    await initOrchestrationPage();
  } else {
    stopOrchPolling();
  }
}

document.querySelectorAll(".nav-link").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.page, true)));
document.querySelectorAll("[data-goto]").forEach(b => b.addEventListener("click", () => switchPage(b.dataset.goto, true)));
if ($("topbar-chat-btn")) $("topbar-chat-btn").onclick = () => switchPage("chat", true);
if ($("setup-open")) $("setup-open").onclick = () => switchPage("settings");

async function refreshStatus() {
  try {
    const s = await api("/api/status");
    applyStatus(s);
  } catch {}
}

async function loadAllConfiguredModels() {
  for (const p of providersList) {
    if (p.configured) {
      await loadProviderModels(p.id, false);
    }
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  renderChatModelScopeSettings();
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
    renderChatModelScopeSettings();
    if (showToastMsg) {
      toast(`Loaded ${(providerMap[provider]?.models || []).length} models from ${getProviderMeta(provider).name}`);
    }
  } catch (err) {
    if (showToastMsg) toast(`Failed to load ${getProviderMeta(provider).name} models: ` + err.message);
  }
}

function isModelAllowedInMainChat(providerId, modelId) {
  const activeBot = getActiveSessionPersona();
  if (activeBot && activeBot.model_scope_mode === "selected") {
    return (activeBot.allowed_models || []).includes(`${providerId}::${modelId}`);
  }
  if (chatModelScope.mode !== "selected") return true;
  const composite = `${providerId}::${modelId}`;
  return chatModelScope.allowed_models.includes(composite);
}

function buildUnifiedModelOptions(sel, opts = {}) {
  if (!sel) return;
  const applyChatScope = !!opts.applyChatScope;
  const prevVal = sel.value;
  sel.innerHTML = "";

  const isAllowed = (pId, mId) => !applyChatScope || isModelAllowedInMainChat(pId, mId);

  const workingGroup = document.createElement("optgroup");
  workingGroup.label = "★ Verified & Enabled Working Models";
  let hasWorking = false;

  providersList.forEach(p => {
    const short = p.short_name || p.id.toUpperCase();
    (p.models || [])
      .filter(m => m.status === "working" && m.enabled !== false && isAllowed(p.id, m.id))
      .forEach(m => {
        hasWorking = true;
        const opt = document.createElement("option");
        opt.value = `${p.id}::${m.id}`;
        opt.textContent = `✓ [${short}] ${m.display_name} (${m.latency_ms} ms)`;
        workingGroup.appendChild(opt);
      });
  });
  if (hasWorking) sel.appendChild(workingGroup);

  providersList.forEach(p => {
    const list = (p.models || []).filter(m => m.enabled !== false && isAllowed(p.id, m.id));
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

function syncModelDropdown() {
  const sel = $("model");
  if (!sel) return;
  buildUnifiedModelOptions(sel, { applyChatScope: true });

  const activeBot = getActiveSessionPersona();
  if (activeBot && activeBot.default_provider && activeBot.default_model) {
    const botComp = `${activeBot.default_provider}::${activeBot.default_model}`;
    if ([...sel.options].some(o => o.value === botComp)) {
      sel.value = botComp;
      return;
    }
  }

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

function renderChatModelScopeSettings() {
  const modeSel = $("chat-scope-mode");
  const pickerWrap = $("chat-scope-picker-wrap");
  const listEl = $("chat-scope-models-list");
  const summaryEl = $("chat-scope-summary");
  if (!modeSel || !pickerWrap || !listEl) return;

  modeSel.value = chatModelScope.mode === "selected" ? "selected" : "all";
  pickerWrap.classList.toggle("hidden", modeSel.value !== "selected");
  if (modeSel.value !== "selected") return;

  listEl.innerHTML = "";
  const allowedSet = new Set(chatModelScope.allowed_models || []);
  let totalAvailable = 0;

  providersList.forEach(p => {
    if (!p.configured) return;
    const enabledModels = (p.models || []).filter(m => m.enabled !== false);
    if (!enabledModels.length) return;

    const group = document.createElement("div");
    group.className = "chat-scope-group";

    const groupTitle = document.createElement("div");
    groupTitle.className = "chat-scope-group-title";
    groupTitle.textContent = `${p.icon || "⚡"} ${p.name}`;
    group.appendChild(groupTitle);

    enabledModels.forEach(m => {
      totalAvailable++;
      const composite = `${p.id}::${m.id}`;
      const row = document.createElement("label");
      row.className = "chat-scope-item";

      const chk = document.createElement("input");
      chk.type = "checkbox";
      chk.value = composite;
      chk.checked = allowedSet.has(composite);
      chk.onchange = () => {
        if (chk.checked) {
          allowedSet.add(composite);
        } else {
          allowedSet.delete(composite);
        }
        chatModelScope.allowed_models = [...allowedSet];
        if (summaryEl) {
          summaryEl.textContent = `${chatModelScope.allowed_models.length} of ${totalAvailable} enabled models selected for Main Chat`;
        }
      };

      const nameSpan = document.createElement("span");
      nameSpan.textContent = `${m.display_name || m.id} (${m.id})`;

      row.append(chk, nameSpan);
      if (m.status === "working") {
        const badge = document.createElement("span");
        badge.className = "model-badge badge-working";
        badge.textContent = `✓ ${m.latency_ms} ms`;
        row.appendChild(badge);
      }
      group.appendChild(row);
    });

    listEl.appendChild(group);
  });

  if (totalAvailable === 0) {
    const empty = document.createElement("div");
    empty.className = "muted-note";
    empty.textContent = "No globally enabled models loaded yet. Connect a provider and enable models in Settings → AI providers first.";
    listEl.appendChild(empty);
  } else if (summaryEl) {
    summaryEl.textContent = `${chatModelScope.allowed_models.length} of ${totalAvailable} enabled models selected for Main Chat`;
  }
}

async function persistChatModelScope() {
  const btn = $("save-chat-scope-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Saving…";
  }
  try {
    const saved = await api("/api/chat/model-scope", {
      method: "POST",
      body: JSON.stringify(chatModelScope)
    });
    chatModelScope = saved;
    syncModelDropdown();
    renderChatModelScopeSettings();
    toast(saved.mode === "selected"
      ? `✓ Main Chat restricted to ${saved.allowed_models.length} selected model(s)`
      : "✓ Main Chat set to all enabled models");
  } catch (err) {
    toast("Failed saving chat model settings: " + err.message);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = "Save Chat Models";
    }
  }
}

$("chat-scope-mode")?.addEventListener("change", async e => {
  chatModelScope.mode = e.target.value === "selected" ? "selected" : "all";
  renderChatModelScopeSettings();
  if (chatModelScope.mode === "all") {
    await persistChatModelScope();
  }
});

$("chat-scope-select-all")?.addEventListener("click", () => {
  const all = [];
  providersList.forEach(p => {
    if (!p.configured) return;
    (p.models || []).filter(m => m.enabled !== false).forEach(m => all.push(`${p.id}::${m.id}`));
  });
  chatModelScope.allowed_models = all;
  renderChatModelScopeSettings();
});

$("chat-scope-clear-all")?.addEventListener("click", () => {
  chatModelScope.allowed_models = [];
  renderChatModelScopeSettings();
});

$("save-chat-scope-btn")?.addEventListener("click", () => persistChatModelScope());

function getDisplayedModels(provider) {
  const meta = getProviderMeta(provider);
  const all = meta.models || [];
  if (meta.supports_free_badge && freeOnlyFilter) {
    return all.filter(m => m.is_free || m.status === "working");
  }
  return all;
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
  renderChatModelScopeSettings();
  renderOrchParticipantsBuilder();
}

async function batchToggleShownModels(enabled) {
  const provider = selectedProvider;
  const meta = getProviderMeta(provider);
  const shown = getDisplayedModels(provider);
  if (!shown.length) {
    toast(`No models loaded for ${meta.name} yet.`);
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
      ? `✓ Checked all ${ids.length} shown models on ${meta.name}`
      : `☐ Unchecked all ${ids.length} shown models on ${meta.name}`);
  } catch (err) {
    toast("Batch toggle failed: " + err.message);
  }
  syncModelDropdown();
  syncAuditorModelDropdown();
  renderModelGrids();
  renderChatModelScopeSettings();
  renderOrchParticipantsBuilder();
}

function renderModelGrids() {
  const provider = selectedProvider;
  const meta = getProviderMeta(provider);
  const allModels = meta.models || [];
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
        summary.textContent = `No models loaded for ${meta.name} yet. Configure your connection in Settings and click Save Key & Load Models.`;
      }
      return;
    }

    if (summary) {
      const freeNote = meta.supports_free_badge ? ` · ${freeCount} free` : "";
      const disNote = disabledCount > 0 ? ` · ${disabledCount} disabled` : "";
      summary.textContent = `${meta.name} returned ${allModels.length} models${freeNote} (showing ${shownModels.length}) · ${workingCount} working · ${failedCount} failed${disNote}. Click card to test, or uncheck Enabled to bench a model:`;
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
      if (m.is_free && meta.supports_free_badge) {
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
  const short = getProviderMeta(provider).short_name;
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
      toast(`✓ [${short}] ${modelId} works! (${res.latency_ms} ms)`);
    } else {
      toast(`✗ ${modelId} failed on ${short}`);
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
  renderChatModelScopeSettings();
  renderOrchParticipantsBuilder();
}

async function testShownModels() {
  const provider = selectedProvider;
  const meta = getProviderMeta(provider);
  const shown = getDisplayedModels(provider).filter(m => m.enabled !== false);
  if (!shown.length) {
    toast(`No enabled models shown on ${meta.name}. Check the models you want to test first.`);
    return;
  }
  toast(`Testing ${shown.length} enabled models on ${meta.name}…`);
  let foundWorking = false;
  for (const m of shown) {
    const activate = !foundWorking;
    await testSingleModel(provider, m.id, activate);
    const updated = (providerMap[provider]?.models || []).find(x => x.id === m.id);
    if (updated && updated.status === "working" && !foundWorking) {
      foundWorking = true;
    }
  }
  const workingCount = (providerMap[provider]?.models || []).filter(m => m.status === "working" && m.enabled !== false).length;
  toast(`Done! ${workingCount} enabled working models found on ${meta.name}.`);
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
  fb.textContent = `Sending request to ${meta.name} to verify connection and retrieve available models…`;

  try {
    const res = await api("/api/setup/provider", {
      method: "POST",
      body: JSON.stringify({ provider, api_key: key })
    });
    $("api-key").value = "";
    if (Array.isArray(res.providers)) {
      hydrateProvidersFromRegistry(res.providers);
    }
    if (providerMap[provider]) {
      providerMap[provider].configured = true;
      providerMap[provider].models = res.models || [];
    }
    if (res.active_provider && res.active_model) {
      setActiveSelectionUI(res.active_provider, res.active_model);
    }
    updateProviderBadges();
    syncModelDropdown();
    syncAuditorModelDropdown();
    renderModelGrids();
    renderChatModelScopeSettings();
    ensureDefaultOrchParticipants();
    $("setup-banner")?.classList.add("hidden");

    const loadedCount = (providerMap[provider]?.models || []).length;
    fb.className = "key-feedback ok";
    fb.textContent = `✓ ${meta.name} connected (${res.latency_ms} ms)! Loaded ${loadedCount} models below.`;
    toast(`${meta.name} connected! (${loadedCount} models)`);
  } catch (err) {
    fb.className = "key-feedback err";
    fb.textContent = `✗ ${meta.name} error: ${err.message}`;
  } finally {
    btn.disabled = false;
    const needsKey = meta.requires_api_key !== false && meta.auth_type !== "none";
    btn.textContent = needsKey ? "Save Key & Load Models ↗" : "Connect & Load Models ↗";
  }
});

if ($("custom-model-form")) {
  $("custom-model-form").addEventListener("submit", async e => {
    e.preventDefault();
    const provider = selectedProvider;
    const meta = getProviderMeta(provider);
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
      if (providerMap[provider]) {
        providerMap[provider].models = res.models || [];
      }
      if (res.ok && res.active_provider && res.active_model) {
        setActiveSelectionUI(res.active_provider, res.active_model);
        toast(`✓ [${meta.short_name}] ${model} verified working (${res.latency_ms} ms)!`);
      } else {
        toast(`Added ${model}, test returned: ${res.error || "failed"}`);
      }
      syncModelDropdown();
      syncAuditorModelDropdown();
      renderModelGrids();
      renderChatModelScopeSettings();
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
if ($("refresh-models-page")) $("refresh-models-page").onclick = () => loadProviderModels(selectedProvider, true);
$("test-all-models-btn").onclick = () => testShownModels();
if ($("test-all-models-page")) $("test-all-models-page").onclick = () => testShownModels();
if ($("check-all-models-btn")) $("check-all-models-btn").onclick = () => batchToggleShownModels(true);
if ($("uncheck-all-models-btn")) $("uncheck-all-models-btn").onclick = () => batchToggleShownModels(false);
if ($("check-all-models-page")) $("check-all-models-page").onclick = () => batchToggleShownModels(true);
if ($("uncheck-all-models-page")) $("uncheck-all-models-page").onclick = () => batchToggleShownModels(false);

$("toggle-key").onclick = () => {
  const i = $("api-key");
  i.type = i.type === "password" ? "text" : "password";
  $("toggle-key").textContent = i.type === "password" ? "Show" : "Hide";
};

/* ============================================================================
   AI ORCHESTRATION FRONTEND INTEGRATION (WITH PERSONAS & 50-CHAR TRANSCRIPT)
   ============================================================================ */

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

function setOrchSubView(viewName) {
  const v = viewName === "messages" ? "messages" : "events";
  orchActiveSubView = v;
  document.querySelectorAll(".orch-view-tab").forEach(b => b.classList.toggle("active", b.dataset.orchview === v));
  $("orch-messages-view")?.classList.toggle("hidden", v !== "messages");
  $("orch-events-view")?.classList.toggle("hidden", v !== "events");
  try { localStorage.setItem(DEVICE_STATE_KEYS.orchSubView, v); } catch {}
}

document.querySelectorAll(".orch-view-tab").forEach(btn => {
  btn.onclick = () => setOrchSubView(btn.dataset.orchview);
});

async function initOrchestrationPage() {
  ensureDefaultOrchParticipants();
  syncAuditorModelDropdown();
  renderOrchParticipantsBuilder();
  setOrchSubView(localStorage.getItem(DEVICE_STATE_KEYS.orchSubView) || orchActiveSubView);
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
  try { localStorage.removeItem(DEVICE_STATE_KEYS.orchJob); } catch {}
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

    const botWrap = document.createElement("label");
    const botSpan = document.createElement("span");
    botSpan.textContent = "Chatbot Persona (Optional)";
    botWrap.appendChild(botSpan);

    const botSelect = document.createElement("select");
    const defaultBotOpt = document.createElement("option");
    defaultBotOpt.value = "";
    defaultBotOpt.textContent = "Standard Role Prompt";
    botSelect.appendChild(defaultBotOpt);

    personasList.filter(b => b.enabled !== false).forEach(b => {
      const o = document.createElement("option");
      o.value = b.id;
      o.textContent = `${b.icon || "🤖"} ${b.name}`;
      if (part.persona_id === b.id) o.selected = true;
      botSelect.appendChild(o);
    });

    const provWrap = document.createElement("label");
    const provSpan = document.createElement("span");
    provSpan.textContent = "Provider";
    provWrap.appendChild(provSpan);

    const provSelect = document.createElement("select");
    providersList.forEach(provObj => {
      const opt = document.createElement("option");
      opt.value = provObj.id;
      opt.textContent = `${provObj.name}${provObj.configured ? " (Connected)" : " (Needs Setup)"}`;
      if (part.provider === provObj.id) opt.selected = true;
      provSelect.appendChild(opt);
    });

    const modWrap = document.createElement("label");
    const modSpan = document.createElement("span");
    modSpan.textContent = "Model";
    modWrap.appendChild(modSpan);

    const modSelect = document.createElement("select");

    function populateModelsForPart() {
      modSelect.innerHTML = "";
      const p = part.provider;
      const meta = getProviderMeta(p);
      const list = (meta.models || []).filter(m => m.enabled !== false);
      if (!list.length) {
        const fallbackOpt = document.createElement("option");
        fallbackOpt.value = part.model || "";
        fallbackOpt.textContent = part.model ? `${part.model} (Configure or enable models in Settings)` : "No enabled models — check Settings → AI providers";
        modSelect.appendChild(fallbackOpt);
        return;
      }

      const working = list.filter(m => m.status === "working");
      const others = meta.supports_free_badge
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
          const freeTag = (meta.supports_free_badge && m.is_free) ? " [FREE]" : "";
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

    botSelect.onchange = () => {
      part.persona_id = botSelect.value;
      const chosenBot = getPersonaById(part.persona_id);
      if (chosenBot && chosenBot.default_provider && chosenBot.default_model) {
        part.provider = chosenBot.default_provider;
        part.model = chosenBot.default_model;
        provSelect.value = part.provider;
        populateModelsForPart();
      }
    };
    provSelect.onchange = () => {
      part.provider = provSelect.value;
      part.model = getDefaultModelForProvider(part.provider);
      populateModelsForPart();
    };
    modSelect.onchange = () => {
      part.model = modSelect.value;
    };

    populateModelsForPart();
    botWrap.appendChild(botSelect);
    provWrap.appendChild(provSelect);
    modWrap.appendChild(modSelect);
    gridRow.append(botWrap, provWrap, modWrap);

    row.append(topBar, gridRow);
    container.appendChild(row);
  });
}

$("orch-add-participant").onclick = () => {
  const configured = getConfiguredProvidersList();
  const defProv = configured[0] || activeProvider || providersList[0]?.id || "";
  const defMod = getDefaultModelForProvider(defProv) || activeModel || "";
  orchParticipantsDraft.push({
    role: "worker",
    name: "Worker / Developer",
    provider: defProv,
    model: defMod,
    persona_id: ""
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
    const meta = getProviderMeta(p.provider);
    if (!meta.configured) {
      errBanner.textContent = `Participant #${i + 1} uses ${meta.name}, which is not configured yet. Configure it in Settings first.`;
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
        participants: orchParticipantsDraft.map((p, idx) => {
          const bot = getPersonaById(p.persona_id);
          const baseName = p.role === "coordinator"
            ? "Coordinator / Architect"
            : p.role === "auditor"
              ? "Quality Assurance / Auditor"
              : "Worker / Developer";
          return {
            role: p.role,
            name: bot ? `${bot.icon || "🤖"} ${bot.name} (${baseName})` : baseName,
            provider: p.provider,
            model: p.model,
            system_prompt: bot ? (bot.voice_blueprint || bot.system_prompt || "") : "",
            sort_order: idx
          };
        })
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
  try { localStorage.setItem(DEVICE_STATE_KEYS.orchJob, jobId); } catch {}
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
    if (err.status === 404) {
      showOrchBuilder();
      return;
    }
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
    const charLen = m.content_length > 0 ? m.content_length : (m.content || "").length;
    st.chars += charLen;
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
    const provShort = getProviderMeta(e.provider).short_name;

    const label = document.createElement("span");
    const boldModel = document.createElement("b");
    boldModel.textContent = e.model;
    const metaSmall = document.createElement("small");
    metaSmall.style.color = "var(--muted)";
    metaSmall.textContent = ` (${provShort} · ${roleShort}) `;
    const pctSpan = document.createElement("span");
    pctSpan.style.fontWeight = "700";
    pctSpan.textContent = pctStr;
    label.append(boldModel, metaSmall, pctSpan);

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
    toast(`✓ Updated Participant #${slotIdx + 1} to ${getProviderMeta(newProv).short_name} · ${newMod}`);
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
    const activeProvName = invokeMeta.provider ? getProviderMeta(invokeMeta.provider).name : "";
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
      $("orch-active-headline").textContent = `Coordinator (${activeModelLabel || (coordPart ? `${getProviderMeta(coordPart.provider).name} · ${coordPart.model}` : "Architect")}) is creating the implementation plan…`;
      $("orch-active-subline").textContent = "Analyzing task and preparing instructions for Worker.";
    } else if (stage === "working") {
      $("orch-active-headline").textContent = `Worker (${activeModelLabel || (scheduledWorker ? `${getProviderMeta(scheduledWorker.provider).name} · ${scheduledWorker.model}` : "Developer")}) is executing round ${curRound}…`;
      $("orch-active-subline").textContent = "Producing deliverable based on Coordinator, QA Auditor, and User instructions.";
    } else if (stage === "auditing") {
      $("orch-active-headline").textContent = `QA Auditor (${activeModelLabel || (auditorPart ? `${getProviderMeta(auditorPart.provider).name} · ${auditorPart.model}` : "Auditor")}) is inspecting round ${curRound}…`;
      $("orch-active-subline").textContent = "Testing Worker deliverable for runtime bugs, missing files, and edge cases before Coordinator review.";
    } else if (stage === "reviewing") {
      $("orch-active-headline").textContent = `Coordinator (${activeModelLabel || (coordPart ? `${getProviderMeta(coordPart.provider).name} · ${coordPart.model}` : "Architect")}) is reviewing round ${curRound}…`;
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
    let roleLabel = p.name || "Worker / Developer";
    if (!p.name) {
      if (p.role === "coordinator") roleLabel = "Coordinator / Architect";
      if (p.role === "auditor") roleLabel = "Quality Assurance / Auditor";
    }
    const provName = getProviderMeta(p.provider).name;
    const modelMsgs = messages.filter(m => m.provider === p.provider && m.model === p.model && m.sender_role === p.role).length;
    const failCount = slotFailures[`slot_${i}`] || slotFailures[`${p.provider}::${p.model}`] || 0;

    const headerDiv = document.createElement("div");
    const roleSpan = document.createElement("span");
    roleSpan.className = "roster-role";
    roleSpan.textContent = `#${i + 1} ${roleLabel} · ${modelMsgs} msg${modelMsgs === 1 ? "" : "s"}`;

    if (isWorkingNow) {
      const stateSpan = document.createElement("span");
      stateSpan.style.color = "var(--accent)";
      stateSpan.style.fontWeight = "700";
      stateSpan.textContent = " · ⚡ WORKING NOW";
      roleSpan.appendChild(stateSpan);
    } else if (failCount > 0) {
      const failSpan = document.createElement("span");
      failSpan.style.color = "var(--err)";
      failSpan.style.fontWeight = "700";
      failSpan.textContent = ` · ✗ ${failCount} fail`;
      roleSpan.appendChild(failSpan);
    }

    const modelBold = document.createElement("b");
    modelBold.textContent = `${provName} · ${p.model}`;
    headerDiv.append(roleSpan, modelBold);
    pill.appendChild(headerDiv);

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

        const provShort = getProviderMeta(latestAuditMsg.provider).name;
        $("orch-audit-model-title").textContent = `${provShort} · ${latestAuditMsg.model} (${latestAuditMsg.latency_ms || 0} ms)`;
        $("orch-audit-summary").textContent = latestAuditMsg.completion_summary || "See full auditor report below.";

        const applyAuditBtn = $("orch-apply-audit-btn");
        const cachedAuditContent = latestAuditMsg.has_full_content
          ? latestAuditMsg.content
          : (loadedOrchMessagesCache.get(`${d.id}:${latestAuditMsg.id}`) || "");
        $("orch-audit-body").textContent = cachedAuditContent || latestAuditMsg.preview || "Click to load full audit report…";

        const auditDetailsEl = auditReportEl.querySelector("details");
        if (auditDetailsEl && !latestAuditMsg.has_full_content && !loadedOrchMessagesCache.has(`${d.id}:${latestAuditMsg.id}`) && latestAuditMsg.id) {
          auditDetailsEl.ontoggle = async () => {
            if (!auditDetailsEl.open || loadedOrchMessagesCache.has(`${d.id}:${latestAuditMsg.id}`)) return;
            try {
              const fullAudit = await api(`/api/orchestration/jobs/${encodeURIComponent(d.id)}/messages/${encodeURIComponent(latestAuditMsg.id)}`);
              loadedOrchMessagesCache.set(`${d.id}:${latestAuditMsg.id}`, fullAudit.content || "");
              $("orch-audit-body").textContent = fullAudit.content || "";
              if (applyAuditBtn) {
                const hasCode = fullAudit.action === "revise" || (fullAudit.content || "").includes("=== FILE:") || (fullAudit.content || "").includes("```");
                applyAuditBtn.classList.toggle("hidden", !hasCode);
              }
            } catch {}
          };
        }

        if (applyAuditBtn) {
          const hasCodeInAudit = latestAuditMsg.action === "revise" || cachedAuditContent.includes("=== FILE:") || cachedAuditContent.includes("```");
          applyAuditBtn.classList.toggle("hidden", !hasCodeInAudit);
          applyAuditBtn.onclick = () => applyAuditorFixes(d.id, latestAuditMsg.id);
        }

        const sendToWorkerBtn = $("orch-send-audit-worker-btn");
        if (sendToWorkerBtn) {
          sendToWorkerBtn.onclick = () => {
            const summaryText = latestAuditMsg.completion_summary || cachedAuditContent || latestAuditMsg.preview;
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

        const fnameBold = document.createElement("b");
        fnameBold.textContent = `📄 ${f.filename}`;
        const sizeSmall = document.createElement("small");
        sizeSmall.textContent = `${sizeKb} KB`;
        selectBtn.append(fnameBold, sizeSmall);

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
        sumNote.innerHTML = "";
        const labelBold = document.createElement("b");
        labelBold.textContent = "Coordinator Summary: ";
        const s = document.createElement("span");
        s.textContent = parsedPkg.summary;
        sumNote.append(labelBold, s);
      } else {
        sumNote.classList.add("hidden");
      }
    }

    const zipBtn = $("orch-download-zip-btn");
    if (zipBtn) {
      zipBtn.textContent = `📦 Download Project (.zip · ${files.length} file${files.length === 1 ? "" : "s"})`;
      zipBtn.onclick = () => downloadProjectZip(files, orchZipName(d));
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

  // Render Collapsible Collaboration Transcript with 50-char previews & click-to-load full message
  $("orch-msg-count").textContent = String(messages.length);
  const msgContainer = $("orch-messages-view");
  const openTurnKeys = new Set(
    [...msgContainer.querySelectorAll("details[open]")].map(el => el.dataset.turnKey)
  );
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
      const turnKey = m.id ? `id_${m.id}` : `idx_${idx}`;
      const cacheKey = `${d.id}:${m.id}`;

      const detailsEl = document.createElement("details");
      detailsEl.className = `orch-msg-collapsible role-${m.sender_role}`;
      detailsEl.dataset.turnKey = turnKey;
      if (openTurnKeys.has(turnKey)) {
        detailsEl.open = true;
      }

      const summaryEl = document.createElement("summary");

      const left = document.createElement("div");
      left.className = "orch-msg-sender";
      let roleTitle = "WORKER / DEVELOPER";
      if (m.sender_role === "coordinator") roleTitle = "COORDINATOR / ARCHITECT";
      if (m.sender_role === "auditor") roleTitle = "QUALITY ASSURANCE / AUDITOR";
      if (m.sender_role === "user") roleTitle = "WORKSPACE OWNER (USER REVISION)";

      const provShort = m.sender_role === "user"
        ? "HUMAN FEEDBACK"
        : getProviderMeta(m.provider).short_name;
      const modelLabel = m.sender_role === "user" ? "Interactive Revision" : `${provShort} · ${m.model}`;

      const roleBadge = document.createElement("span");
      roleBadge.className = `orch-role-tag ${m.sender_role}`;
      roleBadge.textContent = `#${idx + 1} · ${roleTitle}`;
      const modelBold = document.createElement("b");
      modelBold.textContent = modelLabel;
      left.append(roleBadge, modelBold);

      const rawPreview = m.preview || m.completion_summary || (m.sender_role === "user" ? stripRevOptsTag(m.content) : m.content) || "";
      const normalizedPreview = rawPreview.replace(/\s+/g, " ").trim();
      const preview50 = [...normalizedPreview].slice(0, 50).join("");
      const sizeHint = m.content_length > 0 ? ` (${(m.content_length / 1024).toFixed(1)} KB)` : "";

      const previewSpan = document.createElement("span");
      previewSpan.className = "orch-msg-preview";
      previewSpan.textContent = preview50
        ? `— ${preview50}${[...normalizedPreview].length > 50 || m.content_length > 50 ? "…" : ""}${sizeHint}`
        : `(click to load${sizeHint})`;

      const right = document.createElement("div");
      right.className = "orch-msg-meta";
      const roundSpan = document.createElement("span");
      roundSpan.textContent = `Round ${m.round}`;
      const actSpan = document.createElement("span");
      actSpan.className = `action-tag action-${m.action}`;
      actSpan.textContent = (m.action || "continue").toUpperCase();
      right.append(roundSpan, document.createTextNode(" · "), actSpan);
      if (m.latency_ms) {
        const latSpan = document.createElement("span");
        latSpan.textContent = `${(m.latency_ms / 1000).toFixed(1)}s`;
        right.append(document.createTextNode(" · "), latSpan);
      }

      summaryEl.append(left, previewSpan, right);

      const innerCard = document.createElement("div");
      innerCard.style.padding = "12px 14px";

      const renderLoadedTurnBody = fullText => {
        innerCard.innerHTML = "";
        const displayContent = m.sender_role === "user" ? stripRevOptsTag(fullText) : fullText;

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
            downloadProjectZip(turnFiles, orchZipName(d, `turn${idx + 1}`));
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
          const sumBold = document.createElement("b");
          sumBold.textContent = "Summary: ";
          const span = document.createElement("span");
          span.textContent = m.completion_summary;
          sum.append(sumBold, span);
          innerCard.appendChild(sum);
        }
      };

      const initialFull = m.has_full_content ? m.content : loadedOrchMessagesCache.get(cacheKey);
      if (initialFull !== undefined) {
        renderLoadedTurnBody(initialFull);
      } else {
        const placeholder = document.createElement("div");
        placeholder.className = "muted-note";
        placeholder.textContent = "Click to load full message from database…";
        innerCard.appendChild(placeholder);
      }

      let loadingTurn = false;
      const loadFullTurnIfNeeded = async () => {
        if (!detailsEl.open || loadingTurn) return;
        if (m.has_full_content || loadedOrchMessagesCache.has(cacheKey)) return;
        if (!m.id) return;

        loadingTurn = true;
        innerCard.innerHTML = "";
        const loadingNote = document.createElement("div");
        loadingNote.className = "muted-note";
        loadingNote.textContent = "Loading full message from database…";
        innerCard.appendChild(loadingNote);

        try {
          const fullMsg = await api(`/api/orchestration/jobs/${encodeURIComponent(d.id)}/messages/${encodeURIComponent(m.id)}`);
          loadedOrchMessagesCache.set(cacheKey, fullMsg.content || "");
          renderLoadedTurnBody(fullMsg.content || "");
        } catch (err) {
          innerCard.innerHTML = "";
          const errNote = document.createElement("div");
          errNote.className = "orch-error-banner";
          errNote.textContent = "Failed loading message: " + err.message;
          innerCard.appendChild(errNote);
        } finally {
          loadingTurn = false;
        }
      };

      detailsEl.addEventListener("toggle", loadFullTurnIfNeeded);
      if (detailsEl.open && initialFull === undefined) {
        loadFullTurnIfNeeded();
      }

      detailsEl.append(summaryEl, innerCard);
      msgContainer.appendChild(detailsEl);
    });
  }

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
  const provName = p.provider ? getProviderMeta(p.provider).name : "";
  const fromProv = p.from_provider ? getProviderMeta(p.from_provider).name : "";
  const toProv = p.to_provider ? getProviderMeta(p.to_provider).name : "";
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
    case "participant_cooldown":
      return p.model
        ? `${roundPrefix}⏸ ${(p.role || "Model").toUpperCase()} (${provName} · ${p.model}) is cooling down after a rate limit (${p.wait_seconds || "?"}s left) — backup takes this turn.`
        : `${roundPrefix}⏸ All models for this step are cooling down — waiting ${p.wait_seconds || "?"}s for the first one to be ready.`;
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
    participant_cooldown: "Cooling Down",
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

document.addEventListener("keydown", e => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    switchPage("chat", false);
    newSession();
  }
  if (e.key === "Escape") {
    setMobileSidebarOpen(false);
    closeRenameBar();
    resetDeleteButton();
  }
});

checkAuth();


/* ============================================================================
   CHANNELS (Telegram channel studio)
   ============================================================================ */

const CHANNEL_KEY = "cortext_active_channel";
let channelsCache = [];
let activeChannelId = localStorage.getItem(CHANNEL_KEY) || "";
let editingChannelId = null;

const postStatusLabel = {
  draft: "Draft", pending: "Waiting in Telegram", posted: "Posted",
  discarded: "Discarded", removed: "Removed from channel"
};

async function loadChannelsPage() {
  const empty = $("channel-empty");
  try {
    const d = await api("/api/channels");
    channelsCache = d.channels || [];
  } catch (e) {
    // Say what failed on the page itself; a toast is easy to miss.
    $("channel-body").classList.add("hidden");
    empty.classList.remove("hidden");
    empty.innerHTML = `Couldn't load your channels: ${escapeHtml(e.message)} <button class="btn btn-quiet btn-small" onclick="loadChannelsPage()">Retry</button>`;
    return;
  }
  const picker = $("channel-picker");
  if (!channelsCache.some(c => c.id === activeChannelId)) activeChannelId = channelsCache[0] ? channelsCache[0].id : "";
  try { localStorage.setItem(CHANNEL_KEY, activeChannelId); } catch {}
  picker.innerHTML = channelsCache.map(c => `<option value="${escapeHtml(c.id)}">${escapeHtml(c.name)}</option>`).join("");
  picker.value = activeChannelId;
  picker.classList.toggle("hidden", channelsCache.length < 2);
  $("edit-channel-btn").classList.toggle("hidden", !activeChannelId);
  empty.textContent = "No channels yet. Press “＋ New channel” to connect one.";
  empty.classList.toggle("hidden", channelsCache.length > 0 || !$("channel-form").classList.contains("hidden"));
  $("channel-body").classList.toggle("hidden", !activeChannelId);
  if (activeChannelId) await loadActiveChannel();
}

async function loadActiveChannel() {
  const id = activeChannelId;
  const c = channelsCache.find(x => x.id === id);
  if (!c) return;
  // Draw everything from what we already have, then check Telegram in the
  // background: a slow Telegram must never leave the page empty.
  renderChannelStatus(c, null);
  renderChannelStyle(c);
  const check = api("/api/channels/" + encodeURIComponent(id)).then(d => {
    if (id !== activeChannelId) return;
    const i = channelsCache.findIndex(x => x.id === id);
    if (i >= 0) channelsCache[i] = d.channel;
    renderChannelStatus(d.channel, d.check);
  }).catch(e => {
    if (id === activeChannelId) renderChannelStatus(c, { problem: "Couldn't check the bot: " + e.message });
  });
  await Promise.all([check, loadChannelPosts()]);
}

function channelCheckRow(ok, text) {
  return `<div style="margin:3px 0">${ok ? "✅" : "⬜"} ${text}</div>`;
}

function renderChannelStatus(c, chk) {
  const checking = !chk;
  chk = chk || {};
  const bot = c.bot_username ? "@" + escapeHtml(c.bot_username) : "the bot";
  const startLink = c.bot_username ? `https://t.me/${encodeURIComponent(c.bot_username)}?start=${encodeURIComponent(c.pair_code)}` : "";
  let html = `<div style="display:flex;justify-content:space-between;gap:8px;flex-wrap:wrap"><b>${escapeHtml(c.name)}</b><span class="muted-note">${escapeHtml(c.chat_ref)} · ${bot}</span></div>`;
  html += channelCheckRow(!!chk.bot_username, "Bot token works");
  html += channelCheckRow(chk.is_admin, "Bot is an admin of " + escapeHtml(chk.channel_title || c.chat_ref));
  html += channelCheckRow(chk.can_post, "Bot can post messages");
  if (c.owner_id) {
    html += channelCheckRow(true, "Approvals go to " + escapeHtml(c.owner_name || "you") + ` <button class="btn btn-quiet btn-small" onclick="unpairChannel()">Unpair</button>`);
  } else {
    html += channelCheckRow(false, `Approvals in Telegram: ${startLink ? `<a href="${startLink}" target="_blank" rel="noopener">open ${bot}</a> and press Start` : "send"} (or send <code>/start ${escapeHtml(c.pair_code)}</code>)`);
  }
  if (checking) html += `<div class="muted-note" style="margin-top:6px">Checking the bot with Telegram…</div>`;
  else if (chk.problem) html += `<div class="muted-note" style="margin-top:6px">👉 ${escapeHtml(chk.problem)}</div>`;
  else html += `<div class="muted-note" style="margin-top:6px">✅ Ready. Write a post below.</div>`;
  $("channel-status").innerHTML = html;
}

function renderChannelStyle(c) {
  $("channel-style-guide").value = c.style_guide || "";
  const n = (c.examples || []).length;
  $("channel-style-summary").textContent = c.style_guide ? `· learned, ${n} example post${n === 1 ? "" : "s"}` : `· not learned yet${n ? `, ${n} example post${n === 1 ? "" : "s"}` : ""}`;
}

async function loadChannelPosts() {
  const el = $("channel-posts");
  try {
    const d = await api(`/api/channels/${encodeURIComponent(activeChannelId)}/posts`);
    const posts = d.posts || [];
    if (!posts.length) { el.innerHTML = ""; return; }
    el.innerHTML = posts.map(renderChannelPost).join("");
  } catch (e) { el.innerHTML = `<p class="muted-note">${escapeHtml(e.message)}</p>`; }
}

function channelPublicLink(post) {
  const c = channelsCache.find(x => x.id === activeChannelId);
  if (!c || !post.tg_message_id || !c.chat_ref.startsWith("@")) return "";
  return `https://t.me/${encodeURIComponent(c.chat_ref.slice(1))}/${post.tg_message_id}`;
}

function renderChannelPost(p) {
  const id = escapeHtml(p.id);
  const live = p.status === "draft" || p.status === "pending" || p.status === "discarded";
  const link = channelPublicLink(p);
  let tools = "";
  if (live) {
    tools = `
      <button class="btn btn-quiet btn-small" onclick="saveChannelDraft('${id}')">Save edit</button>
      <button class="btn btn-quiet btn-small" onclick="regenerateChannelPost('${id}')">↻ Rewrite</button>
      <button class="btn btn-quiet btn-small" onclick="channelPostAction('${id}','approve')">📨 ${p.status === "pending" ? "Resend" : "Send"} to Telegram</button>
      <button class="btn btn-primary btn-small" onclick="publishChannelPost('${id}')">Post now</button>
      ${p.status !== "discarded" ? `<button class="btn btn-quiet btn-small" onclick="channelPostAction('${id}','discard')">Discard</button>` : ""}`;
  } else if (p.status === "posted") {
    tools = `${link ? `<a class="btn btn-quiet btn-small" href="${link}" target="_blank" rel="noopener">Open post ↗</a>` : ""}
      <button class="btn btn-quiet btn-small" onclick="unpublishChannelPost('${id}')">Remove from channel</button>`;
  }
  tools += `<button class="btn btn-quiet btn-small" onclick="deleteChannelPost('${id}')">Delete</button>`;
  const body = live
    ? `<textarea id="cpost-${id}" dir="auto" rows="${Math.min(14, Math.max(4, (p.draft.match(/\n/g) || []).length + 3))}" class="form-control">${escapeHtml(p.draft)}</textarea>`
    : `<div dir="auto" style="white-space:pre-wrap">${escapeHtml(p.draft)}</div>`;
  return `
    <div class="session-card" style="${p.status === "discarded" || p.status === "removed" ? "opacity:.6" : ""}">
      <div style="display:flex;justify-content:space-between;gap:8px;flex-wrap:wrap">
        <b dir="auto">${escapeHtml(p.idea)}</b>
        <span class="status-pill ${p.status === "posted" ? "connected" : ""}">${escapeHtml(postStatusLabel[p.status] || p.status)}</span>
      </div>
      ${body}
      ${p.error ? `<div class="muted-note">⚠️ ${escapeHtml(p.error)}</div>` : ""}
      <span>${escapeHtml(p.posted_at ? "Posted " + p.posted_at + " UTC" : "Created " + (p.created_at || "") + " UTC")}</span>
      <div class="session-card-tools" style="flex-wrap:wrap">${tools}</div>
    </div>`;
}

async function channelPostAction(id, action, body, busyMsg) {
  if (busyMsg) toast(busyMsg);
  try {
    await api(`/api/channel-posts/${encodeURIComponent(id)}/${action}`, { method: "POST", body: JSON.stringify(body || {}) });
    if (action === "approve") toast("Sent to your Telegram for approval");
    if (action === "publish") toast("Posted to the channel ✅");
  } catch (e) { toast(e.message); }
  await loadChannelPosts();
}

async function saveChannelDraft(id, quiet) {
  const ta = $("cpost-" + id);
  if (!ta) return true;
  try {
    await api(`/api/channel-posts/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify({ draft: ta.value }) });
    if (!quiet) toast("Saved");
    return true;
  } catch (e) { toast(e.message); return false; }
}

async function regenerateChannelPost(id) {
  const fb = prompt("What should change? (leave empty for a fresh version)", "");
  if (fb === null) return;
  await channelPostAction(id, "regenerate", { feedback: fb }, "Rewriting…");
}

async function publishChannelPost(id) {
  if (!confirm("Post this to the channel now?")) return;
  if (!(await saveChannelDraft(id, true))) return;
  await channelPostAction(id, "publish", {}, "Posting…");
}

async function unpublishChannelPost(id) {
  if (!confirm("Delete this post from the channel?")) return;
  await channelPostAction(id, "unpublish");
}

async function deleteChannelPost(id) {
  if (!confirm("Delete this draft from the hub? (It stays on the channel if it was posted.)")) return;
  try { await api(`/api/channel-posts/${encodeURIComponent(id)}`, { method: "DELETE" }); } catch (e) { toast(e.message); }
  await loadChannelPosts();
}

async function unpairChannel() {
  if (!confirm("Stop sending approvals to this Telegram account?")) return;
  try { await api(`/api/channels/${encodeURIComponent(activeChannelId)}/unpair`, { method: "POST", body: "{}" }); } catch (e) { toast(e.message); }
  await loadActiveChannel();
}

async function openChannelForm(channel) {
  editingChannelId = channel ? channel.id : null;
  $("channel-form-title").textContent = channel ? "Channel setup" : "New channel";
  $("channel-name").value = channel ? channel.name : "";
  $("channel-ref").value = channel ? channel.chat_ref : "";
  $("channel-token").value = "";
  $("channel-token").placeholder = channel && channel.has_token ? "Saved. Paste a new one to replace it" : "123456:ABC…";
  $("channel-instructions").value = channel ? channel.instructions || "" : "";
  const personas = await getSkillPersonas();
  $("channel-persona").innerHTML = `<option value="">Main chat model (no persona)</option>` +
    personas.map(p => `<option value="${escapeHtml(p.id)}">${escapeHtml(p.name)}</option>`).join("");
  $("channel-persona").value = channel ? channel.persona_id || "" : "";
  $("delete-channel-btn").classList.toggle("hidden", !channel);
  $("channel-form").classList.remove("hidden");
  $("channel-empty").classList.add("hidden");
}

if ($("new-channel-btn")) {
  $("new-channel-btn").onclick = () => openChannelForm(null);
  $("edit-channel-btn").onclick = () => openChannelForm(channelsCache.find(c => c.id === activeChannelId));
  $("cancel-channel-btn").onclick = () => { $("channel-form").classList.add("hidden"); loadChannelsPage(); };
  $("channel-picker").onchange = (e) => {
    activeChannelId = e.target.value;
    try { localStorage.setItem(CHANNEL_KEY, activeChannelId); } catch {}
    loadActiveChannel();
  };
  $("channel-form").onsubmit = async (e) => {
    e.preventDefault();
    const btn = $("save-channel-btn");
    btn.disabled = true;
    const existing = channelsCache.find(c => c.id === editingChannelId);
    const body = {
      name: $("channel-name").value, chat_ref: $("channel-ref").value, persona_id: $("channel-persona").value,
      bot_token: $("channel-token").value, instructions: $("channel-instructions").value,
      style_guide: existing ? existing.style_guide : undefined
    };
    try {
      const d = editingChannelId
        ? await api("/api/channels/" + encodeURIComponent(editingChannelId), { method: "PUT", body: JSON.stringify(body) })
        : await api("/api/channels", { method: "POST", body: JSON.stringify(body) });
      activeChannelId = d.channel.id;
      try { localStorage.setItem(CHANNEL_KEY, activeChannelId); } catch {}
      $("channel-token").value = "";
      $("channel-form").classList.add("hidden");
      toast("Channel saved");
      await loadChannelsPage();
    } catch (err) { toast(err.message); }
    btn.disabled = false;
  };
  $("delete-channel-btn").onclick = async () => {
    if (!confirm("Delete this channel and all its drafts from the hub? Posts already on Telegram stay.")) return;
    try { await api("/api/channels/" + encodeURIComponent(editingChannelId), { method: "DELETE" }); } catch (e) { toast(e.message); return; }
    $("channel-form").classList.add("hidden");
    activeChannelId = "";
    await loadChannelsPage();
  };
  $("save-channel-style-btn").onclick = async () => {
    const c = channelsCache.find(x => x.id === activeChannelId);
    if (!c) return;
    try {
      await api("/api/channels/" + encodeURIComponent(c.id), { method: "PUT", body: JSON.stringify({
        name: c.name, chat_ref: c.chat_ref, persona_id: c.persona_id, instructions: c.instructions,
        style_guide: $("channel-style-guide").value }) });
      toast("Style guide saved");
      await loadChannelsPage();
    } catch (e) { toast(e.message); }
  };
  $("learn-channel-style-btn").onclick = async () => {
    const btn = $("learn-channel-style-btn");
    const samples = $("channel-samples").value.split(/\n\s*---+\s*\n/).map(s => s.trim()).filter(Boolean);
    btn.disabled = true;
    $("channel-learn-status").textContent = "Reading posts and learning the style… (up to a minute)";
    try {
      const d = await api(`/api/channels/${encodeURIComponent(activeChannelId)}/learn`, { method: "POST", body: JSON.stringify({ samples }) });
      $("channel-learn-status").textContent = `Learned from ${d.learned_from} post${d.learned_from === 1 ? "" : "s"}.`;
      $("channel-samples").value = "";
      renderChannelStyle(d.channel);
      const i = channelsCache.findIndex(x => x.id === d.channel.id);
      if (i >= 0) channelsCache[i] = d.channel;
    } catch (e) { $("channel-learn-status").textContent = e.message; }
    btn.disabled = false;
  };
  $("write-channel-post-btn").onclick = async () => {
    const idea = $("channel-idea").value.trim();
    if (!idea) { toast("Write what the post is about"); return; }
    const btn = $("write-channel-post-btn");
    btn.disabled = true;
    $("channel-write-status").textContent = "Writing…";
    try {
      await api(`/api/channels/${encodeURIComponent(activeChannelId)}/posts`, { method: "POST", body: JSON.stringify({ idea, notes: $("channel-notes").value }) });
      $("channel-idea").value = ""; $("channel-notes").value = "";
      $("channel-write-status").textContent = "";
      await loadChannelPosts();
    } catch (e) { $("channel-write-status").textContent = e.message; }
    btn.disabled = false;
  };
}


/* ============================================================================
   WEB SEARCH SETTINGS (v0.10.0)
   ============================================================================ */

function formatNoteBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

async function loadWebSearchSettings() {
  const sel = $("websearch-mode-select");
  const meta = $("websearch-notes-meta");
  const list = $("websearch-notes-list");
  if (!sel || !list) return;
  try {
    const d = await api("/api/websearch");
    sel.value = d.mode || "auto";
    meta.textContent = d.count
      ? `${d.count} note${d.count === 1 ? "" : "s"} · ${formatNoteBytes(d.bytes || 0)} · the oldest unused ones are dropped after ${d.max_notes}.`
      : "No notes yet. They appear here after the first search.";
    list.innerHTML = "";
    (d.notes || []).forEach(n => {
      const card = document.createElement("div");
      card.className = "websearch-note";
      const head = document.createElement("div");
      head.className = "websearch-note-head";
      const q = document.createElement("b");
      q.setAttribute("dir", "auto");
      q.textContent = n.query;
      const right = document.createElement("small");
      right.textContent = `${formatShortTimestamp(n.created_at)} · used ${n.uses || 0}× `;
      const del = document.createElement("button");
      del.type = "button";
      del.className = "mini-act danger";
      del.textContent = "Delete";
      del.onclick = async () => {
        try {
          await api(`/api/websearch/notes/${encodeURIComponent(n.id)}`, { method: "DELETE" });
          card.remove();
          toast("Note deleted");
          loadWebSearchSettings();
        } catch (e) { toast(e.message || "Could not delete"); }
      };
      right.appendChild(del);
      head.append(q, right);
      const body = document.createElement("pre");
      body.setAttribute("dir", "auto");
      body.textContent = n.summary;
      card.append(head, body);
      (n.sources || []).slice(0, 6).forEach((s, i) => {
        const line = document.createElement("div");
        line.className = "src";
        const a = document.createElement("a");
        a.href = s.url;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        a.textContent = `[${i + 1}] ${s.title || s.url}`;
        line.appendChild(a);
        card.appendChild(line);
      });
      list.appendChild(card);
    });
  } catch (e) {
    if (meta) meta.textContent = "Could not load: " + (e.message || e);
  }
}

if ($("websearch-mode-select")) {
  $("websearch-mode-select").onchange = async e => {
    try {
      const d = await api("/api/websearch", { method: "PUT", body: JSON.stringify({ mode: e.target.value }) });
      const label = { auto: "Auto + /search", command: "Only /search", off: "Off" }[d.mode] || d.mode;
      toast(`Web search: ${label}`);
    } catch (err) { toast(err.message || "Could not save"); }
  };
}
if ($("websearch-clear-btn")) {
  $("websearch-clear-btn").onclick = async () => {
    if (!confirm("Delete all saved web notes? Telegram chats' own notes are not affected.")) return;
    try {
      const d = await api("/api/websearch/notes", { method: "DELETE" });
      toast(`Deleted ${d.deleted || 0} note(s)`);
      loadWebSearchSettings();
    } catch (e) { toast(e.message || "Could not clear"); }
  };
}
