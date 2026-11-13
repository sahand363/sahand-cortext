package provider

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var validProviderIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,39}$`)

type registryEntry struct {
	provider Provider
	config   ProviderConfig
}

// Registry is the single source of truth for all built-in and dynamic providers.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*registryEntry
	order   []string
}

func NewRegistry() *Registry {
	return &Registry{
		entries: make(map[string]*registryEntry),
	}
}

func CanonicalID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

func ValidateProviderID(id string) error {
	norm := CanonicalID(id)
	if !validProviderIDRe.MatchString(norm) {
		return fmt.Errorf("%w: provider ID %q must be 2–40 lowercase letters, digits, hyphens, or underscores", ErrInvalidConfig, id)
	}
	return nil
}

func ValidateConfig(meta ProviderMetadata, cfg ProviderConfig) error {
	if err := ValidateProviderID(meta.ID); err != nil {
		return err
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return fmt.Errorf("%w: base_url is required for provider %q", ErrInvalidConfig, meta.ID)
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: base_url %q must be a valid http:// or https:// URL", ErrInvalidConfig, cfg.BaseURL)
	}
	auth := strings.ToLower(strings.TrimSpace(cfg.AuthType))
	if auth == "" {
		auth = "bearer"
	}
	switch auth {
	case "bearer", "none":
	case "header":
		if strings.TrimSpace(cfg.AuthHeader) == "" {
			return fmt.Errorf("%w: auth_header is required when auth_type is 'header'", ErrInvalidConfig)
		}
	default:
		return fmt.Errorf("%w: unsupported auth_type %q (expected bearer, header, or none)", ErrInvalidConfig, cfg.AuthType)
	}
	return nil
}

// Register adds a new provider to the registry and rejects duplicate IDs.
func (r *Registry) Register(p Provider, cfg ProviderConfig) error {
	if p == nil {
		return fmt.Errorf("%w: nil provider", ErrInvalidConfig)
	}
	meta := p.Metadata()
	id := CanonicalID(p.ID())
	meta.ID = id
	cfg.ID = id

	if err := ValidateConfig(meta, cfg); err != nil {
		return err
	}
	if cfg.AuthType == "" {
		cfg.AuthType = "bearer"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.entries[id]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateProvider, id)
	}
	r.entries[id] = &registryEntry{
		provider: p,
		config:   cloneConfig(cfg),
	}
	r.order = append(r.order, id)
	return nil
}

// UpsertDynamic registers a new dynamic provider or updates an existing dynamic one.
func (r *Registry) UpsertDynamic(p Provider, cfg ProviderConfig) error {
	if p == nil {
		return fmt.Errorf("%w: nil provider", ErrInvalidConfig)
	}
	meta := p.Metadata()
	id := CanonicalID(p.ID())
	meta.ID = id
	cfg.ID = id

	if err := ValidateConfig(meta, cfg); err != nil {
		return err
	}
	if cfg.AuthType == "" {
		cfg.AuthType = "bearer"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, exists := r.entries[id]; exists {
		if existing.provider.Metadata().BuiltIn {
			return fmt.Errorf("%w: %s", ErrBuiltInProtected, id)
		}
		if cfg.APIKey == "" {
			cfg.APIKey = existing.config.APIKey
		}
		if len(cfg.ManualModels) == 0 && len(existing.config.ManualModels) > 0 {
			cfg.ManualModels = append([]string(nil), existing.config.ManualModels...)
		}
		existing.provider = p
		existing.config = cloneConfig(cfg)
		return nil
	}

	r.entries[id] = &registryEntry{
		provider: p,
		config:   cloneConfig(cfg),
	}
	r.order = append(r.order, id)
	return nil
}

// Remove deletes a dynamically registered provider. Built-in providers cannot be deleted.
func (r *Registry) Remove(id string) error {
	norm := CanonicalID(id)
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, exists := r.entries[norm]
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	if entry.provider.Metadata().BuiltIn {
		return fmt.Errorf("%w: %s", ErrBuiltInProtected, norm)
	}
	delete(r.entries, norm)
	for i, item := range r.order {
		if item == norm {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return nil
}

// Get returns the registered Provider and a snapshot of its active ProviderConfig.
func (r *Registry) Get(id string) (Provider, ProviderConfig, error) {
	norm := CanonicalID(id)
	if norm == "" {
		return nil, ProviderConfig{}, fmt.Errorf("%w: empty provider ID", ErrUnknownProvider)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, exists := r.entries[norm]
	if !exists {
		return nil, ProviderConfig{}, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	return entry.provider, cloneConfig(entry.config), nil
}

// UpdateConfig mutates a registered provider's configuration safely under write lock.
func (r *Registry) UpdateConfig(id string, mutate func(*ProviderConfig) error) error {
	norm := CanonicalID(id)
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, exists := r.entries[norm]
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	updated := cloneConfig(entry.config)
	if err := mutate(&updated); err != nil {
		return err
	}
	if err := ValidateConfig(entry.provider.Metadata(), updated); err != nil {
		return err
	}
	entry.config = updated
	return nil
}

// SetBaseURLOverride updates the BaseURL of a registered provider (useful in tests and custom endpoints).
func (r *Registry) SetBaseURLOverride(id, baseURL string) error {
	return r.UpdateConfig(id, func(cfg *ProviderConfig) error {
		cfg.BaseURL = strings.TrimSpace(baseURL)
		return nil
	})
}

func (r *Registry) IsConfigured(id string) bool {
	p, cfg, err := r.Get(id)
	if err != nil || !cfg.Enabled {
		return false
	}
	meta := p.Metadata()
	if !meta.RequiresAPIKey || strings.EqualFold(cfg.AuthType, "none") {
		return true
	}
	return strings.TrimSpace(cfg.APIKey) != ""
}

func (r *Registry) Label(id string) string {
	p, _, err := r.Get(id)
	if err != nil {
		return id
	}
	return p.Metadata().Name
}

func (r *Registry) NormalizeModelID(providerID, modelID string) string {
	m := strings.TrimSpace(modelID)
	p, _, err := r.Get(providerID)
	if err != nil {
		return m
	}
	return p.NormalizeModelID(m)
}

func (r *Registry) List() []RegisteredProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]RegisteredProvider, 0, len(r.order))
	for _, id := range r.order {
		entry, ok := r.entries[id]
		if !ok {
			continue
		}
		meta := entry.provider.Metadata()
		cfg := entry.config
		hasKey := strings.TrimSpace(cfg.APIKey) != ""
		configured := cfg.Enabled && (!meta.RequiresAPIKey || strings.EqualFold(cfg.AuthType, "none") || hasKey)

		headersCopy := make(map[string]string, len(cfg.ExtraHeaders))
		for k, v := range cfg.ExtraHeaders {
			headersCopy[k] = v
		}

		out = append(out, RegisteredProvider{
			ProviderMetadata: meta,
			Enabled:          cfg.Enabled,
			Configured:       configured,
			HasAPIKey:        hasKey,
			BaseURL:          cfg.BaseURL,
			ChatPath:         cfg.ChatPath,
			ModelsPath:       cfg.ModelsPath,
			AuthType:         cfg.AuthType,
			AuthHeader:       cfg.AuthHeader,
			ExtraHeaders:     headersCopy,
			ManualModels:     append([]string(nil), cfg.ManualModels...),
			SortOrder:        cfg.SortOrder,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SortOrder < out[j].SortOrder
	})
	return out
}

func (r *Registry) IDs() []string {
	list := r.List()
	ids := make([]string, 0, len(list))
	for _, item := range list {
		ids = append(ids, item.ID)
	}
	return ids
}

func cloneConfig(c ProviderConfig) ProviderConfig {
	cp := c
	if c.ExtraHeaders != nil {
		cp.ExtraHeaders = make(map[string]string, len(c.ExtraHeaders))
		for k, v := range c.ExtraHeaders {
			cp.ExtraHeaders[k] = v
		}
	}
	if c.Featured != nil {
		cp.Featured = append([]DiscoveredModel(nil), c.Featured...)
	}
	if c.ManualModels != nil {
		cp.ManualModels = append([]string(nil), c.ManualModels...)
	}
	return cp
}
