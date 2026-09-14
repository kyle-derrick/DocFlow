package realtime

import (
	"encoding/json"
	"sync"

	"github.com/docflow/docflow/internal/notify"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type client struct {
	conn *websocket.Conn
	send chan []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[uuid.UUID]map[*client]struct{}
}

func NewHub() *Hub { return &Hub{clients: make(map[uuid.UUID]map[*client]struct{})} }

func (h *Hub) Register(user uuid.UUID, conn *websocket.Conn) func() {
	c := &client{conn: conn, send: make(chan []byte, 16)}
	h.mu.Lock()
	if h.clients[user] == nil {
		h.clients[user] = make(map[*client]struct{})
	}
	h.clients[user][c] = struct{}{}
	h.mu.Unlock()
	go func() {
		for msg := range c.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()
	return func() { h.remove(user, c) }
}

func (h *Hub) remove(user uuid.UUID, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if users := h.clients[user]; users != nil {
		if _, ok := users[c]; ok {
			delete(users, c)
			close(c.send)
		}
		if len(users) == 0 {
			delete(h.clients, user)
		}
	}
}

func (h *Hub) Broadcast(user uuid.UUID, n notify.Notification) {
	msg, err := json.Marshal(n)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients[user] {
		select {
		case c.send <- msg:
		default:
			go h.remove(user, c)
		}
	}
}
