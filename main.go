package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jchv/go-webview2"
)

// AppConfig stores persistent local settings on Windows
type AppConfig struct {
	GeminiAPIKey    string      `json:"gemini_api_key"`
	GeminiModel     string      `json:"gemini_model"`
	AvailableModels []ModelInfo `json:"available_models"`
	Verified        bool        `json:"verified"`
	LastTestedAt    string      `json:"last_tested_at"`
}

// ModelInfo holds metadata from Google's /v1beta/models endpoint
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type VerifyRequest struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

type VerifyResponse struct {
	Valid           bool        `json:"valid"`
	Message         string      `json:"message"`
	ModelUsed       string      `json:"model_used"`
	LatencyMs       int64       `json:"latency_ms"`
	Reply           string      `json:"reply"`
	AvailableModels []ModelInfo `json:"available_models"`
}

func getConfigPath() string {
	appData, err := os.UserConfigDir()
	if err != nil {
		return "cortext_config.json"
	}
	dir := filepath.Join(appData, "SahandCorText")
	_ = os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "config.json")
}

func defaultModels() []ModelInfo {
	return []ModelInfo{
		{ID: "gemini-3.8-flash", DisplayName: "Gemini 3.8 Flash (Fastest & Newest Workhorse)"},
		{ID: "gemini-4-argon", DisplayName: "Gemini 4 Argon (Flagship Deep Reasoning)"},
		{ID: "gemini-3.5-flash", DisplayName: "Gemini 3.5 Flash"},
	}
}

func loadConfig() AppConfig {
	cfg := AppConfig{
		GeminiModel:     "gemini-3.8-flash",
		AvailableModels: defaultModels(),
	}
	data, err := os.ReadFile(getConfigPath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	// Migrate away from retired 2.5 default if stored in an older config file
	if cfg.GeminiModel == "" || strings.Contains(cfg.GeminiModel, "2.5") {
		cfg.GeminiModel = "gemini-3.8-flash"
	}
	if len(cfg.AvailableModels) == 0 {
		cfg.AvailableModels = defaultModels()
	}
	return cfg
}

func saveConfig(cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

// scoreModel ranks models so the newest Flash (3.8) and Argon appear at the very top
func scoreModel(id string) int {
	switch {
	case id == "gemini-3.8-flash":
		return 1000
	case strings.HasPrefix(id, "gemini-3.8-flash"):
		return 950
	case id == "gemini-4-argon":
		return 900
	case strings.HasPrefix(id, "gemini-4"):
		return 850
	case strings.HasPrefix(id, "gemini-3.8"):
		return 800
	case strings.HasPrefix(id, "gemini-3.7-flash"):
		return 750
	case strings.HasPrefix(id, "gemini-3.5-flash"):
		return 700
	case strings.HasPrefix(id, "gemini-3"):
		return 600
	default:
		return 100
	}
}

// fetchAvailableGeminiModels queries Google's live catalog for models supporting generateContent
func fetchAvailableGeminiModels(apiKey string) ([]ModelInfo, string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, "", fmt.Errorf("API key is required to fetch models")
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s&pageSize=100", apiKey)
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("Network error contacting Google API: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Google API returned HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", err
	}

	var list []ModelInfo
	for _, m := range parsed.Models {
		supportsGen := false
		for _, method := range m.SupportedGenerationMethods {
			if method == "generateContent" {
				supportsGen = true
				break
			}
		}
		if !supportsGen {
			continue
		}

		id := strings.TrimPrefix(m.Name, "models/")
		// Filter out retired 1.5 / 2.0 / 2.5 models and non-text specialized endpoints
		if strings.Contains(id, "1.5") || strings.Contains(id, "2.0") || strings.Contains(id, "2.5") ||
			strings.Contains(id, "embedding") || strings.Contains(id, "imagen") || strings.Contains(id, "veo") {
			continue
		}

		display := m.DisplayName
		if display == "" {
			display = id
		} else {
			display = fmt.Sprintf("%s (%s)", m.DisplayName, id)
		}

		list = append(list, ModelInfo{
			ID:          id,
			DisplayName: display,
		})
	}

	if len(list) == 0 {
		list = defaultModels()
	}

	// Sort models so gemini-3.8-flash and gemini-4-argon are first
	sort.Slice(list, func(i, j int) bool {
		si, sj := scoreModel(list[i].ID), scoreModel(list[j].ID)
		if si != sj {
			return si > sj
		}
		return list[i].ID < list[j].ID
	})

	bestDefault := list[0].ID
	return list, bestDefault, nil
}

// verifyGeminiKey discovers live models and verifies generation with the selected model
func verifyGeminiKey(apiKey, model string) VerifyResponse {
	start := time.Now()
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)

	if apiKey == "" {
		return VerifyResponse{
			Valid:   false,
			Message: "Please enter a Gemini API key first.",
		}
	}

	// Step 1: Discover live models available to this API key
	discoveredModels, bestModel, _ := fetchAvailableGeminiModels(apiKey)
	if len(discoveredModels) == 0 {
		discoveredModels = defaultModels()
	}

	// If no model specified or retired 2.5 model selected, auto-switch to best available (e.g. gemini-3.8-flash)
	if model == "" || strings.Contains(model, "2.5") {
		if bestModel != "" {
			model = bestModel
		} else {
			model = "gemini-3.8-flash"
		}
	}

	// Step 2: Test generateContent on the target model
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)

	// Note: gemini-4-argon uses thinking tokens and ignores temperature, so we keep payload clean
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": "Reply with the single word: ONLINE"},
				},
			},
		},
	}

	bodyBytes, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return VerifyResponse{Valid: false, Message: "Request build error: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return VerifyResponse{
			Valid:           false,
			LatencyMs:       latency,
			AvailableModels: discoveredModels,
			Message:         "Network error reaching Gemini API: " + err.Error(),
		}
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errObj map[string]interface{}
		_ = json.Unmarshal(respData, &errObj)
		errMsg := fmt.Sprintf("HTTP %d error from Google API.", resp.StatusCode)
		if e, ok := errObj["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return VerifyResponse{
			Valid:           false,
			LatencyMs:       latency,
			ModelUsed:       model,
			AvailableModels: discoveredModels,
			Message:         errMsg,
		}
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	_ = json.Unmarshal(respData, &parsed)

	replyText := "ONLINE"
	if len(parsed.Candidates) > 0 {
		for _, p := range parsed.Candidates[0].Content.Parts {
			if strings.TrimSpace(p.Text) != "" {
				replyText = strings.TrimSpace(p.Text)
			}
		}
	}

	_ = saveConfig(AppConfig{
		GeminiAPIKey:    apiKey,
		GeminiModel:     model,
		AvailableModels: discoveredModels,
		Verified:        true,
		LastTestedAt:    time.Now().Format("2006-01-02 15:04:05"),
	})

	return VerifyResponse{
		Valid:           true,
		Message:         fmt.Sprintf("Verified & locked onto %s!", model),
		ModelUsed:       model,
		LatencyMs:       latency,
		Reply:           replyText,
		AvailableModels: discoveredModels,
	}
}

func startLocalServer() (string, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(uiHTML))
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(loadConfig())
	})

	mux.HandleFunc("/api/fetch-models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req VerifyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		models, best, err := fetchAvailableGeminiModels(req.APIKey)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      false,
				"message": err.Error(),
				"models":  defaultModels(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":         true,
			"models":     models,
			"best_model": best,
		})
	})

	mux.HandleFunc("/api/verify-gemini", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req VerifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res := verifyGeminiKey(req.APIKey, req.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	go func() {
		_ = http.Serve(listener, mux)
	}()

	return fmt.Sprintf("http://%s", listener.Addr().String()), nil
}

func main() {
	serverURL, err := startLocalServer()
	if err != nil {
		return
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     true,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Sahand CorText — Personal AI Engine (Phase 1)",
			Width:  920,
			Height: 650,
		},
	})

	if w == nil {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", serverURL).Start()
		select {}
	}

	defer w.Destroy()
	w.SetSize(920, 650, webview2.HintNone)
	w.Navigate(serverURL)
	w.Run()
}

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Sahand CorText</title>
<style>
  :root {
    --bg: #0f172a;
    --panel: #1e293b;
    --border: #334155;
    --text: #f8fafc;
    --muted: #94a3b8;
    --accent: #3b82f6;
    --accent-hover: #2563eb;
    --success: #10b981;
    --danger: #ef4444;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: 'Segoe UI', system-ui, sans-serif; }
  body {
    background: var(--bg);
    color: var(--text);
    display: flex;
    height: 100vh;
    overflow: hidden;
  }
  .sidebar {
    width: 240px;
    background: #0b1120;
    border-right: 1px solid var(--border);
    padding: 24px 16px;
    display: flex;
    flex-direction: column;
    justify-content: space-between;
  }
  .brand {
    font-size: 18px;
    font-weight: 700;
    letter-spacing: 0.5px;
    color: #fff;
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .brand-dot {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: var(--accent);
    box-shadow: 0 0 10px var(--accent);
  }
  .nav-item {
    margin-top: 24px;
    padding: 12px 14px;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    font-size: 14px;
    font-weight: 600;
    color: #fff;
  }
  .version-tag {
    font-size: 12px;
    color: var(--muted);
    line-height: 1.5;
  }
  .content {
    flex: 1;
    padding: 40px;
    overflow-y: auto;
    display: flex;
    justify-content: center;
    align-items: flex-start;
  }
  .card {
    width: 100%;
    max-width: 580px;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 32px;
    box-shadow: 0 10px 30px rgba(0,0,0,0.35);
  }
  .card-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 8px;
  }
  .card-title { font-size: 20px; font-weight: 700; }
  .badge {
    font-size: 12px;
    font-weight: 600;
    padding: 4px 10px;
    border-radius: 999px;
    background: #334155;
    color: var(--muted);
  }
  .badge.ok { background: rgba(16, 185, 129, 0.15); color: var(--success); border: 1px solid rgba(16,185,129,0.4); }
  .badge.err { background: rgba(239, 68, 68, 0.15); color: var(--danger); border: 1px solid rgba(239,68,68,0.4); }
  .subtitle {
    font-size: 13px;
    color: var(--muted);
    margin-bottom: 24px;
    line-height: 1.5;
  }
  label {
    display: flex;
    justify-content: space-between;
    align-items: center;
    font-size: 13px;
    font-weight: 600;
    margin-bottom: 8px;
    color: #cbd5e1;
  }
  .input-group {
    display: flex;
    gap: 8px;
    margin-bottom: 20px;
  }
  input, select {
    width: 100%;
    padding: 11px 14px;
    background: #0f172a;
    border: 1px solid var(--border);
    border-radius: 8px;
    color: #fff;
    font-size: 14px;
    outline: none;
  }
  input:focus, select:focus { border-color: var(--accent); }
  .secondary-btn {
    padding: 0 14px;
    background: #0f172a;
    border: 1px solid var(--border);
    color: #cbd5e1;
    border-radius: 8px;
    cursor: pointer;
    font-size: 13px;
    white-space: nowrap;
    transition: all 0.15s;
  }
  .secondary-btn:hover { border-color: var(--accent); color: #fff; }
  .primary-btn {
    width: 100%;
    padding: 13px;
    background: var(--accent);
    color: #fff;
    border: none;
    border-radius: 8px;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.15s;
    margin-top: 8px;
  }
  .primary-btn:hover { background: var(--accent-hover); }
  .primary-btn:disabled { opacity: 0.6; cursor: not-allowed; }
  .result-box {
    margin-top: 22px;
    padding: 16px;
    border-radius: 8px;
    background: #0b1120;
    border: 1px solid var(--border);
    font-size: 13px;
    display: none;
    line-height: 1.6;
  }
  .telemetry {
    margin-top: 10px;
    padding-top: 10px;
    border-top: 1px solid #1e293b;
    display: flex;
    justify-content: space-between;
    color: var(--muted);
    font-family: Consolas, monospace;
    font-size: 12px;
  }
</style>
</head>
<body>
  <div class="sidebar">
    <div>
      <div class="brand"><span class="brand-dot"></span> sahand-cortext</div>
      <div class="nav-item">1. Gemini API & Model Discovery</div>
    </div>
    <div class="version-tag">Phase 1 • Auto-Discovery<br>Default: gemini-3.8-flash</div>
  </div>

  <div class="content">
    <div class="card">
      <div class="card-header">
        <div class="card-title">Primary Brain: Gemini API</div>
        <span id="statusBadge" class="badge">Not Verified</span>
      </div>
      <p class="subtitle">
        Enter your Google Gemini API key. Click <b>Auto-Detect Models</b> to pull every live model enabled on your key (prioritizing <b>gemini-3.8-flash</b> and <b>gemini-4-argon</b>), then click Verify.
      </p>

      <label for="apiKey">Gemini API Key</label>
      <div class="input-group">
        <input type="password" id="apiKey" placeholder="AIzaSy..." autocomplete="off" />
        <button type="button" class="secondary-btn" onclick="toggleKey(this)">Show</button>
      </div>

      <label for="modelSelect">
        <span>Target Gemini Model</span>
        <span id="modelCount" style="color: var(--muted); font-weight: 400; font-size: 12px;">Default presets loaded</span>
      </label>
      <div class="input-group">
        <select id="modelSelect"></select>
        <button type="button" id="detectBtn" class="secondary-btn" onclick="detectModels()">↻ Auto-Detect Models</button>
      </div>

      <button id="verifyBtn" class="primary-btn" onclick="verifyKey()">Verify & Save Gemini API Key</button>

      <div id="resultBox" class="result-box">
        <div id="resultMsg"></div>
        <div class="telemetry">
          <span id="latencyText">Latency: -- ms</span>
          <span id="replyText">Response: --</span>
        </div>
      </div>
    </div>
  </div>

<script>
  function populateDropdown(models, selectedId) {
    const sel = document.getElementById('modelSelect');
    sel.innerHTML = '';
    models.forEach(m => {
      const opt = document.createElement('option');
      opt.value = m.id;
      opt.textContent = m.display_name || m.id;
      sel.appendChild(opt);
    });
    if (selectedId && models.some(m => m.id === selectedId)) {
      sel.value = selectedId;
    } else if (models.length > 0) {
      sel.value = models[0].id;
    }
    document.getElementById('modelCount').textContent = models.length + ' active models available';
  }

  window.addEventListener('DOMContentLoaded', async () => {
    try {
      const res = await fetch('/api/config');
      const cfg = await res.json();
      if (cfg.gemini_api_key) {
        document.getElementById('apiKey').value = cfg.gemini_api_key;
      }
      populateDropdown(cfg.available_models || [], cfg.gemini_model);
      if (cfg.verified) {
        const badge = document.getElementById('statusBadge');
        badge.textContent = 'Verified: ' + cfg.gemini_model;
        badge.className = 'badge ok';
      }
    } catch (e) {}
  });

  function toggleKey(btn) {
    const input = document.getElementById('apiKey');
    if (input.type === 'password') {
      input.type = 'text';
      btn.textContent = 'Hide';
    } else {
      input.type = 'password';
      btn.textContent = 'Show';
    }
  }

  async function detectModels() {
    const apiKey = document.getElementById('apiKey').value.trim();
    const btn = document.getElementById('detectBtn');
    const countLabel = document.getElementById('modelCount');
    if (!apiKey) {
      alert('Please enter your Gemini API key first.');
      return;
    }
    btn.disabled = true;
    btn.textContent = 'Scanning...';
    countLabel.textContent = 'Querying /v1beta/models...';

    try {
      const res = await fetch('/api/fetch-models', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey })
      });
      const data = await res.json();
      if (data.ok && data.models) {
        populateDropdown(data.models, data.best_model);
      } else {
        countLabel.textContent = 'Discovery failed: ' + (data.message || 'Check key');
      }
    } catch (e) {
      countLabel.textContent = 'Error scanning models';
    } finally {
      btn.disabled = false;
      btn.textContent = '↻ Auto-Detect Models';
    }
  }

  async function verifyKey() {
    const apiKey = document.getElementById('apiKey').value.trim();
    const model = document.getElementById('modelSelect').value;
    const btn = document.getElementById('verifyBtn');
    const badge = document.getElementById('statusBadge');
    const box = document.getElementById('resultBox');
    const msg = document.getElementById('resultMsg');
    const lat = document.getElementById('latencyText');
    const rep = document.getElementById('replyText');

    btn.disabled = true;
    btn.textContent = 'Discovering & Verifying...';
    box.style.display = 'block';
    msg.style.color = '#94a3b8';
    msg.textContent = 'Testing live connection with ' + (model || 'gemini-3.8-flash') + '...';

    try {
      const response = await fetch('/api/verify-gemini', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey, model: model })
      });
      const data = await response.json();

      if (data.available_models && data.available_models.length > 0) {
        populateDropdown(data.available_models, data.model_used);
      }

      if (data.valid) {
        badge.textContent = 'Active: ' + data.model_used;
        badge.className = 'badge ok';
        msg.style.color = '#10b981';
        msg.textContent = '✔ ' + data.message;
        lat.textContent = 'Latency: ' + data.latency_ms + ' ms';
        rep.textContent = 'Model Reply: "' + data.reply + '"';
      } else {
        badge.textContent = 'Verification Failed';
        badge.className = 'badge err';
        msg.style.color = '#ef4444';
        msg.textContent = '✖ ' + data.message;
        lat.textContent = 'Latency: ' + (data.latency_ms || 0) + ' ms';
        rep.textContent = 'Status: Rejected';
      }
    } catch (err) {
      badge.textContent = 'Error';
      badge.className = 'badge err';
      msg.style.color = '#ef4444';
      msg.textContent = '✖ Local engine error: ' + err.message;
    } finally {
      btn.disabled = false;
      btn.textContent = 'Verify & Save Gemini API Key';
    }
  }
</script>
</body>
</html>
`
