package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// scriptedMeta is a scripted MetaCoverage: only the series-prior lookup is meaningful.
type scriptedMeta struct {
	prior metaops.SeriesPrior
	err   error
	calls int
}

func (f *scriptedMeta) CoverageFor(context.Context, metaops.BookIdentity) (metaops.Coverage, error) {
	return metaops.Coverage{}, nil
}

func (f *scriptedMeta) CoverageForWork(context.Context, string) (metaops.Coverage, error) {
	return metaops.Coverage{}, nil
}

func (f *scriptedMeta) SeriesGlossary(context.Context, string) (metaops.Glossary, error) {
	return metaops.Glossary{}, nil
}

func (f *scriptedMeta) SeriesPriorFor(_ context.Context, _ metaops.SeriesPriorQuery) (metaops.SeriesPrior, error) {
	f.calls++
	return f.prior, f.err
}

// killingFloor is the predecessor material the live incident's book was missing.
func killingFloor() metaops.SeriesPrior {
	return metaops.SeriesPrior{
		WorkID: "killing-floor", Title: "Killing Floor", Authors: []string{"Lee Child"},
		InShort: "Reacher is arrested in Margrave.", Ending: "He leaves with the conspiracy broken.",
		FinalRecap: "The operation collapses.", FinalRecapChapter: 30,
	}
}

// priorExecutor builds an executor over db with a scripted meta client.
func priorExecutor(t *testing.T, db *store.DB, meta MetaCoverage) *Executor {
	t.Helper()
	return NewExecutor(Config{DB: db, DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0), Meta: meta})
}

// A locally processed predecessor already proves the book is not an opener, so the
// community database is never consulted (its knowledge sheet is inherited via facts/).
func TestIsSeriesOpenerLocalPredecessorWins(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	_ = newSeriesBook(t, db, root, "Jack Reacher", "1", true)
	two := newSeriesBook(t, db, root, "Jack Reacher", "2", false)

	meta := &scriptedMeta{prior: killingFloor()}
	opener, err := priorExecutor(t, db, meta).isSeriesOpener(context.Background(), two)
	if err != nil {
		t.Fatalf("isSeriesOpener: %v", err)
	}
	if opener {
		t.Error("a book with a local predecessor is not a series opener")
	}
	if meta.calls != 0 {
		t.Errorf("upstream consulted %d times despite a local predecessor", meta.calls)
	}
	if fileExistsT(filepath.Join(two.WorkDir, seriesPriorFile)) {
		t.Error("no prior record should be written when the predecessor ran locally")
	}
}

// The live incident: book 2's predecessor was never processed here because it is
// already covered upstream, so no LOCAL predecessor exists. Treating that as an
// opener made the auditor demand a chapter-0 series recap the validator rejected,
// and the fix loop could never converge.
func TestIsSeriesOpenerUpstreamPredecessorMakesItANonOpener(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	two := newSeriesBook(t, db, root, "Jack Reacher", "2", false)

	meta := &scriptedMeta{prior: killingFloor()}
	exe := priorExecutor(t, db, meta)
	opener, prior, err := exe.seriesStatus(context.Background(), two)
	if err != nil {
		t.Fatalf("seriesStatus: %v", err)
	}
	if opener {
		t.Fatal("a book whose predecessor is covered upstream is not a series opener")
	}
	if prior.WorkID != "killing-floor" || prior.FinalRecapChapter != 30 {
		t.Errorf("prior material not carried: %+v", prior)
	}
	if !fileExistsT(filepath.Join(two.WorkDir, seriesPriorFile)) {
		t.Error("the determination was not persisted")
	}
}

// No local predecessor and nothing upstream: unchanged behaviour, still an opener.
func TestIsSeriesOpenerWithNoPredecessorAnywhere(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	two := newSeriesBook(t, db, root, "Jack Reacher", "2", false)

	for name, meta := range map[string]MetaCoverage{
		"no prior upstream": &scriptedMeta{},
		"metadata disabled": nil,
	} {
		opener, err := priorExecutor(t, db, meta).isSeriesOpener(context.Background(), two)
		if err != nil {
			t.Fatalf("%s: isSeriesOpener: %v", name, err)
		}
		if !opener {
			t.Errorf("%s: want opener (today's behaviour), got non-opener", name)
		}
		if fileExistsT(filepath.Join(two.WorkDir, seriesPriorFile)) {
			t.Errorf("%s: a record was written with no prior material", name)
		}
	}

	// A seriesless book is trivially an opener and never reaches the lookup.
	meta := &scriptedMeta{prior: killingFloor()}
	loner := newSeriesBook(t, db, root, "", "1", false)
	opener, err := priorExecutor(t, db, meta).isSeriesOpener(context.Background(), loner)
	if err != nil || !opener {
		t.Fatalf("seriesless book: opener=%v err=%v", opener, err)
	}
	if meta.calls != 0 {
		t.Errorf("a seriesless book consulted upstream %d times", meta.calls)
	}
}

// THE STABILITY PROPERTY. The opener verdict is re-derived on every synthesis /
// audit / fix / validate entry. If an outage between rounds could flip a book back to
// opener, the chapter-0 recap already written would become a hard validation error the
// fixer is told to delete while the auditor keeps demanding it back - the deadlock.
func TestSeriesPriorIsStableAcrossAnOutage(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	two := newSeriesBook(t, db, root, "Jack Reacher", "2", false)

	if opener, err := priorExecutor(t, db, &scriptedMeta{prior: killingFloor()}).isSeriesOpener(context.Background(), two); err != nil || opener {
		t.Fatalf("first round: opener=%v err=%v, want a recorded non-opener", opener, err)
	}

	for name, meta := range map[string]*scriptedMeta{
		"upstream returns nothing": {},
		"upstream errors":          {err: errors.New("upstream down")},
	} {
		exe := priorExecutor(t, db, meta)
		opener, prior, err := exe.seriesStatus(context.Background(), two)
		if err != nil {
			t.Fatalf("%s: seriesStatus: %v", name, err)
		}
		if opener {
			t.Errorf("%s: the recorded determination was retracted", name)
		}
		if prior.WorkID != "killing-floor" || prior.FinalRecap == "" {
			t.Errorf("%s: recorded material not reused: %+v", name, prior)
		}
		if meta.calls != 0 {
			t.Errorf("%s: upstream was re-consulted %d times; the record must be preferred", name, meta.calls)
		}
	}
}

// A malformed record is a real error, not a silent absence: treating it as "no prior"
// would retract exactly the determination the file exists to hold.
func TestSeriesPriorMalformedRecordIsAnError(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	two := newSeriesBook(t, db, root, "Jack Reacher", "2", false)
	if err := os.WriteFile(filepath.Join(two.WorkDir, seriesPriorFile), []byte("{not json"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if _, err := priorExecutor(t, db, &scriptedMeta{}).isSeriesOpener(context.Background(), two); err == nil {
		t.Fatal("a malformed series_prior.json must fail loudly")
	}
}

func TestStageSeriesPriorWritesTheMaterial(t *testing.T) {
	work := t.TempDir()
	st, err := agent.New(work, "synthesizing", 1)
	if err != nil {
		t.Fatal(err)
	}
	p := seriesPrior{
		WorkID: "killing-floor", Title: "Killing Floor", Authors: []string{"Lee Child"},
		InShort: "Reacher is arrested in Margrave.", Ending: "He leaves with the conspiracy broken.",
		FinalRecap: "The operation collapses.", FinalRecapChapter: 30,
	}
	if err := stageSeriesPrior(st, p); err != nil {
		t.Fatalf("stageSeriesPrior: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(st.Dir(), seriesPriorStagedName)) //nolint:gosec // test artifact
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"Killing Floor", "Lee Child", "meta.audiosilo.app", "CC BY-SA 3.0",
		"own words", "Reacher is arrested in Margrave.", "He leaves with the conspiracy broken.",
		"through chapter 30", "The operation collapses.",
		// The volume is identified as AN earlier one, never as the adjacent one.
		"an earlier volume in this series",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("staged material missing %q:\n%s", want, body)
		}
	}
	// The lookup walks past nearer volumes that publish no recaps, so the header must
	// not assert adjacency: an agent told this is "the previous book" would present a
	// three-volumes-back recap as everything that has happened since.
	if strings.Contains(body, "immediately before") || strings.Contains(body, "volume before this book") {
		t.Errorf("staged material claims the volume is the adjacent one:\n%s", body)
	}
	if strings.Contains(body, "\u2014") {
		t.Error("staged material contains an em dash (hyphens only)")
	}
}

func TestStageSeriesPriorWritesNothingWithoutMaterial(t *testing.T) {
	work := t.TempDir()
	st, err := agent.New(work, "synthesizing", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageSeriesPrior(st, seriesPrior{}); err != nil {
		t.Fatalf("stageSeriesPrior: %v", err)
	}
	if fileExistsT(filepath.Join(st.Dir(), seriesPriorStagedName)) {
		t.Error("a book with no prior material must stage no file")
	}
}

// End to end through the real stage: the material reaches the synthesis agent's
// staged dir, and the prompt points at it only when it is there.
func TestSynthesizeStagesSeriesPrior(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	book := newSeriesBook(t, db, root, "Jack Reacher", "2", false)
	seedSidecarManifest(t, book.WorkDir)
	seedFacts(t, book.WorkDir)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOutSidecars(t, req, "book")
		return agent.Result{}, nil
	}
	cfg := withSidecarAgent(t.TempDir(), fake)
	cfg.DB = db
	cfg.Meta = &scriptedMeta{prior: killingFloor()}
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), book, state.Synthesizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	req, _ := fake.lastRequest(string(state.Synthesizing))
	if !fileExistsT(filepath.Join(req.Dir, seriesPriorStagedName)) {
		t.Fatalf("%s was not staged", seriesPriorStagedName)
	}
	if !strings.Contains(fake.lastPrompt(string(state.Synthesizing)), seriesPriorStagedName) {
		t.Error("the synthesis prompt does not point at the staged material")
	}
}

func TestSynthesizeStagesNoSeriesPriorForAnOpener(t *testing.T) {
	work := t.TempDir()
	seedSidecarManifest(t, work)
	seedFacts(t, work)

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOutSidecars(t, req, "book")
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	book := store.Book{ID: 1, Title: "Book", WorkDir: work}
	if _, err := exe.Execute(context.Background(), book, state.Synthesizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	req, _ := fake.lastRequest(string(state.Synthesizing))
	if fileExistsT(filepath.Join(req.Dir, seriesPriorStagedName)) {
		t.Error("an opener must stage no prior material")
	}
	if strings.Contains(fake.lastPrompt(string(state.Synthesizing)), seriesPriorStagedName) {
		t.Error("the prompt names a file that was never staged")
	}
}
