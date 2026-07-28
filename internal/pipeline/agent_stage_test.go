package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/agent/prompts"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- shared seed helpers ---

func writeManifestStruct(t *testing.T, work string, m audio.Manifest) {
	t.Helper()
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func seedProbe(t *testing.T, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, audio.ProbeName), []byte(`{"chapters":[]}`), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

func seedNormalized(t *testing.T, work string, tr transcript.Transcript) {
	t.Helper()
	if err := transcript.WriteNormalized(filepath.Join(work, transcript.JSONDir), tr); err != nil {
		t.Fatalf("seed normalized ch%d: %v", tr.Chapter, err)
	}
}

func markerChapters(nums ...int) []audio.Chapter {
	chs := make([]audio.Chapter, 0, len(nums))
	for i, n := range nums {
		chs = append(chs, audio.Chapter{Chapter: n, Start: float64(i * 2), End: float64(i*2 + 2), Duration: 2})
	}
	return chs
}

// contiguousDraftManifest is a valid 1,2,3 markers manifest an agent might produce.
func correctedManifest() audio.Manifest {
	return audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)}
}

// --- markers_normalizing ---

func TestMarkersNormalizeHappyPath(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	// A non-contiguous draft (1,2,4) - the reason the book reached this stage.
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "excluded opening credits"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 120, Output: 60, CostUSD: 0.02, Turns: 2}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	// The corrected, contiguous manifest replaced the draft.
	m, err := audio.ReadManifest(work)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !audio.Contiguous(m.Chapters) || len(m.Chapters) != 3 {
		t.Errorf("manifest not the corrected contiguous map: %+v", m.Chapters)
	}
	if !scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("markers sentinel missing")
	}
	assertUsageMetrics(t, res.Metrics, "sonnet", 120, 60)
	// The agent stage requested the routed model.
	if r, ok := fake.lastRequest(string(state.MarkersNormalizing)); !ok || r.Model != "sonnet" || r.Web {
		t.Errorf("agent request model=%q web=%v, want sonnet/false", r.Model, r.Web)
	}
}

func TestMarkersNormalizeEmptyLegacyDraftReparsesProbeWithoutAgent(t *testing.T) {
	work := t.TempDir()
	probe := `{
		"format":{"duration":"180.000","tags":{"title":"Mageling"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"Opening Credits"}},
			{"start_time":"10.000","end_time":"90.000","tags":{"title":"Chapter: 1 – New Beginnings"}},
			{"start_time":"90.000","end_time":"170.000","tags":{"title":"Chapter: 2 — The Road"}},
			{"start_time":"170.000","end_time":"180.000","tags":{"title":"End Credits"}}
		]
	}`
	if err := os.WriteFile(filepath.Join(work, audio.ProbeName), []byte(probe), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/books/mageling.m4b", Title: "Mageling", Style: audio.StyleMarkers, Duration: 180})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, _ agent.Request, _ int) (agent.Result, error) {
		t.Fatal("deterministically reparsable markers must not invoke an agent")
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Mageling", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	if fake.count(string(state.MarkersNormalizing)) != 0 {
		t.Fatalf("agent ran %d times, want zero", fake.count(string(state.MarkersNormalizing)))
	}
	m, err := audio.ReadManifest(work)
	if err != nil || !audio.Contiguous(m.Chapters) || m.ChapterCount != 2 {
		t.Fatalf("recovered manifest = %+v err=%v", m, err)
	}
	if !scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Fatal("markers sentinel missing")
	}
	var metrics map[string]any
	if err := json.Unmarshal(res.Metrics, &metrics); err != nil || metrics["deterministic_reparse"] != true {
		t.Fatalf("metrics = %s err=%v", res.Metrics, err)
	}
}

// TestMarkersNormalizeReparseStillConsultsAgentWhenAudioIsUnmapped closes the back door
// in the free-recovery path. The reparse is a shortcut PAST the agent, so it has to clear
// the same bar routing does: a re-derived map that numbers 1..N perfectly while dropping
// an interlude must NOT complete the stage, or the book goes straight to split with a hole
// in it - exactly the failure this stage exists to catch.
func TestMarkersNormalizeReparseStillConsultsAgentWhenAudioIsUnmapped(t *testing.T) {
	work := t.TempDir()
	// Chapters 1-3 parse and number cleanly; the 1000s Interlude between 2 and 3 does not.
	writeProbeJSON(t, work, `{
		"format":{"duration":"5000.000","tags":{"title":"Holed"}},
		"chapters":[
			{"start_time":"20.000","end_time":"1000.000","tags":{"title":"Chapter 1"}},
			{"start_time":"1000.000","end_time":"2000.000","tags":{"title":"Chapter 2"}},
			{"start_time":"2000.000","end_time":"3000.000","tags":{"title":"Interlude"}},
			{"start_time":"3000.000","end_time":"4980.000","tags":{"title":"Chapter 3"}}
		]
	}`)
	writeManifestStruct(t, work, audio.Manifest{Source: "/books/holed.m4b", Title: "Holed", Style: audio.StyleMarkers, Duration: 5000})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, "verdict.json", markerVerdict{Confident: false, Reason: "declined for the test"})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Holed", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err == nil {
		t.Fatal("stage completed on a reparse that leaves an Interlude unmapped")
	}
	if fake.count(string(state.MarkersNormalizing)) == 0 {
		t.Error("the agent was never consulted; the reparse took the free-completion shortcut")
	}
}

func TestValidateMarkersManifestRejectsNumberAliasExplicitly(t *testing.T) {
	out := t.TempDir()
	raw := `{
		"source":"/x/book.m4b","style":"markers","duration":2,"chapter_count":1,
		"chapters":[{"number":1,"start":0,"end":2,"duration":2}]
	}`
	if err := os.WriteFile(filepath.Join(out, audio.ManifestName), []byte(raw), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	draft := audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 2}
	markers := []audio.Marker{{Title: "Chapter 1", Start: 0, End: 2}}
	err := validateMarkersManifest(out, draft, nil, markers, 2, nil)
	if err == nil || !strings.Contains(err.Error(), `unknown field "number"`) {
		t.Fatalf("validation error = %v, want explicit unknown number field", err)
	}
}

// TestValidateMarkersManifestRejectsUndeclaredNarration is the agent-side half of the
// coverage contract. Nine books had an interlude dropped by an agent that WAS consulted
// and answered confidently: the numbering contract it was checked against had nothing to
// say about audio left out of the map. Now a drop has to be declared to be accepted.
func TestValidateMarkersManifestRejectsUndeclaredNarration(t *testing.T) {
	// A map that skips a 1000s interlude between chapters 2 and 3 - perfectly numbered.
	write := func(t *testing.T) string {
		t.Helper()
		out := t.TempDir()
		raw := `{
			"source":"/x/book.m4b","style":"markers","duration":5000,"chapter_count":3,
			"chapters":[
				{"chapter":1,"start":20,"end":1000,"duration":980},
				{"chapter":2,"start":1000,"end":2000,"duration":1000},
				{"chapter":3,"start":3000,"end":4980,"duration":1980}]
		}`
		if err := os.WriteFile(filepath.Join(out, audio.ManifestName), []byte(raw), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
		return out
	}
	draft := audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 5000}
	markers := []audio.Marker{
		{Title: "Opening Credits", Start: 0, End: 20},
		{Title: "Chapter 1", Start: 20, End: 1000},
		{Title: "Chapter 2", Start: 1000, End: 2000},
		{Title: "Interlude", Start: 2000, End: 3000},
		{Title: "Chapter 3", Start: 3000, End: 4980},
		{Title: "End Credits", Start: 4980, End: 5000},
	}

	err := validateMarkersManifest(write(t), draft, nil, markers, 5000, nil)
	if err == nil {
		t.Fatal("a map that silently drops a 1000s Interlude was accepted")
	}
	if !strings.Contains(err.Error(), "Interlude") {
		t.Errorf("error = %v, want it to name the dropped Interlude so the retry can fix it", err)
	}

	// DECLARING the exclusion is what makes it acceptable - that is the whole mechanism,
	// since a bundled preview of the NEXT book genuinely must be left out.
	declared := []markerExclusion{{Title: "Interlude", Start: 2000, End: 3000, Reason: "preview of another book"}}
	if err := validateMarkersManifest(write(t), draft, nil, markers, 5000, declared); err != nil {
		t.Errorf("a declared exclusion was still rejected: %v", err)
	}

	// Credits at the edges never need declaring; they are ordinary non-chapter audio.
	full := t.TempDir()
	raw := `{
		"source":"/x/book.m4b","style":"markers","duration":5000,"chapter_count":4,
		"chapters":[
			{"chapter":1,"start":20,"end":1000,"duration":980},
			{"chapter":2,"start":1000,"end":2000,"duration":1000},
			{"chapter":3,"start":2000,"end":3000,"duration":1000},
			{"chapter":4,"start":3000,"end":4980,"duration":1980}]
	}`
	if err := os.WriteFile(filepath.Join(full, audio.ManifestName), []byte(raw), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := validateMarkersManifest(full, draft, nil, markers, 5000, nil); err != nil {
		t.Errorf("a complete map was rejected over its 20s credits: %v", err)
	}
}

// TestAgentStageRateSampleExcludesBackoff drives markers_normalizing through a
// rate-limit backoff (first attempt rate-limited, second succeeds) with a short
// injected backoff, and asserts the reported RateSample charges only productive agent
// time: 1 unit, and Seconds well below the backoff the run actually slept through.
func TestAgentStageRateSampleExcludesBackoff(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	const backoff = 300 * time.Millisecond
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		if attempt == 1 {
			// Rate-limit the first attempt: RunWithBackoff sleeps `backoff`, then retries.
			return agent.Result{}, &agent.RateLimitError{Detail: "429"}
		}
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "ok"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	exe.backoff = []time.Duration{backoff} // tiny schedule so the test does not sleep for minutes
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	if fake.count(string(state.MarkersNormalizing)) != 2 {
		t.Fatalf("agent ran %d times, want 2 (one rate-limited + one success)", fake.count(string(state.MarkersNormalizing)))
	}
	if res.RateSample == nil {
		t.Fatal("no RateSample; want one")
	}
	if res.RateSample.Units != 1 {
		t.Errorf("RateSample.Units = %d, want 1 (one whole-book agent stage)", res.RateSample.Units)
	}
	// The stage's wall-clock spanned the ~300ms backoff, but the rate charges only
	// productive agent time, so Seconds must be well under the backoff it slept through.
	if res.RateSample.Seconds >= backoff.Seconds() {
		t.Errorf("RateSample.Seconds = %v, want < %v (rate-limit backoff excluded)", res.RateSample.Seconds, backoff.Seconds())
	}
}

func TestMarkersNormalizeNotConfidentParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	draft := audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)}
	writeManifestStruct(t, work, draft)

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: false, Reason: "one marker holds two chapters"})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.HasPrefix(pe.Reason, MarkersNotConfidentPrefix) || !strings.Contains(pe.Reason, "one marker holds two chapters") {
		t.Errorf("park reason = %q, want the %q prefix + the verdict reason", pe.Reason, MarkersNotConfidentPrefix)
	}
	// The draft was NOT overwritten (still non-contiguous) and no sentinel written.
	m, _ := audio.ReadManifest(work)
	if audio.Contiguous(m.Chapters) {
		t.Error("draft manifest was overwritten on a not-confident verdict")
	}
	if scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("sentinel written despite parking")
	}
}

// TestMarkersNormalizeNotConfidentNoManifestParksOnce is the item-3 regression: an
// agent that follows the "do not guess" instruction (a not-confident verdict and NO
// out/manifest.json) parks the book needs_attention with its own reason in ONE
// invocation - not after exhausting the retry budget with the wrong message.
func TestMarkersNormalizeNotConfidentNoManifestParksOnce(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// ONLY a not-confident verdict, no manifest - the validator must accept this.
		writeOut(t, req, "verdict.json", markerVerdict{Confident: false, Reason: "markers are retail samples"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.Contains(pe.Reason, "markers are retail samples") {
		t.Errorf("park reason = %q, want the agent's verdict reason", pe.Reason)
	}
	if n := fake.count(string(state.MarkersNormalizing)); n != 1 {
		t.Errorf("agent invoked %d times, want 1 (a not-confident verdict is valid, no retries)", n)
	}
	if scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("sentinel written despite parking")
	}
}

func TestMarkersNormalizeInvalidManifestExhaustsAndParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// Always produce a NON-contiguous manifest (1,2,4) - the validator rejects it.
		writeOut(t, req, audio.ManifestName, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "done"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if !strings.HasPrefix(pe.Reason, AgentValidationExhaustedPrefix) {
		t.Errorf("park reason = %q, want the %q prefix", pe.Reason, AgentValidationExhaustedPrefix)
	}
	// 3 attempts total (2 retries), and the runner saw the appended validator error.
	if n := fake.count(string(state.MarkersNormalizing)); n != 3 {
		t.Errorf("agent invoked %d times, want 3 (initial + 2 retries)", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.MarkersNormalizing)), "contiguous") {
		t.Errorf("retry prompt did not carry the validator error; got %q", fake.lastPrompt(string(state.MarkersNormalizing)))
	}
}

func TestMarkersNormalizeAgentUnavailableParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	// No agent, and re-detection finds none (this machine may have a real claude CLI,
	// which is not what this test is about).
	exe.redetectAgent = func(context.Context) (agent.Runner, agent.Availability) {
		return nil, agent.Availability{Detail: "no agent CLI found"}
	}
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if pe.Reason != AgentUnavailableMsg {
		t.Errorf("park reason = %q, want AgentUnavailableMsg", pe.Reason)
	}
	// A PREFLIGHT unavailable park (no CLI configured at all) carries NO auto-readmit time:
	// a daemon with no backend must park once for a human, not churn a re-admit every 10min.
	if !pe.RetryAfter.IsZero() {
		t.Errorf("preflight unavailable park must carry no auto-readmit time, got %v", pe.RetryAfter)
	}
}

// TestRateLimitRetryAtFloor: a parsed reset instant that (after the buffer) lands in the past
// or barely ahead is floored to now+rateLimitMinDelay, so the auto-readmit never schedules a
// past/immediate re-admit that would tight-loop a re-park. A comfortably-future reset keeps
// reset+buffer; no parsed reset falls back to the fixed 30min.
func TestRateLimitRetryAtFloor(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	floor := now.Add(rateLimitMinDelay)

	// A past reset instant -> floored.
	if got := rateLimitRetryAt(&agent.RateLimitError{ResetAt: now.Add(-time.Hour)}, now); !got.Equal(floor) {
		t.Errorf("past reset: got %v, want the floor %v", got, floor)
	}
	// A reset barely ahead: reset+buffer (now+2min) is still below the floor -> floored.
	if got := rateLimitRetryAt(&agent.RateLimitError{ResetAt: now}, now); !got.Equal(floor) {
		t.Errorf("barely-ahead reset: got %v, want the floor %v", got, floor)
	}
	// A comfortably-future reset keeps reset + buffer (well above the floor).
	fut := now.Add(time.Hour)
	if got := rateLimitRetryAt(&agent.RateLimitError{ResetAt: fut}, now); !got.Equal(fut.Add(rateLimitReadmitBuffer)) {
		t.Errorf("future reset: got %v, want reset+buffer %v", got, fut.Add(rateLimitReadmitBuffer))
	}
	// No parsed reset -> the fixed fallback (already above the floor).
	if got := rateLimitRetryAt(&agent.RateLimitError{}, now); !got.Equal(now.Add(rateLimitFallbackDelay)) {
		t.Errorf("no reset: got %v, want the 30min fallback", got)
	}
}

// --- qa_adjudicating ---

// seedQAReport writes qa_report.json/.md flagging the given retranscribe-queue chapters
// plus a manifest so the adjudicating stage has both artifacts.
func seedQAReport(t *testing.T, work string, queue []int) *qa.Report {
	t.Helper()
	rep := &qa.Report{Chapters: 3, RetranscribeQueue: queue}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatalf("write qa report: %v", err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	return rep
}

func TestQAAdjudicateAcceptAll(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless closing echo"}}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = true for an accept-all plan, want false")
	}
	if _, err := os.Stat(filepath.Join(work, qa.PlanFile)); err != nil {
		t.Errorf("qa_plan.json not harvested: %v", err)
	}
	if !scheduler.SentinelExists(work, string(state.QAAdjudicating)) {
		t.Error("qa_adjudicating sentinel missing")
	}
}

func TestQAAdjudicateFansOutBoundedPartitionsAndMergesDeterministically(t *testing.T) {
	work := t.TempDir()
	chapters := []int{1, 2, 3, 4, 5, 6}
	rep := &qa.Report{Chapters: len(chapters), RetranscribeQueue: chapters}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatal(err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 12, ChapterCount: len(chapters), Chapters: markerChapters(chapters...)})
	for _, chapter := range chapters {
		seedText(t, work, chapter)
		if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: chapter, Verdict: repair.VerdictBenign}); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		partition, err := qa.LoadReport(req.Dir)
		if err != nil {
			return agent.Result{}, err
		}
		assigned := qa.FlaggedChapters(partition)
		manifest, err := audio.ReadManifest(req.Dir)
		if err != nil {
			return agent.Result{}, err
		}
		if len(assigned) != 2 || len(manifest.Chapters) != len(assigned) {
			return agent.Result{}, errors.New("partition was not bounded to two assigned chapters")
		}
		verdicts, err := repair.LoadTailVerdicts(req.Dir)
		if err != nil || len(verdicts) != len(assigned) {
			return agent.Result{}, errors.New("prior verdicts were not bounded to assigned chapters")
		}
		for _, verdict := range verdicts {
			if verdict.Chapter != assigned[0] && verdict.Chapter != assigned[1] {
				return agent.Result{}, errors.New("partition leaked another chapter's prior verdict")
			}
		}
		entries := make([]qa.PlanEntry, 0, len(assigned))
		for _, chapter := range assigned {
			entries = append(entries, qa.PlanEntry{Chapter: chapter, Action: qa.ActionAccept, Reason: "verified partition"})
		}
		data, err := json.Marshal(qa.Plan{Entries: entries, Notes: fmt.Sprintf("starts %d", assigned[0])})
		if err != nil {
			return agent.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(agent.OutPath(req.Dir), qa.PlanFile), data, 0o644); err != nil { //nolint:gosec // isolated test staging
			return agent.Result{}, err
		}
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 10, Output: 5}}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.AgentConcurrency, cfg.MaxAgentsPerBook = 3, 3
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if got := fake.count(string(state.QAAdjudicating)); got != 3 {
		t.Fatalf("invocations=%d, want 3", got)
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != len(chapters) {
		t.Fatalf("merged entries=%v", plan.Entries)
	}
	for i, entry := range plan.Entries {
		if entry.Chapter != i+1 {
			t.Fatalf("merged order=%v", plan.Entries)
		}
	}
	if plan.Notes != "starts 1\nstarts 2\nstarts 3" {
		t.Fatalf("merged notes=%q", plan.Notes)
	}
}

func TestQAAdjudicateRetranscribePlan(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "mid-chapter loss"}}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if !res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = false for a retranscribe plan, want true")
	}
}

func TestQAAdjudicateInvalidPlanRetries(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// Plan omits the flagged chapter 2 -> plan.Validate fails every round.
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 3 {
		t.Errorf("agent invoked %d times, want 3", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.QAAdjudicating)), "flagged for disposition") {
		t.Errorf("retry prompt did not carry the validator error; got %q", fake.lastPrompt(string(state.QAAdjudicating)))
	}
}

func TestQAAdjudicateRoundCapParks(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// maxQARounds prior successful adjudication rounds -> the hard cap trips (the backstop
	// for a book that makes real progress every round; the stall marker catches a stuck one).
	for i := range maxQARounds {
		runID, serr := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), i+1)
		if serr != nil {
			t.Fatal(serr)
		}
		if ferr := db.FinishStageRun(context.Background(), runID, true, nil); ferr != nil {
			t.Fatal(ferr)
		}
	}
	fake := newFakeRunner()
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	_, err = exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError (round cap)", err)
	}
	if pe.Reason != QANoConvergeMsg {
		t.Errorf("park reason = %q, want %q", pe.Reason, QANoConvergeMsg)
	}
	if fake.count(string(state.QAAdjudicating)) != 0 {
		t.Error("the agent was invoked despite the round cap")
	}
}

// TestQAAdjudicateAutoAcceptsRepairedTails is the item-4 regression: a report whose
// only flagged chapter is tail-flagged AND already repaired via tail_clip is
// auto-accepted by the pipeline with NO agent invocation, yielding an accept-all plan
// and RetranscribeNeeded=false so the book advances to spelling_research rather than
// looping to the round cap on the agent's goodwill.
func TestQAAdjudicateAutoAcceptsRepairedTails(t *testing.T) {
	work := t.TempDir()
	// A tail-rate-only report flagging chapter 2 (its only finding is tail-related).
	rep := &qa.Report{Chapters: 3, TailRate: []qa.TailRateHit{{Chapter: 2, WPS: 5, Span: 2, Tail: "do do do"}}}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatalf("write report: %v", err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	// The durable evidence of a completed tail_clip: a repaired splice + a verdict entry.
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 2, "the real ending text"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 2, Verdict: repair.VerdictBenign, DecodeTag: tailClipDecodeTag}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		t.Errorf("agent invoked for an all-auto-accept round (stage %q)", req.Stage)
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 0 {
		t.Errorf("agent invoked %d times, want 0 (all chapters auto-accepted)", n)
	}
	if res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = true, want false (accept-all)")
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Chapter != 2 || plan.Entries[0].Action != qa.ActionAccept {
		t.Errorf("plan = %+v, want a single accept entry for chapter 2", plan.Entries)
	}
	next, _, err := state.NextState(state.KindAudio, state.QAAdjudicating, state.Outcome{RetranscribeNeeded: res.RetranscribeNeeded})
	if err != nil {
		t.Fatal(err)
	}
	if next != state.SpellingResearch {
		t.Errorf("next state = %q, want spelling_research", next)
	}
}

// A mechanical accept written by the legacy short-window tail repair must not hide the
// chapter forever after the tail geometry changes. The adjudicator should reopen it once,
// let the agent queue a context-expanded tail repair, and only auto-accept it again after
// that repair records tailClipDecodeTag.
func TestQAAdjudicateReopensLegacyAutoAcceptedTailRepair(t *testing.T) {
	work := t.TempDir()
	rep := &qa.Report{Chapters: 3, TailRate: []qa.TailRateHit{{Chapter: 2, WPS: 5, Span: 2, Tail: "do do do"}}}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatalf("write report: %v", err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 2, "legacy short-window splice"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 2, Verdict: repair.VerdictBenign, DecodeTag: retranscribeDecodeTag}); err != nil {
		t.Fatal(err)
	}
	if err := writeAcceptedLedger(work, map[int]acceptedEntry{
		2: {Round: 2, Reason: autoAcceptTailReason, Source: "auto"},
	}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{
			Chapter:      2,
			Action:       qa.ActionTailClip,
			Reason:       "replace the legacy short-window splice with a context-expanded decode",
			ClipStartSec: 0.5, // in range for the 2s markerChapters fixture (< duration-1)
		}}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Fatalf("agent invoked %d times, want 1 to reconsider the legacy repair", n)
	}
	if !res.RetranscribeNeeded {
		t.Fatal("RetranscribeNeeded = false, want true for the replacement tail repair")
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Chapter != 2 || plan.Entries[0].Action != qa.ActionTailClip {
		t.Fatalf("plan = %+v, want chapter 2 tail_clip", plan.Entries)
	}
}

func TestQAAdjudicatePromotesReopenedLegacyAutoAccept(t *testing.T) {
	work := t.TempDir()
	rep := &qa.Report{Chapters: 3, TailRate: []qa.TailRateHit{{Chapter: 2, WPS: 5, Span: 2, Tail: "do do do"}}}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatal(err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 2, Verdict: repair.VerdictBenign, DecodeTag: retranscribeDecodeTag}); err != nil {
		t.Fatal(err)
	}
	if err := writeAcceptedLedger(work, map[int]acceptedEntry{2: {Round: 1, Reason: autoAcceptTailReason, Source: "auto"}}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "verified authentic closing repetition"}}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatal(err)
	}
	entry := loadAcceptedLedger(work)[2]
	if entry.Source != "agent" || entry.Reason != "verified authentic closing repetition" {
		t.Fatalf("accepted ledger entry = %+v, want promoted agent acceptance", entry)
	}
}

// f64ptr returns a pointer to v, for the optional *float64 report fields.
func f64ptr(v float64) *float64 { return &v }

// TestRunAgentParksOnBudgetExceeded: once a book's summed agent cost has reached the
// per-book budget, the next runAgent call parks ParkBudgetExceeded BEFORE invoking the
// agent (no further spend), and the recorded spend the guard reads includes superseded
// rows (so a Retry cannot lower it below the budget).
func TestRunAgentParksOnBudgetExceeded(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	fake := newFakeRunner()
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	exe.bookBudgetUSD = 10.0

	// Record $12 of spend on a finished stage run, over the $10 budget.
	runID, err := db.StartStageRun(ctx, book.ID, string(state.FactPass), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddOpenStageRunUsage(ctx, book.ID, string(state.FactPass), "opus", 0, 0, 12.0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, runID, true, nil); err != nil {
		t.Fatal(err)
	}

	st, err := agent.New(work, string(state.FactPass), 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = exe.runAgent(ctx, book, state.FactPass, scheduler.StageReport{}, st, "fact_pass.md", nil, false,
		func(agent.Result, *agent.Staging) error { return nil })

	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError (budget)", err)
	}
	if pe.Code != state.ParkBudgetExceeded {
		t.Errorf("park code = %q, want %q", pe.Code, state.ParkBudgetExceeded)
	}
	if !pe.RetryAfter.IsZero() {
		t.Errorf("budget park must carry no auto-readmit time, got %v", pe.RetryAfter)
	}
	if !strings.Contains(pe.Reason, "12.00") || !strings.Contains(pe.Reason, "10.00") {
		t.Errorf("park reason = %q, want it to name the spend and budget", pe.Reason)
	}
	if n := fake.count(string(state.FactPass)); n != 0 {
		t.Errorf("agent invoked %d times over budget, want 0", n)
	}

	// Superseding the stage's success (what a Retry does) must NOT lower the spend the
	// guard sees - the book still parks over budget.
	if err := db.SupersedeStageSuccesses(ctx, book.ID, string(state.FactPass)); err != nil {
		t.Fatal(err)
	}
	_, err = exe.runAgent(ctx, book, state.FactPass, scheduler.StageReport{}, st, "fact_pass.md", nil, false,
		func(agent.Result, *agent.Staging) error { return nil })
	if !errors.As(err, &pe) || pe.Code != state.ParkBudgetExceeded {
		t.Errorf("after supersede: err = %v, want still ParkBudgetExceeded (superseded cost still counts)", err)
	}
}

// dbBackedQAExecutor opens a real store, creates a book at work dir `work`, and returns a
// db-backed executor with the fake agent - the setup the stall-marker/round-cap tests need
// so CountStageSuccesses is live (withAgentConfig alone leaves e.db nil).
func dbBackedQAExecutor(t *testing.T, work string, fake *fakeRunner) (*store.DB, *Executor, store.Book) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(t.TempDir(), "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	return db, NewExecutor(cfg), book
}

// TestQAAdjudicateStallMarkerParks is the convergence-signal regression: after TWO
// consecutive no-progress repair rounds (marker count >= 2), qa_adjudicating parks
// ParkQANoConverge WITHOUT another agent round, names the stuck chapters, and DELETES the
// marker so a Retry gets one fresh round. The old fingerprint design would have THRASHED
// on this incident: a re-degenerating tail clip rewrites tail_verdicts.json every round
// (each CLIP-REDEGENERATED verdict relocates its clip_start), so the report+ledger
// fingerprint changed each round, the fixed point never fired, and the book burned all 3
// paid rounds. The progress marker is immune to that ledger churn.
func TestQAAdjudicateStallMarkerParks(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	// The prior round's plan queued chapter 2 for repair - its non-accept entries name the
	// stuck set the park message reports.
	if err := qa.WritePlan(work, &qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop"}}}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		t.Errorf("agent invoked on a stall park (stage %q)", req.Stage)
		return agent.Result{}, nil
	}
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	// One completed round so done >= 1 (done == 0 would take the reset path, which itself
	// clears the marker - here we exercise the stall guard on an established loop).
	runID, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(context.Background(), runID, true, nil); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(work, retranscribeStalledMarker)
	// Count 2 = the SECOND consecutive no-progress round: this is the genuine stall that parks.
	if err := os.WriteFile(markerPath, []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError (stall)", err)
	}
	if pe.Code != state.ParkQANoConverge {
		t.Errorf("park code = %q, want %q", pe.Code, state.ParkQANoConverge)
	}
	if !strings.Contains(pe.Reason, "stopped making progress") || !strings.Contains(pe.Reason, "2") {
		t.Errorf("park reason = %q, want a stall message naming chapter 2", pe.Reason)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 0 {
		t.Errorf("agent invoked %d times on a stall park, want 0", n)
	}
	if _, serr := os.Stat(markerPath); !os.IsNotExist(serr) {
		t.Errorf("stall park must delete the marker, stat err = %v", serr)
	}
}

// TestQAAdjudicateStallMarkerCountOneRunsAgent is the two-round-grace half of the stall
// contract: a SINGLE no-progress round (marker count 1) is not yet a stall - it is the
// round whose feedback (unlocatable notes, known-failed skips) the adjudicator needs, so
// qa_adjudicating PROCEEDS to one resolution agent round and LEAVES the marker in place
// (the next retranscribing round either makes progress and clears it, or increments it to
// 2 and the following adjudication parks).
func TestQAAdjudicateStallMarkerCountOneRunsAgent(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	seedText(t, work, 2)
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless closing echo"}}})
		return agent.Result{}, nil
	}
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	// One completed round so done >= 1 (an established loop, not the done==0 reset path).
	runID, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(context.Background(), runID, true, nil); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(work, retranscribeStalledMarker)
	if err := os.WriteFile(markerPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The open run the round's agent usage accrues onto.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 2); err != nil {
		t.Fatal(err)
	}

	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("count-1 marker must run the agent, not park: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("agent invoked %d times, want 1 (one resolution round at count 1)", n)
	}
	// The marker is LEFT in place at count 1 (only progress or a park removes it).
	if got, rerr := os.ReadFile(markerPath); rerr != nil || strings.TrimSpace(string(got)) != "1" {
		t.Errorf("marker = %q (%v), want it left in place at count 1", got, rerr)
	}
}

// TestQAAdjudicateStaleStallMarkerRunsAgent is the one-fresh-round-after-reset contract:
// when the round budget is reset (CountStageSuccesses == 0) but a stale stall marker is
// still on disk (a Retry/purge-rewind/crash left it), the done == 0 reset drops it so the
// round runs a fresh agent pass instead of falsely parking.
func TestQAAdjudicateStaleStallMarkerRunsAgent(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless echo"}}})
		return agent.Result{}, nil
	}
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	// Open the run (agent usage target) but record NO successes -> done == 0.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(work, retranscribeStalledMarker)
	if err := os.WriteFile(markerPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("done==0 with a stale stall marker must run the agent, not park: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("agent called %d times, want 1 (a reset round budget gets one fresh round)", n)
	}
	if _, serr := os.Stat(markerPath); !os.IsNotExist(serr) {
		t.Errorf("the done==0 reset must drop the stale stall marker, stat err = %v", serr)
	}
}

// TestTailOnlyChaptersTailResiduals drives the widened tail-residual classification
// (item-1) over incident-shaped fixtures: a cross-segment / non-mid multi-loop finding
// whose time or position sits in the chapter's spliced tail zone no longer disqualifies
// the chapter, while a mid-chapter finding, a wph outlier, a within-segment hit, a
// straddling span (starts mid-chapter, ends in the tail), or a finding with no covering
// splice still does. It reads the report + a verdict map only.
func TestTailOnlyChaptersTailResiduals(t *testing.T) {
	verdicts := map[int]repair.TailVerdict{
		2:  {ClipStart: 425.7},
		8:  {ClipStart: 826.1},
		10: {ClipStart: 977.8},
		11: {ClipStart: 500.0},
		12: {ClipStart: 300.0},
		13: {ClipStart: 400.0},
		20: {ClipStart: 100.0},
		21: {ClipStart: 100.0},
		30: {ClipStart: 826.1},
	}
	rep := &qa.Report{
		Chapters: 30,
		// Every listed chapter is flagged via a tail_rate hit (so it is required).
		TailRate: []qa.TailRateHit{
			{Chapter: 2}, {Chapter: 8}, {Chapter: 10}, {Chapter: 11}, {Chapter: 12},
			{Chapter: 13}, {Chapter: 20}, {Chapter: 21}, {Chapter: 25}, {Chapter: 30},
		},
		CrossSegment: []qa.CrossSegmentHit{
			// ch2: located span starts inside the tail (430 >= 425.7-15) -> covered.
			{Chapter: 2, Count: 6, FirstSec: f64ptr(430), LastSec: f64ptr(450), Pos: 99},
			// ch8: the real incident case - span 814-845s, clip_start 826.1: FirstSec
			// 814 >= 811.1 -> covered (the whole span begins in the tail zone).
			{Chapter: 8, Count: 6, FirstSec: f64ptr(814), LastSec: f64ptr(845), Pos: 98},
			// ch11: a genuine mid-chapter cross hit (starts 100s, clip_start 500) -> NOT covered.
			{Chapter: 11, Count: 6, FirstSec: f64ptr(100), LastSec: f64ptr(120), Pos: 20},
			// ch12: no usable time, position in the tail (>= 95) -> covered.
			{Chapter: 12, Count: 6, Pos: 97},
			// ch13: no usable time, "-1.0% (?)" not-located -> NOT covered.
			{Chapter: 13, Count: 6, Pos: -1},
			// ch30: a STRADDLING span - starts mid-chapter (790s) but ends in the tail
			// (845s) past clip_start 826.1. Testing FirstSec (790 < 811.1) -> NOT covered,
			// so a hit that ate real narration before the loop is not auto-accepted.
			{Chapter: 30, Count: 6, FirstSec: f64ptr(790), LastSec: f64ptr(845), Pos: 96},
		},
		MultiLoop: []qa.MultiLoopFinding{
			// ch10: a non-mid multi-loop located in the tail -> covered.
			{Chapter: 10, Count: 6, AtSec: f64ptr(985), Pos: 96, MidChapter: false},
			// ch20: a MID-CHAPTER multi-loop -> always disqualifies.
			{Chapter: 20, Count: 6, AtSec: f64ptr(200), Pos: 40, MidChapter: true},
		},
		WithinSegment: []qa.WithinSegmentHit{
			// ch21: a within-segment loop always disqualifies (even in the tail).
			{Chapter: 21, Count: 8, Pos: 99},
		},
		WPHOutliers: []qa.WPHOutlier{
			{Chapter: 25, WPH: 9000, Z: 4}, // ch25: wph outlier always disqualifies.
		},
		RetranscribeQueue: []int{25},
	}
	got := tailOnlyChapters(rep, verdicts, nil)
	wantTailOnly := map[int]bool{2: true, 8: true, 10: true, 12: true}
	notTailOnly := []int{11, 13, 20, 21, 25, 30}
	for ch := range wantTailOnly {
		if !got[ch] {
			t.Errorf("chapter %d should be tail-only (a covered tail residual)", ch)
		}
	}
	for _, ch := range notTailOnly {
		if got[ch] {
			t.Errorf("chapter %d should NOT be tail-only", ch)
		}
	}
}

// TestAutoAcceptRepairedTailsIncident reproduces the production report shape: 8 chapters
// with a successful splice and only tail-zone residual findings auto-accept, while two
// CLIP-REDEGENERATED chapters (verdict only, no repaired file) and a wph-outlier +
// mid-chapter chapter do not.
func TestAutoAcceptRepairedTailsIncident(t *testing.T) {
	work := t.TempDir()
	spliced := map[int]float64{2: 425.7, 8: 826.1, 10: 977.8, 14: 1217.7, 15: 937.8, 21: 1746.4, 22: 1086.3, 24: 1263.7}
	redegen := []int{5, 16} // CLIP-REDEGENERATED: verdict only, no repaired file
	var tailFlagged []qa.TailRateHit
	var crossHits []qa.CrossSegmentHit
	for ch, cs := range spliced {
		tailFlagged = append(tailFlagged, qa.TailRateHit{Chapter: ch})
		// A cross-segment residual sitting in the tail (last segment past clip_start).
		crossHits = append(crossHits, qa.CrossSegmentHit{Chapter: ch, Count: 6, FirstSec: f64ptr(cs - 5), LastSec: f64ptr(cs + 10), Pos: 98})
		// Durable evidence of a completed splice: repaired file + a verdict entry.
		if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), ch, "the real ending"); err != nil {
			t.Fatal(err)
		}
		if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: ch, ClipStart: cs, Verdict: repair.VerdictBenign, DecodeTag: tailClipDecodeTag}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ch := range redegen {
		tailFlagged = append(tailFlagged, qa.TailRateHit{Chapter: ch})
		// A CLIP-REDEGENERATED verdict (no repaired file) - has a clip_start, but not "done".
		if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: ch, ClipStart: 500, Verdict: repair.VerdictClipRedegenerated}); err != nil {
			t.Fatal(err)
		}
	}
	// ch25: a wph outlier + a mid-chapter run - never tail-only, never repaired.
	rep := &qa.Report{
		Chapters:          30,
		TailRate:          tailFlagged,
		CrossSegment:      crossHits,
		WPHOutliers:       []qa.WPHOutlier{{Chapter: 25, WPH: 9000, Z: 4}},
		RepeatedRuns:      []qa.RepeatedRun{{Chapter: 25, Kind: qa.KindMidChapter, Length: 5}},
		RetranscribeQueue: []int{25},
	}

	entries := (&Executor{}).autoAcceptRepairedTails(rep, work)
	got := map[int]bool{}
	for _, en := range entries {
		if en.Action != qa.ActionAccept {
			t.Errorf("chapter %d auto-entry action = %q, want accept", en.Chapter, en.Action)
		}
		got[en.Chapter] = true
	}
	for ch := range spliced {
		if !got[ch] {
			t.Errorf("chapter %d (spliced, tail-residual only) should auto-accept", ch)
		}
	}
	for _, ch := range append(redegen, 25) {
		if got[ch] {
			t.Errorf("chapter %d should NOT auto-accept", ch)
		}
	}
	if len(entries) != len(spliced) {
		t.Errorf("auto-accepted %d chapters, want %d", len(entries), len(spliced))
	}
}

func TestAutoAcceptDoesNotUseMidRepairForNewTailFinding(t *testing.T) {
	work := t.TempDir()
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 2, "mid-window repaired text"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{
		Chapter: 2, ClipStart: 10, ClipEnd: 20, Verdict: repair.VerdictMidRepaired, DecodeTag: retranscribeDecodeTag,
	}); err != nil {
		t.Fatal(err)
	}
	rep := &qa.Report{Chapters: 2, TailRate: []qa.TailRateHit{{Chapter: 2, Tail: "new tail loop"}}}
	if got := (&Executor{}).autoAcceptRepairedTails(rep, work); len(got) != 0 {
		t.Fatalf("bounded mid repair auto-accepted an unrelated tail finding: %+v", got)
	}
	verdicts, err := repair.TailVerdictsByChapter(work)
	if err != nil {
		t.Fatal(err)
	}
	if retranscribeEntryDone(work, qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip}, verdicts) {
		t.Fatal("prior mid repair satisfied a new tail_clip entry")
	}
	tailVerdicts := map[int]repair.TailVerdict{2: {
		Chapter: 2, Verdict: repair.VerdictBenign, DecodeTag: tailClipDecodeTag,
	}}
	if retranscribeEntryDone(work, qa.PlanEntry{Chapter: 2, Action: qa.ActionMidClip}, tailVerdicts) {
		t.Fatal("prior tail repair satisfied a new mid_clip entry")
	}
}

// TestTailOnlyChaptersMidWindowCoverage is the mid_clip residual-coverage matrix: after a
// MID splice the raw-layer cross-segment / MID-CHAPTER multi-loop hit persists on re-sweep,
// so the residual auto-accept must cover a hit inside the recorded MID window [clip_start,
// clip_end] (else the chapter re-flags forever). A hit outside the window, a MID-CHAPTER
// multi-loop with no recorded MID window, and a mid-window with a straddling upper end all
// still disqualify. It reads the report + verdict map only.
func TestTailOnlyChaptersMidWindowCoverage(t *testing.T) {
	verdicts := map[int]repair.TailVerdict{
		40: {ClipStart: 1680, ClipEnd: 1710}, // a MID splice window
		41: {ClipStart: 1680, ClipEnd: 1710},
		42: {ClipStart: 1680, ClipEnd: 1710},
		43: {ClipStart: 1680, ClipEnd: 1710},
		// ch44 deliberately has NO verdict entry.
		45: {ClipStart: 900}, // a TAIL window (ClipEnd 0): a MID-CHAPTER loop must NOT be covered.
	}
	rep := &qa.Report{
		Chapters: 50,
		MultiLoop: []qa.MultiLoopFinding{
			// ch40: a MID-CHAPTER multi-loop INSIDE the recorded mid window -> covered.
			{Chapter: 40, Count: 6, AtSec: f64ptr(1690), Pos: 55, MidChapter: true},
			// ch41: an in-window loop makes the chapter required; its cross hit is also covered.
			{Chapter: 41, Count: 6, AtSec: f64ptr(1690), Pos: 55, MidChapter: true},
			// ch42: a MID-CHAPTER multi-loop OUTSIDE the mid window (too early) -> disq.
			{Chapter: 42, Count: 6, AtSec: f64ptr(1500), Pos: 40, MidChapter: true},
			// ch43: the loop is covered, but its cross hit straddles the upper bound -> disq.
			{Chapter: 43, Count: 6, AtSec: f64ptr(1690), Pos: 55, MidChapter: true},
			// ch44: a MID-CHAPTER multi-loop with NO recorded mid window -> disq.
			{Chapter: 44, Count: 6, AtSec: f64ptr(1690), Pos: 55, MidChapter: true},
			// ch45: a MID-CHAPTER multi-loop against a TAIL window -> disq (a tail splice
			// never covers an interior loop).
			{Chapter: 45, Count: 6, AtSec: f64ptr(905), Pos: 50, MidChapter: true},
		},
		CrossSegment: []qa.CrossSegmentHit{
			// ch41: a cross-segment residual whose whole span is inside the mid window -> covered.
			{Chapter: 41, Count: 6, FirstSec: f64ptr(1685), LastSec: f64ptr(1705), Pos: 60},
			// ch43: a cross-segment residual whose span ENDS past the mid window (+ epsilon) -> disq.
			{Chapter: 43, Count: 6, FirstSec: f64ptr(1685), LastSec: f64ptr(1760), Pos: 60},
		},
	}
	got := tailOnlyChapters(rep, verdicts, nil)
	for _, ch := range []int{40, 41} {
		if !got[ch] {
			t.Errorf("chapter %d should be a repaired residual (covered by the mid window)", ch)
		}
	}
	for _, ch := range []int{42, 43, 44, 45} {
		if got[ch] {
			t.Errorf("chapter %d should NOT be a repaired residual (outside/uncovered by a mid window)", ch)
		}
	}
}

// TestAutoAcceptRepairedMidWindow is the end-to-end convergence guarantee for a mid repair:
// a chapter whose ONLY remaining findings (a MID-CHAPTER multi-loop + a cross-segment hit
// from the untouched raw layer) are all inside the recorded MID window, and which has both
// durable-evidence files (a repaired file + a MID-REPAIRED verdict), auto-accepts - so a
// repaired interior loop converges instead of re-flagging every sweep.
func TestAutoAcceptRepairedMidWindow(t *testing.T) {
	work := t.TempDir()
	const ch = 8
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), ch, "the real interior narration resumes here"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: ch, ClipStart: 1680, ClipEnd: 1710, Verdict: repair.VerdictMidRepaired}); err != nil {
		t.Fatal(err)
	}
	rep := &qa.Report{
		Chapters:  20,
		MultiLoop: []qa.MultiLoopFinding{{Chapter: ch, Count: 6, AtSec: f64ptr(1690), Pos: 55, MidChapter: true}},
		CrossSegment: []qa.CrossSegmentHit{
			{Chapter: ch, Count: 6, FirstSec: f64ptr(1685), LastSec: f64ptr(1705), Pos: 60},
		},
	}
	entries := (&Executor{}).autoAcceptRepairedTails(rep, work)
	if len(entries) != 1 || entries[0].Chapter != ch || entries[0].Action != qa.ActionAccept {
		t.Fatalf("entries = %+v, want a single accept for ch%d", entries, ch)
	}
}

func TestQAAdjudicateRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedQAReport(t, work, []int{2})
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Open the stage run the scheduler would open, so AddOpenStageRunUsage has a target.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "benign"}}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 100, Output: 50, CostUSD: 0.02}}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	runs, err := db.ListStageRuns(context.Background(), book.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range runs {
		if r.Stage == string(state.QAAdjudicating) {
			found = true
			if r.Model != "sonnet" || r.InputTokens != 100 || r.OutputTokens != 50 {
				t.Errorf("stage run usage = model %q in %d out %d, want sonnet/100/50", r.Model, r.InputTokens, r.OutputTokens)
			}
		}
	}
	if !found {
		t.Error("no qa_adjudicating stage run recorded")
	}
}

func TestValidationRetryPersistsEachInvocationOutcome(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedQAReport(t, work, []int{2})
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		plan := qa.Plan{}
		if attempt == 2 {
			plan.Entries = []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "verified"}}
		}
		writeOut(t, req, qa.PlanFile, plan)
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 10, Output: 5}}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	if _, err := NewExecutor(cfg).Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatal(err)
	}
	invocations, err := db.ListAgentInvocations(context.Background(), book.ID)
	if err != nil || len(invocations) != 2 {
		t.Fatalf("invocations=%+v err=%v", invocations, err)
	}
	if invocations[0].Status != "validation_failed" || invocations[1].Status != "success" {
		t.Fatalf("statuses=%q,%q", invocations[0].Status, invocations[1].Status)
	}
	runs, err := db.ListStageRuns(context.Background(), book.ID)
	if err != nil || runs[0].InputTokens != 20 || runs[0].OutputTokens != 10 {
		t.Fatalf("retry accounting=%+v err=%v", runs, err)
	}
}

// --- invariant: staged dirs hold exactly the contracted inputs ---

func TestAgentStagedDirsHoldOnlyContractedInputs(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	markersFake := newFakeRunner()
	markersFake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "ok"})
		return agent.Result{}, nil
	}
	mexe := NewExecutor(withAgentConfig(t.TempDir(), markersFake))
	if _, err := mexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("markers: %v", err)
	}
	mReq, _ := markersFake.lastRequest(string(state.MarkersNormalizing))
	// The markers staged dir must contain NO transcript files (it is pre-transcription).
	walkAssertNo(t, mReq.Dir, "transcripts")

	// Adjudicating: only the FLAGGED chapter's transcript is staged.
	work2 := t.TempDir()
	seedQAReport(t, work2, []int{2})
	for _, ch := range []int{1, 2, 3} {
		seedText(t, work2, ch)
	}
	adjFake := newFakeRunner()
	adjFake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "ok"}}})
		return agent.Result{}, nil
	}
	aexe := NewExecutor(withAgentConfig(t.TempDir(), adjFake))
	if _, err := aexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work2}, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("adjudicating: %v", err)
	}
	aReq, _ := adjFake.lastRequest(string(state.QAAdjudicating))
	staged := filepath.Join(aReq.Dir, transcript.TextDir)
	if !fileExistsT(filepath.Join(staged, transcript.TextName(2))) {
		t.Error("flagged chapter 2 transcript was not staged")
	}
	for _, ch := range []int{1, 3} {
		if fileExistsT(filepath.Join(staged, transcript.TextName(ch))) {
			t.Errorf("unflagged chapter %d transcript was staged (spoiler-scope leak)", ch)
		}
	}
}

func seedText(t *testing.T, work string, chapter int) {
	t.Helper()
	if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), chapter, "chapter text"); err != nil {
		t.Fatal(err)
	}
}

func walkAssertNo(t *testing.T, root, substr string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(rel, substr) {
			t.Errorf("staged dir contains a forbidden %q file: %s", substr, rel)
		}
		return nil
	})
}

func fileExistsT(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// assertUsageMetrics unmarshals a stage's usage metrics and checks the headline fields.
func assertUsageMetrics(t *testing.T, raw json.RawMessage, model string, in, out int64) {
	t.Helper()
	var m struct {
		Usage struct {
			Model        string `json:"model"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			Invocations  int    `json:"invocations"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse usage metrics: %v (%s)", err, raw)
	}
	if m.Usage.Model != model || m.Usage.InputTokens != in || m.Usage.OutputTokens != out {
		t.Errorf("usage metrics = %+v, want model %s in %d out %d", m.Usage, model, in, out)
	}
	if m.Usage.Invocations < 1 {
		t.Errorf("usage invocations = %d, want >= 1", m.Usage.Invocations)
	}
}

// TestMarkersPromptExplainsVocabularyGap pins both branches of markers.md's
// empty-draft section. The stage renders with missingkey=error, so a field rename
// would fail loudly - but a template whose {{if}} never fires fails SILENTLY, and the
// whole point of this section is to reach the agent on exactly the books that used to
// park. So assert it appears when the parser recognized nothing and stays out of the
// way otherwise.
func TestMarkersPromptExplainsVocabularyGap(t *testing.T) {
	base := markersPromptData{
		Title: "Inflame", Authors: "Dakota Krout", Style: "markers",
		Duration: 48295, ChapterCount: 0,
	}

	gap := base
	gap.MarkersSeen, gap.NoneRecognized = 64, true
	got, err := prompts.Render("markers.md", gap)
	if err != nil {
		t.Fatalf("render (gap): %v", err)
	}
	for _, want := range []string{
		"recognized NONE",
		"64 markers",
		"probe.json",
		"not, by itself, a reason to decline",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gap prompt missing %q", want)
		}
	}
	// It must not NARROW the decline criteria the Output section lists. A book whose
	// titles state a clean order can still be undeliverable (one marker holding several
	// chapters), and the section must not read as "an order you can see means map it
	// anyway".
	if !strings.Contains(got, "holds several chapters") {
		t.Error("gap prompt narrowed the decline criteria: the other not-confident cases must stay stated")
	}

	ambiguous := base
	ambiguous.MarkersSeen, ambiguous.NoneRecognized = 12, false
	got, err = prompts.Render("markers.md", ambiguous)
	if err != nil {
		t.Fatalf("render (ambiguous): %v", err)
	}
	if strings.Contains(got, "recognized NONE") {
		t.Error("the vocabulary-gap section must not render when markers WERE recognized")
	}
}

// TestMarkersPromptStatesTheCoverageContract pins the rules that stop a mapping from
// losing narration, in BOTH renderings of the template. They are prompt-side halves of a
// mechanical check - the validator rejects an undeclared hole - so a prompt that stopped
// stating them would turn a clean run into a retry loop the agent cannot diagnose.
//
// The verdict's "excluded" key is pinned for the same reason audit.json's shape is: it is
// read by a Go struct, and an agent that never learns the key can only fail validation.
func TestMarkersPromptStatesTheCoverageContract(t *testing.T) {
	base := markersPromptData{Title: "Garden of Sanctuary", Authors: "pirateaba", Style: "markers", Duration: 112389, ChapterCount: 19}
	for _, gap := range []bool{true, false} {
		data := base
		data.NoneRecognized, data.MarkersSeen = gap, 25
		got, err := prompts.Render("markers.md", data)
		if err != nil {
			t.Fatalf("render (gap=%v): %v", gap, err)
		}
		for _, want := range []string{
			"Account for every second", // the coverage rule itself
			"Interlude",                // named, because it is the shape that lost 61 hours
			"When in doubt, INCLUDE",   // the tie-breaker, in the safe direction
			`"excluded"`,               // the verdict key the Go reader parses
			"bloopers or outtakes",     // the exclusions that must stay possible
			"DIFFERENT",                // ...including a preview of another book
		} {
			if !strings.Contains(got, want) {
				t.Errorf("gap=%v: markers.md no longer states %q", gap, want)
			}
		}
	}
}

// TestMarkerExclusionShapeMatchesPrompt pins the verdict JSON the prompt shows against the
// struct that reads it, so the two cannot drift apart silently. audit_verify.md drifted
// exactly this way and parked a finished book one cosmetic key from done.
func TestMarkerExclusionShapeMatchesPrompt(t *testing.T) {
	var v markerVerdict
	sample := `{"confident":true,"reason":"r","excluded":[{"title":"End Credits","start":89962.4,"end":89998.1,"reason":"closing credits"}]}`
	dec := json.NewDecoder(strings.NewReader(sample))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("the shape markers.md documents does not parse into markerVerdict: %v", err)
	}
	if len(v.Excluded) != 1 || v.Excluded[0].Title != "End Credits" || v.Excluded[0].End != 89998.1 {
		t.Fatalf("decoded = %+v", v)
	}
}

// TestMarkersNormalizePartialLegacyDraftReparsesProbeWithoutAgent covers the case the
// empty-draft gate used to miss: an older parser understood SOME of a book's marker
// titles and dropped the rest, leaving a non-contiguous draft. The current parser
// resolves the whole table deterministically, so this must recover for free too -
// previously it fell straight through to a paid agent round.
func TestMarkersNormalizePartialLegacyDraftReparsesProbeWithoutAgent(t *testing.T) {
	work := t.TempDir()
	// Every marker parses under the current rules (1..3, contiguous).
	probe := `{
		"format":{"duration":"30.000","tags":{"title":"Mixed"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"Chapter 1: Opening"}},
			{"start_time":"10.000","end_time":"20.000","tags":{"title":"002"}},
			{"start_time":"20.000","end_time":"30.000","tags":{"title":"3. Closing"}}
		]
	}`
	writeProbeJSON(t, work, probe)
	// The stale draft an older parser left: it understood 1 and 3 but not the bare
	// "002", so the chapters are non-contiguous rather than absent.
	writeManifestStruct(t, work, audio.Manifest{
		Source: "/books/mixed.m4b", Title: "Mixed", Style: audio.StyleMarkers, Duration: 30,
		ChapterCount: 2,
		Chapters: []audio.Chapter{
			{Chapter: 1, Title: "Opening", MarkerTitle: "Chapter 1: Opening", Start: 0, End: 10, Duration: 10},
			{Chapter: 3, Title: "Closing", MarkerTitle: "3. Closing", Start: 20, End: 30, Duration: 10},
		},
	})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, _ agent.Request, _ int) (agent.Result, error) {
		t.Fatal("a deterministically resolvable partial draft must not invoke an agent")
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Mixed", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	if fake.count(string(state.MarkersNormalizing)) != 0 {
		t.Fatalf("agent ran %d times, want zero", fake.count(string(state.MarkersNormalizing)))
	}
	m, err := audio.ReadManifest(work)
	if err != nil || !audio.Contiguous(m.Chapters) || m.ChapterCount != 3 {
		t.Fatalf("recovered manifest = %+v err=%v, want a contiguous 3-chapter map", m, err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(res.Metrics, &metrics); err != nil || metrics["deterministic_reparse"] != true {
		t.Fatalf("metrics = %s err=%v", res.Metrics, err)
	}
	// The free path must observe NO rate. It is a sub-millisecond in-memory
	// re-derivation, while the stage's real cost is the agent round it skipped (seed
	// 180s/book); folding it into the EWMA would drag the learned markers_normalizing
	// rate toward zero with every recovered book and then under-predict every book that
	// genuinely needs the agent.
	if res.RateSample != nil {
		t.Errorf("deterministic reparse recorded RateSample %+v; want none (a free run observes nothing)", res.RateSample)
	}
}

// writeProbeJSON drops a probe.json fixture into a book's work dir.
func writeProbeJSON(t *testing.T, workDir, probeJSON string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, audio.ProbeName), []byte(probeJSON), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// TestMarkersNormalizeRepointsBookGaugesAfterHarvest pins that the AGENT path updates
// books.chapters/duration_sec just like the deterministic branch does. Normalization
// exists to drop credits/bonus markers, so the harvested count is routinely lower than
// the draft count inspect recorded - a stale gauge makes the ETA engine size every
// per-chapter stage off the wrong total and feeds contrib the wrong runtime.
func TestMarkersNormalizeRepointsBookGaugesAfterHarvest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	writeProbeJSON(t, work, `{
		"format":{"duration":"40.000","tags":{"title":"Kept"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"Opening Credits"}},
			{"start_time":"10.000","end_time":"20.000","tags":{"title":"Chapter 1"}},
			{"start_time":"20.000","end_time":"30.000","tags":{"title":"Chapter 2"}},
			{"start_time":"30.000","end_time":"40.000","tags":{"title":"Chapter 7"}}
		]
	}`)
	// A draft the deterministic reparse cannot resolve (1,2,7 - a gap), so the agent runs.
	src := filepath.Join(dir, "kept.m4b")
	writeManifestStruct(t, work, audio.Manifest{
		Source: src, Title: "Kept", Style: audio.StyleMarkers, Duration: 40, ChapterCount: 3,
		Chapters: []audio.Chapter{
			{Chapter: 1, Start: 10, End: 20, Duration: 10},
			{Chapter: 2, Start: 20, End: 30, Duration: 10},
			{Chapter: 7, Start: 30, End: 40, Duration: 10},
		},
	})

	book, err := db.CreateBook(ctx, store.NewBook{SourcePath: src, WorkDir: work, Title: "Kept"})
	if err != nil {
		t.Fatal(err)
	}
	// The stale gauge inspect left behind.
	if err := db.SetBookChapters(ctx, book.ID, 3); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "bonus track excluded"})
		writeOut(t, req, audio.ManifestName, audio.Manifest{
			Source: src, Title: "Kept", Style: audio.StyleMarkers, Duration: 40, ChapterCount: 2,
			Chapters: []audio.Chapter{
				{Chapter: 1, Start: 10, End: 20, Duration: 10},
				{Chapter: 2, Start: 20, End: 30, Duration: 10},
			},
		})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 10, Output: 5}}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	if _, err := NewExecutor(cfg).Execute(ctx, book, state.MarkersNormalizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}

	got, err := db.GetBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Chapters != 2 {
		t.Errorf("books.chapters = %d after the agent excluded a marker, want the harvested 2", got.Chapters)
	}
	if got.DurationSec != 40 {
		t.Errorf("books.duration_sec = %v, want the harvested 40", got.DurationSec)
	}
}

// TestMarkersNormalizeNeverOverwritesAContiguousDraft pins the safety property the
// widened gate rests on: a contiguous manifest is never re-derived from probe.json.
// validateMarkersManifest accepts only contiguous agent output, so this is what stops
// a re-run of the stage from throwing away a mapping the agent already produced -
// here, one that deliberately EXCLUDES the credits markers probe.json still lists.
func TestMarkersNormalizeNeverOverwritesAContiguousDraft(t *testing.T) {
	work := t.TempDir()
	probe := `{
		"format":{"duration":"40.000","tags":{"title":"Kept"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}},
			{"start_time":"10.000","end_time":"20.000","tags":{"title":"002"}},
			{"start_time":"20.000","end_time":"30.000","tags":{"title":"003"}},
			{"start_time":"30.000","end_time":"40.000","tags":{"title":"004"}}
		]
	}`
	writeProbeJSON(t, work, probe)
	// An agent-quality mapping: credits dropped, the two real chapters renumbered.
	// A blind reparse of probe.json would replace this with a 4-chapter map.
	agentMap := audio.Manifest{
		Source: "/books/kept.m4b", Title: "Kept", Style: audio.StyleMarkers, Duration: 40,
		ChapterCount: 2,
		Chapters: []audio.Chapter{
			{Chapter: 1, MarkerTitle: "002", Start: 10, End: 20, Duration: 10},
			{Chapter: 2, MarkerTitle: "003", Start: 20, End: 30, Duration: 10},
		},
	}
	writeManifestStruct(t, work, agentMap)

	fake := newFakeRunner()
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, _ = exe.Execute(context.Background(), store.Book{ID: 1, Title: "Kept", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})

	m, err := audio.ReadManifest(work)
	if err != nil {
		t.Fatal(err)
	}
	if m.ChapterCount != 2 || len(m.Chapters) != 2 || m.Chapters[0].MarkerTitle != "002" {
		t.Fatalf("contiguous draft was re-derived from probe.json: %+v", m)
	}
}

// An untimed cross-segment hit (the report's "pos": -1, no first_sec) is what a tail loop
// re-reported off the untouched transcripts-json/ layer looks like, so the positional
// fallback can never cover it. Every such residual was therefore pushed to the agent -
// and the adjudicate prompt tells the agent NOT to disposition a chapter a prior
// tail_clip round already repaired, so it omitted the chapter, the plan validator
// rejected the plan for a missing required entry, and the stage failed identically on
// every retry until the book parked agent_validation_exhausted.
func TestTailOnlyChaptersResolvesUntimedResidualFromRepairedText(t *testing.T) {
	rep := &qa.Report{
		RepeatedRuns: []qa.RepeatedRun{
			{Chapter: 44, Kind: qa.KindEndFade, Length: 6, StartSec: 1029.5, Snippet: " Better Nate than Lever."},
			{Chapter: 45, Kind: qa.KindEndFade, Length: 6, StartSec: 1000, Snippet: " Still looping."},
		},
		CrossSegment: []qa.CrossSegmentHit{
			// ch44: the splice landed - the doubled phrase is gone from the repaired text.
			{Chapter: 44, Count: 5, Pos: -1, Phrase: "Better Nate than Lever. Better Nate"},
			// ch45: the splice under-covered - the phrase is still there.
			{Chapter: 45, Count: 5, Pos: -1, Phrase: "Still looping. Still looping."},
		},
	}
	verdicts := map[int]repair.TailVerdict{
		44: {Chapter: 44, ClipStart: 1027},
		45: {Chapter: 45, ClipStart: 998},
	}
	repaired := map[int]string{
		44: "he'd make the same choice again in a heartbeat.\nAfter all, you know what they\nsay. Better Nate than Lever. Thank you.",
		45: "and then it went wrong.\nStill looping. Still looping. Still looping.",
	}
	resolved := func(chapter int, phrase string) bool {
		return !strings.Contains(normalizeSpace(repaired[chapter]), normalizeSpace(phrase))
	}

	// Without a resolver the window arithmetic alone leaves BOTH with the agent.
	if got := tailOnlyChapters(rep, verdicts, nil); got[44] || got[45] {
		t.Fatalf("untimed hits covered by window arithmetic alone: %#v", got)
	}
	got := tailOnlyChapters(rep, verdicts, resolved)
	if !got[44] {
		t.Errorf("chapter 44 is a resolved residual (phrase gone from the repaired text) and should auto-accept: %#v", got)
	}
	if got[45] {
		t.Errorf("chapter 45's phrase is still in the repaired text - the repair under-covered, so it must stay with the agent: %#v", got)
	}
}

// The resolver reads the repaired layer and is conservative about everything it cannot
// prove: a chapter with no repaired file resolves nothing.
func TestRepairedPhraseResolverReadsRepairedLayer(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, transcript.RepairedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Line breaks fall in different places than the phrase recorded from the segments.
	body := "you know what they\nsay. Better Nate than Lever. Thank you."
	if err := os.WriteFile(filepath.Join(work, transcript.RepairedDir, transcript.TextName(44)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved := repairedPhraseResolver(work)
	if !resolved(44, "Better Nate than Lever. Better Nate") {
		t.Error("the doubled phrase is absent from the repaired text and should resolve")
	}
	if resolved(44, "Better  Nate\nthan Lever.") {
		t.Error("a phrase still present (modulo whitespace) must not resolve")
	}
	if resolved(99, "anything") {
		t.Error("a chapter with no repaired file must resolve nothing")
	}
}
