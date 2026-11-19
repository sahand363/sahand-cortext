package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/persona"
	_ "modernc.org/sqlite"
)

func (a *App) handlePersonas(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.personaStore.List(r.Context(), false)
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": "Failed loading chatbots: " + err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"personas": list})
	case http.MethodPost:
		var input persona.PersonaInput
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&input) != nil {
			jsonOut(w, 400, map[string]string{"error": "Invalid chatbot configuration payload"})
			return
		}
		saved, err := a.personaStore.Save(r.Context(), a.router.Registry(), input)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.personaStore.List(r.Context(), false)
		jsonOut(w, 201, map[string]any{
			"ok":       true,
			"persona":  saved,
			"personas": list,
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handlePersonaByID(w http.ResponseWriter, r *http.Request) {
	id := persona.CanonicalID(strings.TrimPrefix(r.URL.Path, "/api/personas/"))
	if id == "" {
		jsonOut(w, 400, map[string]string{"error": "Chatbot ID is required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := a.personaStore.Get(r.Context(), id)
		if err != nil {
			status := 500
			if errors.Is(err, persona.ErrPersonaNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, p)
	case http.MethodDelete:
		if err := a.personaStore.Delete(r.Context(), id); err != nil {
			status := 500
			if errors.Is(err, persona.ErrPersonaNotFound) {
				status = 404
			}
			jsonOut(w, status, map[string]string{"error": err.Error()})
			return
		}
		list, _ := a.personaStore.List(r.Context(), false)
		jsonOut(w, 200, map[string]any{
			"ok":       true,
			"deleted":  id,
			"personas": list,
		})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleTrainPersona(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req persona.TrainRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024)).Decode(&req) != nil {
		jsonOut(w, 400, map[string]string{"error": "Invalid training payload (max 4 MB)"})
		return
	}
	if strings.Contains(req.Model, "::") {
		parts := strings.SplitN(req.Model, "::", 2)
		req.Provider = parts[0]
		req.Model = parts[1]
	}
	if strings.TrimSpace(req.Provider) == "" {
		req.Provider = a.activeProvider()
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = a.activeModel()
	}
	if !a.isProviderConfigured(req.Provider) {
		jsonOut(w, 400, map[string]string{"error": "Selected trainer provider is not configured yet"})
		return
	}

	res, err := persona.DistillVoicePersona(r.Context(), a.router, a.providerKey, req)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": err.Error()})
		return
	}
	jsonOut(w, 200, res)
}
