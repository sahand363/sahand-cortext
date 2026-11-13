package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// Message represents a normalized conversational turn
type Message struct {
	Role    string `json:"role"` // "system", "user", "assistant"
	Content string `json:"content"`
}

// CallOpts holds optional parameters for model invocations
type CallOpts struct {
	MaxTokens    int
	DisableRetry bool
}

// Router handles model calls across Google Gemini, OpenRouter, CodeCraft API, and AIPrime
type Router struct {
	Client            *http.Client
	GeminiBaseURL     string
	OpenRouterBaseURL string
	CodeCraftBaseURL  string
	AIPrimeBaseURL    string
}

func NewRouter() *Router {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	// Force IPv4 (tcp4) + HTTP/1.1 (disabling HTTP/2 connection multiplexing)
	// so a slow/stalled upstream request on Cloudflare never poisons subsequent requests.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		DisableKeepAlives:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 600 * time.Second, // 10 minutes for heavy reasoning & multi-file code generation
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Router{
		Client:            &http.Client{Transport: transport, Timeout: 620 * time.Second},
		GeminiBaseURL:     "https://generativelanguage.googleapis.com/v1beta/models",
		OpenRouterBaseURL: "https://openrouter.ai/api/v1/chat/completions",
		CodeCraftBaseURL:  "https://codecraftapi.com/v1/chat/completions",
		AIPrimeBaseURL:    "https://api.aiprime.shop/v1/chat/completions",
	}
}

func NormalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "openrouter":
		return "openrouter"
	case "codecraft":
		return "codecraft"
	case "aiprime":
		return "aiprime"
	default:
		return "gemini"
	}
}

func ProviderLabel(p string) string {
	switch NormalizeProvider(p) {
	case "openrouter":
		return "OpenRouter"
	case "codecraft":
		return "CodeCraft API"
	case "aiprime":
		return "AIPrime"
	default:
		return "Google Gemini"
	}
}

func classifyProviderError(err error, elapsedMs int64) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	sec := float64(elapsedMs) / 1000.0
	msg := strings.ToLower(err.Error())

	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return fmt.Errorf("TIMEOUT after %.1fs (model took too long to finish generating)", sec)
	}
	if strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "unexpected eof") {
		return fmt.Errorf("CONNECTION RESET after %.1fs (upstream proxy closed connection): %v", sec, err)
	}
	if strings.Contains(msg, "http 429") {
		return fmt.Errorf("RATE LIMIT (HTTP 429 — provider throttled requests): %v", err)
	}
	if strings.Contains(msg, "http 401") || strings.Contains(msg, "http 402") || strings.Contains(msg, "http 403") {
		return fmt.Errorf("AUTH / WALLET / QUOTA ERROR: %v", err)
	}
	if strings.Contains(msg, "http 500") || strings.Contains(msg, "http 502") || strings.Contains(msg, "http 503") || strings.Contains(msg, "http 504") {
		return fmt.Errorf("PROVIDER OVERLOADED (upstream server error): %v", err)
	}
	if strings.Contains(msg, "empty content") || strings.Contains(msg, "empty response") {
		return fmt.Errorf("EMPTY RESPONSE (model returned 0 tokens): %v", err)
	}
	return err
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "http 500") ||
		strings.Contains(msg, "http 502") ||
		strings.Contains(msg, "http 503") ||
		strings.Contains(msg, "http 504") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "unexpected end of json") ||
		strings.Contains(msg, "invalid json response") ||
		strings.Contains(msg, "empty content") ||
		strings.Contains(msg, "empty response") ||
		strings.Contains(msg, "eof")
}

// Call invokes the appropriate provider with structured history and exponential backoff
func (r *Router) Call(ctx context.Context, provider, key, model string, messages []Message, opts CallOpts) (string, int64, error) {
	provider = NormalizeProvider(provider)
	if strings.TrimSpace(key) == "" {
		return "", 0, fmt.Errorf("missing API key for %s", ProviderLabel(provider))
	}
	if len(messages) == 0 {
		return "", 0, errors.New("empty message history")
	}

	maxRetries := 1
	if opts.DisableRetry {
		maxRetries = 0
	}
	var lastErr error
	var totalLatency int64

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * 500 * time.Millisecond
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", 0, ctx.Err()
			}
		}

		start := time.Now()
		var reply string
		var err error

		switch provider {
		case "openrouter":
			reply, err = r.callOpenAI(ctx, r.OpenRouterBaseURL, key, model, messages, opts, "OpenRouter", true, false)
		case "codecraft":
			reply, err = r.callOpenAI(ctx, r.CodeCraftBaseURL, key, model, messages, opts, "CodeCraft", false, false)
		case "aiprime":
			baseURL := r.AIPrimeBaseURL
			if baseURL == "" {
				baseURL = "https://api.aiprime.shop/v1/chat/completions"
			}
			// Do not send max_tokens cap to AIPrime, and enable SSE streaming over HTTP/1.1
			// so Cloudflare never drops long code-generation turns at 100 seconds.
			aiPrimeOpts := opts
			aiPrimeOpts.MaxTokens = 0
			reply, err = r.callOpenAI(ctx, baseURL, key, model, messages, aiPrimeOpts, "AIPrime", false, true)
		default:
			reply, err = r.callGemini(ctx, key, model, messages, opts)
		}

		elapsed := time.Since(start).Milliseconds()
		totalLatency += elapsed

		if err == nil {
			return reply, elapsed, nil
		}
		lastErr = classifyProviderError(err, elapsed)
		if !isRetryable(err) || ctx.Err() != nil {
			return "", elapsed, lastErr
		}
	}

	return "", totalLatency, lastErr
}

// callGemini formats structured conversation history for Google Gemini
func (r *Router) callGemini(ctx context.Context, key, model string, messages []Message, opts CallOpts) (string, error) {
	cleanModel := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if cleanModel == "" {
		cleanModel = "gemini-2.0-flash"
	}

	baseURL := r.GeminiBaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta/models"
	}
	endpoint := fmt.Sprintf("%s/%s:generateContent", baseURL, cleanModel)

	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}
	type sysInstruction struct {
		Parts []part `json:"parts"`
	}
	type genConfig struct {
		MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	}

	var contents []content
	var sysParts []part

	for _, m := range messages {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if m.Role == "system" {
			sysParts = append(sysParts, part{Text: c})
			continue
		}

		gemRole := "user"
		if m.Role == "assistant" || m.Role == "model" {
			gemRole = "model"
		}

		if len(contents) > 0 && contents[len(contents)-1].Role == gemRole {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, part{Text: c})
		} else {
			contents = append(contents, content{
				Role:  gemRole,
				Parts: []part{{Text: c}},
			})
		}
	}

	if len(contents) == 0 {
		contents = append(contents, content{
			Role:  "user",
			Parts: []part{{Text: "Hello"}},
		})
	} else if contents[0].Role != "user" {
		contents = append([]content{{
			Role:  "user",
			Parts: []part{{Text: "Hello"}},
		}}, contents...)
	}

	payload := map[string]any{
		"contents": contents,
	}
	if len(sysParts) > 0 {
		payload["system_instruction"] = sysInstruction{Parts: sysParts}
	}
	if opts.MaxTokens > 0 {
		payload["generationConfig"] = genConfig{MaxOutputTokens: opts.MaxTokens}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, extractAPIError(raw))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("malformed Gemini response: %w", err)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("empty response from Gemini model")
	}

	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return "", errors.New("empty text response from Gemini model")
	}
	return out, nil
}

// callOpenAI formats standard messages array for OpenAI-compatible gateways (supporting both standard JSON and HTTP/1.1 SSE streaming)
func (r *Router) callOpenAI(ctx context.Context, endpoint, key, model string, messages []Message, opts CallOpts, label string, openRouterHeaders bool, stream bool) (string, error) {
	type oaMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type oaPayload struct {
		Model     string  `json:"model"`
		Messages  []oaMsg `json:"messages"`
		MaxTokens int     `json:"max_tokens,omitempty"`
		Stream    bool    `json:"stream,omitempty"`
	}

	formatted := make([]oaMsg, 0, len(messages))
	for _, m := range messages {
		role := strings.ToLower(m.Role)
		if role != "system" && role != "user" && role != "assistant" {
			role = "user"
		}
		formatted = append(formatted, oaMsg{
			Role:    role,
			Content: m.Content,
		})
	}

	body := oaPayload{
		Model:    model,
		Messages: formatted,
		Stream:   stream,
	}
	if opts.MaxTokens > 0 {
		body.MaxTokens = opts.MaxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream, application/json")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Connection", "close")
	if openRouterHeaders {
		req.Header.Set("HTTP-Referer", "https://github.com/sahand363/sahand-cortext")
		req.Header.Set("X-Title", "Sahand-CorText")
		req.Header.Set("User-Agent", "Sahand-CorText/0.5.4")
	} else {
		req.Header.Set("User-Agent", "curl/8.5.0")
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("%s HTTP %d: %s", label, resp.StatusCode, extractAPIError(rawErr))
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if stream && strings.Contains(contentType, "text/event-stream") {
		return parseOpenAISSEStream(resp.Body, label, model)
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	trimmedRaw := bytes.TrimSpace(raw)
	if len(trimmedRaw) == 0 {
		return "", fmt.Errorf("empty response body from %s model %s", label, model)
	}

	// If server returned SSE stream lines without text/event-stream Content-Type header
	if bytes.HasPrefix(trimmedRaw, []byte("data:")) {
		return parseOpenAISSEStream(bytes.NewReader(trimmedRaw), label, model)
	}

	var parsed struct {
		Error   any `json:"error"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(trimmedRaw, &parsed); err != nil {
		return "", fmt.Errorf("invalid JSON response from %s: %w", label, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("%s error: %s", label, extractAPIError(trimmedRaw))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices returned by %s", label)
	}

	msg := parsed.Choices[0].Message
	out := strings.TrimSpace(msg.Content)
	if out == "" {
		out = strings.TrimSpace(msg.ReasoningContent)
	}
	if out == "" {
		out = strings.TrimSpace(msg.Reasoning)
	}
	if out == "" {
		return "", fmt.Errorf("empty content returned by %s model %s", label, model)
	}
	return out, nil
}

func parseOpenAISSEStream(r io.Reader, label, model string) (string, error) {
	scanner := bufio.NewScanner(r)
	// Allow up to 8 MB per SSE line for large code blocks
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var contentSB strings.Builder
	var reasoningSB strings.Builder
	var rawFallbackSB strings.Builder

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			rawFallbackSB.WriteString(line)
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk struct {
			Error   any `json:"error"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				Message struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return "", fmt.Errorf("%s error: %s", label, extractAPIError([]byte(payload)))
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				contentSB.WriteString(ch.Delta.Content)
			} else if ch.Message.Content != "" {
				contentSB.WriteString(ch.Message.Content)
			}
			if ch.Delta.ReasoningContent != "" {
				reasoningSB.WriteString(ch.Delta.ReasoningContent)
			} else if ch.Delta.Reasoning != "" {
				reasoningSB.WriteString(ch.Delta.Reasoning)
			} else if ch.Message.ReasoningContent != "" {
				reasoningSB.WriteString(ch.Message.ReasoningContent)
			}
		}
	}

	out := strings.TrimSpace(contentSB.String())
	if out == "" {
		out = strings.TrimSpace(reasoningSB.String())
	}

	// If stream broke mid-way AFTER we already received substantial deliverable content, salvage it
	if err := scanner.Err(); err != nil {
		if len(out) > 200 {
			return out, nil
		}
		return "", fmt.Errorf("%s stream error: %w", label, err)
	}

	// Fallback if gateway returned a single non-SSE JSON object despite text/event-stream header
	if out == "" && rawFallbackSB.Len() > 0 {
		rawBytes := []byte(rawFallbackSB.String())
		var single struct {
			Error   any `json:"error"`
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(rawBytes, &single) == nil {
			if single.Error != nil {
				return "", fmt.Errorf("%s error: %s", label, extractAPIError(rawBytes))
			}
			if len(single.Choices) > 0 {
				m := single.Choices[0].Message
				out = strings.TrimSpace(m.Content)
				if out == "" {
					out = strings.TrimSpace(m.ReasoningContent)
				}
				if out == "" {
					out = strings.TrimSpace(m.Reasoning)
				}
			}
		}
	}

	if out == "" {
		return "", fmt.Errorf("empty content returned by %s model %s", label, model)
	}
	return out, nil
}

func extractAPIError(raw []byte) string {
	var eWrap struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &eWrap) == nil {
		switch v := eWrap.Error.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if msg, ok := v["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if eWrap.Message != "" {
			return eWrap.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}
