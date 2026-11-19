package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func (a *App) setting(key string) string {
	var v string
	_ = a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (a *App) providerKey(p string) string {
	if _, cfg, err := a.router.Registry().Get(p); err == nil && strings.TrimSpace(cfg.APIKey) != "" {
		return cfg.APIKey
	}
	norm := provider.CanonicalID(p)
	if v := a.setting(norm + "_api_key"); v != "" {
		return provider.DecryptAPIKey(v)
	}
	envName := strings.ToUpper(strings.ReplaceAll(norm, "-", "_")) + "_API_KEY"
	return os.Getenv(envName)
}

func (a *App) isProviderConfigured(p string) bool {
	if a.router.Registry().IsConfigured(p) {
		return true
	}
	return strings.TrimSpace(a.providerKey(p)) != ""
}

func (a *App) loadTestRecords(p string) map[string]ModelTestRecord {
	norm := provider.CanonicalID(p)
	out := map[string]ModelTestRecord{}
	raw := a.setting(norm + "_model_tests")
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) setModelsEnabledBatch(p string, models []string, enabled bool) {
	norm := provider.CanonicalID(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(norm)
	for _, m := range models {
		modelID := a.router.Registry().NormalizeModelID(norm, m)
		if modelID == "" {
			continue
		}
		rec := records[modelID]
		if rec.Status == "" {
			rec.Status = "untested"
		}
		rec.Disabled = !enabled
		records[modelID] = rec
	}
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(norm+"_model_tests", string(data))
	}
}

func (a *App) setModelEnabled(p, model string, enabled bool) {
	a.setModelsEnabledBatch(p, []string{model}, enabled)
}

func (a *App) saveTestRecord(p, model string, rec ModelTestRecord) {
	norm := provider.CanonicalID(p)
	modelID := a.router.Registry().NormalizeModelID(norm, model)
	a.mu.Lock()
	defer a.mu.Unlock()
	records := a.loadTestRecords(norm)
	if prev, ok := records[modelID]; ok {
		rec.Disabled = prev.Disabled
	}
	records[modelID] = rec
	if data, err := json.Marshal(records); err == nil {
		_ = a.setSetting(norm+"_model_tests", string(data))
	}
}

func (a *App) verifiedWorkingParticipants() []orchestration.Participant {
	var out []orchestration.Participant
	for _, prov := range a.router.Registry().List() {
		if !prov.Configured {
			continue
		}
		records := a.loadTestRecords(prov.ID)
		for modelID, rec := range records {
			if rec.Status == "working" && !rec.Disabled {
				out = append(out, orchestration.Participant{
					Provider: prov.ID,
					Model:    modelID,
				})
			}
		}
	}
	return out
}

func (a *App) activeProvider() string {
	registered := a.router.Registry().List()
	if p := provider.CanonicalID(a.setting("active_provider")); p != "" {
		if _, _, err := a.router.Registry().Get(p); err == nil {
			if a.isProviderConfigured(p) {
				return p
			}
			anyConfigured := false
			for _, item := range registered {
				if item.Configured {
					anyConfigured = true
					break
				}
			}
			if !anyConfigured {
				return p
			}
		}
	}
	for _, item := range registered {
		if item.Configured {
			return item.ID
		}
	}
	if len(registered) > 0 {
		return registered[0].ID
	}
	return ""
}

func (a *App) activeModel() string {
	prov := a.activeProvider()
	if m := strings.TrimSpace(a.setting("active_model")); m != "" {
		if storedProv := provider.CanonicalID(a.setting("active_provider")); storedProv == "" || storedProv == prov {
			return a.router.Registry().NormalizeModelID(prov, m)
		}
	}
	if m := strings.TrimSpace(a.setting(prov + "_active_model")); m != "" {
		return a.router.Registry().NormalizeModelID(prov, m)
	}
	if p, cfg, err := a.router.Registry().Get(prov); err == nil {
		if p.Metadata().DefaultModel != "" {
			return p.Metadata().DefaultModel
		}
		if len(cfg.ManualModels) > 0 {
			return cfg.ManualModels[0]
		}
	}
	return ""
}

func (a *App) setActiveSelection(p, model string) {
	norm := provider.CanonicalID(p)
	cleanModel := a.router.Registry().NormalizeModelID(norm, model)
	_ = a.setSetting("active_provider", norm)
	_ = a.setSetting("active_model", cleanModel)
	_ = a.setSetting(norm+"_active_model", cleanModel)
}

func (a *App) loadChatModelScope() ChatModelScope {
	mode := strings.ToLower(strings.TrimSpace(a.setting("chat_model_scope_mode")))
	if mode != "selected" {
		mode = "all"
	}
	var rawList []string
	if raw := a.setting("chat_allowed_models"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &rawList)
	}
	seen := map[string]bool{}
	allowed := make([]string, 0, len(rawList))
	for _, entry := range rawList {
		parts := strings.SplitN(strings.TrimSpace(entry), "::", 2)
		if len(parts) != 2 {
			continue
		}
		p := provider.CanonicalID(parts[0])
		m := a.router.Registry().NormalizeModelID(p, parts[1])
		if p == "" || m == "" {
			continue
		}
		key := p + "::" + m
		if !seen[key] {
			seen[key] = true
			allowed = append(allowed, key)
		}
	}
	return ChatModelScope{
		Mode:          mode,
		AllowedModels: allowed,
	}
}

func (a *App) saveChatModelScope(scope ChatModelScope) error {
	mode := strings.ToLower(strings.TrimSpace(scope.Mode))
	if mode != "selected" {
		mode = "all"
	}
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(scope.AllowedModels))
	for _, entry := range scope.AllowedModels {
		parts := strings.SplitN(strings.TrimSpace(entry), "::", 2)
		if len(parts) != 2 {
			continue
		}
		p := provider.CanonicalID(parts[0])
		m := a.router.Registry().NormalizeModelID(p, parts[1])
		if p == "" || m == "" {
			continue
		}
		key := p + "::" + m
		if !seen[key] {
			seen[key] = true
			cleaned = append(cleaned, key)
		}
	}
	if err := a.setSetting("chat_model_scope_mode", mode); err != nil {
		return err
	}
	data, _ := json.Marshal(cleaned)
	return a.setSetting("chat_allowed_models", string(data))
}

func (a *App) isModelAllowedForChat(providerID, modelID string) bool {
	scope := a.loadChatModelScope()
	if scope.Mode != "selected" {
		return true
	}
	target := provider.CanonicalID(providerID) + "::" + a.router.Registry().NormalizeModelID(providerID, modelID)
	for _, item := range scope.AllowedModels {
		if item == target {
			return true
		}
	}
	return false
}

func (a *App) handleChatModelScope(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, 200, a.loadChatModelScope())
	case http.MethodPost, http.MethodPut:
		var body ChatModelScope
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid chat model scope payload"})
			return
		}
		if err := a.saveChatModelScope(body); err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed saving chat model scope"})
			return
		}
		jsonOut(w, 200, a.loadChatModelScope())
	default:
		http.Error(w, "method not allowed", 405)
	}
}
