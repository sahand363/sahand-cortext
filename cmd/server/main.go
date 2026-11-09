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
func (a *App) authenticated(r *http.Request) bool {
	c, err := r.Cookie("sahand_session")
	if err != nil {
		return false
	}
	a.mu.RLock()
	exp, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return ok && time.Now().Before(exp)
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
		jsonOut(w, 401, map[string]string{"error": "نام کاربری یا رمز عبور نادرست است"})
		return
	}
	token, err := randomToken()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "session error"})
		return
	}
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: token, Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("sahand_session"); e == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: "", Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: -1})
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, map[string]any{"status": "online", "gemini_configured": a.geminiKey() != "", "version": "0.2.0", "provider": "Gemini"})
}
func (a *App) geminiKey() string {
	if v := a.setting("gemini_api_key"); v != "" {
		return v
	}
	return os.Getenv("GEMINI_API_KEY")
}
func (a *App) saveGeminiKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || len(strings.TrimSpace(body.APIKey)) < 15 {
		jsonOut(w, 400, map[string]string{"error": "کلید API معتبر نیست"})
		return
	}
	if err := a.setSetting("gemini_api_key", strings.TrimSpace(body.APIKey)); err != nil {
		jsonOut(w, 500, map[string]string{"error": "ذخیره کلید انجام نشد"})
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
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
		jsonOut(w, 400, map[string]string{"error": "متن پیام نامعتبر است"})
		return
	}
	var exists int
	if a.db.QueryRow(`SELECT 1 FROM sessions WHERE id=?`, body.SessionID).Scan(&exists) != nil {
		jsonOut(w, 404, map[string]string{"error": "session not found"})
		return
	}
	key := a.geminiKey()
	if key == "" {
		jsonOut(w, 503, map[string]string{"error": "ابتدا کلید Gemini را در بخش تنظیمات ثبت کن"})
		return
	}
	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "user", body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	start := time.Now()
	answer, err := callGemini(key, body.Message, body.Model)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 502, map[string]string{"error": "ارتباط با Gemini ناموفق بود: " + err.Error()})
		return
	}
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
	jsonOut(w, 200, map[string]any{"reply": answer, "latency_ms": elapsed, "model": modelName(body.Model)})
}
func modelName(model string) string {
	switch model {
	case "gemini-2.5-pro":
		return model
	default:
		return "gemini-2.5-flash"
	}
}
func callGemini(key, prompt, model string) (string, error) {
	model = modelName(model)
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	payload := map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": prompt}}}}}
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
		return "", fmt.Errorf("Gemini API HTTP %d", resp.StatusCode)
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
		return "", errors.New("پاسخی دریافت نشد")
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
	server := &http.Server{Addr: env("HUB_ADDR", "127.0.0.1:8787"), Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Sahand-Cortext listening on %s", server.Addr)
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
