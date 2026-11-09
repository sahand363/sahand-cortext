package main

import (
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
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sahand363/sahand-cortext/internal/chat"
	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type App struct {
	db          *sql.DB
	mu          sync.RWMutex
	sessions    map[string]time.Time
	router      *provider.Router
	orchStorage *orchestration.Storage
	orchPool    *orchestration.WorkerPool
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Pinned    bool   `json:"pinned"`
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
	Status      string  `json:"status"`
	LatencyMs   int64   `json:"latency_ms"`
	LastError   string  `json:"last_error"`
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

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
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

func (a *App) initDB() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS sessions(
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT 'New conversation',
			pinned INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS messages(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS auth_sessions(token TEXT PRIMARY KEY, expires_at DATETIME NOT NULL);`,
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA foreign_keys=ON;`,
		`PRAGMA busy_timeout=10000;`,
	}
	for _, q := range queries {
		if _, err := a.db.Exec(q); err != nil {
			return err
		}
	}
	_, _ = a.db.Exec(`ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;`)
	return a.orchStorage.InitSchema()
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

func (a *App) providerKey(p string) string {
	norm := provider.NormalizeProvider(p)
	if v := a.setting(norm + "_api_key"); v != "" {
		return v
	}
	switch norm {
	case "openrouter":
		return os.Getenv("OPENROUTER_API_KEY")
	case "codecraft":
		return os.Getenv("CODECRAFT_API_KEY")
	default:
		return os.Getenv("GEMINI_API_KEY")
	}
}

func (a *App) loadTestRecords(p string) map[string]ModelTestRecord {
	norm := provider.NormalizeProvider(p)
	out := map[string]ModelTestRecord{}
	raw := a.setting(norm + "_model_tests")
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) saveTestRecord(p, model string, rec ModelTestRecord) {
	norm := provider.NormalizeProvider(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(norm)
	records[model] = rec
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(norm+"_model_tests", string(data))
	}
}

func (a *App) activeProvider() string {
	if p := a.setting("active_provider"); p != "" {
		return provider.NormalizeProvider(p)
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

func (a *App) setActiveSelection(p, model string) {
	norm := provider.NormalizeProvider(p)
	_ = a.setSetting("active_provider", norm)
	_ = a.setSetting("active_model", model)
	_ = a.setSetting(norm+"_active_model", model)
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
	http.SetCookie(w, &http.Cookie{
		Name:     "sahand_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   env("HUB_COOKIE_SECURE", "false") == "true",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 86400,
	})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("sahand_session"); e == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
		_, _ = a.db.Exec(`DELETE FROM auth_sessions WHERE token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "sahand_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   env("HUB_COOKIE_SECURE", "false") == "true",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	gCfg := a.providerKey("gemini") != ""
	oCfg := a.providerKey("openrouter") != ""
	cCfg := a.providerKey("codecraft") != ""
	jsonOut(w, 200, map[string]any{
		"status":                "online",
		"version":               "0.4.0",
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
	norm := provider.NormalizeProvider(body.Provider)
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = a.providerKey(norm)
	}
	if len(key) < 10 {
		jsonOut(w, 400, map[string]string{"error": "Please enter a valid API key for " + provider.ProviderLabel(norm)})
		return
	}

	start := time.Now()
	models, err := a.fetchProviderModels(norm, key)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}

	if err := a.setSetting(norm+"_api_key", key); err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed to store API key in database"})
		return
	}

	if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == norm) {
		a.setActiveSelection(norm, models[0].ID)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"latency_ms":      latency,
		"models":          models,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

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

	norm := provider.NormalizeProvider(q)
	key := a.providerKey(norm)
	if key == "" {
		jsonOut(w, 200, map[string]any{
			"provider":        norm,
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	models, err := a.fetchProviderModels(norm, key)
	if err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        norm,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      true,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	jsonOut(w, 200, map[string]any{
		"provider":        norm,
		"models":          models,
		"configured":      true,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

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
	norm := provider.NormalizeProvider(body.Provider)
	model := strings.TrimSpace(body.Model)
	if norm == "gemini" {
		model = strings.TrimPrefix(model, "models/")
	}

	key := a.providerKey(norm)
	if key == "" {
		jsonOut(w, 400, map[string]string{"error": "Save your " + provider.ProviderLabel(norm) + " API key first"})
		return
	}

	testPrompt := []provider.Message{
		{Role: "user", Content: "Reply with the single word OK."},
	}
	reply, latency, err := a.router.Call(r.Context(), norm, key, model, testPrompt, provider.CallOpts{MaxTokens: 16})
	if err != nil {
		a.saveTestRecord(norm, model, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: err.Error(),
		})
		jsonOut(w, 200, map[string]any{
			"ok":              false,
			"provider":        norm,
			"model":           model,
			"status":          "failed",
			"latency_ms":      latency,
			"error":           err.Error(),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	a.saveTestRecord(norm, model, ModelTestRecord{
		Status:    "working",
		LatencyMs: latency,
		LastError: "",
	})
	if body.SetActive {
		a.setActiveSelection(norm, model)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"model":           model,
		"status":          "working",
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) fetchProviderModels(p, key string) ([]ModelInfo, error) {
	norm := provider.NormalizeProvider(p)
	switch norm {
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
		return nil, fmt.Errorf("network error reaching Google Gemini API: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Google Gemini API HTTP %d", resp.StatusCode)
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
	req, err := http.NewRequest(http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error reaching OpenRouter: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenRouter HTTP %d", resp.StatusCode)
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
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error reaching CodeCraft API: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CodeCraft API HTTP %d", resp.StatusCode)
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
		return nil, err
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
			priceInfo = fmt.Sprintf("$%.2f / 1M tokens", inPer1M)
		}
		out = append(out, ModelInfo{
			Provider:    "codecraft",
			ID:          m.ID,
			DisplayName: disp,
			Description: m.Description,
			ContextLen:  m.ContextWindow,
			IsFree:      true,
			PriceInfo:   priceInfo,
			SortPrice:   inPer1M,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
		})
	}

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

func (a *App) listSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,title,pinned,updated_at FROM sessions ORDER BY pinned DESC, updated_at DESC`)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		var pinnedInt int
		if rows.Scan(&s.ID, &s.Title, &pinnedInt, &s.UpdatedAt) == nil {
			s.Pinned = pinnedInt != 0
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
	if _, e = a.db.Exec(`INSERT INTO sessions(id,title,pinned) VALUES(?,?,0)`, id, "New conversation"); e != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	jsonOut(w, 201, Session{ID: id, Title: "New conversation", Pinned: false})
}

func (a *App) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string  `json:"id"`
		Title  *string `json:"title"`
		Pinned *bool   `json:"pinned"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.ID) == "" {
		jsonOut(w, 400, map[string]string{"error": "Session ID is required"})
		return
	}
	id := strings.TrimSpace(body.ID)

	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		if t != "" {
			if len([]rune(t)) > 120 {
				t = string([]rune(t)[:120])
			}
			_, _ = a.db.Exec(`UPDATE sessions SET title=? WHERE id=?`, t, id)
		}
	}

	if body.Pinned != nil {
		pinVal := 0
		if *body.Pinned {
			pinVal = 1
		}
		_, _ = a.db.Exec(`UPDATE sessions SET pinned=? WHERE id=?`, pinVal, id)
	}

	var s Session
	var pinnedInt int
	if err := a.db.QueryRow(`SELECT id,title,pinned,updated_at FROM sessions WHERE id=?`, id).Scan(&s.ID, &s.Title, &pinnedInt, &s.UpdatedAt); err != nil {
		jsonOut(w, 404, map[string]string{"error": "Session not found"})
		return
	}
	s.Pinned = pinnedInt != 0
	jsonOut(w, 200, s)
}

func (a *App) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		id = strings.TrimSpace(body.ID)
	}
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Session ID is required"})
		return
	}
	_, _ = a.db.Exec(`DELETE FROM messages WHERE session_id=?`, id)
	if _, err := a.db.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed to delete session"})
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) getMessages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	rows, e := a.db.Query(`SELECT id,role,content,created_at FROM messages WHERE session_id=? ORDER BY id ASC`, id)
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
	var currentTitle string
	if a.db.QueryRow(`SELECT title FROM sessions WHERE id=?`, body.SessionID).Scan(&currentTitle) != nil {
		jsonOut(w, 404, map[string]string{"error": "session not found"})
		return
	}

	normProvider := body.Provider
	chosenModel := strings.TrimSpace(body.Model)
	if strings.Contains(chosenModel, "::") {
		parts := strings.SplitN(chosenModel, "::", 2)
		normProvider = parts[0]
		chosenModel = parts[1]
	}
	if normProvider == "" {
		normProvider = a.activeProvider()
	}
	normProvider = provider.NormalizeProvider(normProvider)
	if chosenModel == "" {
		chosenModel = a.activeModel()
	}
	if normProvider == "gemini" {
		chosenModel = strings.TrimPrefix(chosenModel, "models/")
	}

	key := a.providerKey(normProvider)
	if key == "" {
		jsonOut(w, 503, map[string]string{"error": "Please save your " + provider.ProviderLabel(normProvider) + " API key in Settings first."})
		return
	}

	// 1. Persist user turn
	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "user", body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	// 2. Fetch entire history for session and apply safe context truncation
	rows, err := a.db.Query(`SELECT role, content FROM messages WHERE session_id=? ORDER BY id ASC`, body.SessionID)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()

	var history []provider.Message
	for rows.Next() {
		var role, content string
		if rows.Scan(&role, &content) == nil {
			history = append(history, provider.Message{Role: role, Content: content})
		}
	}
	promptMessages := chat.TruncateHistory(history, chat.DefaultConfig())

	// 3. Dispatch structured history to provider
	answer, elapsed, err := a.router.Call(r.Context(), normProvider, key, chosenModel, promptMessages, provider.CallOpts{})
	if err != nil {
		a.saveTestRecord(normProvider, chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		jsonOut(w, 502, map[string]string{"error": fmt.Sprintf("[%s · %s] %s", provider.ProviderLabel(normProvider), chosenModel, err.Error())})
		return
	}

	a.saveTestRecord(normProvider, chosenModel, ModelTestRecord{Status: "working", LatencyMs: elapsed})
	a.setActiveSelection(normProvider, chosenModel)

	// 4. Persist assistant reply
	_, err = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "assistant", answer)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	// 5. Update session title
	var count int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, body.SessionID).Scan(&count)
	if count <= 2 && (currentTitle == "" || currentTitle == "New conversation") {
		title := strings.TrimSpace(body.Message)
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
		"provider":   normProvider,
		"model":      chosenModel,
	})
}

// Orchestration REST Endpoints
func (a *App) orchCreateJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Task      string         `json:"task"`
		MaxRounds int            `json:"max_rounds"`
		MaxCalls  int            `json:"max_calls"`
		Metadata  map[string]any `json:"metadata"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req) != nil || strings.TrimSpace(req.Task) == "" {
		jsonOut(w, 400, map[string]string{"error": "Valid task description is required"})
		return
	}

	metaBytes, _ := json.Marshal(req.Metadata)
	job, err := a.orchStorage.CreateJob(r.Context(), req.Task, req.MaxRounds, req.MaxCalls, string(metaBytes))
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed creating job: " + err.Error()})
		return
	}

	jsonOut(w, 201, job)
}

func (a *App) orchListJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}
	status := r.URL.Query().Get("status")

	jobs, err := a.orchStorage.ListJobs(r.Context(), limit, offset, status)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed listing jobs: " + err.Error()})
		return
	}
	jsonOut(w, 200, jobs)
}

func (a *App) orchGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/api/orchestration/jobs/")
	jobID = strings.TrimSuffix(jobID, "/events")
	jobID = strings.TrimSuffix(jobID, "/cancel")

	if jobID == "" {
		jsonOut(w, 400, map[string]string{"error": "Job ID is required"})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/events") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		events, err := a.orchStorage.GetJobEvents(r.Context(), jobID)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed retrieving events: " + err.Error()})
			return
		}
		jsonOut(w, 200, events)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/cancel") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.CancelJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or already completed"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Cancellation failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "cancelled", "cancelled": true})
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	job, err := a.orchStorage.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, orchestration.ErrJobNotFound) {
			jsonOut(w, 404, map[string]string{"error": "Job not found"})
			return
		}
		jsonOut(w, 500, map[string]string{"error": "Error retrieving job: " + err.Error()})
		return
	}
	jsonOut(w, 200, job)
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
	_ = os.MkdirAll(dataDir, 0700)
	db, e := sql.Open("sqlite", filepath.Join(dataDir, "cortext.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	orchStorage := orchestration.NewStorage(db)
	orchPool := orchestration.NewWorkerPool(orchStorage, nil, envInt("ORCHESTRATION_WORKERS", 2))

	app := &App{
		db:          db,
		sessions:    map[string]time.Time{},
		router:      provider.NewRouter(),
		orchStorage: orchStorage,
		orchPool:    orchPool,
	}
	if e = app.initDB(); e != nil {
		log.Fatal(e)
	}

	// Start worker pool with root context
	workerCtx, workerCancel := context.WithCancel(context.Background())
	orchPool.Start(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.auth(app.logout))
	mux.HandleFunc("/api/status", app.auth(app.status))
	mux.HandleFunc("/api/setup/gemini", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/setup/provider", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/models", app.auth(app.handleModels))
	mux.HandleFunc("/api/models/test", app.auth(app.handleTestModel))

	// Sessions & Ordinary Chat
	mux.HandleFunc("/api/sessions", app.auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			app.listSessions(w, r)
		case http.MethodPost:
			app.createSession(w, r)
		case http.MethodPatch, http.MethodPut:
			app.updateSession(w, r)
		case http.MethodDelete:
			app.deleteSession(w, r)
		default:
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

	// Orchestration REST Endpoints
	mux.HandleFunc("/api/orchestration/jobs", app.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			app.orchListJobs(w, r)
		} else if r.Method == http.MethodPost {
			app.orchCreateJob(w, r)
		} else {
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/api/orchestration/jobs/", app.auth(app.orchGetJob))

	webDir := env("HUB_WEB_DIR", "./web/dist")
	mux.Handle("/", http.FileServer(http.Dir(webDir)))

	server := &http.Server{
		Addr:              env("HUB_ADDR", "0.0.0.0:8787"),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful Shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[Server] Graceful shutdown signal received. Stopping worker pool...")
		workerCancel()
		orchPool.Stop()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("Sahand-CorText v0.4.0 listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
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
