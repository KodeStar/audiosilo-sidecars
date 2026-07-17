package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/agent/prompts"
	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// AgentUnavailableMsg is the needs_attention reason an agent stage parks a book with
// when no agent backend can run (no claude/codex CLI resolved, or an explicit
// configured path is missing). It is a human-fixable precondition - install a CLI or
// fix the config path, then Retry - so parking (which Retry re-admits) fits better
// than a hard failure, exactly like MediaToolsUnavailableMsg for the audio stages.
const AgentUnavailableMsg = "agent backend unavailable - install the claude or codex CLI, or set agent.backend/claude_path/codex_path in config.yaml, then Retry"

// Park-reason strings/prefixes for the remaining agent/spelling park conditions. They
// are consts (not inline at the throw site) so the tests that assert them cite one
// source of truth. Typed ParkReason codes are deferred to M6; these are just strings.
const (
	// AgentRateLimitedPrefix prefixes the park reason when the agent backend is
	// rate-limited (the backend's detail + a "retry later" tail follow).
	AgentRateLimitedPrefix = "agent backend is rate-limited"
	// AgentValidationExhaustedPrefix prefixes the park reason when an agent's output
	// fails validation after the retry budget (the validator error follows).
	AgentValidationExhaustedPrefix = "agent output failed validation after retries"
	// MarkersNotConfidentPrefix prefixes the park reason when marker normalization
	// needs a human (the agent's verdict reason follows).
	MarkersNotConfidentPrefix = "marker normalization needs a human"
	// QANoConvergeMsg is the park reason when QA adjudication does not converge.
	QANoConvergeMsg = "QA adjudication did not converge after 3 rounds - see qa_report.md"
	// SpellingGateFailurePrefix prefixes the park reason when the spelling gate Check
	// fails (the gate summary follows).
	SpellingGateFailurePrefix = "spelling corrections failed the gates - fix spelling_research and retry"
)

// ensureAgent re-selects an agent runner when the frozen one is unavailable, adopting
// a now-available result for this and future runs (the operator installs a CLI, then
// Retry). Detect is cheap (PATH/stat + a fast --version). It mirrors ensureASR.
func (e *Executor) ensureAgent(ctx context.Context) (agent.Runner, agent.Availability) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.agentRun != nil && e.agentAvail.Available {
		return e.agentRun, e.agentAvail
	}
	if r, av := e.redetectAgent(ctx); r != nil && av.Available {
		e.agentRun, e.agentAvail = r, av
	}
	return e.agentRun, e.agentAvail
}

// defaultRedetectAgent re-runs agent.Select and reports a usable runner (or nil when
// none is available). It is the production redetectAgent.
func (e *Executor) defaultRedetectAgent(ctx context.Context) (agent.Runner, agent.Availability) {
	r, av, _ := agent.Select(ctx, e.agentSelect, e.secrets)
	return r, av
}

// AgentStatus returns the executor's current agent-runner availability (which a stage
// may have re-detected after a retry), for /system. Safe for concurrent use.
func (e *Executor) AgentStatus() agent.Availability {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.agentAvail
}

// agentUsage is the accumulated token/cost spend of all invocations in one agent
// stage run plus the invocation count and the productive agent seconds, returned by
// runAgent so a stage can fold it into its metrics and its StageResult.RateSample.
type agentUsage struct {
	agent.Usage
	Invocations int
	// Seconds is the productive agent wall-time this run: the time spent in the agent
	// invocations (validation retries included) with rate-limit backoff sleep excluded,
	// so it feeds a per-stage rate that reflects the model's work, not waiting.
	Seconds float64
}

// rateSample builds the one-shot (whole-book) rate sample for an agent stage: 1 unit
// spent in Seconds productive seconds, or nil when no agent work ran (e.g. an all-auto
// qa_adjudicating pass) so the scheduler records nothing.
func (u agentUsage) rateSample() *scheduler.RateSample {
	return rateSample(1, u.Seconds)
}

// add folds one invocation's usage into the running total (the six spend fields plus
// the last non-empty model). Callers own the Invocations count - runAgent's onUsage
// bumps it by one per invocation, a multi-invocation stage sums the per-run totals -
// because agent.Usage itself carries no invocation count.
func (u *agentUsage) add(x agent.Usage) {
	u.Input += x.Input
	u.Output += x.Output
	u.CacheRead += x.CacheRead
	u.CostUSD += x.CostUSD
	u.Turns += x.Turns
	if x.Model != "" {
		u.Model = x.Model
	}
}

// metricsMap renders the usage summary for a stage's StageResult.Metrics.
func (u agentUsage) metricsMap() map[string]any {
	return map[string]any{
		"usage": map[string]any{
			"model":         u.Model,
			"input_tokens":  u.Input,
			"output_tokens": u.Output,
			"cache_read":    u.CacheRead,
			"cost_usd":      u.CostUSD,
			"turns":         u.Turns,
			"invocations":   u.Invocations,
		},
	}
}

// validationError wraps a stage validator's failure so runAgent can distinguish an
// exhausted-after-retries output-validation failure (park, naming the artifact) from
// a backend/transport error (plain error) - RunWithRetry returns the validator's
// error verbatim when the retry budget is spent, so the wrapper survives to the
// errors.As check.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// runAgent is the shared driver every agent stage uses: ensure a runner, resolve the
// stage model, render the prompt, run it (with the agent package's invalid-output +
// rate-limit retry policy), and translate the outcome into the pipeline's park/fail
// vocabulary. It captures usage into the open stage_run after EVERY invocation
// (crash preserves spend) and returns the accumulated usage so the stage can fold it
// into its metrics. validate reads the agent's out/ files from st and returns a
// non-nil error to trigger a retry; when the retry budget is spent the stage parks.
//
// Errors are translated: a rate-limited backend and an unavailable backend PARK
// (actionable, Retry-able); an exhausted output validator PARKS naming why; any other
// error (render, transport, timeout) is returned as a plain error (StatusFailed).
func (e *Executor) runAgent(ctx context.Context, book store.Book, stage state.State, r scheduler.StageReport, st *agent.Staging, promptName string, promptData any, web bool, validate func(agent.Result, *agent.Staging) error) (agentUsage, error) {
	runner, av := e.ensureAgent(ctx)
	if runner == nil || !av.Available {
		return agentUsage{}, scheduler.ParkWithCode(state.ParkAgentUnavailable, AgentUnavailableMsg)
	}
	model := agent.ModelFor(e.agentModels.Claude, e.agentModels.OpenAI, runner.ID(), string(stage))

	prompt, err := prompts.Render(promptName, promptData)
	if err != nil {
		return agentUsage{}, fmt.Errorf("%s: render prompt: %w", stage, err)
	}

	var total agentUsage
	onUsage := func(u agent.Usage) {
		total.add(u)
		total.Invocations++
		if e.db != nil {
			// context.WithoutCancel: the invocation already happened, so record its spend
			// even if ctx is being cancelled - crash/cancel must not lose the accounting.
			if uerr := e.db.AddOpenStageRunUsage(context.WithoutCancel(ctx), book.ID, string(stage), u.Model, u.Input, u.Output, u.CostUSD); uerr != nil {
				e.log.Warn("agent: record usage", "book", book.ID, "stage", string(stage), "err", uerr)
			}
		}
	}

	req := agent.Request{
		Stage:   string(stage),
		Dir:     st.Dir(),
		Prompt:  prompt,
		Model:   model,
		Web:     web,
		Timeout: e.agentTimeout,
		// Liveness heartbeat: while the agent subprocess runs, emit a durable note so a
		// long stage (a 6-minute qa_adjudicating) visibly proves the daemon is alive. It
		// fires only while the child is running (never during rate-limit backoff).
		Heartbeat: func(elapsed time.Duration) {
			if r.Note != nil {
				r.Note(fmt.Sprintf("%s: still running (%s elapsed)", stage, humanDuration(elapsed)))
			}
		},
	}
	backoff := e.backoff
	if backoff == nil {
		backoff = agent.DefaultBackoff()
	}
	start := time.Now()
	_, slept, err := agent.RunWithBackoff(ctx, runner, req, func(res agent.Result) error {
		if verr := validate(res, st); verr != nil {
			return &validationError{msg: verr.Error()}
		}
		return nil
	}, onUsage, backoff)
	// Charge only productive agent time to the rate: wall-clock minus the rate-limit
	// backoff sleep RunWithRetry reports (validation retries stay counted - they are
	// genuine model cost - only waiting out a rate limit is excluded).
	if productive := time.Since(start) - slept; productive > 0 {
		total.Seconds = productive.Seconds()
	}
	if err != nil {
		var rl *agent.RateLimitError
		if errors.As(err, &rl) {
			return total, scheduler.ParkWithCode(state.ParkAgentRateLimited, AgentRateLimitedPrefix+" ("+rl.Detail+") - retry later")
		}
		var na *agent.NotAvailableError
		if errors.As(err, &na) {
			return total, scheduler.ParkWithCode(state.ParkAgentUnavailable, AgentUnavailableMsg)
		}
		var ve *validationError
		if errors.As(err, &ve) {
			return total, scheduler.ParkWithCode(state.ParkAgentValidationExhausted, AgentValidationExhaustedPrefix+": "+ve.msg)
		}
		return total, fmt.Errorf("%s: agent run: %w", stage, err)
	}
	return total, nil
}

// humanDuration renders d as a compact whole-unit elapsed string for a liveness note:
// "45s" under a minute, "6m" for whole minutes, "1h2m" past an hour. Precision beyond
// whole minutes/seconds is noise for a "still running" heartbeat.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%dm", h, m)
}

// countNoun renders a count with a naively pluralized noun for a human note:
// countNoun(1, "chunk") -> "1 chunk", countNoun(3, "chunk") -> "3 chunks". Adequate
// for the pipeline's simple nouns (chapter/chunk); it does not handle irregulars.
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// stageAttempt is a per-stage attempt number used only to name the staged dir
// (_runs/<stage>-a<NN>) so successive scheduler re-runs keep distinct debug dirs. It
// derives from the stage's run count (the scheduler has already opened the current
// run, so this is that run's attempt); it degrades to 1 with no db or on error.
func (e *Executor) stageAttempt(ctx context.Context, book store.Book, stage state.State) int {
	if e.db == nil {
		return 1
	}
	n, err := e.db.CountStageRuns(ctx, book.ID, string(stage))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// --- markers_normalizing (agent, Lane B, Web=false) ---

// markersPromptData feeds markers.md. Field names MUST match the template (rendered
// with missingkey=error, so a drift fails loudly at render time).
type markersPromptData struct {
	Title        string
	Authors      string
	Series       string
	SeriesPos    string
	Style        string
	Duration     float64
	ChapterCount int
}

// markerVerdict is the agent's confidence signal (out/verdict.json): a not-confident
// verdict parks the book for a human rather than adopting a guessed mapping.
type markerVerdict struct {
	Confident bool   `json:"confident"`
	Reason    string `json:"reason"`
}

// markersNormalize maps a non-contiguous recording's raw markers to logical work
// chapters via the agent, replacing manifest.json with a validated contiguous map. A
// not-confident verdict parks the book needs_attention (a human decision point, not a
// failure); an unavailable agent parks with AgentUnavailableMsg.
func (e *Executor) markersNormalize(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	draft, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: read manifest (inspect must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("normalizing markers over %s", countNoun(draft.ChapterCount, "chapter")))
	}
	st, err := agent.New(book.WorkDir, string(state.MarkersNormalizing), e.stageAttempt(ctx, book, state.MarkersNormalizing))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := st.CopyFile(filepath.Join(book.WorkDir, audio.ProbeName), audio.ProbeName); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: stage probe.json: %w", err)
	}
	if err := st.CopyFile(filepath.Join(book.WorkDir, audio.ManifestName), audio.ManifestName); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: stage manifest.json: %w", err)
	}

	inputPaths := make(map[string]bool, len(draft.Chapters))
	for _, ch := range draft.Chapters {
		if ch.FilePath != "" {
			inputPaths[ch.FilePath] = true
		}
	}
	validate := func(_ agent.Result, s *agent.Staging) error {
		// A not-confident verdict is a VALID terminal output: the agent followed the
		// prompt's "do not guess" instruction and declined to invent a mapping, so it may
		// legitimately have written no (or a partial) out/manifest.json. Accept it without
		// a retry and let the post-run path park with its reason. Only a CONFIDENT verdict
		// must satisfy the full manifest contract.
		v, verr := readMarkerVerdict(s.OutDir())
		if verr != nil {
			return fmt.Errorf("out/verdict.json: %v", verr)
		}
		if !v.Confident {
			return nil
		}
		return validateMarkersManifest(s.OutDir(), draft, inputPaths)
	}
	data := markersPromptData{
		Title:        book.Title,
		Authors:      authors(book),
		Series:       book.Series,
		SeriesPos:    book.SeriesPos,
		Style:        draft.Style,
		Duration:     draft.Duration,
		ChapterCount: draft.ChapterCount,
	}
	usage, err := e.runAgent(ctx, book, state.MarkersNormalizing, r, st, "markers.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	// A successful agent run that reports it could not produce a confident mapping is
	// a human decision point, not a failure: park needs_attention (do NOT harvest the
	// draft-quality manifest over the original).
	verdict, err := readMarkerVerdict(st.OutDir())
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: read verdict: %w", err)
	}
	if !verdict.Confident {
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "the agent could not produce a confident marker mapping"
		}
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMarkersNotConfident, MarkersNotConfidentPrefix+": "+reason)
	}

	if err := agent.Harvest(st, []agent.HarvestSpec{{From: audio.ManifestName, To: audio.ManifestName}}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: harvest manifest: %w", err)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	result := scheduler.StageResult{Metrics: metrics(usage.metricsMap()), RateSample: usage.rateSample()}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.MarkersNormalizing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// validateMarkersManifest checks a CONFIDENT agent's out/manifest.json against the
// contract: it parses as an audio.Manifest, keeps the draft's Style, numbers its
// chapters uniquely/orderly/contiguously, every interval is start<end within [0,
// Duration+1s], ChapterCount matches, and its file paths are a subset of the draft's
// (the agent may only renumber/exclude/retitle, never invent an interval or file). The
// caller gates this on a confident verdict (a not-confident verdict skips the manifest
// requirement entirely and parks with its reason), so it need not re-read the verdict.
func validateMarkersManifest(outDir string, draft audio.Manifest, inputPaths map[string]bool) error {
	raw, err := os.ReadFile(filepath.Join(outDir, audio.ManifestName)) //nolint:gosec // outDir is the agent's staged out/ dir under the work dir
	if err != nil {
		return fmt.Errorf("out/manifest.json: %v", err)
	}
	var m audio.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("out/manifest.json is not valid manifest JSON: %v", err)
	}
	if m.Style != draft.Style {
		return fmt.Errorf("style changed from %q to %q - you may not change the recording layout", draft.Style, m.Style)
	}
	if len(m.Chapters) == 0 {
		return fmt.Errorf("out/manifest.json has no chapters")
	}
	if !audio.Contiguous(m.Chapters) {
		return fmt.Errorf("chapter numbers must be unique, ordered, and contiguous (1,2,3,...)")
	}
	for _, ch := range m.Chapters {
		if ch.Start >= ch.End {
			return fmt.Errorf("chapter %d has start >= end", ch.Chapter)
		}
		if ch.Start < 0 || ch.End > draft.Duration+1.0 {
			return fmt.Errorf("chapter %d interval [%.3f,%.3f] is outside the recording [0,%.3f]", ch.Chapter, ch.Start, ch.End, draft.Duration)
		}
		if ch.FilePath != "" && !inputPaths[ch.FilePath] {
			return fmt.Errorf("chapter %d file path %q is not one of the draft manifest's files", ch.Chapter, ch.FilePath)
		}
	}
	if m.ChapterCount != len(m.Chapters) {
		return fmt.Errorf("chapter_count %d does not match the %d chapters", m.ChapterCount, len(m.Chapters))
	}
	return nil
}

// readMarkerVerdict parses out/verdict.json.
func readMarkerVerdict(outDir string) (markerVerdict, error) {
	raw, err := os.ReadFile(filepath.Join(outDir, "verdict.json")) //nolint:gosec // outDir is the agent's staged out/ dir under the work dir
	if err != nil {
		return markerVerdict{}, err
	}
	var v markerVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return markerVerdict{}, err
	}
	return v, nil
}

// --- qa_adjudicating (agent, Lane B, Web=false) ---

// adjudicatePromptData feeds adjudicate.md. AutoAccepted is the comma-joined chapter
// list the pipeline already accepted mechanically (item 4) - the agent must NOT
// disposition those; the template renders an optional block naming them.
type adjudicatePromptData struct {
	Title        string
	Round        int
	AutoAccepted string
}

// autoAcceptTailReason is the fixed reason on a pipeline-authored auto-accept entry.
const autoAcceptTailReason = "already repaired via tail_clip - splice present in transcripts-repaired"

// qaAdjudicate hands the QA sweep's findings to the agent, which writes a qa_plan.json
// dispositioning every flagged chapter (retranscribe / tail_clip / accept). It caps at
// 3 rounds (a plan that keeps re-queuing does not converge), stages only the flagged
// chapters' transcripts, validates the plan against the report, and reports
// RetranscribeNeeded so the state machine branches to retranscribing or
// spelling_research.
//
// Before invoking the agent it AUTO-ACCEPTS every flagged chapter that a prior
// tail_clip round already repaired and whose only findings are tail-related (item 4):
// the tail_rate detector reads transcripts-json/, which a splice does not touch, so
// the hit persists on re-sweep even though the repaired layer is fixed - without this,
// convergence would depend on the agent choosing "accept", and an agent that re-picks
// tail_clip forever would park the book at the round cap despite it being repaired.
// The auto-accept is decided in the STAGE (never in the golden-tested qa detectors).
// When every flagged chapter is auto-accepted, the agent is not invoked at all.
func (e *Executor) qaAdjudicate(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	// Round cap FIRST: CountStageSuccesses is completed rounds (the current run is still
	// open, so not counted). 3 completed rounds without convergence -> park.
	round := 1
	if e.db != nil {
		done, err := e.db.CountStageSuccesses(ctx, book.ID, string(state.QAAdjudicating))
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: count rounds: %w", err)
		}
		if done >= 3 {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkQANoConverge, QANoConvergeMsg)
		}
		round = done + 1
	}
	rep, err := qa.LoadReport(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: load report (qa_sweep must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("adjudicating round %d: %s", round, countNoun(len(qa.FlaggedChapters(rep)), "flagged chapter")))
	}

	autoEntries := e.autoAcceptRepairedTails(rep, book.WorkDir)
	autoSet := make(map[int]bool, len(autoEntries))
	for _, en := range autoEntries {
		autoSet[en.Chapter] = true
	}
	remaining := 0
	for _, ch := range qa.FlaggedChapters(rep) {
		if !autoSet[ch] {
			remaining++
		}
	}

	var plan *qa.Plan
	var usage agentUsage
	if remaining == 0 {
		// Every flagged chapter is auto-accepted (or the report flags nothing): an
		// all-accept plan with no agent invocation.
		plan = &qa.Plan{Entries: autoEntries}
	} else {
		p, u, aerr := e.runQAAdjudicateAgent(ctx, book, r, rep, round, autoEntries, autoSet)
		if aerr != nil {
			return scheduler.StageResult{}, aerr
		}
		plan, usage = p, u
	}

	if err := qa.WritePlan(book.WorkDir, plan); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: write plan: %w", err)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	m := usage.metricsMap()
	m["auto_accepted"] = len(autoEntries)
	result := scheduler.StageResult{
		RetranscribeNeeded: plan.RetranscribeNeeded(),
		Metrics:            metrics(m),
		// nil when every flagged chapter was auto-accepted (no agent invocation ran), so
		// a zero-work adjudication records no rate.
		RateSample: usage.rateSample(),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.QAAdjudicating), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// runQAAdjudicateAgent stages the report + flagged transcripts, runs the agent, and
// returns the MERGED plan (the daemon's auto-accept entries plus the agent's
// dispositions for the remaining chapters) validated against the report. The merge is
// what lets the plan validator require the agent to cover only the non-auto chapters
// while the persisted plan still covers every flagged chapter.
func (e *Executor) runQAAdjudicateAgent(ctx context.Context, book store.Book, r scheduler.StageReport, rep *qa.Report, round int, autoEntries []qa.PlanEntry, autoSet map[int]bool) (*qa.Plan, agentUsage, error) {
	st, err := agent.New(book.WorkDir, string(state.QAAdjudicating), e.stageAttempt(ctx, book, state.QAAdjudicating))
	if err != nil {
		return nil, agentUsage{}, err
	}
	// Required inputs.
	for _, name := range []string{qa.ReportJSONName, qa.ReportMDName, audio.ManifestName} {
		if err := st.CopyFile(filepath.Join(book.WorkDir, name), name); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", name, err)
		}
	}
	// Optional re-entry artifacts (present only on rounds > 1).
	for _, name := range []string{qa.PlanFile, repair.TailVerdictsName, repair.RepairsLogName} {
		if err := e.stageIfPresent(st, book.WorkDir, name, name); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", name, err)
		}
	}
	// The FLAGGED chapters' transcript text (and any repaired copy) - the only
	// transcripts an agent stage is ever allowed to see, and only these chapters.
	for _, ch := range qa.FlaggedChapters(rep) {
		rel := filepath.Join(transcript.TextDir, transcript.TextName(ch))
		if err := e.stageIfPresent(st, book.WorkDir, rel, rel); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", rel, err)
		}
		relRep := filepath.Join(transcript.RepairedDir, transcript.TextName(ch))
		if err := e.stageIfPresent(st, book.WorkDir, relRep, relRep); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", relRep, err)
		}
	}

	// Capture the validated MERGED plan from the successful attempt.
	var merged *qa.Plan
	validate := func(_ agent.Result, s *agent.Staging) error {
		p, perr := qa.LoadPlan(s.OutDir())
		if perr != nil {
			return perr
		}
		mp := mergePlans(autoEntries, autoSet, p)
		if verr := mp.Validate(rep); verr != nil {
			return verr
		}
		merged = mp
		return nil
	}
	data := adjudicatePromptData{Title: book.Title, Round: round, AutoAccepted: chaptersCSV(autoEntries)}
	usage, err := e.runAgent(ctx, book, state.QAAdjudicating, r, st, "adjudicate.md", data, false, validate)
	if err != nil {
		return nil, usage, err
	}
	return merged, usage, nil
}

// autoAcceptRepairedTails returns an accept entry for every flagged chapter whose only
// findings are tail-related AND which a prior tail_clip round already repaired (both
// transcripts-repaired/<ch>.txt and a tail_verdicts.json entry present). The result is
// deterministic (FlaggedChapters is sorted). A tailClipAlreadyDone error is treated as
// "not repaired" so the agent handles the chapter (conservative).
func (e *Executor) autoAcceptRepairedTails(rep *qa.Report, workDir string) []qa.PlanEntry {
	tailOnly := tailOnlyChapters(rep)
	var out []qa.PlanEntry
	for _, ch := range qa.FlaggedChapters(rep) {
		if !tailOnly[ch] {
			continue
		}
		done, err := tailClipAlreadyDone(workDir, ch)
		if err != nil || !done {
			continue
		}
		out = append(out, qa.PlanEntry{Chapter: ch, Action: qa.ActionAccept, Reason: autoAcceptTailReason})
	}
	return out
}

// tailOnlyChapters is the set of flagged (required-disposition) chapters whose ONLY
// findings across the whole report are tail-related: a tail_rate hit and/or a benign
// end_fade repeated run. A chapter carrying any wph outlier, any non-end-fade repeated
// run, any cross/within-segment hit, or ANY multi-loop finding is disqualified (those
// are not addressed by a tail_clip splice). It reads the report only; it never touches
// the golden-tested qa detectors.
func tailOnlyChapters(rep *qa.Report) map[int]bool {
	disq := map[int]bool{}
	for _, o := range rep.WPHOutliers {
		disq[o.Chapter] = true
	}
	for _, r := range rep.RepeatedRuns {
		if r.Kind != qa.KindEndFade {
			disq[r.Chapter] = true
		}
	}
	for _, h := range rep.CrossSegment {
		disq[h.Chapter] = true
	}
	for _, h := range rep.WithinSegment {
		disq[h.Chapter] = true
	}
	for _, f := range rep.MultiLoop {
		disq[f.Chapter] = true
	}
	out := map[int]bool{}
	for _, ch := range qa.FlaggedChapters(rep) {
		if !disq[ch] {
			out[ch] = true
		}
	}
	return out
}

// mergePlans combines the daemon's auto-accept entries with the agent's plan, dropping
// any agent entry for a chapter the daemon already auto-accepted (the auto disposition
// wins). The agent's free-text notes are preserved.
func mergePlans(auto []qa.PlanEntry, autoSet map[int]bool, agentPlan *qa.Plan) *qa.Plan {
	merged := &qa.Plan{Notes: agentPlan.Notes}
	merged.Entries = append(merged.Entries, auto...)
	for _, en := range agentPlan.Entries {
		if autoSet[en.Chapter] {
			continue
		}
		merged.Entries = append(merged.Entries, en)
	}
	return merged
}

// chaptersCSV renders the chapter numbers of a plan-entry slice as a "1, 3, 7" string
// (empty for no entries), for the adjudicate prompt's auto-accepted block.
func chaptersCSV(entries []qa.PlanEntry) string {
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, len(entries))
	for i, en := range entries {
		parts[i] = strconv.Itoa(en.Chapter)
	}
	return strings.Join(parts, ", ")
}

// stageIfPresent copies src (relative to workDir) into the staged dir at rel only when
// it exists, so optional inputs are skipped cleanly.
func (e *Executor) stageIfPresent(st *agent.Staging, workDir, srcRel, dstRel string) error {
	src := filepath.Join(workDir, srcRel)
	if !fsutil.IsFile(src) {
		return nil
	}
	return st.CopyFile(src, dstRel)
}

// --- retranscribing (Lane A, MECHANICAL) ---

// retranscribe executes the qa_plan.json: full-chapter re-transcription (with an
// adoption plausibility check) for "retranscribe" entries, tail-clip repair for
// "tail_clip" entries, and nothing for "accept". It is mechanical (no agent): it
// reuses the ASR backend exactly like asrStage (prompt-free), ffmpeg for clip cuts,
// and internal/repair for the adopt/splice decisions. Re-entering qa_sweep afterwards
// re-runs the sweep (advance() clears that sentinel), so a still-dirty book loops back
// to adjudication.
func (e *Executor) retranscribe(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	plan, err := qa.LoadPlan(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("retranscribing: load plan (qa_adjudicating must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(retranscribeNote(plan))
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("retranscribing: read manifest: %w", err)
	}
	durations := make(map[int]float64, len(manifest.Chapters))
	for _, ch := range manifest.Chapters {
		durations[ch.Chapter] = ch.Duration
	}

	setup, err := e.readyASR(ctx)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	// Resolve the clip cutter only if the plan needs one (a tail_clip entry). A test
	// injects e.clipCutter; production uses ffmpeg.
	cut := e.clipCutter
	if cut == nil && planHasAction(plan, qa.ActionTailClip) {
		ffmpeg, _ := e.ensureTools()
		if ffmpeg == "" {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
		}
		cut = repair.FFmpegClipCutter(ffmpeg)
	}

	// total = the work entries (every non-accept entry). completed = the work entries a
	// prior (interrupted) run already finished, so the FIRST report reflects prior work
	// and an already-repaired entry never ticks the counter on resume - the scheduler's
	// EWMA unit span then measures only the repairs THIS run actually did.
	total, completed := 0, 0
	for _, entry := range plan.Entries {
		if entry.Action == qa.ActionAccept {
			continue
		}
		total++
		if retranscribeEntryDone(book.WorkDir, entry) {
			completed++
		}
	}
	done := completed
	if r.Progress != nil {
		r.Progress(done, total)
	}

	// Time only the repair loop (backend selection, model fetch via readyASR and the
	// clip-cutter resolution are excluded) and count only entries processed THIS run
	// (done - completed), so a resume is not charged for prior work.
	loopStart := time.Now()
	var retranscribed, adopted, kept, spliced, redegen, accepted int
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err // clean pause/cancel; completed chapters remain
		}
		if entry.Action == qa.ActionAccept {
			accepted++
			continue
		}
		// Capture whether this entry was already done BEFORE processing it (processing
		// makes the predicate true), so a resumed entry re-runs idempotently but does not
		// re-tick progress past the baseline.
		wasDone := retranscribeEntryDone(book.WorkDir, entry)
		switch entry.Action {
		case qa.ActionRetranscribe:
			ok, rerr := e.retranscribeChapter(ctx, setup, book, durations[entry.Chapter], entry.Chapter)
			if rerr != nil {
				return scheduler.StageResult{}, rerr
			}
			retranscribed++
			if ok {
				adopted++
			} else {
				kept++
			}
		case qa.ActionTailClip:
			ok, rerr := e.tailClipChapter(ctx, setup, cut, book, durations[entry.Chapter], entry.Chapter)
			if rerr != nil {
				return scheduler.StageResult{}, rerr
			}
			if ok {
				spliced++
			} else {
				redegen++
			}
		default:
			return scheduler.StageResult{}, fmt.Errorf("retranscribing: chapter %d has unknown action %q", entry.Chapter, entry.Action)
		}
		if !wasDone {
			done++
			if r.Progress != nil {
				r.Progress(done, total)
			}
		}
	}

	loopSeconds := time.Since(loopStart).Seconds()
	e.accountScratch(ctx, book)
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"retranscribed":       retranscribed,
			"adopted":             adopted,
			"kept":                kept,
			"clips_spliced":       spliced,
			"clips_redegenerated": redegen,
			"accepted":            accepted,
		}),
		RateSample: rateSample(done-completed, loopSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Retranscribing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// retranscribeNote renders the descriptive stage-entry line for retranscribing: which
// chapters this run will re-transcribe or tail-clip (the non-accept plan entries). An
// all-accept plan (or empty plan) says so plainly rather than naming an empty set.
func retranscribeNote(plan *qa.Plan) string {
	var work []qa.PlanEntry
	for _, en := range plan.Entries {
		if en.Action != qa.ActionAccept {
			work = append(work, en)
		}
	}
	if len(work) == 0 {
		return "re-transcribing: no chapters queued (all accepted)"
	}
	return fmt.Sprintf("re-transcribing %s: %s", countNoun(len(work), "chapter"), chaptersCSV(work))
}

// planHasAction reports whether any plan entry carries the given action.
func planHasAction(p *qa.Plan, a qa.PlanAction) bool {
	for _, e := range p.Entries {
		if e.Action == a {
			return true
		}
	}
	return false
}

// retranscribeEntryDone reports whether a plan entry's repair already completed on a
// prior (interrupted) run, so a resume neither re-counts it as processed work (the
// EWMA span) nor re-ticks progress past the already-done baseline. It mirrors the
// per-chapter resume tests the executors themselves use: a retranscribe entry is done
// when its fresh raw parses complete (retranscribeChapter's reuse test), a tail-clip
// entry when tailClipAlreadyDone finds both durable-evidence files. Accept entries are
// not work and never reach here.
func retranscribeEntryDone(workDir string, entry qa.PlanEntry) bool {
	switch entry.Action {
	case qa.ActionRetranscribe:
		return rawComplete(filepath.Join(workDir, repair.RetranscribeDir, transcript.RawName(entry.Chapter)))
	case qa.ActionTailClip:
		done, err := tailClipAlreadyDone(workDir, entry.Chapter)
		return err == nil && done
	default:
		return false
	}
}

// retranscribeChapter re-transcribes one chapter FRESH into retranscribe/, then uses
// repair.AdoptFresh to decide (never blindly) whether the fresh run replaces the
// original. On adopt it replaces the immutable raw (unfreeze -> write -> re-freeze
// 0444), re-derives the json/text layers, and drops the chapter's stale
// repaired/corrected files (correcting re-runs fully). On keep it leaves everything.
// It returns whether the fresh run was adopted.
func (e *Executor) retranscribeChapter(ctx context.Context, setup ASRSetup, book store.Book, durationSec float64, chapter int) (bool, error) {
	flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(chapter))
	if !fsutil.IsFile(flac) {
		return false, fmt.Errorf("retranscribing: chapter %d FLAC missing (%s); split must run first", chapter, flac)
	}
	freshDir := filepath.Join(book.WorkDir, repair.RetranscribeDir)
	if err := os.MkdirAll(freshDir, 0o750); err != nil {
		return false, err
	}
	freshRawPath := filepath.Join(freshDir, transcript.RawName(chapter))
	// Resume-idempotent: reuse a fresh raw a prior (interrupted) run already produced
	// rather than re-running the expensive full-chapter ASR. The completeness test is
	// the same one asrStage uses (transcript.Complete via rawComplete). The adopt
	// decision below is itself idempotent - after an ADOPT the fresh and main raw hold
	// identical content (AdoptFresh then keeps, a no-op re-derive), after a KEEP nothing
	// changed - so re-deciding a reused raw never corrupts a chapter.
	if !rawComplete(freshRawPath) {
		// Prompt-free, same params as asrStage (no seeded initial prompt: a guess makes a
		// wrong spelling recur).
		job := asr.Job{Audio: flac, OutDir: freshDir, Chapter: chapter, Language: setup.Language}
		if terr := setup.Backend.Transcribe(ctx, job); terr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("retranscribing: transcribe chapter %d: %w", chapter, terr)
		}
	}
	freshRaw, err := os.ReadFile(freshRawPath) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return false, fmt.Errorf("retranscribing: read fresh chapter %d: %w", chapter, err)
	}
	meta := transcript.Meta{Chapter: chapter, Backend: setup.Backend.ID(), Model: setup.Model, Language: setup.Language}
	freshT, err := transcript.Normalize(freshRaw, meta)
	if err != nil {
		return false, fmt.Errorf("retranscribing: normalize fresh chapter %d: %w", chapter, err)
	}

	origWords := 0
	if origT, oerr := transcript.ReadNormalized(filepath.Join(book.WorkDir, transcript.JSONDir), chapter); oerr == nil {
		origWords = len(strings.Fields(transcript.PlainText(origT)))
	}
	freshWords := len(strings.Fields(transcript.PlainText(freshT)))
	decision := repair.AdoptFresh(
		repair.AdoptStats{Words: origWords, DurationSec: durationSec},
		repair.AdoptStats{Words: freshWords, DurationSec: durationSec},
	)
	if !decision.Adopt {
		return false, nil
	}

	// Adopt: replace the immutable raw, re-derive the json/text layers, drop stale
	// repaired/corrected for this chapter.
	rawPath := filepath.Join(book.WorkDir, transcript.RawDir, transcript.RawName(chapter))
	_ = os.Chmod(rawPath, 0o644) //nolint:gosec // lift the immutability guard to replace a re-transcribed chapter
	if err := fsutil.WriteFileAtomic(rawPath, freshRaw, 0o644); err != nil {
		return false, fmt.Errorf("retranscribing: replace raw chapter %d: %w", chapter, err)
	}
	if err := os.Chmod(rawPath, 0o444); err != nil { //nolint:gosec // re-freeze the raw layer
		e.log.Warn("retranscribing: could not re-freeze raw", "path", rawPath, "err", err)
	}
	// Re-derive the json/text layers through the shared helper (from freshRaw, identical
	// to freshT) so this path cannot drift from sanitize's derivation.
	if err := deriveChapterLayers(book.WorkDir, freshRaw, meta); err != nil {
		return false, fmt.Errorf("retranscribing: %w", err)
	}
	removeChapterDerived(book.WorkDir, chapter)
	return true, nil
}

// tailClipChapter runs the mechanical tail-clip repair for one chapter (locate the
// tail loop, cut+re-transcribe the window prompt-free, adjudicate, splice unless the
// clip re-degenerated). It returns whether a splice was written. On a splice it drops
// the chapter's stale corrected file (correcting re-runs fully).
func (e *Executor) tailClipChapter(ctx context.Context, setup ASRSetup, cut repair.ClipCutter, book store.Book, chapterEnd float64, chapter int) (bool, error) {
	// Resume-idempotent: an already-repaired chapter is skipped whole. The durable
	// evidence pair is transcripts-repaired/<ch>.txt AND a tail_verdicts.json entry -
	// both present means ClipAndSplice completed this chapter's splice, so re-running it
	// would cut+re-transcribe+re-splice and append a DUPLICATE repairs.log line. A
	// re-adjudication that wants this chapter redone must express it as "retranscribe"
	// (a full re-run, whose adopt path drops the repaired file), never another
	// "tail_clip" - so the skip cannot suppress a genuine new round's work.
	if done, derr := tailClipAlreadyDone(book.WorkDir, chapter); derr != nil {
		return false, derr
	} else if done {
		return true, nil // a prior run already spliced this chapter
	}
	origT, err := transcript.ReadNormalized(filepath.Join(book.WorkDir, transcript.JSONDir), chapter)
	if err != nil {
		return false, fmt.Errorf("retranscribing: read chapter %d transcript: %w", chapter, err)
	}
	if cut == nil {
		return false, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	transcribe := func(ctx context.Context, clipPath string) ([]byte, error) {
		outDir := filepath.Join(book.WorkDir, repair.ClipsDir)
		// Prompt-free clip transcription (contract: a seeded prompt makes the model echo
		// it over sparse audio). The backend names the raw from the chapter number.
		job := asr.Job{Audio: clipPath, OutDir: outDir, Chapter: chapter, Language: setup.Language}
		if terr := setup.Backend.Transcribe(ctx, job); terr != nil {
			return nil, terr
		}
		return os.ReadFile(filepath.Join(outDir, transcript.RawName(chapter))) //nolint:gosec // path derives from the book's work dir
	}
	res, err := repair.ClipAndSplice(ctx, repair.ClipSpliceRequest{
		WorkDir:    book.WorkDir,
		Chapter:    chapter,
		Transcript: origT,
		ChapterEnd: chapterEnd,
		Cut:        cut,
		Transcribe: transcribe,
	})
	if err != nil {
		return false, fmt.Errorf("retranscribing: tail-clip chapter %d: %w", chapter, err)
	}
	if res.Spliced {
		_ = os.Remove(filepath.Join(book.WorkDir, spelling.CorrectedDir, transcript.TextName(chapter)))
	}
	return res.Spliced, nil
}

// tailClipAlreadyDone reports whether a prior tail-clip run already spliced this
// chapter: both transcripts-repaired/<ch>.txt (the splice) and a tail_verdicts.json
// entry for the chapter (the adjudication record) are present. That pair is the
// durable evidence ClipAndSplice writes on a successful splice, so its presence makes
// re-running the entry a no-op (and prevents a duplicate repairs.log line on resume).
// A CLIP-REDEGENERATED chapter writes only the verdict, not a repaired file, so it is
// NOT skipped - a resume legitimately re-attempts it.
func tailClipAlreadyDone(workDir string, chapter int) (bool, error) {
	if !fsutil.IsFile(filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(chapter))) {
		return false, nil
	}
	verdicts, err := repair.LoadTailVerdicts(workDir)
	if err != nil {
		return false, err
	}
	for _, v := range verdicts {
		if v.Chapter == chapter {
			return true, nil
		}
	}
	return false, nil
}

// removeChapterDerived drops a chapter's stale repaired and corrected text so a later
// correcting run re-derives them from the adopted raw (both are idempotent re-derives,
// so removing them is always safe).
func removeChapterDerived(workDir string, chapter int) {
	_ = os.Remove(filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(chapter)))
	_ = os.Remove(filepath.Join(workDir, spelling.CorrectedDir, transcript.TextName(chapter)))
}
