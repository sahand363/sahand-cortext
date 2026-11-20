package websearch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Note is what one web search leaves behind: a short summary with its
// sources. Raw pages are never kept.
type Note struct {
	ID        string   `json:"id"`
	Query     string   `json:"query"`
	Summary   string   `json:"summary"`
	Sources   []Result `json:"sources"` // title + url (snippets are dropped)
	CreatedAt string   `json:"created_at"`
	LastUsed  string   `json:"last_used"`
	Uses      int      `json:"uses"`
}

// Notebook is one place notes live: the hub owner's shared notes, or one
// Telegram chat's encrypted notes.
type Notebook interface {
	List(ctx context.Context) ([]Note, error)
	// Save stores n, replacing any note with the same query, and prunes
	// the oldest notes past the notebook's cap.
	Save(ctx context.Context, n Note) error
	Touch(ctx context.Context, ids []string) error
}

// MaxOwnerNotes caps the hub owner's notebook (about 1-2 KB each).
const MaxOwnerNotes = 500

// MaxChatNotes caps one Telegram chat's notebook.
const MaxChatNotes = 60

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowStamp() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

// NormalizeQuery folds a query so "Go 1.23  Release" and "go 1.23 release"
// count as the same search.
func NormalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(q))), " ")
}

// ---- SQL notebook (hub owner: web panel + Android app) ----

// SQLStore keeps the owner's notes in the hub database (table web_notes).
type SQLStore struct{ db *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

func (s *SQLStore) InitSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS web_notes (
			id TEXT PRIMARY KEY,
			query TEXT NOT NULL,
			query_norm TEXT NOT NULL,
			summary TEXT NOT NULL,
			sources TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_used DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			uses INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_web_notes_query ON web_notes(query_norm);`)
	return err
}

// List returns every note, newest first. Rows are drained and closed
// before it returns (single SQLite connection).
func (s *SQLStore) List(ctx context.Context) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, query, summary, sources, created_at, last_used, uses FROM web_notes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var src string
		if err := rows.Scan(&n.ID, &n.Query, &n.Summary, &src, &n.CreatedAt, &n.LastUsed, &n.Uses); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(src), &n.Sources)
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *SQLStore) Save(ctx context.Context, n Note) error {
	if strings.TrimSpace(n.Summary) == "" {
		return errors.New("empty note")
	}
	if n.ID == "" {
		n.ID = newID()
	}
	src, _ := json.Marshal(n.Sources)
	norm := NormalizeQuery(n.Query)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM web_notes WHERE query_norm = ?`, norm); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO web_notes (id, query, query_norm, summary, sources) VALUES (?, ?, ?, ?, ?)`,
		n.ID, strings.TrimSpace(n.Query), norm, strings.TrimSpace(n.Summary), string(src)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_notes WHERE id IN (
		SELECT id FROM web_notes ORDER BY last_used DESC, created_at DESC LIMIT -1 OFFSET ?)`, MaxOwnerNotes)
	return err
}

func (s *SQLStore) Touch(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE web_notes SET last_used = CURRENT_TIMESTAMP, uses = uses + 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes one note.
func (s *SQLStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_notes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("note not found")
	}
	return nil
}

// Clear removes every note and returns how many there were.
func (s *SQLStore) Clear(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_notes`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- In-memory list helpers (used by encrypted per-chat notebooks) ----

// MergeNote returns list with n added (same query replaced) and capped at
// max, keeping the most recently used notes.
func MergeNote(list []Note, n Note, max int) []Note {
	if n.ID == "" {
		n.ID = newID()
	}
	stamp := nowStamp()
	if n.CreatedAt == "" {
		n.CreatedAt = stamp
	}
	if n.LastUsed == "" {
		n.LastUsed = stamp
	}
	norm := NormalizeQuery(n.Query)
	out := []Note{n}
	for _, old := range list {
		if NormalizeQuery(old.Query) != norm {
			out = append(out, old)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastUsed > out[j].LastUsed })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// TouchNotes marks ids as used in list.
func TouchNotes(list []Note, ids []string) []Note {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	stamp := nowStamp()
	for i := range list {
		if want[list[i].ID] {
			list[i].LastUsed = stamp
			list[i].Uses++
		}
	}
	return list
}

// ---- Picking notes for a prompt ----

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true, "what": true, "who": true,
	"how": true, "why": true, "when": true, "where": true, "which": true, "with": true, "this": true,
	"that": true, "you": true, "your": true, "can": true, "does": true, "did": true, "about": true,
	"from": true, "have": true, "has": true, "will": true, "would": true, "could": true, "should": true,
	"tell": true, "me": true, "please": true, "is": true, "it": true, "its": true, "of": true, "in": true,
	"on": true, "to": true, "a": true, "an": true, "or": true, "be": true, "do": true, "any": true,
	"some": true, "more": true, "than": true, "then": true, "there": true, "their": true, "they": true,
	"now": true, "new": true, "get": true, "use": true, "using": true, "into": true, "also": true,
	"search": true, "web": true, "find": true, "look": true,
}

// Words splits text into lowercase words (any script, 3+ letters or a
// number), dropping common English filler.
func Words(text string) []string {
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-'
	})
	var out []string
	seen := map[string]bool{}
	for _, w := range f {
		w = strings.Trim(w, ".-")
		if w == "" || seen[w] || stopWords[w] {
			continue
		}
		isNum := strings.IndexFunc(w, unicode.IsLetter) < 0
		if !isNum && len([]rune(w)) < 3 {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// Pick returns up to max notes relevant to text, best first. A note
// scores 2 per word shared with its query and 1 per word shared with its
// summary; it needs a query-word hit or 2+ summary hits to count.
func Pick(notes []Note, text string, max int) []Note {
	words := Words(text)
	if len(words) == 0 || len(notes) == 0 {
		return nil
	}
	type scored struct {
		n     Note
		score int
	}
	var list []scored
	for _, n := range notes {
		q := " " + strings.Join(Words(n.Query), " ") + " "
		sum := strings.ToLower(n.Summary)
		score, qHits := 0, 0
		for _, w := range words {
			if strings.Contains(q, " "+w+" ") {
				score += 2
				qHits++
			} else if strings.Contains(sum, w) {
				score++
			}
		}
		if qHits > 0 || score >= 2 {
			list = append(list, scored{n, score})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].n.CreatedAt > list[j].n.CreatedAt
	})
	var out []Note
	for _, s := range list {
		if len(out) >= max {
			break
		}
		out = append(out, s.n)
	}
	return out
}

// FormatNote renders one note for a prompt.
func FormatNote(n Note) string {
	var sb strings.Builder
	date := n.CreatedAt
	if len(date) >= 10 {
		date = date[:10]
	}
	fmt.Fprintf(&sb, "[Web note · searched %s · query: %q]\n%s\n", date, n.Query, strings.TrimSpace(n.Summary))
	if len(n.Sources) > 0 {
		sb.WriteString("Sources:\n")
		for i, s := range n.Sources {
			fmt.Fprintf(&sb, "[%d] %s - %s\n", i+1, s.Title, s.URL)
		}
	}
	return sb.String()
}
