package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/sahand363/sahand-cortext/internal/websearch"
	_ "modernc.org/sqlite"
)

func TestChatNotebookIsSealedPerChat(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, persona_id TEXT); CREATE TABLE messages(id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT); CREATE TABLE memories(id TEXT PRIMARY KEY, text TEXT, scope TEXT, source_session TEXT);`); err != nil {
		t.Fatal(err)
	}
	ps, err := newPrivateStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, b := ps.ref("bot1", 111), ps.ref("bot1", 222)
	nbA := ps.notebook(a)
	if err := nbA.Save(ctx, websearch.Note{Query: "pineapple prices", Summary: "Pineapples cost 3 euros"}); err != nil {
		t.Fatal(err)
	}
	notes, _ := nbA.List(ctx)
	if len(notes) != 1 || notes[0].Summary != "Pineapples cost 3 euros" {
		t.Fatalf("notes: %+v", notes)
	}
	if other, _ := ps.notebook(b).List(ctx); len(other) != 0 {
		t.Fatal("another chat sees this chat's notes")
	}
	var blob []byte
	_ = db.QueryRow(`SELECT blob FROM tg_private_webnotes`).Scan(&blob)
	if bytes.Contains(blob, []byte("pineapple")) {
		t.Fatal("note stored in plaintext")
	}
	_ = nbA.Touch(ctx, []string{notes[0].ID})
	if n, _ := nbA.List(ctx); n[0].Uses != 1 {
		t.Fatal("touch not saved")
	}
	_ = ps.forgetWebNotes(a)
	if n, _ := nbA.List(ctx); len(n) != 0 {
		t.Fatal("/forget left notes behind")
	}
}
