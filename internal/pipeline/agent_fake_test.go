package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
)

// fakeRunner is a scripted agent.Runner for pipeline tests. It records every Request
// and, per Run, invokes an `act` hook that writes the agent's output files into
// req.Dir/out and returns a Result (carrying a scripted Usage). Later waves reuse it
// by setting `act` to a per-stage script (keyed on req.Stage) and, for validator-retry
// tests, branching on the per-stage attempt number it is passed.
//
// Availability is scripted via `available`; ensureAgent adopts it only when the
// executor is built with AgentAvail.Available true (or redetectAgent returns it).
type fakeRunner struct {
	mu        sync.Mutex
	idStr     string
	web       bool
	available bool
	calls     []agent.Request
	attempts  map[string]int
	usage     agent.Usage

	// act runs on each Run: it writes outputs under agent.OutPath(req.Dir) and
	// returns the Result/error. attempt is the 1-based per-stage invocation count (so a
	// script can fail validation the first N times and pass after). nil act -> a bare
	// success carrying f.usage and writing nothing.
	act func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error)
}

// newFakeRunner returns an available claude-identified fake runner.
func newFakeRunner() *fakeRunner {
	return &fakeRunner{idStr: agent.IDClaude, web: true, available: true, attempts: map[string]int{}}
}

func (f *fakeRunner) ID() string        { return f.idStr }
func (f *fakeRunner) SupportsWeb() bool { return f.web }
func (f *fakeRunner) Detect(context.Context) agent.Availability {
	return agent.Availability{Backend: f.idStr, Available: f.available, Version: "fake"}
}

func (f *fakeRunner) Run(ctx context.Context, req agent.Request) (agent.Result, error) {
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.attempts[req.Stage]++
	attempt := f.attempts[req.Stage]
	act := f.act
	usage := f.usage
	f.mu.Unlock()
	if act != nil {
		return act(f, req, attempt)
	}
	return agent.Result{Text: "ok", Usage: usage}, nil
}

// count returns how many times the fake ran for a stage.
func (f *fakeRunner) count(stage string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[stage]
}

// lastPrompt returns the prompt of the most recent call for a stage (to assert the
// runner saw an appended validator error on a retry).
func (f *fakeRunner) lastPrompt(stage string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Stage == stage {
			return f.calls[i].Prompt
		}
	}
	return ""
}

// lastRequest returns the most recent Request for a stage.
func (f *fakeRunner) lastRequest(stage string) (agent.Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Stage == stage {
			return f.calls[i], true
		}
	}
	return agent.Request{}, false
}

// writeOut writes JSON-marshaled v to req.Dir/out/rel (the agent's output dir), for a
// scripted act to emit an agent artifact.
func writeOut(t *testing.T, req agent.Request, rel string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent.OutPath(req.Dir), rel), append(data, '\n'), 0o644); err != nil { //nolint:gosec // test artifact under the staged out/ dir
		t.Fatal(err)
	}
}

// withAgent builds a Config with the fake runner marked available plus a seeded
// claude model map, so a stage's ensureAgent adopts it without a redetect.
func withAgentConfig(dataDir string, fake *fakeRunner) Config {
	return Config{
		DataDir:    dataDir,
		Agent:      fake,
		AgentAvail: agent.Availability{Backend: agent.IDClaude, Available: true, Version: "fake"},
		AgentModels: AgentModels{Claude: map[string]string{
			"markers_normalizing": "sonnet",
			"qa_adjudicating":     "sonnet",
		}},
	}
}
