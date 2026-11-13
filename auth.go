package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session is a browser login. It is persisted to data/sessions.json so a
// restart does not log everyone out — sessions live SessionDays (7 by default).
// Role is "op" or "user". UnlockedRooms holds the room IDs this session
// has already entered (room password given / open room joined).
type Session struct {
	Token         string    `json:"token"`
	Role          string    `json:"role"`
	DisplayName   string    `json:"display_name"`
	UnlockedRooms []string  `json:"unlocked_rooms,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (s *Session) IsOP() bool { return s != nil && s.Role == "op" }

func (s *Session) HasRoom(id string) bool {
	if s == nil {
		return false
	}
	if s.IsOP() {
		return true
	}
	for _, r := range s.UnlockedRooms {
		if r == id {
			return true
		}
	}
	return false
}

type SessionStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]*Session
	ttl      time.Duration
}

func NewSessionStore(dataDir string, ttl time.Duration) (*SessionStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	st := &SessionStore{
		path:     filepath.Join(dataDir, "sessions.json"),
		sessions: map[string]*Session{},
		ttl:      ttl,
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		var list []*Session
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		now := time.Now()
		for _, s := range list {
			if s != nil && s.ExpiresAt.After(now) {
				if s.Role == "" {
					s.Role = "user"
				}
				st.sessions[s.Token] = s
			}
		}
	}
	return st, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (st *SessionStore) Create(role string) (*Session, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	if role != "op" {
		role = "user"
	}
	now := time.Now()
	s := &Session{Token: tok, Role: role, CreatedAt: now, ExpiresAt: now.Add(st.ttl)}
	st.mu.Lock()
	st.sessions[tok] = s
	err = st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (st *SessionStore) Get(token string) *Session {
	if token == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[token]
	if !ok || s.ExpiresAt.Before(time.Now()) {
		return nil
	}
	return s
}

// SetName assigns the visitor's own display name. Names must be unique
// among currently-active sessions (case-insensitive), so two people
// cannot impersonate each other in the room.
func (st *SessionStore) SetName(token, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 30 {
		return fmt.Errorf("display name must be 1–30 characters")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for tok, s := range st.sessions {
		if tok != token && s.ExpiresAt.After(time.Now()) &&
			strings.EqualFold(strings.TrimSpace(s.DisplayName), name) {
			return fmt.Errorf("that display name is already taken — pick another one")
		}
	}
	s, ok := st.sessions[token]
	if !ok {
		return fmt.Errorf("session not found — please log in again")
	}
	s.DisplayName = name
	return st.saveLocked()
}

// UnlockRoom records that this session entered a room (password given).
func (st *SessionStore) UnlockRoom(token, roomID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[token]
	if !ok {
		return
	}
	for _, r := range s.UnlockedRooms {
		if r == roomID {
			return
		}
	}
	s.UnlockedRooms = append(s.UnlockedRooms, roomID)
	_ = st.saveLocked()
}

// DeleteByName removes every active session with that display name.
// Used by the OP kick: the kicked person is logged out immediately.
func (st *SessionStore) DeleteByName(name string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for tok, s := range st.sessions {
		if strings.EqualFold(strings.TrimSpace(s.DisplayName), strings.TrimSpace(name)) && !s.IsOP() {
			delete(st.sessions, tok)
			n++
		}
	}
	if n > 0 {
		_ = st.saveLocked()
	}
	return n
}

// All returns a snapshot of active sessions (for online lists).
func (st *SessionStore) All() []*Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	out := []*Session{}
	for _, s := range st.sessions {
		if s.ExpiresAt.After(now) && s.DisplayName != "" {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out
}

func (st *SessionStore) Delete(token string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, token)
	_ = st.saveLocked()
}

func (st *SessionStore) saveLocked() error {
	list := make([]*Session, 0, len(st.sessions))
	now := time.Now()
	for _, s := range st.sessions {
		if s.ExpiresAt.After(now) {
			list = append(list, s)
		}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// --- Bans (OP only) ---

// BanStore is data/bans.json: display names the OP has banned.
// A banned name cannot pick that name at identity time and cannot send.
type BanStore struct {
	mu    sync.Mutex
	path  string
	names map[string]bool // lower-cased
}

func NewBanStore(dataDir string) (*BanStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	b := &BanStore{path: filepath.Join(dataDir, "bans.json"), names: map[string]bool{}}
	data, err := os.ReadFile(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		for _, n := range list {
			b.names[strings.ToLower(strings.TrimSpace(n))] = true
		}
	}
	return b, nil
}

func (b *BanStore) IsBanned(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.names[strings.ToLower(strings.TrimSpace(name))]
}

func (b *BanStore) Add(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.names[strings.ToLower(strings.TrimSpace(name))] = true
	_ = b.saveLocked()
}

func (b *BanStore) Remove(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.names, strings.ToLower(strings.TrimSpace(name)))
	_ = b.saveLocked()
}

func (b *BanStore) List() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for n := range b.names {
		out = append(out, n)
	}
	return out
}

func (b *BanStore) saveLocked() error {
	list := []string{}
	for n := range b.names {
		list = append(list, n)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// --- Maths captcha (addition / subtraction, 5-minute expiry) ---

type captcha struct {
	answer    int
	expiresAt time.Time
}

type CaptchaStore struct {
	mu    sync.Mutex
	items map[string]captcha
}

func NewCaptchaStore() *CaptchaStore {
	return &CaptchaStore{items: map[string]captcha{}}
}

func randInt(n int64) int {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// New creates a captcha like "7 + 5 = ?" and returns its id + question.
func (c *CaptchaStore) New() (id, question string, err error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	id = hex.EncodeToString(b)
	a, b2 := randInt(20)+1, randInt(20)+1
	var answer int
	if randInt(2) == 0 {
		question = fmt.Sprintf("%d + %d = ؟", a, b2)
		answer = a + b2
	} else {
		if b2 > a {
			a, b2 = b2, a
		}
		question = fmt.Sprintf("%d - %d = ؟", a, b2)
		answer = a - b2
	}
	c.mu.Lock()
	c.items[id] = captcha{answer: answer, expiresAt: time.Now().Add(5 * time.Minute)}
	c.mu.Unlock()
	return id, question, nil
}

// Verify checks an answer once — a captcha cannot be replayed.
func (c *CaptchaStore) Verify(id string, answer int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	delete(c.items, id)
	if !ok || time.Now().After(it.expiresAt) {
		return false
	}
	return it.answer == answer
}
