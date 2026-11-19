package telegram

// Private, encrypted storage for Telegram chats.
//
// Telegram conversations never go into the panel's sessions/messages tables.
// Each chat (one bot persona + one Telegram chat) gets:
//   - an opaque id:  HMAC(secret, "sid|persona|chat"), so the database does
//     not even reveal which Telegram chat a row belongs to;
//   - its own AES-256-GCM key: HMAC(secret, "key|persona|chat").
//
// The secret lives in <data dir>/telegram-chats.key (0600), outside the
// database and outside backups. With only cortext.db (or a backup zip) the
// chats are unreadable. The hub itself can decrypt a chat only while that
// Telegram chat is talking to it, because it needs the chat id to derive the
// key. There is no panel page or API that lists or reads them.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

const secretFileName = "telegram-chats.key"

// maxPersonFacts caps how many facts the bot keeps about one person.
const maxPersonFacts = 80

type privateStore struct {
	db     *sql.DB
	secret []byte
}

type chatRef struct {
	sid string
	key []byte
}

func loadOrCreateSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, secretFileName)
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr == nil && len(raw) == 32 {
			return raw, nil
		}
		return nil, fmt.Errorf("%s is corrupt", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(raw)+"\n"), 0600); err != nil {
		return nil, err
	}
	return raw, nil
}

func newPrivateStore(db *sql.DB, dataDir string) (*privateStore, error) {
	secret, err := loadOrCreateSecret(dataDir)
	if err != nil {
		return nil, err
	}
	ps := &privateStore{db: db, secret: secret}
	if err := ps.initSchema(); err != nil {
		return nil, err
	}
	return ps, nil
}

func (ps *privateStore) initSchema() error {
	_, err := ps.db.Exec(`
		CREATE TABLE IF NOT EXISTS tg_private_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sid TEXT NOT NULL,
			blob BLOB NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_tg_private_messages_sid ON tg_private_messages(sid, id);
		CREATE TABLE IF NOT EXISTS tg_private_memory (
			sid TEXT PRIMARY KEY,
			blob BLOB NOT NULL,
			extracted_upto INTEGER NOT NULL DEFAULT 0
		);`)
	return err
}

func (ps *privateStore) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, ps.secret)
	m.Write([]byte(strings.Join(parts, "|")))
	return m.Sum(nil)
}

func (ps *privateStore) ref(personaID string, chatID int64) chatRef {
	c := strconv.FormatInt(chatID, 10)
	return chatRef{
		sid: hex.EncodeToString(ps.mac("tg-sid-v1", personaID, c))[:40],
		key: ps.mac("tg-key-v1", personaID, c),
	}
}

func seal(r chatRef, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte(r.sid)), nil
}

func open(r chatRef, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("short ciphertext")
	}
	return gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], []byte(r.sid))
}

type sealedMessage struct {
	Role    string `json:"r"`
	Content string `json:"c"`
	At      int64  `json:"t"`
}

func (ps *privateStore) appendMessage(tx execer, r chatRef, role, content string) error {
	plain, _ := json.Marshal(sealedMessage{Role: role, Content: content, At: time.Now().Unix()})
	blob, err := seal(r, plain)
	if err != nil {
		return err
	}
	if tx == nil {
		tx = ps.db
	}
	_, err = tx.Exec(`INSERT INTO tg_private_messages(sid, blob) VALUES(?, ?)`, r.sid, blob)
	return err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// history returns the chat's messages after afterID, oldest first, with the
// id of the last row read. Rows are fully read and closed before returning.
func (ps *privateStore) history(r chatRef, afterID int64) ([]provider.Message, int64, error) {
	rows, err := ps.db.Query(`SELECT id, blob FROM tg_private_messages WHERE sid = ? AND id > ? ORDER BY id ASC`, r.sid, afterID)
	if err != nil {
		return nil, afterID, err
	}
	type row struct {
		id   int64
		blob []byte
	}
	var raw []row
	for rows.Next() {
		var x row
		if rows.Scan(&x.id, &x.blob) == nil {
			raw = append(raw, x)
		}
	}
	rows.Close()

	last := afterID
	var out []provider.Message
	for _, x := range raw {
		last = x.id
		plain, err := open(r, x.blob)
		if err != nil {
			continue
		}
		var m sealedMessage
		if json.Unmarshal(plain, &m) == nil && m.Content != "" {
			out = append(out, provider.Message{Role: m.Role, Content: m.Content})
		}
	}
	return out, last, nil
}

func (ps *privateStore) clearHistory(r chatRef) error {
	_, err := ps.db.Exec(`DELETE FROM tg_private_messages WHERE sid = ?`, r.sid)
	if err == nil {
		// Memory extraction restarts from the new conversation.
		_, _ = ps.db.Exec(`UPDATE tg_private_memory SET extracted_upto = 0 WHERE sid = ?`, r.sid)
	}
	return err
}

// facts returns what the bot remembers about this person and the message id
// extraction has reached.
func (ps *privateStore) facts(r chatRef) ([]string, int64) {
	var blob []byte
	var upto int64
	if err := ps.db.QueryRow(`SELECT blob, extracted_upto FROM tg_private_memory WHERE sid = ?`, r.sid).Scan(&blob, &upto); err != nil {
		return nil, 0
	}
	plain, err := open(r, blob)
	if err != nil {
		return nil, upto
	}
	var facts []string
	_ = json.Unmarshal(plain, &facts)
	return facts, upto
}

func (ps *privateStore) saveFacts(r chatRef, facts []string, upto int64) error {
	if len(facts) > maxPersonFacts {
		facts = facts[len(facts)-maxPersonFacts:]
	}
	plain, _ := json.Marshal(facts)
	blob, err := seal(r, plain)
	if err != nil {
		return err
	}
	_, err = ps.db.Exec(`INSERT INTO tg_private_memory(sid, blob, extracted_upto) VALUES(?, ?, ?)
		ON CONFLICT(sid) DO UPDATE SET blob = excluded.blob, extracted_upto = excluded.extracted_upto`, r.sid, blob, upto)
	return err
}

func (ps *privateStore) forget(r chatRef) error {
	_, err := ps.db.Exec(`DELETE FROM tg_private_memory WHERE sid = ?`, r.sid)
	return err
}

var tgSessionRe = regexp.MustCompile(`^tg-(-?\d+)(?:-\d+)?$`)

// migratePlaintext moves Telegram chats saved by older versions out of the
// panel's sessions/messages tables into encrypted storage, then deletes the
// plaintext copies. Safe to run on every start.
func (ps *privateStore) migratePlaintext() (int, error) {
	rows, err := ps.db.Query(`SELECT id, coalesce(persona_id,'') FROM sessions WHERE id LIKE 'tg-%'`)
	if err != nil {
		return 0, err
	}
	type sess struct{ id, persona string }
	var list []sess
	for rows.Next() {
		var s sess
		if rows.Scan(&s.id, &s.persona) == nil {
			list = append(list, s)
		}
	}
	rows.Close()

	moved := 0
	for _, s := range list {
		m := tgSessionRe.FindStringSubmatch(s.id)
		if m == nil {
			continue
		}
		chatID, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		r := ps.ref(s.persona, chatID)

		mrows, err := ps.db.Query(`SELECT role, content FROM messages WHERE session_id = ? ORDER BY id ASC`, s.id)
		if err != nil {
			return moved, err
		}
		var msgs []provider.Message
		for mrows.Next() {
			var role, content string
			if mrows.Scan(&role, &content) == nil {
				msgs = append(msgs, provider.Message{Role: role, Content: content})
			}
		}
		mrows.Close()

		tx, err := ps.db.Begin()
		if err != nil {
			return moved, err
		}
		failed := false
		for _, msg := range msgs {
			if err := ps.appendMessage(tx, r, msg.Role, msg.Content); err != nil {
				failed = true
				break
			}
		}
		if !failed {
			for _, q := range []string{
				`DELETE FROM messages WHERE session_id = ?`,
				`DELETE FROM memory_extraction_state WHERE session_id = ?`,
				`DELETE FROM sessions WHERE id = ?`,
			} {
				if _, err := tx.Exec(q, s.id); err != nil && !strings.Contains(err.Error(), "no such table") {
					failed = true
					break
				}
			}
		}
		if failed {
			_ = tx.Rollback()
			continue
		}
		if err := tx.Commit(); err == nil {
			moved++
		}
	}
	return moved, nil
}

// migrateTelegramMemories moves facts that older versions extracted from
// Telegram chats into the panel's Memories page (memories.source_session
// "tg-...") into that person's private, encrypted memory, and deletes the
// visible copies. Facts whose bot can't be told are deleted. Safe to run on
// every start.
func (ps *privateStore) migrateTelegramMemories() (int, error) {
	rows, err := ps.db.Query(`SELECT id, text, scope, source_session FROM memories WHERE source_session LIKE 'tg-%'`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	type mem struct{ id, text, scope, src string }
	var list []mem
	for rows.Next() {
		var m mem
		if rows.Scan(&m.id, &m.text, &m.scope, &m.src) == nil {
			list = append(list, m)
		}
	}
	rows.Close()

	byChat := map[string][]string{}
	refs := map[string]chatRef{}
	for _, m := range list {
		sm := tgSessionRe.FindStringSubmatch(m.src)
		if sm == nil || m.scope == "" || m.scope == "main" || m.scope == "all" {
			continue
		}
		chatID, err := strconv.ParseInt(sm[1], 10, 64)
		if err != nil {
			continue
		}
		r := ps.ref(m.scope, chatID)
		refs[r.sid] = r
		byChat[r.sid] = append(byChat[r.sid], strings.TrimSpace(m.text))
	}
	for sid, add := range byChat {
		r := refs[sid]
		facts, upto := ps.facts(r)
		seen := map[string]bool{}
		for _, f := range facts {
			seen[strings.ToLower(f)] = true
		}
		for _, f := range add {
			if f != "" && !seen[strings.ToLower(f)] {
				facts = append(facts, f)
				seen[strings.ToLower(f)] = true
			}
		}
		if err := ps.saveFacts(r, facts, upto); err != nil {
			return 0, err
		}
	}
	res, err := ps.db.Exec(`DELETE FROM memories WHERE source_session LIKE 'tg-%'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
