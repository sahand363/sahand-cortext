package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func TestChatContinuityAndOrchestrationAPIs(t *testing.T) {
	// 1. Mock upstream server for chat and collaboration
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"complete\",\"message\":\"Acknowledged.\",\"completion_summary\":\"Done\"}"}}]}`))
	}))
	defer mockServer.Close()

	// 2. Setup SQLite
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", mockServer.URL)
	orchStorage := orchestration.NewStorage(db)

	app := &App{
		db:          db,
		sessions:    map[string]time.Time{},
		router:      router,
		orchStorage: orchStorage,
	}
	collabEngine := orchestration.NewCollaborationEngine(
		orchStorage,
		router,
		app.providerKey,
		func() (string, string) { return app.activeProvider(), app.activeModel() },
		orchestration.DefaultCollaborationConfig(),
	)
	collabEngine.SetProviderConfiguredChecker(app.isProviderConfigured)
	orchPool := orchestration.NewWorkerPool(orchStorage, collabEngine, 1)
	app.orchEngine = collabEngine
	app.orchPool = orchPool

	if err := app.initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	_ = app.setSetting("openrouter_api_key", "sk-test-key")

	// 3. Test Orchestration Job REST Endpoints with Participants
	t.Run("Orchestration Job Lifecycle", func(t *testing.T) {
		createBody, _ := json.Marshal(map[string]any{
			"task":       "Perform architectural audit",
			"max_rounds": 4,
			"max_calls":  8,
			"participants": []map[string]string{
				{"role": "coordinator", "name": "Architect", "provider": "openrouter", "model": "model-a"},
				{"role": "worker", "name": "Developer", "provider": "openrouter", "model": "model-b"},
			},
			"metadata": map[string]string{"env": "test"},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/orchestration/jobs", bytes.NewReader(createBody))
		w := httptest.NewRecorder()
		app.orchCreateJob(w, req)
		if w.Code != 201 {
			t.Fatalf("create job failed code %d: %s", w.Code, w.Body.String())
		}

		var created orchestration.Job
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		if created.ID == "" || created.Status != orchestration.StatusQueued {
			t.Fatalf("invalid created job: %+v", created)
		}

		// Get Job by ID
		req = httptest.NewRequest(http.MethodGet, "/api/orchestration/jobs/"+created.ID, nil)
		w = httptest.NewRecorder()
		app.orchGetJob(w, req)
		if w.Code != 200 {
			t.Fatalf("get job failed code %d: %s", w.Code, w.Body.String())
		}

		// Get Collaboration Messages endpoint
		req = httptest.NewRequest(http.MethodGet, "/api/orchestration/jobs/"+created.ID+"/messages", nil)
		w = httptest.NewRecorder()
		app.orchGetJob(w, req)
		if w.Code != 200 {
			t.Fatalf("get messages failed code %d: %s", w.Code, w.Body.String())
		}

		// List Jobs
		req = httptest.NewRequest(http.MethodGet, "/api/orchestration/jobs?limit=10", nil)
		w = httptest.NewRecorder()
		app.orchListJobs(w, req)
		if w.Code != 200 {
			t.Fatalf("list jobs failed code %d: %s", w.Code, w.Body.String())
		}
		var list []orchestration.Job
		_ = json.Unmarshal(w.Body.Bytes(), &list)
		if len(list) != 1 {
			t.Fatalf("expected 1 listed job, got %d", len(list))
		}

		// Cancel Job
		req = httptest.NewRequest(http.MethodPost, "/api/orchestration/jobs/"+created.ID+"/cancel", nil)
		w = httptest.NewRecorder()
		app.orchGetJob(w, req)
		if w.Code != 200 {
			t.Fatalf("cancel job failed code %d: %s", w.Code, w.Body.String())
		}

		// Get Job Events
		req = httptest.NewRequest(http.MethodGet, "/api/orchestration/jobs/"+created.ID+"/events", nil)
		w = httptest.NewRecorder()
		app.orchGetJob(w, req)
		if w.Code != 200 {
			t.Fatalf("get events failed code %d: %s", w.Code, w.Body.String())
		}
		var events []orchestration.Event
		_ = json.Unmarshal(w.Body.Bytes(), &events)
		if len(events) < 2 {
			t.Fatalf("expected at least 2 events, got %d", len(events))
		}
	})

	t.Run("Dynamic 5th OpenAI-Compatible Provider End-to-End", func(t *testing.T) {
		dynServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"mistral-large-latest","name":"Mistral Large"}]}`))
			case "/v1/chat/completions":
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Reply from dynamic 5th provider"}}]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer dynServer.Close()

		// 1. Register 5th provider via REST API
		createProvBody, _ := json.Marshal(map[string]any{
			"id":       "mistral-custom",
			"name":     "Mistral Custom",
			"base_url": dynServer.URL + "/v1",
			"api_key":  "sk-mistral-test-1234",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(createProvBody))
		w := httptest.NewRecorder()
		app.handleProviders(w, req)
		if w.Code != 201 {
			t.Fatalf("create dynamic provider failed (%d): %s", w.Code, w.Body.String())
		}

		// 2. Verify /api/status exposes the new provider dynamically
		req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w = httptest.NewRecorder()
		app.status(w, req)
		var statusResp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &statusResp)
		provs, _ := statusResp["providers"].(map[string]any)
		if provs["mistral-custom"] != true {
			t.Fatalf("expected mistral-custom to be configured in /api/status, got: %+v", statusResp)
		}

		// 3. Create a chat session and send a message routed to the 5th provider
		req = httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
		w = httptest.NewRecorder()
		app.createSession(w, req)
		var sess Session
		_ = json.Unmarshal(w.Body.Bytes(), &sess)

		msgBody, _ := json.Marshal(map[string]any{
			"session_id": sess.ID,
			"message":    "Hello Mistral",
			"provider":   "mistral-custom",
			"model":      "mistral-large-latest",
		})
		req = httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(msgBody))
		w = httptest.NewRecorder()
		app.sendMessage(w, req)
		if w.Code != 200 {
			t.Fatalf("chat with dynamic provider failed (%d): %s", w.Code, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("Reply from dynamic 5th provider")) {
			t.Fatalf("unexpected chat response: %s", w.Body.String())
		}

		// 4. Delete dynamic provider via REST API
		req = httptest.NewRequest(http.MethodDelete, "/api/providers/mistral-custom", nil)
		w = httptest.NewRecorder()
		app.handleProviderByID(w, req)
		if w.Code != 200 {
			t.Fatalf("delete dynamic provider failed (%d): %s", w.Code, w.Body.String())
		}
	})
}
