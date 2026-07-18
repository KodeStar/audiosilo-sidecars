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
	title := compareEnv("AUDIOSILO_COMPARE_TITLE", "Comparison Book")
	book := store.Book{
		ID: 1, SourcePath: source, WorkDir: out, Title: title,
		Authors: []string{compareEnv("AUDIOSILO_COMPARE_AUTHOR", "Unknown")},
		WorkID:  strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_WORK_ID")),
	}
	models := AgentModels{
		Claude: map[string]string{
			string(state.FactPass): "sonnet", string(state.Synthesizing): "opus", string(state.Auditing): "opus", string(state.Fixing): "opus",
		},
		OpenAI: map[string]string{
			string(state.FactPass): "gpt-5.6-terra", string(state.Synthesizing): "gpt-5.6-sol", string(state.Auditing): "gpt-5.6-sol", string(state.Fixing): "gpt-5.6-sol",
		},
	}
	exe := NewExecutor(Config{
		Agent: runner, AgentAvail: availability, AgentModels: models,
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
	book := store.Book{
		ID: 1, SourcePath: compareEnv("AUDIOSILO_COMPARE_WORK", out), WorkDir: out,
		Title:   compareEnv("AUDIOSILO_COMPARE_TITLE", "Comparison Book"),
		Authors: []string{compareEnv("AUDIOSILO_COMPARE_AUTHOR", "Unknown")},
		WorkID:  strings.TrimSpace(os.Getenv("AUDIOSILO_COMPARE_WORK_ID")),
	}
	models := AgentModels{
		Claude: map[string]string{string(state.Fixing): "opus", string(state.Auditing): "opus"},
		OpenAI: map[string]string{string(state.Fixing): "gpt-5.6-sol", string(state.Auditing): "gpt-5.6-sol"},
	}
	exe := NewExecutor(Config{
		Agent: runner, AgentAvail: availability, AgentModels: models,
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

func compareEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
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
