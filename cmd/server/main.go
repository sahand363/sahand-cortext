package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type App struct {
	db       *sql.DB
	mu       sync.RWMutex
	sessions map[string]time.Time
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

type Message struct {
	ID        int64  `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

type ModelInfo struct {
	Provider    string  `json:"provider"`
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Description string  `json:"description"`
	ContextLen  int     `json:"context_len"`
	IsFree      bool    `json:"is_free"`
	PriceInfo   string  `json:"price_info"`
	SortPrice   float64 `json:"-"`
	Status      string  `json:"status"`     // "untested", "working", "failed"
	LatencyMs   int64   `json:"latency_ms"` // last test latency
	LastError   string  `json:"last_error"` // error from last test if failed
}

type ModelTestRecord struct {
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	LastError string `json:"last_error"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func normalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "openrouter":
		return "openrouter"
	case "codecraft":
		return "codecraft"
	default:
		return "gemini"
	}
}

func providerLabel(p string) string {
	switch normalizeProvider(p) {
	case "openrouter":
		return "OpenRouter"
	case "codecraft":
		return "CodeCraft API"
	default:
		return "Google Gemini"
	}
}

func (a *App) initDB() error {
	_, err := a.db.Exec(`
	CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,title TEXT NOT NULL DEFAULT 'New conversation',created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,role TEXT NOT NULL,content TEXT NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS auth_sessions(token TEXT PRIMARY KEY,expires_at DATETIME NOT NULL);
	PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`)
	return err
}

func (a *App) setting(key string) string {
	var v string
	_ = a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (a *App) providerKey(provider string) string {
	p := normalizeProvider(provider)
	if v := a.setting(p + "_api_key"); v != "" {
		return v
	}
	switch p {
	case "openrouter":
		return os.Getenv("OPENROUTER_API_KEY")
	case "codecraft":
		return os.Getenv("CODECRAFT_API_KEY")
	default:
		return os.Getenv("GEMINI_API_KEY")
	}
}

func (a *App) loadTestRecords(provider string) map[string]ModelTestRecord {
	p := normalizeProvider(provider)
	out := map[string]ModelTestRecord{}
	raw := a.setting(p + "_model_tests")
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) saveTestRecord(provider, model string, rec ModelTestRecord) {
	p := normalizeProvider(provider)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(p)
	records[model] = rec
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(p+"_model_tests", string(data))
	}
}

func (a *App) activeProvider() string {
	if p := a.setting("active_provider"); p != "" {
		return normalizeProvider(p)
	}
	if a.providerKey("gemini") != "" {
		return "gemini"
	}
	if a.providerKey("openrouter") != "" {
		return "openrouter"
	}
	if a.providerKey("codecraft") != "" {
		return "codecraft"
	}
	return "gemini"
}

func (a *App) activeModel() string {
	if m := a.setting("active_model"); m != "" {
		return m
	}
	if m := a.setting("gemini_active_model"); m != "" {
		return m
	}
	return "gemini-2.0-flash"
}

func (a *App) setActiveSelection(provider, model string) {
	p := normalizeProvider(provider)
	_ = a.setSetting("active_provider", p)
	_ = a.setSetting("active_model", model)
	_ = a.setSetting(p+"_active_model", model)
}

func (a *App) authenticated(r *http.Request) bool {
	c, err := r.Cookie("sahand_session")
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.RLock()
	exp, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	if ok && time.Now().Before(exp) {
		return true
	}
	var dbExp time.Time
	if err := a.db.QueryRow(`SELECT expires_at FROM auth_sessions WHERE token=?`, c.Value).Scan(&dbExp); err == nil {
		if time.Now().Before(dbExp) {
			a.mu.Lock()
			a.sessions[c.Value] = dbExp
			a.mu.Unlock()
			return true
		}
	}
	return false
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			jsonOut(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		next(w, r)
	}
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	hash := []byte(os.Getenv("HUB_PASSWORD_HASH"))
	if body.Username != env("HUB_USERNAME", "admin") || len(hash) == 0 || bcrypt.CompareHashAndPassword(hash, []byte(body.Password)) != nil {
		time.Sleep(500 * time.Millisecond)
		jsonOut(w, 401, map[string]string{"error": "Invalid username or password"})
		return
	}
	token, err := randomToken()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "session error"})
		return
	}
	exp := time.Now().Add(7 * 24 * time.Hour)
	a.mu.Lock()
	a.sessions[token] = exp
	a.mu.Unlock()
	_, _ = a.db.Exec(`INSERT INTO auth_sessions(token,expires_at) VALUES(?,?)`, token, exp)
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: token, Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: 7 * 86400})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("sahand_session"); e == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
		_, _ = a.db.Exec(`DELETE FROM auth_sessions WHERE token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: "", Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: -1})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	gCfg := a.providerKey("gemini") != ""
	oCfg := a.providerKey("openrouter") != ""
	cCfg := a.providerKey("codecraft") != ""
	jsonOut(w, 200, map[string]any{
		"status":                "online",
		"version":               "0.3.0",
		"gemini_configured":     gCfg,
		"openrouter_configured": oCfg,
		"codecraft_configured":  cCfg,
		"any_configured":        gCfg || oCfg || cCfg,
		"active_provider":       a.activeProvider(),
		"active_model":          a.activeModel(),
		"providers": map[string]bool{
			"gemini":     gCfg,
			"openrouter": oCfg,
			"codecraft":  cCfg,
		},
	})
}

// POST /api/setup/provider (also aliases /api/setup/gemini)
func (a *App) saveProviderKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid request payload"})
		return
	}
	provider := normalizeProvider(body.Provider)
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = a.providerKey(provider)
	}
	if len(key) < 10 {
		jsonOut(w, 400, map[string]string{"error": "Please enter a valid API key for " + providerLabel(provider)})
		return
	}

	start := time.Now()
	models, err := a.fetchProviderModels(provider, key)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}

	if err := a.setSetting(provider+"_api_key", key); err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed to store API key in database"})
		return
	}

	// If no active model is set yet or active provider matches, ensure a sensible default
	if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == provider) {
		a.setActiveSelection(provider, models[0].ID)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        provider,
		"latency_ms":      latency,
		"models":          models,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

// GET /api/models?provider=gemini|openrouter|codecraft|all
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	if q == "all" {
		all := []ModelInfo{}
		errorsMap := map[string]string{}
		for _, p := range []string{"gemini", "openrouter", "codecraft"} {
			k := a.providerKey(p)
			if k == "" {
				continue
			}
			ms, err := a.fetchProviderModels(p, k)
			if err != nil {
				errorsMap[p] = err.Error()
				continue
			}
			all = append(all, ms...)
		}
		jsonOut(w, 200, map[string]any{
			"models":          all,
			"errors":          errorsMap,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	provider := normalizeProvider(q)
	key := a.providerKey(provider)
	if key == "" {
		jsonOut(w, 200, map[string]any{
			"provider":        provider,
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	models, err := a.fetchProviderModels(provider, key)
	if err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        provider,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      true,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	jsonOut(w, 200, map[string]any{
		"provider":        provider,
		"models":          models,
		"configured":      true,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

// POST /api/models/test - Tests a specific model ID on a specific provider
func (a *App) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		SetActive bool   `json:"set_active"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		jsonOut(w, 400, map[string]string{"error": "Model ID is required"})
		return
	}
	provider := normalizeProvider(body.Provider)
	model := strings.TrimSpace(body.Model)
	if provider == "gemini" {
		model = strings.TrimPrefix(model, "models/")
	}

	key := a.providerKey(provider)
	if key == "" {
		jsonOut(w, 400, map[string]string{"error": "Save your " + providerLabel(provider) + " API key first"})
		return
	}

	start := time.Now()
	reply, err := a.callProviderModel(provider, key, "Reply with the single word OK.", model)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		a.saveTestRecord(provider, model, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: err.Error(),
		})
		jsonOut(w, 200, map[string]any{
			"ok":              false,
			"provider":        provider,
			"model":           model,
			"status":          "failed",
			"latency_ms":      latency,
			"error":           err.Error(),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	a.saveTestRecord(provider, model, ModelTestRecord{
		Status:    "working",
		LatencyMs: latency,
		LastError: "",
	})
	if body.SetActive {
		a.setActiveSelection(provider, model)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        provider,
		"model":           model,
		"status":          "working",
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) fetchProviderModels(provider, key string) ([]ModelInfo, error) {
	switch normalizeProvider(provider) {
	case "openrouter":
		return a.fetchOpenRouterModels(key)
	case "codecraft":
		return a.fetchCodeCraftModels(key)
	default:
		return a.fetchGeminiModels(key)
	}
}

func (a *App) fetchGeminiModels(key string) ([]ModelInfo, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models?pageSize=100"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", key)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Network error reaching Google API: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var gErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &gErr) == nil && gErr.Error.Message != "" {
			return nil, fmt.Errorf("Google API HTTP %d: %s", resp.StatusCode, gErr.Error.Message)
		}
		return nil, fmt.Errorf("Google API HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	records := a.loadTestRecords("gemini")
	out := []ModelInfo{}
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
		if !strings.HasPrefix(id, "gemini") && !strings.HasPrefix(id, "gemma") && !strings.HasPrefix(id, "learnlm") {
			continue
		}
		rec, exists := records[id]
		status := "untested"
		var lat int64
		var lastErr string
		if exists {
			status = rec.Status
			lat = rec.LatencyMs
			lastErr = rec.LastError
		}
		disp := m.DisplayName
		if disp == "" {
			disp = id
		}
		out = append(out, ModelInfo{
			Provider:    "gemini",
			ID:          id,
			DisplayName: disp,
			Description: m.Description,
			ContextLen:  m.InputTokenLimit,
			IsFree:      true,
			PriceInfo:   "Google AI Studio Plan",
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if statusRank(out[i].Status) != statusRank(out[j].Status) {
			return statusRank(out[i].Status) < statusRank(out[j].Status)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (a *App) fetchOpenRouterModels(key string) ([]ModelInfo, error) {
	// Validate the key against OpenRouter's /auth/key endpoint first so invalid keys are caught immediately
	authReq, _ := http.NewRequest(http.MethodGet, "https://openrouter.ai/api/v1/auth/key", nil)
	authReq.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 25 * time.Second}
	if authResp, err := client.Do(authReq); err == nil {
		defer authResp.Body.Close()
		if authResp.StatusCode == 401 || authResp.StatusCode == 403 {
			return nil, fmt.Errorf("OpenRouter rejected this API key (HTTP %d)", authResp.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Network error reaching OpenRouter: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenRouter HTTP %d: %s", resp.StatusCode, extractAPIError(raw))
	}

	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	records := a.loadTestRecords("openrouter")
	out := []ModelInfo{}
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		pPrompt, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		pComp, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		isFree := strings.HasSuffix(m.ID, ":free") || (m.Pricing.Prompt != "" && pPrompt == 0 && pComp == 0)

		priceInfo := "Paid model"
		if isFree {
			priceInfo = "FREE ($0 / 1M tokens)"
		} else if pPrompt > 0 {
			priceInfo = fmt.Sprintf("$%.2f / 1M in", pPrompt*1000000)
		}

		rec, exists := records[m.ID]
		status := "untested"
		var lat int64
		var lastErr string
		if exists {
			status = rec.Status
			lat = rec.LatencyMs
			lastErr = rec.LastError
		}
		disp := m.Name
		if disp == "" {
			disp = m.ID
		}
		out = append(out, ModelInfo{
			Provider:    "openrouter",
			ID:          m.ID,
			DisplayName: disp,
			Description: m.Description,
			ContextLen:  m.ContextLength,
			IsFree:      isFree,
			PriceInfo:   priceInfo,
			SortPrice:   pPrompt,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
		})
	}

	// Sort: Working first -> Free models first -> Untested before Failed -> Alphabetical
	sort.Slice(out, func(i, j int) bool {
		if statusRank(out[i].Status) != statusRank(out[j].Status) {
			return statusRank(out[i].Status) < statusRank(out[j].Status)
		}
		if out[i].IsFree != out[j].IsFree {
			return out[i].IsFree
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (a *App) fetchCodeCraftModels(key string) ([]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, "https://codecraftapi.com/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Sahand-CorText/0.3.0")
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Network error reaching CodeCraft API: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CodeCraft API HTTP %d: %s", resp.StatusCode, extractAPIError(raw))
	}

	var parsed struct {
		Data []struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			Description   string   `json:"description"`
			Type          string   `json:"type"`
			ContextWindow int      `json:"context_window"`
			Capabilities  []string `json:"capabilities"`
			Pricing       struct {
				InputPer1K  float64 `json:"input_per_1k"`
				OutputPer1K float64 `json:"output_per_1k"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("Invalid JSON from CodeCraft /v1/models: %v", err)
	}

	records := a.loadTestRecords("codecraft")
	out := []ModelInfo{}
	for _, m := range parsed.Data {
		if m.ID == "" || strings.EqualFold(m.Type, "embedding") || strings.Contains(strings.ToLower(m.ID), "embed") {
			continue
		}
		rec, exists := records[m.ID]
		status := "untested"
		var lat int64
		var lastErr string
		if exists {
			status = rec.Status
			lat = rec.LatencyMs
			lastErr = rec.LastError
		}
		disp := m.Name
		if disp == "" {
			disp = m.ID
		}
		inPer1M := m.Pricing.InputPer1K * 1000
		priceInfo := "Free Starter Credits"
		if inPer1M > 0 {
			priceInfo = fmt.Sprintf("$%.2f / 1M tokens (Free tier eligible)", inPer1M)
		}
		desc := m.Description
		if len(m.Capabilities) > 0 && desc != "" {
			desc = desc + " · [" + strings.Join(m.Capabilities, ", ") + "]"
		}
		out = append(out, ModelInfo{
			Provider:    "codecraft",
			ID:          m.ID,
			DisplayName: disp,
			Description: desc,
			ContextLen:  m.ContextWindow,
			IsFree:      true,
			PriceInfo:   priceInfo,
			SortPrice:   inPer1M,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
		})
	}

	// Sort: Working first -> Untested before Failed -> Lowest token cost first
	sort.Slice(out, func(i, j int) bool {
		if statusRank(out[i].Status) != statusRank(out[j].Status) {
			return statusRank(out[i].Status) < statusRank(out[j].Status)
		}
		if out[i].SortPrice != out[j].SortPrice {
			return out[i].SortPrice < out[j].SortPrice
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func statusRank(s string) int {
	switch s {
	case "working":
		return 0
	case "untested":
		return 1
	default:
		return 2
	}
}

func extractAPIError(raw []byte) string {
	var eWrap struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &eWrap) == nil {
		switch v := eWrap.Error.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if msg, ok := v["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if eWrap.Message != "" {
			return eWrap.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

func (a *App) listSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,title,updated_at FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		if rows.Scan(&s.ID, &s.Title, &s.UpdatedAt) == nil {
			out = append(out, s)
		}
	}
	jsonOut(w, 200, out)
}

func (a *App) createSession(w http.ResponseWriter, r *http.Request) {
	id, e := randomToken()
	if e != nil {
		jsonOut(w, 500, map[string]string{"error": "session error"})
		return
	}
	id = id[:16]
	if _, e = a.db.Exec(`INSERT INTO sessions(id,title) VALUES(?,?)`, id, "New conversation"); e != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	jsonOut(w, 201, Session{ID: id, Title: "New conversation"})
}

func (a *App) getMessages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	rows, e := a.db.Query(`SELECT id,role,content,created_at FROM messages WHERE session_id=? ORDER BY id`, id)
	if e != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if rows.Scan(&m.ID, &m.Role, &m.Content, &m.CreatedAt) == nil {
			out = append(out, m)
		}
	}
	jsonOut(w, 200, out)
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}
	var exists int
	if a.db.QueryRow(`SELECT 1 FROM sessions WHERE id=?`, body.SessionID).Scan(&exists) != nil {
		jsonOut(w, 404, map[string]string{"error": "session not found"})
		return
	}

	provider := body.Provider
	chosenModel := strings.TrimSpace(body.Model)
	if strings.Contains(chosenModel, "::") {
		parts := strings.SplitN(chosenModel, "::", 2)
		provider = parts[0]
		chosenModel = parts[1]
	}
	if provider == "" {
		provider = a.activeProvider()
	}
	provider = normalizeProvider(provider)
	if chosenModel == "" {
		chosenModel = a.activeModel()
	}
	if provider == "gemini" {
		chosenModel = strings.TrimPrefix(chosenModel, "models/")
	}

	key := a.providerKey(provider)
	if key == "" {
		jsonOut(w, 503, map[string]string{"error": "Please save your " + providerLabel(provider) + " API key in Settings first."})
		return
	}

	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "user", body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	start := time.Now()
	answer, err := a.callProviderModel(provider, key, body.Message, chosenModel)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		a.saveTestRecord(provider, chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		jsonOut(w, 502, map[string]string{"error": fmt.Sprintf("[%s · %s] %s", providerLabel(provider), chosenModel, err.Error())})
		return
	}

	a.saveTestRecord(provider, chosenModel, ModelTestRecord{Status: "working", LatencyMs: elapsed})
	a.setActiveSelection(provider, chosenModel)

	_, err = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "assistant", answer)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	var count int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, body.SessionID).Scan(&count)
	if count <= 2 {
		title := body.Message
		if len([]rune(title)) > 42 {
			title = string([]rune(title)[:42]) + "…"
		}
		_, _ = a.db.Exec(`UPDATE sessions SET title=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, title, body.SessionID)
	} else {
		_, _ = a.db.Exec(`UPDATE sessions SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, body.SessionID)
	}
	jsonOut(w, 200, map[string]any{
		"reply":      answer,
		"latency_ms": elapsed,
		"provider":   provider,
		"model":      chosenModel,
	})
}

func (a *App) callProviderModel(provider, key, prompt, model string) (string, error) {
	switch normalizeProvider(provider) {
	case "openrouter":
		return callOpenAICompatible("https://openrouter.ai/api/v1/chat/completions", key, prompt, model, "OpenRouter")
	case "codecraft":
		return callOpenAICompatible("https://codecraftapi.com/v1/chat/completions", key, prompt, model, "CodeCraft")
	default:
		return callGemini(key, prompt, model)
	}
}

func callGemini(key, prompt, model string) (string, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		model = "gemini-2.0-flash"
	}
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	payload := map[string]any{
		"contents": []any{
			map[string]any{
				"parts": []any{
					map[string]string{"text": prompt},
				},
			},
		},
	}
	data, _ := json.Marshal(payload)
	req, e := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var gErr struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &gErr) == nil && gErr.Error.Message != "" {
			return "", fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, gErr.Error.Status, gErr.Error.Message)
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, extractAPIError(raw))
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
	if e = json.Unmarshal(raw, &parsed); e != nil {
		return "", e
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("empty response from Gemini model")
	}
	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	if sb.Len() == 0 {
		return "", errors.New("empty text response from Gemini model")
	}
	return sb.String(), nil
}

func callOpenAICompatible(endpoint, key, prompt, model, label string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []any{
			map[string]string{
				"role":    "user",
				"content": prompt,
			},
		},
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("HTTP-Referer", "https://github.com/sahand363/sahand-cortext")
	req.Header.Set("X-Title", "Sahand-CorText")
	req.Header.Set("User-Agent", "Sahand-CorText/0.3.0")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s HTTP %d: %s", label, resp.StatusCode, extractAPIError(raw))
	}

	var parsed struct {
		Error   any `json:"error"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("invalid JSON response from %s", label)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("%s error: %s", label, extractAPIError(raw))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices returned by %s", label)
	}
	msg := parsed.Choices[0].Message
	out := strings.TrimSpace(msg.Content)
	if out == "" {
		out = strings.TrimSpace(msg.ReasoningContent)
	}
	if out == "" {
		out = strings.TrimSpace(msg.Reasoning)
	}
	if out == "" {
		return "", fmt.Errorf("empty content returned by %s model %s", label, model)
	}
	return out, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sahand-cortext hash-password <password>")
			os.Exit(2)
		}
		h, e := bcrypt.GenerateFromPassword([]byte(os.Args[2]), bcrypt.DefaultCost)
		if e != nil {
			log.Fatal(e)
		}
		fmt.Println(string(h))
		return
	}
	dataDir := env("HUB_DATA_DIR", "./data")
	if e := os.MkdirAll(dataDir, 0700); e != nil {
		log.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(dataDir, "cortext.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, sessions: map[string]time.Time{}}
	if e = app.initDB(); e != nil {
		log.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.auth(app.logout))
	mux.HandleFunc("/api/status", app.auth(app.status))
	mux.HandleFunc("/api/setup/gemini", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/setup/provider", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/models", app.auth(app.handleModels))
	mux.HandleFunc("/api/models/test", app.auth(app.handleTestModel))
	mux.HandleFunc("/api/sessions", app.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			app.listSessions(w, r)
		} else if r.Method == http.MethodPost {
			app.createSession(w, r)
		} else {
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/api/messages", app.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			app.getMessages(w, r)
		} else if r.Method == http.MethodPost {
			app.sendMessage(w, r)
		} else {
			http.Error(w, "method not allowed", 405)
		}
	}))
	webDir := env("HUB_WEB_DIR", "./web/dist")
	mux.Handle("/", http.FileServer(http.Dir(webDir)))
	server := &http.Server{Addr: env("HUB_ADDR", "0.0.0.0:8787"), Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Sahand-CorText listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
