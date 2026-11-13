package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "nokhodi_session"
const nameCookie = "nokhodi_name"

type Server struct {
	cfg      *Config
	cfgPath  string
	store    *Store
	rooms    *RoomStore
	sessions *SessionStore
	bans     *BanStore
	captchas *CaptchaStore
	broker   *Broker

	// presence[roomID][sessionToken] = last seen
	presMu   sync.Mutex
	presence map[string]map[string]presenceEntry
}

type presenceEntry struct {
	Name string
	Seen time.Time
}

func NewServer(cfg *Config, cfgPath string) (*Server, error) {
	rooms, err := NewRoomStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("rooms store: %w", err)
	}
	ids := []string{}
	for _, r := range rooms.All() {
		ids = append(ids, r.ID)
	}
	store, err := NewStore(cfg.DataDir, ids)
	if err != nil {
		return nil, fmt.Errorf("messages store: %w", err)
	}
	sessions, err := NewSessionStore(cfg.DataDir, time.Duration(cfg.SessionDays)*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("sessions store: %w", err)
	}
	bans, err := NewBanStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("bans store: %w", err)
	}
	s := &Server{
		cfg: cfg, cfgPath: cfgPath,
		store: store, rooms: rooms, sessions: sessions, bans: bans,
		captchas: NewCaptchaStore(), broker: NewBroker(),
		presence: map[string]map[string]presenceEntry{},
	}
	go s.pruner()
	return s, nil
}

// pruner deletes self-destructed messages every 30 seconds and tells
// the rooms' clients to reload. Deletions are rare (TTL expiry), so
// broadcasting a prune event to every room on any deletion is fine.
func (s *Server) pruner() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		s.pruneOnce()
	}
}

func (s *Server) pruneOnce() {
	if s.store.PruneExpired() > 0 {
		for _, r := range s.rooms.All() {
			s.broker.Publish(r.ID, Event{Type: "prune"})
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	web, _ := fs.Sub(webFS, "web")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// No-cache for the web UI: after a server update, a browser
		// that reloads must get the NEW page, not a cached old one.
		// (An already-open tab still needs a reload — updating the
		// server cannot reload a page that is already open.)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		// v2.3: secret front door. When SecretPath is set, "/" returns
		// 404 and the chat only opens at "/<SecretPath>". Private room
		// links /r/<code> always open (the code itself is the secret).
		// Assets (/style.css, /app.js) are public — they carry no secret.
		if s.cfg.SecretPath != "" && r.URL.Path == "/" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/"+s.cfg.SecretPath || strings.HasPrefix(r.URL.Path, "/r/") {
			data, err := webFS.ReadFile("web/index.html")
			if err != nil {
				http.Error(w, "frontend missing", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		if r.URL.Path != "/" {
			http.FileServer(http.FS(web)).ServeHTTP(w, r)
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "frontend missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "title": s.cfg.Title, "version": version,
			"messages": s.store.TotalCount(), "rooms": len(s.rooms.All()),
			"online": s.broker.Count(""),
			"tls":    s.cfg.TLSEnabled, "tls_mode": s.cfg.TLSMode,
			"url":                 fmt.Sprintf("%s://<server-ip>:%d", s.cfg.scheme(), s.cfg.Port),
			"secret_path_enabled": s.cfg.SecretPath != "",
		})
	})
	mux.HandleFunc("/api/captcha", s.handleCaptcha)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/identity", s.handleIdentity)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/rooms", s.handleRooms)
	mux.HandleFunc("/api/rooms/", s.handleRoomSub)
	mux.HandleFunc("/api/uploads/", s.handleUploadDownload)
	mux.HandleFunc("/api/admin/passwords", s.handleAdminPasswords)
	mux.HandleFunc("/api/admin/ban", s.handleAdminBan)
	mux.HandleFunc("/api/admin/unban", s.handleAdminUnban)
	mux.HandleFunc("/api/admin/bans", s.handleAdminBans)
	mux.HandleFunc("/api/admin/secret-path", s.handleAdminSecretPath)
	mux.HandleFunc("/api/private/", s.handlePrivateResolve)
	// Legacy v1 aliases: the single room is now #general.
	mux.HandleFunc("/api/messages", s.handleLegacyMessages)
	mux.HandleFunc("/api/events", s.handleLegacyEvents)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) sessionFromRequest(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return s.sessions.Get(c.Value)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, sess *Session) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.Token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: s.cfg.SessionDays * 24 * 3600, Expires: sess.ExpiresAt,
	})
}

func (s *Server) handleCaptcha(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	id, q, err := s.captchas.New()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create captcha")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "question": q})
}

// Login is password-only in v2. The password decides the role:
// OP password -> "op", everyone-else password -> "user".
// One generic error, so a wrong password does not reveal which exists.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var in struct {
		Password string `json:"password"`
		Username string `json:"username"` // accepted, ignored (v1 compat)
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	role := checkRole(s.cfg, in.Password)
	if role == "" {
		writeErr(w, http.StatusUnauthorized, "رمز عبور اشتباه است")
		return
	}
	sess, err := s.sessions.Create(role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create session")
		return
	}
	s.setSessionCookie(w, sess)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": role, "is_op": role == "op", "needs_name": true})
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var in struct {
		DisplayName    string `json:"display_name"`
		CaptchaID      string `json:"captcha_id"`
		CaptchaAnswer  int    `json:"captcha_answer"`
		CaptchaAnswerS string `json:"captcha_answer_str"`
	}
	body, err := readBody(r, 4096)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.CaptchaAnswerS != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(in.CaptchaAnswerS)); err == nil {
			in.CaptchaAnswer = n
		}
	}
	if !s.captchas.Verify(in.CaptchaID, in.CaptchaAnswer) {
		writeErr(w, http.StatusBadRequest, "جواب کپچا اشتباه است — دوباره تلاش کنید")
		return
	}
	if s.bans.IsBanned(in.DisplayName) {
		writeErr(w, http.StatusForbidden, "that display name is banned")
		return
	}
	if err := s.sessions.SetName(sess.Token, in.DisplayName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(in.DisplayName)
	http.SetCookie(w, &http.Cookie{
		Name: nameCookie, Value: name, Path: "/",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   s.cfg.SessionDays * 24 * 3600, Expires: sess.ExpiresAt,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "display_name": name, "role": sess.Role, "is_op": sess.IsOP()})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "has_name": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "has_name": sess.DisplayName != "",
		"display_name": sess.DisplayName, "title": s.cfg.Title,
		"role": sess.Role, "is_op": sess.IsOP(),
		"unlocked_rooms": sess.UnlockedRooms,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	for _, name := range []string{sessionCookie, nameCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) requireIdentity(w http.ResponseWriter, r *http.Request) *Session {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return nil
	}
	if strings.TrimSpace(sess.DisplayName) == "" {
		writeErr(w, http.StatusForbidden, "please pick a display name first")
		return nil
	}
	if s.bans.IsBanned(sess.DisplayName) && !sess.IsOP() {
		writeErr(w, http.StatusForbidden, "you are banned")
		return nil
	}
	return sess
}

func (s *Server) requireOP(w http.ResponseWriter, r *http.Request) *Session {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return nil
	}
	if !sess.IsOP() {
		writeErr(w, http.StatusForbidden, "only the OP can do that")
		return nil
	}
	return sess
}

// --- Rooms ---

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Hidden rooms are NOT in this list, unless the OP asks
		// (OP sees everything) or the session already unlocked one.
		out := []map[string]any{}
		for _, room := range s.rooms.All() {
			if !s.rooms.CanSee(sess, room, "") {
				continue
			}
			out = append(out, room.Public(sess.IsOP()))
		}
		writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
	case http.MethodPost:
		if !sess.IsOP() {
			writeErr(w, http.StatusForbidden, "only the OP can create rooms")
			return
		}
		var in struct {
			Name       string `json:"name"`
			Password   string `json:"password"`
			Hidden     bool   `json:"hidden"`
			TTLSeconds int64  `json:"ttl_seconds"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		room, err := s.rooms.Create(in.Name, in.Password, in.Hidden, in.TTLSeconds)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.store.EnsureRoom(room.ID)
		writeJSON(w, http.StatusCreated, room.Public(true))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// handleRoomSub routes /api/rooms/{id}, /join, /messages, /messages/{mid},
// /events, /presence, /online.
func (s *Server) handleRoomSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusNotFound, "room not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "":
		s.handleRoom(w, r, id)
	case "join":
		s.handleRoomJoin(w, r, id)
	case "private-link":
		s.handleRoomPrivateLink(w, r, id)
	case "upload":
		s.handleRoomUpload(w, r, id)
	case "messages":
		if len(parts) > 2 {
			mid, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad message id")
				return
			}
			s.handleRoomMessage(w, r, id, mid)
			return
		}
		s.handleRoomMessages(w, r, id)
	case "events":
		s.handleRoomEvents(w, r, id)
	case "presence":
		s.handleRoomPresence(w, r, id)
	case "online":
		s.handleRoomOnline(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// roomForRequest returns the room, or writes 404 and returns nil.
// Hidden rooms return 404 (not 403) to anyone without link/unlock,
// so their existence is not leaked.
func (s *Server) roomForRequest(w http.ResponseWriter, r *http.Request, sess *Session, id string) *Room {
	room := s.rooms.Get(id)
	if room == nil {
		writeErr(w, http.StatusNotFound, "room not found")
		return nil
	}
	if !s.rooms.CanSee(sess, room, r.URL.Query().Get("invite")) {
		writeErr(w, http.StatusNotFound, "room not found")
		return nil
	}
	return room
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, room.Public(sess.IsOP()))
	case http.MethodPatch:
		if !sess.IsOP() {
			writeErr(w, http.StatusForbidden, "only the OP can edit rooms")
			return
		}
		var in struct {
			Name       *string `json:"name"`
			Password   *string `json:"password"`
			Hidden     *bool   `json:"hidden"`
			TTLSeconds *int64  `json:"ttl_seconds"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		updated, err := s.rooms.Update(id, in.Name, in.Password, in.Hidden, in.TTLSeconds)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated.Public(true))
	case http.MethodDelete:
		if !sess.IsOP() {
			writeErr(w, http.StatusForbidden, "only the OP can delete rooms")
			return
		}
		if err := s.rooms.Delete(id); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.store.DeleteRoom(id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE only")
	}
}

func (s *Server) handleRoomJoin(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	var in struct {
		Password string `json:"password"`
		Invite   string `json:"invite"`
	}
	_ = decodeJSON(r, &in)
	// Invite can also come from the URL (?invite=...), for hidden links.
	if in.Invite == "" {
		in.Invite = r.URL.Query().Get("invite")
	}
	room := s.rooms.Get(id)
	if room == nil {
		writeErr(w, http.StatusNotFound, "room not found")
		return
	}
	if !s.rooms.CanSee(sess, room, in.Invite) {
		writeErr(w, http.StatusNotFound, "room not found")
		return
	}
	if !s.rooms.CheckRoomPassword(room, in.Password) {
		writeErr(w, http.StatusUnauthorized, "رمز اتاق اشتباه است")
		return
	}
	s.sessions.UnlockRoom(sess.Token, id)
	writeJSON(w, http.StatusOK, room.Public(sess.IsOP()))
}

func (s *Server) handleRoomMessages(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil {
		return
	}
	if !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": s.store.Last(id, limit), "room": room.Public(sess.IsOP())})
	case http.MethodPost:
		var in struct {
			Text string `json:"text"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		text := strings.TrimSpace(in.Text)
		if text == "" {
			writeErr(w, http.StatusBadRequest, "message is empty")
			return
		}
		if len([]rune(text)) > 1000 {
			writeErr(w, http.StatusBadRequest, "message is too long (max 1000 characters)")
			return
		}
		// /me command: "* name does something"
		if strings.HasPrefix(text, "/me ") {
			text = "* " + sess.DisplayName + " " + strings.TrimSpace(strings.TrimPrefix(text, "/me "))
		}
		m, err := s.store.Add(id, sess.DisplayName, text, room.TTLSeconds)
		if err != nil {
			log.Printf("store message: %v", err)
			writeErr(w, http.StatusInternalServerError, "could not save message")
			return
		}
		s.broker.Publish(id, Event{Type: "message", Message: &m})
		writeJSON(w, http.StatusCreated, m)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// DELETE /api/rooms/{id}/messages/{mid} — own message, or any message if OP.
func (s *Server) handleRoomMessage(w http.ResponseWriter, r *http.Request, id string, mid int64) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil {
		return
	}
	if !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	m := s.store.Get(id, mid)
	if m == nil {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	if !sess.IsOP() && m.Name != sess.DisplayName {
		writeErr(w, http.StatusForbidden, "you can only delete your own messages")
		return
	}
	if !s.store.Delete(id, mid) {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	s.broker.Publish(id, Event{Type: "delete", MessageID: mid})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRoomEvents(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil {
		return
	}
	if !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ch := s.broker.Subscribe(id)
	defer s.broker.Unsubscribe(ch)
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleRoomPresence(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil || !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	s.presMu.Lock()
	if s.presence[id] == nil {
		s.presence[id] = map[string]presenceEntry{}
	}
	s.presence[id][sess.Token] = presenceEntry{Name: sess.DisplayName, Seen: time.Now()}
	s.presMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRoomOnline(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	room := s.roomForRequest(w, r, sess, id)
	if room == nil || !s.rooms.CanAccess(sess, room) {
		writeErr(w, http.StatusForbidden, "join this room first")
		return
	}
	s.presMu.Lock()
	defer s.presMu.Unlock()
	names := []string{}
	cutoff := time.Now().Add(-90 * time.Second)
	for tok, e := range s.presence[id] {
		if e.Seen.Before(cutoff) {
			delete(s.presence[id], tok)
			continue
		}
		names = append(names, e.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": names})
}

// --- OP admin ---

func (s *Server) handleAdminPasswords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.requireOP(w, r) == nil {
		return
	}
	var in struct {
		Role        string `json:"role"` // "op" or "user"
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(in.NewPassword) < 6 {
		writeErr(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	}
	if err := setPassword(s.cfg, in.Role, in.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveConfig(s.cfgPath, s.cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminBan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.requireOP(w, r) == nil {
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(r, &in); err != nil || strings.TrimSpace(in.DisplayName) == "" {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.bans.Add(in.DisplayName)
	s.sessions.DeleteByName(in.DisplayName)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminUnban(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.requireOP(w, r) == nil {
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.bans.Remove(in.DisplayName)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminBans(w http.ResponseWriter, r *http.Request) {
	if s.requireOP(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"banned": s.bans.List()})
}

// --- Private room links + secret front door (v2.3) ---

// handlePrivateResolve is GET /api/private/{code} — turns a private
// link code into the room it opens. Requires a logged-in user; the
// code itself is the secret, and the room password (if any) is still
// asked for when joining. Unknown codes return 404, leaking nothing.
func (s *Server) handlePrivateResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sess := s.requireIdentity(w, r)
	if sess == nil {
		return
	}
	code := strings.TrimPrefix(r.URL.Path, "/api/private/")
	code = strings.Trim(code, "/")
	room := s.rooms.GetByPrivateCode(code)
	if room == nil {
		writeErr(w, http.StatusNotFound, "link not found — it may have been deleted by the OP")
		return
	}
	writeJSON(w, http.StatusOK, room.Public(sess.IsOP()))
}

// handleRoomPrivateLink is POST/DELETE /api/rooms/{id}/private-link.
// OP only. POST creates (or regenerates) the room's private link;
// DELETE removes it. The link is /r/<code> and opens only that room.
func (s *Server) handleRoomPrivateLink(w http.ResponseWriter, r *http.Request, id string) {
	if s.requireOP(w, r) == nil {
		return
	}
	room := s.rooms.Get(id)
	if room == nil {
		writeErr(w, http.StatusNotFound, "room not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"room_id": room.ID, "private_code": room.PrivateCode,
			"url": privateLinkURL(r, room.PrivateCode),
		})
	case http.MethodPost:
		code, err := s.rooms.SetPrivateCode(id)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"room_id": id, "private_code": code, "url": privateLinkURL(r, code),
		})
	case http.MethodDelete:
		if err := s.rooms.DeletePrivateCode(id); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET, POST or DELETE only")
	}
}

func privateLinkURL(r *http.Request, code string) string {
	if code == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/r/%s", scheme, r.Host, code)
}

// handleAdminSecretPath is GET/POST /api/admin/secret-path — OP only.
// POST {"action": "enable"|"disable"|"regenerate"} controls the secret
// front door. The menu always shows the current link, so the OP can
// never lock themself out of their own chat.
func (s *Server) handleAdminSecretPath(w http.ResponseWriter, r *http.Request) {
	if s.requireOP(w, r) == nil {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, secretPathResponse(r, s.cfg))
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "GET or POST only")
		return
	}
	var in struct {
		Action string `json:"action"` // enable, disable, regenerate
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	switch in.Action {
	case "enable":
		if s.cfg.SecretPath == "" {
			code, err := newRandomCode(14)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "could not generate a code")
				return
			}
			s.cfg.SecretPath = code
		}
	case "disable":
		s.cfg.SecretPath = ""
	case "regenerate":
		code, err := newRandomCode(14)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not generate a code")
			return
		}
		s.cfg.SecretPath = code
	default:
		writeErr(w, http.StatusBadRequest, "action must be enable, disable or regenerate")
		return
	}
	if err := saveConfig(s.cfgPath, s.cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save config")
		return
	}
	writeJSON(w, http.StatusOK, secretPathResponse(r, s.cfg))
}

func secretPathResponse(r *http.Request, cfg *Config) map[string]any {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	url := ""
	if cfg.SecretPath != "" {
		url = fmt.Sprintf("%s://%s/%s", scheme, r.Host, cfg.SecretPath)
	}
	return map[string]any{
		"enabled": cfg.SecretPath != "", "secret_path": cfg.SecretPath, "url": url,
	}
}

// --- Legacy v1 aliases (single room = #general) ---

func (s *Server) handleLegacyMessages(w http.ResponseWriter, r *http.Request) {
	// Rewrite to the general room handler by faking the path.
	s.handleRoomMessages(w, r, "general")
}

func (s *Server) handleLegacyEvents(w http.ResponseWriter, r *http.Request) {
	s.handleRoomEvents(w, r, "general")
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	var total int64
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			total += int64(n)
			if total > max {
				return nil, fmt.Errorf("body too large")
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}
