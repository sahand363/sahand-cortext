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
	"strings"
)

const defaultAnthropicVersion = "2023-06-01"
const defaultAnthropicMaxTokens = 4096

// AnthropicCompatAdapter implements the Provider contract for Anthropic's /v1/messages
// specification and any Anthropic-compatible proxy or gateway.
type AnthropicCompatAdapter struct {
	meta   ProviderMetadata
	client *http.Client
}

func NewAnthropicCompatAdapter(meta ProviderMetadata, client *http.Client) *AnthropicCompatAdapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &AnthropicCompatAdapter{meta: meta, client: client}
}

func (a *AnthropicCompatAdapter) ID() string                 { return a.meta.ID }
func (a *AnthropicCompatAdapter) Metadata() ProviderMetadata { return a.meta }
func (a *AnthropicCompatAdapter) Capabilities() Capabilities { return a.meta.Capabilities }
func (a *AnthropicCompatAdapter) NormalizeModelID(id string) string {
	return strings.TrimSpace(id)
}

func (a *AnthropicCompatAdapter) Chat(ctx context.Context, cfg ProviderConfig, req ChatRequest) (ChatResponse, error) {
	if !cfg.Enabled {
		return ChatResponse{}, fmt.Errorf("%w: %s", ErrProviderDisabled, a.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if a.meta.RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && key == "" {
		return ChatResponse{}, fmt.Errorf("missing API key for %s", a.meta.Name)
	}
	if len(req.Messages) == 0 {
		return ChatResponse{}, errors.New("empty message history")
	}

	model := a.NormalizeModelID(req.Model)
	if model == "" {
		model = a.meta.DefaultModel
	}
	if model == "" && len(cfg.ManualModels) > 0 {
		model = cfg.ManualModels[0]
	}

	type antMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type antPayload struct {
		Model     string   `json:"model"`
		System    string   `json:"system,omitempty"`
		Messages  []antMsg `json:"messages"`
		MaxTokens int      `json:"max_tokens"`
		Stream    bool     `json:"stream,omitempty"`
	}

	var sysParts []string
	var turns []antMsg

	for _, m := range req.Messages {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			sysParts = append(sysParts, c)
			continue
		}
		antRole := "user"
		if role == "assistant" || role == "model" {
			antRole = "assistant"
		}
		if len(turns) > 0 && turns[len(turns)-1].Role == antRole {
			turns[len(turns)-1].Content += "\n\n" + c
		} else {
			turns = append(turns, antMsg{Role: antRole, Content: c})
		}
	}

	if len(turns) == 0 {
		turns = append(turns, antMsg{Role: "user", Content: "Hello"})
	} else if turns[0].Role != "user" {
		turns = append([]antMsg{{Role: "user", Content: "Hello"}}, turns...)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	body := antPayload{
		Model:     model,
		System:    strings.Join(sysParts, "\n\n"),
		Messages:  turns,
		MaxTokens: maxTokens,
		Stream:    req.Stream,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}

	endpoint := joinEndpoint(cfg.BaseURL, cfg.ChatPath, "/messages")
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
	applyAnthropicAuthAndHeaders(httpReq, cfg)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return ChatResponse{}, fmt.Errorf("%s HTTP %d: %s", a.meta.Name, resp.StatusCode, ExtractAPIError(rawErr))
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if req.Stream && strings.Contains(contentType, "text/event-stream") {
		text, err := parseAnthropicSSEStream(resp.Body, a.meta.Name, model)
		return ChatResponse{Content: text}, err
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	trimmedRaw := bytes.TrimSpace(raw)
	if len(trimmedRaw) == 0 {
		return ChatResponse{}, fmt.Errorf("empty response body from %s model %s", a.meta.Name, model)
	}
	if bytes.HasPrefix(trimmedRaw, []byte("event:")) || bytes.HasPrefix(trimmedRaw, []byte("data:")) {
		text, err := parseAnthropicSSEStream(bytes.NewReader(trimmedRaw), a.meta.Name, model)
		return ChatResponse{Content: text}, err
	}

	var parsed struct {
		Type    string `json:"type"`
		Error   any    `json:"error"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(trimmedRaw, &parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("invalid JSON response from %s: %w", a.meta.Name, err)
	}
	if parsed.Error != nil || parsed.Type == "error" {
		return ChatResponse{}, fmt.Errorf("%s error: %s", a.meta.Name, ExtractAPIError(trimmedRaw))
	}

	var textSB, thinkingSB strings.Builder
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			textSB.WriteString(block.Text)
		case "thinking":
			thinkingSB.WriteString(block.Thinking)
		}
	}

	text, err := validateOrSalvageReasoning(textSB.String(), thinkingSB.String(), a.meta.Name, model)
	return ChatResponse{Content: text}, err
}

func (a *AnthropicCompatAdapter) ListModels(ctx context.Context, cfg ProviderConfig) ([]DiscoveredModel, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrProviderDisabled, a.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if a.meta.RequiresAPIKey && !strings.EqualFold(cfg.AuthType, "none") && key == "" {
		return nil, fmt.Errorf("missing API key for %s", a.meta.Name)
	}

	seen := map[string]bool{}
	out := make([]DiscoveredModel, 0, len(cfg.Featured)+len(cfg.ManualModels)+8)

	addModel := func(m DiscoveredModel) {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		m.Provider = a.meta.ID
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		out = append(out, m)
	}

	for _, f := range cfg.Featured {
		addModel(f)
	}
	for _, manualID := range cfg.ManualModels {
		addModel(DiscoveredModel{
			ID:          manualID,
			DisplayName: manualID,
			Description: "Manual " + a.meta.Name + " model",
			PriceInfo:   "Anthropic Spec",
		})
	}

	if !a.meta.Capabilities.ModelDiscovery || strings.TrimSpace(cfg.ModelsPath) == "" {
		return out, nil
	}

	endpoint := joinEndpoint(cfg.BaseURL, cfg.ModelsPath, "/models")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "Sahand-CorText")
	applyAnthropicAuthAndHeaders(httpReq, cfg)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		if cfg.AllowDiscoveryFallback && len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("network error reaching %s: %v", a.meta.Name, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%s rejected key (HTTP %d): %s", a.meta.Name, resp.StatusCode, ExtractAPIError(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if cfg.AllowDiscoveryFallback && len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("%s HTTP %d: %s", a.meta.Name, resp.StatusCode, ExtractAPIError(raw))
	}

	var parsed struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
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
		disp := m.DisplayName
		if disp == "" {
			disp = m.Name
		}
		addModel(DiscoveredModel{
			ID:          m.ID,
			DisplayName: disp,
			Description: "Anthropic-compatible model",
			PriceInfo:   "Anthropic Spec",
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (a *AnthropicCompatAdapter) TestConnection(ctx context.Context, cfg ProviderConfig) error {
	if a.meta.Capabilities.ModelDiscovery && strings.TrimSpace(cfg.ModelsPath) != "" {
		_, err := a.ListModels(ctx, cfg)
		return err
	}
	testModel := a.meta.DefaultModel
	if testModel == "" && len(cfg.ManualModels) > 0 {
		testModel = cfg.ManualModels[0]
	}
	if testModel == "" {
		return nil
	}
	_, err := a.Chat(ctx, cfg, ChatRequest{
		Model:        testModel,
		Messages:     []Message{{Role: "user", Content: "Reply with the single word OK."}},
		MaxTokens:    64,
		DisableRetry: true,
	})
	return err
}

func applyAnthropicAuthAndHeaders(req *http.Request, cfg ProviderConfig) {
	key := strings.TrimSpace(cfg.APIKey)
	switch strings.ToLower(strings.TrimSpace(cfg.AuthType)) {
	case "none":
	case "bearer":
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	default: // "header" (defaulting to x-api-key for Anthropic spec)
		headerName := strings.TrimSpace(cfg.AuthHeader)
		if headerName == "" {
			headerName = "x-api-key"
		}
		if key != "" {
			req.Header.Set(headerName, key)
		}
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", defaultAnthropicVersion)
	}
	for k, v := range cfg.ExtraHeaders {
		if strings.TrimSpace(k) != "" && v != "" {
			req.Header.Set(k, v)
		}
	}
}

func parseAnthropicSSEStream(r io.Reader, label, model string) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var contentSB, thinkingSB strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var ev struct {
			Type  string `json:"type"`
			Error any    `json:"error"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev.Error != nil || ev.Type == "error" {
			return "", fmt.Errorf("%s error: %s", label, ExtractAPIError([]byte(payload)))
		}
		if ev.Type == "content_block_delta" {
			if ev.Delta.Text != "" {
				contentSB.WriteString(ev.Delta.Text)
			}
			if ev.Delta.Thinking != "" {
				thinkingSB.WriteString(ev.Delta.Thinking)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if len(contentSB.String()) > 200 {
			return strings.TrimSpace(contentSB.String()), nil
		}
		return "", fmt.Errorf("%s stream error: %w", label, err)
	}
	return validateOrSalvageReasoning(contentSB.String(), thinkingSB.String(), label, model)
}
