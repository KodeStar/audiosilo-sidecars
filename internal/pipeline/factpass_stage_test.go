package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// writeOutRaw writes a raw string to the staged out/ dir (for non-JSON agent output).
func writeOutRaw(t *testing.T, req agent.Request, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agent.OutPath(req.Dir), rel), []byte(content), 0o644); err != nil { //nolint:gosec // test artifact under out/
		t.Fatal(err)
	}
}

// seedFactPassInputs writes the chunk plan, per-chunk spelling sheets, and corrected
// chapter texts a completed correcting stage would leave, so fact_pass can run.
func seedFactPassInputs(t *testing.T, work string, chunks []chunkRange) chunkPlan {
	t.Helper()
	plan := chunkPlan{Chunks: chunks}
	for _, c := range chunks {
		plan.ChunkEnds = append(plan.ChunkEnds, c.To)
		// the chunk's spelling sheet
		sheet := filepath.Join(work, factsDir, spelling.SheetName(c.To))
		if err := fsutil.WriteFileAtomic(sheet, []byte("| canonical | ... |\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for k := c.From; k <= c.To; k++ {
			if err := transcript.WriteText(filepath.Join(work, spelling.CorrectedDir), k, "corrected chapter text"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeChunkPlan(work, plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// stagedChapterRange returns the min/max chapter number staged under the dir's
// transcripts-corrected/ (how the fake infers the chunk it is working).
func stagedChapterRange(t *testing.T, dir string) (int, int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, spelling.CorrectedDir))
	if err != nil {
		t.Fatalf("read staged corrected dir: %v", err)
	}
	lo, hi := 0, 0
	for _, ent := range entries {
		n, ok := transcript.ParseChapter(ent.Name())
		if !ok {
			continue
		}
		if lo == 0 || n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	if lo == 0 {
		t.Fatalf("no corrected chapters staged in %s", dir)
	}
	return lo, hi
}

// factPassAct writes valid fact-pass output for whatever chunk was staged (inferred
// from the staged corrected chapters and the prompt's last-chunk marker).
func factPassAct(t *testing.T) func(*fakeRunner, agent.Request, int) (agent.Result, error) {
	return func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		if strings.Contains(req.Prompt, "assembling the compact final knowledge sheet") {
			writeOutRaw(t, req, knowledgeFinalName, "ROSTER\n- a name\nREVEALS\n- a fact\nTHREADS\n- a question\nENDING\n- it ends\n")
			return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 200, Output: 50, CostUSD: 0.05}}, nil
		}
		from, to := stagedChapterRange(t, req.Dir)
		var facts strings.Builder
		for k := from; k <= to; k++ {
			fmt.Fprintf(&facts, "## Chapter %d\nEVENTS:\n- something happens [ch%d @ 00:00-00:10]\n\n", k, k)
		}
		writeOutRaw(t, req, factsChunkName(from, to), facts.String())
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 300, Output: 150, CostUSD: 0.1}}, nil
	}
}

func TestFactPassTwoChunkHappyPath(t *testing.T) {
	work := t.TempDir()
	plan := seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 2}, {From: 3, To: 4}})

	fake := newFakeRunner()
	fake.act = factPassAct(t)
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.FactPass, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("fact_pass: %v", err)
	}
	for _, name := range []string{
		factsChunkName(1, 2),
		factsChunkName(3, 4),
		knowledgeFinalName,
	} {
		if _, err := os.Stat(filepath.Join(work, factsDir, name)); err != nil {
			t.Errorf("facts/%s missing: %v", name, err)
		}
	}
	if n := fake.count(string(state.FactPass)); n != 3 {
		t.Errorf("agent invoked %d times, want 3 (two chunks plus one assembly)", n)
	}
	if !scheduler.SentinelExists(work, string(state.FactPass)) {
		t.Error("fact_pass sentinel missing")
	}
	// fact_pass is NOT a web stage.
	if r, ok := fake.lastRequest(string(state.FactPass)); !ok || r.Web {
		t.Errorf("fact_pass request web = %v, want false", r.Web)
	}
	// Metrics report both chunks.
	if !strings.Contains(string(res.Metrics), `"chunks":2`) {
		t.Errorf("metrics = %s, want chunks:2", res.Metrics)
	}
	_ = plan
}

func TestFactPassResumesSkippingCompleteChunks(t *testing.T) {
	work := t.TempDir()
	seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 2}, {From: 3, To: 4}})
	// Pre-complete chunk 1 (its independent facts file already exists).
	if err := fsutil.WriteFileAtomic(filepath.Join(work, factsDir, factsChunkName(1, 2)), []byte("## Chapter 1\n## Chapter 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = factPassAct(t)
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.FactPass, scheduler.StageReport{}); err != nil {
		t.Fatalf("fact_pass: %v", err)
	}
	// Only chunk 2 plus the final assembly ran.
	if n := fake.count(string(state.FactPass)); n != 2 {
		t.Errorf("agent invoked %d times, want 2 (one remaining chunk plus assembly)", n)
	}
	if _, err := os.Stat(filepath.Join(work, factsDir, knowledgeFinalName)); err != nil {
		t.Errorf("knowledge-final.md missing after resume: %v", err)
	}
}

func TestFactPassHeadingValidationRetries(t *testing.T) {
	work := t.TempDir()
	seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 2}})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		// A facts file missing the '## Chapter 2' heading fails validation every round.
		writeOutRaw(t, req, factsChunkName(1, 2), "## Chapter 1\nEVENTS:\n- a thing\n")
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.FactPass, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if n := fake.count(string(state.FactPass)); n != 3 {
		t.Errorf("agent invoked %d times, want 3", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.FactPass)), "Chapter 2") {
		t.Errorf("retry prompt did not carry the missing-heading error; got %q", fake.lastPrompt(string(state.FactPass)))
	}
}

func TestFactPassChunkStagingInvariant(t *testing.T) {
	work := t.TempDir()
	seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 2}, {From: 3, To: 4}})

	fake := newFakeRunner()
	fake.act = factPassAct(t)
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.FactPass, scheduler.StageReport{}); err != nil {
		t.Fatalf("fact_pass: %v", err)
	}
	// The first chunk's staged dir must hold ONLY chapters 1-2 of the corrected layer.
	c1 := filepath.Join(work, "_runs", "fact_pass-c01-a01", spelling.CorrectedDir)
	for _, k := range []int{1, 2} { // allowed
		if !fsutil.IsFile(filepath.Join(c1, transcript.TextName(k))) {
			t.Errorf("chunk 1 staged dir is missing chapter %d", k)
		}
	}
	for _, k := range []int{3, 4} { // denied - a later chapter must never be staged
		if fsutil.IsFile(filepath.Join(c1, transcript.TextName(k))) {
			t.Errorf("chunk 1 staged dir leaked later chapter %d (spoiler-scope violation)", k)
		}
	}
}

func TestFactPassStagesInheritedSheetForPredecessor(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	pred := newSeriesBook(t, db, root, "Saga", "1", true) // seeds facts/knowledge-final.md

	work := filepath.Join(root, "work-saga-2")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 2}})
	book, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(root, "saga-2.m4b"), WorkDir: work, Title: "Saga 2", Series: "Saga", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}

	fake := newFakeRunner()
	fake.act = factPassAct(t)
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), book, state.FactPass, scheduler.StageReport{}); err != nil {
		t.Fatalf("fact_pass: %v", err)
	}
	// The predecessor's knowledge-final.md was staged for the independent chunk.
	staged := filepath.Join(work, "_runs", "fact_pass-c01-a01", knowledgeInheritedName)
	if !fsutil.IsFile(staged) {
		t.Errorf("inherited knowledge sheet not staged at %s", staged)
	}
	// And the prompt told the agent it inherited a previous book.
	r, ok := fake.lastRequest(string(state.FactPass))
	if !ok || !strings.Contains(strings.ToLower(r.Prompt), "previous book") {
		t.Errorf("fact_pass prompt did not flag the inherited sheet; prompt=%q", r.Prompt)
	}
	_ = pred
}

func TestFactPassRunsIndependentChunksConcurrently(t *testing.T) {
	work := t.TempDir()
	seedFactPassInputs(t, work, []chunkRange{{From: 1, To: 1}, {From: 2, To: 2}})

	fake := newFakeRunner()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		if strings.Contains(req.Prompt, "assembling the compact final knowledge sheet") {
			writeOutRaw(t, req, knowledgeFinalName, "ROSTER\nREVEALS\nTHREADS\nENDING\n")
			return agent.Result{}, nil
		}
		started <- struct{}{}
		<-release
		from, to := stagedChapterRange(t, req.Dir)
		writeOutRaw(t, req, factsChunkName(from, to), fmt.Sprintf("## Chapter %d\nEVENTS:\n- fact\n", from))
		return agent.Result{}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.AgentConcurrency = 2
	done := make(chan error, 1)
	go func() {
		_, err := NewExecutor(cfg).Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.FactPass, scheduler.StageReport{})
		done <- err
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("both independent chunks did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("fact_pass: %v", err)
	}
}
