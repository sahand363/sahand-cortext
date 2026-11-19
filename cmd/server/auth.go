package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
