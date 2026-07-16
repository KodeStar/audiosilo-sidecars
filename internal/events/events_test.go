package events

import (
	"bytes"
	"sync"
	"testing"
)

func TestReplayFromLastEventID(t *testing.T) {
	h := NewHub(0)
	for i := 0; i < 3; i++ {
		if err := h.Publish("tick", map[string]int{"n": i}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	// Subscribing with lastEventID=1 replays events 2 and 3 only.
	replay, sub := h.Subscribe(1)
	defer sub.Close()
	if len(replay) != 2 {
		t.Fatalf("replay len = %d, want 2", len(replay))
	}
	if replay[0].ID != 2 || replay[1].ID != 3 {
		t.Errorf("replay ids = %d,%d want 2,3", replay[0].ID, replay[1].ID)
	}
}

func TestReplayAllAndNone(t *testing.T) {
	h := NewHub(0)
	for i := 0; i < 3; i++ {
		_ = h.Publish("tick", i)
	}
	// lastEventID 0 replays everything.
	all, sub := h.Subscribe(0)
	sub.Close()
	if len(all) != 3 {
		t.Errorf("replay all len = %d, want 3", len(all))
	}
	// lastEventID past the max replays nothing.
	none, sub2 := h.Subscribe(3)
	sub2.Close()
	if len(none) != 0 {
		t.Errorf("replay none len = %d, want 0", len(none))
	}
}

func TestRingBufferDropsOldest(t *testing.T) {
	h := NewHub(4)
	for i := 0; i < 10; i++ { // ids 1..10, ring keeps last 4 (7,8,9,10)
		_ = h.Publish("tick", i)
	}
	replay, sub := h.Subscribe(0)
	sub.Close()
	if len(replay) != 4 {
		t.Fatalf("replay len = %d, want 4 (ring cap)", len(replay))
	}
	if replay[0].ID != 7 || replay[3].ID != 10 {
		t.Errorf("ring window ids = %d..%d, want 7..10", replay[0].ID, replay[3].ID)
	}
}

func TestLiveDeliveryToSubscriber(t *testing.T) {
	h := NewHub(0)
	replay, sub := h.Subscribe(0)
	defer sub.Close()
	if len(replay) != 0 {
		t.Fatalf("unexpected replay: %d", len(replay))
	}
	if err := h.Publish("hello", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	ev := <-sub.C
	if ev.Type != "hello" || ev.ID != 1 {
		t.Errorf("got %+v", ev)
	}
}

func TestSlowSubscriberEvicted(t *testing.T) {
	h := NewHub(0)
	_, sub := h.Subscribe(0)
	// Never drain sub.C; overflow the buffer -> eviction.
	for i := 0; i < subChanBuffer+10; i++ {
		_ = h.Publish("flood", i)
	}
	if h.SubscriberCount() != 0 {
		t.Fatalf("subscriber count = %d, want 0 (evicted)", h.SubscriberCount())
	}
	// Drain buffered events then observe the channel closed.
	for range sub.C { //nolint:revive // draining until close
	}
	// If we get here, the channel was closed by eviction.
}

func TestHeartbeatNotBuffered(t *testing.T) {
	h := NewHub(0)
	replay, sub := h.Subscribe(0)
	defer sub.Close()
	h.Heartbeat()
	ev := <-sub.C
	if ev.Type != HeartbeatType {
		t.Errorf("type = %q, want heartbeat", ev.Type)
	}
	if ev.ID != 0 {
		t.Errorf("heartbeat id = %d, want 0 (no id)", ev.ID)
	}
	// A heartbeat must not become replayable history.
	if len(replay) != 0 {
		t.Errorf("replay len = %d, want 0", len(replay))
	}
	replay2, sub2 := h.Subscribe(0)
	sub2.Close()
	if len(replay2) != 0 {
		t.Errorf("heartbeat leaked into ring: replay len = %d", len(replay2))
	}
}

func TestWriteSSEFormat(t *testing.T) {
	var buf bytes.Buffer
	ev := Event{ID: 5, Type: "hello", Data: []byte(`{"a":1}`)}
	if err := ev.WriteSSE(&buf); err != nil {
		t.Fatalf("WriteSSE: %v", err)
	}
	want := "id: 5\nevent: hello\ndata: {\"a\":1}\n\n"
	if buf.String() != want {
		t.Errorf("SSE = %q, want %q", buf.String(), want)
	}

	// Heartbeat: no id line.
	buf.Reset()
	hb := Event{Type: HeartbeatType, Data: []byte(`{"ts":1}`)}
	if err := hb.WriteSSE(&buf); err != nil {
		t.Fatalf("WriteSSE heartbeat: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("id:")) {
		t.Errorf("heartbeat SSE contains id line: %q", buf.String())
	}
}

// TestPersisterReceivesRealEventsOnly verifies the durable sink sees every
// published real event (with its assigned id) but never a heartbeat.
func TestPersisterReceivesRealEventsOnly(t *testing.T) {
	h := NewHub(8)
	var got []Event
	h.SetPersister(func(ev Event) { got = append(got, ev) })

	if err := h.Publish("book.state", map[string]string{"state": "asr"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := h.Publish("queue.stats", map[string]int{"queued": 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	h.Heartbeat() // must NOT reach the persister

	if len(got) != 2 {
		t.Fatalf("persister saw %d events, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Type != "book.state" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].ID != 2 || got[1].Type != "queue.stats" {
		t.Errorf("event 1 = %+v", got[1])
	}
}

// TestPersisterSeesEventsInIDOrderUnderConcurrency verifies the sink observes
// events strictly in id order even when many goroutines Publish concurrently -
// the guarantee that lets an async persister enqueue in a stable order. The sink
// runs under the hub lock, so the id sequence it sees must be gap-free 1..N.
func TestPersisterSeesEventsInIDOrderUnderConcurrency(t *testing.T) {
	h := NewHub(0)
	var mu sync.Mutex
	var ids []uint64
	h.SetPersister(func(ev Event) {
		mu.Lock()
		ids = append(ids, ev.ID)
		mu.Unlock()
	})

	const goroutines, per = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = h.Publish("tick", i)
			}
		}()
	}
	wg.Wait()

	if len(ids) != goroutines*per {
		t.Fatalf("persisted %d events, want %d", len(ids), goroutines*per)
	}
	for i, id := range ids {
		if id != uint64(i+1) {
			t.Fatalf("id at position %d = %d, want %d (order not preserved)", i, id, i+1)
		}
	}
}

// TestPublishBookCarriesBookID verifies the book scope rides on the Event for the
// durable log without leaking onto the SSE wire payload.
func TestPublishBookCarriesBookID(t *testing.T) {
	h := NewHub(0)
	var got Event
	h.SetPersister(func(ev Event) { got = ev })
	if err := h.PublishBook("book.state", 42, map[string]string{"state": "asr"}); err != nil {
		t.Fatalf("PublishBook: %v", err)
	}
	if got.BookID != 42 {
		t.Errorf("BookID = %d, want 42", got.BookID)
	}
	if bytes.Contains(got.Data, []byte("BookID")) {
		t.Errorf("BookID leaked into wire payload: %s", got.Data)
	}
}
