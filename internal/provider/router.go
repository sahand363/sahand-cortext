package provider

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"math"
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
	return r.store.LoadIntoRegistry(ctx, r.registry, r.Client)
}

func (r *Router) Registry() *Registry { return r.registry }
func (r *Router) Store() *Store       { return r.store }

func (r *Router) SetProviderBaseURL(providerID, baseURL string) error {
	return r.registry.SetBaseURLOverride(providerID, baseURL)
}

// Call dispatches a chat completion through the registered provider adapter with retry & error classification.
func (r *Router) Call(ctx context.Context, providerID, key, model string, messages []Message, opts CallOpts) (string, int64, error) {
	p, cfg, err := r.registry.Get(providerID)
	if err != nil {
		return "", 0, err
	}
	if !cfg.Enabled {
		return "", 0, fmt.Errorf("%w: %s", ErrProviderDisabled, p.Metadata().Name)
	}
	if strings.TrimSpace(key) != "" {
		cfg.APIKey = strings.TrimSpace(key)
	}
	if p.Metadata().RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && strings.TrimSpace(cfg.APIKey) == "" {
		return "", 0, fmt.Errorf("missing API key for %s", p.Metadata().Name)
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
			return resp.Content, elapsed, nil
		}
		lastErr = ClassifyProviderError(callErr, elapsed)
		if !IsRetryable(callErr) || ctx.Err() != nil {
			return "", elapsed, lastErr
		}
	}

	return "", totalLatency, lastErr
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
