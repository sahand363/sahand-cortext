package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func TestChatConversationContinuity(t *testing.T) {
	// 1. Mock upstream AI server capturing transmitted message transcripts
	var recordedPayloads []struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		recordedPayloads = append(recordedPayloads, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"Acknowledged."}}]}`))
	}))
	defer mockServer.Close()

	// 2. In-memory SQLite Database
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	router := provider.NewRouter()
	router.OpenRouterBaseURL = mockServer.URL

	app := &App{
		db:       db,
		sessions: map[string]time.Time{},
		router:   router,
	}
	if err := app.initDB(); err != nil {
		t.Fatalf("initDB failed: %v", err)
	}
	_ = app.setSetting("openrouter_api_key", "sk-test-key")

	// 3. Create two independent sessions to verify isolation
	_, _ = db.Exec(`INSERT INTO sessions (id, title) VALUES ('session-1', 'Session 1')`)
	_, _ = db.Exec(`INSERT INTO sessions (id, title) VALUES ('session-2', 'Session 2')`)

	sendMessageReq := func(sessionID, message string) {
		payload, _ := json.Marshal(map[string]string{
			"session_id": sessionID,
			"message":    message,
			"provider":   "openrouter",
			"model":      "test-model",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(payload))
		w := httptest.NewRecorder()
		app.sendMessage(w, req)
		if w.Code != 200 {
			t.Fatalf("sendMessage failed with code %d: %s", w.Code, w.Body.String())
		}
	}

	// Turn 1 in Session 1
	sendMessageReq("session-1", "My secret code is 42.")
	if len(recordedPayloads) != 1 {
		t.Fatalf("expected 1 turn recorded, got %d", len(recordedPayloads))
	}
	if len(recordedPayloads[0].Messages) != 1 || recordedPayloads[0].Messages[0].Content != "My secret code is 42." {
		t.Errorf("turn 1 payload incorrect: %+v", recordedPayloads[0].Messages)
	}

	// Turn 2 in Session 1: Must receive turn 1 as context
	sendMessageReq("session-1", "What was my secret code?")
	if len(recordedPayloads) != 2 {
		t.Fatalf("expected 2 calls recorded, got %d", len(recordedPayloads))
	}
	msgsTurn2 := recordedPayloads[1].Messages
	if len(msgsTurn2) != 3 { // user -> assistant -> user
		t.Fatalf("expected 3 history turns in Turn 2, got %d: %+v", len(msgsTurn2), msgsTurn2)
	}
	if msgsTurn2[0].Content != "My secret code is 42." || msgsTurn2[0].Role != "user" {
		t.Errorf("turn 1 context missing in Turn 2: %+v", msgsTurn2[0])
	}
	if msgsTurn2[1].Content != "Acknowledged." || msgsTurn2[1].Role != "assistant" {
		t.Errorf("assistant answer missing in Turn 2 context: %+v", msgsTurn2[1])
	}
	if msgsTurn2[2].Content != "What was my secret code?" || msgsTurn2[2].Role != "user" {
		t.Errorf("latest user prompt missing: %+v", msgsTurn2[2])
	}

	// Turn 1 in Session 2: Must never receive Session 1 data
	sendMessageReq("session-2", "Is there any secret code?")
	if len(recordedPayloads) != 3 {
		t.Fatalf("expected 3 calls recorded, got %d", len(recordedPayloads))
	}
	msgsSession2 := recordedPayloads[2].Messages
	if len(msgsSession2) != 1 {
		t.Fatalf("session 2 leaked context from session 1: %+v", msgsSession2)
	}
	if msgsSession2[0].Content != "Is there any secret code?" {
		t.Errorf("unexpected content in session 2: %s", msgsSession2[0].Content)
	}
}
