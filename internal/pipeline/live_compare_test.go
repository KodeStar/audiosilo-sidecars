//go:build agentlive

package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// TestLiveCompareExisting runs only with -tags agentlive. It clones the durable,
// non-audio inputs from a completed work directory, regenerates facts and sidecars
// through the current branch, and preserves a compact comparison report. The source
// work directory is read-only throughout.
func TestLiveCompareExisting(t *testing.T) {
	source := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_WORK"))
	if source == "" {
		t.Skip("set AUDIOSILO_COMPARE_WORK to a completed work directory")
	}
	out := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_OUT"))
	if out == "" {
		t.Skip("set AUDIOSILO_COMPARE_OUT to an empty output directory")
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	resume := os.Getenv("AUDIOSILO_COMPARE_RESUME") == "1"
	if len(entries) != 0 && !resume {
		t.Fatalf("AUDIOSILO_COMPARE_OUT must be empty, or set AUDIOSILO_COMPARE_RESUME=1: entries=%d", len(entries))
	}
	if len(entries) == 0 {
		copyCompareInputs(t, source, out)
	} else {
		t.Logf("resuming comparison from %s", out)
	}

	backend := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_BACKEND"))
	if backend == "" {
		backend = agent.IDCodex
	}
	runner, availability, err := agent.Select(context.Background(), agent.SelectConfig{Backend: backend}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("select %s: %v", backend, err)
	}
	if runner == nil || !availability.Available {
		t.Fatalf("backend unavailable: %+v", availability)
	}
	book, db := compareBook(t, source, out)
	models := AgentModels{
		Claude: map[string]string{
			string(state.FactPass): "sonnet", string(state.Synthesizing): "opus", string(state.Auditing): "opus", string(state.Fixing): "opus",
		},
		OpenAI: map[string]string{
			string(state.FactPass): "gpt-5.6-terra", string(state.Synthesizing): "gpt-5.6-sol", string(state.Auditing): "gpt-5.6-sol", string(state.Fixing): "gpt-5.6-sol",
		},
	}
	exe := NewExecutor(Config{
		DB: db, Agent: runner, AgentAvail: availability, AgentModels: models,
		AgentTimeout: 60 * time.Minute, AgentConcurrency: 2,
	})

	type stageResult struct {
		Elapsed string          `json:"elapsed"`
		Metrics json.RawMessage `json:"metrics"`
	}
	results := map[string]stageResult{}
	ctx := context.Background()
	for _, stage := range []state.State{state.FactPass, state.Synthesizing, state.Validating, state.Auditing} {
		started := time.Now()
		res, runErr := exe.Execute(ctx, book, stage, scheduler.StageReport{
			Note: func(note string) { t.Log(note) },
		})
		if runErr != nil {
			t.Fatalf("%s after %s: %v", stage, time.Since(started).Round(time.Second), runErr)
		}
		results[string(stage)] = stageResult{Elapsed: time.Since(started).Round(time.Second).String(), Metrics: res.Metrics}
		if stage == state.Auditing && !res.AuditPassed {
			t.Log("new output did not pass its first adversarial audit; artifacts are preserved for review")
		}
	}

	oldChars, oldRecaps, err := loadWorkSidecars(source)
	if err != nil {
		t.Fatalf("load baseline sidecars: %v", err)
	}
	newChars, newRecaps, err := loadWorkSidecars(out)
	if err != nil {
		t.Fatalf("load generated sidecars: %v", err)
	}
	summary := map[string]any{
		"source": source, "backend": backend, "model_routing": models,
		"stages":    results,
		"baseline":  map[string]int{"characters": len(oldChars.Characters), "recaps": len(oldRecaps.Recaps)},
		"generated": map[string]int{"characters": len(newChars.Characters), "recaps": len(newRecaps.Recaps)},
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(out, "compare_summary.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("comparison preserved at %s", out)
}

// TestLiveFixExisting applies the production fixer/validator/auditor sequence to a
// preserved comparison directory. It is deliberately separate from the initial
// comparison so a costly first-pass result survives a process interruption and can
// be resumed without regenerating facts or sidecars.
func TestLiveFixExisting(t *testing.T) {
	out := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_OUT"))
	if out == "" {
		t.Skip("set AUDIOSILO_COMPARE_OUT to an existing comparison directory")
	}
	if !fsutil.IsFile(filepath.Join(out, auditReportName)) {
		t.Fatalf("comparison has no %s: %s", auditReportName, out)
	}
	backend := compareEnv("AUDIOSILO_COMPARE_BACKEND", agent.IDCodex)
	runner, availability, err := agent.Select(context.Background(), agent.SelectConfig{Backend: backend}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("select %s: %v", backend, err)
	}
	if runner == nil || !availability.Available {
		t.Fatalf("backend unavailable: %+v", availability)
	}
	book, db := compareBook(t, compareEnv("AUDIOSILO_COMPARE_WORK", out), out)
	models := AgentModels{
		Claude: map[string]string{string(state.Fixing): "opus", string(state.Auditing): "opus"},
		OpenAI: map[string]string{string(state.Fixing): "gpt-5.6-sol", string(state.Auditing): "gpt-5.6-sol"},
	}
	exe := NewExecutor(Config{
		DB: db, Agent: runner, AgentAvail: availability, AgentModels: models,
		AgentTimeout: 60 * time.Minute, AgentConcurrency: 1,
	})
	type stageResult struct {
		Round   int             `json:"round"`
		Stage   string          `json:"stage"`
		Elapsed string          `json:"elapsed"`
		Metrics json.RawMessage `json:"metrics"`
	}
	results := make([]stageResult, 0, state.MaxFixAttempts*3)
	passed := false
	for round := 1; round <= state.MaxFixAttempts; round++ {
		for _, stage := range []state.State{state.Fixing, state.Validating, state.Auditing} {
			started := time.Now()
			res, runErr := exe.Execute(context.Background(), book, stage, scheduler.StageReport{
				Note: func(note string) { t.Log(note) },
			})
			if runErr != nil {
				t.Fatalf("round %d %s after %s: %v", round, stage, time.Since(started).Round(time.Second), runErr)
			}
			results = append(results, stageResult{
				Round: round, Stage: string(stage), Elapsed: time.Since(started).Round(time.Second).String(), Metrics: res.Metrics,
			})
			if stage == state.Auditing && res.AuditPassed {
				passed = true
			}
		}
		if passed {
			break
		}
	}
	data, err := json.MarshalIndent(map[string]any{"backend": backend, "passed": passed, "stages": results}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(out, "compare_fix_summary.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if !passed {
		t.Logf("comparison still has audit findings after %d correction rounds", state.MaxFixAttempts)
	}
}

// TestLiveAuditExisting revalidates and independently audits a preserved comparison
// without first modifying its sidecars. It is the cheap final check after a bounded
// correction loop, and avoids paying for another fact pass or synthesis merely to
// reject an auditor's prior false positive.
func TestLiveAuditExisting(t *testing.T) {
	out := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_OUT"))
	if out == "" {
		t.Skip("set AUDIOSILO_COMPARE_OUT to an existing comparison directory")
	}
	if _, _, err := loadWorkSidecars(out); err != nil {
		t.Fatalf("comparison has no sidecars: %v", err)
	}
	backend := compareEnv("AUDIOSILO_COMPARE_BACKEND", agent.IDCodex)
	runner, availability, err := agent.Select(context.Background(), agent.SelectConfig{Backend: backend}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("select %s: %v", backend, err)
	}
	if runner == nil || !availability.Available {
		t.Fatalf("backend unavailable: %+v", availability)
	}
	book, db := compareBook(t, compareEnv("AUDIOSILO_COMPARE_WORK", out), out)
	models := AgentModels{
		Claude: map[string]string{string(state.Auditing): "opus"},
		OpenAI: map[string]string{string(state.Auditing): "gpt-5.6-sol"},
	}
	exe := NewExecutor(Config{
		DB: db, Agent: runner, AgentAvail: availability, AgentModels: models,
		AgentTimeout: 60 * time.Minute, AgentConcurrency: 1,
	})

	started := time.Now()
	validation, err := exe.Execute(context.Background(), book, state.Validating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("validating: %v", err)
	}
	audit, err := exe.Execute(context.Background(), book, state.Auditing, scheduler.StageReport{
		Note: func(note string) { t.Log(note) },
	})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	data, err := json.MarshalIndent(map[string]any{
		"backend": backend,
		"passed":  audit.AuditPassed,
		"elapsed": time.Since(started).Round(time.Second).String(),
		"stages": map[string]json.RawMessage{
			string(state.Validating): validation.Metrics,
			string(state.Auditing):   audit.Metrics,
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(out, "compare_audit_summary.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if !audit.AuditPassed {
		t.Log("preserved comparison still has audit findings")
	}
}

func compareEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// compareBook reproduces the production series lookup in the otherwise isolated
// live harness. A later volume needs both its series identity and the preceding
// work directory so fact extraction can inherit knowledge-final.md and sidecar
// stages can enforce the chapter-0 series recap contract.
func compareBook(t *testing.T, source, out string) (store.Book, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open comparison store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	title := compareEnv("AUDIOSILO_COMPARE_TITLE", "Comparison Book")
	author := compareEnv("AUDIOSILO_COMPARE_AUTHOR", "Unknown")
	series := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_SERIES"))
	seriesPos := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_SERIES_POS"))
	if series != "" {
		predecessor := strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_PREDECESSOR_WORK"))
		if predecessor == "" {
			t.Fatal("AUDIOSILO_COMPARE_PREDECESSOR_WORK is required when AUDIOSILO_COMPARE_SERIES is set")
		}
		if !fsutil.IsFile(filepath.Join(predecessor, factsDir, knowledgeFinalName)) {
			t.Fatalf("comparison predecessor has no %s: %s", filepath.Join(factsDir, knowledgeFinalName), predecessor)
		}
		_, err = db.CreateBook(context.Background(), store.NewBook{
			SourcePath: predecessor,
			WorkDir:    predecessor,
			Title:      compareEnv("AUDIOSILO_COMPARE_PREDECESSOR_TITLE", "Series predecessor"),
			Authors:    []string{author},
			Series:     series,
			SeriesPos:  compareEnv("AUDIOSILO_COMPARE_PREDECESSOR_SERIES_POS", "1"),
		})
		if err != nil {
			t.Fatalf("seed comparison predecessor: %v", err)
		}
	}

	book, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: source + "#isolated-comparison",
		WorkDir:    out,
		Title:      title,
		Authors:    []string{author},
		Series:     series,
		SeriesPos:  seriesPos,
		WorkID:     strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_WORK_ID")),
	})
	if err != nil {
		t.Fatalf("seed comparison target: %v", err)
	}
	return book, db
}

func TestCompareBookSeedsSeriesPredecessor(t *testing.T) {
	predecessor := t.TempDir()
	if err := os.MkdirAll(filepath.Join(predecessor, factsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(predecessor, factsDir, knowledgeFinalName), []byte("# Prior book\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_COMPARE_SERIES", "Example Series")
	t.Setenv("AUDIOSILO_COMPARE_SERIES_POS", "2")
	t.Setenv("AUDIOSILO_COMPARE_PREDECESSOR_WORK", predecessor)
	t.Setenv("AUDIOSILO_COMPARE_PREDECESSOR_SERIES_POS", "1")

	book, db := compareBook(t, t.TempDir(), t.TempDir())
	pred, found, err := findSeriesPredecessor(context.Background(), db, book)
	if err != nil {
		t.Fatal(err)
	}
	if !found || pred == nil || pred.WorkDir != predecessor {
		t.Fatalf("predecessor = %+v, found = %v; want %s", pred, found, predecessor)
	}
	exe := NewExecutor(Config{DB: db})
	opener, err := exe.isSeriesOpener(context.Background(), book)
	if err != nil {
		t.Fatal(err)
	}
	if opener {
		t.Fatal("series book with seeded predecessor reported as opener")
	}
}

func copyCompareInputs(t *testing.T, source, out string) {
	t.Helper()
	for _, name := range []string{"manifest.json", "chunk_plan.json", spelling.SpellingsFile} {
		copyCompareFile(t, filepath.Join(source, name), filepath.Join(out, name))
	}
	for _, dir := range []string{transcript.TextDir, spelling.CorrectedDir} {
		entries, err := os.ReadDir(filepath.Join(source, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".txt") {
				copyCompareFile(t, filepath.Join(source, dir, entry.Name()), filepath.Join(out, dir, entry.Name()))
			}
		}
	}
	factEntries, err := os.ReadDir(filepath.Join(source, factsDir))
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	for _, entry := range factEntries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "spellings-through-") && strings.HasSuffix(entry.Name(), ".md") {
			copyCompareFile(t, filepath.Join(source, factsDir, entry.Name()), filepath.Join(out, factsDir, entry.Name()))
		}
	}
}

func copyCompareFile(t *testing.T, source, dest string) {
	t.Helper()
	in, err := os.Open(source) //nolint:gosec // explicit local benchmark input
	if err != nil {
		t.Fatalf("open %s: %v", source, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) //nolint:gosec // benchmark scratch
	if err != nil {
		t.Fatalf("create %s: %v", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy %s: %v", source, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dest, err)
	}
}
