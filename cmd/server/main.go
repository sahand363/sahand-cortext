package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
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
	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
	"github.com/sahand363/sahand-cortext/internal/telegram"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const AppVersion = "0.9.0"

type App struct {
	db             *sql.DB
	mu             sync.RWMutex
	sessions       map[string]time.Time
	webStagedSkill map[string]*skill.StagedDraft // keyed by session_id
	router         *provider.Router
	personaStore   *persona.Store
	skillStore     *skill.Store
	memoryStore    *memory.Store
	memoryExtractMu sync.Mutex // serialises background memory extraction runs
	// Mobile (hub-owned): in-memory connection status, fed by /api/mobile/status.
	mobileMu            sync.Mutex
	mobileLastAppSeen   time.Time
	mobileLastRelaySeen time.Time
	mobileRelayID       string
	orchStorage    *orchestration.Storage
	orchEngine     *orchestration.CollaborationEngine
	orchPool       *orchestration.WorkerPool
	loginLimit     *loginLimiter
}

// loginLimiter blocks an IP after repeated failed logins. It is in-memory on
// purpose: a server restart clears a lockout, so the owner can never lock
// themself out permanently.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

type loginAttempt struct {
	count       int
	firstSeen   time.Time
	lockedUntil time.Time
}

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
	loginLockout     = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string]*loginAttempt{}}
}

func (l *loginLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[ip]
	if !ok {
		return false
	}
	now := time.Now()
	if !a.lockedUntil.IsZero() {
		if now.Before(a.lockedUntil) {
			return true
		}
		// Lock expired: fresh start.
		delete(l.attempts, ip)
		return false
	}
	if now.Sub(a.firstSeen) > loginWindow {
		delete(l.attempts, ip)
	}
	return false
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a, ok := l.attempts[ip]
	if !ok || now.Sub(a.firstSeen) > loginWindow {
		a = &loginAttempt{count: 0, firstSeen: now}
		l.attempts[ip] = a
	}
	a.count++
	if a.count >= loginMaxAttempts {
		a.lockedUntil = now.Add(loginLockout)
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// clientIP returns the remote address without the port.
func clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	return strings.TrimSpace(ip)
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	PersonaID string `json:"persona_id"`
	Pinned    bool   `json:"pinned"`
	IsBot     bool   `json:"is_bot"`
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
	Enabled     bool    `json:"enabled"`
}

type ModelTestRecord struct {
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	LastError string `json:"last_error"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type ChatModelScope struct {
	Mode          string   `json:"mode"`           // "all" | "selected"
	AllowedModels []string `json:"allowed_models"` // ["provider::model", ...]
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
			persona_id TEXT NOT NULL DEFAULT '',
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
	if err := a.personaStore.InitSchema(); err != nil {
		return err
	}
	if a.skillStore == nil {
		a.skillStore = skill.NewStore(a.db)
	}
	if a.webStagedSkill == nil {
		a.webStagedSkill = map[string]*skill.StagedDraft{}
	}
	if err := a.skillStore.InitSchema(); err != nil {
		return err
	}
	if a.memoryStore == nil {
		a.memoryStore = memory.NewStore(a.db)
	}
	if err := a.memoryStore.InitSchema(); err != nil {
		return err
	}
	if err := a.orchStorage.InitSchema(); err != nil {
		return err
	}
	return a.router.AttachDB(context.Background(), a.db)
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
	if _, cfg, err := a.router.Registry().Get(p); err == nil && strings.TrimSpace(cfg.APIKey) != "" {
		return cfg.APIKey
	}
	norm := provider.CanonicalID(p)
	if v := a.setting(norm + "_api_key"); v != "" {
		return provider.DecryptAPIKey(v)
	}
	envName := strings.ToUpper(strings.ReplaceAll(norm, "-", "_")) + "_API_KEY"
	return os.Getenv(envName)
}

func (a *App) isProviderConfigured(p string) bool {
	if a.router.Registry().IsConfigured(p) {
		return true
	}
	return strings.TrimSpace(a.providerKey(p)) != ""
}

func (a *App) loadTestRecords(p string) map[string]ModelTestRecord {
	norm := provider.CanonicalID(p)
	out := map[string]ModelTestRecord{}
	raw := a.setting(norm + "_model_tests")
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) setModelsEnabledBatch(p string, models []string, enabled bool) {
	norm := provider.CanonicalID(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(norm)
	for _, m := range models {
		modelID := a.router.Registry().NormalizeModelID(norm, m)
		if modelID == "" {
			continue
		}
		rec := records[modelID]
		if rec.Status == "" {
			rec.Status = "untested"
		}
		rec.Disabled = !enabled
		records[modelID] = rec
	}
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(norm+"_model_tests", string(data))
	}
}

func (a *App) setModelEnabled(p, model string, enabled bool) {
	a.setModelsEnabledBatch(p, []string{model}, enabled)
}

func (a *App) saveTestRecord(p, model string, rec ModelTestRecord) {
	norm := provider.CanonicalID(p)
	modelID := a.router.Registry().NormalizeModelID(norm, model)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(norm)
	if prev, ok := records[modelID]; ok {
		rec.Disabled = prev.Disabled
	}
	records[modelID] = rec
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(norm+"_model_tests", string(data))
	}
}

func (a *App) verifiedWorkingParticipants() []orchestration.Participant {
	var out []orchestration.Participant
	for _, prov := range a.router.Registry().List() {
		if !prov.Configured {
			continue
		}
		records := a.loadTestRecords(prov.ID)
		for modelID, rec := range records {
			if rec.Status == "working" && !rec.Disabled {
				out = append(out, orchestration.Participant{
					Provider: prov.ID,
					Model:    modelID,
				})
			}
		}
	}
	return out
}

func (a *App) activeProvider() string {
	registered := a.router.Registry().List()
	if p := provider.CanonicalID(a.setting("active_provider")); p != "" {
		if _, _, err := a.router.Registry().Get(p); err == nil {
			if a.isProviderConfigured(p) {
				return p
			}
			anyConfigured := false
			for _, item := range registered {
				if item.Configured {
					anyConfigured = true
					break
				}
			}
			if !anyConfigured {
				return p
			}
		}
	}
	for _, item := range registered {
		if item.Configured {
			return item.ID
		}
	}
	if len(registered) > 0 {
		return registered[0].ID
	}
	return ""
}

func (a *App) activeModel() string {
	prov := a.activeProvider()
	if m := strings.TrimSpace(a.setting("active_model")); m != "" {
		if storedProv := provider.CanonicalID(a.setting("active_provider")); storedProv == "" || storedProv == prov {
			return a.router.Registry().NormalizeModelID(prov, m)
		}
	}
	if m := strings.TrimSpace(a.setting(prov + "_active_model")); m != "" {
		return a.router.Registry().NormalizeModelID(prov, m)
	}
	if p, cfg, err := a.router.Registry().Get(prov); err == nil {
		if p.Metadata().DefaultModel != "" {
			return p.Metadata().DefaultModel
		}
		if len(cfg.ManualModels) > 0 {
			return cfg.ManualModels[0]
		}
	}
	return ""
}

func (a *App) setActiveSelection(p, model string) {
	norm := provider.CanonicalID(p)
	cleanModel := a.router.Registry().NormalizeModelID(norm, model)
	_ = a.setSetting("active_provider", norm)
	_ = a.setSetting("active_model", cleanModel)
	_ = a.setSetting(norm+"_active_model", cleanModel)
}

func (a *App) loadChatModelScope() ChatModelScope {
	mode := strings.ToLower(strings.TrimSpace(a.setting("chat_model_scope_mode")))
	if mode != "selected" {
		mode = "all"
	}
	var rawList []string
	if raw := a.setting("chat_allowed_models"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &rawList)
	}
	seen := map[string]bool{}
	allowed := make([]string, 0, len(rawList))
	for _, entry := range rawList {
		parts := strings.SplitN(strings.TrimSpace(entry), "::", 2)
		if len(parts) != 2 {
			continue
		}
		p := provider.CanonicalID(parts[0])
		m := a.router.Registry().NormalizeModelID(p, parts[1])
		if p == "" || m == "" {
			continue
		}
		key := p + "::" + m
		if !seen[key] {
			seen[key] = true
			allowed = append(allowed, key)
		}
	}
	return ChatModelScope{
		Mode:          mode,
		AllowedModels: allowed,
	}
}

func (a *App) saveChatModelScope(scope ChatModelScope) error {
	mode := strings.ToLower(strings.TrimSpace(scope.Mode))
	if mode != "selected" {
		mode = "all"
	}
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(scope.AllowedModels))
	for _, entry := range scope.AllowedModels {
		parts := strings.SplitN(strings.TrimSpace(entry), "::", 2)
		if len(parts) != 2 {
			continue
		}
		p := provider.CanonicalID(parts[0])
		m := a.router.Registry().NormalizeModelID(p, parts[1])
		if p == "" || m == "" {
			continue
		}
		key := p + "::" + m
		if !seen[key] {
			seen[key] = true
			cleaned = append(cleaned, key)
		}
	}
	if err := a.setSetting("chat_model_scope_mode", mode); err != nil {
		return err
	}
	data, _ := json.Marshal(cleaned)
	return a.setSetting("chat_allowed_models", string(data))
}

func (a *App) isModelAllowedForChat(providerID, modelID string) bool {
	scope := a.loadChatModelScope()
	if scope.Mode != "selected" {
		return true
	}
	target := provider.CanonicalID(providerID) + "::" + a.router.Registry().NormalizeModelID(providerID, modelID)
	for _, item := range scope.AllowedModels {
		if item == target {
			return true
		}
	}
	return false
}

func (a *App) handleChatModelScope(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, 200, a.loadChatModelScope())
	case http.MethodPost, http.MethodPut:
		var body ChatModelScope
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid chat model scope payload"})
			return
		}
		if err := a.saveChatModelScope(body); err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed saving chat model scope"})
			return
		}
		jsonOut(w, 200, a.loadChatModelScope())
	default:
		http.Error(w, "method not allowed", 405)
	}
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
	ip := clientIP(r)
	if a.loginLimit != nil && a.loginLimit.locked(ip) {
		jsonOut(w, 429, map[string]string{"error": "Too many failed login attempts. Try again later."})
		return
	}
	if body.Username != env("HUB_USERNAME", "admin") || len(hash) == 0 || bcrypt.CompareHashAndPassword(hash, []byte(body.Password)) != nil {
		if a.loginLimit != nil {
			a.loginLimit.recordFailure(ip)
		}
		time.Sleep(500 * time.Millisecond)
		jsonOut(w, 401, map[string]string{"error": "Invalid username or password"})
		return
	}
	if a.loginLimit != nil {
		a.loginLimit.recordSuccess(ip)
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
	registered := a.router.Registry().List()
	provMap := make(map[string]bool, len(registered))
	anyConfigured := false
	for _, p := range registered {
		provMap[p.ID] = p.Configured
		if p.Configured {
			anyConfigured = true
		}
	}
	personas, _ := a.personaStore.List(r.Context(), false)
	skills, _ := a.skillStore.List(r.Context(), "", false)
	jsonOut(w, 200, map[string]any{
		"status":                "online",
		"version":               AppVersion,
		"gemini_configured":     provMap["gemini"],
		"openrouter_configured": provMap["openrouter"],
		"codecraft_configured":  provMap["codecraft"],
		"aiprime_configured":    provMap["aiprime"],
		"any_configured":        anyConfigured,
		"active_provider":       a.activeProvider(),
		"active_model":          a.activeModel(),
		"chat_model_scope":      a.loadChatModelScope(),
		"providers":             provMap,
		"providers_list":        registered,
		"personas":              personas,
		"skills":                skills,
	})
}

func (a *App) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.skillStore.List(r.Context(), r.URL.Query().Get("persona_id"), false)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"skills": list})
	case http.MethodPost:
		var body skill.Skill
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid skill payload"})
			return
		}
		saved, err := a.skillStore.Save(r.Context(), body)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(saved.PersonaID) != "" {
			_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{strings.TrimSpace(saved.PersonaID)})
		} else {
			_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{skill.ScopeAll})
		}
		list, _ := a.skillStore.List(r.Context(), "", false)
		jsonOut(w, 201, map[string]any{"ok": true, "skill": saved, "skills": list})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleSkillByID(w http.ResponseWriter, r *http.Request) {
	id := skill.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/skills/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Skill ID is required"})
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Enabled   *bool    `json:"enabled"`
			PersonaID *string  `json:"persona_id"`
			Global    *bool    `json:"global"`
			Scopes    []string `json:"scopes"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || (body.Enabled == nil && body.PersonaID == nil && body.Global == nil && body.Scopes == nil) {
			jsonOut(w, 400, map[string]string{"error": "enabled, persona_id, global or scopes is required"})
			return
		}
		existing, err := a.skillStore.Get(r.Context(), id)
		if err != nil {
			status := 500
			if errors.Is(err, skill.ErrSkillNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		if body.Enabled != nil {
			existing.Enabled = *body.Enabled
		}
		if body.PersonaID != nil {
			existing.PersonaID = strings.TrimSpace(*body.PersonaID)
		}
		// Scope changes: global ("all") or an explicit scope list (main chat,
		// bot persona ids). persona_id is kept as the primary scope so the
		// legacy global check (persona_id = '') stays consistent.
		var newScopes []string
		scopeChanged := false
		if body.Global != nil && *body.Global {
			existing.PersonaID = ""
			newScopes = []string{skill.ScopeAll}
			scopeChanged = true
		} else if body.Scopes != nil {
			newScopes = body.Scopes
			scopeChanged = true
			if len(newScopes) > 0 {
				existing.PersonaID = strings.TrimSpace(newScopes[0])
			} else if existing.PersonaID == "" {
				existing.PersonaID = skill.ScopeMain
			}
		}
		if _, err := a.skillStore.Save(r.Context(), *existing); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if scopeChanged {
			_ = a.skillStore.SetScopes(r.Context(), id, newScopes)
		}
		list, _ := a.skillStore.List(r.Context(), "", false)
		jsonOut(w, 200, map[string]any{"ok": true, "skill": existing, "skills": list})
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := a.skillStore.Delete(r.Context(), id); err != nil {
		status := 500
		if errors.Is(err, skill.ErrSkillNotFound) {
			status = 404
		}
		jsonOut(w, status, map[string]string{"error": err.Error()})
		return
	}
	list, _ := a.skillStore.List(r.Context(), "", false)
	jsonOut(w, 200, map[string]any{"ok": true, "deleted": id, "skills": list})
}

func (a *App) handleMemories(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.memoryStore.List(r.Context(), r.URL.Query().Get("scope"))
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"memories": list})
	case http.MethodPost:
		var body struct {
			Text          string `json:"text"`
			Scope         string `json:"scope"`
			SourceSession string `json:"source_session"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid memory payload"})
			return
		}
		saved, err := a.memoryStore.Save(r.Context(), memory.Memory{Text: body.Text, Scope: body.Scope, SourceSession: body.SourceSession})
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.memoryStore.List(r.Context(), "")
		jsonOut(w, 201, map[string]any{"ok": true, "memory": saved, "memories": list})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleMemoryByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/memories/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Memory ID is required"})
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Text  *string `json:"text"`
			Scope *string `json:"scope"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || (body.Text == nil && body.Scope == nil) {
			jsonOut(w, 400, map[string]string{"error": "text or scope is required"})
			return
		}
		existing, err := a.memoryStore.Get(r.Context(), id)
		if err != nil {
			status := 500
			if errors.Is(err, memory.ErrMemoryNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		if body.Text != nil {
			existing.Text = *body.Text
		}
		if body.Scope != nil {
			existing.Scope = *body.Scope
		}
		if _, err := a.memoryStore.Save(r.Context(), *existing); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.memoryStore.List(r.Context(), "")
		jsonOut(w, 200, map[string]any{"ok": true, "memory": existing, "memories": list})
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := a.memoryStore.Delete(r.Context(), id); err != nil {
		status := 500
		if errors.Is(err, memory.ErrMemoryNotFound) {
			status = 404
		}
		jsonOut(w, status, map[string]string{"error": err.Error()})
		return
	}
	list, _ := a.memoryStore.List(r.Context(), "")
	jsonOut(w, 200, map[string]any{"ok": true, "deleted": id, "memories": list})
}

// handleMemoryExtract starts a memory extraction run in the background
// (one session if session_id is given, otherwise every session with new
// messages) and returns immediately; the Memories page refreshes to show
// what was remembered.
func (a *App) handleMemoryExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body)
	go a.extractMemoriesOnce(strings.TrimSpace(body.SessionID))
	jsonOut(w, 202, map[string]any{"ok": true, "started": true})
}

// startMemoryExtractor runs memory extraction in the background: once a
// minute after startup, then every 15 minutes. It never blocks a chat and
// never crashes the server; failures are logged and skipped.
func (a *App) startMemoryExtractor() {
	go func() {
		time.Sleep(1 * time.Minute)
		for {
			a.extractMemoriesOnce("")
			time.Sleep(15 * time.Minute)
		}
	}()
}

// extractMemoriesOnce extracts durable facts from sessions that have new
// messages since the last extraction. If sessionID is given, only that
// session is processed. It runs the model call AFTER closing every DB row,
// so it can never deadlock the single SQLite connection.
func (a *App) extractMemoriesOnce(sessionID string) {
	if a.memoryStore == nil || a.router == nil {
		return
	}
	a.memoryExtractMu.Lock()
	defer a.memoryExtractMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	prov := a.activeProvider()
	mod := a.activeModel()
	if prov == "" || mod == "" || !a.isProviderConfigured(prov) {
		return
	}
	apiKey := a.providerKey(prov)
	if strings.TrimSpace(apiKey) == "" {
		return
	}

	type memSession struct {
		id, personaID string
		lastMsgID     int64
	}
	var sessions []memSession
	var rows *sql.Rows
	var err error
	if sessionID != "" {
		rows, err = a.db.QueryContext(ctx, `SELECT s.id, s.persona_id, COALESCE(m.last_message_id, 0) FROM sessions s LEFT JOIN memory_extraction_state m ON m.session_id = s.id WHERE s.id = ?`, sessionID)
	} else {
		rows, err = a.db.QueryContext(ctx, `SELECT s.id, s.persona_id, COALESCE(m.last_message_id, 0) FROM sessions s LEFT JOIN memory_extraction_state m ON m.session_id = s.id WHERE EXISTS (SELECT 1 FROM messages WHERE session_id = s.id AND id > COALESCE(m.last_message_id, 0))`)
	}
	if err != nil {
		log.Printf("[Memory] Extraction skipped (sessions query): %v", err)
		return
	}
	for rows.Next() {
		var s memSession
		if rows.Scan(&s.id, &s.personaID, &s.lastMsgID) == nil {
			sessions = append(sessions, s)
		}
	}
	rows.Close() // release the single connection BEFORE the model calls below

	for _, sess := range sessions {
		a.extractMemoriesForSession(ctx, sess.id, sess.personaID, sess.lastMsgID, prov, mod, apiKey)
	}
}

func (a *App) extractMemoriesForSession(ctx context.Context, sessionID, personaID string, lastMsgID int64, prov, mod, apiKey string) {
	scope := strings.TrimSpace(personaID)
	if scope == "" {
		scope = memory.ScopeMain
	}
	// Load the new messages (newest first, capped), then reverse to chronological.
	rows, err := a.db.QueryContext(ctx, `SELECT id, role, content FROM messages WHERE session_id = ? AND id > ? ORDER BY id DESC LIMIT 30`, sessionID, lastMsgID)
	if err != nil {
		return
	}
	var msgs []provider.Message
	var maxID int64 = lastMsgID
	for rows.Next() {
		var id int64
		var role, content string
		if rows.Scan(&id, &role, &content) == nil {
			if id > maxID {
				maxID = id
			}
			msgs = append(msgs, provider.Message{Role: role, Content: content})
		}
	}
	rows.Close() // release the single connection BEFORE the model call below
	if len(msgs) < 4 {
		// Not enough new conversation to bother extracting; leave the
		// watermark alone so these messages are retried once more arrive.
		return
	}
	// Chronological order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	existing, _ := a.memoryStore.ListVisible(ctx, []string{scope})
	var existingTexts []string
	for _, m := range existing {
		existingTexts = append(existingTexts, m.Text)
	}
	facts, err := memory.ExtractFromMessages(ctx, a.router, apiKey, prov, mod, existingTexts, msgs)
	if err != nil {
		log.Printf("[Memory] Extraction failed for session %s: %v", sessionID, err)
		return
	}
	saved := 0
	for _, f := range facts {
		if ok, _ := a.memoryStore.Exists(ctx, scope, f); ok {
			continue
		}
		if _, err := a.memoryStore.Save(ctx, memory.Memory{Text: f, Scope: scope, SourceSession: sessionID}); err == nil {
			saved++
		}
	}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO memory_extraction_state(session_id, last_message_id, extracted_at) VALUES(?,?,CURRENT_TIMESTAMP) ON CONFLICT(session_id) DO UPDATE SET last_message_id=excluded.last_message_id, extracted_at=CURRENT_TIMESTAMP`, sessionID, maxID)
	if saved > 0 {
		log.Printf("[Memory] Remembered %d fact(s) from session %s.", saved, sessionID)
	}
}

func (a *App) handlePersonas(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.personaStore.List(r.Context(), false)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed loading chatbots: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"personas": list})
	case http.MethodPost:
		var input persona.PersonaInput
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&input) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid chatbot configuration payload"})
			return
		}
		saved, err := a.personaStore.Save(r.Context(), a.router.Registry(), input)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.personaStore.List(r.Context(), false)
		jsonOut(w, 201, map[string]any{
			"ok":       true,
			"persona":  saved,
			"personas": list,
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handlePersonaByID(w http.ResponseWriter, r *http.Request) {
	id := persona.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/personas/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Chatbot ID is required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := a.personaStore.Get(r.Context(), id)
		if err != nil {
			status := 500
			if errors.Is(err, persona.ErrPersonaNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, p)
	case http.MethodDelete:
		if err := a.personaStore.Delete(r.Context(), id); err != nil {
			status := 500
			if errors.Is(err, persona.ErrPersonaNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.personaStore.List(r.Context(), false)
		jsonOut(w, 200, map[string]any{
			"ok":       true,
			"deleted":  id,
			"personas": list,
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleTrainPersona(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req persona.TrainRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024)).Decode(&req) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid training payload (max 4 MB)"})
		return
	}
	if strings.Contains(req.Model, "::") {
		parts := strings.SplitN(req.Model, "::", 2)
		req.Provider = parts[0]
		req.Model = parts[1]
	}
	if strings.TrimSpace(req.Provider) == "" {
		req.Provider = a.activeProvider()
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = a.activeModel()
	}
	if !a.isProviderConfigured(req.Provider) {
		jsonOut(w, 400, map[string]string{"error": "Selected trainer provider is not configured yet"})
		return
	}

	res, err := persona.DistillVoicePersona(r.Context(), a.router, a.providerKey, req)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	jsonOut(w, 200, res)
}

func (a *App) handleProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, 200, map[string]any{
			"providers":       a.router.Registry().List(),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
	case http.MethodPost:
		var input provider.DynamicProviderInput
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&input) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid provider configuration payload"})
			return
		}
		if a.router.Store() == nil {
			jsonOut(w, 500, map[string]string{"error": "Provider store is not initialized"})
			return
		}
		created, err := a.router.Store().SaveDynamicProvider(r.Context(), a.router.Registry(), a.router.Client, input)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}

		models, listErr := a.fetchProviderModels(r.Context(), created.ID, "")
		if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == created.ID) {
			a.setActiveSelection(created.ID, models[0].ID)
		}

		jsonOut(w, 201, map[string]any{
			"ok":              true,
			"provider":        created,
			"providers":       a.router.Registry().List(),
			"models":          models,
			"discovery_error": errString(listErr),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	id := provider.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/providers/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Provider ID is required"})
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.router.Store() == nil {
		jsonOut(w, 500, map[string]string{"error": "Provider store is not initialized"})
		return
	}
	if err := a.router.Store().DeleteDynamicProvider(r.Context(), a.router.Registry(), id); err != nil {
		status := 400
		if errors.Is(err, provider.ErrUnknownProvider) {
			status = 404
		}
		jsonOut(w, status, map[string]string{"error": err.Error()})
		return
	}

	if provider.CanonicalID(a.setting("active_provider")) == id {
		_ = a.setSetting("active_provider", "")
		_ = a.setSetting("active_model", "")
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"deleted":         id,
		"providers":       a.router.Registry().List(),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
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
	norm := provider.CanonicalID(body.Provider)
	p, cfg, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}

	requiresKey := p.Metadata().RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none")
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = a.providerKey(norm)
	}
	if requiresKey && len(key) < 8 {
		jsonOut(w, 400, map[string]string{"error": "Please enter a valid API key for " + p.Metadata().Name})
		return
	}

	start := time.Now()
	models, err := a.fetchProviderModels(r.Context(), norm, key)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}

	if key != "" && a.router.Store() != nil {
		if err := a.router.Store().SaveAPIKey(r.Context(), a.router.Registry(), norm, key); err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed to store API key in database"})
			return
		}
	}

	if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == norm) {
		a.setActiveSelection(norm, models[0].ID)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"latency_ms":      latency,
		"models":          models,
		"providers":       a.router.Registry().List(),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := provider.CanonicalID(r.URL.Query().Get("provider"))
	if q == "all" {
		all := []ModelInfo{}
		errorsMap := map[string]string{}
		for _, prov := range a.router.Registry().List() {
			if !prov.Configured {
				continue
			}
			ms, err := a.fetchProviderModels(r.Context(), prov.ID, "")
			if err != nil {
				errorsMap[prov.ID] = err.Error()
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

	if _, _, err := a.router.Registry().Get(q); err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        q,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	if !a.isProviderConfigured(q) {
		jsonOut(w, 200, map[string]any{
			"provider":        q,
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	models, err := a.fetchProviderModels(r.Context(), q, "")
	if err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        q,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      true,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	jsonOut(w, 200, map[string]any{
		"provider":        q,
		"models":          models,
		"configured":      true,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) handleToggleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string   `json:"provider"`
		Model    string   `json:"model"`
		Models   []string `json:"models"`
		Enabled  bool     `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid request payload"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	if _, _, err := a.router.Registry().Get(norm); err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	targets := body.Models
	if strings.TrimSpace(body.Model) != "" {
		targets = append(targets, strings.TrimSpace(body.Model))
	}
	if len(targets) == 0 {
		jsonOut(w, 400, map[string]string{"error": "Model ID or models list is required"})
		return
	}

	a.setModelsEnabledBatch(norm, targets, body.Enabled)
	jsonOut(w, 200, map[string]any{
		"ok":       true,
		"provider": norm,
		"count":    len(targets),
		"enabled":  body.Enabled,
	})
}

func (a *App) handleAddCustomModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		jsonOut(w, 400, map[string]string{"error": "Model ID is required"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	p, _, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	modelID := p.NormalizeModelID(body.Model)

	if !a.isProviderConfigured(norm) {
		jsonOut(w, 400, map[string]string{"error": "Configure your " + p.Metadata().Name + " connection first"})
		return
	}

	if a.router.Store() != nil {
		_ = a.router.Store().SaveManualModel(r.Context(), a.router.Registry(), norm, modelID)
	}

	testPrompt := []provider.Message{
		{Role: "user", Content: "Reply with the single word OK."},
	}
	reply, latency, callErr := a.router.Call(r.Context(), norm, a.providerKey(norm), modelID, testPrompt, provider.CallOpts{MaxTokens: 512})
	if callErr != nil {
		a.saveTestRecord(norm, modelID, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: callErr.Error(),
		})
	} else {
		a.saveTestRecord(norm, modelID, ModelTestRecord{
			Status:    "working",
			LatencyMs: latency,
		})
		a.setModelEnabled(norm, modelID, true)
		a.setActiveSelection(norm, modelID)
	}

	models, _ := a.fetchProviderModels(r.Context(), norm, "")
	jsonOut(w, 200, map[string]any{
		"ok":              callErr == nil,
		"provider":        norm,
		"model":           modelID,
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"error":           errString(callErr),
		"models":          models,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	norm := provider.CanonicalID(body.Provider)
	p, _, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	model := p.NormalizeModelID(body.Model)

	if !a.isProviderConfigured(norm) {
		jsonOut(w, 400, map[string]string{"error": "Configure your " + p.Metadata().Name + " connection first"})
		return
	}

	testPrompt := []provider.Message{
		{Role: "user", Content: "Reply with the single word OK."},
	}
	reply, latency, callErr := a.router.Call(r.Context(), norm, a.providerKey(norm), model, testPrompt, provider.CallOpts{MaxTokens: 512})
	if callErr != nil {
		a.saveTestRecord(norm, model, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: callErr.Error(),
		})
		jsonOut(w, 200, map[string]any{
			"ok":              false,
			"provider":        norm,
			"model":           model,
			"status":          "failed",
			"latency_ms":      latency,
			"error":           callErr.Error(),
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

	rec := a.loadTestRecords(norm)[model]
	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"model":           model,
		"status":          "working",
		"enabled":         !rec.Disabled,
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) fetchProviderModels(ctx context.Context, p, keyOverride string) ([]ModelInfo, error) {
	norm := provider.CanonicalID(p)
	discovered, err := a.router.ListModels(ctx, norm, keyOverride)
	if err != nil {
		return nil, err
	}

	records := a.loadTestRecords(norm)
	out := make([]ModelInfo, 0, len(discovered))
	for _, m := range discovered {
		rec, exists := records[m.ID]
		status := "untested"
		var lat int64
		var lastErr string
		enabled := true
		if exists {
			status = rec.Status
			lat = rec.LatencyMs
			lastErr = rec.LastError
			enabled = !rec.Disabled
		}
		out = append(out, ModelInfo{
			Provider:    norm,
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Description: m.Description,
			ContextLen:  m.ContextLen,
			IsFree:      m.IsFree,
			PriceInfo:   m.PriceInfo,
			SortPrice:   m.SortPrice,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
			Enabled:     enabled,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return statusRank(out[i].Status) < statusRank(out[j].Status)
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
	onlyBots := r.URL.Query().Get("bots") == "true"
	query := `SELECT id, title, coalesce(persona_id,''), pinned, updated_at FROM sessions WHERE id NOT LIKE 'tg-%' ORDER BY pinned DESC, updated_at DESC`
	if onlyBots {
		query = `SELECT id, title, coalesce(persona_id,''), pinned, updated_at FROM sessions WHERE id LIKE 'tg-%' ORDER BY pinned DESC, updated_at DESC`
	}
	rows, err := a.db.Query(query)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		var pinnedInt int
		if rows.Scan(&s.ID, &s.Title, &s.PersonaID, &pinnedInt, &s.UpdatedAt) == nil {
			s.Pinned = pinnedInt != 0
			s.IsBot = strings.HasPrefix(s.ID, "tg-")
			out = append(out, s)
		}
	}
	jsonOut(w, 200, out)
}

func (a *App) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PersonaID string `json:"persona_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	personaID := persona.CanonicalID(body.PersonaID)
	title := "New conversation"
	if personaID != "" {
		if p, err := a.personaStore.Get(r.Context(), personaID); err == nil {
			title = p.Icon + " " + p.Name
		} else {
			personaID = ""
		}
	}

	id, e := randomToken()
	if e != nil {
		jsonOut(w, 500, map[string]string{"error": "session error"})
		return
	}
	id = id[:16]
	if _, e = a.db.Exec(`INSERT INTO sessions(id,title,persona_id,pinned) VALUES(?,?,?,0)`, id, title, personaID); e != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	jsonOut(w, 201, Session{ID: id, Title: title, PersonaID: personaID, Pinned: false, IsBot: false})
}

func (a *App) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string  `json:"id"`
		Title     *string `json:"title"`
		PersonaID *string `json:"persona_id"`
		Pinned    *bool   `json:"pinned"`
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

	if body.PersonaID != nil {
		pid := persona.CanonicalID(*body.PersonaID)
		if pid != "" {
			if _, err := a.personaStore.Get(r.Context(), pid); err != nil {
				jsonOut(w, 404, map[string]string{"error": "Chatbot persona not found"})
				return
			}
		}
		_, _ = a.db.Exec(`UPDATE sessions SET persona_id=? WHERE id=?`, pid, id)
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
	if err := a.db.QueryRow(`SELECT id,title,coalesce(persona_id,''),pinned,updated_at FROM sessions WHERE id=?`, id).Scan(&s.ID, &s.Title, &s.PersonaID, &pinnedInt, &s.UpdatedAt); err != nil {
		jsonOut(w, 404, map[string]string{"error": "Session not found"})
		return
	}
	s.Pinned = pinnedInt != 0
	s.IsBot = strings.HasPrefix(s.ID, "tg-")
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

// chatTarget is everything a chat turn resolves before calling the
// model: the session, the active persona, and the provider/model.
// It is shared by the buffered sendMessage and the streaming mobile
// endpoint, so both resolve a chat the same way.
type chatTarget struct {
	sessionID       string
	sessTitle       string
	activePersonaID string
	activeBot       *persona.Persona
	normProvider    string
	chosenModel     string
	providerName    string
}

// chatError is a chat failure that maps to an HTTP status + message
// (sendMessage) or an SSE error event (the streaming endpoint).
type chatError struct {
	status  int
	message string
}

func (e *chatError) Error() string { return e.message }

// resolveChatTarget resolves a chat turn: the session, the active
// persona (the request's, else the session's stored one), and the
// provider/model (the request's, else the persona's / the hub's active
// defaults, with the Main Chat model-scope failover). It returns a
// chatError (with the HTTP status sendMessage would use) when the turn
// cannot run.
func (a *App) resolveChatTarget(ctx context.Context, sessionID, personaID, providerID, model string) (*chatTarget, *chatError) {
	var sessTitle string
	var sessionPersonaID string
	if a.db.QueryRow(`SELECT title, coalesce(persona_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&sessTitle, &sessionPersonaID) != nil {
		return nil, &chatError{status: 404, message: "session not found"}
	}

	activePersonaID := persona.CanonicalID(personaID)
	if activePersonaID == "" {
		activePersonaID = sessionPersonaID
	} else if activePersonaID != sessionPersonaID {
		_, _ = a.db.Exec(`UPDATE sessions SET persona_id=? WHERE id=?`, activePersonaID, sessionID)
	}

	var activeBot *persona.Persona
	if activePersonaID != "" {
		if bot, err := a.personaStore.Get(ctx, activePersonaID); err == nil {
			activeBot = bot
		}
	}

	normProvider := strings.TrimSpace(providerID)
	chosenModel := strings.TrimSpace(model)
	if strings.Contains(chosenModel, "::") {
		parts := strings.SplitN(chosenModel, "::", 2)
		normProvider = parts[0]
		chosenModel = parts[1]
	}
	if normProvider == "" && activeBot != nil && activeBot.DefaultProvider != "" {
		normProvider = activeBot.DefaultProvider
	}
	if normProvider == "" {
		normProvider = a.activeProvider()
	}
	normProvider = provider.CanonicalID(normProvider)

	p, _, regErr := a.router.Registry().Get(normProvider)
	if regErr != nil {
		return nil, &chatError{status: 400, message: regErr.Error()}
	}
	providerName := p.Metadata().Name
	if chosenModel == "" && activeBot != nil && activeBot.DefaultModel != "" && activeBot.DefaultProvider == normProvider {
		chosenModel = activeBot.DefaultModel
	}
	if chosenModel == "" {
		chosenModel = a.activeModel()
	}
	chosenModel = p.NormalizeModelID(chosenModel)

	if activeBot != nil && activeBot.ModelScopeMode == "selected" {
		if !activeBot.IsModelAllowed(a.router.Registry(), normProvider, chosenModel) {
			return nil, &chatError{status: 403, message: fmt.Sprintf("Model %s is not enabled for chatbot %q. Update its allowed models in Settings.", chosenModel, activeBot.Name)}
		}
	} else if !a.isModelAllowedForChat(normProvider, chosenModel) {
		if strings.TrimSpace(model) == "" {
			scope := a.loadChatModelScope()
			switched := false
			for _, entry := range scope.AllowedModels {
				parts := strings.SplitN(entry, "::", 2)
				if len(parts) == 2 && a.isProviderConfigured(parts[0]) {
					if candProv, _, err := a.router.Registry().Get(parts[0]); err == nil {
						normProvider = parts[0]
						chosenModel = parts[1]
						p = candProv
						providerName = candProv.Metadata().Name
						switched = true
						break
					}
				}
			}
			if !switched {
				return nil, &chatError{status: 403, message: "No allowed Main Chat model is currently configured. Update Main Chat model permissions in Settings."}
			}
		} else {
			return nil, &chatError{status: 403, message: "This model is not enabled for Main Chat. Update Main Chat model permissions in Settings."}
		}
	}

	if !a.isProviderConfigured(normProvider) {
		return nil, &chatError{status: 503, message: "Please configure " + providerName + " in Settings first."}
	}

	return &chatTarget{
		sessionID:       sessionID,
		sessTitle:       sessTitle,
		activePersonaID: activePersonaID,
		activeBot:       activeBot,
		normProvider:    normProvider,
		chosenModel:     chosenModel,
		providerName:    providerName,
	}, nil
}

// buildChatPrompt inserts the user's message, then builds the prompt
// (persona system prompt + skill block + memory block + history)
// exactly as sendMessage does. The skill/memory reads and the history
// rows are all drained and closed BEFORE this returns, so the caller
// may make a long model call afterwards — with a single SQLite
// connection the rows must be closed before the model is called, or
// the next DB write blocks forever.
func (a *App) buildChatPrompt(ctx context.Context, t *chatTarget, message string) ([]provider.Message, error) {
	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", message)
	if err != nil {
		return nil, err
	}

	var history []provider.Message
	var sysBuilder strings.Builder
	if t.activeBot != nil {
		sysBuilder.WriteString(t.activeBot.BuildCombinedSystemPrompt())
	}
	if a.skillStore != nil {
		// The main chat sees skills assigned to "main" plus the active bot's skills.
		skillScopes := []string{skill.ScopeMain}
		if t.activePersonaID != "" {
			skillScopes = append(skillScopes, t.activePersonaID)
		}
		sysBuilder.WriteString(a.skillStore.BuildSkillPromptBlock(ctx, skillScopes, message))
	}
	if a.memoryStore != nil {
		// Same scopes as skills: memories from the main chat plus the active
		// bot's memories. Runs BEFORE the history query below, like skills.
		memoryScopes := []string{memory.ScopeMain}
		if t.activePersonaID != "" {
			memoryScopes = append(memoryScopes, t.activePersonaID)
		}
		sysBuilder.WriteString(a.memoryStore.BuildMemoryPromptBlock(ctx, memoryScopes, message))
	}
	if combinedSys := strings.TrimSpace(sysBuilder.String()); combinedSys != "" {
		history = append(history, provider.Message{Role: "system", Content: combinedSys})
	}

	rows, err := a.db.Query(`SELECT role, content FROM messages WHERE session_id=? ORDER BY id ASC`, t.sessionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var role, content string
		if rows.Scan(&role, &content) == nil {
			history = append(history, provider.Message{Role: role, Content: content})
		}
	}
	rows.Close()

	return chat.TruncateHistory(history, chat.DefaultConfig()), nil
}

// finishChat records the model result and writes the assistant's reply
// to the DB exactly as sendMessage does (so history/memory stay correct).
func (a *App) finishChat(t *chatTarget, message, answer string, elapsed int64) error {
	a.saveTestRecord(t.normProvider, t.chosenModel, ModelTestRecord{Status: "working", LatencyMs: elapsed})
	if t.activeBot == nil {
		a.setActiveSelection(t.normProvider, t.chosenModel)
	}

	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", answer)
	if err != nil {
		return err
	}

	var count int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, t.sessionID).Scan(&count)
	defaultBotTitle := ""
	if t.activeBot != nil {
		defaultBotTitle = t.activeBot.Icon + " " + t.activeBot.Name
	}
	if count <= 2 && (t.sessTitle == "" || t.sessTitle == "New conversation" || t.sessTitle == defaultBotTitle) {
		title := strings.TrimSpace(message)
		if len([]rune(title)) > 42 {
			title = string([]rune(title)[:42]) + "…"
		}
		if t.activeBot != nil {
			title = t.activeBot.Icon + " " + title
		}
		_, _ = a.db.Exec(`UPDATE sessions SET title=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, title, t.sessionID)
	} else {
		_, _ = a.db.Exec(`UPDATE sessions SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, t.sessionID)
	}
	return nil
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		PersonaID string `json:"persona_id"`
		Message   string `json:"message"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}
	t, cerr := a.resolveChatTarget(r.Context(), body.SessionID, body.PersonaID, body.Provider, body.Model)
	if cerr != nil {
		jsonOut(w, cerr.status, map[string]string{"error": cerr.message})
		return
	}

	trimmedMsg := strings.TrimSpace(body.Message)

	// Handle staged /learn skill confirmation in Web Chat
	a.mu.Lock()
	staged, hasStaged := a.webStagedSkill[t.sessionID]
	a.mu.Unlock()

	if hasStaged && !strings.HasPrefix(trimmedMsg, "/") {
		lower := strings.ToLower(trimmedMsg)
		isFull := lower == "full" || strings.Contains(lower, "full") || lower == "کامل"
		isCompressed := lower == "compressed" || strings.Contains(lower, "compress") || lower == "فشرده" || lower == "خلاصه"
		isCancel := lower == "cancel" || lower == "لغو"

		if isCancel || isFull || isCompressed {
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)
			a.mu.Lock()
			delete(a.webStagedSkill, t.sessionID)
			a.mu.Unlock()

			var replyText string
			if isCancel {
				replyText = "✖ Cancelled saving skill."
			} else {
				mode := "compressed"
				chosenContent := staged.CompressedContent
				if isFull {
					mode = "full"
					chosenContent = staged.FullContent
				}
				// A skill learned in the main chat (no persona) belongs to the
				// main chat, not to everyone; one learned as a bot belongs to that bot.
				ownerID := t.activePersonaID
				if ownerID == "" {
					ownerID = skill.ScopeMain
				}
				saved, saveErr := a.skillStore.Save(r.Context(), skill.Skill{
					Name:        staged.Name,
					Description: staged.Description,
					SourceURL:   staged.SourceURL,
					StorageMode: mode,
					Content:     chosenContent,
					PersonaID:   ownerID,
					Enabled:     true,
				})
				if saveErr != nil {
					replyText = "⚠️ Failed to save skill: " + saveErr.Error()
				} else {
					_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{ownerID})
					replyText = fmt.Sprintf("✅ Saved skill \"%s\" (%s mode, %d chars) to database! It is now active across your workspace.", saved.Name, saved.StorageMode, len([]rune(saved.Content)))
				}
			}
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", replyText)
			jsonOut(w, 200, map[string]any{"reply": replyText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	// Handle /learn-deep <url> command in Web Chat: learns the page plus its
	// same-site links in the background and posts a summary into the chat.
	if strings.HasPrefix(strings.ToLower(trimmedMsg), "/learn-deep ") {
		parts := strings.Fields(trimmedMsg)
		if len(parts) >= 2 {
			targetURL := strings.TrimSpace(parts[1])
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)
			startText := fmt.Sprintf("🔍 Deep learning %s — visiting the page and its same-site links (up to 10 pages)… I'll post a summary here when it's done.", targetURL)
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", startText)
			sessID := t.sessionID
			provID, apiKey, modelID, personaID := t.normProvider, a.providerKey(t.normProvider), t.chosenModel, t.activePersonaID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer cancel()
				res := a.skillStore.LearnDeep(ctx, a.router, provID, apiKey, modelID, targetURL, personaID, 10, nil)
				var sb strings.Builder
				fmt.Fprintf(&sb, "📚 Deep learn finished for %s\n\nLearned %d skill(s):\n", targetURL, len(res.Learned))
				for _, name := range res.Learned {
					fmt.Fprintf(&sb, "• %s\n", name)
				}
				if len(res.Failed) > 0 {
					fmt.Fprintf(&sb, "\n⚠️ Could not learn %d page(s):\n", len(res.Failed))
					for _, u := range res.Failed {
						fmt.Fprintf(&sb, "• %s\n", u)
					}
				}
				_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, sessID, "assistant", sb.String())
			}()
			jsonOut(w, 200, map[string]any{"reply": startText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	// Handle /learn <url> command in Web Chat
	if strings.HasPrefix(strings.ToLower(trimmedMsg), "/learn ") {
		parts := strings.Fields(trimmedMsg)
		if len(parts) >= 2 {
			targetURL := strings.TrimSpace(parts[1])
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)

			draft, exErr := skill.ExtractSkillFromURL(r.Context(), a.router, t.normProvider, a.providerKey(t.normProvider), t.chosenModel, targetURL, t.activePersonaID)
			var replyText string
			if exErr != nil {
				replyText = "⚠️ " + exErr.Error()
			} else {
				a.mu.Lock()
				a.webStagedSkill[t.sessionID] = draft
				a.mu.Unlock()
				replyText = fmt.Sprintf(
					"📚 Learned Skill: %s\n%s\n\nDo you want me to save the FULL version (~%d chars) or the COMPRESSED version (~%d chars, redundant info removed) to the database?\n\nReply with: Full or Compressed (or Cancel)",
					draft.Name, draft.Description, len([]rune(draft.FullContent)), len([]rune(draft.CompressedContent)),
				)
			}
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", replyText)
			jsonOut(w, 200, map[string]any{"reply": replyText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	promptMessages, err := a.buildChatPrompt(r.Context(), t, body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	answer, elapsed, err := a.router.Call(r.Context(), t.normProvider, a.providerKey(t.normProvider), t.chosenModel, promptMessages, provider.CallOpts{})
	if err != nil {
		a.saveTestRecord(t.normProvider, t.chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		jsonOut(w, 502, map[string]string{"error": fmt.Sprintf("[%s · %s] %s", t.providerName, t.chosenModel, err.Error())})
		return
	}

	if err := a.finishChat(t, body.Message, answer, elapsed); err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	jsonOut(w, 200, map[string]any{
		"reply":      answer,
		"latency_ms": elapsed,
		"provider":   t.normProvider,
		"model":      t.chosenModel,
		"persona_id": t.activePersonaID,
	})
}

func (a *App) orchCreateJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Task                string                      `json:"task"`
		MaxRounds           int                         `json:"max_rounds"`
		MaxCalls            int                         `json:"max_calls"`
		AutoRetry           *bool                       `json:"auto_retry"`
		SwitchParticipant   *bool                       `json:"switch_participant"`
		AllowGlobalFallback *bool                       `json:"allow_global_fallback"`
		Participants        []orchestration.Participant `json:"participants"`
		Metadata            map[string]any              `json:"metadata"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req) != nil || strings.TrimSpace(req.Task) == "" {
		jsonOut(w, 400, map[string]string{"error": "Valid task description is required"})
		return
	}

	if req.MaxRounds > 20 {
		req.MaxRounds = 20
	}
	if req.MaxCalls > 50 {
		req.MaxCalls = 50
	}

	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	if req.AutoRetry != nil {
		req.Metadata["auto_retry"] = *req.AutoRetry
	}
	if req.SwitchParticipant != nil {
		req.Metadata["switch_participant"] = *req.SwitchParticipant
	}
	if req.AllowGlobalFallback != nil {
		req.Metadata["allow_global_fallback"] = *req.AllowGlobalFallback
	}

	if len(req.Participants) > 0 {
		for i := range req.Participants {
			p := strings.TrimSpace(req.Participants[i].Provider)
			m := strings.TrimSpace(req.Participants[i].Model)
			if strings.Contains(m, "::") {
				parts := strings.SplitN(m, "::", 2)
				p = parts[0]
				m = parts[1]
			}
			normProv := provider.CanonicalID(p)
			regProv, _, regErr := a.router.Registry().Get(normProv)
			if regErr != nil {
				jsonOut(w, 400, map[string]string{"error": regErr.Error()})
				return
			}
			m = regProv.NormalizeModelID(m)
			if m == "" {
				jsonOut(w, 400, map[string]string{"error": "Participant model ID cannot be empty"})
				return
			}
			if !a.isProviderConfigured(normProv) {
				jsonOut(w, 400, map[string]string{"error": "Provider " + regProv.Metadata().Name + " is not configured or disabled"})
				return
			}
			req.Participants[i].Provider = normProv
			req.Participants[i].Model = m
		}
		req.Metadata["participants"] = req.Participants
	}

	metaBytes, _ := json.Marshal(req.Metadata)
	job, err := a.orchStorage.CreateJob(r.Context(), req.Task, req.MaxRounds, req.MaxCalls, string(metaBytes))
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed creating job: " + err.Error()})
		return
	}

	if len(req.Participants) > 0 {
		_ = a.orchStorage.SaveParticipants(r.Context(), job.ID, req.Participants)
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
	rawPath := strings.TrimPrefix(r.URL.Path, "/api/orchestration/jobs/")
	if idx := strings.Index(rawPath, "/messages/"); idx != -1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jID := rawPath[:idx]
		msgIDStr := rawPath[idx+len("/messages/"):]
		msgID, err := strconv.ParseInt(msgIDStr, 10, 64)
		if jID == "" || err != nil || msgID <= 0 {
			jsonOut(w, 400, map[string]string{"error": "Valid job ID and message ID are required"})
			return
		}
		msg, err := a.orchStorage.GetCollaborationMessageByID(r.Context(), jID, msgID)
		if err != nil {
			if errors.Is(err, orchestration.ErrMessageNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Collaboration message not found"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Failed loading collaboration message: " + err.Error()})
			return
		}
		jsonOut(w, 200, msg)
		return
	}

	jobID := rawPath
	jobID = strings.TrimSuffix(jobID, "/events")
	jobID = strings.TrimSuffix(jobID, "/messages")
	jobID = strings.TrimSuffix(jobID, "/participants")
	jobID = strings.TrimSuffix(jobID, "/details")
	jobID = strings.TrimSuffix(jobID, "/cancel")
	jobID = strings.TrimSuffix(jobID, "/pause")
	jobID = strings.TrimSuffix(jobID, "/retry")
	jobID = strings.TrimSuffix(jobID, "/iterate")
	jobID = strings.TrimSuffix(jobID, "/audit")
	jobID = strings.TrimSuffix(jobID, "/apply-audit")

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

	if strings.HasSuffix(r.URL.Path, "/messages") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		msgs, err := a.orchStorage.GetCollaborationMessages(r.Context(), jobID)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed retrieving collaboration messages: " + err.Error()})
			return
		}
		jsonOut(w, 200, msgs)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/participants") {
		if r.Method == http.MethodGet {
			parts, err := a.orchStorage.GetParticipants(r.Context(), jobID)
			if err != nil {
				jsonOut(w, 500, map[string]string{"error": "Failed retrieving participants"})
				return
			}
			jsonOut(w, 200, parts)
			return
		} else if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var body struct {
				Participants []orchestration.Participant `json:"participants"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil || len(body.Participants) == 0 {
				jsonOut(w, 400, map[string]string{"error": "Invalid participants list"})
				return
			}
			for i := range body.Participants {
				p := strings.TrimSpace(body.Participants[i].Provider)
				m := strings.TrimSpace(body.Participants[i].Model)
				if strings.Contains(m, "::") {
					parts := strings.SplitN(m, "::", 2)
					p = parts[0]
					m = parts[1]
				}
				normProv := provider.CanonicalID(p)
				regProv, _, regErr := a.router.Registry().Get(normProv)
				if regErr != nil {
					jsonOut(w, 400, map[string]string{"error": regErr.Error()})
					return
				}
				m = regProv.NormalizeModelID(m)
				if m == "" || !a.isProviderConfigured(normProv) {
					jsonOut(w, 400, map[string]string{"error": "Invalid or unconfigured participant provider/model"})
					return
				}
				body.Participants[i].Provider = normProv
				body.Participants[i].Model = m
			}
			if err := a.orchStorage.SaveParticipants(r.Context(), jobID, body.Participants); err != nil {
				jsonOut(w, 500, map[string]string{"error": "Failed updating participants: " + err.Error()})
				return
			}
			_ = a.orchStorage.RecordEvent(r.Context(), jobID, "participants_updated", "running", "user", "Collaboration team roster updated mid-job", "{}")
			jsonOut(w, 200, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "method not allowed", 405)
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

	if strings.HasSuffix(r.URL.Path, "/pause") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.PauseJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or not running"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Pause failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "paused", "paused": true})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/retry") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := a.orchStorage.RetryJob(r.Context(), jobID); err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found or already running"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Retry failed: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": jobID, "status": "queued", "retried": true})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/iterate") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Instruction      string `json:"instruction"`
			ExtraRounds      int    `json:"extra_rounds"`
			ExtraCalls       int    `json:"extra_calls"`
			IncludeFullFiles *bool  `json:"include_full_files"`
			IncludeSummary   *bool  `json:"include_summary"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body) != nil || strings.TrimSpace(body.Instruction) == "" {
			jsonOut(w, 400, map[string]string{"error": "Please enter your revision instructions"})
			return
		}
		incFull := true
		if body.IncludeFullFiles != nil {
			incFull = *body.IncludeFullFiles
		}
		incSum := true
		if body.IncludeSummary != nil {
			incSum = *body.IncludeSummary
		}
		taggedInstruction := fmt.Sprintf("[REV_OPTS:full_files=%t,summary=%t]\n%s", incFull, incSum, strings.TrimSpace(body.Instruction))

		updatedJob, err := a.orchStorage.IterateJob(r.Context(), jobID, taggedInstruction, body.ExtraRounds, body.ExtraCalls)
		if err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found"})
				return
			}
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, updatedJob)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/audit") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid audit request payload"})
			return
		}
		prov := strings.TrimSpace(body.Provider)
		mod := strings.TrimSpace(body.Model)
		if strings.Contains(mod, "::") {
			parts := strings.SplitN(mod, "::", 2)
			prov = parts[0]
			mod = parts[1]
		}
		if prov == "" {
			prov = a.activeProvider()
		}
		if mod == "" {
			mod = a.activeModel()
		}
		auditMsg, err := a.orchEngine.AuditDeliverable(r.Context(), jobID, prov, mod)
		if err != nil {
			jsonOut(w, 502, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, auditMsg)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/apply-audit") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			MessageID int64 `json:"message_id"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body)
		updatedJob, err := a.orchStorage.ApplyAuditResult(r.Context(), jobID, body.MessageID)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, updatedJob)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/details") || r.URL.Query().Get("include") == "all" {
		details, err := a.orchStorage.GetJobDetails(r.Context(), jobID)
		if err != nil {
			if errors.Is(err, orchestration.ErrJobNotFound) {
				jsonOut(w, 404, map[string]string{"error": "Job not found"})
				return
			}
			jsonOut(w, 500, map[string]string{"error": "Error retrieving job details: " + err.Error()})
			return
		}
		jsonOut(w, 200, details)
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

// notifyWatchdog sends a WATCHDOG=1 ping to systemd via $NOTIFY_SOCKET.
// It is a no-op when the process is not running under systemd (no socket
// set) or the socket cannot be reached. It never logs and never fails.
func notifyWatchdog() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("WATCHDOG=1"))
}

// startWatchdogHeartbeat reports liveness to systemd every 10 seconds.
// Before each ping it probes the database with a short timeout; the ping
// is only sent when the database answers. If the single SQLite connection
// is stuck (for example held by a deadlocked query), the probe times out,
// the pings stop, and systemd restarts the service (WatchdogSec=30 in the
// unit file). A process that is merely slow does not trigger a restart:
// the probe would have to fail three times in a row (30s / 10s) first.
// The goroutine runs until the process exits; there is nothing to stop.
func startWatchdogHeartbeat(db *sql.DB) {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return // not running under systemd; nothing to report to
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := db.PingContext(ctx)
			cancel()
			if err == nil {
				notifyWatchdog()
			}
		}
	}()
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
	// A staged restore (from a backup import) becomes live on restart.
	restoreDB(dataDir)
	db, e := sql.Open("sqlite", filepath.Join(dataDir, "cortext.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	startWatchdogHeartbeat(db)

	router := provider.NewRouter()
	personaStore := persona.NewStore(db)
	skillStore := skill.NewStore(db)
	memoryStore := memory.NewStore(db)
	orchStorage := orchestration.NewStorage(db)

	app := &App{
		db:             db,
		sessions:       map[string]time.Time{},
		webStagedSkill: map[string]*skill.StagedDraft{},
		router:         router,
		personaStore:   personaStore,
		skillStore:     skillStore,
		memoryStore:    memoryStore,
		orchStorage:    orchStorage,
		loginLimit:     newLoginLimiter(),
	}

	tgBridge := telegram.NewBridgeManager(db, personaStore, skillStore, memoryStore, router, app.providerKey)
	bridgeCtx, bridgeCancel := context.WithCancel(context.Background())
	tgBridge.StartSync(bridgeCtx)
	app.startMemoryExtractor()

	collabEngine := orchestration.NewCollaborationEngine(
		orchStorage,
		router,
		app.providerKey,
		func() (string, string) { return app.activeProvider(), app.activeModel() },
		orchestration.DefaultCollaborationConfig(),
	)
	collabEngine.SetProviderConfiguredChecker(app.isProviderConfigured)
	collabEngine.SetFallbackCandidatesProvider(app.verifiedWorkingParticipants)

	orchPool := orchestration.NewWorkerPool(orchStorage, collabEngine, envInt("ORCHESTRATION_WORKERS", 2))
	app.orchEngine = collabEngine
	app.orchPool = orchPool

	if e = app.initDB(); e != nil {
		log.Fatal(e)
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	orchPool.Start(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.auth(app.logout))
	mux.HandleFunc("/api/status", app.auth(app.status))
	mux.HandleFunc("/api/providers", app.auth(app.handleProviders))
	mux.HandleFunc("/api/providers/", app.auth(app.handleProviderByID))
	mux.HandleFunc("/api/personas", app.auth(app.handlePersonas))
	mux.HandleFunc("/api/personas/train", app.auth(app.handleTrainPersona))
	mux.HandleFunc("/api/personas/", app.auth(app.handlePersonaByID))
	mux.HandleFunc("/api/skills", app.auth(app.handleSkills))
	mux.HandleFunc("/api/skills/", app.auth(app.handleSkillByID))
	mux.HandleFunc("/api/memories", app.auth(app.handleMemories))
	mux.HandleFunc("/api/memories/extract", app.auth(app.handleMemoryExtract))
	mux.HandleFunc("/api/memories/", app.auth(app.handleMemoryByID))
	mux.HandleFunc("/api/setup/gemini", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/setup/provider", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/models", app.auth(app.handleModels))
	mux.HandleFunc("/api/models/test", app.auth(app.handleTestModel))
	mux.HandleFunc("/api/models/toggle", app.auth(app.handleToggleModel))
	mux.HandleFunc("/api/models/custom", app.auth(app.handleAddCustomModel))
	mux.HandleFunc("/api/chat/model-scope", app.auth(app.handleChatModelScope))

	// Backup (Export / Import): take everything with you — one zip with
	// the database plus the env secrets a fresh hub needs to restore it.
	mux.HandleFunc("/api/backup/export", app.auth(app.backupExport))
	mux.HandleFunc("/api/backup/import", app.auth(app.backupImport))

	// Mobile (hub-owned): the Android app talks to CorText directly or
	// through a dumb relay. The encrypted endpoints verify the mobile token
	// by decrypting the body; the status/settings endpoints use the web login.
	mux.HandleFunc("/api/mobile/health", app.mobileHealth)
	mux.HandleFunc("/api/mobile/sessions", app.mobileSessions)
	mux.HandleFunc("/api/mobile/sessions/list", app.mobileSessions)
	mux.HandleFunc("/api/mobile/sessions/", app.mobileSessionByID)
	mux.HandleFunc("/api/mobile/personas", app.mobilePersonas)
	mux.HandleFunc("/api/mobile/chat", app.mobileChat)
	mux.HandleFunc("/api/mobile/chat/stream", app.mobileChatStream)
	mux.HandleFunc("/api/mobile/models", app.mobileModels)
	mux.HandleFunc("/api/mobile/history", app.mobileHistory)
	mux.HandleFunc("/api/mobile/status", app.auth(app.mobileStatus))
	mux.HandleFunc("/api/mobile/settings", app.auth(app.mobileSettings))
	mux.HandleFunc("/api/mobile/relay.py", app.mobileRelayPy)

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
	// Serve the panel with Cache-Control: no-cache, so a browser never
	// shows a stale panel after an update (the files change on every
	// release, and a normal refresh must always fetch the current
	// index.html/app.js — no hard refresh needed).
	webFiles := http.FileServer(http.Dir(webDir))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		webFiles.ServeHTTP(w, r)
	}))

	server := &http.Server{
		Addr:              env("HUB_ADDR", "0.0.0.0:8787"),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[Server] Graceful shutdown signal received. Stopping services...")
		bridgeCancel()
		workerCancel()
		orchPool.Stop()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if !provider.APIKeyEncryptionReady() {
		log.Printf("[Provider] WARNING: HUB_KEY_ENCRYPTION_KEY is not set; provider API keys are stored unencrypted. Run the installer or 'sudo cortext update' to generate one.")
	}
	log.Printf("Sahand-CorText v%s listening on %s", AppVersion, server.Addr)
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
