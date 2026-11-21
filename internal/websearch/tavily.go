package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tavily (v0.10.3): a search API made for AI tools. With the owner's key
// (Settings › Web search) it is the first source in the chain, because it
// does not bot-check server IPs the way DuckDuckGo does. The free plan
// gives about 1,000 searches a month; each "basic" search costs 1 credit.
// Without a key it is skipped silently and the free chain answers.

// ErrNoKey means the source needs an API key that is not set.
var ErrNoKey = errors.New("no API key set")

// TavilySettingKey is the hub setting holding the encrypted Tavily key.
const TavilySettingKey = "tavily_api_key"

// tavilyURL is a variable so tests can point it at a fake server.
var tavilyURL = "https://api.tavily.com/search"

// TavilyConfig wires Tavily to the hub: Key returns the current key (read
// on every search, so a new key works at once), OnUse counts a used credit.
type TavilyConfig struct {
	Key   func() string
	OnUse func()
}

// Tavily returns a SearchFunc for the Tavily search API.
func Tavily(cfg TavilyConfig) SearchFunc {
	return func(ctx context.Context, query string, max int) ([]Result, error) {
		key := ""
		if cfg.Key != nil {
			key = strings.TrimSpace(cfg.Key())
		}
		if key == "" {
			return nil, ErrNoKey
		}
		if max <= 0 || max > 20 {
			max = 6
		}
		body, _ := json.Marshal(map[string]any{
			"query":          query,
			"search_depth":   "basic", // 1 credit ("advanced" costs 2)
			"max_results":    max,
			"topic":          "general",
			"include_answer": false,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("unreachable: %w", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusUnauthorized:
			return nil, errors.New("key rejected (HTTP 401): check the Tavily key in Settings › Web search")
		case 432, 433:
			return nil, fmt.Errorf("monthly credits used up (HTTP %d)", resp.StatusCode)
		default:
			return nil, fmt.Errorf("answered HTTP %d", resp.StatusCode)
		}
		if cfg.OnUse != nil {
			cfg.OnUse()
		}
		return parseTavily(b, max)
	}
}

func parseTavily(b []byte, max int) ([]Result, error) {
	var data struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("bad Tavily answer: %w", err)
	}
	var out []Result
	for _, r := range data.Results {
		if r.URL == "" {
			continue
		}
		snip := cleanText(r.Content)
		if r.PublishedDate != "" {
			snip = "(published " + r.PublishedDate + ") " + snip
		}
		out = append(out, Result{Title: cleanText(r.Title), URL: r.URL, Snippet: snip})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// pauseFor says how long the chain skips a source after this error.
func pauseFor(err error) time.Duration {
	m := err.Error()
	switch {
	case strings.Contains(m, "credits used up"):
		return time.Hour
	case isRefusal(err):
		return 10 * time.Minute
	}
	return 0
}
