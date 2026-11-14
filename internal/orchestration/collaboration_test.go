package orchestration

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func TestParseStructuredResponseValidationAndFallback(t *testing.T) {
	rawValid := `{"action":"complete","message":"All tests pass and code is clean.","completion_summary":"Implemented feature X."}`
	res1 := ParseStructuredResponse(rawValid, ActionContinue)
	if res1.Action != ActionComplete || res1.Message != "All tests pass and code is clean." || res1.CompletionSummary != "Implemented feature X." {
		t.Fatalf("unexpected valid parse result: %+v", res1)
	}

	rawUnsupported := "```json\n{\"action\":\"invalid_unknown_action\",\"message\":\"Drafting architecture\"}\n```"
	res2 := ParseStructuredResponse(rawUnsupported, ActionContinue)
	if res2.Action != ActionContinue || res2.Message != "Drafting architecture" {
		t.Fatalf("expected fallback to ActionContinue on unsupported action, got: %+v", res2)
	}

	rawMalformed := "Here is my response without valid JSON { broken: 123"
	res3 := ParseStructuredResponse(rawMalformed, ActionRevise)
	if res3.Action != ActionRevise || res3.Message != rawMalformed {
		t.Fatalf("expected safe fallback on malformed JSON, got: %+v", res3)
	}
}

func TestMultiModelCollaborationWorkflowWithRevisions(t *testing.T) {
	var callNum int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callNum, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch n {
		case 1:
			resp := `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"continue\",\"message\":\"Plan: 1. Write handler. 2. Add input validation.\",\"completion_summary\":\"\"}"}]}}]}`
			w.Write([]byte(resp))
		case 2:
			resp := `{"choices":[{"message":{"content":"{\"action\":\"continue\",\"message\":\"func Handler() { /* draft 1 without validation */ }\",\"completion_summary\":\"Initial handler draft\"}"}}]}`
			w.Write([]byte(resp))
		case 3:
			resp := `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"revise\",\"message\":\"Please add missing input validation to Handler().\",\"completion_summary\":\"\"}"}]}}]}`
			w.Write([]byte(resp))
		case 4:
			resp := `{"choices":[{"message":{"content":"{\"action\":\"continue\",\"message\":\"func Handler(in string) error { if in == \\\"\\\" { return ErrInvalid }; return nil }\",\"completion_summary\":\"Added input validation\"}"}}]}`
			w.Write([]byte(resp))
		case 5:
			resp := `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"complete\",\"message\":\"Validation verified. Deliverable approved.\",\"completion_summary\":\"Handler implemented with full input validation.\"}"}]}}]}`
			w.Write([]byte(resp))
		default:
			t.Errorf("unexpected extra model call %d", n)
		}
	}))
	defer ts.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	storage := NewStorage(db)
	if err := storage.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("gemini", ts.URL)
	_ = router.SetProviderBaseURL("openrouter", ts.URL)

	keyResolver := func(p string) string {
		if p == "gemini" {
			return "test-gemini-key"
		}
		if p == "openrouter" {
			return "test-openrouter-key"
		}
		return ""
	}

	engine := NewCollaborationEngine(storage, router, keyResolver, nil, DefaultCollaborationConfig())
	pool := NewWorkerPool(storage, engine, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	participants := []Participant{
		{Role: RoleCoordinator, Name: "Architect", Provider: "gemini", Model: "gemini-2.0-flash"},
		{Role: RoleWorker, Name: "Developer", Provider: "openrouter", Model: "deepseek/deepseek-r1:free"},
	}
	metaBytes, _ := json.Marshal(JobMetadata{Participants: participants})

	job, err := storage.CreateJob(ctx, "Implement a validated Go handler", 5, 12, string(metaBytes))
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	_ = storage.SaveParticipants(ctx, job.ID, participants)

	pool.Start(ctx)
	defer pool.Stop()

	deadline := time.Now().Add(3 * time.Second)
	var finalJob *Job
	for time.Now().Before(deadline) {
		finalJob, _ = storage.GetJob(ctx, job.ID)
		if finalJob != nil && (finalJob.Status == StatusCompleted || finalJob.Status == StatusFailed) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if finalJob == nil || finalJob.Status != StatusCompleted {
		t.Fatalf("expected job to complete, got: %+v", finalJob)
	}
	if finalJob.CurrentStage != StageCompleted {
		t.Errorf("expected stage 'completed', got '%s'", finalJob.CurrentStage)
	}
	if finalJob.CurrentRound != 2 || finalJob.TotalCalls != 5 {
		t.Errorf("expected 2 rounds and 5 total calls, got round=%d calls=%d", finalJob.CurrentRound, finalJob.TotalCalls)
	}
	if !strings.Contains(finalJob.ExecutionResult, "Handler implemented with full input validation.") {
		t.Errorf("expected final result to include completion summary, got: %s", finalJob.ExecutionResult)
	}
}

func TestCollaborationMidRoundRecoveryWithoutDuplicateCalls(t *testing.T) {
	// Simulate a server crash after Coordinator Plan (turn 1) and Worker Round 1 (turn 2) are already saved in SQLite.
	// When the worker recovers the job, it should ONLY invoke the Coordinator Review (turn 3), not repeat turns 1 & 2.
	var apiCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"complete\",\"message\":\"Recovered review complete.\",\"completion_summary\":\"Recovered and approved.\"}"}}]}`))
	}))
	defer ts.Close()

	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	defer db.Close()

	storage := NewStorage(db)
	_ = storage.InitSchema()

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", ts.URL)

	engine := NewCollaborationEngine(storage, router, func(string) string { return "key" }, nil, DefaultCollaborationConfig())

	ctx := context.Background()
	parts := []Participant{
		{Role: RoleCoordinator, Name: "Coord", Provider: "openrouter", Model: "model-a"},
		{Role: RoleWorker, Name: "Worker", Provider: "openrouter", Model: "model-b"},
	}
	job, _ := storage.CreateJob(ctx, "Resumable task", 3, 10, "{}")
	_ = storage.SaveParticipants(ctx, job.ID, parts)

	// Pre-populate Turn 1 (Coordinator Plan) and Turn 2 (Worker Round 1) as if saved before a crash
	_, _ = storage.SaveCollaborationMessage(ctx, CollaborationMessage{
		JobID:      job.ID,
		Round:      1,
		TurnIndex:  1,
		SenderRole: RoleCoordinator,
		Provider:   "openrouter",
		Model:      "model-a",
		Action:     ActionContinue,
		Content:    "Persisted plan from before crash",
	})
	_, _ = storage.SaveCollaborationMessage(ctx, CollaborationMessage{
		JobID:      job.ID,
		Round:      1,
		TurnIndex:  2,
		SenderRole: RoleWorker,
		Provider:   "openrouter",
		Model:      "model-b",
		Action:     ActionContinue,
		Content:    "Persisted worker output from before crash",
	})

	helper := &executionHelperImpl{workerID: "w-recover", jobID: job.ID, storage: storage, ctx: ctx}
	res, err := engine.ProcessJob(ctx, job, helper)
	if err != nil {
		t.Fatalf("ProcessJob recovery failed: %v", err)
	}

	if atomic.LoadInt32(&apiCalls) != 1 {
		t.Errorf("expected exactly 1 model call (Coordinator review) on recovery, got %d", atomic.LoadInt32(&apiCalls))
	}
	if !strings.Contains(res, "Persisted worker output from before crash") || !strings.Contains(res, "Recovered and approved.") {
		t.Errorf("unexpected recovered output: %s", res)
	}
}

func TestCollaborationMaxRoundsEnforcement(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"revise\",\"message\":\"Still working on iteration\",\"completion_summary\":\"\"}"}}]}`))
	}))
	defer ts.Close()

	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	defer db.Close()

	storage := NewStorage(db)
	_ = storage.InitSchema()

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", ts.URL)

	engine := NewCollaborationEngine(storage, router, func(string) string { return "key" }, nil, DefaultCollaborationConfig())

	ctx := context.Background()
	parts := []Participant{
		{Role: RoleCoordinator, Name: "Coord", Provider: "openrouter", Model: "model-a"},
		{Role: RoleWorker, Name: "Worker", Provider: "openrouter", Model: "model-b"},
	}

	job, _ := storage.CreateJob(ctx, "Iterative task", 2, 10, "{}")
	_ = storage.SaveParticipants(ctx, job.ID, parts)

	helper := &executionHelperImpl{workerID: "w1", jobID: job.ID, storage: storage, ctx: ctx}
	result, err := engine.ProcessJob(ctx, job, helper)
	if err != nil {
		t.Fatalf("ProcessJob failed: %v", err)
	}
	if result == "" {
		t.Errorf("expected non-empty result when max rounds reached")
	}
	if atomic.LoadInt32(&calls) != 5 {
		t.Errorf("expected exactly 5 model calls for 2 rounds, got %d", atomic.LoadInt32(&calls))
	}
}

func TestOrchestrationWithDynamicProviderAndCrossAdapterFailover(t *testing.T) {
	// Primary Worker uses native Gemini adapter (configured to return HTTP 503).
	// Backup Worker uses a dynamically registered 5th OpenAI-compatible provider ("custom-groq")
	// and must take over automatically via failover, then Coordinator approves.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "gemini-failing:generateContent"):
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"Gemini overloaded"}}`))

		case strings.Contains(r.URL.Path, "gemini-coord:generateContent"):
			// Handles both initial planning and final review
			w.Header().Set("Content-Type", "application/json")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			rawBytes, _ := json.Marshal(body)
			if strings.Contains(string(rawBytes), "Latest Worker Result") {
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"complete\",\"message\":\"Approved dynamic worker output.\",\"completion_summary\":\"Cross-adapter failover succeeded.\"}"}]}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"continue\",\"message\":\"Build the service.\",\"completion_summary\":\"\"}"}]}}]}`))
			}

		case r.URL.Path == "/dynamic-v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"SUMMARY: Built via dynamic OpenAI-compat provider\n=== FILE: app.go ===\npackage main\n"}}]}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		t.Fatalf("create settings: %v", err)
	}

	storage := NewStorage(db)
	if err := storage.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	router := provider.NewRouter()
	if err := router.AttachDB(context.Background(), db); err != nil {
		t.Fatalf("AttachDB: %v", err)
	}
	_ = router.SetProviderBaseURL("gemini", ts.URL)

	// Register a 5th dynamic OpenAI-compatible provider purely via Store configuration
	_, err = router.Store().SaveDynamicProvider(context.Background(), router.Registry(), router.Client, provider.DynamicProviderInput{
		ID:           "custom-groq",
		Name:         "Custom Groq",
		BaseURL:      ts.URL + "/dynamic-v1",
		AuthType:     "bearer",
		APIKey:       "gsk-dynamic-test",
		ManualModels: []string{"llama-3.3-70b"},
	})
	if err != nil {
		t.Fatalf("SaveDynamicProvider: %v", err)
	}

	keyResolver := func(p string) string {
		if p == "gemini" {
			return "test-gemini-key"
		}
		if _, cfg, err := router.Registry().Get(p); err == nil {
			return cfg.APIKey
		}
		return ""
	}

	engine := NewCollaborationEngine(storage, router, keyResolver, nil, DefaultCollaborationConfig())
	engine.SetProviderConfiguredChecker(func(p string) bool {
		return router.Registry().IsConfigured(p) || keyResolver(p) != ""
	})

	ctx := context.Background()
	participants := []Participant{
		{Role: RoleCoordinator, Name: "Architect", Provider: "gemini", Model: "gemini-coord"},
		{Role: RoleWorker, Name: "Primary Gemini Worker", Provider: "gemini", Model: "gemini-failing"},
		{Role: RoleWorker, Name: "Backup Dynamic Worker", Provider: "custom-groq", Model: "llama-3.3-70b"},
	}
	metaBytes, _ := json.Marshal(JobMetadata{Participants: participants})
	job, err := storage.CreateJob(ctx, "Test cross-adapter failover", 3, 10, string(metaBytes))
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	_ = storage.SaveParticipants(ctx, job.ID, participants)

	helper := &executionHelperImpl{workerID: "w-failover", jobID: job.ID, storage: storage, ctx: ctx}
	result, err := engine.ProcessJob(ctx, job, helper)
	if err != nil {
		t.Fatalf("ProcessJob failed: %v", err)
	}
	if !strings.Contains(result, "Cross-adapter failover succeeded.") || !strings.Contains(result, "=== FILE: app.go ===") {
		t.Fatalf("unexpected final deliverable after cross-adapter failover: %s", result)
	}
}
