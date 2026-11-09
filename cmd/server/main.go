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
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT 'New conversation',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		role TEXT NOT NULL, content TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	PRAGMA journal_mode=WAL;
	PRAGMA foreign_keys=ON;
	`)
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
			jsonOut(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
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
		time.Sleep(700 * time.Millisecond)
		jsonOut(w, 401, map[string]string{"error": "نام کاربری یا رمز عبور نادرست است"})
		return
	}
	token, err := randomToken()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: token, Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sahand_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "sahand_session", Value: "", Path: "/", HttpOnly: true, Secure: env("HUB_COOKIE_SECURE", "false") == "true", SameSite: http.SameSiteStrictMode, MaxAge: -1})
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
	id, err := randomToken()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	id = id[:16]
	_, err = a.db.Exec(`INSERT INTO sessions(id,title) VALUES(?,?)`, id, "New conversation")
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	jsonOut(w, 201, Session{ID: id, Title: "New conversation"})
}

func (a *App) getMessages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	rows, err := a.db.Query(`SELECT id,role,content,created_at FROM messages WHERE session_id=? ORDER BY id`, id)
	if err != nil {
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
	if os.Getenv("GEMINI_API_KEY") == "" {
		jsonOut(w, 503, map[string]string{"error": "ابتدا GEMINI_API_KEY را در تنظیمات سرور وارد کن"})
		return
	}
	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, body.SessionID, "user", body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	start := time.Now()
	answer, err := callGemini(a.db, body.SessionID, body.Model)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 502, map[string]string{"error": "خطا در ارتباط با Gemini: " + err.Error()})
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
		if len([]rune(title)) > 48 {
			title = string([]rune(title)[:48]) + "…"
		}
		_, _ = a.db.Exec(`UPDATE sessions SET title=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, title, body.SessionID)
	} else {
		_, _ = a.db.Exec(`UPDATE sessions SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, body.SessionID)
	}
	jsonOut(w, 200, map[string]any{"reply": answer, "latency_ms": elapsed, "model": envModel(body.Model)})
}

func envModel(model string) string {
	switch model {
	case "", "gemini-3.8-flash":
		return "gemini-3.8-flash"
	case "gemini-4-argon":
		return "gemini-4-argon"
	default:
		return "gemini-3.8-flash"
	}
}

func callGemini(db *sql.DB, sessionID, model string) (string, error) {
	model = envModel(model)
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	rows, err := db.Query(`SELECT role, content FROM messages WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	contents := []any{}
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return "", err
		}
		apiRole := "user"
		if role == "assistant" {
			apiRole = "model"
		}
		contents = append(contents, map[string]any{"role": apiRole, "parts": []any{map[string]string{"text": content}}})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	payload := map[string]any{"contents": contents}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Gemini API returned HTTP %d", resp.StatusCode)
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
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("پاسخی از Gemini دریافت نشد")
	}
	return parsed.Candidates[0].Content.Parts[0].Text, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sahand-cortext hash-password <password>")
			os.Exit(2)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[2]), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(hash))
		return
	}
	dataDir := env("HUB_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "cortext.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, sessions: map[string]time.Time{}}
	if err := app.initDB(); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.auth(app.logout))
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
	mux.HandleFunc("/api/status", app.auth(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]any{"status": "online", "gemini_configured": os.Getenv("GEMINI_API_KEY") != "", "version": "0.1.0"})
	}))
	webDir := env("HUB_WEB_DIR", "./web/dist")
	mux.Handle("/", http.FileServer(http.Dir(webDir)))
	addr := env("HUB_ADDR", "127.0.0.1:8787")
	server := &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Sahand-Cortext listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
