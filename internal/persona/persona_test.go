package persona

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

func openPersonaTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE sessions(
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT 'New conversation',
		pinned INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		t.Fatalf("create sessions table: %v", err)
	}
	return db
}

func TestPersonaStoreCRUDAndModelScope(t *testing.T) {
	db := openPersonaTestDB(t)
	store := NewStore(db)
	if err := store.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	reg := provider.NewRouter().Registry()

	saved, err := store.Save(context.Background(), reg, PersonaInput{
		ID:                   "sahand-clone",
		Name:                 "Sahand Digital Twin",
		Icon:                 "😎",
		Description:          "Talks like Sahand",
		SystemPrompt:         "Never act like an assistant.",
		KnowledgeNotes:       "Loves Go and self-hosted Linux servers.",
		VoiceBlueprint:       "Short, witty, mixes English and Persian naturally.",
		FewShotExamples:      []FewShotExample{{User: "چطوری؟", Assistant: "هستم داداش، درگیر سرورم 😅"}},
		DefaultProvider:      "openrouter",
		DefaultModel:         "deepseek/deepseek-chat",
		ModelScopeMode:       "selected",
		AllowedModels:        []string{"openrouter::deepseek/deepseek-chat", "gemini::gemini-2.0-flash"},
		TelegramEnabled:      true,
		TelegramBotToken:     "123456:ABC-DEF-secret",
		TelegramAllowedUsers: []string{"111222333", "444555666"},
	})
	if err != nil {
		t.Fatalf("Save persona: %v", err)
	}

	if saved.TelegramBotToken != "" || !saved.HasTelegramToken {
		t.Fatalf("expected token to be redacted in standard Get/Save, has_token=%v", saved.HasTelegramToken)
	}

	withSecret, err := store.GetWithSecrets(context.Background(), "sahand-clone")
	if err != nil || withSecret.TelegramBotToken != "123456:ABC-DEF-secret" {
		t.Fatalf("expected raw token in GetWithSecrets, got: %+v, err=%v", withSecret, err)
	}

	// Verify model allowlist enforcement
	if !saved.IsModelAllowed(reg, "openrouter", "deepseek/deepseek-chat") {
		t.Errorf("expected allowed model to pass")
	}
	if saved.IsModelAllowed(reg, "openrouter", "openai/gpt-4o") {
		t.Errorf("expected unallowed model to be rejected")
	}

	// Verify combined system prompt compilation
	combined := saved.BuildCombinedSystemPrompt()
	if !strings.Contains(combined, "VOICE, TONE & PERSONALITY BLUEPRINT") || !strings.Contains(combined, "چطوری؟") {
		t.Errorf("combined system prompt missing expected sections:\n%s", combined)
	}

	// Updating without passing a new token must preserve the existing secret token
	updated, err := store.Save(context.Background(), reg, PersonaInput{
		ID:             "sahand-clone",
		Name:           "Sahand Clone v2",
		ModelScopeMode: "all",
	})
	if err != nil || !updated.HasTelegramToken {
		t.Fatalf("expected existing telegram token to persist on edit, got: %+v, err=%v", updated, err)
	}

	if err := store.Delete(context.Background(), "sahand-clone"); err != nil {
		t.Fatalf("Delete persona: %v", err)
	}
}

func TestParseChatExportsAndDistillVoice(t *testing.T) {
	tgJSON := `{
		"name": "Chat with Ali",
		"messages": [
			{"id": 1, "type": "message", "from": "Ali", "text": "سلام داش سرور اوکی شد؟"},
			{"id": 2, "type": "message", "from": "Sahand", "text": "آره ردیفه، الان دارم تستش میکنم 🚀"},
			{"id": 3, "type": "message", "from": "Ali", "text": "ایول دمت گرم"},
			{"id": 4, "type": "message", "from": "Sahand", "text": "چاکریم"}
		]
	}`

	turns, format := ParseChatExport(tgJSON)
	if format != "telegram_json" || len(turns) != 4 {
		t.Fatalf("expected 4 telegram_json turns, got format=%s len=%d", format, len(turns))
	}

	pairs, matched := ExtractTargetPairs(turns, "Sahand", 10)
	if matched != 2 || len(pairs) != 2 {
		t.Fatalf("expected 2 matched pairs for Sahand, got matched=%d len=%d", matched, len(pairs))
	}
	if !strings.Contains(pairs[0].User, "سرور اوکی شد") || !strings.Contains(pairs[0].Assistant, "ردیفه") {
		t.Errorf("unexpected pair pairing: %+v", pairs[0])
	}

	// Test WhatsApp .txt format parsing
	waTxt := `11/10/2026, 8:15 PM - Friend: Are you coming tonight?
11/10/2026, 8:16 PM - Sahand: nah man coding all night lol`
	waTurns, waFormat := ParseChatExport(waTxt)
	if waFormat != "whatsapp_txt" || len(waTurns) != 2 {
		t.Fatalf("expected 2 whatsapp_txt turns, got format=%s len=%d", waFormat, len(waTurns))
	}

	// Test DistillVoicePersona with mock LLM server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"voice_blueprint\":\"You are Sahand. Keep replies short, witty, and mix casual Persian with tech slang.\",\"few_shot_examples\":[{\"user\":\"سرور اوکی شد؟\",\"assistant\":\"آره ردیفه 🚀\"}]}"}}]}`))
	}))
	defer ts.Close()

	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", ts.URL)

	res, err := DistillVoicePersona(context.Background(), router, func(string) string { return "sk-test" }, TrainRequest{
		TargetName: "Sahand",
		RawChatLog: tgJSON,
		Provider:   "openrouter",
		Model:      "test-model",
	})
	if err != nil {
		t.Fatalf("DistillVoicePersona: %v", err)
	}
	if !strings.Contains(res.VoiceBlueprint, "You are Sahand") || len(res.FewShotExamples) != 1 {
		t.Fatalf("unexpected TrainResult: %+v", res)
	}
}
