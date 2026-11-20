package telegram

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/sahand363/sahand-cortext/internal/websearch"
)

// Web-search notes for one Telegram chat. Like the chat itself they are
// sealed with the chat's own key (table tg_private_webnotes), so the
// owner cannot see what a contact searched for, and /forget erases them.

func (ps *privateStore) initWebNotesSchema() error {
	_, err := ps.db.Exec(`CREATE TABLE IF NOT EXISTS tg_private_webnotes (
		sid TEXT PRIMARY KEY,
		blob BLOB NOT NULL
	);`)
	return err
}

type chatNotebook struct {
	ps  *privateStore
	ref chatRef
	mu  *sync.Mutex
}

var webNotesMu sync.Mutex // serialises read-modify-write of note blobs

func (ps *privateStore) notebook(r chatRef) websearch.Notebook {
	return &chatNotebook{ps: ps, ref: r, mu: &webNotesMu}
}

func (c *chatNotebook) load() []websearch.Note {
	var blob []byte
	if err := c.ps.db.QueryRow(`SELECT blob FROM tg_private_webnotes WHERE sid = ?`, c.ref.sid).Scan(&blob); err != nil {
		return nil
	}
	plain, err := open(c.ref, blob)
	if err != nil {
		return nil
	}
	var notes []websearch.Note
	_ = json.Unmarshal(plain, &notes)
	return notes
}

func (c *chatNotebook) store(notes []websearch.Note) error {
	plain, _ := json.Marshal(notes)
	blob, err := seal(c.ref, plain)
	if err != nil {
		return err
	}
	_, err = c.ps.db.Exec(`INSERT INTO tg_private_webnotes(sid, blob) VALUES(?, ?)
		ON CONFLICT(sid) DO UPDATE SET blob = excluded.blob`, c.ref.sid, blob)
	return err
}

func (c *chatNotebook) List(context.Context) ([]websearch.Note, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load(), nil
}

func (c *chatNotebook) Save(_ context.Context, n websearch.Note) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store(websearch.MergeNote(c.load(), n, websearch.MaxChatNotes))
}

func (c *chatNotebook) Touch(_ context.Context, ids []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	notes := c.load()
	if len(notes) == 0 {
		return nil
	}
	return c.store(websearch.TouchNotes(notes, ids))
}

func (ps *privateStore) forgetWebNotes(r chatRef) error {
	_, err := ps.db.Exec(`DELETE FROM tg_private_webnotes WHERE sid = ?`, r.sid)
	return err
}
