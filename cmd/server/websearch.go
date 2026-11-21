package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
	"github.com/sahand363/sahand-cortext/internal/websearch"
)

// Web search (v0.10.0): every chat can search the web for free
// (Tavily when the owner set a key, then DuckDuckGo, Google News and Wikipedia). Each search leaves a short note in web_notes,
// which later turns reuse. See internal/websearch.

func newWebEngine(a *App) *websearch.Engine {
	return &websearch.Engine{Search: websearch.DefaultSearch(a.tavilyConfig()), Fetch: skill.FetchAndCleanURL}
}

// tavilyKey is the owner's Tavily key: Settings › Web search (stored
// encrypted), else the TAVILY_API_KEY environment variable.
func (a *App) tavilyKey() string {
	if v := strings.TrimSpace(a.setting(websearch.TavilySettingKey)); v != "" {
		return provider.DecryptAPIKey(v)
	}
	return strings.TrimSpace(os.Getenv("TAVILY_API_KEY"))
}

const tavilyUsageKey = "tavily_usage" // "YYYY-MM:count", searches this month

var tavilyUsageMu sync.Mutex

func (a *App) tavilyUsedThisMonth() int {
	month, n := splitUsage(a.setting(tavilyUsageKey))
	if month != time.Now().Format("2006-01") {
		return 0
	}
	return n
}

func splitUsage(v string) (string, int) {
	month, count, ok := strings.Cut(v, ":")
	if !ok {
		return "", 0
	}
	n, _ := strconv.Atoi(count)
	return month, n
}

func (a *App) tavilyConfig() websearch.TavilyConfig {
	return websearch.TavilyConfig{
		Key: a.tavilyKey,
		OnUse: func() {
			tavilyUsageMu.Lock()
			defer tavilyUsageMu.Unlock()
			now := time.Now().Format("2006-01")
			month, n := splitUsage(a.setting(tavilyUsageKey))
			if month != now {
				n = 0
			}
			_ = a.setSetting(tavilyUsageKey, fmt.Sprintf("%s:%d", now, n+1))
		},
	}
}

// keyHint shows only the start and the last 4 characters of a key.
func keyHint(k string) string {
	if len(k) <= 10 {
		return "••••"
	}
	return k[:5] + "…" + k[len(k)-4:]
}

func (a *App) tavilyStatus() map[string]any {
	k := a.tavilyKey()
	st := map[string]any{"set": k != "", "used_this_month": a.tavilyUsedThisMonth(), "from_env": false}
	if k != "" {
		st["hint"] = keyHint(k)
		st["from_env"] = strings.TrimSpace(a.setting(websearch.TavilySettingKey)) == ""
	}
	return st
}

// PUT    /api/websearch/tavily {key} -> tests the key with one search, then saves it encrypted
// DELETE /api/websearch/tavily       -> removes the saved key
func (a *App) handleTavilyKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		var body struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "invalid payload"})
			return
		}
		key := strings.TrimSpace(body.Key)
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			jsonOut(w, 400, map[string]string{"error": "Paste the Tavily key (it starts with tvly-)."})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		test := websearch.Tavily(websearch.TavilyConfig{Key: func() string { return key }, OnUse: a.tavilyConfig().OnUse})
		if _, err := test(ctx, "CorText connection test", 1); err != nil {
			jsonOut(w, 400, map[string]string{"error": "Tavily did not accept this key: " + err.Error()})
			return
		}
		enc, ok := provider.EncryptAPIKey(key)
		if !ok {
			enc = key
		}
		if err := a.setSetting(websearch.TavilySettingKey, enc); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true, "tavily": a.tavilyStatus()})
	case http.MethodDelete:
		if _, err := a.db.Exec(`DELETE FROM settings WHERE key=?`, websearch.TavilySettingKey); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true, "tavily": a.tavilyStatus()})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) webSearchMode() string { return websearch.NormalizeMode(a.setting(websearch.SettingKey)) }

const searchUsage = "🔎 Usage: /search <what to look up>\nExample: /search Go 1.24 release date\n\nIn auto mode (Settings › Web search) I also search on my own when a question needs fresh facts."

// runChatTurn calls the model for a web-panel or app chat turn, with web
// search: saved notes in the prompt, the model may ask for searches, and
// forcedQuery (from "/search") runs one first. emit streams the answer
// (nil = buffered); status reports progress (may be nil).
func (a *App) runChatTurn(ctx context.Context, t *chatTarget, msgs []provider.Message, userMessage, forcedQuery string, emit, status func(string)) (string, int64, error) {
	start := time.Now()
	key := a.providerKey(t.normProvider)
	call := func(ctx context.Context, m []provider.Message, onChunk func(string)) (string, error) {
		if onChunk == nil {
			out, _, err := a.router.Call(ctx, t.normProvider, key, t.chosenModel, m, provider.CallOpts{})
			return out, err
		}
		out, _, err := a.router.CallStream(ctx, t.normProvider, key, t.chosenModel, m, provider.CallOpts{}, onChunk)
		return out, err
	}
	if a.web == nil {
		a.web = newWebEngine(a)
	}
	var nb websearch.Notebook
	if a.webNotes != nil {
		nb = a.webNotes
	}
	if emit == nil {
		// Buffered chat: the engine still needs a non-nil stream hook to
		// know it may use the streaming call; keep it buffered instead.
		call0 := call
		call = func(ctx context.Context, m []provider.Message, _ func(string)) (string, error) {
			return call0(ctx, m, nil)
		}
	}
	answer, err := a.web.Run(ctx, websearch.Turn{
		Mode:        a.webSearchMode(),
		Notebook:    nb,
		Messages:    msgs,
		UserMessage: userMessage,
		ForcedQuery: forcedQuery,
		Call:        call,
		Emit:        emit,
		Status:      status,
	})
	return answer, time.Since(start).Milliseconds(), err
}

// GET  /api/websearch          -> {mode, notes, count}
// PUT  /api/websearch {mode}   -> saves the mode
// DELETE /api/websearch/notes  -> clears all notes
// DELETE /api/websearch/notes/<id>
func (a *App) handleWebSearch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		notes, err := a.webNotes.List(r.Context())
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if notes == nil {
			notes = []websearch.Note{}
		}
		size := 0
		for _, n := range notes {
			size += len(n.Query) + len(n.Summary)
			for _, s := range n.Sources {
				size += len(s.Title) + len(s.URL)
			}
		}
		jsonOut(w, 200, map[string]any{"mode": a.webSearchMode(), "notes": notes, "count": len(notes), "bytes": size, "max_notes": websearch.MaxOwnerNotes, "tavily": a.tavilyStatus()})
	case http.MethodPut, http.MethodPost:
		var body struct {
			Mode string `json:"mode"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			jsonOut(w, 400, map[string]string{"error": "invalid payload"})
			return
		}
		mode := websearch.NormalizeMode(body.Mode)
		if err := a.setSetting(websearch.SettingKey, mode); err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true, "mode": mode})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleWebNotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/websearch/notes"), "/")
	if id == "" {
		n, err := a.webNotes.Clear(r.Context())
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true, "deleted": n})
		return
	}
	if err := a.webNotes.Delete(r.Context(), id); err != nil {
		jsonOut(w, 404, map[string]string{"error": err.Error()})
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true})
}
