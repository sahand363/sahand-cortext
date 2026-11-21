package websearch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

// Modes (hub setting "websearch_mode").
const (
	ModeAuto    = "auto"    // the model searches when it needs to, plus /search
	ModeCommand = "command" // only /search <query>
	ModeOff     = "off"     // no search, no notes
)

// SettingKey is the hub settings key holding the mode.
const SettingKey = "websearch_mode"

// NormalizeMode maps a stored setting to a mode; empty means auto.
func NormalizeMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ModeCommand:
		return ModeCommand
	case ModeOff:
		return ModeOff
	default:
		return ModeAuto
	}
}

const (
	maxSearchesPerTurn = 2
	notesPerPrompt     = 3
	resultsPerSearch   = 6
	pagesToRead        = 3
	pageExcerptChars   = 5000
	maxNoteChars       = 1800
	// reuseWindow: a search whose query matches a note made this recently
	// reuses the note instead of hitting the search engine again.
	reuseWindow = 6 * time.Hour
)

// CallFunc runs the chat model once. onChunk gets streamed text and may
// be nil for a buffered call.
type CallFunc func(ctx context.Context, msgs []provider.Message, onChunk func(string)) (string, error)

// FetchFunc reads a web page as plain text (must refuse private addresses).
type FetchFunc func(ctx context.Context, url string) (string, error)

// Engine runs web-search-aware chat turns.
type Engine struct {
	Search SearchFunc
	Fetch  FetchFunc
	// Now is the clock (tests replace it).
	Now func() time.Time
}

// Turn is one chat turn.
type Turn struct {
	Mode     string
	Notebook Notebook
	// Messages is the prompt as the chat built it (system first, if any).
	Messages []provider.Message
	// UserMessage is the user's text, used to pick saved notes.
	UserMessage string
	// ForcedQuery is set for "/search <query>": search before answering.
	ForcedQuery string
	Call        CallFunc
	// Emit streams answer text to the user (nil for buffered chats).
	Emit func(string)
	// Status reports progress, e.g. "Searching the web: …" (may be nil).
	Status func(string)
}

var searchMarkerRe = regexp.MustCompile(`(?is)\[\[\s*SEARCH\s*:\s*(.+?)\s*\]\]`)

const markerPrefix = "[[SEARCH:"

// ParseSearchRequest reports whether answer is a search request: the
// marker must open the answer (a model that writes text first is
// answering, not asking).
func ParseSearchRequest(answer string) (string, bool) {
	t := strings.TrimSpace(answer)
	m := searchMarkerRe.FindStringSubmatchIndex(t)
	if m == nil || m[0] != 0 {
		return "", false
	}
	q := strings.TrimSpace(t[m[2]:m[3]])
	q = strings.Trim(q, "\"'` ")
	if q == "" {
		return "", false
	}
	if len([]rune(q)) > 200 {
		q = string([]rune(q)[:200])
	}
	return q, true
}

// StripMarkers removes any stray search markers from an answer.
func StripMarkers(answer string) string {
	return strings.TrimSpace(searchMarkerRe.ReplaceAllString(answer, ""))
}

// ParseCommand reports whether text is "/search <query>" (also /websearch,
// and the Persian /جستجو).
func ParseCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	for _, cmd := range []string{"/search", "/websearch", "/جستجو"} {
		if len(t) >= len(cmd) && strings.EqualFold(t[:len(cmd)], cmd) {
			rest := t[len(cmd):]
			if rest != "" && rest[0] == '@' { // /search@MyBot query
				if sp := strings.IndexAny(rest, " \n\t"); sp >= 0 {
					rest = rest[sp:]
				} else {
					rest = ""
				}
			}
			if rest != "" && rest[0] != ' ' && rest[0] != '\n' && rest[0] != '\t' {
				continue
			}
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func instructions(today string) string {
	return "\n\n=== WEB SEARCH ===\n" +
		"You can search the web. Today is " + today + ". If a good answer needs current or specific facts you do not reliably know " +
		"(news, prices, releases, versions, schedules, people, anything recent, or the user asks you to look something up) " +
		"and the web notes in this prompt do not cover it, reply with ONLY this one line and nothing else:\n" +
		"[[SEARCH: a short search query]]\n" +
		"Write the query in the language most likely to find good sources (English is often best for international news), " +
		"but always answer in the user's language. Never mention HTTP codes or search internals to the user.\n" +
		"The hub will search, give you a short note, and ask you again. If a note covers the topic but lacks the detail asked about, " +
		"search again with a narrower query for just that part. Notes can be out of date for fast-changing topics, so check their date. " +
		"Do not search for small talk, general knowledge you are sure of, or code you can write yourself. " +
		"When your answer uses web information, end it with the source links you used.\n" +
		"Web notes in this prompt are real search results the hub fetched for you, so use them. If earlier replies in this chat said " +
		"the web search did not work, that is out of date: ignore it and never repeat it.\n"
}

func notesBlock(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n=== WEB NOTES (saved from earlier web searches) ===\n")
	for _, n := range notes {
		sb.WriteString(FormatNote(n))
		sb.WriteString("\n")
	}
	return sb.String()
}

// addSystem appends text to the first system message (or adds one).
func addSystem(msgs []provider.Message, text string) []provider.Message {
	if text == "" {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content += text
		return out
	}
	return append([]provider.Message{{Role: "system", Content: strings.TrimSpace(text)}}, out...)
}

// pickText is what notes are matched against: the user's message plus the
// previous user message, so a short follow-up ("and the price?") still
// finds the note its topic made.
func pickText(t *Turn) string {
	text := t.UserMessage
	seen := 0
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if t.Messages[i].Role == "user" {
			seen++
			if seen == 2 {
				text += "\n" + t.Messages[i].Content
				break
			}
		}
	}
	return text
}

// Run executes the turn and returns the final answer (markers removed).
// Text is streamed through t.Emit only once the hub knows it is an answer,
// never a search request.
func (e *Engine) Run(ctx context.Context, t Turn) (string, error) {
	mode := NormalizeMode(t.Mode)
	status := t.Status
	if status == nil {
		status = func(string) {}
	}
	msgs := t.Messages
	today := e.now().Format("2006-01-02")

	if mode != ModeOff && t.Notebook != nil {
		if all, err := t.Notebook.List(ctx); err == nil {
			picked := Pick(all, pickText(&t), notesPerPrompt)
			if len(picked) > 0 {
				msgs = addSystem(msgs, notesBlock(picked))
				ids := make([]string, len(picked))
				for i, n := range picked {
					ids[i] = n.ID
				}
				_ = t.Notebook.Touch(ctx, ids)
			}
		}
	}
	searches := 0
	allowModelSearch := mode == ModeAuto
	if t.ForcedQuery != "" && mode != ModeOff {
		allowModelSearch = true // a /search turn may refine once more
	}
	if allowModelSearch {
		msgs = addSystem(msgs, instructions(today))
	}

	haveNotes := false
	if len(msgs) > 0 && strings.Contains(msgs[0].Content, "=== WEB NOTES") {
		haveNotes = true
	}
	research := func(q string) {
		searches++
		if n := recentNote(ctx, t.Notebook, q, e.now()); n != nil {
			haveNotes = true
			_ = t.Notebook.Touch(ctx, []string{n.ID})
			msgs = addSystem(msgs, "\n\n=== NEW WEB NOTE (searched a moment ago) ===\n"+FormatNote(*n))
			return
		}
		status("Searching the web: " + q)
		note, err := e.Research(ctx, t.Notebook, q, t.Call)
		if err != nil {
			log.Printf("websearch: search %q failed: %v", q, err)
			if haveNotes {
				msgs = addSystem(msgs, fmt.Sprintf("\n\n=== EXTRA SEARCH DID NOT WORK ===\nAn extra search for %q did not work, but the web notes above are real search results. Answer the user from them now, in full, with their sources. Do not say the web search failed.\n", q))
				return
			}
			msgs = addSystem(msgs, fmt.Sprintf("\n\n=== WEB SEARCH FAILED ===\nThe search for %q failed (%s). Answer from what you know, and say briefly that the web search did not work this time.\n", q, err.Error()))
			return
		}
		haveNotes = true
		msgs = addSystem(msgs, "\n\n=== NEW WEB NOTE (just searched) ===\n"+FormatNote(*note))
	}

	if t.ForcedQuery != "" {
		if mode == ModeOff {
			msgs = addSystem(msgs, "\n\n(Web search is turned off in the hub settings. Answer from what you know and mention that search is off.)\n")
		} else {
			research(t.ForcedQuery)
		}
	}

	for {
		gate := newGate(t.Emit)
		answer, err := t.Call(ctx, msgs, gate.write)
		if err != nil {
			gate.flush()
			return "", err
		}
		q, isSearch := ParseSearchRequest(answer)
		if !isSearch {
			gate.flush()
			return StripMarkers(answer), nil
		}
		if !allowModelSearch || searches >= maxSearchesPerTurn {
			if haveNotes {
				msgs = addSystem(msgs, "\n\nYou have already searched for this message: the web notes above are your search results. Answer the user now from them, in full, with their sources. Do not ask for another search and do not say the search failed.\n")
			} else {
				msgs = addSystem(msgs, "\n\nNo more web searches are possible for this message. Answer now with what you know, and say what you could not confirm. Do not ask for another search.\n")
			}
			g2 := newGate(t.Emit)
			final, err := t.Call(ctx, msgs, g2.write)
			if err != nil {
				return "", err
			}
			if _, again := ParseSearchRequest(final); again {
				msg := "I couldn't finish the web search for this one. Try asking again a bit more specifically, or use /search with your own query."
				if t.Emit != nil {
					t.Emit(msg)
				}
				return msg, nil
			}
			g2.flush()
			return StripMarkers(final), nil
		}
		research(q)
	}
}

// recentNote returns a saved note for the same search made within
// reuseWindow with the same words (any order). A narrower query searches
// again, so a follow-up for detail is never answered from the broad note.
func recentNote(ctx context.Context, nb Notebook, q string, now time.Time) *Note {
	if nb == nil {
		return nil
	}
	all, err := nb.List(ctx)
	if err != nil {
		return nil
	}
	qw := Words(q)
	if len(qw) == 0 {
		return nil
	}
	var best *Note
	for i := range all {
		n := &all[i]
		at, err := time.Parse("2006-01-02 15:04:05", n.CreatedAt)
		if err != nil || now.Sub(at) > reuseWindow || now.Sub(at) < -time.Minute {
			continue
		}
		if NormalizeQuery(n.Query) != NormalizeQuery(q) {
			nw := Words(n.Query)
			if !(subset(qw, nw) && subset(nw, qw)) {
				continue
			}
		}
		if best == nil || n.CreatedAt > best.CreatedAt {
			best = n
		}
	}
	return best
}

func subset(a, b []string) bool {
	set := make(map[string]bool, len(b))
	for _, w := range b {
		set[w] = true
	}
	for _, w := range a {
		if !set[w] {
			return false
		}
	}
	return true
}

// Research searches, reads the top pages, has the model write a short
// note, saves it in nb (when not nil) and returns it.
// richSnippetChars: a snippet this long already carries the page's facts.
const richSnippetChars = 600

func (e *Engine) Research(ctx context.Context, nb Notebook, query string, call CallFunc) (*Note, error) {
	if e.Search == nil {
		return nil, errors.New("web search is not configured")
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	results, err := e.Search(sctx, query, resultsPerSearch)
	cancel()
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, ErrNoResults
	}

	// Read the top pages in parallel; a page that fails is skipped.
	excerpts := make([]string, len(results))
	if e.Fetch != nil {
		var wg sync.WaitGroup
		for i := 0; i < len(results) && i < pagesToRead; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				fctx, cancel := context.WithTimeout(ctx, 12*time.Second)
				defer cancel()
				if unreadable(results[i].URL) {
					return
				}
				// Tavily already sends the relevant passages; no page read needed.
				if len([]rune(results[i].Snippet)) >= richSnippetChars {
					return
				}
				text, err := e.Fetch(fctx, results[i].URL)
				if err != nil {
					return
				}
				r := []rune(text)
				if len(r) > pageExcerptChars {
					text = string(r[:pageExcerptChars])
				}
				excerpts[i] = text
			}(i)
		}
		wg.Wait()
	}

	var src strings.Builder
	for i, r := range results {
		fmt.Fprintf(&src, "[%d] %s\nURL: %s\nSnippet: %s\n", i+1, r.Title, r.URL, r.Snippet)
		if excerpts[i] != "" {
			fmt.Fprintf(&src, "Page text (excerpt):\n%s\n", excerpts[i])
		}
		src.WriteString("\n")
	}

	today := e.now().Format("2006-01-02")
	summary := ""
	if call != nil {
		prompt := []provider.Message{
			{Role: "system", Content: "You write short research notes that will be saved and reused in later conversations. Be factual and compact."},
			{Role: "user", Content: "Search query: " + query + "\nDate of search: " + today + "\n\nSearch results:\n\n" + src.String() +
				"\nWrite a research note about the query from these results ONLY:\n" +
				"- at most 180 words, plain text, no preamble, no headings\n" +
				"- the key facts, numbers, names and dates, as short sentences or '-' lines\n" +
				"- put the source number after each fact, like [2]\n" +
				"- say clearly when the results disagree or do not answer the query\n" +
				"- write it in the language of the query"},
		}
		nctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		out, err := call(nctx, prompt, nil)
		cancel()
		if err == nil {
			summary = strings.TrimSpace(StripMarkers(out))
		}
	}
	if summary == "" {
		// The model could not summarise: keep the snippets as the note.
		var sb strings.Builder
		for i, r := range results {
			if r.Snippet != "" {
				fmt.Fprintf(&sb, "- %s [%d]\n", r.Snippet, i+1)
			}
		}
		summary = strings.TrimSpace(sb.String())
	}
	if summary == "" {
		return nil, ErrNoResults
	}
	if r := []rune(summary); len(r) > maxNoteChars {
		summary = string(r[:maxNoteChars]) + "…"
	}

	sources := make([]Result, 0, len(results))
	for _, r := range results {
		sources = append(sources, Result{Title: r.Title, URL: r.URL})
	}
	stamp := e.now().UTC().Format("2006-01-02 15:04:05")
	note := Note{ID: newID(), Query: query, Summary: summary, Sources: sources, CreatedAt: stamp, LastUsed: stamp}
	if nb != nil {
		_ = nb.Save(ctx, note)
	}
	return &note, nil
}

// gate holds streamed text back while it might still be a search marker,
// then lets an answer through as it streams.
type gate struct {
	emit    func(string)
	buf     strings.Builder
	decided bool // true once we know this is an answer (streaming on)
	marker  bool // true once we know this is a search request
}

func newGate(emit func(string)) *gate { return &gate{emit: emit} }

func (g *gate) write(chunk string) {
	if g.emit == nil || chunk == "" || g.marker {
		return
	}
	if g.decided {
		g.emit(chunk)
		return
	}
	g.buf.WriteString(chunk)
	head := strings.ToUpper(strings.TrimLeft(g.buf.String(), " \t\r\n"))
	if head == "" {
		return
	}
	compact := strings.ReplaceAll(head, " ", "")
	if len(compact) >= len(markerPrefix) {
		if strings.HasPrefix(compact, markerPrefix) {
			g.marker = true
			return
		}
	} else if strings.HasPrefix(markerPrefix, compact) {
		return // could still become a marker
	}
	g.decided = true
	g.emit(g.buf.String())
	g.buf.Reset()
}

// flush releases held text that turned out to be an answer.
func (g *gate) flush() {
	if g.emit == nil || g.marker || g.decided {
		return
	}
	if s := g.buf.String(); s != "" {
		g.emit(s)
	}
	g.buf.Reset()
	g.decided = true
}
