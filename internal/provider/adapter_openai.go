package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// OpenAICompatAdapter implements the Provider contract for any OpenAI-compatible HTTP service
// (OpenRouter, AIPrime, CodeCraft, Groq, Together, Mistral, DeepSeek, Ollama, vLLM, LM Studio, etc.).
type OpenAICompatAdapter struct {
	meta   ProviderMetadata
	client *http.Client
}

func NewOpenAICompatAdapter(meta ProviderMetadata, client *http.Client) *OpenAICompatAdapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAICompatAdapter{meta: meta, client: client}
}

func (o *OpenAICompatAdapter) ID() string                 { return o.meta.ID }
func (o *OpenAICompatAdapter) Metadata() ProviderMetadata { return o.meta }
func (o *OpenAICompatAdapter) Capabilities() Capabilities { return o.meta.Capabilities }
func (o *OpenAICompatAdapter) NormalizeModelID(id string) string {
	return strings.TrimSpace(id)
}

func (o *OpenAICompatAdapter) Chat(ctx context.Context, cfg ProviderConfig, req ChatRequest) (ChatResponse, error) {
	if !cfg.Enabled {
		return ChatResponse{}, fmt.Errorf("%w: %s", ErrProviderDisabled, o.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if o.meta.RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && key == "" {
		return ChatResponse{}, fmt.Errorf("missing API key for %s", o.meta.Name)
	}
	if len(req.Messages) == 0 {
		return ChatResponse{}, errors.New("empty message history")
	}

	model := o.NormalizeModelID(req.Model)
	if model == "" {
		model = o.meta.DefaultModel
	}
	if model == "" && len(cfg.ManualModels) > 0 {
		model = cfg.ManualModels[0]
	}

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

	formatted := make([]oaMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "system" && role != "user" && role != "assistant" {
			role = "user"
		}
		formatted = append(formatted, oaMsg{
			Role:    role,
			Content: m.Content,
		})
	}
	if len(formatted) == 0 {
		return ChatResponse{}, errors.New("empty message history")
	}

	body := oaPayload{
		Model:    model,
		Messages: formatted,
		Stream:   req.Stream,
	}
	if req.MaxTokens > 0 && !cfg.OmitMaxTokens {
		body.MaxTokens = req.MaxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}

	endpoint := joinEndpoint(cfg.BaseURL, cfg.ChatPath, "/chat/completions")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream, application/json")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("User-Agent", "Sahand-CorText")
	applyProviderAuthAndHeaders(httpReq, cfg)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return ChatResponse{}, fmt.Errorf("%s HTTP %d: %s", o.meta.Name, resp.StatusCode, ExtractAPIError(rawErr))
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if req.Stream && strings.Contains(contentType, "text/event-stream") {
		text, err := parseOpenAISSEStream(resp.Body, o.meta.Name, model)
		return ChatResponse{Content: text}, err
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	trimmedRaw := bytes.TrimSpace(raw)
	if len(trimmedRaw) == 0 {
		return ChatResponse{}, fmt.Errorf("empty response body from %s model %s", o.meta.Name, model)
	}

	if bytes.HasPrefix(trimmedRaw, []byte("data:")) {
		text, err := parseOpenAISSEStream(bytes.NewReader(trimmedRaw), o.meta.Name, model)
		return ChatResponse{Content: text}, err
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
		return ChatResponse{}, fmt.Errorf("invalid JSON response from %s: %w", o.meta.Name, err)
	}
	if parsed.Error != nil {
		return ChatResponse{}, fmt.Errorf("%s error: %s", o.meta.Name, ExtractAPIError(trimmedRaw))
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("no choices returned by %s", o.meta.Name)
	}

	msg := parsed.Choices[0].Message
	reasoning := strings.TrimSpace(msg.ReasoningContent)
	if reasoning == "" {
		reasoning = strings.TrimSpace(msg.Reasoning)
	}
	text, err := validateOrSalvageReasoning(msg.Content, reasoning, o.meta.Name, model)
	return ChatResponse{Content: text}, err
}

func (o *OpenAICompatAdapter) ListModels(ctx context.Context, cfg ProviderConfig) ([]DiscoveredModel, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrProviderDisabled, o.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if o.meta.RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && key == "" {
		return nil, fmt.Errorf("missing API key for %s", o.meta.Name)
	}

	seen := map[string]bool{}
	out := make([]DiscoveredModel, 0, len(cfg.Featured)+len(cfg.ManualModels)+16)

	addModel := func(m DiscoveredModel) {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		m.Provider = o.meta.ID
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		out = append(out, m)
	}

	for _, f := range cfg.Featured {
		addModel(f)
	}

	defaultPlan := "API Key Plan"
	if strings.EqualFold(cfg.AuthType, "none") {
		defaultPlan = "Local / Open Endpoint"
	} else if cfg.PricingFormat == "wallet" {
		defaultPlan = o.meta.Name + " Wallet Plan"
	}

	for _, manualID := range cfg.ManualModels {
		addModel(DiscoveredModel{
			ID:          manualID,
			DisplayName: manualID,
			Description: "Custom " + o.meta.Name + " model",
			IsFree:      strings.EqualFold(cfg.AuthType, "none"),
			PriceInfo:   defaultPlan,
		})
	}

	if !o.meta.Capabilities.ModelDiscovery || strings.TrimSpace(cfg.ModelsPath) == "" {
		return out, nil
	}

	endpoint := joinEndpoint(cfg.BaseURL, cfg.ModelsPath, "/models")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "Sahand-CorText")
	applyProviderAuthAndHeaders(httpReq, cfg)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		if cfg.AllowDiscoveryFallback && len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("network error reaching %s: %v", o.meta.Name, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%s rejected key (HTTP %d): %s", o.meta.Name, resp.StatusCode, ExtractAPIError(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if cfg.AllowDiscoveryFallback && len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("%s HTTP %d: %s", o.meta.Name, resp.StatusCode, ExtractAPIError(raw))
	}

	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			Type          string `json:"type"`
			ContextLength int    `json:"context_length"`
			ContextWindow int    `json:"context_window"`
			Pricing       struct {
				Prompt      string  `json:"prompt"`
				Completion  string  `json:"completion"`
				InputPer1K  float64 `json:"input_per_1k"`
				OutputPer1K float64 `json:"output_per_1k"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if cfg.AllowDiscoveryFallback && len(out) > 0 {
			return out, nil
		}
		return nil, err
	}

	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		if cfg.PricingFormat != "openrouter" {
			if strings.EqualFold(m.Type, "embedding") || strings.Contains(strings.ToLower(m.ID), "embed") {
				continue
			}
		}

		ctxLen := m.ContextLength
		if ctxLen == 0 {
			ctxLen = m.ContextWindow
		}

		isFree := strings.EqualFold(cfg.AuthType, "none")
		priceInfo := defaultPlan
		var sortPrice float64

		switch cfg.PricingFormat {
		case "openrouter":
			pPrompt, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
			pComp, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
			isFree = strings.HasSuffix(m.ID, ":free") || (m.Pricing.Prompt != "" && pPrompt == 0 && pComp == 0)
			sortPrice = pPrompt
			if isFree {
				priceInfo = "FREE ($0 / 1M tokens)"
			} else if pPrompt > 0 {
				priceInfo = fmt.Sprintf("$%.2f / 1M in", pPrompt*1000000)
			} else {
				priceInfo = "Paid model"
			}
		case "codecraft":
			inPer1M := m.Pricing.InputPer1K * 1000
			isFree = true
			sortPrice = inPer1M
			if inPer1M > 0 {
				priceInfo = fmt.Sprintf("$%.2f / 1M tokens", inPer1M)
			} else {
				priceInfo = "Free Starter Credits"
			}
		}

		addModel(DiscoveredModel{
			ID:          m.ID,
			DisplayName: m.Name,
			Description: m.Description,
			ContextLen:  ctxLen,
			IsFree:      isFree,
			PriceInfo:   priceInfo,
			SortPrice:   sortPrice,
		})
	}

	featuredIDs := map[string]bool{}
	for _, f := range cfg.Featured {
		featuredIDs[f.ID] = true
	}

	sort.SliceStable(out, func(i, j int) bool {
		if featuredIDs[out[i].ID] != featuredIDs[out[j].ID] {
			return featuredIDs[out[i].ID]
		}
		if cfg.PricingFormat == "openrouter" && out[i].IsFree != out[j].IsFree {
			return out[i].IsFree
		}
		if cfg.PricingFormat == "codecraft" && out[i].SortPrice != out[j].SortPrice {
			return out[i].SortPrice < out[j].SortPrice
		}
		return out[i].ID < out[j].ID
	})

	return out, nil
}

func (o *OpenAICompatAdapter) TestConnection(ctx context.Context, cfg ProviderConfig) error {
	if o.meta.Capabilities.ModelDiscovery && strings.TrimSpace(cfg.ModelsPath) != "" {
		_, err := o.ListModels(ctx, cfg)
		return err
	}
	testModel := o.meta.DefaultModel
	if testModel == "" && len(cfg.ManualModels) > 0 {
		testModel = cfg.ManualModels[0]
	}
	if testModel == "" {
		return nil
	}
	_, err := o.Chat(ctx, cfg, ChatRequest{
		Model:        testModel,
		Messages:     []Message{{Role: "user", Content: "Reply with the single word OK."}},
		MaxTokens:    64,
		DisableRetry: true,
	})
	return err
}

func joinEndpoint(baseURL, path, defaultPath string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	p := strings.TrimSpace(path)
	if p == "" {
		p = defaultPath
	}
	if p == "" {
		return b
	}
	return b + "/" + strings.TrimLeft(p, "/")
}

func applyProviderAuthAndHeaders(req *http.Request, cfg ProviderConfig) {
	key := strings.TrimSpace(cfg.APIKey)
	switch strings.ToLower(strings.TrimSpace(cfg.AuthType)) {
	case "none":
		// No authentication header
	case "header":
		if key != "" && strings.TrimSpace(cfg.AuthHeader) != "" {
			req.Header.Set(strings.TrimSpace(cfg.AuthHeader), key)
		}
	default: // "bearer"
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	for k, v := range cfg.ExtraHeaders {
		if strings.TrimSpace(k) != "" && v != "" {
			req.Header.Set(k, v)
		}
	}
}

// validateOrSalvageReasoning ensures unfinished chain-of-thought scratchpads are never passed off as deliverables.
func validateOrSalvageReasoning(content, reasoning, label, model string) (string, error) {
	c := strings.TrimSpace(content)
	if c != "" {
		return c, nil
	}
	r := strings.TrimSpace(reasoning)
	if r == "" {
		return "", fmt.Errorf("empty content returned by %s model %s", label, model)
	}
	if strings.Contains(r, "=== FILE:") ||
		strings.Contains(r, "```") ||
		(strings.Contains(r, `"action"`) && strings.Contains(r, `"message"`)) ||
		len(r) < 600 {
		return r, nil
	}
	return "", fmt.Errorf("%s model %s exhausted output tokens during internal reasoning without writing final deliverable", label, model)
}

func parseOpenAISSEStream(r io.Reader, label, model string) (string, error) {
	scanner := bufio.NewScanner(r)
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
			return "", fmt.Errorf("%s error: %s", label, ExtractAPIError([]byte(payload)))
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

	contentStr := strings.TrimSpace(contentSB.String())
	reasoningStr := strings.TrimSpace(reasoningSB.String())

	if err := scanner.Err(); err != nil {
		if len(contentStr) > 200 {
			return contentStr, nil
		}
		return "", fmt.Errorf("%s stream error: %w", label, err)
	}

	if contentStr == "" && reasoningStr == "" && rawFallbackSB.Len() > 0 {
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
				return "", fmt.Errorf("%s error: %s", label, ExtractAPIError(rawBytes))
			}
			if len(single.Choices) > 0 {
				m := single.Choices[0].Message
				contentStr = strings.TrimSpace(m.Content)
				reasoningStr = strings.TrimSpace(m.ReasoningContent)
				if reasoningStr == "" {
					reasoningStr = strings.TrimSpace(m.Reasoning)
				}
			}
		}
	}

	return validateOrSalvageReasoning(contentStr, reasoningStr, label, model)
}
