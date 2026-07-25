package benchmark

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func TestCalibrateRequiresExplicitSingleCase(t *testing.T) {
	if _, err := Calibrate(context.Background(), RunOptions{ResultsDir: t.TempDir(), ProfileID: "all", CaseID: "case"}); err == nil || !strings.Contains(err.Error(), "explicit profile and case") {
		t.Fatalf("err=%v", err)
	}
}

type benchmarkRunner struct {
	id       string
	requests []agent.Request
}

func (r *benchmarkRunner) ID() string { return r.id }
func (r *benchmarkRunner) Detect(context.Context) agent.Availability {
	return agent.Availability{Backend: r.id, Available: true, Path: "/fake/" + r.id, Version: "fake"}
}
func (r *benchmarkRunner) SupportsWeb() bool { return false }
func (r *benchmarkRunner) Run(_ context.Context, req agent.Request) (agent.Result, error) {
	r.requests = append(r.requests, req)
	if err := os.MkdirAll(agent.OutPath(req.Dir), 0o750); err != nil {
		return agent.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(agent.OutPath(req.Dir), "audit.json"), []byte("{\"pass\":true,\"findings\":[]}\n"), 0o644); err != nil {
		return agent.Result{}, err
	}
	return agent.Result{Usage: agent.Usage{Model: req.Model}}, nil
}

func TestPrepareCopiesOnlyCheckpointAndReferenceArtifacts(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, source, "manifest.json", `{}`)
	writeTestFile(t, source, "marker_titles.txt", "Chapter 1\n")
	writeTestFile(t, source, "transcripts-text/ch001.txt", "Private transcript")
	writeTestFile(t, source, "transcripts-repaired/ch001.txt", "Repaired transcript")
	writeTestFile(t, source, "transcripts-raw/ch001.json", `{"private":true}`)
	writeTestFile(t, source, "chapters/ch001.flac", "audio")
	writeTestFile(t, source, "sidecars/characters.json", `{"characters":[]}`)
	writeTestFile(t, source, "sidecars/recaps.json", `{"recaps":[]}`)
	writeTestFile(t, source, "facts/knowledge-final.md", "# ROSTER")
	writeTestFile(t, source, "audit.json", `{"pass":true,"findings":[]}`)
	writeTestFile(t, source, "chunk_plan.json", `{}`)
	writeTestFile(t, source, "corrections.json", `{}`)
	writeTestFile(t, source, "spellings.json", `{}`)
	writeTestFile(t, source, "validation_report.json", `{"clean":true}`)
	writeTestFile(t, source, "_runs/leak.txt", "no")

	dst := filepath.Join(t.TempDir(), "suite")
	suite, err := Prepare(SuiteSpec{Version: 1, Cases: []CaseSpec{{ID: "case-1", Title: "Book", SourceDir: source}}}, dst, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(suite.Cases) != 1 || suite.Cases[0].SHA256 == "" {
		t.Fatalf("suite=%+v", suite)
	}
	for _, rel := range []string{"cases/case-1/input/transcripts-raw/ch001.json", "cases/case-1/input/chapters/ch001.flac", "cases/case-1/reference/_runs/leak.txt"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); !os.IsNotExist(err) {
			t.Errorf("private excluded artifact copied: %s", rel)
		}
	}
	for _, rel := range []string{"cases/case-1/input/transcripts-text/ch001.txt", "cases/case-1/reference/sidecars/characters.json", "suite.yaml"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if _, err := Prepare(SuiteSpec{Version: 1, Cases: []CaseSpec{{ID: "case-1", Title: "Book", SourceDir: source}}}, dst, time.Now()); err == nil {
		t.Error("Prepare should refuse non-empty destination")
	}
}

func TestLoadMatrixRejectsUnknownStageAndEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.yaml")
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), "version: 1\nprofiles:\n  - id: bad\n    backend: codex\n    max_agents_per_book: 1\n    models: {not_a_stage: x}\n    judges: [{backend: codex, model: x}]\n")
	if _, err := LoadMatrix(path); err == nil || !strings.Contains(err.Error(), "invalid model route") {
		t.Fatalf("err=%v", err)
	}
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), "version: 1\nprofiles:\n  - id: bad\n    backend: codex\n    max_agents_per_book: 1\n    models: {fact_pass: x}\n    efforts: {fact_pass: enormous}\n    judges: [{backend: codex, model: x}]\n")
	if _, err := LoadMatrix(path); err == nil || !strings.Contains(err.Error(), "invalid effort route") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadMatrixRequiresEveryReplayStage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.yaml")
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), "version: 1\nprofiles:\n  - id: incomplete\n    backend: codex\n    max_agents_per_book: 1\n    models: {fact_pass: x}\n    judges: [{backend: codex, model: x}]\n")
	if _, err := LoadMatrix(path); err == nil || !strings.Contains(err.Error(), "missing model route") {
		t.Fatalf("err=%v", err)
	}
}

func TestCommittedMatricesValidate(t *testing.T) {
	for _, path := range []string{"../../benchmarks/codex-matrix.yaml", "../../benchmarks/codex-effort-matrix.yaml", "../../benchmarks/cross-provider-matrix.yaml"} {
		if _, err := LoadMatrix(path); err != nil {
			t.Errorf("LoadMatrix(%s): %v", path, err)
		}
	}
}

func TestLoadSuiteRejectsTraversalAndBadDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suite.yaml")
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), "version: 1\ncases:\n  - id: unsafe\n    title: Book\n    input_dir: ../outside\n    reference_dir: ref\n    input_sha256: '"+strings.Repeat("0", 64)+"'\n")
	if _, err := LoadSuite(path); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("traversal err=%v", err)
	}
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), "version: 1\ncases:\n  - id: bad-digest\n    title: Book\n    input_dir: input\n    reference_dir: ref\n    input_sha256: nope\n")
	if _, err := LoadSuite(path); err == nil || !strings.Contains(err.Error(), "input_sha256") {
		t.Fatalf("digest err=%v", err)
	}
}

func TestScoreUsesSetOverlapAsDiagnostic(t *testing.T) {
	work, ref := t.TempDir(), t.TempDir()
	writeTestFile(t, work, "validation_report.json", `{"clean":true,"errors":[],"warnings":["w"]}`)
	writeSidecars(t, work, []string{"Alice", "Bob"}, []int{0, 5, 10})
	writeSidecars(t, ref, []string{"alice", "Cara"}, []int{0, 6, 10})
	q := Score(work, ref)
	if !q.ValidationClean || q.ValidationWarnings != 1 || q.CharacterRecall != 0.5 || q.CharacterJaccard != 1.0/3.0 || q.RecapPointJaccard != 0.5 {
		t.Fatalf("quality=%+v", q)
	}
}

func TestBuildReportMarksUnknownCostAndFindsProfiles(t *testing.T) {
	root := t.TempDir()
	r := RunResult{Version: 1, Profile: "luna", Case: "c", Repeat: 1, Completed: true, WallSeconds: 60, Quality: Quality{ValidationClean: true, CharacterRecall: 1, RecapPointJaccard: 1}}
	r.Invocations = []store.AgentInvocation{{Status: "success", InputTokens: 10, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:01Z"}}
	raw, _ := json.Marshal(r)
	writeTestFile(t, root, "luna/c/r01/result.json", string(raw))
	report, err := BuildReport(root, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Profiles) != 1 || report.Profiles[0].CostEstimateComplete || report.Profiles[0].ReportedCostAvailable || report.Profiles[0].ReportedCostComplete || len(report.Pareto) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestBuildReportSeparatesReportedAndEstimatedCost(t *testing.T) {
	root := t.TempDir()
	estimated := 1.25
	r := RunResult{Version: 1, Profile: "claude", Case: "c", Repeat: 1, Completed: true, WallSeconds: 60}
	r.Invocations = []store.AgentInvocation{{
		Status: "success", InputTokens: 10, CostUSD: 2.5, CostReported: true,
		EstimatedAPICostUSD: &estimated, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:01Z",
	}}
	raw, _ := json.Marshal(r)
	writeTestFile(t, root, "claude/c/r01/result.json", string(raw))
	report, err := BuildReport(root, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	p := report.Profiles[0]
	if !p.ReportedCostAvailable || !p.ReportedCostComplete || p.MeanReportedCostUSD != 2.5 || !p.CostEstimateComplete || p.MeanEstimatedCostUSD != estimated {
		t.Fatalf("profile=%+v", p)
	}
}

func TestRunContinuesAllTasksAndReturnsJoinedError(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "cases", "case-1", "input")
	if err := os.MkdirAll(input, 0o750); err != nil {
		t.Fatal(err)
	}
	digest, err := treeDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	suitePath := filepath.Join(root, "suite.yaml")
	if err := writeYAML(suitePath, Suite{Version: 1, Cases: []PreparedCase{{ID: "case-1", Title: "Book", InputDir: "cases/case-1/input", Reference: "cases/case-1/reference", SHA256: digest}}}); err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{}
	for _, stage := range replayAgentStages {
		routes[string(stage)] = "model"
	}
	matrixPath := filepath.Join(root, "matrix.yaml")
	if err := writeYAML(matrixPath, Matrix{Version: 1, Profiles: []Profile{
		{ID: "one", Backend: agent.IDCodex, Models: routes, MaxAgentsPerBook: 1, Judges: []Judge{{Backend: agent.IDCodex, Model: "judge"}}},
		{ID: "two", Backend: agent.IDCodex, Models: routes, MaxAgentsPerBook: 1, Judges: []Judge{{Backend: agent.IDCodex, Model: "judge"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	results, err := Run(context.Background(), RunOptions{
		SuitePath: suitePath, MatrixPath: matrixPath, ResultsDir: filepath.Join(root, "results"),
		RunnerFactory: func(context.Context, string, string) (agent.Runner, agent.Availability, error) {
			calls++
			return nil, agent.Availability{Backend: agent.IDCodex, Detail: "unavailable in test"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "runner unavailable") {
		t.Fatalf("Run error = %v, want joined runner failures", err)
	}
	if calls != 2 || len(results) != 2 {
		t.Fatalf("calls=%d results=%d, want every task attempted", calls, len(results))
	}
	for _, id := range []string{"one", "two"} {
		if _, statErr := os.Stat(filepath.Join(root, "results", id, "case-1", "r01", "result.json")); statErr != nil {
			t.Errorf("%s result was not persisted: %v", id, statErr)
		}
	}
}

func TestRunHoldoutUsesResolvedBackendAndRoutes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	manifest := audio.Manifest{Source: "/book", Style: audio.StyleMarkers, Duration: 10, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 1, Start: 0, End: 10, Duration: 10}}}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, source, "manifest.json", string(rawManifest))
	writeTestFile(t, source, "spellings.json", `{"title":"Book","chunk_ends":[],"preamble":[],"ledger":[],"unresolved":[],"clusters":[],"non_merges":[]}`)
	writeTestFile(t, source, "validation_report.json", `{"clean":true,"errors":[],"warnings":[]}`)
	writeTestFile(t, source, "facts/knowledge-final.md", "# Facts\n")
	writeTestFile(t, source, "sidecars/characters.json", `{}`)
	writeTestFile(t, source, "sidecars/recaps.json", `{}`)

	runner := &benchmarkRunner{id: agent.IDCodex}
	opts := RunOptions{Timeout: time.Minute, RunnerFactory: func(_ context.Context, backend, path string) (agent.Runner, agent.Availability, error) {
		if backend != agent.IDCodex || path != "/fake/codex" {
			t.Fatalf("factory route = %s %s", backend, path)
		}
		return runner, runner.Detect(context.Background()), nil
	}}
	profile := Profile{ID: "candidate", Backend: agent.IDClaude}
	judge := Judge{Backend: agent.IDCodex, CLIPath: "/fake/codex", Model: "judge-model", Effort: "high"}
	holdout, err := runHoldout(context.Background(), opts, Matrix{}, PreparedCase{ID: "case-1", Title: "Book"}, profile, judge, source, filepath.Join(root, "run"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !holdout.Passed || len(runner.requests) != 1 {
		t.Fatalf("holdout=%+v requests=%d", holdout, len(runner.requests))
	}
	req := runner.requests[0]
	if req.Stage != string(state.Auditing) || req.Model != "judge-model" || req.Effort != "high" {
		t.Fatalf("judge request = %+v", req)
	}
}

func writeSidecars(t *testing.T, root string, names []string, points []int) {
	t.Helper()
	chars := map[string]any{"characters": []map[string]string{}}
	rows := make([]map[string]string, 0, len(names))
	for _, n := range names {
		rows = append(rows, map[string]string{"name": n})
	}
	chars["characters"] = rows
	recaps := make([]map[string]any, 0, len(points))
	for _, p := range points {
		recaps = append(recaps, map[string]any{"through": map[string]int{"chapter": p}})
	}
	cb, _ := json.Marshal(chars)
	rb, _ := json.Marshal(map[string]any{"recaps": recaps})
	writeTestFile(t, root, "sidecars/characters.json", string(cb))
	writeTestFile(t, root, "sidecars/recaps.json", string(rb))
}

func writeTestFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
