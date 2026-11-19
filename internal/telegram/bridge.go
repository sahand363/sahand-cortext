package telegram

import (
	"bytes"
	"context"
	"database/sql"
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
	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
)

type BridgeManager struct {
	dbConn        interface{}
	personaStore  *persona.Store
	skillStore    *skill.Store
	memoryStore   *memory.Store
	router        *provider.Router
	keyLookup     func(string) string
	activePollers map[string]*botPoller
	stagedSkills  map[int64]*skill.StagedDraft // keyed by Telegram chatID
	mu            sync.Mutex

	private    *privateStore   // encrypted per-chat history and memory
	extracting map[string]bool // chats with a memory extraction running
}

type botPoller struct {
	personaID string
	botToken  string
	cancel    context.CancelFunc
}

func NewBridgeManager(db interface{}, personaStore *persona.Store, skillStore *skill.Store, memoryStore *memory.Store, router *provider.Router, keyLookup func(string) string) *BridgeManager {
	return &BridgeManager{
		dbConn:        db,
		personaStore:  personaStore,
		skillStore:    skillStore,
		memoryStore:   memoryStore,
		router:        router,
		keyLookup:     keyLookup,
		activePollers: make(map[string]*botPoller),
		stagedSkills:  make(map[int64]*skill.StagedDraft),
		extracting:    make(map[string]bool),
	}
}

// EnablePrivateChats turns on encrypted storage for Telegram chats, keyed by
// a secret kept in dataDir, and moves any plaintext chats from older
// versions into it. Must be called before StartSync.
func (b *BridgeManager) EnablePrivateChats(dataDir string) error {
	db, ok := b.dbConn.(*sql.DB)
	if !ok {
		return fmt.Errorf("telegram: no database")
	}
	ps, err := newPrivateStore(db, dataDir)
	if err != nil {
		return err
	}
	b.private = ps
	if n, err := ps.migratePlaintext(); err != nil {
		log.Printf("[Telegram] Private chat migration incomplete: %v", err)
	} else if n > 0 {
		log.Printf("[Telegram] Encrypted %d Telegram chat(s) and removed their plaintext copies", n)
	}
	if n, err := ps.migrateTelegramMemories(); err != nil {
		log.Printf("[Telegram] Moving Telegram memories to private storage failed: %v", err)
	} else if n > 0 {
		log.Printf("[Telegram] Moved %d Telegram-learned fact(s) out of the Memories page into private storage", n)
	}
	return nil
}

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

	for id, poller := range b.activePollers {
		tok, ok := desired[id]
		if !ok || tok != poller.botToken {
			poller.cancel()
			delete(b.activePollers, id)
		}
	}

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
			Ok     bool       `json:"ok"`
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

			mgr.handleIncomingMessage(ctx, client, apiBase, p, up.Message.Chat.ID, up.Message.From.ID, personLabel(up.Message.From.FirstName, up.Message.From.Username), up.Message.Text)
		}
	}
}

// runDeepLearn learns a page and its same-site links in the background and
// reports progress to the Telegram chat. It runs in its own goroutine so the
// poll loop is never blocked by the (slow) fetch + AI calls.
func (b *BridgeManager) runDeepLearn(client *http.Client, apiBase string, chatID int64, p *persona.Persona, prov, mod, targetURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	res := b.skillStore.LearnDeep(ctx, b.router, prov, b.keyLookup(prov), mod, targetURL, p.ID, 10, func(done int, pageURL, skillName string) {
		if skillName == "" {
			sendTelegramMessage(client, apiBase, chatID, "⚠️ Could not learn: "+pageURL)
		} else {
			sendTelegramMessage(client, apiBase, chatID, "✅ Learned: "+skillName)
		}
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "📚 Deep learn finished for %s\n\nLearned %d skill(s).", targetURL, len(res.Learned))
	if len(res.Failed) > 0 {
		fmt.Fprintf(&sb, "\n\n⚠️ Could not learn %d page(s):\n", len(res.Failed))
		for _, u := range res.Failed {
			fmt.Fprintf(&sb, "• %s\n", u)
		}
	}
	sendTelegramMessage(client, apiBase, chatID, sb.String())
}

// isPersianRune reports whether r is a Persian/Arabic-script letter.
func isPersianRune(r rune) bool {
	return (r >= 0x0600 && r <= 0x06FF) || (r >= 0xFB50 && r <= 0xFDFF) || (r >= 0xFE70 && r <= 0xFEFF)
}

func isLatinLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// rtlWrap forces a Telegram message to render right-to-left when it is
// dominantly Persian/Arabic. Telegram has no dir="auto" (the web chat already
// uses dir="auto" on its bubbles), so without this, mixed Persian + English
// text (URLs, skill names, numbers) comes out scrambled. The message is
// wrapped in a Right-to-Left Isolate so embedded English words and URLs keep
// their own left-to-right order inside the RTL flow.
func rtlWrap(text string) string {
	persian, latin := 0, 0
	for _, r := range text {
		if isPersianRune(r) {
			persian++
		} else if isLatinLetter(r) {
			latin++
		}
	}
	if persian > 0 && persian >= latin {
		return "\u2067" + text + "\u2069"
	}
	return text
}

func sendTelegramMessage(client *http.Client, apiBase string, chatID int64, text string) {
	url := apiBase + "/sendMessage"
	payload, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    rtlWrap(text),
	})
	_, _ = client.Post(url, "application/json", bytes.NewReader(payload))
}

// ResolveProviderAndModel picks the provider and model a persona answers with.
func (b *BridgeManager) ResolveProviderAndModel(p *persona.Persona) (string, string, error) {
	return b.resolveProviderAndModel(p)
}

func (b *BridgeManager) resolveProviderAndModel(p *persona.Persona) (string, string, error) {
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
		return "", "", err
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

	if p.ModelScopeMode == "selected" && !p.IsModelAllowed(b.router.Registry(), prov, mod) {
		if len(p.AllowedModels) > 0 {
			parts := strings.SplitN(p.AllowedModels[0], "::", 2)
			if len(parts) == 2 {
				prov = parts[0]
				mod = parts[1]
			}
		}
	}
	return prov, mod, nil
}

func (b *BridgeManager) handleIncomingMessage(
	ctx context.Context,
	client *http.Client,
	apiBase string,
	p *persona.Persona,
	chatID int64,
	userID int64,
	who string,
	text string,
) {

	trimmedText := strings.TrimSpace(text)

	// 1. Check if there is a pending /learn skill waiting for "full" or "compressed" confirmation
	b.mu.Lock()
	staged, hasStaged := b.stagedSkills[chatID]
	b.mu.Unlock()

	if hasStaged && !strings.HasPrefix(trimmedText, "/") {
		lower := strings.ToLower(trimmedText)
		isFull := lower == "full" || strings.Contains(lower, "full") || lower == "کامل"
		isCompressed := lower == "compressed" || strings.Contains(lower, "compress") || lower == "فشرده" || lower == "خلاصه"
		isCancel := lower == "cancel" || lower == "لغو"

		if isCancel {
			b.mu.Lock()
			delete(b.stagedSkills, chatID)
			b.mu.Unlock()
			sendTelegramMessage(client, apiBase, chatID, "✖ Cancelled saving skill.")
			return
		}

		if isFull || isCompressed {
			mode := "compressed"
			chosenContent := staged.CompressedContent
			if isFull {
				mode = "full"
				chosenContent = staged.FullContent
			}

			saved, err := b.skillStore.Save(ctx, skill.Skill{
				Name:        staged.Name,
				Description: staged.Description,
				SourceURL:   staged.SourceURL,
				StorageMode: mode,
				Content:     chosenContent,
				PersonaID:   staged.PersonaID,
				Enabled:     true,
			})
			b.mu.Lock()
			delete(b.stagedSkills, chatID)
			b.mu.Unlock()

			if err != nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Failed to save skill: "+err.Error())
				return
			}
			// A skill learned by this bot belongs to this bot (only it, until
			// the owner ticks more places in the Skills page).
			_ = b.skillStore.SetScopes(ctx, saved.ID, []string{p.ID})
			sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("✅ Saved skill \"%s\" (%s mode, %d chars) to database!\n\nIt is now active in my knowledge base—continue chatting whenever you're ready.", saved.Name, saved.StorageMode, len([]rune(saved.Content))))
			return
		}
	}

	// 2. Handle Slash Commands
	if strings.HasPrefix(trimmedText, "/") {
		parts := strings.Fields(trimmedText)
		cmd := strings.ToLower(parts[0])
		if idx := strings.Index(cmd, "@"); idx != -1 {
			cmd = cmd[:idx]
		}

		switch cmd {
		case "/start", "/help":
			msg := fmt.Sprintf("🤖 %s %s\n\nCommands:\n/learn <url> - Visit a link, extract a skill, and save it to DB\n/learn-deep <url> - Learn a page AND its same-site links (up to 10 pages)\n/skills - List active skills in my knowledge base\n/new - Start a fresh conversation\n/memory - What I remember about you\n/forget - Erase what I remember about you\n/status - Check bot status\n\n🔒 Our chat is stored encrypted. Not even the bot's owner can read it.", p.Icon, p.Name)
			sendTelegramMessage(client, apiBase, chatID, msg)
			return

		case "/learn":
			if len(parts) < 2 {
				sendTelegramMessage(client, apiBase, chatID, "Usage: /learn https://example.com/guide")
				return
			}
			targetURL := strings.TrimSpace(parts[1])
			prov, mod, err := b.resolveProviderAndModel(p)
			if err != nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Invalid provider configuration.")
				return
			}
			sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("🌐 Visiting %s and extracting skill with %s…", targetURL, mod))

			draft, err := skill.ExtractSkillFromURL(ctx, b.router, prov, b.keyLookup(prov), mod, targetURL, p.ID)
			if err != nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ "+err.Error())
				return
			}

			b.mu.Lock()
			b.stagedSkills[chatID] = draft
			b.mu.Unlock()

			fullChars := len([]rune(draft.FullContent))
			compChars := len([]rune(draft.CompressedContent))
			promptMsg := fmt.Sprintf(
				"📚 Learned Skill: %s\n%s\n\nDo you want me to save the FULL version (~%d chars) or the COMPRESSED version (~%d chars, redundant info removed) to the database?\n\nReply with: Full or Compressed (or Cancel)",
				draft.Name, draft.Description, fullChars, compChars,
			)
			sendTelegramMessage(client, apiBase, chatID, promptMsg)
			return

		case "/learn-deep":
			if len(parts) < 2 {
				sendTelegramMessage(client, apiBase, chatID, "Usage: /learn-deep https://example.com/guide")
				return
			}
			targetURL := strings.TrimSpace(parts[1])
			prov, mod, err := b.resolveProviderAndModel(p)
			if err != nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Invalid provider configuration.")
				return
			}
			sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("🔍 Deep learning %s — visiting the page and its same-site links (up to 10 pages)…", targetURL))
			go b.runDeepLearn(client, apiBase, chatID, p, prov, mod, targetURL)
			return

		case "/skills":
			skills, err := b.skillStore.List(ctx, p.ID, true)
			if err != nil || len(skills) == 0 {
				sendTelegramMessage(client, apiBase, chatID, "📭 No skills saved yet. Use /learn <url> to teach me from any link!")
				return
			}
			var sb strings.Builder
			sb.WriteString("🧠 Active Skills in Knowledge Base:\n\n")
			for i, sk := range skills {
				fmt.Fprintf(&sb, "%d. %s [%s]\n   %s\n", i+1, sk.Name, sk.StorageMode, sk.Description)
			}
			sendTelegramMessage(client, apiBase, chatID, sb.String())
			return

		case "/new":
			if b.private == nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Private storage is unavailable on the server.")
				return
			}
			_ = b.private.clearHistory(b.private.ref(p.ID, chatID))
			sendTelegramMessage(client, apiBase, chatID, "✨ Started a fresh conversation! I still remember what you've told me about yourself; /forget erases that.")
			return

		case "/memory":
			if b.private == nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Private storage is unavailable on the server.")
				return
			}
			facts, _ := b.private.facts(b.private.ref(p.ID, chatID))
			if len(facts) == 0 {
				sendTelegramMessage(client, apiBase, chatID, "🧠 I don't remember anything about you yet. I learn as we chat.")
				return
			}
			var sb strings.Builder
			sb.WriteString("🧠 What I remember about you (only visible in this chat):\n\n")
			for _, f := range facts {
				sb.WriteString("• " + f + "\n")
			}
			sb.WriteString("\n/forget erases all of it.")
			sendTelegramMessage(client, apiBase, chatID, sb.String())
			return

		case "/forget":
			if b.private == nil {
				sendTelegramMessage(client, apiBase, chatID, "⚠️ Private storage is unavailable on the server.")
				return
			}
			r := b.private.ref(p.ID, chatID)
			_ = b.private.forget(r)
			_ = b.private.clearHistory(r)
			sendTelegramMessage(client, apiBase, chatID, "🧹 Done. I've erased everything I remembered about you, and our chat history.")
			return

		case "/status":
			_, mod, _ := b.resolveProviderAndModel(p)
			sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("🟢 %s is online\nModel: %s", p.Name, mod))
			return
		}
	}

	// 3. Normal Chat Flow: encrypted history + what the bot knows about this person
	if b.private == nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Private storage is unavailable on the server, so I can't keep our chat safe right now.")
		return
	}
	ref := b.private.ref(p.ID, chatID)

	if err := b.private.appendMessage(nil, ref, "user", trimmedText); err != nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Could not save your message securely.")
		return
	}
	history, _, err := b.private.history(ref, 0)
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Database error loading history.")
		return
	}

	sysPrompt := p.BuildCombinedSystemPrompt()
	if b.skillStore != nil {
		sysPrompt += b.skillStore.BuildSkillPromptBlock(ctx, []string{p.ID}, trimmedText)
	}
	if b.memoryStore != nil {
		sysPrompt += b.memoryStore.BuildMemoryPromptBlock(ctx, []string{p.ID}, trimmedText)
	}
	sysPrompt += personPromptBlock(who, b.privateFacts(ref))
	history = append([]provider.Message{{Role: "system", Content: sysPrompt}}, history...)
	promptMessages := chat.TruncateHistory(history, chat.DefaultConfig())

	prov, mod, err := b.resolveProviderAndModel(p)
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, "⚠️ Invalid provider configuration.")
		return
	}

	apiKey := b.keyLookup(prov)
	answer, _, err := b.router.Call(ctx, prov, apiKey, mod, promptMessages, provider.CallOpts{})
	if err != nil {
		sendTelegramMessage(client, apiBase, chatID, fmt.Sprintf("⚠️ AI error: %s", err.Error()))
		return
	}

	_ = b.private.appendMessage(nil, ref, "assistant", answer)
	sendTelegramMessage(client, apiBase, chatID, answer)

	go b.learnAboutPerson(ref, prov, apiKey, mod)
}

func (b *BridgeManager) privateFacts(r chatRef) []string {
	facts, _ := b.private.facts(r)
	return facts
}

// personLabel names the Telegram user for the model ("Sara (@sara_k)").
func personLabel(firstName, username string) string {
	firstName = strings.TrimSpace(firstName)
	username = strings.TrimSpace(username)
	switch {
	case firstName != "" && username != "":
		return fmt.Sprintf("%s (@%s)", firstName, username)
	case firstName != "":
		return firstName
	case username != "":
		return "@" + username
	}
	return ""
}

func personPromptBlock(who string, facts []string) string {
	var sb strings.Builder
	sb.WriteString("\n\n## The person you are chatting with\n")
	if who != "" {
		sb.WriteString("Telegram name: " + who + "\n")
	}
	if len(facts) == 0 {
		sb.WriteString("You don't know anything else about them yet. Learn about them naturally as you talk.\n")
		return sb.String()
	}
	sb.WriteString("What you remember about them from earlier conversations (use it naturally, don't recite it):\n")
	for _, f := range facts {
		sb.WriteString("- " + f + "\n")
	}
	return sb.String()
}

// learnEvery is how many new messages (both sides) trigger a memory update.
const learnEvery = 6

// learnAboutPerson extracts durable facts about the person from messages not
// yet processed and saves them, encrypted, to this chat's memory. Runs in the
// background after a reply; one extraction per chat at a time.
func (b *BridgeManager) learnAboutPerson(ref chatRef, prov, apiKey, mod string) {
	b.mu.Lock()
	if b.extracting[ref.sid] {
		b.mu.Unlock()
		return
	}
	b.extracting[ref.sid] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.extracting, ref.sid)
		b.mu.Unlock()
	}()

	facts, upto := b.private.facts(ref)
	msgs, last, err := b.private.history(ref, upto)
	if err != nil || len(msgs) < learnEvery {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	newFacts, err := memory.ExtractFromMessages(ctx, b.router, apiKey, prov, mod, facts, msgs)
	if err != nil {
		log.Printf("[Telegram] Memory update skipped: %v", err)
		return
	}
	_ = b.private.saveFacts(ref, append(facts, newFacts...), last)
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
