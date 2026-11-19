// Package channel runs "channel studio" bots: each one writes posts for a
// Telegram channel in a chosen persona's voice, keeps them as drafts in the
// hub, asks the owner to approve them (in the hub or by buttons in a
// Telegram DM) and publishes them to the channel.
package channel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

var ErrNotFound = errors.New("not found")

// Post statuses.
const (
	StatusDraft     = "draft"     // in the hub, not sent anywhere
	StatusPending   = "pending"   // sent to the owner's Telegram for approval
	StatusPosted    = "posted"    // published on the channel
	StatusDiscarded = "discarded" // thrown away
	StatusRemoved   = "removed"   // was posted, then deleted from the channel
)

const maxExamples = 15

type Channel struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	PersonaID    string   `json:"persona_id"`
	ChatRef      string   `json:"chat_ref"` // "@channelname" or "-100…"
	BotUsername  string   `json:"bot_username"`
	HasToken     bool     `json:"has_token"`
	OwnerID      int64    `json:"owner_id"`
	OwnerName    string   `json:"owner_name"`
	PairCode     string   `json:"pair_code"`
	Instructions string   `json:"instructions"` // standing notes from the owner
	StyleGuide   string   `json:"style_guide"`
	Examples     []string `json:"examples"`
	Enabled      bool     `json:"enabled"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`

	token string // decrypted bot token, never serialised
}

type Post struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Idea      string `json:"idea"`
	Notes     string `json:"notes"`
	Draft     string `json:"draft"`    // current text (may be edited by the owner)
	AIDraft   string `json:"ai_draft"` // last text the model wrote
	Status    string `json:"status"`
	TGMessage int64  `json:"tg_message_id"`
	Error     string `json:"error"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	PostedAt  string `json:"posted_at"`

	approvalChat int64
	approvalMsg  int64
	awaitingRev  bool
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) InitSchema() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS channels (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		persona_id TEXT NOT NULL DEFAULT '',
		chat_ref TEXT NOT NULL DEFAULT '',
		bot_token TEXT NOT NULL DEFAULT '',
		bot_username TEXT NOT NULL DEFAULT '',
		owner_id INTEGER NOT NULL DEFAULT 0,
		owner_name TEXT NOT NULL DEFAULT '',
		pair_code TEXT NOT NULL DEFAULT '',
		instructions TEXT NOT NULL DEFAULT '',
		style_guide TEXT NOT NULL DEFAULT '',
		examples TEXT NOT NULL DEFAULT '[]',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS channel_posts (
		id TEXT PRIMARY KEY,
		channel_id TEXT NOT NULL,
		idea TEXT NOT NULL DEFAULT '',
		notes TEXT NOT NULL DEFAULT '',
		draft TEXT NOT NULL DEFAULT '',
		ai_draft TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'draft',
		tg_message_id INTEGER NOT NULL DEFAULT 0,
		approval_chat INTEGER NOT NULL DEFAULT 0,
		approval_msg INTEGER NOT NULL DEFAULT 0,
		awaiting_revision INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		posted_at TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_channel_posts_channel ON channel_posts(channel_id, created_at);`)
	return err
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newPairCode() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

const channelCols = `id, name, persona_id, chat_ref, bot_token, bot_username, owner_id, owner_name, pair_code, instructions, style_guide, examples, enabled, created_at, updated_at`

func scanChannel(sc interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	var tok, ex string
	var en int
	if err := sc.Scan(&c.ID, &c.Name, &c.PersonaID, &c.ChatRef, &tok, &c.BotUsername, &c.OwnerID, &c.OwnerName, &c.PairCode, &c.Instructions, &c.StyleGuide, &ex, &en, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.token = provider.DecryptAPIKey(tok)
	c.HasToken = strings.TrimSpace(tok) != ""
	c.Enabled = en == 1
	_ = json.Unmarshal([]byte(ex), &c.Examples)
	if c.Examples == nil {
		c.Examples = []string{}
	}
	return &c, nil
}

func (s *Store) List(ctx context.Context) ([]*Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM channels ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// Save inserts or updates a channel. token == "" keeps the stored token.
func (s *Store) Save(ctx context.Context, c *Channel, token string) error {
	if c.ID == "" {
		c.ID = newID()
		c.PairCode = newPairCode()
		enc, _ := provider.EncryptAPIKey(strings.TrimSpace(token))
		ex, _ := json.Marshal(c.Examples)
		_, err := s.db.ExecContext(ctx, `INSERT INTO channels (id, name, persona_id, chat_ref, bot_token, bot_username, pair_code, instructions, style_guide, examples, enabled) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.Name, c.PersonaID, c.ChatRef, enc, c.BotUsername, c.PairCode, c.Instructions, c.StyleGuide, string(ex), boolInt(c.Enabled))
		return err
	}
	if strings.TrimSpace(token) != "" {
		enc, _ := provider.EncryptAPIKey(strings.TrimSpace(token))
		if _, err := s.db.ExecContext(ctx, `UPDATE channels SET bot_token = ? WHERE id = ?`, enc, c.ID); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET name=?, persona_id=?, chat_ref=?, bot_username=?, instructions=?, style_guide=?, enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		c.Name, c.PersonaID, c.ChatRef, c.BotUsername, c.Instructions, c.StyleGuide, boolInt(c.Enabled), c.ID)
	return err
}

func (s *Store) SetStyle(ctx context.Context, id, guide string, examples []string) error {
	if len(examples) > maxExamples {
		examples = examples[len(examples)-maxExamples:]
	}
	ex, _ := json.Marshal(examples)
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET style_guide=?, examples=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, guide, string(ex), id)
	return err
}

// AddExample appends a real post to the channel's examples (newest last).
func (s *Store) AddExample(ctx context.Context, id, text string) error {
	c, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if len([]rune(text)) < 40 {
		return nil
	}
	for _, e := range c.Examples {
		if e == text {
			return nil
		}
	}
	return s.SetStyle(ctx, id, c.StyleGuide, append(c.Examples, text))
}

func (s *Store) SetOwner(ctx context.Context, id string, ownerID int64, ownerName string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET owner_id=?, owner_name=?, pair_code=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, ownerID, ownerName, newPairCode(), id)
	return err
}

func (s *Store) ResetOwner(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET owner_id=0, owner_name='', pair_code=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, newPairCode(), id)
	return err
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_posts WHERE channel_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	return err
}

const postCols = `id, channel_id, idea, notes, draft, ai_draft, status, tg_message_id, approval_chat, approval_msg, awaiting_revision, error, created_at, updated_at, posted_at`

func scanPost(sc interface{ Scan(...any) error }) (*Post, error) {
	var p Post
	var aw int
	if err := sc.Scan(&p.ID, &p.ChannelID, &p.Idea, &p.Notes, &p.Draft, &p.AIDraft, &p.Status, &p.TGMessage, &p.approvalChat, &p.approvalMsg, &aw, &p.Error, &p.CreatedAt, &p.UpdatedAt, &p.PostedAt); err != nil {
		return nil, err
	}
	p.awaitingRev = aw == 1
	return &p, nil
}

func (s *Store) Posts(ctx context.Context, channelID string, limit int) ([]*Post, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+postCols+` FROM channel_posts WHERE channel_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPost(ctx context.Context, id string) (*Post, error) {
	p, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postCols+` FROM channel_posts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) SavePost(ctx context.Context, p *Post) error {
	if p.ID == "" {
		p.ID = newID()
		if p.Status == "" {
			p.Status = StatusDraft
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO channel_posts (id, channel_id, idea, notes, draft, ai_draft, status) VALUES (?,?,?,?,?,?,?)`,
			p.ID, p.ChannelID, p.Idea, p.Notes, p.Draft, p.AIDraft, p.Status)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE channel_posts SET idea=?, notes=?, draft=?, ai_draft=?, status=?, tg_message_id=?, approval_chat=?, approval_msg=?, awaiting_revision=?, error=?, posted_at=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		p.Idea, p.Notes, p.Draft, p.AIDraft, p.Status, p.TGMessage, p.approvalChat, p.approvalMsg, boolInt(p.awaitingRev), p.Error, p.PostedAt, p.ID)
	return err
}

func (s *Store) DeletePost(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_posts WHERE id = ?`, id)
	return err
}

// AwaitingRevision returns the post of this channel the owner asked to
// revise in Telegram, if any.
func (s *Store) AwaitingRevision(ctx context.Context, channelID string) (*Post, error) {
	p, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postCols+` FROM channel_posts WHERE channel_id = ? AND awaiting_revision = 1 ORDER BY updated_at DESC LIMIT 1`, channelID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// EditLessons returns recent published posts whose final text differs from
// what the model wrote: what the owner changes is the best style signal.
func (s *Store) EditLessons(ctx context.Context, channelID string, n int) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ai_draft, draft FROM channel_posts WHERE channel_id = ? AND status = 'posted' AND ai_draft != '' AND ai_draft != draft ORDER BY posted_at DESC LIMIT ?`, channelID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if rows.Scan(&a, &b) == nil {
			out = append(out, [2]string{a, b})
		}
	}
	return out, rows.Err()
}

// IsOurMessage reports whether a channel message id was posted by the hub.
func (s *Store) IsOurMessage(ctx context.Context, channelID string, msgID int64) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM channel_posts WHERE channel_id = ? AND tg_message_id = ?`, channelID, msgID).Scan(&n)
	return n > 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
