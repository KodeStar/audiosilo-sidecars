package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// seedSpellingInputs writes a manifest (with marker titles) and n chapters of
// transcript text so the spelling and correcting stages have real inputs.
func seedSpellingInputs(t *testing.T, work string, n int) {
	t.Helper()
	m := audio.Manifest{Source: "/x/b.m4b", Title: "Book", Style: audio.StyleMarkers, Duration: float64(n), ChapterCount: n}
	for i := 1; i <= n; i++ {
		m.Chapters = append(m.Chapters, audio.Chapter{
			Chapter: i, MarkerTitle: fmt.Sprintf("Chapter %d: A Title", i),
			Start: float64(i - 1), End: float64(i), Duration: 1,
		})
		if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), i, "the quick brown fox jumps over the lazy dog"); err != nil {
			t.Fatal(err)
		}
	}
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatal(err)
	}
}

// validSpellingAct writes valid, gate-passing corrections + spellings (no rules, no
// ledger), reading the chunk plan from the staged dir for the exact chunk_ends.
func validSpellingAct(t *testing.T, title string, refs []string) func(*fakeRunner, agent.Request, int) (agent.Result, error) {
	return func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		plan, err := loadChunkPlan(req.Dir)
		if err != nil {
			t.Fatalf("read staged chunk plan: %v", err)
		}
		writeOut(t, req, spelling.CorrectionsFile, spelling.Corrections{Rules: []spelling.Rule{}, Unresolved: []string{}, ReferenceFiles: refs})
		writeOut(t, req, spelling.SpellingsFile, spelling.Spellings{Title: title, ChunkEnds: plan.ChunkEnds, Ledger: []spelling.LedgerEntry{}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 200, Output: 80, CostUSD: 0.05, Turns: 3}}, nil
	}
}

func TestSpellingResearchHappyPath(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 3)

	fake := newFakeRunner()
	fake.act = validSpellingAct(t, "Book", []string{"marker_titles.txt"})
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.SpellingResearch, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("spelling_research: %v", err)
	}
	// The daemon pre-work landed.
	for _, name := range []string{markerTitlesFile, chunkPlanFile} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
	// The agent outputs were harvested.
	for _, name := range []string{spelling.CorrectionsFile, spelling.SpellingsFile} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Errorf("%s not harvested: %v", name, err)
		}
	}
	if !scheduler.SentinelExists(work, string(state.SpellingResearch)) {
		t.Error("spelling_research sentinel missing")
	}
	// It is the web stage: the request carried the web flag.
	if r, ok := fake.lastRequest(string(state.SpellingResearch)); !ok || !r.Web {
		t.Errorf("spelling request web = %v, want true", r.Web)
	}
	assertUsageMetrics(t, res.Metrics, "sonnet", 200, 80)
}

func TestSpellingResearchStagesCarryoverRefs(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	// A predecessor with a full carryover payload.
	pred := newSeriesBook(t, db, root, "Saga", "1", true)
	if err := transcript.WriteText(filepath.Join(pred.WorkDir, spelling.CorrectedDir), 1, "Aria and Borin travelled far"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pred.WorkDir, markerTitlesFile), []byte("Chapter 1\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	for _, f := range []string{spelling.SpellingsFile, spelling.CorrectionsFile} {
		if err := os.WriteFile(filepath.Join(pred.WorkDir, f), []byte("{}"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}

	// The target volume.
	work := filepath.Join(root, "work-saga-2")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedSpellingInputs(t, work, 2)
	book, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(root, "saga-2.m4b"), WorkDir: work, Title: "Saga 2", Series: "Saga", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}

	fake := newFakeRunner()
	fake.act = validSpellingAct(t, "Saga 2", []string{"marker_titles.txt", "spelling-refs"})
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), book, state.SpellingResearch, scheduler.StageReport{}); err != nil {
		t.Fatalf("spelling_research: %v", err)
	}

	// spelling-refs/ landed in the work dir with the predecessor's payload.
	refs := filepath.Join(work, spellingRefsDir)
	for _, name := range []string{"prior-spellings.json", "prior-corrections.json", "prior-marker_titles.txt", transcript.TextName(1)} {
		if _, err := os.Stat(filepath.Join(refs, name)); err != nil {
			t.Errorf("spelling-refs/%s missing: %v", name, err)
		}
	}
	// And it was staged into the agent's dir.
	r, ok := fake.lastRequest(string(state.SpellingResearch))
	if !ok {
		t.Fatal("no spelling request recorded")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, spellingRefsDir, "prior-spellings.json")); err != nil {
		t.Errorf("spelling-refs not staged into the agent dir: %v", err)
	}
}

func TestSpellingResearchForbiddenReferenceFileParks(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 2)

	fake := newFakeRunner()
	// Every attempt cites a file the agent authored - the gate-3 integrity boundary.
	fake.act = validSpellingAct(t, "Book", []string{"my-own-notes.txt"})
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.SpellingResearch, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if n := fake.count(string(state.SpellingResearch)); n != 3 {
		t.Errorf("agent invoked %d times, want 3", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.SpellingResearch)), "not allowed") {
		t.Errorf("retry prompt did not carry the reference-file restriction; got %q", fake.lastPrompt(string(state.SpellingResearch)))
	}
}

func TestSpellingResearchDryRunCheckFailureRetries(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 2)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		plan, err := loadChunkPlan(req.Dir)
		if err != nil {
			t.Fatalf("read staged chunk plan: %v", err)
		}
		// A dead rule: its LHS never appears, so its RHS is written nowhere and is
		// attested nowhere - gate 2 (RHS absent) and gate 3 (RHS unattested) fail.
		writeOut(t, req, spelling.CorrectionsFile, spelling.Corrections{
			Rules:          []spelling.Rule{{Pattern: `(?<![A-Za-z])Zzqfoo(?![A-Za-z])`, Replacement: "Zzqbar", Note: "invented"}},
			ReferenceFiles: []string{"marker_titles.txt"},
		})
		writeOut(t, req, spelling.SpellingsFile, spelling.Spellings{Title: "Book", ChunkEnds: plan.ChunkEnds, Ledger: []spelling.LedgerEntry{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.SpellingResearch, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.Contains(fake.lastPrompt(string(state.SpellingResearch)), "spelling gates") {
		t.Errorf("retry prompt did not carry the dry-run check failure; got %q", fake.lastPrompt(string(state.SpellingResearch)))
	}
}

func TestSpellingResearchChunkEndsMismatchRetries(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 2)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, spelling.CorrectionsFile, spelling.Corrections{Rules: []spelling.Rule{}, ReferenceFiles: []string{"marker_titles.txt"}})
		// A chunk_ends that does not match the plan.
		writeOut(t, req, spelling.SpellingsFile, spelling.Spellings{Title: "Book", ChunkEnds: []int{999}, Ledger: []spelling.LedgerEntry{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.SpellingResearch, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.Contains(fake.lastPrompt(string(state.SpellingResearch)), "chunk_ends") {
		t.Errorf("retry prompt did not carry the chunk_ends mismatch; got %q", fake.lastPrompt(string(state.SpellingResearch)))
	}
}

func TestAllowedReferenceFile(t *testing.T) {
	allowed := []string{"marker_titles.txt", "spelling-refs", "spelling-refs/prior-spellings.json", "spelling-refs/ch001.txt"}
	denied := []string{"", "corrections.json", "my-notes.txt", "/etc/passwd", "spelling-refs/../secret.txt", "../marker_titles.txt", "spellings.json"}
	for _, r := range allowed {
		if !allowedReferenceFile(r) {
			t.Errorf("reference %q should be allowed", r)
		}
	}
	for _, r := range denied {
		if allowedReferenceFile(r) {
			t.Errorf("reference %q should be denied", r)
		}
	}
}

// --- correcting ---

// writeJSONFile marshals v to path (creating parents), for seeding a work-dir artifact.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// seedResearchOutputs writes the corrections.json + spellings.json a completed
// spelling_research stage would leave (plus marker_titles.txt for the gate), so the
// correcting stage can run standalone.
func seedResearchOutputs(t *testing.T, work string, corr spelling.Corrections, sp spelling.Spellings) {
	t.Helper()
	writeJSONFile(t, filepath.Join(work, spelling.CorrectionsFile), corr)
	writeJSONFile(t, filepath.Join(work, spelling.SpellingsFile), sp)
	if err := os.WriteFile(filepath.Join(work, markerTitlesFile), []byte("Chapter 1\nChapter 2\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

func TestCorrectingHappyPath(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 2)
	// Compute + write the chunk plan (spelling_research would have).
	plan, err := computeChunkPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	seedResearchOutputs(t, work,
		spelling.Corrections{Rules: []spelling.Rule{}, ReferenceFiles: []string{"marker_titles.txt"}},
		spelling.Spellings{Title: "Book", ChunkEnds: plan.ChunkEnds, Ledger: []spelling.LedgerEntry{}})

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Correcting, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("correcting: %v", err)
	}
	// The corrected layer + log landed and the spelling sheet(s) were generated.
	for i := 1; i <= 2; i++ {
		if _, err := os.Stat(filepath.Join(work, spelling.CorrectedDir, transcript.TextName(i))); err != nil {
			t.Errorf("corrected ch%d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "corrections.log")); err != nil {
		t.Errorf("corrections.log missing: %v", err)
	}
	sheet := filepath.Join(work, factsDir, spelling.SheetName(plan.ChunkEnds[len(plan.ChunkEnds)-1]))
	if _, err := os.Stat(sheet); err != nil {
		t.Errorf("spelling sheet missing: %v", err)
	}
	if !scheduler.SentinelExists(work, string(state.Correcting)) {
		t.Error("correcting sentinel missing")
	}
	if len(res.Metrics) == 0 {
		t.Error("correcting returned no metrics")
	}
}

func TestCorrectingCheckFailureParks(t *testing.T) {
	work := t.TempDir()
	seedSpellingInputs(t, work, 2)
	plan, err := computeChunkPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	// A dead rule (LHS/RHS attested nowhere) fails the gates in Check.
	seedResearchOutputs(t, work,
		spelling.Corrections{
			Rules:          []spelling.Rule{{Pattern: `(?<![A-Za-z])Zzqfoo(?![A-Za-z])`, Replacement: "Zzqbar", Note: "invented"}},
			ReferenceFiles: []string{"marker_titles.txt"},
		},
		spelling.Spellings{Title: "Book", ChunkEnds: plan.ChunkEnds, Ledger: []spelling.LedgerEntry{}})

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err = exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Correcting, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError on a gate failure", err)
	}
	if !strings.HasPrefix(pe.Reason, SpellingGateFailurePrefix) {
		t.Errorf("park reason = %q, want the %q prefix", pe.Reason, SpellingGateFailurePrefix)
	}
	if scheduler.SentinelExists(work, string(state.Correcting)) {
		t.Error("correcting sentinel written despite a gate failure")
	}
}
