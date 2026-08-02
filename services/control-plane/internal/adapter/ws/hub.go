// Package ws implements the realtime event stream consumed by the desktop app
// and the CLI.
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Frame types exchanged over the socket.
const (
	frameEvent      = "event"
	frameGap        = "gap"
	framePong       = "pong"
	frameError      = "error"
	frameSubscribed = "subscribed"
	cmdSubscribe    = "subscribe"
	cmdUnsubscribe  = "unsubscribe"
	cmdPing         = "ping"
)

// clientFrame is a message from the client.
type clientFrame struct {
	Type     string   `json:"type"`
	Topics   []string `json:"topics,omitempty"`
	AfterSeq int64    `json:"after_seq,omitempty"`
}

// serverFrame is a message to the client.
type serverFrame struct {
	Type    string        `json:"type"`
	Topic   string        `json:"topic,omitempty"`
	Seq     int64         `json:"seq,omitempty"`
	Event   *domain.Event `json:"event,omitempty"`
	From    int64         `json:"from,omitempty"`
	To      int64         `json:"to,omitempty"`
	Topics  []string      `json:"topics,omitempty"`
	Message string        `json:"message,omitempty"`
}

// Timing constants. The write deadline prevents a dead TCP connection from
// pinning a goroutine indefinitely; the pong deadline is set to slightly more
// than two ping intervals so a single lost packet does not drop the client.
const (
	pingInterval = 20 * time.Second
	pongWait     = 50 * time.Second
	writeWait    = 10 * time.Second
	maxFrameSize = 8 * 1024
)

// Hub serves websocket connections from the event bus.
type Hub struct {
	bus    port.Bus
	events port.EventRepository
	log    *slog.Logger

	mu    sync.Mutex
	conns int
}

// NewHub constructs the hub.
func NewHub(bus port.Bus, events port.EventRepository, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{bus: bus, events: events, log: log}
}

// Connections reports the number of live sockets.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

// Handle serves one websocket connection.
//
// The connection uses a strict single-writer design: exactly one goroutine
// writes to the socket. Concurrent writes to a gorilla-style connection are a
// data race that manifests as corrupted frames, and it is far easier to prevent
// structurally than to debug in production.
func (h *Hub) Handle(c *websocket.Conn) {
	principal, _ := c.Locals("principal").(domain.Principal)

	h.mu.Lock()
	h.conns++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.conns--
		h.mu.Unlock()
		_ = c.Close()
	}()

	sub := h.bus.Subscribe(256)
	defer sub.Close()

	c.SetReadLimit(maxFrameSize)
	_ = c.SetReadDeadline(time.Now().Add(pongWait))
	c.SetPongHandler(func(string) error {
		return c.SetReadDeadline(time.Now().Add(pongWait))
	})

	// outbound serialises every write through the single writer goroutine.
	outbound := make(chan serverFrame, 64)
	done := make(chan struct{})

	// Reader goroutine: processes client commands until the socket closes.
	go func() {
		defer close(done)
		for {
			var frame clientFrame
			if err := c.ReadJSON(&frame); err != nil {
				return
			}
			switch frame.Type {
			case cmdSubscribe:
				valid := make([]string, 0, len(frame.Topics))
				for _, t := range frame.Topics {
					if domain.ValidTopic(t) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					trySend(outbound, serverFrame{Type: frameError, Message: "no valid topics supplied"})
					continue
				}
				sub.Subscribe(valid...)
				trySend(outbound, serverFrame{Type: frameSubscribed, Topics: valid})

				// Replay history from the client's cursor before live events
				// resume, so a reconnect leaves no hole in the timeline.
				if frame.AfterSeq >= 0 {
					h.replay(outbound, valid, frame.AfterSeq)
				}

			case cmdUnsubscribe:
				sub.Unsubscribe(frame.Topics...)

			case cmdPing:
				trySend(outbound, serverFrame{Type: framePong})
			}
		}
	}()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	// Writer loop: the only place that writes to the socket.
	for {
		select {
		case <-done:
			return

		case frame := <-outbound:
			if !h.write(c, frame) {
				return
			}

		case e, ok := <-sub.Events():
			if !ok {
				return
			}
			if !h.write(c, serverFrame{Type: frameEvent, Topic: e.Topic, Seq: e.Seq, Event: e}) {
				return
			}

		case gap, ok := <-sub.Gaps():
			if !ok {
				return
			}
			if !h.write(c, serverFrame{Type: frameGap, Topic: gap.Topic, From: gap.From, To: gap.To}) {
				return
			}

		case <-ticker.C:
			_ = c.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
				h.log.Debug("websocket ping failed", "user", principal.Email, "error", err)
				return
			}
		}
	}
}

func (h *Hub) write(c *websocket.Conn, frame serverFrame) bool {
	payload, err := json.Marshal(frame)
	if err != nil {
		h.log.Warn("failed to encode websocket frame", "type", frame.Type, "error", err)
		return true
	}
	_ = c.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
		return false
	}
	return true
}

// replay streams durable history for the requested topics from a cursor.
func (h *Hub) replay(out chan<- serverFrame, topics []string, afterSeq int64) {
	for _, topic := range topics {
		runID, ok := runIDFromTopic(topic)
		if !ok {
			continue
		}
		// Replay is bounded: a client reconnecting after a long absence gets a
		// page, then continues live. Unbounded replay would let one reconnect
		// pull an entire build history into memory.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		events, err := h.events.Query(ctx, port.EventQuery{
			RunID: runID, AfterSeq: afterSeq, Limit: 500,
		})
		cancel()
		if err != nil {
			h.log.Warn("event replay failed", "topic", topic, "error", err)
			continue
		}
		for _, e := range events {
			trySend(out, serverFrame{Type: frameEvent, Topic: e.Topic, Seq: e.Seq, Event: e})
		}
	}
}

func runIDFromTopic(topic string) (domain.ID, bool) {
	const prefix = "run:"
	if len(topic) <= len(prefix) || topic[:len(prefix)] != prefix {
		return domain.Nil, false
	}
	id, err := domain.ParseID(topic[len(prefix):])
	if err != nil {
		return domain.Nil, false
	}
	return id, true
}

// trySend enqueues without blocking: a client too slow to drain its own control
// frames is already being handled by the bus's gap mechanism.
func trySend(out chan<- serverFrame, frame serverFrame) {
	select {
	case out <- frame:
	default:
	}
}
