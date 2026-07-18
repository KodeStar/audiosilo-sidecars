package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- shared seed + config helpers for the sidecar (synthesis/validate/audit/fix) stages ---

// seedSidecarManifest writes a 3-chapter markers manifest (chapters 1..3).
func seedSidecarManifest(t *testing.T, work string) {
	t.Helper()
	writeManifestStruct(t, work, audio.Manifest{
		Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3,
		Chapters: markerChapters(1, 2, 3),
	})
}

// seedFacts writes a minimal facts/ dir (the notes-only stages stage its *.md).
func seedFacts(t *testing.T, work string) {
	t.Helper()
	dir := filepath.Join(work, spelling.FactsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "knowledge-final.md"), []byte("# roster\nAlice is a knight.\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// seedTranscriptsText writes a few non-overlapping transcript text files so the
// validating stage's ngram check has a clean source layer.
func seedTranscriptsText(t *testing.T, work string, lines ...string) {
	t.Helper()
	for i, line := range lines {
		if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), i+1, line); err != nil {
			t.Fatal(err)
		}
	}
}

// seedWorkSidecars writes chars/recs into work/sidecars/ (as synthesize would harvest).
func seedWorkSidecars(t *testing.T, work string, chars *model.Characters, recs *model.Recaps) {
	t.Helper()
	dir := filepath.Join(work, sidecarsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, charactersFileName), chars)
	writeJSON(t, filepath.Join(dir, recapsFileName), recs)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// withSidecarAgent extends withAgentConfig with the sidecar stages' model keys and a
// stub fallback (so ready/contributing advance in the integration tests).
func withSidecarAgent(dataDir string, fake *fakeRunner) Config {
	cfg := withAgentConfig(dataDir, fake)
	for _, s := range []string{"synthesizing", "auditing", "fixing"} {
		cfg.AgentModels.Claude[s] = "opus"
	}
	cfg.Fallback = scheduler.NewStubExecutor(0, 0)
	// The contributing stage is real (M7); run it in LOCAL mode so a full-pipeline
	// integration test reaches done without a GitHub credential or upstream match.
	cfg.ContribMode = contribModeLocal
	cfg.ExportRoot = filepath.Join(dataDir, "export")
	return cfg
}

// writeOutSidecars scripts a fake agent emitting valid out/characters.json +
// out/recaps.json for the given work slug.
func writeOutSidecars(t *testing.T, req agent.Request, work string) {
	t.Helper()
	writeOut(t, req, charactersFileName, baseChars(work))
	writeOut(t, req, recapsFileName, baseRecaps(work))
}

// --- synthesizing ---

func TestSynthesizeHappyPath(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedFacts(t, work)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOutSidecars(t, req, "book")
		return agent.Result{Usage: agent.Usage{Model: "opus", Input: 300, Output: 200, CostUSD: 0.1}}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Synthesizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if _, _, err := loadWorkSidecars(work); err != nil {
		t.Errorf("sidecars not harvested under sidecars/: %v", err)
	}
	if !scheduler.SentinelExists(work, string(state.Synthesizing)) {
		t.Error("synthesizing sentinel missing")
	}
	assertUsageMetrics(t, res.Metrics, "opus", 300, 200)
	var m struct {
		Cards  int `json:"cards"`
		Recaps int `json:"recaps"`
	}
	if err := json.Unmarshal(res.Metrics, &m); err != nil {
		t.Fatal(err)
	}
	if m.Cards != 1 || m.Recaps != 1 {
		t.Errorf("metrics cards=%d recaps=%d, want 1/1", m.Cards, m.Recaps)
	}
	if r, ok := fake.lastRequest(string(state.Synthesizing)); !ok || r.Web {
		t.Errorf("synthesis request web=%v, want false (notes-only, no web)", r.Web)
	}
}

func TestSynthesizeStagedDirIsNotesOnly(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedFacts(t, work)
	// Seed transcripts + manifest in the WORK dir; the staged dir must NOT get them.
	seedTranscriptsText(t, work, "one two three", "four five six")

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOutSidecars(t, req, "book")
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Synthesizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	req, _ := fake.lastRequest(string(state.Synthesizing))
	// DENIED: no transcripts, no manifest, no qa artifacts in the staged dir.
	walkAssertNo(t, req.Dir, "transcripts")
	walkAssertNo(t, req.Dir, audio.ManifestName)
	walkAssertNo(t, req.Dir, "qa_report")
	// ALLOWED: the facts notes and the authoring contract are staged.
	if !fileExistsT(filepath.Join(req.Dir, factsDir, "knowledge-final.md")) {
		t.Error("facts/knowledge-final.md was not staged")
	}
	if !fileExistsT(filepath.Join(req.Dir, authoringName)) {
		t.Error("authoring.md was not staged")
	}
}

func TestSynthesizeCapViolationRetriesThenParks(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedFacts(t, work)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		// A description over the 1500 cap fails validation every attempt.
		c := baseChars("book")
		c.Characters[0].Description = strings.Repeat("a", capDescription+1)
		writeOut(t, req, charactersFileName, c)
		writeOut(t, req, recapsFileName, baseRecaps("book"))
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Synthesizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if n := fake.count(string(state.Synthesizing)); n != 3 {
		t.Errorf("agent invoked %d times, want 3 (initial + 2 retries)", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.Synthesizing)), "cap") {
		t.Errorf("retry prompt did not carry the cap validator error; got %q", fake.lastPrompt(string(state.Synthesizing)))
	}
	if scheduler.SentinelExists(work, string(state.Synthesizing)) {
		t.Error("sentinel written despite parking")
	}
}

// --- validating ---

func TestValidateCleanSidecars(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedWorkSidecars(t, work, baseChars("book"), baseRecaps("book"))
	seedTranscriptsText(t, work, "wholly unrelated narration alpha", "distinct beta gamma delta")

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Validating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("validating: %v", err)
	}
	rep := readValidationReport(t, work)
	if !rep.Clean || len(rep.Errors) != 0 || len(rep.Warnings) != 0 {
		t.Errorf("report = %+v, want clean with no errors/warnings", rep)
	}
	if !scheduler.SentinelExists(work, string(state.Validating)) {
		t.Error("validating sentinel missing")
	}
	var m struct {
		Errors   int `json:"errors"`
		Warnings int `json:"warnings"`
	}
	_ = json.Unmarshal(res.Metrics, &m)
	if m.Errors != 0 || m.Warnings != 0 {
		t.Errorf("metrics errors=%d warnings=%d, want 0/0", m.Errors, m.Warnings)
	}
}

func TestValidateNgramCatchesVerbatim(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	// An 8+ word run copied verbatim from the transcript into a description.
	const stolen = "the ancient tower stood alone against the crimson sky"
	chars := baseChars("book")
	chars.Characters[0].Description = "In this book, " + stolen + " throughout."
	seedWorkSidecars(t, work, chars, baseRecaps("book"))
	seedTranscriptsText(t, work, "before it "+stolen+" after it", "unrelated text here")

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Validating, scheduler.StageReport{}); err != nil {
		t.Fatalf("validating: %v", err)
	}
	rep := readValidationReport(t, work)
	if rep.Clean {
		t.Fatal("report is clean, want a near-verbatim error")
	}
	if !containsSub(rep.Errors, "near-verbatim overlap") {
		t.Errorf("errors %v missing the ngram overlap", rep.Errors)
	}
}

func TestValidateFlagsEmDash(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	chars := baseChars("book")
	chars.Characters[0].Description = "A knight " + string(emDash) + " brave and true."
	seedWorkSidecars(t, work, chars, baseRecaps("book"))
	seedTranscriptsText(t, work, "unrelated one", "unrelated two")

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Validating, scheduler.StageReport{}); err != nil {
		t.Fatalf("validating: %v", err)
	}
	rep := readValidationReport(t, work)
	if rep.Clean || !containsSub(rep.Errors, "em dash") {
		t.Errorf("report = %+v, want an em-dash error", rep)
	}
}

// --- auditing ---

func seedForAudit(t *testing.T, work string, valClean bool) {
	t.Helper()
	seedSidecarManifest(t, work)
	seedFacts(t, work)
	seedWorkSidecars(t, work, baseChars("book"), baseRecaps("book"))
	writeJSON(t, filepath.Join(work, validationReportName), validationReport{Clean: valClean, Errors: errorsFor(valClean)})
}

func errorsFor(clean bool) []string {
	if clean {
		return []string{}
	}
	return []string{"synthetic error"}
}

func TestAuditPassPath(t *testing.T) {
	work := t.TempDir()
	seedForAudit(t, work, true)
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
		return agent.Result{Usage: agent.Usage{Model: "opus", Input: 80, Output: 40}}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if !res.AuditPassed {
		t.Error("AuditPassed = false, want true (clean pass)")
	}
	if !scheduler.SentinelExists(work, string(state.Auditing)) {
		t.Error("auditing sentinel missing")
	}
}

func TestAuditBlockerFails(t *testing.T) {
	work := t.TempDir()
	seedForAudit(t, work, true)
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
			{Severity: SeverityBlocker, Locus: "characters[0].description", Text: "spoiler", Evidence: "ch5", Suggestion: "move it"},
		}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if res.AuditPassed {
		t.Error("AuditPassed = true, want false (a BLOCKER finding)")
	}
	var m struct {
		Blocker int `json:"blocker"`
	}
	_ = json.Unmarshal(res.Metrics, &m)
	if m.Blocker != 1 {
		t.Errorf("metrics blocker=%d, want 1", m.Blocker)
	}
}

func TestAuditInconsistentPassOverridden(t *testing.T) {
	work := t.TempDir()
	seedForAudit(t, work, true) // validation clean, so the override is driven by the FIX finding
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{
			{Severity: SeverityFix, Locus: "recaps[0].text", Text: "too long", Evidence: "cap", Suggestion: "trim"},
		}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if res.AuditPassed {
		t.Error("AuditPassed = true, want false (Pass=true overridden by a FIX finding)")
	}
}

// TestValidateWarningRidesToAuditPass is the item-1 regression: a non-opener whose
// ONLY validation issue is the advisory missing chapter:0 series recap produces a
// CLEAN report (a warning, no error), so the audit's effectivePass is not blocked - a
// passing agent report yields AuditPassed=true and the book advances to ready. Before
// the errors/warnings split, that lone warning made validation_report.clean=false and
// burned the whole fix loop on a book that was actually fine.
func TestValidateWarningRidesToAuditPass(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A same-series predecessor holding a knowledge-final.md sheet makes the target a
	// NON-opener, so the missing chapter:0 series recap is a warning (not an error).
	predWork := filepath.Join(dir, "pred")
	if err := os.MkdirAll(filepath.Join(predWork, spelling.FactsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(predWork, spelling.FactsDir, knowledgeFinalName), []byte("# roster\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if _, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "p.m4b"), WorkDir: predWork, Title: "Book One", Series: "S", SeriesPos: "1"}); err != nil {
		t.Fatalf("create predecessor: %v", err)
	}

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedSidecarManifest(t, work)
	seedFacts(t, work)
	seedWorkSidecars(t, work, baseChars("book"), baseRecaps("book")) // no chapter:0 series recap
	seedTranscriptsText(t, work, "clean narration alpha bravo", "distinct charlie delta echo")

	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book", Series: "S", SeriesPos: "2"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	cfg := withSidecarAgent(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)

	// validating: a warning, no error -> the report is clean.
	if _, err := exe.Execute(context.Background(), book, state.Validating, scheduler.StageReport{}); err != nil {
		t.Fatalf("validating: %v", err)
	}
	rep := readValidationReport(t, work)
	if !rep.Clean {
		t.Fatalf("report clean=false, want clean (warning-only): %+v", rep)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("errors=%v, want none", rep.Errors)
	}
	if !containsSub(rep.Warnings, "should carry a chapter:0") {
		t.Errorf("warnings=%v, want the missing series-recap warning", rep.Warnings)
	}

	// auditing: open the run so usage recording has a target, then a passing agent
	// report yields AuditPassed (the warning did not block the pass).
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.Auditing), 1); err != nil {
		t.Fatal(err)
	}
	res, err := exe.Execute(context.Background(), book, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if !res.AuditPassed {
		t.Fatal("AuditPassed=false for a warning-only book with a passing agent, want true")
	}
	next, _, err := state.NextState(state.Auditing, state.Outcome{AuditPassed: res.AuditPassed})
	if err != nil {
		t.Fatal(err)
	}
	if next != state.Ready {
		t.Errorf("next state = %q, want ready", next)
	}
}

func TestAuditPassOverriddenByUncleanValidation(t *testing.T) {
	work := t.TempDir()
	seedForAudit(t, work, false) // validation NOT clean
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if res.AuditPassed {
		t.Error("AuditPassed = true, want false (unclean validation overrides pass)")
	}
}

// --- fixing ---

func TestFixReplacesSidecars(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedFacts(t, work)
	// Start from a card the fixer will replace.
	seedWorkSidecars(t, work, baseChars("book"), baseRecaps("book"))
	writeJSON(t, filepath.Join(work, validationReportName), validationReport{Clean: false, Errors: []string{"characters[0].description too long"}})
	writeJSON(t, filepath.Join(work, auditReportName), AuditReport{Pass: false, Findings: []AuditFinding{
		{Severity: SeverityFix, Locus: "characters[0].name", Text: "x", Evidence: "y", Suggestion: "z"},
	}})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		c := baseChars("book")
		c.Characters[0].Name = "Alicia the Fixed"
		writeOut(t, req, charactersFileName, c)
		writeOut(t, req, recapsFileName, baseRecaps("book"))
		return agent.Result{Usage: agent.Usage{Model: "opus", Input: 50, Output: 25}}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Fixing, scheduler.StageReport{}); err != nil {
		t.Fatalf("fixing: %v", err)
	}
	chars, _, err := loadWorkSidecars(work)
	if err != nil {
		t.Fatal(err)
	}
	if chars.Characters[0].Name != "Alicia the Fixed" {
		t.Errorf("fix did not replace the sidecar: name=%q", chars.Characters[0].Name)
	}
	if !scheduler.SentinelExists(work, string(state.Fixing)) {
		t.Error("fixing sentinel missing")
	}
}

func readValidationReport(t *testing.T, work string) validationReport {
	t.Helper()
	rep, err := loadValidationReport(work)
	if err != nil {
		t.Fatalf("read validation report: %v", err)
	}
	return rep
}

// --- integration: the audit/fix loop at the scheduler level ---

// startSidecarBook creates a book seeded with the synthesis prerequisites and forced
// to the synthesizing state, plus a running scheduler over the real executor. It
// returns the book, db, and a stop func.
func startSidecarBook(t *testing.T, fake *fakeRunner) (store.Book, *store.DB, func()) {
	t.Helper()
	dir := t.TempDir()
	workRoot := filepath.Join(dir, "work")
	work := filepath.Join(workRoot, "fixture")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedSidecarManifest(t, work)
	seedFacts(t, work)
	seedTranscriptsText(t, work, "clean narration alpha bravo", "distinct charlie delta echo")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	hub := events.NewHub(1024)
	cfg := withSidecarAgent(dir, fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	sched := scheduler.New(db, hub, exe, 2, workRoot, false)

	book, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	if err := db.SetBookPipelineState(context.Background(), book.ID, string(state.Synthesizing)); err != nil {
		t.Fatalf("set state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()
	sched.Notify()
	return book, db, func() { cancel(); <-done; _ = db.Close() }
}

func TestSidecarLoopAuditFailThenPass(t *testing.T) {
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		switch req.Stage {
		case string(state.Synthesizing), string(state.Fixing):
			writeOutSidecars(t, req, "book")
		case string(state.Auditing):
			if attempt == 1 {
				writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
					{Severity: SeverityBlocker, Locus: "characters[0].description", Text: "leak", Evidence: "ch3", Suggestion: "fix"},
				}})
			} else {
				writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
			}
		}
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	book, db, stop := startSidecarBook(t, fake)
	defer stop()

	final := waitState(t, db, book.ID, "done", 30*time.Second)
	if final.State != "done" {
		t.Fatalf("book state = %q (status %q err %q), want done", final.State, final.Status, final.Error)
	}
	// One fix round; validating and auditing each ran twice (the second entry proves
	// advance() cleared the re-entered sentinels).
	assertSuccesses(t, db, book.ID, string(state.Fixing), 1)
	assertSuccesses(t, db, book.ID, string(state.Validating), 2)
	assertSuccesses(t, db, book.ID, string(state.Auditing), 2)
}

func TestSidecarLoopFixCapParks(t *testing.T) {
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		switch req.Stage {
		case string(state.Synthesizing), string(state.Fixing):
			writeOutSidecars(t, req, "book")
		case string(state.Auditing):
			// The auditor never passes -> the fix loop runs to its cap.
			writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
				{Severity: SeverityBlocker, Locus: "characters[0].description", Text: "leak", Evidence: "ch3", Suggestion: "fix"},
			}})
		}
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	book, db, stop := startSidecarBook(t, fake)
	defer stop()

	final := waitStatus(t, db, book.ID, string(state.StatusNeedsAttention), 30*time.Second)
	if final.Status != string(state.StatusNeedsAttention) {
		t.Fatalf("status = %q (state %q), want needs_attention", final.Status, final.State)
	}
	if final.State != string(state.Auditing) {
		t.Errorf("parked at %q, want auditing", final.State)
	}
	// The park now carries the fix-count trajectory (never accepted: every round is a
	// BLOCKER, so acceptTrajectory refuses and the loop runs to the cap).
	if !strings.Contains(final.Error, "did not converge after 3 fix round(s)") || !strings.Contains(final.Error, "blockers 1") {
		t.Errorf("park reason = %q, want the fix-trajectory message with blockers", final.Error)
	}
	assertSuccesses(t, db, book.ID, string(state.Fixing), state.MaxFixAttempts)
}

func assertSuccesses(t *testing.T, db *store.DB, bookID int64, stage string, want int) {
	t.Helper()
	n, err := db.CountStageSuccesses(context.Background(), bookID, stage)
	if err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("%s succeeded %d times, want %d", stage, n, want)
	}
}
