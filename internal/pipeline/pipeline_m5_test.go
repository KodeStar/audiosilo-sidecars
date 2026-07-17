package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// TestPipelineSpellingToSynthesisFlow drives a book from qa_sweep-clean through the
// three M5 stages this change owns (spelling_research -> correcting -> fact_pass) via
// the real scheduler and a fake agent runner, and asserts it hands off into
// synthesizing. It stops at the synthesizing handoff rather than driving to done so it
// stays independent of the concurrently-landing synthesis/audit stages: reaching
// synthesizing proves fact_pass completed and advanced.
func TestPipelineSpellingToSynthesisFlow(t *testing.T) {
	dir := t.TempDir()
	workRoot := filepath.Join(dir, "work")
	work := filepath.Join(workRoot, "fixture")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)

	// The inputs a book has once qa_sweep passes: a manifest + transcript text.
	seedSpellingInputs(t, work, 3)

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		switch req.Stage {
		case string(state.SpellingResearch):
			return validSpellingAct(t, "Fixture", []string{"marker_titles.txt"})(f, req, attempt)
		case string(state.FactPass):
			return factPassAct(t)(f, req, attempt)
		default:
			// Beyond fact_pass (synthesizing onward) is out of this change's scope; a
			// bare result lets those stages fail/park harmlessly - we only assert the
			// book reached the synthesizing handoff.
			return agent.Result{}, nil
		}
	}
	cfg := Config{
		DB:         db,
		DataDir:    dir,
		Agent:      fake,
		AgentAvail: agent.Availability{Backend: agent.IDClaude, Available: true},
		Fallback:   scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond),
	}
	exe := NewExecutor(cfg)
	sched := scheduler.New(db, hub, exe, 2, workRoot, false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()

	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(dir, "fixture.m4b"), WorkDir: work, Title: "Fixture",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Fast-forward to a clean qa_sweep result: the mechanical stages before it are not
	// under test here.
	if err := db.SetBookState(context.Background(), b.ID, string(state.SpellingResearch), "", "", ""); err != nil {
		t.Fatalf("set state: %v", err)
	}
	sched.Notify()

	final := waitState(t, db, b.ID, string(state.Synthesizing), 30*time.Second)
	cancel()
	<-done

	if final.State != string(state.Synthesizing) {
		t.Fatalf("book state = %q (status %q err %q), want synthesizing (the fact_pass handoff)", final.State, final.Status, final.Error)
	}
	// The M5 stages left their real artifacts (not stub sentinels): corrections,
	// spellings, the corrected layer, and the fact-pass knowledge-final sheet.
	for _, rel := range []string{
		"corrections.json", "spellings.json",
		filepath.Join(factsDir, knowledgeFinalName),
	} {
		if _, err := os.Stat(filepath.Join(work, rel)); err != nil {
			t.Errorf("expected artifact %s missing: %v", rel, err)
		}
	}
	// Each agent stage ran exactly once.
	if n := fake.count(string(state.SpellingResearch)); n != 1 {
		t.Errorf("spelling_research ran %d times, want 1", n)
	}
	if n := fake.count(string(state.FactPass)); n != 1 {
		t.Errorf("fact_pass ran %d times, want 1", n)
	}
}
