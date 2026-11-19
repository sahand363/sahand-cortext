package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func (a *App) handleProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, 200, map[string]any{
			"providers":       a.router.Registry().List(),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
	case http.MethodPost:
		var input provider.DynamicProviderInput
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&input) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid provider configuration payload"})
			return
		}
		if a.router.Store() == nil {
			jsonOut(w, 500, map[string]string{"error": "Provider store is not initialized"})
			return
		}
		created, err := a.router.Store().SaveDynamicProvider(r.Context(), a.router.Registry(), a.router.Client, input)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}

		models, listErr := a.fetchProviderModels(r.Context(), created.ID, "")
		if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == created.ID) {
			a.setActiveSelection(created.ID, models[0].ID)
		}

		jsonOut(w, 201, map[string]any{
			"ok":              true,
			"provider":        created,
			"providers":       a.router.Registry().List(),
			"models":          models,
			"discovery_error": errString(listErr),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	id := provider.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/providers/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Provider ID is required"})
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.router.Store() == nil {
		jsonOut(w, 500, map[string]string{"error": "Provider store is not initialized"})
		return
	}
	if err := a.router.Store().DeleteDynamicProvider(r.Context(), a.router.Registry(), id); err != nil {
		status := 400
		if errors.Is(err, provider.ErrUnknownProvider) {
			status = 404
		}
		jsonOut(w, status, map[string]string{"error": err.Error()})
		return
	}

	if provider.CanonicalID(a.setting("active_provider")) == id {
		_ = a.setSetting("active_provider", "")
		_ = a.setSetting("active_model", "")
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"deleted":         id,
		"providers":       a.router.Registry().List(),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) saveProviderKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid request payload"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	p, cfg, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}

	requiresKey := p.Metadata().RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none")
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = a.providerKey(norm)
	}
	if requiresKey && len(key) < 8 {
		jsonOut(w, 400, map[string]string{"error": "Please enter a valid API key for " + p.Metadata().Name})
		return
	}

	start := time.Now()
	models, err := a.fetchProviderModels(r.Context(), norm, key)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		jsonOut(w, 400, map[string]any{"error": err.Error()})
		return
	}

	if key != "" && a.router.Store() != nil {
		if err := a.router.Store().SaveAPIKey(r.Context(), a.router.Registry(), norm, key); err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed to store API key in database"})
			return
		}
	}

	if len(models) > 0 && (a.setting("active_model") == "" || a.activeProvider() == norm) {
		a.setActiveSelection(norm, models[0].ID)
	}

	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"latency_ms":      latency,
		"models":          models,
		"providers":       a.router.Registry().List(),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := provider.CanonicalID(r.URL.Query().Get("provider"))
	if q == "all" {
		all := []ModelInfo{}
		errorsMap := map[string]string{}
		for _, prov := range a.router.Registry().List() {
			if !prov.Configured {
				continue
			}
			ms, err := a.fetchProviderModels(r.Context(), prov.ID, "")
			if err != nil {
				errorsMap[prov.ID] = err.Error()
				continue
			}
			all = append(all, ms...)
		}
		jsonOut(w, 200, map[string]any{
			"models":          all,
			"errors":          errorsMap,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	if _, _, err := a.router.Registry().Get(q); err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        q,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	if !a.isProviderConfigured(q) {
		jsonOut(w, 200, map[string]any{
			"provider":        q,
			"models":          []ModelInfo{},
			"configured":      false,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	models, err := a.fetchProviderModels(r.Context(), q, "")
	if err != nil {
		jsonOut(w, 400, map[string]any{
			"provider":        q,
			"error":           err.Error(),
			"models":          []ModelInfo{},
			"configured":      true,
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}
	jsonOut(w, 200, map[string]any{
		"provider":        q,
		"models":          models,
		"configured":      true,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func (a *App) handleToggleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string   `json:"provider"`
		Model    string   `json:"model"`
		Models   []string `json:"models"`
		Enabled  bool     `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&body) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid request payload"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	if _, _, err := a.router.Registry().Get(norm); err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	targets := body.Models
	if strings.TrimSpace(body.Model) != "" {
		targets = append(targets, strings.TrimSpace(body.Model))
	}
	if len(targets) == 0 {
		jsonOut(w, 400, map[string]string{"error": "Model ID or models list is required"})
		return
	}

	a.setModelsEnabledBatch(norm, targets, body.Enabled)
	jsonOut(w, 200, map[string]any{
		"ok":       true,
		"provider": norm,
		"count":    len(targets),
		"enabled":  body.Enabled,
	})
}

func (a *App) handleAddCustomModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		jsonOut(w, 400, map[string]string{"error": "Model ID is required"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	p, _, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	modelID := p.NormalizeModelID(body.Model)

	if !a.isProviderConfigured(norm) {
		jsonOut(w, 400, map[string]string{"error": "Configure your " + p.Metadata().Name + " connection first"})
		return
	}

	if a.router.Store() != nil {
		_ = a.router.Store().SaveManualModel(r.Context(), a.router.Registry(), norm, modelID)
	}

	testPrompt := []provider.Message{
		{Role: "user", Content: "Reply with the single word OK."},
	}
	reply, latency, callErr := a.router.Call(r.Context(), norm, a.providerKey(norm), modelID, testPrompt, provider.CallOpts{MaxTokens: 512, SkipUsage: true})
	if callErr != nil {
		a.saveTestRecord(norm, modelID, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: callErr.Error(),
		})
	} else {
		a.saveTestRecord(norm, modelID, ModelTestRecord{
			Status:    "working",
			LatencyMs: latency,
		})
		a.setModelEnabled(norm, modelID, true)
		a.setActiveSelection(norm, modelID)
	}

	models, _ := a.fetchProviderModels(r.Context(), norm, "")
	jsonOut(w, 200, map[string]any{
		"ok":              callErr == nil,
		"provider":        norm,
		"model":           modelID,
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"error":           errString(callErr),
		"models":          models,
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *App) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		SetActive bool   `json:"set_active"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		jsonOut(w, 400, map[string]string{"error": "Model ID is required"})
		return
	}
	norm := provider.CanonicalID(body.Provider)
	p, _, err := a.router.Registry().Get(norm)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	model := p.NormalizeModelID(body.Model)

	if !a.isProviderConfigured(norm) {
		jsonOut(w, 400, map[string]string{"error": "Configure your " + p.Metadata().Name + " connection first"})
		return
	}

	testPrompt := []provider.Message{
		{Role: "user", Content: "Reply with the single word OK."},
	}
	reply, latency, callErr := a.router.Call(r.Context(), norm, a.providerKey(norm), model, testPrompt, provider.CallOpts{MaxTokens: 512, SkipUsage: true})
	if callErr != nil {
		a.saveTestRecord(norm, model, ModelTestRecord{
			Status:    "failed",
			LatencyMs: latency,
			LastError: callErr.Error(),
		})
		jsonOut(w, 200, map[string]any{
			"ok":              false,
			"provider":        norm,
			"model":           model,
			"status":          "failed",
			"latency_ms":      latency,
			"error":           callErr.Error(),
			"active_provider": a.activeProvider(),
			"active_model":    a.activeModel(),
		})
		return
	}

	a.saveTestRecord(norm, model, ModelTestRecord{
		Status:    "working",
		LatencyMs: latency,
		LastError: "",
	})
	if body.SetActive {
		a.setActiveSelection(norm, model)
	}

	rec := a.loadTestRecords(norm)[model]
	jsonOut(w, 200, map[string]any{
		"ok":              true,
		"provider":        norm,
		"model":           model,
		"status":          "working",
		"enabled":         !rec.Disabled,
		"latency_ms":      latency,
		"reply":           strings.TrimSpace(reply),
		"active_provider": a.activeProvider(),
		"active_model":    a.activeModel(),
	})
}

// GET /api/usage/summary?days=N — aggregated usage analytics for the
// Usage & analytics page. All aggregates are computed in SQL, never by
// loading every row into memory. Each query is fully drained and its
// rows closed before the next query runs (single-connection rule).
// No cost/money is returned: the hub has no structured per-model
// pricing to compute it from — only tokens, requests and latency.
func (a *App) usageSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	days := 30
	if q := strings.TrimSpace(r.URL.Query().Get("days")); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}
	cutoff := time.Now().Unix() - int64(days)*86400

	type usageTotals struct {
		Requests         int64   `json:"requests"`
		Successful       int64   `json:"successful"`
		Failed           int64   `json:"failed"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		TotalTokens      int64   `json:"total_tokens"`
		AvgLatencyMs     float64 `json:"avg_latency_ms"`
	}
	type usageByModel struct {
		Provider    string  `json:"provider"`
		Model       string  `json:"model"`
		Requests    int64   `json:"requests"`
		TotalTokens int64   `json:"total_tokens"`
		AvgLatency  float64 `json:"avg_latency_ms"`
	}
	type usageByDay struct {
		Day         string `json:"day"`
		Requests    int64  `json:"requests"`
		TotalTokens int64  `json:"total_tokens"`
	}

	var totals usageTotals
	err := a.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN success=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0), COALESCE(AVG(latency_ms),0)
		FROM usage_events WHERE created_at >= ?`, cutoff).Scan(
		&totals.Requests, &totals.Successful, &totals.Failed,
		&totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens,
		&totals.AvgLatencyMs)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	byModel := []usageByModel{}
	rows, err := a.db.Query(`SELECT provider, model, COUNT(*),
		COALESCE(SUM(total_tokens),0), COALESCE(AVG(latency_ms),0)
		FROM usage_events WHERE created_at >= ?
		GROUP BY provider, model ORDER BY COUNT(*) DESC`, cutoff)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	for rows.Next() {
		var m usageByModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.Requests, &m.TotalTokens, &m.AvgLatency); err != nil {
			rows.Close()
			jsonOut(w, 500, map[string]string{"error": "database error"})
			return
		}
		byModel = append(byModel, m)
	}
	rows.Close()

	byDay := []usageByDay{}
	rows, err = a.db.Query(`SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') AS day,
		COUNT(*), COALESCE(SUM(total_tokens),0)
		FROM usage_events WHERE created_at >= ?
		GROUP BY day ORDER BY day ASC`, cutoff)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}
	for rows.Next() {
		var d usageByDay
		if err := rows.Scan(&d.Day, &d.Requests, &d.TotalTokens); err != nil {
			rows.Close()
			jsonOut(w, 500, map[string]string{"error": "database error"})
			return
		}
		byDay = append(byDay, d)
	}
	rows.Close()

	jsonOut(w, 200, map[string]any{
		"totals":     totals,
		"by_model":   byModel,
		"by_day":     byDay,
		"range_days": days,
	})
}

func (a *App) fetchProviderModels(ctx context.Context, p, keyOverride string) ([]ModelInfo, error) {
	norm := provider.CanonicalID(p)
	discovered, err := a.router.ListModels(ctx, norm, keyOverride)
	if err != nil {
		return nil, err
	}

	records := a.loadTestRecords(norm)
	out := make([]ModelInfo, 0, len(discovered))
	for _, m := range discovered {
		rec, exists := records[m.ID]
		status := "untested"
		var lat int64
		var lastErr string
		enabled := true
		if exists {
			status = rec.Status
			lat = rec.LatencyMs
			lastErr = rec.LastError
			enabled = !rec.Disabled
		}
		out = append(out, ModelInfo{
			Provider:    norm,
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Description: m.Description,
			ContextLen:  m.ContextLen,
			IsFree:      m.IsFree,
			PriceInfo:   m.PriceInfo,
			SortPrice:   m.SortPrice,
			Status:      status,
			LatencyMs:   lat,
			LastError:   lastErr,
			Enabled:     enabled,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return statusRank(out[i].Status) < statusRank(out[j].Status)
	})
	return out, nil
}

func statusRank(s string) int {
	switch s {
	case "working":
		return 0
	case "untested":
		return 1
	default:
		return 2
	}
}
