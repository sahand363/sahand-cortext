package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

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
