// Package events is a Server-Sent Events hub: Publish fans a typed event out to
// all subscribers, assigns each a monotonically increasing id, and keeps the last
// N events in a ring buffer so a reconnecting client can replay everything it
// missed from its Last-Event-ID. Heartbeats are separate: they carry no id and
// are not buffered, so they keep the connection (and the UI liveness indicator)
// alive without polluting the replay stream. Slow subscribers whose buffer fills
// are evicted rather than allowed to block the publisher.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultRingSize is the default number of real events retained for replay.
const DefaultRingSize = 1024

// subChanBuffer bounds a subscriber's live-event backlog before eviction.
const subChanBuffer = 64

// HeartbeatType is the SSE event type used for keepalive/liveness frames.
const HeartbeatType = "heartbeat"

// Event is one server-sent event. A real event carries a nonzero monotonic ID
// and is retained for replay; a heartbeat carries ID 0 and is ephemeral.
type Event struct {
	ID   uint64
	Type string
	// BookID is the book this event concerns (0 = daemon-wide). It is carried for
	// the durable event log's book_id column and is NOT serialized onto the SSE
	// wire (the web payload keeps book_id inside Data). Set via PublishBook.
	BookID int64
	Data   json.RawMessage
}

// WriteSSE writes e in the text/event-stream wire format to w. The id line is
// emitted only for real events (ID != 0), so a client's Last-Event-ID advances
// only past durable events and never onto an ephemeral heartbeat.
func (e Event) WriteSSE(w io.Writer) error {
	if e.ID != 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", e.ID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", e.Type); err != nil {
		return err
	}
	data := e.Data
	if len(data) == 0 {
		data = []byte("{}")
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

// Subscription is a live feed of events for one connected client.
type Subscription struct {
	C   <-chan Event
	hub *Hub
	ch  chan Event
}

// Close unsubscribes and releases resources. Safe to call more than once.
func (s *Subscription) Close() { s.hub.remove(s) }

// Hub is the fan-out event bus.
type Hub struct {
	mu      sync.Mutex
	subs    map[*Subscription]struct{}
	ring    []Event
	ringCap int
	nextID  uint64
	persist func(Event) // optional durable sink; set via SetPersister
}

// SetPersister installs an optional sink invoked for every published (real,
// non-heartbeat) event after it has been assigned an id and fanned out. The hub
// stays the live fan-out; the sink is how the durable event log (internal/store)
// captures the same stream for later log views. It is invoked UNDER the hub lock,
// in id order, so the durable log observes events in the same order the ids were
// assigned - therefore the sink MUST be non-blocking (enqueue to a buffered
// channel and drop on overflow; do the actual DB write on a background goroutine)
// so a slow durable write never stalls publishers. Not safe to call concurrently
// with Publish; set it once at wiring time.
func (h *Hub) SetPersister(fn func(Event)) { h.persist = fn }

// Cursor returns the id of the newest real event. A caller can capture it before
// constructing a REST snapshot, then subscribe from that id to replay every event
// that raced with the snapshot.
func (h *Hub) Cursor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextID
}

// NewHub returns a hub retaining ringSize recent events for replay. A ringSize
// <= 0 uses DefaultRingSize.
func NewHub(ringSize int) *Hub {
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &Hub{
		subs:    map[*Subscription]struct{}{},
		ring:    make([]Event, 0, ringSize),
		ringCap: ringSize,
	}
}

// Publish assigns a new id to a daemon-wide (book-less) event, records it in the
// ring buffer, and delivers it to every subscriber. A subscriber whose buffer is
// full is evicted.
func (h *Hub) Publish(eventType string, payload any) error {
	return h.PublishBook(eventType, 0, payload)
}

// PublishBook is Publish for a book-scoped event: bookID is carried on the Event
// (for the durable log's book_id column) without changing the SSE wire payload.
func (h *Hub) PublishBook(eventType string, bookID int64, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.nextID++
	ev := Event{ID: h.nextID, Type: eventType, BookID: bookID, Data: data}
	h.ring = append(h.ring, ev)
	if len(h.ring) > h.ringCap {
		h.ring = h.ring[len(h.ring)-h.ringCap:]
	}
	h.fanout(ev)
	// Persist UNDER the lock so the durable sink observes events in id order. The
	// sink is required to be non-blocking (see SetPersister), so this never stalls.
	if h.persist != nil {
		h.persist(ev)
	}
	h.mu.Unlock()
	return nil
}

// Heartbeat delivers an ephemeral heartbeat event (no id, not buffered) to every
// subscriber. Used for keepalive and the UI liveness indicator.
func (h *Hub) Heartbeat() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fanout(NewHeartbeat())
}

// NewHeartbeat returns a fresh ephemeral heartbeat event (no id). Handlers use it
// to send an immediate liveness frame the moment a client connects.
func NewHeartbeat() Event {
	data, _ := json.Marshal(map[string]int64{"ts": time.Now().Unix()})
	return Event{Type: HeartbeatType, Data: data}
}

// fanout delivers ev to all subscribers, evicting any whose buffer is full.
// Caller holds the mutex.
func (h *Hub) fanout(ev Event) {
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			// Slow consumer: evict so it can never block the publisher.
			delete(h.subs, s)
			close(s.ch)
		}
	}
}

// Subscribe registers a new subscriber and atomically captures the events it
// missed. It returns the replay slice (every buffered event with id >
// lastEventID, oldest first) and the live Subscription; the caller writes the
// replay, then streams from Subscription.C. Capturing replay and registering
// under one lock guarantees no event is missed or duplicated at the seam.
func (h *Hub) Subscribe(lastEventID uint64) ([]Event, *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []Event
	for _, ev := range h.ring {
		if ev.ID > lastEventID {
			replay = append(replay, ev)
		}
	}
	ch := make(chan Event, subChanBuffer)
	sub := &Subscription{C: ch, hub: h, ch: ch}
	h.subs[sub] = struct{}{}
	return replay, sub
}

// remove unsubscribes sub and closes its channel (idempotent).
func (h *Hub) remove(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.ch)
	}
}

// SubscriberCount returns the number of live subscribers (for tests/metrics).
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// RunHeartbeat emits a heartbeat every interval until ctx is cancelled. Intended
// to run in its own goroutine for the lifetime of the server.
func (h *Hub) RunHeartbeat(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Heartbeat()
		}
	}
}
