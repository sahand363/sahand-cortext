package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiMultiTurnSerialization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("expected x-goog-api-key header")
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
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"Paris is the capital of France."}]}}]}`))
	}))
	defer ts.Close()

	router := NewRouter()
	router.GeminiBaseURL = ts.URL

	history := []Message{
		{Role: "system", Content: "Act as an expert Go engineer."},
		{Role: "user", Content: "What is the capital of France?"},
		{Role: "assistant", Content: "The capital of France is Paris."},
		{Role: "user", Content: "Can you confirm that in one sentence?"},
	}

	reply, lat, err := router.Call(context.Background(), "gemini", "test-key", "gemini-2.0-flash", history, CallOpts{})
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

func TestOpenAIMultiTurnSerialization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-openrouter-key" {
			t.Errorf("missing Authorization header")
		}

		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}

		if len(payload.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(payload.Messages))
		}
		if payload.Messages[0].Role != "user" || payload.Messages[1].Role != "assistant" || payload.Messages[2].Role != "user" {
			t.Errorf("unexpected message roles: %+v", payload.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"Got the full history!"}}]}`))
	}))
	defer ts.Close()

	router := NewRouter()
	router.OpenRouterBaseURL = ts.URL

	history := []Message{
		{Role: "user", Content: "Question 1"},
		{Role: "assistant", Content: "Answer 1"},
		{Role: "user", Content: "Question 2 with context from 1"},
	}

	reply, _, err := router.Call(context.Background(), "openrouter", "test-openrouter-key", "meta-llama/llama-3.3-70b-instruct:free", history, CallOpts{})
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if reply != "Got the full history!" {
		t.Errorf("unexpected reply: %s", reply)
	}
}
