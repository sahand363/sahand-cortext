package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/chat"
	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
)

type BridgeManager struct {
	db           *provider.DBTX // or *sql.DB
	dbConn       interface{}    // generic db handle
	personaStore *persona.Store
	router       *provider.Router
	keyLookup    func(string) string
	activePollers map[string]*botPoller
	mu           sync.Mutex
}

type botPoller struct {
	personaID   string
	botToken    string
	cancel      context.CancelFunc
}

func NewBridgeManager(db interface{}, personaStore *persona.Store, router *provider.Router, keyLookup func(string) string) *BridgeManager {
	return &BridgeManager{
		dbConn:        db,
		personaStore:  personaStore,
		router:        router,
		keyLookup:     keyLookup,
		activePollers: make(map[string]*botPoller),
	}
}

// StartSync scans SQLite for enabled personas with Telegram tokens and starts/stops background pollers.
func (b *BridgeManager) StartSync(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		defer ticker.Stop()
		b.syncPollers(ctx)
		for {
			select {
			case <-ctx.Done():
				b.StopAll()
				return
			case <-ticker.C:
				b.syncPollers(ctx)
			}
		}
	}()
}

func (b *BridgeManager) StopAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, p := range b.activePollers {
		p.cancel()
		delete(b.activePollers, id)
	}
}

func (b *BridgeManager) syncPollers(ctx context.Context) {
	personas, err := b.personaStore.List(ctx, true)
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	desired := map[string]string{}
	for _, p := range personas {
		if p.Enabled && p.TelegramEnabled && strings.TrimSpace(p.TelegramBotToken) != "" {
			desired[p.ID] = strings.TrimSpace(p.TelegramBotToken)
		}
	}

	// Stop pollers that were disabled or had tokens removed
	for id, poller := range b.activePollers {
		tok, ok := desired[id]
		if !ok || tok != poller.botToken {
			poller.cancel()
			delete(b.activePollers, id)
		}
	}

	// Start new pollers
	for id, tok := range desired {
		if _, exists := b.activePollers[id]; !exists {
			pCtx, pCancel := context.WithCancel(ctx)
			poller := &botPoller{personaID: id, botToken: tok, cancel: pCancel}
			b.activePollers[id] = poller
			go pollTelegramBot(pCtx, b, id, tok)
		}
	}
}

type tgUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		From      struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func pollTelegramBot(ctx context.Context, mgr *BridgeManager, personaID, token string) {
	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}
	apiBase := fmt.Sprintf("https://api.telegram.org/bot%s", token)

	log.Printf("[Telegram] Started polling worker for chatbot persona %q", personaID)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		url := fmt.Sprintf("%s/getUpdates?offset=%d&timeout=30", apiBase, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var res struct {
			Ok     bool     `json:"ok"`
			Result []tgUpdate `json:"result"`
		}
		if json.Unmarshal(body, &res) != nil || !res.Ok {
			time.Sleep(5 * time.Second)
			continue
		}

		for _, up := range res.Result {
			offset = up.UpdateID + 1
			if up.Message == nil || strings.TrimSpace(up.Message.Text) == "" {
				continue
			}

			// Verify User ID allowlist
			p, err := mgr.personaStore.Get(ctx, personaID)
			if err != nil || !p.Enabled {
				continue
			}

			if len(p.TelegramAllowedUsers) > 0 {
				senderIDStr := strconv.FormatInt(up.Message.From.ID, 10)
				allowed := false
				for _, allowedID := range p.TelegramAllowedUsers {
					if strings.TrimSpace(allowedID) == senderIDStr {
						allowed = true
						break
					}
				}
				if !allowed {
					sendTelegramMessage(client, apiBase, up.Message.Chat.ID, "⛔ Access denied: Your Telegram User ID is not authorized for this bot.")
					continue
				}
			}

			mgr.handleIncomingMessage(ctx, client, apiBase, p, up.Message.Chat.ID, up.Message.From.ID, up.Message.Text)
		}
	}
}

func sendTelegramMessage(client *http.Client, apiBase string, chatID int64, text string) {
	url := apiBase + "/sendMessage"
	payload, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
		"parse_mode": "Markdown",
	})
	_, _ = client.Post(url, "application/json", bytes.NewReader(payload))
}

func (b *BridgeManager) handleIncomingMessage(
	ctx context.Context,
	client *http.Client,
	apiBase string,
	p *persona.Persona,
	chatID int64,
	userID int64,
	text string,
) {
	db, ok := b.dbConn.(*sql.DB)
	if !ok {
		return
	}

	trimmedText := strings.TrimSpace(text)

	// Handle commands
	if strings.HasPrefix(trimmedText, "/") {
		parts := strings.Fields(trimmedText)
		cmd := strings.ToLower(parts[0])
		if idx := strings.Index(cmd, "@"); idx != -1 {
			cmd = cmd[:idx]
		}

		switch cmd {
		case "/start", "/help":
			msg := fmt.Sprintf("🤖 *%s* %s\n\nCommands:\n/new - Start a fresh conversation\n/model - View current model\n/status - Check bot status", p.Icon, p.Name)
			sendTelegramMessage(client, apiBase, chatID, msg)
			return
		case "/new":
			sessID := fmt.Sprintf("tg-%d-%d", chatID, time.Now().Unix())
			_, _ = db.Exec(`INSERT INTO sessions(id, title, persona_id, pinned) VALUES(?, ?, ?, 0)`, sessID, fmt.Sprintf("%s Telegram Chat", p.Icon), p.ID)
			sendTelegramMessage(client, apiBase, chatID, "✨ Started a fresh conversation!")
			return
		case "/status":
			mod := p.DefaultModel
			if mod == "" {
				mod = "Default Workspace Model"
			}
			sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("🟢 *%s* is online\nModel: `%s`", p.Name, mod))
			return
		}
	}

	// Resolve or create persistent Telegram chat session per user chat ID
	sessID := fmt.Sprintf("tg-%d", chatID)
	var existingPersona string
	err := db.QueryRow(`SELECT coalesce(persona_id,'') FROM sessions WHERE id = ?`, sessID).Scan(&existingPersona)
	if err != nil {
		_, _ = db.Exec(`INSERT INTO sessions(id, title, persona_id, pinned) VALUES(?, ?, ?, 0)`,
			sessID, fmt.Sprintf("%s %s", p.Icon, p.Name), p.ID)
	}

	// Save user message
	_, _ = db.Exec(`INSERT INTO messages(session_id, role, content) VALUES(?, 'user', ?)`, sessID, trimmedText)

	// Load chat history
	rows, err := db.Query(`SELECT role, content FROM messages WHERE session_id = ? ORDER BY id ASC`, sessID)
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Database error loading history.")
		return
	}
	defer rows.Close()

	var history []provider.Message
	if sysPrompt := p.BuildCombinedSystemPrompt(); sysPrompt != "" {
		history = append(history, provider.Message{Role: "system", Content: sysPrompt})
	}
	for rows.Next() {
		var role, content string
		if rows.Scan(&role, &content) == nil {
			history = append(history, provider.Message{Role: role, Content: content})
		}
	}
	promptMessages := chat.TruncateHistory(history, chat.DefaultConfig())

	prov := p.DefaultProvider
	if prov == "" {
		if v := b.setting("active_provider"); v != "" {
			prov = v
		} else {
			prov = "gemini"
		}
	}
	prov = provider.CanonicalID(prov)

	regProv, _, err := b.router.Registry().Get(prov)
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Invalid provider configuration.")
		return
	}

	mod := p.DefaultModel
	if mod == "" {
		if v := b.setting("active_model"); v != "" {
			mod = v
		} else {
			mod = regProv.Metadata().DefaultModel
		}
	}
	mod = regProv.NormalizeModelID(mod)

	// Enforce persona model allowlist
	if p.ModelScopeMode == "selected" && !p.IsModelAllowed(b.router.Registry(), prov, mod) {
		if len(p.AllowedModels) > 0 {
			parts := strings.SplitN(p.AllowedModels[0], "::", 2)
			if len(parts) == 2 {
				prov = parts[0]
				mod = parts[1]
			}
		}
	}

	apiKey := b.keyLookup(prov)
	answer, _, err := b.router.Call(ctx, prov, apiKey, mod, promptMessages, provider.CallOpts{})
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("⚠️ AI error: %s", err.Error()))
		return
	}

	// Save assistant reply
	_, _ = db.Exec(`INSERT INTO messages(session_id, role, content) VALUES(?, 'assistant', ?)`, sessID, answer)
	_, _ = db.Exec(`UPDATE sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, sessID)

	sendTelegramMessage(client, apiBase, chatID, answer)
}

func (b *BridgeManager) setting(key string) string {
	db, ok := b.dbConn.(*sql.DB)
	if !ok {
		return ""
	}
	var v string
	_ = db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}
