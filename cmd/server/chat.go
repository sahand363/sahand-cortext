package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/chat"
	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
	_ "modernc.org/sqlite"
)

// chatTarget is everything a chat turn resolves before calling the
// model: the session, the active persona, and the provider/model.
// It is shared by the buffered sendMessage and the streaming mobile
// endpoint, so both resolve a chat the same way.
type chatTarget struct {
	sessionID       string
	sessTitle       string
	activePersonaID string
	activeBot       *persona.Persona
	normProvider    string
	chosenModel     string
	providerName    string
}

// chatError is a chat failure that maps to an HTTP status + message
// (sendMessage) or an SSE error event (the streaming endpoint).
type chatError struct {
	status  int
	message string
}

func (e *chatError) Error() string { return e.message }

// resolveChatTarget resolves a chat turn: the session, the active
// persona (the request's, else the session's stored one), and the
// provider/model (the request's, else the persona's / the hub's active
// defaults, with the Main Chat model-scope failover). It returns a
// chatError (with the HTTP status sendMessage would use) when the turn
// cannot run.
func (a *App) resolveChatTarget(ctx context.Context, sessionID, personaID, providerID, model string) (*chatTarget, *chatError) {
	var sessTitle string
	var sessionPersonaID string
	if a.db.QueryRow(`SELECT title, coalesce(persona_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&sessTitle, &sessionPersonaID) != nil {
		return nil, &chatError{status: 404, message: "session not found"}
	}

	activePersonaID := persona.CanonicalID(personaID)
	if activePersonaID == "" {
		activePersonaID = sessionPersonaID
	} else if activePersonaID != sessionPersonaID {
		_, _ = a.db.Exec(`UPDATE sessions SET persona_id=? WHERE id=?`, activePersonaID, sessionID)
	}

	var activeBot *persona.Persona
	if activePersonaID != "" {
		if bot, err := a.personaStore.Get(ctx, activePersonaID); err == nil {
			activeBot = bot
		}
	}

	normProvider := strings.TrimSpace(providerID)
	chosenModel := strings.TrimSpace(model)
	if strings.Contains(chosenModel, "::") {
		parts := strings.SplitN(chosenModel, "::", 2)
		normProvider = parts[0]
		chosenModel = parts[1]
	}
	if normProvider == "" && activeBot != nil && activeBot.DefaultProvider != "" {
		normProvider = activeBot.DefaultProvider
	}
	if normProvider == "" {
		normProvider = a.activeProvider()
	}
	normProvider = provider.CanonicalID(normProvider)

	p, _, regErr := a.router.Registry().Get(normProvider)
	if regErr != nil {
		return nil, &chatError{status: 400, message: regErr.Error()}
	}
	providerName := p.Metadata().Name
	if chosenModel == "" && activeBot != nil && activeBot.DefaultModel != "" && activeBot.DefaultProvider == normProvider {
		chosenModel = activeBot.DefaultModel
	}
	if chosenModel == "" {
		chosenModel = a.activeModel()
	}
	chosenModel = p.NormalizeModelID(chosenModel)

	if activeBot != nil && activeBot.ModelScopeMode == "selected" {
		if !activeBot.IsModelAllowed(a.router.Registry(), normProvider, chosenModel) {
			return nil, &chatError{status: 403, message: fmt.Sprintf("Model %s is not enabled for chatbot %q. Update its allowed models in Settings.", chosenModel, activeBot.Name)}
		}
	} else if !a.isModelAllowedForChat(normProvider, chosenModel) {
		if strings.TrimSpace(model) == "" {
			scope := a.loadChatModelScope()
			switched := false
			for _, entry := range scope.AllowedModels {
				parts := strings.SplitN(entry, "::", 2)
				if len(parts) == 2 && a.isProviderConfigured(parts[0]) {
					if candProv, _, err := a.router.Registry().Get(parts[0]); err == nil {
						normProvider = parts[0]
						chosenModel = parts[1]
						p = candProv
						providerName = candProv.Metadata().Name
						switched = true
						break
					}
				}
			}
			if !switched {
				return nil, &chatError{status: 403, message: "No allowed Main Chat model is currently configured. Update Main Chat model permissions in Settings."}
			}
		} else {
			return nil, &chatError{status: 403, message: "This model is not enabled for Main Chat. Update Main Chat model permissions in Settings."}
		}
	}

	if !a.isProviderConfigured(normProvider) {
		return nil, &chatError{status: 503, message: "Please configure " + providerName + " in Settings first."}
	}

	return &chatTarget{
		sessionID:       sessionID,
		sessTitle:       sessTitle,
		activePersonaID: activePersonaID,
		activeBot:       activeBot,
		normProvider:    normProvider,
		chosenModel:     chosenModel,
		providerName:    providerName,
	}, nil
}

// buildChatPrompt inserts the user's message, then builds the prompt
// (persona system prompt + skill block + memory block + history)
// exactly as sendMessage does. The skill/memory reads and the history
// rows are all drained and closed BEFORE this returns, so the caller
// may make a long model call afterwards — with a single SQLite
// connection the rows must be closed before the model is called, or
// the next DB write blocks forever.
func (a *App) buildChatPrompt(ctx context.Context, t *chatTarget, message string) ([]provider.Message, error) {
	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", message)
	if err != nil {
		return nil, err
	}

	var history []provider.Message
	var sysBuilder strings.Builder
	if t.activeBot != nil {
		sysBuilder.WriteString(t.activeBot.BuildCombinedSystemPrompt())
	}
	if a.skillStore != nil {
		// The main chat sees skills assigned to "main" plus the active bot's skills.
		skillScopes := []string{skill.ScopeMain}
		if t.activePersonaID != "" {
			skillScopes = append(skillScopes, t.activePersonaID)
		}
		sysBuilder.WriteString(a.skillStore.BuildSkillPromptBlock(ctx, skillScopes, message))
	}
	if a.memoryStore != nil {
		// Same scopes as skills: memories from the main chat plus the active
		// bot's memories. Runs BEFORE the history query below, like skills.
		memoryScopes := []string{memory.ScopeMain}
		if t.activePersonaID != "" {
			memoryScopes = append(memoryScopes, t.activePersonaID)
		}
		sysBuilder.WriteString(a.memoryStore.BuildMemoryPromptBlock(ctx, memoryScopes, message))
	}
	if combinedSys := strings.TrimSpace(sysBuilder.String()); combinedSys != "" {
		history = append(history, provider.Message{Role: "system", Content: combinedSys})
	}

	rows, err := a.db.Query(`SELECT role, content FROM messages WHERE session_id=? ORDER BY id ASC`, t.sessionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var role, content string
		if rows.Scan(&role, &content) == nil {
			history = append(history, provider.Message{Role: role, Content: content})
		}
	}
	rows.Close()

	return chat.TruncateHistory(history, chat.DefaultConfig()), nil
}

// finishChat records the model result and writes the assistant's reply
// to the DB exactly as sendMessage does (so history/memory stay correct).
func (a *App) finishChat(t *chatTarget, message, answer string, elapsed int64) error {
	a.saveTestRecord(t.normProvider, t.chosenModel, ModelTestRecord{Status: "working", LatencyMs: elapsed})
	if t.activeBot == nil {
		a.setActiveSelection(t.normProvider, t.chosenModel)
	}

	_, err := a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", answer)
	if err != nil {
		return err
	}

	var count int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, t.sessionID).Scan(&count)
	defaultBotTitle := ""
	if t.activeBot != nil {
		defaultBotTitle = t.activeBot.Icon + " " + t.activeBot.Name
	}
	if count <= 2 && (t.sessTitle == "" || t.sessTitle == "New conversation" || t.sessTitle == defaultBotTitle) {
		title := strings.TrimSpace(message)
		if len([]rune(title)) > 42 {
			title = string([]rune(title)[:42]) + "…"
		}
		if t.activeBot != nil {
			title = t.activeBot.Icon + " " + title
		}
		_, _ = a.db.Exec(`UPDATE sessions SET title=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, title, t.sessionID)
	} else {
		_, _ = a.db.Exec(`UPDATE sessions SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, t.sessionID)
	}
	return nil
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		PersonaID string `json:"persona_id"`
		Message   string `json:"message"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || strings.TrimSpace(body.Message) == "" || len(body.Message) > 30000 {
		jsonOut(w, 400, map[string]string{"error": "Invalid message text"})
		return
	}
	t, cerr := a.resolveChatTarget(r.Context(), body.SessionID, body.PersonaID, body.Provider, body.Model)
	if cerr != nil {
		jsonOut(w, cerr.status, map[string]string{"error": cerr.message})
		return
	}

	trimmedMsg := strings.TrimSpace(body.Message)

	// Handle staged /learn skill confirmation in Web Chat
	a.mu.Lock()
	staged, hasStaged := a.webStagedSkill[t.sessionID]
	a.mu.Unlock()

	if hasStaged && !strings.HasPrefix(trimmedMsg, "/") {
		lower := strings.ToLower(trimmedMsg)
		isFull := lower == "full" || strings.Contains(lower, "full") || lower == "کامل"
		isCompressed := lower == "compressed" || strings.Contains(lower, "compress") || lower == "فشرده" || lower == "خلاصه"
		isCancel := lower == "cancel" || lower == "لغو"

		if isCancel || isFull || isCompressed {
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)
			a.mu.Lock()
			delete(a.webStagedSkill, t.sessionID)
			a.mu.Unlock()

			var replyText string
			if isCancel {
				replyText = "✖ Cancelled saving skill."
			} else {
				mode := "compressed"
				chosenContent := staged.CompressedContent
				if isFull {
					mode = "full"
					chosenContent = staged.FullContent
				}
				// A skill learned in the main chat (no persona) belongs to the
				// main chat, not to everyone; one learned as a bot belongs to that bot.
				ownerID := t.activePersonaID
				if ownerID == "" {
					ownerID = skill.ScopeMain
				}
				saved, saveErr := a.skillStore.Save(r.Context(), skill.Skill{
					Name:        staged.Name,
					Description: staged.Description,
					SourceURL:   staged.SourceURL,
					StorageMode: mode,
					Content:     chosenContent,
					PersonaID:   ownerID,
					Enabled:     true,
				})
				if saveErr != nil {
					replyText = "⚠️ Failed to save skill: " + saveErr.Error()
				} else {
					_ = a.skillStore.SetScopes(r.Context(), saved.ID, []string{ownerID})
					replyText = fmt.Sprintf("✅ Saved skill \"%s\" (%s mode, %d chars) to database! It is now active across your workspace.", saved.Name, saved.StorageMode, len([]rune(saved.Content)))
				}
			}
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", replyText)
			jsonOut(w, 200, map[string]any{"reply": replyText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	// Handle /learn-deep <url> command in Web Chat: learns the page plus its
	// same-site links in the background and posts a summary into the chat.
	if strings.HasPrefix(strings.ToLower(trimmedMsg), "/learn-deep ") {
		parts := strings.Fields(trimmedMsg)
		if len(parts) >= 2 {
			targetURL := strings.TrimSpace(parts[1])
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)
			startText := fmt.Sprintf("🔍 Deep learning %s — visiting the page and its same-site links (up to 10 pages)… I'll post a summary here when it's done.", targetURL)
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", startText)
			sessID := t.sessionID
			provID, apiKey, modelID, personaID := t.normProvider, a.providerKey(t.normProvider), t.chosenModel, t.activePersonaID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer cancel()
				res := a.skillStore.LearnDeep(ctx, a.router, provID, apiKey, modelID, targetURL, personaID, 10, nil)
				var sb strings.Builder
				fmt.Fprintf(&sb, "📚 Deep learn finished for %s\n\nLearned %d skill(s):\n", targetURL, len(res.Learned))
				for _, name := range res.Learned {
					fmt.Fprintf(&sb, "• %s\n", name)
				}
				if len(res.Failed) > 0 {
					fmt.Fprintf(&sb, "\n⚠️ Could not learn %d page(s):\n", len(res.Failed))
					for _, u := range res.Failed {
						fmt.Fprintf(&sb, "• %s\n", u)
					}
				}
				_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, sessID, "assistant", sb.String())
			}()
			jsonOut(w, 200, map[string]any{"reply": startText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	// Handle /learn <url> command in Web Chat
	if strings.HasPrefix(strings.ToLower(trimmedMsg), "/learn ") {
		parts := strings.Fields(trimmedMsg)
		if len(parts) >= 2 {
			targetURL := strings.TrimSpace(parts[1])
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "user", body.Message)

			draft, exErr := skill.ExtractSkillFromURL(r.Context(), a.router, t.normProvider, a.providerKey(t.normProvider), t.chosenModel, targetURL, t.activePersonaID)
			var replyText string
			if exErr != nil {
				replyText = "⚠️ " + exErr.Error()
			} else {
				a.mu.Lock()
				a.webStagedSkill[t.sessionID] = draft
				a.mu.Unlock()
				replyText = fmt.Sprintf(
					"📚 Learned Skill: %s\n%s\n\nDo you want me to save the FULL version (~%d chars) or the COMPRESSED version (~%d chars, redundant info removed) to the database?\n\nReply with: Full or Compressed (or Cancel)",
					draft.Name, draft.Description, len([]rune(draft.FullContent)), len([]rune(draft.CompressedContent)),
				)
			}
			_, _ = a.db.Exec(`INSERT INTO messages(session_id,role,content) VALUES(?,?,?)`, t.sessionID, "assistant", replyText)
			jsonOut(w, 200, map[string]any{"reply": replyText, "provider": t.normProvider, "model": t.chosenModel, "persona_id": t.activePersonaID})
			return
		}
	}

	promptMessages, err := a.buildChatPrompt(r.Context(), t, body.Message)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	answer, elapsed, err := a.router.Call(r.Context(), t.normProvider, a.providerKey(t.normProvider), t.chosenModel, promptMessages, provider.CallOpts{})
	if err != nil {
		a.saveTestRecord(t.normProvider, t.chosenModel, ModelTestRecord{Status: "failed", LatencyMs: elapsed, LastError: err.Error()})
		jsonOut(w, 502, map[string]string{"error": fmt.Sprintf("[%s · %s] %s", t.providerName, t.chosenModel, err.Error())})
		return
	}

	if err := a.finishChat(t, body.Message, answer, elapsed); err != nil {
		jsonOut(w, 500, map[string]string{"error": "database error"})
		return
	}

	jsonOut(w, 200, map[string]any{
		"reply":      answer,
		"latency_ms": elapsed,
		"provider":   t.normProvider,
		"model":      t.chosenModel,
		"persona_id": t.activePersonaID,
	})
}
