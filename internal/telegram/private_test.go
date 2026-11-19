package telegram

import (
	"bytes"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPrivateStoreEncryptsAndMigrates(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, persona_id TEXT, pinned INTEGER, updated_at TEXT);
		CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT);
		CREATE TABLE memory_extraction_state(session_id TEXT PRIMARY KEY, last_message_id INTEGER);
		INSERT INTO sessions(id, persona_id) VALUES('tg--100123', 'bot1'), ('web-1', '');
		INSERT INTO messages(session_id, role, content) VALUES('tg--100123','user','my secret is pineapple'),('tg--100123','assistant','noted'),('web-1','user','hello');
	`); err != nil {
		t.Fatal(err)
	}

	ps, err := newPrivateStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ps.migratePlaintext()
	if err != nil || n != 1 {
		t.Fatalf("migrate: n=%d err=%v", n, err)
	}
	var left int
	db.QueryRow(`SELECT count(*) FROM messages WHERE session_id LIKE 'tg-%'`).Scan(&left)
	if left != 0 {
		t.Fatal("plaintext telegram messages remain")
	}
	db.QueryRow(`SELECT count(*) FROM messages`).Scan(&left)
	if left != 1 {
		t.Fatal("web chat must be untouched")
	}

	r := ps.ref("bot1", -100123)
	hist, _, err := ps.history(r, 0)
	if err != nil || len(hist) != 2 || hist[0].Content != "my secret is pineapple" {
		t.Fatalf("history: %+v %v", hist, err)
	}

	// No plaintext anywhere in the private table.
	rows, _ := db.Query(`SELECT sid, blob FROM tg_private_messages`)
	for rows.Next() {
		var sid string
		var blob []byte
		rows.Scan(&sid, &blob)
		if bytes.Contains(blob, []byte("pineapple")) || bytes.Contains([]byte(sid), []byte("100123")) {
			t.Fatal("plaintext leaked")
		}
	}
	rows.Close()

	// Another bot or chat cannot read it.
	if h, _, _ := ps.history(ps.ref("bot2", -100123), 0); len(h) != 0 {
		t.Fatal("other persona read the chat")
	}

	if err := ps.saveFacts(r, []string{"Likes pineapple"}, 2); err != nil {
		t.Fatal(err)
	}
	if f, upto := ps.facts(r); len(f) != 1 || upto != 2 {
		t.Fatalf("facts: %v %d", f, upto)
	}
	_ = ps.forget(r)
	if f, _ := ps.facts(r); len(f) != 0 {
		t.Fatal("forget failed")
	}

	// Same secret file on restart derives the same keys.
	ps2, err := newPrivateStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, _ := ps2.history(ps2.ref("bot1", -100123), 0); len(h) != 2 {
		t.Fatal("restart lost access")
	}
}
