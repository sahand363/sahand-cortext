package main

import (
	"sync"
)

// Event is one SSE push to a room. Type is "message", "delete" or "prune".
type Event struct {
	Type      string   `json:"type"`
	RoomID    string   `json:"room_id"`
	Message   *Message `json:"message,omitempty"`
	MessageID int64    `json:"message_id,omitempty"`
}

type client struct {
	room string
	ch   chan Event
}

// Broker fans events out to the clients of one room only.
// Each client gets a buffered channel so one slow phone cannot block a room.
type Broker struct {
	mu      sync.Mutex
	clients map[chan Event]client
}

func NewBroker() *Broker {
	return &Broker{clients: map[chan Event]client{}}
}

func (b *Broker) Subscribe(roomID string) chan Event {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.clients[ch] = client{room: roomID, ch: ch}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(roomID string, ev Event) {
	ev.RoomID = roomID
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, c := range b.clients {
		if c.room != roomID {
			continue
		}
		select {
		case ch <- ev:
		default:
			// Client is too slow / disconnected; drop this one event
			// for them rather than blocking everyone else.
		}
	}
}

func (b *Broker) Count(roomID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.clients {
		if roomID == "" || c.room == roomID {
			n++
		}
	}
	return n
}
