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
// chapter 9 (cross-segment only, allowed but not required).
func baseReport() *Report {
	return &Report{
		Chapters:          10,
		RetranscribeQueue: []int{2},
		TailRate:          []TailRateHit{{Chapter: 2, WPS: 9}},
		RepeatedRuns: []RepeatedRun{
			{Chapter: 5, Kind: KindMidChapter, Length: 4},
			{Chapter: 3, Kind: KindEndFade, Length: 3}, // benign, allowed not required
		},
		MultiLoop: []MultiLoopFinding{
			{Chapter: 8, Count: 6, MidChapter: true},
		},
		CrossSegment: []CrossSegmentHit{
			{Chapter: 9, Count: 6},
		},
	}
}

func fullPlan() *Plan {
	return &Plan{Entries: []PlanEntry{
		{Chapter: 2, Action: ActionRetranscribe, Reason: "wph outlier + tail rate"},
		{Chapter: 5, Action: ActionTailClip, Reason: "mid-chapter loop"},
		{Chapter: 8, Action: ActionAccept, Reason: "benign echo"},
	}}
}

func TestPlanValidate_Valid(t *testing.T) {
	if err := fullPlan().Validate(baseReport()); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	// An extra accept for an allowed-but-not-required chapter (cross-segment ch9) is OK.
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 9, Action: ActionAccept, Reason: "prose repeat, fine"})
	if err := p.Validate(baseReport()); err != nil {
		t.Fatalf("expected valid with allowed extra, got %v", err)
	}
}

func TestPlanValidate_MissingRequired(t *testing.T) {
	p := fullPlan()
	p.Entries = p.Entries[:2] // drop the ch8 entry
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "chapter 8") {
		t.Fatalf("expected missing-ch8 error, got %v", err)
	}
}

func TestPlanValidate_EntryForUnflagged(t *testing.T) {
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 99, Action: ActionAccept, Reason: "why"})
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "chapter 99") {
		t.Fatalf("expected unflagged-ch99 error, got %v", err)
	}
}

func TestPlanValidate_DuplicateEntry(t *testing.T) {
	p := fullPlan()
	p.Entries = append(p.Entries, PlanEntry{Chapter: 2, Action: ActionAccept, Reason: "dup"})
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "expected exactly one") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestPlanValidate_EmptyReason(t *testing.T) {
	p := fullPlan()
	p.Entries[0].Reason = "  "
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "empty reason") {
		t.Fatalf("expected empty-reason error, got %v", err)
	}
}

func TestPlanValidate_InvalidAction(t *testing.T) {
	p := fullPlan()
	p.Entries[0].Action = "delete"
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "invalid action") {
		t.Fatalf("expected invalid-action error, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecValid: a positive clip_start_sec on a tail_clip entry
// (chapter 5) is accepted, and a zero/omitted value on any action stays valid.
func TestPlanValidate_ClipStartSecValid(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = 1180.5 // chapter 5 is the tail_clip entry
	if err := p.Validate(baseReport()); err != nil {
		t.Fatalf("expected valid clip_start_sec on a tail_clip entry, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecNegative: a negative clip_start_sec is rejected.
func TestPlanValidate_ClipStartSecNegative(t *testing.T) {
	p := fullPlan()
	p.Entries[1].ClipStartSec = -3
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "negative clip_start_sec") {
		t.Fatalf("expected negative-clip_start_sec error, got %v", err)
	}
}

// TestPlanValidate_ClipStartSecOnNonTailClip: clip_start_sec set on a non-tail_clip
// entry (chapter 2 is retranscribe) is rejected - only a tail_clip window is relocatable.
func TestPlanValidate_ClipStartSecOnNonTailClip(t *testing.T) {
	p := fullPlan()
	p.Entries[0].ClipStartSec = 100 // chapter 2 is the retranscribe entry
	err := p.Validate(baseReport())
	if err == nil || !strings.Contains(err.Error(), "clip_start_sec on a") {
		t.Fatalf("expected clip_start_sec-on-non-tail_clip error, got %v", err)
	}
}

func TestPlanValidate_NilReport(t *testing.T) {
	if err := fullPlan().Validate(nil); err == nil {
		t.Fatal("expected error for nil report")
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
