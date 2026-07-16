package state

import "testing"

// TestTableCoversAllStatesWithLane asserts every state appears in the table and
// that stage states carry a real lane while waypoints/terminal do not.
func TestTableCoversAllStatesWithLane(t *testing.T) {
	all := All()
	if len(all) != len(table) {
		t.Fatalf("All() returned %d states, table has %d", len(all), len(table))
	}
	seen := map[State]bool{}
	for i, s := range all {
		if seen[s] {
			t.Fatalf("state %q duplicated in All()", s)
		}
		seen[s] = true
		if Order(s) != i {
			t.Errorf("state %q at index %d has order %d", s, i, Order(s))
		}
	}
	stages := map[State]bool{
		Inspecting: true, MarkersNormalizing: true, Splitting: true, ASR: true,
		Sanitizing: true, QASweep: true, QAAdjudicating: true, Retranscribing: true,
		SpellingResearch: true, Correcting: true, FactPass: true, Synthesizing: true,
		Validating: true, Auditing: true, Fixing: true, Contributing: true,
	}
	for s := range table {
		if stages[s] {
			if LaneOf(s) == LaneNone {
				t.Errorf("stage %q must have a lane", s)
			}
			if !IsStage(s) {
				t.Errorf("stage %q must report IsStage", s)
			}
		} else if LaneOf(s) != LaneNone {
			t.Errorf("waypoint/terminal %q must have LaneNone, got %q", s, LaneOf(s))
		}
	}
}

// TestLanesAssigned pins the lane of each stage to the validated pipeline model.
func TestLanesAssigned(t *testing.T) {
	want := map[State]Lane{
		Inspecting: LaneMechanical, MarkersNormalizing: LaneAgent, Splitting: LaneMechanical,
		ASR: LaneASR, Sanitizing: LaneMechanical, QASweep: LaneMechanical,
		QAAdjudicating: LaneAgent, Retranscribing: LaneASR, SpellingResearch: LaneAgent,
		Correcting: LaneMechanical, FactPass: LaneAgent, Synthesizing: LaneAgent,
		Validating: LaneMechanical, Auditing: LaneAgent, Fixing: LaneAgent,
		Contributing: LaneMechanical,
	}
	for s, lane := range want {
		if LaneOf(s) != lane {
			t.Errorf("lane of %q = %q, want %q", s, LaneOf(s), lane)
		}
	}
}

// TestNextStateLegalPerTable drives every state through every branch and asserts
// the result is a declared successor (or the documented park exception).
func TestNextStateLegalPerTable(t *testing.T) {
	branches := []Outcome{
		{},
		{MarkersContiguous: true},
		{QAClean: true},
		{RetranscribeNeeded: true},
		{AuditPassed: true},
		{FixAttempts: MaxFixAttempts},
	}
	for _, s := range All() {
		if IsTerminal(s) {
			if _, _, err := NextState(s, Outcome{}); err == nil {
				t.Errorf("terminal %q should error on NextState", s)
			}
			continue
		}
		for _, o := range branches {
			next, status, err := NextState(s, o)
			if err != nil {
				t.Errorf("NextState(%q, %+v) error: %v", s, o, err)
				continue
			}
			// The park exception: auditing with the fix budget spent stays put.
			if s == Auditing && !o.AuditPassed && o.FixAttempts >= MaxFixAttempts {
				if next != Auditing || status != StatusNeedsAttention {
					t.Errorf("audit park: got (%q,%q), want (auditing,needs_attention)", next, status)
				}
				continue
			}
			if status != StatusNone {
				t.Errorf("NextState(%q,%+v) status = %q, want none", s, o, status)
			}
			if !legalNext(s, next) {
				t.Errorf("NextState(%q,%+v) = %q, not a declared successor %v", s, o, next, table[s].Next)
			}
		}
	}
}

// TestConditionalSkips checks the two skip branches route around the optional
// stages exactly as the pipeline requires.
func TestConditionalSkips(t *testing.T) {
	// Contiguous markers skip markers_normalizing.
	if n, _, _ := NextState(Inspecting, Outcome{MarkersContiguous: true}); n != Splitting {
		t.Errorf("contiguous markers: got %q, want splitting", n)
	}
	if n, _, _ := NextState(Inspecting, Outcome{}); n != MarkersNormalizing {
		t.Errorf("non-contiguous markers: got %q, want markers_normalizing", n)
	}
	// A clean qa_sweep skips adjudication (and thus retranscribe).
	if n, _, _ := NextState(QASweep, Outcome{QAClean: true}); n != SpellingResearch {
		t.Errorf("clean qa: got %q, want spelling_research", n)
	}
	if n, _, _ := NextState(QASweep, Outcome{}); n != QAAdjudicating {
		t.Errorf("dirty qa: got %q, want qa_adjudicating", n)
	}
	// Adjudication may loop back through retranscribe -> qa_sweep.
	if n, _, _ := NextState(QAAdjudicating, Outcome{RetranscribeNeeded: true}); n != Retranscribing {
		t.Errorf("adjudicate retranscribe: got %q, want retranscribing", n)
	}
	if n, _, _ := NextState(Retranscribing, Outcome{}); n != QASweep {
		t.Errorf("retranscribe loop: got %q, want qa_sweep", n)
	}
}

// TestFixLoopCappedThenParks walks the audit/fix loop and asserts it fixes up to
// MaxFixAttempts times, then parks needs_attention.
func TestFixLoopCappedThenParks(t *testing.T) {
	for attempts := 0; attempts < MaxFixAttempts; attempts++ {
		next, status, err := NextState(Auditing, Outcome{FixAttempts: attempts})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempts, err)
		}
		if next != Fixing || status != StatusNone {
			t.Fatalf("attempt %d: got (%q,%q), want (fixing,none)", attempts, next, status)
		}
		// A fix pass returns to validating.
		if n, _, _ := NextState(Fixing, Outcome{}); n != Validating {
			t.Fatalf("fixing should go to validating, got %q", n)
		}
	}
	// Budget spent: park.
	next, status, err := NextState(Auditing, Outcome{FixAttempts: MaxFixAttempts})
	if err != nil {
		t.Fatal(err)
	}
	if next != Auditing || status != StatusNeedsAttention {
		t.Fatalf("exhausted: got (%q,%q), want (auditing,needs_attention)", next, status)
	}
	// A passing audit always reaches ready regardless of prior attempts.
	if n, _, _ := NextState(Auditing, Outcome{AuditPassed: true, FixAttempts: MaxFixAttempts}); n != Ready {
		t.Fatalf("passing audit should reach ready, got %q", n)
	}
}

// TestHappyPathToDone follows the all-skips, first-pass-audit route end to end.
func TestHappyPathToDone(t *testing.T) {
	want := []State{
		Queued, Inspecting, Splitting, ASR, Sanitizing, QASweep,
		SpellingResearch, Correcting, FactPass, Synthesizing, Validating,
		Auditing, Ready, Contributing, Done,
	}
	cur := Queued
	got := []State{cur}
	for !IsTerminal(cur) {
		o := Outcome{MarkersContiguous: true, QAClean: true, AuditPassed: true}
		next, _, err := NextState(cur, o)
		if err != nil {
			t.Fatalf("from %q: %v", cur, err)
		}
		got = append(got, next)
		cur = next
	}
	if len(got) != len(want) {
		t.Fatalf("path %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestHoldsSeriesLock asserts the predicate is true for every state before Ready
// and false from Ready onward (the finished-for-lock-purposes boundary).
func TestHoldsSeriesLock(t *testing.T) {
	for _, s := range All() {
		want := Order(s) < Order(Ready)
		if HoldsSeriesLock(s) != want {
			t.Errorf("HoldsSeriesLock(%q) = %v, want %v", s, HoldsSeriesLock(s), want)
		}
	}
	// Spot-check the boundary explicitly.
	if !HoldsSeriesLock(Auditing) {
		t.Error("Auditing (pre-Ready) should hold the series lock")
	}
	for _, s := range []State{Ready, Contributing, Done} {
		if HoldsSeriesLock(s) {
			t.Errorf("%q (Ready or later) must not hold the series lock", s)
		}
	}
}

// TestCanStartGuards covers the status, waypoint, and series-lock gates - the
// allowed and denied cases for each.
func TestCanStartGuards(t *testing.T) {
	// Mechanical stage: startable regardless of the series lock.
	if !CanStart(Inspecting, StatusNone, false) {
		t.Error("mechanical stage should start without the series lock")
	}
	// Any non-none status blocks dispatch.
	for _, st := range []Status{StatusPaused, StatusNeedsAttention, StatusFailed} {
		if CanStart(Inspecting, st, true) {
			t.Errorf("status %q must block start", st)
		}
	}
	// Agent stage requires being lowest-in-series.
	if CanStart(FactPass, StatusNone, false) {
		t.Error("agent stage must not start when not lowest-in-series")
	}
	if !CanStart(FactPass, StatusNone, true) {
		t.Error("agent stage should start when lowest-in-series")
	}
	// Waypoints and terminal are not directly startable.
	for _, s := range []State{Queued, Ready, Done} {
		if CanStart(s, StatusNone, true) {
			t.Errorf("waypoint/terminal %q must not be startable", s)
		}
	}
}
