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
	// 1. Valid JSON
	rawValid := `{"action":"complete","message":"All tests pass and code is clean.","completion_summary":"Implemented feature X."}`
	res1 := ParseStructuredResponse(rawValid, ActionContinue)
	if res1.Action != ActionComplete || res1.Message != "All tests pass and code is clean." || res1.CompletionSummary != "Implemented feature X." {
		t.Fatalf("unexpected valid parse result: %+v", res1)
	}

	// 2. Markdown-wrapped JSON with unsupported action falls back to defaultAction
	rawUnsupported := "```json\n{\"action\":\"invalid_unknown_action\",\"message\":\"Drafting architecture\"}\n```"
	res2 := ParseStructuredResponse(rawUnsupported, ActionContinue)
	if res2.Action != ActionContinue || res2.Message != "Drafting architecture" {
		t.Fatalf("expected fallback to ActionContinue on unsupported action, got: %+v", res2)
	}

	// 3. Malformed JSON / Plaintext does not crash
	rawMalformed := "Here is my response without valid JSON { broken: 123"
	res3 := ParseStructuredResponse(rawMalformed, ActionRevise)
	if res3.Action != ActionRevise || res3.Message != rawMalformed {
		t.Fatalf("expected safe fallback on malformed JSON, got: %+v", res3)
	}
}

func TestMultiModelCollaborationWorkflowWithRevisions(t *testing.T) {
	var callNum int32

	// Mock server simulating two distinct providers/models:
	// Call 1: Coordinator (Gemini) creates plan -> action: continue
	// Call 2: Worker (OpenRouter) produces initial draft -> action: continue
	// Call 3: Coordinator (Gemini) reviews and requests revision -> action: revise
	// Call 4: Worker (OpenRouter) revises work -> action: continue
	// Call 5: Coordinator (Gemini) reviews and accepts -> action: complete
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callNum, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch n {
		case 1: // Coordinator Plan (Gemini format)
			resp := `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"continue\",\"message\":\"Plan: 1. Write handler. 2. Add input validation.\",\"completion_summary\":\"\"}"}]}}]}`
			w.Write([]byte(resp))
		case 2: // Worker Round 1 (OpenAI format)
			resp := `{"choices":[{"message":{"content":"{\"action\":\"continue\",\"message\":\"func Handler() { /* draft 1 without validation */ }\",\"completion_summary\":\"Initial handler draft\"}"}}]}`
			w.Write([]byte(resp))
		case 3: // Coordinator Review Round 1 (Gemini format -> requests revision)
			resp := `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"revise\",\"message\":\"Please add missing input validation to Handler().\",\"completion_summary\":\"\"}"}]}}]}`
			w.Write([]byte(resp))
		case 4: // Worker Round 2 Revision (OpenAI format)
			resp := `{"choices":[{"message":{"content":"{\"action\":\"continue\",\"message\":\"func Handler(in string) error { if in == \\\"\\\" { return ErrInvalid }; return nil }\",\"completion_summary\":\"Added input validation\"}"}}]}`
			w.Write([]byte(resp))
		case 5: // Coordinator Review Round 2 (Gemini format -> completes task)
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
	router.GeminiBaseURL = ts.URL
	router.OpenRouterBaseURL = ts.URL

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
	if finalJob.CurrentRound != 2 || finalJob.TotalCalls != 5 {
		t.Errorf("expected 2 rounds and 5 total calls, got round=%d calls=%d", finalJob.CurrentRound, finalJob.TotalCalls)
	}
	if !strings.Contains(finalJob.ExecutionResult, "Handler implemented with full input validation.") {
		t.Errorf("expected final result to include completion summary, got: %s", finalJob.ExecutionResult)
	}

	msgs, err := storage.GetCollaborationMessages(ctx, job.ID)
	if err != nil {
		t.Fatalf("get collaboration messages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 persisted collaboration messages, got %d", len(msgs))
	}
	if msgs[2].Action != ActionRevise || msgs[4].Action != ActionComplete {
		t.Errorf("unexpected action sequence: msg[2]=%s, msg[4]=%s", msgs[2].Action, msgs[4].Action)
	}
}

func TestCollaborationMaxRoundsEnforcement(t *testing.T) {
	var calls int32
	// Models always return "revise" / "continue" and never mark complete
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
	router.OpenRouterBaseURL = ts.URL

	engine := NewCollaborationEngine(storage, router, func(string) string { return "key" }, nil, DefaultCollaborationConfig())

	ctx := context.Background()
	parts := []Participant{
		{Role: RoleCoordinator, Name: "Coord", Provider: "openrouter", Model: "model-a"},
		{Role: RoleWorker, Name: "Worker", Provider: "openrouter", Model: "model-b"},
	}

	// Max 2 rounds allowed -> 1 plan + (2 rounds * 2 calls) = 5 calls max
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
