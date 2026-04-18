package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// EventBus manages WebSocket connections and broadcasts events.
type EventBus struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

func NewEventBus() *EventBus {
	return &EventBus{
		clients: make(map[*websocket.Conn]bool),
	}
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

func (eb *EventBus) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}

	eb.mu.Lock()
	eb.clients[conn] = true
	eb.mu.Unlock()

	log.Printf("ws client connected (%d total)", len(eb.clients))

	// Keep connection alive, read and discard client messages
	for {
		_, _, err := conn.Read(context.Background())
		if err != nil {
			break
		}
	}

	eb.mu.Lock()
	delete(eb.clients, conn)
	eb.mu.Unlock()

	conn.Close(websocket.StatusNormalClosure, "")
	log.Printf("ws client disconnected (%d remaining)", len(eb.clients))
}

func (eb *EventBus) Broadcast(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	eb.mu.Lock()
	defer eb.mu.Unlock()

	for conn := range eb.clients {
		err := conn.Write(context.Background(), websocket.MessageText, data)
		if err != nil {
			conn.Close(websocket.StatusInternalError, "write failed")
			delete(eb.clients, conn)
		}
	}
}
