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
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	InputLimit  int    `json:"input_limit"`
	OutputLimit int    `json:"output_limit"`
	Status      string `json:"status"`     // "untested", "working", "failed"
	LatencyMs   int64  `json:"latency_ms"` // last test latency
	LastError   string `json:"last_error"` // error from last test if failed
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

func (a *App) loadTestRecords() map[string]ModelTestRecord {
	out := map[string]ModelTestRecord{}
	raw := a.setting("gemini_model_tests")
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) saveTestRecord(model string, rec ModelTestRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords()
	records[model] = rec
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting("gemini_model_tests", string(data))
	}
}

func (a *App) activeModel() string {
	if m := a.setting("gemini_active_model"); m != "" {
		return m
	}
	return "gemini-2.0-flash"
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
	jsonOut(w, 200, map[string]any{
		"status":            "online",
		"gemini_configured": a.geminiKey() != "",
		"active_model":      a.activeModel(),
		"version":           "0.2.2",
		"provider":          "Gemini",
	})
}

func (a *App) geminiKey() string {
	if v := a.setting("gemini_api_key"); v != "" {
		return v
	}
	return os.Getenv("GEMINI_API_KEY")
}

// Save API key and immediately fetch all available models for that key
func (a *App) saveGeminiKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid request payload"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = a.geminiKey()
	}
	if len(key) < 15 {
		jsonOut(w, 400, map[string]string{"error": "Please enter a valid Gemini API key."})
		return
	}

	// Verify key by listing models from Google API
	start := time.Now()
	models, err := a.fetchModelsFromGoogle(key)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}

	// Save key in SQLite
	if err := a.setSetting("gemini_api_key", key); err != nil {
		jsonOut(w, 500, map[string]string{"error": "Failed to store API key in database"})
		return
	}

	jsonOut(w, 200, map[string]any{
		"ok":           true,
		"latency_ms":   latency,
		"models":       models,
		"active_model": a.activeModel(),
	})
}

// GET /api/models - Returns all generateContent models from Google + their test status
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	key := a.geminiKey()
	if key == "" {
		jsonOut(w, 200, map[string]any{"models": []ModelInfo{}, "active_model": a.activeModel(), "configured": false})
		return
	}
	models, err := a.fetchModelsFromGoogle(key)
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error(), "models": []ModelInfo{}, "active_model": a.activeModel()})
		return
	}
	jsonOut(w, 200, map[string]any{
		"models":       models,
		"active_model": a.activeModel(),
		"configured":   true,
	})
}

// POST /api/models/test - Tests a specific model ID and sets it as active if it works
func (a *App) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Model      string `json:"model"`
		SetActive  bool   `json:"set_active"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		jsonOut(w, 400, map[string]string{"error": "Model ID is required"})
		return
	}
	model := strings.TrimPrefix(strings.TrimSpace(body.Model), "models/")
	key := a.geminiKey()
	if key == "" {
		jsonOut(w, 400, map[string]string{"error": "Save your Gemini API key first"})
		return
	}

	start := time.Now()
	reply, err := callGemini(key, "Reply with the single word OK.", model, 12)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		a.saveTestRecord(model, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: err.Error(),
		})
		jsonOut(w, 200, map[string]any{
			"ok":           false,
			"model":        model,
			"status":       "failed",
			"latency_ms":   latency,
			"error":        err.Error(),
			"active_model": a.activeModel(),
		})
		return
	}

	a.saveTestRecord(model, ModelTestRecord{
		Status:    "working",
		LatencyMs: latency,
		LastError: "",
	})
	if body.SetActive {
		_ = a.setSetting("gemini_active_model", model)
	}

	jsonOut(w, 200, map[string]any{
		"ok":           true,
		"model":        model,
		"status":       "working",
		"latency_ms":   latency,
		"reply":        strings.TrimSpace(reply),
		"active_model": a.activeModel(),
	})
}

func (a *App) fetchModelsFromGoogle(key string) ([]ModelInfo, error) {
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
			OutputTokenLimit           int      `json:"outputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	records := a.loadTestRecords()
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
		// Focus on Gemini / Gemma text & multimodal chat models
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
			ID:          id,
			DisplayName: disp,
			Description: m.Description,
			InputLimit:  m.InputTokenLimit,
			OutputLimit: m.OutputTokenLimit,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
		})
	}

	// Sort: working models first, then untested, then failed; newest versions first
	sort.Slice(out, func(i, j int) bool {
		rank := func(s string) int {
			switch s {
			case "working":
				return 0
			case "untested":
				return 1
			default:
				return 2
			}
		}
		if rank(out[i].Status) != rank(out[j].Status) {
			return rank(out[i].Status) < rank(out[j].Status)
		}
		return out[i].ID > out[j].ID
	})

	return out, nil
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
	key := a.geminiKey()
	if key == "" {
		jsonOut(w, 503, map[string]string{"error": "Please save your Gemini API key in Settings first."})
		return
	}
	chosenModel := strings.TrimPrefix(strings.TrimSpace(body.Model), "models/")
	if chosenModel == "" {
		chosenModel = a.activeModel()
	}

	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "user", body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	start := time.Now()
	answer, err := callGemini(key, body.Message, chosenModel, 0)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		a.saveTestRecord(chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		jsonOut(w, 502, map[string]string{"error": fmt.Sprintf("Model '%s' failed: %s (Tip: Go to 'Models & providers' or 'Settings' and click models to see which ones work with your API key)", chosenModel, err.Error())})
		return
	}

	// Mark model as working and active
	a.saveTestRecord(chosenModel, ModelTestRecord{Status: "working", LatencyMs: elapsed})
	_ = a.setSetting("gemini_active_model", chosenModel)

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
	jsonOut(w, 200, map[string]any{"reply": answer, "latency_ms": elapsed, "model": chosenModel})
}

func callGemini(key, prompt, model string, maxTokens int) (string, error) {
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
	if maxTokens > 0 {
		payload["generationConfig"] = map[string]any{"maxOutputTokens": maxTokens}
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
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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
		return "", errors.New("empty response from model")
	}
	return parsed.Candidates[0].Content.Parts[0].Text, nil
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
	mux.HandleFunc("/api/setup/gemini", app.auth(app.saveGeminiKey))
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
