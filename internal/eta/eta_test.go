package eta

import (
	"math"
	"reflect"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// stagesOf extracts the ordered stage list of a book's remaining optimistic path.
func stagesOf(b Book, rates map[string]float64) []state.State {
	var out []state.State
	for _, s := range remainingSegments(b, rates) {
		out = append(out, s.stage)
	}
	return out
}

func TestObserveEWMA(t *testing.T) {
	tests := []struct {
		name    string
		stage   string
		wall    float64
		units   int
		current map[string]float64
		want    float64
		ok      bool
	}{
		{"seed prior, per-chapter", "splitting", 100, 10, nil, 0.3*10 + 0.7*4, true}, // observed 10, seed 4
		{"current rate blends", "splitting", 100, 10, map[string]float64{"splitting": 8}, 0.3*10 + 0.7*8, true},
		{"book stage one unit", "qa_sweep", 20, 1, nil, 0.3*20 + 0.7*10, true}, // observed 20, seed 10
		{"unknown stage skipped", "nope", 100, 1, nil, 0, false},
		{"zero units skipped", "asr", 100, 0, nil, 0, false},
		{"negative units skipped", "asr", 100, -3, nil, 0, false},
		{"zero wallclock skipped", "asr", 0, 5, nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Observe(tc.stage, tc.wall, tc.units, tc.current)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnitIsBook(t *testing.T) {
	book := []string{"inspecting", "markers_normalizing", "qa_sweep", "qa_adjudicating",
		"spelling_research", "correcting", "synthesizing", "validating", "auditing", "fixing", "contributing"}
	notBook := []string{"splitting", "asr", "sanitizing", "retranscribing", "fact_pass"}
	for _, s := range book {
		if !UnitIsBook(s) {
			t.Errorf("UnitIsBook(%q) = false, want true", s)
		}
	}
	for _, s := range notBook {
		if UnitIsBook(s) {
			t.Errorf("UnitIsBook(%q) = true, want false", s)
		}
	}
	if UnitIsBook("nonsense") {
		t.Error("UnitIsBook(unknown) = true, want false")
	}
}

// TestRemainingPathRouting asserts the optimistic remaining path from every state:
// the mainline skips the conditionals, and each off-mainline stage routes back.
func TestRemainingPathRouting(t *testing.T) {
	full := []state.State{state.Inspecting, state.Splitting, state.ASR, state.Sanitizing,
		state.QASweep, state.SpellingResearch, state.Correcting, state.FactPass,
		state.Synthesizing, state.Validating, state.Auditing, state.Contributing}
	tail := func(from state.State) []state.State {
		for i, s := range full {
			if s == from {
				return full[i:]
			}
		}
		return nil
	}
	cases := map[state.State][]state.State{
		state.Queued:             full,
		state.Inspecting:         full,
		state.MarkersNormalizing: append([]state.State{state.MarkersNormalizing}, tail(state.Splitting)...),
		state.Splitting:          tail(state.Splitting),
		state.ASR:                tail(state.ASR),
		state.Sanitizing:         tail(state.Sanitizing),
		state.QASweep:            tail(state.QASweep),
		state.QAAdjudicating:     append([]state.State{state.QAAdjudicating}, tail(state.SpellingResearch)...),
		state.Retranscribing:     append([]state.State{state.Retranscribing}, tail(state.QASweep)...),
		state.SpellingResearch:   tail(state.SpellingResearch),
		state.Correcting:         tail(state.Correcting),
		state.FactPass:           tail(state.FactPass),
		state.Synthesizing:       tail(state.Synthesizing),
		state.Validating:         tail(state.Validating),
		state.Auditing:           tail(state.Auditing),
		state.Fixing:             append([]state.State{state.Fixing}, tail(state.Validating)...),
		state.Ready:              {state.Contributing},
		state.Contributing:       {state.Contributing},
	}
	for from, want := range cases {
		b := Book{State: from}
		got := stagesOf(b, nil)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("path from %q =\n  %v\nwant\n  %v", from, got, want)
		}
	}
	// Terminal has no ETA and no path.
	if segs := stagesOf(Book{State: state.Done}, nil); segs != nil {
		t.Errorf("done path = %v, want nil", segs)
	}
}

func TestBookETANoETAConditions(t *testing.T) {
	for _, st := range []state.Status{state.StatusPaused, state.StatusNeedsAttention, state.StatusFailed} {
		if _, ok := BookETA(Book{State: state.ASR, Status: st, Chapters: 5}, nil); ok {
			t.Errorf("BookETA with status %q = ok, want no ETA", st)
		}
	}
	if _, ok := BookETA(Book{State: state.Done}, nil); ok {
		t.Error("BookETA(done) = ok, want no ETA")
	}
	if secs, ok := BookETA(Book{State: state.Contributing}, nil); !ok || secs != 1 {
		t.Errorf("BookETA(contributing) = %v,%v, want 1,true", secs, ok)
	}
}

func TestBookETASumsSeeds(t *testing.T) {
	// A queued book with 10 known chapters, all-seed rates.
	b := Book{State: state.Queued, Chapters: 10}
	// inspecting 5 + splitting 4*10 + asr 36*10 + sanitizing 1*10 + qa_sweep 10 +
	// spelling 600 + correcting 15 + fact_pass 8*900 + synth 700 + validating 10 +
	// auditing 700 + contributing 1.
	want := 5.0 + 40 + 360 + 10 + 10 + 600 + 15 + 7200 + 700 + 10 + 700 + 1
	got, ok := BookETA(b, nil)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("BookETA = %v (ok %v), want %v", got, ok, want)
	}
}

func TestUnknownChaptersDefault(t *testing.T) {
	// chapters 0, no progress -> the per-chapter stages assume DefaultChapters.
	b := Book{State: state.Splitting}
	segs := remainingSegments(b, nil)
	if segs[0].stage != state.Splitting {
		t.Fatalf("first segment = %q", segs[0].stage)
	}
	if want := float64(DefaultChapters) * 4; segs[0].seconds != want {
		t.Errorf("splitting seconds = %v, want %v (60 default chapters * seed 4)", segs[0].seconds, want)
	}
}

func TestFactPassChunkDefaultAndProgress(t *testing.T) {
	// No fact_pass progress row -> DefaultChunks.
	b := Book{State: state.FactPass, Chapters: 10}
	if got := remainingSegments(b, nil)[0].seconds; got != float64(DefaultChunks)*900 {
		t.Errorf("fact_pass default seconds = %v, want %v", got, float64(DefaultChunks)*900)
	}
	// A fact_pass progress row: current stage remaining = total - done.
	b.Progress = map[string]Progress{"fact_pass": {Done: 1, Total: 4}}
	if got := remainingSegments(b, nil)[0].seconds; got != 3*900 {
		t.Errorf("fact_pass remaining seconds = %v, want %v (3 chunks left)", got, 3*900)
	}
}

func TestCurrentStageRemainingFromProgress(t *testing.T) {
	// A resumed ASR: 7 of 10 chapters left drives the current-stage estimate; later
	// chapter stages still use the full chapter count.
	b := Book{
		State:    state.ASR,
		Chapters: 10,
		Progress: map[string]Progress{"asr": {Done: 3, Total: 10}},
	}
	segs := remainingSegments(b, nil)
	if segs[0].stage != state.ASR || segs[0].seconds != 7*36 {
		t.Fatalf("asr segment = %+v, want 7*36 seconds", segs[0])
	}
	// sanitizing (a later per-chapter stage) uses the FULL 10 chapters, not a remainder.
	for _, s := range segs {
		if s.stage == state.Sanitizing && s.seconds != 10*1 {
			t.Errorf("sanitizing seconds = %v, want 10 (full chapter count)", s.seconds)
		}
	}
}

func TestChaptersFromProgress(t *testing.T) {
	// No chapter-stage rows -> 0 (caller falls back to DefaultChapters).
	if got := ChaptersFromProgress(nil); got != 0 {
		t.Errorf("ChaptersFromProgress(nil) = %d, want 0", got)
	}
	// A non-chapter stage's progress does not count.
	if got := ChaptersFromProgress(map[string]Progress{"qa_sweep": {Done: 0, Total: 1}}); got != 0 {
		t.Errorf("ChaptersFromProgress(qa_sweep row) = %d, want 0", got)
	}
	// The largest Total across splitting/asr/sanitizing wins.
	got := ChaptersFromProgress(map[string]Progress{
		"splitting":  {Done: 40, Total: 40},
		"asr":        {Done: 3, Total: 42},
		"sanitizing": {Done: 0, Total: 0},
	})
	if got != 42 {
		t.Errorf("ChaptersFromProgress = %d, want 42 (max chapter-stage Total)", got)
	}
}

func TestRetranscribeSubsetFallback(t *testing.T) {
	// A retranscribing book with a known chapter count and NO progress row estimates the
	// ~4% flagged subset, not the whole book: 84 chapters -> round(3.36) = 3, seed 60s.
	b := Book{State: state.Retranscribing, Chapters: 84}
	segs := remainingSegments(b, nil)
	if len(segs) == 0 || segs[0].stage != state.Retranscribing {
		t.Fatalf("first segment = %+v, want retranscribing", segs)
	}
	if want := 3.0 * 60; segs[0].seconds != want {
		t.Errorf("retranscribing seconds = %v, want %v (~4%% of 84 chapters * seed 60)", segs[0].seconds, want)
	}
	// A progress row is authoritative: 5 flagged entries, 1 done -> 4 remaining * 60.
	b.Progress = map[string]Progress{"retranscribing": {Done: 1, Total: 5}}
	if got := remainingSegments(b, nil)[0].seconds; got != 4*60 {
		t.Errorf("retranscribing remaining seconds = %v, want %v (4 entries left)", got, 4*60)
	}
	// Unknown chapter count still yields at least 1 unit (min 1), never 0.
	if got := retranscribeSubset(0); got < 1 {
		t.Errorf("retranscribeSubset(0) = %d, want >= 1", got)
	}
}

func TestObservedRateOverridesSeed(t *testing.T) {
	b := Book{State: state.Splitting, Chapters: 5}
	rates := map[string]float64{"splitting": 10} // observed 10 s/chapter
	if got := remainingSegments(b, rates)[0].seconds; got != 5*10 {
		t.Errorf("splitting seconds = %v, want 50 (observed rate)", got)
	}
	// A zero/negative stored rate is ignored (falls back to the seed).
	if got := remainingSegments(b, map[string]float64{"splitting": 0})[0].seconds; got != 5*4 {
		t.Errorf("splitting seconds with zero rate = %v, want 20 (seed)", got)
	}
}
