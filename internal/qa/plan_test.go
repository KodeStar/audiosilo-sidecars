package qa

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlan writes p to workDir/qa_plan.json for tests, mirroring the harvested
// production artifact (pretty JSON + trailing newline). Production writes the plan
// via agent.Harvest, so this lives only in the test.
func writePlan(t *testing.T, workDir string, p *Plan) {
	t.Helper()
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, PlanFile), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// baseReport builds a report flagging: chapter 2 (retranscribe queue + tail rate),
// chapter 5 (mid-chapter repeated run), chapter 8 (mid-chapter multi-loop), and
// chapter 3 (end-fade requiring adjudication), and chapter 9 (cross-segment only,
// allowed but not required).
func baseReport() *Report {
	return &Report{
		Chapters:          10,
		RetranscribeQueue: []int{2},
		TailRate:          []TailRateHit{{Chapter: 2, WPS: 9}},
		RepeatedRuns: []RepeatedRun{
			{Chapter: 5, Kind: KindMidChapter, Length: 4},
			{Chapter: 3, Kind: KindEndFade, Length: 3},
		},
		MultiLoop: []MultiLoopFinding{
			{Chapter: 8, Count: 6, MidChapter: true},
		},
		CrossSegment: []CrossSegmentHit{
			{Chapter: 9, Count: 6},
		},
	}
}

// baseDurations gives every flagged chapter a generous audio length so the clip-window
// bound check in Validate never rejects the in-range values the other cases use (they go
// up to ~1205s). The out-of-range cases pass their own tight durations map.
func baseDurations() map[int]float64 {
	return map[int]float64{2: 2000, 3: 2000, 5: 2000, 8: 2000, 9: 2000}
}

func fullPlan() *Plan {
	return &Plan{Entries: []PlanEntry{
		{Chapter: 2, Action: ActionRetranscribe, Reason: "wph outlier + tail rate"},
		{Chapter: 5, Action: ActionTailClip, Reason: "mid-chapter loop"},
		{Chapter: 8, Action: ActionAccept, Reason: "benign echo"},
		{Chapter: 3, Action: ActionTailClip, Reason: "repeated terminal suffix"},
	}}
}

func TestPlanValidate_Valid(t *testing.T) {
	if err := fullPlan().Validate(baseReport(), baseDurations()); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	// An extra accept for an allowed-but-not-required chapter (cross-segment ch9) is OK.
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 9, Action: ActionAccept, Reason: "prose repeat, fine"})
	if err := p.Validate(baseReport(), baseDurations()); err != nil {
		t.Fatalf("expected valid with allowed extra, got %v", err)
	}
}

func TestPlanValidate_MissingRequired(t *testing.T) {
	p := fullPlan()
	p.Entries = append(p.Entries[:2], p.Entries[3]) // drop only the ch8 entry
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "chapter 8") {
		t.Fatalf("expected missing-ch8 error, got %v", err)
	}
}

func TestPlanValidate_EntryForUnflagged(t *testing.T) {
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 99, Action: ActionAccept, Reason: "why"})
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "chapter 99") {
		t.Fatalf("expected unflagged-ch99 error, got %v", err)
	}
}

func TestPlanValidate_DuplicateEntry(t *testing.T) {
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 2, Action: ActionAccept, Reason: "dup"})
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "expected exactly one") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestPlanValidate_EmptyReason(t *testing.T) {
	p := fullPlan()
	p.Entries[0].Reason = "  "
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "empty reason") {
		t.Fatalf("expected empty-reason error, got %v", err)
	}
}

func TestPlanValidate_InvalidAction(t *testing.T) {
	p := fullPlan()
	p.Entries[0].Action = "delete"
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "invalid action") {
		t.Fatalf("expected invalid-action error, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecValid: a positive clip_start_sec on a tail_clip entry
// (chapter 5) is accepted, and a zero/omitted value on any action stays valid.
func TestPlanValidate_ClipStartSecValid(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = 1180.5 // chapter 5 is the tail_clip entry
	if err := p.Validate(baseReport(), baseDurations()); err != nil {
		t.Fatalf("expected valid clip_start_sec on a tail_clip entry, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecNegative: a negative clip_start_sec is rejected.
func TestPlanValidate_ClipStartSecNegative(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = -3
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "negative clip_start_sec") {
		t.Fatalf("expected negative-clip_start_sec error, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecOnNonTailClip: clip_start_sec set on a non-tail_clip
// entry (chapter 2 is retranscribe) is rejected - only a tail_clip window is relocatable.
func TestPlanValidate_ClipStartSecOnNonTailClip(t *testing.T) {
	p := fullPlan()
	p.Entries[0].ClipStartSec = 100 // chapter 2 is the retranscribe entry
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "clip_start_sec on a") {
		t.Fatalf("expected clip_start_sec-on-non-tail_clip error, got %v", err)
	}
}

// TestPlanValidate_MidClipValid: a mid_clip entry with a bounded [start, end] window
// (end > start > 0) on a flagged chapter is accepted. Chapter 5 (a mid-chapter run) is
// re-dispositioned as a mid_clip.
func TestPlanValidate_MidClipValid(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1180, ClipEndSec: 1205}
	if err := p.Validate(baseReport(), baseDurations()); err != nil {
		t.Fatalf("expected a valid mid_clip entry, got %v", err)
	}
}

// TestPlanValidate_MidClipMissingEnd: a mid_clip with a start but no clip_end_sec (0) is
// rejected - the window is unbounded.
func TestPlanValidate_MidClipMissingEnd(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1180}
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "clip_end_sec") {
		t.Fatalf("expected a missing-clip_end_sec error, got %v", err)
	}
}

// TestPlanValidate_MidClipEndNotAfterStart: a mid_clip whose clip_end_sec is not strictly
// past clip_start_sec is rejected (an empty or inverted window).
func TestPlanValidate_MidClipEndNotAfterStart(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1200, ClipEndSec: 1180}
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "must be greater than clip_start_sec") {
		t.Fatalf("expected an end<=start error, got %v", err)
	}
}

// TestPlanValidate_MidClipMissingStart: a mid_clip with an end but no clip_start_sec (0)
// is rejected - the window has no start.
func TestPlanValidate_MidClipMissingStart(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipEndSec: 1200}
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "no clip_start_sec > 0") {
		t.Fatalf("expected a missing-clip_start_sec error, got %v", err)
	}
}

// TestPlanValidate_MidClipNegativeEnd: a negative clip_end_sec is rejected before the
// window-bounds checks.
func TestPlanValidate_MidClipNegativeEnd(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1180, ClipEndSec: -5}
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "negative clip_end_sec") {
		t.Fatalf("expected a negative-clip_end_sec error, got %v", err)
	}
}

// TestPlanValidate_ClipEndSecOnNonMidClip: clip_end_sec set on a non-mid_clip entry
// (chapter 5 is a tail_clip in fullPlan) is rejected - only mid_clip cuts a bounded window.
func TestPlanValidate_ClipEndSecOnNonMidClip(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipEndSec = 1200 // chapter 5 is the tail_clip entry
	err := p.Validate(baseReport(), baseDurations())
	if err == nil || !strings.Contains(err.Error(), "clip_end_sec on a") {
		t.Fatalf("expected a clip_end_sec-on-non-mid_clip error, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecOnMidClip: clip_start_sec is allowed on a mid_clip entry
// (it starts the interior window), the relaxation of the tail_clip-only rule.
func TestPlanValidate_ClipStartSecOnMidClip(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1180, ClipEndSec: 1205}
	if err := p.Validate(baseReport(), baseDurations()); err != nil {
		t.Fatalf("expected clip_start_sec allowed on a mid_clip, got %v", err)
	}
}

func TestPlanValidate_NilReport(t *testing.T) {
	if err := fullPlan().Validate(nil, nil); err == nil {
		t.Fatal("expected error for nil report")
	}
}

// TestPlanValidate_TailClipInRangeDuration: a tail_clip clip_start_sec comfortably below
// the chapter duration is accepted when a durations map is supplied.
func TestPlanValidate_TailClipInRangeDuration(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = 1700 // chapter 5 is the tail_clip entry
	durs := map[int]float64{2: 2000, 3: 2000, 5: 1720.296, 8: 2000, 9: 2000}
	if err := p.Validate(baseReport(), durs); err != nil {
		t.Fatalf("expected an in-range tail_clip to validate, got %v", err)
	}
}

// TestPlanValidate_TailClipOverDuration reproduces the production incident: a tail_clip
// clip_start_sec of 1752 (an absolute source-file timestamp) supplied for a 1720.296s
// chapter is rejected, and the error names the chapter and the duration.
func TestPlanValidate_TailClipOverDuration(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = 1752 // chapter 5 is the tail_clip entry
	durs := map[int]float64{2: 2000, 3: 2000, 5: 1720.296, 8: 2000, 9: 2000}
	err := p.Validate(baseReport(), durs)
	if err == nil {
		t.Fatal("expected an out-of-range tail_clip clip_start_sec to be rejected")
	}
	for _, want := range []string{"chapter 5", "1752", "1720.3", "out of range"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestPlanValidate_MidClipStartOverDuration: a mid_clip whose clip_start_sec is at/past
// the chapter duration is rejected.
func TestPlanValidate_MidClipStartOverDuration(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1730, ClipEndSec: 1740}
	durs := map[int]float64{2: 2000, 3: 2000, 5: 1720.296, 8: 2000, 9: 2000}
	err := p.Validate(baseReport(), durs)
	if err == nil || !strings.Contains(err.Error(), "clip_start_sec") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected an out-of-range mid_clip clip_start_sec error, got %v", err)
	}
}

// TestPlanValidate_MidClipEndOverDuration: a mid_clip whose clip_end_sec runs well past
// the chapter duration (beyond the small tolerance) is rejected.
func TestPlanValidate_MidClipEndOverDuration(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1700, ClipEndSec: 1752}
	durs := map[int]float64{2: 2000, 3: 2000, 5: 1720.296, 8: 2000, 9: 2000}
	err := p.Validate(baseReport(), durs)
	if err == nil || !strings.Contains(err.Error(), "clip_end_sec") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected an out-of-range mid_clip clip_end_sec error, got %v", err)
	}
}

// TestPlanValidate_MidClipEndWithinTolerance: a mid_clip clip_end_sec a fraction past the
// manifest duration (segment end times can round slightly past it) is accepted.
func TestPlanValidate_MidClipEndWithinTolerance(t *testing.T) {
	p := fullPlan()
	p.Entries[1] = PlanEntry{Chapter: 5, Action: ActionMidClip, Reason: "interior loop", ClipStartSec: 1700, ClipEndSec: 1721}
	durs := map[int]float64{2: 2000, 3: 2000, 5: 1720.296, 8: 2000, 9: 2000}
	if err := p.Validate(baseReport(), durs); err != nil {
		t.Fatalf("expected a near-end mid_clip window within tolerance to validate, got %v", err)
	}
}

// TestPlanValidate_MissingDurationSkipsBound: a chapter absent from the durations map (or
// with a zero duration) skips the bound check, so an otherwise-valid plan validates even
// with a large clip_start_sec - the guard is defensive, never a hard requirement.
func TestPlanValidate_MissingDurationSkipsBound(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = 99999 // chapter 5 tail_clip, absurd value
	// durations omits chapter 5 entirely, and gives chapter 5's peers zero.
	durs := map[int]float64{2: 0}
	if err := p.Validate(baseReport(), durs); err != nil {
		t.Fatalf("expected a missing duration to skip the bound check, got %v", err)
	}
	// An explicit zero duration for the chapter likewise skips the check.
	if err := p.Validate(baseReport(), map[int]float64{5: 0}); err != nil {
		t.Fatalf("expected a zero duration to skip the bound check, got %v", err)
	}
}

// TestClipStartInRange pins the shared clip-window predicate's boundary and zero-duration
// semantics: reject at or past (duration - ClipStartFloorSec); an unknown (<= 0) duration is
// always in range. The validator, the three internal/repair guards, and the retranscribe
// dispatch pre-check all key on this.
func TestClipStartInRange(t *testing.T) {
	const floor = ClipStartFloorSec
	cases := []struct {
		name        string
		start, dur  float64
		wantInRange bool
	}{
		{"well inside", 10, 40, true},
		{"just below boundary", 40 - floor - 0.1, 40, true},
		{"exactly at boundary is rejected", 40 - floor, 40, false},
		{"past the end", 50, 40, false},
		{"zero duration is unbounded", 9999, 0, true},
		{"negative duration is unbounded", 9999, -1, true},
		{"zero start in a real chapter", 0, 40, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClipStartInRange(tc.start, tc.dur); got != tc.wantInRange {
				t.Errorf("ClipStartInRange(%.2f, %.2f) = %v, want %v", tc.start, tc.dur, got, tc.wantInRange)
			}
		})
	}
}

func TestPlanRetranscribeNeeded(t *testing.T) {
	if !fullPlan().RetranscribeNeeded() {
		t.Error("expected RetranscribeNeeded=true (has retranscribe/tail_clip)")
	}
	allAccept := &Plan{Entries: []PlanEntry{{Chapter: 2, Action: ActionAccept, Reason: "ok"}}}
	if allAccept.RetranscribeNeeded() {
		t.Error("expected RetranscribeNeeded=false when all accept")
	}
	// A mid_clip is repair work (Action != accept), so it counts as non-accept.
	midOnly := &Plan{Entries: []PlanEntry{{Chapter: 5, Action: ActionMidClip, Reason: "loop", ClipStartSec: 10, ClipEndSec: 20}}}
	if !midOnly.RetranscribeNeeded() || len(midOnly.NonAcceptEntries()) != 1 {
		t.Error("expected a mid_clip to count as non-accept repair work")
	}
}

// TestFlaggedAndAllowedChapters pins the two disposition sets: FlaggedChapters is the
// REQUIRED subset (retranscribe queue + tail rate + all repeated runs + mid-chapter
// multi-loops), while AllowedChapters is the full surface (adds cross/within-segment
// hits). qa_adjudicating stages every AllowedChapters transcript so the agent can verify
// and accept an allowed-but-not-flagged chapter against its real text.
func TestFlaggedAndAllowedChapters(t *testing.T) {
	rep := baseReport()
	if got, want := FlaggedChapters(rep), []int{2, 3, 5, 8}; !equalInts(got, want) {
		t.Errorf("FlaggedChapters = %v, want %v", got, want)
	}
	// AllowedChapters is a superset adding cross-segment ch9, sorted.
	if got, want := AllowedChapters(rep), []int{2, 3, 5, 8, 9}; !equalInts(got, want) {
		t.Errorf("AllowedChapters = %v, want %v", got, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWriteLoadPlanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := fullPlan()
	p.Notes = "adjudicated round 1"
	writePlan(t, dir, p)
	got, err := LoadPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Notes != p.Notes || len(got.Entries) != len(p.Entries) {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Entries[0].Action != ActionRetranscribe {
		t.Errorf("action = %q", got.Entries[0].Action)
	}
}

func TestLoadReport_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	rep := baseReport()
	if err := WriteReport(dir, rep); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RetranscribeQueue) != 1 || got.RetranscribeQueue[0] != 2 {
		t.Errorf("retranscribe queue = %v", got.RetranscribeQueue)
	}
	if len(got.TailRate) != 1 || got.TailRate[0].Chapter != 2 {
		t.Errorf("tail rate = %v", got.TailRate)
	}
	// Sanity: the written file is where LoadReport looks.
	if _, err := LoadReport(filepath.Dir(dir)); err == nil {
		t.Error("expected LoadReport to fail in a dir without the report")
	}
}
