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
	"strings"
	"time"

	"github.com/jchv/go-webview2"
)

// AppConfig stores persistent local settings on Windows
type AppConfig struct {
	GeminiAPIKey string `json:"gemini_api_key"`
	GeminiModel  string `json:"gemini_model"`
	Verified     bool   `json:"verified"`
	LastTestedAt string `json:"last_tested_at"`
}

// VerifyRequest represents the payload from the UI "Verify" button
type VerifyRequest struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

// VerifyResponse is returned to the UI with verification telemetry
type VerifyResponse struct {
	Valid     bool   `json:"valid"`
	Message   string `json:"message"`
	ModelUsed string `json:"model_used"`
	LatencyMs int64  `json:"latency_ms"`
	Reply     string `json:"reply"`
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

func loadConfig() AppConfig {
	cfg := AppConfig{
		GeminiModel: "gemini-2.5-flash",
	}
	data, err := os.ReadFile(getConfigPath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.GeminiModel == "" {
		cfg.GeminiModel = "gemini-2.5-flash"
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

// verifyGeminiKey calls the official Gemini API with a minimal 5-token test prompt
func verifyGeminiKey(apiKey, model string) VerifyResponse {
	start := time.Now()
	apiKey = strings.TrimSpace(apiKey)
	if model == "" {
		model = "gemini-2.5-flash"
	}

	if apiKey == "" {
		return VerifyResponse{
			Valid:   false,
			Message: "Please enter a Gemini API key first.",
		}
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)

	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]string{
					{"text": "Reply with the exact single word: ONLINE"},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"maxOutputTokens": 10,
			"temperature":     0.0,
		},
	}

	bodyBytes, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return VerifyResponse{Valid: false, Message: "Failed to build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return VerifyResponse{
			Valid:     false,
			LatencyMs: latency,
			Message:   "Network error reaching Gemini API (check internet/VPN): " + err.Error(),
		}
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errObj map[string]interface{}
		_ = json.Unmarshal(respData, &errObj)
		errMsg := fmt.Sprintf("HTTP %d: Invalid API key or quota error.", resp.StatusCode)
		if e, ok := errObj["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return VerifyResponse{
			Valid:     false,
			LatencyMs: latency,
			ModelUsed: model,
			Message:   errMsg,
		}
	}

	// Extract model reply
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
	if len(parsed.Candidates) > 0 && len(parsed.Candidates[0].Content.Parts) > 0 {
		replyText = strings.TrimSpace(parsed.Candidates[0].Content.Parts[0].Text)
	}

	// Save verified key to local disk
	_ = saveConfig(AppConfig{
		GeminiAPIKey: apiKey,
		GeminiModel:  model,
		Verified:     true,
		LastTestedAt: time.Now().Format("2006-01-02 15:04:05"),
	})

	return VerifyResponse{
		Valid:     true,
		Message:   "Gemini API Key verified and saved successfully!",
		ModelUsed: model,
		LatencyMs: latency,
		Reply:     replyText,
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
			Width:  900,
			Height: 620,
		},
	})

	if w == nil {
		// Fallback if WebView2 runtime is missing on the machine
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", serverURL).Start()
		select {}
	}

	defer w.Destroy()
	w.SetSize(900, 620, webview2.HintNone)
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
    max-width: 560px;
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
    display: block;
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
  .toggle-btn {
    padding: 0 14px;
    background: #0f172a;
    border: 1px solid var(--border);
    color: var(--muted);
    border-radius: 8px;
    cursor: pointer;
    font-size: 13px;
  }
  .toggle-btn:hover { color: #fff; }
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
      <div class="nav-item">1. Gemini API Setup</div>
    </div>
    <div class="version-tag">Phase 1 • Step 1 Build<br>Engine: Go + WebView2</div>
  </div>

  <div class="content">
    <div class="card">
      <div class="card-header">
        <div class="card-title">Primary Brain: Gemini API</div>
        <span id="statusBadge" class="badge">Not Verified</span>
      </div>
      <p class="subtitle">
        Enter your Google Gemini API key below. Clicking Verify will send a live test pulse to Google's servers, measure response latency, and save your key locally.
      </p>

      <label for="apiKey">Gemini API Key</label>
      <div class="input-group">
        <input type="password" id="apiKey" placeholder="AIzaSy..." autocomplete="off" />
        <button type="button" class="toggle-btn" onclick="toggleKey()">Show</button>
      </div>

      <label for="modelSelect">Target Gemini Model</label>
      <div class="input-group">
        <select id="modelSelect">
          <option value="gemini-2.5-flash">gemini-2.5-flash (Fastest & Recommended)</option>
          <option value="gemini-2.5-pro">gemini-2.5-pro (Deep Reasoning & Coding)</option>
        </select>
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
  window.addEventListener('DOMContentLoaded', async () => {
    try {
      const res = await fetch('/api/config');
      const cfg = await res.json();
      if (cfg.gemini_api_key) {
        document.getElementById('apiKey').value = cfg.gemini_api_key;
      }
      if (cfg.gemini_model) {
        document.getElementById('modelSelect').value = cfg.gemini_model;
      }
      if (cfg.verified) {
        const badge = document.getElementById('statusBadge');
        badge.textContent = 'Verified & Saved';
        badge.className = 'badge ok';
      }
    } catch (e) {}
  });

  function toggleKey() {
    const input = document.getElementById('apiKey');
    const btn = event.target;
    if (input.type === 'password') {
      input.type = 'text';
      btn.textContent = 'Hide';
    } else {
      input.type = 'password';
      btn.textContent = 'Show';
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
    btn.textContent = 'Contacting Gemini API...';
    box.style.display = 'block';
    msg.style.color = '#94a3b8';
    msg.textContent = 'Sending test prompt to ' + model + '...';

    try {
      const response = await fetch('/api/verify-gemini', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey, model: model })
      });
      const data = await response.json();

      if (data.valid) {
        badge.textContent = 'Verified & Active';
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
