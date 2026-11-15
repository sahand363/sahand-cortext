package main

// Mobile (hub-owned) — the Android app talks to CorText directly (Direct
// mode) or through a dumb relay (Relay mode). One token (settings key
// "mobile_token") is the app's ONLY credential. It is BOTH the encryption
// key (SHA-256(token), see internal/mobile) and the auth credential: the
// hub verifies it by successfully decrypting the request body. The token
// is NEVER sent over the network in plaintext; the user copies it from
// the hub's Mobile page into the app. The relay holds no token and never
// decrypts. The AI models only ever talk to the hub; they know nothing
// about the app or its IP. See PROTOCOL-v2.md.
//
// How the mobile API reuses the web chat: the mobile chat / history /
// sessions handlers do NOT duplicate the AI call, the memory/skill prompt
// injection, or the DB writes. They dispatch internally to the exact same
// handlers the web UI uses (sendMessage / getMessages / createSession)
// via httptest.NewRecorder. That guarantees the app gets exactly the same
// provider/model defaults, memory + skill behaviour and DB writes as the
// web chat — and the existing handlers are not touched at all, so a
// mobile failure cannot crash or block the main chat.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/mobile"
)

const (
	mobileSettingToken     = "mobile_token"
	mobileSettingEnabled   = "mobile_enabled"
	mobileSettingRelayIP   = "mobile_relay_ip"
	mobileSettingRelayPort = "mobile_relay_port"
)

// --- token + settings -------------------------------------------------------

// mobileToken returns the stored token, or "" if none has been generated.
func (a *App) mobileToken() string { return a.setting(mobileSettingToken) }

// mobileEnabled reports whether the mobile feature is activated.
func (a *App) mobileEnabled() bool { return a.setting(mobileSettingEnabled) == "true" }

// newMobileToken generates a fresh 32-byte base64 token.
func newMobileToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ensureMobileToken returns the stored token, generating + storing one if absent.
func (a *App) ensureMobileToken() (string, error) {
	if t := a.mobileToken(); t != "" {
		return t, nil
	}
	t, err := newMobileToken()
	if err != nil {
		return "", err
	}
	if err := a.setSetting(mobileSettingToken, t); err != nil {
		return "", err
	}
	return t, nil
}

// regenerateMobileToken creates a fresh token and stores it. The old token
// stops working at once (the app and the relay must be updated with it).
func (a *App) regenerateMobileToken() (string, error) {
	t, err := newMobileToken()
	if err != nil {
		return "", err
	}
	if err := a.setSetting(mobileSettingToken, t); err != nil {
		return "", err
	}
	return t, nil
}

// mobileRelayPort returns the configured middle-server port (default 8080).
func (a *App) mobileRelayPort() int {
	if v := a.setting(mobileSettingRelayPort); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	return 8080
}

// mobileHubPort returns the port CorText listens on (from HUB_ADDR, default 8787).
func (a *App) mobileHubPort() int {
	addr := env("HUB_ADDR", "0.0.0.0:8787")
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(addr[i+1:])); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	return 8787
}

// --- status tracking --------------------------------------------------------

func (a *App) recordMobileAppSeen() {
	a.mobileMu.Lock()
	a.mobileLastAppSeen = time.Now()
	a.mobileMu.Unlock()
}

func (a *App) recordMobileRelaySeen(id string) {
	a.mobileMu.Lock()
	a.mobileLastRelaySeen = time.Now()
	a.mobileRelayID = id
	a.mobileMu.Unlock()
}

// noteMobileRelay records the relay header (if present) on a mobile request.
func (a *App) noteMobileRelay(r *http.Request) {
	if id := strings.TrimSpace(r.Header.Get("X-Mobile-Relay")); id != "" {
		a.recordMobileRelaySeen(id)
	}
}

// --- encrypted body helpers -------------------------------------------------

// readMobileBody decrypts the request body with the stored token's key.
// The token is verified by successful decryption: if the tag does not
// verify, it writes HTTP 400 plain JSON {"error": "decryption failed"} and
// returns nil. It also records the app + relay as seen. If the mobile
// feature is not activated, it writes HTTP 403 and returns nil.
func (a *App) readMobileBody(w http.ResponseWriter, r *http.Request) []byte {
	if !a.mobileEnabled() {
		jsonOut(w, 403, map[string]string{"error": "mobile app is not activated"})
		return nil
	}
	token := a.mobileToken()
	if token == "" {
		jsonOut(w, 403, map[string]string{"error": "mobile app is not activated"})
		return nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "invalid request body"})
		return nil
	}
	pt, err := mobile.Decrypt(token, strings.TrimSpace(string(raw)))
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "decryption failed"})
		return nil
	}
	a.noteMobileRelay(r)
	a.recordMobileAppSeen()
	return pt
}

// writeMobileEncrypted encrypts v as JSON and writes it as the response
// body (the encrypted wire format).
func (a *App) writeMobileEncrypted(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "encoding error"})
		return
	}
	wire, err := mobile.Encrypt(a.mobileToken(), data)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "encryption error"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(wire))
}

// writeMobilePlainError writes a plain JSON error body (used when a mobile
// chat/history/session request fails inside CorText, so the app can read
// and show the "error" text).
func writeMobilePlainError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- internal dispatch to the web handlers ----------------------------------

// dispatchChat runs the web UI's sendMessage handler internally (the exact
// same function, same behaviour) and returns its HTTP status + JSON body.
func (a *App) dispatchChat(r *http.Request, sessionID, message string) (int, []byte) {
	body, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": message})
	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(string(body)))
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.sendMessage(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchHistory runs the web UI's getMessages handler internally.
func (a *App) dispatchHistory(r *http.Request, sessionID string) (int, []byte) {
	req := httptest.NewRequest(http.MethodGet, "/api/messages?session_id="+url.QueryEscape(sessionID), nil)
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.getMessages(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchCreateSession runs the web UI's createSession handler internally
// (main chat, no persona).
func (a *App) dispatchCreateSession(r *http.Request) (int, []byte) {
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"persona_id": ""}`))
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.createSession(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// --- mobile API -------------------------------------------------------------

// GET /api/mobile/health — plain (NOT encrypted), no auth. For the app's
// "Test connection" and the relay's health probe.
func (a *App) mobileHealth(w http.ResponseWriter, r *http.Request) {
	a.noteMobileRelay(r)
	jsonOut(w, 200, map[string]any{"ok": true, "mobile_enabled": a.mobileEnabled()})
}

// POST /api/mobile/sessions — encrypted body {}. Creates a CorText session
// (main chat) and replies (encrypted) {"id": "<session id>"}.
func (a *App) mobileSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.readMobileBody(w, r) == nil {
		return
	}
	status, body := a.dispatchCreateSession(r)
	if status != http.StatusCreated {
		writeMobilePlainError(w, status, body)
		return
	}
	var sess Session
	if json.Unmarshal(body, &sess) != nil || sess.ID == "" {
		jsonOut(w, 502, map[string]string{"error": "unexpected session response"})
		return
	}
	a.writeMobileEncrypted(w, map[string]string{"id": sess.ID})
}

// POST /api/mobile/chat — encrypted body {"session_id","message"}.
// Replies (encrypted) {"reply": "..."}. On a CorText error it returns a
// non-200 PLAIN JSON {"error": "..."} so the app can show the error text.
func (a *App) mobileChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pt := a.readMobileBody(w, r)
	if pt == nil {
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(pt, &body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}
	status, respBody := a.dispatchChat(r, body.SessionID, body.Message)
	if status != http.StatusOK {
		writeMobilePlainError(w, status, respBody)
		return
	}
	var resp struct {
		Reply string `json:"reply"`
	}
	if json.Unmarshal(respBody, &resp) != nil {
		jsonOut(w, 502, map[string]string{"error": "unexpected chat response"})
		return
	}
	a.writeMobileEncrypted(w, map[string]string{"reply": resp.Reply})
}

// POST /api/mobile/history — encrypted body {"session_id"}.
// Replies (encrypted) {"messages": [{"role","content",...}]}.
func (a *App) mobileHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pt := a.readMobileBody(w, r)
	if pt == nil {
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(pt, &body) != nil || strings.TrimSpace(body.SessionID) == "" {
		jsonOut(w, 400, map[string]string{"error": "session_id is required"})
		return
	}
	status, respBody := a.dispatchHistory(r, body.SessionID)
	if status != http.StatusOK {
		writeMobilePlainError(w, status, respBody)
		return
	}
	var msgs []Message
	if json.Unmarshal(respBody, &msgs) != nil {
		jsonOut(w, 502, map[string]string{"error": "unexpected history response"})
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	a.writeMobileEncrypted(w, map[string]any{"messages": msgs})
}

// mobileStatusPayload builds the GET /api/mobile/status reply. The token is
// included here (the logged-in admin is allowed to see it so they can copy
// it into the app); it is never returned on any other endpoint.
func (a *App) mobileStatusPayload() map[string]any {
	a.mobileMu.Lock()
	appSeen := a.mobileLastAppSeen
	relaySeen := a.mobileLastRelaySeen
	relayID := a.mobileRelayID
	a.mobileMu.Unlock()
	var appSeenStr, relaySeenStr any
	if !appSeen.IsZero() {
		appSeenStr = appSeen.Format(time.RFC3339)
	}
	if !relaySeen.IsZero() {
		relaySeenStr = relaySeen.Format(time.RFC3339)
	}
	return map[string]any{
		"enabled":         a.mobileEnabled(),
		"token":           a.mobileToken(),
		"relay_ip":        a.setting(mobileSettingRelayIP),
		"relay_port":      a.mobileRelayPort(),
		"hub_port":        a.mobileHubPort(),
		"app_last_seen":   appSeenStr,
		"relay_last_seen": relaySeenStr,
		"relay_id":        relayID,
	}
}

// GET /api/mobile/status — web-UI auth only (the normal CorText login
// cookie, NOT the mobile token). For the hub Mobile page.
func (a *App) mobileStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	jsonOut(w, 200, a.mobileStatusPayload())
}

// POST /api/mobile/settings — web-UI auth only. Body (plain JSON):
// {"enabled": bool, "relay_ip": "...", "relay_port": 8080,
//  "regenerate_token": bool}. Replies with the status shape.
func (a *App) mobileSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Enabled         *bool   `json:"enabled"`
		RelayIP         *string `json:"relay_ip"`
		RelayPort       *int    `json:"relay_port"`
		RegenerateToken bool    `json:"regenerate_token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "invalid settings"})
		return
	}
	if body.Enabled != nil {
		_ = a.setSetting(mobileSettingEnabled, strconv.FormatBool(*body.Enabled))
	}
	if body.RelayIP != nil {
		_ = a.setSetting(mobileSettingRelayIP, strings.TrimSpace(*body.RelayIP))
	}
	if body.RelayPort != nil && *body.RelayPort > 0 && *body.RelayPort <= 65535 {
		_ = a.setSetting(mobileSettingRelayPort, strconv.Itoa(*body.RelayPort))
	}
	// Enabling for the first time (or regenerating) creates a fresh token.
	if body.RegenerateToken || (a.mobileEnabled() && a.mobileToken() == "") {
		if _, err := a.regenerateMobileToken(); err != nil {
			jsonOut(w, 500, map[string]string{"error": "could not generate token"})
			return
		}
	}
	jsonOut(w, 200, a.mobileStatusPayload())
}

// GET /api/mobile/relay.py — plain (NOT encrypted, no auth). Serves the
// embedded relay script. This is what the one-liner downloads.
func (a *App) mobileRelayPy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mobile.RelayPy)
}
