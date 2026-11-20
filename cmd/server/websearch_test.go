package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/websearch"
	_ "modernc.org/sqlite"
)

// A fake model: it asks for a search unless a web note is in its prompt,
// writes a note when asked to, and answers from the note.
func fakeSearchModel(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		reply := "Plain answer."
		switch {
		case strings.Contains(body, "research notes"):
			reply = "- CorText 9.9 was released on 2026-11-19 [1]"
		case strings.Contains(body, "WEB NOTE"):
			reply = "CorText 9.9 came out on 19 November 2026. Source: https://example.com/release"
		case strings.Contains(body, "release"):
			reply = "[[SEARCH: cortext 9.9 release date]]"
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
}

func TestWebSearchChatFlow(t *testing.T) {
	mock := fakeSearchModel(t)
	defer mock.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", mock.URL)
	searches := 0
	app := &App{
		db:          db,
		sessions:    map[string]time.Time{},
		router:      router,
		orchStorage: orchestration.NewStorage(db),
		web: &websearch.Engine{Search: func(ctx context.Context, q string, max int) ([]websearch.Result, error) {
			searches++
			return []websearch.Result{{Title: "CorText 9.9", URL: "https://example.com/release", Snippet: "Released Nov 19, 2026"}}, nil
		}},
	}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	_ = app.setSetting("openrouter_api_key", "sk-test-key")

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	w := httptest.NewRecorder()
	app.createSession(w, req)
	var sess Session
	_ = json.Unmarshal(w.Body.Bytes(), &sess)

	send := func(msg string) string {
		b, _ := json.Marshal(map[string]any{"session_id": sess.ID, "message": msg, "provider": "openrouter", "model": "model-a"})
		w := httptest.NewRecorder()
		app.sendMessage(w, httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(b)))
		if w.Code != 200 {
			t.Fatalf("chat %q failed (%d): %s", msg, w.Code, w.Body.String())
		}
		var resp struct {
			Reply string `json:"reply"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Reply
	}

	if got := send("When was the CorText 9.9 release?"); !strings.Contains(got, "19 November 2026") || strings.Contains(got, "[[") {
		t.Fatalf("first answer: %q", got)
	}
	if searches != 1 {
		t.Fatalf("searches = %d", searches)
	}

	// The note is saved and listed for the panel.
	w = httptest.NewRecorder()
	app.handleWebSearch(w, httptest.NewRequest(http.MethodGet, "/api/websearch", nil))
	var list struct {
		Mode  string           `json:"mode"`
		Count int              `json:"count"`
		Notes []websearch.Note `json:"notes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Mode != "auto" || list.Count != 1 || !strings.Contains(list.Notes[0].Summary, "2026-11-19") {
		t.Fatalf("notes api: %s", w.Body.String())
	}

	// A follow-up on the same topic reuses the note: no new search.
	if got := send("Tell me more about the CorText 9.9 release"); !strings.Contains(got, "19 November") {
		t.Fatalf("follow-up answer: %q", got)
	}
	if searches != 1 {
		t.Fatalf("follow-up searched again (%d)", searches)
	}

	// Bare /search shows usage; /search <q> forces a search.
	if got := send("/search"); !strings.Contains(got, "Usage") {
		t.Fatalf("bare /search: %q", got)
	}
	if got := send("/search something else entirely"); !strings.Contains(got, "19 November") {
		t.Fatalf("/search answer: %q", got)
	}
	if searches != 2 {
		t.Fatalf("/search did not search (%d)", searches)
	}

	// Mode off: no search, no notes, the marker never reaches the user.
	w = httptest.NewRecorder()
	app.handleWebSearch(w, httptest.NewRequest(http.MethodPut, "/api/websearch", strings.NewReader(`{"mode":"off"}`)))
	if w.Code != 200 {
		t.Fatalf("set mode: %s", w.Body.String())
	}
	if got := send("Any news on a new release?"); strings.Contains(got, "[[") {
		t.Fatalf("marker leaked in off mode: %q", got)
	}
	if searches != 2 {
		t.Fatalf("searched while off (%d)", searches)
	}

	// Clear all notes.
	w = httptest.NewRecorder()
	app.handleWebNotes(w, httptest.NewRequest(http.MethodDelete, "/api/websearch/notes", nil))
	if !strings.Contains(w.Body.String(), `"deleted":2`) {
		t.Fatalf("clear: %s", w.Body.String())
	}
}
