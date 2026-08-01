package scheduler

import (
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// TestPurgeAllowedStates pins which states may release a book's scratch. The
// allowed/denied split is load-bearing twice over: purging a RUNNING book would pull
// a stage's inputs out from under it, while refusing a PARKED one leaves an ebook
// holding its copyrighted source text for as long as the park lasts - and
// needs_attention waits on a human, so that can be weeks.
func TestPurgeAllowedStates(t *testing.T) {
	cases := []struct {
		name        string
		book        store.Book
		wantAllowed bool
	}{
		{"done", store.Book{State: string(state.Done)}, true},
		{"paused", store.Book{State: string(state.FactPass), Status: string(state.StatusPaused)}, true},
		{"failed", store.Book{State: string(state.FactPass), Status: string(state.StatusFailed)}, true},
		{"parked for a human", store.Book{State: string(state.ChapterMapping), Status: string(state.StatusNeedsAttention)}, true},
		{"running", store.Book{State: string(state.FactPass)}, false},
		{"queued", store.Book{State: string(state.Queued)}, false},
	}
	for _, c := range cases {
		if got := purgeAllowed(c.book); got != c.wantAllowed {
			t.Errorf("purgeAllowed(%s) = %v, want %v", c.name, got, c.wantAllowed)
		}
	}
}
