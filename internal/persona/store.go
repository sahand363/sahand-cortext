package persona

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

var (
	ErrPersonaNotFound = errors.New("chatbot persona not found")
	ErrInvalidPersona  = errors.New("invalid chatbot persona configuration")
	idSlugRegex        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,39}$`)
)

// FewShotExample stores a real or curated input->reply pair used to anchor a persona's voice.
type FewShotExample struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

// Persona represents a pluggable chatbot / AI specialist / voice clone stored in SQLite.
type Persona struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	Icon                 string           `json:"icon"`
	Description          string           `json:"description"`
	SystemPrompt         string           `json:"system_prompt"`
	KnowledgeNotes       string           `json:"knowledge_notes"`
	VoiceBlueprint       string           `json:"voice_blueprint"`
	FewShotExamples      []FewShotExample `json:"few_shot_examples"`
	DefaultProvider      string           `json:"default_provider"`
	DefaultModel         string           `json:"default_model"`
	ModelScopeMode       string           `json:"model_scope_mode"` // "all" | "selected"
	AllowedModels        []string         `json:"allowed_models"`   // ["provider::model", ...]
	TelegramEnabled      bool             `json:"telegram_enabled"`
	TelegramBotToken     string           `json:"telegram_bot_token,omitempty"`
	HasTelegramToken     bool             `json:"has_telegram_token"`
	TelegramAllowedUsers []string         `json:"telegram_allowed_users"`
	Enabled              bool             `json:"enabled"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
}

// PersonaInput is the payload accepted when creating or updating a Persona.
type PersonaInput struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	Icon                 string           `json:"icon"`
	Description          string           `json:"description"`
	SystemPrompt         string           `json:"system_prompt"`
	KnowledgeNotes       string           `json:"knowledge_notes"`
	VoiceBlueprint       string           `json:"voice_blueprint"`
	FewShotExamples      []FewShotExample `json:"few_shot_examples"`
	DefaultProvider      string           `json:"default_provider"`
	DefaultModel         string           `json:"default_model"`
	ModelScopeMode       string           `json:"model_scope_mode"`
	AllowedModels        []string         `json:"allowed_models"`
	TelegramEnabled      bool             `json:"telegram_enabled"`
	TelegramBotToken     string           `json:"telegram_bot_token"`
	ClearTelegramToken   bool             `json:"clear_telegram_token"`
	TelegramAllowedUsers []string         `json:"telegram_allowed_users"`
	Enabled              *bool            `json:"enabled"`
}

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

	queries := []string{
		`CREATE TABLE IF NOT EXISTS personas (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			icon TEXT NOT NULL DEFAULT '🤖',
			description TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			knowledge_notes TEXT NOT NULL DEFAULT '',
			voice_blueprint TEXT NOT NULL DEFAULT '',
			few_shot_json TEXT NOT NULL DEFAULT '[]',
			default_provider TEXT NOT NULL DEFAULT '',
			default_model TEXT NOT NULL DEFAULT '',
			model_scope_mode TEXT NOT NULL DEFAULT 'all',
			allowed_models_json TEXT NOT NULL DEFAULT '[]',
			telegram_enabled INTEGER NOT NULL DEFAULT 0,
			telegram_bot_token TEXT NOT NULL DEFAULT '',
			telegram_allowed_users_json TEXT NOT NULL DEFAULT '[]',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`ALTER TABLE sessions ADD COLUMN persona_id TEXT NOT NULL DEFAULT '';`,
	}

	for i, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			// Ignore duplicate column error on ALTER TABLE
			if i == 1 && strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("persona schema init (%d): %w", i, err)
		}
	}
	return nil
}

func CanonicalID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func randomPersonaID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "bot-" + hex.EncodeToString(b)
}

func cleanAllowedModels(reg *provider.Registry, items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, entry := range items {
		parts := strings.SplitN(strings.TrimSpace(entry), "::", 2)
		if len(parts) != 2 {
			continue
		}
		p := provider.CanonicalID(parts[0])
		m := strings.TrimSpace(parts[1])
		if reg != nil {
			m = reg.NormalizeModelID(p, m)
		}
		if p == "" || m == "" {
			continue
		}
		key := p + "::" + m
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func cleanStringSlice(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
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

func cleanFewShotExamples(items []FewShotExample) []FewShotExample {
	out := make([]FewShotExample, 0, len(items))
	for _, ex := range items {
		u := strings.TrimSpace(ex.User)
		a := strings.TrimSpace(ex.Assistant)
		if u == "" && a == "" {
			continue
		}
		if u == "" {
			u = "Hey"
		}
		if a == "" {
			continue
		}
		out = append(out, FewShotExample{User: u, Assistant: a})
		if len(out) >= 12 {
			break
		}
	}
	return out
}

// Save creates or updates a Persona in SQLite.
func (s *Store) Save(ctx context.Context, reg *provider.Registry, input PersonaInput) (*Persona, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("persona store not initialized")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: chatbot name is required", ErrInvalidPersona)
	}

	id := CanonicalID(input.ID)
	if id == "" {
		id = CanonicalID(name)
	}
	if id == "" {
		id = randomPersonaID()
	}
	if !idSlugRegex.MatchString(id) {
		return nil, fmt.Errorf("%w: id must be 2-40 lowercase letters, digits, hyphens, or underscores", ErrInvalidPersona)
	}

	icon := strings.TrimSpace(input.Icon)
	if icon == "" {
		icon = "🤖"
	}

	scopeMode := strings.ToLower(strings.TrimSpace(input.ModelScopeMode))
	if scopeMode != "selected" {
		scopeMode = "all"
	}
	allowedModels := cleanAllowedModels(reg, input.AllowedModels)

	defProv := provider.CanonicalID(input.DefaultProvider)
	defMod := strings.TrimSpace(input.DefaultModel)
	if strings.Contains(defMod, "::") {
		parts := strings.SplitN(defMod, "::", 2)
		defProv = provider.CanonicalID(parts[0])
		defMod = strings.TrimSpace(parts[1])
	}
	if reg != nil && defProv != "" && defMod != "" {
		defMod = reg.NormalizeModelID(defProv, defMod)
	}

	// If scope is "selected" and default model isn't in allowed_models, align default with first allowed model
	if scopeMode == "selected" && len(allowedModels) > 0 {
		defKey := defProv + "::" + defMod
		found := false
		for _, item := range allowedModels {
			if item == defKey {
				found = true
				break
			}
		}
		if !found {
			parts := strings.SplitN(allowedModels[0], "::", 2)
			if len(parts) == 2 {
				defProv = parts[0]
				defMod = parts[1]
			}
		}
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	fewShots := cleanFewShotExamples(input.FewShotExamples)
	tgUsers := cleanStringSlice(input.TelegramAllowedUsers)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Preserve existing Telegram token unless explicitly replaced or cleared
	var existingToken string
	_ = s.db.QueryRowContext(ctx, `SELECT telegram_bot_token FROM personas WHERE id = ?`, id).Scan(&existingToken)

	finalToken := existingToken
	if input.ClearTelegramToken {
		finalToken = ""
	} else if strings.TrimSpace(input.TelegramBotToken) != "" {
		finalToken = strings.TrimSpace(input.TelegramBotToken)
	}

	fewShotBytes, _ := json.Marshal(fewShots)
	allowedBytes, _ := json.Marshal(allowedModels)
	tgUsersBytes, _ := json.Marshal(tgUsers)

	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	tgEnabledInt := 0
	if input.TelegramEnabled {
		tgEnabledInt = 1
	}

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO personas (
			id, name, icon, description, system_prompt, knowledge_notes, voice_blueprint,
			few_shot_json, default_provider, default_model, model_scope_mode, allowed_models_json,
			telegram_enabled, telegram_bot_token, telegram_allowed_users_json, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			icon=excluded.icon,
			description=excluded.description,
			system_prompt=excluded.system_prompt,
			knowledge_notes=excluded.knowledge_notes,
			voice_blueprint=excluded.voice_blueprint,
			few_shot_json=excluded.few_shot_json,
			default_provider=excluded.default_provider,
			default_model=excluded.default_model,
			model_scope_mode=excluded.model_scope_mode,
			allowed_models_json=excluded.allowed_models_json,
			telegram_enabled=excluded.telegram_enabled,
			telegram_bot_token=excluded.telegram_bot_token,
			telegram_allowed_users_json=excluded.telegram_allowed_users_json,
			enabled=excluded.enabled,
			updated_at=excluded.updated_at`,
		id, name, icon, strings.TrimSpace(input.Description), strings.TrimSpace(input.SystemPrompt),
		strings.TrimSpace(input.KnowledgeNotes), strings.TrimSpace(input.VoiceBlueprint),
		string(fewShotBytes), defProv, defMod, scopeMode, string(allowedBytes),
		tgEnabledInt, finalToken, string(tgUsersBytes), enabledInt, now, now)
	if err != nil {
		return nil, err
	}

	return s.getLocked(ctx, id, false)
}

// Get retrieves a Persona by ID (with Telegram bot token redacted for safe API responses).
func (s *Store) Get(ctx context.Context, id string) (*Persona, error) {
	if s == nil || s.db == nil {
		return nil, ErrPersonaNotFound
	}
	return s.getLocked(ctx, CanonicalID(id), false)
}

// GetWithSecrets retrieves a Persona including its raw Telegram bot token (for internal Telegram workers).
func (s *Store) GetWithSecrets(ctx context.Context, id string) (*Persona, error) {
	if s == nil || s.db == nil {
		return nil, ErrPersonaNotFound
	}
	return s.getLocked(ctx, CanonicalID(id), true)
}

func (s *Store) getLocked(ctx context.Context, id string, includeSecrets bool) (*Persona, error) {
	var p Persona
	var fewShotJSON, allowedJSON, tgUsersJSON, rawToken string
	var tgEnabledInt, enabledInt int

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, icon, description, system_prompt, knowledge_notes, voice_blueprint,
		       few_shot_json, default_provider, default_model, model_scope_mode, allowed_models_json,
		       telegram_enabled, telegram_bot_token, telegram_allowed_users_json, enabled, created_at, updated_at
		FROM personas
		WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.Icon, &p.Description, &p.SystemPrompt, &p.KnowledgeNotes, &p.VoiceBlueprint,
		&fewShotJSON, &p.DefaultProvider, &p.DefaultModel, &p.ModelScopeMode, &allowedJSON,
		&tgEnabledInt, &rawToken, &tgUsersJSON, &enabledInt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPersonaNotFound
		}
		return nil, err
	}

	_ = json.Unmarshal([]byte(fewShotJSON), &p.FewShotExamples)
	if p.FewShotExamples == nil {
		p.FewShotExamples = []FewShotExample{}
	}
	_ = json.Unmarshal([]byte(allowedJSON), &p.AllowedModels)
	if p.AllowedModels == nil {
		p.AllowedModels = []string{}
	}
	_ = json.Unmarshal([]byte(tgUsersJSON), &p.TelegramAllowedUsers)
	if p.TelegramAllowedUsers == nil {
		p.TelegramAllowedUsers = []string{}
	}

	p.TelegramEnabled = tgEnabledInt != 0
	p.Enabled = enabledInt != 0
	p.HasTelegramToken = strings.TrimSpace(rawToken) != ""
	if includeSecrets {
		p.TelegramBotToken = rawToken
	}
	return &p, nil
}

// List returns all registered Personas sorted by name (with tokens redacted).
func (s *Store) List(ctx context.Context, includeSecrets bool) ([]Persona, error) {
	if s == nil || s.db == nil {
		return []Persona{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, icon, description, system_prompt, knowledge_notes, voice_blueprint,
		       few_shot_json, default_provider, default_model, model_scope_mode, allowed_models_json,
		       telegram_enabled, telegram_bot_token, telegram_allowed_users_json, enabled, created_at, updated_at
		FROM personas
		ORDER BY name ASC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Persona{}
	for rows.Next() {
		var p Persona
		var fewShotJSON, allowedJSON, tgUsersJSON, rawToken string
		var tgEnabledInt, enabledInt int
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Icon, &p.Description, &p.SystemPrompt, &p.KnowledgeNotes, &p.VoiceBlueprint,
			&fewShotJSON, &p.DefaultProvider, &p.DefaultModel, &p.ModelScopeMode, &allowedJSON,
			&tgEnabledInt, &rawToken, &tgUsersJSON, &enabledInt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(fewShotJSON), &p.FewShotExamples)
		if p.FewShotExamples == nil {
			p.FewShotExamples = []FewShotExample{}
		}
		_ = json.Unmarshal([]byte(allowedJSON), &p.AllowedModels)
		if p.AllowedModels == nil {
			p.AllowedModels = []string{}
		}
		_ = json.Unmarshal([]byte(tgUsersJSON), &p.TelegramAllowedUsers)
		if p.TelegramAllowedUsers == nil {
			p.TelegramAllowedUsers = []string{}
		}
		p.TelegramEnabled = tgEnabledInt != 0
		p.Enabled = enabledInt != 0
		p.HasTelegramToken = strings.TrimSpace(rawToken) != ""
		if includeSecrets {
			p.TelegramBotToken = rawToken
		}
		out = append(out, p)
	}
	return out, nil
}

// Delete removes a Persona and detaches any sessions pointing to it.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return ErrPersonaNotFound
	}
	norm := CanonicalID(id)
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM personas WHERE id = ?`, norm)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrPersonaNotFound
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET persona_id = '' WHERE persona_id = ?`, norm)
	return nil
}

// IsModelAllowed checks whether a given provider::model is allowed by this Persona's scope rules.
func (p *Persona) IsModelAllowed(reg *provider.Registry, providerID, modelID string) bool {
	if p == nil || p.ModelScopeMode != "selected" {
		return true
	}
	normProv := provider.CanonicalID(providerID)
	normMod := strings.TrimSpace(modelID)
	if reg != nil {
		normMod = reg.NormalizeModelID(normProv, normMod)
	}
	target := normProv + "::" + normMod
	for _, allowed := range p.AllowedModels {
		if allowed == target {
			return true
		}
	}
	return false
}

// BuildCombinedSystemPrompt compiles the Persona's role instructions, voice blueprint, knowledge notes,
// and few-shot anchor dialogues into a clean system prompt for any LLM.
func (p *Persona) BuildCombinedSystemPrompt() string {
	if p == nil {
		return ""
	}
	var sections []string

	if sp := strings.TrimSpace(p.SystemPrompt); sp != "" {
		sections = append(sections, sp)
	}
	if vb := strings.TrimSpace(p.VoiceBlueprint); vb != "" {
		sections = append(sections, "=== VOICE, TONE & PERSONALITY BLUEPRINT ===\n"+vb)
	}
	if kn := strings.TrimSpace(p.KnowledgeNotes); kn != "" {
		sections = append(sections, "=== REFERENCE KNOWLEDGE & CONTEXT ===\n"+kn)
	}
	if len(p.FewShotExamples) > 0 {
		var exSB strings.Builder
		exSB.WriteString("=== STYLE ANCHOR EXAMPLES (MATCH THIS EXACT RHYTHM, PHRASING & TONE) ===\n")
		for i, ex := range p.FewShotExamples {
			fmt.Fprintf(&exSB, "Example %d:\nIncoming: %s\nReply: %s\n\n", i+1, ex.User, ex.Assistant)
		}
		sections = append(sections, strings.TrimSpace(exSB.String()))
	}
	return strings.Join(sections, "\n\n")
}
