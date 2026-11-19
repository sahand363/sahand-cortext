package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	_ "modernc.org/sqlite"
)

type fakeTG struct {
	mu    sync.Mutex
	calls []string
	sent  []map[string]any
	next  int64
}

func (f *fakeTG) handler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	method := parts[len(parts)-1]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "getMe":
		io.WriteString(w, `{"ok":true,"result":{"id":42,"is_bot":true,"username":"capa_bot"}}`)
	case "sendMessage":
		f.next++
		f.sent = append(f.sent, body)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": f.next}})
	default:
		io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func setup(t *testing.T) (*Service, *fakeTG, *[]string) {
	t.Helper()
	var prompts []string
	var pmu sync.Mutex
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []provider.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		prompts = append(prompts, req.Messages[0].Content)
		pmu.Unlock()
		reply := "Post: Big news about AI agents 🚀"
		if len(req.Messages) > 2 {
			reply = "Shorter post about AI agents"
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}})
	}))
	t.Cleanup(llm.Close)
	tg := &fakeTG{}
	tgs := httptest.NewServer(http.HandlerFunc(tg.handler))
	t.Cleanup(tgs.Close)

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, persona_id TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	ps := persona.NewStore(db)
	if err := ps.InitSchema(); err != nil {
		t.Fatal(err)
	}
	st := NewStore(db)
	if err := st.InitSchema(); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter()
	_ = router.SetProviderBaseURL("openrouter", llm.URL)
	svc := NewService(st, ps, router, func(string) string { return "sk-test" }, func(*persona.Persona) (string, string, error) { return "openrouter", "m", nil })
	svc.apiBase = tgs.URL
	return svc, tg, &prompts
}

func TestNormalizeChatRef(t *testing.T) {
	for in, want := range map[string]string{
		"t.me/aicapa300": "@aicapa300", "https://t.me/s/aicapa300": "@aicapa300", "@aicapa300": "@aicapa300",
		"aicapa300": "@aicapa300", "-1001234": "-1001234", "https://t.me/aicapa300/5": "@aicapa300",
	} {
		if got := NormalizeChatRef(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestDraftApprovePublishFlow(t *testing.T) {
	svc, tg, prompts := setup(t)
	ctx := context.Background()
	c := &Channel{Name: "AI CAPA", ChatRef: "@aicapa300", Enabled: true, Instructions: "Always end with #AI", Examples: []string{}}
	if err := svc.Store.Save(ctx, c, "123:ABC"); err != nil {
		t.Fatal(err)
	}
	c, _ = svc.Store.Get(ctx, c.ID)
	if c.token != "123:ABC" || !c.HasToken || c.PairCode == "" {
		t.Fatalf("token/pair: %+v", c)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "123:ABC") {
		t.Fatal("token leaked into JSON")
	}

	post, err := svc.Generate(ctx, c.ID, "AI agents", "they are getting better")
	if err != nil {
		t.Fatal(err)
	}
	if post.Draft != "Big news about AI agents 🚀" || post.Status != StatusDraft {
		t.Fatalf("draft: %+v", post)
	}
	if !strings.Contains((*prompts)[0], "Always end with #AI") {
		t.Fatal("standing instructions missing from prompt")
	}

	// Approval needs a paired owner.
	if _, err := svc.SendForApproval(ctx, post.ID); err == nil || !strings.Contains(err.Error(), c.PairCode) {
		t.Fatalf("expected pairing error, got %v", err)
	}
	// Wrong code is refused, right code pairs.
	svc.handle(ctx, c, mkMsg(7, "/start WRONG"))
	if c2, _ := svc.Store.Get(ctx, c.ID); c2.OwnerID != 0 {
		t.Fatal("paired with a wrong code")
	}
	svc.handle(ctx, c, mkMsg(7, "/start "+strings.ToLower(c.PairCode)))
	c, _ = svc.Store.Get(ctx, c.ID)
	if c.OwnerID != 7 {
		t.Fatal("pairing failed")
	}

	if _, err := svc.SendForApproval(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	post, _ = svc.Store.GetPost(ctx, post.ID)
	if post.Status != StatusPending || post.approvalMsg == 0 {
		t.Fatalf("pending: %+v", post)
	}

	// A stranger can't press the buttons.
	svc.handle(ctx, c, mkCallback(99, "cp:post:"+post.ID, post.approvalMsg))
	if p, _ := svc.Store.GetPost(ctx, post.ID); p.Status != StatusPending {
		t.Fatal("stranger published")
	}

	// Revise from Telegram: tap, then send notes.
	svc.handle(ctx, c, mkCallback(7, "cp:rev:"+post.ID, post.approvalMsg))
	if p, _ := svc.Store.AwaitingRevision(ctx, c.ID); p == nil {
		t.Fatal("not awaiting revision")
	}
	if _, err := svc.Regenerate(ctx, post.ID, "make it shorter"); err != nil {
		t.Fatal(err)
	}
	post, _ = svc.Store.GetPost(ctx, post.ID)
	if post.Draft != "Shorter post about AI agents" || post.Status != StatusPending || post.awaitingRev {
		t.Fatalf("revised: %+v", post)
	}
	oldApproval := int64(1)
	// The old approval message's buttons are dead.
	svc.handle(ctx, c, mkCallback(7, "cp:post:"+post.ID, oldApproval))
	if p, _ := svc.Store.GetPost(ctx, post.ID); p.Status == StatusPosted && oldApproval != p.approvalMsg {
		t.Fatal("stale button published")
	}

	// Owner edits by hand, then approves in Telegram.
	if _, err := svc.EditDraft(ctx, post.ID, "My own words about AI agents, edited by hand #AI"); err != nil {
		t.Fatal(err)
	}
	post, _ = svc.Store.GetPost(ctx, post.ID)
	svc.handle(ctx, c, mkCallback(7, "cp:post:"+post.ID, post.approvalMsg))
	post, _ = svc.Store.GetPost(ctx, post.ID)
	if post.Status != StatusPosted || post.TGMessage == 0 {
		t.Fatalf("not posted: %+v", post)
	}
	tg.mu.Lock()
	last := tg.sent[len(tg.sent)-2] // the channel post, then "✅ Posted" to owner
	tg.mu.Unlock()
	if last["chat_id"] != "@aicapa300" || last["text"] != "My own words about AI agents, edited by hand #AI" {
		t.Fatalf("channel post: %v", last)
	}

	// The published post becomes an example and the edit becomes a lesson.
	c, _ = svc.Store.Get(ctx, c.ID)
	if len(c.Examples) != 1 {
		t.Fatalf("examples: %v", c.Examples)
	}
	if _, err := svc.Generate(ctx, c.ID, "next", ""); err != nil {
		t.Fatal(err)
	}
	lastPrompt := (*prompts)[len(*prompts)-1]
	if !strings.Contains(lastPrompt, "What the owner published") || !strings.Contains(lastPrompt, "REAL POSTS") {
		t.Fatal("lessons/examples not in prompt")
	}

	// Our own channel post isn't re-learned; the owner's manual one is.
	svc.handle(ctx, c, mkChannelPost(post.TGMessage, "My own words about AI agents, edited by hand #AI but different"))
	svc.handle(ctx, c, mkChannelPost(500, "A post the owner wrote by hand in the Telegram app, long enough to learn."))
	c, _ = svc.Store.Get(ctx, c.ID)
	if len(c.Examples) != 2 {
		t.Fatalf("examples after channel posts: %d %v", len(c.Examples), c.Examples)
	}

	if _, err := svc.Unpublish(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
}

func mkMsg(from int64, text string) update {
	var u update
	json.Unmarshal([]byte(`{"update_id":1,"message":{"from":{"id":`+itoa(from)+`},"chat":{"id":`+itoa(from)+`,"type":"private"},"text":`+quote(text)+`}}`), &u)
	return u
}

func mkCallback(from int64, data string, msgID int64) update {
	var u update
	json.Unmarshal([]byte(`{"update_id":2,"callback_query":{"id":"cb","from":{"id":`+itoa(from)+`},"data":`+quote(data)+`,"message":{"message_id":`+itoa(msgID)+`,"chat":{"id":`+itoa(from)+`}}}}`), &u)
	return u
}

func mkChannelPost(id int64, text string) update {
	var u update
	json.Unmarshal([]byte(`{"update_id":3,"channel_post":{"message_id":`+itoa(id)+`,"text":`+quote(text)+`}}`), &u)
	return u
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
