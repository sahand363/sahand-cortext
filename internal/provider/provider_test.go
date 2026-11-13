package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 1, 2, 3, 11: Registry registration, lookup, duplicate rejection, unknown provider handling, and capabilities
func TestRegistryRegistrationLookupDuplicatesAndUnknown(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterBuiltInProviders(reg, http.DefaultClient); err != nil {
		t.Fatalf("RegisterBuiltInProviders failed: %v", err)
	}

	// Verify all 4 built-in providers are registered and discoverable
	ids := reg.IDs()
	expected := []string{"gemini", "openrouter", "aiprime", "codecraft"}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d built-in providers, got %v", len(expected), ids)
	}
	for i, want := range expected {
		if ids[i] != want {
			t.Errorf("provider order[%d] = %q, want %q", i, ids[i], want)
		}
	}

	// Duplicate registration must be rejected
	dupAdapter := NewOpenAICompatAdapter(ProviderMetadata{
		ID:          "openrouter",
		Name:        "Duplicate OpenRouter",
		AdapterType: "openai_compat",
	}, http.DefaultClient)
	err := reg.Register(dupAdapter, ProviderConfig{
		ID:      "openrouter",
		Enabled: true,
		BaseURL: "https://example.com/v1",
	})
	if !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("expected ErrDuplicateProvider, got: %v", err)
	}

	// Built-in providers cannot be removed
	if err := reg.Remove("gemini"); !errors.Is(err, ErrBuiltInProtected) {
		t.Fatalf("expected ErrBuiltInProtected when removing built-in, got: %v", err)
	}

	// Unknown provider must return ErrUnknownProvider (never silently fall back to Gemini)
	if _, _, err := reg.Get("nonexistent-provider"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got: %v", err)
	}

	// Verify capability reporting differs accurately between native Gemini and OpenAI-compatible adapters
	gemProv, _, _ := reg.Get("gemini")
	if gemProv.Capabilities().Streaming {
		t.Errorf("expected Gemini native adapter Streaming capability to be false")
	}
	orProv, _, _ := reg.Get("openrouter")
	if !orProv.Capabilities().Streaming || !orProv.Capabilities().ReasoningOutput {
		t.Errorf("expected OpenRouter capabilities Streaming=true and ReasoningOutput=true, got %+v", orProv.Capabilities())
	}
}

// 7, 8: Native Gemini multi-turn serialization & x-goog-api-key authentication
func TestGeminiMultiTurnSerialization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("expected x-goog-api-key header, got %q", r.Header.Get("x-goog-api-key"))
		}

		var payload struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"system_instruction"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload err: %v", err)
		}

		if payload.SystemInstruction == nil || payload.SystemInstruction.Parts[0].Text != "Act as an expert Go engineer." {
			t.Errorf("system instruction missing or mismatch: %+v", payload.SystemInstruction)
		}
		if len(payload.Contents) != 3 {
			t.Fatalf("expected 3 turns (user, model, user), got %d", len(payload.Contents))
		}
		if payload.Contents[0].Role != "user" || payload.Contents[1].Role != "model" || payload.Contents[2].Role != "user" {
			t.Errorf("roles incorrect in Gemini payload: %+v", payload.Contents)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"Paris is the capital of France."}]}}]}`))
	}))
	defer ts.Close()

	router := NewRouter()
	if err := router.SetProviderBaseURL("gemini", ts.URL); err != nil {
		t.Fatalf("SetProviderBaseURL: %v", err)
	}

	history := []Message{
		{Role: "system", Content: "Act as an expert Go engineer."},
		{Role: "user", Content: "What is the capital of France?"},
		{Role: "assistant", Content: "The capital of France is Paris."},
		{Role: "user", Content: "Can you confirm that in one sentence?"},
	}

	reply, lat, err := router.Call(context.Background(), "gemini", "test-key", "models/gemini-2.0-flash", history, CallOpts{})
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if reply != "Paris is the capital of France." {
		t.Errorf("unexpected reply: %s", reply)
	}
	if lat < 0 {
		t.Errorf("invalid latency: %d", lat)
	}
}

// 5, 7, 8, 9: OpenRouter headers, bearer auth, free-model discovery, and AIPrime retry-streaming behavior
func TestBuiltInOpenAICompatPresetsPreserveBehavior(t *testing.T) {
	var aiPrimeAttempts int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openrouter/models":
			if r.Header.Get("Authorization") != "Bearer sk-or-test" {
				t.Errorf("missing OpenRouter bearer token")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"data": [
					{"id": "openai/gpt-4o", "name": "GPT-4o", "context_length": 128000, "pricing": {"prompt": "0.000005", "completion": "0.000015"}},
					{"id": "deepseek/deepseek-r1:free", "name": "DeepSeek R1 Free", "context_length": 64000, "pricing": {"prompt": "0", "completion": "0"}}
				]
			}`))

		case "/openrouter/chat/completions":
			if r.Header.Get("HTTP-Referer") != "https://github.com/sahand363/sahand-cortext" || r.Header.Get("X-Title") != "Sahand-CorText" {
				t.Errorf("expected OpenRouter referer/title headers, got referer=%q title=%q", r.Header.Get("HTTP-Referer"), r.Header.Get("X-Title"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OpenRouter ok"}}]}`))

		case "/aiprime/chat/completions":
			attempt := atomic.AddInt32(&aiPrimeAttempts, 1)
			var reqBody map[string]any
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			if _, hasMaxTokens := reqBody["max_tokens"]; hasMaxTokens {
				t.Errorf("expected AIPrime preset to omit max_tokens")
			}
			if attempt == 1 {
				// Simulate transient 502 on first attempt -> should retry with stream=true
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"upstream gateway timeout"}`))
				return
			}
			if reqBody["stream"] != true {
				t.Errorf("expected AIPrime second attempt to enable stream=true")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Streamed \"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"AIPrime reply\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	router := NewRouter()
	_ = router.SetProviderBaseURL("openrouter", ts.URL+"/openrouter")
	_ = router.SetProviderBaseURL("aiprime", ts.URL+"/aiprime")

	// OpenRouter discovery should sort free models first and tag IsFree
	models, err := router.ListModels(context.Background(), "openrouter", "sk-or-test")
	if err != nil {
		t.Fatalf("ListModels openrouter: %v", err)
	}
	if len(models) != 2 || models[0].ID != "deepseek/deepseek-r1:free" || !models[0].IsFree {
		t.Fatalf("expected free model sorted first with IsFree=true, got: %+v", models)
	}

	// OpenRouter custom headers check
	reply, _, err := router.Call(context.Background(), "openrouter", "sk-or-test", "openai/gpt-4o", []Message{{Role: "user", Content: "ping"}}, CallOpts{})
	if err != nil || reply != "OpenRouter ok" {
		t.Fatalf("OpenRouter call failed: reply=%q err=%v", reply, err)
	}

	// AIPrime retry + SSE stream fallback check
	aiReply, _, err := router.Call(context.Background(), "aiprime", "sk-ap-test", "glm-5.3-flash", []Message{{Role: "user", Content: "ping"}}, CallOpts{MaxTokens: 256})
	if err != nil || aiReply != "Streamed AIPrime reply" {
		t.Fatalf("AIPrime retry-stream call failed: reply=%q err=%v", aiReply, err)
	}
}

// 4, 5, 6, 8, 9, 10, 12, 13: Dynamic OpenAI-compatible provider config, custom headers, manual models,
// connection testing, SQLite persistence, and application restart survival.
func TestDynamicOpenAICompatProviderPersistenceAndRestart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-custom-api-key") != "dyn-secret-123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid custom header key"}`))
			return
		}
		if r.Header.Get("X-Workspace-Tag") != "cortext-lab" {
			t.Errorf("expected custom extra header X-Workspace-Tag, got %q", r.Header.Get("X-Workspace-Tag"))
		}

		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen-2.5-coder-32b","name":"Qwen 2.5 Coder 32B","context_length":32768}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello from 5th dynamic provider!"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	db := openTestDB(t)

	// Instance 1: Configure a 5th OpenAI-compatible provider purely through configuration
	router1 := NewRouter()
	if err := router1.AttachDB(context.Background(), db); err != nil {
		t.Fatalf("AttachDB 1: %v", err)
	}

	created, err := router1.Store().SaveDynamicProvider(context.Background(), router1.Registry(), router1.Client, DynamicProviderInput{
		ID:           "my-vllm-cluster",
		Name:         "My vLLM Cluster",
		BaseURL:      ts.URL + "/v1",
		AuthType:     "header",
		AuthHeader:   "x-custom-api-key",
		APIKey:       "dyn-secret-123",
		ExtraHeaders: map[string]string{"X-Workspace-Tag": "cortext-lab"},
		ManualModels: []string{"manual-fallback-7b"},
	})
	if err != nil {
		t.Fatalf("SaveDynamicProvider failed: %v", err)
	}
	if created.BuiltIn || !created.Configured {
		t.Fatalf("expected non-builtin configured provider, got: %+v", created)
	}

	// Test connection & model discovery (should include both manual and discovered models)
	if err := router1.TestConnection(context.Background(), "my-vllm-cluster", ""); err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
	models, err := router1.ListModels(context.Background(), "my-vllm-cluster", "")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models (1 manual + 1 discovered), got %+v", models)
	}

	// Instance 2: Simulate full application restart with a fresh Router reading the same SQLite DB
	router2 := NewRouter()
	if err := router2.AttachDB(context.Background(), db); err != nil {
		t.Fatalf("AttachDB 2 (simulated restart): %v", err)
	}

	if !router2.Registry().IsConfigured("my-vllm-cluster") {
		t.Fatalf("expected dynamic provider 'my-vllm-cluster' to survive restart and remain configured")
	}

	reply, _, err := router2.Call(context.Background(), "my-vllm-cluster", "", "qwen-2.5-coder-32b", []Message{
		{Role: "user", Content: "Say hello"},
	}, CallOpts{})
	if err != nil || reply != "Hello from 5th dynamic provider!" {
		t.Fatalf("chat after restart failed: reply=%q err=%v", reply, err)
	}

	// Verify deletion cleans up registry & SQLite
	if err := router2.Store().DeleteDynamicProvider(context.Background(), router2.Registry(), "my-vllm-cluster"); err != nil {
		t.Fatalf("DeleteDynamicProvider failed: %v", err)
	}
	if _, _, err := router2.Registry().Get("my-vllm-cluster"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected deleted provider to return ErrUnknownProvider, got: %v", err)
	}
}

// 6, 10: Keyless local OpenAI-compatible provider without /models discovery (manual models only)
func TestKeylessManualOnlyDynamicProvider(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("expected no Authorization header when auth_type=none")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	db := openTestDB(t)
	router := NewRouter()
	_ = router.AttachDB(context.Background(), db)

	_, err := router.Store().SaveDynamicProvider(context.Background(), router.Registry(), router.Client, DynamicProviderInput{
		ID:               "local-llama",
		Name:             "Local Llama.cpp",
		BaseURL:          ts.URL + "/v1",
		AuthType:         "none",
		DisableDiscovery: true,
		ManualModels:     []string{"llama-3.2-3b-q4"},
	})
	if err != nil {
		t.Fatalf("SaveDynamicProvider: %v", err)
	}

	if err := router.TestConnection(context.Background(), "local-llama", ""); err != nil {
		t.Fatalf("TestConnection on manual-only keyless provider failed: %v", err)
	}

	models, err := router.ListModels(context.Background(), "local-llama", "")
	if err != nil || len(models) != 1 || models[0].ID != "llama-3.2-3b-q4" {
		t.Fatalf("expected single manual model without calling /models, got: %+v, err=%v", models, err)
	}
}

// 3: Unknown provider rejection during Router.Call
func TestUnknownProviderRejectedOnCall(t *testing.T) {
	router := NewRouter()
	_, _, err := router.Call(context.Background(), "mystery-ai", "sk-123", "some-model", []Message{
		{Role: "user", Content: "Hi"},
	}, CallOpts{})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got: %v", err)
	}
}
