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
	// 1. Mock upstream server for chat
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"Acknowledged."}}]}`))
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
	router.OpenRouterBaseURL = mockServer.URL
	orchStorage := orchestration.NewStorage(db)
	orchPool := orchestration.NewWorkerPool(orchStorage, nil, 1)

	app := &App{
		db:          db,
		sessions:    map[string]time.Time{},
		router:      router,
		orchStorage: orchStorage,
		orchPool:    orchPool,
	}
	if err := app.initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	_ = app.setSetting("openrouter_api_key", "sk-test-key")

	// 3. Test Orchestration Job REST Endpoints
	t.Run("Orchestration Job Lifecycle", func(t *testing.T) {
		// Create Job
		createBody, _ := json.Marshal(map[string]any{
			"task":       "Perform architectural audit",
			"max_rounds": 4,
			"max_calls":  8,
			"metadata":   map[string]string{"env": "test"},
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
		if len(events) < 2 { // created + cancelled
			t.Fatalf("expected at least 2 events, got %d", len(events))
		}
	})
}
