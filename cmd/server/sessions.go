package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/persona"
	_ "modernc.org/sqlite"
)

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
