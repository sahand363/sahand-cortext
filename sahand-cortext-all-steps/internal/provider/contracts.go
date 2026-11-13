package provider

import "context"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type CallOpts struct {
	MaxTokens    int
	Stream       bool
	DisableRetry bool
}

type ChatRequest struct {
	Model        string
	Messages     []Message
	MaxTokens    int
	Stream       bool
	DisableRetry bool
}

type ChatResponse struct {
	Content   string
	LatencyMs int64
}

type Capabilities struct {
	Chat            bool `json:"chat"`
	Streaming       bool `json:"streaming"`
	ModelDiscovery  bool `json:"model_discovery"`
	SystemPrompt    bool `json:"system_prompt"`
	ReasoningOutput bool `json:"reasoning_output"`
	JSONMode        bool `json:"json_mode"`
}

type ProviderMetadata struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	ShortName         string       `json:"short_name"`
	Icon              string       `json:"icon"`
	Description       string       `json:"description"`
	KeyLabel          string       `json:"key_label"`
	KeyPlaceholder    string       `json:"key_placeholder"`
	HelpText          string       `json:"help_text,omitempty"`
	HelpURL           string       `json:"help_url,omitempty"`
	HelpButton        string       `json:"help_button,omitempty"`
	BuiltIn           bool         `json:"built_in"`
	AdapterType       string       `json:"adapter_type"` // "gemini" | "openai_compat"
	RequiresAPIKey    bool         `json:"requires_api_key"`
	SupportsFreeBadge bool         `json:"supports_free_badge"`
	DefaultModel      string       `json:"default_model,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
}

type DiscoveredModel struct {
	Provider    string  `json:"provider"`
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Description string  `json:"description"`
	ContextLen  int     `json:"context_len"`
	IsFree      bool    `json:"is_free"`
	PriceInfo   string  `json:"price_info"`
	SortPrice   float64 `json:"-"`
}

type ProviderConfig struct {
	ID                     string            `json:"id"`
	Enabled                bool              `json:"enabled"`
	APIKey                 string            `json:"-"`
	BaseURL                string            `json:"base_url"`
	ChatPath               string            `json:"chat_path"`
	ModelsPath             string            `json:"models_path"`
	AuthType               string            `json:"auth_type"`   // "bearer" | "header" | "none"
	AuthHeader             string            `json:"auth_header"` // e.g. "Authorization", "x-api-key"
	ExtraHeaders           map[string]string `json:"extra_headers,omitempty"`
	OmitMaxTokens          bool              `json:"omit_max_tokens,omitempty"`
	StreamOnRetry          bool              `json:"stream_on_retry,omitempty"`
	AllowDiscoveryFallback bool              `json:"allow_discovery_fallback,omitempty"`
	PricingFormat          string            `json:"pricing_format,omitempty"` // "openrouter" | "codecraft" | "wallet" | "none"
	Featured               []DiscoveredModel `json:"featured_models,omitempty"`
	ManualModels           []string          `json:"manual_models,omitempty"`
	SortOrder              int               `json:"sort_order"`
}

// RegisteredProvider is the API-safe view returned to the frontend.
type RegisteredProvider struct {
	ProviderMetadata
	Enabled      bool              `json:"enabled"`
	Configured   bool              `json:"configured"`
	HasAPIKey    bool              `json:"has_api_key"`
	BaseURL      string            `json:"base_url"`
	ChatPath     string            `json:"chat_path"`
	ModelsPath   string            `json:"models_path"`
	AuthType     string            `json:"auth_type"`
	AuthHeader   string            `json:"auth_header"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	ManualModels []string          `json:"manual_models,omitempty"`
	SortOrder    int               `json:"sort_order"`
}

// Provider defines the contract every built-in or dynamic AI provider adapter implements.
type Provider interface {
	ID() string
	Metadata() ProviderMetadata
	Capabilities() Capabilities
	NormalizeModelID(modelID string) string
	Chat(ctx context.Context, cfg ProviderConfig, req ChatRequest) (ChatResponse, error)
	ListModels(ctx context.Context, cfg ProviderConfig) ([]DiscoveredModel, error)
	TestConnection(ctx context.Context, cfg ProviderConfig) error
}
