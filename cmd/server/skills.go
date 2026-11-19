package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/skill"
	_ "modernc.org/sqlite"
)

func (a *App) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.skillStore.List(r.Context(), r.URL.Query().Get("persona_id"), false)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"skills": list})
	case http.MethodPost:
		var body skill.Skill
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid skill payload"})
			return
		}
		saved, err := a.skillStore.Save(r.Context(), body)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(saved.PersonaID) != "" {
			_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{strings.TrimSpace(saved.PersonaID)})
		} else {
			_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{skill.ScopeAll})
		}
		list, _ := a.skillStore.List(r.Context(), "", false)
		jsonOut(w, 201, map[string]any{"ok": true, "skill": saved, "skills": list})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleSkillByID(w http.ResponseWriter, r *http.Request) {
	id := skill.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/skills/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Skill ID is required"})
		return
	}
	if r.Method == http.MethodPut {
		var body struct {
			Enabled   *bool    `json:"enabled"`
			PersonaID *string  `json:"persona_id"`
			Global    *bool    `json:"global"`
			Scopes    []string `json:"scopes"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || (body.Enabled == nil && body.PersonaID == nil && body.Global == nil && body.Scopes == nil) {
			jsonOut(w, 400, map[string]string{"error": "enabled, persona_id, global or scopes is required"})
			return
		}
		existing, err := a.skillStore.Get(r.Context(), id)
		if err != nil {
			status := 500
			if errors.Is(err, skill.ErrSkillNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		if body.Enabled != nil {
			existing.Enabled = *body.Enabled
		}
		if body.PersonaID != nil {
			existing.PersonaID = strings.TrimSpace(*body.PersonaID)
		}
		// Scope changes: global ("all") or an explicit scope list (main chat,
		// bot persona ids). persona_id is kept as the primary scope so the
		// legacy global check (persona_id = '') stays consistent.
		var newScopes []string
		scopeChanged := false
		if body.Global != nil && *body.Global {
			existing.PersonaID = ""
			newScopes = []string{skill.ScopeAll}
			scopeChanged = true
		} else if body.Scopes != nil {
			newScopes = body.Scopes
			scopeChanged = true
			if len(newScopes) > 0 {
				existing.PersonaID = strings.TrimSpace(newScopes[0])
			} else if existing.PersonaID == "" {
				existing.PersonaID = skill.ScopeMain
			}
		}
		if _, err := a.skillStore.Save(r.Context(), *existing); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if scopeChanged {
			_ = a.skillStore.SetScopes(r.Context(), id, newScopes)
		}
		list, _ := a.skillStore.List(r.Context(), "", false)
		jsonOut(w, 200, map[string]any{"ok": true, "skill": existing, "skills": list})
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := a.skillStore.Delete(r.Context(), id); err != nil {
		status := 500
		if errors.Is(err, skill.ErrSkillNotFound) {
			status = 404
		}
		jsonOut(w, status, map[string]string{"error": err.Error()})
		return
	}
	list, _ := a.skillStore.List(r.Context(), "", false)
	jsonOut(w, 200, map[string]any{"ok": true, "deleted": id, "skills": list})
}
