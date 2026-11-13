package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TTL options (seconds). 0 = never delete.
const (
	TTLNever  int64 = 0
	TTL10Min  int64 = 600
	TTL1Hour  int64 = 3600
	TTL24Hour int64 = 86400
	TTL7Days  int64 = 604800
)

func validTTL(v int64) bool {
	switch v {
	case TTLNever, TTL10Min, TTL1Hour, TTL24Hour, TTL7Days:
		return true
	}
	return false
}

func ttlLabel(v int64) string {
	switch v {
	case TTLNever:
		return "never"
	case TTL10Min:
		return "10 minutes"
	case TTL1Hour:
		return "1 hour"
	case TTL24Hour:
		return "24 hours"
	case TTL7Days:
		return "7 days"
	}
	return "never"
}

// Room is one mIRC-style channel. Only the OP creates / edits / deletes rooms.
// Hidden rooms are not listed, are only reachable via their secret
// InviteToken link, still need the room password, and return 404 —
// no name, no messages — to anyone without link + password.
type Room struct {
	ID           string `json:"id"`
	Name         string `json:"name"` // without the leading '#'
	Hidden       bool   `json:"hidden"`
	PasswordHash string `json:"password_hash,omitempty"`
	Salt         string `json:"salt,omitempty"`
	TTLSeconds   int64  `json:"ttl_seconds"`
	InviteToken  string `json:"invite_token"`
	// v2.3: private link code, e.g. "shdvbfsbd4r4t". Empty = no private
	// link. The link /r/<PrivateCode> opens ONLY this room, full screen,
	// with no room list, OP panel or online list.
	PrivateCode string    `json:"private_code,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func (r *Room) Locked() bool { return r.PasswordHash != "" }

// Public is what the API returns. For a hidden room, the OP alone
// gets the invite link; everyone else gets nothing at all (see CanSee).
func (r *Room) Public(isOP bool) map[string]any {
	m := map[string]any{
		"id": r.ID, "name": r.Name, "hidden": r.Hidden,
		"locked": r.Locked(), "ttl_seconds": r.TTLSeconds,
		"ttl_label": ttlLabel(r.TTLSeconds),
	}
	if isOP {
		m["invite_token"] = r.InviteToken
		m["private_code"] = r.PrivateCode
	}
	return m
}

// GetByPrivateCode finds the room whose private link code matches,
// or nil. The code is the secret — anyone who has it may open that
// room's link (the room password, if any, is still asked for).
func (rs *RoomStore) GetByPrivateCode(code string) *Room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if code == "" {
		return nil
	}
	for _, r := range rs.rooms {
		if r.PrivateCode == code {
			return r
		}
	}
	return nil
}

// SetPrivateCode creates (or regenerates) a room's private link code.
// Returns the new code. The old code stops working at once.
func (rs *RoomStore) SetPrivateCode(id string) (string, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rooms[id]
	if !ok {
		return "", fmt.Errorf("room not found")
	}
	for {
		code, err := newRandomCode(14)
		if err != nil {
			return "", err
		}
		taken := false
		for _, other := range rs.rooms {
			if other.PrivateCode == code {
				taken = true
				break
			}
		}
		if taken {
			continue
		}
		r.PrivateCode = code
		if err := rs.saveLocked(); err != nil {
			return "", err
		}
		return code, nil
	}
}

// DeletePrivateCode removes a room's private link. The link stops working.
func (rs *RoomStore) DeletePrivateCode(id string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rooms[id]
	if !ok {
		return fmt.Errorf("room not found")
	}
	r.PrivateCode = ""
	return rs.saveLocked()
}

type RoomStore struct {
	mu    sync.Mutex
	path  string
	rooms map[string]*Room
	order []string // creation order, general first
}

func NewRoomStore(dataDir string) (*RoomStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	rs := &RoomStore{path: filepath.Join(dataDir, "rooms.json"), rooms: map[string]*Room{}}
	data, err := os.ReadFile(rs.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else if len(data) > 0 {
		var list []*Room
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		for _, r := range list {
			if r != nil && r.ID != "" {
				rs.rooms[r.ID] = r
				rs.order = append(rs.order, r.ID)
			}
		}
	}
	if _, ok := rs.rooms["general"]; !ok {
		token, _ := newTokenHex(8)
		rs.rooms["general"] = &Room{
			ID: "general", Name: "general", TTLSeconds: TTLNever,
			InviteToken: token, CreatedAt: time.Now(),
		}
		rs.order = append([]string{"general"}, rs.order...)
		if err := rs.saveLocked(); err != nil {
			return nil, err
		}
	}
	return rs, nil
}

func (rs *RoomStore) saveLocked() error {
	list := []*Room{}
	for _, id := range rs.order {
		if r, ok := rs.rooms[id]; ok {
			list = append(list, r)
		}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := rs.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, rs.path)
}

func (rs *RoomStore) Get(id string) *Room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.rooms[id]
}

func (rs *RoomStore) All() []*Room {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := []*Room{}
	for _, id := range rs.order {
		if r, ok := rs.rooms[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

// CanSee: does this session even learn that the room exists?
// OP: everything. Visible room: everyone. Hidden room: only if the
// session already unlocked it, or the caller supplies the invite token.
func (rs *RoomStore) CanSee(sess *Session, r *Room, invite string) bool {
	if r == nil {
		return false
	}
	if sess.IsOP() {
		return true
	}
	if !r.Hidden {
		return true
	}
	if sess.HasRoom(r.ID) {
		return true
	}
	return invite != "" && invite == r.InviteToken
}

// CanAccess: can this session read/send in the room?
// OP: always. Anyone else: only after a successful join (unlocked).
// Open visible rooms are joined with an empty password, so joining
// is one click; locked/hidden rooms need their password first.
func (rs *RoomStore) CanAccess(sess *Session, r *Room) bool {
	if r == nil || sess == nil {
		return false
	}
	return sess.HasRoom(r.ID)
}

func cleanRoomName(name string) (string, error) {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	if name == "" || len([]rune(name)) > 30 {
		return "", fmt.Errorf("room name must be 1–30 characters")
	}
	if strings.ContainsAny(name, " \t\n/#?") {
		return "", fmt.Errorf("room name cannot contain spaces or / # ?")
	}
	return name, nil
}

func (rs *RoomStore) Create(name, password string, hidden bool, ttl int64) (*Room, error) {
	name, err := cleanRoomName(name)
	if err != nil {
		return nil, err
	}
	if !validTTL(ttl) {
		return nil, fmt.Errorf("invalid self-destruct timer")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, r := range rs.rooms {
		if strings.EqualFold(r.Name, name) {
			return nil, fmt.Errorf("a room named #%s already exists", name)
		}
	}
	id, err := newTokenHex(6)
	if err != nil {
		return nil, err
	}
	token, err := newTokenHex(8)
	if err != nil {
		return nil, err
	}
	r := &Room{ID: id, Name: name, Hidden: hidden, TTLSeconds: ttl, InviteToken: token, CreatedAt: time.Now()}
	if password != "" {
		salt, err := newSalt()
		if err != nil {
			return nil, err
		}
		r.Salt = salt
		r.PasswordHash = hashPassword(password, salt)
	}
	rs.rooms[id] = r
	rs.order = append(rs.order, id)
	if err := rs.saveLocked(); err != nil {
		delete(rs.rooms, id)
		return nil, err
	}
	return r, nil
}

// Update applies OP edits. password == nil: keep; *password == "": remove;
// otherwise set to the new password. Returns the updated room.
func (rs *RoomStore) Update(id string, name *string, password *string, hidden *bool, ttl *int64) (*Room, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rooms[id]
	if !ok {
		return nil, fmt.Errorf("room not found")
	}
	if name != nil {
		n, err := cleanRoomName(*name)
		if err != nil {
			return nil, err
		}
		for _, other := range rs.rooms {
			if other.ID != id && strings.EqualFold(other.Name, n) {
				return nil, fmt.Errorf("a room named #%s already exists", n)
			}
		}
		r.Name = n
	}
	if password != nil {
		if *password == "" {
			r.PasswordHash, r.Salt = "", ""
		} else {
			salt, err := newSalt()
			if err != nil {
				return nil, err
			}
			r.Salt = salt
			r.PasswordHash = hashPassword(*password, salt)
		}
	}
	if hidden != nil {
		r.Hidden = *hidden
	}
	if ttl != nil {
		if !validTTL(*ttl) {
			return nil, fmt.Errorf("invalid self-destruct timer")
		}
		r.TTLSeconds = *ttl
	}
	if err := rs.saveLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

func (rs *RoomStore) Delete(id string) error {
	if id == "general" {
		return fmt.Errorf("the #general room cannot be deleted")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.rooms[id]; !ok {
		return fmt.Errorf("room not found")
	}
	delete(rs.rooms, id)
	for i, oid := range rs.order {
		if oid == id {
			rs.order = append(rs.order[:i], rs.order[i+1:]...)
			break
		}
	}
	return rs.saveLocked()
}

func (rs *RoomStore) CheckRoomPassword(r *Room, password string) bool {
	if r == nil || !r.Locked() {
		return true
	}
	return hashMatches(password, r.Salt, r.PasswordHash)
}
