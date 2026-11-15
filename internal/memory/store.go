// Package memory turns conversation history into reusable memory: durable
// facts extracted from earlier conversations, stored in SQLite, and injected
// into the LLM system prompt when they are relevant to the current message.
// It mirrors the Skills engine's visibility model (scopes "all" / "main" /
// a bot's persona id) and the same single-connection discipline: rows are
// fully read and closed before any long AI call.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

// ScopeAll is the assignment scope meaning "visible to everyone" (main chat + all bots).
const ScopeAll = "all"

// ScopeMain is the assignment scope for the main (web panel) chat.
const ScopeMain = "main"

var ErrMemoryNotFound = errors.New("memory not found")

// Memory is one durable fact remembered from an earlier conversation.
type Memory struct {
	ID            string `json:"id"`
	Text          string `json:"text"`
	Scope         string `json:"scope"` // "all" | "main" | persona id
	SourceSession string `json:"source_session,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) InitSchema() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		text TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT 'main',
		source_session TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(scope);`); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memory_extraction_state (
		session_id TEXT PRIMARY KEY,
		last_message_id INTEGER NOT NULL DEFAULT 0,
		extracted_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`)
	return err
}

// newID returns a random 16-hex-char id.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return ScopeMain
	}
	return scope
}

func (s *Store) Save(ctx context.Context, m Memory) (*Memory, error) {
	m.Text = strings.TrimSpace(m.Text)
	if m.Text == "" {
		return nil, errors.New("memory text is required")
	}
	if len([]rune(m.Text)) > 2000 {
		m.Text = string([]rune(m.Text)[:2000])
	}
	if m.ID == "" {
		m.ID = newID()
	}
	m.Scope = normalizeScope(m.Scope)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (id, text, scope, source_session, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			text = excluded.text,
			scope = excluded.scope,
			source_session = excluded.source_session,
			updated_at = CURRENT_TIMESTAMP
	`, m.ID, m.Text, m.Scope, strings.TrimSpace(m.SourceSession))
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, m.ID)
}

func (s *Store) Get(ctx context.Context, id string) (*Memory, error) {
	var m Memory
	err := s.db.QueryRowContext(ctx, `SELECT id, text, scope, source_session, created_at, updated_at FROM memories WHERE id = ?`, id).Scan(&m.ID, &m.Text, &m.Scope, &m.SourceSession, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMemoryNotFound
		}
		return nil, err
	}
	return &m, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

func (s *Store) SetScope(ctx context.Context, id, scope string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, normalizeScope(scope), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// List returns all memories (newest first), optionally filtered to one scope.
func (s *Store) List(ctx context.Context, scope string) ([]Memory, error) {
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(scope) == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, text, scope, source_session, created_at, updated_at FROM memories ORDER BY updated_at DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, text, scope, source_session, created_at, updated_at FROM memories WHERE scope = ? ORDER BY updated_at DESC`, strings.TrimSpace(scope))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if rows.Scan(&m.ID, &m.Text, &m.Scope, &m.SourceSession, &m.CreatedAt, &m.UpdatedAt) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// ListVisible returns the memories visible to the caller's scopes.
// A memory scoped "all" is visible to every caller. The rows are fully
// read and closed before returning, so the caller may safely run the long
// AI call afterwards on the single-connection SQLite.
func (s *Store) ListVisible(ctx context.Context, scopes []string) ([]Memory, error) {
	seen := map[string]bool{ScopeAll: true}
	var args []any
	args = append(args, ScopeAll)
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		args = append(args, sc)
	}
	placeholders := strings.Repeat("?,", len(args))
	placeholders = strings.TrimSuffix(placeholders, ",")
	q := `SELECT id, text, scope, source_session, created_at, updated_at FROM memories WHERE scope IN (` + placeholders + `) ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if rows.Scan(&m.ID, &m.Text, &m.Scope, &m.SourceSession, &m.CreatedAt, &m.UpdatedAt) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// Exists reports whether a memory with the same text (case-insensitive,
// trimmed) already exists in the given scope.
func (s *Store) Exists(ctx context.Context, scope, text string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE scope = ? AND LOWER(TRIM(text)) = LOWER(TRIM(?))`, normalizeScope(scope), strings.TrimSpace(text)).Scan(&n)
	return n > 0, err
}

// BuildMemoryPromptBlock formats the memories relevant to the current
// message for injection into the LLM system prompt. When there are few
// memories (8 or fewer) they are all injected, so the model always knows
// the durable facts; with more, only the most relevant ones are injected,
// capped by a character budget so memory never bloats the prompt.
func (s *Store) BuildMemoryPromptBlock(ctx context.Context, scopes []string, userMessage string) string {
	mems, err := s.ListVisible(ctx, scopes)
	if err != nil || len(mems) == 0 {
		return ""
	}
	const maxMemories = 5
	const maxChars = 4000
	const injectAllWhen = 8

	type scored struct {
		m     Memory
		score int
	}
	userLower := strings.ToLower(userMessage)
	var list []scored
	for _, m := range mems {
		list = append(list, scored{m: m, score: memoryScore(userLower, m.Text)})
	}
	if len(list) > injectAllWhen {
		sort.SliceStable(list, func(i, j int) bool { return list[i].score > list[j].score })
		var top []scored
		for _, sc := range list {
			if sc.score > 0 && len(top) < maxMemories {
				top = append(top, sc)
			}
		}
		if len(top) == 0 {
			return ""
		}
		list = top
	}
	var sb strings.Builder
	sb.WriteString("\n\n=== REMEMBERED FACTS ===\n")
	sb.WriteString("The following are durable facts remembered from earlier conversations with this user. Use them when relevant; they are true unless the user says otherwise:\n\n")
	total := 0
	for _, sc := range list {
		line := "- " + strings.TrimSpace(sc.m.Text) + "\n"
		if total+len(line) > maxChars {
			break
		}
		sb.WriteString(line)
		total += len(line)
	}
	return sb.String()
}

// memoryScore counts how many distinct words (4+ letters) from the memory
// text appear in the user's message.
func memoryScore(userLower, text string) int {
	seen := map[string]bool{}
	score := 0
	for _, w := range strings.Fields(strings.ToLower(text)) {
		clean := strings.Trim(w, ".,:;!?()[]{}\"'`")
		if len([]rune(clean)) < 4 || seen[clean] {
			continue
		}
		seen[clean] = true
		if strings.Contains(userLower, clean) {
			score++
		}
	}
	return score
}
