package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Store manages persistent provider configurations in SQLite while preserving backward compatibility
// with existing settings rows (<id>_api_key, <id>_custom_models).
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) InitSchema() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS provider_configs (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			short_name TEXT NOT NULL DEFAULT '',
			icon TEXT NOT NULL DEFAULT '⚡',
			description TEXT NOT NULL DEFAULT '',
			adapter_type TEXT NOT NULL DEFAULT 'openai_compat',
			base_url TEXT NOT NULL,
			chat_path TEXT NOT NULL DEFAULT '/chat/completions',
			models_path TEXT NOT NULL DEFAULT '/models',
			auth_type TEXT NOT NULL DEFAULT 'bearer',
			auth_header TEXT NOT NULL DEFAULT 'Authorization',
			extra_headers_json TEXT NOT NULL DEFAULT '{}',
			options_json TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 1,
			built_in INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL DEFAULT 100,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`)
	return err
}

type storedProviderOptions struct {
	KeyLabel               string   `json:"key_label,omitempty"`
	KeyPlaceholder         string   `json:"key_placeholder,omitempty"`
	HelpText               string   `json:"help_text,omitempty"`
	HelpURL                string   `json:"help_url,omitempty"`
	HelpButton             string   `json:"help_button,omitempty"`
	DefaultModel           string   `json:"default_model,omitempty"`
	OmitMaxTokens          bool     `json:"omit_max_tokens,omitempty"`
	StreamOnRetry          bool     `json:"stream_on_retry,omitempty"`
	AllowDiscoveryFallback bool     `json:"allow_discovery_fallback,omitempty"`
	SupportsFreeBadge      bool     `json:"supports_free_badge,omitempty"`
	PricingFormat          string   `json:"pricing_format,omitempty"`
	ManualModels           []string `json:"manual_models,omitempty"`
}

// LoadIntoRegistry hydrates dynamic providers, built-in overrides, API keys, and manual models from SQLite + env.
func (s *Store) LoadIntoRegistry(ctx context.Context, reg *Registry, client *http.Client) error {
	if s != nil && s.db != nil {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, name, short_name, icon, description, adapter_type, base_url, chat_path, models_path,
			       auth_type, auth_header, extra_headers_json, options_json, enabled, built_in, sort_order
			FROM provider_configs
			ORDER BY sort_order ASC, created_at ASC`)
		if err != nil {
			return fmt.Errorf("load provider_configs: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var id, name, shortName, icon, desc, adapterType, baseURL, chatPath, modelsPath, authType, authHeader, headersJSON, optsJSON string
			var enabledInt, builtInInt, sortOrder int
			if err := rows.Scan(&id, &name, &shortName, &icon, &desc, &adapterType, &baseURL, &chatPath, &modelsPath,
				&authType, &authHeader, &headersJSON, &optsJSON, &enabledInt, &builtInInt, &sortOrder); err != nil {
				continue
			}

			var extraHeaders map[string]string
			_ = json.Unmarshal([]byte(headersJSON), &extraHeaders)
			var opts storedProviderOptions
			_ = json.Unmarshal([]byte(optsJSON), &opts)

			if builtInInt != 0 {
				_ = reg.UpdateConfig(id, func(cfg *ProviderConfig) error {
					if baseURL != "" {
						cfg.BaseURL = baseURL
					}
					if chatPath != "" {
						cfg.ChatPath = chatPath
					}
					if modelsPath != "" {
						cfg.ModelsPath = modelsPath
					}
					cfg.Enabled = enabledInt != 0
					if len(opts.ManualModels) > 0 {
						cfg.ManualModels = opts.ManualModels
					}
					return nil
				})
				continue
			}

			meta, cfg := buildDynamicMetadataAndConfig(id, name, shortName, icon, desc, baseURL, chatPath, modelsPath, authType, authHeader, extraHeaders, opts, enabledInt != 0, sortOrder)
			adapter := NewOpenAICompatAdapter(meta, client)
			_ = reg.UpsertDynamic(adapter, cfg)
		}
	}

	// Hydrate API keys and legacy custom models for all registered providers
	for _, item := range reg.List() {
		id := item.ID
		key := s.resolveAPIKey(id)
		manual := s.resolveManualModels(id)
		_ = reg.UpdateConfig(id, func(cfg *ProviderConfig) error {
			if key != "" {
				cfg.APIKey = key
			}
			if len(manual) > 0 {
				cfg.ManualModels = mergeUniqueStrings(cfg.ManualModels, manual)
			}
			return nil
		})
	}
	return nil
}

// SaveDynamicProvider persists a custom OpenAI-compatible provider and registers it in the live Registry.
func (s *Store) SaveDynamicProvider(ctx context.Context, reg *Registry, client *http.Client, input DynamicProviderInput) (RegisteredProvider, error) {
	id := CanonicalID(input.ID)
	if err := ValidateProviderID(id); err != nil {
		return RegisteredProvider{}, err
	}
	if existing, _, err := reg.Get(id); err == nil && existing.Metadata().BuiltIn {
		return RegisteredProvider{}, fmt.Errorf("%w: %s", ErrBuiltInProtected, id)
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = id
	}
	shortName := strings.ToUpper(strings.TrimSpace(input.ShortName))
	if shortName == "" {
		shortName = strings.ToUpper(id)
		if len(shortName) > 12 {
			shortName = shortName[:12]
		}
	}
	icon := strings.TrimSpace(input.Icon)
	if icon == "" {
		icon = "⚡"
	}
	chatPath := strings.TrimSpace(input.ChatPath)
	if chatPath == "" {
		chatPath = "/chat/completions"
	}
	modelsPath := strings.TrimSpace(input.ModelsPath)
	if modelsPath == "" && !input.DisableDiscovery {
		modelsPath = "/models"
	}
	if input.DisableDiscovery {
		modelsPath = ""
	}
	authType := strings.ToLower(strings.TrimSpace(input.AuthType))
	if authType == "" {
		authType = "bearer"
	}
	authHeader := strings.TrimSpace(input.AuthHeader)
	if authHeader == "" && authType == "bearer" {
		authHeader = "Authorization"
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	sortOrder := input.SortOrder
	if sortOrder <= 0 {
		sortOrder = 100
	}

	opts := storedProviderOptions{
		KeyLabel:               strings.TrimSpace(input.KeyLabel),
		KeyPlaceholder:         strings.TrimSpace(input.KeyPlaceholder),
		DefaultModel:           strings.TrimSpace(input.DefaultModel),
		OmitMaxTokens:          input.OmitMaxTokens,
		StreamOnRetry:          input.StreamOnRetry,
		AllowDiscoveryFallback: len(input.ManualModels) > 0,
		SupportsFreeBadge:      input.SupportsFreeBadge,
		PricingFormat:          strings.TrimSpace(input.PricingFormat),
		ManualModels:           cleanStringList(input.ManualModels),
	}

	meta, cfg := buildDynamicMetadataAndConfig(
		id, name, shortName, icon, strings.TrimSpace(input.Description),
		strings.TrimSpace(input.BaseURL), chatPath, modelsPath,
		authType, authHeader, input.ExtraHeaders, opts, enabled, sortOrder,
	)
	if strings.TrimSpace(input.APIKey) != "" {
		cfg.APIKey = strings.TrimSpace(input.APIKey)
	} else if _, existingCfg, err := reg.Get(id); err == nil {
		cfg.APIKey = existingCfg.APIKey
	} else {
		cfg.APIKey = s.resolveAPIKey(id)
	}

	if err := ValidateConfig(meta, cfg); err != nil {
		return RegisteredProvider{}, err
	}

	if s != nil && s.db != nil {
		s.mu.Lock()
		headersBytes, _ := json.Marshal(cfg.ExtraHeaders)
		optsBytes, _ := json.Marshal(opts)
		enabledInt := 0
		if cfg.Enabled {
			enabledInt = 1
		}
		now := time.Now().UTC()
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO provider_configs
				(id, name, short_name, icon, description, adapter_type, base_url, chat_path, models_path,
				 auth_type, auth_header, extra_headers_json, options_json, enabled, built_in, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'openai_compat', ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name=excluded.name,
				short_name=excluded.short_name,
				icon=excluded.icon,
				description=excluded.description,
				base_url=excluded.base_url,
				chat_path=excluded.chat_path,
				models_path=excluded.models_path,
				auth_type=excluded.auth_type,
				auth_header=excluded.auth_header,
				extra_headers_json=excluded.extra_headers_json,
				options_json=excluded.options_json,
				enabled=excluded.enabled,
				sort_order=excluded.sort_order,
				updated_at=excluded.updated_at`,
			id, meta.Name, meta.ShortName, meta.Icon, meta.Description,
			cfg.BaseURL, cfg.ChatPath, cfg.ModelsPath, cfg.AuthType, cfg.AuthHeader,
			string(headersBytes), string(optsBytes), enabledInt, cfg.SortOrder, now, now)
		if err == nil && strings.TrimSpace(input.APIKey) != "" {
			_, _ = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				id+"_api_key", strings.TrimSpace(input.APIKey))
		}
		if err == nil && len(opts.ManualModels) > 0 {
			if rawManual, mErr := json.Marshal(opts.ManualModels); mErr == nil {
				_, _ = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
					id+"_custom_models", string(rawManual))
			}
		}
		s.mu.Unlock()
		if err != nil {
			return RegisteredProvider{}, err
		}
	}

	adapter := NewOpenAICompatAdapter(meta, client)
	if err := reg.UpsertDynamic(adapter, cfg); err != nil {
		return RegisteredProvider{}, err
	}

	for _, item := range reg.List() {
		if item.ID == id {
			return item, nil
		}
	}
	return RegisteredProvider{}, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
}

func (s *Store) DeleteDynamicProvider(ctx context.Context, reg *Registry, id string) error {
	norm := CanonicalID(id)
	if err := reg.Remove(norm); err != nil {
		return err
	}
	if s != nil && s.db != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = s.db.ExecContext(ctx, `DELETE FROM provider_configs WHERE id = ? AND built_in = 0`, norm)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM settings WHERE key IN (?, ?, ?, ?)`,
			norm+"_api_key", norm+"_custom_models", norm+"_model_tests", norm+"_active_model")
	}
	return nil
}

func (s *Store) SaveAPIKey(ctx context.Context, reg *Registry, id, apiKey string) error {
	norm := CanonicalID(id)
	trimmed := strings.TrimSpace(apiKey)
	if err := reg.UpdateConfig(norm, func(cfg *ProviderConfig) error {
		cfg.APIKey = trimmed
		return nil
	}); err != nil {
		return err
	}
	if s != nil && s.db != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			norm+"_api_key", trimmed)
		return err
	}
	return nil
}

func (s *Store) SaveManualModel(ctx context.Context, reg *Registry, id, modelID string) error {
	norm := CanonicalID(id)
	clean := reg.NormalizeModelID(norm, modelID)
	if clean == "" {
		return nil
	}
	var merged []string
	if err := reg.UpdateConfig(norm, func(cfg *ProviderConfig) error {
		cfg.ManualModels = mergeUniqueStrings([]string{clean}, cfg.ManualModels)
		merged = append([]string(nil), cfg.ManualModels...)
		return nil
	}); err != nil {
		return err
	}
	if s != nil && s.db != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if raw, err := json.Marshal(merged); err == nil {
			_, _ = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				norm+"_custom_models", string(raw))
		}
	}
	return nil
}

func (s *Store) resolveAPIKey(id string) string {
	norm := CanonicalID(id)
	if s != nil && s.db != nil {
		var v string
		if s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, norm+"_api_key").Scan(&v) == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	envName := strings.ToUpper(strings.ReplaceAll(norm, "-", "_")) + "_API_KEY"
	return strings.TrimSpace(os.Getenv(envName))
}

func (s *Store) resolveManualModels(id string) []string {
	norm := CanonicalID(id)
	if s == nil || s.db == nil {
		return nil
	}
	var raw string
	if s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, norm+"_custom_models").Scan(&raw) != nil || raw == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return cleanStringList(out)
}

type DynamicProviderInput struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	ShortName         string            `json:"short_name"`
	Icon              string            `json:"icon"`
	Description       string            `json:"description"`
	BaseURL           string            `json:"base_url"`
	ChatPath          string            `json:"chat_path"`
	ModelsPath        string            `json:"models_path"`
	DisableDiscovery  bool              `json:"disable_discovery"`
	AuthType          string            `json:"auth_type"`
	AuthHeader        string            `json:"auth_header"`
	APIKey            string            `json:"api_key"`
	KeyLabel          string            `json:"key_label"`
	KeyPlaceholder    string            `json:"key_placeholder"`
	ExtraHeaders      map[string]string `json:"extra_headers"`
	DefaultModel      string            `json:"default_model"`
	ManualModels      []string          `json:"manual_models"`
	OmitMaxTokens     bool              `json:"omit_max_tokens"`
	StreamOnRetry     bool              `json:"stream_on_retry"`
	SupportsFreeBadge bool              `json:"supports_free_badge"`
	PricingFormat     string            `json:"pricing_format"`
	Enabled           *bool             `json:"enabled"`
	SortOrder         int               `json:"sort_order"`
}

func buildDynamicMetadataAndConfig(
	id, name, shortName, icon, desc, baseURL, chatPath, modelsPath, authType, authHeader string,
	extraHeaders map[string]string,
	opts storedProviderOptions,
	enabled bool,
	sortOrder int,
) (ProviderMetadata, ProviderConfig) {
	requiresKey := !strings.EqualFold(authType, "none")
	keyLabel := opts.KeyLabel
	if keyLabel == "" {
		if requiresKey {
			keyLabel = fmt.Sprintf("%s API Key (%s)", name, baseURL)
		} else {
			keyLabel = fmt.Sprintf("%s Endpoint (No API key required)", name)
		}
	}
	keyPlaceholder := opts.KeyPlaceholder
	if keyPlaceholder == "" {
		if requiresKey {
			keyPlaceholder = fmt.Sprintf("Paste your %s API key…", name)
		} else {
			keyPlaceholder = "Optional — leave blank if server requires no key"
		}
	}
	if desc == "" {
		desc = fmt.Sprintf("OpenAI-compatible provider at %s", baseURL)
	}
	defaultModel := opts.DefaultModel
	if defaultModel == "" && len(opts.ManualModels) > 0 {
		defaultModel = opts.ManualModels[0]
	}

	meta := ProviderMetadata{
		ID:                id,
		Name:              name,
		ShortName:         shortName,
		Icon:              icon,
		Description:       desc,
		KeyLabel:          keyLabel,
		KeyPlaceholder:    keyPlaceholder,
		BuiltIn:           false,
		AdapterType:       "openai_compat",
		RequiresAPIKey:    requiresKey,
		SupportsFreeBadge: opts.SupportsFreeBadge,
		DefaultModel:      defaultModel,
		Capabilities: Capabilities{
			Chat:            true,
			Streaming:       true,
			ModelDiscovery:  strings.TrimSpace(modelsPath) != "",
			SystemPrompt:    true,
			ReasoningOutput: true,
			JSONMode:        true,
		},
	}

	cfg := ProviderConfig{
		ID:                     id,
		Enabled:                enabled,
		BaseURL:                baseURL,
		ChatPath:               chatPath,
		ModelsPath:             modelsPath,
		AuthType:               authType,
		AuthHeader:             authHeader,
		ExtraHeaders:           extraHeaders,
		OmitMaxTokens:          opts.OmitMaxTokens,
		StreamOnRetry:          opts.StreamOnRetry,
		AllowDiscoveryFallback: opts.AllowDiscoveryFallback,
		PricingFormat:          opts.PricingFormat,
		ManualModels:           opts.ManualModels,
		SortOrder:              sortOrder,
	}
	return meta, cfg
}

func cleanStringList(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range items {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func mergeUniqueStrings(primary, secondary []string) []string {
	return cleanStringList(append(append([]string(nil), primary...), secondary...))
}
