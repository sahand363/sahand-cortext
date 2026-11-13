package provider

import (
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

// GeminiAdapter implements the native Google Gemini generateContent & models API.
type GeminiAdapter struct {
	meta   ProviderMetadata
	client *http.Client
}

func NewGeminiAdapter(meta ProviderMetadata, client *http.Client) *GeminiAdapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &GeminiAdapter{meta: meta, client: client}
}

func (g *GeminiAdapter) ID() string                 { return g.meta.ID }
func (g *GeminiAdapter) Metadata() ProviderMetadata { return g.meta }
func (g *GeminiAdapter) Capabilities() Capabilities { return g.meta.Capabilities }

func (g *GeminiAdapter) NormalizeModelID(modelID string) string {
	return strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
}

func (g *GeminiAdapter) Chat(ctx context.Context, cfg ProviderConfig, req ChatRequest) (ChatResponse, error) {
	if !cfg.Enabled {
		return ChatResponse{}, fmt.Errorf("%w: %s", ErrProviderDisabled, g.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return ChatResponse{}, fmt.Errorf("missing API key for %s", g.meta.Name)
	}
	if len(req.Messages) == 0 {
		return ChatResponse{}, errors.New("empty message history")
	}

	cleanModel := g.NormalizeModelID(req.Model)
	if cleanModel == "" {
		cleanModel = g.meta.DefaultModel
	}
	if cleanModel == "" {
		cleanModel = "gemini-2.0-flash"
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
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

	for _, m := range req.Messages {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if strings.EqualFold(m.Role, "system") {
			sysParts = append(sysParts, part{Text: c})
			continue
		}

		gemRole := "user"
		if strings.EqualFold(m.Role, "assistant") || strings.EqualFold(m.Role, "model") {
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
	if req.MaxTokens > 0 {
		payload["generationConfig"] = genConfig{MaxOutputTokens: req.MaxTokens}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", key)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, ExtractAPIError(raw))
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
		return ChatResponse{}, fmt.Errorf("malformed Gemini response: %w", err)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return ChatResponse{}, errors.New("empty response from Gemini model")
	}

	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		return ChatResponse{}, errors.New("empty text response from Gemini model")
	}
	return ChatResponse{Content: out}, nil
}

func (g *GeminiAdapter) ListModels(ctx context.Context, cfg ProviderConfig) ([]DiscoveredModel, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrProviderDisabled, g.meta.Name)
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("missing API key for %s", g.meta.Name)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	endpoint := baseURL + "?pageSize=100"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-goog-api-key", key)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("network error reaching %s: %v", g.meta.Name, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Google Gemini API HTTP %d: %s", resp.StatusCode, ExtractAPIError(raw))
	}

	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	out := make([]DiscoveredModel, 0, len(parsed.Models))

	for _, customID := range cfg.ManualModels {
		id := g.NormalizeModelID(customID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, DiscoveredModel{
			Provider:    g.meta.ID,
			ID:          id,
			DisplayName: id,
			Description: "Custom Gemini model",
			IsFree:      true,
			PriceInfo:   "Google AI Studio Plan",
		})
	}

	for _, m := range parsed.Models {
		supportsGen := false
		for _, method := range m.SupportedGenerationMethods {
			if method == "generateContent" {
				supportsGen = true
				break
			}
		}
		if !supportsGen {
			continue
		}
		id := g.NormalizeModelID(m.Name)
		if (!strings.HasPrefix(id, "gemini") && !strings.HasPrefix(id, "gemma") && !strings.HasPrefix(id, "learnlm")) || seen[id] {
			continue
		}
		seen[id] = true
		disp := m.DisplayName
		if disp == "" {
			disp = id
		}
		out = append(out, DiscoveredModel{
			Provider:    g.meta.ID,
			ID:          id,
			DisplayName: disp,
			Description: m.Description,
			ContextLen:  m.InputTokenLimit,
			IsFree:      true,
			PriceInfo:   "Google AI Studio Plan",
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (g *GeminiAdapter) TestConnection(ctx context.Context, cfg ProviderConfig) error {
	_, err := g.ListModels(ctx, cfg)
	return err
}
