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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/mobile"
	"github.com/sahand363/sahand-cortext/internal/provider"
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
// provider/model are passed through when non-empty; when both are empty the
// handler falls back to the active provider/model, exactly as the web chat
// does when they are not given. personaID, when non-empty, is the persona
// for this chat — sendMessage uses the session's stored persona when it is
// empty, and updates the session's persona when it is given and different.
func (a *App) dispatchChat(r *http.Request, sessionID, message, provider, model, personaID string) (int, []byte) {
	payload := map[string]string{"session_id": sessionID, "message": message}
	if strings.TrimSpace(provider) != "" {
		payload["provider"] = strings.TrimSpace(provider)
	}
	if strings.TrimSpace(model) != "" {
		payload["model"] = strings.TrimSpace(model)
	}
	if strings.TrimSpace(personaID) != "" {
		payload["persona_id"] = strings.TrimSpace(personaID)
	}
	body, _ := json.Marshal(payload)
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

// dispatchCreateSession runs the web UI's createSession handler internally.
// personaID ("" = main chat) is passed through; createSession validates
// it against the persona store and falls back to "" when unknown.
func (a *App) dispatchCreateSession(r *http.Request, personaID string) (int, []byte) {
	payload := map[string]string{"persona_id": personaID}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(string(body)))
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.createSession(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchListSessions runs the web UI's listSessions handler internally
// (all non-Telegram sessions, pinned first, then most recent).
func (a *App) dispatchListSessions(r *http.Request) (int, []byte) {
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.listSessions(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchUpdateSession runs the web UI's updateSession handler
// internally (PATCH) to set a session's pinned flag.
func (a *App) dispatchUpdateSession(r *http.Request, id string, pinned bool) (int, []byte) {
	payload := map[string]any{"id": id, "pinned": pinned}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions", strings.NewReader(string(body)))
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.updateSession(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchDeleteSession runs the web UI's deleteSession handler
// internally (DELETE ?id=) to delete a session and its messages.
func (a *App) dispatchDeleteSession(r *http.Request, id string) (int, []byte) {
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions?id="+url.QueryEscape(id), nil)
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.deleteSession(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dispatchModels runs the web UI's handleModels handler internally (all
// providers) and returns its HTTP status + JSON body.
func (a *App) dispatchModels(r *http.Request) (int, []byte) {
	req := httptest.NewRequest(http.MethodGet, "/api/models?provider=all", nil)
	req = req.WithContext(r.Context())
	rec := httptest.NewRecorder()
	a.handleModels(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// --- mobile API -------------------------------------------------------------

// GET /api/mobile/health — plain (NOT encrypted), no auth. For the app's
// "Test connection" and the relay's health probe.
func (a *App) mobileHealth(w http.ResponseWriter, r *http.Request) {
	a.noteMobileRelay(r)
	jsonOut(w, 200, map[string]any{"ok": true, "mobile_enabled": a.mobileEnabled()})
}

// GET /api/mobile/sessions — encrypted body {} (the token check; the
// body is otherwise ignored). Lists the app/hub chats — the same list
// the web panel shows (non-Telegram sessions, pinned first, then most
// recent). Replies (encrypted) {"sessions": [{id, title, persona_id,
// pinned, updated_at}]}. The app and the hub list the same sessions
// table, so a chat started in either place shows up in the other.
//
// POST /api/mobile/sessions — encrypted body {"persona_id": "..."} (optional).
// Creates a CorText session (main chat when persona_id is empty, or a
// persona chat when it names a persona) and replies (encrypted)
// {"id": "<session id>"}.
func (a *App) mobileSessions(w http.ResponseWriter, r *http.Request) {
	// POST /api/mobile/sessions/list — the list endpoint as a POST.
	// The app uses POST, not GET-with-a-body, for the list: a GET
	// request body is not reliably carried by HttpURLConnection, so
	// the list came back empty even though the hub had the chats.
	// GET /api/mobile/sessions still lists, for backward compatibility.
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/list") {
		if a.readMobileBody(w, r) == nil {
			return
		}
		a.mobileListSessions(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if a.readMobileBody(w, r) == nil {
			return
		}
		a.mobileListSessions(w, r)
	case http.MethodPost:
		pt := a.readMobileBody(w, r)
		if pt == nil {
			return
		}
		var body struct {
			PersonaID string `json:"persona_id"`
		}
		_ = json.Unmarshal(pt, &body)
		status, respBody := a.dispatchCreateSession(r, body.PersonaID)
		if status != http.StatusCreated {
			writeMobilePlainError(w, status, respBody)
			return
		}
		var sess Session
		if json.Unmarshal(respBody, &sess) != nil || sess.ID == "" {
			jsonOut(w, 502, map[string]string{"error": "unexpected session response"})
			return
		}
		a.writeMobileEncrypted(w, map[string]string{"id": sess.ID})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// mobileListSessions is the shared list implementation for GET
// /api/mobile/sessions and POST /api/mobile/sessions/list. It reuses
// the web UI's listSessions handler, so the app and the hub list the
// same sessions table — a chat started in either place shows up in
// the other.
func (a *App) mobileListSessions(w http.ResponseWriter, r *http.Request) {
	status, body := a.dispatchListSessions(r)
	if status != http.StatusOK {
		writeMobilePlainError(w, status, body)
		return
	}
	var sessions []Session
	if json.Unmarshal(body, &sessions) != nil {
		jsonOut(w, 502, map[string]string{"error": "unexpected sessions response"})
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}
	a.writeMobileEncrypted(w, map[string]any{"sessions": sessions})
}

// mobilePersonaItem is one entry of the GET /api/mobile/personas reply.
type mobilePersonaItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GET or POST /api/mobile/personas — encrypted body {} (the token
// check; the body is otherwise ignored). Lists the hub's enabled
// personas (encrypted) {"personas": [{"id", "name"}]}. For the app's
// persona picker. POST is accepted as well as GET: the app uses POST,
// because a GET request body is not reliably carried by
// HttpURLConnection, so the list came back empty.
func (a *App) mobilePersonas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.readMobileBody(w, r) == nil {
		return
	}
	list, err := a.personaStore.List(r.Context(), false)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "could not list personas"})
		return
	}
	out := []mobilePersonaItem{}
	for _, p := range list {
		if !p.Enabled {
			continue
		}
		out = append(out, mobilePersonaItem{ID: p.ID, Name: p.Name})
	}
	a.writeMobileEncrypted(w, map[string]any{"personas": out})
}

// mobileSessionByID handles the /api/mobile/sessions/{id} subtree:
//   POST /api/mobile/sessions/{id}/pin — encrypted body {"pinned": bool}.
//     Sets the pinned flag and replies (encrypted) with the updated session.
//   DELETE /api/mobile/sessions/{id} — encrypted body {} (the token check).
//     Deletes the chat (its session row + its messages) and replies
//     (encrypted) {"ok": true}.
func (a *App) mobileSessionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/mobile/sessions/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "pin" && r.Method == http.MethodPost {
		pt := a.readMobileBody(w, r)
		if pt == nil {
			return
		}
		var body struct {
			Pinned *bool `json:"pinned"`
		}
		if json.Unmarshal(pt, &body) != nil || body.Pinned == nil {
			jsonOut(w, 400, map[string]string{"error": "pinned is required"})
			return
		}
		status, respBody := a.dispatchUpdateSession(r, parts[0], *body.Pinned)
		if status != http.StatusOK {
			writeMobilePlainError(w, status, respBody)
			return
		}
		var sess Session
		if json.Unmarshal(respBody, &sess) != nil {
			jsonOut(w, 502, map[string]string{"error": "unexpected session response"})
			return
		}
		a.writeMobileEncrypted(w, sess)
		return
	}
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		if a.readMobileBody(w, r) == nil {
			return
		}
		status, respBody := a.dispatchDeleteSession(r, parts[0])
		if status != http.StatusOK {
			writeMobilePlainError(w, status, respBody)
			return
		}
		a.writeMobileEncrypted(w, map[string]bool{"ok": true})
		return
	}
	http.Error(w, "not found", 404)
}

// POST /api/mobile/chat — encrypted body {"session_id","message"}.
// Optional "provider" and "model" choose the model for this chat (the
// app's model picker); when omitted the hub uses the active provider/model,
// exactly as the web chat does. Optional "persona_id" chooses the persona
// for this chat — when omitted the session's stored persona is used, and
// when given and different the session's persona is updated to it.
// Replies (encrypted) {"reply": "..."}.
// On a CorText error it returns a non-200 PLAIN JSON {"error": "..."} so
// the app can show the error text.
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
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		PersonaID string `json:"persona_id"`
	}
	if json.Unmarshal(pt, &body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}
	status, respBody := a.dispatchChat(r, body.SessionID, body.Message, body.Provider, body.Model, body.PersonaID)
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

// mobileSSEWriter encrypts each SSE event with the mobile token and
// flushes it immediately, so the app receives chunks as they arrive.
// Each event is one line: data: <encrypted wire>\n\n — the app decrypts
// the wire to get {"chunk": "..."} / {"done": true} / {"error": "..."}.
type mobileSSEWriter struct {
	w     http.ResponseWriter
	token string
}

func newMobileSSEWriter(w http.ResponseWriter, token string) *mobileSSEWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	return &mobileSSEWriter{w: w, token: token}
}

func (s *mobileSSEWriter) event(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	wire, err := mobile.Encrypt(s.token, data)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", wire)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// POST /api/mobile/chat/stream — encrypted body {"session_id","message","provider?","model?","persona_id?"}.
// Streams the AI reply as SSE: each data: line is one encrypted event
// {"chunk": "..."}; when the reply is complete the hub writes the FULL
// reply to the DB exactly like the buffered chat (so history/memory stay
// correct) and sends {"done": true}. On error it sends {"error": "..."}.
// /learn and /learn-deep commands and staged-skill confirmations are
// NOT streamed — they dispatch to sendMessage and return its reply as a
// single chunk, exactly like the buffered endpoint. The buffered
// /api/mobile/chat is untouched (the app falls back to it when the
// stream endpoint is missing on an older hub).
func (a *App) mobileChatStream(w http.ResponseWriter, r *http.Request) {
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
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		PersonaID string `json:"persona_id"`
	}
	if json.Unmarshal(pt, &body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}

	trimmedMsg := strings.TrimSpace(body.Message)

	// Commands and staged-skill confirmations are handled by sendMessage
	// (they return a short reply, not a model answer) — return that reply
	// as one chunk, not a stream.
	a.mu.Lock()
	_, hasStaged := a.webStagedSkill[body.SessionID]
	a.mu.Unlock()
	if (hasStaged && !strings.HasPrefix(trimmedMsg, "/")) || strings.HasPrefix(trimmedMsg, "/learn ") || strings.HasPrefix(trimmedMsg, "/learn-deep ") {
		status, respBody := a.dispatchChat(r, body.SessionID, body.Message, body.Provider, body.Model, body.PersonaID)
		sw := newMobileSSEWriter(w, a.mobileToken())
		if status != http.StatusOK {
			var errResp struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(respBody, &errResp)
			if errResp.Error == "" {
				errResp.Error = "chat failed"
			}
			sw.event(map[string]string{"error": errResp.Error})
			return
		}
		var resp struct {
			Reply string `json:"reply"`
		}
		if json.Unmarshal(respBody, &resp) != nil {
			sw.event(map[string]string{"error": "unexpected chat response"})
			return
		}
		sw.event(map[string]string{"chunk": resp.Reply})
		sw.event(map[string]bool{"done": true})
		return
	}

	// Resolve the chat (session / persona / provider / model) exactly
	// like sendMessage. This happens BEFORE any SSE header is sent, so
	// a failure here is still a plain HTTP error and the app can fall
	// back to the buffered endpoint.
	t, cerr := a.resolveChatTarget(r.Context(), body.SessionID, body.PersonaID, body.Provider, body.Model)
	if cerr != nil {
		errBody, _ := json.Marshal(map[string]string{"error": cerr.message})
		writeMobilePlainError(w, cerr.status, errBody)
		return
	}

	promptMessages, err := a.buildChatPrompt(r.Context(), t, body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	// Start the stream. From here on, every chunk is flushed as it
	// arrives. The skill/memory reads and the history rows are already
	// closed (buildChatPrompt), so the long model call holds no DB
	// connection, and the final DB write happens after the stream ends.
	sw := newMobileSSEWriter(w, a.mobileToken())
	answer, elapsed, err := a.router.CallStream(r.Context(), t.normProvider, a.providerKey(t.normProvider), t.chosenModel, promptMessages, provider.CallOpts{}, func(chunk string) {
		if chunk == "" {
			return
		}
		sw.event(map[string]string{"chunk": chunk})
	})
	if err != nil {
		a.saveTestRecord(t.normProvider, t.chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		sw.event(map[string]string{"error": fmt.Sprintf("[%s · %s] %s", t.providerName, t.chosenModel, err.Error())})
		return
	}

	// The stream finished: write the FULL reply to the DB exactly like
	// the buffered chat (history/memory stay correct), then signal done.
	if err := a.finishChat(t, body.Message, answer, elapsed); err != nil {
		sw.event(map[string]string{"error": "database error"})
		return
	}
	sw.event(map[string]bool{"done": true})
}

// POST /api/mobile/models — encrypted body {}. Returns the available
// models (encrypted) {"models": [...], "active_provider": "...",
// "active_model": "..."} — the same list the web UI's Models page shows.
// For the app's model picker. Only models with enabled=true are listed
// by the app; the hub returns the full list here.
func (a *App) mobileModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.readMobileBody(w, r) == nil {
		return
	}
	status, respBody := a.dispatchModels(r)
	if status != http.StatusOK {
		writeMobilePlainError(w, status, respBody)
		return
	}
	var resp struct {
		Models         []ModelInfo `json:"models"`
		ActiveProvider string      `json:"active_provider"`
		ActiveModel    string      `json:"active_model"`
	}
	if json.Unmarshal(respBody, &resp) != nil {
		jsonOut(w, 502, map[string]string{"error": "unexpected models response"})
		return
	}
	if resp.Models == nil {
		resp.Models = []ModelInfo{}
	}
	// The app's model picker must show exactly what the web main
	// chat shows. The web chat limits the list to the chat model
	// scope (mode "selected" + allowed_models, chosen in the web UI);
	// the app only knows the global Enabled flag, so the hub applies
	// the scope here. When the mode is "all", every enabled model
	// stays, matching the web chat.
	if scope := a.loadChatModelScope(); scope.Mode == "selected" {
		allowed := map[string]bool{}
		for _, key := range scope.AllowedModels {
			allowed[key] = true
		}
		kept := make([]ModelInfo, 0, len(resp.Models))
		for _, m := range resp.Models {
			if allowed[m.Provider+"::"+m.ID] {
				kept = append(kept, m)
			}
		}
		resp.Models = kept
	}
	a.writeMobileEncrypted(w, map[string]any{
		"models":          resp.Models,
		"active_provider": resp.ActiveProvider,
		"active_model":    resp.ActiveModel,
	})
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
