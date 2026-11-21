package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTavilySearchAndChainOrder(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.24 released","url":"https://go.dev/blog/go1.24","content":"Go 1.24 is out.","published_date":"2025-02-11"}]}`))
	}))
	defer srv.Close()
	old := tavilyURL
	tavilyURL = srv.URL
	defer func() { tavilyURL = old }()

	key, used := "", 0
	cfg := TavilyConfig{Key: func() string { return key }, OnUse: func() { used++ }}
	fallbackCalls := 0
	fallback := func(ctx context.Context, q string, max int) ([]Result, error) {
		fallbackCalls++
		return []Result{{Title: "ddg", URL: "https://example.com"}}, nil
	}
	search := Chain(NamedSearch{"tavily", Tavily(cfg)}, NamedSearch{"duckduckgo", fallback})

	// No key: Tavily is skipped, the free source answers.
	res, err := search(context.Background(), "go 1.24", 5)
	if err != nil || res[0].Title != "ddg" || used != 0 {
		t.Fatalf("no key: %v %v used=%d", res, err, used)
	}

	// With a key: Tavily answers first.
	key = "tvly-test123456"
	res, err = search(context.Background(), "go 1.24", 5)
	if err != nil || len(res) != 1 || res[0].URL != "https://go.dev/blog/go1.24" {
		t.Fatalf("tavily: %v %v", res, err)
	}
	if gotAuth != "Bearer tvly-test123456" || gotBody["search_depth"] != "basic" || used != 1 {
		t.Fatalf("request: auth=%q body=%v used=%d", gotAuth, gotBody, used)
	}
	if !strings.Contains(res[0].Snippet, "2025-02-11") {
		t.Fatalf("snippet lacks date: %q", res[0].Snippet)
	}

	// Credits used up: falls back and Tavily is paused.
	status = 432
	calls := fallbackCalls
	if res, err = search(context.Background(), "x", 5); err != nil || res[0].Title != "ddg" {
		t.Fatalf("432: %v %v", res, err)
	}
	status = http.StatusOK
	if res, _ = search(context.Background(), "y", 5); res[0].Title != "ddg" || fallbackCalls != calls+2 {
		t.Fatalf("tavily should be paused after 432: %v", res)
	}
}

func TestTavilyBadKeyAndNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	old := tavilyURL
	tavilyURL = srv.URL
	defer func() { tavilyURL = old }()
	if _, err := Tavily(TavilyConfig{Key: func() string { return "bad" }})(context.Background(), "q", 1); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
	if _, err := Tavily(TavilyConfig{})(context.Background(), "q", 1); !errors.Is(err, ErrNoKey) {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
}
