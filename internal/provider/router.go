package provider

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Router coordinates the central Provider Registry, SQLite config Store, shared HTTP client, and retry execution.
type Router struct {
	Client   *http.Client
	registry *Registry
	store    *Store
}

// NewVPSHTTPClient creates an IPv4-first HTTP/1.1 client tuned for long LLM inference turns.
func NewVPSHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 15 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		DisableKeepAlives:     false,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 600 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: 620 * time.Second}
}

// NewRouter initializes a Registry pre-populated with all built-in providers.
func NewRouter() *Router {
	client := NewVPSHTTPClient()
	reg := NewRegistry()
	_ = RegisterBuiltInProviders(reg, client)
	return &Router{
		Client:   client,
		registry: reg,
	}
}

// AttachDB binds SQLite persistence, creates provider_configs if needed, and loads dynamic providers & keys.
func (r *Router) AttachDB(ctx context.Context, db *sql.DB) error {
	r.store = NewStore(db)
	if err := r.store.InitSchema(); err != nil {
		return err
	}
	if err := r.store.LoadIntoRegistry(ctx, r.registry, r.Client); err != nil {
		return err
	}
	// Re-encrypt any plaintext API keys now that the encryption key (if set)
	// is available. Idempotent; safe to run on every startup.
	if n, err := r.store.MigrateAPIKeys(ctx); err == nil && n > 0 {
		log.Printf("[Provider] Encrypted %d stored API key(s).", n)
	}
	return nil
}

func (r *Router) Registry() *Registry { return r.registry }
func (r *Router) Store() *Store       { return r.store }

func (r *Router) SetProviderBaseURL(providerID, baseURL string) error {
	return r.registry.SetBaseURLOverride(providerID, baseURL)
}

// Call dispatches a chat completion through the registered provider adapter with retry & error classification.
// One usage_events row is recorded per call that reaches a provider (see recordUsage); validation failures
// (unknown/disabled provider, missing key) never reach a provider and are not recorded.
func (r *Router) Call(ctx context.Context, providerID, key, model string, messages []Message, opts CallOpts) (string, int64, error) {
	content, latency, usage, attempted, err := r.doCall(ctx, providerID, key, model, messages, opts)
	if attempted {
		r.recordUsage(ctx, providerID, model, usage, latency, err, opts)
	}
	return content, latency, err
}

// doCall is the work behind Call. It also returns the token usage the
// provider reported (zero-valued with Available=false when it reported
// none) and whether a provider call was actually attempted.
func (r *Router) doCall(ctx context.Context, providerID, key, model string, messages []Message, opts CallOpts) (string, int64, Usage, bool, error) {
	p, cfg, err := r.registry.Get(providerID)
	if err != nil {
		return "", 0, Usage{}, false, err
	}
	if !cfg.Enabled {
		return "", 0, Usage{}, false, fmt.Errorf("%w: %s", ErrProviderDisabled, p.Metadata().Name)
	}
	if strings.TrimSpace(key) != "" {
		cfg.APIKey = strings.TrimSpace(key)
	}
	if p.Metadata().RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && strings.TrimSpace(cfg.APIKey) == "" {
		return "", 0, Usage{}, false, fmt.Errorf("missing API key for %s", p.Metadata().Name)
	}

	var lastErr error
	var totalLatency int64

	for attempt := 0; ; attempt++ {
		start := time.Now()
		stream := opts.Stream || (cfg.StreamOnRetry && attempt > 0)
		resp, callErr := p.Chat(ctx, cfg, ChatRequest{
			Model:        model,
			Messages:     messages,
			MaxTokens:    opts.MaxTokens,
			Stream:       stream,
			DisableRetry: opts.DisableRetry,
		})
		elapsed := time.Since(start).Milliseconds()
		totalLatency += elapsed

		if callErr == nil {
			ClearCooldown(providerID, model)
			return resp.Content, elapsed, resp.Usage, true, nil
		}
		lastErr = ClassifyProviderError(callErr, elapsed)
		if ctx.Err() != nil {
			return "", elapsed, Usage{}, true, lastErr
		}
		wait, again := retryWait(attempt, callErr, opts)
		if !again {
			MarkCooldown(providerID, model, CooldownFor(callErr), lastErr.Error())
			return "", totalLatency, Usage{}, true, lastErr
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return "", totalLatency, Usage{}, true, ctx.Err()
		}
	}
}

// retryWait decides whether a failed call is tried again on the same model,
// and after how long. A model with a backup (FailFastOnRateLimit) is not
// waited on for a 429: the caller parks it and moves to the backup.
func retryWait(attempt int, err error, opts CallOpts) (time.Duration, bool) {
	if opts.DisableRetry || !IsRetryable(err) {
		return 0, false
	}
	switch {
	case IsRateLimit(err):
		if opts.FailFastOnRateLimit || attempt >= 1 {
			return 0, false
		}
		wait := 5 * time.Second
		if he, ok := AsHTTPError(err); ok {
			if he.DailyQuota {
				return 0, false
			}
			if he.RetryAfter > 0 {
				wait = he.RetryAfter
			}
		}
		if wait > 20*time.Second {
			return 0, false // too long to hold a reply open; park it instead
		}
		return Jitter(wait), true
	case IsOverloaded(err):
		if attempt >= 2 {
			return 0, false
		}
		if attempt == 0 {
			return Jitter(2 * time.Second), true
		}
		return Jitter(6 * time.Second), true
	default:
		if attempt >= 1 {
			return 0, false
		}
		return Jitter(time.Second), true
	}
}

// recordUsage writes one usage_events row for a provider call. It is
// best-effort and never fails the chat: no store, no table, or an
// insert error only logs. Session/persona are not known at this layer
// and are recorded empty (per-provider/model/day totals are the
// must-have; a per-session breakdown is not attempted here).
func (r *Router) recordUsage(ctx context.Context, providerID, model string, usage Usage, latencyMs int64, err error, opts CallOpts) {
	if opts.SkipUsage || r.store == nil {
		return
	}
	ev := UsageEvent{
		Provider:  providerID,
		Model:     model,
		LatencyMs: latencyMs,
		Success:   err == nil,
	}
	if err == nil {
		ev.PromptTokens = usage.PromptTokens
		ev.CompletionTokens = usage.CompletionTokens
		ev.TotalTokens = usage.TotalTokens
	} else {
		ev.Error = err.Error()
		if len(ev.Error) > 500 {
			ev.Error = ev.Error[:500]
		}
	}
	if recErr := r.store.RecordUsageEvent(ctx, ev); recErr != nil {
		log.Printf("[Usage] record failed: %v", recErr)
	}
}

// CallStream is Call, but it yields the reply in pieces: onChunk is
// invoked with each piece of the reply text, in order, as the provider
// produces it, and the full reply is still returned when the stream
// finishes. It uses the provider's ChatStream when it implements the
// Streamer interface (OpenAI- and Anthropic-compatible adapters);
// otherwise it falls back to the buffered Call and delivers the whole
// reply as a single chunk (Gemini has no streaming form here, so a
// Gemini reply arrives in one piece at the end — the call still works).
// Retry and error classification match Call.
func (r *Router) CallStream(ctx context.Context, providerID, key, model string, messages []Message, opts CallOpts, onChunk func(string)) (string, int64, error) {
	content, latency, usage, attempted, err := r.doCallStream(ctx, providerID, key, model, messages, opts, onChunk)
	if attempted {
		r.recordUsage(ctx, providerID, model, usage, latency, err, opts)
	}
	return content, latency, err
}

// doCallStream is the work behind CallStream (see its doc comment).
func (r *Router) doCallStream(ctx context.Context, providerID, key, model string, messages []Message, opts CallOpts, onChunk func(string)) (string, int64, Usage, bool, error) {
	p, cfg, err := r.registry.Get(providerID)
	if err != nil {
		return "", 0, Usage{}, false, err
	}
	if !cfg.Enabled {
		return "", 0, Usage{}, false, fmt.Errorf("%w: %s", ErrProviderDisabled, p.Metadata().Name)
	}
	if strings.TrimSpace(key) != "" {
		cfg.APIKey = strings.TrimSpace(key)
	}
	if p.Metadata().RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && strings.TrimSpace(cfg.APIKey) == "" {
		return "", 0, Usage{}, false, fmt.Errorf("missing API key for %s", p.Metadata().Name)
	}

	streamer, canStream := p.(Streamer)
	if !canStream {
		// No incremental streaming for this provider: run the buffered
		// call and deliver the whole reply as one chunk. Uses doCall
		// (not Call) so the reply is recorded exactly once, here in
		// CallStream.
		content, latency, usage, attempted, err := r.doCall(ctx, providerID, key, model, messages, opts)
		if err == nil && onChunk != nil && content != "" {
			onChunk(content)
		}
		return content, latency, usage, attempted, err
	}

	var lastErr error
	var totalLatency int64

	// Once any chunk has reached the caller, a retry would stream the
	// reply again from the start and the client would show it twice.
	emitted := false
	trackedChunk := onChunk
	if onChunk != nil {
		trackedChunk = func(s string) {
			if s != "" {
				emitted = true
			}
			onChunk(s)
		}
	}

	for attempt := 0; ; attempt++ {
		start := time.Now()
		resp, callErr := streamer.ChatStream(ctx, cfg, ChatRequest{
			Model:        model,
			Messages:     messages,
			MaxTokens:    opts.MaxTokens,
			Stream:       true,
			DisableRetry: opts.DisableRetry,
		}, trackedChunk)
		elapsed := time.Since(start).Milliseconds()
		totalLatency += elapsed

		if callErr == nil {
			ClearCooldown(providerID, model)
			return resp.Content, elapsed, resp.Usage, true, nil
		}
		lastErr = ClassifyProviderError(callErr, elapsed)
		if ctx.Err() != nil || emitted {
			return "", elapsed, Usage{}, true, lastErr
		}
		wait, again := retryWait(attempt, callErr, opts)
		if !again {
			MarkCooldown(providerID, model, CooldownFor(callErr), lastErr.Error())
			return "", totalLatency, Usage{}, true, lastErr
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return "", totalLatency, Usage{}, true, ctx.Err()
		}
	}
}

func (r *Router) ListModels(ctx context.Context, providerID, keyOverride string) ([]DiscoveredModel, error) {
	p, cfg, err := r.registry.Get(providerID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(keyOverride) != "" {
		cfg.APIKey = strings.TrimSpace(keyOverride)
	}
	return p.ListModels(ctx, cfg)
}

func (r *Router) TestConnection(ctx context.Context, providerID, keyOverride string) error {
	p, cfg, err := r.registry.Get(providerID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(keyOverride) != "" {
		cfg.APIKey = strings.TrimSpace(keyOverride)
	}
	return p.TestConnection(ctx, cfg)
}
