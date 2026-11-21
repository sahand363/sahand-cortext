package websearch

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

func TestParseLite(t *testing.T) {
	b, err := os.ReadFile("testdata/lite.html")
	if err != nil {
		t.Fatal(err)
	}
	res := parseLite(string(b))
	if len(res) < 3 {
		t.Fatalf("want 3+ results, got %d", len(res))
	}
	if res[0].URL != "https://go.dev/doc/go1.23" {
		t.Fatalf("first url = %q", res[0].URL)
	}
	if !strings.Contains(res[0].Title, "Go 1.23") || res[0].Snippet == "" {
		t.Fatalf("bad first result: %+v", res[0])
	}
	for _, r := range res {
		if strings.Contains(r.URL, "duckduckgo.com") {
			t.Fatalf("unwrapped link leaked: %s", r.URL)
		}
	}
}

func TestParseSearchRequest(t *testing.T) {
	cases := map[string]string{
		"[[SEARCH: go 1.23 release date]]":  "go 1.23 release date",
		"  \n[[search:  bitcoin price ]]\n": "bitcoin price",
		"[[ SEARCH : \"weather berlin\" ]]": "weather berlin",
		"Sure! [[SEARCH: x]]":               "",
		"No marker here":                    "",
	}
	for in, want := range cases {
		got, ok := ParseSearchRequest(in)
		if want == "" && ok {
			t.Errorf("%q: unexpected search %q", in, got)
		}
		if want != "" && (!ok || got != want) {
			t.Errorf("%q: got %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	if q, ok := ParseCommand("/search golang generics"); !ok || q != "golang generics" {
		t.Fatalf("got %q %v", q, ok)
	}
	if q, ok := ParseCommand("/search@MyBot price of gold"); !ok || q != "price of gold" {
		t.Fatalf("got %q %v", q, ok)
	}
	if _, ok := ParseCommand("/searching"); ok {
		t.Fatal("/searching is not /search")
	}
	if q, ok := ParseCommand("/search"); !ok || q != "" {
		t.Fatal("bare /search should parse with empty query")
	}
}

func TestGateHoldsMarkerAndStreamsAnswers(t *testing.T) {
	var out strings.Builder
	g := newGate(func(s string) { out.WriteString(s) })
	for _, c := range []string{" [", "[SEA", "RCH: x]]"} {
		g.write(c)
	}
	g.flush()
	if out.Len() != 0 {
		t.Fatalf("marker leaked: %q", out.String())
	}

	out.Reset()
	g = newGate(func(s string) { out.WriteString(s) })
	for _, c := range []string{"[", "1] Hello", " world"} {
		g.write(c)
	}
	g.flush()
	if out.String() != "[1] Hello world" {
		t.Fatalf("answer = %q", out.String())
	}

	out.Reset()
	g = newGate(func(s string) { out.WriteString(s) })
	g.write("Hi")
	if out.String() != "Hi" {
		t.Fatalf("plain text should stream at once, got %q", out.String())
	}
}

type memNotebook struct {
	mu    sync.Mutex
	notes []Note
}

func (m *memNotebook) List(context.Context) ([]Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Note(nil), m.notes...), nil
}
func (m *memNotebook) Save(_ context.Context, n Note) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes = MergeNote(m.notes, n, 10)
	return nil
}
func (m *memNotebook) Touch(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes = TouchNotes(m.notes, ids)
	return nil
}

func TestRunSearchesSavesNoteAndReuses(t *testing.T) {
	searchCalls := 0
	e := &Engine{
		Search: func(ctx context.Context, q string, max int) ([]Result, error) {
			searchCalls++
			return []Result{{Title: "Go 1.23 is released", URL: "https://go.dev/blog/go1.23", Snippet: "Go 1.23 was released on August 13, 2024."}}, nil
		},
		Fetch: func(ctx context.Context, u string) (string, error) {
			return "Go 1.23 released August 13 2024 with iterators.", nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
	}
	nb := &memNotebook{}
	calls := 0
	call := func(ctx context.Context, msgs []provider.Message, onChunk func(string)) (string, error) {
		calls++
		sys := msgs[0].Content
		if strings.Contains(sys, "research notes") {
			return "- Go 1.23 was released on 2024-08-13 [1]", nil
		}
		if strings.Contains(sys, "NEW WEB NOTE") || strings.Contains(sys, "WEB NOTES") {
			if onChunk != nil {
				onChunk("Go 1.23 came out on 13 August 2024.")
			}
			return "Go 1.23 came out on 13 August 2024.", nil
		}
		if onChunk != nil {
			onChunk("[[SEARCH: go 1.23 ")
			onChunk("release date]]")
		}
		return "[[SEARCH: go 1.23 release date]]", nil
	}
	var streamed strings.Builder
	var statuses []string
	ans, err := e.Run(context.Background(), Turn{
		Mode:        ModeAuto,
		Notebook:    nb,
		Messages:    []provider.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: "When was Go 1.23 released?"}},
		UserMessage: "When was Go 1.23 released?",
		Call:        call,
		Emit:        func(s string) { streamed.WriteString(s) },
		Status:      func(s string) { statuses = append(statuses, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "August 2024") || streamed.String() != ans {
		t.Fatalf("answer %q streamed %q", ans, streamed.String())
	}
	if strings.Contains(streamed.String(), "SEARCH") {
		t.Fatal("marker reached the user")
	}
	if searchCalls != 1 || len(nb.notes) != 1 || len(statuses) != 1 {
		t.Fatalf("searches=%d notes=%d statuses=%v", searchCalls, len(nb.notes), statuses)
	}

	// Second turn on the same topic: the saved note is in the prompt and
	// the model answers without searching again.
	_, err = e.Run(context.Background(), Turn{
		Mode:        ModeAuto,
		Notebook:    nb,
		Messages:    []provider.Message{{Role: "user", Content: "What's new in Go 1.23 release?"}},
		UserMessage: "What's new in Go 1.23 release?",
		Call:        call,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls != 1 {
		t.Fatalf("second turn should reuse the note, searched %d times", searchCalls)
	}
	if nb.notes[0].Uses != 1 {
		t.Fatalf("note use not counted: %+v", nb.notes[0])
	}
}

func TestRunStopsAfterMaxSearches(t *testing.T) {
	e := &Engine{Search: func(ctx context.Context, q string, max int) ([]Result, error) {
		return []Result{{Title: "t", URL: "https://example.com", Snippet: "s"}}, nil
	}}
	n := 0
	call := func(ctx context.Context, msgs []provider.Message, onChunk func(string)) (string, error) {
		if strings.Contains(msgs[0].Content, "research notes") {
			return "note", nil
		}
		n++
		return "[[SEARCH: again " + string(rune('a'+n)) + "]]", nil
	}
	ans, err := e.Run(context.Background(), Turn{Mode: ModeAuto, Messages: []provider.Message{{Role: "user", Content: "q"}}, UserMessage: "q", Call: call})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ans, "[[") || ans == "" {
		t.Fatalf("bad final answer %q", ans)
	}
	if n != maxSearchesPerTurn+2 {
		t.Fatalf("model calls = %d", n)
	}
}

func TestCommandModeNoInstructions(t *testing.T) {
	e := &Engine{}
	var sys string
	call := func(ctx context.Context, msgs []provider.Message, onChunk func(string)) (string, error) {
		sys = msgs[0].Content
		return "hi", nil
	}
	_, _ = e.Run(context.Background(), Turn{Mode: ModeCommand, Messages: []provider.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "hello"}}, UserMessage: "hello", Call: call})
	if strings.Contains(sys, "WEB SEARCH") {
		t.Fatal("command mode must not invite the model to search")
	}
}

func TestPickMatchesFollowUpsAndPersian(t *testing.T) {
	notes := []Note{
		{ID: "1", Query: "gold price today", Summary: "Gold is $2,650 an ounce [1]", CreatedAt: "2026-11-20"},
		{ID: "2", Query: "قیمت دلار در تهران", Summary: "دلار ۷۰ هزار تومان", CreatedAt: "2026-11-20"},
	}
	if p := Pick(notes, "what is the gold price?", 3); len(p) != 1 || p[0].ID != "1" {
		t.Fatalf("english pick %+v", p)
	}
	if p := Pick(notes, "قیمت دلار امروز چنده؟", 3); len(p) != 1 || p[0].ID != "2" {
		t.Fatalf("persian pick %+v", p)
	}
	if p := Pick(notes, "write me a poem", 3); len(p) != 0 {
		t.Fatalf("unrelated pick %+v", p)
	}
}

// A question asked again soon after reuses the fresh note instead of
// hitting the search engine, and a failed extra search never makes the
// model deny the notes it already has.
func TestRecentNoteReuseAndFailureWithNotes(t *testing.T) {
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	searches := 0
	fail := false
	e := &Engine{
		Search: func(ctx context.Context, q string, max int) ([]Result, error) {
			searches++
			if fail {
				return nil, ErrNoResults
			}
			return []Result{{Title: "News", URL: "https://example.com/n", Snippet: "Iran news today"}}, nil
		},
		Now: func() time.Time { return now },
	}
	nb := &memNotebook{}
	var lastSys string
	call := func(ctx context.Context, msgs []provider.Message, onChunk func(string)) (string, error) {
		if strings.Contains(msgs[0].Content, "research notes") {
			return "- Iran news item [1]", nil
		}
		lastSys = msgs[0].Content
		if !strings.Contains(lastSys, "NEW WEB NOTE") && !strings.Contains(lastSys, "SEARCH DID NOT WORK") && !strings.Contains(lastSys, "already searched") {
			return "[[SEARCH: اخبار ایران امروز]]", nil
		}
		if strings.Count(lastSys, "NEW WEB NOTE") < 2 && !strings.Contains(lastSys, "already searched") && !strings.Contains(lastSys, "DID NOT WORK") {
			return "[[SEARCH: مهمترین اخبار ایران امروز]]", nil
		}
		return "answer from notes", nil
	}
	run := func() string {
		ans, err := e.Run(context.Background(), Turn{Mode: ModeAuto, Notebook: nb,
			Messages:    []provider.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "اخبار امروز ایران"}},
			UserMessage: "اخبار امروز ایران", Call: call})
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	if ans := run(); ans != "answer from notes" {
		t.Fatalf("first turn answer %q", ans)
	}
	first := searches
	fail = true
	if ans := run(); ans != "answer from notes" {
		t.Fatalf("second turn answer %q", ans)
	}
	if searches != first {
		t.Fatalf("second turn should reuse fresh notes, searched %d more times", searches-first)
	}
	if strings.Contains(lastSys, "WEB SEARCH FAILED") {
		t.Fatal("model was told the search failed although it had notes")
	}
}
