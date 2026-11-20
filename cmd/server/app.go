package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/channel"
	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
	"github.com/sahand363/sahand-cortext/internal/websearch"
	_ "modernc.org/sqlite"
)

type App struct {
	db              *sql.DB
	mu              sync.RWMutex
	sessions        map[string]time.Time
	webStagedSkill  map[string]*skill.StagedDraft // keyed by session_id
	router          *provider.Router
	personaStore    *persona.Store
	skillStore      *skill.Store
	memoryStore     *memory.Store
	memoryExtractMu sync.Mutex // serialises background memory extraction runs
	// Mobile (hub-owned): in-memory connection status, fed by /api/mobile/status.
	mobileMu            sync.Mutex
	mobileLastAppSeen   time.Time
	mobileLastRelaySeen time.Time
	mobileRelayID       string
	orchStorage         *orchestration.Storage
	orchEngine          *orchestration.CollaborationEngine
	orchPool            *orchestration.WorkerPool
	loginLimit          *loginLimiter
	channels            *channel.Service
	web                 *websearch.Engine
	webNotes            *websearch.SQLStore
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
		`CREATE TABLE IF NOT EXISTS usage_events(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			persona_id TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			success INTEGER NOT NULL DEFAULT 1,
			error TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events(created_at);`,
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
	if a.channels != nil {
		if err := a.channels.Store.InitSchema(); err != nil {
			return err
		}
	}
	if a.webNotes == nil {
		a.webNotes = websearch.NewSQLStore(a.db)
	}
	if err := a.webNotes.InitSchema(); err != nil {
		return err
	}
	if a.web == nil {
		a.web = newWebEngine()
	}
	if err := a.orchStorage.InitSchema(); err != nil {
		return err
	}
	return a.router.AttachDB(context.Background(), a.db)
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
